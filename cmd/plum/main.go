// Command plum is a comprehension-debt system for agent-written code.
//
// When an agent builds a feature, the code may work while your mental model of
// the repository silently goes stale. plum extracts mechanical evidence about
// what changed, synthesises it without inherited context, lets you explore the
// result as a navigable energy landscape, and only then interrogates you against
// recorded execution.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/k3-mt/plum/internal/cli"
)

// version is set at build time with -X main.version.
var version = "dev"

func main() {
	cli.Version = version
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Main(ctx, os.Args[1:]))
}
