package session

import (
	"errors"
	"time"

	"github.com/ZahakJ/ferry/internal/keys"
)

type introduction struct {
	Key keys.Public `json:"key"`
}

// Introduce swaps long-term identities over a fresh code session and
// returns the peer's. Both messages travel sealed under the code's key, so
// only someone who knew the words could have sent them.
func Introduce(s *Session, me *keys.Identity) (keys.Public, error) {
	errc := make(chan error, 1)
	go func() { errc <- s.Conn.SendJSON(introduction{Key: me.Public()}) }()
	var in introduction
	if err := s.Conn.RecvJSON(&in); err != nil {
		return keys.Public{}, err
	}
	if err := <-errc; err != nil {
		return keys.Public{}, err
	}
	if len(in.Key.Sign) != 32 || len(in.Key.Box) != 32 || in.Key.Peer == "" {
		return keys.Public{}, errors.New("the other side sent an incomplete identity")
	}
	if string(in.Key.Sign) == string(me.Public().Sign) {
		return keys.Public{}, errors.New("that is this machine's own code")
	}
	return in.Key, nil
}

// Remember stores a newly paired peer under name (or a free variant of
// it) and returns the name used.
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
