package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// A box's reply to pollCommand: `id -u`, then /proc/net/tcp and tcp6.
const pollReply = `2000
  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000  2000        0 1 1 0000000000000000 100 0 0 10 0
   1: 00000000:1F40 00000000:0000 0A 00000000:00000000 00:00000000 00000000  2000        0 2 1 0000000000000000 100 0 0 10 0
   2: 00000000:08AE 00000000:0000 0A 00000000:00000000 00:00000000 00000000  2000        0 3 1 0000000000000000 100 0 0 10 0
   3: 0100007F:0438 00000000:0000 0A 00000000:00000000 00:00000000 00000000  2000        0 4 1 0000000000000000 100 0 0 10 0
   4: 0100007F:0898 00000000:0000 0A 00000000:00000000 00:00000000 00000000  2000        0 5 1 0000000000000000 100 0 0 10 0
   5: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000  2000        0 6 1 0000000000000000 100 0 0 10 0
   6: 0100007F:9C40 00000000:0000 0A 00000000:00000000 00:00000000 00000000  2000        0 7 1 0000000000000000 100 0 0 10 0
   7: 0100007F:0FA0 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 8 1 0000000000000000 100 0 0 10 0
   8: 0100007F:1776 0100007F:D431 01 00000000:00000000 00:00000000 00000000  2000        0 9 1 0000000000000000 100 0 0 10 0
   9: 0201A8C0:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  2000        0 10 1 0000000000000000 100 0 0 10 0
  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:1435 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  2000        0 11 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000000000000:2328 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  2000        0 12 1 0000000000000000 100 0 0 10 0
   2: 00000000000000000000000001000000:0BB8 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  2000        0 13 1 0000000000000000 100 0 0 10 0
   3: 0000000000000000FFFF00000100007F:1F41 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  2000        0 14 1 0000000000000000 100 0 0 10 0
   4: 00000000000000000000000001000000:1068 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  2000        0 15 1 0000000000000000 100 0 0 10 0
   5: 00000000000000000000000000000000:1068 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  2000        0 16 1 0000000000000000 100 0 0 10 0
`

func TestParseListeners(t *testing.T) {
	got, err := parseListeners(pollReply)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{
		3000: "127.0.0.1", // tcp 127.0.0.1, and ::1 too: v4 wins
		8000: "127.0.0.1", // 0.0.0.0
		5173: "::1",       // ::1 only
		9000: "127.0.0.1", // :: (dual stack)
		8001: "127.0.0.1", // ::ffff:127.0.0.1
		4200: "127.0.0.1", // ::1 then ::
		// not: 2222 1080 2200 (the box's plumbing), 80 (privileged), 40000
		// (ephemeral), 4000 (another uid), 6006 (not LISTEN), 8080 (a LAN address)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestParseListenersBadReply(t *testing.T) {
	for _, s := range []string{"", "\n\n", "Permission denied\n", "bash: id: command not found\n"} {
		if _, err := parseListeners(s); err == nil {
			t.Errorf("%q: no error", s)
		}
	}
	got, err := parseListeners("2000\n") // no /proc/net/tcp6, nothing listening
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestSpec(t *testing.T) {
	if s := (boxPort{Box: 3000, Local: 3001, Target: "127.0.0.1"}).spec(); s != "127.0.0.1:3001:127.0.0.1:3000" {
		t.Error(s)
	}
	if s := (boxPort{Box: 5173, Local: 5173, Target: "::1"}).spec(); s != "127.0.0.1:5173:[::1]:5173" {
		t.Error(s)
	}
}

// fakeConn is a box: Run answers polls from listen; Control records calls.
type fakeConn struct {
	listen string
	calls  []string
	refuse map[string]bool // specs Control fails
}

func (c *fakeConn) Run(cmd string) ([]byte, error) {
	if cmd != pollCommand {
		return nil, errors.New("unexpected command " + cmd)
	}
	return []byte(c.listen), nil
}

func (c *fakeConn) Control(op, spec string) error {
	c.calls = append(c.calls, op+" "+spec)
	if c.refuse[spec] {
		return errors.New("refused")
	}
	return nil
}

func (c *fakeConn) take() []string { s := c.calls; c.calls = nil; return s }

func listenLines(ports ...string) string {
	var b strings.Builder
	b.WriteString("2000\n")
	for _, p := range ports { // hex port; "6:" prefix marks ::1
		if v6, ok := strings.CutPrefix(p, "6:"); ok {
			b.WriteString("   0: 00000000000000000000000001000000:" + v6 + " 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  2000 0 1\n")
		} else {
			b.WriteString("   0: 0100007F:" + p + " 00000000:0000 0A 00000000:00000000 00:00000000 00000000  2000 0 1\n")
		}
	}
	return b.String()
}

func TestForwarder(t *testing.T) {
	c := &fakeConn{refuse: map[string]bool{}}
	f := newForwarder(c)
	busy := map[int]bool{3000: true} // something of his already on 3000
	f.free = func(p int) bool { return !busy[p] }
	var notified []boxPort
	f.notify = func(p boxPort) { notified = append(notified, p) }
	var saved []boxPort
	saves := 0
	f.save = func(ps []boxPort) { saved = ps; saves++ }

	// 3000 (taken here: goes to 3001) and 5173 on ::1.
	c.listen = listenLines("0BB8", "6:1435")
	if err := f.tick(); err != nil {
		t.Fatal(err)
	}
	if got, want := c.take(), []string{"forward 127.0.0.1:3001:127.0.0.1:3000", "forward 127.0.0.1:5173:[::1]:5173"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v, want %v", got, want)
	}
	if want := []boxPort{{3000, 3001, "127.0.0.1"}, {5173, 5173, "::1"}}; !reflect.DeepEqual(notified, want) || !reflect.DeepEqual(saved, want) {
		t.Fatalf("notified %v saved %v", notified, saved)
	}

	// Nothing changed: nothing to do, nothing saved.
	f.tick()
	if got := c.take(); len(got) != 0 || saves != 1 {
		t.Fatalf("calls %v, saves %d", got, saves)
	}

	// 3000 closes, 8000 opens.
	c.listen = listenLines("6:1435", "1F40")
	f.tick()
	if got, want := c.take(), []string{"cancel 127.0.0.1:3001:127.0.0.1:3000", "forward 127.0.0.1:8000:127.0.0.1:8000"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v, want %v", got, want)
	}
	if want := []boxPort{{5173, 5173, "::1"}, {8000, 8000, "127.0.0.1"}}; !reflect.DeepEqual(saved, want) {
		t.Fatalf("saved %v", saved)
	}

	// 9000 cannot be forwarded at all: tried on 20 ports once, not every poll.
	for l := 9000; l < 9020; l++ {
		c.refuse[(boxPort{9000, l, "127.0.0.1"}).spec()] = true
	}
	c.listen = listenLines("6:1435", "1F40", "2328")
	f.tick()
	if got := c.take(); len(got) != 20 {
		t.Fatalf("%d attempts, want 20: %v", len(got), got)
	}
	f.tick()
	if got := c.take(); len(got) != 0 {
		t.Fatalf("retried a failed port every poll: %v", got)
	}
	// Once it closes and reopens (and the refusal is gone), it is tried again.
	c.listen = listenLines("6:1435", "1F40")
	f.tick()
	c.refuse = map[string]bool{}
	c.listen = listenLines("6:1435", "1F40", "2328")
	f.tick()
	if got, want := c.take(), []string{"forward 127.0.0.1:9000:127.0.0.1:9000"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v, want %v", got, want)
	}

	// A failed poll changes nothing.
	c.listen = "ssh: connection closed\n"
	if err := f.tick(); err == nil {
		t.Fatal("bad poll: no error")
	}
	if got := c.take(); len(got) != 0 || len(f.fwd) != 3 {
		t.Fatalf("calls %v, fwd %v", got, f.fwd)
	}

	// Everything closes.
	c.listen = "2000\n"
	f.tick()
	if got := c.take(); len(got) != 3 || len(saved) != 0 {
		t.Fatalf("calls %v, saved %v", got, saved)
	}
}

// A forward's local port must not collide with one this machine uses on
// either loopback address.
func TestLocalFree(t *testing.T) {
	p := freeLocalPort()
	if !localFree(p) {
		t.Skipf("port %d not free on both loopbacks", p)
	}
}

func TestShareable(t *testing.T) {
	for p, want := range map[int]bool{3000: true, 5173: true, 1024: true, 32767: true, 1023: false, 22: false, 2222: false, 1080: false, 2200: false, 32768: false, 60000: false} {
		if shareable(p) != want {
			t.Errorf("shareable(%d) = %v", p, !want)
		}
	}
}
