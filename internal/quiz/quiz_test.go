package quiz

import (
	"testing"

	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/trace"
)

func TestGradeRejectsAccidentalSubstrings(t *testing.T) {
	q := Question{Expected: "auth: no token for absent"}
	// "b" appears inside the recorded value. Accepting it would make the whole
	// exercise theatre, which is exactly what P4 exists to prevent.
	if Grade(q, "b") {
		t.Error("a single letter must not count as knowing the answer")
	}
	if !Grade(q, "auth: no token for absent") {
		t.Error("the exact recorded value must count")
	}
	if !Grade(q, `"auth: no token for absent."`) {
		t.Error("punctuation and quoting must not matter")
	}
	if Grade(q, "cache miss") {
		t.Error("a different value must not count")
	}
	if Grade(q, "") {
		t.Error("an empty answer is not correct")
	}
}

func TestGradeMultipleChoiceIsExact(t *testing.T) {
	q := Question{Expected: "Cache.lookup", Options: []string{"Cache.lookup", "Cache.decorate"}}
	if !Grade(q, "cache.lookup") {
		t.Error("case should not matter")
	}
	if Grade(q, "lookup") {
		t.Error("a partial option must not count when options were offered")
	}
}

func TestSelectionPrefersVarietyOverInvocationOrder(t *testing.T) {
	l := trace.Landscape{Wells: []trace.Well{
		{Symbol: "a.go::atoi", Phase: "enter", Doc: "atoi parses."},
		{Symbol: "a.go::Risky", Phase: "enter", Risk: true, Depth: 3},
	}}
	var events []trace.Event
	// Five trivial invocations of the same helper, and one of a risky frame.
	for i := 0; i < 5; i++ {
		id := string(rune('a' + i))
		events = append(events,
			trace.Event{Kind: "call", Symbol: "a.go::atoi", InvocationID: id, Args: map[string]string{"s": id}},
			trace.Event{Kind: "return", Symbol: "a.go::atoi", InvocationID: id, Result: id})
	}
	events = append(events,
		trace.Event{Kind: "call", Symbol: "a.go::Risky", InvocationID: "z", Args: map[string]string{"k": "x"}},
		trace.Event{Kind: "raise", Symbol: "a.go::Risky", InvocationID: "z", Exception: "boom"})

	qs := Generate(nil, events, l, 2)
	if len(qs) != 2 {
		t.Fatalf("got %d questions", len(qs))
	}
	syms := map[bundle.SymbolID]int{}
	for _, q := range qs {
		syms[q.Symbol]++
	}
	if syms["a.go::Risky"] == 0 {
		t.Errorf("the risky frame that raised was not asked about: %+v", qs)
	}
	if syms["a.go::atoi"] > 1 {
		t.Errorf("the same trivial helper was asked about twice: %+v", qs)
	}
}

func TestTargetedSymbolsWin(t *testing.T) {
	l := trace.Landscape{Wells: []trace.Well{
		{Symbol: "a.go::atoi", Phase: "enter"},
		{Symbol: "a.go::Other", Phase: "enter"},
	}}
	events := []trace.Event{
		{Kind: "call", Symbol: "a.go::atoi", InvocationID: "1", Args: map[string]string{"s": "1"}},
		{Kind: "return", Symbol: "a.go::atoi", InvocationID: "1", Result: "1"},
		{Kind: "call", Symbol: "a.go::Other", InvocationID: "2", Args: map[string]string{"s": "2"}},
		{Kind: "return", Symbol: "a.go::Other", InvocationID: "2", Result: "2"},
	}
	qs := Generate([]bundle.SymbolID{"a.go::Other"}, events, l, 1)
	if len(qs) != 1 || qs[0].Symbol != "a.go::Other" {
		t.Errorf("telemetry targeting was ignored: %+v", qs)
	}
}
