package explore

import (
	"testing"

	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/trace"
)

func landscape() trace.Landscape {
	return trace.Landscape{Wells: []trace.Well{
		{Symbol: "a.go::AskedTwice", Phase: "enter", Doc: "documented"},
		{Symbol: "a.go::NeverVisited", Phase: "enter", Doc: "documented"},
		{Symbol: "a.go::VisitedUndocumented", Phase: "enter"},
		{Symbol: "a.go::VisitedFine", Phase: "enter", Doc: "documented"},
		{Symbol: "a.go::VisitedFine", Phase: "resume", Doc: "documented"},
	}}
}

// Exploration telemetry is a direct read on where your model was thin, and that
// is what makes M4 targeted rather than random (spec §11.1).
func TestTargetSymbolsRanksByWhereUnderstandingWasExpensive(t *testing.T) {
	tel := []Event{
		{Symbol: "a.go::AskedTwice", Action: "prompt"},
		{Symbol: "a.go::AskedTwice", Action: "prompt"},
		{Symbol: "a.go::AskedTwice", Action: "click"},
		{Symbol: "a.go::VisitedUndocumented", Action: "click"},
		{Symbol: "a.go::VisitedFine", Action: "click"},
	}
	got := TargetSymbols(tel, landscape(), 3)
	want := []bundle.SymbolID{"a.go::AskedTwice", "a.go::NeverVisited", "a.go::VisitedUndocumented"}
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rank %d = %s, want %s (full order %v)", i, got[i], want[i], got)
		}
	}
	// A frame you visited and that is documented is the least likely debt.
	for _, id := range got {
		if id == "a.go::VisitedFine" {
			t.Error("a visited, documented frame should not be targeted before the others")
		}
	}
}

func TestTelemetryAndDoneGateRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())
	if s.IsDone("s1") {
		t.Fatal("a session starts un-explored")
	}
	if err := s.Append(Event{SessionID: "s1", Symbol: "a.go::X", Action: "click", DwellMS: 42}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Event{SessionID: "s2", Symbol: "a.go::Y", Action: "click"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("s1")
	if err != nil || len(got) != 1 || got[0].DwellMS != 42 {
		t.Fatalf("load = %v (%v)", got, err)
	}
	if err := s.MarkDone("s1"); err != nil {
		t.Fatal(err)
	}
	if !s.IsDone("s1") || s.IsDone("s2") {
		t.Error("done is per session")
	}
}

func TestMissesAccumulate(t *testing.T) {
	s := NewStore(t.TempDir())
	for _, m := range []Miss{
		{SessionID: "s1", Kind: "exception", Symbol: "a.go::X"},
		{SessionID: "s2", Kind: "exception", Symbol: "a.go::Y"},
	} {
		if err := s.AppendMiss(m); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LoadMisses()
	if err != nil || len(got) != 2 {
		t.Fatalf("misses = %v (%v)", got, err)
	}
}
