// Package server is the explore UI (M3): net/http, embedded vanilla JS and
// inline SVG. No framework, no build step, no Electron.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kelalaike/plum/internal/ask"
	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/claims"
	"github.com/kelalaike/plum/internal/config"
	"github.com/kelalaike/plum/internal/explore"
	"github.com/kelalaike/plum/internal/interpret"
	"github.com/kelalaike/plum/internal/lang"
	"github.com/kelalaike/plum/internal/synth"
	"github.com/kelalaike/plum/internal/trace"
)

//go:embed assets
var assets embed.FS

type Server struct {
	Cfg        *config.Config
	Bundle     *bundle.Bundle
	Landscape  trace.Landscape
	Events     []trace.Event
	Claims     []claims.Claim
	Synthesis  string
	Telemetry  *explore.Store
	Provider   synth.Provider
	Ask        *ask.Store
	Bridge     *ask.Tmux
	JournalDir string
	ClaimsPath string
	// SessionDir and Adapters let the server read the stored interpretation and
	// check it against the working tree.
	SessionDir string
	Adapters   *lang.Registry
	mux        *http.ServeMux
	done       chan struct{}
}

// Config bundles what the server needs beyond the session data itself.
type Config struct {
	Ask        *ask.Store
	Bridge     *ask.Tmux
	Provider   synth.Provider
	JournalDir string
	ClaimsPath string
	SessionDir string
	Adapters   *lang.Registry
}

func New(cfg *config.Config, b *bundle.Bundle, l trace.Landscape, ev []trace.Event, cs []claims.Claim, synthesis string, tel *explore.Store, opts Config) *Server {
	s := &Server{
		Cfg: cfg, Bundle: b, Landscape: l, Events: ev, Claims: cs,
		Synthesis: synthesis, Telemetry: tel, Provider: opts.Provider,
		Ask: opts.Ask, Bridge: opts.Bridge,
		SessionDir: opts.SessionDir, Adapters: opts.Adapters,
		JournalDir: opts.JournalDir, ClaimsPath: opts.ClaimsPath,
		mux: http.NewServeMux(), done: make(chan struct{}),
	}
	sub, _ := fs.Sub(assets, "assets")
	s.mux.Handle("/", http.FileServer(http.FS(sub)))
	s.mux.HandleFunc("/api/landscape", s.handleLandscape)
	s.mux.HandleFunc("/api/symbol/", s.handleSymbol)
	s.mux.HandleFunc("/api/ask", s.handleAsk)
	s.mux.HandleFunc("/api/ask/", s.handleAskPoll)
	s.mux.HandleFunc("/api/keep", s.handleKeep)
	s.mux.HandleFunc("/api/telemetry", s.handleTelemetry)
	s.mux.HandleFunc("/api/done", s.handleDone)
	return s
}

// Serve binds a localhost port and opens a browser. Cold start is a few
// milliseconds: everything is already in memory and the assets are embedded.
func (s *Server) Serve(ctx context.Context, addr string, open bool) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	url := "http://" + ln.Addr().String()
	fmt.Println("plum explore →", url)
	fmt.Println("no score, no gate, no timer. press ctrl-c when you are done,")
	fmt.Println("or click \"I have met this code\" to unlock `plum quiz`.")

	srv := &http.Server{Handler: s.mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	if open {
		openBrowser(url)
	}
	select {
	case <-ctx.Done():
	case <-s.done:
		time.Sleep(200 * time.Millisecond) // let the response flush
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

type landscapePayload struct {
	AskRoute string `json:"ask_route"`
	// Summary and Narration say what the recording actually did, in plain
	// language composed from the evidence. A landscape names symbols; on its own
	// it does not tell you what happened.
	Summary   string       `json:"summary"`
	Narration []trace.Step `json:"narration"`
	// Interpretation is a reading, not a record: what a model made of the
	// evidence. Kept separate from everything above it, which is verified.
	Interpretation *interpretationPayload `json:"interpretation,omitempty"`
	Session        bundle.Session         `json:"session"`
	Landscape      trace.Landscape        `json:"landscape"`
	Gate           bundle.Gate            `json:"gate"`
	Synthesis      string                 `json:"synthesis"`
	Claims         []claims.Claim         `json:"claims"`
	Symbols        []bundle.Symbol        `json:"symbols"`
	Notes          []string               `json:"notes"`
	Unannotated    []string               `json:"unannotated"`
}

func (s *Server) handleLandscape(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, landscapePayload{
		AskRoute:       s.askRoute(),
		Summary:        trace.Summary(s.Landscape, s.Bundle),
		Narration:      trace.Narrate(s.Landscape, s.Bundle),
		Interpretation: s.interpretation(),
		Session:        s.Bundle.Session, Landscape: s.Landscape, Gate: s.Bundle.Gate,
		Synthesis: s.Synthesis, Claims: s.Claims, Symbols: s.Bundle.Symbols,
		Notes: s.Landscape.Notes(), Unannotated: s.Landscape.UnannotatedExpensive(),
	})
}

// PromptContext is assembled mechanically, not retrieved by search. The bundle
// already knows everything needed (spec §10.2).
type PromptContext struct {
	Symbol      bundle.SymbolID       `json:"symbol"`
	Source      string                `json:"source"`
	Signature   string                `json:"signature"`
	Doc         string                `json:"doc"`
	Invocations []trace.Event         `json:"invocations"`
	Callers     []bundle.Edge         `json:"callers"`
	Callees     []bundle.Edge         `json:"callees"`
	Risks       []bundle.RiskMarker   `json:"risks"`
	Rationale   []bundle.JournalEntry `json:"rationale"`
	Seams       []claims.Claim        `json:"seams"`
	CallSites   []bundle.CallSite     `json:"call_sites"`
	Comments    []bundle.Comment      `json:"comments"`
	// Related carries the neighbours' actual code, not just their names. A
	// question like "is this intentional?" often turns on what the caller does
	// before it calls — an edge alone cannot answer that.
	Related []RelatedSymbol `json:"related"`
}

// RelatedSymbol is a caller or callee, with enough source to be useful.
type RelatedSymbol struct {
	Symbol    bundle.SymbolID `json:"symbol"`
	Relation  string          `json:"relation"` // caller | callee
	Signature string          `json:"signature"`
	Doc       string          `json:"doc"`
	Excerpt   string          `json:"excerpt"`
}

const maxSamples = 8

func (s *Server) buildContext(sym bundle.SymbolID) PromptContext {
	b := s.Bundle
	symbol := b.Lookup(sym)
	var seams []claims.Claim
	for _, c := range s.Claims {
		if c.Symbol == sym {
			seams = append(seams, c)
		}
	}
	return PromptContext{
		Symbol:      sym,
		Related:     s.related(sym),
		Source:      s.source(symbol),
		Signature:   symbol.Signature,
		Doc:         symbol.Doc,
		Invocations: trace.For(s.Events, sym, maxSamples),
		Callers:     b.EdgesTo(sym),
		Callees:     b.EdgesFrom(sym),
		Risks:       b.RisksFor(sym),
		Rationale:   b.JournalFor(sym),
		Seams:       seams,
		CallSites:   symbol.CallSites,
		Comments:    symbol.Comments,
	}
}

const (
	maxRelated      = 6
	maxExcerptLines = 40
)

// related gathers the callers and callees of a symbol with their source, so a
// question about intent has the surrounding code to reason from. Short
// neighbours travel whole; long ones are trimmed to their signature and the
// lines around the call, because an unbounded context is its own failure.
func (s *Server) related(sym bundle.SymbolID) []RelatedSymbol {
	seen := map[bundle.SymbolID]bool{sym: true}
	var out []RelatedSymbol

	collect := func(id bundle.SymbolID, relation string) {
		if seen[id] || len(out) >= maxRelated || !s.Bundle.Has(id) {
			return
		}
		seen[id] = true
		other := s.Bundle.Lookup(id)
		r := RelatedSymbol{
			Symbol: id, Relation: relation,
			Signature: other.Signature, Doc: other.Doc,
		}
		src := s.source(other)
		lines := strings.Split(src, "\n")
		if src != "" && len(lines) <= maxExcerptLines {
			r.Excerpt = src
		} else if src != "" {
			// Too long to send whole: keep the lines around where it calls us.
			r.Excerpt = aroundCall(lines, other, sym)
		}
		out = append(out, r)
	}

	for _, e := range s.Bundle.EdgesTo(sym) {
		collect(e.From, "caller")
	}
	for _, e := range s.Bundle.EdgesFrom(sym) {
		collect(e.To, "callee")
	}
	return out
}

// aroundCall trims a long neighbour to the lines surrounding its call to target.
func aroundCall(lines []string, caller bundle.Symbol, target bundle.SymbolID) string {
	line := 0
	for _, cs := range caller.CallSites {
		if cs.Callee == target || strings.HasSuffix(string(target), "."+cs.CalleeRaw) || cs.CalleeRaw == target.Qualified() {
			line = cs.Line - caller.LineStart
			break
		}
	}
	start, end := line-5, line+6
	if start < 0 {
		start = 0
	}
	if end > len(lines) {
		end = len(lines)
	}
	if start >= end {
		return ""
	}
	return "…\n" + strings.Join(lines[start:end], "\n") + "\n…"
}

// source reads the declaration exactly as it stands in the working tree.
func (s *Server) source(sym bundle.Symbol) string {
	if sym.File == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(s.Cfg.Root, sym.File))
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	start, end := sym.LineStart-1, sym.LineEnd
	if start < 0 {
		start = 0
	}
	if end > len(lines) {
		end = len(lines)
	}
	if start >= end {
		return ""
	}
	return strings.Join(lines[start:end], "\n")
}

func (s *Server) handleSymbol(w http.ResponseWriter, r *http.Request) {
	id := bundle.SymbolID(strings.TrimPrefix(r.URL.Path, "/api/symbol/"))
	if id == "" {
		http.Error(w, "symbol id required", http.StatusBadRequest)
		return
	}
	writeJSON(w, s.buildContext(id))
}

type askRequest struct {
	Symbol   bundle.SymbolID `json:"symbol"`
	Question string          `json:"question"`
}

type askResponse struct {
	AskID      string `json:"ask_id,omitempty"`
	Status     string `json:"status"` // pending | answered | failed
	Answer     string `json:"answer,omitempty"`
	Grounded   bool   `json:"grounded"`
	Route      string `json:"route"`
	Target     string `json:"target,omitempty"`
	Unanswered bool   `json:"unanswered"`
	Error      string `json:"error,omitempty"`
}

// handleAsk routes a question. The context it carries is assembled mechanically
// from the bundle — never retrieved by a search — which is what makes the answer
// worth grounding a decision on (spec §10.2).
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	var req askRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.Telemetry.Append(explore.Event{
		SessionID: s.Bundle.Session.ID, Symbol: req.Symbol, Action: "prompt", Query: req.Question,
	})

	pc := s.buildContext(req.Symbol)
	// Grounded means the answer can come from evidence about *this* frame:
	// a recorded execution, or its own source plus something that explains it.
	// Journal rationale alone is file-level and does not qualify.
	grounded := len(pc.Invocations) > 0 ||
		(pc.Source != "" && (pc.Doc != "" || len(pc.Rationale) > 0 || len(pc.Seams) > 0))
	if !grounded {
		// A question that cannot be answered from the assembled context is itself
		// a finding: the rationale was never recorded (spec §10.2).
		_ = s.Telemetry.Append(explore.Event{
			SessionID: s.Bundle.Session.ID, Symbol: req.Symbol, Action: "unanswerable", Query: req.Question,
		})
	}

	resp := askResponse{Grounded: grounded, Unanswered: !grounded}
	ctxText := renderContext(pc)

	// Preferred route: hand the question to the agent session already running in
	// a tmux pane. It answers with the developer's own tools and quota, and the
	// answer arrives as a file plum can watch.
	if s.Ask != nil && s.Bridge != nil {
		id := ask.NextID(time.Now())
		areq := ask.Request{
			ID: id, SessionID: s.Bundle.Session.ID, Symbol: req.Symbol,
			Question: req.Question, CreatedAt: time.Now().UTC(), Grounded: grounded, Route: "tmux",
		}
		if err := s.Ask.Write(areq, ctxText); err != nil {
			resp.Status, resp.Error = "failed", err.Error()
			writeJSON(w, resp)
			return
		}
		target, err := s.Bridge.Send(r.Context(), s.Cfg.Root, areq)
		if err != nil {
			// The prompt file is on disk either way, so the question is never lost.
			resp.Status = "failed"
			resp.AskID = id
			resp.Error = err.Error() + "\n\nThe question and its full context are waiting in " +
				ask.Dir + "/" + id + ".md — answer it from any agent and it will appear here."
			writeJSON(w, resp)
			return
		}
		areq.Target = target
		_ = s.Ask.Write(areq, ctxText)
		resp.AskID, resp.Status, resp.Route, resp.Target = id, "pending", "tmux", target
		writeJSON(w, resp)
		return
	}

	// Fallbacks: a configured API provider, or the raw assembled context, which
	// is exactly what a model would have been given.
	if s.Provider != nil {
		answer, err := s.Provider.Complete(r.Context(), askSystemPrompt, ctxText+"\n\n## Question\n"+req.Question)
		if err != nil {
			resp.Status, resp.Error = "failed", err.Error()
		} else {
			resp.Status, resp.Answer, resp.Route = "answered", answer, s.providerName()
		}
		writeJSON(w, resp)
		return
	}
	resp.Status = "answered"
	resp.Route = "context-only"
	resp.Answer = "No answering route is configured, so here is the assembled context, unedited.\n\n" + ctxText
	writeJSON(w, resp)
}

const askSystemPrompt = `You answer questions about one function, grounded only in the context supplied.
The context is assembled mechanically from an AST bundle and recorded executions —
it is not a search result. If the context does not contain the answer, say exactly
what is missing and stop. Never guess intent that was not recorded. Cite recorded
invocations by their argument and return values when they support the answer.`

// handleAskPoll reports whether the agent has written its answer yet.
func (s *Server) handleAskPoll(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/ask/")
	if id == "" || s.Ask == nil {
		http.Error(w, "unknown question", http.StatusNotFound)
		return
	}
	a := s.Ask.Poll(id)
	writeJSON(w, askResponse{AskID: id, Status: a.Status, Answer: a.Text, Route: "tmux"})
}

type keepRequest struct {
	AskID   string          `json:"ask_id"`
	Symbol  bundle.SymbolID `json:"symbol"`
	Answer  string          `json:"answer"`
	Journal bool            `json:"journal"`
	Claim   bool            `json:"claim"`
	Comment bool            `json:"comment"`
}

// handleKeep turns an answer worth keeping into something durable: rationale in
// the journal, a fingerprinted claim, or a patch proposing the comment. Source
// is never edited in place.
func (s *Server) handleKeep(w http.ResponseWriter, r *http.Request) {
	var req keepRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.Ask == nil {
		http.Error(w, "no ask store configured", http.StatusBadRequest)
		return
	}
	areq := ask.Request{ID: req.AskID, Symbol: req.Symbol, Question: "asked while exploring"}
	if meta, err := s.Ask.Meta(req.AskID); err == nil {
		areq = *meta
	}
	answer := req.Answer
	if answer == "" {
		answer = s.Ask.Poll(req.AskID).Text
	}
	res, err := ask.Keep(s.Cfg.Root, s.JournalDir, s.ClaimsPath, areq, answer,
		s.Bundle.Lookup(req.Symbol),
		ask.Enrichment{Journal: req.Journal, Claim: req.Claim, Comment: req.Comment})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.Telemetry.Append(explore.Event{
		SessionID: s.Bundle.Session.ID, Symbol: req.Symbol, Action: "keep", Query: req.AskID,
	})
	writeJSON(w, res)
}

type interpretationPayload struct {
	Markdown    string `json:"markdown"`
	Provider    string `json:"provider"`
	GeneratedAt string `json:"generated_at"`
	Stale       bool   `json:"stale"`
	StaleReason string `json:"stale_reason,omitempty"`
}

// interpretation returns the stored reading for this session, if one was made,
// along with whether the code has moved under it.
func (s *Server) interpretation() *interpretationPayload {
	if s.SessionDir == "" {
		return nil
	}
	file, err := interpret.Load(s.SessionDir)
	if err != nil {
		return nil
	}
	entry, ok := file.Entries[string(interpret.ScopeSession)]
	if !ok {
		return nil
	}
	out := &interpretationPayload{
		Markdown:    entry.Markdown,
		Provider:    entry.Provider,
		GeneratedAt: entry.GeneratedAt.Format("2006-01-02 15:04"),
	}
	// Checked against the working tree, not against the bundle: a reading is
	// only useful while it still describes the code as it is now.
	current := map[bundle.SymbolID]string{}
	seen := map[string]bool{}
	for _, sym := range s.Bundle.Symbols {
		if seen[sym.File] {
			continue
		}
		seen[sym.File] = true
		src, err := os.ReadFile(filepath.Join(s.Cfg.Root, sym.File))
		if err != nil {
			continue
		}
		a := s.Adapters.For(sym.File)
		if a == nil {
			continue
		}
		parsed, err := a.ParseSymbols(sym.File, src)
		if err != nil {
			continue
		}
		for _, p := range parsed {
			current[p.ID] = p.Fingerprint
		}
	}
	for _, f := range file.Stale(current) {
		if f.Key == string(interpret.ScopeSession) {
			out.Stale = true
			out.StaleReason = strings.Join(idsToStrings(f.Moved), ", ") + " changed since this was written"
		}
	}
	return out
}

func idsToStrings(ids []bundle.SymbolID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}

// askRoute names how questions will be answered, so the UI can say so plainly
// rather than leaving the developer guessing where their question went.
func (s *Server) askRoute() string {
	if s.Ask != nil && s.Bridge != nil {
		return "tmux"
	}
	if s.Provider != nil {
		return s.providerName()
	}
	return "context-only"
}

func (s *Server) providerName() string {
	if s.Provider == nil {
		return "none"
	}
	return s.Provider.Name()
}

// AssembleContext builds the same mechanically-assembled brief the UI sends,
// for callers outside the server (the `plum ask` command).
func AssembleContext(cfg *config.Config, b *bundle.Bundle, events []trace.Event, cs []claims.Claim, sym bundle.SymbolID) string {
	s := &Server{Cfg: cfg, Bundle: b, Events: events, Claims: cs}
	return renderContext(s.buildContext(sym))
}

// AssembleContextJSON is AssembleContext as structured data, for callers that
// want to build their own prompt or index it.
func AssembleContextJSON(cfg *config.Config, b *bundle.Bundle, events []trace.Event, cs []claims.Claim, sym bundle.SymbolID) PromptContext {
	s := &Server{Cfg: cfg, Bundle: b, Events: events, Claims: cs}
	return s.buildContext(sym)
}

func renderContext(pc PromptContext) string {
	var w strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&w, f+"\n", a...) }
	p("## Symbol")
	p("%s", pc.Symbol)
	p("")
	if pc.Doc != "" {
		p("## Declaration doc")
		p("%s", pc.Doc)
		p("")
	}
	p("## Source")
	p("```%s", fenceLanguage(pc.Symbol.File()))
	p("%s", pc.Source)
	p("```")
	p("")
	if len(pc.Invocations) > 0 {
		p("## Recorded invocations (real execution, not a summary)")
		for _, e := range pc.Invocations {
			switch e.Kind {
			case "call":
				p("- call args=%v", e.Args)
			case "return":
				p("- return %s", e.Result)
			case "raise":
				p("- raised %s", e.Exception)
			}
		}
		p("")
	} else {
		p("## Recorded invocations")
		p("None. This frame was never executed by the traced test run.")
		p("")
	}
	if len(pc.Callers) > 0 || len(pc.Callees) > 0 {
		p("## Edges")
		for _, e := range pc.Callers {
			p("- called by %s", e.From)
		}
		for _, e := range pc.Callees {
			p("- calls %s", e.To)
		}
		p("")
	}
	if len(pc.Related) > 0 {
		p("## Neighbouring code")
		p("")
		for _, r := range pc.Related {
			p("### %s (%s)", r.Symbol, r.Relation)
			if r.Doc != "" {
				p("%s", r.Doc)
			}
			if r.Excerpt != "" {
				p("```%s", fenceLanguage(r.Symbol.File()))
				p("%s", r.Excerpt)
				p("```")
			} else if r.Signature != "" {
				p("`%s`", r.Signature)
			}
			p("")
		}
	}
	if len(pc.CallSites) > 0 {
		p("## Call sites and their rationale comments")
		for _, cs := range pc.CallSites {
			r := cs.Rationale
			if r == "" {
				r = "(unannotated)"
			}
			p("- line %d → %s: %s", cs.Line, cs.CalleeRaw, r)
		}
		p("")
	}
	if len(pc.Risks) > 0 {
		p("## Risk markers")
		for _, r := range pc.Risks {
			p("- %s at line %d: %s", r.Kind, r.Line, r.Note)
		}
		p("")
	}
	if len(pc.Rationale) > 0 {
		p("## Rationale recorded live")
		for _, j := range pc.Rationale {
			p("- %s", j.Rationale)
			for _, a := range j.Alternatives {
				p("  - rejected: %s", a)
			}
		}
		p("")
	} else {
		p("## Rationale recorded live")
		p("None recorded for this symbol.")
		p("")
	}
	if len(pc.Seams) > 0 {
		p("## Claims about this symbol")
		for _, c := range pc.Seams {
			kind := "assertion"
			if c.Executable {
				kind = "executable"
			}
			p("- [%s] %s", kind, c.Claim)
		}
	}
	return w.String()
}

func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	var e explore.Event
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	e.SessionID = s.Bundle.Session.ID
	if err := s.Telemetry.Append(e); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDone(w http.ResponseWriter, r *http.Request) {
	if err := s.Telemetry.MarkDone(s.Bundle.Session.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = s.Telemetry.Append(explore.Event{SessionID: s.Bundle.Session.ID, Action: "done"})
	writeJSON(w, map[string]string{"status": "ok", "next": "plum quiz " + s.Bundle.Session.ID})
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

// fenceLanguage labels the source block with the language it is actually in.
func fenceLanguage(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".py", ".pyi":
		return "python"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".rb":
		return "ruby"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "toml"
	case ".json":
		return "json"
	}
	return ""
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("content-type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
