package interpret

import (
	"strings"
	"testing"
	"time"

	"github.com/kelalaike/plum/internal/bundle"
)

func testBundle() *bundle.Bundle {
	return &bundle.Bundle{
		Session: bundle.Session{ID: "2026-08-20-abcd"},
		Symbols: []bundle.Symbol{
			{ID: "a.go::Get", Name: "Get", Fingerprint: "sha256:aaa"},
			{ID: "a.go::decorate", Name: "decorate", Fingerprint: "sha256:bbb"},
		},
		RiskMarkers: []bundle.RiskMarker{{
			Kind: "swallowed_error", Symbol: "a.go::Get", File: "a.go", Line: 12,
			Note: "error checked and then discarded",
		}},
		Journal: []bundle.JournalEntry{{
			Rationale:    "realm is read per call so tests can change it",
			Alternatives: []string{"reading it once at startup"},
		}},
		Surface: bundle.SurfaceDelta{Modified: []bundle.SurfaceMod{{
			SurfaceItem: bundle.SurfaceItem{Kind: "export", Name: "auth.Get"},
			Before:      "func Get(k string) error", After: "func Get(k string, o any) error",
		}}},
	}
}

// The brief must lead with the mechanical evidence and label it as settled, or
// the model has no way to tell what it may not contradict.
func TestBriefLeadsWithEstablishedFact(t *testing.T) {
	b := testBundle()
	got := Brief(ScopeSession, "", "The run went through 2 functions.",
		[]string{"Get was called with k = \"x\".", "Get then called decorate."},
		"## Symbol\na.go::Get\n", b)

	factsAt := strings.Index(got, "established mechanically")
	if factsAt < 0 {
		t.Fatal("the brief does not mark the recording as established fact")
	}
	for _, want := range []string{
		"The run went through 2 functions.",
		"Get then called decorate.",
		"realm is read per call so tests can change it",
		"considered and rejected: reading it once at startup",
		"swallowed_error at a.go:12",
		"changed: auth.Get",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("brief is missing %q", want)
		}
	}
}

// A session with no journal must say so plainly: "nobody wrote down why" is the
// finding, and inviting the model to fill that gap is how a tool starts lying.
func TestBriefSaysWhenNoRationaleWasRecorded(t *testing.T) {
	b := testBundle()
	b.Journal = nil
	got := Brief(ScopeSession, "", "summary", nil, "", b)
	if !strings.Contains(got, "Nothing about why this was built this way was written down") {
		t.Error("an empty journal should be stated, not omitted")
	}
}

func TestSystemPromptForbidsInvention(t *testing.T) {
	for _, want := range []string{
		"Mark every inference",
		"Never invent a rationale that was not recorded",
		"What the evidence does not settle",
	} {
		if !strings.Contains(SystemPrompt, want) {
			t.Errorf("the prompt does not say %q", want)
		}
	}
}

// A reading is only useful while it still describes the code as it is now.
func TestReadingsGoStaleWhenTheirSubjectMoves(t *testing.T) {
	f := &File{Entries: map[string]Entry{}}
	entry := Entry{
		Scope: ScopeSession, Markdown: "a reading", Provider: "tmux/x",
		Fingerprints: map[bundle.SymbolID]string{
			"a.go::Get":      "sha256:aaa",
			"a.go::decorate": "sha256:bbb",
		},
		GeneratedAt: time.Now(),
	}
	f.Entries[entry.Key()] = entry

	unchanged := map[bundle.SymbolID]string{"a.go::Get": "sha256:aaa", "a.go::decorate": "sha256:bbb"}
	if got := f.Stale(unchanged); len(got) != 0 {
		t.Fatalf("unchanged code should not stale a reading: %v", got)
	}

	moved := map[bundle.SymbolID]string{"a.go::Get": "sha256:aaa", "a.go::decorate": "sha256:ZZZ"}
	got := f.Stale(moved)
	if len(got) != 1 || len(got[0].Moved) != 1 || got[0].Moved[0] != "a.go::decorate" {
		t.Fatalf("stale = %+v", got)
	}

	// A symbol that no longer exists at all is just as stale.
	deleted := map[bundle.SymbolID]string{"a.go::Get": "sha256:aaa"}
	if got := f.Stale(deleted); len(got) != 1 || len(got[0].Moved) != 1 {
		t.Fatalf("a deleted subject should stale the reading: %+v", got)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	f, err := Load(dir)
	if err != nil || len(f.Entries) != 0 {
		t.Fatalf("a fresh session has no readings: %v %v", f, err)
	}
	entry := Entry{
		Scope: ScopeTest, Subject: "TestGetPut", Markdown: "# In one sentence\n...",
		Provider: "tmux/pane", Fingerprints: map[bundle.SymbolID]string{"a.go::Get": "sha256:aaa"},
		GeneratedAt: time.Now().UTC().Truncate(time.Second),
	}
	f.Entries[entry.Key()] = entry
	if err := Save(dir, f); err != nil {
		t.Fatal(err)
	}
	back, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := back.Entries["test:TestGetPut"]
	if !ok {
		t.Fatalf("keyed wrongly: %v", back.Entries)
	}
	if got.Markdown != entry.Markdown || got.Provider != entry.Provider {
		t.Errorf("round trip lost content: %+v", got)
	}
	if got.Fingerprints["a.go::Get"] != "sha256:aaa" {
		t.Error("fingerprints must survive, or staleness cannot be checked")
	}
}

func TestFingerprintsForNarrowsToTheFramesDescribed(t *testing.T) {
	b := testBundle()
	all := FingerprintsFor(b, nil)
	if len(all) != 2 {
		t.Errorf("no ids means everything in the session: %v", all)
	}
	one := FingerprintsFor(b, []bundle.SymbolID{"a.go::Get", "a.go::missing"})
	if len(one) != 1 || one["a.go::Get"] != "sha256:aaa" {
		t.Errorf("a reading depends only on frames that exist: %v", one)
	}
}
