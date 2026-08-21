package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/k3-mt/plum/internal/ask"
	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/synth"
)

// Explaining a fragment somebody pointed at.
//
// The thing that makes this worth more than pasting the code into a chat window
// is what travels with it. plum knows which frame this is, what the run actually
// passed into it, what came back, what the arguments looked like afterwards, and
// what the recorded execution did — so the model is answering about this run
// rather than about code in general. A model given only the text can tell you
// what the code would do; given the evidence it can tell you what it did.

type explainRequest struct {
	Symbol    bundle.SymbolID `json:"symbol"`
	Selection string          `json:"selection"`
	FromLine  int             `json:"from_line"`
	ToLine    int             `json:"to_line"`
	// Station is the index into the journey the reader is standing on, so the
	// recorded values that travel with the question are that frame's.
	Station int `json:"station"`
}

type explainResponse struct {
	// ID is set when the question went to an agent, which answers in its own
	// time. The page polls for it.
	ID     string `json:"id,omitempty"`
	Status string `json:"status"` // pending | answered | failed
	Answer string `json:"answer,omitempty"`
	Route  string `json:"route"` // tmux | api | none
	Target string `json:"target,omitempty"`
	// Grounded says whether recorded values went with the question. An
	// explanation from the text alone is a different thing from one that has
	// seen the run, and conflating them is how a plausible guess gets filed as
	// evidence.
	Grounded bool   `json:"grounded"`
	Error    string `json:"error,omitempty"`
	TookMS   int64  `json:"took_ms,omitempty"`
	// Note explains a route plum chose rather than the one it preferred.
	Note string `json:"note,omitempty"`
	// Brief and BriefPath are the question itself, offered when nothing could be
	// asked. A dead end that hands you the thing you were trying to send is not
	// a dead end.
	Brief     string `json:"brief,omitempty"`
	BriefPath string `json:"brief_path,omitempty"`
	// Panes are the tmux panes that could be pointed at, when the agent was not
	// recognised automatically.
	Panes []paneOption `json:"panes,omitempty"`
	// Instruction is the one line that points any agent at this question, for an
	// agent plum cannot reach — one in an IDE, in another terminal, anywhere.
	Instruction string `json:"instruction,omitempty"`
	// CanAskAPI says a model is configured, so the window can offer it as an
	// alternative to waiting rather than spending the quota unasked.
	CanAskAPI bool `json:"can_ask_api,omitempty"`
}

type paneOption struct {
	Target  string `json:"target"`
	Command string `json:"command"`
	Path    string `json:"path"`
	Title   string `json:"title,omitempty"`
	// IsAgent says whether anything in this pane can read a brief and write an
	// answer. A shell cannot: send-keys succeeds, the shell prints an error into
	// its own scrollback, and the window waits for an answer that is never coming.
	IsAgent bool `json:"is_agent"`
}

func (s *Server) handleExplain(w http.ResponseWriter, r *http.Request) {
	var req explainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Selection) == "" {
		http.Error(w, "nothing selected", http.StatusBadRequest)
		return
	}
	system, user := s.explainPrompt(req)
	grounded := s.evidenceFor(req) != ""

	// The agent already running in a tmux pane is the right thing to ask. It has
	// the repository open, it answers with the developer's own tools and quota,
	// and it is the same session that wrote the code — so the explanation comes
	// from something that has the whole picture rather than from a stranger
	// holding one fragment.
	if s.Ask != nil && s.Bridge != nil {
		id := ask.NextID(time.Now())
		areq := ask.Request{
			ID: id, SessionID: s.sessionID, Symbol: req.Symbol,
			Question:  explainQuestion(req),
			CreatedAt: time.Now().UTC(), Grounded: grounded, Route: "tmux",
		}
		// The brief carries the evidence. Whoever picks it up needs nothing else.
		if err := s.Ask.Write(areq, system+"\n\n"+user); err != nil {
			writeJSON(w, explainResponse{Status: "failed", Route: "tmux", Error: err.Error()})
			return
		}
		target, err := s.Bridge.Send(r.Context(), s.Cfg.Root, areq)
		if err != nil {
			// No pane is not a failure. Plenty of people run their agent in an
			// IDE, where there is no pane to find and never will be — and the
			// protocol never needed one: the question is a file, and any agent
			// with the repository open can answer it.
			//
			// So the question stays asked. plum keeps waiting, and the window
			// hands over the one line that points an agent at it.
			writeJSON(w, s.waiting(r.Context(), id, grounded, err.Error()))
			return
		}
		areq.Target = target
		_ = s.Ask.Write(areq, system+"\n\n"+user)
		writeJSON(w, explainResponse{
			ID: id, Status: "pending", Route: "tmux", Target: target, Grounded: grounded,
		})
		return
	}

	// No tmux at all. Same answer: leave the question where an agent can find it.
	if s.Ask != nil {
		id := ask.NextID(time.Now())
		areq := ask.Request{
			ID: id, SessionID: s.sessionID, Symbol: req.Symbol,
			Question:  explainQuestion(req),
			CreatedAt: time.Now().UTC(), Grounded: grounded, Route: "file",
		}
		if err := s.Ask.Write(areq, system+"\n\n"+user); err == nil {
			writeJSON(w, s.waiting(r.Context(), id, grounded, ""))
			return
		}
	}

	// Nowhere to leave it either: the API, if one is configured.
	out, err := s.explainViaAPI(system, user)
	if err != nil {
		writeJSON(w, explainResponse{
			Status: "failed", Route: "none", Grounded: grounded,
			Error: err.Error(), Brief: system + "\n\n" + user,
		})
		return
	}
	out.Grounded = grounded
	writeJSON(w, out)
}

// waiting is the state a question is in when it has been written down and
// nothing has picked it up yet.
//
// It is deliberately "pending" rather than "failed". The question exists, it is
// complete, and it is somewhere an agent can read it — an agent in an IDE, in
// another terminal, on another machine with the repo checked out. What is
// missing is somebody telling that agent to look, which is a thing the reader
// can do in one paste rather than a thing plum should call an error.
func (s *Server) waiting(ctx context.Context, id string, grounded bool, why string) explainResponse {
	briefPath := ask.Dir + "/" + id + ".md"
	answerPath := ask.Dir + "/" + id + ".answer.md"
	out := explainResponse{
		ID: id, Status: "pending", Route: "file", Grounded: grounded,
		BriefPath: briefPath,
		// One line, ready to paste into whatever is running. It names both files
		// so the agent needs nothing else and asks nothing back.
		Instruction: "Read " + briefPath + " and answer the question in it, writing your answer to " +
			answerPath + ". Change no source files.",
		Panes:     s.panes(ctx),
		CanAskAPI: s.Provider != nil,
	}
	if why != "" {
		out.Note = why
	}
	return out
}

// handleExplainAPI asks the model for a question already written down, when the
// reader would rather not wait for an agent. Offered rather than automatic: a
// developer running their own agent has already paid for it, and quietly
// spending their API quota instead is not plum's call to make.
func (s *Server) handleExplainAPI(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/explain-api/")
	if id == "" || s.Ask == nil {
		http.Error(w, "no such question", http.StatusNotFound)
		return
	}
	brief, err := os.ReadFile(s.Ask.PromptPath(id))
	if err != nil {
		http.Error(w, "that question is not on disk", http.StatusNotFound)
		return
	}
	out, err := s.explainViaAPI("", string(brief))
	if err != nil {
		writeJSON(w, explainResponse{ID: id, Status: "failed", Route: "api", Error: err.Error()})
		return
	}
	out.ID = id
	writeJSON(w, out)
}

// explainViaAPI is the direct route. It is the fallback rather than the default
// because the agent in the pane has the repository open and answers on the
// developer's own terms.
func (s *Server) explainViaAPI(system, user string) (explainResponse, error) {
	provider := s.Provider
	if provider == nil {
		return explainResponse{}, fmt.Errorf(
			"nothing here can answer it: no agent is running in a tmux pane, and ANTHROPIC_API_KEY is not set")
	}
	// Reasoning if the provider offers it. Asked for through the interface
	// rather than by naming a concrete type, so a caller that wants thinking
	// does not have to know which provider it is holding.
	if t, ok := provider.(synth.Thinker); ok {
		provider = t.Thinking()
	}
	started := time.Now()
	// Not the request's context: a question that takes a minute should not be
	// cancelled by a browser that gave up at thirty seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	answer, err := provider.Complete(ctx, system, user)
	if err != nil {
		return explainResponse{}, err
	}
	return explainResponse{
		Status: "answered", Answer: answer, Route: "api",
		Target: provider.Name(), TookMS: time.Since(started).Milliseconds(),
	}, nil
}

// panes lists what is running in tmux, so a reader whose agent was not
// recognised can point plum at it rather than read a message about it.
func (s *Server) panes(ctx context.Context) []paneOption {
	if s.Bridge == nil {
		return nil
	}
	found, err := ask.Panes(ctx)
	if err != nil {
		return nil
	}
	var out []paneOption
	for _, p := range found {
		// The pane plum is running in cannot answer plum's questions.
		if strings.Contains(strings.Join(p.Processes, " "), "plum watch") {
			continue
		}
		name, isAgent := p.AgentName()
		if !isAgent {
			name = p.Command
		}
		out = append(out, paneOption{
			Target: p.Target, Command: name, Path: p.Path, Title: p.Title, IsAgent: isAgent,
		})
	}
	// Agents first: they are the ones that will actually answer, and a list that
	// buries them among shells invites the choice that silently fails.
	sort.SliceStable(out, func(i, j int) bool { return out[i].IsAgent && !out[j].IsAgent })
	return out
}

// handlePane pins the pane questions go to, for the rest of this window's life.
// Editing a config file to answer a question you have already asked is the kind
// of interruption that makes a feature go unused.
func (s *Server) handlePane(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Target string `json:"target"`
		// Force accepts a pane plum does not believe can answer. Offered rather
		// than forbidden: an agent plum has not heard of is a real thing.
		Force bool `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.Bridge == nil {
		http.Error(w, "this window has no tmux bridge", http.StatusNotFound)
		return
	}
	// Refuse a pane that cannot answer, unless the reader insists. Accepting it
	// silently is the failure this exists to prevent: the send succeeds and the
	// question is never read by anything.
	if !body.Force {
		for _, p := range s.panes(r.Context()) {
			if p.Target != body.Target || p.IsAgent {
				continue
			}
			writeJSON(w, map[string]any{
				"target": body.Target, "ok": false, "is_agent": false,
				"warning": p.Target + " is running " + p.Command + ", not an agent — it cannot read " +
					"a brief or write an answer, so the question would go unanswered",
			})
			return
		}
	}
	s.mu.Lock()
	s.Bridge.Target = body.Target
	s.Bridge.Force = body.Force
	s.mu.Unlock()
	writeJSON(w, map[string]any{"target": body.Target, "ok": true})
}

// handleExplainPoll returns an agent's answer once it lands on disk.
func (s *Server) handleExplainPoll(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/explain/")
	if id == "" || s.Ask == nil {
		http.Error(w, "no such question", http.StatusNotFound)
		return
	}
	a := s.Ask.Poll(id)
	writeJSON(w, explainResponse{
		ID: id, Status: a.Status, Answer: a.Text, Error: a.Error, Route: "tmux",
	})
}

// explainQuestion is the one-line form that goes in the metadata and in front of
// the agent. The brief has the detail; this says what is being asked.
func explainQuestion(req explainRequest) string {
	where := ""
	if req.FromLine > 0 {
		where = fmt.Sprintf(" at lines %d-%d", req.FromLine, req.ToLine)
	}
	return fmt.Sprintf("Explain this selection from %s%s.", req.Symbol, where)
}

// explainPrompt assembles the question. Everything in it is something plum
// recorded or parsed; nothing is summarised by a model on the way in, because a
// summary of the evidence is not the evidence.
func (s *Server) explainPrompt(req explainRequest) (system, user string) {
	system = strings.Join([]string{
		"You explain one fragment of code that a developer has selected while looking at a recorded execution of it.",
		"",
		"You are given the fragment, the declaration around it, what the run passed in and got back, and what it went on to call.",
		"Those recorded values are evidence: prefer them over reasoning about what the code would do in general.",
		"",
		"Answer in GitHub-flavoured markdown, under 120 words. No preamble, no restating the code, no offer to help further.",
		"",
		"Use exactly this shape:",
		"",
		"**One sentence** saying what the fragment does and the job it does for the rest of the codebase — who relies on it and what would break without it. This matters more than the mechanics.",
		"",
		"Then a short bullet list, at most four items, of what the run actually shows. Cite real values in backticks.",
		"",
		"Then, only if something is genuinely unsettled, one final line beginning `Unsettled:` naming the missing evidence. Omit it entirely when the evidence is complete — do not manufacture doubt.",
		"",
		"Say it once. A reader who wants more will select more.",
	}, "\n")

	var b strings.Builder
	fmt.Fprintf(&b, "## The selection\n\nFrom %s", req.Symbol)
	if req.FromLine > 0 {
		fmt.Fprintf(&b, ", lines %d–%d", req.FromLine, req.ToLine)
	}
	fmt.Fprintf(&b, ":\n\n```\n%s\n```\n\n", req.Selection)

	s.mu.RLock()
	pc := s.buildContext(req.Symbol)
	s.mu.RUnlock()

	if pc.Source != "" {
		fmt.Fprintf(&b, "## The whole declaration it sits in\n\n```\n%s\n```\n\n", pc.Source)
	}
	if pc.Doc != "" {
		fmt.Fprintf(&b, "## What its author says it does\n\n%s\n\n", pc.Doc)
	}
	if ev := s.evidenceFor(req); ev != "" {
		b.WriteString(ev)
	}
	if len(pc.Risks) > 0 {
		b.WriteString("## Findings a predicate flagged here\n\n")
		for _, m := range pc.Risks {
			fmt.Fprintf(&b, "- line %d: %s — %s\n", m.Line, m.Kind, m.Note)
		}
		b.WriteString("\n")
	}
	b.WriteString("## The question\n\nExplain the selection.")
	return system, b.String()
}

// evidenceFor renders what the run actually recorded for the frame in view.
// Empty when nothing was recorded, which the caller reports rather than hides.
func (s *Server) evidenceFor(req explainRequest) string {
	run := s.probeRun()
	if run == nil || len(run.Values) == 0 {
		return ""
	}
	wells := run.Landscape.Wells
	idx := -1
	for i, w := range wells {
		if w.Symbol == req.Symbol {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ""
	}
	fv, ok := run.Values[idx]
	if !ok {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## What the run recorded here (test %s)\n\n", run.Test)
	for _, a := range fv.Args {
		fmt.Fprintf(&b, "- `%s` came in as `%s`\n", a.Name, a.Value)
		if a.After != "" {
			fmt.Fprintf(&b, "  - and the caller held `%s` afterwards, so this call changed it\n", a.After)
		}
	}
	if fv.Raised != "" {
		fmt.Fprintf(&b, "- it panicked with `%s`\n", fv.Raised)
	} else if fv.Result != "" {
		fmt.Fprintf(&b, "- it returned `%s`\n", fv.Result)
	} else if len(fv.Args) > 0 {
		b.WriteString("- it returned nothing; whatever it does is in the arguments above or elsewhere\n")
	}

	// What it went on to call.
	//
	// For a function whose branches differ by what they call rather than by what
	// they return, this is the deciding evidence and it was missing. Asked about
	// Set.Meet, which returns nil from both an early return and a successful
	// write, the recorded "returned nil" could not say which path ran — and the
	// answer was sitting in the next frame, where saveLocked either appears or
	// does not.
	if calls := s.calleesOf(run, idx); calls != "" {
		b.WriteString("\nIt then called, in the order the run entered them:\n\n")
		b.WriteString(calls)
	}
	b.WriteString("\n")
	return b.String()
}

// calleeBudget bounds what travels with the question. A frame with forty calls
// under it would bury the fragment being asked about; what is left out is
// counted rather than dropped in silence.
const calleeBudget = 8

// calleesOf renders the frames this one entered directly, with the values each
// was given and gave back.
func (s *Server) calleesOf(run *ProbeRun, idx int) string {
	wells := run.Landscape.Wells
	if idx < 0 || idx >= len(wells) {
		return ""
	}
	depth := wells[idx].Depth
	sym := wells[idx].Symbol

	type call struct {
		idx   int
		times int
	}
	var direct []call
	for j := idx + 1; j < len(wells); j++ {
		w := wells[j]
		if w.Depth <= depth {
			// Control coming back into this same frame is still this frame:
			// what follows a resume was called by it too.
			if w.Depth == depth && w.Symbol == sym && w.Phase == "resume" {
				continue
			}
			break
		}
		if w.Depth != depth+1 || w.Phase == "resume" {
			continue
		}
		// The same call made twice in a row is one line with a count, for the
		// same reason the journey collapses it: repetition is a fact about the
		// loop, not eight facts about the callee.
		if n := len(direct); n > 0 && wells[direct[n-1].idx].Symbol == w.Symbol {
			direct[n-1].times++
			continue
		}
		direct = append(direct, call{idx: j})
	}
	if len(direct) == 0 {
		return ""
	}

	var b strings.Builder
	for i, c := range direct {
		if i >= calleeBudget {
			fmt.Fprintf(&b, "- … and %d more calls, not listed\n", len(direct)-calleeBudget)
			break
		}
		w := wells[c.idx]
		label := w.Label
		if label == "" {
			label = string(w.Symbol)
		}
		fmt.Fprintf(&b, "- `%s`", label)
		if c.times > 1 {
			fmt.Fprintf(&b, ", %d times in a row,", c.times+1)
		}
		fv, ok := run.Values[c.idx]
		if !ok {
			b.WriteString(" — nothing was recorded for it\n")
			continue
		}
		var args []string
		for _, a := range fv.Args {
			args = append(args, a.Name+" = `"+a.Value+"`")
		}
		if len(args) > 0 {
			fmt.Fprintf(&b, " with %s", strings.Join(args, ", "))
		}
		switch {
		case fv.Raised != "":
			fmt.Fprintf(&b, ", which panicked with `%s`", fv.Raised)
		case fv.Result != "":
			fmt.Fprintf(&b, ", which returned `%s`", fv.Result)
		default:
			b.WriteString(", which returned nothing")
		}
		b.WriteString("\n")
		for _, a := range fv.Args {
			if a.After != "" {
				fmt.Fprintf(&b, "  - and it changed `%s` to `%s`\n", a.Name, a.After)
			}
		}
	}
	return b.String()
}
