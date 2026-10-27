package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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

// A lent box (see `lend`) is a machine someone else lends you, reached with
// ssh over `ferry pipe`. It is a one-way mirror: this machine reaches into
// the box (ssh, its files mounted here, its ports on localhost here), and the
// box gets nothing back except, while `ferry shell` is open, public-internet
// traffic through a filtered SOCKS proxy here (cmd_proxy.go). Your files go
// to the box only when you bring them (`ferry bring`), and come back when you
// take them (`ferry take`). `ferry leave` undoes everything join set up.

// boxState is what join set up, so shell and leave can find and undo it.
type boxState struct {
	Contact    string `json:"contact"`
	User       string `json:"user"`     // the account on the box
	HisUser    string `json:"his_user"` // this machine's account
	HisHome    string `json:"his_home"` // where bring puts your folders in the box: the same path as here
	Mount      string `json:"mount"`    // where the box's copy of your home is mounted here
	NoInternet bool   `json:"no_internet,omitempty"`

	// Left by ferry before 0-c39c318+1, which let the box reach back into
	// this machine; read only so that join, shell and leave can remove it.
	SSHD string `json:"sshd,omitempty"`
	Port int    `json:"port,omitempty"`
}

const (
	boxHisPort  = 2200 // old ferry: the box's way back into this machine (never forwarded now)
	boxSocks    = 1080 // in the box: 127.0.0.1:1080 → the filtered SOCKS proxy here
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
	noInternet := fs.Bool("no-internet", false, "never carry the box's web traffic through this machine")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: ferry join [--no-internet] <code> (or the name of a box you already paired with)")
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
	st := &boxState{Contact: name, User: *user, HisUser: me, HisHome: home, Mount: filepath.Join(home, name), NoInternet: *noInternet}
	if err := os.MkdirAll(boxDir(name), 0o700); err != nil {
		return err
	}
	if old, err := loadBox(name); err == nil {
		old.NoInternet = *noInternet
		removeOldWayBack(o, old)
	} else {
		removeOldWayBack(o, st) // a box dir without state: still sweep for leftovers
	}
	if err := st.save(); err != nil {
		return err
	}
	if err := writeSSHConfig(st); err != nil {
		return err
	}
	o.say("Added `Host %s` to ~/.ssh/config.", name)
	os.MkdirAll(st.Mount, 0o755)

	o.say("")
	o.say("%s", o.bold("Before you use it, know this:"))
	o.say("  The lender can see everything in the box, including whatever you bring into it.")
	o.say("  The box can't reach this machine. The only thing that comes back is its web traffic,")
	if st.NoInternet {
		o.say("  and you turned that off (--no-internet).")
	} else {
		o.say("  while ferry shell is open, to public internet addresses only (ferry join --no-internet: never).")
	}
	o.say("")
	o.result("Joined %s. Now: ferry shell %s   (ferry bring <folder> copies a project in)", name, name)
	o.event("joined", map[string]any{"name": name, "internet": !st.NoInternet})
	return nil
}

// removeOldWayBack removes what an older ferry join set up for the box to
// reach back into this machine: its private sshd (and the sessions on it),
// the box's key in ~/.ssh/authorized_keys, the reverse tunnels in the ssh
// config block, and the ssh sessions carrying them. It says so in one line.
func removeOldWayBack(o *out, st *boxState) {
	d := boxDir(st.Contact)
	did := false
	if st.SSHD == "user" || exists(filepath.Join(d, "sshd_config")) || len(procsMatching(filepath.Join(d, "sshd_config"))) > 0 {
		stopSSHD(st)
		did = true
	}
	if n, _ := unauthorize(st.Contact); n > 0 {
		did = true
	}
	for _, f := range []string{"sshd_config", "sshd.pid", "sshd.log", "sshd.lock", "host_key", "host_key.pub", "authorized_keys"} {
		if os.Remove(filepath.Join(d, f)) == nil {
			did = true
		}
	}
	if blockHasTunnels(st.Contact) {
		stopMaster(st.Contact) // its tunnels came from the old block
		stopPipes(st.Contact)  // and so did those of plain ssh sessions
		if mounted(st.Mount) {
			unmount(st.Mount) // the old mount showed the box's own home; shell mounts yours
		}
		if writeSSHConfig(st) == nil {
			did = true
		}
	}
	if st.SSHD != "" || st.Port != 0 {
		st.SSHD, st.Port = "", 0
		did = true
	}
	if did {
		st.save()
		o.say("Removed what an older ferry set up for %s to reach this machine (private sshd, its key, reverse tunnels).", st.Contact)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// blockHasTunnels says whether the box's ssh config block forwards anything
// from the box to this machine (as older ferry versions wrote it).
func blockHasTunnels(name string) bool {
	b, err := os.ReadFile(sshConfigPath())
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.Index(s, sshMarkOpen+name+"\n")
	if i < 0 {
		return false
	}
	j := strings.Index(s[i:], sshMarkEnd+name+"\n")
	if j < 0 {
		return false
	}
	blk := strings.ToLower(s[i : i+j])
	return strings.Contains(blk, "remoteforward") || strings.Contains(blk, "dynamicforward")
}

// validHisHome says whether a home path can be recreated in the box at the
// same place (lend makes /home and /Users writable for this).
func validHisHome(h string) bool {
	rest, ok := strings.CutPrefix(h, "/home/")
	if !ok {
		rest, ok = strings.CutPrefix(h, "/Users/")
	}
	if !ok || rest == "" || rest == "." || rest == ".." {
		return false
	}
	for _, r := range rest {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-') {
			return false
		}
	}
	return true
}

func cmdShell(ctx context.Context, o *out, args []string) error {
	fs := flag.NewFlagSet("shell", flag.ContinueOnError)
	noPorts := fs.Bool("no-ports", false, "don't share the box's ports to localhost here (ferry config box-ports off: never)")
	noInternet := fs.Bool("no-internet", false, "don't carry the box's web traffic this time (ferry config box-internet off: never)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: ferry shell [--no-ports] [--no-internet] <box>")
	}
	st, err := loadBox(pos[0])
	if err != nil {
		return fmt.Errorf("%q is not a box you joined (ferry join <code>)", pos[0])
	}
	removeOldWayBack(o, st)
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		return err
	}
	cfg, _ := session.LoadFileConfig()
	ports := !*noPorts && (cfg == nil || !cfg.NoBoxPorts)
	inet := !*noInternet && !st.NoInternet && (cfg == nil || !cfg.NoBoxInternet)
	if !ports && !inet {
		mountBox(o, st, nil)
		return syscall.Exec(ssh, []string{"ssh", "-t", st.Contact}, os.Environ())
	}
	return shellWithMaster(ctx, o, st, ssh, ports, inet)
}

// mountBox mounts the box's copy of your home (where ferry bring puts your
// folders) at ~/<box>, so your own editor works on them. This machine
// connects to the box for it; nothing in the box connects here.
func mountBox(o *out, st *boxState, c *sshConn) {
	sshfs, _ := exec.LookPath("sshfs")
	if sshfs == "" {
		o.say(o.dim("(install sshfs to see the box's files at " + st.Mount + ")"))
		return
	}
	if mounted(st.Mount) {
		return
	}
	os.MkdirAll(st.Mount, 0o755)
	remote := "" // the box's own home
	if validHisHome(st.HisHome) {
		if c != nil {
			c.Run("mkdir -p -- " + shq(st.HisHome))
		}
		remote = st.HisHome
	}
	try := func(r string) ([]byte, error) {
		return exec.Command(sshfs, st.Contact+":"+r, st.Mount, "-o", "reconnect,ServerAliveInterval=15,ServerAliveCountMax=3").CombinedOutput()
	}
	out, err := try(remote)
	if err != nil && remote != "" {
		out, err = try("")
	}
	if err != nil {
		o.say("note: could not mount the box's files at %s: %s", st.Mount, strings.TrimSpace(string(out)))
	}
}

// shellWithMaster logs in over the box's ssh master and, while the session
// lasts, shares the box's ports here and carries its web traffic. The last
// shell to close ends the master.
func shellWithMaster(ctx context.Context, o *out, st *boxState, ssh string, ports, inet bool) error {
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
		o.say("note: %v; logging in without port sharing or internet through this machine", err)
		lf.Close()
		mountBox(o, st, nil)
		return syscall.Exec(ssh, []string{"ssh", "-t", st.Contact}, os.Environ())
	}
	mountBox(o, st, &c)
	w := &boxWatch{st: st, c: c, ports: ports, inet: inet}
	w.start(o) // the first shell to a box serves it now; others take over when it closes
	// A closed terminal hangs up only the session leader, which is now this
	// process, not ssh: pass it on, then stay to clean up after it.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	cmd := exec.Command(ssh, "-t", "-S", strings.ReplaceAll(c.cm, "%", "%%"), "-o", "ControlMaster=no", "-o", "ClearAllForwardings=yes", st.Contact)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		w.stop()
		return err
	}
	wctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.run(wctx); close(done) }()
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
	w.stop()
	if syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		c.exit()
		os.Remove(portsFile(st.Contact))
	}
	lf.Close()
	if n, r := w.counts(); r > 0 {
		o.say("ferry: the box's internet through this machine: %d connections, %d refused (local or private addresses; see %s)", n, r, filepath.Join(boxDir(st.Contact), "proxy.log"))
	}
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
	stopMaster(name)    // ferry shell's connection, its shared ports, the proxy's tunnel, and the sessions on it
	stopPipes(name)     // your open ssh sessions to the box
	os.Remove(st.Mount) // only if empty
	stopSSHD(st)        // left by an older ferry, if any
	if n, _ := unauthorize(name); n > 0 {
		o.say("Removed the box's key from ~/.ssh/authorized_keys (an older ferry put it there).")
	}
	if err := removeSSHConfig(name); err == nil {
		o.say("Removed `Host %s` from ~/.ssh/config.", name)
	}
	if forgetHostKey(name) > 0 {
		o.say("Removed %s's host key from ~/.ssh/known_hosts.", name)
	}
	os.RemoveAll(boxDir(name))
	os.Remove(filepath.Dir(boxDir(name))) // only if no other box is left
	if cs, err := keys.LoadContacts(); err == nil {
		if _, ok := cs[name]; ok {
			delete(cs, name)
			cs.Save()
			o.say("Forgot the pairing with %s.", name)
		}
	}
	o.result("Left %s. Nothing of it is left on this machine (folders you took home stay yours).", name)
	o.event("left", map[string]any{"name": name})
	return nil
}

// ---- what older ferry versions set up, for removing it ----

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

// forgetHostKey drops the box's lines from ~/.ssh/known_hosts (stored under
// its HostKeyAlias, the box name, plain or hashed) and returns how many.
func forgetHostKey(name string) int {
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".ssh", "known_hosts")
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	var keep []string
	n := 0
	for _, l := range strings.SplitAfter(string(b), "\n") {
		if f := strings.Fields(l); len(f) > 1 && knownHostMatches(f[0], name) {
			n++
			continue
		}
		keep = append(keep, l)
	}
	if n > 0 {
		os.WriteFile(p, []byte(strings.Join(keep, "")), 0o600)
	}
	return n
}

// knownHostMatches says whether a known_hosts host field names host, plainly
// or as a hashed |1|salt|hash entry.
func knownHostMatches(field, host string) bool {
	for _, h := range strings.Split(field, ",") {
		if h == host {
			return true
		}
		if parts := strings.Split(h, "|"); len(parts) == 4 && parts[1] == "1" {
			salt, err1 := base64.StdEncoding.DecodeString(parts[2])
			want, err2 := base64.StdEncoding.DecodeString(parts[3])
			if err1 == nil && err2 == nil {
				m := hmac.New(sha1.New, salt)
				m.Write([]byte(host))
				if hmac.Equal(m.Sum(nil), want) {
					return true
				}
			}
		}
	}
	return false
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
	b.WriteString("    StrictHostKeyChecking accept-new\n    ServerAliveInterval 15\n")
	// Nothing of this machine goes to the box: no agent, no X11, no tunnels.
	// ferry shell adds the one forward it serves, the filtered proxy, itself.
	b.WriteString("    ForwardAgent no\n    ForwardX11 no\n    GSSAPIDelegateCredentials no\n    PermitLocalCommand no\n")
	// who you are here, so the box can put what you bring at the same path
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
	for _, c := range [][]string{{"fusermount3", "-u", "-z", dir}, {"fusermount", "-u", "-z", dir}, {"umount", dir}, {"diskutil", "unmount", "force", dir}} {
		if exec.Command(c[0], c[1:]...).Run() == nil {
			return
		}
	}
}
