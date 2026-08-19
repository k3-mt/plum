package trace

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kelalaike/plum/internal/bundle"
)

// Collector runs a repository's test suite with only the changed symbols
// instrumented. The AST pass already named them, which is why M2 is nearly free
// once M0 exists (spec §9.1).
type Collector struct {
	Root        string // repo root
	Scratch     string // where the instrumented copy is built
	TestCommand string
	MaxEvents   int
	Out         io.Writer // progress
}

type Result struct {
	Events       []Event
	Instrumented []bundle.SymbolID
	Skipped      []string
	TestOutput   string
	TestErr      error
	ScratchDir   string
}

// Run copies the tree, attaches the instrumentation each language needs, runs
// the suite and ingests the JSONL. Go is instrumented by rewriting the scratch
// copy; Python attaches a sys.monitoring shim through the environment. Both can
// be active in the same run — the event schema is the same either way.
func (c *Collector) Run(ctx context.Context, b *bundle.Bundle) (*Result, error) {
	goIDs := targetsFor(b, ".go")
	pyIDs := targetsFor(b, ".py")
	if len(goIDs)+len(pyIDs) == 0 {
		return nil, fmt.Errorf("no instrumentable symbols in this session (tracing covers Go and Python; other languages need a shim under shims/)")
	}
	if err := os.RemoveAll(c.Scratch); err != nil {
		return nil, err
	}
	if err := copyTree(c.Root, c.Scratch); err != nil {
		return nil, fmt.Errorf("copying the tree to %s: %w", c.Scratch, err)
	}

	res := &Result{ScratchDir: c.Scratch}
	env := []string{"PLUM_TRACE=1", "PLUM_REPO_ROOT=" + c.Scratch}

	if len(goIDs) > 0 {
		if err := c.instrumentGo(b, goIDs, res); err != nil {
			res.Skipped = append(res.Skipped, "go: "+err.Error())
		}
	}
	if len(pyIDs) > 0 {
		pyEnv, err := c.instrumentPython(pyIDs, res)
		if err != nil {
			res.Skipped = append(res.Skipped, "python: "+err.Error())
		} else {
			env = append(env, pyEnv...)
		}
	}
	if len(res.Instrumented) == 0 {
		return res, fmt.Errorf("nothing could be instrumented: %s", strings.Join(res.Skipped, "; "))
	}

	tracePath := filepath.Join(c.Scratch, "plum-trace.jsonl")
	if err := os.WriteFile(tracePath, nil, 0o644); err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", c.TestCommand)
	cmd.Dir = c.Scratch
	cmd.Env = append(os.Environ(), append(env,
		"PLUM_TRACE_OUT="+tracePath,
		fmt.Sprintf("PLUM_TRACE_MAX=%d", c.MaxEvents),
	)...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	res.TestErr = cmd.Run() // a failing suite still produced real execution
	res.TestOutput = buf.String()

	events, err := ReadFile(tracePath)
	if err != nil {
		return res, fmt.Errorf("reading trace output: %w", err)
	}
	SortByTime(events)
	res.Events = events
	return res, nil
}

// targetsFor is the instrumentation set for one language: changed, non-deleted,
// non-test functions. Only symbols present in Bundle.Symbols are ever
// instrumented — the AST pass decided this, and paying for anything else is waste.
func targetsFor(b *bundle.Bundle, ext string) []bundle.SymbolID {
	var out []bundle.SymbolID
	for _, s := range b.Symbols {
		if s.Change == "deleted" || filepath.Ext(s.File) != ext {
			continue
		}
		if s.Kind != "func" && s.Kind != "method" {
			continue
		}
		if isTestPath(s.File) || s.Name == "init" || s.Name == "main" {
			continue
		}
		out = append(out, s.ID)
	}
	return out
}

func isTestPath(path string) bool {
	base := filepath.Base(path)
	return strings.HasSuffix(base, "_test.go") ||
		strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") ||
		strings.Contains(filepath.ToSlash(path), "/tests/")
}

// instrumentGo rewrites the scratch copy, adding a deferred probe to each target.
func (c *Collector) instrumentGo(b *bundle.Bundle, ids []bundle.SymbolID, res *Result) error {
	modPath, err := modulePath(filepath.Join(c.Scratch, "go.mod"))
	if err != nil {
		return fmt.Errorf("tracing Go needs a go.mod at the repo root: %w", err)
	}
	shimDir := filepath.Join(c.Scratch, "plumtrace")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(shimDir, "plumtrace.go"), []byte(GoShimSource), 0o644); err != nil {
		return err
	}
	byFile := map[string][]bundle.SymbolID{}
	for _, id := range ids {
		byFile[id.File()] = append(byFile[id.File()], id)
	}
	for file, fileIDs := range byFile {
		done, skipped, err := inject(filepath.Join(c.Scratch, file), file, fileIDs, modPath+"/plumtrace")
		if err != nil {
			res.Skipped = append(res.Skipped, file+": "+err.Error())
			continue
		}
		res.Instrumented = append(res.Instrumented, done...)
		res.Skipped = append(res.Skipped, skipped...)
	}
	return nil
}

// instrumentPython writes the sys.monitoring shim into the scratch copy and
// returns the environment that attaches it. Nothing in the project's own source
// is touched: CPython imports sitecustomize at startup when it is on PYTHONPATH,
// which reaches pytest, unittest and plain scripts identically.
func (c *Collector) instrumentPython(ids []bundle.SymbolID, res *Result) ([]string, error) {
	shimDir := filepath.Join(c.Scratch, ".plum-shim")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(shimDir, "plum_shim.py"), []byte(PythonShimSource), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(shimDir, "sitecustomize.py"), []byte(PythonSiteCustomize), 0o644); err != nil {
		return nil, err
	}
	symbols := make([]string, 0, len(ids))
	for _, id := range ids {
		symbols = append(symbols, string(id))
	}
	res.Instrumented = append(res.Instrumented, ids...)

	pythonPath := shimDir
	if existing := os.Getenv("PYTHONPATH"); existing != "" {
		pythonPath += string(os.PathListSeparator) + existing
	}
	return []string{
		"PYTHONPATH=" + pythonPath,
		"PLUM_SYMBOLS=" + strings.Join(symbols, ","),
		"PYTHONDONTWRITEBYTECODE=1",
	}, nil
}

// inject rewrites one file in the scratch copy, adding a deferred probe to the
// top of each target function body.
func inject(path, rel string, ids []bundle.SymbolID, shimImport string) ([]bundle.SymbolID, []string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, nil, err
	}
	want := map[string]bundle.SymbolID{}
	for _, id := range ids {
		want[id.Qualified()] = id
	}

	var done []bundle.SymbolID
	var skipped []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		qual := fn.Name.Name
		if fn.Recv != nil && len(fn.Recv.List) > 0 {
			qual = recvName(fn.Recv.List[0].Type) + "." + fn.Name.Name
		}
		id, ok := want[qual]
		if !ok {
			continue
		}
		nameResults(fn)
		stmt, err := probeStmt(id, fn)
		if err != nil {
			skipped = append(skipped, string(id)+": "+err.Error())
			continue
		}
		fn.Body.List = append([]ast.Stmt{stmt}, fn.Body.List...)
		done = append(done, id)
	}
	if len(done) == 0 {
		return nil, skipped, nil
	}
	addImport(f, shimImport)

	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		return nil, skipped, err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return nil, skipped, err
	}
	return done, skipped, nil
}

// probeStmt builds `defer plumtrace.Enter("id", plumtrace.KV{...})(&r1, &r2)`.
// Named results are passed by pointer so the deferred half reads the value the
// caller actually saw; unnamed results are simply not recorded.
func probeStmt(id bundle.SymbolID, fn *ast.FuncDecl) (ast.Stmt, error) {
	call := &ast.CallExpr{
		Fun: &ast.SelectorExpr{X: ast.NewIdent("plumtrace"), Sel: ast.NewIdent("Enter")},
		Args: []ast.Expr{
			&ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(string(id))},
		},
	}
	if fn.Type.Params != nil {
		for _, p := range fn.Type.Params.List {
			if _, variadic := p.Type.(*ast.Ellipsis); variadic {
				continue // a variadic slice formats poorly and is rarely the question
			}
			for _, n := range p.Names {
				if n.Name == "_" {
					continue
				}
				call.Args = append(call.Args, &ast.CompositeLit{
					Type: &ast.SelectorExpr{X: ast.NewIdent("plumtrace"), Sel: ast.NewIdent("KV")},
					Elts: []ast.Expr{
						&ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(n.Name)},
						ast.NewIdent(n.Name),
					},
				})
			}
		}
	}

	outer := &ast.CallExpr{Fun: call}
	if fn.Type.Results != nil {
		for _, r := range fn.Type.Results.List {
			for _, n := range r.Names {
				if n.Name == "_" {
					continue
				}
				outer.Args = append(outer.Args, &ast.UnaryExpr{Op: token.AND, X: ast.NewIdent(n.Name)})
			}
		}
	}
	return &ast.DeferStmt{Call: outer}, nil
}

// nameResults gives unnamed results generated names, so the deferred half can
// read the value the caller actually saw. Naming a result never changes
// behaviour in Go — a bare `return a, b` still compiles and still returns a, b.
func nameResults(fn *ast.FuncDecl) {
	if fn.Type.Results == nil {
		return
	}
	i := 0
	for _, r := range fn.Type.Results.List {
		if len(r.Names) > 0 {
			i += len(r.Names)
			continue
		}
		r.Names = []*ast.Ident{ast.NewIdent(fmt.Sprintf("plumR%d", i))}
		i++
	}
}

func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return recvName(t.X)
	case *ast.IndexListExpr:
		return recvName(t.X)
	}
	return ""
}

func addImport(f *ast.File, path string) {
	for _, imp := range f.Imports {
		if imp.Path != nil && imp.Path.Value == strconv.Quote(path) {
			return
		}
	}
	spec := &ast.ImportSpec{Path: &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(path)}}
	for _, d := range f.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			gd.Specs = append(gd.Specs, spec)
			return
		}
	}
	f.Decls = append([]ast.Decl{&ast.GenDecl{Tok: token.IMPORT, Specs: []ast.Spec{spec}}}, f.Decls...)
}

func modulePath(gomod string) (string, error) {
	data, err := os.ReadFile(gomod)
	if err != nil {
		return "", err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module ")), nil
		}
	}
	return "", fmt.Errorf("no module line in %s", gomod)
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
