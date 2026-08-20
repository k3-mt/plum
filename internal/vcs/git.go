// Package vcs is git plumbing over os/exec on the git binary (spec §3.3).
// Deliberately not cgo libgit2 — that would end cross-compilation.
package vcs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Repo struct {
	Dir string
}

func New(dir string) *Repo { return &Repo{Dir: dir} }

func (r *Repo) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// GitDir is where hooks and refs live — not always .git inside the work tree
// (a worktree or a submodule puts it elsewhere).
func (r *Repo) GitDir(ctx context.Context) (string, error) {
	out, err := r.git(ctx, "rev-parse", "--absolute-git-dir")
	return strings.TrimSpace(out), err
}

// Root returns the absolute path of the repository working tree.
func (r *Repo) Root(ctx context.Context) (string, error) {
	out, err := r.git(ctx, "rev-parse", "--show-toplevel")
	return strings.TrimSpace(out), err
}

func (r *Repo) RevParse(ctx context.Context, rev string) (string, error) {
	out, err := r.git(ctx, "rev-parse", rev)
	return strings.TrimSpace(out), err
}

// HasCommits reports whether HEAD resolves — a fresh repo with no commits has no HEAD.
func (r *Repo) HasCommits(ctx context.Context) bool {
	_, err := r.RevParse(ctx, "HEAD")
	return err == nil
}

func (r *Repo) WorkingTreeDirty(ctx context.Context) (bool, error) {
	out, err := r.git(ctx, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// Snapshot produces a dangling commit capturing uncommitted work — including
// untracked files — without touching the index or the working tree. This is the
// trick that makes wrapping an agent session viable (spec §6.2): agents
// routinely leave work uncommitted, and the session must still be auditable.
//
// It is built against a throwaway index file rather than `git stash create`,
// which refuses to run at all once intent-to-add entries are present, and which
// cannot see untracked files in the first place.
func (r *Repo) Snapshot(ctx context.Context) (string, error) {
	dirty, err := r.WorkingTreeDirty(ctx)
	if err != nil || !dirty {
		return "", err
	}
	gitDir, err := r.git(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	index := filepath.Join(strings.TrimSpace(gitDir), "plum-snapshot-index")
	defer os.Remove(index)

	env := append(os.Environ(), "GIT_INDEX_FILE="+index)
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir, cmd.Env = r.Dir, env
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
		}
		return strings.TrimSpace(out.String()), nil
	}

	head, err := r.RevParse(ctx, "HEAD")
	if err != nil {
		return "", err
	}
	if _, err := run("read-tree", head); err != nil {
		return "", err
	}
	// -A picks up modifications, deletions and untracked files, still honouring
	// .gitignore. The real index is untouched because GIT_INDEX_FILE points away.
	if _, err := run("add", "-A", "."); err != nil {
		return "", err
	}
	tree, err := run("write-tree")
	if err != nil {
		return "", err
	}
	return run("commit-tree", tree, "-p", head, "-m", "plum: uncommitted work at session end")
}

// Diff returns the unified diff between two revisions with zero context lines,
// which keeps hunks tight and symbol mapping precise (spec §6.3).
func (r *Repo) Diff(ctx context.Context, from, to string) (string, error) {
	args := []string{"diff", "--unified=0", "--no-color", "--find-renames", from}
	if to != "" {
		args = append(args, to)
	}
	return r.git(ctx, args...)
}

// NameStatus returns the machine-readable file change list for a range.
func (r *Repo) NameStatus(ctx context.Context, from, to string) (string, error) {
	args := []string{"diff", "--numstat", "--find-renames", from}
	if to != "" {
		args = append(args, to)
	}
	return r.git(ctx, args...)
}

func (r *Repo) Status(ctx context.Context, from, to string) (string, error) {
	args := []string{"diff", "--name-status", "--find-renames", from}
	if to != "" {
		args = append(args, to)
	}
	return r.git(ctx, args...)
}

// Show reads a blob at a revision. Missing paths return ("", nil) so callers can
// treat "did not exist at StartSHA" as the added case.
func (r *Repo) Show(ctx context.Context, rev, path string) (string, error) {
	out, err := r.git(ctx, "show", rev+":"+path)
	if err != nil {
		return "", nil
	}
	return out, nil
}

// ListFiles returns tracked paths at a revision.
func (r *Repo) ListFiles(ctx context.Context, rev string) ([]string, error) {
	out, err := r.git(ctx, "ls-tree", "-r", "--name-only", rev)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, l)
		}
	}
	return files, nil
}

// Grep finds fixed-string occurrences at a revision, returning path:line pairs.
// Used to find the code that reads a configuration key, which is almost never
// the code that changed in the same session.
func (r *Repo) Grep(ctx context.Context, rev, needle string, pathspecs []string, max int) ([]GrepHit, error) {
	args := []string{"grep", "--no-color", "-n", "-I", "--fixed-strings", "-e", needle, rev}
	if len(pathspecs) > 0 {
		args = append(args, "--")
		args = append(args, pathspecs...)
	}
	out, err := r.git(ctx, args...)
	if err != nil {
		return nil, nil // git grep exits 1 when nothing matches: not an error here
	}
	var hits []GrepHit
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		// "<rev>:<path>:<line>:<text>"
		rest := strings.TrimPrefix(line, rev+":")
		path, remainder, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		lineNo, text, ok := strings.Cut(remainder, ":")
		if !ok {
			continue
		}
		n := 0
		for _, c := range lineNo {
			if c < '0' || c > '9' {
				n = 0
				break
			}
			n = n*10 + int(c-'0')
		}
		if n == 0 {
			continue
		}
		hits = append(hits, GrepHit{Path: path, Line: n, Text: strings.TrimSpace(text)})
		if max > 0 && len(hits) >= max {
			break
		}
	}
	return hits, nil
}

type GrepHit struct {
	Path string
	Line int
	Text string
}

// NotesRef is where plum attaches its analysis to a commit. Notes are the right
// primitive for this: they bind data to a commit after the fact, without
// rewriting history and without touching the commit message.
const NotesRef = "refs/notes/plum"

// NoteAdd attaches (or replaces) plum's note on a commit.
func (r *Repo) NoteAdd(ctx context.Context, commit, message string) error {
	_, err := r.git(ctx, "notes", "--ref", NotesRef, "add", "-f", "-m", message, commit)
	return err
}

// NoteShow reads plum's note for a commit, or "" when there is none.
func (r *Repo) NoteShow(ctx context.Context, commit string) (string, error) {
	out, err := r.git(ctx, "notes", "--ref", NotesRef, "show", commit)
	if err != nil {
		return "", nil // no note is not an error; it is the common case
	}
	return strings.TrimSpace(out), nil
}

// IsRevision reports whether a string names something git can resolve, so a
// command can accept either a session id or a commit.
func (r *Repo) IsRevision(ctx context.Context, ref string) bool {
	out, err := r.git(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil && strings.TrimSpace(out) != ""
}

// WorkingTreeID is a content fingerprint of the working tree: the id of the
// tree git would write if everything were staged right now.
//
// It has to be content, not `git status`, which reports only *which* paths
// changed. A second edit to an already-modified file leaves the status output
// identical, so a debounce built on that silently drops every turn after the
// first.
//
// Built against a throwaway index, so neither the real index nor the working
// tree is touched.
func (r *Repo) WorkingTreeID(ctx context.Context, excludes ...string) (string, error) {
	gitDir, err := r.git(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	index := filepath.Join(strings.TrimSpace(gitDir), "plum-fingerprint-index")
	defer os.Remove(index)

	env := append(os.Environ(), "GIT_INDEX_FILE="+index)
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir, cmd.Env = r.Dir, env
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
		}
		return strings.TrimSpace(out.String()), nil
	}

	if head, err := r.RevParse(ctx, "HEAD"); err == nil {
		if _, err := run("read-tree", head); err != nil {
			return "", err
		}
	}
	add := []string{"add", "-A", "--", "."}
	for _, ex := range excludes {
		add = append(add, ":(exclude)"+ex)
	}
	if _, err := run(add...); err != nil {
		return "", err
	}
	return run("write-tree")
}

// EmptyTree is git's canonical empty-tree object, used as the "before" side when
// a repository has no prior commit.
const EmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
