// Package report renders a bundle as markdown in read-first order (spec §6.8):
// what could break other people first, source order never.
package report

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kelalaike/plum/internal/bundle"
)

// maxPerSection bounds how much of any one list the report prints. A report
// nobody finishes reading is a report that found nothing (spec §7): the fix is
// to cut sections, not detail, so the remainder is counted and named rather than
// silently dropped. -v prints everything.
const maxPerSection = 12

type Options struct {
	// Stale lists claims whose subject's fingerprint moved since they were
	// written (P5). Empty when no synthesis has run.
	Stale []StaleClaim
	// UnannotatedBarriers are expensive calls whose call site carries no comment —
	// the costly decision on this path was never explained (spec §9.4).
	UnannotatedBarriers []string
	// Landscape notes from a trace run, e.g. an unclosed path.
	LandscapeNotes []string
	// Reached maps each changed symbol to the tests whose execution entered it.
	// When traces exist this replaces the name-matching heuristic: "untested"
	// stops meaning "no test file mentions this" and starts meaning "no test's
	// execution ever entered it", which is the thing you actually wanted to know.
	Reached map[bundle.SymbolID][]string
	Traced  bool
	Verbose bool
}

type StaleClaim struct {
	ID     string
	Claim  string
	Symbol bundle.SymbolID
	Reason string
}

func Render(b *bundle.Bundle, opt Options) string {
	var w strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&w, f+"\n", a...) }

	if b.Gate.Fired {
		p("GATE FIRED — %s", strings.Join(b.Gate.Reasons, " · "))
	} else {
		p("gate clear — %s, nothing above threshold", plural(len(b.Symbols), "symbol"))
	}
	p("")
	p("# session %s", b.Session.ID)
	p("")
	p("`%s..%s` · %s · %s",
		short(b.Session.StartSHA), short(b.Session.EndSHA),
		b.Session.Agent, b.Session.EndedAt.Sub(b.Session.StartedAt).Round(1e9))
	if b.Session.Command != "" {
		p("")
		p("    %s", b.Session.Command)
	}
	p("")

	// Signature changes on existing exports go first: they are the highest-signal
	// event this tool produces, because they break callers nobody looked at.
	if n := len(b.Surface.Added) + len(b.Surface.Removed) + len(b.Surface.Modified); n > 0 {
		p("## ⚠ Public surface changed")
		p("")
		// Signature changes on existing exports are never truncated: they are the
		// highest-signal event this tool produces.
		for _, m := range b.Surface.Modified {
			if m.Kind == "config_key" {
				p("- **value changed** `%s`", m.Name)
				p("    - before: `%s`", m.Before)
				p("    - after:  `%s`", m.After)
				p("    - nothing fails to build when a setting changes; behaviour just differs")
				continue
			}
			p("- **signature changed** `%s`", m.Name)
			p("    - before: `%s`", m.Before)
			p("    - after:  `%s`", m.After)
			p("    - every existing caller was written against the old shape")
		}
		for _, it := range b.Surface.Removed {
			p("- **removed** %s `%s` (`%s`)", it.Kind, it.Name, it.File)
		}
		added := b.Surface.Added
		shown, hidden := limit(len(added), opt.Verbose)
		for _, it := range added[:shown] {
			sig := ""
			if it.Signature != "" {
				sig = " — `" + clip(oneline(it.Signature), 110) + "`"
			}
			p("- new %s `%s` (`%s`)%s", it.Kind, it.Name, it.File, sig)
		}
		if hidden > 0 {
			p("- … and %d more new public items — `plum report -v` for the full list", hidden)
			p("")
			p("  by kind: %s", byKind(added))
		}
		p("")
	}

	if len(b.Divergence.Findings) > 0 {
		p("## ⚠ Divergence from repo conventions — score %.2f", b.Divergence.Score)
		p("")
		findings := bySeverity(b.Divergence.Findings)
		shown, hidden := limit(len(findings), opt.Verbose)
		for _, f := range findings[:shown] {
			p("- `%s` **%s** [%s/%s]", f.Symbol, f.Convention, f.Severity, f.Source)
			p("    - expected: %s", f.Expected)
			p("    - observed: %s", f.Observed)
		}
		if hidden > 0 {
			p("- … and %d more findings — `plum report -v`", hidden)
		}
		p("")
	}

	if len(opt.Stale) > 0 {
		p("## ⚠ Stale claims")
		p("")
		p("These claims were written against a version of the code that no longer exists.")
		p("")
		for _, s := range opt.Stale {
			p("- `%s` %s", s.ID, s.Claim)
			p("    - subject `%s` — %s", s.Symbol, s.Reason)
		}
		p("")
	}

	if len(b.RiskMarkers) > 0 {
		p("## Risk markers")
		p("")
		byKind := map[string][]bundle.RiskMarker{}
		var kinds []string
		for _, r := range b.RiskMarkers {
			if _, ok := byKind[r.Kind]; !ok {
				kinds = append(kinds, r.Kind)
			}
			byKind[r.Kind] = append(byKind[r.Kind], r)
		}
		sort.Strings(kinds)
		for _, k := range kinds {
			marks := byKind[k]
			p("- **%s** (%d)", k, len(marks))
			shown, hidden := limit(len(marks), opt.Verbose)
			for _, r := range marks[:shown] {
				p("    - `%s:%d` %s", r.File, r.Line, r.Note)
			}
			if hidden > 0 {
				p("    - … and %d more — `plum report -v`", hidden)
			}
		}
		p("")
	}

	if codeSymbols(b) > 0 {
		p("## Symbols changed")
		p("")
		byFile := map[string][]bundle.Symbol{}
		var files []string
		for _, s := range b.Symbols {
			if s.Kind == "config_key" {
				continue // settings have their own section
			}
			if _, ok := byFile[s.File]; !ok {
				files = append(files, s.File)
			}
			byFile[s.File] = append(byFile[s.File], s)
		}
		sort.Strings(files)
		shownFiles, hiddenFiles := limit(len(files), opt.Verbose)
		for _, f := range files[:shownFiles] {
			p("`%s`", f)
			p("")
			for _, s := range byFile[f] {
				flags := []string{}
				if s.Doc == "" && s.Kind != "var" && s.Kind != "const" {
					flags = append(flags, "undocumented")
				}
				// Prefer recorded execution over the name match wherever it exists.
				switch {
				case opt.Traced && len(opt.Reached[s.ID]) > 0:
				case opt.Traced && s.Change != "deleted" && (s.Kind == "func" || s.Kind == "method"):
					flags = append(flags, "no test reaches it")
				case !opt.Traced && !s.Tested:
					flags = append(flags, "untested")
				}
				if isTest(s.File) {
					flags = nil // a test file is evidence, not debt
				}
				suffix := ""
				if len(flags) > 0 {
					suffix = "  _(" + strings.Join(flags, ", ") + ")_"
				}
				p("- %s `%s` %s L%d–%d%s", pad(s.Change), s.Name, s.Kind, s.LineStart, s.LineEnd, suffix)
				if opt.Verbose && s.Signature != "" {
					p("    - `%s`", oneline(s.Signature))
				}
			}
			p("")
		}
		if hiddenFiles > 0 {
			p("… and %d more files — `plum report -v`", hiddenFiles)
			p("")
		}
	}

	if edges := crossModule(b.Edges); len(edges) > 0 {
		p("## New cross-module edges")
		p("")
		p("Coupling this session introduced between packages.")
		p("")
		shown, hidden := limit(len(edges), opt.Verbose)
		for _, e := range edges[:shown] {
			p("- `%s` → `%s`", e.From, e.To)
		}
		if hidden > 0 {
			p("- … and %d more — `plum report -v`", hidden)
		}
		p("")
	}

	if keys, edges := configChanges(b); len(keys) > 0 || len(edges) > 0 {
		p("## Configuration")
		p("")
		p("A changed setting is a behaviour change that no compiler and no test")
		p("signature announces.")
		p("")
		shown, hidden := limit(len(keys), opt.Verbose)
		for _, k := range keys[:shown] {
			p("- %s `%s` — `%s`", pad(k.Change), k.ID, oneline(k.Signature))
			if k.Doc != "" {
				p("    - comment: %s", oneline(k.Doc))
			}
			for _, e := range edges {
				if e.To == k.ID {
					p("    - read by `%s` (matched by %s)", e.From, strings.TrimPrefix(e.Kind, "config:"))
				}
			}
		}
		if hidden > 0 {
			p("- … and %d more changed settings — `plum report -v`", hidden)
		}
		orphans := 0
		for _, k := range keys {
			if !readBy(edges, k.ID) {
				orphans++
			}
		}
		if orphans > 0 {
			p("")
			p("%d changed settings have no reader anywhere in the tree at this revision.", orphans)
			p("Either they are read by a name this pass cannot see — built at runtime,")
			p("or spelled differently — or nothing reads them at all.")
		}
		p("")
	}

	if n := len(b.Deps.Added) + len(b.Deps.Removed) + len(b.Deps.Upgraded); n > 0 {
		p("## Dependencies")
		p("")
		for _, d := range b.Deps.Added {
			p("- **added** %s `%s@%s`", d.Ecosystem, d.Name, d.Version)
		}
		for _, d := range b.Deps.Upgraded {
			p("- upgraded %s `%s` %s → %s", d.Ecosystem, d.Name, d.Before, d.After)
		}
		for _, d := range b.Deps.Removed {
			p("- removed %s `%s`", d.Ecosystem, d.Name)
		}
		p("")
	}

	if len(opt.UnannotatedBarriers) > 0 {
		p("## Expensive calls with no rationale")
		p("")
		p("A costly decision on this path was never explained at the call site.")
		p("")
		for _, s := range opt.UnannotatedBarriers {
			p("- %s", s)
		}
		p("")
	}

	if len(opt.LandscapeNotes) > 0 {
		p("## Landscape")
		p("")
		for _, n := range opt.LandscapeNotes {
			p("- %s", n)
		}
		p("")
	}

	untested, tested := coverage(b, opt)
	if len(untested) > 0 {
		p("## Untested new symbols")
		p("")
		if opt.Traced {
			p("%d of %d changed code symbols were never entered by any test's execution.", len(untested), len(untested)+len(tested))
		} else {
			p("%d of %d changed symbols are not named by any changed test.", len(untested), b.Coverage.SymbolCount)
			p("")
			p("_This is a name match. Run `plum trace` and it becomes exact._")
		}
		p("")
		shown, hidden := limit(len(untested), opt.Verbose)
		for _, id := range untested[:shown] {
			p("- `%s`", id)
		}
		if hidden > 0 {
			p("- … and %d more — `plum report -v`", hidden)
		}
		p("")
	}

	// Which test reaches which change is the map that grows as the suite does.
	if opt.Traced && len(tested) > 0 {
		p("## Which tests reach this change")
		p("")
		byTest := map[string][]bundle.SymbolID{}
		for _, id := range tested {
			for _, name := range opt.Reached[id] {
				byTest[name] = append(byTest[name], id)
			}
		}
		var names []string
		for name := range byTest {
			names = append(names, name)
		}
		sort.Strings(names)
		shown, hidden := limit(len(names), opt.Verbose)
		for _, name := range names[:shown] {
			p("- **%s** reaches %s", name, plural(len(byTest[name]), "changed symbol"))
			for _, id := range byTest[name] {
				p("    - `%s`", id)
			}
			p("    - `plum explore -test %q`", name)
		}
		if hidden > 0 {
			p("- … and %d more tests — `plum report -v`", hidden)
		}
		p("")
	}

	p("## Rationale (from journal)")
	p("")
	if len(b.Journal) == 0 {
		p("_No journal entries for this session._ What was considered and rejected is")
		p("unrecoverable from the diff — this is the loss P3 warns about. Record it")
		p("next time with `plum note`.")
	} else {
		for _, j := range b.Journal {
			p("- `%s` %s — %s", j.TS.Format("15:04:05"), j.File, j.Rationale)
			for _, alt := range j.Alternatives {
				p("    - rejected: %s", alt)
			}
		}
	}
	p("")
	return w.String()
}

// limit returns how many entries to print and how many are held back.
func limit(n int, verbose bool) (int, int) {
	if verbose || n <= maxPerSection {
		return n, 0
	}
	return maxPerSection, n - maxPerSection
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// byKind summarises a truncated surface list so the shape is still legible.
func byKind(items []bundle.SurfaceItem) string {
	counts := map[string]int{}
	for _, i := range items {
		counts[i.Kind]++
	}
	var kinds []string
	for k := range counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	var parts []string
	for _, k := range kinds {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
	}
	return strings.Join(parts, ", ")
}

// configChanges separates settings from code so the report can say what a
// changed default now does, and which function will notice.
func codeSymbols(b *bundle.Bundle) int {
	n := 0
	for _, s := range b.Symbols {
		if s.Kind != "config_key" {
			n++
		}
	}
	return n
}

func configChanges(b *bundle.Bundle) ([]bundle.Symbol, []bundle.Edge) {
	var keys []bundle.Symbol
	for _, s := range b.Symbols {
		if s.Kind == "config_key" {
			keys = append(keys, s)
		}
	}
	var edges []bundle.Edge
	for _, e := range b.Edges {
		if strings.HasPrefix(e.Kind, "config:") {
			edges = append(edges, e)
		}
	}
	return keys, edges
}

func readBy(edges []bundle.Edge, id bundle.SymbolID) bool {
	for _, e := range edges {
		if e.To == id {
			return true
		}
	}
	return false
}

func bySeverity(fs []bundle.DivergenceFinding) []bundle.DivergenceFinding {
	out := make([]bundle.DivergenceFinding, len(fs))
	copy(out, fs)
	rank := map[string]int{"high": 0, "warn": 1, "info": 2}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Severity] < rank[out[j].Severity] })
	return out
}

func crossModule(edges []bundle.Edge) []bundle.Edge {
	var out []bundle.Edge
	for _, e := range edges {
		if e.CrossesModule && e.New {
			out = append(out, e)
		}
	}
	return out
}

// coverage splits the changed code symbols into those some test's execution
// entered and those none did, preferring recorded execution over the name match.
func coverage(b *bundle.Bundle, opt Options) (untested, tested []bundle.SymbolID) {
	if !opt.Traced {
		return b.Coverage.Untested, nil
	}
	for _, s := range b.Symbols {
		if s.Change == "deleted" || isTest(s.File) || s.Kind == "config_key" {
			continue
		}
		if s.Kind != "func" && s.Kind != "method" {
			continue // a type or a constant is not something a test enters
		}
		if len(opt.Reached[s.ID]) > 0 {
			tested = append(tested, s.ID)
		} else {
			untested = append(untested, s.ID)
		}
	}
	sort.Slice(untested, func(i, j int) bool { return untested[i] < untested[j] })
	sort.Slice(tested, func(i, j int) bool { return tested[i] < tested[j] })
	return untested, tested
}

func isTest(path string) bool {
	base := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		base = path[i+1:]
	}
	return strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, "test_") ||
		strings.HasSuffix(base, ".test.ts") || strings.HasSuffix(base, ".spec.ts") ||
		strings.HasSuffix(base, "_test.py")
}

func pad(change string) string {
	switch change {
	case "added":
		return "**added**   "
	case "deleted":
		return "**deleted** "
	default:
		return "modified"
	}
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func oneline(s string) string { return strings.Join(strings.Fields(s), " ") }

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
