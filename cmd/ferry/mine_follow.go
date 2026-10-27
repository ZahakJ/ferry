package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ZahakJ/ferry/internal/agent"
	"github.com/ZahakJ/ferry/internal/keys"
	"github.com/ZahakJ/ferry/internal/session"
)

// ---- what a device has: CPUs, memory, load, NVIDIA GPUs ----

const statsScript = `if [ "$(uname -s)" = Darwin ]; then
  echo "os darwin"; echo "cpus $(sysctl -n hw.ncpu)"
  ps=$(sysctl -n hw.pagesize)
  fp=$(vm_stat | awk '/Pages (free|inactive|speculative)/ {gsub("\\.","",$NF); s+=$NF} END {print s+0}')
  echo "mem $(sysctl -n hw.memsize) $((fp * ps))"
  echo "load $(sysctl -n vm.loadavg | awk '{print $2}')"
else
  echo "os linux"; echo "cpus $(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN)"
  echo "mem $(awk '/^MemTotal:/{t=$2} /^MemAvailable:/{a=$2} END {printf "%.0f %.0f", t*1024, a*1024}' /proc/meminfo)"
  echo "load $(cut -d' ' -f1 /proc/loadavg)"
fi
nvidia-smi --query-gpu=name,memory.total --format=csv,noheader,nounits 2>/dev/null | sed 's/^/gpu /'
exit 0`

type gpuInfo struct {
	Name  string `json:"name"`
	MemMB int64  `json:"mem_mb"`
}

type devStatus struct {
	Name     string    `json:"name"`
	This     bool      `json:"this,omitempty"`
	Online   bool      `json:"online"` // its ferry answers
	Path     string    `json:"path"`   // direct | none
	OS       string    `json:"os,omitempty"`
	CPUs     int       `json:"cpus,omitempty"`
	MemTotal int64     `json:"mem_total,omitempty"`
	MemFree  int64     `json:"mem_free,omitempty"`
	Load     float64   `json:"load,omitempty"`
	GPUs     []gpuInfo `json:"gpus,omitempty"`
	Mount    string    `json:"mount,omitempty"`
	Mounted  bool      `json:"mounted"`
	Seen     time.Time `json:"seen,omitzero"`
	Note     string    `json:"note,omitempty"`
}

type mineStatus struct {
	At      time.Time   `json:"at"`
	Devices []devStatus `json:"devices"`
}

func parseStats(out string, d *devStatus) error {
	got := false
	for _, l := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(l), " ")
		switch k {
		case "os":
			d.OS, got = v, true
		case "cpus":
			d.CPUs, _ = strconv.Atoi(v)
		case "mem":
			f := strings.Fields(v)
			if len(f) == 2 {
				d.MemTotal, _ = strconv.ParseInt(f[0], 10, 64)
				d.MemFree, _ = strconv.ParseInt(f[1], 10, 64)
			}
		case "load":
			d.Load, _ = strconv.ParseFloat(v, 64)
		case "gpu":
			name, mem, _ := strings.Cut(v, ",")
			g := gpuInfo{Name: strings.TrimSpace(name)}
			g.MemMB, _ = strconv.ParseInt(strings.TrimSpace(mem), 10, 64)
			if g.Name != "" {
				d.GPUs = append(d.GPUs, g)
			}
		}
	}
	if !got {
		return errors.New("no answer from the device")
	}
	return nil
}

func localStats() devStatus {
	id, _ := keys.Load()
	d := devStatus{This: true, Online: true, Path: "here"}
	if id != nil {
		d.Name = id.Name
	}
	out, _ := exec.Command("/bin/sh", "-c", statsScript).Output()
	parseStats(string(out), &d)
	return d
}

func loadStatus() (*mineStatus, error) {
	b, err := os.ReadFile(minePath("status.json"))
	if err != nil {
		return nil, err
	}
	var s mineStatus
	return &s, json.Unmarshal(b, &s)
}

// ---- the follower, inside ferry up ----

type devRun struct {
	online, reachable bool
	fails             int
	seen              time.Time
	stats             devStatus
	cache             syncCache
}

type mineFollower struct {
	a    *agent.Local
	kick chan struct{}

	stopMu sync.Mutex
	mu     sync.Mutex
	port   int // the mesh sshd's port: refused to friends whether or not it runs right now
	sshd   *exec.Cmd
	devs   map[string]*devRun
	tick   int
}

// startMine serves the mesh from ferry up: the private sshd, the intents
// your devices send each other, and the follower that keeps ~/m mounted,
// sessions synced and owed messages delivered. With no devices linked it
// only waits.
func startMine(ctx context.Context, a *agent.Local) *mineFollower {
	f := &mineFollower{a: a, kick: make(chan struct{}, 1), devs: map[string]*devRun{}}
	if st, err := loadMineState(); err == nil {
		f.port = meshPortOf(st)
	}
	a.ServeMesh(func() int { f.mu.Lock(); defer f.mu.Unlock(); return f.port })
	a.Handle("mine-ping", func(s *session.Session, c keys.Contact, in session.Intent) {
		s.Conn.SendJSON(session.IntentReply{OK: true})
	})
	a.Handle("mine-hello", f.onHello)
	a.Handle("mine-forget", f.onForget)
	a.Handle("mine-leave", f.onLeave)
	a.HandleIPC("mine-kick", func(context.Context, json.RawMessage) (any, error) {
		f.poke()
		return map[string]bool{"ok": true}, nil
	})
	go f.run(ctx)
	return f
}

func (f *mineFollower) poke() {
	select {
	case f.kick <- struct{}{}:
	default:
	}
}

const mineTick = 15 * time.Second

func (f *mineFollower) run(ctx context.Context) {
	defer f.stopSSHD()
	// ssh's ProxyCommand (ferry pipe) must find this agent's socket, or it
	// goes to the rendezvous as a second peer with this identity.
	for i := 0; i < 100 && !agent.Running() && ctx.Err() == nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	// ssh connections opened before this ferry up ran carry their own
	// rendezvous connection; start them again through this one.
	ds, _ := mineDevices()
	for _, d := range ds {
		if mineMasterAlive(d.Name) {
			stopMineMaster(d.Name)
		}
	}
	t := time.NewTicker(mineTick)
	defer t.Stop()
	kicked := true
	for {
		f.once(ctx, kicked)
		kicked = false
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-f.kick:
			kicked = true
		}
	}
}

func (f *mineFollower) once(ctx context.Context, kicked bool) {
	f.tick++
	ds, _ := mineDevices()
	st, err := loadMineState()
	if err != nil || len(ds) == 0 {
		f.stopSSHD()
		if len(ds) == 0 && st != nil && len(st.Pending) > 0 {
			deliverPending(ctx, f.a.Ask, f.a.Logf) // a last mine-leave, say
		}
		return
	}
	f.ensureSSHD(st)
	unlock := mineLock()
	mineConverge(f.a.Logf) // in case contacts changed under us (ferry pair over a device, say)
	unlock()
	doSync := kicked || f.tick%4 == 1
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // owed messages do not wait for slow or offline devices
		defer wg.Done()
		deliverPending(ctx, f.a.Ask, f.a.Logf)
	}()
	for _, d := range ds {
		f.mu.Lock()
		r := f.devs[d.Name]
		if r == nil {
			r = &devRun{cache: syncCache{}}
			f.devs[d.Name] = r
		}
		f.mu.Unlock()
		wg.Add(1)
		go func(d keys.Contact, r *devRun) {
			defer wg.Done()
			f.follow(ctx, d, r, st, kicked, doSync)
		}(d, r)
	}
	wg.Wait()
	f.writeStatus(ds)
}

// follow checks one device: reachable? its numbers, its mount, its
// sessions, the forwards asked for.
func (f *mineFollower) follow(ctx context.Context, d keys.Contact, r *devRun, st *mineState, kicked, doSync bool) {
	mp := mineMount(d.Name)
	// An unreachable device is tried every tick for a minute, then every
	// fourth (once a minute), so the rendezvous is not asked all the time.
	if !r.online && r.fails > 4 && !kicked && f.tick%4 != 0 {
		return
	}
	started, err := ensureMineMaster(ctx, d.Name)
	var out []byte
	if err == nil {
		out, err = mineRun(ctx, d.Name, statsScript, "", 45*time.Second)
	}
	s := devStatus{Name: d.Name, Mount: mp, Path: "none"}
	if err == nil {
		err = parseStats(string(out), &s)
	}
	if err != nil {
		if r.online {
			f.a.Logf("mine: %s is unreachable: %v", d.Name, err)
		}
		r.online, r.fails = false, r.fails+1
		pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		rep, perr := f.a.Ask(pctx, d, session.Intent{Want: "mine-ping"})
		cancel()
		r.reachable = perr == nil && rep.OK
		s.Online = r.reachable
		if r.reachable {
			s.Note = "its ferry answers, but ssh through it failed: " + err.Error()
		}
		if mounted(mp) { // a mount of a device that is gone only hangs whoever looks
			unmount(mp)
			f.a.Logf("mine: unmounted %s (%s is unreachable)", mp, d.Name)
		}
		s.Seen = r.seen
		r.stats = s
		return
	}
	if !r.online {
		f.a.Logf("mine: %s is reachable", d.Name)
	}
	r.online, r.reachable, r.fails, r.seen = true, true, 0, time.Now()
	s.Online, s.Path, s.Seen = true, "direct", r.seen
	if started {
		for _, fw := range st.Fwds {
			if fw.Dev == d.Name {
				mineForward(d.Name, fw.Port, fw.Local)
			}
		}
	}
	s.Mounted, s.Note = ensureMineMount(d.Name, f.a.Logf)
	r.stats = s
	if doSync {
		f.syncFrom(ctx, d, r)
	}
}

func (f *mineFollower) syncFrom(ctx context.Context, d keys.Contact, r *devRun) {
	unlock, err := syncLock()
	if err != nil {
		return
	}
	defer unlock()
	id, _ := keys.Load()
	res, err := syncSessions(ctx, sshPeer{d.Name}, d.Name, id.Name, d.Device.Home, mineHome(), claudeProjects(), r.cache)
	if err != nil {
		f.a.Logf("mine: Claude sessions from %s: %v", d.Name, err)
		return
	}
	if res.New+res.Extended+res.Conflicts > 0 {
		f.a.Logf("mine: Claude sessions from %s: %d new, %d grown, %d diverged (kept both)", d.Name, res.New, res.Extended, res.Conflicts)
	}
}

func (f *mineFollower) writeStatus(ds []keys.Contact) {
	s := mineStatus{At: time.Now()}
	f.mu.Lock()
	for _, d := range ds {
		if r := f.devs[d.Name]; r != nil {
			st := r.stats
			if st.Name == "" {
				st = devStatus{Name: d.Name, Path: "none", Mount: mineMount(d.Name)}
			}
			st.Mounted = mounted(mineMount(d.Name))
			s.Devices = append(s.Devices, st)
		}
	}
	f.mu.Unlock()
	b, _ := json.MarshalIndent(s, "", "  ")
	writeAtomic(minePath("status.json"), b, 0o600)
}

// ---- ~/m/<device> ----

// ensureMineMount mounts the device's home at ~/m/<device> unless it is,
// and replaces a mount that stopped answering.
func ensureMineMount(dev string, logf func(string, ...any)) (bool, string) {
	mp := mineMount(dev)
	sshfs, _ := exec.LookPath("sshfs")
	if sshfs == "" {
		return false, "install sshfs to see its files at " + tildePath(mp)
	}
	if mounted(mp) {
		if mountAnswers(mp) {
			return true, ""
		}
		unmount(mp)
		logf("mine: %s stopped answering; mounting it again", mp)
	}
	os.MkdirAll(mp, 0o755)
	opts := "reconnect,ServerAliveInterval=15,ServerAliveCountMax=3,BatchMode=yes,ControlPath=none"
	if runtime.GOOS == "darwin" {
		opts += ",volname=" + dev
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sshfs, dev+":", mp, "-o", opts)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil && !mounted(mp) {
		return false, "could not mount: " + lastLine(string(out))
	}
	logf("mine: mounted %s's home at %s", dev, mp)
	return true, ""
}

// mountAnswers says whether a mount point answers within a few seconds.
func mountAnswers(mp string) bool {
	done := make(chan error, 1)
	go func() { _, err := os.ReadDir(mp); done <- err }()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(8 * time.Second):
		return false
	}
}

// ---- the private sshd ----

func meshPortOf(st *mineState) int {
	if st.SystemSSHD {
		return 22
	}
	return st.Port
}

// sshdPids lists processes running our sshd config (a ferry up that was
// killed outright leaves its sshd behind).
func sshdPids() []int {
	cfg := minePath("sshd_config")
	if _, err := os.Stat("/proc/self"); err == nil {
		return procsMatching(cfg)
	}
	out, _ := exec.Command("pgrep", "-f", "-u", strconv.Itoa(os.Getuid()), cfg).Output()
	var ps []int
	for _, f := range strings.Fields(string(out)) {
		if p, err := strconv.Atoi(f); err == nil && p != os.Getpid() {
			ps = append(ps, p)
		}
	}
	return ps
}

func (f *mineFollower) ensureSSHD(st *mineState) {
	f.mu.Lock()
	running := f.sshd != nil
	f.port = meshPortOf(st)
	f.mu.Unlock()
	if st.SystemSSHD || running {
		return
	}
	if ps := sshdPids(); len(ps) > 0 {
		killTree(ps)
		for _, p := range ps {
			syscall.Kill(p, syscall.SIGTERM)
		}
		for i := 0; i < 40 && listening(st.Port); i++ {
			time.Sleep(50 * time.Millisecond)
		}
		f.a.Logf("mine: stopped a mesh sshd left behind by an earlier ferry up")
	}
	bin := findSSHD()
	if bin == "" {
		f.a.Logf("mine: no sshd on this machine (install openssh-server); your other devices cannot reach this one")
		return
	}
	if listening(st.Port) { // someone else took the port
		unlock := mineLock()
		if s2, err := loadMineState(); err == nil {
			st = s2
		}
		st.Port = freeLocalPort()
		st.save()
		writeMineSSHD(st)
		unlock()
		f.mu.Lock()
		f.port = st.Port
		f.mu.Unlock()
	}
	log, _ := os.OpenFile(minePath("sshd.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	cmd := exec.Command(bin, "-D", "-e", "-f", minePath("sshd_config"))
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		f.a.Logf("mine: could not start the mesh sshd: %v", err)
		if log != nil {
			log.Close()
		}
		return
	}
	f.mu.Lock()
	f.sshd = cmd
	f.mu.Unlock()
	go func() {
		err := cmd.Wait()
		if log != nil {
			log.Close()
		}
		f.mu.Lock()
		if f.sshd == cmd {
			f.sshd = nil
		}
		f.mu.Unlock()
		f.a.Logf("mine: the mesh sshd stopped (%v); see %s", err, minePath("sshd.log"))
	}()
	for i := 0; i < 50 && !listening(st.Port); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if listening(st.Port) {
		f.a.Logf("mine: mesh sshd on 127.0.0.1:%d, for your own devices only", st.Port)
	}
}

// stopSSHD stops the private sshd and every session it serves.
// Two callers (the follower ending, ferry up exiting) wait for each other,
// so the process cannot exit half way through.
func (f *mineFollower) stopSSHD() {
	f.stopMu.Lock()
	defer f.stopMu.Unlock()
	f.mu.Lock()
	cmd := f.sshd
	f.sshd = nil
	f.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		killTree([]int{cmd.Process.Pid})
	}
}

// ---- what devices tell each other ----

type helloData struct {
	Members []mineMember `json:"members"`
}

type forgetData struct {
	Key keys.Public `json:"key"`
}

// onHello: one of your devices introduces devices that joined the mesh
// through it. They are yours: it could add them here over ssh anyway.
func (f *mineFollower) onHello(s *session.Session, c keys.Contact, in session.Intent) {
	var d helloData
	if json.Unmarshal(in.Data, &d) != nil {
		s.Conn.SendJSON(session.IntentReply{Why: "bad introduction"})
		return
	}
	id, _ := keys.Load()
	unlock := mineLock()
	names, err := recordMine(d.Members, id.Public())
	if err == nil {
		err = mineConverge(nil)
	}
	unlock()
	if err != nil {
		s.Conn.SendJSON(session.IntentReply{Why: err.Error()})
		return
	}
	f.a.Logf("mine: %s introduced %s; they are your devices here too", c.Name, strings.Join(names, ", "))
	s.Conn.SendJSON(session.IntentReply{OK: true})
	f.poke()
}

// onForget: one of your devices removed a device from the mesh (ferry mine
// rm); it goes here too. If it is this device, this one leaves.
func (f *mineFollower) onForget(s *session.Session, c keys.Contact, in session.Intent) {
	var d forgetData
	if json.Unmarshal(in.Data, &d) != nil {
		s.Conn.SendJSON(session.IntentReply{Why: "bad request"})
		return
	}
	id, _ := keys.Load()
	if string(d.Key.Sign) == string(id.Public().Sign) {
		f.onLeave(s, c, in)
		return
	}
	unlock := mineLock()
	gone, err := forgetMine(func(k keys.Contact) bool { return string(k.Key.Sign) == string(d.Key.Sign) })
	unlock()
	if err != nil {
		s.Conn.SendJSON(session.IntentReply{Why: err.Error()})
		return
	}
	if len(gone) > 0 {
		f.a.Logf("mine: %s removed %s from your devices; removed here too", c.Name, strings.Join(gone, ", "))
	}
	s.Conn.SendJSON(session.IntentReply{OK: true})
}

// onLeave: this device was taken out of the mesh (ferry mine rm on one of
// the others): it forgets all of your devices.
func (f *mineFollower) onLeave(s *session.Session, c keys.Contact, in session.Intent) {
	unlock := mineLock()
	gone, err := forgetMine(func(keys.Contact) bool { return true })
	if st, e := loadMineState(); e == nil {
		st.Pending = nil
		st.save()
	}
	unlock()
	if err != nil {
		s.Conn.SendJSON(session.IntentReply{Why: err.Error()})
		return
	}
	f.a.Logf("mine: %s took this device out of your mesh; forgot %s", c.Name, strings.Join(gone, ", "))
	s.Conn.SendJSON(session.IntentReply{OK: true})
}

// forgetMine removes the devices drop matches and everything set up for
// them, and drops what was still owed to them. The caller holds mineLock.
func forgetMine(drop func(keys.Contact) bool) ([]string, error) {
	cs, err := keys.LoadContacts()
	if err != nil {
		return nil, err
	}
	var gone []string
	ids := map[string]bool{}
	for n, c := range cs {
		if c.Mine() && drop(c) {
			gone = append(gone, n)
			ids[c.Key.ID()] = true
			delete(cs, n)
		}
	}
	sort.Strings(gone)
	if len(gone) == 0 {
		return nil, nil
	}
	if err := cs.Save(); err != nil {
		return nil, err
	}
	if st, err := loadMineState(); err == nil {
		var keep []mineOp
		for _, op := range st.Pending {
			if !ids[op.To.ID()] || op.Want == "mine-leave" {
				keep = append(keep, op)
			}
		}
		st.Pending = keep
		st.save()
	}
	return gone, mineConverge(nil)
}

// ---- messages owed to devices that were offline ----

type askFunc func(context.Context, keys.Contact, session.Intent) (session.IntentReply, error)

// queueOp owes a device a message until it has been delivered.
func queueOp(st *mineState, to keys.Contact, want string, data any) {
	b, _ := json.Marshal(data)
	st.Pending = append(st.Pending, mineOp{To: to.Key, ToName: to.Name, Want: want, Data: b, Added: time.Now().UTC()})
}

// deliverPending sends what is owed to whoever is online and returns how
// many messages were delivered and how many still wait.
func deliverPending(ctx context.Context, ask askFunc, logf func(string, ...any)) (sent, waiting int) {
	unlock := mineLock()
	st, err := loadMineState()
	unlock()
	if err != nil || len(st.Pending) == 0 {
		return 0, 0
	}
	done := map[int]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, op := range st.Pending {
		wg.Add(1)
		go func(i int, op mineOp) {
			defer wg.Done()
			c := keys.Contact{Name: op.ToName, Key: op.To}
			actx, cancel := context.WithTimeout(ctx, 30*time.Second)
			r, err := ask(actx, c, session.Intent{Want: op.Want, Data: op.Data})
			cancel()
			switch {
			case err != nil && strings.Contains(err.Error(), "stream reset"):
				// it turned us away at the handshake: it no longer knows this
				// device, so there is nothing left to tell it
				logf("mine: %s no longer knows this device; not telling it (%s)", op.ToName, strings.TrimPrefix(op.Want, "mine-"))
			case err != nil && time.Since(op.Added) > 30*24*time.Hour:
				logf("mine: %s was offline for 30 days; dropped the %s owed to it", op.ToName, strings.TrimPrefix(op.Want, "mine-"))
			case err != nil:
				return // offline: next time
			case !r.OK:
				logf("mine: %s declined %s: %s", op.ToName, op.Want, r.Why)
			}
			mu.Lock()
			done[i] = err == nil // true: delivered; false: given up on
			mu.Unlock()
		}(i, op)
	}
	wg.Wait()
	if len(done) == 0 {
		return 0, len(st.Pending)
	}
	unlock = mineLock()
	defer unlock()
	cur, err := loadMineState()
	if err != nil {
		return 0, len(st.Pending)
	}
	sentOps := map[string]bool{}
	told := 0
	for i, ok := range done {
		op := st.Pending[i]
		sentOps[op.To.ID()+op.Want+string(op.Data)+op.Added.String()] = true
		if ok {
			told++
			logf("mine: told %s (%s)", op.ToName, strings.TrimPrefix(op.Want, "mine-"))
		}
	}
	var keep []mineOp
	for _, op := range cur.Pending {
		if !sentOps[op.To.ID()+op.Want+string(op.Data)+op.Added.String()] {
			keep = append(keep, op)
		}
	}
	cur.Pending = keep
	cur.save()
	return told, len(keep)
}

// kickOrDeliver hands owed messages to ferry up if it runs, or delivers
// them from here.
func kickOrDeliver(ctx context.Context, o *out) {
	if agent.Running() {
		agent.Call(ctx, "mine-kick", nil, nil)
		return
	}
	cfg, err := session.LoadConfig()
	if err != nil {
		return
	}
	l, err := agent.NewLocal(cfg)
	if err != nil {
		return
	}
	defer l.Close()
	sent, waiting := deliverPending(ctx, l.Ask, func(f string, a ...any) { o.say("%s", o.dim(fmt.Sprintf(f, a...))) })
	_ = sent
	if waiting > 0 {
		o.say("%d message(s) for devices that are offline wait here; they go out when ferry up runs here and they are online.", waiting)
	}
}
