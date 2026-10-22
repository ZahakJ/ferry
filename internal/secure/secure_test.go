package secure

import (
	"errors"
	"net"
	"testing"

	"github.com/ZahakJ/ferry/internal/keys"
)

func pair(t *testing.T, accept func(keys.Public) error) (*Conn, *Conn, error, error) {
	a, _ := keys.New("a")
	b, _ := keys.New("b")
	x, y := net.Pipe()
	var cb *Conn
	var eb error
	done := make(chan struct{})
	go func() {
		cb, _, eb = Handshake(y, b, []byte("ctx"), false, accept)
		if eb != nil {
			y.Close()
		}
		close(done)
	}()
	ca, _, ea := Handshake(x, a, []byte("ctx"), true, func(keys.Public) error { return nil })
	if ea != nil {
		x.Close()
	}
	<-done
	return ca, cb, ea, eb
}

func TestHandshake(t *testing.T) {
	ca, cb, ea, eb := pair(t, func(keys.Public) error { return nil })
	if ea != nil || eb != nil {
		t.Fatal(ea, eb)
	}
	go func() { ca.Send([]byte("one")); ca.Send([]byte("two")) }()
	for _, want := range []string{"one", "two"} {
		m, err := cb.Recv()
		if err != nil || string(m) != want {
			t.Fatal(m, err)
		}
	}
}

func TestRejectUnknown(t *testing.T) {
	no := errors.New("not a contact")
	_, _, _, eb := pair(t, func(keys.Public) error { return no })
	if !errors.Is(eb, no) {
		t.Fatal(eb)
	}
}

// A flipped bit in transit must fail authentication, not decrypt to garbage.
func TestTamper(t *testing.T) {
	a, b := net.Pipe()
	k := make([]byte, 32)
	ca, _ := FromPAKE(a, k, []byte("t"), true)
	cb, _ := FromPAKE(&flipper{b}, k, []byte("t"), false)
	go ca.Send([]byte("hello"))
	if _, err := cb.Recv(); !errors.Is(err, ErrBadFrame) {
		t.Fatal(err)
	}
}

type flipper struct{ net.Conn }

func (f *flipper) Read(p []byte) (int, error) {
	n, err := f.Conn.Read(p)
	if n > 4 {
		p[n-1] ^= 1
	}
	return n, err
}
