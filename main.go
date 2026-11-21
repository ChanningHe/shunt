// Command shunt is a local passthrough router for Claude Code: requests are
// forwarded to Anthropic unchanged, except subagent requests for a model
// shunt/<route>, which are sanitized and sent to the route's third-party
// Anthropic-compatible provider.
package main

import (
	"fmt"
	"os"

	"shunt/internal/cli"
)

func main() {
	if err := cli.Run(os.Stdout, os.Stderr, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "shunt:", err)
		os.Exit(1)
	}
}
