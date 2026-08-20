package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/config"
)

func seed(t *testing.T) *Store {
	t.Helper()
	cfg := config.Default(t.TempDir())
	s := New(cfg)
	// Same-day ids whose lexical order is the reverse of their chronological
	// order — the date prefix is only day-granular.
	for _, tc := range []struct {
		id    string
		start time.Time
	}{
		{"2026-08-19-zz11", time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)},
		{"2026-08-19-aa22", time.Date(2026, 8, 19, 17, 0, 0, 0, time.UTC)},
	} {
		b := &bundle.Bundle{Session: bundle.Session{ID: tc.id, StartedAt: tc.start}}
		if err := s.Save(b); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestLatestIsChronologicalNotLexical(t *testing.T) {
	s := seed(t)
	got, err := s.Latest()
	if err != nil {
		t.Fatal(err)
	}
	if got != "2026-08-19-aa22" {
		t.Errorf("latest = %s, want the session that started later", got)
	}
}

func TestResolve(t *testing.T) {
	s := seed(t)
	for _, ref := range []string{"", "latest", "2026-08-19-aa22", "aa22"} {
		got, err := s.Resolve(ref)
		if err != nil {
			t.Fatalf("resolve %q: %v", ref, err)
		}
		if ref != "" && ref != "latest" && got != "2026-08-19-aa22" {
			t.Errorf("resolve %q = %q", ref, got)
		}
	}
	if _, err := s.Resolve("nope"); err == nil {
		t.Error("an unknown reference should fail loudly")
	}
	if _, err := s.Resolve("2026-08-19"); err == nil {
		t.Error("an ambiguous prefix should say so rather than guessing")
	}
}

func TestStateDirIsPerRepoAndOutsideIt(t *testing.T) {
	a, b := StateDir("/repos/one"), StateDir("/repos/two")
	if a == b {
		t.Error("two repos must not share explore telemetry")
	}
	if filepath.HasPrefix(a, "/repos/one") {
		t.Error("telemetry must never live inside the repo — it describes you, not the code")
	}
}
