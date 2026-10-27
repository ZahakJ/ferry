package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZahakJ/ferry/internal/keys"
	"github.com/ZahakJ/ferry/internal/session"
)

func TestMapProjectDir(t *testing.T) {
	noCwd := func() (string, error) { return "", errors.New("none") }
	for _, c := range []struct{ dir, peer, local, want string }{
		{"-home-zman-work-ferry", "/home/zman", "/Users/zman", "-Users-zman-work-ferry"},
		{"-Users-zman-work-ferry", "/Users/zman", "/home/zman", "-home-zman-work-ferry"},
		{"-home-zman", "/home/zman", "/Users/zman", "-Users-zman"},
		{"-home-zman--local-share-x", "/home/zman", "/Users/zman", "-Users-zman--local-share-x"},
		{"-tmp-scratch", "/home/zman", "/Users/zman", "-tmp-scratch"},        // not in the home: same path
		{"-home-zmanx-a", "/home/zman", "/Users/zman", "-home-zmanx-a"},      // another home
		{"-home-zman-work", "/home/zman", "/home/zman", "-home-zman-work"},   // same home
		{"-home-zman-work", "/home/zman", "/home/zman2", "-home-zman2-work"}, // same OS, other user
	} {
		if got := mapProjectDir(c.dir, c.peer, c.local, noCwd); got != c.want {
			t.Errorf("%s (%s→%s) = %q, want %q", c.dir, c.peer, c.local, got, c.want)
		}
	}
	// Over 200 characters Claude Code cuts the name and adds a hash of the
	// whole path, so the real path must come from the sessions.
	long := "/home/zman/" + strings.Repeat("deep/", 45) + "proj"
	dir := claudeProjectKey(long)
	if len(dir) <= 200 {
		t.Fatalf("test path too short: %d", len(dir))
	}
	want := claudeProjectKey("/Users/zman" + strings.TrimPrefix(long, "/home/zman"))
	if got := mapProjectDir(dir, "/home/zman", "/Users/zman", func() (string, error) { return long, nil }); got != want {
		t.Errorf("long: got %q, want %q", got, want)
	}
	if got := mapProjectDir(dir, "/home/zman", "/Users/zman", func() (string, error) { return "/home/zman/other", nil }); got != "" {
		t.Errorf("long with a cwd that does not match the name: %q, want skip", got)
	}
	if got := mapProjectDir(dir, "/home/zman", "/Users/zman", noCwd); got != "" {
		t.Errorf("long without a cwd: %q, want skip", got)
	}
	// A short name whose mapped form crosses 200 needs the real path too.
	edge := "/Users/zman/" + strings.Repeat("a", 186)
	ed := claudeProjectKey(edge)
	if len(ed) > 200 {
		t.Fatalf("edge too long")
	}
	if got := mapProjectDir(ed, "/Users/zman", "/home/zmanzman", func() (string, error) { return edge, nil }); got != claudeProjectKey("/home/zmanzman/"+strings.Repeat("a", 186)) {
		t.Errorf("edge: %q", got)
	}
}

// The encoding matches folders Claude Code made on this machine, when there
// are any to compare with.
func TestClaudeKeyMatchesRealFolders(t *testing.T) {
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, ".claude", "projects")
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Skip("no ~/.claude/projects here")
	}
	checked := 0
	for _, e := range ents {
		if !e.IsDir() || checked > 40 {
			continue
		}
		fs, _ := filepath.Glob(filepath.Join(root, e.Name(), "*.jsonl"))
		for _, f := range fs {
			b, _ := exec.Command("grep", "-m1", "-o", `"cwd":"[^"]*"`, f).Output()
			c, err := parseCwd(string(b))
			if err != nil {
				continue
			}
			if claudeProjectKey(c) == e.Name() {
				checked++
				break
			}
		}
	}
	if checked == 0 {
		t.Skip("no sessions with a cwd that names their folder")
	}
}

func TestRemoteDir(t *testing.T) {
	for _, c := range []struct{ cwd, want string }{
		{"/home/zman/work/x", "/Users/zman/work/x"},
		{"/home/zman", "/Users/zman"},
		{"/home/zman/m/mac/proj", "/Users/zman/proj"},
		{"/home/zman/m/mac", "/Users/zman"},
		{"/home/zman/m", ""},
		{"/tmp", ""},
		{"/home/zmanx/a", ""},
	} {
		if got := remoteDir(c.cwd, "/home/zman", "/home/zman/m", "/Users/zman"); got != c.want {
			t.Errorf("%s: %q, want %q", c.cwd, got, c.want)
		}
	}
}

// onCommand survives two shells: what arrives is exactly the arguments.
func TestOnCommandQuoting(t *testing.T) {
	dir := t.TempDir()
	args := []string{"printf", "%s|", "a b", "it's", "$HOME", "`x`", ""}
	cmd := exec.Command("/bin/sh", "-c", onCommand(dir, "dev", args))
	cmd.Env = append(os.Environ(), "SHELL=/bin/sh")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "a b|it's|$HOME|`x`||"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	cmd = exec.Command("/bin/sh", "-c", onCommand(dir, "dev", []string{"pwd"}))
	cmd.Env = append(os.Environ(), "SHELL=/bin/sh")
	out, _ = cmd.Output()
	if real, _ := filepath.EvalSymlinks(dir); strings.TrimSpace(string(out)) != real && strings.TrimSpace(string(out)) != dir {
		t.Errorf("pwd: %q, want %q", out, dir)
	}
	cmd = exec.Command("/bin/sh", "-c", onCommand(dir+"/nope", "dev", []string{"true"}))
	cmd.Env = append(os.Environ(), "SHELL=/bin/sh")
	b, _ := cmd.CombinedOutput()
	if !strings.Contains(string(b), "running in ~") {
		t.Errorf("missing folder not reported: %q", b)
	}
}

// dirPeer is another device's ~/.claude/projects in a local folder.
type dirPeer struct{ root string }

func (d dirPeer) List(ctx context.Context) (map[string]int64, error) {
	m := map[string]int64{}
	err := filepath.Walk(d.root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			r, _ := filepath.Rel(d.root, p)
			m[filepath.ToSlash(r)] = fi.Size()
		}
		return nil
	})
	return m, err
}

func (d dirPeer) Hashes(ctx context.Context, want map[string]int64) (map[string]string, error) {
	m := map[string]string{}
	for r, n := range want {
		h, err := hashPrefix(filepath.Join(d.root, r), n)
		if err == nil {
			m[r] = h
		}
	}
	return m, nil
}

func (d dirPeer) Fetch(ctx context.Context, rel string, off, n int64) ([]byte, error) {
	f, err := os.Open(filepath.Join(d.root, rel))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	f.Seek(off, 0)
	return io.ReadAll(io.LimitReader(f, n))
}

func (d dirPeer) Cwd(ctx context.Context, dir string) (string, error) {
	fs, _ := filepath.Glob(filepath.Join(d.root, dir, "*.jsonl"))
	for _, f := range fs {
		b, _ := exec.Command("grep", "-m1", "-o", `"cwd":"[^"]*"`, f).Output()
		if c, err := parseCwd(string(b)); err == nil {
			return c, nil
		}
	}
	return "", errors.New("none")
}

func put(t *testing.T, root, rel, s string) {
	t.Helper()
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(root, rel string) string {
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func TestSyncSessions(t *testing.T) {
	mac, here := t.TempDir(), t.TempDir() // the Mac's projects, and this Linux device's
	ctx := context.Background()
	sync := func() syncResult {
		t.Helper()
		r, err := syncSessions(ctx, dirPeer{mac}, "mac", "shadow", "/Users/zman", "/home/zman", here, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	l1 := `{"type":"user","cwd":"/Users/zman/work/ferry","n":1}` + "\n"
	l2 := `{"type":"assistant","n":2}` + "\n"
	put(t, mac, "-Users-zman-work-ferry/s1.jsonl", l1)
	put(t, mac, "-Users-zman-work-ferry/s1/subagents/agent-a.jsonl", l1)
	put(t, mac, "-Users-zman-work-ferry/s1/subagents/agent-a.meta.json", `{"agentType":"x"}`)
	put(t, mac, "-Users-zman-work-ferry/memory/MEMORY.md", "the mac's memory")
	put(t, mac, "-Users-zman-work-ferry/.hidden.jsonl", l1)
	put(t, mac, "-tmp-x/s9.jsonl", l1+`{"partial`) // a line still being written
	r := sync()
	if r.New != 4 {
		t.Errorf("new: %+v", r)
	}
	if got := read(here, "-home-zman-work-ferry/s1.jsonl"); got != l1 {
		t.Errorf("new session under the mapped folder: %q", got)
	}
	if got := read(here, "-home-zman-work-ferry/s1/subagents/agent-a.meta.json"); got != `{"agentType":"x"}` {
		t.Errorf("side file: %q", got)
	}
	if got := read(here, "-tmp-x/s9.jsonl"); got != l1 {
		t.Errorf("a half-written line came along: %q", got)
	}
	for _, rel := range []string{"-home-zman-work-ferry/memory/MEMORY.md", "-home-zman-work-ferry/.hidden.jsonl", "-Users-zman-work-ferry/s1.jsonl"} {
		if read(here, rel) != "<missing>" {
			t.Errorf("%s should not have been copied", rel)
		}
	}
	// The session goes on there: the new lines are appended here.
	put(t, mac, "-Users-zman-work-ferry/s1.jsonl", l1+l2)
	if r := sync(); r.Extended != 1 {
		t.Errorf("extend: %+v", r)
	}
	if got := read(here, "-home-zman-work-ferry/s1.jsonl"); got != l1+l2 {
		t.Errorf("after extending: %q", got)
	}
	// Then here: theirs is the beginning of ours, so nothing happens here.
	l3 := `{"type":"user","n":3,"on":"shadow"}` + "\n"
	put(t, here, "-home-zman-work-ferry/s1.jsonl", l1+l2+l3)
	if r := sync(); r != (syncResult{}) {
		t.Errorf("ours is longer: %+v", r)
	}
	// And then on both: the two differ, so theirs is kept beside ours.
	l3m := `{"type":"user","n":3,"on":"mac"}` + "\n"
	put(t, mac, "-Users-zman-work-ferry/s1.jsonl", l1+l2+l3m+l2)
	if r := sync(); r.Conflicts != 1 {
		t.Errorf("diverged: %+v", r)
	}
	if got := read(here, "-home-zman-work-ferry/s1.jsonl"); got != l1+l2+l3 {
		t.Errorf("ours was changed: %q", got)
	}
	if got := read(here, "-home-zman-work-ferry/s1.ferry-mac.jsonl"); got != l1+l2+l3m+l2 {
		t.Errorf("theirs not kept: %q", got)
	}
	// Their copy goes on: the kept copy follows it.
	put(t, mac, "-Users-zman-work-ferry/s1.jsonl", l1+l2+l3m+l2+l1)
	sync()
	if got := read(here, "-home-zman-work-ferry/s1.ferry-mac.jsonl"); got != l1+l2+l3m+l2+l1 {
		t.Errorf("kept copy not extended: %q", got)
	}
	// Diverged at exactly the same length: still noticed.
	put(t, here, "-home-zman-work-ferry/s5.jsonl", l1+`{"on":"shadow"}`+"\n")
	put(t, mac, "-Users-zman-work-ferry/s5.jsonl", l1+`{"on":"mac123"}`+"\n")
	sync()
	if read(here, "-home-zman-work-ferry/s5.ferry-mac.jsonl") != l1+`{"on":"mac123"}`+"\n" {
		t.Error("equal-length divergence not kept")
	}
	// Diverged and shorter there: kept too, ours untouched.
	put(t, here, "-home-zman-work-ferry/s2.jsonl", l1+l2+l3)
	put(t, mac, "-Users-zman-work-ferry/s2.jsonl", l1+l3m)
	sync()
	if read(here, "-home-zman-work-ferry/s2.jsonl") != l1+l2+l3 || read(here, "-home-zman-work-ferry/s2.ferry-mac.jsonl") != l1+l3m {
		t.Error("shorter diverged copy not kept beside ours")
	}
	// Nothing is ever deleted: files gone there stay here.
	os.RemoveAll(filepath.Join(mac, "-Users-zman-work-ferry"))
	sync()
	if read(here, "-home-zman-work-ferry/s1.jsonl") != l1+l2+l3 {
		t.Error("a file gone there was removed here")
	}
	// A copy of this device's own version, kept there, is not pulled back.
	put(t, mac, "-Users-zman-x/s3.ferry-shadow.jsonl", l1)
	sync()
	if read(here, "-home-zman-x/s3.ferry-shadow.jsonl") != "<missing>" {
		t.Error("our own diverged copy came back")
	}
	// A side file that exists here is never replaced.
	put(t, here, "-home-zman-y/s4/tool-results/r.txt", "ours")
	put(t, mac, "-Users-zman-y/s4/tool-results/r.txt", "theirs, longer")
	sync()
	if read(here, "-home-zman-y/s4/tool-results/r.txt") != "ours" {
		t.Error("side file replaced")
	}
}

func TestSyncCacheSkipsUnchanged(t *testing.T) {
	mac, here := t.TempDir(), t.TempDir()
	put(t, mac, "-Users-zman-a/s.jsonl", "1\n2\n")
	put(t, here, "-home-zman-a/s.jsonl", "1\n2\n3\n")
	cache := syncCache{}
	p := &countPeer{dirPeer: dirPeer{mac}}
	for i := 0; i < 3; i++ {
		syncSessions(context.Background(), p, "mac", "me", "/Users/zman", "/home/zman", here, cache)
	}
	if p.hashes != 1 {
		t.Errorf("hashed %d times for an unchanged pair, want 1", p.hashes)
	}
}

type countPeer struct {
	dirPeer
	hashes int
}

func (c *countPeer) Hashes(ctx context.Context, w map[string]int64) (map[string]string, error) {
	c.hashes++
	return c.dirPeer.Hashes(ctx, w)
}

func TestSyncable(t *testing.T) {
	for rel, want := range map[string]bool{
		"-home-zman/a.jsonl": true, "-home-zman/u/subagents/agent-x.meta.json": true, "-home-zman/u/tool-results/t.txt": true,
		"-home-zman/memory/MEMORY.md": false, "-home-zman/memory/x.jsonl": false, "-home-zman/.x.jsonl": false,
		"a.jsonl": false, "../x/a.jsonl": false, "-home-zman/a.sh": false, "-home-zman/../../a.jsonl": false,
		"-home-zman/a b.jsonl": false, "-home-zman/-rf.jsonl": false,
	} {
		if syncable(rel) != want {
			t.Errorf("%s: %v", rel, !want)
		}
	}
}

func TestParseSizeListAndHashes(t *testing.T) {
	m := parseSizeList("12 ./-home-zman/a.jsonl\n0 ./-home-zman/b.jsonl\njunk\n5 notdot\n")
	if len(m) != 2 || m["-home-zman/a.jsonl"] != 12 {
		t.Errorf("%v", m)
	}
	// The hash the shell computes there matches ours.
	dir := t.TempDir()
	put(t, dir, "f", "hello world\n")
	h := sha256.Sum256([]byte("hello"))
	got, _ := hashPrefix(filepath.Join(dir, "f"), 5)
	if got != hex.EncodeToString(h[:]) {
		t.Error("hashPrefix")
	}
}

func TestParseStats(t *testing.T) {
	var d devStatus
	err := parseStats("os linux\ncpus 16\nmem 33000000000 12000000000\nload 1.25\ngpu NVIDIA GeForce RTX 4070, 12282\n", &d)
	if err != nil || d.CPUs != 16 || d.MemTotal != 33e9 || d.MemFree != 12e9 || d.Load != 1.25 || len(d.GPUs) != 1 || d.GPUs[0].MemMB != 12282 || d.GPUs[0].Name != "NVIDIA GeForce RTX 4070" {
		t.Errorf("%+v %v", d, err)
	}
	// and the script runs here
	out, err := exec.Command("/bin/sh", "-c", statsScript).Output()
	var l devStatus
	if err != nil || parseStats(string(out), &l) != nil || l.CPUs < 1 || l.MemTotal <= 0 {
		t.Errorf("stats here: %q %v", out, err)
	}
}

func TestParsePorts(t *testing.T) {
	proc := "proc\n1000\n  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 0100007F:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 1 1\n" +
		"   1: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 2 1\n" +
		"   2: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3 1\n" +
		"   3: 0100007F:0BB8 0100007F:D431 01 00000000:00000000 00:00000000 00000000  1000        0 4 1\n"
	ls := parsePorts(proc)
	if len(ls) != 2 || ls[0].Port != 3000 || ls[0].Addr != "127.0.0.1" || ls[1].Port != 8080 {
		t.Errorf("proc: %+v", ls)
	}
	lsof := "lsof\nCOMMAND   PID USER   FD   TYPE DEVICE SIZE/OFF NODE NAME\n" +
		"node    123 zman   21u  IPv4 0x1      0t0  TCP 127.0.0.1:3000 (LISTEN)\n" +
		"python  456 zman   3u   IPv6 0x2      0t0  TCP *:8000 (LISTEN)\n" +
		"python  456 zman   4u   IPv6 0x2      0t0  TCP [::1]:8001 (LISTEN)\n"
	ls = parsePorts(lsof)
	if len(ls) != 3 || ls[0].Command != "node" || ls[1].Addr != "*" || ls[2].Addr != "::1" {
		t.Errorf("lsof: %+v", ls)
	}
}

func TestParseCwd(t *testing.T) {
	c, err := parseCwd(`"cwd":"/Users/zman/work/café"`)
	if err != nil || c != "/Users/zman/work/café" {
		t.Errorf("%q %v", c, err)
	}
}

// The ssh config gets one marked block per device, first, and loses it
// again when the device goes; nothing else in the file changes.
func TestMineConvergeBlocks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FERRY_HOME", filepath.Join(home, "ferry"))
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	orig := "Host *\n    ControlMaster no\n"
	os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte(orig), 0o600)
	(&mineState{Port: 4022}).save()
	dev := keys.Device{User: "zman", Home: "/Users/zman", OS: "darwin", SSHKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA", HostKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB"}
	k := keys.Public{Name: "mac", Sign: []byte("0123456789abcdef0123456789abcdef")}
	if _, err := recordMine([]mineMember{{Name: "mac", Key: k, Device: dev}}, keys.Public{Sign: []byte("self")}); err != nil {
		t.Fatal(err)
	}
	if err := mineConverge(nil); err != nil {
		t.Fatal(err)
	}
	mineConverge(nil) // twice: still one block
	b, _ := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	s := string(b)
	if strings.Count(s, "Host mac\n") != 1 || !strings.HasPrefix(s, mineMarkOpen+"mac\n") || !strings.HasSuffix(s, orig) {
		t.Errorf("config:\n%s", s)
	}
	for _, want := range []string{"ProxyCommand", " pipe mac", "StrictHostKeyChecking yes", "IdentitiesOnly yes", "ControlMaster auto", "User zman", "ForwardAgent no"} {
		if !strings.Contains(s, want) {
			t.Errorf("block lacks %q", want)
		}
	}
	ak, _ := os.ReadFile(minePath("authorized_keys"))
	if !strings.Contains(string(ak), dev.SSHKey+" ferry-mine:mac") || !strings.HasPrefix(string(ak), "no-agent-forwarding,no-X11-forwarding ") {
		t.Errorf("authorized_keys: %q", ak)
	}
	kh, _ := os.ReadFile(minePath("known_hosts"))
	if string(kh) != "mac "+dev.HostKey+"\n" {
		t.Errorf("known_hosts: %q", kh)
	}
	// A friend is never written anywhere.
	cs, _ := keys.LoadContacts()
	cs["sam"] = keys.Contact{Name: "sam", Key: keys.Public{Name: "sam", Sign: []byte("x")}, Paired: time.Now()}
	cs.Save()
	mineConverge(nil)
	if b, _ := os.ReadFile(filepath.Join(home, ".ssh", "config")); strings.Contains(string(b), "sam") {
		t.Error("a friend got a block")
	}
	// A block another ferry folder (FERRY_HOME) wrote is not ours to remove.
	foreign := mineMarkOpen + "laptop\n" + blockDirTag + "/somewhere/else\nHost laptop\n" + mineMarkEnd + "laptop\n\n"
	b, _ = os.ReadFile(filepath.Join(home, ".ssh", "config"))
	os.WriteFile(filepath.Join(home, ".ssh", "config"), append([]byte(foreign), b...), 0o600)
	forgetMine(func(c keys.Contact) bool { return c.Name == "mac" })
	b, _ = os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if string(b) != foreign+orig {
		t.Errorf("after rm:\n%q", b)
	}
	if ak, _ := os.ReadFile(minePath("authorized_keys")); len(ak) != 0 {
		t.Errorf("key left: %q", ak)
	}
	if cs, _ := keys.LoadContacts(); cs["sam"].Name != "sam" {
		t.Error("removing a device removed a friend")
	}
}

func TestSSHDConfigLocalOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FERRY_HOME", filepath.Join(home, "ferry"))
	writeMineSSHD(&mineState{Port: 4022})
	b, _ := os.ReadFile(minePath("sshd_config"))
	s := string(b)
	for _, want := range []string{"ListenAddress 127.0.0.1\n", "PasswordAuthentication no\n", "KbdInteractiveAuthentication no\n", "PermitRootLogin no\n", "AllowTcpForwarding local\n", "GatewayPorts no\n", "Port 4022\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("sshd_config lacks %q", want)
		}
	}
	if strings.Contains(s, "0.0.0.0") || strings.Contains(s, "ListenAddress ::") {
		t.Error("listens beyond loopback")
	}
}

// Owed messages: delivered ones go, offline ones wait, and one for a device
// that turns this one away at the handshake (it forgot us) is dropped.
func TestDeliverPending(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FERRY_HOME", filepath.Join(home, "ferry"))
	st := &mineState{}
	for _, n := range []string{"up", "away", "gone"} {
		queueOp(st, keys.Contact{Name: n, Key: keys.Public{Name: n, Sign: []byte(n + "-key-0123456789abcdef0123456789")}}, "mine-forget", forgetData{})
	}
	st.save()
	ask := func(ctx context.Context, c keys.Contact, in session.Intent) (session.IntentReply, error) {
		switch c.Name {
		case "up":
			return session.IntentReply{OK: true}, nil
		case "gone":
			return session.IntentReply{}, errors.New("failed to negotiate: stream reset")
		}
		return session.IntentReply{}, errors.New("offline: NO_RESERVATION")
	}
	sent, waiting := deliverPending(context.Background(), ask, t.Logf)
	st2, _ := loadMineState()
	if sent != 1 || waiting != 1 || len(st2.Pending) != 1 || st2.Pending[0].ToName != "away" {
		t.Errorf("sent %d waiting %d left %+v", sent, waiting, st2.Pending)
	}
}
