package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/ZahakJ/ferry/internal/agent"
	"github.com/ZahakJ/ferry/internal/keys"
	"github.com/ZahakJ/ferry/internal/link"
	"github.com/ZahakJ/ferry/internal/rv"
	"github.com/ZahakJ/ferry/internal/session"
)

// cmdCheck tests each thing ferry needs, in the order it needs them, and
// says plainly what each result means.
func cmdCheck(ctx context.Context, o *out, args []string) error {
	args, err := parse(flag.NewFlagSet("check", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	cfg, err := session.LoadConfig()
	if err != nil {
		return err
	}
	type result struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	}
	var results []result
	report := func(name string, ok bool, detail string) {
		results = append(results, result{name, ok, detail})
		mark := o.green("✓")
		if !ok {
			mark = o.red("✗")
		}
		o.result("%s %-12s %s", mark, name, detail)
	}

	id, err := keys.Load()
	switch {
	case errors.Is(err, keys.ErrNoIdentity):
		report("identity", true, "none yet; one is made the first time you pair")
	case err != nil:
		report("identity", false, err.Error())
	default:
		cs, _ := keys.LoadContacts()
		report("identity", true, fmt.Sprintf("%s (%s), %d contact(s)", id.Name, id.Public().ID(), len(cs)))
	}

	if cfg.Rendezvous == "" {
		report("rendezvous", false, "not set: ferry config rendezvous <multiaddr>")
		o.event("check", map[string]any{"results": results})
		return fmt.Errorf("%w: %v", errUnreachable, session.ErrNoRendezvous)
	}
	t0 := time.Now()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	node, err := rv.Dial(cctx, cfg.Rendezvous, nil)
	cancel()
	if err != nil {
		report("rendezvous", false, err.Error())
	} else {
		report("rendezvous", true, fmt.Sprintf("reached in %s", time.Since(t0).Round(time.Millisecond)))
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := node.Reserve(rctx); err != nil {
			report("relay slot", false, err.Error()+" (contacts could not reach this machine)")
		} else {
			report("relay slot", true, "granted: contacts can reach this machine for signaling")
		}
		cancel()
		node.Close()
	}

	gctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	lo := cfg.LinkOptions(gctx, 1)
	if cfg.Port > 0 {
		switch bound, pub := link.FixedPort(lo); {
		case !bound:
			report("port", false, fmt.Sprintf("udp %d is taken (by `ferry up`?); this run uses any free port", cfg.Port))
		case len(pub) > 0:
			report("port", true, fmt.Sprintf("udp %d, also offered as %s:%d for a router port forward", cfg.Port, strings.Join(pub, ", "), cfg.Port))
		default:
			report("port", true, fmt.Sprintf("udp %d", cfg.Port))
		}
	}
	cands, err := link.Gather(gctx, lo)
	cancel()
	if err != nil {
		report("network", false, err.Error())
	} else {
		var host, srflx []string
		for _, c := range cands {
			switch c.Type {
			case "host":
				host = append(host, c.Address)
			case "srflx":
				srflx = append(srflx, c.Address)
			}
		}
		report("local", len(host) > 0, strings.Join(uniq(host), " "))
		if len(srflx) > 0 {
			report("public", true, strings.Join(uniq(srflx), " ")+" (from STUN; direct connections are likely)")
		} else {
			report("public", false, "STUN gave no public address: this network seems to block UDP, so direct connections will likely fail. Try another network, or a VPN or tailnet")
		}
	}
	if agent.Running() {
		report("agent", true, "ferry up is running")
	} else {
		report("agent", true, "not running (start with `ferry up` to stay reachable)")
	}
	o.event("check", map[string]any{"results": results})
	for _, r := range results {
		if !r.OK && (r.Name == "rendezvous" || r.Name == "relay slot") {
			return fmt.Errorf("%w: ferry cannot work until the rendezvous is reachable", errUnreachable)
		}
	}
	return nil
}

func uniq(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
