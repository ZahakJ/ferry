package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ZahakJ/ferry/internal/agent"
	"github.com/ZahakJ/ferry/internal/keys"
	"github.com/ZahakJ/ferry/internal/pipe"
	"github.com/ZahakJ/ferry/internal/session"
)

// cmdPipe connects stdin and stdout to a port on a contact's localhost,
// over the same direct link files use. It is made for ssh:
//
//	ssh -o ProxyCommand='ferry pipe box 2222' friend@box
//
// stdout carries only the stream; everything ferry says goes to stderr.
func cmdPipe(ctx context.Context, o *out, args []string) error {
	fs := flag.NewFlagSet("pipe", flag.ContinueOnError)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return usagef("usage: ferry pipe <contact> [port]")
	}
	port := 0 // none named: the one port the other side serves, if it serves exactly one
	if len(pos) == 2 {
		if port, err = strconv.Atoi(pos[1]); err != nil || port < 1 || port > 65535 {
			return usagef("%q is not a port", pos[1])
		}
	}
	cfg, err := session.LoadConfig()
	if err != nil {
		return err
	}
	id, err := keys.Load()
	if err != nil {
		return fmt.Errorf("%w; pair with the other machine first: ferry pair <code>", err)
	}
	cs, err := keys.LoadContacts()
	if err != nil {
		return err
	}
	c, ok := cs[pos[0]]
	if !ok {
		return fmt.Errorf("%w: %q is not a contact (ferry contacts)", errUnreachable, pos[0])
	}
	// With ferry up running, the pipe goes through it, so this machine is
	// one peer at the rendezvous however many pipes it has open.
	if agent.Running() {
		err := agent.Pipe(ctx, c.Name, port, os.Stdin, os.Stdout, func() { os.Stdout.Close() })
		if !errors.Is(err, agent.ErrNoAgentPipe) {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			if err != nil && strings.Contains(err.Error(), "declined: ") {
				return fmt.Errorf("%w: %s", errDeclined, strings.SplitN(err.Error(), "declined: ", 2)[1])
			}
			return err
		}
	}
	ep, err := session.Online(ctx, cfg, id)
	if err != nil {
		return err
	}
	defer ep.Close()
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	s, err := ep.Dial(dctx, c)
	cancel()
	if err != nil {
		if strings.Contains(err.Error(), "stream reset") {
			return fmt.Errorf("%w: %s turned this machine away; it does not know it as a contact (pair again)", errAuth, c.Name)
		}
		return err
	}
	defer s.Close()
	if err := s.Conn.SendJSON(session.Intent{Want: "pipe", Port: port}); err != nil {
		return err
	}
	var r session.IntentReply
	if err := s.Conn.RecvJSON(&r); err != nil {
		return err
	}
	if !r.OK {
		return fmt.Errorf("%w: %s", errDeclined, r.Why)
	}
	l, err := s.Link(ctx, 1)
	if err != nil {
		return err
	}
	defer l.Close()
	if os.Getenv("FERRY_PIPE_VERBOSE") != "" {
		o.say("ferry: pipe to %s is up (%s)", c.Name, l.Path)
	}
	_, _, err = pipe.Pump(ctx, l, os.Stdin, os.Stdout, func() { os.Stdout.Close() })
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
