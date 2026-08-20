package vcs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func testRepo(t *testing.T) *Repo {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@plum.test"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write(t, root, "a.txt", "one\n")
	commit(t, root, "first")
	return New(root)
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, root, msg string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", msg}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// The whole point of a content fingerprint: `git status` reports only *which*
// paths changed, so a second edit to an already-modified file leaves it
// identical — and any debounce built on that drops every turn after the first.
func TestWorkingTreeIDTracksContentNotJustPaths(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	clean, err := repo.WorkingTreeID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	write(t, repo.Dir, "a.txt", "one\ntwo\n")
	first, err := repo.WorkingTreeID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first == clean {
		t.Fatal("an edit did not move the fingerprint")
	}

	write(t, repo.Dir, "a.txt", "one\ntwo\nthree\n")
	second, err := repo.WorkingTreeID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Error("a second edit to the same file must move the fingerprint")
	}

	// Untracked files count too — an agent's new file is the common case.
	write(t, repo.Dir, "b.txt", "new\n")
	third, err := repo.WorkingTreeID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if third == second {
		t.Error("a new untracked file must move the fingerprint")
	}
}

func TestWorkingTreeIDHonoursExcludes(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()
	write(t, repo.Dir, "src/a.go", "package a\n")
	before, err := repo.WorkingTreeID(ctx, ".plum")
	if err != nil {
		t.Fatal(err)
	}
	// A tool's own bookkeeping must not look like a change, or it defeats the
	// debounce it is trying to support.
	write(t, repo.Dir, ".plum/auto-state.json", `{"last":"x"}`)
	after, err := repo.WorkingTreeID(ctx, ".plum")
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Error("an excluded path changed the fingerprint")
	}
}

func TestWorkingTreeIDLeavesIndexAndTreeAlone(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()
	write(t, repo.Dir, "a.txt", "one\ntwo\n")
	if _, err := repo.WorkingTreeID(ctx); err != nil {
		t.Fatal(err)
	}
	dirty, err := repo.WorkingTreeDirty(ctx)
	if err != nil || !dirty {
		t.Error("fingerprinting must not stage or stash the user's work")
	}
	out, err := os.ReadFile(filepath.Join(repo.Dir, "a.txt"))
	if err != nil || string(out) != "one\ntwo\n" {
		t.Errorf("working tree was disturbed: %q", out)
	}
}

// Notes bind analysis to a commit after the fact, without rewriting history.
func TestNotesRoundTrip(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()
	head, err := repo.RevParse(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if note, _ := repo.NoteShow(ctx, head); note != "" {
		t.Errorf("a fresh commit has no note, got %q", note)
	}
	if err := repo.NoteAdd(ctx, head, "plum session 2026-08-20-abcd\ngate clear"); err != nil {
		t.Fatal(err)
	}
	note, err := repo.NoteShow(ctx, head)
	if err != nil || note == "" {
		t.Fatalf("note = %q (%v)", note, err)
	}
	// Re-adding replaces rather than failing, so repeated captures are safe.
	if err := repo.NoteAdd(ctx, head, "plum session 2026-08-20-ffff"); err != nil {
		t.Fatal(err)
	}
	if note, _ := repo.NoteShow(ctx, head); note != "plum session 2026-08-20-ffff" {
		t.Errorf("note = %q", note)
	}
	// The commit itself is untouched.
	if got, _ := repo.RevParse(ctx, "HEAD"); got != head {
		t.Error("attaching a note rewrote history")
	}
}

func TestIsRevision(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()
	for _, ref := range []string{"HEAD", "HEAD~0"} {
		if !repo.IsRevision(ctx, ref) {
			t.Errorf("%s should resolve", ref)
		}
	}
	for _, ref := range []string{"2026-08-20-abcd", "not-a-thing", ""} {
		if repo.IsRevision(ctx, ref) {
			t.Errorf("%q should not resolve as a commit", ref)
		}
	}
}
