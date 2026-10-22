package link

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/ZahakJ/ferry/internal/secure"
)

// Pair opens a link between two goroutines over loopback.
func Pair(t testing.TB, o Options) (a, b *Link) {
	t.Helper()
	x, y := net.Pipe()
	k := make([]byte, 32)
	sa, _ := secure.FromPAKE(x, k, nil, true)
	sb, _ := secure.FromPAKE(y, k, nil, false)
	o.Loopback = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var eb error
	wg.Add(1)
	go func() { defer wg.Done(); b, eb = Open(ctx, sb, false, o) }()
	a, ea := Open(ctx, sa, true, o)
	wg.Wait()
	if ea != nil || eb != nil {
		t.Fatal(ea, eb)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func TestLink(t *testing.T) {
	a, b := Pair(t, Options{Lanes: 4})
	if len(a.Lanes) != 4 || len(b.Lanes) != 4 {
		t.Fatalf("lanes %d %d", len(a.Lanes), len(b.Lanes))
	}
	if a.Path != "direct" {
		t.Errorf("path %q", a.Path)
	}
	if err := a.SendCtl(map[string]string{"hi": "there"}); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := b.Ctl.Read(buf)
	if err != nil || string(buf[:n]) != `{"hi":"there"}` {
		t.Fatal(string(buf[:n]), err)
	}
	ctx := context.Background()
	msg := make([]byte, MaxMessage)
	for i, ln := range a.Lanes {
		msg[0] = byte(i)
		if err := ln.Write(ctx, msg); err != nil {
			t.Fatal(err)
		}
	}
	for i, ln := range b.Lanes {
		big := make([]byte, MaxMessage)
		n, err := ln.RW.Read(big)
		if err != nil || n != MaxMessage || big[0] != byte(i) {
			t.Fatal(i, n, err)
		}
	}
}
