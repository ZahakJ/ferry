// Package linktest opens links between two goroutines for tests.
package linktest

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/ZahakJ/ferry/internal/link"
	"github.com/ZahakJ/ferry/internal/secure"
)

// Pair opens a link over loopback and closes it when the test ends.
func Pair(t testing.TB, o link.Options) (a, b *link.Link) {
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
	go func() { defer wg.Done(); b, eb = link.Open(ctx, sb, false, o) }()
	a, ea := link.Open(ctx, sa, true, o)
	wg.Wait()
	if ea != nil || eb != nil {
		t.Fatal(ea, eb)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}
