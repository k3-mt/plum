// Package js is the JavaScript and TypeScript adapter.
//
// Unlike Go and Python, there is no parser to borrow: Node exposes no AST to
// userland, and pulling in a JavaScript grammar would mean either cgo or a
// vendored parser. So this is a structural scanner — it tracks brace depth,
// class bodies and comment state to find declarations, rather than matching
// lines in isolation. That is enough to name every function and method
// exactly, which is what the instrumentation set and the SymbolIDs need.
//
// It is honest about its limits: it does not resolve types, and it does not
// follow dynamic construction. Where it cannot tell, it says nothing rather
// than guessing.
package js

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/trace"
)

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return "typescript" }

func (a *Adapter) Extensions() []string {
	return []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"}
}

// line is one source line with the lexical state the scanner derived for it.
type line struct {
	raw     string
	code    string // comments and string contents blanked, so braces can be counted
	num     int    // 1-based
	depth   int    // brace depth at the start of the line
	comment string // an own-line comment, "" otherwise
	blank   bool
}

// scan blanks out comments and string literals, then records brace depth. Doing
// this once means every later question — where does this class end, is this a
// declaration or a call — is asked against code rather than against text.
func scan(src []byte) []line {
	var out []line
	depth := 0
	inBlock := false

	for i, raw := range strings.Split(string(src), "\n") {
		var code strings.Builder
		var commentText strings.Builder
		inStr := byte(0)
		inTemplate := false
		lineComment := false
		codeSeen := false

		for j := 0; j < len(raw); j++ {
			c := raw[j]
			switch {
			case inBlock:
				if c == '*' && j+1 < len(raw) && raw[j+1] == '/' {
					inBlock = false
					j++
				} else {
					commentText.WriteByte(c)
				}
				code.WriteByte(' ')
			case lineComment:
				commentText.WriteByte(c)
				code.WriteByte(' ')
			case inStr != 0:
				if c == '\\' {
					j++
					code.WriteString("  ")
					continue
				}
				if c == inStr {
					inStr = 0
				}
				code.WriteByte(' ')
			case inTemplate:
				if c == '`' {
					inTemplate = false
				}
				code.WriteByte(' ')
			case c == '/' && j+1 < len(raw) && raw[j+1] == '/':
				lineComment = true
				j++
			case c == '/' && j+1 < len(raw) && raw[j+1] == '*':
				inBlock = true
				j++
				code.WriteString("  ")
			case c == '"' || c == '\'':
				inStr = c
				code.WriteByte(c)
			case c == '`':
				inTemplate = true
				code.WriteByte(c)
			default:
				if c != ' ' && c != '\t' {
					codeSeen = true
				}
				code.WriteByte(c)
			}
		}

		l := line{raw: raw, code: code.String(), num: i + 1, depth: depth}
		l.blank = strings.TrimSpace(raw) == ""
		if text := strings.TrimSpace(commentText.String()); text != "" && !codeSeen {
			l.comment = strings.TrimLeft(text, "*/ ")
		}
		for _, c := range l.code {
			switch c {
			case '{':
				depth++
			case '}':
				depth--
			}
		}
		out = append(out, l)
	}
	return out
}

var (
	classRe   = regexp.MustCompile(`(?:^|\s)(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`)
	funcRe    = regexp.MustCompile(`(?:^|\s)(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)\s*[(<]`)
	arrowRe   = regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*(?:async\s*)?(?:function\b|\(|<|[A-Za-z_$][\w$]*\s*=>)`)
	varRe     = regexp.MustCompile(`^\s*(?:export\s+)?(const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+)?=`)
	methodRe  = regexp.MustCompile(`^\s*(?:public\s+|private\s+|protected\s+|readonly\s+)*(?:static\s+)?(?:async\s+)?(?:\*\s*)?(?:(get|set)\s+)?([A-Za-z_$#][\w$]*)\s*(?:<[^>]*>)?\s*\(`)
	exportsRe = regexp.MustCompile(`module\.exports\s*=\s*\{([^}]*)\}`)
	envRe     = regexp.MustCompile(`process\.env\.([A-Z][A-Z0-9_]*)|process\.env\[['"]([A-Z][A-Z0-9_]*)['"]\]`)
	routeRe   = regexp.MustCompile(`(?:app|router|server)\.(get|post|put|patch|delete|use)\(\s*['"` + "`" + `]([^'"` + "`" + `]+)`)
	callRe    = regexp.MustCompile(`([A-Za-z_$][\w$.]*)\s*\(`)
)

// notCallable are the keywords that look exactly like a method declaration.
var notCallable = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"return": true, "function": true, "typeof": true, "await": true, "yield": true,
	"do": true, "else": true, "new": true, "delete": true, "void": true, "in": true, "of": true,
}

type decl struct {
	name      string // qualified: Class.method
	kind      string // func | method | class | var | const
	start     int
	end       int
	signature string
	doc       string
	exported  bool
}

func (a *Adapter) declarations(src []byte) []decl {
	lines := scan(src)
	var out []decl

	// classes tracks which class body we are inside, so a method gets the
	// Class.method SymbolID the shim will report at runtime.
	var classes []classFrame

	for i, l := range lines {
		for len(classes) > 0 && l.depth <= classes[len(classes)-1].depth {
			classes = classes[:len(classes)-1]
		}
		if l.blank || strings.TrimSpace(l.code) == "" {
			continue
		}
		code := l.code
		exported := strings.Contains(code, "export ")

		if m := classRe.FindStringSubmatch(code); m != nil {
			d := decl{name: m[1], kind: "class", start: l.num, end: blockEnd(lines, i),
				signature: strings.TrimSpace(l.raw), doc: docAbove(lines, i), exported: exported}
			out = append(out, d)
			classes = append(classes, classFrame{name: m[1], depth: l.depth})
			continue
		}
		// Only module scope and class bodies declare symbols this tool can name.
		// A function or a const inside another function body is a local: giving
		// it a SymbolID would be wrong, and — because hunk mapping credits the
		// innermost declaration — it would displace the function that contains it.
		atTop := l.depth == 0
		inClassBody := len(classes) > 0 && l.depth == classes[len(classes)-1].depth+1

		if m := funcRe.FindStringSubmatch(code); m != nil && (atTop || inClassBody) {
			out = append(out, decl{name: qualify(classes, m[1]), kind: kindFor(classes), start: l.num,
				end: blockEnd(lines, i), signature: strings.TrimSpace(l.raw),
				doc: docAbove(lines, i), exported: exported})
			continue
		}
		if m := arrowRe.FindStringSubmatch(code); m != nil && (atTop || inClassBody) {
			out = append(out, decl{name: qualify(classes, m[1]), kind: kindFor(classes), start: l.num,
				end: blockEnd(lines, i), signature: strings.TrimSpace(l.raw),
				doc: docAbove(lines, i), exported: exported})
			continue
		}
		// A method is only a method inside a class body, one level in.
		if inClassBody {
			if m := methodRe.FindStringSubmatch(code); m != nil && !notCallable[m[2]] {
				name := classes[len(classes)-1].name + "." + m[2]
				out = append(out, decl{name: name, kind: "method", start: l.num,
					end: blockEnd(lines, i), signature: strings.TrimSpace(l.raw),
					doc: docAbove(lines, i), exported: !strings.HasPrefix(m[2], "_") && !strings.HasPrefix(m[2], "#")})
				continue
			}
		}
		if atTop && len(classes) == 0 {
			if m := varRe.FindStringSubmatch(code); m != nil {
				kind := "var"
				if m[1] == "const" {
					kind = "const"
				}
				out = append(out, decl{name: m[2], kind: kind, start: l.num, end: l.num,
					signature: strings.TrimSpace(l.raw), doc: docAbove(lines, i), exported: exported})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// classFrame is one open class body: its name, and the brace depth it started at.
type classFrame struct {
	name  string
	depth int
}

func kindFor(classes []classFrame) string {
	if len(classes) > 0 {
		return "method"
	}
	return "func"
}

func qualify(classes []classFrame, name string) string {
	if len(classes) > 0 {
		return classes[len(classes)-1].name + "." + name
	}
	return name
}

// blockEnd finds the line the declaration's brace block closes on. A
// declaration with no block (an interface member, a one-line arrow) ends on its
// own line.
//
// depth is recorded at the *start* of a line, so the closing brace's own line
// still reads as inside the block. The first line back at the outer depth is
// therefore the one after the block, and the block ends on the line before it.
func blockEnd(lines []line, i int) int {
	start := lines[i]
	if !strings.Contains(start.code, "{") {
		return start.num
	}
	target := start.depth
	for j := i + 1; j < len(lines); j++ {
		if lines[j].depth <= target {
			return lines[j-1].num
		}
	}
	return lines[len(lines)-1].num
}

// docAbove returns the contiguous comment block immediately above a line — the
// JSDoc or the // paragraph that explains it.
func docAbove(lines []line, i int) string {
	var block []string
	for j := i - 1; j >= 0; j-- {
		if lines[j].comment == "" {
			break
		}
		block = append([]string{lines[j].comment}, block...)
	}
	return strings.TrimSpace(strings.Join(block, "\n"))
}

// ---------------------------------------------------------------- Adapter

func (a *Adapter) ParseSymbols(path string, src []byte) ([]bundle.Symbol, error) {
	rel := filepath.ToSlash(path)
	lines := scan(src)
	offsets := lineOffsets(src)
	decls := a.declarations(src)

	local := map[string]string{}
	for _, d := range decls {
		local[lastSegment(d.name)] = d.name
	}

	var out []bundle.Symbol
	for _, d := range decls {
		s := bundle.Symbol{
			ID:        bundle.MakeID(rel, d.name),
			Kind:      d.kind,
			Name:      d.name,
			File:      rel,
			LineStart: d.start,
			LineEnd:   d.end,
			ByteStart: offsetOf(offsets, d.start-1),
			ByteEnd:   offsetOf(offsets, d.end),
			Signature: d.signature,
			Doc:       d.doc,
			Exported:  d.exported,
		}
		s.Fingerprint = fingerprint(lines, d)
		for _, l := range lines {
			if l.num >= d.start && l.num <= d.end && l.comment != "" {
				s.Comments = append(s.Comments, bundle.Comment{Text: l.comment, LineStart: l.num, LineEnd: l.num})
			}
		}
		s.CallSites = callSites(rel, lines, d, local)
		out = append(out, s)
	}
	return out, nil
}

// callSites binds each outbound call to the comment block directly above it —
// the string that says why the call is being made here (spec §9.4).
//
// A callee only resolves to a local symbol when the shape of the call supports
// it: a bare `f()`, or `this.m()` inside the class that declares m. Anything
// deeper — `this.entries.get(key)` — is a call on some other object, and
// binding it by bare name would invent an edge that does not exist.
func callSites(rel string, lines []line, d decl, local map[string]string) []bundle.CallSite {
	enclosing := ""
	if i := strings.Index(d.name, "."); i >= 0 {
		enclosing = d.name[:i]
	}
	var out []bundle.CallSite
	seen := map[string]bool{}
	for i, l := range lines {
		if l.num <= d.start || l.num > d.end {
			continue
		}
		for _, m := range callRe.FindAllStringSubmatch(l.code, -1) {
			name := m[1]
			short := lastSegment(name)
			if notCallable[short] || short == d.name || seen[name] {
				continue
			}
			seen[name] = true
			callee := bundle.SymbolID("::" + name)
			if qual, ok := resolveCallee(name, enclosing, local); ok {
				callee = bundle.MakeID(rel, qual)
			}
			out = append(out, bundle.CallSite{
				Callee: callee, CalleeRaw: name, Line: l.num,
				Rationale: docAbove(lines, i),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

// resolveCallee decides whether a call names something declared in this file.
func resolveCallee(name, enclosingClass string, local map[string]string) (string, bool) {
	switch {
	case !strings.Contains(name, "."):
		// A bare call: it can only mean a declaration in scope.
		if qual, ok := local[name]; ok && !strings.Contains(qual, ".") {
			return qual, true
		}
	case strings.HasPrefix(name, "this.") && strings.Count(name, ".") == 1 && enclosingClass != "":
		// this.m() inside a class body means that class's own method.
		method := strings.TrimPrefix(name, "this.")
		if qual, ok := local[method]; ok && qual == enclosingClass+"."+method {
			return qual, true
		}
	}
	return "", false
}

func (a *Adapter) PublicSurface(path string, src []byte) ([]bundle.SurfaceItem, error) {
	rel := filepath.ToSlash(path)
	module := moduleName(rel)
	decls := a.declarations(src)

	// module.exports = { A, B } is how CommonJS names its surface; ES exports
	// are already marked on the declaration.
	commonJS := map[string]bool{}
	if m := exportsRe.FindStringSubmatch(string(src)); m != nil {
		for _, part := range strings.Split(m[1], ",") {
			name := strings.TrimSpace(strings.SplitN(part, ":", 2)[0])
			if name != "" {
				commonJS[name] = true
			}
		}
	}

	exportedClass := map[string]bool{}
	for _, d := range decls {
		if d.kind == "class" && (d.exported || commonJS[d.name]) {
			exportedClass[d.name] = true
		}
	}

	var out []bundle.SurfaceItem
	for _, d := range decls {
		top := lastSegment(d.name) == d.name
		owner := strings.SplitN(d.name, ".", 2)[0]
		switch {
		case top && (d.exported || commonJS[d.name]):
		case !top && exportedClass[owner] && d.exported:
		default:
			continue
		}
		out = append(out, bundle.SurfaceItem{
			Kind: "export", Name: module + "." + d.name, File: rel,
			Signature: d.signature, Symbol: bundle.MakeID(rel, d.name),
		})
	}

	text := string(src)
	for _, m := range envRe.FindAllStringSubmatch(text, -1) {
		name := m[1]
		if name == "" {
			name = m[2]
		}
		out = append(out, bundle.SurfaceItem{Kind: "env_var", Name: name, File: rel,
			Symbol: bundle.MakeID(rel, ownerAt(decls, lineOf(text, m[0])))})
	}
	for _, m := range routeRe.FindAllStringSubmatch(text, -1) {
		out = append(out, bundle.SurfaceItem{Kind: "route", Name: strings.ToUpper(m[1]) + " " + m[2], File: rel})
	}
	return out, nil
}

func (a *Adapter) RiskMarkers(path string, src []byte, syms []bundle.Symbol) ([]bundle.RiskMarker, error) {
	rel := filepath.ToSlash(path)
	lines := scan(src)
	decls := a.declarations(src)
	changed := map[bundle.SymbolID]bool{}
	for _, s := range syms {
		changed[s.ID] = true
	}

	var out []bundle.RiskMarker
	mark := func(kind string, l line, note string) {
		id := bundle.MakeID(rel, ownerAt(decls, l.num))
		if len(changed) > 0 && !changed[id] {
			return
		}
		out = append(out, bundle.RiskMarker{Kind: kind, Symbol: id, File: rel, Line: l.num, Note: note})
	}

	for i, l := range lines {
		code := l.code
		switch {
		case emptyCatch(lines, i):
			mark("swallowed_error", l, "catch block with no body — the error is observed and discarded")
		case strings.Contains(code, "fetch(") && !strings.Contains(code, "signal"):
			if !blockMentions(lines, i, 4, "signal", "AbortController") {
				mark("network_without_timeout", l, "fetch with no AbortSignal — the request cannot be cancelled or timed out")
			}
		case strings.Contains(code, "JSON.parse(") && !blockMentions(lines, i, 3, "try", "catch"):
			mark("unguarded_parse", l, "JSON.parse throws on malformed input, and nothing here catches it")
		case strings.Contains(code, "== ") && !strings.Contains(code, "=== ") && !strings.Contains(code, "!== "):
			mark("loose_equality", l, "== coerces types before comparing; === is what almost every case wants")
		}
		if l.depth == 0 {
			if m := varRe.FindStringSubmatch(code); m != nil && m[1] != "const" {
				mark("module_level_state", l, "module-level "+m[1]+" "+m[2]+" — shared by every importer in the process")
			}
		}
	}
	return out, nil
}

// emptyCatch spots `catch (e) {}` however it is spaced or wrapped.
func emptyCatch(lines []line, i int) bool {
	idx := strings.Index(lines[i].code, "catch")
	if idx < 0 {
		return false
	}
	rest := lines[i].code[idx:]
	brace := strings.Index(rest, "{")
	if brace < 0 {
		return false
	}
	tail := strings.TrimSpace(rest[brace+1:])
	if tail == "}" {
		return true
	}
	if tail != "" {
		return false
	}
	for j := i + 1; j < len(lines); j++ {
		body := strings.TrimSpace(lines[j].code)
		if body == "" {
			continue
		}
		return body == "}"
	}
	return false
}

func blockMentions(lines []line, i, radius int, needles ...string) bool {
	for j := i - radius; j <= i+radius; j++ {
		if j < 0 || j >= len(lines) {
			continue
		}
		for _, needle := range needles {
			if strings.Contains(lines[j].code, needle) {
				return true
			}
		}
	}
	return false
}

func (a *Adapter) CallEdges(path string, src []byte) ([]bundle.Edge, error) {
	rel := filepath.ToSlash(path)
	syms, err := a.ParseSymbols(path, src)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []bundle.Edge
	for _, s := range syms {
		for _, cs := range s.CallSites {
			if s.ID == cs.Callee {
				continue
			}
			key := string(s.ID) + ">" + string(cs.Callee)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, bundle.Edge{From: s.ID, To: cs.Callee, Kind: "call"})
		}
	}
	_ = rel
	return out, nil
}

func (a *Adapter) Comments(path string, src []byte) ([]bundle.Comment, error) {
	var out []bundle.Comment
	for _, l := range scan(src) {
		if l.comment != "" {
			out = append(out, bundle.Comment{Text: l.comment, LineStart: l.num, LineEnd: l.num})
		}
	}
	return out, nil
}

// Normalise drops comments and collapses whitespace, preserving identifiers:
// reformatting must not move a fingerprint, a rename or a logic change must.
func (a *Adapter) Normalise(src []byte) ([]byte, error) {
	var kept []string
	for _, l := range scan(src) {
		if f := strings.Join(strings.Fields(l.code), " "); f != "" {
			kept = append(kept, f)
		}
	}
	return []byte(strings.Join(kept, "\n")), nil
}

// ShimSpec preloads the shim into every node process the test command spawns,
// including workers a test runner forks. The preload is CommonJS because
// --require is; it registers the ESM loader for itself.
func (a *Adapter) ShimSpec(syms []bundle.SymbolID) (trace.ShimSpec, error) {
	return trace.ShimSpec{
		Language: "node",
		Mode:     "env",
		Symbols:  syms,
		Dir:      ".plum-shim-node",
		Files: map[string]string{
			"plum-shim.cjs":   trace.NodeShimSource,
			"plum-loader.mjs": trace.NodeLoaderSource,
		},
		Env: map[string]string{
			"PLUM_SYMBOLS": "${SYMBOLS}",
			"NODE_OPTIONS": "--require ${SHIM_DIR}/plum-shim.cjs",
		},
	}, nil
}

// ---------------------------------------------------------------- helpers

func fingerprint(lines []line, d decl) string {
	var kept []string
	for _, l := range lines {
		if l.num < d.start || l.num > d.end {
			continue
		}
		if f := strings.Join(strings.Fields(l.code), " "); f != "" {
			kept = append(kept, f)
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(kept, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ownerAt names the innermost declaration containing a line.
func ownerAt(decls []decl, num int) string {
	best, span := "", 1<<30
	for _, d := range decls {
		if num >= d.start && num <= d.end && d.end-d.start < span {
			best, span = d.name, d.end-d.start
		}
	}
	return best
}

func lineOf(text, needle string) int {
	i := strings.Index(text, needle)
	if i < 0 {
		return 0
	}
	return strings.Count(text[:i], "\n") + 1
}

func moduleName(rel string) string {
	rel = strings.TrimSuffix(rel, filepath.Ext(rel))
	rel = strings.TrimSuffix(rel, "/index")
	return strings.ReplaceAll(rel, "/", ".")
}

func lastSegment(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

func lineOffsets(src []byte) []int {
	offs := []int{0}
	for i, b := range src {
		if b == '\n' {
			offs = append(offs, i+1)
		}
	}
	return append(offs, len(src))
}

func offsetOf(offs []int, line int) int {
	if line < 0 {
		return 0
	}
	if line >= len(offs) {
		return offs[len(offs)-1]
	}
	return offs[line]
}
