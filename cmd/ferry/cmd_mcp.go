package main

import (
	"context"
	"os"

	"github.com/ZahakJ/ferry/internal/agent"
	"github.com/ZahakJ/ferry/internal/mcp"
	"github.com/ZahakJ/ferry/internal/session"
)

// cmdMCP serves the MCP tools on stdin/stdout. If `ferry up` is running it
// forwards to that agent; otherwise it is the agent for as long as the
// model's session lasts, and shares itself with shell commands too.
func cmdMCP(ctx context.Context, o *out, args []string) error {
	cfg, err := session.LoadConfig()
	if err != nil {
		return err
	}
	var a agent.API
	if agent.Running() {
		a = &agent.Remote{}
	} else {
		l, err := agent.NewLocal(cfg)
		if err != nil {
			return err
		}
		defer l.Close()
		// Going online can fail (no network yet); tools then report it and retry.
		go l.Listen(ctx, false)
		go agent.Serve(ctx, l)
		a = l
	}
	s := &mcp.Server{A: a, Version: version}
	return s.Serve(ctx, os.Stdin, os.Stdout)
}
