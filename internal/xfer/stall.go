package xfer

import (
	"errors"
	"sync/atomic"
	"time"
)

// StallAfter is how long a transfer may go without moving a byte, or
// without a word from the other side, before ferry stops it and says why.
// Nothing is lost: running the same send again resumes where it stopped.
var StallAfter = 60 * time.Second

// acceptWait is how long the sender waits for the receiver to answer an
// offer. A receiver with a million files has more to prepare, so this is
// longer than StallAfter.
func acceptWait() time.Duration { return 2 * StallAfter }

// ErrStalled means a transfer stopped moving and was given up.
var ErrStalled = errors.New("transfer stalled")

// Status is told what a transfer is doing whenever that changes, in a few
// words fit for a status line: "waiting for the receiver to accept".
type Status func(what string)

// Phases Send reports.
const (
	StatusAccept  = "waiting for the receiver to accept"
	StatusSending = "sending"
)

func (s Status) say(what string) {
	if s != nil {
		s(what)
	}
}

// clock is a time that goroutines can set and read.
type clock struct{ ns atomic.Int64 }

func (c *clock) mark()                { c.ns.Store(time.Now().UnixNano()) }
func (c *clock) since() time.Duration { return time.Since(time.Unix(0, c.ns.Load())) }
func newClock() *clock                { c := &clock{}; c.mark(); return c }
func (c *clock) newer(d *clock) *clock {
	if c.ns.Load() >= d.ns.Load() {
		return c
	}
	return d
}

// round shows a duration as people say it: 60s, 2m0s.
func round(d time.Duration) time.Duration {
	if d >= time.Second {
		return d.Round(time.Second)
	}
	return d.Round(time.Millisecond)
}
