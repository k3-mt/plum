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

// EmptyTree is git's canonical empty-tree object, used as the "before" side when
// a repository has no prior commit.
const EmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
