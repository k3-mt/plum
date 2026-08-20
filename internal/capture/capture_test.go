package capture

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/k3-mt/plum/internal/config"
	"github.com/k3-mt/plum/internal/vcs"
)

func gitRepo(t *testing.T) (*config.Config, *vcs.Repo) {
	t.Helper()
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "init", "-q")
	run("git", "config", "user.email", "t@plum.test")
	run("git", "config", "user.name", "t")
	run("git", "add", "-A")
	run("git", "commit", "-qm", "first")
	return config.Default(root), vcs.New(root)
}

// Agents routinely leave work uncommitted. `git stash create` gives a dangling
// commit without touching the index or the working tree, which is the trick that
// makes wrapping a session viable (spec §6.2).
func TestUncommittedWorkIsCapturedAndTheWorkingTreeIsUntouched(t *testing.T) {
	cfg, repo := gitRepo(t)
	ctx := context.Background()

	// The "agent" edits a tracked file and adds an untracked one, committing nothing.
	if err := os.WriteFile(filepath.Join(cfg.Root, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Root, "b.txt"), []byte("new file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Run(ctx, cfg, repo, []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Session.StartSHA == res.Session.EndSHA {
		t.Fatal("uncommitted work was not captured: the range is empty")
	}

	// The snapshot contains the edit...
	got, err := repo.Show(ctx, res.Session.EndSHA, "a.txt")
	if err != nil || got != "one\ntwo\n" {
		t.Errorf("snapshot of a.txt = %q (%v)", got, err)
	}
	if got, _ := repo.Show(ctx, res.Session.EndSHA, "b.txt"); got != "new file\n" {
		t.Errorf("untracked file missing from the snapshot: %q", got)
	}
	// ...and the working tree still has it, unstaged and unstashed.
	onDisk, err := os.ReadFile(filepath.Join(cfg.Root, "a.txt"))
	if err != nil || string(onDisk) != "one\ntwo\n" {
		t.Errorf("working tree was disturbed: %q (%v)", onDisk, err)
	}
	if dirty, _ := repo.WorkingTreeDirty(ctx); !dirty {
		t.Error("the working tree should still be dirty — capture must not stash the user's work away")
	}
}

// A non-zero exit is still a session worth auditing.
func TestFailingCommandStillProducesASession(t *testing.T) {
	cfg, repo := gitRepo(t)
	res, err := Run(context.Background(), cfg, repo, []string{"false"})
	if err != nil {
		t.Fatalf("a failing agent command must not fail the capture: %v", err)
	}
	if res.RunErr == nil {
		t.Error("the command's failure should be reported, not hidden")
	}
	if res.Session.StartSHA == "" {
		t.Error("no session recorded")
	}
}

func TestSessionIDIsStableAndDated(t *testing.T) {
	at := time.Date(2026, 8, 19, 14, 30, 0, 0, time.UTC)
	a := SessionID(at, "abc123")
	if a != SessionID(at, "abc123") {
		t.Error("the same session must always get the same id")
	}
	if a == SessionID(at, "def456") {
		t.Error("different ranges must get different ids")
	}
	if got := a[:10]; got != "2026-08-19" {
		t.Errorf("id should sort by date, got %q", got)
	}
}

func TestDetectAgent(t *testing.T) {
	for argv, want := range map[string]string{
		"claude": "claude-code", "aider": "aider", "codex": "codex", "zsh": "zsh",
	} {
		if got := detectAgent([]string{argv}); got != want {
			t.Errorf("detectAgent(%q) = %q, want %q", argv, got, want)
		}
	}
	if got := detectAgent(nil); got != "manual" {
		t.Errorf("no argv = %q", got)
	}
}

func TestMarkRoundTrip(t *testing.T) {
	cfg, repo := gitRepo(t)
	ctx := context.Background()
	if _, err := MarkStart(ctx, cfg, repo, "cursor"); err != nil {
		t.Fatal(err)
	}
	m, err := LoadMark(cfg)
	if err != nil || m.Agent != "cursor" {
		t.Fatalf("mark = %+v (%v)", m, err)
	}
	if err := ClearMark(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMark(cfg); err == nil {
		t.Error("a cleared mark must not resolve")
	}
}
