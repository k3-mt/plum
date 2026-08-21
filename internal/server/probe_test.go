package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/k3-mt/plum/internal/ask"
	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/probe"
	"github.com/k3-mt/plum/internal/trace"
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

// A window with neither a probe nor a picker is a session window. Asking it
// about a probe must say so rather than returning an empty run that reads like
// a test that did nothing.
func TestASessionWindowHasNoProbeEndpoints(t *testing.T) {
	s, _ := testServer(t)
	for _, path := range []string{"/api/probe", "/api/probe/run", "/api/probe/fixture", "/api/tests"} {
		rec := recorderFor(s, path)
		if rec.Code != 404 {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
	}
}

// A window that can choose a test is a test window before anything is chosen.
// Serving the session page until a selection was made showed the reader the
// wrong page and hid the picker that would have fixed it.
func TestAPickerWindowIsATestWindowBeforeAnythingIsChosen(t *testing.T) {
	s, _ := testServer(t)
	s.Discover = func() ([]probe.Test, error) { return nil, nil }

	rec := recorderFor(s, "/")
	if !strings.Contains(rec.Body.String(), "id=\"testlist\"") {
		t.Error("/ served the session page; a window that can pick tests must show the picker")
	}

	run := decodeProbe(t, s, "/api/probe")
	if run.Why != "nothing chosen yet" {
		t.Errorf("why = %q, want an empty state rather than an error", run.Why)
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

// Choosing a test must answer at once.
//
// Running the test inside the request was the original design, and Run drains
// the whole queue before returning — so whichever request arrived first ended up
// executing everybody else's tests while holding its connection open. Past what
// the client would wait for, that request returns nothing at all, and nothing at
// all is what a blank window looks like. Measured before the fix: four failures
// in eight rapid selections.
func TestChoosingATestAnswersWithoutWaitingForTheRun(t *testing.T) {
	s, _ := testServer(t)
	s.Discover = func() ([]probe.Test, error) { return nil, nil }
	s.Mint = func(test string) (*probe.Probe, error) {
		return &probe.Probe{ID: "aaaa", Test: test, Command: "true"}, nil
	}
	// A run that never finishes. The request must not be waiting on it.
	block := make(chan struct{})
	defer close(block)
	s.RunProbe = func(ctx context.Context, p *probe.Probe) (*ProbeRun, error) {
		<-block
		return &ProbeRun{Passed: true}, nil
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/probe/select",
			strings.NewReader(`{"test":"TestThing"}`))
		s.mux.ServeHTTP(rec, req)
		done <- rec
	}()

	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		var run ProbeRun
		if err := json.NewDecoder(rec.Body).Decode(&run); err != nil {
			t.Fatal(err)
		}
		// Something, and something true: the test it is running and that it is.
		if run.Test != "TestThing" {
			t.Errorf("test = %q", run.Test)
		}
		if run.Why != "running" {
			t.Errorf("why = %q, want the honest in-progress state", run.Why)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("choosing a test waited for the run to finish — that is the bug")
	}
}

// What makes this worth more than pasting into a chat window is what travels
// with the question. If the recorded values are missing, the model is answering
// about code in general rather than about this run — so they have to be there,
// and when they are not that has to be said rather than assumed away.
func TestExplainCarriesTheRunsRecordedValues(t *testing.T) {
	s, _ := testServer(t)
	s.Probe = &probe.Probe{ID: "aaaa", Test: "TestThing", Command: "true"}
	s.runner.last = &ProbeRun{
		Test: "TestThing",
		Landscape: trace.Landscape{Wells: []trace.Well{
			{Symbol: "cache.go::Get", Label: "Get"},
		}},
		Values: map[int]FrameValues{
			0: {
				Args:   []NamedValue{{Name: "key", Value: `"user:42"`, After: `"user:99"`}},
				Result: `"user:42!"`,
			},
		},
	}

	req := explainRequest{Symbol: "cache.go::Get", Selection: "return key + \"!\""}
	system, user := s.explainPrompt(req)

	for _, want := range []string{
		`return key + "!"`,      // the selection itself
		`"user:42"`,             // what the run passed in
		`"user:99"`,             // and what the caller held afterwards
		"changed it",            // said in words, not left to be noticed
		`"user:42!"`,            // what came back
		"TestThing",             // which run this was
		"Get returns the token", // the author's own words
	} {
		if !strings.Contains(user, want) {
			t.Errorf("the question does not carry %q", want)
		}
	}
	// The two instructions that keep an answer honest: name a gap rather than
	// fill it, and do not invent one where the evidence is complete.
	if !strings.Contains(system, "Unsettled:") {
		t.Error("the system prompt should require a named gap rather than an invented answer")
	}
	if !strings.Contains(system, "do not manufacture doubt") {
		t.Error("the system prompt should forbid hedging when the evidence settles it")
	}
	if s.evidenceFor(req) == "" {
		t.Error("evidence should be reported as present")
	}
}

// A frame the run never entered has no recorded values, and an explanation from
// the text alone is a different thing. Conflating the two is how a plausible
// guess gets filed as evidence.
func TestExplainSaysWhenItHasNoRecordedValues(t *testing.T) {
	s, _ := testServer(t)
	s.Probe = &probe.Probe{ID: "aaaa", Test: "TestThing"}
	s.runner.last = &ProbeRun{Test: "TestThing"}

	req := explainRequest{Symbol: "cache.go::Get", Selection: "return key"}
	if ev := s.evidenceFor(req); ev != "" {
		t.Errorf("evidence = %q, want none for a frame that never ran", ev)
	}
	_, user := s.explainPrompt(req)
	if strings.Contains(user, "What the run recorded here") {
		t.Error("the question claims recorded values it does not have")
	}
}

// A question nothing has picked up yet is pending, not failed.
//
// An agent in an IDE has no tmux pane and never will, which is a completely
// ordinary way to work. The protocol never needed a pane: the question is a
// file, and any agent with the repository open can answer it. Calling that an
// error told a whole class of user their setup was broken when it was not.
func TestAQuestionNothingHasPickedUpIsPendingRatherThanFailed(t *testing.T) {
	s, _ := testServer(t)
	s.Ask = ask.NewStore(s.Cfg.Root)
	s.Provider = nil // no model, no tmux: the case that used to be a dead end

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/explain",
		strings.NewReader(`{"symbol":"cache.go::Get","selection":"return key"}`)))
	var out explainResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "pending" || out.Route != "file" {
		t.Fatalf("status=%q route=%q, want a written-down question waiting", out.Status, out.Route)
	}
	// The one line that points any agent at it — both paths, so the agent needs
	// nothing else and asks nothing back.
	if !strings.Contains(out.Instruction, out.BriefPath) ||
		!strings.Contains(out.Instruction, ".answer.md") {
		t.Errorf("instruction = %q, want it to name the brief and where to answer", out.Instruction)
	}
	if _, err := os.ReadFile(s.Ask.PromptPath(out.ID)); err != nil {
		t.Errorf("the brief is not on disk: %v", err)
	}
}

// The agent already running in a tmux pane is the right thing to ask: it has the
// repository open, answers with the developer's own tools and quota, and is
// often the session that wrote the code. The exchange is a file on disk, so a
// question survives the window being closed and can be answered from anywhere.
func TestExplainHandsTheQuestionToTheAgentAndWaitsForTheFile(t *testing.T) {
	s, _ := testServer(t)
	s.Ask = ask.NewStore(s.Cfg.Root)
	s.Bridge = &ask.Tmux{Target: "test:0"}
	s.Probe = &probe.Probe{ID: "aaaa", Test: "TestThing"}

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/explain",
		strings.NewReader(`{"symbol":"cache.go::Get","selection":"return key","from_line":5,"to_line":5}`)))
	var out explainResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	// tmux is not running in a test, so sending fails — but the brief must be on
	// disk regardless. A question that only existed inside a send-keys call
	// would be lost by the thing most likely to go wrong.
	if out.ID == "" {
		t.Fatal("no question id; the brief was never written")
	}
	brief, err := os.ReadFile(s.Ask.PromptPath(out.ID))
	if err != nil {
		t.Fatalf("the brief is not on disk: %v", err)
	}
	for _, want := range []string{"return key", "Get returns the token", "cache.go::Get"} {
		if !strings.Contains(string(brief), want) {
			t.Errorf("the brief does not carry %q", want)
		}
	}
	// A dead end that hands you the thing you were trying to send is not a dead
	// end: the path to the brief and the brief itself both come back, so the
	// page can offer it for pasting anywhere.
	if out.Status == "failed" {
		if !strings.Contains(out.BriefPath, ask.Dir) {
			t.Errorf("brief_path = %q, want where the question is waiting", out.BriefPath)
		}
		if !strings.Contains(out.Brief, "return key") {
			t.Error("the brief itself should come back, so it can be copied elsewhere")
		}
	}

	// Polling before an answer exists is pending, not an error.
	if got := decodeExplain(t, s, "/api/explain/"+out.ID); got.Status != "pending" {
		t.Errorf("status = %q before the agent answers", got.Status)
	}

	// The agent writes its answer as a file, and that is the whole protocol.
	if err := os.WriteFile(s.Ask.AnswerPath(out.ID), []byte("It returns the key with a bang."), 0o644); err != nil {
		t.Fatal(err)
	}
	got := decodeExplain(t, s, "/api/explain/"+out.ID)
	if got.Status != "answered" || !strings.Contains(got.Answer, "bang") {
		t.Errorf("after the answer landed: %+v", got)
	}
}

func decodeExplain(t *testing.T, s *Server, path string) explainResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var out explainResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A configured model is offered, not spent.
//
// A developer running their own agent has already paid for it. Quietly billing
// their API instead, because plum could not find a pane, is not plum's call to
// make — so the window says a model is available and waits for them to choose.
func TestAModelIsOfferedRatherThanSpentWhenNoAgentAnswers(t *testing.T) {
	s, _ := testServer(t)
	s.Ask = ask.NewStore(s.Cfg.Root)
	s.Bridge = &ask.Tmux{Target: "nosuch:0"} // sending will fail
	s.Provider = &stubProvider{answer: "It returns the key with a bang."}

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/explain",
		strings.NewReader(`{"symbol":"cache.go::Get","selection":"return key"}`)))
	var out explainResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "pending" {
		t.Fatalf("status = %q, want the question to wait rather than bill an API call", out.Status)
	}
	if out.Answer != "" {
		t.Error("it answered from the API without being asked to")
	}
	if !out.CanAskAPI {
		t.Error("a configured model should be offered")
	}

	// And taking the offer works, using the brief already on disk.
	got := decodeExplain(t, s, "/api/explain-api/"+out.ID)
	if got.Status != "answered" || !strings.Contains(got.Answer, "bang") {
		t.Errorf("asking the API explicitly: %+v", got)
	}
}

// stubProvider stands in for the API so the fallback can be tested without a key.
type stubProvider struct{ answer string }

func (s *stubProvider) Name() string { return "stub" }
func (s *stubProvider) Complete(ctx context.Context, system, user string) (string, error) {
	return s.answer, nil
}

// For a function whose branches differ by what they call rather than by what
// they return, the callees are the deciding evidence.
//
// Asked about Set.Meet — which returns nil from an early return and from a
// successful write — the recorded "returned nil" could not say which path ran.
// The answer was one frame down, where saveLocked either appears or does not.
func TestTheBriefCarriesWhatTheFrameWentOnToCall(t *testing.T) {
	s, _ := testServer(t)
	s.Probe = &probe.Probe{ID: "aaaa", Test: "TestThing"}
	s.runner.last = &ProbeRun{
		Test: "TestThing",
		Landscape: trace.Landscape{Wells: []trace.Well{
			{Symbol: "met.go::Set.Meet", Label: "Set.Meet", Depth: 0},
			{Symbol: "met.go::Set.saveLocked", Label: "Set.saveLocked", Depth: 1},
			{Symbol: "met.go::Set.Meet", Label: "Set.Meet", Depth: 0, Phase: "resume"},
			{Symbol: "met.go::Set.other", Label: "Set.other", Depth: 1},
			// A sibling of Meet, not a callee: it must not be swept in.
			{Symbol: "met.go::Elsewhere", Label: "Elsewhere", Depth: 0},
			{Symbol: "met.go::NotMine", Label: "NotMine", Depth: 1},
		}},
		Values: map[int]FrameValues{
			0: {Args: []NamedValue{{Name: "id", Value: `"cache.go::Get"`}}, Result: "nil"},
			1: {Result: "nil"},
			3: {Args: []NamedValue{{Name: "n", Value: "2"}}, Result: "true"},
		},
	}

	got := s.evidenceFor(explainRequest{Symbol: "met.go::Set.Meet", Selection: "x"})
	for _, want := range []string{
		"It then called",
		"`Set.saveLocked`, which returned `nil`", // the branch-deciding fact
		"`Set.other` with n = `2`, which returned `true`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the evidence does not carry %q:\n%s", want, got)
		}
	}
	// A frame at the same depth as Meet is a sibling; its callees are not Meet's.
	if strings.Contains(got, "NotMine") {
		t.Errorf("a sibling's callee was swept in:\n%s", got)
	}
	if strings.Contains(got, "Elsewhere") {
		t.Errorf("a sibling frame was reported as a callee:\n%s", got)
	}
}

// A frame with forty calls under it would bury the fragment being asked about.
// What is left out is counted rather than dropped in silence.
func TestTheCalleeListIsBoundedAndSaysWhatItLeftOut(t *testing.T) {
	s, _ := testServer(t)
	s.Probe = &probe.Probe{ID: "aaaa", Test: "TestThing"}
	wells := []trace.Well{{Symbol: "a.go::Top", Label: "Top", Depth: 0}}
	for i := 0; i < calleeBudget+5; i++ {
		wells = append(wells, trace.Well{
			Symbol: bundle.SymbolID(fmt.Sprintf("a.go::C%d", i)),
			Label:  fmt.Sprintf("C%d", i), Depth: 1,
		})
	}
	s.runner.last = &ProbeRun{Test: "TestThing", Landscape: trace.Landscape{Wells: wells},
		Values: map[int]FrameValues{0: {Result: "nil"}}}

	got := s.evidenceFor(explainRequest{Symbol: "a.go::Top", Selection: "x"})
	if !strings.Contains(got, "and 5 more calls, not listed") {
		t.Errorf("the cap was applied silently:\n%s", got)
	}
	if strings.Contains(got, "C"+fmt.Sprint(calleeBudget+1)) {
		t.Error("more than the budget was listed")
	}
}

// The same call made twice in a row is one line with a count: repetition is a
// fact about the loop, not eight facts about the callee.
func TestRepeatedCallsCollapseWithACount(t *testing.T) {
	s, _ := testServer(t)
	s.Probe = &probe.Probe{ID: "aaaa", Test: "TestThing"}
	s.runner.last = &ProbeRun{
		Test: "TestThing",
		Landscape: trace.Landscape{Wells: []trace.Well{
			{Symbol: "a.go::Top", Label: "Top", Depth: 0},
			{Symbol: "a.go::rank", Label: "rank", Depth: 1},
			{Symbol: "a.go::rank", Label: "rank", Depth: 1},
			{Symbol: "a.go::rank", Label: "rank", Depth: 1},
		}},
		Values: map[int]FrameValues{0: {Result: "nil"}, 1: {Result: "1"}},
	}
	got := s.evidenceFor(explainRequest{Symbol: "a.go::Top", Selection: "x"})
	if !strings.Contains(got, "`rank`, 3 times in a row,") {
		t.Errorf("repeats were not collapsed:\n%s", got)
	}
}

// A leaf calls nothing, and the brief should not invent a heading for it.
func TestALeafFrameCarriesNoCalleeSection(t *testing.T) {
	s, _ := testServer(t)
	s.Probe = &probe.Probe{ID: "aaaa", Test: "TestThing"}
	s.runner.last = &ProbeRun{
		Test:      "TestThing",
		Landscape: trace.Landscape{Wells: []trace.Well{{Symbol: "a.go::Leaf", Label: "Leaf"}}},
		Values:    map[int]FrameValues{0: {Result: "nil"}},
	}
	if got := s.evidenceFor(explainRequest{Symbol: "a.go::Leaf", Selection: "x"}); strings.Contains(got, "It then called") {
		t.Errorf("a leaf was given a callee section:\n%s", got)
	}
}
