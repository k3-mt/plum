package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/k3-mt/plum/internal/ask"
	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/claims"
	"github.com/k3-mt/plum/internal/config"
	"github.com/k3-mt/plum/internal/explore"
	"github.com/k3-mt/plum/internal/lang"
	"github.com/k3-mt/plum/internal/lang/gopkg"
	"github.com/k3-mt/plum/internal/met"
	"github.com/k3-mt/plum/internal/store"
	"github.com/k3-mt/plum/internal/trace"
)

// The fixture's source and its bundle have to agree. They did not: the bundle
// described a call site the source did not contain, which only passed because
// the server read the bundle's copy rather than the file. Now that it reads the
// file, an inconsistent fixture is a test of an impossible state.
const source = `package auth

// Get returns the token.
func Get(key string) string {
	// the map is authoritative
	_ = lookup(key)
	return key + "!"
}

func lookup(k string) string { return k }

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
			LineStart: 3, LineEnd: 6, Doc: "Get returns the token.", Fingerprint: "fp1",
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
		Met:        met.Load(filepath.Join(root, "state")),
	}), tel
}

func TestAssetsAreEmbeddedAndSmall(t *testing.T) {
	s, _ := testServer(t)
	// Per page, not summed across every page in the binary. The budget is about
	// what a browser has to fetch to show you something, and nobody loads both
	// of these at once; adding them together would make each page's allowance
	// shrink every time another page was added, which is the wrong pressure.
	pages := map[string][]string{
		"session": {"/", "/app.css", "/code.js", "/view.js", "/flow.js", "/landscape.js"},
		"probe":   {"/probe.html", "/probe.css", "/code.js", "/probe.js"},
	}
	for name, paths := range pages {
		total := 0
		for _, path := range paths {
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: %s = %d", name, path, rec.Code)
			}
			total += rec.Body.Len()
		}
		// No framework, no build step: a page must stay tiny (spec §10.5).
		if total > 100*1024 {
			t.Errorf("%s page weight %d bytes exceeds the 100KB budget", name, total)
		}
		t.Logf("%s page: %d bytes", name, total)
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
	// Each page is scanned as its own program, because that is how the browser
	// sees it: the session page loads four scripts that call into each other,
	// the probe page loads two, and a helper defined only on the other page is a
	// real failure rather than a false one.
	for _, page := range [][]string{
		{"assets/code.js", "assets/view.js", "assets/flow.js", "assets/landscape.js"},
		{"assets/code.js", "assets/probe.js"},
	} {
		checkScriptProgram(t, page)
	}
}

func checkScriptProgram(t *testing.T, names []string) {
	t.Helper()
	var all strings.Builder
	for _, name := range names {
		src, err := assets.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		all.WriteString(string(src))
		all.WriteString("\n")
	}
	js := stripCode(all.String())

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
		"requestAnimationFrame": true, "cancelAnimationFrame": true,
		"getComputedStyle": true, "structuredClone": true, "queueMicrotask": true,
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
// stripCode blanks out comments, string bodies and regex literals in one pass,
// because doing it in separate passes cannot be right in any order. Comments
// first reads the `'//'` inside a string as a comment and leaves an unterminated
// quote that swallows the code below it; strings first opens a quote on the
// apostrophe in a comment like "the frame's body"; and either way a regex like
// /^["'`]/ opens a string that runs to the next quote anywhere in the file.
//
// All three are the same problem — you cannot know what a character means
// without knowing what you are already inside — so all three are tracked at
// once. This test is only as good as this scanner: every blind spot here shows
// up as a helper the page really defines being reported as missing, or worse, a
// missing one going unreported.
func stripCode(js string) string {
	var out strings.Builder
	const (
		plain = iota
		inLine
		inBlock
		inString
		inRegex
	)
	state, quote, inClass := plain, byte(0), false
	last := byte(0) // last significant character, for telling regex from division

	for i := 0; i < len(js); i++ {
		c := js[i]
		switch state {
		case inLine:
			if c == '\n' {
				state = plain
				out.WriteByte(c)
				continue
			}
			out.WriteByte(' ')
		case inBlock:
			if c == '*' && i+1 < len(js) && js[i+1] == '/' {
				state = plain
				i++
				out.WriteString("  ")
				continue
			}
			if c == '\n' {
				out.WriteByte(c)
				continue
			}
			out.WriteByte(' ')
		case inString:
			if c == '\\' {
				i++
				out.WriteString("  ")
				continue
			}
			if c == quote {
				state, quote = plain, 0
			}
			out.WriteByte(map[bool]byte{true: c, false: ' '}[c == quote])
		case inRegex:
			// A slash inside a character class is a literal slash, not the end.
			switch {
			case c == '\\':
				i++
				out.WriteString("  ")
				continue
			case c == '[':
				inClass = true
			case c == ']':
				inClass = false
			case c == '/' && !inClass:
				state = plain
				last = '/'
				out.WriteByte(' ')
				continue
			}
			out.WriteByte(' ')
		default:
			switch {
			case c == '/' && i+1 < len(js) && js[i+1] == '/':
				state = inLine
				out.WriteString("  ")
				i++
			case c == '/' && i+1 < len(js) && js[i+1] == '*':
				state = inBlock
				out.WriteString("  ")
				i++
			case c == '/' && regexPosition(last):
				state, inClass = inRegex, false
				out.WriteByte(' ')
			case c == '\'' || c == '"' || c == '`':
				state, quote = inString, c
				out.WriteByte(c)
			default:
				out.WriteByte(c)
			}
			if c != ' ' && c != '\t' && c != '\n' && state == plain {
				last = c
			}
		}
	}
	return out.String()
}

// regexPosition says whether a slash here can only begin a regex literal rather
// than divide something. Division always follows a value — a name, a number, a
// closing bracket — and a regex never does.
func regexPosition(last byte) bool {
	switch last {
	case 0, '(', ',', '=', ':', '[', '!', '&', '|', '?', '{', '}', ';', '+', '-', '*', '%', '<', '>', '~', '^':
		return true
	}
	return false
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

// The window outliving one recording is the whole difference between `plum
// explore` and `plum watch`: the agent stops, capture writes a new session, and
// what you are looking at has to become that session without you asking.
func TestWatchFollowsTheNewestSession(t *testing.T) {
	s, _ := testServer(t)
	s.SessionDir = filepath.Join(s.Cfg.Root, "session")
	if err := os.MkdirAll(s.SessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s.sessions = store.New(s.Cfg)
	s.follow = true

	stop := make(chan struct{})
	defer close(stop)
	client := s.hub.add()
	defer s.hub.remove(client)
	go s.watch(stop)

	time.Sleep(2 * watchInterval)
	writeSession(t, s.Cfg.SessionsDir(), "s2")

	select {
	case what := <-client:
		if what != "session" {
			t.Fatalf("event = %q, want session", what)
		}
	case <-time.After(10 * watchInterval):
		t.Fatal("a newly captured session never reached the window")
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.sessionID != "s2" {
		t.Errorf("sessionID = %q, want s2", s.sessionID)
	}
	if s.Bundle.Session.ID != "s2" {
		t.Errorf("bundle session = %q — the window is still showing the previous recording", s.Bundle.Session.ID)
	}
	if s.SessionDir != filepath.Join(s.Cfg.SessionsDir(), "s2") {
		t.Errorf("SessionDir = %q, want the new session's directory", s.SessionDir)
	}
}

// Following must not carry a narrowed view across. The filter was derived from
// one recording's traces; applied to the next it would draw a path nothing took.
func TestFollowingDropsTheTestFilter(t *testing.T) {
	s, _ := testServer(t)
	s.sessions = store.New(s.Cfg)
	s.follow = true
	s.TestFilter = "TestGet"
	writeSession(t, s.Cfg.SessionsDir(), "s2")

	if !s.followLatest() {
		t.Fatal("followLatest did not move to the new session")
	}
	if s.TestFilter != "" {
		t.Errorf("TestFilter = %q — a filter must not survive a session change", s.TestFilter)
	}
}

// A second `plum watch` finds the first one through this, and refuses to attach
// unless the repository matches: the port is a hash of a path, and a collision
// must not silently point the window at somebody else's codebase.
func TestHealthNamesTheRepositoryAndSession(t *testing.T) {
	s, _ := testServer(t)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var health map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if health["plum"] != "ok" || health["repo"] != s.Cfg.Root || health["session"] != "s1" {
		t.Errorf("health = %v", health)
	}
}

// "I have met this code" ends an explore. It must not end a window someone left
// on a second screen — the next session is exactly what they are waiting for.
func TestResidentWindowSurvivesBeingDone(t *testing.T) {
	s, tel := testServer(t)
	s.resident = true
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/done", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !tel.IsDone("s1") {
		t.Error("the session should still be marked met")
	}
	select {
	case <-s.done:
		t.Fatal("a resident window shut itself down when the session was marked met")
	default:
	}
}

func writeSession(t *testing.T, sessionsDir, id string) {
	t.Helper()
	dir := filepath.Join(sessionsDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b := bundle.Bundle{Session: bundle.Session{ID: id, StartedAt: time.Now()}}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bundle.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The agent writes the code and the Stop hook captures it moments later, so an
// edit and a capture routinely land in the same tick. The capture must not
// swallow the edit: the source pane would then be showing text the page has
// never been told changed.
func TestACaptureInTheSameTickDoesNotSwallowAnEdit(t *testing.T) {
	s, _ := testServer(t)
	s.SessionDir = filepath.Join(s.Cfg.Root, "session")
	if err := os.MkdirAll(s.SessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	st := s.baseline()

	// Both between one tick and the next, the way they actually arrive.
	if err := os.WriteFile(filepath.Join(s.Cfg.Root, "cache.go"), []byte(source+"\n// edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.SessionDir, "bundle.json"), []byte(`{"session":{"id":"s1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if sent := s.tick(st); len(sent) != 2 || sent[0] != "session" || sent[1] != "source" {
		t.Fatalf("tick sent %v, want both session and source", sent)
	}
	// And having reported them, it must not report them again.
	if sent := s.tick(st); len(sent) != 0 {
		t.Errorf("a quiet tick sent %v", sent)
	}
}

// Moving to another recording is not an edit to the one you were reading. The
// file list changes wholesale, and calling that a source change would tell the
// reader their code moved under them when they were simply shown other code.
func TestFollowingANewSessionDoesNotReportItAsASourceEdit(t *testing.T) {
	s, _ := testServer(t)
	s.sessions = store.New(s.Cfg)
	s.follow = true
	st := s.baseline()
	writeSession(t, s.Cfg.SessionsDir(), "s2")

	if sent := s.tick(st); len(sent) != 1 || sent[0] != "session" {
		t.Fatalf("tick sent %v, want session alone", sent)
	}
	if sent := s.tick(st); len(sent) != 0 {
		t.Errorf("the tick after a session swap sent %v — the baseline was not reset", sent)
	}
}

// The loop the window exists to close: an agent changes a symbol, the meter says
// you have not met it, reading it pays that down. If a brief did not count, the
// number could only ever be cleared wholesale and would stop meaning anything.
func TestReadingABriefPaysDownTheDebt(t *testing.T) {
	s, _ := testServer(t)

	s.mu.RLock()
	before := s.debt()
	s.mu.RUnlock()
	if before.Unmet != 1 || before.Total != 1 {
		t.Fatalf("before reading: %+v, want the one changed symbol unmet", before)
	}

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/symbol/cache.go::Get", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/debt", nil))
	var after met.Debt
	if err := json.NewDecoder(rec.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	if after.Unmet != 0 {
		t.Errorf("after reading the brief: %+v, want the debt paid", after)
	}
}

func TestMeetingTheCodeClearsTheWholeChangedSet(t *testing.T) {
	s, _ := testServer(t)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/done", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if d := s.debt(); d.Unmet != 0 {
		t.Errorf("%+v, want nothing outstanding", d)
	}
}

// The landscape payload carries every changed symbol in the session and runs to
// megabytes on a real capture. A window glancing at the meter must not pay that.
func TestTheMeterCanBeAskedForOnItsOwn(t *testing.T) {
	s, _ := testServer(t)
	small := httptest.NewRecorder()
	s.mux.ServeHTTP(small, httptest.NewRequest(http.MethodGet, "/api/debt", nil))
	big := httptest.NewRecorder()
	s.mux.ServeHTTP(big, httptest.NewRequest(http.MethodGet, "/api/landscape", nil))
	if small.Body.Len() >= big.Body.Len() {
		t.Errorf("debt %d bytes vs landscape %d — the cheap question is not cheap",
			small.Body.Len(), big.Body.Len())
	}
}

// An export has no reader and no server. It must not carry an install manifest
// pointing at endpoints that do not exist, and it must not show a debt meter:
// zero would read as "you have met all of this", which is a different claim.
func TestExportCarriesNoInstallManifest(t *testing.T) {
	s, _ := testServer(t)
	out, err := s.Export()
	if err != nil {
		t.Fatal(err)
	}
	for _, dead := range []string{"manifest.webmanifest", `href="/icon-192.png"`} {
		if strings.Contains(string(out), dead) {
			t.Errorf("the export still references %s", dead)
		}
	}
}

// Served as text/plain, a manifest is ignored and the page is not installable —
// which is the whole of what it is for.
func TestTheManifestIsServedAsAManifest(t *testing.T) {
	s, _ := testServer(t)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("content-type"); !strings.HasPrefix(ct, "application/manifest+json") {
		t.Errorf("content-type = %q", ct)
	}
}

// A bundle is a photograph. Between the agent writing a line and the Stop hook
// capturing it there is a window — often the most interesting minute of the
// session — in which a meter measured only against the capture sits perfectly
// still. So the window also asks the files.
func TestTheMeterSeesCodeWrittenSinceTheCapture(t *testing.T) {
	s, _ := testServer(t)

	// Pin the bundle to what is actually on disk, so the starting point is
	// agreement rather than a fixture that never matched.
	reg := lang.NewRegistry(gopkg.New())
	src, err := os.ReadFile(filepath.Join(s.Cfg.Root, "cache.go"))
	if err != nil {
		t.Fatal(err)
	}
	syms, err := reg.For("cache.go").ParseSymbols("cache.go", src)
	if err != nil {
		t.Fatal(err)
	}
	var pinned bool
	for _, sym := range syms {
		if sym.ID == "cache.go::Get" {
			s.Bundle.Symbols[0].Fingerprint = sym.Fingerprint
			pinned = true
		}
	}
	if !pinned {
		t.Fatal("the fixture no longer parses to a symbol this test can pin")
	}

	s.mu.RLock()
	quiet := s.debt()
	s.mu.RUnlock()
	if quiet.Drifted != 0 {
		t.Fatalf("with the tree matching the capture: %+v, want no drift", quiet)
	}

	// The agent edits the function body. Nothing has been captured; the meter
	// must say so anyway.
	edited := strings.Replace(string(src), `return key + "!"`, `return key + "?"`, 1)
	if edited == string(src) {
		t.Fatal("the fixture changed; this edit no longer alters the body")
	}
	if err := os.WriteFile(filepath.Join(s.Cfg.Root, "cache.go"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if d := s.debt(); d.Drifted != 1 {
		t.Errorf("after an uncaptured edit: %+v, want 1 drifted", d)
	}
}

// Reformatting is not a change. The fingerprint is over the normalised subtree,
// so the meter must not twitch every time an agent runs a formatter.
func TestReformattingIsNotDrift(t *testing.T) {
	s, _ := testServer(t)
	reg := lang.NewRegistry(gopkg.New())
	src, err := os.ReadFile(filepath.Join(s.Cfg.Root, "cache.go"))
	if err != nil {
		t.Fatal(err)
	}
	syms, err := reg.For("cache.go").ParseSymbols("cache.go", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, sym := range syms {
		if sym.ID == "cache.go::Get" {
			s.Bundle.Symbols[0].Fingerprint = sym.Fingerprint
		}
	}
	if err := os.WriteFile(filepath.Join(s.Cfg.Root, "cache.go"),
		[]byte(strings.Replace(string(src), "\n\treturn key", "\n\n\treturn key", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if d := s.debt(); d.Drifted != 0 {
		t.Errorf("%+v — a blank line is not a change to the code", d)
	}
}

// A first capture names every file in the repository. Re-parsing all of them on
// a watcher tick is a build, not a meter — and a silent zero there would read as
// "nothing is being written", which is both wrong and reassuring.
func TestDriftBeyondTheBudgetIsReportedUnmeasuredRatherThanZero(t *testing.T) {
	s, _ := testServer(t)
	s.Bundle.Symbols = nil
	for i := 0; i < driftFileBudget+1; i++ {
		s.Bundle.Symbols = append(s.Bundle.Symbols, bundle.Symbol{
			ID:          bundle.SymbolID(fmt.Sprintf("f%d.go::X", i)),
			File:        fmt.Sprintf("f%d.go", i),
			Fingerprint: "fp",
		})
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := s.debt()
	if d.Unmeasured == "" {
		t.Errorf("%+v — the cap must be reported, not applied silently", d)
	}
	if d.Drifted != 0 {
		t.Errorf("drifted = %d, want no number offered at all", d.Drifted)
	}
}

// A page whose script does not parse is a blank page. The undefined-call check
// above passed happily on a file with a duplicated function header that no
// browser could load, because it looks for missing names and nothing else.
//
// There is no JavaScript parser in the standard library and this tool does not
// take dependencies, so this is the cheap half of one: with comments, strings
// and regex literals blanked out, every bracket must close, in order, before the
// file ends. That will not catch every syntax error, but it catches the ones an
// editing mistake actually produces — a lost brace, a doubled header, a
// half-applied replacement.
func TestEveryScriptHasBalancedBrackets(t *testing.T) {
	for _, name := range []string{
		"assets/code.js", "assets/view.js", "assets/flow.js",
		"assets/landscape.js", "assets/probe.js",
	} {
		src, err := assets.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		js := stripCode(string(src))

		var stack []rune
		line := 1
		closers := map[rune]rune{')': '(', ']': '[', '}': '{'}
		for _, c := range js {
			switch c {
			case '\n':
				line++
			case '(', '[', '{':
				stack = append(stack, c)
			case ')', ']', '}':
				if len(stack) == 0 {
					t.Errorf("%s:%d: %q closes nothing", name, line, c)
					return
				}
				if got := stack[len(stack)-1]; got != closers[c] {
					t.Errorf("%s:%d: %q closes a %q", name, line, c, got)
					return
				}
				stack = stack[:len(stack)-1]
			}
		}
		if len(stack) > 0 {
			t.Errorf("%s: %d bracket(s) never closed — the page would not load", name, len(stack))
		}
	}
}

// Code moves. A capture records where a symbol was; every edit since shifts it,
// and slicing the current file with the recorded span shows whatever now
// occupies those lines.
//
// Observed for real: handleSymbol was recorded at 485–503, server.go grew by a
// hundred lines, and the pane showed buildContext under handleSymbol's name —
// with handleSymbol's recorded arguments beside it. That is worse than showing
// nothing, because it is confidently wrong about the one thing this window
// promises: that you are looking at the code that ran.
func TestSourceComesFromWhereTheSymbolIsNowNotWhereItWas(t *testing.T) {
	s, _ := testServer(t)

	// The bundle keeps the position from capture time.
	if got := s.Bundle.Lookup("cache.go::Get").LineStart; got != 3 {
		t.Fatalf("fixture drift: bundle says Get starts at %d", got)
	}

	// The file grows above it, exactly as a session of edits does.
	grown := "package auth\n\n" +
		"func Added() string { return \"a\" }\n\n" +
		"func AlsoAdded() string { return \"b\" }\n\n" +
		strings.TrimPrefix(source, "package auth\n\n")
	if err := os.WriteFile(filepath.Join(s.Cfg.Root, "cache.go"), []byte(grown), 0o644); err != nil {
		t.Fatal(err)
	}

	pc := s.buildContext("cache.go::Get")
	if !strings.Contains(pc.Source, `return key + "!"`) {
		t.Errorf("source = %q\nwant the body of Get, wherever it now lives", pc.Source)
	}
	if strings.Contains(pc.Source, "func Added") || strings.Contains(pc.Source, "func AlsoAdded") {
		t.Error("the pane is showing the code that moved into Get's recorded lines")
	}
	// The line the reader is told about has to be the line it is on now, or
	// every click that carries a line — identifier resolution, the call-site
	// highlight — resolves against the wrong function.
	if pc.Symbol_.LineStart == 3 {
		t.Error("the reported line is still the recorded one; it moved")
	}
	// And it still knows the session changed this symbol: where it is and what
	// the session did with it are different questions.
	if !pc.Changed {
		t.Error("a symbol the bundle holds is still a changed symbol")
	}
}

// Drift is worth saying out loud. Anything the capture recorded by line number
// against a symbol that has since been edited — the risk markers above all — is
// describing code that has moved.
func TestAnEditedSymbolIsReportedAsDrifted(t *testing.T) {
	s, _ := testServer(t)
	// Pin the bundle to what is actually on disk. The fixture carries a stand-in
	// fingerprint, so the starting point has to be agreement or every symbol
	// reads as drifted before anything is touched.
	cur, ok := s.currentSymbol("cache.go::Get")
	if !ok {
		t.Fatal("the fixture no longer parses")
	}
	s.Bundle.Symbols[0].Fingerprint = cur.Fingerprint

	if s.buildContext("cache.go::Get").Drifted {
		t.Fatal("nothing has been edited yet")
	}
	edited := strings.Replace(source, `return key + "!"`, `return key + "?"`, 1)
	if edited == source {
		t.Fatal("the fixture changed; this edit no longer alters the body")
	}
	if err := os.WriteFile(filepath.Join(s.Cfg.Root, "cache.go"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if !s.buildContext("cache.go::Get").Drifted {
		t.Error("the body changed since the capture and nothing said so")
	}
}

// A finding is recorded as an absolute line in the file as it stood. Move the
// function and that number points at a stranger — observed here as a
// swallowed-error finding at "line 500" for a handler that had moved to 658,
// putting the finding inside a different function entirely.
func TestFindingsMoveWithTheCodeTheyWereFoundIn(t *testing.T) {
	s, _ := testServer(t)
	// The fixture's risk marker sits on line 5 of the file as captured.
	if got := s.Bundle.RisksFor("cache.go::Get"); len(got) != 1 || got[0].Line != 5 {
		t.Fatalf("fixture drift: risks = %+v", got)
	}
	// Pin the fingerprint so the body counts as unchanged; only its position moves.
	cur, ok := s.currentSymbol("cache.go::Get")
	if !ok {
		t.Fatal("the fixture no longer parses")
	}
	s.Bundle.Symbols[0].Fingerprint = cur.Fingerprint

	grown := "package auth\n\nfunc Added() string { return \"a\" }\n\n" +
		strings.TrimPrefix(source, "package auth\n\n")
	if err := os.WriteFile(filepath.Join(s.Cfg.Root, "cache.go"), []byte(grown), 0o644); err != nil {
		t.Fatal(err)
	}

	moved, ok := s.currentSymbol("cache.go::Get")
	if !ok {
		t.Fatal("Get vanished")
	}
	shift := moved.LineStart - 3 // the fixture captured Get at line 3
	if shift == 0 {
		t.Fatal("the edit did not move it")
	}
	risks := s.buildContext("cache.go::Get").Risks
	if len(risks) != 1 || risks[0].Line != 5+shift {
		t.Errorf("finding at line %d, want %d — it should move with the code",
			risks[0].Line, 5+shift)
	}
}

// When the body itself changed, no arithmetic can place the finding. Saying so
// beats pointing confidently at a line that means nothing now.
func TestAFindingInEditedCodeAdmitsItLostItsPlace(t *testing.T) {
	s, _ := testServer(t)
	cur, ok := s.currentSymbol("cache.go::Get")
	if !ok {
		t.Fatal("the fixture no longer parses")
	}
	s.Bundle.Symbols[0].Fingerprint = cur.Fingerprint

	edited := strings.Replace(source, `return key + "!"`, `return key + "?"`, 1)
	if err := os.WriteFile(filepath.Join(s.Cfg.Root, "cache.go"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	risks := s.buildContext("cache.go::Get").Risks
	if len(risks) != 1 {
		t.Fatalf("risks = %+v", risks)
	}
	if risks[0].Line != 0 {
		t.Errorf("line = %d, want it withheld rather than guessed", risks[0].Line)
	}
	if !strings.Contains(risks[0].Note, "no longer applies") {
		t.Errorf("note = %q, want it to say why the line is missing", risks[0].Note)
	}
}

// The answer comes from a model, so it is untrusted text. The renderer must
// build it from text nodes rather than assigning innerHTML, or a stray angle
// bracket in an explanation becomes markup in the page.
func TestTheAnswerRendererNeverAssignsMarkup(t *testing.T) {
	src, err := assets.ReadFile("assets/probe.js")
	if err != nil {
		t.Fatal(err)
	}
	js := stripCode(string(src))
	start := strings.Index(js, "function markdown(")
	if start < 0 {
		t.Fatal("the markdown renderer is gone; this test needs rewriting")
	}
	end := strings.Index(js[start:], "\nfunction lineLink(")
	if end < 0 {
		end = len(js) - start
	}
	body := js[start : start+end]

	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(body, banned) {
			t.Errorf("the renderer uses %s; model output would become markup", banned)
		}
	}
	if !strings.Contains(body, "createTextNode") {
		t.Error("the renderer should be building text nodes")
	}
}
