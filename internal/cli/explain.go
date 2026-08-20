package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/kelalaike/plum/internal/trace"
)

// cmdExplain says what a recording actually did, in plain language.
//
// The landscape names symbols. That answers "which functions" and nothing else:
// a reader still has to hold the arguments, the returned values, the call-site
// comments and the costs in their head to know what happened. This says it.
//
// Every sentence is composed from recorded evidence, so it is available with no
// model and no network, and where the evidence is missing it says that instead
// of inventing something plausible.
func cmdExplain(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	testName := fs.String("test", "", "explain the path of one named test")
	brief := fs.Bool("brief", false, "the one-paragraph summary only")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := env.Store.ResolveRef(ctx, env.Repo, first(fs.Args()))
	if err != nil {
		return err
	}
	b, err := env.Store.Load(id)
	if err != nil {
		return err
	}

	l, err := loadLandscape(env, id)
	if err != nil {
		return fmt.Errorf("no landscape for %s — run `plum trace` first, since this describes a recording", id)
	}
	if *testName != "" {
		events, err := trace.ReadFile(env.Store.TracePath(id))
		if err != nil {
			return fmt.Errorf("no traces for %s — run `plum trace` first", id)
		}
		scoped, err := scopeToTest(events, *testName)
		if err != nil {
			return err
		}
		derived := trace.DeriveChainN(scoped, b, trace.ChainHottest, trace.DefaultMaxFrames)
		derived.TestID = *testName
		l = &derived
	}

	fmt.Println(trace.Summary(*l, b))
	if *brief {
		return nil
	}
	fmt.Println()
	for _, step := range trace.Narrate(*l, b) {
		indent := ""
		if step.Kind == "transition" {
			indent = "    "
		}
		fmt.Printf("%s%s\n", indent, step.Text)
		if step.Note != "" {
			fmt.Printf("%s    ⚠ %s\n", indent, wrapNote(step.Note, len(indent)+6))
		}
	}
	return nil
}

// wrapNote keeps a long caveat readable in a terminal rather than running it
// off the edge.
func wrapNote(note string, indent int) string {
	const width = 76
	words := strings.Fields(note)
	if len(words) == 0 {
		return ""
	}
	var lines []string
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width-indent {
			lines = append(lines, line)
			line = w
			continue
		}
		line += " " + w
	}
	lines = append(lines, line)
	return strings.Join(lines, "\n"+strings.Repeat(" ", indent))
}
