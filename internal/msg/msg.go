// Package msg defines ferry's signed messages and the local log that holds them.
//
// A message is plain text or JSON with a type. Every message is signed by
// its sender's long-term key, so a recipient can tell who wrote it even
// after it has sat in a mailbox. Received messages are data: ferry never
// runs anything because a message asked it to.
package msg

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ZahakJ/ferry/internal/keys"
)

// Types with a defined meaning. Others are allowed and passed through.
const (
	Note      = "note"       // free text
	Request   = "request"    // asks the other side to do something; a human or model decides
	Patch     = "patch"      // a diff in Text
	FileOffer = "file-offer" // a file is on its way or waiting; Data says which
)

// MaxText bounds a message so one can always travel in a mailbox envelope.
const MaxText = 48 << 10

// Message is one signed message.
type Message struct {
	V       int             `json:"v"`
	ID      string          `json:"id"`
	From    string          `json:"from"` // sender key ID
	To      string          `json:"to"`   // recipient key ID
	TS      int64           `json:"ts"`   // unix milliseconds, sender's clock
	Type    string          `json:"type"`
	Text    string          `json:"text,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	ReplyTo string          `json:"reply_to,omitempty"`
	Sig     []byte          `json:"sig,omitempty"`
}

// New makes an unsigned message.
func New(to keys.Public, typ, text string, data json.RawMessage) (*Message, error) {
	if typ == "" {
		typ = Note
	}
	if len(text)+len(data) > MaxText {
		return nil, fmt.Errorf("message is %d bytes; the limit is %d. Send big things as a file", len(text)+len(data), MaxText)
	}
	if len(data) > 0 && !json.Valid(data) {
		return nil, errors.New("data is not valid JSON")
	}
	id := make([]byte, 12)
	rand.Read(id)
	return &Message{V: 1, ID: hex.EncodeToString(id), To: to.ID(), TS: time.Now().UnixMilli(), Type: typ, Text: text, Data: data}, nil
}

func (m *Message) signedBytes() []byte {
	c := *m
	c.Sig = nil
	b, _ := json.Marshal(c)
	return append([]byte("ferry msg v1\x00"), b...)
}

// Sign stamps the message as from id.
func (m *Message) Sign(id *keys.Identity) {
	m.From = id.Public().ID()
	m.Sig = ed25519.Sign(id.SignKey, m.signedBytes())
}

// Verify checks that m was signed by from and is addressed to me.
func (m *Message) Verify(from, me keys.Public) error {
	if m.V != 1 {
		return fmt.Errorf("message version %d is not supported", m.V)
	}
	if m.From != from.ID() || m.To != me.ID() {
		return errors.New("message is not between these two contacts")
	}
	if !ed25519.Verify(from.Sign, m.signedBytes(), m.Sig) {
		return errors.New("bad signature")
	}
	if len(m.ID) == 0 || len(m.ID) > 64 || len(m.Type) > 64 || len(m.ReplyTo) > 64 || len(m.Text)+len(m.Data) > MaxText {
		return errors.New("malformed message")
	}
	return nil
}

// Time is the sender's timestamp.
func (m *Message) Time() time.Time { return time.UnixMilli(m.TS) }
