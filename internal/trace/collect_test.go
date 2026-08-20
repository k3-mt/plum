package trace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kelalaike/plum/internal/bundle"
)

// fakeAdapter stands in for a language the engine has never heard of. If the
// collector can instrument it, the seam is real: adding Ruby means writing an
// adapter, not editing the engine.
type fakeAdapter struct {
	name string
	exts []string
	spec ShimSpec
	err  error
}

func (f *fakeAdapter) Name() string         { return f.name }
func (f *fakeAdapter) Extensions() []string { return f.exts }
func (f *fakeAdapter) ShimSpec(syms []bundle.SymbolID) (ShimSpec, error) {
	if f.err != nil {
		return ShimSpec{}, f.err
	}
	s := f.spec
	s.Symbols = syms
	return s, nil
}

// rewritingAdapter also implements Rewriter, the way the Go adapter does.
type rewritingAdapter struct {
	fakeAdapter
	touched string
	context []bundle.SymbolID
}

func (r *rewritingAdapter) Instrument(scratchRoot string, ids, context []bundle.SymbolID) (Instrumented, error) {
	r.touched = scratchRoot
	r.context = context
	return Instrumented{Done: ids, Env: map[string]string{"REWRITTEN": "1"}}, nil
}

func repo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.rb"), []byte("# ruby\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func bundleWith(ids ...bundle.SymbolID) *bundle.Bundle {
	b := &bundle.Bundle{Session: bundle.Session{ID: "s1"}}
	for _, id := range ids {
		b.Symbols = append(b.Symbols, bundle.Symbol{
			ID: id, Kind: "func", Name: id.Qualified(), File: id.File(), Change: "added",
		})
	}
	return b
}

// The whole point of the ShimSpec redesign: files and environment travel in the
// spec, and the collector honours them without knowing what language it is.
func TestEnvShimIsHonouredDeclaratively(t *testing.T) {
	root := repo(t)
	scratch := filepath.Join(t.TempDir(), "scratch")
	adapter := &fakeAdapter{
		name: "ruby", exts: []string{".rb"},
		spec: ShimSpec{
			Language: "ruby", Mode: "env", Dir: ".plum-shim-ruby",
			Files: map[string]string{"shim.rb": "# the shim\n", "nested/more.rb": "# nested\n"},
			Env: map[string]string{
				"RUBYOPT":      "-r${SHIM_DIR}/shim.rb",
				"PLUM_SYMBOLS": "${SYMBOLS}",
			},
			PathVars: []string{"RUBYLIB"},
		},
	}
	c := &Collector{
		Root: root, Scratch: scratch, MaxEvents: 100,
		Adapters: []Instrumenter{adapter},
		// Report the environment the shim would have seen, so the test can
		// assert on it without a real runtime.
		TestCommand: "env > env.txt",
	}
	res, err := c.Run(context.Background(), bundleWith("a.rb::Cache.get", "a.rb::Cache.put"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Instrumented) != 2 {
		t.Fatalf("instrumented %v", res.Instrumented)
	}

	for _, name := range []string{"shim.rb", "nested/more.rb"} {
		if _, err := os.Stat(filepath.Join(scratch, ".plum-shim-ruby", name)); err != nil {
			t.Errorf("declared file %s was not written: %v", name, err)
		}
	}
	envText, err := os.ReadFile(filepath.Join(scratch, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	env := string(envText)
	shimDir := filepath.Join(scratch, ".plum-shim-ruby")
	if !strings.Contains(env, "RUBYOPT=-r"+shimDir+"/shim.rb") {
		t.Errorf("${SHIM_DIR} was not substituted:\n%s", grepEnv(env, "RUBYOPT"))
	}
	if !strings.Contains(env, "PLUM_SYMBOLS=a.rb::Cache.get,a.rb::Cache.put") {
		t.Errorf("${SYMBOLS} was not substituted:\n%s", grepEnv(env, "PLUM_SYMBOLS"))
	}
	if !strings.Contains(env, "RUBYLIB="+shimDir) {
		t.Errorf("path var not set:\n%s", grepEnv(env, "RUBYLIB"))
	}
	if !strings.Contains(env, "PLUM_TRACE_OUT=") || !strings.Contains(env, "PLUM_TRACE=1") {
		t.Error("the shim contract variables must always be set")
	}
}

// A path variable the project already relies on must be prepended to, never
// replaced — otherwise the shim breaks the very run it is observing.
func TestPathVarsArePrependedNotReplaced(t *testing.T) {
	t.Setenv("RUBYLIB", "/somewhere/the/project/needs")
	root := repo(t)
	scratch := filepath.Join(t.TempDir(), "scratch")
	c := &Collector{
		Root: root, Scratch: scratch, MaxEvents: 100,
		TestCommand: "env > env.txt",
		Adapters: []Instrumenter{&fakeAdapter{
			name: "ruby", exts: []string{".rb"},
			spec: ShimSpec{Mode: "env", Dir: "shimdir", PathVars: []string{"RUBYLIB"}},
		}},
	}
	if _, err := c.Run(context.Background(), bundleWith("a.rb::f")); err != nil {
		t.Fatal(err)
	}
	env, _ := os.ReadFile(filepath.Join(scratch, "env.txt"))
	want := filepath.Join(scratch, "shimdir") + string(os.PathListSeparator) + "/somewhere/the/project/needs"
	if !strings.Contains(string(env), "RUBYLIB="+want) {
		t.Errorf("existing RUBYLIB was lost:\n%s", grepEnv(string(env), "RUBYLIB"))
	}
}

func TestRewriteModeDelegatesToTheAdapter(t *testing.T) {
	root := repo(t)
	scratch := filepath.Join(t.TempDir(), "scratch")
	adapter := &rewritingAdapter{fakeAdapter: fakeAdapter{
		name: "ruby", exts: []string{".rb"},
		spec: ShimSpec{Mode: "rewrite"},
	}}
	c := &Collector{
		Root: root, Scratch: scratch, MaxEvents: 100,
		TestCommand: "env > env.txt", Adapters: []Instrumenter{adapter},
		Context: []bundle.SymbolID{"a.rb::surrounding"},
	}
	res, err := c.Run(context.Background(), bundleWith("a.rb::f"))
	if err != nil {
		t.Fatal(err)
	}
	if adapter.touched != scratch {
		t.Errorf("the adapter was handed %q, want the scratch root %q", adapter.touched, scratch)
	}
	if len(adapter.context) != 1 || adapter.context[0] != "a.rb::surrounding" {
		t.Errorf("a rewriting adapter must receive the surrounding set too: %v", adapter.context)
	}
	if len(res.Instrumented) != 1 {
		t.Errorf("instrumented %v", res.Instrumented)
	}
	env, _ := os.ReadFile(filepath.Join(scratch, "env.txt"))
	if !strings.Contains(string(env), "REWRITTEN=1") {
		t.Error("environment returned by a Rewriter must reach the test command")
	}
}

func TestRewriteModeWithoutRewriterIsAnError(t *testing.T) {
	root := repo(t)
	c := &Collector{
		Root: root, Scratch: filepath.Join(t.TempDir(), "scratch"), MaxEvents: 100,
		TestCommand: "true",
		Adapters: []Instrumenter{&fakeAdapter{
			name: "ruby", exts: []string{".rb"}, spec: ShimSpec{Mode: "rewrite"},
		}},
	}
	_, err := c.Run(context.Background(), bundleWith("a.rb::f"))
	if err == nil || !strings.Contains(err.Error(), "trace.Rewriter") {
		t.Errorf("expected a clear error about the missing interface, got %v", err)
	}
}

// Configuration is read, not executed. Mode "none" must be a quiet no-op.
func TestNoneModeInstrumentsNothing(t *testing.T) {
	root := repo(t)
	c := &Collector{
		Root: root, Scratch: filepath.Join(t.TempDir(), "scratch"), MaxEvents: 100,
		TestCommand: "true",
		Adapters: []Instrumenter{&fakeAdapter{
			name: "config", exts: []string{".rb"}, spec: ShimSpec{Mode: "none"},
		}},
	}
	if _, err := c.Run(context.Background(), bundleWith("a.rb::f")); err == nil {
		t.Error("a session where nothing can be instrumented should say so")
	}
}

func TestTestsAndDeletedSymbolsAreNeverInstrumented(t *testing.T) {
	c := &Collector{Adapters: []Instrumenter{&fakeAdapter{name: "js", exts: []string{".js"}}}}
	b := bundleWith("src/a.js::f")
	b.Symbols = append(b.Symbols,
		bundle.Symbol{ID: "src/a.test.js::t", Kind: "func", Name: "t", File: "src/a.test.js", Change: "added"},
		bundle.Symbol{ID: "src/gone.js::g", Kind: "func", Name: "g", File: "src/gone.js", Change: "deleted"},
		bundle.Symbol{ID: "src/a.js::main", Kind: "func", Name: "main", File: "src/a.js", Change: "added"},
	)
	got := c.targets(b)["js"]
	if len(got) != 1 || got[0] != "src/a.js::f" {
		t.Errorf("instrumentation set = %v", got)
	}
}

func grepEnv(env, key string) string {
	for _, line := range strings.Split(env, "\n") {
		if strings.HasPrefix(line, key+"=") {
			return line
		}
	}
	return "(" + key + " not set)"
}

func TestTestsSummaryAndFiltering(t *testing.T) {
	events := []Event{
		{Kind: "call", Symbol: "a.go::f", TestID: "TestOne", Depth: 0},
		{Kind: "call", Symbol: "a.go::g", TestID: "TestOne", Depth: 1},
		{Kind: "return", Symbol: "a.go::g", TestID: "TestOne", Depth: 1},
		{Kind: "call", Symbol: "a.go::f", TestID: "TestTwo", Depth: 0},
		{Kind: "raise", Symbol: "a.go::f", TestID: "TestTwo", Depth: 0},
		{Kind: "call", Symbol: "a.go::h", Depth: 0}, // no test attribution
	}
	runs := Tests(events)
	if len(runs) != 3 {
		t.Fatalf("got %d runs: %+v", len(runs), runs)
	}
	byName := map[string]TestRun{}
	for _, r := range runs {
		byName[r.Name] = r
	}
	one := byName["TestOne"]
	if one.Frames != 2 || one.MaxDepth != 2 || one.Raised {
		t.Errorf("TestOne = %+v", one)
	}
	if two := byName["TestTwo"]; !two.Raised {
		t.Error("a test whose execution raised should say so")
	}
	if _, ok := byName["(no test)"]; !ok {
		t.Error("unattributed events must still be visible, not dropped")
	}

	if got := ForTest(events, "TestOne"); len(got) != 3 {
		t.Errorf("filtering to one test gave %d events", len(got))
	}

	reached := Reached(events)
	if tests := reached["a.go::f"]; len(tests) != 2 || tests[0] != "TestOne" {
		t.Errorf("a.go::f reached by %v, want both tests", tests)
	}
	if _, ok := reached["a.go::h"]; ok {
		t.Error("an unattributed call proves no test reached the symbol")
	}
}

// The surrounding code travels to the shim separately from the changed set, so
// a shim can record it for structure only. Instrumenting it as deeply as the
// change would cost more and say less.
func TestContextSymbolsReachTheShimSeparately(t *testing.T) {
	root := repo(t)
	scratch := filepath.Join(t.TempDir(), "scratch")
	c := &Collector{
		Root: root, Scratch: scratch, MaxEvents: 100,
		TestCommand: "env > env.txt",
		Context:     []bundle.SymbolID{"a.rb::Untouched.helper", "a.rb::alsoUntouched"},
		Adapters: []Instrumenter{&fakeAdapter{
			name: "ruby", exts: []string{".rb"},
			spec: ShimSpec{Mode: "env", Dir: "shimdir", Env: map[string]string{
				"PLUM_SYMBOLS":         "${SYMBOLS}",
				"PLUM_CONTEXT_SYMBOLS": "${CONTEXT_SYMBOLS}",
			}},
		}},
	}
	res, err := c.Run(context.Background(), bundleWith("a.rb::Cache.get"))
	if err != nil {
		t.Fatal(err)
	}
	// Only the changed set counts as instrumented: context is scenery.
	if len(res.Instrumented) != 1 || res.Instrumented[0] != "a.rb::Cache.get" {
		t.Errorf("instrumented = %v", res.Instrumented)
	}
	env, _ := os.ReadFile(filepath.Join(scratch, "env.txt"))
	if !strings.Contains(string(env), "PLUM_SYMBOLS=a.rb::Cache.get\n") {
		t.Errorf("deep set wrong:\n%s", grepEnv(string(env), "PLUM_SYMBOLS"))
	}
	if !strings.Contains(string(env), "PLUM_CONTEXT_SYMBOLS=a.rb::Untouched.helper,a.rb::alsoUntouched") {
		t.Errorf("context set wrong:\n%s", grepEnv(string(env), "PLUM_CONTEXT_SYMBOLS"))
	}
}

func TestContextForRoutesByAdapter(t *testing.T) {
	goAdapter := &fakeAdapter{name: "go", exts: []string{".go"}}
	jsAdapter := &fakeAdapter{name: "js", exts: []string{".js"}}
	c := &Collector{
		Adapters: []Instrumenter{goAdapter, jsAdapter},
		Context:  []bundle.SymbolID{"a.go::f", "b.js::g", "c.rb::h"},
	}
	if got := c.contextFor(goAdapter); len(got) != 1 || got[0] != "a.go::f" {
		t.Errorf("go context = %v", got)
	}
	if got := c.contextFor(jsAdapter); len(got) != 1 || got[0] != "b.js::g" {
		t.Errorf("js context = %v", got)
	}
}
