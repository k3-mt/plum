package met

import (
	"fmt"
	"os"
	"testing"

	"github.com/k3-mt/plum/internal/bundle"
)

func b(syms ...bundle.Symbol) *bundle.Bundle { return &bundle.Bundle{Symbols: syms} }

func sym(id, fp string) bundle.Symbol {
	return bundle.Symbol{ID: bundle.SymbolID(id), Fingerprint: fp}
}

// The whole point of keying on a fingerprint rather than a boolean: having read
// Get last week says nothing about the Get that exists now, and a meter that
// counted it as met would be reporting the reader's memory rather than the code.
func TestMeetingASymbolDoesNotCoverALaterVersionOfIt(t *testing.T) {
	s := Load(t.TempDir())
	before := b(sym("a.go::Get", "fp1"), sym("a.go::Put", "fp1"))

	if d := s.Of(before, nil); d.Unmet != 2 || d.Total != 2 || d.Stale != 0 {
		t.Fatalf("fresh checkout: %+v, want 2 unmet of 2, none stale", d)
	}
	if err := s.Meet("a.go::Get", "fp1"); err != nil {
		t.Fatal(err)
	}
	if d := s.Of(before, nil); d.Unmet != 1 || d.Stale != 0 {
		t.Fatalf("after reading Get: %+v, want 1 unmet", d)
	}

	// The agent rewrites Get. The debt comes back, and it is a different kind of
	// debt: code that changed under the reader, not code they never saw.
	after := b(sym("a.go::Get", "fp2"), sym("a.go::Put", "fp1"))
	d := s.Of(after, nil)
	if d.Unmet != 2 {
		t.Errorf("unmet = %d, want both again", d.Unmet)
	}
	if d.Stale != 1 {
		t.Errorf("stale = %d, want Get alone — Put was never read at any version", d.Stale)
	}
}

func TestMeetAllClearsTheChangedSetAndSurvivesAReload(t *testing.T) {
	dir := t.TempDir()
	s := Load(dir)
	bun := b(sym("a.go::Get", "fp1"), sym("a.go::Put", "fp1"))
	if err := s.MeetAll(bun); err != nil {
		t.Fatal(err)
	}
	if d := s.Of(bun, nil); d.Unmet != 0 {
		t.Fatalf("after meeting the lot: %+v", d)
	}
	// The debt outlives the window. Reopening tomorrow must not start from zero.
	if d := Load(dir).Of(bun, nil); d.Unmet != 0 {
		t.Errorf("reloaded from disk: %+v, want the debt still paid", d)
	}
}

// A first capture can name tens of thousands of symbols. The page draws a few
// dozen frames, and only needs to know which of those to render hollow.
func TestFramesNamesOnlyWhatTheLandscapeDraws(t *testing.T) {
	s := Load(t.TempDir())
	bun := b(sym("a.go::Get", "fp1"), sym("a.go::Put", "fp1"), sym("a.go::Del", "fp1"))
	d := s.Of(bun, []bundle.SymbolID{"a.go::Get"})
	if len(d.Frames) != 1 || d.Frames[0] != "a.go::Get" {
		t.Errorf("frames = %v, want the drawn symbol alone", d.Frames)
	}
	if d.Unmet != 3 {
		t.Errorf("unmet = %d — the count is over the whole changed set, not the drawing", d.Unmet)
	}
}

// A symbol with no fingerprint cannot be compared against anything, so counting
// it either way would be a guess. It is left out of the total, not called met.
func TestSymbolsWithoutAFingerprintAreNotCounted(t *testing.T) {
	s := Load(t.TempDir())
	if d := s.Of(b(sym("a.go::Get", "")), nil); d.Total != 0 || d.Unmet != 0 {
		t.Errorf("%+v, want an empty measurement", d)
	}
}

func TestLoadOnAnUnreadableFileMeansNothingMet(t *testing.T) {
	dir := t.TempDir()
	s := Load(dir)
	if err := s.Meet("a.go::Get", "fp1"); err != nil {
		t.Fatal(err)
	}
	if err := writeGarbage(s.path); err != nil {
		t.Fatal(err)
	}
	// Nothing met is the truthful reading, and it must not stop a window opening.
	if d := Load(dir).Of(b(sym("a.go::Get", "fp1")), nil); d.Unmet != 1 {
		t.Errorf("%+v, want the symbol counted unmet again", d)
	}
}

func writeGarbage(path string) error { return os.WriteFile(path, []byte("{not json"), 0o644) }

// "I have met this code" is a claim. The quiz is where it gets checked against a
// recorded execution, so a missed question has to put the debt back on — a meter
// that only ever went down would be measuring clicks, not comprehension.
func TestAMissedQuestionPutsTheDebtBackOn(t *testing.T) {
	dir := t.TempDir()
	s := Load(dir)
	bun := b(sym("a.go::Get", "fp1"), sym("a.go::Put", "fp1"))
	if err := s.MeetAll(bun); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget("a.go::Get"); err != nil {
		t.Fatal(err)
	}
	d := s.Of(bun, nil)
	if d.Unmet != 1 {
		t.Errorf("%+v, want the missed symbol owed again", d)
	}
	// And it must not come back on the next window either.
	if d := Load(dir).Of(bun, nil); d.Unmet != 1 {
		t.Errorf("reloaded: %+v — forgetting has to survive a restart", d)
	}
}

func TestMeetInTakesTheFingerprintFromTheBundle(t *testing.T) {
	s := Load(t.TempDir())
	bun := b(sym("a.go::Get", "fp1"))
	if err := s.MeetIn(bun, "a.go::Get"); err != nil {
		t.Fatal(err)
	}
	if d := s.Of(bun, nil); d.Unmet != 0 {
		t.Errorf("%+v, want it met at the version the bundle holds", d)
	}
}

// A symbol the bundle never captured has no fingerprint to be met at. Recording
// one anyway would write an entry that can never match anything.
func TestMeetInIgnoresASymbolTheBundleDoesNotHold(t *testing.T) {
	s := Load(t.TempDir())
	bun := b(sym("a.go::Get", "fp1"))
	if err := s.MeetIn(bun, "a.go::Nowhere"); err != nil {
		t.Fatal(err)
	}
	if d := s.Of(bun, nil); d.Unmet != 1 {
		t.Errorf("%+v", d)
	}
}

// A landscape draws one symbol as several wells: descending into it, and again
// on each resume. Six of the thirteen frames on a real chain here were the same
// function. The page wants a set of symbols to render hollow, not a list of the
// frames they were drawn as.
func TestASymbolDrawnAsSeveralFramesIsNamedOnce(t *testing.T) {
	s := Load(t.TempDir())
	bun := b(sym("a.go::Get", "fp1"), sym("a.go::Put", "fp1"))
	drawn := []bundle.SymbolID{"a.go::Get", "a.go::Put", "a.go::Get", "a.go::Get"}
	d := s.Of(bun, drawn)
	if len(d.Frames) != 2 {
		t.Errorf("frames = %v, want each unmet symbol once", d.Frames)
	}
	if d.Unmet != 2 {
		t.Errorf("unmet = %d — deduping the drawing must not touch the count", d.Unmet)
	}
}

// The landscape draws one chain out of however many were recorded, so most of a
// session's debt is never on the picture. The worklist is what makes the meter
// something you can act on rather than only read, and its order is the report's:
// what could break other people first, source order never.
func TestTheWorklistLeadsWithWhatBreaksOtherPeople(t *testing.T) {
	bun := &bundle.Bundle{
		Symbols: []bundle.Symbol{
			{ID: "z.go::plain", Name: "plain", Fingerprint: "fp", Tested: true},
			// Exported, new, and still last: test code never breaks anyone else.
			{ID: "a_test.go::TestIt", Name: "TestIt", File: "a_test.go", Fingerprint: "fp", Exported: true, Change: "added"},
			{ID: "a.go::Untested", Name: "Untested", Fingerprint: "fp"},
			{ID: "a.go::NewExport", Name: "NewExport", Fingerprint: "fp", Exported: true, Change: "added", Tested: true},
			{ID: "a.go::Risky", Name: "Risky", Fingerprint: "fp", Tested: true},
			{ID: "a.go::Changed", Name: "Changed", Fingerprint: "fp", Exported: true, Change: "modified", Tested: true},
		},
		RiskMarkers: []bundle.RiskMarker{{Symbol: "a.go::Risky", Kind: "swallowed_error"}},
	}
	got := []string{}
	for _, it := range Load(t.TempDir()).Of(bun, nil).Worklist {
		got = append(got, it.Name)
	}
	want := []string{"Changed", "Risky", "NewExport", "Untested", "plain", "TestIt"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("worklist = %v, want %v", got, want)
		}
	}
}

// A list nobody finishes reading is a list that found nothing. What is held back
// is counted and named rather than silently cut.
func TestTheWorklistIsCappedAndSaysWhatItHeldBack(t *testing.T) {
	var syms []bundle.Symbol
	for i := 0; i < worklistMax+7; i++ {
		syms = append(syms, bundle.Symbol{
			ID:          bundle.SymbolID(fmt.Sprintf("a.go::S%03d", i)),
			Name:        fmt.Sprintf("S%03d", i),
			Fingerprint: "fp", Tested: true,
		})
	}
	d := Load(t.TempDir()).Of(&bundle.Bundle{Symbols: syms}, nil)
	if len(d.Worklist) != worklistMax {
		t.Errorf("worklist = %d, want the cap of %d", len(d.Worklist), worklistMax)
	}
	if d.More != 7 {
		t.Errorf("more = %d, want the 7 held back named", d.More)
	}
}

// Meeting a symbol takes it off the list. Otherwise the worklist would still be
// telling you to read what you just read.
func TestReadingSomethingTakesItOffTheWorklist(t *testing.T) {
	s := Load(t.TempDir())
	bun := b(sym("a.go::Get", "fp1"), sym("a.go::Put", "fp1"))
	if err := s.Meet("a.go::Get", "fp1"); err != nil {
		t.Fatal(err)
	}
	d := s.Of(bun, nil)
	if len(d.Worklist) != 1 || d.Worklist[0].Symbol != "a.go::Put" {
		t.Errorf("worklist = %+v, want only the one still owed", d.Worklist)
	}
}
