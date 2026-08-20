// Package interpret is the layer above the recording.
//
// `plum explain` says what happened: these functions ran, with these arguments,
// returning these values, costing this much. Every sentence there is composed
// from evidence, so it is always true and never needs a model.
//
// What it cannot say is what the change is *for*. Purpose is not recoverable
// from a trace — it lives in a head, a ticket, or a comment nobody wrote. So
// this layer asks a model, and everything about it is arranged so the answer
// stays honest: the prompt carries the mechanical evidence and forbids going
// beyond it, the result is stored beside the code it describes with the
// fingerprints of every symbol it covers, and it goes stale the moment any of
// them changes.
//
// An interpretation is never presented as fact. It is a reading, attributed to
// whatever produced it, over structure that was verified without it (P1).
package interpret

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/k3-mt/plum/internal/bundle"
)

// Scope names what an interpretation is about.
type Scope string

const (
	ScopeSession Scope = "session"
	ScopeTest    Scope = "test"
	ScopeSymbol  Scope = "symbol"
)

// Entry is one stored reading.
type Entry struct {
	Scope    Scope  `json:"scope"`
	Subject  string `json:"subject,omitempty"` // test name or symbol id; empty for a session
	Markdown string `json:"markdown"`
	Provider string `json:"provider"`
	// Fingerprints of every symbol this reading covers, captured when it was
	// written. When one moves, the reading is suspect — the same mechanism that
	// keeps claims honest (P5).
	Fingerprints map[bundle.SymbolID]string `json:"fingerprints"`
	GeneratedAt  time.Time                  `json:"generated_at"`
}

func (e Entry) Key() string {
	if e.Subject == "" {
		return string(e.Scope)
	}
	return string(e.Scope) + ":" + e.Subject
}

// File is every interpretation recorded for a session, keyed by scope.
type File struct {
	Entries map[string]Entry `json:"entries"`
}

func Path(sessionDir string) string { return filepath.Join(sessionDir, "interpretation.json") }

func Load(sessionDir string) (*File, error) {
	f := &File{Entries: map[string]Entry{}}
	data, err := os.ReadFile(Path(sessionDir))
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, f); err != nil {
		return nil, err
	}
	if f.Entries == nil {
		f.Entries = map[string]Entry{}
	}
	return f, nil
}

func Save(sessionDir string, f *File) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(Path(sessionDir), append(data, '\n'), 0o644)
}

// Stale reports which readings no longer describe the code they were written
// against.
func (f *File) Stale(current map[bundle.SymbolID]string) []Finding {
	var out []Finding
	keys := make([]string, 0, len(f.Entries))
	for k := range f.Entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e := f.Entries[k]
		var moved []bundle.SymbolID
		for id, fp := range e.Fingerprints {
			now, ok := current[id]
			if !ok || now != fp {
				moved = append(moved, id)
			}
		}
		if len(moved) == 0 {
			continue
		}
		sort.Slice(moved, func(i, j int) bool { return moved[i] < moved[j] })
		out = append(out, Finding{Key: k, Scope: e.Scope, Subject: e.Subject, Moved: moved})
	}
	return out
}

type Finding struct {
	Key     string
	Scope   Scope
	Subject string
	Moved   []bundle.SymbolID
}

// SystemPrompt is deliberately strict. An interpretation that quietly invents a
// purpose is worse than none: it reads exactly like one that was recorded, and
// the reader has no way to tell them apart.
const SystemPrompt = `You are reading a change to a codebase you did not write, in order to explain
what it is FOR.

Everything factual has already been established mechanically: which functions
changed, what they were called with, what they returned, what the call-site
comments say, what the risk predicates found, what the developer journalled.
That evidence is below. Do not restate it as your own finding, and do not
contradict it.

Your job is the part evidence cannot supply: purpose, and the shape of the
design. Write for someone who will maintain this next month.

Structure the answer exactly like this:

## In one sentence
What this change is for. Not what it does — what it is for.

## How it works
The mechanism, in the order it happens. Refer to real function names and real
recorded values. Two short paragraphs at most.

## What the evidence does not settle
The specific things you had to guess, and what would settle each one — a
comment, a test, a journal entry, a name. Be concrete: "why the realm is read
per call rather than cached is not recorded anywhere" beats "intent is unclear".
If the evidence settles everything, say so in one line.

## Where this is likely to bite
Grounded in what is actually there — a risk marker, an untested path, a value
in the recording. If nothing stands out, say that instead of inventing a worry.

Rules:
- Mark every inference. "The recording shows X" is a fact; "this suggests Y" is
  yours, and must read that way.
- Never invent a rationale that was not recorded. If nobody wrote down why,
  the answer is that nobody wrote down why.
- No praise, no grading, no suggestions for improvement unless asked.
- Plain words. Keep technical terms, but the first time one appears, gloss it.`

// Brief assembles what the model is given: the mechanical narration, the
// assembled per-symbol evidence, and the session's own facts.
func Brief(scope Scope, subject, summary string, steps []string, evidence string, b *bundle.Bundle) string {
	var w strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&w, f+"\n", a...) }

	switch scope {
	case ScopeTest:
		p("# Interpreting the path of test %q, session %s", subject, b.Session.ID)
	case ScopeSymbol:
		p("# Interpreting %s, session %s", subject, b.Session.ID)
	default:
		p("# Interpreting session %s", b.Session.ID)
	}
	p("")
	p("## What the recording shows (established mechanically — treat as fact)")
	p("")
	p("%s", summary)
	p("")
	for _, s := range steps {
		p("- %s", s)
	}
	p("")

	if len(b.Journal) > 0 {
		p("## Rationale the developer recorded during the session")
		for _, j := range b.Journal {
			p("- %s", j.Rationale)
			for _, alt := range j.Alternatives {
				p("  - considered and rejected: %s", alt)
			}
		}
		p("")
	} else {
		p("## Rationale the developer recorded during the session")
		p("None. Nothing about why this was built this way was written down.")
		p("")
	}

	if len(b.Surface.Modified) > 0 || len(b.Surface.Added) > 0 {
		p("## Public surface this change moved")
		for _, m := range b.Surface.Modified {
			p("- changed: %s (%s -> %s)", m.Name, oneLine(m.Before), oneLine(m.After))
		}
		for _, a := range b.Surface.Added {
			p("- added: %s %s", a.Kind, a.Name)
		}
		p("")
	}
	if len(b.RiskMarkers) > 0 {
		p("## Mechanical risk predicates that fired")
		for _, r := range b.RiskMarkers {
			p("- %s at %s:%d — %s", r.Kind, r.File, r.Line, r.Note)
		}
		p("")
	}
	if evidence != "" {
		p("## Assembled evidence for the frames involved")
		p("")
		p("%s", evidence)
	}
	return w.String()
}

// FingerprintsFor records what a reading depends on, so it can go stale.
func FingerprintsFor(b *bundle.Bundle, ids []bundle.SymbolID) map[bundle.SymbolID]string {
	all := b.Fingerprints()
	out := map[bundle.SymbolID]string{}
	if len(ids) == 0 {
		for id, fp := range all {
			out[id] = fp
		}
		return out
	}
	for _, id := range ids {
		if fp, ok := all[id]; ok {
			out[id] = fp
		}
	}
	return out
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
