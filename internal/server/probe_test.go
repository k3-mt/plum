package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/k3-mt/plum/internal/probe"
)

// Saving three times in a second must not start three instrumented test runs,
// and must not drop the last one either: the run in flight is already reading
// code that has been superseded.
func TestRunsCoalesceRatherThanStackOrDrop(t *testing.T) {
	s, _ := testServer(t)
	s.Probe = &probe.Probe{ID: "aaaa", Test: "TestThing", Command: "true"}

	release := make(chan struct{})
	s.RunProbe = func(ctx context.Context, p *probe.Probe) (*ProbeRun, error) {
		<-release
		return &ProbeRun{Passed: true}, nil
	}

	done := make(chan struct{})
	go func() { s.Run(context.Background(), "first"); close(done) }()

	// Wait until the first run is genuinely in flight before piling on.
	for i := 0; i < 1000; i++ {
		s.runner.mu.Lock()
		running := s.runner.running
		s.runner.mu.Unlock()
		if running {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// Three saves while it is busy. They should become exactly one more run.
	for _, why := range []string{"save one", "save two", "save three"} {
		s.Run(context.Background(), why)
	}
	s.runner.mu.Lock()
	pending := s.runner.pending
	s.runner.mu.Unlock()
	if pending != "save three" {
		t.Errorf("pending = %q, want the most recent reason — the earlier ones are superseded", pending)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run loop never finished")
	}

	s.runner.mu.Lock()
	defer s.runner.mu.Unlock()
	if s.runner.running {
		t.Error("still marked running after the queue drained")
	}
	if s.runner.pending != "" {
		t.Errorf("pending = %q after draining", s.runner.pending)
	}
	if s.runner.last == nil {
		t.Error("no result kept")
	}
}

// A window with no probe is a session window. Asking it about a probe must say
// so rather than returning an empty run that reads like a test that did nothing.
func TestASessionWindowHasNoProbeEndpoints(t *testing.T) {
	s, _ := testServer(t)
	for _, path := range []string{"/api/probe", "/api/probe/run", "/api/probe/fixture"} {
		rec := recorderFor(s, path)
		if rec.Code != 404 {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
	}
}

// Nothing has run yet is a different thing from a test that recorded nothing,
// and the page has to be able to tell them apart.
func TestBeforeTheFirstRunTheWindowSaysSo(t *testing.T) {
	s, _ := testServer(t)
	s.Probe = &probe.Probe{ID: "aaaa", Test: "TestThing", Command: "true"}
	s.RunProbe = func(ctx context.Context, p *probe.Probe) (*ProbeRun, error) {
		return &ProbeRun{Passed: true}, nil
	}
	run := decodeProbe(t, s, "/api/probe")
	if run.Why != "not run yet" {
		t.Errorf("why = %q", run.Why)
	}
	if run.Test != "TestThing" {
		t.Errorf("test = %q — the page needs the name before there is a result", run.Test)
	}
}

func recorderFor(s *Server, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func decodeProbe(t *testing.T, s *Server, path string) ProbeRun {
	t.Helper()
	rec := recorderFor(s, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s = %d", path, rec.Code)
	}
	var run ProbeRun
	if err := json.NewDecoder(rec.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	return run
}
