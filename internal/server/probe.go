package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/k3-mt/plum/internal/probe"
	"github.com/k3-mt/plum/internal/trace"
)

// A probe run is the loop this window exists for: run one test, draw how it
// propagated, change the code, run it again. Everything else the page could
// show is a different question, and asking four questions at once is how the
// last version of this page stopped answering any of them.
type ProbeRun struct {
	Handle  string `json:"handle"`
	Test    string `json:"test"`
	Command string `json:"command"`
	Fixture string `json:"fixture,omitempty"`
	// FixtureBody travels with the run so the sample data sits beside the
	// picture it produced, rather than in another window you have to go and find.
	FixtureBody string `json:"fixture_body"`
	FixtureErr  string `json:"fixture_error,omitempty"`

	Passed bool   `json:"passed"`
	Output string `json:"output,omitempty"`
	// Recorded is how many events the run produced. It is the difference
	// between "the test ran and never entered your code" and "the test never
	// ran at all" — a build failure, a bad command, a missing dependency. Both
	// draw an empty picture, and blaming the wrong one sends you looking in the
	// wrong place.
	Recorded   int    `json:"recorded"`
	DurationMS int64  `json:"duration_ms"`
	At         string `json:"at"`
	// Why says what caused this run: you asked, or a file changed. A picture
	// that redrew itself must say so, or you are reading a result for code you
	// have not looked at yet.
	Why string `json:"why"`

	Landscape trace.Landscape `json:"landscape"`
	Narration []trace.Step    `json:"narration"`
	Summary   string          `json:"summary"`
	Error     string          `json:"error,omitempty"`
}

// runner serialises probe runs and coalesces the ones that pile up behind a
// fast typist. A save while a run is in flight must not start a second copy of
// an instrumented test suite, and must not be dropped either: the run in flight
// is already reading stale code.
type runner struct {
	mu      sync.Mutex
	running bool
	pending string // why the coalesced run is wanted, "" when none
	last    *ProbeRun
}

func (s *Server) probeRun() *ProbeRun {
	s.runner.mu.Lock()
	defer s.runner.mu.Unlock()
	return s.runner.last
}

// Run executes the probe, or notes that another run should follow the one
// already going. It returns when this call's work is done, not when the queue is.
func (s *Server) Run(ctx context.Context, why string) {
	if s.Probe == nil || s.RunProbe == nil {
		return
	}
	s.runner.mu.Lock()
	if s.runner.running {
		s.runner.pending = why
		s.runner.mu.Unlock()
		return
	}
	s.runner.running = true
	s.runner.mu.Unlock()

	for {
		out := s.execute(ctx, why)
		s.runner.mu.Lock()
		s.runner.last = out
		next := s.runner.pending
		s.runner.pending = ""
		if next == "" {
			s.runner.running = false
			s.runner.mu.Unlock()
			s.hub.broadcast("probe")
			return
		}
		s.runner.mu.Unlock()
		// Tell the page about the run that just finished even though another is
		// starting: on a long edit the intermediate results are the feedback.
		s.hub.broadcast("probe")
		why = next
	}
}

func (s *Server) execute(ctx context.Context, why string) *ProbeRun {
	started := time.Now()
	out, err := s.RunProbe(ctx, s.Probe)
	if out == nil {
		out = &ProbeRun{}
	}
	out.Handle, out.Test, out.Command = s.Probe.Handle(), s.Probe.Test, s.Probe.Command
	out.Fixture = s.Probe.Fixture
	out.Why = why
	out.At = started.Format("15:04:05")
	if out.DurationMS == 0 {
		out.DurationMS = time.Since(started).Milliseconds()
	}
	if err != nil {
		out.Error = err.Error()
	}
	out.FixtureBody, out.FixtureErr = s.readFixture()
	return out
}

// readFixture is best effort in both directions: a probe with no fixture is
// normal, and a fixture that is not there yet is worth saying rather than
// hiding, since the agent may be about to write it.
func (s *Server) readFixture() (string, string) {
	if s.Probe == nil || s.Probe.Fixture == "" {
		return "", ""
	}
	data, err := os.ReadFile(filepath.Join(s.Cfg.Root, s.Probe.Fixture))
	if err != nil {
		return "", err.Error()
	}
	return string(data), ""
}

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	if s.Probe == nil {
		http.Error(w, "this window is not watching a probe", http.StatusNotFound)
		return
	}
	run := s.probeRun()
	if run == nil {
		// Nothing has run yet: say what is about to happen rather than drawing
		// an empty picture that looks like a test that did nothing.
		body, ferr := s.readFixture()
		run = &ProbeRun{
			Handle: s.Probe.Handle(), Test: s.Probe.Test, Command: s.Probe.Command,
			Fixture: s.Probe.Fixture, FixtureBody: body, FixtureErr: ferr,
			Why: "not run yet",
		}
	}
	writeJSON(w, run)
}

func (s *Server) handleProbeRun(w http.ResponseWriter, r *http.Request) {
	if s.Probe == nil {
		http.Error(w, "this window is not watching a probe", http.StatusNotFound)
		return
	}
	s.Run(r.Context(), "you asked")
	writeJSON(w, s.probeRun())
}

// handleFixture writes the sample data and runs immediately. Saving and running
// are one act here: the only reason to edit a fixture in this window is to find
// out what the code does with it.
func (s *Server) handleFixture(w http.ResponseWriter, r *http.Request) {
	if s.Probe == nil || s.Probe.Fixture == "" {
		http.Error(w, "this probe has no fixture", http.StatusNotFound)
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	path := filepath.Join(s.Cfg.Root, s.Probe.Fixture)
	if err := os.WriteFile(path, []byte(body.Body), 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Run(r.Context(), "you changed "+s.Probe.Fixture)
	writeJSON(w, s.probeRun())
}

// ProbeRunner is what the CLI injects. Building an instrumented run needs the
// adapter registry, the context symbols and a scratch tree — all of which the
// CLI has already assembled for `plum trace`, and none of which this package
// should learn how to assemble a second time.
type ProbeRunner func(ctx context.Context, p *probe.Probe) (*ProbeRun, error)

func (s *Server) probeSummary() string {
	if s.Probe == nil {
		return ""
	}
	return fmt.Sprintf("%s · %s", s.Probe.Handle(), s.Probe.Test)
}
