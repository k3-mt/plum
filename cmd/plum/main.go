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
	"runtime/debug"
	"syscall"

	"github.com/k3-mt/plum/internal/cli"
)

// version is set at build time with -X main.version, which is what `make build`
// and the release workflow do.
var version = "dev"

// resolveVersion falls back to the module version Go stamps into the binary.
// `go install github.com/k3-mt/plum/cmd/plum@v0.1.0` never runs the Makefile, so
// without this the headline install route produces a binary that calls itself
// "dev" — and a tool about knowing what you are running should know that first.
func resolveVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	// Built from a checkout rather than installed: the commit is the version.
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev != "" {
		if len(rev) > 12 {
			rev = rev[:12]
		}
		return rev + dirty
	}
	return version
}

func main() {
	cli.Version = resolveVersion()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Main(ctx, os.Args[1:]))
}
