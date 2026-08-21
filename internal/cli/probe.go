package cli

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/probe"
)

// cmdProbe mints the handle the whole loop turns on.
//
// The intended shape is that you finish a feature, ask an agent to write a test
// for it, and the last thing it does is run this. What it prints is a handle,
// and the handle is the entire interface between "the test exists" and "the
// window is watching it run" — no paths to remember, no command to retype.
func cmdProbe(ctx context.Context, env *Env, args []string) error {
	if len(args) > 0 && args[0] == "list" {
		return probeList(env)
	}
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	test := fs.String("test", "", "the test to watch, by name")
	cmdOverride := fs.String("cmd", "", "how to run just that test (default: the configured test command, narrowed)")
	fixture := fs.String("fixture", "", "repo-relative sample data the test reads")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	name := *test
	if name == "" {
		name = first(fs.Args())
	}
	if name == "" {
		return fmt.Errorf("usage: plum probe -test <name> [-fixture <path>] [-cmd <command>]")
	}

	command := *cmdOverride
	if command == "" {
		scoped, ok := probe.ScopeCommand(env.Cfg.Trace.TestCommand, name, packageOf(env, name))
		if !ok {
			return fmt.Errorf("cannot narrow %q to a single test — pass -cmd with a command that runs only %s,\n"+
				"because a run that quietly executed the whole suite would still draw a picture,\n"+
				"and the picture would not be of what the window says it is showing",
				env.Cfg.Trace.TestCommand, name)
		}
		command = scoped
	}

	if *fixture != "" {
		if _, err := os.Stat(filepath.Join(env.Cfg.Root, *fixture)); err != nil {
			// Not fatal: the agent may be about to write it. Said plainly, so a
			// window showing an empty fixture pane is not a mystery.
			fmt.Printf("note: %s does not exist yet\n", *fixture)
		}
	}

	p, err := probe.Mint(env.Cfg.Root, name, command, *fixture)
	if err != nil {
		return err
	}
	fmt.Println(p.Handle())
	fmt.Println()
	fmt.Println("  test    ", p.Test)
	fmt.Println("  runs    ", p.Command)
	if p.Fixture != "" {
		fmt.Println("  fixture ", p.Fixture)
	}
	fmt.Println()
	fmt.Printf("watch it: plum watch %s\n", p.Handle())
	return nil
}

func probeList(env *Env) error {
	ps, err := probe.List(env.Cfg.Root)
	if err != nil {
		return err
	}
	if len(ps) == 0 {
		return fmt.Errorf("no probes yet — `plum probe -test <name>` mints one")
	}
	for _, p := range ps {
		fixture := ""
		if p.Fixture != "" {
			fixture = "  ← " + p.Fixture
		}
		fmt.Printf("%-12s %-44s %s%s\n", p.Handle(), p.Test, p.Command, fixture)
	}
	return nil
}

// packageOf finds the directory holding the test, so the run can be narrowed to
// it.
//
// It searches the working tree rather than the session bundle. A probe is minted
// the moment a test is written, which is before any capture has seen it — asking
// the bundle would fail in exactly the case this is for, and the first version
// of this did.
//
// Best effort throughout: without a directory the probe still works, it is just
// slower, and slower is something you notice rather than something that misleads
// you.
func packageOf(env *Env, test string) string {
	var found string
	needle := []byte(test)
	_ = filepath.Walk(env.Cfg.Root, func(path string, info os.FileInfo, err error) error {
		if err != nil || found != "" {
			return nil
		}
		rel, rerr := filepath.Rel(env.Cfg.Root, path)
		if rerr != nil {
			return nil
		}
		if info.IsDir() {
			if strings.HasPrefix(info.Name(), ".") && info.Name() != "." || env.Cfg.Excluded(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if !bundle.IsTestPath(rel) || info.Size() > 4<<20 {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if bytes.Contains(data, needle) {
			found = filepath.Dir(rel)
		}
		return nil
	})
	return found
}
