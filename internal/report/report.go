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
	Verbose        bool
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
			p("- **signature changed** `%s`", m.Name)
			p("    - before: `%s`", m.Before)
			p("    - after:  `%s`", m.After)
			p("    - every existing caller compiles against the old shape")
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

	if len(b.Symbols) > 0 {
		p("## Symbols changed")
		p("")
		byFile := map[string][]bundle.Symbol{}
		var files []string
		for _, s := range b.Symbols {
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
				if !s.Tested {
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

	if len(b.Coverage.Untested) > 0 {
		p("## Untested new symbols")
		p("")
		p("%d of %d changed symbols are not named by any changed test.", len(b.Coverage.Untested), b.Coverage.SymbolCount)
		p("")
		shown, hidden := limit(len(b.Coverage.Untested), opt.Verbose)
		for _, id := range b.Coverage.Untested[:shown] {
			p("- `%s`", id)
		}
		if hidden > 0 {
			p("- … and %d more — `plum report -v`", hidden)
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
