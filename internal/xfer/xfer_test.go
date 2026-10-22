package xfer

import (
	"bytes"
	"context"
	"crypto/rand"
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
	_, sendErr = Send(context.Background(), a, src, m, nil)
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
