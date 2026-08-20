package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/claims"
	"github.com/k3-mt/plum/internal/lang/dbt"
	"github.com/k3-mt/plum/internal/trace"
)

// The page is a view of files on disk, and those files change while you are
// looking at them: an agent edits the source in one pane, `plum trace` rewrites
// the landscape, `plum interpret` stores a reading. Reloading by hand to find
// out is the kind of friction that stops a tool being used.
//
// So the server watches, and the page listens. No dependency and no build step:
// a digest of the files' size and modification time, compared on a short timer,
// pushed to the browser over server-sent events.
const watchInterval = 700 * time.Millisecond

type liveHub struct {
	mu      sync.Mutex
	clients map[chan string]bool
}

func newHub() *liveHub { return &liveHub{clients: map[chan string]bool{}} }

func (h *liveHub) add() chan string {
	ch := make(chan string, 4)
	h.mu.Lock()
	h.clients[ch] = true
	h.mu.Unlock()
	return ch
}

func (h *liveHub) remove(ch chan string) {
	h.mu.Lock()
	delete(h.clients, ch)
	close(ch)
	h.mu.Unlock()
}

func (h *liveHub) broadcast(what string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- what:
		default: // a client too slow to keep up will catch the next one
		}
	}
}

// handleLive streams reload events. Each message names what changed, so the
// page can say so rather than silently redrawing under the reader.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")

	ch := s.hub.add()
	defer s.hub.remove(ch)
	fmt.Fprint(w, "retry: 1000\n\n")
	flusher.Flush()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case what := <-ch:
			fmt.Fprintf(w, "event: reload\ndata: %s\n\n", what)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": keep-alive\n\n") // proxies drop an idle stream
			flusher.Flush()
		}
	}
}

// watchState is the baseline a tick compares against: what the session and the
// source looked like the last time the page was told anything.
type watchState struct {
	sources  []string
	session  string
	source   string
	sessions string
}

func (s *Server) baseline() *watchState {
	src := s.sourceFiles()
	return &watchState{
		sources:  src,
		session:  s.digest(s.sessionFiles()),
		source:   s.digest(src),
		sessions: s.digest(s.sessionBundles()),
	}
}

// watch reloads session state when it changes on disk, and tells the page.
func (s *Server) watch(stop <-chan struct{}) {
	st := s.baseline()
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.tick(st)
		}
	}
}

// tick is one comparison against the baseline. It returns what it told the page,
// which is the whole reason it is a function rather than the body of the loop:
// the interesting cases here are about which events get sent together, and a
// test that has to race a ticker to find that out will pass whether the logic is
// right or not.
func (s *Server) tick(st *watchState) []string {
	// A new recording outranks a change within the current one: the agent
	// stopped, capture ran, and what you are looking at is now the previous
	// piece of work.
	if s.follow {
		if d := s.digest(s.sessionBundles()); d != st.sessions {
			st.sessions = d
			if s.followLatest() {
				// A different recording names a different set of files. This is
				// a new baseline, not an edit: the reader is being shown other
				// code, and calling that a source change would be a lie.
				st.sources = s.sourceFiles()
				st.session = s.digest(s.sessionFiles())
				st.source = s.digest(st.sources)
				return []string{"session"}
			}
		}
	}

	var sent []string
	if d := s.digest(s.sessionFiles()); d != st.session {
		st.session = d
		s.reload()
		// The reload may have replaced the bundle, and a different bundle names
		// different files. Refresh the list — but not the baseline: an edit that
		// lands in the same tick as a capture is still an edit, and rebaselining
		// here would swallow it. That is the common case, not a corner one: the
		// agent writes the code and the Stop hook captures it moments later.
		st.sources = s.sourceFiles()
		s.hub.broadcast("session")
		sent = append(sent, "session")
	}
	// Source changing without a re-capture still matters: it is what makes a
	// stored reading stale, and the source pane is showing it. Reached on the
	// same tick as a session change, deliberately — two things did change.
	if d := s.digest(st.sources); d != st.source {
		st.source = d
		s.hub.broadcast("source")
		sent = append(sent, "source")
	}
	return sent
}

func (s *Server) sessionFiles() []string {
	if s.SessionDir == "" {
		return nil
	}
	var out []string
	_ = filepath.WalkDir(s.SessionDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// followLatest rebinds the window to the newest recording, and reports whether
// it moved. Latest() loads every bundle in order to sort them, which is far too
// expensive to run twice a second, so the caller only reaches here once a cheap
// stat over the session directory says something actually changed.
func (s *Server) followLatest() bool {
	id, err := s.sessions.Latest()
	if err != nil || id == "" {
		return false
	}
	s.mu.RLock()
	same := id == s.sessionID
	s.mu.RUnlock()
	if same {
		return false
	}
	s.mu.Lock()
	s.SessionDir = s.sessions.Dir(id)
	s.sessionID = id
	// A test filter was derived from the recording it came from. Carrying it
	// into the next one would silently show a path that was never taken.
	s.TestFilter = ""
	s.mu.Unlock()
	s.reload()
	s.hub.broadcast("session")
	return true
}

// sessionBundles is the cheap question — has a session appeared or been
// rewritten — as opposed to Latest(), which is the expensive answer.
func (s *Server) sessionBundles() []string {
	dir := s.Cfg.SessionsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(dir, e.Name(), "bundle.json"))
		}
	}
	sort.Strings(out)
	return out
}

func (s *Server) sourceFiles() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for _, sym := range s.Bundle.Symbols {
		if sym.File == "" || seen[sym.File] {
			continue
		}
		seen[sym.File] = true
		out = append(out, filepath.Join(s.Cfg.Root, sym.File))
	}
	sort.Strings(out)
	return out
}

// digest is size and modification time, which is enough to notice an edit and
// cheap enough to run four times a second.
func (s *Server) digest(paths []string) string {
	h := sha256.New()
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			fmt.Fprintf(h, "%s:missing\n", p)
			continue
		}
		fmt.Fprintf(h, "%s:%d:%d\n", p, info.Size(), info.ModTime().UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// reload re-reads what the session wrote, so a `plum trace` or `plum interpret`
// in another pane shows up here without restarting anything.
func (s *Server) reload() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if b, err := bundle.Read(filepath.Join(s.SessionDir, "bundle.json")); err == nil && b != nil {
		s.Bundle = b
	}
	if events, err := trace.ReadFile(filepath.Join(s.SessionDir, "traces", "events.jsonl")); err == nil {
		s.Events = events
	}
	if cs, err := claims.Load(filepath.Join(s.SessionDir, "claims.yaml")); err == nil {
		s.Claims = cs
	}
	if md, err := os.ReadFile(filepath.Join(s.SessionDir, "synthesis.md")); err == nil {
		s.Synthesis = string(md)
	}
	// The flow is the whole DAG regardless of which test is in view, so it is
	// always reloaded: nothing about it is narrowed by a filter.
	if f, err := dbt.LoadFlow(filepath.Join(s.SessionDir, "flow.json")); err == nil {
		s.Flow = f
	}
	// The landscape is only replaced when the page is showing the whole
	// recording. A view narrowed to one test was derived here and would be
	// silently widened by re-reading the file.
	if s.TestFilter == "" {
		if l, err := trace.LoadLandscape(filepath.Join(s.SessionDir, "landscape.json")); err == nil {
			s.Landscape = *l
		}
		return
	}
	if len(s.Events) > 0 {
		scoped := trace.ForTest(s.Events, s.TestFilter)
		if len(scoped) > 0 {
			l := trace.DeriveChainN(scoped, s.Bundle, trace.ChainHottest, trace.DefaultMaxFrames)
			l.TestID = s.TestFilter
			s.Landscape = l
		}
	}
}
