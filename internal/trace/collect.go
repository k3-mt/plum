package trace

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/kelalaike/plum/internal/bundle"
)

// Collector runs a repository's test suite with only the changed symbols
// instrumented. The AST pass already named them, which is why M2 is nearly free
// once M0 exists (spec §9.1).
//
// The collector knows nothing about any particular language. It asks each
// adapter for a ShimSpec and honours it: writing the declared files into a
// scratch copy and setting the declared environment, or handing the scratch
// copy to an adapter that rewrites source itself. Adding a language means
// writing an adapter, not editing this file.
type Collector struct {
	Root        string // repo root
	Scratch     string // where the instrumented copy is built
	TestCommand string
	MaxEvents   int
	// Adapters decide what can be instrumented and how. Pass the same registry
	// the extractor used, so the instrumentation set matches the bundle.
	Adapters []Instrumenter
	Out      io.Writer // progress
}

type Result struct {
	Events       []Event
	Instrumented []bundle.SymbolID
	Skipped      []string
	Languages    []string
	TestOutput   string
	TestErr      error
	ScratchDir   string
}

// Run copies the tree, attaches whatever instrumentation each adapter asks for,
// runs the suite and ingests the JSONL every shim speaks.
func (c *Collector) Run(ctx context.Context, b *bundle.Bundle) (*Result, error) {
	targets := c.targets(b)
	if len(targets) == 0 {
		return nil, fmt.Errorf("no instrumentable symbols in this session (%s)", c.coverageNote())
	}
	if err := os.RemoveAll(c.Scratch); err != nil {
		return nil, err
	}
	if err := copyTree(c.Root, c.Scratch); err != nil {
		return nil, fmt.Errorf("copying the tree to %s: %w", c.Scratch, err)
	}

	res := &Result{ScratchDir: c.Scratch}
	env := map[string]string{"PLUM_TRACE": "1", "PLUM_REPO_ROOT": c.Scratch}

	for _, a := range c.Adapters {
		ids := targets[a.Name()]
		if len(ids) == 0 {
			continue
		}
		spec, err := a.ShimSpec(ids)
		if err != nil {
			res.Skipped = append(res.Skipped, a.Name()+": "+err.Error())
			continue
		}
		applied, err := c.apply(a, spec, ids, env)
		if err != nil {
			res.Skipped = append(res.Skipped, a.Name()+": "+err.Error())
			continue
		}
		if len(applied.Done) > 0 {
			res.Languages = append(res.Languages, a.Name())
		}
		res.Instrumented = append(res.Instrumented, applied.Done...)
		res.Skipped = append(res.Skipped, applied.Skipped...)
	}
	if len(res.Instrumented) == 0 {
		return res, fmt.Errorf("nothing could be instrumented: %s", strings.Join(res.Skipped, "; "))
	}

	tracePath := filepath.Join(c.Scratch, "plum-trace.jsonl")
	if err := os.WriteFile(tracePath, nil, 0o644); err != nil {
		return nil, err
	}
	env["PLUM_TRACE_OUT"] = tracePath
	env["PLUM_TRACE_MAX"] = strconv.Itoa(c.MaxEvents)

	cmd := exec.CommandContext(ctx, "sh", "-c", c.TestCommand)
	cmd.Dir = c.Scratch
	cmd.Env = append(os.Environ(), flatten(env)...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	res.TestErr = cmd.Run() // a failing suite still produced real execution
	res.TestOutput = buf.String()

	events, err := ReadFile(tracePath)
	if err != nil {
		return res, fmt.Errorf("reading trace output: %w", err)
	}
	// A shim can only discover at runtime that a symbol cannot be instrumented —
	// an ES const binding cannot be rebound, for instance. It reports that in the
	// stream, and it is subtracted from the instrumented set rather than left as
	// a claim the traces do not support.
	var kept []Event
	for _, e := range events {
		if e.Kind == "uninstrumented" {
			res.Skipped = append(res.Skipped, string(e.Symbol)+": "+e.Exception)
			res.Instrumented = removeID(res.Instrumented, e.Symbol)
			continue
		}
		kept = append(kept, e)
	}
	SortByTime(kept)
	res.Events = kept
	return res, nil
}

func removeID(ids []bundle.SymbolID, drop bundle.SymbolID) []bundle.SymbolID {
	out := ids[:0]
	for _, id := range ids {
		if id != drop {
			out = append(out, id)
		}
	}
	return out
}

// apply honours one adapter's ShimSpec against the scratch copy.
func (c *Collector) apply(a Instrumenter, spec ShimSpec, ids []bundle.SymbolID, env map[string]string) (Instrumented, error) {
	switch spec.Mode {
	case "none", "":
		return Instrumented{}, nil

	case "rewrite":
		// Source instrumentation is the adapter's own business: it is the only
		// thing that knows how to put a probe inside a function of its language.
		r, ok := a.(Rewriter)
		if !ok {
			return Instrumented{}, fmt.Errorf("declares mode \"rewrite\" but does not implement trace.Rewriter")
		}
		out, err := r.Instrument(c.Scratch, ids)
		if err != nil {
			return out, err
		}
		for k, v := range out.Env {
			env[k] = v
		}
		return out, nil

	case "env":
		dir := spec.Dir
		if dir == "" {
			dir = ".plum-shim-" + a.Name()
		}
		abs := filepath.Join(c.Scratch, dir)
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return Instrumented{}, err
		}
		names := make([]string, 0, len(spec.Files))
		for name := range spec.Files {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			path := filepath.Join(abs, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return Instrumented{}, err
			}
			if err := os.WriteFile(path, []byte(spec.Files[name]), 0o644); err != nil {
				return Instrumented{}, err
			}
		}
		symbols := make([]string, 0, len(ids))
		for _, id := range ids {
			symbols = append(symbols, string(id))
		}
		expand := strings.NewReplacer("${SHIM_DIR}", abs, "${SYMBOLS}", strings.Join(symbols, ","))
		for k, v := range spec.Env {
			env[k] = expand.Replace(v)
		}
		// Path-list variables are prepended to, never replaced: the project may
		// depend on its own PYTHONPATH or NODE_PATH to import at all.
		for _, name := range spec.PathVars {
			value := abs
			if existing, ok := env[name]; ok && existing != "" {
				value = abs + string(os.PathListSeparator) + existing
			} else if existing := os.Getenv(name); existing != "" {
				value = abs + string(os.PathListSeparator) + existing
			}
			env[name] = value
		}
		return Instrumented{Done: ids}, nil
	}
	return Instrumented{}, fmt.Errorf("unknown shim mode %q", spec.Mode)
}

// targets groups the instrumentation set by adapter: changed, non-deleted,
// non-test functions whose file that adapter claims. Only symbols present in
// Bundle.Symbols are ever instrumented, and paying for anything else is waste.
func (c *Collector) targets(b *bundle.Bundle) map[string][]bundle.SymbolID {
	out := map[string][]bundle.SymbolID{}
	for _, s := range b.Symbols {
		if s.Change == "deleted" || isTestPath(s.File) {
			continue
		}
		if s.Kind != "func" && s.Kind != "method" {
			continue
		}
		if s.Name == "init" || s.Name == "main" {
			continue
		}
		if a := c.adapterFor(s.File); a != nil {
			out[a.Name()] = append(out[a.Name()], s.ID)
		}
	}
	return out
}

func (c *Collector) adapterFor(path string) Instrumenter {
	ext := strings.ToLower(filepath.Ext(path))
	for _, a := range c.Adapters {
		for _, e := range a.Extensions() {
			if e == ext {
				return a
			}
		}
	}
	return nil
}

// coverageNote says which languages this build can actually trace, so a session
// with nothing to instrument explains itself.
func (c *Collector) coverageNote() string {
	var names []string
	for _, a := range c.Adapters {
		if spec, err := a.ShimSpec(nil); err == nil && spec.Mode != "none" {
			names = append(names, a.Name())
		}
	}
	if len(names) == 0 {
		return "no configured language has a shim"
	}
	return "tracing covers " + strings.Join(names, ", ") + " in this repository's configured languages"
}

func isTestPath(path string) bool {
	base := filepath.Base(path)
	return strings.HasSuffix(base, "_test.go") ||
		strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") ||
		strings.HasSuffix(base, ".test.ts") || strings.HasSuffix(base, ".test.js") ||
		strings.HasSuffix(base, ".spec.ts") || strings.HasSuffix(base, ".spec.js") ||
		strings.Contains(filepath.ToSlash(path), "/tests/")
}

func flatten(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// copyTree mirrors the working tree into the scratch directory, skipping .git
// and anything plum itself produced.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		base := filepath.Base(rel)
		if d.IsDir() {
			switch base {
			case ".git", "node_modules", ".venv", "dist", "bin":
				return filepath.SkipDir
			}
			if strings.HasPrefix(rel, filepath.Join(".plum", "sessions")) {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(filepath.Join(dst, rel))
		if err != nil {
			return err
		}
		defer out.Close()
		w := bufio.NewWriter(out)
		if _, err := io.Copy(w, in); err != nil {
			return err
		}
		return w.Flush()
	})
}

// CopyTree is exported for the claims runner, which needs the same
// scratch-copy semantics: never write generated tests into the real repo.
func CopyTree(src, dst string) error { return copyTree(src, dst) }
