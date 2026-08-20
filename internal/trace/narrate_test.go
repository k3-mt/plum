package trace

import (
	"strings"
	"testing"

	"github.com/k3-mt/plum/internal/bundle"
)

func narrateBundle() *bundle.Bundle {
	return &bundle.Bundle{
		Session: bundle.Session{ID: "s1"},
		Symbols: []bundle.Symbol{
			{ID: "a.go::Cache.Get", Name: "Cache.Get", Doc: "Get returns the token for a key, or ErrMiss.",
				CallSites: []bundle.CallSite{{
					Callee: "a.go::Cache.decorate", CalleeRaw: "c.decorate", Line: 9,
					Rationale: "the realm suffix is applied on the way out",
				}}},
			{ID: "a.go::Cache.decorate", Name: "Cache.decorate"},
		},
		RiskMarkers: []bundle.RiskMarker{{
			Kind: "widened_type", Symbol: "a.go::Cache.Get",
			Note: "parameter opts is typed as any — the compiler stops helping callers",
		}},
	}
}

func narrateEvents() []Event {
	return []Event{
		{Kind: "call", Symbol: "a.go::Cache.Get", InvocationID: "1", Depth: 0, TSNanos: 0,
			Args: map[string]string{"key": "user:42", "opts": "<nil>"}},
		{Kind: "call", Symbol: "a.go::Cache.decorate", InvocationID: "2", ParentID: "1", Depth: 1, TSNanos: 20_000,
			Args: map[string]string{"v": "tok"}},
		{Kind: "return", Symbol: "a.go::Cache.decorate", InvocationID: "2", Depth: 1, TSNanos: 40_000, Result: "tok@prod"},
		{Kind: "return", Symbol: "a.go::Cache.Get", InvocationID: "1", Depth: 0, TSNanos: 60_000, Result: "tok@prod, <nil>"},
	}
}

// A landscape names symbols. The narration has to say what happened — the
// values that went in, the values that came back, and why the call was made.
func TestNarrationSaysWhatHappened(t *testing.T) {
	b := narrateBundle()
	l := Derive(narrateEvents(), b)
	var text strings.Builder
	for _, s := range Narrate(l, b) {
		text.WriteString(s.Text + "\n")
	}
	got := text.String()

	for _, want := range []string{
		`Cache.Get was called with key = "user:42" and opts = nothing`, // the empty case is named, not printed as <nil>
		`It returned "tok@prod" and no error`,                          // a Go (value, error) pair read as English
		`Its own description: "Get returns the token for a key, or ErrMiss."`,
		`Flagged: parameter opts is typed as any`,
		`Cache.Get then called Cache.decorate`,
		`The code says why: "the realm suffix is applied on the way out"`, // the call-site comment
		`Control came back into Cache.Get`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("narration is missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// Where the evidence is silent, the narration says so instead of inventing
// something plausible.
func TestNarrationNamesWhatIsMissing(t *testing.T) {
	b := narrateBundle()
	l := Derive(narrateEvents(), b)
	var notes []string
	for _, s := range Narrate(l, b) {
		if s.Note != "" {
			notes = append(notes, s.Note)
		}
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "no description was written for this function") {
		t.Errorf("an undocumented frame should say so: %v", notes)
	}
	// Cache.Get has a doc, so it must not be accused of lacking one.
	for _, s := range Narrate(l, b) {
		if strings.Contains(s.Text, "Cache.Get was called") && strings.Contains(s.Note, "no description") {
			t.Error("a documented function was reported as undocumented")
		}
	}
}

func TestSummaryReadsAsASentence(t *testing.T) {
	b := narrateBundle()
	l := Derive(narrateEvents(), b)
	l.TestID = "TestGetPut"
	got := Summary(l, b)
	for _, want := range []string{`Running "TestGetPut"`, "2 functions", "Cache.Get → Cache.decorate", "Every frame that was entered came back."} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q: %s", want, got)
		}
	}
}

func TestSummaryReportsAnEscapedFailure(t *testing.T) {
	b := narrateBundle()
	ev := []Event{
		{Kind: "call", Symbol: "a.go::Cache.Get", InvocationID: "1", Depth: 0},
		{Kind: "call", Symbol: "a.go::Cache.decorate", InvocationID: "2", ParentID: "1", Depth: 1, TSNanos: 10},
		{Kind: "raise", Symbol: "a.go::Cache.decorate", InvocationID: "2", Depth: 1, TSNanos: 20, Exception: "boom"},
		{Kind: "raise", Symbol: "a.go::Cache.Get", InvocationID: "1", Depth: 0, TSNanos: 30, Exception: "boom"},
	}
	l := Derive(ev, b)
	if got := Summary(l, b); !strings.Contains(got, "failure that nothing caught") {
		t.Errorf("summary = %q", got)
	}
	var sawUnwind bool
	for _, s := range Narrate(l, b) {
		if strings.Contains(s.Text, "unwound") {
			sawUnwind = true
			if !strings.Contains(s.Note, "none of them get to finish") {
				t.Errorf("the cliff should explain itself: %q", s.Note)
			}
		}
	}
	if !sawUnwind {
		t.Error("a raise should be narrated as an unwind")
	}
}

func TestNarrationOfAnEmptyRecording(t *testing.T) {
	b := narrateBundle()
	if got := Summary(Landscape{}, b); !strings.Contains(got, "Nothing was recorded") {
		t.Errorf("summary = %q", got)
	}
	if steps := Narrate(Landscape{}, b); len(steps) != 0 {
		t.Errorf("steps = %v", steps)
	}
}

func TestSurroundingFramesAreNamedAsSuch(t *testing.T) {
	b := narrateBundle()
	ev := []Event{
		{Kind: "call", Symbol: "a.go::Router.handle", InvocationID: "1", Depth: 0}, // never changed
		{Kind: "call", Symbol: "a.go::Cache.Get", InvocationID: "2", ParentID: "1", Depth: 1, TSNanos: 10},
		{Kind: "return", Symbol: "a.go::Cache.Get", InvocationID: "2", Depth: 1, TSNanos: 20, Result: "x"},
		{Kind: "return", Symbol: "a.go::Router.handle", InvocationID: "1", Depth: 0, TSNanos: 30, Result: "y"},
	}
	l := Derive(ev, b)
	var found bool
	for _, s := range Narrate(l, b) {
		if strings.Contains(s.Text, "Router.handle was called") {
			found = true
			if !strings.Contains(s.Note, "did not change this function") {
				t.Errorf("surrounding code should say why it is here: %q", s.Note)
			}
		}
	}
	if !found {
		t.Error("the surrounding frame was not narrated")
	}
}

// A sentence flattened to a string loses the difference between an identifier,
// a value the run produced, and prose a person wrote — and that difference is
// what tells a reader how much to trust each part.
func TestSentencesCarryLabelledSpans(t *testing.T) {
	b := narrateBundle()
	l := Derive(narrateEvents(), b)
	steps := Narrate(l, b)

	var first Step
	for _, s := range steps {
		if s.Kind == "frame" {
			first = s
			break
		}
	}
	if len(first.Spans) == 0 {
		t.Fatal("no spans: the browser would have to guess with a regex")
	}

	// The flattened text and the spans must always agree, or the terminal and
	// the page would be describing different runs.
	var rebuilt strings.Builder
	kinds := map[string]string{}
	for _, sp := range first.Spans {
		rebuilt.WriteString(sp.Text)
		if _, seen := kinds[sp.Kind]; !seen {
			kinds[sp.Kind] = sp.Text
		}
	}
	if rebuilt.String() != first.Text {
		t.Errorf("spans do not reconstruct the sentence:\n%q\n%q", rebuilt.String(), first.Text)
	}

	if got := kinds["code"]; got != "Cache.Get" {
		t.Errorf("the identifier was labelled %q", got)
	}
	if !strings.Contains(kinds["value"], "user:42") {
		t.Errorf("the recorded argument was not labelled as a value: %q", kinds["value"])
	}
	if !strings.Contains(kinds["quote"], "Get returns the token") {
		t.Errorf("the doc comment was not labelled as prose: %q", kinds["quote"])
	}
	if !strings.Contains(kinds["risk"], "compiler stops helping") {
		t.Errorf("the risk marker was not labelled: %q", kinds["risk"])
	}
}

func TestTransitionSpansSeparateCostFromProse(t *testing.T) {
	b := narrateBundle()
	l := Derive(narrateEvents(), b)
	for _, s := range Narrate(l, b) {
		if s.Kind != "transition" || !strings.Contains(s.Text, "then called") {
			continue
		}
		kinds := map[string]string{}
		for _, sp := range s.Spans {
			kinds[sp.Kind] += sp.Text
		}
		if kinds["cost"] == "" {
			t.Error("a measured duration should be labelled as one")
		}
		if !strings.Contains(kinds["quote"], "realm suffix") {
			t.Errorf("the call-site comment should be labelled as prose: %q", kinds["quote"])
		}
		if !strings.Contains(kinds["code"], "Cache.decorate") {
			t.Errorf("the callee should be labelled as an identifier: %q", kinds["code"])
		}
		return
	}
	t.Fatal("no descend transition was narrated")
}

// Quoting every recorded value turned the integer 3 into "3", which reads as a
// string — the sort of type confusion a reviewer chases into the code.
func TestNumbersAndBooleansAreNotQuotedIntoStrings(t *testing.T) {
	for in, want := range map[string]string{
		"3":         "3",
		"-2.5":      "-2.5",
		"true":      "true",
		"False":     "False",
		"[1, 2]":    "[1, 2]",
		`{"a": 1}`:  `{"a": 1}`,
		"map[k:v]":  "map[k:v]",
		"'SKU-1'":   "'SKU-1'", // already a repr; do not double-quote it
		"some text": `"some text"`,
		"":          "nothing",
		"None":      "nothing",
	} {
		if got := HumanValue(in); got != want {
			t.Errorf("HumanValue(%q) = %s, want %s", in, got, want)
		}
	}
}

// Surrounding code is recorded for structure only — entering and leaving,
// nothing captured. Reading that absence as "it returned nothing" states a fact
// about the code that the recording does not hold.
func TestAnUncapturedReturnIsNotReportedAsNothing(t *testing.T) {
	b := testBundle()
	b.Symbols = []bundle.Symbol{{ID: "a.go::Verify", Name: "Verify"}}
	ev := []Event{
		call("a.go::Verify", "1", "", 0, 0),
		call("a.go::lookup", "2", "1", 1, 100),
		// A context frame's probe records the exit but captures no value.
		ret("a.go::lookup", "2", 1, 200, ""),
		ret("a.go::Verify", "1", 0, 300, "ok"),
	}
	steps := Narrate(DeriveChain(ev, b, ChainHottest), b)
	var seen bool
	for _, s := range steps {
		if !strings.Contains(s.Text, "lookup") || s.Kind != "frame" {
			continue
		}
		seen = true
		if strings.Contains(s.Text, "returned nothing") {
			t.Errorf("narration invents a return value: %q", s.Text)
		}
		if !strings.Contains(s.Note, "not captured") {
			t.Errorf("note should say the value was not captured, got %q", s.Note)
		}
	}
	if !seen {
		t.Fatal("the context frame was never narrated")
	}

	// A function that genuinely returned a nil/None still says so.
	if got := HumanValue("None"); got != "nothing" {
		t.Errorf("an actual None = %q", got)
	}
}
