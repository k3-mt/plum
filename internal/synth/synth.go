// Package synth runs synthesis in a fresh context (P2). The builder does not
// narrate: a self-summary reproduces its own blind spots, so the input here is
// bundle.json plus the diff — never the building agent's transcript.
package synth

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/claims"
	"github.com/kelalaike/plum/internal/config"
)

type Provider interface {
	Name() string
	Complete(ctx context.Context, system, user string) (string, error)
}

type Result struct {
	Markdown string
	Claims   []claims.Claim
	Provider string
}

const systemPrompt = `You are writing a seams document for a code change you did not make.

You are deliberately running without the building agent's context. Everything you
know comes from mechanical evidence: an AST-derived change bundle and the diff.
Do not restate the diff. Do not praise the code. Do not speculate about intent
where the evidence does not support it — say "not recorded" instead.

Write about seams: what must be true for this code to work, and what happens when
each of those things stops being true.

Structure the answer exactly like this:

## What changed, in one paragraph

## Assumptions this code makes
One bullet per assumption. For each, name the symbol it lives in and what breaks
first when the assumption fails.

## Invariants
Things that must hold across calls. Name the symbol that would violate each one
if it were wrong.

## Failure modes
What happens under: an error from a dependency, a concurrent caller, an empty or
malformed input, a slow or unavailable network. Only cover the ones the evidence
supports.

## Where to look first if this breaks
Ranked. Two or three entries.

Then, as the very last thing, a fenced code block tagged plum-claims containing
one claim per line in this exact format:

    <executable|assertion> :: <symbol id> :: <claim text>

Mark a claim executable only if a test could decide it from the code alone.
Everything else is an assertion — a trust-me statement — and being honest about
which is which is the point of the exercise.`

// Run synthesises the seams doc and extracts claims, fingerprinting each claim
// against the symbol it is about so staleness can be detected later (P5).
func Run(ctx context.Context, cfg *config.Config, b *bundle.Bundle, diff string, p Provider) (*Result, error) {
	user := Brief(b, diff, cfg.Synthesis.MaxDiff)
	md, err := p.Complete(ctx, systemPrompt, user)
	if err != nil {
		return nil, err
	}
	cs := extractClaims(md, b)
	return &Result{Markdown: strings.TrimSpace(md) + "\n", Claims: cs, Provider: p.Name()}, nil
}

var claimBlock = regexp.MustCompile("(?s)```plum-claims\\s*\n(.*?)```")

func extractClaims(md string, b *bundle.Bundle) []claims.Claim {
	m := claimBlock.FindStringSubmatch(md)
	if m == nil {
		return nil
	}
	fps := b.Fingerprints()
	var out []claims.Claim
	n := 0
	for _, line := range strings.Split(m[1], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// The separator is " :: " with spaces: a SymbolID contains a bare "::"
		// of its own, and splitting on that would cut the id in half.
		parts := strings.SplitN(line, " :: ", 3)
		if len(parts) != 3 {
			continue
		}
		kind := strings.TrimSpace(strings.TrimPrefix(parts[0], "-"))
		sym := bundle.SymbolID(strings.TrimSpace(parts[1]))
		n++
		out = append(out, claims.Claim{
			ID:          fmt.Sprintf("c-%03d", n),
			Claim:       strings.TrimSpace(parts[2]),
			Symbol:      sym,
			Executable:  strings.EqualFold(kind, "executable"),
			Fingerprint: fps[sym],
		})
	}
	return out
}

// Brief renders the bundle as the synthesis input. It is deliberately compact
// and mechanical: prose written over verified structure, never instead of it (P1).
func Brief(b *bundle.Bundle, diff string, maxDiff int) string {
	var w strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&w, f+"\n", a...) }

	p("# Change bundle for session %s", b.Session.ID)
	p("")
	p("Range %s..%s, agent %s.", shortSHA(b.Session.StartSHA), shortSHA(b.Session.EndSHA), b.Session.Agent)
	p("")

	p("## Symbols changed")
	for _, s := range b.Symbols {
		doc := s.Doc
		if doc == "" {
			doc = "(no doc)"
		}
		p("- [%s] %s `%s` — %s", s.Change, s.ID, oneline(s.Signature), oneline(doc))
	}
	p("")

	if n := len(b.Surface.Added) + len(b.Surface.Modified) + len(b.Surface.Removed); n > 0 {
		p("## Public surface")
		for _, m := range b.Surface.Modified {
			p("- signature changed: %s: %s -> %s", m.Name, oneline(m.Before), oneline(m.After))
		}
		for _, i := range b.Surface.Added {
			p("- added %s: %s", i.Kind, i.Name)
		}
		for _, i := range b.Surface.Removed {
			p("- removed %s: %s", i.Kind, i.Name)
		}
		p("")
	}

	if len(b.RiskMarkers) > 0 {
		p("## Risk markers (mechanical AST predicates, not opinions)")
		for _, r := range b.RiskMarkers {
			p("- %s at %s:%d in %s — %s", r.Kind, r.File, r.Line, r.Symbol, r.Note)
		}
		p("")
	}

	if len(b.Divergence.Findings) > 0 {
		p("## Divergence from repo conventions (score %.2f)", b.Divergence.Score)
		for _, f := range b.Divergence.Findings {
			p("- %s in %s: expected %s, observed %s [%s]", f.Convention, f.Symbol, f.Expected, f.Observed, f.Source)
		}
		p("")
	}

	if len(b.Edges) > 0 {
		p("## Call edges out of the changed set")
		for _, e := range b.Edges {
			tag := ""
			if e.CrossesModule {
				tag = " (crosses module)"
			}
			p("- %s -> %s%s", e.From, e.To, tag)
		}
		p("")
	}

	if len(b.Coverage.Untested) > 0 {
		p("## Not named by any changed test")
		for _, id := range b.Coverage.Untested {
			p("- %s", id)
		}
		p("")
	}

	if len(b.Journal) > 0 {
		p("## Rationale recorded live during the session")
		for _, j := range b.Journal {
			p("- %s (%s): %s", j.File, j.Tool, j.Rationale)
			for _, a := range j.Alternatives {
				p("  - rejected: %s", a)
			}
		}
		p("")
	} else {
		p("## Rationale")
		p("None recorded. Treat every 'why' below as reconstruction, and say so.")
		p("")
	}

	if diff != "" {
		if len(diff) > maxDiff && maxDiff > 0 {
			diff = diff[:maxDiff] + "\n... (diff truncated)"
		}
		p("## Diff")
		p("```diff")
		p("%s", diff)
		p("```")
	}
	return w.String()
}

// SymbolIDs lists the changed symbols, for prompts that need the join keys.
func SymbolIDs(b *bundle.Bundle) []string {
	var out []string
	for _, s := range b.Symbols {
		out = append(out, string(s.ID))
	}
	sort.Strings(out)
	return out
}

func oneline(s string) string { return strings.Join(strings.Fields(s), " ") }

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
