package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	return New(cfg, b, l, events, cs, "# seams", tel, nil), tel
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
	ask := func(sym string) askResponse {
		body := strings.NewReader(`{"symbol":"` + sym + `","question":"why?"}`)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/ask", body))
		var out askResponse
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := ask("cache.go::Get"); !got.Grounded || got.Unanswered {
		t.Errorf("a symbol with traces and rationale is grounded: %+v", got)
	}
	if got := ask("cache.go::Unknown"); got.Grounded || !got.Unanswered {
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
