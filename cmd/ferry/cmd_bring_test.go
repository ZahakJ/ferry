package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestClaudeProjectKey(t *testing.T) {
	long := "/home/av/" + strings.Repeat("very_long-dir.name/", 14) + "é😀end"
	for in, want := range map[string]string{
		// real pairs from ~/.claude/projects (the cwd inside a session, and its folder)
		"/home/zman/work/tendon/.tendon/go/tmteib50g/w1":                             "-home-zman-work-tendon--tendon-go-tmteib50g-w1",
		"/home/zman/.heddle/repo-8889c58171d51fa7/worktrees/thread-1785482701286-47": "-home-zman--heddle-repo-8889c58171d51fa7-worktrees-thread-1785482701286-47",
		"/home/zman/.local/share/aether":                                             "-home-zman--local-share-aether",
		"/home/avicenna/proj":                                                        "-home-avicenna-proj",
		// from Claude Code's own function, run under node
		"/home/a/b_c.d é😀": "-home-a-b-c-d----",
		long:               "-home-av-very-long-dir-name-very-long-dir-name-very-long-dir-name-very-long-dir-name-very-long-dir-name-very-long-dir-name-very-long-dir-name-very-long-dir-name-very-long-dir-name-very-long-dir-name-v-if0yew",
	} {
		if got := claudeProjectKey(in); got != want {
			t.Errorf("%q:\n got %s\nwant %s", in, got, want)
		}
	}
}

func TestSecretFile(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[remote \"o\"]\n\turl = https://x-access-token:ghp_abc@github.com/a/b\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "clean", ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, "clean", ".git", "config"), []byte("[remote \"o\"]\n\turl = git@github.com:a/b\n"), 0o644)
	for rel, want := range map[string]bool{
		".env": true, ".env.local": true, "sub/.envrc": true, "cert.pem": true, "id_rsa": true, "id_ed25519.pub": true,
		".netrc": true, ".git-credentials": true, "credentials.json": true, "gcp/credentials-prod.json": true, ".npmrc": true,
		".git/config": true, "clean/.git/config": false,
		"main.go": false, "id_utils.go": false, "environment.md": false, "credentials.go": false, "README.md": false,
	} {
		if got := secretFile(rel, filepath.Join(dir, rel)); got != want {
			t.Errorf("%s: %v, want %v", rel, got, want)
		}
	}
}

func TestGitProtected(t *testing.T) {
	for rel, want := range map[string]bool{
		".git/config": true, ".git/hooks/pre-commit": true, ".git/hooks": true, ".GIT/Config": true,
		"sub/.git/config": true, ".git/modules/x/config": true, ".git/modules/x/hooks/post-checkout": true,
		".git/worktrees/w/config.worktree": true, ".git/config.worktree": true,
		".git/objects/ab/cdef": false, ".git/refs/heads/main": false, ".git/HEAD": false, ".git/index": false,
		"src/config": false, "hooks/x": false, "config": false,
	} {
		if got := gitProtected(rel, 'f'); got != want {
			t.Errorf("%s: %v, want %v", rel, got, want)
		}
	}
	if !gitProtected("sub/.git", 'f') { // a .git file pointing git somewhere else
		t.Error(".git file not protected")
	}
	if gitProtected(".git", 'd') {
		t.Error(".git folder itself protected")
	}
}

func TestCleanRelAndLinks(t *testing.T) {
	for in, ok := range map[string]bool{
		"a": true, "./a/b": true, "a/b/": true,
		"": false, ".": false, "..": false, "../a": false, "a/../../b": false, "/etc/passwd": false, "a//b": false, "a\\b": false,
	} {
		if _, got := cleanRel(in); got != ok {
			t.Errorf("cleanRel(%q) = %v", in, got)
		}
	}
	for _, c := range []struct {
		rel, target string
		ok          bool
	}{
		{"a", "b", true}, {"d/a", "../b", true}, {"d/e/a", "../../b", true},
		{"a", "../b", false}, {"d/a", "../../b", false}, {"a", "/etc/passwd", false}, {"a", "", false},
	} {
		if got := linkInside(c.rel, c.target); got != c.ok {
			t.Errorf("linkInside(%q, %q) = %v", c.rel, c.target, got)
		}
	}
}

func TestParseBoxList(t *testing.T) {
	raw := "ok\x00" +
		"f 5 1700000000.75 a.txt\x00\x00" +
		"d 4096 1700000000.0 sub\x00\x00" +
		"l 3 1700000001.0 sub/link\x00../a.txt\x00" +
		"f 9 1700000002.0 sub/with space.txt\x00\x00" +
		"f 1 1.0 ../escape\x00\x00" + // refused
		"f 1 1.0 /abs\x00\x00" + // refused
		"x 1 1.0 weird\x00\x00" + // refused
		"f -1 1.0 neg\x00\x00" + // refused
		"f 1 NaN nan\x00\x00" // refused
	m, ok, err := parseBoxList([]byte(raw))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	var got []string
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	if want := []string{"a.txt", "sub", "sub/link", "sub/with space.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if m["a.txt"].mtime != 1700000000 || m["sub/link"].link != "../a.txt" {
		t.Fatalf("%+v %+v", m["a.txt"], m["sub/link"])
	}
	if _, ok, _ := parseBoxList(nil); ok {
		t.Fatal("missing folder reported as there")
	}
}

func TestPlanSend(t *testing.T) {
	local := []treeEntry{
		{rel: "new", typ: 'f', size: 1, mtime: 10},
		{rel: "same", typ: 'f', size: 2, mtime: 10},
		{rel: "older-there", typ: 'f', size: 3, mtime: 20},
		{rel: "newer-there", typ: 'f', size: 4, mtime: 10},
		{rel: "dir", typ: 'd'},
		{rel: "dir/x", typ: 'f', size: 5, mtime: 10},
	}
	box := map[string]treeEntry{
		"same":        {typ: 'f', size: 2, mtime: 10},
		"older-there": {typ: 'f', size: 3, mtime: 15},
		"newer-there": {typ: 'f', size: 4, mtime: 11},
		"dir":         {typ: 'f', size: 1, mtime: 1}, // a file where we have a folder
	}
	p := planSend(local, box)
	var sent []string
	for _, e := range p.send {
		sent = append(sent, e.rel)
	}
	if !reflect.DeepEqual(sent, []string{"new", "older-there"}) || p.same != 1 ||
		!reflect.DeepEqual(p.newerThere, []string{"newer-there"}) || !reflect.DeepEqual(p.conflicts, []string{"dir"}) {
		t.Fatalf("%+v", p)
	}
}

// tarOf builds what a (possibly hostile) box might send.
func tarOf(t *testing.T, hs ...*tar.Header) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range hs {
		body := []byte(strings.Repeat("x", int(h.Size)))
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write(body)
	}
	tw.Close()
	return &b
}

func TestTakeRefusesHostileBox(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "proj")
	outside := filepath.Join(base, "outside")
	os.MkdirAll(root, 0o755)
	os.MkdirAll(outside, 0o755)
	os.Symlink(outside, filepath.Join(root, "evil")) // a symlink here into somewhere else
	os.WriteFile(filepath.Join(root, "newer.txt"), []byte("mine"), 0o644)
	future := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(root, "newer.txt"), future, future)
	when := time.Now().Add(-time.Hour).Truncate(time.Second)
	want := map[string]treeEntry{
		"ok.txt":       {typ: 'f', size: 3},
		"sub/deep.txt": {typ: 'f', size: 2},
		"evil/x":       {typ: 'f', size: 1},
		"newer.txt":    {typ: 'f', size: 5},
		"run.sh":       {typ: 'f', size: 1},
		"sized":        {typ: 'f', size: 1},
		"badlink":      {typ: 'l'},
	}
	tb := tarOf(t,
		&tar.Header{Name: "./ok.txt", Size: 3, Mode: 0o4755, ModTime: when}, // setuid stripped
		&tar.Header{Name: "./sub/deep.txt", Size: 2, Mode: 0o644, ModTime: when},
		&tar.Header{Name: "./evil/x", Size: 1, Mode: 0o644, ModTime: when},     // would write through the symlink
		&tar.Header{Name: "../outside/y", Size: 1, Mode: 0o644, ModTime: when}, // escape
		&tar.Header{Name: "/etc/z", Size: 1, Mode: 0o644, ModTime: when},       // absolute
		&tar.Header{Name: "./unasked", Size: 1, Mode: 0o644, ModTime: when},    // never asked for
		&tar.Header{Name: "./newer.txt", Size: 5, Mode: 0o644, ModTime: when},  // older than ours
		&tar.Header{Name: "./run.sh", Size: 1, Mode: 0o755, ModTime: when},
		&tar.Header{Name: "./ok.txt", Size: 3, Mode: 0o644, ModTime: when}, // twice
		&tar.Header{Name: "./sized", Size: 9, Mode: 0o644, ModTime: when},  // not the size it listed
		&tar.Header{Name: "./badlink", Typeflag: tar.TypeSymlink, Linkname: "/home/me/.ssh/id_rsa", ModTime: when},
		&tar.Header{Name: "./dev", Typeflag: tar.TypeChar, ModTime: when},
	)
	res, err := extractTake(tb, root, want, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if res.files != 3 {
		t.Errorf("wrote %d files, want 3 (ok.txt, sub/deep.txt, run.sh)", res.files)
	}
	if fi, err := os.Stat(filepath.Join(root, "ok.txt")); err != nil || fi.Mode().Perm() != 0o755 || fi.Mode()&os.ModeSetuid != 0 || !fi.ModTime().Equal(when) {
		t.Errorf("ok.txt: %v %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(root, "run.sh")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("run.sh: %v %v", fi, err)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Errorf("wrote outside the folder: %v", ents)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "newer.txt")); string(b) != "mine" {
		t.Errorf("overwrote a newer file: %q", b)
	}
	if !reflect.DeepEqual(res.newerHere, []string{"newer.txt"}) {
		t.Errorf("newerHere %v", res.newerHere)
	}
	for _, p := range []string{"unasked", "badlink", "dev", "sized"} {
		if _, err := os.Lstat(filepath.Join(root, p)); err == nil {
			t.Errorf("%s was written", p)
		}
	}
	if len(res.refused) < 7 {
		t.Errorf("refused %v", res.refused)
	}
}

func TestBringTakeRoundTrip(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "a", "b"), 0o755)
	os.MkdirAll(filepath.Join(src, "node_modules", "x"), 0o755)
	os.MkdirAll(filepath.Join(src, ".ssh"), 0o755)
	os.WriteFile(filepath.Join(src, "a", "b", "c.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(src, ".env"), []byte("SECRET=1"), 0o600)
	os.WriteFile(filepath.Join(src, "node_modules", "x", "i.js"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(src, ".ssh", "k"), []byte("k"), 0o600)
	os.Symlink("a/b/c.txt", filepath.Join(src, "link"))
	es, secrets, excluded, err := walkLocal(src, walkOpts{project: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(secrets, []string{".env", ".ssh/"}) || !reflect.DeepEqual(excluded, []string{"node_modules/"}) {
		t.Fatalf("secrets %v excluded %v", secrets, excluded)
	}
	var tb bytes.Buffer
	if err := writeTar(&tb, src, es); err != nil {
		t.Fatal(err)
	}
	// What ferry bring sends is what ferry take accepts back.
	want := map[string]treeEntry{}
	for _, e := range es {
		want[e.rel] = e
	}
	dst := t.TempDir()
	res, err := extractTake(&tb, dst, want, 0o644)
	if err != nil || len(res.refused) != 0 {
		t.Fatal(err, res.refused)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a", "b", "c.txt")); string(b) != "hello" {
		t.Fatalf("c.txt %q", b)
	}
	if l, _ := os.Readlink(filepath.Join(dst, "link")); l != "a/b/c.txt" {
		t.Fatalf("link %q", l)
	}
	for _, p := range []string{".env", ".ssh", "node_modules"} {
		if _, err := os.Lstat(filepath.Join(dst, p)); err == nil {
			t.Errorf("%s came along", p)
		}
	}
	// Taking into the same folder again: everything is up to date.
	box := map[string]treeEntry{}
	for _, e := range es {
		box[e.rel] = e
	}
	if p := planTake(dst, box); len(p.order) != 0 || p.same != 2 {
		t.Fatalf("second take: %+v", p)
	}
}

func TestValidHisHome(t *testing.T) {
	for h, ok := range map[string]bool{"/home/avicenna": true, "/Users/Zahak.J": true, "/home/a_b-c": true,
		"/home/friend/x": false, "/home/": false, "/home/..": false, "/root": false, "/home/a b": false, "/var/home/a": false} {
		if validHisHome(h) != ok {
			t.Errorf("%s: %v", h, !ok)
		}
	}
}
