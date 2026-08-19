package pyast

import (
	"strings"
	"testing"

	"github.com/kelalaike/plum/internal/bundle"
)

const src = `"""Token cache."""

import os
import threading
import urllib.request

HITS = 0
LOOKUPS = {}
MAX_RETRIES = 3


class Cache:
    """Holds tokens keyed by user id."""

    def __init__(self, ttl):
        self.entries = {}

    def get(self, key, opts=None):
        """Return the token for a key, or None."""
        global HITS
        HITS += 1
        # the realm suffix is applied on the way out so callers never see a raw token
        return self.decorate(self.entries.get(key))

    def decorate(self, v):
        return "%s@%s" % (v, os.environ["AUTH_REALM"])

    def _private(self):
        pass

    def refresh(self, key, retries=[]):
        """Re-fetch a token."""
        try:
            resp = urllib.request.urlopen("https://idp.example.com/" + key)
        except Exception:
            pass
        threading.Thread(target=lambda: None).start()
        return "refreshed"


def top_level(a: int, *, flag: bool = False) -> str:
    try:
        return str(a)
    except:
        return ""
`

func adapter(t *testing.T) *Adapter {
	t.Helper()
	a := New()
	if a == nil {
		t.Skip("no python3 interpreter available")
	}
	return a
}

func symbols(t *testing.T) map[bundle.SymbolID]bundle.Symbol {
	t.Helper()
	out, err := adapter(t).ParseSymbols("app/cache.py", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	m := map[bundle.SymbolID]bundle.Symbol{}
	for _, s := range out {
		m[s.ID] = s
	}
	return m
}

func TestSymbolsAndKinds(t *testing.T) {
	m := symbols(t)
	for id, want := range map[bundle.SymbolID]string{
		"app/cache.py::Cache":          "class",
		"app/cache.py::Cache.get":      "method",
		"app/cache.py::Cache.decorate": "method",
		"app/cache.py::top_level":      "func",
		"app/cache.py::HITS":           "var",
		"app/cache.py::MAX_RETRIES":    "const",
		"app/cache.py::LOOKUPS":        "var",
	} {
		s, ok := m[id]
		if !ok {
			t.Fatalf("missing %s", id)
		}
		if s.Kind != want {
			t.Errorf("%s kind = %q, want %q", id, s.Kind, want)
		}
	}
	if m["app/cache.py::Cache._private"].Exported {
		t.Error("a leading underscore means not exported")
	}
}

// The whole point of parsing with Python rather than with regexes: signatures
// that are exact, including defaults, keyword-only markers and annotations.
func TestSignaturesAreExact(t *testing.T) {
	m := symbols(t)
	for id, want := range map[bundle.SymbolID]string{
		"app/cache.py::Cache.get": "def get(self, key, opts=None)",
		"app/cache.py::top_level": "def top_level(a: int, *, flag: bool=False) -> str",
	} {
		if got := m[id].Signature; got != want {
			t.Errorf("%s signature = %q, want %q", id, got, want)
		}
	}
}

func TestDocstringsAndCallSiteComments(t *testing.T) {
	m := symbols(t)
	if got := m["app/cache.py::Cache.get"].Doc; got != "Return the token for a key, or None." {
		t.Errorf("docstring = %q", got)
	}
	if got := m["app/cache.py::Cache"].Doc; got != "Holds tokens keyed by user id." {
		t.Errorf("class docstring = %q", got)
	}
	// The comment above a call says why it is being called here — the thing that
	// is missing when you read agent-written code (spec §9.4).
	var found bool
	for _, cs := range m["app/cache.py::Cache.get"].CallSites {
		if cs.CalleeRaw == "self.decorate" {
			found = true
			if !strings.Contains(cs.Rationale, "realm suffix is applied on the way out") {
				t.Errorf("call-site rationale = %q", cs.Rationale)
			}
			if cs.Callee != "app/cache.py::Cache.decorate" {
				t.Errorf("intra-file callee not resolved: %s", cs.Callee)
			}
		}
	}
	if !found {
		t.Error("no call site recorded for self.decorate")
	}
}

func TestFingerprintIgnoresFormattingAndDocstringsButNotLogic(t *testing.T) {
	a := adapter(t)
	fp := func(body string) string {
		syms, err := a.ParseSymbols("x.py", []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range syms {
			if s.Name == "f" {
				return s.Fingerprint
			}
		}
		t.Fatalf("no symbol f in %q", body)
		return ""
	}
	base := "def f(x):\n    return x + 1\n"
	reformatted := "def f(x):\n    # a new comment\n\n    return x  +  1\n"
	documented := "def f(x):\n    \"\"\"Now with a docstring.\"\"\"\n    return x + 1\n"
	changed := "def f(x):\n    return x + 2\n"
	renamed := "def f(y):\n    return y + 1\n"

	if fp(base) != fp(reformatted) {
		t.Error("reformatting and comments must not move a fingerprint")
	}
	if fp(base) != fp(documented) {
		t.Error("adding a docstring must not move a fingerprint: prose about code is not code")
	}
	if fp(base) == fp(changed) {
		t.Error("a logic change must move the fingerprint")
	}
	if fp(base) == fp(renamed) {
		t.Error("a rename must move the fingerprint")
	}
}

func TestRiskMarkers(t *testing.T) {
	a := adapter(t)
	syms, err := a.ParseSymbols("app/cache.py", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	marks, err := a.RiskMarkers("app/cache.py", []byte(src), syms)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, m := range marks {
		kinds[m.Kind] = true
	}
	for _, want := range []string{
		"module_level_state",      // HITS is rebound with `global`
		"swallowed_error",         // except Exception: pass, spanning two lines
		"bare_except",             //
		"unsynchronised_thread",   //
		"network_without_timeout", // urlopen with no timeout=
		"mutable_default_arg",     // retries=[]
	} {
		if !kinds[want] {
			t.Errorf("missing risk marker %q (got %v)", want, keys(kinds))
		}
	}
	// A constant assigned once is not shared mutable state.
	for _, m := range marks {
		if m.Symbol == "app/cache.py::MAX_RETRIES" {
			t.Error("a constant was flagged as module-level state")
		}
	}
}

// A method on an exported class is reachable surface: a signature change there
// is what silently breaks callers nobody looked at.
func TestPublicSurfaceIncludesMethodsAndEnvVars(t *testing.T) {
	items, err := adapter(t).PublicSurface("app/cache.py", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string][]string{}
	for _, i := range items {
		byKind[i.Kind] = append(byKind[i.Kind], i.Name)
	}
	for _, want := range []string{"app.cache.Cache", "app.cache.Cache.get", "app.cache.top_level"} {
		if !contains(byKind["export"], want) {
			t.Errorf("missing export %q (got %v)", want, byKind["export"])
		}
	}
	if contains(byKind["export"], "app.cache.Cache._private") {
		t.Error("an underscored method is not public surface")
	}
}

func TestSyntaxErrorIsReportedNotSwallowed(t *testing.T) {
	if _, err := adapter(t).ParseSymbols("bad.py", []byte("def broken(:\n")); err == nil {
		t.Error("a file that does not parse should say so rather than silently yielding nothing")
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

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
