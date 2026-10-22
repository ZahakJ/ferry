package link_test

import (
	"testing"
	"time"

	"github.com/ZahakJ/ferry/internal/link"
	"github.com/ZahakJ/ferry/internal/linktest"
)

// Setting up should take milliseconds. It once took seconds, at random,
// when ICE was offered every container bridge on the machine.
func TestSetupTime(t *testing.T) {
	for i := 0; i < 10; i++ {
		t0 := time.Now()
		a, b := linktest.Pair(t, link.Options{Lanes: 3})
		if d := time.Since(t0); d > 2*time.Second {
			t.Errorf("setup %d took %v", i, d)
		}
		a.Close()
		b.Close()
	}
}
