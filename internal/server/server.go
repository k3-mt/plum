// Package server is the explore UI (M3): net/http, embedded vanilla JS and
// inline SVG. No framework, no build step, no Electron.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/k3-mt/plum/internal/ask"
	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/claims"
	"github.com/k3-mt/plum/internal/config"
	"github.com/k3-mt/plum/internal/explore"
	"github.com/k3-mt/plum/internal/interpret"
	"github.com/k3-mt/plum/internal/lang"
	"github.com/k3-mt/plum/internal/lang/dbt"
	"github.com/k3-mt/plum/internal/met"
	"github.com/k3-mt/plum/internal/probe"
	"github.com/k3-mt/plum/internal/synth"
	"github.com/k3-mt/plum/internal/trace"
)

//go:embed assets
var assets embed.FS

type Server struct {
	Cfg       *config.Config
	Bundle    *bundle.Bundle
	Landscape trace.Landscape
	// Flow is the warehouse picture, when the session has one. A dbt build is a
	// DAG, not a call stack, so it gets its own drawing rather than being bent
	// into a path that invents returns nothing performed.
	Flow      *dbt.Flow
	Events    []trace.Event
	Claims    []claims.Claim
	Synthesis string
	Telemetry *explore.Store
	// Met is what the debt meter is measured against: which symbols this reader
	// has seen, at which version. Absent for an export, which has no reader.
	Met        *met.Set
	Provider   synth.Provider
	Ask        *ask.Store
	Bridge     *ask.Tmux
	JournalDir string
	ClaimsPath string
	// SessionDir and Adapters let the server read the stored interpretation and
	// check it against the working tree.
	SessionDir string
	Adapters   *lang.Registry
	// TestFilter narrows the view to one test's path, and is remembered so a
	// live reload does not silently widen it back to the whole recording.
	TestFilter string
	// sessions, when set, is what makes the window follow the repository rather
	// than one recording: the watcher asks it for the newest session and rebinds.
	sessions  Sessions
	follow    bool
	sessionID string
	// resident keeps the server up after "I have met this code". `plum explore`
	// is finished at that point; a window you left on a second screen is not.
	resident   bool
	window     bool
	profileDir string
	// mu guards the session state, which the watcher replaces underneath
	// whatever request happens to be reading it.
	mu sync.RWMutex
	// drifted memoises the working-tree comparison, which is far too expensive
	// to redo on every request for the meter.
	drifted driftCache
	// trended is where the meter has been, so it can say which way it is going.
	trended trendLog
	// Probe and RunProbe turn this window from a view of a session into a view
	// of one test: what it does, and what it does after you change something.
	Probe    *probe.Probe
	RunProbe ProbeRunner
	// Discover and Mint let the window choose its own subject: every test in
	// the repository, in a list, without going back to the command line.
	Discover TestFinder
	Mint     Minter
	runner   runner
	hub      *liveHub
	mux      *http.ServeMux
	done     chan struct{}
}

// Sessions is the part of the session store the window needs in order to follow
// a repository over time. *store.Store already satisfies it, so following costs
// no change to the store and no import of it here.
type Sessions interface {
	Latest() (string, error)
	Dir(id string) string
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
	TestFilter string
	Flow       *dbt.Flow
	// Watch reloads the page when the session or the source changes on disk.
	Watch bool
	// Sessions and Follow make the window outlive any one recording: when the
	// agent stops and a new session is captured, the window moves to it rather
	// than continuing to show a session that is no longer what you are working on.
	Sessions Sessions
	Follow   bool
	// Resident, Window and ProfileDir are what `plum watch` sets and `plum
	// explore` does not: stay up, open a frame rather than a tab, and keep that
	// frame's position in a profile of its own.
	Resident   bool
	Window     bool
	ProfileDir string
	// Probe and RunProbe make this a test window rather than a session window;
	// Discover and Mint let it change which test that is.
	Probe    *probe.Probe
	RunProbe ProbeRunner
	Discover TestFinder
	Mint     Minter
	// Met carries the debt across sessions. It belongs to the reader, not to any
	// one recording, which is why it is passed in rather than read from a session.
	Met *met.Set
}

func New(cfg *config.Config, b *bundle.Bundle, l trace.Landscape, ev []trace.Event, cs []claims.Claim, synthesis string, tel *explore.Store, opts Config) *Server {
	s := &Server{
		Cfg: cfg, Bundle: b, Landscape: l, Flow: opts.Flow, Events: ev, Claims: cs,
		Synthesis: synthesis, Telemetry: tel, Provider: opts.Provider,
		Ask: opts.Ask, Bridge: opts.Bridge,
		SessionDir: opts.SessionDir, Adapters: opts.Adapters, TestFilter: opts.TestFilter,
		sessions: opts.Sessions, follow: opts.Follow && opts.Sessions != nil,
		resident: opts.Resident, window: opts.Window, profileDir: opts.ProfileDir,
		Met:        opts.Met,
		Probe:      opts.Probe,
		RunProbe:   opts.RunProbe,
		Discover:   opts.Discover,
		Mint:       opts.Mint,
		hub:        newHub(),
		JournalDir: opts.JournalDir, ClaimsPath: opts.ClaimsPath,
		mux: http.NewServeMux(), done: make(chan struct{}),
	}
	// Go's table has no entry for .webmanifest, so the file server would send it
	// as text/plain and the browser would decline to treat the page as
	// installable — the one thing the manifest exists to make possible.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
	sub, _ := fs.Sub(assets, "assets")
	files := http.FileServer(http.FS(sub))
	// A window watching a probe answers one question, so it serves one page. The
	// session page is still there under `plum explore`; it is simply not what
	// this window is for.
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// A window that can choose a test is a test window, whether or not one
		// has been chosen yet. Falling through to the session page when nothing
		// is selected showed the reader the wrong page and hid the picker that
		// would have fixed it.
		if (s.Probe != nil || s.Discover != nil) && r.URL.Path == "/" {
			r = r.Clone(r.Context())
			r.URL.Path = "/probe.html"
		}
		files.ServeHTTP(w, r)
	})
	s.mux.HandleFunc("/api/landscape", s.handleLandscape)
	s.mux.HandleFunc("/api/symbol/", s.handleSymbol)
	s.mux.HandleFunc("/api/ask", s.handleAsk)
	s.mux.HandleFunc("/api/ask/", s.handleAskPoll)
	s.mux.HandleFunc("/api/keep", s.handleKeep)
	s.mux.HandleFunc("/api/telemetry", s.handleTelemetry)
	s.mux.HandleFunc("/api/done", s.handleDone)
	s.mux.HandleFunc("/api/live", s.handleLive)
	s.mux.HandleFunc("/api/health", s.handleHealth)
	s.mux.HandleFunc("/api/debt", s.handleDebt)
	s.mux.HandleFunc("/api/probe", s.handleProbe)
	s.mux.HandleFunc("/api/probe/run", s.handleProbeRun)
	s.mux.HandleFunc("/api/probe/fixture", s.handleFixture)
	s.mux.HandleFunc("/api/tests", s.handleTests)
	s.mux.HandleFunc("/api/probe/select", s.handleSelect)
	s.mux.HandleFunc("/api/resolve", s.handleResolve)
	s.mux.HandleFunc("/api/explain", s.handleExplain)
	s.mux.HandleFunc("/api/explain/", s.handleExplainPoll)
	s.mux.HandleFunc("/api/pane", s.handlePane)
	s.mux.HandleFunc("/api/explain-api/", s.handleExplainAPI)
	if b != nil {
		s.sessionID = b.Session.ID
	}
	// A following window starts before there is anything to show: you run it
	// once and leave it there, and the first session arrives later.
	if opts.Watch && (opts.SessionDir != "" || s.follow) {
		go s.watch(s.done)
	}
	return s
}

// handleHealth lets a second `plum watch` discover the first one instead of
// racing it for the port. It names the repository because the port is derived
// from that path, and a hash collision must not silently attach the window to
// somebody else's repository.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, map[string]string{
		"plum":    "ok",
		"repo":    s.Cfg.Root,
		"session": s.sessionID,
	})
}

// debt measures the reader against the session currently in view. The caller
// holds at least a read lock.
func (s *Server) debt() met.Debt {
	if s.Met == nil {
		return met.Debt{}
	}
	drawn := make([]bundle.SymbolID, 0, len(s.Landscape.Wells))
	for _, w := range s.Landscape.Wells {
		drawn = append(drawn, w.Symbol)
	}
	d := s.Met.Of(s.Bundle, drawn)
	d.Drifted, d.Unmeasured = s.drift()
	// The trend follows the whole gap, not just the captured half: code being
	// written right now is exactly what makes the number climb while you watch.
	d.Trend = s.trended.observe(d.Unmet+d.Drifted, time.Now())
	d.TrendMinutes = int(trendWindow / time.Minute)
	return d
}

// handleDebt is the meter on its own, without the landscape around it. A window
// sitting on a second screen asks for this and nothing else, which keeps a
// glance cheap: the landscape payload carries every changed symbol in the
// session and can run to megabytes.
func (s *Server) handleDebt(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, s.debt())
}

// handleResolve says what one name means inside one declaration.
//
// It asks the language, because only the language knows. A text search over the
// same source reports a closure as a call, a package qualifier as a local, and
// has nothing at all to say about where a value came from — and those are the
// three questions a reader clicking a name is actually asking.
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	symbol := bundle.SymbolID(r.URL.Query().Get("symbol"))
	if name == "" || symbol == "" {
		http.Error(w, "name and symbol are required", http.StatusBadRequest)
		return
	}
	file := symbol.File()
	if file == "" || s.Adapters == nil {
		writeJSON(w, bundle.Resolution{Name: name, Kind: "unknown", Note: "no adapter for this file"})
		return
	}
	resolver := s.Adapters.ResolverFor(file)
	if resolver == nil {
		// Said plainly. A language whose adapter cannot walk a scope should not
		// have its reader shown a guess dressed as a fact.
		writeJSON(w, bundle.Resolution{
			Name: name, Kind: "unsupported",
			Note: "plum resolves names for " + supportedResolvers(s.Adapters) +
				" so far; for " + s.Adapters.Language(file) + " it can only show where the text appears",
		})
		return
	}
	src, err := os.ReadFile(filepath.Join(s.Cfg.Root, file))
	if err != nil {
		writeJSON(w, bundle.Resolution{Name: name, Kind: "unknown", Note: "the file is not in the working tree"})
		return
	}
	// The line the reader clicked, so the resolver answers for the scope they
	// are standing in rather than for the first binding with that name. Two
	// variables sharing a name in one function are two variables.
	line, _ := strconv.Atoi(r.URL.Query().Get("line"))
	if line == 0 {
		s.mu.RLock()
		line = s.Bundle.Lookup(symbol).LineStart
		s.mu.RUnlock()
	}
	if line == 0 {
		// The bundle may predate the symbol; the working tree is the authority
		// on where it is now.
		if syms, perr := s.Adapters.For(file).ParseSymbols(file, src); perr == nil {
			for _, sym := range syms {
				if sym.ID == symbol {
					line = sym.LineStart
				}
			}
		}
	}
	res, err := resolver.ResolveIdentifier(file, src, line, name)
	if err != nil {
		writeJSON(w, bundle.Resolution{Name: name, Kind: "unknown", Note: err.Error()})
		return
	}
	writeJSON(w, res)
}

// supportedResolvers names the languages that can answer, so the message is a
// statement about plum rather than about the reader's code.
func supportedResolvers(reg *lang.Registry) string {
	var names []string
	for _, a := range reg.All() {
		if _, ok := a.(lang.Resolver); ok {
			names = append(names, a.Name())
		}
	}
	if len(names) == 0 {
		return "no language"
	}
	return strings.Join(names, " and ")
}

// Serve binds a localhost port and opens a browser. Cold start is a few
// milliseconds: everything is already in memory and the assets are embedded.
func (s *Server) Serve(ctx context.Context, addr string, open bool) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	url := "http://" + ln.Addr().String()
	if s.resident {
		fmt.Println("plum watch →", url)
		fmt.Println("the window follows the newest session. leave it open; ctrl-c stops it.")
	} else {
		fmt.Println("plum explore →", url)
		fmt.Println("no score, no gate, no timer. press ctrl-c when you are done,")
		fmt.Println("or click \"I have met this code\" to unlock `plum quiz`.")
	}

	srv := &http.Server{Handler: s.mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	if open {
		// A frame if one can be had, a tab if not. Never nothing.
		if !s.window || !openWindow(url, s.profileDir) {
			openBrowser(url)
		}
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
	Flow           *dbt.Flow              `json:"flow,omitempty"`
	Gate           bundle.Gate            `json:"gate"`
	Synthesis      string                 `json:"synthesis"`
	Claims         []claims.Claim         `json:"claims"`
	Symbols        []bundle.Symbol        `json:"symbols"`
	Notes          []string               `json:"notes"`
	Unannotated    []string               `json:"unannotated"`
	// Debt is how far this reader's model has drifted from the changed set. It
	// travels with the landscape so the meter is correct on the first paint
	// rather than a beat later.
	Debt met.Debt `json:"debt"`
	// Resident says this page is a window someone left open rather than a tab
	// they opened to read one recording. The page uses it to decide whether it
	// is allowed to collapse to its meter: a narrow explore tab showing nothing
	// but a number would simply be broken.
	Resident bool `json:"resident"`
}

func (s *Server) handleLandscape(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, landscapePayload{
		AskRoute:       s.askRoute(),
		Summary:        trace.Summary(s.Landscape, s.Bundle),
		Narration:      trace.Narrate(s.Landscape, s.Bundle),
		Interpretation: s.interpretation(),
		Session:        s.Bundle.Session, Landscape: s.Landscape, Flow: s.Flow, Gate: s.Bundle.Gate,
		Synthesis: s.Synthesis, Claims: s.Claims, Symbols: s.Bundle.Symbols,
		Notes: s.Landscape.Notes(), Unannotated: s.Landscape.UnannotatedExpensive(),
		Debt: s.debt(), Resident: s.resident,
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
	// Symbol_ is the resolved declaration, which may have come from the working
	// tree rather than from the bundle.
	Symbol_ bundle.Symbol `json:"declaration"`
	// Changed says whether this session touched it, or the run merely passed
	// through it.
	Changed bool `json:"changed"`
	// Drifted says the symbol has been edited since the capture, so anything
	// recorded against it by line number — the risk markers especially — is
	// describing code that has since moved.
	Drifted bool `json:"drifted"`
	// Narration is what this frame did, in the sentences the landscape uses.
	Narration []trace.Step `json:"narration"`
	// Markdown is this same context rendered as a brief — what `plum context`
	// prints. It travels with the JSON so the page can put it on the clipboard
	// without assembling anything itself.
	Markdown string `json:"markdown"`
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

// symbolFor resolves a symbol to its declaration.
//
// The bundle only holds what the session changed. Since tracing began recording
// the surrounding code a run passes through, a reader can click a frame the
// bundle has never heard of — and a brief with no source, no signature and no
// doc is worse than useless, because it looks like the evidence is missing
// rather than merely unlooked-for. So an unknown symbol is parsed out of the
// working tree on demand.
// symbolFor answers two different questions that were tangled together: where
// this symbol is *now*, and whether this session changed it.
//
// It used to return the bundle's copy whenever the bundle had one, and the
// bundle's copy carries the line span from when the capture was taken. Every
// edit since moves the code and leaves that span pointing at whatever now
// occupies those lines. Observed here: handleSymbol was recorded at 485–503,
// the file has since grown, and the pane showed buildContext under
// handleSymbol's name — with handleSymbol's recorded arguments beside it.
//
// That is the worst thing this window can do. Its whole premise is "this is the
// code that ran"; showing different code under that promise is worse than
// showing nothing, because it is confidently wrong. So position and text come
// from the working tree whenever the file still parses, and only what the
// session did with the symbol comes from the bundle.
func (s *Server) symbolFor(id bundle.SymbolID) (bundle.Symbol, bool) {
	changed := s.Bundle.Has(id)
	recorded := s.Bundle.Lookup(id)
	current, ok := s.currentSymbol(id)
	if !ok {
		// Nothing to check against — a deleted file, an unparseable one, a
		// language with no adapter. The capture is all there is.
		return recorded, changed
	}
	// Change and Tested describe what the session did, which the working tree
	// cannot know. Everything else — lines, signature, doc, call sites — has to
	// describe the code as it is, because that is the code being displayed.
	current.Change = recorded.Change
	current.Tested = recorded.Tested
	return current, changed
}

// currentSymbol finds the symbol in the working tree as it stands.
func (s *Server) currentSymbol(id bundle.SymbolID) (bundle.Symbol, bool) {
	file := id.File()
	if file == "" || s.Adapters == nil {
		return bundle.Symbol{}, false
	}
	a := s.Adapters.For(file)
	if a == nil {
		return bundle.Symbol{}, false
	}
	src, err := os.ReadFile(filepath.Join(s.Cfg.Root, file))
	if err != nil {
		return bundle.Symbol{}, false
	}
	syms, err := a.ParseSymbols(file, src)
	if err != nil {
		return bundle.Symbol{}, false
	}
	for _, sym := range syms {
		if sym.ID == id {
			return sym, true
		}
	}
	return bundle.Symbol{}, false
}

// risksFor returns the recorded findings with their lines moved to where the
// code is now.
//
// A finding is recorded as an absolute line number in the file as it stood at
// capture time. Move the function and that number points at a stranger — the
// handler here was captured at 485 and now sits at 658, so its "line 500"
// finding was landing in the middle of a different function.
//
// The offset within the declaration is the part that survives, and when the
// body is unchanged — same fingerprint — shifting by the difference is exact
// rather than a guess. When the body has changed, no arithmetic can place it:
// the line is dropped to zero and the note says so, because a finding pointing
// confidently at the wrong line is worse than one that admits it lost its place.
func (s *Server) risksFor(id bundle.SymbolID) []bundle.RiskMarker {
	found := s.Bundle.RisksFor(id)
	if len(found) == 0 {
		return found
	}
	recorded := s.Bundle.Lookup(id)
	current, ok := s.currentSymbol(id)
	if !ok || recorded.LineStart == 0 || current.LineStart == 0 {
		return found
	}
	shift := current.LineStart - recorded.LineStart
	drifted := recorded.Fingerprint != "" && current.Fingerprint != recorded.Fingerprint

	out := make([]bundle.RiskMarker, 0, len(found))
	for _, m := range found {
		if drifted {
			m.Line = 0
			m.Note = strings.TrimSpace(m.Note) +
				" (recorded before this symbol was edited; the line it was on no longer applies)"
			out = append(out, m)
			continue
		}
		if m.Line > 0 {
			m.Line += shift
		}
		out = append(out, m)
	}
	return out
}

// drifted reports whether the symbol has changed since the capture. When it
// has, anything the capture recorded by line number — risk markers most of all
// — is describing code that has since moved, and saying so is the difference
// between a stale finding and a wrong one.
func (s *Server) hasDrifted(id bundle.SymbolID) bool {
	recorded := s.Bundle.Lookup(id)
	if recorded.Fingerprint == "" {
		return false
	}
	current, ok := s.currentSymbol(id)
	if !ok {
		return false
	}
	return current.Fingerprint != recorded.Fingerprint
}

func (s *Server) buildContext(sym bundle.SymbolID) PromptContext {
	b := s.Bundle
	symbol, changed := s.symbolFor(sym)
	var seams []claims.Claim
	for _, c := range s.Claims {
		if c.Symbol == sym {
			seams = append(seams, c)
		}
	}
	return PromptContext{
		Symbol:      sym,
		Symbol_:     symbol,
		Changed:     changed,
		Narration:   trace.StepsFor(s.Landscape, b, sym),
		Related:     s.related(sym),
		Source:      s.source(symbol),
		Signature:   symbol.Signature,
		Doc:         symbol.Doc,
		Invocations: trace.For(s.Events, sym, maxSamples),
		Callers:     b.EdgesTo(sym),
		Callees:     b.EdgesFrom(sym),
		Risks:       s.risksFor(sym),
		Drifted:     s.hasDrifted(sym),
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
		other, _ := s.symbolFor(id)
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
	s.mu.RLock()
	pc := s.buildContext(id)
	pc.Markdown = renderContext(pc)
	fingerprint := s.fingerprint(id)
	s.mu.RUnlock()
	// Fetching a brief is the moment the code is actually in front of the
	// reader, as opposed to being drawn as a shape with a name on it. That is
	// what the debt is counting, so that is where it is paid down.
	if s.Met != nil {
		_ = s.Met.Meet(id, fingerprint)
	}
	writeJSON(w, pc)
}

// fingerprint is the bundle's version of a symbol, not the working tree's. The
// debt is measured against the bundle, so meeting a symbol has to be recorded
// against the same thing or the number never moves.
func (s *Server) fingerprint(id bundle.SymbolID) string {
	for _, sym := range s.Bundle.Symbols {
		if sym.ID == id {
			return sym.Fingerprint
		}
	}
	return ""
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

// AssembleContext builds the same brief the UI copies to the clipboard.
func AssembleContext(in ContextInput, sym bundle.SymbolID) string {
	return renderContext(in.server().buildContext(sym))
}

// ContextInput is everything a brief is assembled from. It is a struct rather
// than a parameter list because the server and `plum context` must build the
// identical thing — a brief that differs depending on which door you came
// through is a brief nobody can rely on — and a missing argument is easier to
// notice as a missing field.
type ContextInput struct {
	Cfg       *config.Config
	Bundle    *bundle.Bundle
	Events    []trace.Event
	Claims    []claims.Claim
	Adapters  *lang.Registry
	Landscape trace.Landscape
}

func (in ContextInput) server() *Server {
	return &Server{
		Cfg: in.Cfg, Bundle: in.Bundle, Events: in.Events,
		Claims: in.Claims, Adapters: in.Adapters, Landscape: in.Landscape,
	}
}

// AssembleContextJSON is AssembleContext as structured data, for callers that
// want to build their own prompt or index it.
func AssembleContextJSON(in ContextInput, sym bundle.SymbolID) PromptContext {
	return in.server().buildContext(sym)
}

// renderContext writes the brief a reader copies to the clipboard, or an agent
// receives as a question's context.
//
// It is written to be pasted somewhere else and understood cold, so it leads
// with what the thing is and closes with what is missing about it. A brief that
// silently omits the gaps reads as though the evidence were complete.
func renderContext(pc PromptContext) string {
	var w strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&w, f+"\n", a...) }
	sym := pc.Symbol_

	kind := sym.Kind
	if kind == "" {
		kind = "symbol"
	}
	p("# %s", pc.Symbol)
	p("")
	where := sym.File
	if sym.LineStart > 0 {
		where = fmt.Sprintf("%s:%d-%d", sym.File, sym.LineStart, sym.LineEnd)
	}
	p("%s in `%s`.", strings.ToUpper(kind[:1])+kind[1:], where)
	if pc.Changed {
		p("This session changed it.")
	} else {
		p("This session did **not** change it; the traced run passed through it.")
	}
	if isTestPath(sym.File) {
		p("It lives in a test file, so it is the thing exercising the change rather than part of it.")
	}
	p("")

	if pc.Signature != "" {
		p("## Signature")
		p("```%s", fenceLanguage(sym.File))
		p("%s", pc.Signature)
		p("```")
		p("")
	}

	p("## Declaration doc")
	if pc.Doc != "" {
		p("%s", pc.Doc)
	} else {
		p("_None. Nothing records what this is for._")
	}
	p("")

	p("## Source")
	if pc.Source != "" {
		p("```%s", fenceLanguage(sym.File))
		p("%s", pc.Source)
		p("```")
	} else {
		p("_Not available: the declaration could not be located in the working tree, so it has probably moved or been deleted since this session was captured._")
	}
	p("")

	if len(pc.Narration) > 0 {
		p("## What it did in the recording")
		for _, step := range pc.Narration {
			p("- %s", step.Text)
			if step.Note != "" {
				p("    - %s", step.Note)
			}
		}
		p("")
	}

	p("## Recorded invocations")
	if len(pc.Invocations) == 0 {
		p("_None. No traced test entered this._")
	}
	for _, e := range pc.Invocations {
		test := ""
		if e.TestID != "" {
			test = fmt.Sprintf(" (during %s)", e.TestID)
		}
		switch e.Kind {
		case "call":
			if args := trace.HumanArgs(e.Args); args != "" {
				p("- called with %s%s", args, test)
			} else {
				p("- called with no arguments%s", test)
			}
		case "return":
			p("- returned %s%s", trace.HumanValue(e.Result), test)
		case "raise":
			p("- raised %s%s", trace.HumanValue(e.Exception), test)
		}
	}
	p("")

	if len(pc.Callers) > 0 || len(pc.Callees) > 0 {
		p("## Edges")
		for _, e := range pc.Callers {
			p("- called by `%s`", e.From)
		}
		for _, e := range pc.Callees {
			p("- calls `%s`", e.To)
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

	local, external := splitCallSites(pc.CallSites)
	if len(local) > 0 || len(external) > 0 {
		p("## Calls it makes, and whether anything explains them")
		for _, cs := range local {
			if cs.Rationale != "" {
				p("- line %d → `%s` — %q", cs.Line, cs.CalleeRaw, oneLineOf(cs.Rationale))
				continue
			}
			p("- line %d → `%s` — **unannotated**", cs.Line, cs.CalleeRaw)
		}
		if len(external) > 0 {
			// Unresolved calls are listed but not counted against anyone. Some
			// are library calls nobody would comment; others are interface
			// dispatch this pass cannot follow. Saying which it is would be a
			// guess, so it says neither.
			names := make([]string, 0, len(external))
			seen := map[string]bool{}
			for _, cs := range external {
				if !seen[cs.CalleeRaw] {
					seen[cs.CalleeRaw] = true
					names = append(names, "`"+cs.CalleeRaw+"`")
				}
			}
			p("- and %s this pass could not resolve to a declaration in the repository — a library, or dispatch through an interface: %s",
				plural(len(external), "call"), strings.Join(names, ", "))
		}
		p("")
	}

	if len(pc.Risks) > 0 {
		p("## Risk markers")
		for _, r := range pc.Risks {
			p("- **%s** at line %d — %s", r.Kind, r.Line, r.Note)
		}
		p("")
	}

	p("## Rationale recorded live")
	if len(pc.Rationale) > 0 {
		for _, j := range pc.Rationale {
			p("- %s", j.Rationale)
			for _, alt := range j.Alternatives {
				p("    - considered and rejected: %s", alt)
			}
		}
	} else {
		p("_None for this file. Why it was built this way was not written down._")
	}
	p("")

	if len(pc.Seams) > 0 {
		p("## Claims about it")
		for _, c := range pc.Seams {
			kind := "assertion"
			if c.Executable {
				kind = "executable"
			}
			p("- [%s] %s", kind, c.Claim)
		}
		p("")
	}

	if gaps := missingFrom(pc); len(gaps) > 0 {
		p("## What is missing")
		p("")
		p("Recorded gaps, not opinions — each is something nobody wrote down:")
		for _, g := range gaps {
			p("- %s", g)
		}
		p("")
	}
	return w.String()
}

// missingFrom names the gaps in the evidence, so a reader pasting this
// somewhere knows what the brief could not tell them.
func missingFrom(pc PromptContext) []string {
	// A test is the thing exercising the change, not part of it. Its name is its
	// documentation, its calls are assertions, and holding it to the same
	// standard buries the gaps that matter under ones that do not.
	if isTestPath(pc.Symbol_.File) {
		if len(pc.Invocations) == 0 {
			return []string{"no recorded execution: this test did not run in the traced session"}
		}
		return nil
	}

	var out []string
	if pc.Doc == "" && pc.Symbol_.Kind != "config_key" {
		out = append(out, "no declaration doc: what this is for is not recorded")
	}
	if len(pc.Invocations) == 0 {
		out = append(out, "no recorded execution: no traced test entered it, so its real behaviour is unobserved")
	}
	if len(pc.Rationale) == 0 {
		out = append(out, "no journalled rationale for this file: why it was built this way is unrecoverable from the diff")
	}
	local, _ := splitCallSites(pc.CallSites)
	var unannotated int
	for _, cs := range local {
		if cs.Rationale == "" {
			unannotated++
		}
	}
	switch {
	case unannotated == 1 && len(local) == 1:
		out = append(out, "its one call into this repository's own code carries no comment saying why it is made")
	case unannotated == 1:
		out = append(out, fmt.Sprintf("1 of its %d calls into this repository's own code carries no comment saying why", len(local)))
	case unannotated > 1:
		out = append(out, fmt.Sprintf("%d of its %d calls into this repository's own code carry no comment saying why", unannotated, len(local)))
	}
	if len(pc.Seams) == 0 && pc.Changed {
		out = append(out, "no claims: nothing has been asserted about it that could be checked or go stale")
	}
	return out
}

// splitCallSites separates calls into this repository's own code from calls into
// libraries. Only the first kind can meaningfully lack an explanation.
func splitCallSites(sites []bundle.CallSite) (local, external []bundle.CallSite) {
	for _, cs := range sites {
		if strings.HasPrefix(string(cs.Callee), "::") {
			external = append(external, cs)
			continue
		}
		local = append(local, cs)
	}
	return local, external
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func isTestPath(path string) bool { return bundle.IsTestPath(path) }

func oneLineOf(s string) string { return strings.Join(strings.Fields(s), " ") }

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
	// "I have met this code" is a claim the reader makes about themselves. plum
	// takes it at face value and clears the debt; the quiz is where it is checked.
	if s.Met != nil {
		_ = s.Met.MeetAll(s.Bundle)
	}
	writeJSON(w, map[string]string{"status": "ok", "next": "plum quiz " + s.Bundle.Session.ID})
	// Meeting the code ends an explore. It does not end a window that is there
	// to show you the next session too.
	if s.resident {
		return
	}
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
