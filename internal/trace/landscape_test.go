package trace

import (
	"strings"
	"testing"

	"github.com/kelalaike/plum/internal/bundle"
)

func testBundle() *bundle.Bundle {
	return &bundle.Bundle{
		Session: bundle.Session{ID: "s1"},
		Symbols: []bundle.Symbol{
			{ID: "a.go::Verify", Name: "Verify", Doc: "Verify checks a token."},
			{ID: "a.go::check", Name: "check"},
			{ID: "a.go::MustGet", Name: "MustGet", Doc: "MustGet panics when absent."},
			{ID: "a.go::lookup", Name: "lookup"},
		},
		RiskMarkers: []bundle.RiskMarker{{Kind: "package_level_state", Symbol: "a.go::lookup"}},
	}
}

func call(sym bundle.SymbolID, id, parent string, depth int, ts int64) Event {
	return Event{Kind: "call", Symbol: sym, InvocationID: id, ParentID: parent, Depth: depth, TSNanos: ts}
}
func ret(sym bundle.SymbolID, id string, depth int, ts int64, result string) Event {
	return Event{Kind: "return", Symbol: sym, InvocationID: id, Depth: depth, TSNanos: ts, Result: result}
}
func raise(sym bundle.SymbolID, id string, depth int, ts int64, exc string) Event {
	return Event{Kind: "raise", Symbol: sym, InvocationID: id, Depth: depth, TSNanos: ts, Exception: exc}
}

// The path must close: every frame that goes down comes back up, and the trace
// ends at the depth it started (spec §9.3).
func TestDerivePathCloses(t *testing.T) {
	ev := []Event{
		call("a.go::Verify", "1", "", 0, 0),
		call("a.go::check", "2", "1", 1, 100),
		call("a.go::lookup", "3", "2", 2, 200),
		ret("a.go::lookup", "3", 2, 400, "tok, <nil>"),
		ret("a.go::check", "2", 1, 500, "true"),
		ret("a.go::Verify", "1", 0, 600, "true"),
	}
	l := Derive(ev, testBundle())
	if !l.Closed {
		t.Fatalf("path did not close: open at %s", l.OpenFrame)
	}
	// It ends at the depth it started: the last return pops past the entry point.
	if l.Wells[0].Depth != 0 || l.Wells[len(l.Wells)-1].Depth != 0 {
		t.Errorf("depths = %d..%d, want 0..0", l.Wells[0].Depth, l.Wells[len(l.Wells)-1].Depth)
	}
	var descend, ascend int
	for _, b := range l.Barriers {
		switch b.Direction {
		case "descend":
			descend++
		case "ascend":
			ascend++
		}
	}
	// Two descents; two ascents are recorded as barriers, the last of which pops
	// past the entry point and so needs no further well.
	if descend != 2 || ascend != 2 {
		t.Errorf("descend %d, ascend %d — a round trip has one ascent per descent", descend, ascend)
	}
}

// A resumed well carries the same SymbolID as its enter twin and renders faded.
func TestResumedWellsShareTheirSymbol(t *testing.T) {
	ev := []Event{
		call("a.go::Verify", "1", "", 0, 0),
		call("a.go::lookup", "2", "1", 1, 100),
		ret("a.go::lookup", "2", 1, 200, "x"),
		ret("a.go::Verify", "1", 0, 300, "y"),
	}
	l := Derive(ev, testBundle())
	var resumed *Well
	for i := range l.Wells {
		if l.Wells[i].Phase == "resume" {
			resumed = &l.Wells[i]
		}
	}
	if resumed == nil {
		t.Fatal("no resumed well — returning into a frame must be visible")
	}
	if resumed.Symbol != "a.go::Verify" {
		t.Errorf("resumed symbol = %s", resumed.Symbol)
	}
	// The invocation list is scoped to the phase: args going in, value coming out.
	if len(resumed.Invocations) == 0 {
		t.Error("resumed well should carry the value that came back")
	}
}

// A panic unwinding several frames is a cliff, not a staircase (spec §9.3).
func TestUnwindIsOneCliffNotAStaircase(t *testing.T) {
	ev := []Event{
		call("a.go::Verify", "1", "", 0, 0),
		call("a.go::check", "2", "1", 1, 100),
		call("a.go::MustGet", "3", "2", 2, 200),
		raise("a.go::MustGet", "3", 2, 300, "boom"),
		raise("a.go::check", "2", 1, 310, "boom"),
	}
	l := Derive(ev, testBundle())
	var unwinds []Barrier
	for _, b := range l.Barriers {
		if b.Direction == "unwind" {
			unwinds = append(unwinds, b)
		}
	}
	if len(unwinds) != 1 {
		t.Fatalf("got %d unwind barriers, want exactly 1 cliff", len(unwinds))
	}
	if unwinds[0].Frames != 2 {
		t.Errorf("cliff spans %d frames, want 2", unwinds[0].Frames)
	}
	if unwinds[0].Kind != "raise" {
		t.Errorf("kind = %q", unwinds[0].Kind)
	}
	// It lands on the frame that caught it.
	if got := l.Wells[unwinds[0].ToIdx].Symbol; got != "a.go::Verify" {
		t.Errorf("cliff lands on %s, want a.go::Verify", got)
	}
}

// A frame that descends and never comes back leaves the landscape open, and
// that must be surfaced rather than smoothed over.
func TestUnclosedPathIsReported(t *testing.T) {
	ev := []Event{
		call("a.go::Verify", "1", "", 0, 0),
		call("a.go::lookup", "2", "1", 1, 100),
		ret("a.go::lookup", "2", 1, 200, "x"),
	}
	l := Derive(ev, testBundle())
	if l.Closed {
		t.Fatal("path reported closed, but Verify never returned")
	}
	if l.OpenFrame != "a.go::Verify" {
		t.Errorf("open frame = %q", l.OpenFrame)
	}
	var mentioned bool
	for _, n := range l.Notes() {
		if strings.Contains(n, "does not close") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Errorf("notes do not surface the open path: %v", l.Notes())
	}
}

func TestEscapedPanicStillDrawsTheCliff(t *testing.T) {
	ev := []Event{
		call("a.go::check", "1", "", 0, 0),
		call("a.go::MustGet", "2", "1", 1, 100),
		raise("a.go::MustGet", "2", 1, 200, "boom"),
		raise("a.go::check", "1", 0, 210, "boom"),
	}
	l := Derive(ev, testBundle())
	if l.Escaped == "" {
		t.Error("a panic that nothing caught should be recorded as escaped")
	}
	last := l.Wells[len(l.Wells)-1]
	if last.Phase != "escape" || last.Depth != 0 {
		t.Errorf("escape well = %+v, want phase escape at depth 0", last)
	}
}

// Barrier height is log-scaled: a 340ms network hop must dwarf a 2µs call
// without a 100µs call vanishing entirely.
func TestLogNormKeepsSmallBarriersVisible(t *testing.T) {
	const minC, maxC = int64(2000), int64(340_000_000)
	small := logNorm(2_000, minC, maxC)
	mid := logNorm(100_000, minC, maxC)
	big := logNorm(340_000_000, minC, maxC)
	if !(small < mid && mid < big) {
		t.Fatalf("not monotonic: %v %v %v", small, mid, big)
	}
	if mid < 0.2 {
		t.Errorf("a 100µs call vanished: height %v", mid)
	}
	if big < 0.95 {
		t.Errorf("the slowest barrier should be at the top: %v", big)
	}
	if small < 0.05 {
		t.Errorf("heights are clamped above 0.05, got %v", small)
	}
}

func TestChainSelection(t *testing.T) {
	events := []Event{
		// chain 1: three frames, fast
		call("a.go::Verify", "1", "", 0, 0),
		call("a.go::check", "2", "1", 1, 10),
		ret("a.go::check", "2", 1, 20, ""),
		ret("a.go::Verify", "1", 0, 30, ""),
		// chain 2: two frames but slow, and it raises
		call("a.go::MustGet", "3", "", 0, 1000),
		raise("a.go::MustGet", "3", 0, 900_000, "boom"),
	}
	b := testBundle()
	if got := DeriveChain(events, b, ChainHottest); got.Wells[0].Symbol != "a.go::Verify" {
		t.Errorf("hottest picked %s", got.Wells[0].Symbol)
	}
	if got := DeriveChain(events, b, ChainSlowest); got.Wells[0].Symbol != "a.go::MustGet" {
		t.Errorf("slowest picked %s", got.Wells[0].Symbol)
	}
	if got := DeriveChain(events, b, ChainRaising); got.Wells[0].Symbol != "a.go::MustGet" {
		t.Errorf("raising picked %s", got.Wells[0].Symbol)
	}
}

// An expensive barrier with no call-site comment is a first-class finding.
func TestUnannotatedExpensiveBarrierIsSurfaced(t *testing.T) {
	b := testBundle()
	ev := []Event{
		call("a.go::Verify", "1", "", 0, 0),
		call("a.go::lookup", "2", "1", 1, 500_000_000),
		ret("a.go::lookup", "2", 1, 500_000_100, ""),
		ret("a.go::Verify", "1", 0, 500_000_200, ""),
	}
	l := Derive(ev, b)
	if len(l.UnannotatedExpensive()) == 0 {
		t.Fatal("a 500ms unexplained call should be surfaced")
	}

	// Now annotate the call site: the finding must disappear.
	b.Symbols[0].CallSites = []bundle.CallSite{{
		Callee: "a.go::lookup", CalleeRaw: "lookup", Line: 10,
		Rationale: "the local map is authoritative, so this is the only remote hop",
	}}
	l = Derive(ev, b)
	if got := l.UnannotatedExpensive(); len(got) != 0 {
		t.Errorf("annotated barrier still reported: %v", got)
	}
}
