package rv

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/ZahakJ/ferry/internal/rvtest"
	"github.com/libp2p/go-libp2p/core/network"
)

func TestRendezvous(t *testing.T) {
	addr, _ := rvtest.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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

	// b waits on a slot; a finds it and talks to it through the relay.
	if err := b.Reserve(ctx); err != nil {
		t.Fatal(err)
	}
	b.Handle("/ferry/test", func(s network.Stream) {
		io.Copy(s, s)
		s.Close()
	})
	if err := b.Announce(ctx, "42"); err != nil {
		t.Fatal(err)
	}
	ids, err := a.Lookup(ctx, "42")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != b.Host.ID() {
		t.Fatalf("lookup got %v, want %v", ids, b.Host.ID())
	}
	s, err := a.Open(ctx, b.Host.ID(), "/ferry/test")
	if err != nil {
		t.Fatal(err)
	}
	s.Write([]byte("hello"))
	s.CloseWrite()
	got, _ := io.ReadAll(s)
	if string(got) != "hello" {
		t.Fatalf("echo got %q", got)
	}

	// Mailbox: b registers, a deposits, b drains and acks.
	if err := b.Register(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Deposit(ctx, b.Host.ID(), []byte("sealed"), time.Hour); err != nil {
		t.Fatal(err)
	}
	env, err := b.Drain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || !bytes.Equal(env[0].Data, []byte("sealed")) {
		t.Fatalf("drain got %v", env)
	}
	if err := b.Ack(ctx, []string{env[0].ID}); err != nil {
		t.Fatal(err)
	}
	if env, _ := b.Drain(ctx); len(env) != 0 {
		t.Fatalf("after ack still %d envelopes", len(env))
	}
}
