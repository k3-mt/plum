package js

import (
	"strings"
	"testing"

	"github.com/kelalaike/plum/internal/bundle"
)

const src = `'use strict';

const MAX = 10;
let hits = 0;

// Cache holds tokens keyed by user id.
class Cache {
  constructor(ttl) {
    this.entries = new Map();
  }

  // get returns the token for a key, or undefined.
  get(key, opts) {
    // the realm suffix is applied on the way out
    return this.decorate(this.lookup(key));
  }

  lookup(key) {
    return this.entries.get(key);
  }

  async refresh(key) {
    try {
      await fetch('https://idp.example.com/' + key);
    } catch (e) {
    }
    return 'refreshed';
  }

  _private() {
    return 1;
  }
}

function makeCache(ttl) {
  return new Cache(ttl);
}

const helper = (a, b) => a + b;

module.exports = { Cache, makeCache };
`

func symbols(t *testing.T) map[bundle.SymbolID]bundle.Symbol {
	t.Helper()
	out, err := New().ParseSymbols("src/cache.js", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	m := map[bundle.SymbolID]bundle.Symbol{}
	for _, s := range out {
		m[s.ID] = s
	}
	return m
}

// A class method is not a top-level declaration, and a `const` inside a method
// body is not a declaration at all. Getting this wrong means the instrumentation
// set is wrong, and nothing downstream can recover.
func TestClassMethodsAreFoundAndQualified(t *testing.T) {
	m := symbols(t)
	for id, want := range map[bundle.SymbolID]string{
		"src/cache.js::Cache":             "class",
		"src/cache.js::Cache.get":         "method",
		"src/cache.js::Cache.lookup":      "method",
		"src/cache.js::Cache.refresh":     "method",
		"src/cache.js::Cache.constructor": "method",
		"src/cache.js::makeCache":         "func",
		"src/cache.js::helper":            "func",
		"src/cache.js::MAX":               "const",
		"src/cache.js::hits":              "var",
	} {
		s, ok := m[id]
		if !ok {
			t.Fatalf("missing %s", id)
		}
		if s.Kind != want {
			t.Errorf("%s kind = %q, want %q", id, s.Kind, want)
		}
	}
	// A local inside a method body must never surface as a declaration.
	for id := range m {
		if strings.HasSuffix(string(id), "::Cache.v") || strings.HasSuffix(string(id), "::v") {
			t.Errorf("a local variable leaked into the symbol table: %s", id)
		}
	}
}

func TestLineSpansCoverTheWholeBody(t *testing.T) {
	get := symbols(t)["src/cache.js::Cache.get"]
	if get.LineStart >= get.LineEnd {
		t.Fatalf("get spans %d..%d", get.LineStart, get.LineEnd)
	}
	refresh := symbols(t)["src/cache.js::Cache.refresh"]
	if refresh.LineEnd-refresh.LineStart < 5 {
		t.Errorf("refresh spans %d..%d, too short to include its try/catch", refresh.LineStart, refresh.LineEnd)
	}
}

func TestDocsAndCallSiteRationale(t *testing.T) {
	m := symbols(t)
	if got := m["src/cache.js::Cache.get"].Doc; !strings.Contains(got, "returns the token") {
		t.Errorf("doc = %q", got)
	}
	if got := m["src/cache.js::Cache"].Doc; !strings.Contains(got, "holds tokens") && !strings.Contains(got, "Cache holds") {
		t.Errorf("class doc = %q", got)
	}
	var found bool
	for _, cs := range m["src/cache.js::Cache.get"].CallSites {
		if cs.CalleeRaw == "this.decorate" {
			found = true
			if !strings.Contains(cs.Rationale, "realm suffix") {
				t.Errorf("call-site rationale = %q", cs.Rationale)
			}
		}
	}
	if !found {
		t.Errorf("no call site for this.decorate")
	}
}

func TestExportedSurfaceIncludesCommonJSAndMethods(t *testing.T) {
	items, err := New().PublicSurface("src/cache.js", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, i := range items {
		if i.Kind == "export" {
			names = append(names, i.Name)
		}
	}
	for _, want := range []string{"src.cache.Cache", "src.cache.Cache.get", "src.cache.makeCache"} {
		if !contains(names, want) {
			t.Errorf("missing export %q (got %v)", want, names)
		}
	}
	if contains(names, "src.cache.Cache._private") {
		t.Error("an underscored method is not public surface")
	}
	if contains(names, "src.cache.helper") {
		t.Error("helper is not exported by module.exports and must not be surface")
	}
}

func TestRiskMarkers(t *testing.T) {
	a := New()
	syms, err := a.ParseSymbols("src/cache.js", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	marks, err := a.RiskMarkers("src/cache.js", []byte(src), syms)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, m := range marks {
		kinds[m.Kind] = true
	}
	for _, want := range []string{"swallowed_error", "network_without_timeout", "module_level_state"} {
		if !kinds[want] {
			t.Errorf("missing %q (got %v)", want, keysOf(kinds))
		}
	}
	// const MAX is not mutable state.
	for _, m := range marks {
		if m.Kind == "module_level_state" && strings.Contains(string(m.Symbol), "MAX") {
			t.Error("a const was flagged as module-level state")
		}
	}
}

// Braces and identifiers inside strings and comments must not move the scanner,
// or every span after them is wrong.
func TestScannerIgnoresBracesInStringsAndComments(t *testing.T) {
	tricky := `class A {
  // } this brace is in a comment
  m() {
    const s = "} not a close brace {";
    const t = ` + "`" + `template } with { braces` + "`" + `;
    return s + t;
  }
  after() {
    return 1;
  }
}
`
	m := map[string]bundle.Symbol{}
	syms, err := New().ParseSymbols("x.js", []byte(tricky))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range syms {
		m[s.Name] = s
	}
	if _, ok := m["A.after"]; !ok {
		t.Fatalf("a brace inside a string or comment broke the scan: got %v", names(syms))
	}
	if m["A.m"].LineEnd >= m["A.after"].LineStart {
		t.Errorf("m spans %d..%d, overlapping after at %d", m["A.m"].LineStart, m["A.m"].LineEnd, m["A.after"].LineStart)
	}
}

func TestFingerprintIgnoresCommentsButNotLogic(t *testing.T) {
	a := New()
	fp := func(body string) string {
		syms, err := a.ParseSymbols("x.js", []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range syms {
			if s.Name == "f" {
				return s.Fingerprint
			}
		}
		t.Fatalf("no f in %q", body)
		return ""
	}
	base := "function f(x) {\n  return x + 1;\n}\n"
	recommented := "function f(x) {\n  // explains itself now\n  return x  +  1;\n}\n"
	changed := "function f(x) {\n  return x + 2;\n}\n"

	if fp(base) != fp(recommented) {
		t.Error("comments and spacing must not move a fingerprint")
	}
	if fp(base) == fp(changed) {
		t.Error("a logic change must move the fingerprint")
	}
}

func TestShimSpecIsDeclarative(t *testing.T) {
	spec, err := New().ShimSpec([]bundle.SymbolID{"src/cache.js::Cache.get"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Mode != "env" {
		t.Errorf("mode = %q", spec.Mode)
	}
	if _, ok := spec.Files["plum-shim.cjs"]; !ok {
		t.Error("the shim source must travel with the spec, or the collector cannot write it")
	}
	if !strings.Contains(spec.Env["NODE_OPTIONS"], "${SHIM_DIR}") {
		t.Errorf("NODE_OPTIONS = %q — it must reference the substituted shim dir", spec.Env["NODE_OPTIONS"])
	}
	if spec.Env["PLUM_SYMBOLS"] != "${SYMBOLS}" {
		t.Errorf("the instrumentation set must reach the shim: %q", spec.Env["PLUM_SYMBOLS"])
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func keysOf(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func names(syms []bundle.Symbol) []string {
	var out []string
	for _, s := range syms {
		out = append(out, s.Name)
	}
	return out
}
