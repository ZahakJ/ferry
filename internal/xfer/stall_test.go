package xfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZahakJ/ferry/internal/link"
	"github.com/ZahakJ/ferry/internal/linktest"
)

// quick makes the watchdog fire in test time.
func quick(t *testing.T, d time.Duration) {
	old := StallAfter
	StallAfter = d
	t.Cleanup(func() { StallAfter = old })
}

// statuses records what Send says it is doing.
type statuses struct {
	mu  sync.Mutex
	got []string
}

func (s *statuses) add(what string) { s.mu.Lock(); s.got = append(s.got, what); s.mu.Unlock() }
func (s *statuses) has(what string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.got {
		if g == what {
			return true
		}
	}
	return false
}

// sendStalled runs Send against a receiver played by peer, and checks
// that it gives up with ErrStalled, soon, saying why.
func sendStalled(t *testing.T, peer func(b *link.Link), why string) *statuses {
	t.Helper()
	quick(t, 300*time.Millisecond)
	src := filepath.Join(t.TempDir(), "big folder")
	writeRandom(t, filepath.Join(src, "a b.pak"), 8*PieceSize)
	m, err := Scan(src)
	if err != nil {
		t.Fatal(err)
	}
	a, b := linktest.Pair(t, link.Options{Lanes: 2})
	go peer(b)
	st := &statuses{}
	t0 := time.Now()
	_, err = Send(context.Background(), a, src, m, nil, st.add)
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("want ErrStalled, got %v", err)
	}
	if !strings.Contains(err.Error(), why) {
		t.Fatalf("error does not say why (%q): %v", why, err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("took %v to notice", d)
	}
	if !st.has("waiting for the receiver to accept") {
		t.Fatalf("no waiting status: %q", st.got)
	}
	return st
}

// A receiver that never answers the offer.
func TestStallNoAccept(t *testing.T) {
	sendStalled(t, func(b *link.Link) { ReadOffer(b) }, "has not answered the offer")
}

// A receiver that accepts and keeps talking but takes no data.
func TestStallNoData(t *testing.T) {
	st := sendStalled(t, func(b *link.Link) {
		if _, err := ReadOffer(b); err != nil {
			return
		}
		b.SendCtl(ctl{T: "accept"})
		for {
			if b.SendCtl(ctl{T: "progress", Bytes: 0}) != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}, "taking no more")
	if !st.has("sending") {
		t.Fatalf("no sending status: %q", st.got)
	}
}

// A receiver that accepts and then goes silent.
func TestStallSilentReceiver(t *testing.T) {
	sendStalled(t, func(b *link.Link) {
		if _, err := ReadOffer(b); err == nil {
			b.SendCtl(ctl{T: "accept"})
		}
	}, "gone quiet")
}

// A sender that stops sending must not hold the receiver open forever,
// and what did arrive is kept for a resume.
func TestReceiverStall(t *testing.T) {
	quick(t, 200*time.Millisecond)
	src := filepath.Join(t.TempDir(), "data.bin")
	writeRandom(t, src, 4*PieceSize)
	m, err := Scan(src)
	if err != nil {
		t.Fatal(err)
	}
	a, b := linktest.Pair(t, link.Options{Lanes: 2})
	heard := make(chan ctl, 16)
	go func() {
		a.SendCtl(ctl{T: "offer", ID: m.ID(), Name: m.Name, Total: m.Total, NFiles: len(m.Files)})
		a.SendCtl(ctl{T: "files", Files: m.Files})
		for {
			c, err := readCtl(a)
			if err != nil {
				return
			}
			heard <- c
		}
	}()
	o, err := ReadOffer(b)
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	t0 := time.Now()
	_, err = Receive(context.Background(), b, o, dest, nil, nil)
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("want ErrStalled, got %v", err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("took %v to notice", d)
	}
	if _, err := os.Stat(statePath(dest, o.ID)); err != nil {
		t.Fatal("resume state not kept:", err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case c := <-heard:
			if c.T == "cancel" && strings.Contains(c.Reason, "nothing has arrived") {
				return
			}
		case <-deadline:
			t.Fatal("the sender was not told why")
		}
	}
}

// A watchdog this impatient must still leave healthy transfers alone,
// including a resume, where the sender reads pieces it does not send.
// The receiver reports progress every 250ms and fsyncs a checkpoint
// between reports, so the window must sit well above that cadence or a
// busy machine reads as a stall.
func TestNoFalseStall(t *testing.T) {
	quick(t, 2*time.Second)
	src := filepath.Join(t.TempDir(), "data.bin")
	writeRandom(t, src, 200*PieceSize)
	dest := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	transfer(t, ctx, src, dest, func(done, total int64) {
		if done > total/2 {
			cancel()
		}
	})
	if err, r := transfer(t, context.Background(), src, dest, nil); err != nil || r.err != nil {
		t.Fatal(err, r.err)
	}
}
