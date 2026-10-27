package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ZahakJ/ferry/internal/agent"
	"github.com/ZahakJ/ferry/internal/keys"
	"github.com/ZahakJ/ferry/internal/pair"
	"github.com/ZahakJ/ferry/internal/session"
	"golang.org/x/term"
)

func cmdMine(ctx context.Context, o *out, args []string) error {
	if flagHook != nil {
		_, err := parse(flag.NewFlagSet("mine", flag.ContinueOnError), args)
		return err
	}
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "", "status", "ls":
		return mineStatusCmd(ctx, o, args, false)
	case "res":
		return mineStatusCmd(ctx, o, args, true)
	case "add":
		return mineLink(ctx, o, args, false)
	case "join":
		return mineLink(ctx, o, args, true)
	case "rm", "remove":
		return mineRm(ctx, o, args)
	case "sync":
		return mineSyncCmd(ctx, o, args)
	case "ports":
		return minePorts(ctx, o, args)
	case "fwd":
		return mineFwdCmd(ctx, o, args)
	case "fallback":
		return mineFallback(o, args)
	case "service":
		return mineService(o, args)
	case "help":
		return cmdHelp(ctx, o, []string{"mine"})
	}
	return usagef("ferry mine: %q is not a subcommand; see ferry help mine", sub)
}

// ---- add / join ----

func mineLink(ctx context.Context, o *out, args []string, joining bool) error {
	fs := flag.NewFlagSet("mine", flag.ContinueOnError)
	name := fs.String("name", "", "this device's name (default: its host name)")
	system := fs.Bool("system-sshd", false, "let your devices in through the system sshd (port 22) instead of ferry's private one")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if joining != (len(pos) > 0) {
		if joining {
			return usagef("usage: ferry mine join <code>   (the code ferry mine add printed on the other device)")
		}
		return usagef("usage: ferry mine add   (then on the other device: ferry mine join <code>)")
	}
	id, _, err := keys.LoadOrCreate()
	if err != nil {
		return err
	}
	want := *name
	if want == "" && id.Name == keys.DefaultName() {
		want = deviceName() // every device of one user would otherwise be called after the user
	}
	if want != "" && want != id.Name {
		if err := keys.CheckName(want); err != nil {
			return usagef("%v", err)
		}
		id.Name = want
		if err := keys.Save(id); err != nil {
			return err
		}
		o.say("This device is called %s %s.", o.bold(id.Name), o.dim("(ferry config name <name> changes it)"))
	}
	dev, _, err := setupMineLocal(*system)
	if err != nil {
		return err
	}
	cfg, err := session.LoadConfig()
	if err != nil {
		return err
	}
	var s *session.Session
	if !joining {
		s, err = session.Offer(ctx, cfg, session.PurposeMine, func(c pair.Code) {
			o.event("code", map[string]any{"code": c.String()})
			if o.json {
				return
			}
			o.say("On your other device, run:")
			o.say("")
			o.result("    ferry mine join %s", c)
			o.say("")
			o.say(o.dim("It links the two as one owner's: each gets full access to the other. Works once. Waiting…"))
		})
	} else {
		code, perr := pair.Parse(strings.Join(pos, "-"))
		if perr != nil {
			return usagef("%v", perr)
		}
		s, err = session.Join(ctx, cfg, session.PurposeMine, code)
	}
	if err != nil {
		return err
	}
	defer s.Close()
	peer, err := session.Introduce(s, id)
	if err != nil {
		return err
	}
	o.event("check", map[string]any{"words": s.SAS, "peer": peer.Name})
	o.say("")
	o.say("Both screens should show the same two words: %s", o.bold(s.SAS))
	ok := confirm(o, "Do they match on the other device? [y/N] ")
	mine := mineHello{OK: ok}
	if ok {
		mine.Device, mine.Roster = dev, roster(peer.Sign)
	}
	errc := make(chan error, 1)
	go func() { errc <- s.Conn.SendJSON(mine) }()
	var theirs mineHello
	rerr := s.Conn.RecvJSON(&theirs)
	if err := <-errc; err != nil {
		return err
	}
	if rerr != nil {
		return rerr
	}
	switch {
	case !ok:
		return fmt.Errorf("%w: you said the words differ, so nothing was linked. If they really differ, someone may be in the middle: make a new code", errDeclined)
	case !theirs.OK:
		return fmt.Errorf("%w: the other device said the words differ, so nothing was linked", errDeclined)
	}
	if err := validDevice(theirs.Device); err != nil {
		return err
	}
	unlock := mineLock()
	before := roster(peer.Sign)
	members := append([]mineMember{{Name: peer.Name, Key: peer, Device: *theirs.Device}}, theirs.Roster...)
	names, err := recordMine(members, id.Public())
	if err == nil {
		// Each side introduces the other side's devices to its own older
		// ones, so the two meshes become one. Owed until they are online.
		if st, e := loadMineState(); e == nil {
			newKeys := map[string]bool{}
			for _, m := range members {
				newKeys[string(m.Key.Sign)] = true
			}
			var intro []mineMember
			for _, m := range members {
				if string(m.Key.Sign) != string(id.Public().Sign) {
					intro = append(intro, m)
				}
			}
			for _, old := range before {
				if !newKeys[string(old.Key.Sign)] {
					queueOp(st, keys.Contact{Name: old.Name, Key: old.Key}, "mine-hello", helloData{Members: intro})
				}
			}
			st.save()
		}
		err = mineConverge(nil)
	}
	unlock()
	if err != nil {
		return err
	}
	cs, _ := keys.LoadContacts()
	pn := peer.Name
	if c, ok := cs.ByKey(peer.Sign); ok {
		pn = c.Name
	}
	o.say("%s Linked %s as one of your devices (check words %s).", o.green("✓"), o.bold(pn), s.SAS)
	if len(names) > 1 {
		o.say("  Your devices here now: %s", strings.Join(allMineNames(), ", "))
	}
	if shim, err := installShim(); err != nil {
		o.say("  note: %v", err)
	} else if !onPath(filepath.Dir(shim)) {
		o.say("  note: %s is not on your PATH; add it, or use `ferry on`", tildePath(filepath.Dir(shim)))
	}
	kickOrDeliver(ctx, o)
	if !agent.Running() {
		o.say("")
		o.say("%s ferry up must run on each device for the others to reach it: %s", o.bold("Next:"), "ferry mine service")
	}
	o.say("")
	o.result("Now: on %s uname -a    ls ~/m/%s    ferry mine", pn, pn)
	o.event("linked", map[string]any{"name": pn, "check": s.SAS, "devices": allMineNames()})
	return nil
}

type mineHello struct {
	OK     bool         `json:"ok"`
	Device *keys.Device `json:"device,omitempty"`
	Roster []mineMember `json:"roster,omitempty"`
}

func allMineNames() []string {
	ds, _ := mineDevices()
	var n []string
	for _, d := range ds {
		n = append(n, d.Name)
	}
	return n
}

// confirm asks a yes/no question on the terminal (or reads the answer from
// stdin when it is not one).
func confirm(o *out, q string) bool {
	fmt.Fprint(os.Stderr, q)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(os.Stderr, a)
	}
	return a == "y" || a == "yes"
}

// ---- rm ----

func mineRm(ctx context.Context, o *out, args []string) error {
	pos, err := parse(flag.NewFlagSet("mine", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: ferry mine rm <device>")
	}
	d, err := mineDevice(pos[0])
	if err != nil {
		return err
	}
	others := roster(d.Key.Sign)
	// Say exactly what goes, before it goes.
	o.say("Removing %s from your devices. This goes:", o.bold(d.Name))
	o.say("  - the link with %s (contact %s, id %s)", d.Name, d.Name, d.Key.ID())
	o.say("  - its keys from %s and %s", tildePath(minePath("authorized_keys")), tildePath(minePath("known_hosts")))
	o.say("  - `Host %s` from ~/.ssh/config, and the ssh connection to it", d.Name)
	if mounted(mineMount(d.Name)) || exists(mineMount(d.Name)) {
		o.say("  - the mount %s (and the folder, if empty)", tildePath(mineMount(d.Name)))
	}
	if st, err := loadMineState(); err == nil {
		for _, f := range st.Fwds {
			if f.Dev == d.Name {
				o.say("  - the forward localhost:%d → %s:%d", f.Local, d.Name, f.Port)
			}
		}
	}
	o.say("  - on %s: everything it has for your devices (it is told to leave the mesh)", d.Name)
	if len(others) > 0 {
		var n []string
		for _, m := range others {
			n = append(n, m.Name)
		}
		o.say("  - on %s: %s (they are told to forget it)", strings.Join(n, ", "), d.Name)
	}
	o.say("  Claude sessions already copied from %s stay; they are yours.", d.Name)
	unlock := mineLock()
	st, err := loadMineState()
	if err != nil {
		st = &mineState{}
	}
	queueOp(st, d, "mine-leave", nil)
	for _, m := range others {
		queueOp(st, keys.Contact{Name: m.Name, Key: m.Key}, "mine-forget", forgetData{Key: d.Key})
	}
	st.save()
	_, err = forgetMine(func(c keys.Contact) bool { return c.Name == d.Name })
	unlock()
	if err != nil {
		return err
	}
	kickOrDeliver(ctx, o)
	o.result("Removed %s.", d.Name)
	o.event("removed", map[string]any{"name": d.Name})
	return nil
}

// ---- status / res ----

func mineStatusCmd(ctx context.Context, o *out, args []string, res bool) error {
	if _, err := parse(flag.NewFlagSet("mine", flag.ContinueOnError), args); err != nil {
		return err
	}
	ds, err := mineDevices()
	if err != nil {
		return err
	}
	this := localStats()
	this.Mount = ""
	all := []devStatus{this}
	live := ""
	if s, err := loadStatus(); err == nil && agent.Running() && time.Since(s.At) < 45*time.Second {
		all = append(all, s.Devices...)
	} else if len(ds) > 0 {
		live = "ferry up is not running here, so this is a live check"
		all = append(all, probeAll(ctx, ds)...)
	}
	if res {
		return printRes(o, all)
	}
	o.event("mine", map[string]any{"devices": all})
	if o.json {
		return nil
	}
	if len(ds) == 0 {
		o.say("No other devices linked. On this one: ferry mine add; on the other: ferry mine join <code>.")
	}
	for _, d := range all {
		dot, state := o.green("●"), "online"
		switch {
		case d.This:
			state = "this device"
		case !d.Online:
			dot, state = o.dim("○"), "offline"
		case d.Path != "direct":
			dot, state = o.red("●"), "online, no direct path"
		}
		line := fmt.Sprintf("%s %-14s %-13s", dot, d.Name, state)
		if d.CPUs > 0 {
			line += fmt.Sprintf(" %-6s %3d CPU  %s RAM (%s free)  load %.1f  GPU %s", d.OS, d.CPUs, gb(d.MemTotal), gb(d.MemFree), d.Load, gpuText(d.GPUs))
		}
		if d.Mounted {
			line += "  " + tildePath(d.Mount)
		}
		o.result("%s", line)
		if d.Note != "" {
			o.say("    %s", o.dim(d.Note))
		}
	}
	if live != "" {
		o.say("%s", o.dim(live))
	}
	return nil
}

func gb(b int64) string { return fmt.Sprintf("%.1f GB", float64(b)/1e9) }

func gpuText(gs []gpuInfo) string {
	if len(gs) == 0 {
		return "none"
	}
	var s []string
	for _, g := range gs {
		s = append(s, fmt.Sprintf("%s %d GB", g.Name, (g.MemMB+512)/1024))
	}
	return strings.Join(s, "; ")
}

func printRes(o *out, all []devStatus) error {
	var cpus int
	var mt, mf int64
	var gpus []string
	var names []string
	for _, d := range all {
		if !d.This && (!d.Online || d.Path != "direct") {
			continue
		}
		names = append(names, d.Name)
		cpus += d.CPUs
		mt += d.MemTotal
		mf += d.MemFree
		for _, g := range d.GPUs {
			gpus = append(gpus, d.Name+": "+g.Name)
		}
	}
	o.event("res", map[string]any{"devices": names, "cpus": cpus, "mem_total": mt, "mem_free": mf, "gpus": gpus})
	for _, d := range all {
		if d.This || (d.Online && d.Path == "direct") {
			o.result("%-14s %3d CPU  %9s RAM (%s free)  GPU %s", d.Name, d.CPUs, gb(d.MemTotal), gb(d.MemFree), gpuText(d.GPUs))
		}
	}
	gp := "none"
	if len(gpus) > 0 {
		gp = strings.Join(gpus, "; ")
	}
	o.result("%-14s %3d CPU  %9s RAM (%s free)  GPU %s", o.bold("together"), cpus, gb(mt), gb(mf), gp)
	return nil
}

// probeAll checks each device now, from this process.
func probeAll(ctx context.Context, ds []keys.Contact) []devStatus {
	out := make([]devStatus, len(ds))
	var wg sync.WaitGroup
	for i, d := range ds {
		wg.Add(1)
		go func(i int, d keys.Contact) {
			defer wg.Done()
			s := devStatus{Name: d.Name, Path: "none", Mount: mineMount(d.Name)}
			b, err := mineRun(ctx, d.Name, statsScript, "", 45*time.Second)
			if err == nil && parseStats(string(b), &s) == nil {
				s.Online, s.Path = true, "direct"
			}
			s.Mounted = mounted(s.Mount)
			out[i] = s
		}(i, d)
	}
	wg.Wait()
	return out
}

// ---- sync ----

func mineSyncCmd(ctx context.Context, o *out, args []string) error {
	pos, err := parse(flag.NewFlagSet("mine", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	ds, err := mineDevices()
	if err != nil {
		return err
	}
	id, _ := keys.Load()
	unlock, err := syncLock()
	if err != nil {
		return err
	}
	defer unlock()
	var total syncResult
	for _, d := range ds {
		if len(pos) > 0 && pos[0] != d.Name {
			continue
		}
		r, err := syncSessions(ctx, sshPeer{d.Name}, d.Name, id.Name, d.Device.Home, mineHome(), claudeProjects(), nil)
		if err != nil {
			o.say("%s %s: %v", o.red("✗"), d.Name, err)
			o.event("sync", map[string]any{"device": d.Name, "error": err.Error()})
			continue
		}
		total.New += r.New
		total.Extended += r.Extended
		total.Conflicts += r.Conflicts
		o.result("%s %-14s %d new, %d grown, %d diverged (kept both)", o.green("✓"), d.Name, r.New, r.Extended, r.Conflicts)
		o.event("sync", map[string]any{"device": d.Name, "new": r.New, "extended": r.Extended, "kept_both": r.Conflicts})
	}
	return nil
}

// ---- ports / fwd ----

const portsScript = `if [ -r /proc/net/tcp ]; then echo proc; id -u; cat /proc/net/tcp /proc/net/tcp6 2>/dev/null
else echo lsof; lsof -nP -iTCP -sTCP:LISTEN -a -u "$(id -u)" 2>/dev/null; fi; exit 0`

type listener struct {
	Port    int    `json:"port"`
	Addr    string `json:"addr"`
	Command string `json:"command,omitempty"`
}

// parsePorts reads portsScript's output: the TCP ports the device's user
// listens on.
func parsePorts(out string) []listener {
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		return nil
	}
	seen := map[int]bool{}
	var ls []listener
	add := func(l listener) {
		if !seen[l.Port] {
			seen[l.Port] = true
			ls = append(ls, l)
		}
	}
	switch strings.TrimSpace(lines[0]) {
	case "proc":
		uid := strings.TrimSpace(lines[1])
		for _, l := range lines[2:] {
			f := strings.Fields(l)
			if len(f) < 8 || f[0] == "sl" || f[3] != "0A" || f[7] != uid {
				continue
			}
			hexIP, hexPort, ok := strings.Cut(f[1], ":")
			p, err := strconv.ParseUint(hexPort, 16, 16)
			ip := procIP(hexIP)
			if !ok || err != nil || ip == nil {
				continue
			}
			add(listener{Port: int(p), Addr: ip.String()})
		}
	case "lsof":
		for _, l := range lines[1:] {
			f := strings.Fields(l)
			if len(f) < 9 || f[0] == "COMMAND" || f[len(f)-1] != "(LISTEN)" {
				continue
			}
			n := f[len(f)-2]
			i := strings.LastIndexByte(n, ':')
			p, err := strconv.Atoi(n[i+1:])
			if i < 0 || err != nil {
				continue
			}
			add(listener{Port: p, Addr: strings.Trim(n[:i], "[]"), Command: f[0]})
		}
	}
	sort.Slice(ls, func(i, j int) bool { return ls[i].Port < ls[j].Port })
	return ls
}

func minePorts(ctx context.Context, o *out, args []string) error {
	pos, err := parse(flag.NewFlagSet("mine", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: ferry mine ports <device>")
	}
	d, err := mineDevice(pos[0])
	if err != nil {
		return err
	}
	b, err := mineRun(ctx, d.Name, portsScript, "", 45*time.Second)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", errUnreachable, d.Name, err)
	}
	ls := parsePorts(string(b))
	st, _ := loadMineState()
	fwd := map[int]int{}
	if st != nil {
		for _, f := range st.Fwds {
			if f.Dev == d.Name {
				fwd[f.Port] = f.Local
			}
		}
	}
	o.event("ports", map[string]any{"device": d.Name, "ports": ls, "forwarded": fwd})
	if len(ls) == 0 {
		o.say("Nothing of yours listens on TCP on %s.", d.Name)
		return nil
	}
	for _, l := range ls {
		extra := ""
		if lp, ok := fwd[l.Port]; ok {
			extra = fmt.Sprintf("  → localhost:%d here", lp)
		}
		o.result(":%-6d %-16s %s%s", l.Port, l.Addr, l.Command, extra)
	}
	o.say("%s", o.dim(fmt.Sprintf("ferry mine fwd %s <port> puts one on localhost here.", d.Name)))
	return nil
}

func mineForward(dev string, port, local int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	spec := fmt.Sprintf("127.0.0.1:%d:localhost:%d", local, port)
	out, err := exec.CommandContext(ctx, "ssh", "-F", "none", "-S", mineControlPath(dev), "-O", "forward", "-L", spec, dev).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", lastLine(string(out)))
	}
	return nil
}

func mineUnforward(dev string, port, local int) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	exec.CommandContext(ctx, "ssh", "-F", "none", "-S", mineControlPath(dev), "-O", "cancel", "-L", fmt.Sprintf("127.0.0.1:%d:localhost:%d", local, port), dev).Run()
}

func mineFwdCmd(ctx context.Context, o *out, args []string) error {
	fs := flag.NewFlagSet("mine", flag.ContinueOnError)
	stop := fs.Bool("stop", false, "stop forwarding that port")
	local := fs.Int("local", 0, "the port on 127.0.0.1 here (default: the same, or the next free one)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	unlock := mineLock()
	st, serr := loadMineState()
	unlock()
	if serr != nil {
		st = &mineState{}
	}
	if len(pos) == 0 {
		o.event("fwds", map[string]any{"fwds": st.Fwds})
		if len(st.Fwds) == 0 {
			o.say("No ports forwarded. ferry mine fwd <device> <port>")
		}
		for _, f := range st.Fwds {
			o.result("localhost:%-6d → %s:%d", f.Local, f.Dev, f.Port)
		}
		return nil
	}
	if len(pos) != 2 {
		return usagef("usage: ferry mine fwd [--stop] <device> <port>")
	}
	d, err := mineDevice(pos[0])
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(pos[1])
	if err != nil || port < 1 || port > 65535 {
		return usagef("%q is not a port", pos[1])
	}
	unlock = mineLock()
	defer unlock()
	st, err = loadMineState()
	if err != nil {
		return err
	}
	var keep []mineFwd
	var had *mineFwd
	for _, f := range st.Fwds {
		if f.Dev == d.Name && f.Port == port {
			f := f
			had = &f
			continue
		}
		keep = append(keep, f)
	}
	if *stop {
		if had == nil {
			return fmt.Errorf("%s:%d is not forwarded", d.Name, port)
		}
		mineUnforward(d.Name, had.Port, had.Local)
		st.Fwds = keep
		o.result("Stopped localhost:%d → %s:%d.", had.Local, d.Name, port)
		o.event("unforwarded", map[string]any{"device": d.Name, "port": port, "local": had.Local})
		return st.save()
	}
	if had != nil {
		o.result("Already forwarded: http://localhost:%d → %s:%d", had.Local, d.Name, port)
		return nil
	}
	if _, err := ensureMineMaster(ctx, d.Name); err != nil {
		return fmt.Errorf("%w: %s: %v", errUnreachable, d.Name, err)
	}
	lp := *local
	if lp == 0 {
		for l := port; l < port+20 && l <= 65535; l++ {
			if localFree(l) {
				lp = l
				break
			}
		}
		if lp == 0 {
			return fmt.Errorf("ports %d-%d are all taken here; pick one with --local", port, port+19)
		}
	}
	if err := mineForward(d.Name, port, lp); err != nil {
		return err
	}
	st.Fwds = append(keep, mineFwd{Dev: d.Name, Port: port, Local: lp})
	if err := st.save(); err != nil {
		return err
	}
	o.result("http://localhost:%d → %s:%d", lp, d.Name, port)
	o.say("%s", o.dim("127.0.0.1 only; it comes back by itself after a reconnect while ferry up runs. ferry mine fwd --stop "+d.Name+" "+pos[1]+" ends it."))
	o.event("forwarded", map[string]any{"device": d.Name, "port": port, "local": lp})
	return nil
}

// ---- fallback ----

const (
	fbMarkOpen = "# >>> ferry mine "
	fbMarkEnd  = "# <<< ferry mine "
)

// fallbackRCs are the shell startup files the fallback is sourced from:
// those that exist, and when creating, the one for the login shell.
func fallbackRCs(creating bool) []string {
	h := mineHome()
	var out []string
	for _, rc := range []string{".bashrc", ".zshrc"} {
		p := filepath.Join(h, rc)
		if exists(p) || (creating && strings.HasSuffix(os.Getenv("SHELL"), strings.TrimPrefix(strings.TrimSuffix(rc, "rc"), "."))) {
			out = append(out, p)
		}
	}
	return out
}

func mineFallback(o *out, args []string) error {
	pos, err := parse(flag.NewFlagSet("mine", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || (pos[0] != "on" && pos[0] != "off") {
		return usagef("usage: ferry mine fallback on|off")
	}
	unlock := mineLock()
	defer unlock()
	st, err := loadMineState()
	if err != nil {
		return errors.New("no devices linked yet (ferry mine add)")
	}
	if pos[0] == "off" {
		for _, rc := range fallbackRCs(false) {
			if removeMarked(rc, fbMarkOpen, fbMarkEnd, "fallback") {
				o.say("Removed it from %s.", tildePath(rc))
			}
		}
		os.Remove(minePath("fallback.sh"))
		st.Fallback = false
		o.result("Command fallback is off. Open shells keep it until they restart.")
		o.event("fallback", map[string]any{"on": false})
		return st.save()
	}
	exe, _ := os.Executable()
	env := ""
	if h := os.Getenv("FERRY_HOME"); h != "" {
		env = "FERRY_HOME=" + shq(h) + " "
	}
	script := fmt.Sprintf(`# ferry mine fallback: a command missing here runs on the first of your
# devices that has it. ferry mine fallback off removes this.
case $- in *i*) ;; *) return 0 2>/dev/null ;; esac
command_not_found_handle() { env %s%s on --fallback "$@"; }
command_not_found_handler() { env %s%s on --fallback "$@"; }
`, env, shq(exe), env, shq(exe))
	if err := writeAtomic(minePath("fallback.sh"), []byte(script), 0o644); err != nil {
		return err
	}
	line := fmt.Sprintf("[ -f %s ] && . %s\n", shq(minePath("fallback.sh")), shq(minePath("fallback.sh")))
	rcs := fallbackRCs(true)
	if len(rcs) == 0 {
		rcs = []string{filepath.Join(mineHome(), ".bashrc")}
	}
	for _, rc := range rcs {
		old, _ := os.ReadFile(rc)
		rest, _ := stripMarked(string(old), fbMarkOpen, fbMarkEnd, "fallback")
		if rest != "" && !strings.HasSuffix(rest, "\n") {
			rest += "\n"
		}
		// last in the file, so it wins over a distribution's handler
		if err := os.WriteFile(rc, []byte(rest+fbMarkOpen+"fallback\n"+line+fbMarkEnd+"fallback\n"), 0o644); err != nil {
			return err
		}
		o.say("Added two marked lines to %s.", tildePath(rc))
	}
	st.Fallback = true
	o.result("Command fallback is on in new shells: a command missing here runs on the first reachable device that has it.")
	o.event("fallback", map[string]any{"on": true, "rc": rcs})
	return st.save()
}

// ---- service ----

func mineService(o *out, args []string) error {
	fs := flag.NewFlagSet("mine", flag.ContinueOnError)
	remove := fs.Bool("remove", false, "uninstall it")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	exe, _ := os.Executable()
	home := mineHome()
	run := func(name string, a ...string) error {
		out, err := exec.Command(name, a...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %s", name, strings.Join(a, " "), strings.TrimSpace(string(out)))
		}
		return nil
	}
	switch runtime.GOOS {
	case "linux":
		p := filepath.Join(home, ".config", "systemd", "user", "ferry.service")
		if *remove {
			run("systemctl", "--user", "disable", "--now", "ferry")
			os.Remove(p)
			run("systemctl", "--user", "daemon-reload")
			o.result("Removed %s; ferry up no longer starts by itself.", tildePath(p))
			return nil
		}
		env := ""
		if h := os.Getenv("FERRY_HOME"); h != "" {
			env = fmt.Sprintf("Environment=FERRY_HOME=%s\n", h)
		}
		unit := fmt.Sprintf("[Unit]\nDescription=ferry: keeps this device reachable by your other devices and contacts\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\n%sExecStart=%s up\nRestart=always\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n", env, exe)
		if err := writeAtomic(p, []byte(unit), 0o644); err != nil {
			return err
		}
		if agent.Running() {
			o.say("note: a ferry up is already running here; stop it, or the service waits until it does.")
		}
		if err := run("systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		if err := run("systemctl", "--user", "enable", "--now", "ferry"); err != nil {
			return err
		}
		o.result("Installed %s and started it: ferry up now runs whenever you are logged in.", tildePath(p))
		o.say("%s", o.dim("To keep it running when you are logged out too: loginctl enable-linger "+os.Getenv("USER")))
	case "darwin":
		p := filepath.Join(home, "Library", "LaunchAgents", "dev.ferry.up.plist")
		dom := fmt.Sprintf("gui/%d", os.Getuid())
		if *remove {
			run("launchctl", "bootout", dom, p)
			os.Remove(p)
			o.result("Removed %s; ferry up no longer starts by itself.", tildePath(p))
			return nil
		}
		env := ""
		if h := os.Getenv("FERRY_HOME"); h != "" {
			env = "<key>FERRY_HOME</key><string>" + h + "</string>"
		}
		plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>dev.ferry.up</string>
<key>ProgramArguments</key><array><string>%s</string><string>up</string></array>
<key>EnvironmentVariables</key><dict><key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>%s</dict>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
<key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, exe, env, filepath.Join(home, "Library", "Logs", "ferry.log"))
		if err := writeAtomic(p, []byte(plist), 0o644); err != nil {
			return err
		}
		run("launchctl", "bootout", dom, p)
		if err := run("launchctl", "bootstrap", dom, p); err != nil {
			return err
		}
		o.result("Installed %s and started it: ferry up now runs whenever you are logged in.", tildePath(p))
	default:
		return fmt.Errorf("ferry mine service knows Linux (systemd) and macOS (launchd); on %s run ferry up yourself", runtime.GOOS)
	}
	return nil
}

// ---- on ----

// cmdOn runs a command on one of your devices: stdin, stdout, stderr and
// the exit code pass through, in the same folder when the current one is
// in your home (or in ~/m/<device>).
func cmdOn(ctx context.Context, o *out, args []string) error {
	fs := flag.NewFlagSet("on", flag.ContinueOnError)
	if flagHook != nil {
		_, err := parse(fs, args)
		return err
	}
	fallback := false
	if len(args) > 0 && args[0] == "--fallback" {
		fallback, args = true, args[1:]
	}
	if len(args) == 0 {
		return usagef("usage: on <device|gpu> [command…]")
	}
	if fallback {
		return onFallback(ctx, o, args)
	}
	target, cmd := args[0], args[1:]
	id, _ := keys.Load()
	if target == "gpu" {
		t, err := pickGPU(ctx)
		if err != nil {
			return err
		}
		target = t
	}
	if id != nil && target == id.Name {
		return runHere(cmd)
	}
	d, err := mineDevice(target)
	if err != nil {
		return err
	}
	return execOn(d, cmd, "")
}

// runHere runs the command on this device itself.
func runHere(cmd []string) error {
	if len(cmd) == 0 {
		sh := os.Getenv("SHELL")
		if sh == "" {
			sh = "/bin/sh"
		}
		cmd = []string{sh}
	}
	p, err := exec.LookPath(cmd[0])
	if err != nil {
		return err
	}
	return syscall.Exec(p, cmd, os.Environ())
}

// remoteDir is the folder on a device with home remoteHome that matches cwd
// here: the same place relative to the home, whether cwd is in this home or
// in ~/m/<device>. "" means its home.
func remoteDir(cwd, localHome, mountRoot, remoteHome string) string {
	rel := ""
	if r, ok := strings.CutPrefix(cwd, mountRoot+"/"); ok {
		_, rest, _ := strings.Cut(r, "/") // past the device name
		rel = rest
	} else if cwd == mountRoot {
		return ""
	} else if cwd == localHome {
		return remoteHome
	} else if r, ok := strings.CutPrefix(cwd, localHome+"/"); ok {
		rel = r
	} else {
		return ""
	}
	if rel == "" {
		return remoteHome
	}
	return remoteHome + "/" + rel
}

// onCommand is what ssh runs there: cd to the folder, then the command in
// the user's login shell (so its PATH: Homebrew, ~/.local/bin).
func onCommand(dir, dev string, cmd []string) string {
	q := make([]string, len(cmd))
	for i, a := range cmd {
		q[i] = shq(a)
	}
	inner := `exec "${SHELL:-/bin/sh}" -l`
	if len(cmd) > 0 {
		inner = `exec "${SHELL:-/bin/sh}" -lc "$2"`
	}
	script := `[ -z "$1" ] || cd -- "$1" 2>/dev/null || printf 'on: no %s on ` + dev + `; running in ~\n' "$1" >&2; ` + inner
	return "exec /bin/sh -c " + shq(script) + " on " + shq(dir) + " " + shq(strings.Join(q, " "))
}

func execOn(d keys.Contact, cmd []string, note string) error {
	cwd, _ := os.Getwd()
	dir := remoteDir(cwd, mineHome(), mineMountRoot(), d.Device.Home)
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		return err
	}
	tty := "-T"
	if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
		tty = "-t"
	}
	if note != "" {
		if term.IsTerminal(int(os.Stderr.Fd())) {
			note = "\033[2m" + note + "\033[0m"
		}
		fmt.Fprintln(os.Stderr, note)
	}
	return syscall.Exec(ssh, []string{"ssh", tty, "-q", "-o", "BatchMode=yes", d.Name, onCommand(dir, d.Name, cmd)}, os.Environ())
}

// reachable lists your devices that the follower last saw reachable, or,
// without a fresh status, all of them.
func reachable() []keys.Contact {
	ds, _ := mineDevices()
	s, err := loadStatus()
	if err != nil || time.Since(s.At) > 2*time.Minute {
		return ds
	}
	up := map[string]bool{}
	for _, d := range s.Devices {
		up[d.Name] = d.Online && d.Path == "direct"
	}
	var out []keys.Contact
	for _, d := range ds {
		if up[d.Name] {
			out = append(out, d)
		}
	}
	return out
}

// firstWith returns the first device (in name order) for which the script
// succeeds there, asking all at once.
func firstWith(ctx context.Context, ds []keys.Contact, script string, timeout time.Duration) (keys.Contact, bool) {
	ok := make([]bool, len(ds))
	var wg sync.WaitGroup
	for i, d := range ds {
		wg.Add(1)
		go func(i int, d keys.Contact) {
			defer wg.Done()
			_, err := mineRun(ctx, d.Name, script, "", timeout)
			ok[i] = err == nil
		}(i, d)
	}
	wg.Wait()
	for i, d := range ds {
		if ok[i] {
			return d, true
		}
	}
	return keys.Contact{}, false
}

var cmdNameRE = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)

func onFallback(ctx context.Context, o *out, args []string) error {
	name := args[0]
	shell := filepath.Base(os.Getenv("SHELL"))
	if shell == "" || shell == "." {
		shell = "sh"
	}
	if cmdNameRE.MatchString(name) {
		script := `exec "${SHELL:-/bin/sh}" -lc 'command -v -- "$0" >/dev/null 2>&1' ` + shq(name)
		if d, ok := firstWith(ctx, reachable(), script, 6*time.Second); ok {
			return execOn(d, args, fmt.Sprintf("↪ %s ran on %s", name, d.Name))
		}
	}
	fmt.Fprintf(os.Stderr, "%s: %s: command not found\n", shell, name)
	os.Exit(127)
	return nil
}

// pickGPU names a device with an NVIDIA GPU: this one if it has one, else
// the first reachable one that does.
func pickGPU(ctx context.Context) (string, error) {
	id, _ := keys.Load()
	if exec.Command("nvidia-smi", "-L").Run() == nil && id != nil {
		return id.Name, nil
	}
	if s, err := loadStatus(); err == nil && time.Since(s.At) < 2*time.Minute {
		for _, d := range s.Devices {
			if d.Online && d.Path == "direct" && len(d.GPUs) > 0 {
				return d.Name, nil
			}
		}
	}
	ds, _ := mineDevices()
	if d, ok := firstWith(ctx, ds, "nvidia-smi -L >/dev/null 2>&1", 30*time.Second); ok {
		return d.Name, nil
	}
	return "", fmt.Errorf("%w: none of your reachable devices has an NVIDIA GPU (ferry mine shows them)", errUnreachable)
}
