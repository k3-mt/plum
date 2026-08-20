package trace

import (
	"sort"

	"github.com/kelalaike/plum/internal/bundle"
)

// TestRun is one test's recorded execution: what it entered, how deep it went,
// and whether anything escaped.
//
// A test is the only artifact that is named, executable, committed and about a
// single intention. That makes it the natural unit for a landscape — "show me
// what `verify throws through frames` does" is a question a developer already
// has, where "show me chain 2 of 7" is not.
type TestRun struct {
	Name     string            `json:"name"`
	Symbols  []bundle.SymbolID `json:"symbols"`
	Frames   int               `json:"frames"`
	MaxDepth int               `json:"max_depth"`
	Raised   bool              `json:"raised"`
	Events   int               `json:"events"`
}

// Tests summarises the recording, one entry per test, ordered by name so a
// re-run lists them the same way.
func Tests(events []Event) []TestRun {
	byName := map[string]*TestRun{}
	seen := map[string]map[bundle.SymbolID]bool{}

	for _, e := range events {
		name := e.TestID
		if name == "" {
			name = "(no test)"
		}
		run, ok := byName[name]
		if !ok {
			run = &TestRun{Name: name}
			byName[name] = run
			seen[name] = map[bundle.SymbolID]bool{}
		}
		run.Events++
		if e.Depth+1 > run.MaxDepth {
			run.MaxDepth = e.Depth + 1
		}
		switch e.Kind {
		case "call":
			run.Frames++
			if !seen[name][e.Symbol] {
				seen[name][e.Symbol] = true
				run.Symbols = append(run.Symbols, e.Symbol)
			}
		case "raise":
			run.Raised = true
		}
	}

	out := make([]TestRun, 0, len(byName))
	for _, run := range byName {
		sort.Slice(run.Symbols, func(i, j int) bool { return run.Symbols[i] < run.Symbols[j] })
		out = append(out, *run)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ForTest narrows a recording to one test, so the landscape drawn from it is
// that test's path and nothing else.
func ForTest(events []Event, name string) []Event {
	var out []Event
	for _, e := range events {
		if e.TestID == name {
			out = append(out, e)
		}
	}
	return out
}

// Reached maps each executed symbol to the tests that entered it. This is what
// makes "untested" exact: not "a test file mentions this name" but "no test's
// execution ever entered it".
func Reached(events []Event) map[bundle.SymbolID][]string {
	out := map[bundle.SymbolID][]string{}
	seen := map[string]bool{}
	for _, e := range events {
		if e.Kind != "call" || e.TestID == "" {
			continue
		}
		key := string(e.Symbol) + "\x00" + e.TestID
		if seen[key] {
			continue
		}
		seen[key] = true
		out[e.Symbol] = append(out[e.Symbol], e.TestID)
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}
