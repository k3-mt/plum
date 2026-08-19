// Package capture owns session boundaries — the fiddliest part of the tool.
// Agents routinely leave work uncommitted, so the end of a session is captured
// with `git stash create`: a dangling commit that touches neither the index nor
// the working tree (spec §6.2).
package capture

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/config"
	"github.com/kelalaike/plum/internal/journal"
	"github.com/kelalaike/plum/internal/vcs"
)

type Result struct {
	Session bundle.Session
	Journal []bundle.JournalEntry
	RunErr  error // the wrapped command's exit status; a failure is still worth auditing
}

// Run wraps an agent session. Stdio is inherited so the agent stays fully
// interactive inside the wrapper.
func Run(ctx context.Context, cfg *config.Config, repo *vcs.Repo, argv []string) (*Result, error) {
	start, err := repo.RevParse(ctx, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("no HEAD to start from (commit something first): %w", err)
	}
	startedAt := time.Now().UTC()

	var runErr error
	if len(argv) > 0 {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = cfg.Root
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		runErr = cmd.Run() // a non-zero exit is still a session worth auditing
	}

	sess, err := Close(ctx, cfg, repo, start, startedAt, strings.Join(argv, " "), detectAgent(argv))
	if err != nil {
		return nil, err
	}
	j, _ := journal.Harvest(filepath.Join(cfg.Root, cfg.Repo.JournalDir), startedAt)
	sess.JournalPresent = len(j) > 0
	return &Result{Session: *sess, Journal: j, RunErr: runErr}, nil
}

// Close resolves the end of a session from a known start point. Shared by
// `plum run` and `plum mark end`.
func Close(ctx context.Context, cfg *config.Config, repo *vcs.Repo, start string, startedAt time.Time, command, agent string) (*bundle.Session, error) {
	end, err := repo.RevParse(ctx, "HEAD")
	if err != nil {
		return nil, err
	}
	// Dangling commit capturing uncommitted work. Index and working tree untouched.
	if snap, err := repo.Snapshot(ctx); err == nil && snap != "" {
		end = snap
	}
	return &bundle.Session{
		ID:        SessionID(startedAt, start),
		StartSHA:  start,
		EndSHA:    end,
		StartedAt: startedAt,
		EndedAt:   time.Now().UTC(),
		Command:   command,
		Agent:     agent,
		Repo:      cfg.Root,
	}, nil
}

// SessionID is date-plus-hash so directories sort chronologically and never collide.
func SessionID(t time.Time, startSHA string) string {
	h := sha1.Sum([]byte(t.Format(time.RFC3339Nano) + startSHA))
	return t.Format("2006-01-02") + "-" + hex.EncodeToString(h[:])[:4]
}

// detectAgent records which tool produced the session. Nothing downstream of
// bundle.json depends on the answer — it is provenance, not behaviour.
func detectAgent(argv []string) string {
	if len(argv) == 0 {
		return "manual"
	}
	base := filepath.Base(argv[0])
	switch {
	case strings.Contains(base, "claude"):
		return "claude-code"
	case strings.Contains(base, "aider"):
		return "aider"
	case strings.Contains(base, "codex"):
		return "codex"
	case strings.Contains(base, "cursor"):
		return "cursor"
	}
	return base
}

// markFile records an open session between `plum mark start` and `plum mark end`,
// which is how GUI editors get boundaries without a wrapper process (spec §14.2).
type Mark struct {
	StartSHA  string    `json:"start_sha"`
	StartedAt time.Time `json:"started_at"`
	Command   string    `json:"command"`
	Agent     string    `json:"agent"`
}

func markPath(cfg *config.Config) string {
	return filepath.Join(cfg.Root, config.Dir, "current-session.json")
}

func MarkStart(ctx context.Context, cfg *config.Config, repo *vcs.Repo, agent string) (*Mark, error) {
	start, err := repo.RevParse(ctx, "HEAD")
	if err != nil {
		return nil, err
	}
	m := &Mark{StartSHA: start, StartedAt: time.Now().UTC(), Command: "mark", Agent: agent}
	data, _ := json.MarshalIndent(m, "", "  ")
	if err := os.MkdirAll(filepath.Dir(markPath(cfg)), 0o755); err != nil {
		return nil, err
	}
	return m, os.WriteFile(markPath(cfg), data, 0o644)
}

func LoadMark(cfg *config.Config) (*Mark, error) {
	data, err := os.ReadFile(markPath(cfg))
	if err != nil {
		return nil, err
	}
	var m Mark
	return &m, json.Unmarshal(data, &m)
}

func ClearMark(cfg *config.Config) error {
	err := os.Remove(markPath(cfg))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
