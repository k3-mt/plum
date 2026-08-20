package cli

import (
	"flag"
	"reflect"
	"testing"
)

func newFS() (*flag.FlagSet, *string, *bool, *multiFlag) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	file := fs.String("file", "", "")
	quiet := fs.Bool("quiet", false, "")
	var alts multiFlag
	fs.Var(&alts, "rejected", "")
	return fs, file, quiet, &alts
}

// The documented form is `plum note "why" -rejected "what I didn't"`. Go's flag
// package stops at the first positional, so before this the flag was recorded as
// part of the prose: nothing errored, nothing looked wrong, and the note was
// missing the half that mattered.
func TestFlagsAfterPositionalsAreStillFlags(t *testing.T) {
	fs, _, _, alts := newFS()
	if err := parseArgs(fs, []string{"why I did it", "-rejected", "the thing I didn't"}); err != nil {
		t.Fatal(err)
	}
	if got := []string(*alts); !reflect.DeepEqual(got, []string{"the thing I didn't"}) {
		t.Errorf("rejected = %v", got)
	}
	if got := fs.Args(); !reflect.DeepEqual(got, []string{"why I did it"}) {
		t.Errorf("positional = %v", got)
	}
}

func TestBooleanFlagsDoNotEatTheNextArgument(t *testing.T) {
	fs, _, quiet, _ := newFS()
	if err := parseArgs(fs, []string{"session-id", "-quiet"}); err != nil {
		t.Fatal(err)
	}
	if !*quiet {
		t.Error("-quiet not set")
	}
	if got := fs.Args(); !reflect.DeepEqual(got, []string{"session-id"}) {
		t.Errorf("a bool flag swallowed a positional: %v", got)
	}
}

// `plum run -- claude -p "..."` hands everything after -- to the agent. Hoisting
// a flag out of that would rewrite somebody else's command line.
//
// flag itself consumes the leading "--" as the end-of-flags marker, which it
// always did and which cmdRun already allows for. What matters here is that the
// agent's own flags survive as positionals rather than being parsed as ours.
func TestNothingAfterDoubleDashIsHoisted(t *testing.T) {
	fs, file, quiet, _ := newFS()
	if err := parseArgs(fs, []string{"--", "claude", "-quiet", "-file", "x"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"claude", "-quiet", "-file", "x"}
	if got := fs.Args(); !reflect.DeepEqual(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
	if *quiet || *file != "" {
		t.Error("a flag meant for the wrapped command was parsed as ours")
	}
}

// The marker can also arrive after a positional, which is how `plum run` is
// actually typed when a session ref comes first.
func TestDoubleDashMidwayStillProtectsTheRest(t *testing.T) {
	fs, file, _, _ := newFS()
	if err := parseArgs(fs, []string{"-file", "a.go", "sess", "--", "claude", "-file", "theirs"}); err != nil {
		t.Fatal(err)
	}
	if *file != "a.go" {
		t.Errorf("file = %q — ours should win, theirs should not be hoisted", *file)
	}
	want := []string{"sess", "--", "claude", "-file", "theirs"}
	if got := fs.Args(); !reflect.DeepEqual(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
}

// A rationale is prose, and prose can start with a dash. Only flags this command
// actually defines are moved.
func TestUnknownDashArgumentsStayWhereTheyAre(t *testing.T) {
	fs, _, _, _ := newFS()
	err := parseArgs(fs, []string{"-not-a-flag-here", "-file", "x.go"})
	if err != nil {
		// flag reports the unknown flag, which is the same as before — the
		// point is that it is not silently reordered into a value.
		return
	}
	if got := fs.Args(); len(got) == 0 || got[0] != "-not-a-flag-here" {
		t.Errorf("args = %v", got)
	}
}

func TestEqualsFormDoesNotTakeAnExtraArgument(t *testing.T) {
	fs, file, _, _ := newFS()
	if err := parseArgs(fs, []string{"why", "-file=rates.go", "more prose"}); err != nil {
		t.Fatal(err)
	}
	if *file != "rates.go" {
		t.Errorf("file = %q", *file)
	}
	if got := fs.Args(); !reflect.DeepEqual(got, []string{"why", "more prose"}) {
		t.Errorf("positional = %v", got)
	}
}
