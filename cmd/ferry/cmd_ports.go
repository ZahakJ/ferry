package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Port sharing: while `ferry shell <box>` runs, a server the friend starts
// in the box (npm run dev on :3000) shows up at http://localhost:3000 on his
// own machine, the way an editor's remote mode forwards ports.
//
// ferry shell keeps one ssh master connection per box in the background
// (ControlPath under the box dir); the interactive ssh and everything below
// ride it. Every 2 s it reads the box's /proc/net/tcp{,6}, and for each port
// the box user listens on (loopback or any address) it adds a local forward
// to 127.0.0.1 here with `ssh -O forward`; when the port closes, `-O cancel`.
// Only this direction is automatic, and only onto 127.0.0.1.

// boxPort is one shared port: Box in the box, Local on 127.0.0.1 here.
type boxPort struct {
	Box    int    `json:"box"`
	Local  int    `json:"local"`
	Target string `json:"target"` // the box address the forward connects to: 127.0.0.1 or ::1
}

func (p boxPort) spec() string {
	t := p.Target
	if strings.Contains(t, ":") {
		t = "[" + t + "]"
	}
	return fmt.Sprintf("127.0.0.1:%d:%s:%d", p.Local, t, p.Box)
}

const (
	pollEvery   = 2 * time.Second
	pollCommand = "id -u; cat /proc/net/tcp /proc/net/tcp6 2>/dev/null"
)

// shareable says whether a box port is worth forwarding: not privileged, not
// the box's own plumbing (its sshd, and the two tunnels back to this
// machine), and not in Linux's ephemeral range, where programs put internal
// listeners on random ports (debuggers, language servers).
func shareable(p int) bool {
	return p >= 1024 && p < 32768 && p != 2222 && p != boxSocks && p != boxHisPort
}

// parseListeners reads `id -u` then /proc/net/tcp and /proc/net/tcp6 as
// printed by pollCommand, and returns the shareable ports uid listens on at
// a loopback or wildcard address, each with the address to connect to.
func parseListeners(out string) (map[int]string, error) {
	lines := strings.Split(out, "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i == len(lines) {
		return nil, errors.New("empty reply from the box")
	}
	uid, err := strconv.Atoi(strings.TrimSpace(lines[i]))
	if err != nil {
		return nil, fmt.Errorf("unexpected reply from the box: %q", lines[i])
	}
	got := map[int]string{}
	for _, l := range lines[i+1:] {
		f := strings.Fields(l)
		if len(f) < 8 || f[0] == "sl" || f[3] != "0A" { // 0A: LISTEN
			continue
		}
		if u, err := strconv.Atoi(f[7]); err != nil || u != uid {
			continue
		}
		hexIP, hexPort, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		port64, err := strconv.ParseUint(hexPort, 16, 16)
		port := int(port64)
		if err != nil || !shareable(port) {
			continue
		}
		ip := procIP(hexIP)
		if ip == nil || !(ip.IsLoopback() || ip.IsUnspecified()) {
			continue
		}
		// Reach it the way it listens: ::1 only if that is all there is.
		target := "127.0.0.1"
		if ip.Equal(net.IPv6loopback) {
			target = "::1"
		}
		if prev, seen := got[port]; !seen || prev == "::1" {
			got[port] = target
		}
	}
	return got, nil
}

// procIP decodes an address from /proc/net/tcp*: 32-bit words in host
// (little-endian) order.
func procIP(s string) net.IP {
	b, err := hex.DecodeString(s)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return nil
	}
	for w := 0; w < len(b); w += 4 {
		b[w], b[w+1], b[w+2], b[w+3] = b[w+3], b[w+2], b[w+1], b[w]
	}
	return net.IP(b)
}

// boxConn is the ssh master connection to a box.
type boxConn interface {
	Run(cmd string) ([]byte, error) // a command in the box
	Control(op, spec string) error  // ssh -O forward|cancel -L spec
}

// forwarder keeps the local forwards in step with the box's listeners.
type forwarder struct {
	conn   boxConn
	free   func(port int) bool // can this machine take 127.0.0.1:port?
	notify func(p boxPort)     // a port was just shared
	save   func(ps []boxPort)
	fwd    map[int]boxPort // by box port
	failed map[int]bool    // box ports that could not be forwarded; retried once they close and reopen
}

func newForwarder(c boxConn) *forwarder {
	return &forwarder{conn: c, free: localFree, notify: func(boxPort) {}, save: func([]boxPort) {},
		fwd: map[int]boxPort{}, failed: map[int]bool{}}
}

func (f *forwarder) list() []boxPort {
	ps := make([]boxPort, 0, len(f.fwd))
	for _, p := range f.fwd {
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Box < ps[j].Box })
	return ps
}

// tick polls the box once and applies the difference.
func (f *forwarder) tick() error {
	out, err := f.conn.Run(pollCommand)
	if err != nil {
		return err
	}
	want, err := parseListeners(string(out))
	if err != nil {
		return err
	}
	f.apply(want)
	return nil
}

// apply cancels forwards for ports that closed and adds them for new ones.
// A box port whose number is taken here goes to the next free one above it.
func (f *forwarder) apply(want map[int]string) (added, gone []boxPort) {
	for bp, p := range f.fwd {
		if _, ok := want[bp]; !ok {
			f.conn.Control("cancel", p.spec())
			delete(f.fwd, bp)
			gone = append(gone, p)
		}
	}
	for bp := range f.failed {
		if _, ok := want[bp]; !ok {
			delete(f.failed, bp)
		}
	}
	ports := make([]int, 0, len(want))
	for bp := range want {
		ports = append(ports, bp)
	}
	sort.Ints(ports)
	for _, bp := range ports {
		if _, ok := f.fwd[bp]; ok || f.failed[bp] {
			continue
		}
		done := false
		for l := bp; l < bp+20 && l <= 65535; l++ {
			if !f.free(l) {
				continue
			}
			p := boxPort{Box: bp, Local: l, Target: want[bp]}
			if f.conn.Control("forward", p.spec()) == nil {
				f.fwd[bp] = p
				added = append(added, p)
				f.notify(p)
				done = true
				break
			}
		}
		if !done {
			f.failed[bp] = true
		}
	}
	if len(added)+len(gone) > 0 {
		f.save(f.list())
	}
	return added, gone
}

// localFree says whether 127.0.0.1:port is free here, and [::1]:port too,
// so "localhost" cannot land on something else of his that listens on ::1.
func localFree(port int) bool {
	l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	l.Close()
	l6, err := net.Listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
	if err != nil {
		return !errors.Is(err, syscall.EADDRINUSE) // no IPv6 here: nothing to collide with
	}
	l6.Close()
	return true
}

// ---- the ssh master ----

// controlPath is where the box's ssh master listens, short enough for a
// unix socket (and for the random suffix ssh adds while creating it).
func controlPath(name string) string {
	p := filepath.Join(boxDir(name), "cm")
	if len(p) > 80 {
		p = filepath.Join(os.TempDir(), fmt.Sprintf("ferry-%d-%x", os.Getuid(), shortHash(boxDir(name))))
	}
	return p
}

func shortHash(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h = (h ^ uint32(s[i])) * 16777619
	}
	return h
}

// sshConn talks to the box through its master.
type sshConn struct{ name, cm string }

// base: no config files (so no forwards from them sneak into a -O request,
// where `cancel` would take the box's tunnels with it), only the master.
func (c sshConn) base(extra ...string) []string {
	a := []string{"-F", "none", "-S", strings.ReplaceAll(c.cm, "%", "%%"), "-o", "ControlMaster=no", "-o", "BatchMode=yes"}
	return append(a, extra...)
}

func (c sshConn) Run(cmd string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "ssh", append(c.base(c.name), cmd)...).Output()
}

func (c sshConn) Control(op, spec string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", c.base("-O", op, "-L", spec, c.name)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (c sshConn) alive() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "ssh", c.base("-O", "check", c.name)...).Run() == nil
}

func (c sshConn) exit() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exec.CommandContext(ctx, "ssh", c.base("-O", "exit", c.name)...).Run()
}

// ensureMaster starts the box's ssh master unless one is up. It carries the
// ssh config's tunnels (the box's way back here), so it is the connection the
// box reaches this machine through while any ferry shell is open.
func ensureMaster(st *boxState) (sshConn, error) {
	c := sshConn{name: st.Contact, cm: controlPath(st.Contact)}
	d := boxDir(st.Contact)
	lf, err := os.OpenFile(filepath.Join(d, "master.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return c, err
	}
	defer lf.Close()
	syscall.Flock(int(lf.Fd()), syscall.LOCK_EX) // two shells at once start one master
	if c.alive() {
		return c, nil
	}
	os.Remove(c.cm)                           // a stale socket would make ssh skip multiplexing
	os.Remove(filepath.Join(d, "ports.json")) // forwards died with the old master
	log, err := os.Create(filepath.Join(d, "ssh.log"))
	if err != nil {
		return c, err
	}
	defer log.Close()
	// -f backgrounds it after login (a passphrase prompt still reaches the
	// terminal). ControlPersist ends it 15 s after its last session, in case
	// this process dies before it can say so.
	cmd := exec.Command("ssh", "-M", "-S", strings.ReplaceAll(c.cm, "%", "%%"), "-o", "ControlPersist=15s", "-N", "-f", st.Contact)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, log, log
	if err := cmd.Run(); err != nil {
		b, _ := os.ReadFile(filepath.Join(d, "ssh.log"))
		return c, fmt.Errorf("ssh: %s", strings.TrimSpace(string(b)))
	}
	for i := 0; i < 50 && !c.alive(); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if !c.alive() {
		return c, errors.New("the ssh master did not come up")
	}
	return c, nil
}

// stopMaster ends the box's ssh master and every session riding it.
func stopMaster(name string) {
	c := sshConn{name: name, cm: controlPath(name)}
	c.exit()
	killTree(procsMatching("\x00" + c.cm + "\x00"))
	os.Remove(c.cm)
}

// ---- the watcher ----

func portsFile(name string) string { return filepath.Join(boxDir(name), "ports.json") }

func loadPorts(name string) []boxPort {
	var ps []boxPort
	if b, err := os.ReadFile(portsFile(name)); err == nil {
		json.Unmarshal(b, &ps)
	}
	return ps
}

func savePorts(name string, ps []boxPort) {
	b, _ := json.MarshalIndent(ps, "", "  ")
	tmp := portsFile(name) + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil { // fails once leave removed the dir: fine
		os.Rename(tmp, portsFile(name))
	}
}

// watchPorts shares the box's ports until ctx ends. With several shells open
// to one box, one of them watches (ports.lock); another takes over, from
// ports.json, when it closes.
func watchPorts(ctx context.Context, st *boxState, c sshConn) {
	f := newForwarder(c)
	f.save = func(ps []boxPort) { savePorts(st.Contact, ps) }
	f.notify = func(p boxPort) {
		msg := fmt.Sprintf("ferry: box port %d is http://localhost:%d on your machine", p.Box, p.Local)
		if p.Local != p.Box {
			msg += fmt.Sprintf(" (%d was taken there)", p.Box)
		}
		// a status-line message in the box's tmux: nothing is written over the screen
		c.Run("tmux display-message -d 6000 '" + msg + "' 2>/dev/null; true")
	}
	var lock *os.File
	defer func() {
		if lock != nil {
			lock.Close()
		}
	}()
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		if lock == nil {
			if lf, err := os.OpenFile(filepath.Join(boxDir(st.Contact), "ports.lock"), os.O_CREATE|os.O_RDWR, 0o600); err == nil {
				if syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
					lock = lf
					for _, p := range loadPorts(st.Contact) {
						f.fwd[p.Box] = p
					}
				} else {
					lf.Close()
				}
			}
		}
		if lock != nil && ctx.Err() == nil {
			f.tick() // a failed poll (box busy, link hiccup) just waits for the next
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---- ferry ports ----

func cmdPorts(ctx context.Context, o *out, args []string) error {
	pos, err := parse(flag.NewFlagSet("ports", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: ferry ports <box>")
	}
	name := pos[0]
	if _, err := loadBox(name); err != nil {
		return fmt.Errorf("%q is not a box you joined", name)
	}
	ps := loadPorts(name)
	if !(sshConn{name: name, cm: controlPath(name)}).alive() {
		ps = nil
	}
	o.event("ports", map[string]any{"box": name, "ports": ps})
	if len(ps) == 0 {
		o.say("No ports shared from %s right now. While ferry shell %s is open, ports it listens on show up here.", name, name)
		return nil
	}
	for _, p := range ps {
		o.result("box :%-5d  →  http://localhost:%d", p.Box, p.Local)
	}
	return nil
}
