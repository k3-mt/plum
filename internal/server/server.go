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

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/claims"
	"github.com/kelalaike/plum/internal/config"
	"github.com/kelalaike/plum/internal/explore"
	"github.com/kelalaike/plum/internal/synth"
	"github.com/kelalaike/plum/internal/trace"
)

//go:embed assets
var assets embed.FS

type Server struct {
	Cfg       *config.Config
	Bundle    *bundle.Bundle
	Landscape trace.Landscape
	Events    []trace.Event
	Claims    []claims.Claim
	Synthesis string
	Telemetry *explore.Store
	Provider  synth.Provider
	mux       *http.ServeMux
	done      chan struct{}
}

func New(cfg *config.Config, b *bundle.Bundle, l trace.Landscape, ev []trace.Event, cs []claims.Claim, synthesis string, tel *explore.Store, p synth.Provider) *Server {
	s := &Server{
		Cfg: cfg, Bundle: b, Landscape: l, Events: ev, Claims: cs,
		Synthesis: synthesis, Telemetry: tel, Provider: p,
		mux: http.NewServeMux(), done: make(chan struct{}),
	}
	sub, _ := fs.Sub(assets, "assets")
	s.mux.Handle("/", http.FileServer(http.FS(sub)))
	s.mux.HandleFunc("/api/landscape", s.handleLandscape)
	s.mux.HandleFunc("/api/symbol/", s.handleSymbol)
	s.mux.HandleFunc("/api/ask", s.handleAsk)
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
	Session     bundle.Session  `json:"session"`
	Landscape   trace.Landscape `json:"landscape"`
	Gate        bundle.Gate     `json:"gate"`
	Synthesis   string          `json:"synthesis"`
	Claims      []claims.Claim  `json:"claims"`
	Symbols     []bundle.Symbol `json:"symbols"`
	Notes       []string        `json:"notes"`
	Unannotated []string        `json:"unannotated"`
}

func (s *Server) handleLandscape(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, landscapePayload{
		Session: s.Bundle.Session, Landscape: s.Landscape, Gate: s.Bundle.Gate,
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
	Answer     string `json:"answer"`
	Grounded   bool   `json:"grounded"`
	Provider   string `json:"provider"`
	Unanswered bool   `json:"unanswered"`
}

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

	answer, err := s.answer(r.Context(), pc, req.Question)
	if err != nil {
		answer = "could not reach the synthesis provider: " + err.Error()
	}
	resp := askResponse{Answer: answer, Grounded: grounded, Provider: s.providerName()}
	if !grounded {
		// A question that cannot be answered from the assembled context is itself
		// a finding: the rationale was never recorded (spec §10.2).
		resp.Unanswered = true
		_ = s.Telemetry.Append(explore.Event{
			SessionID: s.Bundle.Session.ID, Symbol: req.Symbol, Action: "unanswerable", Query: req.Question,
		})
	}
	writeJSON(w, resp)
}

func (s *Server) providerName() string {
	if s.Provider == nil {
		return "none"
	}
	return s.Provider.Name()
}

func (s *Server) answer(ctx context.Context, pc PromptContext, question string) (string, error) {
	ctxText := renderContext(pc)
	if s.Provider == nil {
		return "No synthesis provider configured — here is the assembled context, unedited.\n\n" + ctxText, nil
	}
	system := `You answer questions about one function, grounded only in the context supplied.
The context is assembled mechanically from an AST bundle and recorded executions —
it is not a search result. If the context does not contain the answer, say exactly
what is missing and stop. Never guess intent that was not recorded. Cite recorded
invocations by their argument and return values when they support the answer.`
	return s.Provider.Complete(ctx, system, ctxText+"\n\n## Question\n"+question)
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
	p("```go")
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
