package msg

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"

	"github.com/ZahakJ/ferry/internal/keys"
)

// Mailbox envelopes are sealed with a key only the two contacts can
// compute (X25519 between their long-term keys). The rendezvous stores
// them without being able to read them. Unlike live sessions they have no
// forward secrecy: someone who later steals a machine's key could read
// envelopes they recorded. Live delivery is preferred whenever both sides
// are online.

type envelope struct {
	V     int    `json:"v"`
	From  string `json:"k"` // sender key ID, to pick the right contact
	Nonce []byte `json:"n"`
	CT    []byte `json:"c"`
}

func boxKey(id *keys.Identity, peer keys.Public) ([]byte, error) {
	shared, err := id.Shared(peer)
	if err != nil {
		return nil, err
	}
	a, b := id.Public().ID(), peer.ID()
	if a > b {
		a, b = b, a
	}
	k := make([]byte, 32)
	_, err = io.ReadFull(hkdf.New(sha256.New, shared, []byte(a+"\x00"+b), []byte("ferry mailbox v1")), k)
	return k, err
}

// Seal signs m and seals it for to.
func Seal(id *keys.Identity, to keys.Public, m *Message) ([]byte, error) {
	m.Sign(id)
	plain, _ := json.Marshal(m)
	k, err := boxKey(id, to)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(k)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	rand.Read(nonce)
	from := id.Public().ID()
	ad := []byte("ferry mbox v1\x00" + from + "\x00" + to.ID())
	return json.Marshal(envelope{V: 1, From: from, Nonce: nonce, CT: aead.Seal(nil, nonce, plain, ad)})
}

// ErrNotForUs means an envelope is from no contact we know, or not ours.
var ErrNotForUs = errors.New("envelope is not from a known contact")

// Open unseals and verifies an envelope from one of our contacts.
func Open(id *keys.Identity, contacts keys.Contacts, data []byte) (*Message, keys.Contact, error) {
	var e envelope
	if err := json.Unmarshal(data, &e); err != nil || e.V != 1 {
		return nil, keys.Contact{}, ErrNotForUs
	}
	var from keys.Contact
	found := false
	for _, c := range contacts {
		if c.Key.ID() == e.From {
			from, found = c, true
		}
	}
	if !found {
		return nil, keys.Contact{}, ErrNotForUs
	}
	k, err := boxKey(id, from.Key)
	if err != nil {
		return nil, from, err
	}
	aead, err := chacha20poly1305.NewX(k)
	if err != nil {
		return nil, from, err
	}
	if len(e.Nonce) != aead.NonceSize() {
		return nil, from, ErrNotForUs
	}
	ad := []byte("ferry mbox v1\x00" + e.From + "\x00" + id.Public().ID())
	plain, err := aead.Open(nil, e.Nonce, e.CT, ad)
	if err != nil {
		return nil, from, ErrNotForUs
	}
	var m Message
	if err := json.Unmarshal(plain, &m); err != nil {
		return nil, from, err
	}
	if err := m.Verify(from.Key, id.Public()); err != nil {
		return nil, from, err
	}
	return &m, from, nil
}
