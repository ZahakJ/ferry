package main

import (
	"context"
	"fmt"
	"os"
)

func cmdHelp(ctx context.Context, o *out, args []string) error {
	for _, a := range args {
		if a == "--llm" || a == "-llm" {
			fmt.Print(llmHelp)
			return nil
		}
	}
	if len(args) == 0 {
		usage(o)
		return nil
	}
	for _, c := range commands {
		if c.name == args[0] {
			fmt.Fprintf(os.Stderr, "ferry %s %s\n\n  %s\n", c.name, c.args, c.summary)
			return nil
		}
	}
	return usagef("no help for %q; try `ferry help`", args[0])
}

const llmHelp = `ferry: files and messages between two machines (TODO: full page)
`
