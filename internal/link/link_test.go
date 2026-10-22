package link_test

import (
	"context"
	"testing"

	"github.com/ZahakJ/ferry/internal/link"
	"github.com/ZahakJ/ferry/internal/linktest"
)

func TestLink(t *testing.T) {
	a, b := linktest.Pair(t, link.Options{Lanes: 4})
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
	msg := make([]byte, link.MaxMessage)
	for i, ln := range a.Lanes {
		msg[0] = byte(i)
		if err := ln.Write(ctx, msg); err != nil {
			t.Fatal(err)
		}
	}
	for i, ln := range b.Lanes {
		big := make([]byte, link.MaxMessage)
		n, err := ln.RW.Read(big)
		if err != nil || n != link.MaxMessage || big[0] != byte(i) {
			t.Fatal(i, n, err)
		}
	}
}
