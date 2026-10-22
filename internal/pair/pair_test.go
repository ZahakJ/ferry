package pair

import (
	"errors"
	"net"
	"testing"
)

func run2(t *testing.T, a, b Code) (ra, rb *Result, ea, eb error) {
	t.Helper()
	x, y := net.Pipe()
	done := make(chan struct{})
	go func() {
		rb, eb = Run(y, b, "pair", []byte("bind"), false)
		if eb != nil {
			y.Close()
		}
		close(done)
	}()
	ra, ea = Run(x, a, "pair", []byte("bind"), true)
	if ea != nil {
		x.Close()
	}
	<-done
	return
}

func TestPairing(t *testing.T) {
	c := NewCode()
	ra, rb, ea, eb := run2(t, c, c)
	if ea != nil || eb != nil {
		t.Fatal(ea, eb)
	}
	if ra.SAS != rb.SAS {
		t.Fatalf("SAS differ: %q %q", ra.SAS, rb.SAS)
	}
	go ra.Conn.Send([]byte("hi"))
	m, err := rb.Conn.Recv()
	if err != nil || string(m) != "hi" {
		t.Fatal(m, err)
	}
}

func TestWrongCode(t *testing.T) {
	a := NewCode()
	b := a
	b.Words = append([]string{}, a.Words...)
	b.Words[2] = "zebra"
	if a.Words[2] == "zebra" {
		b.Words[2] = "acorn"
	}
	_, _, ea, eb := run2(t, a, b)
	if !errors.Is(ea, ErrWrongCode) || !errors.Is(eb, ErrWrongCode) {
		t.Fatalf("want wrong-code on both sides, got %v / %v", ea, eb)
	}
}

func TestParse(t *testing.T) {
	c, err := Parse(" 7 Acorn-tulip  otter ")
	if err != nil || c.String() != "7-acorn-tulip-otter" {
		t.Fatal(c, err)
	}
	if _, err := Parse("7-acron-tulip-otter"); err == nil || err.Error() != `"acron" is not a code word; did you mean "acorn"?` {
		t.Fatal(err)
	}
	if _, err := Parse("acorn-tulip-otter"); err == nil {
		t.Fatal("want error")
	}
}
