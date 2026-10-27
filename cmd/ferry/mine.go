package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ZahakJ/ferry/internal/keys"
)

// ferry mine: all of one owner's devices as one machine.
//
// Devices linked with `ferry mine add` / `ferry mine join` are contacts of
// kind "mine". Unlike a friend, each gets full access to the others: every
// device runs a private sshd on its own 127.0.0.1 (key-only, only the mesh
// keys), which only its own devices can reach, through ferry's direct link
// (the agent refuses everyone else). On top of that ssh: each device's home
// mounted at ~/m/<device>, `on <device> <cmd>`, Claude sessions synced, and
// ports forwarded on request.
//
// Everything mine keeps lives in <ferry dir>/mine. Outside it, ferry touches
// one marked block per device in ~/.ssh/config, an `on` shim in ~/.local/bin,
// ~/m/<device> mount points, and (only if you turn the fallback on) a marked
// line in ~/.bashrc and ~/.zshrc. `ferry mine rm` removes them.

// mineState is this device's mesh settings and the messages it still owes
// other devices (introductions and revocations that wait for them to be
// online).
type mineState struct {
	Port       int       `json:"port"`                  // the private sshd, on 127.0.0.1
	SystemSSHD bool      `json:"system_sshd,omitempty"` // use the system sshd (port 22) instead
	Fallback   bool      `json:"fallback,omitempty"`
	Pending    []mineOp  `json:"pending,omitempty"`
	Fwds       []mineFwd `json:"fwds,omitempty"`
}

// mineOp is one message owed to another device.
type mineOp struct {
	To     keys.Public     `json:"to"`
	ToName string          `json:"to_name"`
	Want   string          `json:"want"` // mine-hello | mine-forget | mine-leave
	Data   json.RawMessage `json:"data,omitempty"`
	Added  time.Time       `json:"added"`
}

// mineFwd is a port of another device forwarded to 127.0.0.1 here.
type mineFwd struct {
	Dev   string `json:"dev"`
	Port  int    `json:"port"`  // there
	Local int    `json:"local"` // here
}

// mineMember is a device as one device introduces it to another.
type mineMember struct {
	Name   string      `json:"name"`
	Key    keys.Public `json:"key"`
	Device keys.Device `json:"device"`
}

func mineDir() string             { return filepath.Join(keys.Dir(), "mine") }
func minePath(f string) string    { return filepath.Join(mineDir(), f) }
func mineHome() string            { h, _ := os.UserHomeDir(); return h }
func mineMountRoot() string       { return filepath.Join(mineHome(), "m") }
func mineMount(dev string) string { return filepath.Join(mineMountRoot(), dev) }

func loadMineState() (*mineState, error) {
	b, err := os.ReadFile(minePath("state.json"))
	if err != nil {
		return nil, err
	}
	var st mineState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s is damaged: %w", minePath("state.json"), err)
	}
	return &st, nil
}

func (st *mineState) save() error {
	b, _ := json.MarshalIndent(st, "", "  ")
	return writeAtomic(minePath("state.json"), b, 0o600)
}

func writeAtomic(path string, b []byte, mode os.FileMode) error {
	if old, err := os.ReadFile(path); err == nil && string(old) == string(b) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// mineLock serializes changes to the mesh (contacts, state, config files)
// between ferry up and the commands.
func mineLock() func() {
	os.MkdirAll(mineDir(), 0o700)
	f, err := os.OpenFile(minePath("lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	return func() { f.Close() }
}

// mineDevices returns your own devices, by name.
func mineDevices() ([]keys.Contact, error) {
	cs, err := keys.LoadContacts()
	if err != nil {
		return nil, err
	}
	var out []keys.Contact
	for _, n := range cs.Names() {
		if cs[n].Mine() {
			out = append(out, cs[n])
		}
	}
	return out, nil
}

func mineDevice(name string) (keys.Contact, error) {
	ds, err := mineDevices()
	if err != nil {
		return keys.Contact{}, err
	}
	var names []string
	for _, d := range ds {
		if d.Name == name {
			return d, nil
		}
		names = append(names, d.Name)
	}
	if len(names) == 0 {
		return keys.Contact{}, fmt.Errorf("%q is not one of your devices; none are linked yet (ferry mine add)", name)
	}
	return keys.Contact{}, fmt.Errorf("%q is not one of your devices; they are: %s", name, strings.Join(names, ", "))
}

// ---- this device's side of the mesh ----

// setupMineLocal makes this device's mesh keys and sshd config, once, and
// returns what it tells the others about itself.
func setupMineLocal(systemSSHD bool) (*keys.Device, *mineState, error) {
	if err := os.MkdirAll(mineDir(), 0o700); err != nil {
		return nil, nil, err
	}
	id, _, err := keys.LoadOrCreate()
	if err != nil {
		return nil, nil, err
	}
	for _, k := range []string{"id_ed25519", "host_key"} {
		p := minePath(k)
		if exists(p) {
			continue
		}
		out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "ferry-mine:"+id.Name, "-f", p).CombinedOutput()
		if err != nil {
			return nil, nil, fmt.Errorf("ssh-keygen (is OpenSSH installed?): %s", strings.TrimSpace(string(out)))
		}
	}
	st, err := loadMineState()
	if err != nil {
		st = &mineState{}
	}
	if systemSSHD {
		st.SystemSSHD = true
	}
	if !st.SystemSSHD && st.Port == 0 {
		st.Port = freeLocalPort()
	}
	if err := st.save(); err != nil {
		return nil, nil, err
	}
	if err := writeMineSSHD(st); err != nil {
		return nil, nil, err
	}
	dev, err := thisDevice(st)
	return dev, st, err
}

func thisDevice(st *mineState) (*keys.Device, error) {
	user := os.Getenv("USER")
	if user == "" {
		user = filepath.Base(mineHome())
	}
	ck, err := pubKey(minePath("id_ed25519.pub"))
	if err != nil {
		return nil, err
	}
	hkFile := minePath("host_key.pub")
	if st.SystemSSHD {
		hkFile = "/etc/ssh/ssh_host_ed25519_key.pub"
	}
	hk, err := pubKey(hkFile)
	if err != nil {
		return nil, err
	}
	return &keys.Device{User: user, Home: mineHome(), OS: runtime.GOOS, SSHKey: ck, HostKey: hk}, nil
}

// pubKey reads "type base64" from an OpenSSH public key file.
func pubKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(b))
	if len(f) < 2 || !strings.HasPrefix(f[0], "ssh-") {
		return "", fmt.Errorf("%s is not an ssh public key", path)
	}
	return f[0] + " " + f[1], nil
}

var sshKeyRE = regexp.MustCompile(`^ssh-[a-z0-9-]+ [A-Za-z0-9+/=]+$`)

func validDevice(d *keys.Device) error {
	switch {
	case d == nil:
		return errors.New("the other device sent no device details (is its ferry up to date?)")
	case !sshKeyRE.MatchString(d.SSHKey) || !sshKeyRE.MatchString(d.HostKey):
		return errors.New("the other device sent a malformed ssh key")
	case !validHomePath(d.Home) || d.User == "" || strings.ContainsAny(d.User, " \t\n\"'\\/"):
		return fmt.Errorf("the other device sent an unusable home or user (%q, %q)", d.Home, d.User)
	}
	return nil
}

func validHomePath(h string) bool {
	return strings.HasPrefix(h, "/") && !strings.ContainsAny(h, "\n\t\"'\\") && filepath.Clean(h) == h && h != "/"
}

func findSSHD() string {
	for _, p := range []string{"/usr/sbin/sshd", "/usr/bin/sshd", "/usr/local/sbin/sshd", "/opt/homebrew/sbin/sshd"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	p, _ := exec.LookPath("sshd")
	if p != "" {
		p, _ = filepath.Abs(p)
	}
	return p
}

func freeLocalPort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 22022
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func listening(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err == nil {
		c.Close()
	}
	return err == nil
}

// sq quotes a path for ssh_config / sshd_config.
func sq(p string) string { return `"` + p + `"` }

// writeMineSSHD writes the private sshd's config: 127.0.0.1 only, keys only,
// only this user, only the keys of your own devices.
func writeMineSSHD(st *mineState) error {
	user := os.Getenv("USER")
	if user == "" {
		user = filepath.Base(mineHome())
	}
	cfg := fmt.Sprintf(`# ferry mine: the sshd your own devices reach this one through (ferry pipe).
# It listens on 127.0.0.1 only and opens only for the keys in authorized_keys.
Port %d
ListenAddress 127.0.0.1
HostKey %s
PidFile none
AuthorizedKeysFile %s
AllowUsers %s
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
UsePAM no
StrictModes no
AllowTcpForwarding local
AllowAgentForwarding no
AllowStreamLocalForwarding no
X11Forwarding no
PermitTunnel no
GatewayPorts no
PermitUserEnvironment no
LogLevel VERBOSE
Subsystem sftp internal-sftp
`, st.Port, sq(minePath("host_key")), sq(minePath("authorized_keys")), user)
	return writeAtomic(minePath("sshd_config"), []byte(cfg), 0o600)
}

// ---- converging the files outside ferry's folder with the contacts ----

const (
	mineMarkOpen = "# >>> ferry mine "
	mineMarkEnd  = "# <<< ferry mine "
	mineKeyTag   = " ferry-mine:"
	shimMark     = "# made by ferry mine"
)

// mineConverge makes authorized_keys, known_hosts, ~/.ssh/config, the
// mounts and the ssh connections match the devices in contacts: blocks
// and keys for each, and nothing for a device that is gone. It reports
// each thing it removes through say.
func mineConverge(say func(string, ...any)) error {
	if say == nil {
		say = func(string, ...any) {}
	}
	ds, err := mineDevices()
	if err != nil {
		return err
	}
	st, err := loadMineState()
	if err != nil {
		st = &mineState{}
	}
	keep := map[string]bool{}
	var ak, kh strings.Builder
	for _, d := range ds {
		keep[d.Name] = true
		fmt.Fprintf(&ak, "no-agent-forwarding,no-X11-forwarding %s%s%s\n", d.Device.SSHKey, mineKeyTag, d.Name)
		fmt.Fprintf(&kh, "%s %s\n", d.Name, d.Device.HostKey)
	}
	if err := writeAtomic(minePath("known_hosts"), []byte(kh.String()), 0o600); err != nil {
		return err
	}
	if st.SystemSSHD {
		if err := writeAtomic(minePath("authorized_keys"), nil, 0o600); err != nil {
			return err
		}
		if err := systemAuthorize(ak.String()); err != nil {
			return err
		}
	} else if err := writeAtomic(minePath("authorized_keys"), []byte(ak.String()), 0o600); err != nil {
		return err
	}
	// ~/.ssh/config: one block per device, first in the file.
	for _, n := range mineBlocks() {
		if !keep[n] {
			if removeMarked(sshConfigPath(), mineMarkOpen, mineMarkEnd, n) {
				say("Removed `Host %s` from ~/.ssh/config.", n)
			}
			mp := mineMount(n)
			if mounted(mp) {
				unmount(mp)
				say("Unmounted %s.", mp)
			}
			if os.Remove(mp) == nil {
				say("Removed the empty folder %s.", mp)
			}
			stopMineMaster(n)
		}
	}
	for _, d := range ds {
		if err := writeMarked(sshConfigPath(), mineMarkOpen, mineMarkEnd, d.Name, mineBlock(d)); err != nil {
			return err
		}
	}
	var fw []mineFwd
	for _, f := range st.Fwds {
		if keep[f.Dev] {
			fw = append(fw, f)
		}
	}
	if len(fw) != len(st.Fwds) {
		st.Fwds = fw
		st.save()
	}
	if len(ds) == 0 {
		if removeShim() {
			say("Removed the `on` command from ~/.local/bin.")
		}
		for _, rc := range fallbackRCs(false) {
			if removeMarked(rc, fbMarkOpen, fbMarkEnd, "fallback") {
				say("Removed the command fallback from %s.", tildePath(rc))
			}
		}
		if st.Fallback {
			st.Fallback = false
			st.save()
		}
		os.Remove(minePath("fallback.sh"))
		os.Remove(minePath("status.json"))
		os.Remove(mineMountRoot()) // only if empty
	}
	return nil
}

// systemAuthorize replaces ferry's marked lines in ~/.ssh/authorized_keys
// (only with --system-sshd).
func systemAuthorize(lines string) error {
	p := filepath.Join(mineHome(), ".ssh", "authorized_keys")
	old, _ := os.ReadFile(p)
	var keep []string
	for _, l := range strings.SplitAfter(string(old), "\n") {
		if l == "" || strings.Contains(l, mineKeyTag) {
			continue
		}
		keep = append(keep, l)
	}
	s := strings.Join(keep, "")
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	os.MkdirAll(filepath.Dir(p), 0o700)
	return os.WriteFile(p, []byte(s+lines), 0o600)
}

func mineControlPath(dev string) string {
	p := minePath("cm-" + dev)
	if len(p) > 80 {
		p = filepath.Join(os.TempDir(), fmt.Sprintf("ferry-%d-%x-%s", os.Getuid(), shortHash(mineDir()), dev))
	}
	return p
}

// proxyCommand is how ssh reaches a device: ferry pipe, with this ferry's
// folder if it is not the default one.
func proxyCommand(dev string) string {
	exe, _ := os.Executable()
	pc := fmt.Sprintf("%q pipe %s", exe, dev)
	if h := os.Getenv("FERRY_HOME"); h != "" {
		pc = fmt.Sprintf("env FERRY_HOME=%q %s", h, pc)
	}
	return pc
}

func mineBlock(d keys.Contact) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# your own device (ferry mine); `ferry mine rm %s` removes this\n", d.Name)
	fmt.Fprintf(&b, "%s%s\n", blockDirTag, keys.Dir())
	fmt.Fprintf(&b, "Host %s\n    HostName %s\n    User %s\n    ProxyCommand %s\n", d.Name, d.Name, d.Device.User, proxyCommand(d.Name))
	fmt.Fprintf(&b, "    HostKeyAlias %s\n    UserKnownHostsFile %s\n    GlobalKnownHostsFile /dev/null\n    StrictHostKeyChecking yes\n", d.Name, sq(minePath("known_hosts")))
	fmt.Fprintf(&b, "    IdentityFile %s\n    IdentitiesOnly yes\n", sq(minePath("id_ed25519")))
	fmt.Fprintf(&b, "    ControlMaster auto\n    ControlPath %s\n    ControlPersist yes\n", sq(strings.ReplaceAll(mineControlPath(d.Name), "%", "%%")))
	b.WriteString("    ServerAliveInterval 15\n    ServerAliveCountMax 3\n    ForwardAgent no\n    ForwardX11 no\n")
	return b.String()
}

const blockDirTag = "# ferry folder: "

// mineBlocks lists the devices that have a block in ~/.ssh/config written
// for this ferry folder (another FERRY_HOME's blocks are not ours to touch).
func mineBlocks() []string {
	b, _ := os.ReadFile(sshConfigPath())
	s := string(b)
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if n, ok := strings.CutPrefix(l, mineMarkOpen); ok && n != "" {
			i := strings.Index(s, mineMarkOpen+n+"\n")
			j := strings.Index(s[i:], mineMarkEnd+n+"\n")
			if j > 0 && strings.Contains(s[i:i+j], "\n"+blockDirTag+keys.Dir()+"\n") {
				out = append(out, n)
			}
		}
	}
	return out
}

// stripMarked removes the block for name between open+name and end+name lines.
func stripMarked(s, open, end, name string) (string, bool) {
	o, e := open+name+"\n", end+name+"\n"
	i := strings.Index(s, o)
	if i < 0 {
		return s, false
	}
	j := strings.Index(s[i:], e)
	if j < 0 {
		return s, false
	}
	rest := s[i+j+len(e):]
	if i == 0 || strings.HasSuffix(s[:i], "\n") {
		rest = strings.TrimPrefix(rest, "\n") // the blank line that followed it
	}
	return s[:i] + rest, true
}

// writeMarked puts a marked block for name first in the file at path,
// replacing the old one. It leaves the file alone if nothing changes.
func writeMarked(path, open, end, name, body string) error {
	blk := open + name + "\n" + body + end + name + "\n\n"
	old, _ := os.ReadFile(path)
	if strings.HasPrefix(string(old), blk) {
		return nil
	}
	rest, _ := stripMarked(string(old), open, end, name)
	os.MkdirAll(filepath.Dir(path), 0o700)
	return os.WriteFile(path, []byte(blk+rest), 0o600) // in place: a symlinked dotfile stays one
}

func removeMarked(path, open, end, name string) bool {
	old, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	rest, ok := stripMarked(string(old), open, end, name)
	if !ok {
		return false
	}
	return os.WriteFile(path, []byte(rest), 0o600) == nil // in place; keeps the file's mode
}

// ---- ssh to a device ----

// mineSSHArgs are ssh's arguments for a device, from its block in
// ~/.ssh/config; nothing ever prompts.
func mineSSHArgs(dev string, args ...string) []string {
	return append([]string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=30", "-q", dev}, args...)
}

// mineRun runs a POSIX sh script on a device and returns its stdout.
func mineRun(ctx context.Context, dev, script string, stdin string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", mineSSHArgs(dev, "exec /bin/sh -c "+shq(script))...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.WaitDelay = 2 * time.Second // a ControlPersist master keeps our pipes open
	out, err := cmd.Output()
	if err != nil {
		if s := strings.TrimSpace(stderr.String()); s != "" {
			return out, fmt.Errorf("%v: %s", err, lastLine(s))
		}
	}
	return out, err
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func mineMasterAlive(dev string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "ssh", "-F", "none", "-S", mineControlPath(dev), "-O", "check", dev).Run() == nil
}

func stopMineMaster(dev string) {
	cp := mineControlPath(dev)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exec.CommandContext(ctx, "ssh", "-F", "none", "-S", cp, "-O", "exit", dev).Run()
	killTree(procsMatching("\x00pipe\x00" + dev + "\x00"))
	os.Remove(cp)
}

// ---- the `on` shim ----

func shimPath() string { return filepath.Join(mineHome(), ".local", "bin", "on") }

// installShim puts a tiny `on` in ~/.local/bin that runs `ferry on`. It
// never replaces an `on` that ferry did not make.
func installShim() (string, error) {
	p := shimPath()
	if b, err := os.ReadFile(p); err == nil && !strings.Contains(string(b), shimMark) {
		return "", fmt.Errorf("%s exists and is not ferry's; use `ferry on <device> <cmd>` instead", tildePath(p))
	}
	exe, _ := os.Executable()
	env := ""
	if h := os.Getenv("FERRY_HOME"); h != "" {
		env = "FERRY_HOME=" + shq(h) + " "
	}
	s := fmt.Sprintf("#!/bin/sh\n%s: on <device> <command...> runs it on one of your own devices\nexec env %s%s on \"$@\"\n", shimMark, env, shq(exe))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	return p, writeAtomic(p, []byte(s), 0o755)
}

func removeShim() bool {
	b, err := os.ReadFile(shimPath())
	if err != nil || !strings.Contains(string(b), shimMark) {
		return false
	}
	return os.Remove(shimPath()) == nil
}

func onPath(dir string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p == dir {
			return true
		}
	}
	return false
}

// ---- device names ----

// deviceName suggests a name for this device from its host name.
func deviceName() string {
	h, _ := os.Hostname()
	h = strings.ToLower(h)
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	h = regexp.MustCompile(`[^a-z0-9_-]+`).ReplaceAllString(h, "-")
	h = strings.Trim(h, "-_")
	if len(h) > 32 {
		h = strings.Trim(h[:32], "-_")
	}
	if keys.CheckName(h) != nil {
		return ""
	}
	return h
}

// mineNameFor picks the local name for a device: its own name, unless that
// is taken by someone else or is a word `on` gives a meaning to.
func mineNameFor(cs keys.Contacts, want string, k keys.Public) string {
	if want == "gpu" {
		want = "gpu-box"
	}
	return cs.FreeName(want, k)
}

// recordMine adds or updates devices as yours. It returns the names used.
func recordMine(ms []mineMember, self keys.Public) ([]string, error) {
	cs, err := keys.LoadContacts()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, m := range ms {
		if string(m.Key.Sign) == string(self.Sign) || len(m.Key.Sign) == 0 {
			continue
		}
		if validDevice(&m.Device) != nil {
			continue
		}
		dev := m.Device
		name := m.Name
		if old, ok := cs.ByKey(m.Key.Sign); ok {
			delete(cs, old.Name)
			name = old.Name
		} else {
			if keys.CheckName(name) != nil {
				name = m.Key.Name
			}
			name = mineNameFor(cs, name, m.Key)
		}
		cs[name] = keys.Contact{Name: name, Key: m.Key, Paired: time.Now().UTC(), Kind: keys.KindMine, Device: &dev}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, cs.Save()
}

// roster lists your devices as introductions for another, leaving out skip.
func roster(skip []byte) []mineMember {
	ds, _ := mineDevices()
	var out []mineMember
	for _, d := range ds {
		if string(d.Key.Sign) == string(skip) {
			continue
		}
		out = append(out, mineMember{Name: d.Name, Key: d.Key, Device: *d.Device})
	}
	return out
}

// ensureMineMaster starts the device's ssh master (one connection that
// `on`, ssh, sync and forwards all ride) unless it is up. It reports
// whether it had to start one.
func ensureMineMaster(ctx context.Context, dev string) (started bool, err error) {
	if mineMasterAlive(dev) {
		return false, nil
	}
	if lf, err := os.OpenFile(minePath("master-"+dev+".lock"), os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		defer lf.Close()
		syscall.Flock(int(lf.Fd()), syscall.LOCK_EX) // ferry up and a command starting one at once
		if mineMasterAlive(dev) {
			return false, nil
		}
	}
	os.Remove(mineControlPath(dev)) // a stale socket would make ssh skip multiplexing
	log, err := os.OpenFile(minePath("ssh-"+dev+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return false, err
	}
	defer log.Close()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=30", "-o", "ControlMaster=yes", "-f", "-N", dev)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		b, _ := os.ReadFile(minePath("ssh-" + dev + ".log"))
		if s := lastLine(string(b)); s != "" {
			return false, errors.New(s)
		}
		return false, err
	}
	for i := 0; i < 30 && !mineMasterAlive(dev); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if !mineMasterAlive(dev) {
		return false, errors.New("the ssh connection did not come up")
	}
	return true, nil
}
