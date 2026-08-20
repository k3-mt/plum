package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kelalaike/plum/internal/ask"
	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/claims"
	"github.com/kelalaike/plum/internal/config"
	"github.com/kelalaike/plum/internal/explore"
	"github.com/kelalaike/plum/internal/lang"
	"github.com/kelalaike/plum/internal/lang/gopkg"
	"github.com/kelalaike/plum/internal/trace"
)

const source = `package auth

// Get returns the token.
func Get(key string) string {
	return key + "!"
}

func Helper(v string) string {
	return v
}
`

const testSource = `package auth

import "testing"

func TestHelper(t *testing.T) {
	if Helper("x") != "x" {
		t.Fatal("no")
	}
}
`

func testServer(t *testing.T) (*Server, *explore.Store) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cache.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cache_test.go"), []byte(testSource), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default(root)
	b := &bundle.Bundle{
		Session: bundle.Session{ID: "s1"},
		Symbols: []bundle.Symbol{{
			ID: "cache.go::Get", Name: "Get", Kind: "func", File: "cache.go",
			LineStart: 3, LineEnd: 6, Doc: "Get returns the token.",
			Signature: "func Get(key string) string",
			CallSites: []bundle.CallSite{{Callee: "cache.go::lookup", CalleeRaw: "lookup", Line: 4, Rationale: "the map is authoritative"}},
		}},
		RiskMarkers: []bundle.RiskMarker{{Kind: "swallowed_error", Symbol: "cache.go::Get", Line: 5, Note: "discarded"}},
		Journal:     []bundle.JournalEntry{{File: "cache.go", Rationale: "kept the plain map"}},
	}
	events := []trace.Event{
		{Kind: "call", Symbol: "cache.go::Get", InvocationID: "1", Args: map[string]string{"key": "user:42"}},
		{Kind: "return", Symbol: "cache.go::Get", InvocationID: "1", Result: "user:42!"},
	}
	l := trace.Derive(events, b)
	tel := explore.NewStore(filepath.Join(root, "state"))
	cs := []claims.Claim{{ID: "c-001", Claim: "Get appends a bang", Symbol: "cache.go::Get", Executable: true}}
	return New(cfg, b, l, events, cs, "# seams", tel, Config{
		Adapters:   lang.NewRegistry(gopkg.New()),
		Ask:        ask.NewStore(root),
		JournalDir: ".plum/journal",
		ClaimsPath: filepath.Join(root, "claims.yaml"),
	}), tel
}

func TestAssetsAreEmbeddedAndSmall(t *testing.T) {
	s, _ := testServer(t)
	total := 0
	for _, path := range []string{"/", "/app.css", "/landscape.js", "/flow.js"} {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		total += rec.Body.Len()
	}
	// No framework, no build step: the whole page must stay tiny (spec §10.5).
	if total > 100*1024 {
		t.Errorf("page weight %d bytes exceeds the 100KB budget", total)
	}
}

func TestSymbolContextIsAssembledMechanically(t *testing.T) {
	s, _ := testServer(t)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/symbol/cache.go::Get", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var pc PromptContext
	if err := json.NewDecoder(rec.Body).Decode(&pc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pc.Source, `return key + "!"`) {
		t.Errorf("source = %q — it must be the exact declaration text", pc.Source)
	}
	if len(pc.Invocations) != 2 {
		t.Errorf("invocations = %d, want the recorded call and return", len(pc.Invocations))
	}
	if len(pc.Risks) != 1 || len(pc.Rationale) != 1 || len(pc.Seams) != 1 {
		t.Errorf("assembled context is incomplete: %+v", pc)
	}
	if pc.CallSites[0].Rationale == "" {
		t.Error("call-site rationale should reach the UI")
	}
}

// A question that cannot be answered from the assembled context is itself a
// finding: the rationale was never recorded (spec §10.2).
func TestUnansweredQuestionsAreLogged(t *testing.T) {
	s, tel := testServer(t)
	askOne := func(sym string) askResponse {
		body := strings.NewReader(`{"symbol":"` + sym + `","question":"why?"}`)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/ask", body))
		var out askResponse
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := askOne("cache.go::Get"); !got.Grounded || got.Unanswered {
		t.Errorf("a symbol with traces and rationale is grounded: %+v", got)
	}
	if got := askOne("cache.go::Unknown"); got.Grounded || !got.Unanswered {
		t.Errorf("a symbol with neither traces nor rationale is a finding: %+v", got)
	}
	events, err := tel.Load("s1")
	if err != nil {
		t.Fatal(err)
	}
	var prompts, unanswerable int
	for _, e := range events {
		switch e.Action {
		case "prompt":
			prompts++
		case "unanswerable":
			unanswerable++
		}
	}
	if prompts != 2 || unanswerable != 1 {
		t.Errorf("telemetry = %d prompts, %d unanswerable", prompts, unanswerable)
	}
}

// Interrogation stays locked until the explore phase ends (P8).
func TestDoneUnlocksInterrogation(t *testing.T) {
	s, tel := testServer(t)
	if tel.IsDone("s1") {
		t.Fatal("locked by default")
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/done", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !tel.IsDone("s1") {
		t.Error("explore phase did not unlock the quiz")
	}
	<-s.done // the server stops itself once the phase is over
	_ = context.Background()
}

func TestTelemetryEndpointStampsTheSession(t *testing.T) {
	s, tel := testServer(t)
	body := strings.NewReader(`{"symbol":"cache.go::Get","action":"click","dwell_ms":900}`)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/telemetry", body))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d", rec.Code)
	}
	events, _ := tel.Load("s1")
	if len(events) != 1 || events[0].DwellMS != 900 {
		t.Errorf("events = %+v", events)
	}
}

// Clicking a frame in the UI puts its whole brief on the clipboard, so the
// rendered markdown has to travel with the JSON rather than being reassembled
// in the browser.
func TestSymbolResponseCarriesTheRenderedBrief(t *testing.T) {
	s, _ := testServer(t)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/symbol/cache.go::Get", nil))
	var pc PromptContext
	if err := json.NewDecoder(rec.Body).Decode(&pc); err != nil {
		t.Fatal(err)
	}
	if pc.Markdown == "" {
		t.Fatal("no brief to copy")
	}
	for _, want := range []string{
		"# cache.go::Get",
		"## Source", `return key + "!"`,
		"## Recorded invocations",
		"## Risk markers",
	} {
		if !strings.Contains(pc.Markdown, want) {
			t.Errorf("brief is missing %q", want)
		}
	}
	// The brief is what `plum context` prints, so the two cannot drift.
	same := AssembleContext(ContextInput{
		Cfg: s.Cfg, Bundle: s.Bundle, Events: s.Events,
		Claims: s.Claims, Adapters: s.Adapters, Landscape: s.Landscape,
	}, "cache.go::Get")
	if pc.Markdown != same {
		t.Error("the copied brief differs from what plum context prints")
	}
}

// The page is a single script with no build step, so a function that is called
// but never defined is a blank page — and nothing in Go's tests would notice.
// This is the cheapest guard against that.
func TestEveryFunctionTheScriptCallsIsDefined(t *testing.T) {
	// Every script the page loads is scanned as one program, because that is how
	// the browser sees them: the flow renderer calls helpers defined next door
	// and vice versa, and scanning either alone would report false failures and
	// miss real ones.
	var all strings.Builder
	for _, name := range []string{"assets/landscape.js", "assets/flow.js"} {
		src, err := assets.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		all.WriteString(string(src))
		all.WriteString("\n")
	}
	js := stripJSLiterals(stripJSComments(all.String()))

	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^(?:async )?function ([A-Za-z_$][\w$]*)`).FindAllStringSubmatch(js, -1) {
		defined[m[1]] = true
	}
	// Arrow and expression forms count as definitions too.
	for _, m := range regexp.MustCompile(`(?:const|let|var) ([A-Za-z_$][\w$]*)\s*=`).FindAllStringSubmatch(js, -1) {
		defined[m[1]] = true
	}
	if len(defined) == 0 {
		t.Fatal("no functions found; the extraction is wrong, not the script")
	}

	keywords := map[string]bool{
		"if": true, "for": true, "while": true, "switch": true, "catch": true,
		"return": true, "function": true, "typeof": true, "await": true,
		"async": true, "var": true, "const": true, "let": true, "new": true,
		"else": true, "do": true, "of": true, "in": true, "delete": true, "void": true,
	}
	builtin := map[string]bool{
		"fetch": true, "setTimeout": true, "clearTimeout": true, "setInterval": true,
		"clearInterval": true, "parseInt": true, "encodeURIComponent": true,
		"decodeURIComponent": true, "alert": true, "isNaN": true,
	}

	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)(?:^|[^.\w$])([a-z][\w$]*)\s*\(`).FindAllStringSubmatch(js, -1) {
		name := m[1]
		if defined[name] || keywords[name] || builtin[name] || seen[name] {
			continue
		}
		seen[name] = true
		// Parameters and destructured locals are out of scope: only bare calls
		// that look like top-level helpers matter.
		if regexp.MustCompile(`[(,]\s*` + regexp.QuoteMeta(name) + `\s*[,)]`).MatchString(js) {
			continue
		}
		t.Errorf("the page calls %s() but never defines it — it would throw at boot", name)
	}
}

// stripJSComments removes comments before literals are scanned. An apostrophe
// in prose ("the frame's cost") would otherwise open a string that never
// closes, blanking the rest of the file and hiding every definition in it.
func stripJSComments(js string) string {
	var out strings.Builder
	for i := 0; i < len(js); i++ {
		if js[i] == '/' && i+1 < len(js) && js[i+1] == '/' {
			for i < len(js) && js[i] != '\n' {
				i++
			}
			out.WriteByte('\n')
			continue
		}
		if js[i] == '/' && i+1 < len(js) && js[i+1] == '*' {
			i += 2
			for i+1 < len(js) && !(js[i] == '*' && js[i+1] == '/') {
				i++
			}
			i++
			continue
		}
		out.WriteByte(js[i])
	}
	return out.String()
}

// stripJSLiterals blanks string and template contents so a CSS value like
// "var(--risk)" is not read as a call to var().
func stripJSLiterals(js string) string {
	var out strings.Builder
	quote := byte(0)
	for i := 0; i < len(js); i++ {
		c := js[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
				out.WriteString("  ")
				continue
			}
			if c == quote {
				quote = 0
				out.WriteByte(c)
				continue
			}
			out.WriteByte(' ')
		case c == '\'' || c == '"' || c == '`':
			quote = c
			out.WriteByte(c)
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// The page is a view of files that change while you are looking at them. If the
// watcher does not notice, the reader is reading yesterday's evidence.
func TestWatcherNoticesSessionAndSourceChanges(t *testing.T) {
	s, _ := testServer(t)
	s.SessionDir = filepath.Join(s.Cfg.Root, "session")
	if err := os.MkdirAll(s.SessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.SessionDir, "landscape.json"), []byte(`{"wells":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	defer close(stop)
	client := s.hub.add()
	defer s.hub.remove(client)
	go s.watch(stop)

	// A session artifact appearing — what `plum interpret` does in the other pane.
	time.Sleep(2 * watchInterval)
	if err := os.WriteFile(filepath.Join(s.SessionDir, "interpretation.json"), []byte(`{"entries":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case what := <-client:
		if what != "session" {
			t.Errorf("event = %q, want session", what)
		}
	case <-time.After(6 * watchInterval):
		t.Fatal("a new session artifact did not reach the page")
	}

	// Source changing without a re-capture still matters: it is what turns a
	// stored reading stale, and the source pane is showing it.
	if err := os.WriteFile(filepath.Join(s.Cfg.Root, "cache.go"), []byte(source+"\n// edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(8 * watchInterval)
	for {
		select {
		case what := <-client:
			if what == "source" {
				return
			}
			// A session write can land in more than one tick; keep waiting for
			// the event this case is about rather than failing on a neighbour.
		case <-deadline:
			t.Fatal("an edit to the source did not reach the page")
		}
	}
}

// A view narrowed to one test must stay narrowed when the session reloads,
// or the reader's frame silently widens under them.
func TestReloadKeepsTheTestFilter(t *testing.T) {
	s, _ := testServer(t)
	s.SessionDir = filepath.Join(s.Cfg.Root, "session")
	s.TestFilter = "TestOne"
	if err := os.MkdirAll(s.SessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A landscape on disk covering two tests' worth of frames.
	if err := os.WriteFile(filepath.Join(s.SessionDir, "landscape.json"),
		[]byte(`{"wells":[{"symbol":"cache.go::Get","label":"Get","phase":"enter"},{"symbol":"cache.go::Other","label":"Other","phase":"enter"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Events = []trace.Event{
		{Kind: "call", Symbol: "cache.go::Get", InvocationID: "1", TestID: "TestOne"},
		{Kind: "return", Symbol: "cache.go::Get", InvocationID: "1", TestID: "TestOne", Result: "x"},
	}
	s.reload()
	if s.Landscape.TestID != "TestOne" {
		t.Errorf("the filter was lost: test id = %q", s.Landscape.TestID)
	}
	for _, w := range s.Landscape.Wells {
		if w.Symbol == "cache.go::Other" {
			t.Error("reloading widened a narrowed view back to the whole recording")
		}
	}
}

// The bundle only holds what the session changed, but the landscape now draws
// the surrounding code a run passes through. Clicking one of those frames must
// not produce an empty brief — that reads as "the evidence is missing" rather
// than "nobody looked it up".
func TestBriefForAFrameTheBundleNeverCaptured(t *testing.T) {
	s, _ := testServer(t)
	// A symbol that exists in the working tree but not in the bundle.
	pc := s.buildContext("cache.go::Helper")
	if pc.Changed {
		t.Error("an unchanged symbol must not be reported as changed")
	}
	if pc.Source == "" {
		t.Fatal("no source: the declaration was not resolved from the working tree")
	}
	if !strings.Contains(pc.Source, "func Helper") {
		t.Errorf("source = %q", pc.Source)
	}
	brief := renderContext(pc)
	for _, want := range []string{
		"This session did **not** change it",
		"func Helper",
		"## What is missing",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief is missing %q:\n%s", want, brief)
		}
	}
}

// A test is the thing exercising the change, not part of it: holding it to the
// same standard buries the gaps that matter under ones that do not.
func TestBriefDoesNotScoldTestFiles(t *testing.T) {
	s, _ := testServer(t)
	pc := s.buildContext("cache_test.go::TestHelper")
	brief := renderContext(pc)

	if !strings.Contains(brief, "It lives in a test file") {
		t.Error("the brief should say a test file is the exercising code")
	}
	for _, unwanted := range []string{
		"no declaration doc",
		"no journalled rationale",
		"no claims",
	} {
		if strings.Contains(brief, unwanted) {
			t.Errorf("a test file was scolded for %q:\n%s", unwanted, brief)
		}
	}
}

// Nobody writes a comment above fmt.Sprintf. Counting library calls as
// unexplained buries the calls that genuinely are.
func TestOnlyCallsIntoTheRepositoryCountAsUnexplained(t *testing.T) {
	pc := PromptContext{
		Symbol_: bundle.Symbol{ID: "a.go::F", File: "a.go", Doc: "F does a thing."},
		Doc:     "F does a thing.",
		CallSites: []bundle.CallSite{
			{Callee: "a.go::helper", CalleeRaw: "helper", Line: 3},
			{Callee: "::fmt.Sprintf", CalleeRaw: "fmt.Sprintf", Line: 4},
			{Callee: "::errors.New", CalleeRaw: "errors.New", Line: 5},
		},
		Invocations: []trace.Event{{Kind: "call"}},
		Rationale:   []bundle.JournalEntry{{Rationale: "because"}},
		Seams:       []claims.Claim{{Claim: "x"}},
	}
	gaps := missingFrom(pc)
	if len(gaps) != 1 {
		t.Fatalf("gaps = %v, want only the unannotated local call", gaps)
	}
	if !strings.Contains(gaps[0], "its one call into this repository's own code") {
		t.Errorf("gap = %q", gaps[0])
	}
	brief := renderContext(pc)
	if !strings.Contains(brief, "could not resolve to a declaration in the repository") {
		t.Errorf("library calls should be listed but not blamed:\n%s", brief)
	}
}
