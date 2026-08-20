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
	"github.com/kelalaike/plum/internal/trace"
)

const source = `package auth

// Get returns the token.
func Get(key string) string {
	return key + "!"
}
`

func testServer(t *testing.T) (*Server, *explore.Store) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cache.go"), []byte(source), 0o644); err != nil {
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
		Ask:        ask.NewStore(root),
		JournalDir: ".plum/journal",
		ClaimsPath: filepath.Join(root, "claims.yaml"),
	}), tel
}

func TestAssetsAreEmbeddedAndSmall(t *testing.T) {
	s, _ := testServer(t)
	total := 0
	for _, path := range []string{"/", "/app.css", "/landscape.js"} {
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
	if !strings.Contains(pc.Source, "return key + \"!\"") {
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
		"## Symbol", "cache.go::Get",
		"## Source", `return key + "!"`,
		"## Recorded invocations",
		"## Risk markers",
	} {
		if !strings.Contains(pc.Markdown, want) {
			t.Errorf("brief is missing %q", want)
		}
	}
	// The brief is what `plum context` prints, so the two cannot drift.
	if pc.Markdown != AssembleContext(s.Cfg, s.Bundle, s.Events, s.Claims, "cache.go::Get") {
		t.Error("the copied brief differs from what plum context prints")
	}
}

// The page is a single script with no build step, so a function that is called
// but never defined is a blank page — and nothing in Go's tests would notice.
// This is the cheapest guard against that.
func TestEveryFunctionTheScriptCallsIsDefined(t *testing.T) {
	src, err := assets.ReadFile("assets/landscape.js")
	if err != nil {
		t.Fatal(err)
	}
	js := stripJSLiterals(stripJSComments(string(src)))

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
		t.Errorf("landscape.js calls %s() but never defines it — the page would throw at boot", name)
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
	select {
	case what := <-client:
		if what != "source" {
			t.Errorf("event = %q, want source", what)
		}
	case <-time.After(6 * watchInterval):
		t.Fatal("an edit to the source did not reach the page")
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
