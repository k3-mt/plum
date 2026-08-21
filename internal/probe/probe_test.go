package probe

import (
	"strings"
	"testing"
)

// A handle is printed by an agent and pasted by a person, possibly days apart.
// If probing the same test twice allocated a second one, the handle somebody
// wrote down last week would quietly stop meaning what it meant.
func TestTheSameTestAlwaysMintsTheSameHandle(t *testing.T) {
	root := t.TempDir()
	a, err := Mint(root, "TestCacheEvicts", "go test -run x ./...", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Mint(root, "TestCacheEvicts", "go test -run y ./...", "fixtures/a.json")
	if err != nil {
		t.Fatal(err)
	}
	if a.Handle() != b.Handle() {
		t.Errorf("%s then %s — re-probing must land on the same handle", a.Handle(), b.Handle())
	}
	// And re-minting updates in place rather than leaving the old command behind.
	got, err := Load(root, a.Handle())
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != "go test -run y ./..." || got.Fixture != "fixtures/a.json" {
		t.Errorf("loaded %+v, want the second mint's details", got)
	}
}

// Somebody will paste one of each.
func TestAHandleLoadsWithOrWithoutItsPrefix(t *testing.T) {
	root := t.TempDir()
	p, err := Mint(root, "TestThing", "go test ./...", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, form := range []string{p.Handle(), p.ID, "  " + p.Handle() + "  "} {
		if _, err := Load(root, form); err != nil {
			t.Errorf("Load(%q): %v", form, err)
		}
	}
	if _, err := Load(root, "plum:nope"); err == nil {
		t.Error("an unknown handle should say so, not return a zero probe")
	}
}

// -count=1 is not a nicety. Go caches a successful result against the content of
// the tree it ran on, so the second run of a probe returns "(cached)" in
// milliseconds, executes nothing, records no events, and the window draws an
// empty picture of a test that passed. This was observed, not theorised.
func TestGoRunsAreAnchoredScopedAndUncached(t *testing.T) {
	got, ok := ScopeCommand("go test ./...", "TestCache", "internal/cache")
	if !ok {
		t.Fatal("go test should be recognised")
	}
	for _, want := range []string{"-count=1", "-run ^TestCache$", "./internal/cache/"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "./...") {
		t.Errorf("%q still builds every package — that was 7.5s against 0.5s", got)
	}
}

// Anchored, or TestCache also runs TestCacheEvictsUnderPressure and the window
// draws a picture of more than it claims.
func TestTheTestNameIsAnchored(t *testing.T) {
	got, _ := ScopeCommand("go test ./...", "TestCache", "")
	if !strings.Contains(got, "^TestCache$") {
		t.Errorf("%q would also match longer names", got)
	}
}

// The failure being avoided here is silent. A "single test" run that quietly
// executed the whole suite would still draw a picture, and the picture would not
// be of what the window says it is showing.
func TestAnUnrecognisedRunnerSaysSoRatherThanGuessing(t *testing.T) {
	got, ok := ScopeCommand("make check", "TestThing", "internal/x")
	if ok {
		t.Error("make check cannot be narrowed; claiming otherwise is the bug")
	}
	if got != "make check" {
		t.Errorf("got %q, want the command untouched", got)
	}
}

func TestPytestAndJestAreNarrowedToo(t *testing.T) {
	if got, ok := ScopeCommand("pytest", "test_evicts", "app/cache"); !ok ||
		!strings.Contains(got, "-k test_evicts") || !strings.Contains(got, "app/cache") {
		t.Errorf("pytest: %q ok=%v", got, ok)
	}
	if got, ok := ScopeCommand("npx vitest run", "evicts", ""); !ok || !strings.Contains(got, "-t evicts") {
		t.Errorf("vitest: %q ok=%v", got, ok)
	}
}
