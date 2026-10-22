package rv

import (
	"context"
	"io"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ZahakJ/ferry/internal/rvtest"
	"github.com/libp2p/go-libp2p/core/network"
)

// TestRelayBulk measures bulk data through the rendezvous's circuit relay,
// the fallback for peers that cannot reach each other directly. Set
// FERRY_RELAY_BULK to the megabytes to send; FERRY_RELAY_STREAMS (default 1)
// splits them over that many streams on the one circuit.
func TestRelayBulk(t *testing.T) {
	mb, _ := strconv.Atoi(os.Getenv("FERRY_RELAY_BULK"))
	if mb == 0 {
		t.Skip("FERRY_RELAY_BULK not set")
	}
	streams, _ := strconv.Atoi(os.Getenv("FERRY_RELAY_STREAMS"))
	streams = max(streams, 1)
	addr, _ := rvtest.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	a, err := Dial(ctx, addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Dial(ctx, addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Reserve(ctx); err != nil {
		t.Fatal(err)
	}
	var got sync.WaitGroup
	got.Add(streams)
	b.Handle("/ferry/bulk", func(s network.Stream) {
		defer got.Done()
		io.Copy(io.Discard, s)
		s.Close()
	})

	per := int64(mb) << 20 / int64(streams)
	buf := make([]byte, 256<<10)
	t0 := time.Now()
	var sent sync.WaitGroup
	for range streams {
		s, err := a.Open(ctx, b.Host.ID(), "/ferry/bulk")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("conn %s", s.Conn().RemoteMultiaddr())
		sent.Add(1)
		go func() {
			defer sent.Done()
			for left := per; left > 0; left -= int64(len(buf)) {
				if _, err := s.Write(buf[:min(left, int64(len(buf)))]); err != nil {
					t.Error(err)
					return
				}
			}
			s.CloseWrite()
		}()
	}
	sent.Wait()
	got.Wait()
	d := time.Since(t0)
	t.Logf("%d MB over %d stream(s) in %s: %.0f Mbit/s", mb, streams, d.Round(time.Millisecond), float64(mb)*8.388608/d.Seconds())
}
