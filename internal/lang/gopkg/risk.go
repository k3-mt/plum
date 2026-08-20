package gopkg

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strings"

	"github.com/kelalaike/plum/internal/bundle"
)

// RiskMarkers runs the AST predicates from spec §6.5. Each predicate is small,
// mechanical and returns zero or more markers — no scoring, no prose.
func (a *Adapter) RiskMarkers(path string, src []byte, syms []bundle.Symbol) ([]bundle.RiskMarker, error) {
	p, err := parse(path, src)
	if err != nil {
		return nil, err
	}
	rel := filepath.ToSlash(path)
	changed := map[bundle.SymbolID]bool{}
	for _, s := range syms {
		changed[s.ID] = true
	}
	var out []bundle.RiskMarker
	mark := func(kind string, id bundle.SymbolID, pos token.Pos, note string) {
		if len(changed) > 0 && !changed[id] {
			return // only report inside the changed set
		}
		out = append(out, bundle.RiskMarker{
			Kind: kind, Symbol: id, File: rel, Line: p.line(pos), Note: note,
		})
	}

	// A package-level var is only shared *mutable* state if something can write
	// it. A compiled regex, a sentinel error or a lookup table assigned once is a
	// constant in everything but syntax, and flagging it is the false-positive
	// class that makes people stop reading the report (spec §7).
	mutated := mutatedIdents(p)
	for _, d := range p.file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, n := range vs.Names {
				if n.Name == "_" {
					continue
				}
				note := "package-level mutable var " + n.Name + " — shared across every caller and every test"
				switch {
				case mutated[n.Name]:
				case ast.IsExported(n.Name):
					note = "exported package-level var " + n.Name + " — any importing package can write it"
				default:
					continue // assigned once and never written: a table, not state
				}
				mark("package_level_state", bundle.MakeID(rel, n.Name), n.Pos(), note)
			}
		}
	}

	for _, d := range p.file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		qual := fn.Name.Name
		if fn.Recv != nil && len(fn.Recv.List) > 0 {
			qual = receiverName(p, fn.Recv.List[0].Type) + "." + fn.Name.Name
		}
		id := bundle.MakeID(rel, qual)

		hasWaitGroup := containsIdent(fn.Body, "WaitGroup") || containsIdent(fn.Body, "errgroup") || containsIdent(fn.Body, "Wait")
		hasSleep := containsSelector(fn.Body, "time", "Sleep") || containsSelector(fn.Body, "time", "After") ||
			containsIdent(fn.Body, "Backoff") || containsIdent(fn.Body, "backoff")

		if fn.Name.Name == "init" {
			mark("init_side_effects", id, fn.Pos(), "init() runs implicitly at import time — invisible at the call site")
		}

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.IfStmt:
				// if err != nil {} — the error was observed and discarded.
				if isErrNilCheck(node.Cond) && node.Body != nil && len(node.Body.List) == 0 {
					mark("swallowed_error", id, node.Pos(), "error checked and then discarded")
				}
			case *ast.AssignStmt:
				if isBlankErrorAssign(node) {
					mark("swallowed_error", id, node.Pos(), "result assigned to _ — a returned error is dropped here")
				}
			case *ast.GoStmt:
				if !hasWaitGroup {
					mark("unsynchronised_goroutine", id, node.Pos(),
						"goroutine started with no WaitGroup/Wait in scope — completion is unobservable")
				}
			case *ast.DeferStmt:
				if call, ok := node.Call.Fun.(*ast.FuncLit); ok && recoversSilently(call) {
					mark("swallowed_panic", id, node.Pos(), "recover() with an empty body — the panic vanishes")
				}
			case *ast.ForStmt:
				if looksLikeRetry(node) && !hasSleep {
					mark("retry_without_backoff", id, node.Pos(), "retry loop with no sleep or backoff — a hot spin under failure")
				}
			case *ast.CallExpr:
				if kind, note := unboundedCall(calleeName(node.Fun)); kind != "" {
					mark(kind, id, node.Pos(), note)
				}
			}
			return true
		})

		for _, param := range widenedParams(fn) {
			mark("widened_type", id, fn.Pos(), "parameter "+param+" is typed as any/interface{} — the compiler stops helping callers")
		}
	}
	return out, nil
}

// mutatedIdents collects every identifier written to after its declaration:
// assignment, increment, decrement, or having its address taken.
func mutatedIdents(p *parsed) map[string]bool {
	out := map[string]bool{}
	record := func(e ast.Expr) {
		for {
			switch t := e.(type) {
			case *ast.Ident:
				out[t.Name] = true
				return
			case *ast.IndexExpr:
				e = t.X
			case *ast.SelectorExpr:
				e = t.X
			case *ast.StarExpr:
				e = t.X
			case *ast.ParenExpr:
				e = t.X
			default:
				return
			}
		}
	}
	ast.Inspect(p.file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			// The declaration itself is a GenDecl, never an AssignStmt, so every
			// assignment reaching here is a later write.
			for _, lhs := range node.Lhs {
				record(lhs)
			}
		case *ast.IncDecStmt:
			record(node.X)
		case *ast.UnaryExpr:
			if node.Op == token.AND {
				record(node.X)
			}
		case *ast.RangeStmt:
			if node.Key != nil {
				record(node.Key)
			}
			if node.Value != nil {
				record(node.Value)
			}
		}
		return true
	})
	return out
}

func isErrNilCheck(cond ast.Expr) bool {
	bin, ok := cond.(*ast.BinaryExpr)
	if !ok || bin.Op != token.NEQ {
		return false
	}
	x, ok := bin.X.(*ast.Ident)
	if !ok || !strings.Contains(strings.ToLower(x.Name), "err") {
		return false
	}
	y, ok := bin.Y.(*ast.Ident)
	return ok && y.Name == "nil"
}

func isBlankErrorAssign(as *ast.AssignStmt) bool {
	if len(as.Lhs) == 0 || len(as.Rhs) != 1 {
		return false
	}
	if _, ok := as.Rhs[0].(*ast.CallExpr); !ok {
		return false
	}
	allBlank := true
	for _, l := range as.Lhs {
		id, ok := l.(*ast.Ident)
		if !ok || id.Name != "_" {
			allBlank = false
		}
	}
	return allBlank
}

func recoversSilently(fl *ast.FuncLit) bool {
	found := false
	ast.Inspect(fl.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && calleeName(call.Fun) == "recover" {
			found = true
		}
		return true
	})
	if !found {
		return false
	}
	// A recover whose block does nothing else than call recover() is silent.
	stmts := 0
	ast.Inspect(fl.Body, func(n ast.Node) bool {
		if _, ok := n.(ast.Stmt); ok {
			stmts++
		}
		return true
	})
	return stmts <= 3
}

func looksLikeRetry(f *ast.ForStmt) bool {
	src := strings.ToLower(exprString(f))
	return strings.Contains(src, "retry") || strings.Contains(src, "attempt")
}

func exprString(n ast.Node) string {
	var sb strings.Builder
	ast.Inspect(n, func(x ast.Node) bool {
		if id, ok := x.(*ast.Ident); ok {
			sb.WriteString(id.Name)
			sb.WriteByte(' ')
		}
		return true
	})
	return sb.String()
}

// unboundedCall flags network, subprocess and database calls that carry no
// context and therefore no deadline.
func unboundedCall(full string) (string, string) {
	switch full {
	case "http.Get", "http.Post", "http.PostForm", "http.Head":
		return "network_without_timeout", full + " uses http.DefaultClient — no timeout, no context, no cancellation"
	case "net.Dial", "net.DialTimeout":
		return "network_without_timeout", full + " — prefer a DialContext so the caller can cancel"
	case "exec.Command":
		return "subprocess_without_context", "exec.Command has no context — the child outlives a cancelled caller"
	case "ioutil.ReadAll", "io.ReadAll":
		return "unbounded_read", "io.ReadAll on an unbounded source reads the whole body into memory"
	}
	if strings.HasSuffix(full, ".Query") || strings.HasSuffix(full, ".Exec") {
		if !strings.Contains(full, "Context") {
			return "db_without_context", full + " — the Context variant is what makes a slow query cancellable"
		}
	}
	return "", ""
}

func widenedParams(fn *ast.FuncDecl) []string {
	var out []string
	if fn.Type.Params == nil {
		return nil
	}
	for _, f := range fn.Type.Params.List {
		if isAnyType(f.Type) {
			for _, n := range f.Names {
				out = append(out, n.Name)
			}
			if len(f.Names) == 0 {
				out = append(out, "_")
			}
		}
	}
	return out
}

func isAnyType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == "any"
	case *ast.InterfaceType:
		return t.Methods == nil || len(t.Methods.List) == 0
	}
	return false
}

func containsIdent(n ast.Node, name string) bool {
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		if id, ok := x.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

func containsSelector(n ast.Node, pkg, sel string) bool {
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		if s, ok := x.(*ast.SelectorExpr); ok && s.Sel.Name == sel {
			if id, ok := s.X.(*ast.Ident); ok && id.Name == pkg {
				found = true
			}
		}
		return !found
	})
	return found
}

// CallEdges resolves intra-file calls exactly; cross-file callees are emitted
// with an unqualified target for the extractor to resolve repo-wide.
func (a *Adapter) CallEdges(path string, src []byte) ([]bundle.Edge, error) {
	p, err := parse(path, src)
	if err != nil {
		return nil, err
	}
	rel := filepath.ToSlash(path)
	// Callees are written as they appear at the call site ("c.refresh"), while
	// declarations are keyed by receiver type ("Cache.refresh"). Bind them on the
	// bare method name, and only when it is unambiguous within the file.
	local := map[string]bool{}
	byLastSegment := map[string][]string{}
	for _, d := range p.file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok {
			qual := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				qual = receiverName(p, fn.Recv.List[0].Type) + "." + fn.Name.Name
			}
			local[qual] = true
			byLastSegment[fn.Name.Name] = append(byLastSegment[fn.Name.Name], qual)
		}
	}

	var out []bundle.Edge
	seen := map[string]bool{}
	for _, d := range p.file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		from := fn.Name.Name
		if fn.Recv != nil && len(fn.Recv.List) > 0 {
			from = receiverName(p, fn.Recv.List[0].Type) + "." + fn.Name.Name
		}
		fromID := bundle.MakeID(rel, from)
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := calleeName(call.Fun)
			if name == "" || isBuiltin(name) {
				return true
			}
			var to bundle.SymbolID
			cands := byLastSegment[lastSegment(name)]
			switch {
			case local[name]:
				to = bundle.MakeID(rel, name)
			case len(cands) == 1:
				to = bundle.MakeID(rel, cands[0])
			default:
				// Unresolved: keep the call as written, receiver and all, so the
				// extractor can tell `helper()` from `c.entries.get()`.
				to = bundle.SymbolID("::" + name)
			}
			key := string(fromID) + ">" + string(to)
			if seen[key] {
				return true
			}
			seen[key] = true
			out = append(out, bundle.Edge{From: fromID, To: to, Kind: "call"})
			return true
		})
	}
	return out, nil
}

func isBuiltin(name string) bool {
	switch name {
	case "len", "cap", "make", "new", "append", "copy", "delete", "panic", "recover", "print", "println", "close", "complex", "real", "imag", "min", "max", "clear":
		return true
	}
	return false
}
