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
	// Values is what each frame was called with and what it returned, keyed by
	// its index in the landscape. It is kept apart from the narration because a
	// sentence is the wrong shape for it: eight arguments flattened into prose
	// is a paragraph nobody reads, where the same eight as rows is a glance.
	Values    map[int]FrameValues `json:"values,omitempty"`
	Narration []trace.Step        `json:"narration"`
	Summary   string              `json:"summary"`
	Error     string              `json:"error,omitempty"`
}

type FrameValues struct {
	Args   []NamedValue `json:"args,omitempty"`
	Result string       `json:"result,omitempty"`
	// Raised is what the frame panicked with, when it did. It outranks a return
	// value because there was not one.
	Raised string `json:"raised,omitempty"`
}

type NamedValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	// After is what the caller holds once the call returns, when the callee
	// changed it. Empty means it came back as it went in, which is the normal
	// case and the one worth saying nothing about.
	After string `json:"after,omitempty"`
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
	// Say it started. The page shows "running" from here rather than from the
	// moment a request came back, so a run triggered by a file changing looks
	// the same as one you asked for.
	s.hub.broadcast("probe")

	for {
		out := s.execute(ctx, why)
		s.runner.mu.Lock()
		if out != nil {
			s.runner.last = out
		}
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
	// Read once, under the lock. Selecting a test replaces this while a run is
	// starting, and a run that read the field twice could report one test's
	// result under another's name.
	s.runner.mu.Lock()
	p := s.Probe
	s.runner.mu.Unlock()
	if p == nil {
		return nil
	}
	out, err := s.RunProbe(ctx, p)
	if out == nil {
		out = &ProbeRun{}
	}
	out.Handle, out.Test, out.Command = p.Handle(), p.Test, p.Command
	out.Fixture = p.Fixture
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
	// A window with no picker and no probe has no answer to give: it is a
	// session window and this endpoint means nothing to it.
	if s.Probe == nil && s.Discover == nil {
		http.Error(w, "this window is not watching a probe", http.StatusNotFound)
		return
	}
	// Nothing chosen yet is a state the page draws, not an error. It opens on an
	// empty journey and a picker, which is the right thing to show somebody who
	// has not said what they want to look at.
	if s.Probe == nil {
		writeJSON(w, &ProbeRun{Why: "nothing chosen yet"})
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

// handleTests lists what the window can be pointed at. It rediscovers on every
// request rather than caching: a test written a moment ago is the one you are
// most likely to be looking for, and a list that needed a restart to notice it
// would be wrong exactly when it mattered.
func (s *Server) handleTests(w http.ResponseWriter, r *http.Request) {
	if s.Discover == nil {
		http.Error(w, "this window cannot discover tests", http.StatusNotFound)
		return
	}
	found, err := s.Discover()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	current := ""
	if s.Probe != nil {
		current = s.Probe.Test
	}
	writeJSON(w, map[string]any{"tests": found, "current": current})
}

// handleSelect points the window at another test and runs it. Selecting is
// running: the only reason to choose a test here is to see what it does.
func (s *Server) handleSelect(w http.ResponseWriter, r *http.Request) {
	if s.Discover == nil || s.Mint == nil {
		http.Error(w, "this window cannot switch tests", http.StatusNotFound)
		return
	}
	var body struct {
		Test string `json:"test"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, err := s.Mint(body.Test)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.runner.mu.Lock()
	s.Probe = p
	// The previous test's result must not sit under the new test's name while
	// the new one runs. Clearing it makes the window say "running" instead of
	// showing an answer to a question nobody asked.
	s.runner.last = nil
	s.runner.mu.Unlock()

	// Started, not awaited. Running the test inside the request was the design
	// mistake here: Run drains the whole queue before returning, so whichever
	// request happened to arrive first ended up executing everybody else's
	// tests while holding its connection open — and a request that outlives
	// what the client is willing to wait for is a request that returns nothing,
	// which is what a blank window looked like.
	//
	// The page already listens for results. Queue the run, answer immediately
	// with the state that is true right now, and let the answer arrive when it
	// exists.
	go s.Run(context.Background(), "you chose "+p.Test)
	writeJSON(w, s.currentOr(p, "running"))
}

// currentOr never hands back nothing.
//
// Run coalesces: if a run was already in flight, it queues this one and returns
// immediately, and the result for the test just chosen does not exist yet. The
// honest answer then is "this one, running" — not null, which is what the page
// received, and which it drew as an empty screen with no explanation.
func (s *Server) currentOr(p *probe.Probe, why string) *ProbeRun {
	if run := s.probeRun(); run != nil && run.Test == p.Test {
		return run
	}
	body, ferr := s.readFixture()
	return &ProbeRun{
		Handle: p.Handle(), Test: p.Test, Command: p.Command,
		Fixture: p.Fixture, FixtureBody: body, FixtureErr: ferr,
		Why: why,
	}
}

func (s *Server) handleProbeRun(w http.ResponseWriter, r *http.Request) {
	if s.Probe == nil {
		http.Error(w, "this window is not watching a probe", http.StatusNotFound)
		return
	}
	go s.Run(context.Background(), "you asked")
	writeJSON(w, s.currentOr(s.Probe, "running"))
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
	go s.Run(context.Background(), "you changed "+s.Probe.Fixture)
	writeJSON(w, s.currentOr(s.Probe, "running"))
}

// ProbeRunner is what the CLI injects. Building an instrumented run needs the
// adapter registry, the context symbols and a scratch tree — all of which the
// CLI has already assembled for `plum trace`, and none of which this package
// should learn how to assemble a second time.
type ProbeRunner func(ctx context.Context, p *probe.Probe) (*ProbeRun, error)

// TestFinder lists what this repository has to look at, and Minter turns one of
// those names into a probe. Both live in the CLI for the same reason the runner
// does: they need the adapter registry and the configured test command, and the
// server should not learn how to assemble those a second time.
type TestFinder func() ([]probe.Test, error)

type Minter func(test string) (*probe.Probe, error)

func (s *Server) probeSummary() string {
	if s.Probe == nil {
		return ""
	}
	return fmt.Sprintf("%s · %s", s.Probe.Handle(), s.Probe.Test)
}
