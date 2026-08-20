// Instrumentation for the Go adapter: a scratch copy of the tree is rewritten so
// that each changed function reports its own calls and returns. Lives here, with
// the parser, rather than in the engine — the engine knows only that some
// adapters rewrite and some attach through the environment.
package gopkg

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/trace"
)

// Instrument rewrites a scratch copy of the tree, adding a deferred probe to
// the top of each changed function. It implements trace.Rewriter: Go cannot be
// instrumented by dropping in a file and setting an environment variable, so
// the work lives here, beside the parser that understands the language, rather
// than in the engine.
//
// The repository itself is never written to. Instrumentation is a property of a
// trace run, not of the code under audit.
func (a *Adapter) Instrument(scratchRoot string, ids, context []bundle.SymbolID) (trace.Instrumented, error) {
	var out trace.Instrumented
	modPath, err := modulePath(filepath.Join(scratchRoot, "go.mod"))
	if err != nil {
		return out, fmt.Errorf("tracing Go needs a go.mod at the repo root: %w", err)
	}
	shimDir := filepath.Join(scratchRoot, "plumtrace")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		return out, err
	}
	if err := os.WriteFile(filepath.Join(shimDir, "plumtrace.go"), []byte(trace.GoShimSource), 0o644); err != nil {
		return out, err
	}
	deep := map[bundle.SymbolID]bool{}
	byFile := map[string][]bundle.SymbolID{}
	for _, id := range ids {
		deep[id] = true
		byFile[id.File()] = append(byFile[id.File()], id)
	}
	for _, id := range context {
		if deep[id] {
			continue
		}
		byFile[id.File()] = append(byFile[id.File()], id)
	}
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, file := range files {
		done, skipped, err := inject(filepath.Join(scratchRoot, file), file, byFile[file], deep, modPath+"/plumtrace")
		if err != nil {
			out.Skipped = append(out.Skipped, file+": "+err.Error())
			continue
		}
		for _, id := range done {
			if deep[id] {
				out.Done = append(out.Done, id)
			}
		}
		out.Skipped = append(out.Skipped, skipped...)
	}

	// Tests are instrumented too, but only as labels: entering one names the
	// recording that follows. Without this every frame is attributed to the run
	// as a whole, and "which test exercises this?" has no answer.
	if err := markTests(scratchRoot, modPath+"/plumtrace"); err != nil {
		out.Skipped = append(out.Skipped, "test attribution: "+err.Error())
	}
	return out, nil
}

// markTests injects a label probe at the top of every Test function in the
// scratch copy, so each recorded frame knows which test it ran under.
func markTests(scratchRoot, shimImport string) error {
	return filepath.Walk(scratchRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			return nil
		}
		marked := false
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv != nil || !isTestFunc(fn) {
				continue
			}
			fn.Body.List = append([]ast.Stmt{&ast.DeferStmt{Call: &ast.CallExpr{
				Fun: &ast.CallExpr{
					Fun: &ast.SelectorExpr{X: ast.NewIdent("plumtrace"), Sel: ast.NewIdent("EnterTest")},
					Args: []ast.Expr{&ast.BasicLit{
						Kind: token.STRING, Value: strconv.Quote(fn.Name.Name),
					}},
				},
			}}}, fn.Body.List...)
			marked = true
		}
		if !marked {
			return nil
		}
		addImport(f, shimImport)
		var buf bytes.Buffer
		if err := format.Node(&buf, fset, f); err != nil {
			return nil
		}
		return os.WriteFile(path, buf.Bytes(), 0o644)
	})
}

// isTestFunc recognises the shapes `go test` will actually run.
func isTestFunc(fn *ast.FuncDecl) bool {
	name := fn.Name.Name
	if !strings.HasPrefix(name, "Test") && !strings.HasPrefix(name, "Benchmark") && !strings.HasPrefix(name, "Fuzz") {
		return false
	}
	if len(name) > 4 && name[4] >= 'a' && name[4] <= 'z' {
		return false // TestingHelper is not a test
	}
	return fn.Type.Params != nil && len(fn.Type.Params.List) == 1
}

// inject rewrites one file in the scratch copy, adding a deferred probe to the
// top of each target function body.
func inject(path, rel string, ids []bundle.SymbolID, deep map[bundle.SymbolID]bool, shimImport string) ([]bundle.SymbolID, []string, error) {
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
		if deep[id] {
			nameResults(fn)
		}
		stmt, err := probeStmt(id, fn, deep[id])
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
//
// A context frame gets `defer plumtrace.EnterContext("id")()` instead: the
// shape of the path, with nothing captured from it.
func probeStmt(id bundle.SymbolID, fn *ast.FuncDecl, deep bool) (ast.Stmt, error) {
	if !deep {
		return &ast.DeferStmt{Call: &ast.CallExpr{
			Fun: &ast.CallExpr{
				Fun:  &ast.SelectorExpr{X: ast.NewIdent("plumtrace"), Sel: ast.NewIdent("EnterContext")},
				Args: []ast.Expr{&ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(string(id))}},
			},
		}}, nil
	}
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
