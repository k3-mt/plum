// Package store is the on-disk layout of the per-repo footprint (spec §3.2):
// bundles, synthesis and claims are committed; traces and telemetry are not.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/config"
)

type Store struct {
	cfg *config.Config
}

func New(cfg *config.Config) *Store { return &Store{cfg: cfg} }

func (s *Store) Dir(id string) string           { return s.cfg.SessionDir(id) }
func (s *Store) BundlePath(id string) string    { return filepath.Join(s.Dir(id), "bundle.json") }
func (s *Store) SynthesisPath(id string) string { return filepath.Join(s.Dir(id), "synthesis.md") }
func (s *Store) ClaimsPath(id string) string    { return filepath.Join(s.Dir(id), "claims.yaml") }
func (s *Store) LandscapePath(id string) string { return filepath.Join(s.Dir(id), "landscape.json") }
func (s *Store) TracesDir(id string) string     { return filepath.Join(s.Dir(id), "traces") }
func (s *Store) TracePath(id string) string     { return filepath.Join(s.TracesDir(id), "events.jsonl") }

// List returns session IDs oldest first, ordered by the start time recorded in
// each bundle. The ID's date prefix is only day-granular, so lexical order is
// not chronological order once two sessions land on the same day.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.cfg.SessionsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	started := map[string]time.Time{}
	for _, id := range ids {
		if b, err := s.Load(id); err == nil {
			started[id] = b.Session.StartedAt
		} else if fi, err := os.Stat(s.Dir(id)); err == nil {
			started[id] = fi.ModTime()
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		if started[ids[i]].Equal(started[ids[j]]) {
			return ids[i] < ids[j]
		}
		return started[ids[i]].Before(started[ids[j]])
	})
	return ids, nil
}

func (s *Store) Latest() (string, error) {
	ids, err := s.List()
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("no sessions recorded yet — run `plum run -- <command>` first")
	}
	return ids[len(ids)-1], nil
}

// Resolve accepts "", "latest", a full ID or a unique prefix.
func (s *Store) Resolve(ref string) (string, error) {
	if ref == "" || ref == "latest" {
		return s.Latest()
	}
	ids, err := s.List()
	if err != nil {
		return "", err
	}
	var hits []string
	for _, id := range ids {
		if id == ref {
			return id, nil
		}
		if strings.HasPrefix(id, ref) || strings.HasSuffix(id, ref) {
			hits = append(hits, id)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", fmt.Errorf("no session matching %q", ref)
	default:
		return "", fmt.Errorf("%q matches %d sessions: %s", ref, len(hits), strings.Join(hits, ", "))
	}
}

func (s *Store) Save(b *bundle.Bundle) error {
	return bundle.Write(s.BundlePath(b.Session.ID), b)
}

func (s *Store) Load(id string) (*bundle.Bundle, error) {
	return bundle.Read(s.BundlePath(id))
}

func (s *Store) WriteFile(id, name string, data []byte) error {
	if err := os.MkdirAll(s.Dir(id), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.Dir(id), name), data, 0o644)
}

// StateDir holds things that describe *you against this codebase*, not the
// codebase: explore telemetry and prediction misses. Never committed (§3.2).
func StateDir(repoRoot string) string {
	sum := sha256.Sum256([]byte(repoRoot))
	hash := hex.EncodeToString(sum[:])[:12]
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), "plum", hash)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "plum", hash)
}
