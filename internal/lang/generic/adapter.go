// Package generic is the honest fallback adapter for languages with no native
// parser wired in yet (Python, TypeScript, JavaScript). It is line-based, not
// AST-based: it finds declarations, doc blocks and call sites well enough to be
// useful, and it never claims resolution it cannot do. Replacing it with
// tree-sitter-under-wazero is a drop-in swap behind lang.Adapter (spec §3.3).
package generic

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/trace"
)

type Adapter struct {
	name string
	exts []string
	decl *regexp.Regexp // capture 1 = kind, capture 2 = name
	cmt  string
}

func Python() *Adapter {
	return &Adapter{
		name: "python",
		exts: []string{".py"},
		decl: regexp.MustCompile(`^(\s*)(def|class|async def)\s+([A-Za-z_][A-Za-z0-9_]*)`),
		cmt:  "#",
	}
}

func TypeScript() *Adapter {
	return &Adapter{
		name: "typescript",
		exts: []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"},
		decl: regexp.MustCompile(`^(\s*)(?:export\s+)?(?:default\s+)?(function|class|const|let|var)\s+([A-Za-z_$][A-Za-z0-9_$]*)`),
		cmt:  "//",
	}
}

func (a *Adapter) Name() string         { return a.name }
func (a *Adapter) Extensions() []string { return a.exts }

type decl struct {
	kind, name string
	indent     int
	start, end int // 1-based inclusive line numbers
}

func (a *Adapter) declarations(src []byte) []decl {
	lines := strings.Split(string(src), "\n")
	var ds []decl
	for i, l := range lines {
		m := a.decl.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		ds = append(ds, decl{kind: normKind(m[2]), name: m[3], indent: len(m[1]), start: i + 1})
	}
	// A declaration runs until the next declaration at the same or shallower
	// indent, or end of file. Crude, but stable and never overlapping wrongly.
	for i := range ds {
		ds[i].end = len(lines)
		for j := i + 1; j < len(ds); j++ {
			if ds[j].indent <= ds[i].indent {
				ds[i].end = ds[j].start - 1
				break
			}
		}
	}
	return ds
}

func normKind(k string) string {
	switch k {
	case "def", "async def", "function":
		return "func"
	case "class":
		return "class"
	}
	return "var"
}

func (a *Adapter) ParseSymbols(path string, src []byte) ([]bundle.Symbol, error) {
	rel := filepath.ToSlash(path)
	lines := strings.Split(string(src), "\n")
	offsets := lineOffsets(src)
	ds := a.declarations(src)

	// Qualify a method with its enclosing class, matching the SymbolID shape the
	// Go adapter produces: "Class.method".
	qual := func(i int) string {
		d := ds[i]
		if d.indent == 0 {
			return d.name
		}
		for j := i - 1; j >= 0; j-- {
			if ds[j].indent < d.indent && ds[j].kind == "class" {
				return ds[j].name + "." + d.name
			}
		}
		return d.name
	}

	var out []bundle.Symbol
	for i, d := range ds {
		name := qual(i)
		s := bundle.Symbol{
			ID:        bundle.MakeID(rel, name),
			Kind:      map[bool]string{true: "method", false: d.kind}[strings.Contains(name, ".") && d.kind == "func"],
			Name:      name,
			File:      rel,
			LineStart: d.start,
			LineEnd:   d.end,
			ByteStart: offsets[d.start-1],
			ByteEnd:   offsets[min(d.end, len(offsets)-1)],
			Signature: strings.TrimSpace(lines[d.start-1]),
			Doc:       a.docFor(lines, d),
			Exported:  !strings.HasPrefix(d.name, "_"),
		}
		s.Comments = a.commentsIn(lines, d.start, d.end)
		s.CallSites = a.callSitesIn(rel, lines, d)
		norm, _ := a.Normalise(src[s.ByteStart:s.ByteEnd])
		sum := sha256.Sum256(norm)
		s.Fingerprint = "sha256:" + hex.EncodeToString(sum[:])
		out = append(out, s)
	}
	return out, nil
}

// docFor reads a Python triple-quoted docstring on the line after the def, or a
// contiguous comment block directly above the declaration.
func (a *Adapter) docFor(lines []string, d decl) string {
	if a.name == "python" && d.start < len(lines) {
		next := strings.TrimSpace(lines[d.start])
		for _, q := range []string{`"""`, "'''"} {
			if strings.HasPrefix(next, q) {
				body := strings.TrimPrefix(next, q)
				if strings.HasSuffix(body, q) && len(body) > 2 {
					return strings.TrimSpace(strings.TrimSuffix(body, q))
				}
				var sb []string
				if body != "" {
					sb = append(sb, body)
				}
				for i := d.start + 1; i < len(lines) && i < d.end; i++ {
					if strings.Contains(lines[i], q) {
						sb = append(sb, strings.TrimSpace(strings.Split(lines[i], q)[0]))
						break
					}
					sb = append(sb, strings.TrimSpace(lines[i]))
				}
				return strings.TrimSpace(strings.Join(sb, "\n"))
			}
		}
	}
	var block []string
	for i := d.start - 2; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" || !strings.HasPrefix(t, a.cmt) {
			break
		}
		block = append([]string{strings.TrimSpace(strings.TrimPrefix(t, a.cmt))}, block...)
	}
	return strings.Join(block, "\n")
}

func (a *Adapter) commentsIn(lines []string, from, to int) []bundle.Comment {
	var out []bundle.Comment
	for i := from - 1; i < to && i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, a.cmt) {
			out = append(out, bundle.Comment{
				Text:      strings.TrimSpace(strings.TrimPrefix(t, a.cmt)),
				LineStart: i + 1, LineEnd: i + 1,
			})
		}
	}
	return out
}

var callRe = regexp.MustCompile(`([A-Za-z_$][A-Za-z0-9_$.]*)\s*\(`)

func (a *Adapter) callSitesIn(rel string, lines []string, d decl) []bundle.CallSite {
	var out []bundle.CallSite
	seen := map[string]bool{}
	for i := d.start; i < d.end && i < len(lines); i++ {
		for _, m := range callRe.FindAllStringSubmatch(lines[i], -1) {
			name := m[1]
			if isKeyword(name) || seen[name] {
				continue
			}
			seen[name] = true
			cs := bundle.CallSite{
				Callee:    bundle.MakeID(rel, lastSegment(name)),
				CalleeRaw: name,
				Line:      i + 1,
			}
			if i > 0 {
				if t := strings.TrimSpace(lines[i-1]); strings.HasPrefix(t, a.cmt) {
					cs.Rationale = strings.TrimSpace(strings.TrimPrefix(t, a.cmt))
				}
			}
			out = append(out, cs)
		}
	}
	return out
}

func isKeyword(n string) bool {
	switch n {
	case "if", "for", "while", "return", "print", "len", "str", "int", "switch", "catch", "function", "super", "typeof", "require":
		return true
	}
	return false
}

func lastSegment(n string) string {
	if i := strings.LastIndex(n, "."); i >= 0 {
		return n[i+1:]
	}
	return n
}

var envRe = regexp.MustCompile(`(?:os\.environ(?:\.get)?\[?\(?|process\.env\.)["']?([A-Z][A-Z0-9_]{2,})["']?`)
var routeRe = regexp.MustCompile(`(?:@app\.route|app\.(?:get|post|put|delete)|router\.(?:get|post|put|delete))\(\s*["']([^"']+)["']`)

func (a *Adapter) PublicSurface(path string, src []byte) ([]bundle.SurfaceItem, error) {
	rel := filepath.ToSlash(path)
	var out []bundle.SurfaceItem
	syms, err := a.ParseSymbols(path, src)
	if err != nil {
		return nil, err
	}
	for _, s := range syms {
		if s.Exported && !strings.Contains(s.Name, ".") {
			out = append(out, bundle.SurfaceItem{Kind: "export", Name: s.Name, File: rel, Symbol: s.ID, Signature: s.Signature})
		}
	}
	text := string(src)
	for _, m := range envRe.FindAllStringSubmatch(text, -1) {
		out = append(out, bundle.SurfaceItem{Kind: "env_var", Name: m[1], File: rel})
	}
	for _, m := range routeRe.FindAllStringSubmatch(text, -1) {
		out = append(out, bundle.SurfaceItem{Kind: "route", Name: m[1], File: rel})
	}
	return out, nil
}

type pred struct {
	kind string
	re   *regexp.Regexp
	note string
}

var pyPreds = []pred{
	{"swallowed_error", regexp.MustCompile(`except[^:]*:\s*pass\b`), "except ...: pass — the exception is observed and discarded"},
	{"swallowed_error", regexp.MustCompile(`except\s*:`), "bare except catches SystemExit and KeyboardInterrupt too"},
	{"unsynchronised_thread", regexp.MustCompile(`threading\.Thread\(`), "bare threading.Thread — no join in sight, completion is unobservable"},
	{"network_without_timeout", regexp.MustCompile(`requests\.(get|post|put|delete)\((?:[^)]*)\)`), "requests call — check for a timeout= argument"},
	{"unbounded_read", regexp.MustCompile(`\.read\(\s*\)`), "unbounded read() pulls the whole stream into memory"},
}

var tsPreds = []pred{
	{"swallowed_error", regexp.MustCompile(`catch\s*\([^)]*\)\s*\{\s*\}`), "empty catch block — the error vanishes"},
	{"floating_promise", regexp.MustCompile(`(?m)^\s*[A-Za-z_$][\w$.]*\([^;]*\)\s*;?\s*//\s*async`), "async call with no await — the rejection is unhandled"},
	{"network_without_timeout", regexp.MustCompile(`fetch\(`), "fetch without an AbortController cannot be cancelled"},
}

func (a *Adapter) RiskMarkers(path string, src []byte, syms []bundle.Symbol) ([]bundle.RiskMarker, error) {
	rel := filepath.ToSlash(path)
	preds := tsPreds
	if a.name == "python" {
		preds = pyPreds
	}
	lines := strings.Split(string(src), "\n")
	owner := func(line int) bundle.SymbolID {
		var best bundle.SymbolID
		bestSpan := 1 << 30
		for _, s := range syms {
			if line >= s.LineStart && line <= s.LineEnd && s.LineEnd-s.LineStart < bestSpan {
				best, bestSpan = s.ID, s.LineEnd-s.LineStart
			}
		}
		return best
	}
	var out []bundle.RiskMarker
	for i, l := range lines {
		for _, p := range preds {
			if p.re.MatchString(l) {
				out = append(out, bundle.RiskMarker{Kind: p.kind, Symbol: owner(i + 1), File: rel, Line: i + 1, Note: p.note})
			}
		}
	}
	return out, nil
}

func (a *Adapter) CallEdges(path string, src []byte) ([]bundle.Edge, error) {
	rel := filepath.ToSlash(path)
	syms, err := a.ParseSymbols(path, src)
	if err != nil {
		return nil, err
	}
	localNames := map[string]bundle.SymbolID{}
	for _, s := range syms {
		localNames[lastSegment(s.Name)] = s.ID
	}
	var out []bundle.Edge
	seen := map[string]bool{}
	for _, s := range syms {
		for _, cs := range s.CallSites {
			to := bundle.SymbolID("::" + lastSegment(cs.CalleeRaw))
			if id, ok := localNames[lastSegment(cs.CalleeRaw)]; ok {
				to = id
			}
			key := string(s.ID) + ">" + string(to)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, bundle.Edge{From: s.ID, To: to, Kind: "call"})
		}
	}
	_ = rel
	return out, nil
}

func (a *Adapter) Comments(path string, src []byte) ([]bundle.Comment, error) {
	lines := strings.Split(string(src), "\n")
	return a.commentsIn(lines, 1, len(lines)), nil
}

// Normalise strips comments and collapses whitespace. Identifiers survive, so a
// rename still moves the fingerprint.
func (a *Adapter) Normalise(src []byte) ([]byte, error) {
	var kept []string
	for _, l := range strings.Split(string(src), "\n") {
		if i := strings.Index(l, a.cmt); i >= 0 {
			l = l[:i]
		}
		if f := strings.Join(strings.Fields(l), " "); f != "" {
			kept = append(kept, f)
		}
	}
	return []byte(strings.Join(kept, "\n")), nil
}

// ShimSpec attaches the shim for this adapter's language. Both are "env" mode:
// files dropped into a scratch directory plus the environment that loads them.
// The collector honours this without knowing anything about either runtime.
func (a *Adapter) ShimSpec(syms []bundle.SymbolID) (trace.ShimSpec, error) {
	switch a.name {
	case "python":
		return trace.ShimSpec{
			Language: "python",
			Mode:     "env",
			Symbols:  syms,
			Dir:      ".plum-shim-python",
			Files: map[string]string{
				"plum_shim.py":     trace.PythonShimSource,
				"sitecustomize.py": trace.PythonSiteCustomize,
			},
			Env:      map[string]string{"PLUM_SYMBOLS": "${SYMBOLS}", "PYTHONDONTWRITEBYTECODE": "1"},
			PathVars: []string{"PYTHONPATH"},
		}, nil

	case "typescript":
		// NODE_OPTIONS --require preloads the shim into every node process the
		// test command spawns, including the workers a test runner forks.
		return trace.ShimSpec{
			Language: "node",
			Mode:     "env",
			Symbols:  syms,
			Dir:      ".plum-shim-node",
			Files:    map[string]string{"plum-shim.cjs": trace.NodeShimSource},
			Env: map[string]string{
				"PLUM_SYMBOLS": "${SYMBOLS}",
				"NODE_OPTIONS": "--require ${SHIM_DIR}/plum-shim.cjs",
			},
		}, nil
	}
	return trace.ShimSpec{Mode: "none"}, fmt.Errorf("no shim for %s", a.name)
}

func lineOffsets(src []byte) []int {
	offs := []int{0}
	for i, b := range src {
		if b == '\n' {
			offs = append(offs, i+1)
		}
	}
	offs = append(offs, len(src))
	return offs
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
