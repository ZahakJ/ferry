package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/ZahakJ/ferry/internal/agent"
	"github.com/ZahakJ/ferry/internal/keys"
	"github.com/ZahakJ/ferry/internal/msg"
	"github.com/ZahakJ/ferry/internal/session"
)

func openAgent() (agent.API, error) {
	cfg, err := session.LoadConfig()
	if err != nil {
		return nil, err
	}
	return agent.Open(cfg)
}

func cmdMsg(ctx context.Context, o *out, args []string) error {
	fs := flag.NewFlagSet("msg", flag.ContinueOnError)
	typ := fs.String("type", msg.Note, "note, request, patch, file-offer, or your own")
	data := fs.String("data", "", "JSON to attach")
	reply := fs.String("reply-to", "", "ID of the message this answers")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return usagef(`usage: ferry msg <contact> "text"   (or pipe the text in)`)
	}
	to := pos[0]
	text := strings.Join(pos[1:], " ")
	if text == "-" || (text == "" && !term.IsTerminal(int(os.Stdin.Fd()))) {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, msg.MaxText+1))
		if err != nil {
			return err
		}
		text = strings.TrimRight(string(b), "\n")
	}
	if text == "" && *data == "" {
		return usagef(`nothing to send: ferry msg %s "text"`, to)
	}
	a, err := openAgent()
	if err != nil {
		return err
	}
	defer a.Close()
	r, err := a.Send(ctx, agent.SendRequest{To: to, Type: *typ, Text: text, Data: json.RawMessage(*data), ReplyTo: *reply})
	if err != nil {
		return err
	}
	how := "delivered"
	if r.Via == "mailbox" {
		how = "left in their mailbox; they get it next time they are online"
	}
	o.say("%s Sent to %s, %s.", o.green("✓"), to, how)
	o.event("sent", map[string]any{"id": r.ID, "to": to, "via": r.Via, "seq": r.Seq})
	return nil
}

func printRecord(o *out, r msg.Record) {
	if o.json {
		o.event("message", recordJSON(r))
		return
	}
	who := clean(r.Contact)
	arrow := o.dim("←")
	if r.Dir == "out" {
		arrow = o.dim("→")
	}
	t := r.Msg.Time().Local()
	when := t.Format("15:04")
	if time.Since(t) > 20*time.Hour {
		when = t.Format("Jan 2 15:04")
	}
	typ := ""
	if r.Msg.Type != msg.Note {
		typ = o.bold("["+clean(r.Msg.Type)+"]") + " "
	}
	body := clean(r.Msg.Text)
	if len(r.Msg.Data) > 0 {
		body = strings.TrimSpace(body + " " + o.dim(clean(string(r.Msg.Data))))
	}
	o.result("%s %s %-10s %s  %s%s", o.dim(fmt.Sprintf("#%d", r.Seq)), arrow, who, o.dim(when), typ, body)
}

func recordJSON(r msg.Record) map[string]any {
	m := map[string]any{
		"seq": r.Seq, "dir": r.Dir, "contact": r.Contact, "via": r.Via,
		"id": r.Msg.ID, "type": r.Msg.Type, "text": r.Msg.Text, "sent": r.Msg.Time().UTC(), "logged": r.At,
	}
	if len(r.Msg.Data) > 0 {
		m["data"] = r.Msg.Data
	}
	if r.Msg.ReplyTo != "" {
		m["reply_to"] = r.Msg.ReplyTo
	}
	return m
}

func cmdInbox(ctx context.Context, o *out, args []string) error {
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	since := fs.Int64("since", 0, "only messages after this number")
	from := fs.String("from", "", "only this contact")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	a, err := openAgent()
	if err != nil {
		return err
	}
	defer a.Close()
	res, err := a.Inbox(ctx, *since, *from)
	if err != nil {
		return err
	}
	for _, r := range res.Messages {
		printRecord(o, r)
	}
	if len(res.Messages) == 0 {
		o.say(o.dim("No new messages."))
	} else {
		o.say(o.dim(fmt.Sprintf("Next time: ferry inbox --since %d", res.Next)))
	}
	o.event("cursor", map[string]any{"next": res.Next})
	return nil
}

func cmdWait(ctx context.Context, o *out, args []string) error {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	from := fs.String("from", "", "only this contact")
	timeout := fs.Duration("timeout", 10*time.Minute, "give up after this long (e.g. 30s, 10m, 2h)")
	since := fs.Int64("since", -1, "return the first message after this number (default: from now)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	a, err := openAgent()
	if err != nil {
		return err
	}
	defer a.Close()
	who := "anyone"
	if *from != "" {
		who = *from
	}
	o.say(o.dim(fmt.Sprintf("Waiting up to %s for a message from %s…", human(*timeout), who)))
	r, err := a.Wait(ctx, *from, *since, *timeout)
	if errors.Is(err, agent.ErrTimeout) {
		return fmt.Errorf("%w: nothing from %s in %s", errTimeout, who, human(*timeout))
	}
	if err != nil {
		return err
	}
	printRecord(o, *r)
	return nil
}

func cmdFiles(ctx context.Context, o *out, args []string) error {
	fs := flag.NewFlagSet("files", flag.ContinueOnError)
	since := fs.Int64("since", 0, "only files after this number")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	a, err := openAgent()
	if err != nil {
		return err
	}
	defer a.Close()
	list, err := a.Files(ctx, *since)
	if err != nil {
		return err
	}
	if o.json {
		o.event("files", map[string]any{"files": list})
		return nil
	}
	if len(list) == 0 {
		o.say(o.dim("No files received yet. Files contacts send while `ferry up` runs land in " + agent.FilesDir()))
	}
	for _, f := range list {
		o.result("%s %-10s %-9s %s", o.dim(fmt.Sprintf("#%d", f.Seq)), clean(f.Contact), size(f.Bytes), clean(f.Path))
	}
	return nil
}

func cmdUp(ctx context.Context, o *out, args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	unit := fs.Bool("unit", false, "print a systemd user unit for running ferry in the background, and how to install it")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *unit {
		exe, _ := os.Executable()
		fmt.Printf(unitFile, exe)
		o.say("")
		o.say("To install it (ferry does not do this for you):")
		o.say("  mkdir -p ~/.config/systemd/user")
		o.say("  ferry up --unit > ~/.config/systemd/user/ferry.service")
		o.say("  systemctl --user daemon-reload && systemctl --user enable --now ferry")
		return nil
	}
	cfg, err := session.LoadConfig()
	if err != nil {
		return err
	}
	if agent.Running() {
		return errors.New("ferry is already up on this machine")
	}
	a, err := agent.NewLocal(cfg)
	if err != nil {
		return err
	}
	defer a.Close()
	if err := a.Listen(ctx, true); err != nil {
		return fmt.Errorf("%w: %v", errUnreachable, err)
	}
	st, _ := a.Status(ctx)
	id, _ := keys.Load()
	cs, _ := keys.LoadContacts()
	o.say("%s ferry is up as %s %s. %d contact(s) can reach this machine.", o.green("●"), o.bold(id.Name), o.dim(st.ID), len(cs))
	o.say(o.dim("Messages are kept for `ferry inbox`; files land in " + agent.FilesDir() + ". Ctrl-C to stop."))
	o.event("up", map[string]any{"name": id.Name, "id": st.ID, "socket": agent.SocketPath()})
	err = agent.Serve(ctx, a)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

const unitFile = `[Unit]
Description=ferry agent: keeps this machine reachable by its contacts
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s up
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`
