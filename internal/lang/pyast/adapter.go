// Package pyast is the Python adapter. It shells out to the repository's own
// interpreter and lets Python parse Python: `ast` gives exact structure and
// `tokenize` gives back the comments `ast` discards. No grammar, no bindings,
// no cgo — CGO_ENABLED=0 still holds, because the parser is a subprocess rather
// than a linked library.
//
// When python3 is not on PATH, callers fall back to the line-based adapter in
// internal/lang/generic. Degraded, but never wrong about what it claims.
package pyast

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/trace"
)

//go:embed extract.py
var extractScript string

type Adapter struct {
	python string

	mu    sync.Mutex
	cache map[string]*result // keyed by path + content hash
}

// New returns a Python adapter, or nil when no interpreter is available.
func New() *Adapter {
	for _, candidate := range []string{"python3", "python"} {
		if path, err := exec.LookPath(candidate); err == nil {
			if usable(path) {
				return &Adapter{python: path, cache: map[string]*result{}}
			}
		}
	}
	return nil
}

// usable rejects interpreters too old for ast.unparse (3.9+), rather than
// failing later with a confusing traceback.
func usable(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "-c",
		"import sys; print(sys.version_info >= (3, 9))").Output()
	return err == nil && strings.TrimSpace(string(out)) == "True"
}

func (a *Adapter) Name() string { return "python" }

func (a *Adapter) Extensions() []string { return []string{".py", ".pyi"} }

type result struct {
	Symbols []struct {
		Name       string   `json:"name"`
		Kind       string   `json:"kind"`
		LineStart  int      `json:"line_start"`
		LineEnd    int      `json:"line_end"`
		Signature  string   `json:"signature"`
		Doc        string   `json:"doc"`
		Exported   bool     `json:"exported"`
		Decorators []string `json:"decorators"`
		Comments   []struct {
			Text      string `json:"text"`
			LineStart int    `json:"line_start"`
			LineEnd   int    `json:"line_end"`
		} `json:"comments"`
		CallSites []struct {
			CalleeRaw string `json:"callee_raw"`
			Line      int    `json:"line"`
			Rationale string `json:"rationale"`
		} `json:"call_sites"`
		Norm string `json:"norm"`
	} `json:"symbols"`
	Comments []struct {
		Text      string `json:"text"`
		LineStart int    `json:"line_start"`
		LineEnd   int    `json:"line_end"`
	} `json:"comments"`
	Surface []struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Signature string `json:"signature"`
		Symbol    string `json:"symbol"`
	} `json:"surface"`
	Risks []struct {
		Kind   string `json:"kind"`
		Symbol string `json:"symbol"`
		Line   int    `json:"line"`
		Note   string `json:"note"`
	} `json:"risks"`
	Edges []struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"edges"`
	Error string `json:"error"`
}

// run parses one file. Results are cached on content, because a single session
// asks for the same file from four different passes.
func (a *Adapter) run(path string, src []byte) (*result, error) {
	sum := sha256.Sum256(src)
	key := path + ":" + hex.EncodeToString(sum[:8])

	a.mu.Lock()
	if r, ok := a.cache[key]; ok {
		a.mu.Unlock()
		return r, nil
	}
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.python, "-c", extractScript, path)
	cmd.Stdin = bytes.NewReader(src)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("python extractor on %s: %w: %s", path, err, strings.TrimSpace(errb.String()))
	}
	var r result
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		return nil, fmt.Errorf("python extractor on %s: %w", path, err)
	}
	if r.Error != "" {
		return nil, fmt.Errorf("%s: %s", path, r.Error)
	}

	a.mu.Lock()
	a.cache[key] = &r
	a.mu.Unlock()
	return &r, nil
}

func (a *Adapter) ParseSymbols(path string, src []byte) ([]bundle.Symbol, error) {
	r, err := a.run(path, src)
	if err != nil {
		return nil, err
	}
	rel := filepath.ToSlash(path)
	offsets := lineOffsets(src)

	local := map[string]string{} // bare name -> qualified, for intra-file binding
	for _, s := range r.Symbols {
		local[lastSegment(s.Name)] = s.Name
	}

	var out []bundle.Symbol
	for _, s := range r.Symbols {
		sym := bundle.Symbol{
			ID:        bundle.MakeID(rel, s.Name),
			Kind:      s.Kind,
			Name:      s.Name,
			File:      rel,
			LineStart: s.LineStart,
			LineEnd:   s.LineEnd,
			ByteStart: offsetOf(offsets, s.LineStart-1),
			ByteEnd:   offsetOf(offsets, s.LineEnd),
			Signature: s.Signature,
			Doc:       s.Doc,
			Exported:  s.Exported,
			// The fingerprint hashes the token stream Python itself produced,
			// with comments and the docstring dropped: reformatting must not
			// invalidate a claim, a rename or a logic change must.
			Fingerprint: hash(s.Norm),
		}
		for _, c := range s.Comments {
			sym.Comments = append(sym.Comments, bundle.Comment{Text: c.Text, LineStart: c.LineStart, LineEnd: c.LineEnd})
		}
		for _, cs := range s.CallSites {
			callee := bundle.SymbolID("::" + lastSegment(cs.CalleeRaw))
			if qual, ok := local[lastSegment(cs.CalleeRaw)]; ok {
				callee = bundle.MakeID(rel, qual)
			}
			sym.CallSites = append(sym.CallSites, bundle.CallSite{
				Callee: callee, CalleeRaw: cs.CalleeRaw, Line: cs.Line, Rationale: cs.Rationale,
			})
		}
		out = append(out, sym)
	}
	return out, nil
}

func (a *Adapter) PublicSurface(path string, src []byte) ([]bundle.SurfaceItem, error) {
	r, err := a.run(path, src)
	if err != nil {
		return nil, err
	}
	rel := filepath.ToSlash(path)
	module := moduleName(rel)
	var out []bundle.SurfaceItem
	for _, s := range r.Surface {
		name := s.Name
		if s.Kind == "export" {
			name = module + "." + name
		}
		out = append(out, bundle.SurfaceItem{
			Kind: s.Kind, Name: name, File: rel,
			Signature: s.Signature, Symbol: bundle.MakeID(rel, s.Symbol),
		})
	}
	return out, nil
}

func (a *Adapter) RiskMarkers(path string, src []byte, syms []bundle.Symbol) ([]bundle.RiskMarker, error) {
	r, err := a.run(path, src)
	if err != nil {
		return nil, err
	}
	rel := filepath.ToSlash(path)
	changed := map[bundle.SymbolID]bool{}
	for _, s := range syms {
		changed[s.ID] = true
	}
	var out []bundle.RiskMarker
	for _, m := range r.Risks {
		id := bundle.MakeID(rel, m.Symbol)
		if len(changed) > 0 && !changed[id] {
			continue // only report inside the changed set
		}
		out = append(out, bundle.RiskMarker{Kind: m.Kind, Symbol: id, File: rel, Line: m.Line, Note: m.Note})
	}
	return out, nil
}

func (a *Adapter) CallEdges(path string, src []byte) ([]bundle.Edge, error) {
	r, err := a.run(path, src)
	if err != nil {
		return nil, err
	}
	rel := filepath.ToSlash(path)
	local := map[string]string{}
	for _, s := range r.Symbols {
		local[lastSegment(s.Name)] = s.Name
	}
	seen := map[string]bool{}
	var out []bundle.Edge
	for _, e := range r.Edges {
		from := bundle.MakeID(rel, e.From)
		to := bundle.SymbolID("::" + lastSegment(e.To))
		if qual, ok := local[lastSegment(e.To)]; ok {
			to = bundle.MakeID(rel, qual)
		}
		if from == to {
			continue
		}
		key := string(from) + ">" + string(to)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, bundle.Edge{From: from, To: to, Kind: "call"})
	}
	return out, nil
}

func (a *Adapter) Comments(path string, src []byte) ([]bundle.Comment, error) {
	r, err := a.run(path, src)
	if err != nil {
		return nil, err
	}
	var out []bundle.Comment
	for _, c := range r.Comments {
		out = append(out, bundle.Comment{Text: c.Text, LineStart: c.LineStart, LineEnd: c.LineEnd})
	}
	return out, nil
}

// Normalise is used for ad-hoc fragments; whole-symbol fingerprints come from
// the extractor's own token stream, which is exact.
func (a *Adapter) Normalise(src []byte) ([]byte, error) {
	var kept []string
	for _, line := range strings.Split(string(src), "\n") {
		if i := strings.Index(line, "#"); i >= 0 && !strings.Contains(line[:i], `"`) && !strings.Contains(line[:i], "'") {
			line = line[:i]
		}
		if trimmed := strings.TrimRight(line, " \t"); strings.TrimSpace(trimmed) != "" {
			kept = append(kept, trimmed)
		}
	}
	return []byte(strings.Join(kept, "\n")), nil
}

// ShimSpec instruments Python with sys.monitoring, filtered to the changed set.
// The shim is a separate process speaking JSONL; the core never absorbs it.
func (a *Adapter) ShimSpec(syms []bundle.SymbolID) (trace.ShimSpec, error) {
	ids := make([]string, 0, len(syms))
	for _, s := range syms {
		ids = append(ids, string(s))
	}
	return trace.ShimSpec{
		Language: "python",
		Mode:     "env",
		Symbols:  syms,
		Env: map[string]string{
			"PLUM_TRACE":   "1",
			"PLUM_SYMBOLS": strings.Join(ids, ","),
		},
	}, nil
}

func hash(norm string) string {
	sum := sha256.Sum256([]byte(norm))
	return "sha256:" + hex.EncodeToString(sum[:])
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

func lastSegment(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

// moduleName renders a path the way an import would name it, so surface items
// read as `app.cache.Cache` rather than as a file path.
func moduleName(rel string) string {
	rel = strings.TrimSuffix(strings.TrimSuffix(rel, ".pyi"), ".py")
	rel = strings.TrimSuffix(rel, "/__init__")
	return strings.ReplaceAll(rel, "/", ".")
}

// Available reports whether a usable interpreter was found.
func Available() bool { return New() != nil }
