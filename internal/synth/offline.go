package synth

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/k3-mt/plum/internal/bundle"
)

// Offline composes the seams doc mechanically from the bundle. It writes no
// prose it cannot support with a fact, so it is duller than a model — and it
// never hallucinates a rationale that was never recorded.
//
// It exists so the whole pipeline is exercisable with no API key and no network,
// which matters for tests, for CI, and for the first ten minutes with the tool.
type Offline struct {
	Bundle *bundle.Bundle
}

func (o *Offline) Name() string { return "offline/mechanical" }

func (o *Offline) Complete(ctx context.Context, system, user string) (string, error) {
	b := o.Bundle
	var w strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&w, f+"\n", a...) }

	p("# Seams — session %s", b.Session.ID)
	p("")
	p("_Composed mechanically from bundle.json with no model in the loop. Every")
	p("line below is derived from an AST fact; nothing here is inferred intent._")
	p("")

	p("## What changed, in one paragraph")
	p("")
	kinds := map[string]int{}
	files := map[string]bool{}
	for _, s := range b.Symbols {
		kinds[s.Change]++
		files[s.File] = true
	}
	p("%d symbols across %d files: %d added, %d modified, %d deleted. %s",
		len(b.Symbols), len(files), kinds["added"], kinds["modified"], kinds["deleted"],
		surfaceSentence(b))
	p("")

	p("## Assumptions this code makes")
	p("")
	any := false
	for _, r := range b.RiskMarkers {
		any = true
		p("- `%s` assumes %s", r.Symbol, assumptionFor(r))
		p("    - first thing that breaks: %s", breakageFor(r))
	}
	for _, s := range b.Symbols {
		if s.Change == "deleted" || s.Doc != "" || isTest(s.File) {
			continue
		}
		if len(s.CallSites) > 4 {
			any = true
			p("- `%s` has %d outbound calls and no declaration doc — every assumption it makes is unstated", s.ID, len(s.CallSites))
			p("    - first thing that breaks: the next reader reconstructs them from the body, and gets one wrong")
		}
	}
	if !any {
		p("- No mechanical predicate fired. That is not the same as \"no assumptions\";")
		p("  it means none of the ones this tool can see are present.")
	}
	p("")

	p("## Invariants")
	p("")
	inv := false
	for _, e := range b.Edges {
		if e.CrossesModule && e.New {
			inv = true
			p("- `%s` now depends on `%s` across a module boundary — that dependency direction must stay acyclic", e.From, e.To)
		}
	}
	for _, m := range b.Surface.Modified {
		inv = true
		if m.Kind == "config_key" {
			p("- every reader of `%s` now sees `%s` where it saw `%s`", m.Name, oneline(m.After), oneline(m.Before))
			continue
		}
		p("- callers of `%s` must be updated together: the signature moved from `%s` to `%s`", m.Name, oneline(m.Before), oneline(m.After))
	}
	if !inv {
		p("- No cross-module coupling and no signature changes in this session.")
	}
	p("")

	p("## Failure modes")
	p("")
	fm := failureModes(b)
	if len(fm) == 0 {
		p("- Nothing in the changed set touches error handling, concurrency, network or file IO in a way this pass can see.")
	}
	for _, f := range fm {
		p("- %s", f)
	}
	p("")

	p("## Where to look first if this breaks")
	p("")
	for i, s := range lookFirst(b) {
		p("%d. `%s` — %s", i+1, s.id, s.why)
	}
	p("")

	p("## Rationale")
	p("")
	if len(b.Journal) == 0 {
		p("Not recorded. What was considered and rejected during this session is")
		p("unrecoverable from the diff — the loss P3 warns about.")
	} else {
		for _, j := range b.Journal {
			p("- `%s`: %s", j.File, j.Rationale)
			for _, a := range j.Alternatives {
				p("    - rejected: %s", a)
			}
		}
	}
	p("")

	p("```plum-claims")
	for _, c := range offlineClaims(b) {
		p("%s", c)
	}
	p("```")
	return w.String(), nil
}

// isTest keeps test files out of the seams doc: a test is evidence about the
// code, not a thing the code assumes.
func isTest(path string) bool {
	base := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		base = path[i+1:]
	}
	return strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, "test_") ||
		strings.HasSuffix(base, ".test.ts") || strings.HasSuffix(base, ".spec.ts") ||
		strings.HasSuffix(base, "_test.py")
}

func surfaceSentence(b *bundle.Bundle) string {
	switch {
	case len(b.Surface.Modified) > 0:
		return fmt.Sprintf("%d existing exports changed signature, which is the part that breaks callers silently.", len(b.Surface.Modified))
	case len(b.Surface.Added) > 0:
		return fmt.Sprintf("%d new items entered the public surface.", len(b.Surface.Added))
	default:
		return "The public surface is unchanged."
	}
}

func assumptionFor(r bundle.RiskMarker) string {
	switch r.Kind {
	case "package_level_state":
		return "no two callers (or two tests) mutate this package-level state at once"
	case "swallowed_error":
		return "the discarded error can never be the one that mattered"
	case "swallowed_panic":
		return "a panic here is always recoverable and never worth reporting"
	case "unsynchronised_goroutine":
		return "the goroutine finishes before anything observes its effect"
	case "retry_without_backoff":
		return "the failure it retries against clears immediately"
	case "network_without_timeout", "db_without_context", "subprocess_without_context":
		return "the remote side always answers, and answers quickly"
	case "unbounded_read":
		return "the input is small enough to hold in memory"
	case "widened_type":
		return "callers pass the right dynamic type, because the compiler no longer checks"
	case "init_side_effects":
		return "importing this package is always safe and always wanted"
	}
	return r.Note
}

func breakageFor(r bundle.RiskMarker) string {
	switch r.Kind {
	case "package_level_state":
		return "a parallel test run, which is where this surfaces first"
	case "swallowed_error", "swallowed_panic":
		return "a silent wrong answer instead of a loud failure"
	case "unsynchronised_goroutine":
		return "a flaky test, then a race in production"
	case "retry_without_backoff":
		return "a hot loop that turns a slow dependency into an outage"
	case "network_without_timeout", "db_without_context", "subprocess_without_context":
		return "a hung request holding its caller open indefinitely"
	case "unbounded_read":
		return "memory growth proportional to whatever the other side sends"
	}
	return "unclear from the AST alone — this is where a trace would answer it"
}

func failureModes(b *bundle.Bundle) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, r := range b.RiskMarkers {
		switch r.Kind {
		case "swallowed_error", "swallowed_panic":
			add(fmt.Sprintf("**Dependency returns an error**: `%s` discards it at %s:%d, so the caller sees success.", r.Symbol, r.File, r.Line))
		case "unsynchronised_goroutine":
			add(fmt.Sprintf("**Concurrent caller**: `%s` starts a goroutine nothing waits on — ordering is unobservable.", r.Symbol))
		case "package_level_state":
			add(fmt.Sprintf("**Concurrent caller**: `%s` is package-level state shared by every caller in the process.", r.Symbol))
		case "network_without_timeout", "db_without_context", "subprocess_without_context":
			add(fmt.Sprintf("**Slow or unavailable network**: `%s` has no deadline at %s:%d.", r.Symbol, r.File, r.Line))
		case "unbounded_read":
			add(fmt.Sprintf("**Malformed or oversized input**: `%s` reads without a bound.", r.Symbol))
		}
	}
	for _, f := range b.Divergence.Findings {
		if f.Convention == "error_handling" && strings.Contains(f.Observed, "panic") {
			add(fmt.Sprintf("**Error from a dependency**: `%s` panics where the rest of the repo returns a wrapped error — the failure crosses a boundary it was not designed to cross.", f.Symbol))
		}
	}
	sort.Strings(out)
	return out
}

type ranked struct {
	id  bundle.SymbolID
	why string
}

func lookFirst(b *bundle.Bundle) []ranked {
	score := map[bundle.SymbolID]float64{}
	why := map[bundle.SymbolID]string{}
	for _, m := range b.Surface.Modified {
		if m.Symbol != "" {
			score[m.Symbol] += 3
			why[m.Symbol] = "its signature changed, so callers may be compiling against a shape that no longer exists"
		}
	}
	for _, r := range b.RiskMarkers {
		score[r.Symbol] += 2
		if why[r.Symbol] == "" {
			why[r.Symbol] = r.Kind + " — " + r.Note
		}
	}
	for _, f := range b.Divergence.Findings {
		score[f.Symbol] += 1.5
		if why[f.Symbol] == "" {
			why[f.Symbol] = "diverges from the repo's " + f.Convention + " convention"
		}
	}
	for _, id := range b.Coverage.Untested {
		score[id] += 1
		if why[id] == "" {
			why[id] = "changed and not named by any changed test"
		}
	}
	var out []ranked
	for id, s := range score {
		_ = s
		out = append(out, ranked{id, why[id]})
	}
	sort.Slice(out, func(i, j int) bool {
		if score[out[i].id] != score[out[j].id] {
			return score[out[i].id] > score[out[j].id]
		}
		return out[i].id < out[j].id
	})
	if len(out) > 3 {
		out = out[:3]
	}
	if len(out) == 0 && len(b.Symbols) > 0 {
		out = append(out, ranked{b.Symbols[0].ID, "first changed symbol in the session; nothing scored higher"})
	}
	return out
}

func offlineClaims(b *bundle.Bundle) []string {
	var out []string
	for _, m := range b.Surface.Modified {
		if m.Symbol == "" {
			continue
		}
		if m.Kind == "config_key" {
			// A setting has no callers to update; what is being asserted is that
			// the new value is right everywhere the old one was read.
			out = append(out, fmt.Sprintf("assertion :: %s :: %s is correct for every environment that reads it, not just this one",
				m.Symbol, oneline(m.After)))
			continue
		}
		out = append(out, fmt.Sprintf("assertion :: %s :: every existing caller has been updated for the new signature", m.Symbol))
	}
	for _, r := range b.RiskMarkers {
		switch r.Kind {
		case "swallowed_error":
			out = append(out, fmt.Sprintf("assertion :: %s :: the error discarded at line %d can never be actionable", r.Symbol, r.Line))
		case "package_level_state":
			out = append(out, fmt.Sprintf("executable :: %s :: calling this twice in one process gives the same result as calling it once", r.Symbol))
		case "unsynchronised_goroutine":
			out = append(out, fmt.Sprintf("assertion :: %s :: the goroutine's effect is observed before anything depends on it", r.Symbol))
		}
	}
	for _, s := range b.Symbols {
		if s.Change == "added" && s.Exported && s.Kind != "var" && s.Kind != "const" && !isTest(s.File) {
			out = append(out, fmt.Sprintf("executable :: %s :: %s behaves as its signature promises for a representative input", s.ID, s.Name))
		}
	}
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}
