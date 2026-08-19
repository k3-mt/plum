// Package explore records what you looked at. Exploration is itself telemetry:
// what you clicked, what you asked twice, what you skipped past is a direct read
// on where your model was thin — and it is what makes M4's questions targeted
// rather than random (P8).
package explore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/trace"
)

type Event struct {
	SessionID string          `json:"session_id"`
	Symbol    bundle.SymbolID `json:"symbol"`
	Action    string          `json:"action"` // click | prompt | expand_source | skip | done
	Query     string          `json:"query,omitempty"`
	DwellMS   int64           `json:"dwell_ms"`
	TS        time.Time       `json:"ts"`
}

// Store lives in the state dir, never in git: it describes you against this
// codebase, not the codebase (spec §3.2).
type Store struct {
	dir string
	mu  sync.Mutex
}

func NewStore(stateDir string) *Store { return &Store{dir: stateDir} }

func (s *Store) path() string { return filepath.Join(s.dir, "explore.jsonl") }

func (s *Store) donePath(sessionID string) string {
	return filepath.Join(s.dir, "done-"+sessionID)
}

func (s *Store) Append(e Event) error {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(e)
}

func (s *Store) Load(sessionID string) ([]Event, error) {
	data, err := os.ReadFile(s.path())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Event
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Event
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if sessionID == "" || e.SessionID == sessionID {
			out = append(out, e)
		}
	}
	return out, nil
}

// MarkDone ends the explore phase and unlocks interrogation. Retrieval practice
// only works on something you have already met (P8).
func (s *Store) MarkDone(sessionID string) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.donePath(sessionID), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644)
}

func (s *Store) IsDone(sessionID string) bool {
	_, err := os.Stat(s.donePath(sessionID))
	return err == nil
}

// AppendMiss records a wrong prediction. Aggregated over months these yield
// something no document provides: "you consistently mis-predict this codebase's
// error propagation" (spec §11.2).
func (s *Store) AppendMiss(m Miss) error {
	if m.TS.IsZero() {
		m.TS = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "misses.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(m)
}

func (s *Store) LoadMisses() ([]Miss, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, "misses.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Miss
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m Miss
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out, nil
}

type Miss struct {
	SessionID string          `json:"session_id"`
	Symbol    bundle.SymbolID `json:"symbol"`
	Kind      string          `json:"kind"` // return_value | next_frame | exception | cost
	Question  string          `json:"question"`
	Expected  string          `json:"expected"`
	Given     string          `json:"given"`
	TS        time.Time       `json:"ts"`
}

// TargetSymbols is what makes interrogation worth building rather than a random
// question generator (spec §11.1).
func TargetSymbols(tel []Event, l trace.Landscape, n int) []bundle.SymbolID {
	score := map[bundle.SymbolID]float64{}
	for _, w := range l.Wells {
		if w.Phase != "enter" {
			continue
		}
		switch {
		case promptCount(tel, w.Symbol) >= 2:
			// Understanding was expensive here — most likely not to have stuck.
			score[w.Symbol] += 3.0
		case !visited(tel, w.Symbol):
			// Unexplored frames on the hot path are the actual comprehension debt.
			score[w.Symbol] += 2.5
		case w.Doc == "":
			score[w.Symbol] += 1.5
		}
		if w.Risk {
			score[w.Symbol] += 1.0
		}
	}
	return topN(score, n)
}

func promptCount(tel []Event, sym bundle.SymbolID) int {
	n := 0
	for _, e := range tel {
		if e.Symbol == sym && e.Action == "prompt" {
			n++
		}
	}
	return n
}

func visited(tel []Event, sym bundle.SymbolID) bool {
	for _, e := range tel {
		if e.Symbol == sym && (e.Action == "click" || e.Action == "expand_source") {
			return true
		}
	}
	return false
}

func topN(score map[bundle.SymbolID]float64, n int) []bundle.SymbolID {
	type kv struct {
		id bundle.SymbolID
		v  float64
	}
	var all []kv
	for id, v := range score {
		all = append(all, kv{id, v})
	}
	// Deterministic: score first, then id, so a re-run asks the same questions.
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j].v > all[i].v || (all[j].v == all[i].v && all[j].id < all[i].id) {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	var out []bundle.SymbolID
	for i := 0; i < len(all) && i < n; i++ {
		out = append(out, all[i].id)
	}
	return out
}
