// Package met records which symbols you have met, and at which version of them.
//
// Everything else plum stores describes the repository. This describes the gap
// between the repository and you: an agent changing a symbol opens that gap, and
// reading the symbol closes it. That gap is the one number a window left on a
// second screen exists to show, and nothing already stored can be asked for it —
// telemetry says what you clicked, not whether what you clicked still exists in
// the form you saw.
//
// It lives in the state dir with the rest of what describes you against this
// codebase, and is never committed (§3.2): two people working on one repository
// have different debts, and merging them would be meaningless.
package met

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/k3-mt/plum/internal/bundle"
)

type Set struct {
	mu   sync.Mutex
	path string
	// symbols maps a symbol to the fingerprint it carried when you met it.
	// A fingerprint rather than a boolean, because having met Get last week says
	// nothing about the Get that exists now — that difference is the whole debt.
	symbols map[bundle.SymbolID]string
}

// Load never fails. A missing file means nothing has been met, which is the
// truthful reading of a fresh checkout; an unreadable one means the same, and
// must not be the reason a window refuses to open.
func Load(stateDir string) *Set {
	s := &Set{
		path:    filepath.Join(stateDir, "met.json"),
		symbols: map[bundle.SymbolID]string{},
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return s
	}
	var on stored
	if json.Unmarshal(data, &on) == nil && on.Symbols != nil {
		s.symbols = on.Symbols
	}
	return s
}

type stored struct {
	Symbols map[bundle.SymbolID]string `json:"symbols"`
}

// Meet records one symbol at the version actually put in front of you. Called
// when the page fetches a brief: that is the moment the code is on the screen,
// as opposed to merely being drawn as a shape with a name on it.
func (s *Set) Meet(id bundle.SymbolID, fingerprint string) error {
	if id == "" || fingerprint == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.symbols[id] == fingerprint {
		return nil // already met at this version; do not rewrite the file
	}
	s.symbols[id] = fingerprint
	return s.saveLocked()
}

// Forget puts a symbol back. It is what a missed question means: the reader
// said they had met this code, the quiz checked that claim against a recorded
// execution, and the claim did not hold. A meter that only ever went down would
// be measuring clicks rather than comprehension.
func (s *Set) Forget(id bundle.SymbolID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.symbols[id]; !ok {
		return nil
	}
	delete(s.symbols, id)
	return s.saveLocked()
}

// MeetIn records a symbol at the version the given bundle holds, which is the
// version the debt is measured against. Callers that already have the bundle
// should not have to go looking for the fingerprint themselves.
func (s *Set) MeetIn(b *bundle.Bundle, id bundle.SymbolID) error {
	if b == nil {
		return nil
	}
	return s.Meet(id, b.Lookup(id).Fingerprint)
}

// MeetAll is what "I have met this code" means: the whole changed set at once.
// It is a claim the reader makes about themselves, and plum takes it at face
// value — the quiz is where it gets checked.
func (s *Set) MeetAll(b *bundle.Bundle) error {
	if b == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, sym := range b.Symbols {
		if sym.Fingerprint == "" || s.symbols[sym.ID] == sym.Fingerprint {
			continue
		}
		s.symbols[sym.ID] = sym.Fingerprint
		changed = true
	}
	if !changed {
		return nil
	}
	return s.saveLocked()
}

// Item is one entry on the worklist: an unmet symbol and the reason it sits
// where it does in the order.
type Item struct {
	Symbol bundle.SymbolID `json:"symbol"`
	Name   string          `json:"name"`
	File   string          `json:"file"`
	Why    string          `json:"why"`
}

// rank puts the worklist in the order `plum report` reads in: what could break
// other people first, source order never. The landscape draws one chain out of
// however many were recorded, so most of a session's debt is never on the
// picture — without an order to work through, the meter would be a number you
// could read but not act on.
func rank(b *bundle.Bundle, sym bundle.Symbol) (int, string) {
	risky := b.HasRisk(sym.ID)
	switch {
	case bundle.IsTestPath(sym.File):
		// Test code is changed code and belongs on the list, but it is never
		// what breaks somebody else. An exported Test function outranking a
		// changed signature would be the order backwards.
		return 5, "a test"

	// The reasons are written for the person who has to act on them, not for
	// the tool. "Signature changed on an existing export" is exact and means
	// nothing to most of the people the list is for; "other code calls this,
	// and it changed" is what they need to know.
	case sym.Exported && sym.Change == "modified":
		// The highest-signal event the tool produces: it breaks callers nobody
		// looked at.
		return 0, "other code calls this, and it changed"
	case risky:
		return 1, "flagged as risky"
	case sym.Exported:
		return 2, "new, and other code can call it"
	case !sym.Tested:
		return 3, "no test runs through it"
	default:
		return 4, "changed"
	}
}

// worklistMax bounds the panel. A list nobody finishes reading is a list that
// found nothing; what is held back is counted and named, never silently cut.
const worklistMax = 25

// Debt is the number on the window.
type Debt struct {
	Unmet int `json:"unmet"`
	Total int `json:"total"`
	// Stale counts the unmet symbols you had met before, at an older version —
	// as opposed to ones you have never seen at all. The distinction matters to
	// a reader: code that changed under you is a different problem from code
	// that is simply new.
	Stale int `json:"stale"`
	// Frames names the unmet symbols the landscape is drawing, so the page can
	// dim them. The full unmet set runs to tens of thousands on a first capture
	// and is no use to a drawing that shows a few dozen frames.
	Frames []bundle.SymbolID `json:"frames"`
	// Worklist is the unmet symbols in the order they are worth reading, and
	// More is how many did not fit. The meter says how much is owed; this is
	// what you can do about it.
	Worklist []Item `json:"worklist"`
	More     int    `json:"more"`
	// Drifted counts symbols the working tree no longer agrees with the capture
	// about: code being written right now, which no session has recorded yet.
	// Kept apart from Unmet rather than folded into it, because they are answers
	// to different questions — one is measured against a recording, the other
	// against the files on disk this second.
	Drifted int `json:"drifted"`
	// Trend is how far the unmet count has moved across the window the meter
	// looks back over, and TrendMinutes is how long that window is. Positive
	// means the debt is growing — the agent is getting ahead of you. Zero means
	// there is nothing to compare against yet, which is why the page shows no
	// arrow rather than a flat one.
	Trend        int `json:"trend"`
	TrendMinutes int `json:"trend_minutes"`
	// Unmeasured says the drift was not computed, and why. A capture that names
	// every file in the repository cannot be re-parsed on a watcher tick, and a
	// silent zero there would read as "nothing is being written", which is the
	// one answer that is both wrong and reassuring.
	Unmeasured string `json:"unmeasured,omitempty"`
}

// Of measures one bundle's changed set against what has been met. drawn is the
// symbols the landscape currently shows; everything else is counted but not
// named.
func (s *Set) Of(b *bundle.Bundle, drawn []bundle.SymbolID) Debt {
	var d Debt
	if b == nil {
		return d
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Two maps because the drawn-frames pass strikes symbols off as it names
	// them, and the worklist still needs to know the whole set.
	unmet := make(map[bundle.SymbolID]bool, len(b.Symbols))
	unmetAll := make(map[bundle.SymbolID]bool, len(b.Symbols))
	for _, sym := range b.Symbols {
		if sym.Fingerprint == "" {
			continue // nothing to compare against; counting it would be a guess
		}
		d.Total++
		seen, ok := s.symbols[sym.ID]
		if seen == sym.Fingerprint {
			continue
		}
		d.Unmet++
		if ok {
			d.Stale++
		}
		unmet[sym.ID] = true
		unmetAll[sym.ID] = true
	}
	// A landscape draws one symbol as several wells — descending into it, and
	// again on each resume — so the drawn list repeats. The page wants to know
	// which symbols to render hollow, which is a set, not a list of frames.
	for _, id := range drawn {
		if unmet[id] {
			unmet[id] = false // named once; the rest of its wells are the same symbol
			d.Frames = append(d.Frames, id)
		}
	}
	sort.Slice(d.Frames, func(i, j int) bool { return d.Frames[i] < d.Frames[j] })

	type ranked struct {
		item Item
		tier int
	}
	var all []ranked
	for _, sym := range b.Symbols {
		if !unmetAll[sym.ID] {
			continue
		}
		tier, why := rank(b, sym)
		all = append(all, ranked{Item{Symbol: sym.ID, Name: sym.Name, File: sym.File, Why: why}, tier})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].tier != all[j].tier {
			return all[i].tier < all[j].tier
		}
		return all[i].item.Symbol < all[j].item.Symbol
	})
	for i, r := range all {
		if i >= worklistMax {
			d.More = len(all) - worklistMax
			break
		}
		d.Worklist = append(d.Worklist, r.item)
	}
	return d
}

// saveLocked writes through a temporary file and renames. The window rewrites
// this while the page is reading it, and a half-written met.json would report a
// debt of everything — which is the one wrong answer that looks plausible.
func (s *Set) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(stored{Symbols: s.symbols}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
