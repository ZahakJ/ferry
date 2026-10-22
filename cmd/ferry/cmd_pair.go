package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/ZahakJ/ferry/internal/keys"
	"github.com/ZahakJ/ferry/internal/pair"
	"github.com/ZahakJ/ferry/internal/session"
)

func cmdPair(ctx context.Context, o *out, args []string) error {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	name := fs.String("name", "", "what to call them (default: the name they chose)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *name != "" {
		if err := keys.CheckName(*name); err != nil {
			return usagef("%v", err)
		}
	}
	id, created, err := keys.LoadOrCreate()
	if err != nil {
		return err
	}
	if created {
		o.say("Made a new identity for this machine, called %s %s.", o.bold(id.Name), o.dim("(change it: ferry config name <name>)"))
	}
	cfg, err := session.LoadConfig()
	if err != nil {
		return err
	}
	var s *session.Session
	if len(pos) == 0 {
		s, err = session.Offer(ctx, cfg, session.PurposePair, func(c pair.Code) {
			o.event("code", map[string]any{"code": c.String()})
			if o.json {
				return
			}
			o.say("Tell the other person to run:")
			o.say("")
			o.result("    ferry pair %s", c)
			o.say("")
			o.say(o.dim("Say it out loud or send it privately. It works once. Waiting…"))
		})
	} else {
		code, perr := pair.Parse(strings.Join(pos, "-"))
		if perr != nil {
			return usagef("%v", perr)
		}
		s, err = session.Join(ctx, cfg, session.PurposePair, code)
	}
	if err != nil {
		return err
	}
	defer s.Close()
	peer, err := session.Introduce(s, id)
	if err != nil {
		return err
	}
	want := *name
	if want == "" {
		want = peer.Name
	}
	saved, err := session.Remember(want, peer)
	if err != nil {
		return err
	}
	o.say("%s Paired with %s. Both screens should show the same two words: %s", o.green("✓"), o.bold(saved), o.bold(s.SAS))
	o.say("From now on:  ferry send %s ./file    ferry msg %s \"hello\"", saved, saved)
	o.event("paired", map[string]any{"name": saved, "id": peer.ID(), "check": s.SAS})
	return nil
}

func cmdContacts(ctx context.Context, o *out, args []string) error {
	cs, err := keys.LoadContacts()
	if err != nil {
		return err
	}
	var list []map[string]any
	for _, n := range cs.Names() {
		c := cs[n]
		list = append(list, map[string]any{"name": n, "id": c.Key.ID(), "paired": c.Paired})
	}
	if o.json {
		o.event("contacts", map[string]any{"contacts": list})
		return nil
	}
	if len(cs) == 0 {
		o.say("No contacts yet. Pair with someone: ferry pair")
		return nil
	}
	for _, n := range cs.Names() {
		c := cs[n]
		o.result("%-16s %s  paired %s", n, o.dim(c.Key.ID()), c.Paired.Local().Format("2006-01-02"))
	}
	return nil
}

func cmdForget(ctx context.Context, o *out, args []string) error {
	if len(args) != 1 {
		return usagef("usage: ferry forget <contact>")
	}
	cs, err := keys.LoadContacts()
	if err != nil {
		return err
	}
	if _, ok := cs[args[0]]; !ok {
		_, err := findContact(args[0])
		return err
	}
	delete(cs, args[0])
	if err := cs.Save(); err != nil {
		return err
	}
	o.say("Forgot %s. To talk again, pair again.", args[0])
	o.event("forgot", map[string]any{"name": args[0]})
	return nil
}

func cmdConfig(ctx context.Context, o *out, args []string) error {
	cfg, err := session.LoadConfig()
	if err != nil {
		return err
	}
	id, _ := keys.Load()
	show := func() {
		name := ""
		if id != nil {
			name = id.Name
		}
		o.event("config", map[string]any{"name": name, "rendezvous": cfg.Rendezvous, "rendezvous_http": cfg.RendezvousHTTP, "lanes": cfg.Lanes, "dir": keys.Dir()})
		o.result("name             %s", name)
		o.result("rendezvous       %s", cfg.Rendezvous)
		o.result("rendezvous_http  %s", cfg.RendezvousHTTP)
		o.result("lanes            %d", cfg.Lanes)
		o.result("%s", o.dim("stored in "+keys.Dir()))
	}
	switch len(args) {
	case 0:
		show()
		return nil
	case 2:
	default:
		return usagef("usage: ferry config [name|rendezvous|rendezvous_http|lanes <value>]")
	}
	k, v := args[0], args[1]
	// Edit the file as written, without environment overrides.
	if cfg, err = session.LoadFileConfig(); err != nil {
		return err
	}
	switch k {
	case "name":
		if err := keys.CheckName(v); err != nil {
			return usagef("%v", err)
		}
		if id == nil {
			if id, _, err = keys.LoadOrCreate(); err != nil {
				return err
			}
		}
		id.Name = v
		if err := keys.Save(id); err != nil {
			return err
		}
		o.say("This machine is now called %s. Contacts you pair with from now on will see that name.", v)
		return nil
	case "rendezvous":
		cfg.Rendezvous = v
	case "rendezvous_http":
		cfg.RendezvousHTTP = strings.TrimRight(v, "/")
	case "lanes":
		var n int
		if _, err := fmt.Sscan(v, &n); err != nil || n < 1 || n > 32 {
			return usagef("lanes is a number from 1 to 32")
		}
		cfg.Lanes = n
	default:
		return usagef("unknown setting %q; settings are name, rendezvous, rendezvous_http, lanes", k)
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	o.say("Saved.")
	return nil
}

var errNoHelp = errors.New("no such help topic")
