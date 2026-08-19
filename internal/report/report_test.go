package report

import (
	"strings"
	"testing"

	"github.com/kelalaike/plum/internal/bundle"
)

func sample() *bundle.Bundle {
	return &bundle.Bundle{
		Session: bundle.Session{ID: "2026-08-19-a3f2", StartSHA: "aaaaaaaaaa", EndSHA: "bbbbbbbbbb", Agent: "claude-code"},
		Symbols: []bundle.Symbol{
			{ID: "a.go::Get", Name: "Get", Kind: "method", File: "a.go", LineStart: 10, LineEnd: 20, Change: "modified", Doc: "Get returns."},
			{ID: "a.go::helper", Name: "helper", Kind: "func", File: "a.go", LineStart: 30, LineEnd: 35, Change: "added"},
		},
		Surface: bundle.SurfaceDelta{
			Modified: []bundle.SurfaceMod{{
				SurfaceItem: bundle.SurfaceItem{Kind: "export", Name: "auth.Get", File: "a.go"},
				Before:      "func Get(k string) error", After: "func Get(k string, o any) error",
			}},
			Added: []bundle.SurfaceItem{{Kind: "route", Name: "GET /tokens", File: "a.go"}},
		},
		RiskMarkers: []bundle.RiskMarker{{Kind: "swallowed_error", Symbol: "a.go::helper", File: "a.go", Line: 32, Note: "error checked and then discarded"}},
		Divergence:  bundle.Divergence{Score: 0.55, Findings: []bundle.DivergenceFinding{{Convention: "error_handling", Expected: "wrapped", Observed: "panic", Symbol: "a.go::helper", Severity: "high", Source: "empirical"}}},
		Coverage:    bundle.Coverage{Untested: []bundle.SymbolID{"a.go::helper"}, SymbolCount: 2},
		Gate:        bundle.Gate{Fired: true, Reasons: []string{"new public surface", "divergence 0.55"}},
	}
}

// Read-first ordering, not source ordering — and a signature change on an
// existing export outranks everything else (spec §6.8).
func TestSectionOrdering(t *testing.T) {
	out := Render(sample(), Options{})
	order := []string{
		"GATE FIRED",
		"## ⚠ Public surface changed",
		"## ⚠ Divergence from repo conventions",
		"## Risk markers",
		"## Symbols changed",
		"## Untested new symbols",
		"## Rationale (from journal)",
	}
	last := -1
	for _, section := range order {
		i := strings.Index(out, section)
		if i < 0 {
			t.Fatalf("missing section %q", section)
		}
		if i < last {
			t.Errorf("section %q is out of read-first order", section)
		}
		last = i
	}
	sigIdx := strings.Index(out, "signature changed")
	newIdx := strings.Index(out, "new route")
	if sigIdx > newIdx {
		t.Error("signature changes on existing exports must come before new surface")
	}
	// Deliberately language-neutral: Python and YAML have callers too, and
	// neither of them compiles.
	if !strings.Contains(out, "every existing caller was written against the old shape") {
		t.Error("the report should say why a signature change matters")
	}
}

func TestNoJournalIsCalledOutRatherThanOmitted(t *testing.T) {
	out := Render(sample(), Options{})
	if !strings.Contains(out, "No journal entries") || !strings.Contains(out, "plum note") {
		t.Error("a session with no rationale should say so, and say how to fix it (P3)")
	}
}

func TestGateClearReadsDifferently(t *testing.T) {
	b := sample()
	b.Gate = bundle.Gate{}
	if out := Render(b, Options{}); !strings.HasPrefix(out, "gate clear") {
		t.Errorf("clear gate should be stated plainly, got %q", out[:40])
	}
}

func TestStaleClaimsAppearWhenPresent(t *testing.T) {
	out := Render(sample(), Options{Stale: []StaleClaim{{ID: "c-001", Claim: "x is idempotent", Symbol: "a.go::Get", Reason: "fingerprint moved"}}})
	if !strings.Contains(out, "## ⚠ Stale claims") || !strings.Contains(out, "c-001") {
		t.Error("stale claims must be surfaced in the report (P5)")
	}
}

func TestTestFilesAreNotFlaggedAsDebt(t *testing.T) {
	b := sample()
	b.Symbols = append(b.Symbols, bundle.Symbol{
		ID: "a_test.go::TestGet", Name: "TestGet", Kind: "func", File: "a_test.go",
		LineStart: 1, LineEnd: 5, Change: "added",
	})
	out := Render(b, Options{})
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "TestGet") && strings.Contains(line, "untested") {
			t.Errorf("a test file was reported as comprehension debt: %q", line)
		}
	}
}
