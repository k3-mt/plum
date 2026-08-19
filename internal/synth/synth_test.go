package synth

import (
	"context"
	"strings"
	"testing"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/config"
)

func demoBundle() *bundle.Bundle {
	return &bundle.Bundle{
		Session: bundle.Session{ID: "s1"},
		Symbols: []bundle.Symbol{
			{ID: "a.go::Cache.Get", Name: "Cache.Get", Kind: "method", File: "a.go", Change: "modified", Exported: true, Fingerprint: "sha256:aaa"},
			{ID: "a.go::Cache.Refresh", Name: "Cache.Refresh", Kind: "method", File: "a.go", Change: "added", Exported: true, Fingerprint: "sha256:bbb"},
		},
		RiskMarkers: []bundle.RiskMarker{
			{Kind: "network_without_timeout", Symbol: "a.go::Cache.Refresh", File: "a.go", Line: 40, Note: "no deadline"},
			{Kind: "swallowed_error", Symbol: "a.go::Cache.Refresh", File: "a.go", Line: 41, Note: "discarded"},
		},
		Surface: bundle.SurfaceDelta{Modified: []bundle.SurfaceMod{{
			SurfaceItem: bundle.SurfaceItem{Kind: "export", Name: "auth.Cache.Get", Symbol: "a.go::Cache.Get"},
			Before:      "func Get(k string) error", After: "func Get(k string, o any) error",
		}}},
	}
}

func TestOfflineSynthesisIsGroundedInTheBundle(t *testing.T) {
	b := demoBundle()
	res, err := Run(context.Background(), config.Default("/repo"), b, "", &Offline{Bundle: b})
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{
		"## Assumptions this code makes", "## Invariants", "## Failure modes",
		"## Where to look first if this breaks",
	} {
		if !strings.Contains(res.Markdown, section) {
			t.Errorf("missing section %q", section)
		}
	}
	if !strings.Contains(res.Markdown, "the remote side always answers") {
		t.Error("a network call with no deadline should surface as an assumption")
	}
	if !strings.Contains(res.Markdown, "Not recorded") {
		t.Error("a session with no journal should say the rationale is unrecoverable (P3)")
	}
}

// A SymbolID contains "::" of its own, so the claim separator must be " :: ".
func TestClaimExtractionKeepsWholeSymbolIDs(t *testing.T) {
	b := demoBundle()
	md := "text\n\n```plum-claims\n" +
		"executable :: a.go::Cache.Get :: Get is idempotent for a fixed key\n" +
		"assertion :: a.go::Cache.Refresh :: refresh never blocks the request path\n" +
		"garbage line with no separators\n" +
		"```\n"
	cs := extractClaims(md, b)
	if len(cs) != 2 {
		t.Fatalf("got %d claims: %+v", len(cs), cs)
	}
	if cs[0].Symbol != "a.go::Cache.Get" {
		t.Errorf("symbol = %q", cs[0].Symbol)
	}
	if cs[0].Claim != "Get is idempotent for a fixed key" {
		t.Errorf("claim = %q", cs[0].Claim)
	}
	if !cs[0].Executable || cs[1].Executable {
		t.Error("executable flags are wrong")
	}
	if cs[0].Fingerprint != "sha256:aaa" {
		t.Errorf("claims must capture the subject's fingerprint at write time, got %q", cs[0].Fingerprint)
	}
	if cs[0].ID != "c-001" || cs[1].ID != "c-002" {
		t.Errorf("ids = %q %q", cs[0].ID, cs[1].ID)
	}
}

// The builder does not narrate (P2): the brief carries mechanical evidence only.
func TestBriefCarriesEvidenceAndTruncatesTheDiff(t *testing.T) {
	b := demoBundle()
	brief := Brief(b, strings.Repeat("x", 5000), 100)
	if !strings.Contains(brief, "a.go::Cache.Refresh") || !strings.Contains(brief, "network_without_timeout") {
		t.Error("the brief should carry symbols and risk markers")
	}
	if !strings.Contains(brief, "diff truncated") {
		t.Error("an oversized diff should be truncated, not sent whole")
	}
	if strings.Contains(brief, "transcript") {
		t.Error("the brief must never reference the building agent's transcript")
	}
}
