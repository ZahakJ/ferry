// Package pair turns a short spoken code into a shared secret.
//
// A code looks like "7-acorn-tulip-otter": a slot number, which is only a
// meeting point on the rendezvous, and three secret words (31 bits). The
// words never leave the machine. The two sides run CPace, a password-
// authenticated key exchange, so someone watching or even relaying the
// exchange learns nothing about the words, and an attacker who wants to
// guess them gets exactly one try per code before both sides give up.
package pair

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"

	"filippo.io/cpace"

	"github.com/ZahakJ/ferry/internal/rv"
	"github.com/ZahakJ/ferry/internal/secure"
	"github.com/ZahakJ/ferry/internal/words"
)

// Code is a parsed pairing code.
type Code struct {
	Slot  string
	Words []string
}

// NumWords is how many secret words a code has.
const NumWords = 3

// NewCode makes a fresh random code.
func NewCode() Code {
	n, err := rand.Int(rand.Reader, big.NewInt(99))
	if err != nil {
		panic(err)
	}
	return Code{Slot: strconv.Itoa(int(n.Int64()) + 1), Words: words.Random(NumWords)}
}

func (c Code) String() string { return c.Slot + "-" + strings.Join(c.Words, "-") }

// Parse reads a code, forgiving spaces, case and stray punctuation, and
// explains what is wrong if it cannot.
func Parse(s string) (Code, error) {
	f := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == '-' || r == ' ' || r == '_' || r == '.' || r == ','
	})
	if len(f) != NumWords+1 {
		return Code{}, fmt.Errorf("a code is a number and %d words, like 7-acorn-tulip-otter; that one has %d parts", NumWords, len(f))
	}
	if _, err := strconv.Atoi(f[0]); err != nil {
		return Code{}, errors.New("a code starts with a number, like 7-acorn-tulip-otter")
	}
	for _, w := range f[1:] {
		if !words.Valid(w) {
			if c := words.Closest(w); c != "" {
				return Code{}, fmt.Errorf("%q is not a code word; did you mean %q?", w, c)
			}
			return Code{}, fmt.Errorf("%q is not a code word; check the spelling", w)
		}
	}
	return Code{Slot: f[0], Words: f[1:]}, nil
}

// ErrWrongCode means the other side used a different code.
var ErrWrongCode = errors.New("the codes do not match")

// Result of a successful exchange.
type Result struct {
	Conn *secure.Conn
	// SAS is two words both sides can compare out loud.
	SAS string
}

// Run executes the exchange over rw. purpose ("pair", "send"...) and the
// carrier binding (both libp2p peer IDs, joiner first) are mixed into the
// key so a session cannot be redirected to another purpose or connection.
// The joiner (the side that typed the code) starts.
func Run(rw io.ReadWriter, c Code, purpose string, binding []byte, joiner bool) (*Result, error) {
	ad := append([]byte("ferry pair v1\x00"+purpose+"\x00"), binding...)
	ctx := cpace.NewContextInfo("joiner", "creator", ad)
	pw := strings.Join(c.Words, " ")
	var key, transcript []byte
	if joiner {
		msgA, st, err := cpace.Start(pw, ctx)
		if err != nil {
			return nil, err
		}
		if err := rv.WriteFrame(rw, msgA); err != nil {
			return nil, err
		}
		msgB, err := rv.ReadFrame(rw, 64)
		if err != nil {
			return nil, err
		}
		if key, err = st.Finish(msgB); err != nil {
			return nil, err
		}
		transcript = append(msgA, msgB...)
	} else {
		msgA, err := rv.ReadFrame(rw, 64)
		if err != nil {
			return nil, err
		}
		msgB, k, err := cpace.Exchange(pw, ctx, msgA)
		if err != nil {
			return nil, err
		}
		if err := rv.WriteFrame(rw, msgB); err != nil {
			return nil, err
		}
		key = k
		transcript = append(msgA, msgB...)
	}
	th := sha256.Sum256(append(transcript, ad...))
	conn, err := secure.FromPAKE(rw, key, th[:], joiner)
	if err != nil {
		return nil, err
	}
	// Key confirmation: if the words differ, the keys differ, and the first
	// sealed message fails to open on the other side.
	if err := confirm(conn, joiner); err != nil {
		return nil, err
	}
	return &Result{Conn: conn, SAS: sas(conn.Binding)}, nil
}

const confirmMsg = "ferry confirm"

func confirm(c *secure.Conn, joiner bool) error {
	send := func() error { return c.Send([]byte(confirmMsg)) }
	recv := func() error {
		m, err := c.Recv()
		if errors.Is(err, secure.ErrBadFrame) || (err == nil && string(m) != confirmMsg) {
			return ErrWrongCode
		}
		if err != nil {
			// The other side hangs up when our confirmation fails to open.
			return fmt.Errorf("%w (or the other side gave up)", ErrWrongCode)
		}
		return nil
	}
	if joiner {
		if err := send(); err != nil {
			return err
		}
		return recv()
	}
	if err := recv(); err != nil {
		return err
	}
	return send()
}

func sas(binding []byte) string {
	h := sha256.Sum256(append([]byte("ferry sas v1\x00"), binding...))
	n := len(words.List)
	a := (int(h[0])<<8 | int(h[1])) % n
	b := (int(h[2])<<8 | int(h[3])) % n
	return words.List[a] + " " + words.List[b]
}
