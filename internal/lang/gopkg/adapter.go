// Package gopkg is the native Go adapter. It uses stdlib go/ast and go/parser:
// free, exact, no bindings, and the reason M0 ships without tree-sitter (spec §3.3).
package gopkg

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/scanner"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/trace"
)

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string         { return "go" }
func (a *Adapter) Extensions() []string { return []string{".go"} }

type parsed struct {
	fset *token.FileSet
	file *ast.File
	src  []byte
}

func parse(path string, src []byte) (*parsed, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &parsed{fset: fset, file: f, src: src}, nil
}

func (p *parsed) line(pos token.Pos) int { return p.fset.Position(pos).Line }

func (p *parsed) text(n ast.Node) string {
	var buf bytes.Buffer
	// printer.Fprint gives a canonical rendering, which keeps signatures stable
	// against reformatting in the source.
	if err := printer.Fprint(&buf, p.fset, n); err != nil {
		return ""
	}
	return buf.String()
}

// ParseSymbols returns every declaration in the file with line spans, docs,
// body comments, outbound call sites and a normalised fingerprint.
func (a *Adapter) ParseSymbols(path string, src []byte) ([]bundle.Symbol, error) {
	p, err := parse(path, src)
	if err != nil {
		return nil, err
	}
	rel := filepath.ToSlash(path)
	comments := commentIndex(p)

	// Method calls are written `c.decorate` but declared as `Cache.decorate`, so
	// a call site can only be bound to its declaration through this index.
	// Without it the rationale comment above a method call never attaches.
	local := map[string]string{}
	for _, d := range p.file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok {
			qual := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				qual = receiverName(p, fn.Recv.List[0].Type) + "." + fn.Name.Name
			}
			local[fn.Name.Name] = qual
		}
	}

	var out []bundle.Symbol
	for _, d := range p.file.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			out = append(out, a.funcSymbol(p, rel, decl, comments, local))
		case *ast.GenDecl:
			out = append(out, a.genSymbols(p, rel, decl)...)
		}
	}
	return out, nil
}

func (a *Adapter) funcSymbol(p *parsed, rel string, fn *ast.FuncDecl, comments map[int]*ast.CommentGroup, local map[string]string) bundle.Symbol {
	qual := fn.Name.Name
	kind := "func"
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		kind = "method"
		qual = receiverName(p, fn.Recv.List[0].Type) + "." + fn.Name.Name
	}
	start, end := fn.Pos(), fn.End()
	sym := bundle.Symbol{
		ID:        bundle.MakeID(rel, qual),
		Kind:      kind,
		Name:      qual,
		File:      rel,
		LineStart: p.line(start),
		LineEnd:   p.line(end),
		ByteStart: p.fset.Position(start).Offset,
		ByteEnd:   p.fset.Position(end).Offset,
		Signature: signature(p, fn),
		Doc:       docText(fn.Doc),
		Exported:  ast.IsExported(fn.Name.Name),
	}
	if fn.Body != nil {
		sym.Comments = bodyComments(p, fn.Body.Pos(), fn.Body.End())
		sym.CallSites = callSites(p, rel, fn, comments, local, receiverIdent(fn))
	}
	sym.Fingerprint = fingerprint(p.src, sym.ByteStart, sym.ByteEnd)
	return sym
}

func (a *Adapter) genSymbols(p *parsed, rel string, gd *ast.GenDecl) []bundle.Symbol {
	var out []bundle.Symbol
	for _, spec := range gd.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			sym := bundle.Symbol{
				ID:        bundle.MakeID(rel, s.Name.Name),
				Kind:      "type",
				Name:      s.Name.Name,
				File:      rel,
				LineStart: p.line(gd.Pos()),
				LineEnd:   p.line(s.End()),
				ByteStart: p.fset.Position(gd.Pos()).Offset,
				ByteEnd:   p.fset.Position(s.End()).Offset,
				Signature: "type " + s.Name.Name + " " + p.text(s.Type),
				Doc:       firstDoc(gd.Doc, s.Doc),
				Exported:  ast.IsExported(s.Name.Name),
			}
			sym.Fingerprint = fingerprint(p.src, sym.ByteStart, sym.ByteEnd)
			out = append(out, sym)
		case *ast.ValueSpec:
			kind := "var"
			if gd.Tok == token.CONST {
				kind = "const"
			}
			for _, name := range s.Names {
				if name.Name == "_" {
					continue
				}
				sym := bundle.Symbol{
					ID:        bundle.MakeID(rel, name.Name),
					Kind:      kind,
					Name:      name.Name,
					File:      rel,
					LineStart: p.line(s.Pos()),
					LineEnd:   p.line(s.End()),
					ByteStart: p.fset.Position(s.Pos()).Offset,
					ByteEnd:   p.fset.Position(s.End()).Offset,
					Signature: kind + " " + p.text(s),
					Doc:       firstDoc(gd.Doc, s.Doc),
					Exported:  ast.IsExported(name.Name),
				}
				sym.Fingerprint = fingerprint(p.src, sym.ByteStart, sym.ByteEnd)
				out = append(out, sym)
			}
		}
	}
	return out
}

func receiverName(p *parsed, e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return receiverName(p, t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // generic receiver: Cache[T]
		return receiverName(p, t.X)
	case *ast.IndexListExpr:
		return receiverName(p, t.X)
	}
	return strings.TrimPrefix(p.text(e), "*")
}

func signature(p *parsed, fn *ast.FuncDecl) string {
	var buf bytes.Buffer
	buf.WriteString("func ")
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		buf.WriteString("(" + p.text(fn.Recv.List[0].Type) + ") ")
	}
	buf.WriteString(fn.Name.Name)
	buf.WriteString(strings.TrimPrefix(p.text(fn.Type), "func"))
	return strings.Join(strings.Fields(buf.String()), " ")
}

func docText(g *ast.CommentGroup) string {
	if g == nil {
		return ""
	}
	return strings.TrimSpace(g.Text())
}

func firstDoc(gs ...*ast.CommentGroup) string {
	for _, g := range gs {
		if t := docText(g); t != "" {
			return t
		}
	}
	return ""
}

// commentIndex maps the line a comment block ends on to that block, so a call
// site can find the contiguous comment directly above it (spec §9.4).
func commentIndex(p *parsed) map[int]*ast.CommentGroup {
	m := map[int]*ast.CommentGroup{}
	for _, g := range p.file.Comments {
		m[p.line(g.End())] = g
	}
	return m
}

func bodyComments(p *parsed, from, to token.Pos) []bundle.Comment {
	var out []bundle.Comment
	for _, g := range p.file.Comments {
		if g.Pos() >= from && g.End() <= to {
			out = append(out, bundle.Comment{
				Text:      strings.TrimSpace(g.Text()),
				LineStart: p.line(g.Pos()),
				LineEnd:   p.line(g.End()),
			})
		}
	}
	return out
}

// callSites walks a function body binding each outbound call to the comment
// block immediately above it. "" rationale means the call was never explained —
// on an expensive barrier that is itself a finding.
// receiverIdent is the method's receiver variable, so `c.decorate` can be told
// apart from `http.Get`.
func receiverIdent(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 || len(fn.Recv.List[0].Names) == 0 {
		return ""
	}
	return fn.Recv.List[0].Names[0].Name
}

func callSites(p *parsed, rel string, fn *ast.FuncDecl, comments map[int]*ast.CommentGroup, local map[string]string, recv string) []bundle.CallSite {
	var out []bundle.CallSite
	seen := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := calleeName(call.Fun)
		if name == "" {
			return true
		}
		line := p.line(call.Pos())
		key := name + ":" + strconv.Itoa(line)
		if seen[key] {
			return true
		}
		seen[key] = true
		// Bind to a local declaration only when the call's shape says it is one:
		// a bare `helper()`, or `c.decorate()` on this method's own receiver.
		// Binding on the bare name alone turns `http.Get` into `Cache.Get`.
		callee := name
		switch {
		case !strings.Contains(name, "."):
			if qual, ok := local[name]; ok {
				callee = qual
			}
		case recv != "" && strings.HasPrefix(name, recv+".") && strings.Count(name, ".") == 1:
			if qual, ok := local[strings.TrimPrefix(name, recv+".")]; ok {
				callee = qual // c.decorate -> Cache.decorate
			}
		}
		cs := bundle.CallSite{
			Callee:    bundle.MakeID(rel, callee),
			CalleeRaw: name,
			Line:      line,
		}
		if g, ok := comments[line-1]; ok {
			cs.Rationale = docText(g)
		}
		out = append(out, cs)
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

func calleeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		if x := calleeName(t.X); x != "" {
			return x + "." + t.Sel.Name
		}
		return t.Sel.Name
	case *ast.IndexExpr:
		return calleeName(t.X)
	case *ast.IndexListExpr:
		return calleeName(t.X)
	case *ast.ParenExpr:
		return calleeName(t.X)
	}
	return ""
}

func lastSegment(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

// Comments returns every comment in the file with its line span.
func (a *Adapter) Comments(path string, src []byte) ([]bundle.Comment, error) {
	p, err := parse(path, src)
	if err != nil {
		return nil, err
	}
	var out []bundle.Comment
	for _, g := range p.file.Comments {
		out = append(out, bundle.Comment{
			Text:      strings.TrimSpace(g.Text()),
			LineStart: p.line(g.Pos()),
			LineEnd:   p.line(g.End()),
		})
	}
	return out, nil
}

// Normalise strips comments and collapses whitespace while preserving
// identifiers: reformatting must not invalidate a fingerprint, renaming must.
func (a *Adapter) Normalise(src []byte) ([]byte, error) {
	var out bytes.Buffer
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	var s scanner.Scanner
	// Errors are swallowed: a fragment sliced out of a file is frequently not a
	// complete compilation unit, and the token stream is still stable evidence.
	s.Init(file, src, func(token.Position, string) {}, 0)
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue // implicit semicolons are formatting, not content
		}
		if out.Len() > 0 {
			out.WriteByte(' ')
		}
		if lit != "" {
			out.WriteString(lit)
		} else {
			out.WriteString(tok.String())
		}
	}
	return out.Bytes(), nil
}

func fingerprint(src []byte, from, to int) string {
	if from < 0 || to > len(src) || from >= to {
		return ""
	}
	a := &Adapter{}
	norm, err := a.Normalise(src[from:to])
	if err != nil {
		return ""
	}
	return Hash(norm)
}

// ShimSpec instruments Go by rewriting a scratch copy of the tree: each named
// symbol gets a deferred probe at function entry. Only the changed set is
// touched, which is why M2 is nearly free once M0 exists (spec §9.1).
func (a *Adapter) ShimSpec(syms []bundle.SymbolID) (trace.ShimSpec, error) {
	return trace.ShimSpec{
		Language: "go",
		Mode:     "rewrite",
		Symbols:  syms,
		Env:      map[string]string{"PLUM_TRACE": "1"},
	}, nil
}
