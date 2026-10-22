// Package secure turns a byte stream into a channel of sealed messages.
//
// Every message is a length-prefixed frame sealed with ChaCha20-Poly1305.
// Each direction has its own key, and the nonce is a message counter, so a
// frame cannot be replayed, reordered, reflected back, or dropped without
// the next one failing to open.
//
// The keys come from one of two handshakes:
//
//   - FromPAKE, after a pairing code has been agreed (see package pair);
//   - Handshake, between two contacts who already know each other's keys:
//     each side sends a fresh X25519 key signed with its long-term Ed25519
//     key over the whole transcript, so a recorded session cannot be
//     decrypted later and nobody in the middle can splice in.
package secure

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"

	"github.com/ZahakJ/ferry/internal/keys"
	"github.com/ZahakJ/ferry/internal/rv"
)

// MaxMessage is the largest message a Conn carries.
const MaxMessage = 1 << 20

// Conn is a sealed message channel.
type Conn struct {
	rw         io.ReadWriter
	sendMu     sync.Mutex
	send, recv cipherState
	// Binding is a secret both sides share, unique to this session, for
	// deriving further keys and short authentication strings.
	Binding []byte
}

type cipherState struct {
	aead interface {
		Seal(dst, nonce, plaintext, additionalData []byte) []byte
		Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
	}
	n uint64
}

func (c *cipherState) nonce() []byte {
	var b [chacha20poly1305.NonceSize]byte
	binary.BigEndian.PutUint64(b[4:], c.n)
	c.n++
	return b[:]
}

// ErrBadFrame means a frame failed to open: corrupted, replayed, or forged.
var ErrBadFrame = errors.New("message failed authentication")

// Send seals and writes one message.
func (c *Conn) Send(msg []byte) error {
	if len(msg) > MaxMessage {
		return fmt.Errorf("message of %d bytes is over the %d limit", len(msg), MaxMessage)
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return rv.WriteFrame(c.rw, c.send.aead.Seal(nil, c.send.nonce(), msg, nil))
}

// Recv reads and opens one message. It must not be called concurrently.
func (c *Conn) Recv() ([]byte, error) {
	b, err := rv.ReadFrame(c.rw, MaxMessage+chacha20poly1305.Overhead)
	if err != nil {
		return nil, err
	}
	m, err := c.recv.aead.Open(nil, c.recv.nonce(), b, nil)
	if err != nil {
		return nil, ErrBadFrame
	}
	return m, nil
}

// SendJSON sends v as JSON.
func (c *Conn) SendJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Send(b)
}

// RecvJSON reads one JSON message into v.
func (c *Conn) RecvJSON(v any) error {
	b, err := c.Recv()
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func derive(secret, salt []byte, label string) []byte {
	out := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, secret, salt, []byte(label)), out); err != nil {
		panic(err)
	}
	return out
}

func newConn(rw io.ReadWriter, secret, transcript []byte, initiator bool) (*Conn, error) {
	ki := derive(secret, transcript, "ferry secure v1 initiator")
	kr := derive(secret, transcript, "ferry secure v1 responder")
	if !initiator {
		ki, kr = kr, ki
	}
	s, err := chacha20poly1305.New(ki)
	if err != nil {
		return nil, err
	}
	r, err := chacha20poly1305.New(kr)
	if err != nil {
		return nil, err
	}
	return &Conn{
		rw:      rw,
		send:    cipherState{aead: s},
		recv:    cipherState{aead: r},
		Binding: derive(secret, transcript, "ferry secure v1 binding"),
	}, nil
}

// FromPAKE builds a Conn from a key agreed by a PAKE over transcript.
func FromPAKE(rw io.ReadWriter, key, transcript []byte, initiator bool) (*Conn, error) {
	return newConn(rw, key, transcript, initiator)
}

type hello struct {
	Proto string      `json:"proto"`
	Key   keys.Public `json:"key"`
	Eph   []byte      `json:"eph"`
	Sig   []byte      `json:"sig,omitempty"`
}

// Handshake authenticates a contact session. context binds the session to
// its carrier (for example both libp2p peer IDs). accept decides whether the
// peer's long-term key is welcome; it is called before any secret is derived.
func Handshake(rw io.ReadWriter, me *keys.Identity, context []byte, initiator bool,
	accept func(keys.Public) error) (*Conn, keys.Public, error) {

	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, keys.Public{}, err
	}
	mine := hello{Proto: "ferry/1", Key: me.Public(), Eph: eph.PublicKey().Bytes()}
	// Round 1: ephemeral keys and identities, in the clear.
	var theirs hello
	if err := exchange(rw, initiator, mine, &theirs); err != nil {
		return nil, keys.Public{}, err
	}
	if theirs.Proto != "ferry/1" {
		return nil, keys.Public{}, fmt.Errorf("peer speaks %q, not ferry/1: one side needs updating", theirs.Proto)
	}
	if len(theirs.Key.Sign) != ed25519.PublicKeySize {
		return nil, keys.Public{}, errors.New("peer sent a malformed key")
	}
	if err := accept(theirs.Key); err != nil {
		return nil, theirs.Key, err
	}
	ini, rsp := mine, theirs
	if !initiator {
		ini, rsp = theirs, mine
	}
	th := transcript(context, ini, rsp)
	// Round 2: each side signs the transcript with its long-term key.
	sig := ed25519.Sign(me.SignKey, append([]byte("ferry handshake v1\x00"), th...))
	var theirSig []byte
	if err := exchange(rw, initiator, sig, &theirSig); err != nil {
		return nil, keys.Public{}, err
	}
	if !ed25519.Verify(theirs.Key.Sign, append([]byte("ferry handshake v1\x00"), th...), theirSig) {
		return nil, theirs.Key, errors.New("peer's signature does not match its key: possible tampering")
	}
	pe, err := ecdh.X25519().NewPublicKey(theirs.Eph)
	if err != nil {
		return nil, theirs.Key, err
	}
	shared, err := eph.ECDH(pe)
	if err != nil {
		return nil, theirs.Key, err
	}
	c, err := newConn(rw, shared, th, initiator)
	return c, theirs.Key, err
}

func transcript(context []byte, ini, rsp hello) []byte {
	h := sha256.New()
	put := func(b []byte) {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(b)))
		h.Write(l[:])
		h.Write(b)
	}
	put([]byte("ferry transcript v1"))
	put(context)
	for _, x := range []hello{ini, rsp} {
		put(x.Key.Sign)
		put(x.Key.Box)
		put(x.Eph)
	}
	return h.Sum(nil)
}

// exchange sends mine and reads theirs; the initiator writes first.
func exchange(rw io.ReadWriter, initiator bool, mine, theirs any) error {
	b, err := json.Marshal(mine)
	if err != nil {
		return err
	}
	if initiator {
		if err := rv.WriteFrame(rw, b); err != nil {
			return err
		}
	}
	in, err := rv.ReadFrame(rw, 64<<10)
	if err != nil {
		return err
	}
	if !initiator {
		if err := rv.WriteFrame(rw, b); err != nil {
			return err
		}
	}
	return json.Unmarshal(in, theirs)
}
