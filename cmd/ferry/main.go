// Command ferry sends files and messages between two machines.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ZahakJ/ferry/internal/pair"
	"github.com/ZahakJ/ferry/internal/secure"
	"github.com/ZahakJ/ferry/internal/session"
)

var version = "dev"

type command struct {
	name    string
	args    string
	summary string
	run     func(ctx context.Context, o *out, args []string) error
}

var commands []command

func init() {
	commands = []command{
		{"send", "[contact] <path>", "send a file or folder; prints a code unless a contact is named", cmdSend},
		{"get", "[code] [--to dir]", "receive with a code, or from a contact", cmdGet},
		{"pair", "[code] [--name name]", "pair with someone once, so later sends need no code", cmdPair},
		{"contacts", "", "list paired contacts", cmdContacts},
		{"forget", "<contact>", "remove a contact", cmdForget},
		{"config", "[key [value]]", "show or change settings", cmdConfig},
		{"version", "", "print the version", cmdVersion},
		{"help", "[topic] [--llm]", "show help", cmdHelp},
	}
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	jsonMode := false
	var args []string
	for _, a := range argv {
		if a == "--json" || a == "-json" {
			jsonMode = true
			continue
		}
		args = append(args, a)
	}
	o := newOut(jsonMode)
	if len(args) == 0 {
		usage(o)
		return exitUsage
	}
	name := args[0]
	if name == "-h" || name == "--help" {
		name = "help"
	}
	if name == "--version" || name == "-v" {
		name = "version"
	}
	var cmd *command
	for i := range commands {
		if commands[i].name == name {
			cmd = &commands[i]
		}
	}
	if cmd == nil {
		o.say("ferry: %q is not a command.", name)
		if s := suggest(name); s != "" {
			o.say("Did you mean `ferry %s`?", s)
		}
		o.say("Run `ferry help` to see them all.")
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := cmd.run(ctx, o, args[1:])
	if err == nil {
		return exitOK
	}
	code := exitCode(ctx, err)
	if code == exitInterrupt {
		o.say("Stopped.")
	} else {
		o.say("%s %s", o.red("ferry:"), explain(err))
	}
	o.event("error", map[string]any{"error": explain(err), "exit": code, "kind": kind(code)})
	return code
}

// usageError is a mistake in how a command was called.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

func exitCode(ctx context.Context, err error) int {
	var ue usageError
	switch {
	case ctx.Err() != nil && errors.Is(err, context.Canceled):
		return exitInterrupt
	case errors.As(err, &ue), errors.Is(err, flag.ErrHelp):
		return exitUsage
	case errors.Is(err, pair.ErrWrongCode), errors.Is(err, session.ErrCodeUsed), errors.Is(err, secure.ErrBadFrame), errors.Is(err, errAuth):
		return exitAuth
	case errors.Is(err, session.ErrOffline), errors.Is(err, session.ErrNobodyThere), errors.Is(err, errUnreachable):
		return exitOffline
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, errTimeout):
		return exitTimeout
	case errors.Is(err, errIntegrity):
		return exitIntegrity
	case errors.Is(err, errDeclined):
		return exitDeclined
	}
	return exitError
}

var (
	errAuth        = errors.New("authentication failed")
	errUnreachable = errors.New("unreachable")
	errTimeout     = errors.New("timed out")
	errIntegrity   = errors.New("integrity check failed")
	errDeclined    = errors.New("declined")
)

func kind(code int) string {
	return map[int]string{
		exitError: "error", exitUsage: "usage", exitAuth: "auth", exitOffline: "offline",
		exitTimeout: "timeout", exitIntegrity: "integrity", exitDeclined: "declined", exitInterrupt: "interrupted",
	}[code]
}

// explain turns an error into a sentence that says what to do.
func explain(err error) string {
	switch {
	case errors.Is(err, session.ErrNoRendezvous):
		return "no rendezvous is set. ferry needs one to find the other machine:\n" +
			"  ferry config rendezvous /dns4/<host>/tcp/4001/p2p/<peer-id>\n" +
			"Ask whoever runs your Concord rendezvous for its address."
	case errors.Is(err, pair.ErrWrongCode):
		return "the codes did not match. Check the words with the other person and try again with a new code."
	}
	return err.Error()
}

func suggest(name string) string {
	aliases := map[string]string{"receive": "get", "recv": "get", "download": "get", "upload": "send", "doctor": "check", "status": "check"}
	if s, ok := aliases[name]; ok {
		return s
	}
	for _, c := range commands {
		if strings.HasPrefix(c.name, name) || strings.HasPrefix(name, c.name) {
			return c.name
		}
	}
	return ""
}

func usage(o *out) {
	fmt.Fprintf(os.Stderr, "%s — send files and messages between two machines.\n\n", o.bold("ferry"))
	fmt.Fprintln(os.Stderr, "  ferry send ./photos         prints a code; the other side runs ferry get <code>")
	fmt.Fprintln(os.Stderr, "  ferry pair                  pair once; then: ferry send <name> ./file")
	fmt.Fprintln(os.Stderr)
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  %-30s %s\n", c.name+" "+c.args, o.dim(c.summary))
	}
	fmt.Fprintln(os.Stderr, "\nAdd --json to any command for machine-readable output. More: ferry help <command>")
}

// parse runs fs over args, allowing flags anywhere, and returns positionals.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(new(strings.Builder))
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usagef("%v", err)
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func cmdVersion(ctx context.Context, o *out, args []string) error {
	o.result("ferry %s", version)
	o.event("version", map[string]any{"version": version})
	return nil
}
