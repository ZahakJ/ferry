package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ZahakJ/ferry/internal/agent"
	"github.com/ZahakJ/ferry/internal/keys"
	"github.com/ZahakJ/ferry/internal/link"
	"github.com/ZahakJ/ferry/internal/pair"
	"github.com/ZahakJ/ferry/internal/session"
	"github.com/ZahakJ/ferry/internal/xfer"
)

// intent opens every contact session: what the dialer wants.
type intent struct {
	Want string `json:"want"` // files | msg
}

type intentReply struct {
	OK  bool   `json:"ok"`
	Why string `json:"why,omitempty"`
}

func cmdSend(ctx context.Context, o *out, args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	lanes := fs.Int("lanes", 0, "parallel connections (default from config, 8)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	var to, path string
	switch len(pos) {
	case 1:
		path = pos[0]
	case 2:
		to, path = pos[0], pos[1]
	default:
		return usagef("usage: ferry send [contact] <path>")
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("cannot send %s: %w", path, errors.Unwrap(err))
	}
	m, err := xfer.Scan(path)
	if err != nil {
		return err
	}
	for _, s := range m.Skipped {
		o.say("%s %s (not a regular file)", o.dim("skipping"), s)
	}
	cfg, err := session.LoadConfig()
	if err != nil {
		return err
	}
	what := describe(m)

	var s *session.Session
	if to == "" {
		s, err = session.Offer(ctx, cfg, session.PurposeSend, func(c pair.Code) {
			o.event("code", map[string]any{"code": c.String(), "name": m.Name, "bytes": m.Total, "files": countFiles(m)})
			if o.json {
				return
			}
			o.say("Sending %s.", what)
			o.say("On the other machine, run:")
			o.say("")
			o.result("    ferry get %s", c)
			o.say("")
			o.say(o.dim("Waiting for them… (Ctrl-C to cancel)"))
		})
		if err != nil {
			return err
		}
		o.say("Connected. %s", o.dim("(both sides see: "+s.SAS+")"))
		defer s.Close()
		return sendOver(ctx, o, s, path, m, *lanes)
	}
	return sendToContact(ctx, o, to, path, m)
}

func sendToContact(ctx context.Context, o *out, to, path string, m *xfer.Manifest) error {
	a, err := openAgent()
	if err != nil {
		return err
	}
	defer a.Close()
	o.say("Sending %s to %s.", describe(m), to)
	p := o.progress("↑", m.Total)
	r, err := a.SendFile(ctx, to, path, p.update)
	p.end()
	if err != nil {
		return err
	}
	o.say("%s Sent %s in %s (%s/s, %s).", o.green("✓"), size(r.Bytes), human(time.Duration(r.Seconds*float64(time.Second))), size(int64(float64(r.Bytes)/max(r.Seconds, 0.001))), r.Path)
	o.event("done", map[string]any{"name": r.Name, "bytes": r.Bytes, "seconds": r.Seconds, "path": r.Path})
	return nil
}

func sendOver(ctx context.Context, o *out, s *session.Session, path string, m *xfer.Manifest, lanes int) error {
	l, err := s.Link(ctx, lanes)
	if err != nil {
		return err
	}
	defer l.Close()
	o.event("connected", map[string]any{"path": l.Path, "lanes": len(l.Lanes)})
	p := o.progress("↑", m.Total)
	saved, err := xfer.Send(ctx, l, path, m, p.update)
	p.end()
	if err != nil {
		if strings.Contains(err.Error(), "does not match") || strings.Contains(err.Error(), "integrity") {
			return fmt.Errorf("%w: %v", errIntegrity, err)
		}
		if strings.Contains(err.Error(), "declined") {
			return fmt.Errorf("%w: %v", errDeclined, err)
		}
		return err
	}
	avg := p.average()
	o.say("%s Sent %s in %s (%s/s, %s).", o.green("✓"), size(m.Total), human(time.Since(p.start)), size(int64(avg)), l.Path)
	o.event("done", map[string]any{"name": m.Name, "saved_as": saved, "bytes": m.Total, "seconds": time.Since(p.start).Seconds(), "path": l.Path})
	return nil
}

func dialContact(ctx context.Context, o *out, cfg *session.Config, name, want string) (*session.Session, error) {
	id, err := keys.Load()
	if errors.Is(err, keys.ErrNoIdentity) {
		return nil, fmt.Errorf("you have no contacts yet. Pair first: `ferry pair`")
	}
	if err != nil {
		return nil, err
	}
	c, err := findContact(name)
	if err != nil {
		return nil, err
	}
	ep, err := session.Online(ctx, cfg, id)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUnreachable, err)
	}
	s, err := ep.Dial(ctx, c)
	if err != nil {
		ep.Close()
		return nil, err
	}
	if err := s.Conn.SendJSON(intent{Want: want}); err != nil {
		ep.Close()
		return nil, err
	}
	var r intentReply
	if err := s.Conn.RecvJSON(&r); err != nil {
		ep.Close()
		return nil, err
	}
	if !r.OK {
		ep.Close()
		return nil, fmt.Errorf("%w: %s says: %s", errDeclined, name, r.Why)
	}
	s.Own(ep)
	return s, nil
}

func findContact(name string) (keys.Contact, error) {
	cs, err := keys.LoadContacts()
	if err != nil {
		return keys.Contact{}, err
	}
	if c, ok := cs[name]; ok {
		return c, nil
	}
	if len(cs) == 0 {
		return keys.Contact{}, fmt.Errorf("%q is not a contact, and you have none yet. Pair first: `ferry pair`", name)
	}
	return keys.Contact{}, fmt.Errorf("%q is not a contact. You have: %s", name, strings.Join(cs.Names(), ", "))
}

func cmdGet(ctx context.Context, o *out, args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	dest := fs.String("to", ".", "folder to save into")
	from := fs.String("from", "", "only accept from this contact")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	cfg, err := session.LoadConfig()
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		code, err := pair.Parse(strings.Join(pos, "-"))
		if err != nil {
			return usagef("%v", err)
		}
		s, err := session.Join(ctx, cfg, session.PurposeSend, code)
		if err != nil {
			return err
		}
		defer s.Close()
		o.say("Connected. %s", o.dim("(both sides see: "+s.SAS+")"))
		return receiveOver(ctx, o, s, *dest, 0)
	}
	return getFromContact(ctx, o, cfg, *from, *dest, 0)
}

func receiveOver(ctx context.Context, o *out, s *session.Session, dest string, lanes int) error {
	l, err := s.Link(ctx, lanes)
	if err != nil {
		return err
	}
	defer l.Close()
	return receiveOnLink(ctx, o, l, dest)
}

func receiveOnLink(ctx context.Context, o *out, l *link.Link, dest string) error {
	offer, err := xfer.ReadOffer(l)
	if err != nil {
		return err
	}
	m := offer.Manifest
	o.say("Receiving %s.", describe(m))
	o.event("offer", map[string]any{"name": m.Name, "bytes": m.Total, "files": countFiles(m), "path": l.Path})
	p := o.progress("↓", m.Total)
	saved, err := xfer.Receive(ctx, l, offer, dest, p.update, nil)
	p.end()
	if err != nil {
		if strings.Contains(err.Error(), "does not match") || strings.Contains(err.Error(), "integrity") {
			return fmt.Errorf("%w: %v", errIntegrity, err)
		}
		if ctx.Err() == nil {
			return fmt.Errorf("%v\nRun the same command again (with a new code) to resume where it stopped", err)
		}
		return err
	}
	rel := saved
	if r, err := filepath.Rel(".", saved); err == nil && !strings.HasPrefix(r, "..") {
		rel = r
	}
	o.say("%s Received %s in %s (%s/s). Checked: every byte matches.", o.green("✓"), size(m.Total), human(time.Since(p.start)), size(int64(p.average())))
	o.result("%s", rel)
	abs, _ := filepath.Abs(saved)
	o.event("done", map[string]any{"saved": abs, "bytes": m.Total, "seconds": time.Since(p.start).Seconds(), "verified": true})
	return nil
}

func getFromContact(ctx context.Context, o *out, cfg *session.Config, from, dest string, lanes int) error {
	if agent.Running() {
		return usagef("`ferry up` is running, and it already receives files from contacts into %s; see them with `ferry files`", agent.FilesDir())
	}
	id, err := keys.Load()
	if errors.Is(err, keys.ErrNoIdentity) {
		return usagef("give a code (ferry get <code>), or pair with someone first (ferry pair)")
	}
	if err != nil {
		return err
	}
	if from != "" {
		if _, err := findContact(from); err != nil {
			return err
		}
	}
	ep, err := session.Online(ctx, cfg, id)
	if err != nil {
		return fmt.Errorf("%w: %v", errUnreachable, err)
	}
	defer ep.Close()
	type got struct{ err error }
	done := make(chan got, 1)
	busy := make(chan struct{}, 1)
	err = ep.Listen(ctx, func() keys.Contacts { cs, _ := keys.LoadContacts(); return cs }, func(s *session.Session, c keys.Contact) {
		defer s.Close()
		var in intent
		if s.Conn.RecvJSON(&in) != nil {
			return
		}
		if in.Want != "files" || (from != "" && c.Name != from) {
			s.Conn.SendJSON(intentReply{Why: "not accepting that right now"})
			return
		}
		select {
		case busy <- struct{}{}:
		default:
			s.Conn.SendJSON(intentReply{Why: "busy with another transfer; try again shortly"})
			return
		}
		s.Conn.SendJSON(intentReply{OK: true})
		o.say("%s is sending.", c.Name)
		done <- got{receiveOver(ctx, o, s, dest, lanes)}
	})
	if err != nil {
		return fmt.Errorf("%w: %v", errUnreachable, err)
	}
	who := "a contact"
	if from != "" {
		who = from
	}
	o.say("Waiting for %s to send… (Ctrl-C to stop)", who)
	o.event("waiting", map[string]any{"from": from})
	select {
	case g := <-done:
		return g.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func describe(m *xfer.Manifest) string {
	n := countFiles(m)
	if n == 1 && m.Files[0].Path == "" {
		return fmt.Sprintf("%s (%s)", m.Name, size(m.Total))
	}
	files := "files"
	if n == 1 {
		files = "file"
	}
	return fmt.Sprintf("%s/ (%d %s, %s)", m.Name, n, files, size(m.Total))
}

func countFiles(m *xfer.Manifest) int {
	n := 0
	for _, f := range m.Files {
		if !f.Dir {
			n++
		}
	}
	return n
}
