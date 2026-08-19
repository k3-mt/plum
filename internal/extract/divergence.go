package extract

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kelalaike/plum/internal/bundle"
)

// baseline is what the repository actually did before this session, sampled from
// StartSHA. Config-declared conventions and this empirical baseline are both
// consulted, and every finding records which one produced it — that is how you
// find out which source generates fewer false positives (spec §14.6).
type baseline struct {
	sampled       int
	wrappedErrors int
	bareErrors    int
	panics        int
	exported      int
	exportedDoc   int
	constructors  int
	globalVars    int
}

func (b baseline) wrapRate() float64 {
	total := b.wrappedErrors + b.bareErrors + b.panics
	if total == 0 {
		return 0
	}
	return float64(b.wrappedErrors) / float64(total)
}

func (b baseline) docRate() float64 {
	if b.exported == 0 {
		return 0
	}
	return float64(b.exportedDoc) / float64(b.exported)
}

const baselineSampleLimit = 120

func (e *Extractor) divergence(ctx context.Context, b *bundle.Bundle, states map[string]*fileState) {
	base := e.sampleBaseline(ctx, b.Session.StartSHA, states)

	var findings []bundle.DivergenceFinding
	newSyms := 0
	flagged := map[bundle.SymbolID]string{}

	note := func(f bundle.DivergenceFinding) {
		findings = append(findings, f)
		if worse(f.Severity, flagged[f.Symbol]) {
			flagged[f.Symbol] = f.Severity
		}
	}

	for _, s := range b.Symbols {
		if s.Change == "deleted" || isTestFile(s.File) {
			continue
		}
		newSyms++
		fs := states[s.File]
		if fs == nil || len(fs.after) == 0 {
			continue
		}

		// Documentation: empirical only. If the repo does not document its
		// exports either, silence is the convention and there is no finding.
		if s.Exported && s.Doc == "" && base.sampled > 0 && base.docRate() >= 0.6 && s.Kind != "var" && s.Kind != "const" {
			note(bundle.DivergenceFinding{
				Convention: "exported_symbols_documented",
				Expected:   "doc comment (" + pct(base.docRate()) + " of existing exports have one)",
				Observed:   "no doc comment",
				Symbol:     s.ID, Severity: "warn", Source: "empirical",
			})
		}

		if fs.language != "go" {
			continue
		}
		st := goStyle(s, fs.after)

		if st.panicsOnError {
			sev, src := "warn", "config"
			if base.sampled > 0 && base.wrapRate() >= 0.6 {
				sev, src = "high", "empirical"
			}
			if e.Cfg.Conventions.ErrorHandling == "panic" {
				sev = "info"
			}
			note(bundle.DivergenceFinding{
				Convention: "error_handling",
				Expected:   e.Cfg.Conventions.ErrorHandling + baselineNote(base),
				Observed:   "panic on the error path",
				Symbol:     s.ID, Severity: sev, Source: src,
			})
		}
		if st.bareErrorReturn && e.Cfg.Conventions.ErrorHandling == "wrapped_error" && base.sampled > 0 && base.wrapRate() >= 0.6 {
			note(bundle.DivergenceFinding{
				Convention: "error_handling",
				Expected:   "wrapped error (fmt.Errorf with %w)",
				Observed:   "bare return err — the call chain is lost",
				Symbol:     s.ID, Severity: "warn", Source: "empirical",
			})
		}
		if st.nakedReturn && e.Cfg.Forbids("naked_return") {
			note(bundle.DivergenceFinding{
				Convention: "naked_return",
				Expected:   "explicit return values",
				Observed:   "naked return with named results",
				Symbol:     s.ID, Severity: "info", Source: "config",
			})
		}
		// Only a var the risk pass judged to be genuinely writable counts here:
		// a compiled regex or a lookup table is not the convention violation the
		// config is talking about.
		if s.Kind == "var" && e.Cfg.Forbids("package_level_state") && hasMarker(b, s.ID, "package_level_state") {
			sev := "high"
			if base.globalVars > 3 {
				sev = "warn" // the repo already does this; it is a habit, not a novelty
			}
			note(bundle.DivergenceFinding{
				Convention: "package_level_state",
				Expected:   "state held on a struct and injected",
				Observed:   "new package-level var " + s.Name,
				Symbol:     s.ID, Severity: sev, Source: "config",
			})
		}
		if s.Name == "init" && e.Cfg.Forbids("init_side_effects") {
			note(bundle.DivergenceFinding{
				Convention: "init_side_effects",
				Expected:   "explicit initialisation from main or a constructor",
				Observed:   "init() function",
				Symbol:     s.ID, Severity: "high", Source: "config",
			})
		}
		if st.newsUpDeps && e.Cfg.Conventions.DIStyle == "constructor" && base.constructors >= 3 {
			note(bundle.DivergenceFinding{
				Convention: "di_style",
				Expected:   "dependencies passed to a constructor (" + itoa(base.constructors) + " New* constructors in the sampled baseline)",
				Observed:   "dependency constructed inline inside the function",
				Symbol:     s.ID, Severity: "info", Source: "config",
			})
		}
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Symbol != findings[j].Symbol {
			return findings[i].Symbol < findings[j].Symbol
		}
		return findings[i].Convention < findings[j].Convention
	})
	b.Divergence.Findings = findings

	if newSyms > 0 {
		var weighted float64
		for _, sev := range flagged {
			w, ok := e.Cfg.Conventions.Weights[sev]
			if !ok {
				w = 0.5
			}
			weighted += w
		}
		b.Divergence.Score = round2(weighted / float64(newSyms))
	}
}

// sampleBaseline reads up to baselineSampleLimit Go files that sit alongside the
// changed files at StartSHA. Nearby code is the convention that matters; the far
// side of a monorepo is not.
func (e *Extractor) sampleBaseline(ctx context.Context, rev string, states map[string]*fileState) baseline {
	var base baseline
	files, err := e.Repo.ListFiles(ctx, rev)
	if err != nil {
		return base
	}
	dirs := map[string]bool{}
	for path := range states {
		dirs[filepath.Dir(path)] = true
	}
	var candidates []string
	for _, f := range files {
		if filepath.Ext(f) != ".go" || isTestFile(f) {
			continue
		}
		if _, changed := states[f]; changed {
			continue
		}
		if dirs[filepath.Dir(f)] {
			candidates = append([]string{f}, candidates...) // same directory first
		} else {
			candidates = append(candidates, f)
		}
	}
	if len(candidates) > baselineSampleLimit {
		candidates = candidates[:baselineSampleLimit]
	}
	for _, path := range candidates {
		src, _ := e.Repo.Show(ctx, rev, path)
		if src == "" {
			continue
		}
		base.sampled++
		accumulate(&base, path, []byte(src))
	}
	return base
}

func accumulate(base *baseline, path string, src []byte) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return
	}
	for _, d := range f.Decls {
		switch decl := d.(type) {
		case *ast.GenDecl:
			if decl.Tok == token.VAR {
				base.globalVars++
			}
			for _, spec := range decl.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ast.IsExported(ts.Name.Name) {
					base.exported++
					if decl.Doc != nil || ts.Doc != nil {
						base.exportedDoc++
					}
				}
			}
		case *ast.FuncDecl:
			if ast.IsExported(decl.Name.Name) {
				base.exported++
				if decl.Doc != nil {
					base.exportedDoc++
				}
			}
			if strings.HasPrefix(decl.Name.Name, "New") {
				base.constructors++
			}
			if decl.Body == nil {
				continue
			}
			ast.Inspect(decl.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CallExpr:
					if name := calleeString(node.Fun); name == "fmt.Errorf" || name == "errors.Wrap" {
						base.wrappedErrors++
					} else if name == "panic" {
						base.panics++
					}
				case *ast.ReturnStmt:
					for _, r := range node.Results {
						if id, ok := r.(*ast.Ident); ok && id.Name == "err" {
							base.bareErrors++
						}
					}
				}
				return true
			})
		}
	}
}

type style struct {
	panicsOnError   bool
	bareErrorReturn bool
	nakedReturn     bool
	newsUpDeps      bool
}

// goStyle re-parses the file and inspects only the declaration in question, so
// the finding is about the changed symbol and not its neighbours.
func goStyle(s bundle.Symbol, src []byte) style {
	var st style
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, s.File, src, parser.SkipObjectResolution)
	if err != nil {
		return st
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if fset.Position(fn.Pos()).Line != s.LineStart && !within(fset, fn, s) {
			continue
		}
		named := hasNamedResults(fn)
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				name := calleeString(node.Fun)
				if name == "panic" {
					st.panicsOnError = true
				}
				if strings.HasPrefix(name, "New") && len(name) > 3 {
					st.newsUpDeps = true
				}
			case *ast.ReturnStmt:
				if len(node.Results) == 0 && named {
					st.nakedReturn = true
				}
				for _, r := range node.Results {
					if id, ok := r.(*ast.Ident); ok && id.Name == "err" {
						st.bareErrorReturn = true
					}
				}
			}
			return true
		})
	}
	return st
}

func within(fset *token.FileSet, fn *ast.FuncDecl, s bundle.Symbol) bool {
	start := fset.Position(fn.Pos()).Line
	end := fset.Position(fn.End()).Line
	return start >= s.LineStart && end <= s.LineEnd
}

func hasNamedResults(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil {
		return false
	}
	for _, r := range fn.Type.Results.List {
		if len(r.Names) > 0 {
			return true
		}
	}
	return false
}

func calleeString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		if x, ok := t.X.(*ast.Ident); ok {
			return x.Name + "." + t.Sel.Name
		}
		return t.Sel.Name
	}
	return ""
}

func hasMarker(b *bundle.Bundle, id bundle.SymbolID, kind string) bool {
	for _, r := range b.RiskMarkers {
		if r.Symbol == id && r.Kind == kind {
			return true
		}
	}
	return false
}

func worse(a, b string) bool {
	rank := map[string]int{"": 0, "info": 1, "warn": 2, "high": 3}
	return rank[a] > rank[b]
}

// baselineNote only quotes an empirical rate when there was something to sample.
// "0% of existing error paths wrap" from an empty sample is a lie, not a finding.
func baselineNote(b baseline) string {
	if b.sampled == 0 {
		return " (declared in config; no comparable code was sampled at StartSHA)"
	}
	return " (" + pct(b.wrapRate()) + " of " + itoa(b.sampled) + " sampled files' error paths wrap)"
}

func pct(f float64) string {
	return itoa(int(f*100+0.5)) + "%"
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
