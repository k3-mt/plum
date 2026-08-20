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

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/claims"
	"github.com/kelalaike/plum/internal/trace"
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

// watch reloads session state when it changes on disk, and tells the page.
func (s *Server) watch(stop <-chan struct{}) {
	sources := s.sourceFiles()
	last := map[string]string{
		"session": s.digest(s.sessionFiles()),
		"source":  s.digest(sources),
	}
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if d := s.digest(s.sessionFiles()); d != last["session"] {
				last["session"] = d
				s.reload()
				s.hub.broadcast("session")
				continue
			}
			// Source changing without a re-capture still matters: it is what
			// makes a stored reading stale, and the source pane is showing it.
			if d := s.digest(sources); d != last["source"] {
				last["source"] = d
				s.hub.broadcast("source")
			}
		}
	}
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

func (s *Server) sourceFiles() []string {
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
