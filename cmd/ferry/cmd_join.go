package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ZahakJ/ferry/internal/keys"
	"github.com/ZahakJ/ferry/internal/session"
)

// A lent box (see `lend` / `ferry box`) is a machine someone else lends you,
// reached with ssh over `ferry pipe`. `ferry join` makes the two machines
// feel like one: ssh config, your internet and your files and programs
// reachable from the box (through reverse tunnels inside your own ssh
// session), and the box's home mounted here. `ferry leave` undoes all of it.

// boxState is what join set up, so shell and leave can find and undo it.
type boxState struct {
	Contact string `json:"contact"`
	User    string `json:"user"`     // the account on the box
	SSHD    string `json:"sshd"`     // user | system | none: how the box reaches this machine
	Port    int    `json:"port"`     // that sshd's port on 127.0.0.1
	HisUser string `json:"his_user"` // this machine's account
	HisHome string `json:"his_home"`
	Mount   string `json:"mount"` // where the box's home is mounted here
}

const (
	boxHisPort  = 2200 // in the box: 127.0.0.1:2200 → this machine's sshd
	boxSocks    = 1080 // in the box: 127.0.0.1:1080 → SOCKS out through this machine
	sshMarkOpen = "# >>> ferry box "
	sshMarkEnd  = "# <<< ferry box "
)

func boxDir(name string) string { return filepath.Join(keys.Dir(), "box", name) }

func loadBox(name string) (*boxState, error) {
	b, err := os.ReadFile(filepath.Join(boxDir(name), "box.json"))
	if err != nil {
		return nil, err
	}
	var st boxState
	return &st, json.Unmarshal(b, &st)
}

func (st *boxState) save() error {
	b, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(filepath.Join(boxDir(st.Contact), "box.json"), b, 0o600)
}

func cmdJoin(ctx context.Context, o *out, args []string) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	user := fs.String("user", "friend", "your account on the box")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: ferry join <code> (or the name of a box you already paired with)")
	}
	name := pos[0]
	cs, _ := keys.LoadContacts()
	if _, ok := cs[name]; !ok {
		if err := cmdPair(ctx, o, []string{name}); err != nil {
			return err
		}
		after, _ := keys.LoadContacts()
		name = ""
		var newest time.Time
		for n, c := range after { // the one just paired (re-pairing a known box keeps its name)
			if c.Paired.After(newest) {
				name, newest = n, c.Paired
			}
		}
		if name == "" {
			return errors.New("paired, but could not tell which contact is the box")
		}
	}
	home, _ := os.UserHomeDir()
	me := os.Getenv("USER")
	if me == "" {
		me = filepath.Base(home)
	}
	st := &boxState{Contact: name, User: *user, HisUser: me, HisHome: home, Mount: filepath.Join(home, name)}
	if old, err := loadBox(name); err == nil {
		stopSSHD(old) // joining again: the old private sshd goes, a new one comes
	}
	if err := os.MkdirAll(boxDir(name), 0o700); err != nil {
		return err
	}

	// How the box reaches this machine: a private sshd of ours on 127.0.0.1
	// (nothing system-wide changes), else the system one if it listens there.
	if sshd := findSSHD(); sshd != "" {
		st.SSHD, st.Port = "user", freeLocalPort()
		if err := writeUserSSHD(st); err != nil {
			return err
		}
	} else if listening(22) {
		st.SSHD, st.Port = "system", 22
	} else {
		st.SSHD = "none"
		o.say("%s no ssh server on this machine, so the box can't reach it: ~/his and `there` won't work.", o.red("note:"))
		o.say("  To enable them, install one (it does not need to run as a service): sudo apt install openssh-server")
	}
	if err := st.save(); err != nil {
		return err
	}
	if err := writeSSHConfig(st); err != nil {
		return err
	}
	o.say("Added `Host %s` to ~/.ssh/config.", name)

	// The box has its own key for reaching back here; allow it, from the
	// tunnel only, with nothing but a shell and sftp.
	if st.SSHD != "none" {
		if err := ensureSSHD(st); err != nil {
			return err
		}
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		pub, err := exec.CommandContext(cctx, "ssh", "-o", "BatchMode=yes", "-o", "ClearAllForwardings=yes", name, "cat .lend/box_key.pub").Output()
		cancel()
		key := strings.TrimSpace(string(pub))
		switch {
		case err != nil:
			return fmt.Errorf("could not ssh to %s to fetch its key (is your ssh key the one you gave the lender?): %v", name, err)
		case !strings.HasPrefix(key, "ssh-"):
			o.say("%s the box has no key for reaching back (an older box); ~/his and `there` won't work until the lender updates it.", o.red("note:"))
		default:
			if err := authorize(st, key); err != nil {
				return err
			}
		}
	}
	os.MkdirAll(st.Mount, 0o755)

	o.say("")
	o.say("%s", o.bold("Before you use it, know this:"))
	o.say("  The lender can see everything inside the box. While you are connected, the box")
	o.say("  can reach your files and run commands on this machine as you, so the lender can too.")
	o.say("  Disconnecting ends that; `ferry leave %s` removes it for good.", name)
	o.say("")
	o.result("Joined %s. Now: ferry shell %s   (or plain: ssh %s)", name, name, name)
	o.event("joined", map[string]any{"name": name, "sshd": st.SSHD})
	return nil
}

func cmdShell(ctx context.Context, o *out, args []string) error {
	fs := flag.NewFlagSet("shell", flag.ContinueOnError)
	noPorts := fs.Bool("no-ports", false, "don't share the box's ports to localhost here (ferry config box-ports off: never)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: ferry shell [--no-ports] <box>")
	}
	st, err := loadBox(pos[0])
	if err != nil {
		return fmt.Errorf("%q is not a box you joined (ferry join <code>)", pos[0])
	}
	if err := ensureSSHD(st); err != nil {
		o.say("note: %v; ~/his and `there` won't work this time", err)
	}
	if sshfs, _ := exec.LookPath("sshfs"); sshfs == "" {
		o.say(o.dim("(install sshfs to see the box's files at " + st.Mount + ")"))
	} else if !mounted(st.Mount) {
		os.MkdirAll(st.Mount, 0o755)
		c := exec.Command(sshfs, st.Contact+":", st.Mount, "-o", "reconnect,ServerAliveInterval=15,ServerAliveCountMax=3")
		if out, err := c.CombinedOutput(); err != nil {
			o.say("note: could not mount the box's home at %s: %s", st.Mount, strings.TrimSpace(string(out)))
		}
	}
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		return err
	}
	if cfg, err := session.LoadFileConfig(); *noPorts || (err == nil && cfg.NoBoxPorts) {
		return syscall.Exec(ssh, []string{"ssh", "-t", st.Contact}, os.Environ())
	}
	return shellWithPorts(ctx, o, st, ssh)
}

// shellWithPorts logs in over the box's ssh master and shares the box's
// ports here while the session lasts. The last shell to close ends the
// master, and with it the box's way back to this machine.
func shellWithPorts(ctx context.Context, o *out, st *boxState, ssh string) error {
	// Every open shell holds shell.lock shared; whoever leaves it unshared is the last.
	lf, err := os.OpenFile(filepath.Join(boxDir(st.Contact), "shell.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	syscall.Flock(int(lf.Fd()), syscall.LOCK_SH)
	c, err := ensureMaster(st)
	if err != nil && ctx.Err() != nil {
		return ctx.Err() // ^C at the login
	}
	if err != nil {
		o.say("note: %v; logging in without sharing the box's ports", err)
		lf.Close()
		return syscall.Exec(ssh, []string{"ssh", "-t", st.Contact}, os.Environ())
	}
	// A closed terminal hangs up only the session leader, which is now this
	// process, not ssh: pass it on, then stay to clean up after it.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	cmd := exec.Command(ssh, "-t", "-S", strings.ReplaceAll(c.cm, "%", "%%"), "-o", "ControlMaster=no", "-o", "ClearAllForwardings=yes", st.Contact)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	wctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { watchPorts(wctx, st, c); close(done) }()
	go func() { // ferry shell hung up or told to stop: so is its ssh
		var sig os.Signal
		select {
		case sig = <-hup:
		case <-ctx.Done():
			sig = syscall.SIGTERM
		case <-wctx.Done():
			return
		}
		cmd.Process.Signal(sig)
		select {
		case <-wctx.Done():
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
		}
	}()
	werr := cmd.Wait()
	stop()
	<-done
	if syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		c.exit()
		os.Remove(portsFile(st.Contact))
	}
	lf.Close()
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		os.Exit(ee.ExitCode()) // the box shell's own status, like plain ssh
	}
	return werr
}

func cmdLeave(ctx context.Context, o *out, args []string) error {
	pos, err := parse(flag.NewFlagSet("leave", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: ferry leave <box>")
	}
	name := pos[0]
	st, err := loadBox(name)
	if err != nil {
		return fmt.Errorf("%q is not a box you joined", name)
	}
	if mounted(st.Mount) {
		unmount(st.Mount)
		o.say("Unmounted %s.", st.Mount)
	}
	stopMaster(name)    // ferry shell's connection, its shared ports, and the sessions on it
	stopPipes(name)     // your open ssh sessions to the box, and the tunnels in them
	os.Remove(st.Mount) // only if empty
	stopSSHD(st)
	if st.SSHD == "system" {
		if n, _ := unauthorize(name); n > 0 {
			o.say("Removed the box's key from ~/.ssh/authorized_keys.")
		}
	}
	if err := removeSSHConfig(name); err == nil {
		o.say("Removed `Host %s` from ~/.ssh/config.", name)
	}
	os.RemoveAll(boxDir(name))
	if cs, err := keys.LoadContacts(); err == nil {
		if _, ok := cs[name]; ok {
			delete(cs, name)
			cs.Save()
			o.say("Forgot the pairing with %s.", name)
		}
	}
	o.result("Left %s. Nothing of it is left on this machine.", name)
	o.event("left", map[string]any{"name": name})
	return nil
}

// ---- this machine's sshd, for the box to reach back ----

func findSSHD() string {
	for _, p := range []string{"/usr/sbin/sshd", "/usr/bin/sshd", "/usr/local/sbin/sshd", "/opt/homebrew/sbin/sshd"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	p, _ := exec.LookPath("sshd")
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

func writeUserSSHD(st *boxState) error {
	d := boxDir(st.Contact)
	hk := filepath.Join(d, "host_key")
	if _, err := os.Stat(hk); err != nil {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "ferry-box-"+st.Contact, "-f", hk).CombinedOutput(); err != nil {
			return fmt.Errorf("ssh-keygen: %s", out)
		}
	}
	// Listens on 127.0.0.1 only: the box gets here solely through the
	// tunnel inside your own ssh session. Key only, no forwarding.
	cfg := fmt.Sprintf(`Port %d
ListenAddress 127.0.0.1
HostKey %s
PidFile %s
AuthorizedKeysFile %s
AllowUsers %s
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
UsePAM no
StrictModes no
AllowTcpForwarding no
AllowAgentForwarding no
X11Forwarding no
PermitTunnel no
LogLevel VERBOSE
Subsystem sftp internal-sftp
`, st.Port, hk, filepath.Join(d, "sshd.pid"), filepath.Join(d, "authorized_keys"), st.HisUser)
	if err := os.WriteFile(filepath.Join(d, "sshd_config"), []byte(cfg), 0o600); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(d, "authorized_keys"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		f.Close()
	}
	return err
}

func sshdRunning(st *boxState) bool {
	b, err := os.ReadFile(filepath.Join(boxDir(st.Contact), "sshd.pid"))
	if err != nil {
		return false
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid > 0 && syscall.Kill(pid, 0) == nil && listening(st.Port)
}

// ensureSSHD starts our private sshd if join set one up and it is not up.
func ensureSSHD(st *boxState) error {
	if st.SSHD != "user" || sshdRunning(st) {
		return nil
	}
	d := boxDir(st.Contact)
	// Several ferry pipes (ssh, sshfs) can start at once; one starts sshd.
	if lf, err := os.OpenFile(filepath.Join(d, "sshd.lock"), os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		defer lf.Close()
		syscall.Flock(int(lf.Fd()), syscall.LOCK_EX)
		if sshdRunning(st) {
			return nil
		}
	}
	out, err := exec.Command(findSSHD(), "-f", filepath.Join(d, "sshd_config"), "-E", filepath.Join(d, "sshd.log")).CombinedOutput()
	if err != nil {
		return fmt.Errorf("could not start the sshd the box reaches you through: %s", strings.TrimSpace(string(out)))
	}
	for i := 0; i < 50 && !listening(st.Port); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

// stopSSHD stops our private sshd and every session it is serving, so
// whatever the box had open on this machine closes now, not at its leisure.
func stopSSHD(st *boxState) {
	cfg := filepath.Join(boxDir(st.Contact), "sshd_config")
	var roots []int
	if b, err := os.ReadFile(filepath.Join(boxDir(st.Contact), "sshd.pid")); err == nil {
		if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 0 {
			roots = append(roots, pid)
		}
	}
	// sshd rewrites its title ("sshd: /usr/sbin/sshd -f <cfg> [listener]"), so match the path
	roots = append(roots, procsMatching(cfg)...)
	killTree(roots)
}

// stopPipes ends this machine's ssh sessions to the box (their ProxyCommand).
func stopPipes(name string) { killTree(procsMatching("\x00pipe\x00" + name + "\x00")) }

// procsMatching lists processes whose command line contains s (Linux; elsewhere none).
func procsMatching(s string) []int {
	var out []int
	ps, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, p := range ps {
		if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), s) {
			if pid, _ := strconv.Atoi(filepath.Base(filepath.Dir(p))); pid > 0 && pid != os.Getpid() {
				out = append(out, pid)
			}
		}
	}
	return out
}

// killTree sends SIGTERM to each pid and all of its descendants.
func killTree(roots []int) {
	kids := map[int][]int{}
	ps, _ := filepath.Glob("/proc/[0-9]*/stat")
	for _, p := range ps {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...: comm may hold spaces, so split after ')'
		f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
		if len(f) > 1 {
			pid, _ := strconv.Atoi(filepath.Base(filepath.Dir(p)))
			ppid, _ := strconv.Atoi(f[1])
			kids[ppid] = append(kids[ppid], pid)
		}
	}
	seen := map[int]bool{}
	var walk func(int)
	walk = func(p int) {
		if seen[p] {
			return
		}
		seen[p] = true
		for _, k := range kids[p] {
			walk(k)
		}
		syscall.Kill(p, syscall.SIGTERM)
	}
	for _, r := range roots {
		walk(r)
	}
}

const keyOpts = `from="127.0.0.1",no-agent-forwarding,no-X11-forwarding,no-port-forwarding`

func authorize(st *boxState, key string) error {
	line := keyOpts + " " + key + " ferry-box:" + st.Contact + "\n"
	if st.SSHD == "user" {
		return os.WriteFile(filepath.Join(boxDir(st.Contact), "authorized_keys"), []byte(line), 0o600)
	}
	home, _ := os.UserHomeDir()
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	unauthorize(st.Contact)
	f, err := os.OpenFile(filepath.Join(home, ".ssh", "authorized_keys"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
}

func unauthorize(name string) (int, error) {
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".ssh", "authorized_keys")
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	var keep []string
	n := 0
	for _, l := range strings.SplitAfter(string(b), "\n") {
		if strings.HasSuffix(strings.TrimSpace(l), " ferry-box:"+name) {
			n++
			continue
		}
		keep = append(keep, l)
	}
	if n == 0 {
		return 0, nil
	}
	return n, os.WriteFile(p, []byte(strings.Join(keep, "")), 0o600)
}

// ---- ~/.ssh/config: one marked block per box, first in the file ----

func sshConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ssh", "config")
}

func stripBlock(s, name string) (string, bool) {
	open, end := sshMarkOpen+name+"\n", sshMarkEnd+name+"\n"
	i := strings.Index(s, open)
	if i < 0 {
		return s, false
	}
	j := strings.Index(s[i:], end)
	if j < 0 {
		return s, false
	}
	return s[:i] + s[i+j+len(end):], true
}

func writeSSHConfig(st *boxState) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s\n", sshMarkOpen, st.Contact)
	fmt.Fprintf(&b, "# added by ferry join; ferry leave %s removes it\n", st.Contact)
	fmt.Fprintf(&b, "Host %s\n    User %s\n    ProxyCommand %q pipe %s\n    HostKeyAlias %s\n", st.Contact, st.User, exe, st.Contact, st.Contact)
	b.WriteString("    StrictHostKeyChecking accept-new\n    ServerAliveInterval 15\n    ExitOnForwardFailure no\n")
	fmt.Fprintf(&b, "    RemoteForward 127.0.0.1:%d\n", boxSocks) // your internet, as SOCKS
	if st.SSHD != "none" {
		fmt.Fprintf(&b, "    RemoteForward 127.0.0.1:%d 127.0.0.1:%d\n", boxHisPort, st.Port) // the box → you
	}
	fmt.Fprintf(&b, "    SetEnv LEND_HIS_USER=%s LEND_HIS_HOME=%s\n", st.HisUser, st.HisHome)
	fmt.Fprintf(&b, "%s%s\n\n", sshMarkEnd, st.Contact)

	p := sshConfigPath()
	os.MkdirAll(filepath.Dir(p), 0o700)
	old, _ := os.ReadFile(p)
	rest, _ := stripBlock(string(old), st.Contact)
	return os.WriteFile(p, []byte(b.String()+rest), 0o600)
}

func removeSSHConfig(name string) error {
	p := sshConfigPath()
	old, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	rest, ok := stripBlock(string(old), name)
	if !ok {
		return errors.New("no block")
	}
	return os.WriteFile(p, []byte(strings.TrimLeft(rest, "\n")), 0o600)
}

// ---- the box's home, mounted here ----

func mounted(dir string) bool {
	b, err := os.ReadFile("/proc/self/mounts")
	if err != nil { // macOS: ask mount(8)
		out, _ := exec.Command("mount").Output()
		return strings.Contains(string(out), " on "+dir+" ")
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) > 1 && f[1] == dir {
			return true
		}
	}
	return false
}

func unmount(dir string) {
	for _, c := range [][]string{{"fusermount3", "-u", "-z", dir}, {"fusermount", "-u", "-z", dir}, {"umount", dir}} {
		if exec.Command(c[0], c[1:]...).Run() == nil {
			return
		}
	}
}
