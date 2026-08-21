package probe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/lang"
	"github.com/k3-mt/plum/internal/lang/gopkg"
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

// The window can only point at what it can find, so what counts as a test is
// worth pinning. Each language's convention differs, and a shared rule would be
// wrong in both directions.
func TestWhatCountsAsATest(t *testing.T) {
	cases := []struct {
		lang, name, kind string
		want             bool
	}{
		{"go", "TestThing", "func", true},
		{"go", "BenchmarkThing", "func", true},
		{"go", "FuzzThing", "func", true},
		// The character after the prefix has to start a new word, or every
		// helper with an unlucky name becomes a test the window offers to run.
		{"go", "TestingHelper", "func", false},
		{"go", "Testify", "func", false},
		{"go", "helper", "func", false},
		{"go", "Server.TestThing", "method", false},
		{"python", "test_evicts", "func", true},
		{"python", "helper", "func", false},
		{"typescript", "testEvicts", "func", true},
		{"typescript", "render", "func", false},
		{"go", "TestThing", "type", false},
	}
	for _, c := range cases {
		got := isTestName(c.lang, bundle.Symbol{Name: c.name, Kind: c.kind})
		if got != c.want {
			t.Errorf("isTestName(%s, %s/%s) = %v, want %v", c.lang, c.name, c.kind, got, c.want)
		}
	}
}

func TestDiscoveryFindsTestsAndPairsThemWithTheirHandles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := `package pkg

import "testing"

func TestOne(t *testing.T) {}
func TestTwo(t *testing.T) {}
func helper() {}
`
	if err := os.WriteFile(filepath.Join(root, "pkg", "a_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	// Not a test file, so nothing in it is offered however it is named.
	if err := os.WriteFile(filepath.Join(root, "pkg", "a.go"), []byte("package pkg\n\nfunc TestLookalike() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Mint(root, "TestOne", "go test -run x ./pkg/", ""); err != nil {
		t.Fatal(err)
	}

	found, err := Discover(root, lang.NewRegistry(gopkg.New()), "go test ./...")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("found %d tests, want TestOne and TestTwo: %+v", len(found), found)
	}
	if found[0].Name != "TestOne" || found[1].Name != "TestTwo" {
		t.Errorf("got %s, %s", found[0].Name, found[1].Name)
	}
	if found[0].Handle == "" {
		t.Error("a test already minted should carry its handle — it is the one you watched before")
	}
	if found[1].Handle != "" {
		t.Error("a test never minted has no handle to show")
	}
	if !strings.Contains(found[0].Command, "./pkg/") || !strings.Contains(found[0].Command, "-count=1") {
		t.Errorf("command = %q, want it narrowed to the package and uncached", found[0].Command)
	}
}

// Probes are committed. Re-selecting a test in the window must not show up as a
// diff on a timestamp nobody asked to change.
func TestReMintingAnUnchangedProbeDoesNotTouchTheFile(t *testing.T) {
	root := t.TempDir()
	first, err := Mint(root, "TestThing", "go test ./...", "")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, dir, first.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Mint(root, "TestThing", "go test ./...", ""); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, dir, first.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("the file churned:\n%s\nbecame\n%s", before, after)
	}
	// A real change still lands.
	if _, err := Mint(root, "TestThing", "go test -count=1 ./pkg/", ""); err != nil {
		t.Fatal(err)
	}
	got, err := Load(root, first.Handle())
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != "go test -count=1 ./pkg/" {
		t.Errorf("command = %q, want the new one", got.Command)
	}
	if !got.Created.Equal(first.Created) {
		t.Error("the original creation time should survive an update")
	}
}
