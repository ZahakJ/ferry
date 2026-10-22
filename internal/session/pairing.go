package session

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"time"

	"github.com/ZahakJ/ferry/internal/keys"
)

type introduction struct {
	Key keys.Public `json:"key"`
	// Sig proves the sender holds Key: it signs this session's binding
	// and the key itself, so a key copied from someone else cannot be
	// presented as one's own.
	Sig []byte `json:"sig"`
}

func introBytes(binding []byte, k keys.Public) []byte {
	b, _ := json.Marshal(k)
	return append(append([]byte("ferry introduce v1\x00"), binding...), b...)
}

// Introduce swaps long-term identities over a fresh code session and
// returns the peer's. Both messages travel sealed under the code's key, so
// only someone who knew the words could have sent them.
func Introduce(s *Session, me *keys.Identity) (keys.Public, error) {
	pub := me.Public()
	mine := introduction{Key: pub, Sig: ed25519.Sign(me.SignKey, introBytes(s.Conn.Binding, pub))}
	errc := make(chan error, 1)
	go func() { errc <- s.Conn.SendJSON(mine) }()
	var in introduction
	if err := s.Conn.RecvJSON(&in); err != nil {
		return keys.Public{}, err
	}
	if err := <-errc; err != nil {
		return keys.Public{}, err
	}
	if len(in.Key.Sign) != ed25519.PublicKeySize || len(in.Key.Box) != 32 || in.Key.Peer == "" {
		return keys.Public{}, errors.New("the other side sent an incomplete identity")
	}
	if !ed25519.Verify(in.Key.Sign, introBytes(s.Conn.Binding, in.Key), in.Sig) {
		return keys.Public{}, errors.New("the other side could not prove it holds the key it sent")
	}
	if string(in.Key.Sign) == string(pub.Sign) {
		return keys.Public{}, errors.New("that is this machine's own code")
	}
	return in.Key, nil
}

// Remember stores a newly paired peer under name (or a free variant of
// it) and returns the name used. Pairing again with the same machine
// updates its entry.
func Remember(name string, k keys.Public) (string, error) {
	cs, err := keys.LoadContacts()
	if err != nil {
		return "", err
	}
	if old, ok := cs.ByKey(k.Sign); ok {
		delete(cs, old.Name)
	}
	n := cs.FreeName(name, k)
	cs[n] = keys.Contact{Name: n, Key: k, Paired: time.Now().UTC()}
	return n, cs.Save()
}
