package msg

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZahakJ/ferry/internal/keys"
)

func TestSealOpen(t *testing.T) {
	a, _ := keys.New("a")
	b, _ := keys.New("b")
	m, _ := New(b.Public(), Request, "review branch x", nil)
	env, err := Seal(a, b.Public(), m)
	if err != nil {
		t.Fatal(err)
	}
	cs := keys.Contacts{"a": {Name: "a", Key: a.Public()}}
	got, from, err := Open(b, cs, env)
	if err != nil || got.Text != "review branch x" || from.Name != "a" || got.Type != Request {
		t.Fatal(got, from, err)
	}
	// A stranger's envelope, or one for someone else, does not open.
	c, _ := keys.New("c")
	if _, _, err := Open(c, keys.Contacts{"a": {Name: "a", Key: a.Public()}}, env); err == nil {
		t.Fatal("opened someone else's envelope")
	}
	if _, _, err := Open(b, keys.Contacts{}, env); err == nil {
		t.Fatal("opened envelope from a stranger")
	}
	// Tampering breaks it.
	env[len(env)-5] ^= 1
	if _, _, err := Open(b, cs, env); err == nil {
		t.Fatal("tampered envelope opened")
	}
}

func TestForgedSignature(t *testing.T) {
	a, _ := keys.New("a")
	b, _ := keys.New("b")
	m, _ := New(b.Public(), Note, "hi", nil)
	m.Sign(a)
	m.Text = "changed"
	if m.Verify(a.Public(), b.Public()) == nil {
		t.Fatal("altered message verified")
	}
}

func TestStore(t *testing.T) {
	s, _ := OpenStore(t.TempDir())
	a, _ := keys.New("a")
	m, _ := New(a.Public(), Note, "one", nil)
	if added, _ := s.Add(&Record{Dir: "in", Contact: "a", Msg: m}); !added {
		t.Fatal("not added")
	}
	if added, _ := s.Add(&Record{Dir: "in", Contact: "a", Msg: m}); added {
		t.Fatal("duplicate added")
	}
	m2, _ := New(a.Public(), Note, "two", nil)
	s.Add(&Record{Dir: "in", Contact: "a", Msg: m2, At: time.Now()})
	rs, _ := s.Since(1, "", "")
	if len(rs) != 1 || rs[0].Msg.Text != "two" || rs[0].Seq != 2 {
		t.Fatal(rs)
	}
}

// One oversized or damaged line must not make the log unreadable.
func TestStoreSurvivesBadLine(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(dir)
	a, _ := keys.New("a")
	m, _ := New(a.Public(), Note, "before", nil)
	s.Add(&Record{Dir: "in", Contact: "a", Msg: m})
	f, _ := os.OpenFile(filepath.Join(dir, "messages.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	f.Write(append(bytes.Repeat([]byte("x"), 2<<20), '\n'))
	f.Write([]byte("{not json\n"))
	f.Close()
	m2, _ := New(a.Public(), Note, "after", nil)
	if _, err := s.Add(&Record{Dir: "in", Contact: "a", Msg: m2}); err != nil {
		t.Fatal(err)
	}
	rs, err := s.Since(0, "", "")
	if err != nil || len(rs) != 2 || rs[1].Msg.Text != "after" {
		t.Fatal(rs, err)
	}
}

func TestReplyToBounded(t *testing.T) {
	a, _ := keys.New("a")
	b, _ := keys.New("b")
	m, _ := New(b.Public(), Note, "hi", nil)
	m.ReplyTo = strings.Repeat("r", 1000)
	m.Sign(a)
	if m.Verify(a.Public(), b.Public()) == nil {
		t.Fatal("huge reply_to accepted")
	}
}
