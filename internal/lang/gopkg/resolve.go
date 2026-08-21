package gopkg

import (
	"go/ast"
	"go/token"
	"strings"

	"github.com/k3-mt/plum/internal/bundle"
)

// ResolveIdentifier says what a name means inside the declaration containing a
// line: where it was introduced, what it was introduced from, where it is
// written and read, and — for a call — where it goes.
//
// It is a scope walk, not a text search, because the two disagree constantly.
// In one real function here, `sym := pc.Symbol_` makes sym derived from a
// parameter, `p := func(...)` makes p a local closure that a text search reports
// as a call, and `strings` is a package that a text search reports as a local.
// A reader clicking those three names deserves three different answers.
//
// It is honest about its own limits: this resolves within one declaration and
// against the file's imports. It is not a type checker, so a field selected off
// a value is reported as a field without claiming to know the value's type.
func (a *Adapter) ResolveIdentifier(path string, src []byte, line int, name string) (bundle.Resolution, error) {
	out := bundle.Resolution{Name: name, Kind: "unknown"}
	p, err := parse(path, src)
	if err != nil {
		return out, err
	}
	lineOf := func(pos token.Pos) int { return p.fset.Position(pos).Line }

	fn := enclosingFunc(p.file, lineOf, line)
	if fn == nil {
		// Still worth answering for a package-level name.
		if r, ok := a.packageLevel(p, lineOf, name); ok {
			return r, nil
		}
		out.Note = "no declaration contains that line"
		return out, nil
	}

	// A package qualifier is neither a variable nor a call, and saying so first
	// stops every `strings.Builder` reading as a local named strings.
	if imp, ok := importNamed(p.file, name); ok && usedAsQualifier(fn, lineOf, name) {
		out.Kind = "package"
		out.Type = imp
		out.DeclaredAt = lineOf(p.file.Pos())
		out.Reads = identLines(fn, lineOf, name)
		return out, nil
	}

	// Parameters, results and the receiver come from the signature, which is
	// where a reader would look for them.
	if r, ok := signatureName(fn, p, lineOf, name); ok {
		r.Reads, r.Writes = readsAndWrites(fn, lineOf, name, p, line)
		return r, nil
	}

	// Everything declared inside the body: the := and var forms, and the range
	// and type-switch bindings that a text search cannot see are bindings at all.
	//
	// Scoped to the block the reader clicked in. One 190-line function here
	// declares kind twice — once in the body and once inside a loop — and they
	// are two different variables. Reporting them as one puts writes from a
	// stranger's scope in your list, which is worse than saying nothing.
	if r, scope, ok := localName(fn, p, lineOf, name, line); ok {
		r.Reads, r.Writes = readsAndWrites(scope, lineOf, name, p, line)
		return r, nil
	}

	// A call, resolved through the same edges the extractor records.
	if r, ok := callName(fn, p, lineOf, name); ok {
		return r, nil
	}

	// A package-level declaration in this file — a function, type or constant
	// the reader can go and read.
	if r, ok := a.packageLevel(p, lineOf, name); ok {
		r.Reads = identLines(fn, lineOf, name)
		return r, nil
	}

	// A field or method selected off something. Without a type checker the
	// receiver's type is not known, and claiming one would be the kind of
	// confident wrong answer this whole thing exists to avoid.
	if selectedAnywhere(fn, name) {
		out.Kind = "field"
		out.Reads = identLines(fn, lineOf, name)
		out.Note = "selected off a value; plum does not type-check, so what it belongs to is not resolved here"
		return out, nil
	}

	out.Reads = identLines(fn, lineOf, name)
	if len(out.Reads) == 0 {
		out.Note = "not found in this declaration"
	} else {
		out.Note = "declared outside this file"
	}
	return out, nil
}

func enclosingFunc(f *ast.File, lineOf func(token.Pos) int, line int) *ast.FuncDecl {
	var found *ast.FuncDecl
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if lineOf(fn.Pos()) <= line && line <= lineOf(fn.End()) {
			found = fn
		}
	}
	return found
}

func importNamed(f *ast.File, name string) (string, bool) {
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		local := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			local = path[i+1:]
		}
		if imp.Name != nil {
			local = imp.Name.Name
		}
		if local == name {
			return path, true
		}
	}
	return "", false
}

// usedAsQualifier keeps a variable that shadows an import name from being
// reported as the import.
func usedAsQualifier(fn *ast.FuncDecl, lineOf func(token.Pos) int, name string) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return true
	})
	return found
}

func signatureName(fn *ast.FuncDecl, p *parsed, lineOf func(token.Pos) int, name string) (bundle.Resolution, bool) {
	kinds := []struct {
		list *ast.FieldList
		kind string
	}{
		{fn.Recv, "receiver"},
		{fn.Type.Params, "parameter"},
		{fn.Type.Results, "result"},
	}
	for _, k := range kinds {
		if k.list == nil {
			continue
		}
		for _, f := range k.list.List {
			for _, id := range f.Names {
				if id.Name != name {
					continue
				}
				return bundle.Resolution{
					Name: name, Kind: k.kind,
					Type:       exprOf(p, f.Type),
					DeclaredAt: lineOf(id.Pos()),
					Doc:        fieldDoc(f),
				}, true
			}
		}
	}
	return bundle.Resolution{}, false
}

func fieldDoc(f *ast.Field) string {
	if f.Doc != nil {
		return strings.TrimSpace(f.Doc.Text())
	}
	if f.Comment != nil {
		return strings.TrimSpace(f.Comment.Text())
	}
	return ""
}

// localName finds where a name is introduced inside the body, and what it was
// introduced from. The second half is the answer a reader is usually after: the
// expression on the right of the assignment is where the value came from.
// localName finds where a name is introduced inside the body, and what it was
// introduced from. The second half is the answer a reader is usually after: the
// expression on the right of the assignment is where the value came from.
//
// It returns the block that binding belongs to as well, because a name means
// different things in different blocks and the uses of one are not the uses of
// the other.
func localName(fn *ast.FuncDecl, p *parsed, lineOf func(token.Pos) int, name string, at int) (bundle.Resolution, ast.Node, bool) {
	type candidate struct {
		res   bundle.Resolution
		scope ast.Node
		span  int
	}
	var best *candidate

	// The innermost binding visible from the clicked line wins. Scopes nest, so
	// the smallest enclosing one is the one the reader is standing in.
	offer := func(scope ast.Node, id *ast.Ident, kind string, from, typ ast.Expr) {
		if id == nil || id.Name != name || scope == nil {
			return
		}
		lo, hi := lineOf(scope.Pos()), lineOf(scope.End())
		if at != 0 && (at < lo || at > hi) {
			return
		}
		span := hi - lo
		if best != nil && best.span <= span {
			return
		}
		best = &candidate{
			res: bundle.Resolution{
				Name: name, Kind: kind,
				DeclaredAt:  lineOf(id.Pos()),
				DerivedFrom: exprOf(p, from),
				Type:        exprOf(p, typ),
			},
			scope: scope, span: span,
		}
	}

	// scopeOf returns the innermost node that binds declarations for a
	// statement: a block, or the statement itself for the forms that bind in
	// their own header.
	var stack []ast.Node
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if n == nil {
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			return true
		}
		switch n.(type) {
		case *ast.BlockStmt, *ast.ForStmt, *ast.RangeStmt, *ast.IfStmt,
			*ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt,
			*ast.CaseClause, *ast.CommClause, *ast.FuncLit:
			stack = append(stack, n)
		default:
			stack = append(stack, nil) // keeps push and pop balanced
		}
		var here ast.Node = fn.Body
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i] != nil {
				here = stack[i]
				break
			}
		}

		switch s := n.(type) {
		case *ast.AssignStmt:
			if s.Tok != token.DEFINE {
				return true
			}
			for i, lhs := range s.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				var from ast.Expr
				// One value on the right shared by several names on the left is
				// a multiple return: each name comes from the same call.
				if len(s.Rhs) == len(s.Lhs) {
					from = s.Rhs[i]
				} else if len(s.Rhs) == 1 {
					from = s.Rhs[0]
				}
				kind := "local"
				if _, isFunc := from.(*ast.FuncLit); isFunc {
					kind = "local function"
				}
				offer(here, id, kind, from, nil)
			}
		case *ast.ValueSpec:
			for i, id := range s.Names {
				var from ast.Expr
				if i < len(s.Values) {
					from = s.Values[i]
				}
				offer(here, id, "local", from, s.Type)
			}
		case *ast.RangeStmt:
			if id, ok := s.Key.(*ast.Ident); ok {
				offer(s, id, "range key", s.X, nil)
			}
			if id, ok := s.Value.(*ast.Ident); ok {
				offer(s, id, "range value", s.X, nil)
			}
		case *ast.TypeSwitchStmt:
			if a, ok := s.Assign.(*ast.AssignStmt); ok && len(a.Lhs) == 1 && len(a.Rhs) == 1 {
				if id, ok := a.Lhs[0].(*ast.Ident); ok {
					offer(s, id, "type switch binding", a.Rhs[0], nil)
				}
			}
		}
		return true
	})
	if best == nil {
		return bundle.Resolution{}, nil, false
	}
	return best.res, best.scope, true
}

func callName(fn *ast.FuncDecl, p *parsed, lineOf func(token.Pos) int, name string) (bundle.Resolution, bool) {
	var out bundle.Resolution
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || found {
			return true
		}
		raw := exprOf(p, call.Fun)
		if raw != name && lastSegment(raw) != name {
			return true
		}
		found = true
		out = bundle.Resolution{
			Name: name, Kind: "call",
			DeclaredAt: lineOf(call.Pos()),
			Type:       raw,
		}
		return false
	})
	return out, found
}

func (a *Adapter) packageLevel(p *parsed, lineOf func(token.Pos) int, name string) (bundle.Resolution, bool) {
	for _, d := range p.file.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			if decl.Name.Name == name && decl.Recv == nil {
				return bundle.Resolution{
					Name: name, Kind: "function",
					DeclaredAt: lineOf(decl.Pos()),
					Type:       exprOf(p, decl.Type),
					Doc:        docText(decl.Doc),
				}, true
			}
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					if sp.Name.Name == name {
						return bundle.Resolution{
							Name: name, Kind: "type",
							DeclaredAt: lineOf(sp.Pos()),
							Doc:        docText(decl.Doc),
						}, true
					}
				case *ast.ValueSpec:
					for _, id := range sp.Names {
						if id.Name != name {
							continue
						}
						kind := "package variable"
						if decl.Tok == token.CONST {
							kind = "constant"
						}
						var from ast.Expr
						if len(sp.Values) > 0 {
							from = sp.Values[0]
						}
						return bundle.Resolution{
							Name: name, Kind: kind,
							DeclaredAt:  lineOf(id.Pos()),
							DerivedFrom: exprOf(p, from),
							Type:        exprOf(p, sp.Type),
							Doc:         docText(decl.Doc),
						}, true
					}
				}
			}
		}
	}
	return bundle.Resolution{}, false
}

// readsAndWrites separates the lines that change a name from the ones that only
// look at it. A name reassigned halfway through is a different thing to follow
// than one set once, and the flat "used at" list hid that difference.
// readsAndWrites separates the lines that change a name from the ones that only
// look at it, within the scope that binds it. A name reassigned halfway through
// is a different thing to follow than one set once, and a flat list of uses hid
// both that difference and the boundary between two same-named variables.
func readsAndWrites(scope ast.Node, lineOf func(token.Pos) int, name string, p *parsed, at int) (reads, writes []int) {
	// A nested block that rebinds the same name is a different variable, and
	// its lines belong to it rather than to this one.
	shadowed := map[int]bool{}
	ast.Inspect(scope, func(n ast.Node) bool {
		inner, ok := n.(*ast.BlockStmt)
		if !ok || inner == scope {
			return true
		}
		if !bindsIn(inner, name) {
			return true
		}
		lo, hi := lineOf(inner.Pos()), lineOf(inner.End())
		if at != 0 && at >= lo && at <= hi {
			return true // the reader is inside it; this is their scope
		}
		for ln := lo; ln <= hi; ln++ {
			shadowed[ln] = true
		}
		return false
	})

	written := map[int]bool{}
	ast.Inspect(scope, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range s.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == name {
					written[lineOf(id.Pos())] = true
				}
			}
		case *ast.IncDecStmt:
			if id, ok := s.X.(*ast.Ident); ok && id.Name == name {
				written[lineOf(id.Pos())] = true
			}
		}
		return true
	})
	for _, ln := range identLines(scope, lineOf, name) {
		if shadowed[ln] {
			continue
		}
		if written[ln] {
			writes = append(writes, ln)
			continue
		}
		reads = append(reads, ln)
	}
	return reads, writes
}

// bindsIn reports whether a block introduces its own binding for a name, which
// is what makes it a different variable from the one outside it.
func bindsIn(block *ast.BlockStmt, name string) bool {
	found := false
	for _, stmt := range block.List {
		switch s := stmt.(type) {
		case *ast.AssignStmt:
			if s.Tok != token.DEFINE {
				continue
			}
			for _, lhs := range s.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == name {
					found = true
				}
			}
		case *ast.DeclStmt:
			if gen, ok := s.Decl.(*ast.GenDecl); ok {
				for _, spec := range gen.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, id := range vs.Names {
							if id.Name == name {
								found = true
							}
						}
					}
				}
			}
		}
	}
	return found
}

func identLines(n ast.Node, lineOf func(token.Pos) int, name string) []int {
	seen := map[int]bool{}
	var out []int
	ast.Inspect(n, func(node ast.Node) bool {
		id, ok := node.(*ast.Ident)
		if !ok || id.Name != name {
			return true
		}
		ln := lineOf(id.Pos())
		if !seen[ln] {
			seen[ln] = true
			out = append(out, ln)
		}
		return true
	})
	return out
}

func selectedAnywhere(fn *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
			found = true
		}
		return true
	})
	return found
}

// exprOf is the file's canonical rendering of an expression, and tolerates the
// nil node that a declaration without an initialiser produces.
func exprOf(p *parsed, n ast.Node) string {
	if n == nil || (n != nil && isNilExpr(n)) {
		return ""
	}
	return strings.Join(strings.Fields(p.text(n)), " ")
}

// isNilExpr catches a typed-nil ast.Expr held in an ast.Node interface, which is
// not caught by comparing the interface itself against nil.
func isNilExpr(n ast.Node) bool {
	switch v := n.(type) {
	case ast.Expr:
		return v == nil
	}
	return false
}
