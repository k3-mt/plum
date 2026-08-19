package cli_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/capture"
	"github.com/kelalaike/plum/internal/claims"
	"github.com/kelalaike/plum/internal/config"
	"github.com/kelalaike/plum/internal/explore"
	"github.com/kelalaike/plum/internal/extract"
	"github.com/kelalaike/plum/internal/lang"
	"github.com/kelalaike/plum/internal/lang/gopkg"
	"github.com/kelalaike/plum/internal/quiz"
	"github.com/kelalaike/plum/internal/report"
	"github.com/kelalaike/plum/internal/store"
	"github.com/kelalaike/plum/internal/synth"
	"github.com/kelalaike/plum/internal/trace"
	"github.com/kelalaike/plum/internal/vcs"
)

const before = `package auth

import "fmt"

// Cache holds tokens keyed by user id.
type Cache struct{ entries map[string]string }

// NewCache constructs an empty Cache.
func NewCache() *Cache { return &Cache{entries: map[string]string{}} }

// Get returns the token for a key.
func (c *Cache) Get(key string) (string, error) {
	v, ok := c.entries[key]
	if !ok {
		return "", fmt.Errorf("get %q: not found", key)
	}
	return v, nil
}

// Put stores a token.
func (c *Cache) Put(key, token string) { c.entries[key] = token }
`

const after = `package auth

import "fmt"

// Cache holds tokens keyed by user id.
type Cache struct{ entries map[string]string }

// NewCache constructs an empty Cache.
func NewCache() *Cache { return &Cache{entries: map[string]string{}} }

// Get returns the token for a key.
func (c *Cache) Get(key string) (string, error) {
	// the decorator is applied on the way out so callers never see a raw token
	v, err := c.lookup(key)
	if err != nil {
		return "", err
	}
	return c.decorate(v), nil
}

func (c *Cache) lookup(key string) (string, error) {
	v, ok := c.entries[key]
	if !ok {
		return "", fmt.Errorf("lookup %q: not found", key)
	}
	return v, nil
}

func (c *Cache) decorate(v string) string { return v + "!" }

// Put stores a token.
func (c *Cache) Put(key, token string) { c.entries[key] = token }

// MustGet panics when the key is absent.
func (c *Cache) MustGet(key string) string {
	v, err := c.lookup(key)
	if err != nil {
		panic("auth: no token for " + key)
	}
	return v
}
`

const testFile = `package auth

import "testing"

func TestGet(t *testing.T) {
	c := NewCache()
	c.Put("user:42", "tok")
	if v, err := c.Get("user:42"); err != nil || v != "tok!" {
		t.Fatalf("got %q %v", v, err)
	}
}

func TestMustGetPanics(t *testing.T) {
	c := NewCache()
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic")
		}
	}()
	_ = c.MustGet("absent")
}
`

// TestPipelineEndToEnd walks the whole tool over a real repository: capture the
// session, extract the bundle, synthesise seams, instrument and trace, derive
// the landscape, target questions from telemetry, and verify a claim.
func TestPipelineEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go test inside a scratch copy")
	}
	root := t.TempDir()
	ctx := context.Background()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("go.mod", "module example.com/e2e\n\ngo 1.23\n")
	write("internal/auth/cache.go", before)
	git("init", "-q")
	git("config", "user.email", "e2e@plum.test")
	git("config", "user.name", "e2e")
	git("add", "-A")
	git("commit", "-qm", "before")

	cfg := config.Default(root)
	repo := vcs.New(root)
	reg := lang.NewRegistry(gopkg.New())
	st := store.New(cfg)

	startSHA, err := repo.RevParse(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	// The "agent" session: it leaves the work uncommitted, as agents do.
	write("internal/auth/cache.go", after)
	write("internal/auth/cache_test.go", testFile)

	res, err := capture.Run(ctx, cfg, repo, []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Session.StartSHA != startSHA || res.Session.EndSHA == startSHA {
		t.Fatalf("session range = %s..%s", res.Session.StartSHA, res.Session.EndSHA)
	}

	b, err := extract.New(repo, cfg, reg).Extract(ctx, res.Session, res.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(b); err != nil {
		t.Fatal(err)
	}

	// --- M0: the bundle names symbols, not line numbers -------------------
	changed := map[bundle.SymbolID]string{}
	for _, s := range b.Symbols {
		changed[s.ID] = s.Change
	}
	for id, want := range map[bundle.SymbolID]string{
		"internal/auth/cache.go::Cache.Get":      "modified",
		"internal/auth/cache.go::Cache.lookup":   "added",
		"internal/auth/cache.go::Cache.decorate": "added",
		"internal/auth/cache.go::Cache.MustGet":  "added",
	} {
		if got := changed[id]; got != want {
			t.Errorf("%s = %q, want %q (all: %v)", id, got, want, changed)
		}
	}
	if changed["internal/auth/cache.go::Cache.Put"] != "" {
		t.Error("an untouched symbol must not appear in the bundle")
	}
	if !b.Gate.Fired {
		t.Error("a session adding new public surface should fire the gate")
	}
	if md := report.Render(b, report.Options{}); !strings.Contains(md, "Cache.MustGet") {
		t.Error("the report should name the new export")
	}

	// --- M1: synthesis and claims, with fingerprints for staleness --------
	sres, err := synth.Run(ctx, cfg, b, "", &synth.Offline{Bundle: b})
	if err != nil {
		t.Fatal(err)
	}
	if len(sres.Claims) == 0 {
		t.Fatal("synthesis produced no claims")
	}
	for _, c := range sres.Claims {
		if c.Fingerprint == "" && b.Has(c.Symbol) {
			t.Errorf("claim %s about a captured symbol has no fingerprint — staleness would be undetectable", c.ID)
		}
	}
	if err := claims.Save(st.ClaimsPath(b.Session.ID), sres.Claims); err != nil {
		t.Fatal(err)
	}

	// --- M2: instrument only the changed set, trace, derive the landscape --
	col := &trace.Collector{
		Root: root, Scratch: filepath.Join(t.TempDir(), "scratch"),
		TestCommand: "go test ./...", MaxEvents: 10000,
	}
	tres, err := col.Run(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(tres.Events) == 0 {
		t.Fatalf("no events recorded; test output:\n%s", tres.TestOutput)
	}
	for _, e := range tres.Events {
		if !b.Has(e.Symbol) {
			t.Errorf("instrumented a symbol outside the changed set: %s", e.Symbol)
		}
	}

	l := trace.Derive(tres.Events, b)
	if !l.Closed && l.Escaped == "" {
		t.Errorf("the hot path neither closed nor escaped: open at %s", l.OpenFrame)
	}
	var sawArgs, sawResult bool
	for _, e := range tres.Events {
		if e.Kind == "call" && e.Args["key"] != "" {
			sawArgs = true
		}
		if e.Kind == "return" && strings.Contains(e.Result, "tok") {
			sawResult = true
		}
	}
	if !sawArgs || !sawResult {
		t.Error("traces must record real arguments and real return values, not a summary")
	}

	// The raising chain must show one cliff spanning several frames.
	raising := trace.DeriveChain(tres.Events, b, trace.ChainRaising)
	var cliff *trace.Barrier
	for i := range raising.Barriers {
		if raising.Barriers[i].Direction == "unwind" {
			cliff = &raising.Barriers[i]
		}
	}
	if cliff == nil {
		t.Fatal("a deliberate panic produced no unwind barrier")
	}

	// --- M3/M4: telemetry targets the questions, graded on the traces ------
	tel := explore.NewStore(filepath.Join(t.TempDir(), "state"))
	if err := tel.Append(explore.Event{SessionID: b.Session.ID, Symbol: "internal/auth/cache.go::Cache.decorate", Action: "click"}); err != nil {
		t.Fatal(err)
	}
	targets := explore.TargetSymbols(mustLoad(t, tel, b.Session.ID), l, 4)
	if len(targets) == 0 {
		t.Fatal("no targets derived from telemetry")
	}
	qs := quiz.Generate(nil, tres.Events, l, 5)
	if len(qs) == 0 {
		t.Fatal("no questions generated from recorded invocations")
	}
	for _, q := range qs {
		if q.Expected == "" {
			t.Errorf("question %q has no recorded answer to grade against", q.Prompt)
		}
		if !quiz.Grade(q, q.Expected) {
			t.Errorf("the recorded answer does not grade as correct: %q", q.Expected)
		}
		if quiz.Grade(q, "definitely not the answer at all") {
			t.Errorf("a wrong answer graded as correct for %q", q.Prompt)
		}
	}

	// --- M5: executable claims run, trust-me assertions are surfaced -------
	verdicts, err := claims.Verify(ctx, root, filepath.Join(t.TempDir(), "claims"), []claims.Claim{
		{ID: "c-ok", Claim: "decorate appends a bang", Symbol: "internal/auth/cache.go::Cache.decorate", Executable: true,
			Test: "func TestClaimOK(t *testing.T) {\n\tc := NewCache()\n\tif c.decorate(\"x\") != \"x!\" {\n\t\tt.Fatal(\"no bang\")\n\t}\n}"},
		{ID: "c-bad", Claim: "decorate is a no-op", Symbol: "internal/auth/cache.go::Cache.decorate", Executable: true,
			Test: "func TestClaimBad(t *testing.T) {\n\tc := NewCache()\n\tif c.decorate(\"x\") != \"x\" {\n\t\tt.Fatalf(\"decorate changed the value: %q\", c.decorate(\"x\"))\n\t}\n}"},
		{ID: "c-trust", Claim: "refresh never blocks", Symbol: "internal/auth/cache.go::Cache.Get", Executable: false},
	}, trace.CopyTree)
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, v := range verdicts {
		status[v.Claim.ID] = v.Status
	}
	if status["c-ok"] != "pass" || status["c-bad"] != "fail" || status["c-trust"] != "unverifiable" {
		t.Errorf("verdicts = %v", status)
	}

	// The repository itself must be untouched by any of this.
	generated, _ := filepath.Glob(filepath.Join(root, "internal", "auth", "plum_claim_*"))
	if len(generated) != 0 {
		t.Errorf("claim tests leaked into the repo: %v", generated)
	}
	src, err := os.ReadFile(filepath.Join(root, "internal", "auth", "cache.go"))
	if err != nil || string(src) != after {
		t.Error("instrumentation leaked into the working tree")
	}
}

func mustLoad(t *testing.T, s *explore.Store, id string) []explore.Event {
	t.Helper()
	ev, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}
