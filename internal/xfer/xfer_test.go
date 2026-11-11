package xfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZahakJ/ferry/internal/link"
	"github.com/ZahakJ/ferry/internal/linktest"
)

func writeRandom(t *testing.T, path string, size int) []byte {
	t.Helper()
	b := make([]byte, size)
	rand.Read(b)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return b
}

type result struct {
	saved string
	err   error
}

func transfer(t *testing.T, ctx context.Context, src, dest string, onProgress Progress) (sendErr error, recv result) {
	a, b := linktest.Pair(t, link.Options{Lanes: 3})
	m, err := Scan(src)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan result)
	go func() {
		o, err := ReadOffer(b)
		if err != nil {
			done <- result{err: err}
			return
		}
		s, err := Receive(ctx, b, o, dest, onProgress, nil)
		if err != nil {
			b.Close() // as a dying process would
		}
		done <- result{s, err}
	}()
	_, sendErr = Send(context.Background(), a, src, m, nil, nil)
	return sendErr, <-done
}

func TestDirectory(t *testing.T) {
	src := filepath.Join(t.TempDir(), "proj")
	big := writeRandom(t, filepath.Join(src, "big.bin"), 5*PieceSize+123)
	small := writeRandom(t, filepath.Join(src, "sub", "small.txt"), 10)
	os.WriteFile(filepath.Join(src, "empty"), nil, 0o600)
	os.MkdirAll(filepath.Join(src, "emptydir"), 0o755)
	os.Symlink("big.bin", filepath.Join(src, "link"))
	dest := t.TempDir()
	if err, r := transfer(t, context.Background(), src, dest, nil); err != nil || r.err != nil {
		t.Fatal(err, r.err)
	}
	for p, want := range map[string][]byte{"big.bin": big, "sub/small.txt": small, "empty": {}} {
		got, err := os.ReadFile(filepath.Join(dest, "proj", p))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if st, err := os.Stat(filepath.Join(dest, "proj", "emptydir")); err != nil || !st.IsDir() {
		t.Fatal("emptydir missing")
	}
	if _, err := os.Lstat(filepath.Join(dest, "proj", "link")); err == nil {
		t.Fatal("symlink should be skipped")
	}
	// Same name again lands beside, not on top.
	if err, r := transfer(t, context.Background(), src, dest, nil); err != nil || r.err != nil || filepath.Base(r.saved) != "proj-2" {
		t.Fatal(err, r)
	}
}

func TestResume(t *testing.T) {
	src := filepath.Join(t.TempDir(), "data.bin")
	want := writeRandom(t, src, 400*PieceSize)
	dest := t.TempDir()

	// First attempt dies part way through.
	ctx, cancel := context.WithCancel(context.Background())
	_, r := transfer(t, ctx, src, dest, func(done, total int64) {
		if done > total/3 {
			cancel()
		}
	})
	if r.err == nil {
		t.Fatal("first attempt should have been cut off")
	}
	if _, err := os.Stat(filepath.Join(dest, "data.bin.ferry-part")); err != nil {
		t.Fatal("partial file missing:", err)
	}

	// Second attempt picks up where it left off.
	var first int64 = -1
	err, r := transfer(t, context.Background(), src, dest, func(done, total int64) {
		if first < 0 {
			first = done
		}
	})
	if err != nil || r.err != nil {
		t.Fatal(err, r.err)
	}
	if first <= 0 {
		t.Fatalf("second attempt started from %d bytes; expected to resume", first)
	}
	got, _ := os.ReadFile(filepath.Join(dest, "data.bin"))
	if !bytes.Equal(got, want) {
		t.Fatal("content differs after resume")
	}
	if m, _ := filepath.Glob(filepath.Join(dest, ".ferry-*")); len(m) != 0 {
		t.Fatal("state left behind:", m)
	}
}

func TestCheckRejectsEscapes(t *testing.T) {
	for _, p := range []string{"../x", "/etc/passwd", "a/../../b", `a\..\b`, ""} {
		m := &Manifest{Name: "ok", Files: []File{{Path: "fine"}, {Path: p, Size: 1}}, Total: 1}
		if err := m.Check(); err == nil || !strings.Contains(err.Error(), "unsafe") {
			t.Errorf("%q: %v", p, err)
		}
	}
	for _, n := range []string{"..", "a/b", ".ferry-x", ""} {
		m := &Manifest{Name: n, Files: []File{{Path: "", Size: 0}}}
		if m.Check() == nil {
			t.Errorf("name %q accepted", n)
		}
	}
}

func TestCheckRejectsCollisions(t *testing.T) {
	bad := [][]File{
		{{Path: "a.ferry-part", Size: 1}, {Path: "a", Size: 1}}, // part-file clash
		{{Path: "x/a/../b", Size: 1}, {Path: "x/b", Size: 1}},   // two names, one file
		{{Path: "./b", Size: 1}},
		{{Path: "README", Size: 1}, {Path: "readme", Size: 1}}, // one file on macOS
		{{Path: "a", Size: 1 << 62}, {Path: "b", Size: 1 << 62}, {Path: "c", Size: 1 << 62}, {Path: "d", Size: 1 << 62}},
	}
	for i, fs := range bad {
		var total int64
		for _, f := range fs {
			total += f.Size
		}
		m := &Manifest{Name: "ok", Files: fs, Total: total}
		if m.Check() == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestDotRootRenamed(t *testing.T) {
	src := filepath.Join(t.TempDir(), ".git")
	writeRandom(t, filepath.Join(src, "config"), 10)
	dest := t.TempDir()
	if err, r := transfer(t, context.Background(), src, dest, nil); err != nil || r.err != nil {
		t.Fatal(err, r.err)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
		t.Fatal("a .git folder was created in the destination")
	}
	if _, err := os.Stat(filepath.Join(dest, "dot-git", "config")); err != nil {
		t.Fatal(err)
	}
}

// Many files must not mean many open files or goroutines.
func TestManyFiles(t *testing.T) {
	src := filepath.Join(t.TempDir(), "many")
	for i := 0; i < 3000; i++ {
		p := filepath.Join(src, fmt.Sprintf("d%02d", i%40), fmt.Sprintf("f%04d", i))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(fmt.Sprint(i)), 0o644)
	}
	dest := t.TempDir()
	if err, r := transfer(t, context.Background(), src, dest, nil); err != nil || r.err != nil {
		t.Fatal(err, r.err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "many", "d17", "f2017"))
	if err != nil || string(got) != "2017" {
		t.Fatal(string(got), err)
	}
}

// transferAs is transfer with receive options.
func transferAs(t *testing.T, src, dest string, opts ...ReceiveOption) (sendErr error, recv result) {
	a, b := linktest.Pair(t, link.Options{Lanes: 2})
	m, err := Scan(src)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan result)
	go func() {
		o, err := ReadOffer(b)
		if err != nil {
			done <- result{err: err}
			return
		}
		s, err := Receive(context.Background(), b, o, dest, nil, nil, opts...)
		if err != nil {
			b.Close()
		}
		done <- result{s, err}
	}()
	_, sendErr = Send(context.Background(), a, src, m, nil, nil)
	return sendErr, <-done
}

// A folder from a contact can hold files that tools and coding agents act
// on by name, at any depth. None of them keeps its name, and nothing
// received is executable, unless the receiver asks for the files as sent.
func TestNestedControlFilesRenamed(t *testing.T) {
	src := filepath.Join(t.TempDir(), "proj")
	for _, p := range []string{"main.go", ".git/config", ".git/hooks/pre-commit", ".claude/settings.json", ".envrc", ".vscode/tasks.json", "CLAUDE.md", "sub/AGENTS.md", "sub/.envrc"} {
		writeRandom(t, filepath.Join(src, filepath.FromSlash(p)), 10)
	}
	os.Chmod(filepath.Join(src, ".git", "hooks", "pre-commit"), 0o755)
	os.Chmod(filepath.Join(src, "main.go"), 0o755)

	dest := t.TempDir()
	if err, r := transferAs(t, src, dest); err != nil || r.err != nil {
		t.Fatal(err, r.err)
	}
	root := filepath.Join(dest, "proj")
	for _, p := range []string{".git", ".claude", ".envrc", ".vscode", "CLAUDE.md", "sub/AGENTS.md", "sub/.envrc"} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p))); err == nil {
			t.Errorf("%s was saved under its own name", p)
		}
	}
	for _, p := range []string{"main.go", "dot-git/config", "dot-git/hooks/pre-commit", "dot-claude/settings.json", "dot-envrc", "dot-vscode/tasks.json", "received-CLAUDE.md", "sub/received-AGENTS.md", "sub/dot-envrc"} {
		st, err := os.Stat(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if st.Mode().Perm()&0o111 != 0 {
			t.Errorf("%s is executable: %v", p, st.Mode())
		}
	}
	if st, err := os.Stat(filepath.Join(root, "dot-git", "hooks")); err != nil || st.Mode().Perm()&0o700 != 0o700 {
		t.Errorf("folders stay usable: %v %v", st, err)
	}

	// --as-sent: names and execute bits as the sender had them (the top
	// level is still renamed, which this folder does not need).
	dest = t.TempDir()
	if err, r := transferAs(t, src, dest, AsSent()); err != nil || r.err != nil {
		t.Fatal(err, r.err)
	}
	root = filepath.Join(dest, "proj")
	for _, p := range []string{".git/config", ".claude/settings.json", ".envrc", "CLAUDE.md", "sub/AGENTS.md"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(p))); err != nil {
			t.Errorf("as sent: %s: %v", p, err)
		}
	}
	if st, err := os.Stat(filepath.Join(root, ".git", "hooks", "pre-commit")); err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Errorf("as sent: the execute bit was dropped: %v %v", st, err)
	}
}

// .git and dot-git in one folder would both be saved as dot-git: refused.
func TestRenamedNamesCollide(t *testing.T) {
	src := filepath.Join(t.TempDir(), "proj")
	writeRandom(t, filepath.Join(src, ".git", "config"), 10)
	writeRandom(t, filepath.Join(src, "dot-git", "config"), 10)
	dest := t.TempDir()
	_, r := transferAs(t, src, dest)
	if r.err == nil || !strings.Contains(r.err.Error(), "saved as one") {
		t.Fatal("expected a refusal, got", r.err)
	}
	if _, err := os.Stat(filepath.Join(dest, "proj")); err == nil {
		t.Fatal("a refused transfer left files behind")
	}
}

func TestSavedName(t *testing.T) {
	for in, want := range map[string]string{
		".git": "dot-git", "..env": "dot-env", "CLAUDE.md": "received-CLAUDE.md", "claude.md": "received-claude.md",
		"AGENTS.md": "received-AGENTS.md", "CLAUDE.local.md": "received-CLAUDE.local.md", "README.md": "README.md", "dot-git": "dot-git",
	} {
		if got := SavedName(in); got != want {
			t.Errorf("SavedName(%q) = %q, want %q", in, got, want)
		}
	}
}
