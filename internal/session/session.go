package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/ZahakJ/ferry/internal/keys"
	"github.com/ZahakJ/ferry/internal/link"
	"github.com/ZahakJ/ferry/internal/pair"
	"github.com/ZahakJ/ferry/internal/rv"
	"github.com/ZahakJ/ferry/internal/secure"
)

const (
	protoCode    = "/ferry/code/1"
	protoContact = "/ferry/contact/1"
)

// Session is an authenticated, sealed channel to one peer.
type Session struct {
	Conn *secure.Conn
	// Peer is the contact's key for contact sessions, empty for codes.
	Peer keys.Public
	// SAS is two words both sides can compare, for code sessions.
	SAS string
	// Dialer is true on the side that started the session.
	Dialer bool
	cfg    *Config
	stream network.Stream
	owned  *rv.Node // closed with the session, if ours
	others []io.Closer
}

// Own makes c close when the session does.
func (s *Session) Own(c io.Closer) { s.others = append(s.others, c) }

// Link opens the fast data connection for this session.
func (s *Session) Link(ctx context.Context, lanes int) (*link.Link, error) {
	return link.Open(ctx, s.Conn, s.Dialer, s.cfg.LinkOptions(ctx, lanes))
}

// Close ends the session.
func (s *Session) Close() error {
	if s.stream != nil {
		s.stream.Close()
	}
	if s.owned != nil {
		s.owned.Close()
	}
	for _, c := range s.others {
		c.Close()
	}
	return nil
}

// ---- one-time codes ----

type header struct {
	V       int    `json:"v"`
	Purpose string `json:"purpose"`
}

type headerReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Purposes a code can be for. Mixing them up gives a helpful error.
const (
	PurposeSend = "send"
	PurposePair = "pair"
)

var purposeHint = map[string]string{
	PurposeSend: "this code is for receiving a file: run `ferry get <code>`",
	PurposePair: "this code is for pairing: run `ferry pair <code>`",
}

// ErrCodeUsed means a code attempt failed and the code is now spent.
var ErrCodeUsed = errors.New("code spent")

// Offer makes a code, reports it through onCode, and waits for the other
// side to use it. A code is good for exactly one attempt: if the wrong
// words arrive, Offer fails rather than let anyone guess again.
func Offer(ctx context.Context, cfg *Config, purpose string, onCode func(pair.Code)) (*Session, error) {
	if err := cfg.need(); err != nil {
		return nil, err
	}
	node, err := rv.Dial(ctx, cfg.Rendezvous, nil)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			node.Close()
		}
	}()
	if err := node.Reserve(ctx); err != nil {
		return nil, err
	}
	code := pair.NewCode()
	incoming := make(chan network.Stream, 4)
	node.Handle(protoCode, func(s network.Stream) {
		select {
		case incoming <- s:
		default:
			s.Reset()
		}
	})
	if err := node.Announce(ctx, code.Slot); err != nil {
		return nil, fmt.Errorf("could not announce the code: %w", err)
	}
	onCode(code)
	for {
		var st network.Stream
		select {
		case st = <-incoming:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		var h header
		b, err := rv.ReadFrame(st, 4096)
		if err == nil {
			err = json.Unmarshal(b, &h)
		}
		if err != nil {
			st.Reset()
			continue // not a ferry peer; keep waiting
		}
		if h.Purpose != purpose {
			msg := "this code is for something else"
			if hint, found := purposeHint[purpose]; found {
				msg = hint
			}
			reply(st, headerReply{Error: msg})
			st.Close()
			continue // an honest mix-up costs nothing; no words were tried
		}
		reply(st, headerReply{OK: true})
		binding := []byte(st.Conn().RemotePeer().String() + "\x00" + st.Conn().LocalPeer().String())
		st.SetDeadline(time.Now().Add(30 * time.Second))
		res, err := pair.Run(st, code, purpose, binding, false)
		if err != nil {
			st.Reset()
			if errors.Is(err, pair.ErrWrongCode) {
				return nil, fmt.Errorf("%w: someone tried this code with the wrong words, so it is no longer valid; make a new one", ErrCodeUsed)
			}
			return nil, err
		}
		st.SetDeadline(time.Time{})
		ok = true
		return &Session{Conn: res.Conn, SAS: res.SAS, cfg: cfg, stream: st, owned: node}, nil
	}
}

func reply(st network.Stream, r headerReply) {
	b, _ := json.Marshal(r)
	rv.WriteFrame(st, b)
}

// ErrNobodyThere means no one is waiting on the code's slot.
var ErrNobodyThere = errors.New("nobody is waiting for that code")

// Join uses a code someone else made.
func Join(ctx context.Context, cfg *Config, purpose string, code pair.Code) (*Session, error) {
	if err := cfg.need(); err != nil {
		return nil, err
	}
	node, err := rv.Dial(ctx, cfg.Rendezvous, nil)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			node.Close()
		}
	}()
	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	ids, err := node.Lookup(lctx, code.Slot)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("%w (is the other side still running? codes work once)", ErrNobodyThere)
	}
	var lastErr error
	for _, id := range ids {
		s, err := join(ctx, node, id, purpose, code)
		if err == nil {
			ok = true
			s.cfg, s.owned = cfg, node
			return s, nil
		}
		lastErr = err
		if errors.Is(err, pair.ErrWrongCode) || errors.Is(err, errPurpose) {
			break
		}
	}
	return nil, lastErr
}

var errPurpose = errors.New("wrong kind of code")

func join(ctx context.Context, node *rv.Node, id peer.ID, purpose string, code pair.Code) (*Session, error) {
	st, err := node.Open(ctx, id, protoCode)
	if err != nil {
		return nil, fmt.Errorf("could not reach the other side through the rendezvous: %w", err)
	}
	st.SetDeadline(time.Now().Add(30 * time.Second))
	b, _ := json.Marshal(header{V: 1, Purpose: purpose})
	if err := rv.WriteFrame(st, b); err != nil {
		st.Reset()
		return nil, err
	}
	var hr headerReply
	b, err = rv.ReadFrame(st, 4096)
	if err == nil {
		err = json.Unmarshal(b, &hr)
	}
	if err != nil {
		st.Reset()
		return nil, err
	}
	if !hr.OK {
		st.Close()
		return nil, fmt.Errorf("%w: %s", errPurpose, hr.Error)
	}
	binding := []byte(st.Conn().LocalPeer().String() + "\x00" + st.Conn().RemotePeer().String())
	res, err := pair.Run(st, code, purpose, binding, true)
	if err != nil {
		st.Reset()
		return nil, err
	}
	st.SetDeadline(time.Time{})
	return &Session{Conn: res.Conn, SAS: res.SAS, Dialer: true, stream: st}, nil
}

// ---- contacts ----

// Endpoint is this machine on the rendezvous under its own identity, so
// contacts can reach it.
type Endpoint struct {
	ID   *keys.Identity
	Node *rv.Node
	cfg  *Config
}

// Online connects to the rendezvous as id.
func Online(ctx context.Context, cfg *Config, id *keys.Identity) (*Endpoint, error) {
	if err := cfg.need(); err != nil {
		return nil, err
	}
	k, err := rv.PeerKey(id.SignKey)
	if err != nil {
		return nil, err
	}
	node, err := rv.Dial(ctx, cfg.Rendezvous, k)
	if err != nil {
		return nil, err
	}
	// Opening our mailbox lets contacts leave messages while we are away.
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	node.Register(rctx)
	cancel()
	return &Endpoint{ID: id, Node: node, cfg: cfg}, nil
}

// Close goes offline.
func (e *Endpoint) Close() error { return e.Node.Close() }

// Config returns the configuration the endpoint was made with.
func (e *Endpoint) Config() *Config { return e.cfg }

func contactPeer(c keys.Public) (peer.ID, error) {
	p, err := peer.Decode(c.Peer)
	if err != nil {
		return "", fmt.Errorf("contact %q has no usable address; pair again", c.Name)
	}
	return p, nil
}

// Dial opens a session to a contact. It fails fast if they are offline.
func (e *Endpoint) Dial(ctx context.Context, c keys.Contact) (*Session, error) {
	p, err := contactPeer(c.Key)
	if err != nil {
		return nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	st, err := e.Node.Open(dctx, p, protoContact)
	if err != nil {
		if strings.Contains(err.Error(), "NO_RESERVATION") {
			return nil, fmt.Errorf("%w: %s is not online. On their machine, `ferry get` (or `ferry up`) must be running", ErrOffline, c.Name)
		}
		return nil, fmt.Errorf("%w: cannot reach %s: %v", ErrOffline, c.Name, err)
	}
	st.SetDeadline(time.Now().Add(30 * time.Second))
	binding := []byte(st.Conn().LocalPeer().String() + "\x00" + st.Conn().RemotePeer().String())
	conn, peerKey, err := secure.Handshake(st, e.ID, binding, true, func(k keys.Public) error {
		if string(k.Sign) != string(c.Key.Sign) {
			return fmt.Errorf("the machine answering as %s has a different key than when you paired: refusing", c.Name)
		}
		return nil
	})
	if err != nil {
		st.Reset()
		return nil, err
	}
	st.SetDeadline(time.Time{})
	return &Session{Conn: conn, Peer: peerKey, Dialer: true, cfg: e.cfg, stream: st}, nil
}

// ErrOffline means a contact could not be reached.
var ErrOffline = errors.New("offline")

// Listen accepts sessions from known contacts until ctx ends. Strangers
// are turned away before any of their data is read beyond the handshake.
func (e *Endpoint) Listen(ctx context.Context, known func() keys.Contacts, handle func(*Session, keys.Contact)) error {
	if err := e.Node.Reserve(ctx); err != nil {
		return err
	}
	e.Node.Handle(protoContact, func(st network.Stream) {
		st.SetDeadline(time.Now().Add(30 * time.Second))
		var who keys.Contact
		binding := []byte(st.Conn().RemotePeer().String() + "\x00" + st.Conn().LocalPeer().String())
		conn, peerKey, err := secure.Handshake(st, e.ID, binding, false, func(k keys.Public) error {
			c, ok := known().ByKey(k.Sign)
			if !ok {
				return errors.New("not a contact")
			}
			if c.Key.Peer != st.Conn().RemotePeer().String() {
				return errors.New("contact key on an unexpected peer ID")
			}
			who = c
			return nil
		})
		if err != nil {
			st.Reset()
			return
		}
		st.SetDeadline(time.Time{})
		handle(&Session{Conn: conn, Peer: peerKey, cfg: e.cfg, stream: st}, who)
	})
	return nil
}

// Intent is the first message on every contact session: what the dialer
// wants. New kinds of work (such as jobs) are new values of Want; a side
// that does not know one answers with OK false and a reason, so older and
// newer versions fail politely rather than misunderstand each other.
type Intent struct {
	Want string `json:"want"` // msg | files
}

// IntentReply accepts or declines an Intent.
type IntentReply struct {
	OK  bool   `json:"ok"`
	Why string `json:"why,omitempty"`
}
