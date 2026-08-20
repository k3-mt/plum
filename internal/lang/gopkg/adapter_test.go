package gopkg

import (
	"strings"
	"testing"

	"github.com/kelalaike/plum/internal/bundle"
)

const src = `package auth

import (
	"errors"
	"fmt"
	"net/http"
	"os"
)

// ErrMiss is returned when a key is absent.
var ErrMiss = errors.New("miss")

var hits int

// Cache holds tokens.
type Cache struct {
	entries map[string]string
}

// Get returns the token for a key.
func (c *Cache) Get(key string, opts any) (token string, err error) {
	hits++
	// the identity provider is the source of truth once the local map misses
	v, err := c.refresh(key)
	if err != nil {
		return "", fmt.Errorf("get %q: %w", key, err)
	}
	return v, nil
}

func (c *Cache) refresh(key string) (string, error) {
	resp, err := http.Get("https://idp.example/" + key)
	if err != nil {
	}
	_ = resp
	go func() { c.entries[key] = "x" }()
	return os.Getenv("AUTH_REALM"), nil
}

func mustParse(s string) string {
	if s == "" {
		panic("empty")
	}
	return s
}
`

func syms(t *testing.T) map[bundle.SymbolID]bundle.Symbol {
	t.Helper()
	out, err := New().ParseSymbols("internal/auth/cache.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	m := map[bundle.SymbolID]bundle.Symbol{}
	for _, s := range out {
		m[s.ID] = s
	}
	return m
}

func TestParseSymbolsIDsAndKinds(t *testing.T) {
	m := syms(t)
	for id, want := range map[bundle.SymbolID]string{
		"internal/auth/cache.go::Cache.Get":     "method",
		"internal/auth/cache.go::Cache.refresh": "method",
		"internal/auth/cache.go::mustParse":     "func",
		"internal/auth/cache.go::Cache":         "type",
		"internal/auth/cache.go::ErrMiss":       "var",
		"internal/auth/cache.go::hits":          "var",
	} {
		s, ok := m[id]
		if !ok {
			t.Fatalf("missing %s", id)
		}
		if s.Kind != want {
			t.Errorf("%s kind = %q, want %q", id, s.Kind, want)
		}
	}
}

func TestSignatureIsStableAndComplete(t *testing.T) {
	got := syms(t)["internal/auth/cache.go::Cache.Get"].Signature
	want := "func (*Cache) Get(key string, opts any) (token string, err error)"
	if got != want {
		t.Errorf("signature = %q, want %q", got, want)
	}
}

func TestDocAndExportedFlags(t *testing.T) {
	m := syms(t)
	if d := m["internal/auth/cache.go::Cache.Get"].Doc; d != "Get returns the token for a key." {
		t.Errorf("doc = %q", d)
	}
	if m["internal/auth/cache.go::Cache.refresh"].Doc != "" {
		t.Error("unexported refresh should have no doc")
	}
	if !m["internal/auth/cache.go::Cache.Get"].Exported || m["internal/auth/cache.go::Cache.refresh"].Exported {
		t.Error("exported flags are wrong")
	}
}

// The comment above a call says why it is being called here, and that is the
// thing missing when you read agent-written code (spec §9.4).
func TestCallSiteRationaleBindsToTheCallBelowIt(t *testing.T) {
	get := syms(t)["internal/auth/cache.go::Cache.Get"]
	var found bool
	for _, cs := range get.CallSites {
		if cs.CalleeRaw == "c.refresh" {
			found = true
			if !strings.Contains(cs.Rationale, "identity provider is the source of truth") {
				t.Errorf("rationale = %q", cs.Rationale)
			}
		}
	}
	if !found {
		t.Fatal("no call site recorded for c.refresh")
	}
}

func TestFingerprintIgnoresFormattingButNotLogic(t *testing.T) {
	a := New()
	base := "func f(x int) int {\n\treturn x + 1\n}\n"
	reformatted := "func f(x int) int {\n\n\t// a new comment\n\treturn x  +  1\n}\n"
	changed := "func f(x int) int {\n\treturn x + 2\n}\n"
	renamed := "func g(x int) int {\n\treturn x + 1\n}\n"

	fp := func(s string) string {
		n, err := a.Normalise([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return Hash(n)
	}
	if fp(base) != fp(reformatted) {
		t.Error("reformatting must not invalidate a fingerprint")
	}
	if fp(base) == fp(changed) {
		t.Error("a logic change must move the fingerprint")
	}
	if fp(base) == fp(renamed) {
		t.Error("a rename must move the fingerprint")
	}
}

func TestPublicSurfaceFindsExportsEnvVars(t *testing.T) {
	items, err := New().PublicSurface("internal/auth/cache.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string][]string{}
	for _, i := range items {
		kinds[i.Kind] = append(kinds[i.Kind], i.Name)
	}
	if !contains(kinds["export"], "auth.Cache.Get") {
		t.Errorf("exports = %v", kinds["export"])
	}
	if contains(kinds["export"], "auth.Cache.refresh") {
		t.Error("unexported method must not be public surface")
	}
	if !contains(kinds["env_var"], "AUTH_REALM") {
		t.Errorf("env vars = %v", kinds["env_var"])
	}
}

func TestRiskMarkers(t *testing.T) {
	all, err := New().ParseSymbols("internal/auth/cache.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	marks, err := New().RiskMarkers("internal/auth/cache.go", []byte(src), all)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, m := range marks {
		kinds[m.Kind] = true
	}
	for _, want := range []string{
		"package_level_state", "swallowed_error", "unsynchronised_goroutine",
		"network_without_timeout", "widened_type",
	} {
		if !kinds[want] {
			t.Errorf("missing risk marker %q (got %v)", want, keys(kinds))
		}
	}
}

func TestCallEdgesResolveLocallyAndFlagUnknowns(t *testing.T) {
	edges, err := New().CallEdges("internal/auth/cache.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var local, external bool
	for _, e := range edges {
		if e.From == "internal/auth/cache.go::Cache.Get" && e.To == "internal/auth/cache.go::Cache.refresh" {
			local = true
		}
		if strings.HasPrefix(string(e.To), "::") {
			external = true
		}
	}
	if !local {
		t.Errorf("intra-file call not resolved: %v", edges)
	}
	if !external {
		t.Error("an unresolved callee should stay unqualified rather than being invented")
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

const tables = `package auth

import "regexp"
import "errors"

// hunkRe is compiled once and only ever read.
var hunkRe = regexp.MustCompile("^@@")

// ErrMiss is a sentinel.
var errMiss = errors.New("miss")

var lookups = map[string]int{"a": 1}

var counter int

var Version = "dev"

func bump() {
	counter++
}
`

// A compiled regex or a lookup table assigned once is a constant in everything
// but syntax. Flagging it is the false-positive class that makes people stop
// reading the report (spec §7).
func TestPackageLevelStateOnlyFlagsWhatCanBeWritten(t *testing.T) {
	a := New()
	syms, err := a.ParseSymbols("x.go", []byte(tables))
	if err != nil {
		t.Fatal(err)
	}
	marks, err := a.RiskMarkers("x.go", []byte(tables), syms)
	if err != nil {
		t.Fatal(err)
	}
	flagged := map[string]bool{}
	for _, m := range marks {
		if m.Kind == "package_level_state" {
			flagged[string(m.Symbol)] = true
		}
	}
	for _, quiet := range []string{"x.go::hunkRe", "x.go::errMiss", "x.go::lookups"} {
		if flagged[quiet] {
			t.Errorf("%s is written once and never mutated — it must not be flagged", quiet)
		}
	}
	if !flagged["x.go::counter"] {
		t.Error("counter is incremented, so it is genuinely shared mutable state")
	}
	if !flagged["x.go::Version"] {
		t.Error("an exported var can be written by any importing package")
	}
}

// Binding a call to a local declaration on its bare name alone turns the stdlib
// call `http.Get` into this package's own `Cache.Get` — an invented edge, and a
// rationale comment attached to the wrong thing.
func TestCallSitesBindOnlyWhatTheShapeSupports(t *testing.T) {
	src := `package auth

import "net/http"

type Cache struct{}

func (c *Cache) Get(key string) string {
	// explains the local call
	v := c.decorate(key)
	resp, _ := http.Get("https://example.com/" + key)
	_ = resp
	return helper(v)
}

func (c *Cache) decorate(v string) string { return v }

func helper(v string) string { return v }
`
	syms, err := New().ParseSymbols("cache.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var get bundle.Symbol
	for _, s := range syms {
		if s.Name == "Cache.Get" {
			get = s
		}
	}
	byRaw := map[string]bundle.CallSite{}
	for _, cs := range get.CallSites {
		byRaw[cs.CalleeRaw] = cs
	}
	if got := byRaw["c.decorate"].Callee; got != "cache.go::Cache.decorate" {
		t.Errorf("c.decorate bound to %q, want the method on this receiver", got)
	}
	if !strings.Contains(byRaw["c.decorate"].Rationale, "explains the local call") {
		t.Errorf("the rationale comment did not attach: %q", byRaw["c.decorate"].Rationale)
	}
	if got := byRaw["helper"].Callee; got != "cache.go::helper" {
		t.Errorf("a bare call bound to %q", got)
	}
	if got := byRaw["http.Get"].Callee; got == "cache.go::Cache.Get" {
		t.Error("http.Get was bound to this package's own Cache.Get")
	}
}
