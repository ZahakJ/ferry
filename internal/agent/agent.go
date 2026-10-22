// Package agent is everything ferry does on behalf of this machine:
// sending and receiving messages and files with contacts.
//
// One agent runs per machine. `ferry up` runs it in the background and
// serves it on a unix socket; commands then talk to it. Without a
// background agent, each command runs its own for as long as it lasts.
// Both cases implement API, so callers do not care which they have.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ZahakJ/ferry/internal/keys"
	"github.com/ZahakJ/ferry/internal/msg"
	"github.com/ZahakJ/ferry/internal/session"
	"github.com/ZahakJ/ferry/internal/xfer"
)

// API is what an agent offers. Errors that matter to callers wrap the
// sentinel errors in this package and in session.
type API interface {
	Status(ctx context.Context) (*Status, error)
	Contacts(ctx context.Context) ([]Contact, error)
	Send(ctx context.Context, req SendRequest) (*SendResult, error)
	Inbox(ctx context.Context, since int64, from string) (*InboxResult, error)
	Wait(ctx context.Context, from string, since int64, timeout time.Duration) (*msg.Record, error)
	SendFile(ctx context.Context, to, path string, progress xfer.Progress) (*FileResult, error)
	Files(ctx context.Context, since int64) ([]Received, error)
	Close() error
}

// Status describes the agent.
type Status struct {
	Name       string `json:"name"`
	ID         string `json:"id"`
	Background bool   `json:"background"` // a `ferry up` agent
	Online     bool   `json:"online"`
	Listening  bool   `json:"listening"`
	Rendezvous string `json:"rendezvous"`
	Contacts   int    `json:"contacts"`
	LastSeq    int64  `json:"last_seq"`
	Error      string `json:"error,omitempty"`
}

// Contact as listed to callers.
type Contact struct {
	Name   string    `json:"name"`
	ID     string    `json:"id"`
	Paired time.Time `json:"paired"`
}

// SendRequest is a message to send.
type SendRequest struct {
	To      string          `json:"to"`
	Type    string          `json:"type,omitempty"`
	Text    string          `json:"text,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	ReplyTo string          `json:"reply_to,omitempty"`
}

// SendResult says how a message went.
type SendResult struct {
	ID  string `json:"id"`
	Via string `json:"via"` // direct | mailbox
	Seq int64  `json:"seq"`
}

// InboxResult is a page of messages and the cursor to continue from.
type InboxResult struct {
	Messages []msg.Record `json:"messages"`
	Next     int64        `json:"next"`
}

// FileResult says how a file transfer went.
type FileResult struct {
	Name    string  `json:"name"`
	Bytes   int64   `json:"bytes"`
	Seconds float64 `json:"seconds"`
	Path    string  `json:"path"` // network path: direct | relay (TURN)
}

// Received is a file a contact sent to this machine's agent.
type Received struct {
	Seq     int64     `json:"seq"`
	Contact string    `json:"contact"`
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Bytes   int64     `json:"bytes"`
	At      time.Time `json:"at"`
}

// ErrTimeout means Wait saw nothing in time.
var ErrTimeout = errors.New("no message arrived in time")

// Local is an agent running in this process.
type Local struct {
	id    *keys.Identity
	cfg   *session.Config
	store *msg.Store
	files *fileLog

	mu         sync.Mutex
	ep         *session.Endpoint
	listening  bool
	background bool
	news       chan struct{}   // closed and replaced when a message lands
	busy       map[string]bool // contacts with a transfer in progress
	stop       context.CancelFunc
}

// NewLocal makes an agent for this machine. It goes online lazily.
func NewLocal(cfg *session.Config) (*Local, error) {
	id, _, err := keys.LoadOrCreate()
	if err != nil {
		return nil, err
	}
	st, err := msg.OpenStore(keys.Dir())
	if err != nil {
		return nil, err
	}
	return &Local{id: id, cfg: cfg, store: st, files: &fileLog{path: filepath.Join(keys.Dir(), "files.jsonl")}, news: make(chan struct{})}, nil
}

// FilesDir is where files from contacts land.
func FilesDir() string { return filepath.Join(keys.Dir(), "files") }

func (a *Local) online(ctx context.Context) (*session.Endpoint, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ep != nil {
		return a.ep, nil
	}
	ep, err := session.Online(ctx, a.cfg, a.id)
	if err != nil {
		return nil, err
	}
	a.ep = ep
	return ep, nil
}

// Listen accepts sessions from contacts and polls the mailbox until ctx
// ends. Only one process per machine should listen; see package doc.
func (a *Local) Listen(ctx context.Context, background bool) error {
	ep, err := a.online(ctx)
	if err != nil {
		return err
	}
	if err := ep.Listen(ctx, contactsNow, a.serve); err != nil {
		return err
	}
	a.mu.Lock()
	a.listening, a.background = true, background
	a.mu.Unlock()
	go func() {
		t := time.NewTicker(mailboxPoll(background))
		defer t.Stop()
		for {
			a.drain(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

func mailboxPoll(background bool) time.Duration {
	if background {
		return 15 * time.Second // live sessions carry most traffic
	}
	return 2 * time.Second
}

func contactsNow() keys.Contacts {
	cs, _ := keys.LoadContacts()
	return cs
}

// Close goes offline.
func (a *Local) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ep != nil {
		a.ep.Close()
		a.ep = nil
	}
	return nil
}

func (a *Local) notify() {
	a.mu.Lock()
	close(a.news)
	a.news = make(chan struct{})
	a.mu.Unlock()
}

func (a *Local) newsChan() chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.news
}

// ---- serving contacts ----

type ack struct {
	ID string `json:"id"`
	OK bool   `json:"ok"`
}

func (a *Local) serve(s *session.Session, c keys.Contact) {
	defer s.Close()
	var in session.Intent
	if s.Conn.RecvJSON(&in) != nil {
		return
	}
	switch in.Want {
	case "msg":
		s.Conn.SendJSON(session.IntentReply{OK: true})
		for {
			var m msg.Message
			if s.Conn.RecvJSON(&m) != nil {
				return
			}
			// Acknowledge only what is verified and safely stored, so the
			// sender falls back to the mailbox rather than lose a message.
			ok := m.Verify(c.Key, a.id.Public()) == nil
			if ok {
				added, err := a.store.Add(&msg.Record{Dir: "in", Contact: c.Name, Via: "direct", Msg: &m})
				ok = err == nil
				if added {
					a.notify()
				}
			}
			if s.Conn.SendJSON(ack{ID: m.ID, OK: ok}) != nil {
				return
			}
		}
	case "files":
		// One transfer at a time per contact; free space is checked
		// before any data is accepted.
		if !a.claimTransfer(c.Name) {
			s.Conn.SendJSON(session.IntentReply{Why: "already receiving a transfer from you; try again when it finishes"})
			return
		}
		defer a.releaseTransfer(c.Name)
		s.Conn.SendJSON(session.IntentReply{OK: true})
		a.receiveFile(s, c)
	default:
		s.Conn.SendJSON(session.IntentReply{Why: fmt.Sprintf("this machine does not accept %q", in.Want)})
	}
}

func (a *Local) receiveFile(s *session.Session, c keys.Contact) {
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()
	l, err := s.Link(ctx, 0)
	if err != nil {
		return
	}
	defer l.Close()
	o, err := xfer.ReadOffer(l)
	if err != nil {
		return
	}
	dest := filepath.Join(FilesDir(), c.Name)
	xfer.Receive(ctx, l, o, dest, nil, func(saved string) {
		a.files.add(&Received{Contact: c.Name, Name: filepath.Base(saved), Path: saved, Bytes: o.Manifest.Total})
		a.notify()
	})
}

// drain moves mailbox envelopes into the local log.
func (a *Local) drain(ctx context.Context) error {
	ep, err := a.online(ctx)
	if err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	envs, err := ep.Node.Drain(dctx)
	if err != nil {
		return err
	}
	cs := contactsNow()
	var ids []string
	got := false
	for _, e := range envs {
		m, c, err := msg.Open(a.id, cs, e.Data)
		if err == nil {
			added, serr := a.store.Add(&msg.Record{Dir: "in", Contact: c.Name, Via: "mailbox", Msg: m})
			if serr != nil {
				continue // keep it in the mailbox and try again later
			}
			got = got || added
		}
		// Junk, strangers and stored messages are all removed.
		ids = append(ids, e.ID)
	}
	ep.Node.Ack(dctx, ids)
	if got {
		a.notify()
	}
	return nil
}

// ---- API ----

func (a *Local) Status(ctx context.Context) (*Status, error) {
	cs := contactsNow()
	last, _ := a.store.Last()
	a.mu.Lock()
	defer a.mu.Unlock()
	return &Status{
		Name: a.id.Name, ID: a.id.Public().ID(), Background: a.background, Online: a.ep != nil,
		Listening: a.listening, Rendezvous: a.cfg.Rendezvous, Contacts: len(cs), LastSeq: last,
	}, nil
}

func (a *Local) Contacts(ctx context.Context) ([]Contact, error) {
	cs, err := keys.LoadContacts()
	if err != nil {
		return nil, err
	}
	out := []Contact{}
	for _, n := range cs.Names() {
		out = append(out, Contact{Name: n, ID: cs[n].Key.ID(), Paired: cs[n].Paired})
	}
	return out, nil
}

// ErrUnknownContact means a name is not in the address book.
var ErrUnknownContact = errors.New("unknown contact")

func contact(name string) (keys.Contact, error) {
	cs, err := keys.LoadContacts()
	if err != nil {
		return keys.Contact{}, err
	}
	if c, ok := cs[name]; ok {
		return c, nil
	}
	if len(cs) == 0 {
		return keys.Contact{}, fmt.Errorf("%w: %q; you have no contacts yet, pair first with `ferry pair`", ErrUnknownContact, name)
	}
	return keys.Contact{}, fmt.Errorf("%w: %q; contacts are: %s", ErrUnknownContact, name, strings.Join(cs.Names(), ", "))
}

func (a *Local) Send(ctx context.Context, req SendRequest) (*SendResult, error) {
	c, err := contact(req.To)
	if err != nil {
		return nil, err
	}
	m, err := msg.New(c.Key, req.Type, req.Text, req.Data)
	if err != nil {
		return nil, err
	}
	m.ReplyTo = req.ReplyTo
	m.Sign(a.id)
	ep, err := a.online(ctx)
	if err != nil {
		return nil, err
	}
	via := "direct"
	if err := a.sendDirect(ctx, ep, c, m); err != nil {
		via = "mailbox"
		env, err := msg.Seal(a.id, c.Key, m)
		if err != nil {
			return nil, err
		}
		p, _ := peerOf(c)
		if err := ep.Node.Deposit(ctx, p, env, 14*24*time.Hour); err != nil {
			if strings.Contains(err.Error(), "rejected") {
				return nil, fmt.Errorf("%w: %s is offline and has no mailbox on this rendezvous yet; they need to run ferry once", session.ErrOffline, c.Name)
			}
			return nil, fmt.Errorf("%s is offline, and leaving the message in their mailbox failed: %w", c.Name, err)
		}
	}
	r := &msg.Record{Dir: "out", Contact: c.Name, Via: via, Msg: m}
	a.store.Add(r)
	return &SendResult{ID: m.ID, Via: via, Seq: r.Seq}, nil
}

func (a *Local) sendDirect(ctx context.Context, ep *session.Endpoint, c keys.Contact, m *msg.Message) error {
	dctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	s, err := ep.Dial(dctx, c)
	if err != nil {
		return err
	}
	defer s.Close()
	done := make(chan error, 1)
	go func() {
		if err := s.Conn.SendJSON(session.Intent{Want: "msg"}); err != nil {
			done <- err
			return
		}
		var r session.IntentReply
		if err := s.Conn.RecvJSON(&r); err != nil || !r.OK {
			done <- errors.New("declined")
			return
		}
		if err := s.Conn.SendJSON(m); err != nil {
			done <- err
			return
		}
		var k ack
		if err := s.Conn.RecvJSON(&k); err != nil || !k.OK || k.ID != m.ID {
			done <- errors.New("not acknowledged")
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-dctx.Done():
		return dctx.Err()
	}
}

func (a *Local) Inbox(ctx context.Context, since int64, from string) (*InboxResult, error) {
	if from != "" {
		if _, err := contact(from); err != nil {
			return nil, err
		}
	}
	a.mu.Lock()
	listening := a.listening
	a.mu.Unlock()
	if !listening {
		// Nobody is collecting mail in the background; fetch it now.
		if err := a.drain(ctx); err != nil && !errors.Is(err, session.ErrNoRendezvous) {
			return nil, err
		}
	}
	rs, err := a.store.Since(since, from, "")
	if err != nil {
		return nil, err
	}
	next := since
	for _, r := range rs {
		next = max(next, r.Seq)
	}
	if rs == nil {
		rs = []msg.Record{}
	}
	return &InboxResult{Messages: rs, Next: next}, nil
}

func (a *Local) Wait(ctx context.Context, from string, since int64, timeout time.Duration) (*msg.Record, error) {
	if from != "" {
		if _, err := contact(from); err != nil {
			return nil, err
		}
	}
	if since < 0 {
		since, _ = a.store.Last()
	}
	a.mu.Lock()
	listening := a.listening
	a.mu.Unlock()
	if !listening {
		if err := a.Listen(ctx, false); err != nil {
			return nil, err
		}
	}
	deadline := time.After(timeout)
	for {
		news := a.newsChan()
		rs, err := a.store.Since(since, from, "in")
		if err != nil {
			return nil, err
		}
		if len(rs) > 0 {
			return &rs[0], nil
		}
		select {
		case <-news:
		case <-deadline:
			return nil, ErrTimeout
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (a *Local) SendFile(ctx context.Context, to, path string, progress xfer.Progress) (*FileResult, error) {
	c, err := contact(to)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, err
	}
	m, err := xfer.Scan(abs)
	if err != nil {
		return nil, err
	}
	ep, err := a.online(ctx)
	if err != nil {
		return nil, err
	}
	s, err := ep.Dial(ctx, c)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	if err := s.Conn.SendJSON(session.Intent{Want: "files"}); err != nil {
		return nil, err
	}
	var r session.IntentReply
	if err := s.Conn.RecvJSON(&r); err != nil {
		return nil, err
	}
	if !r.OK {
		return nil, fmt.Errorf("%w: %s says: %s", session.ErrDeclined, c.Name, r.Why)
	}
	l, err := s.Link(ctx, a.cfg.LanesFor(m.Total))
	if err != nil {
		return nil, err
	}
	defer l.Close()
	t0 := time.Now()
	if _, err := xfer.Send(ctx, l, abs, m, progress); err != nil {
		return nil, err
	}
	return &FileResult{Name: m.Name, Bytes: m.Total, Seconds: time.Since(t0).Seconds(), Path: l.Path}, nil
}

func (a *Local) Files(ctx context.Context, since int64) ([]Received, error) {
	return a.files.since(since)
}

func (a *Local) claimTransfer(contact string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy == nil {
		a.busy = map[string]bool{}
	}
	if a.busy[contact] {
		return false
	}
	a.busy[contact] = true
	return true
}

func (a *Local) releaseTransfer(contact string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.busy, contact)
}
