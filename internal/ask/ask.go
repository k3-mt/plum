// Package ask routes a question from the explore UI to a Claude Code session
// running in a tmux pane, and watches for the answer to come back.
//
// The point is not chat. The point is that the question arrives with context
// that was assembled mechanically from the bundle — the exact declaration, the
// real recorded arguments and return values, the edges, the risk markers, the
// rationale that was or was not journalled — rather than retrieved by a search
// that might miss. An answer grounded in that is worth keeping; an answer the
// context cannot support is itself a finding (spec §10.2).
package ask

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kelalaike/plum/internal/bundle"
)

// Dir is where questions and answers live, relative to the repo root. It sits
// under .plum/ so an agent working in the repo can read and write it with no
// special permission, and it is gitignored: a question is a moment, not a fact
// about the code.
const Dir = ".plum/ask"

type Request struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	Symbol    bundle.SymbolID `json:"symbol"`
	Question  string          `json:"question"`
	CreatedAt time.Time       `json:"created_at"`
	Grounded  bool            `json:"grounded"`
	Route     string          `json:"route"` // tmux | api | none
	Target    string          `json:"target,omitempty"`
}

type Answer struct {
	ID       string    `json:"id"`
	Status   string    `json:"status"` // pending | answered | failed
	Text     string    `json:"answer,omitempty"`
	Error    string    `json:"error,omitempty"`
	Answered time.Time `json:"answered_at,omitempty"`
}

// Store owns the on-disk protocol between plum and whatever answers questions.
type Store struct{ Root string }

func NewStore(root string) *Store { return &Store{Root: root} }

func (s *Store) dir() string                 { return filepath.Join(s.Root, Dir) }
func (s *Store) PromptPath(id string) string { return filepath.Join(s.dir(), id+".md") }
func (s *Store) AnswerPath(id string) string { return filepath.Join(s.dir(), id+".answer.md") }
func (s *Store) MetaPath(id string) string   { return filepath.Join(s.dir(), id+".json") }

// NextID is time-ordered so the directory reads chronologically.
func NextID(now time.Time) string { return now.UTC().Format("20060102-150405.000") }

// Write lays down the prompt and its metadata. The prompt is a complete,
// self-contained brief: whoever picks it up needs nothing else.
func (s *Store) Write(req Request, contextMD string) error {
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return err
	}
	var w strings.Builder
	fmt.Fprintf(&w, "# plum question %s\n\n", req.ID)
	fmt.Fprintf(&w, "A developer is reading `%s` in `plum explore` and asked:\n\n", req.Symbol)
	fmt.Fprintf(&w, "> %s\n\n", req.Question)
	w.WriteString(`Answer **only** from the context below. It was assembled mechanically from an
AST bundle and from recorded executions — it is not a search result, and it is
not a summary. If the context does not contain the answer, say exactly what is
missing and stop; a question the evidence cannot answer is a finding in its own
right, and guessing destroys the point of the exercise.

Be concrete. Cite recorded invocations by their actual argument and return
values where they support the answer. Keep it short — a few paragraphs at most.

`)
	fmt.Fprintf(&w, "When you are done, write your answer to:\n\n    %s\n\n", filepath.Join(Dir, req.ID+".answer.md"))
	w.WriteString("Write nothing else, and change no source files: plum offers the developer\nthe choice of what to keep.\n\n---\n\n")
	w.WriteString(contextMD)

	if err := os.WriteFile(s.PromptPath(req.ID), []byte(w.String()), 0o644); err != nil {
		return err
	}
	meta, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.MetaPath(req.ID), meta, 0o644)
}

func (s *Store) Meta(id string) (*Request, error) {
	data, err := os.ReadFile(s.MetaPath(id))
	if err != nil {
		return nil, err
	}
	var req Request
	return &req, json.Unmarshal(data, &req)
}

// Poll reports whether an answer has landed yet.
func (s *Store) Poll(id string) Answer {
	data, err := os.ReadFile(s.AnswerPath(id))
	if err != nil {
		return Answer{ID: id, Status: "pending"}
	}
	info, _ := os.Stat(s.AnswerPath(id))
	answered := time.Time{}
	if info != nil {
		answered = info.ModTime()
	}
	return Answer{ID: id, Status: "answered", Text: strings.TrimSpace(string(data)), Answered: answered}
}

// Wait blocks until an answer appears or the context is done. Used by the CLI;
// the browser polls the endpoint instead.
func (s *Store) Wait(ctx context.Context, id string, every time.Duration) (Answer, error) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if a := s.Poll(id); a.Status == "answered" {
			return a, nil
		}
		select {
		case <-ctx.Done():
			return Answer{ID: id, Status: "pending"}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// Pending lists questions still waiting, oldest first.
func (s *Store) Pending() []Request {
	entries, err := os.ReadDir(s.dir())
	if err != nil {
		return nil
	}
	var out []Request
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if s.Poll(id).Status == "answered" {
			continue
		}
		if req, err := s.Meta(id); err == nil {
			out = append(out, *req)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// ---------------------------------------------------------------- tmux

// Tmux delivers the question to a Claude Code session already running in a
// pane. It sends a one-line instruction pointing at the prompt file rather than
// pasting the context into the prompt: the context is large, and a file is
// something the agent can re-read.
type Tmux struct {
	Target string // "session:window.pane", or "" to auto-detect
}

func (t *Tmux) Name() string { return "tmux" }

type Pane struct {
	ID      string
	Target  string
	Command string
	Title   string
	Path    string
}

// Panes lists every pane in every session.
func Panes(ctx context.Context) ([]Pane, error) {
	out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F",
		"#{pane_id}\t#{session_name}:#{window_index}.#{pane_index}\t#{pane_current_command}\t#{pane_title}\t#{pane_current_path}").Output()
	if err != nil {
		return nil, fmt.Errorf("tmux is not running, or has no panes: %w", err)
	}
	var panes []Pane
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 5 {
			continue
		}
		panes = append(panes, Pane{ID: f[0], Target: f[1], Command: f[2], Title: f[3], Path: f[4]})
	}
	return panes, nil
}

// FindPane picks the pane most likely to be a Claude Code session: one whose
// command or title names it, preferring a pane rooted in this repository, and
// never the pane plum itself is running in.
func FindPane(ctx context.Context, repoRoot string) (Pane, error) {
	panes, err := Panes(ctx)
	if err != nil {
		return Pane{}, err
	}
	self := os.Getenv("TMUX_PANE")
	score := func(p Pane) int {
		if p.ID == self {
			return -1
		}
		s := 0
		hay := strings.ToLower(p.Command + " " + p.Title)
		switch {
		case strings.Contains(hay, "claude"):
			s += 10
		case strings.Contains(hay, "node"), strings.Contains(hay, "aider"), strings.Contains(hay, "codex"):
			s += 4
		default:
			return -1 // do not send keystrokes into a shell that is not an agent
		}
		if repoRoot != "" && strings.HasPrefix(p.Path, repoRoot) {
			s += 3
		}
		return s
	}
	best, bestScore := Pane{}, 0
	for _, p := range panes {
		if s := score(p); s > bestScore {
			best, bestScore = p, s
		}
	}
	if bestScore == 0 {
		return Pane{}, fmt.Errorf("no pane looks like an agent session (looked for claude, aider, codex in %d panes) — set [ask] tmux_target in .plum/config.toml to choose one explicitly", len(panes))
	}
	return best, nil
}

// Send delivers the instruction. The literal flag keeps the shell and tmux from
// interpreting anything in the text.
func (t *Tmux) Send(ctx context.Context, repoRoot string, req Request) (string, error) {
	target := t.Target
	if target == "" {
		pane, err := FindPane(ctx, repoRoot)
		if err != nil {
			return "", err
		}
		target = pane.Target
	}
	instruction := fmt.Sprintf(
		"Read %s and answer the question in it, writing your answer to %s — it comes from plum explore and is about %s.",
		filepath.Join(Dir, req.ID+".md"),
		filepath.Join(Dir, req.ID+".answer.md"),
		req.Symbol,
	)
	if out, err := exec.CommandContext(ctx, "tmux", "send-keys", "-t", target, "-l", instruction).CombinedOutput(); err != nil {
		return target, fmt.Errorf("tmux send-keys to %s: %w: %s", target, err, strings.TrimSpace(string(out)))
	}
	// Enter is sent separately: -l would type the word "Enter".
	if out, err := exec.CommandContext(ctx, "tmux", "send-keys", "-t", target, "Enter").CombinedOutput(); err != nil {
		return target, fmt.Errorf("tmux send-keys Enter to %s: %w: %s", target, err, strings.TrimSpace(string(out)))
	}
	return target, nil
}

// Available reports whether tmux is usable right now.
func Available(ctx context.Context) bool {
	if _, err := exec.LookPath("tmux"); err != nil {
		return false
	}
	_, err := Panes(ctx)
	return err == nil
}
