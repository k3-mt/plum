package extract

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/config"
	"github.com/kelalaike/plum/internal/lang"
	"github.com/kelalaike/plum/internal/vcs"
)

// Extractor assembles a bundle from a commit range. It reads blobs from git
// objects rather than the working tree, so a session is reproducible after the
// fact and independent of whatever the agent left lying around (P7).
type Extractor struct {
	Repo *vcs.Repo
	Cfg  *config.Config
	Reg  *lang.Registry
}

func New(repo *vcs.Repo, cfg *config.Config, reg *lang.Registry) *Extractor {
	return &Extractor{Repo: repo, Cfg: cfg, Reg: reg}
}

type fileState struct {
	path     string
	change   string
	before   []byte
	after    []byte
	language string
	hunks    []Hunk
	added    int
	deleted  int
}

// Extract produces the bundle for a session. The session record supplies the
// commit range; everything else is derived here.
func (e *Extractor) Extract(ctx context.Context, sess bundle.Session, journal []bundle.JournalEntry) (*bundle.Bundle, error) {
	b := &bundle.Bundle{SchemaVersion: bundle.SchemaVersion, Session: sess, Journal: journal}

	rawDiff, err := e.Repo.Diff(ctx, sess.StartSHA, sess.EndSHA)
	if err != nil {
		return nil, err
	}
	numstat, _ := e.Repo.NameStatus(ctx, sess.StartSHA, sess.EndSHA)
	namestatus, _ := e.Repo.Status(ctx, sess.StartSHA, sess.EndSHA)

	counts := ParseNumstat(numstat)
	kinds := ParseNameStatus(namestatus)
	hunks := HunksByFile(ParseDiff(rawDiff))

	states := map[string]*fileState{}
	for path, kind := range kinds {
		if e.Cfg.Excluded(path) {
			continue
		}
		fs := &fileState{path: path, change: kind, hunks: hunks[path]}
		if c, ok := counts[path]; ok {
			fs.added, fs.deleted = c[0], c[1]
		}
		if kind != "added" {
			src, _ := e.Repo.Show(ctx, sess.StartSHA, path)
			fs.before = []byte(src)
		}
		if kind != "deleted" {
			src, _ := e.Repo.Show(ctx, sess.EndSHA, path)
			fs.after = []byte(src)
		}
		fs.language = e.Reg.Language(path)
		states[path] = fs

		b.Files = append(b.Files, bundle.FileChange{
			Path:      path,
			Change:    kind,
			Added:     fs.added,
			Deleted:   fs.deleted,
			Language:  fs.language,
			Binary:    isBinary(fs.after),
			Migration: isMigration(path),
		})
	}

	e.symbols(b, states)
	e.surface(ctx, b, states)
	e.edges(b, states)
	e.risks(b, states)
	e.deps(ctx, b, sess)
	e.divergence(ctx, b, states)
	e.coverage(b, states)
	e.gate(b)
	b.Sort()
	return b, nil
}

// symbols parses both sides of every changed file, maps hunks onto the new-side
// declarations, and records deletions from the old side.
func (e *Extractor) symbols(b *bundle.Bundle, states map[string]*fileState) {
	for _, fs := range sortedStates(states) {
		a := e.Reg.For(fs.path)
		if a == nil {
			continue
		}
		var beforeSyms, afterSyms []bundle.Symbol
		if len(fs.before) > 0 {
			beforeSyms, _ = a.ParseSymbols(fs.path, fs.before)
		}
		if len(fs.after) > 0 {
			afterSyms, _ = a.ParseSymbols(fs.path, fs.after)
		}
		beforeByID := index(beforeSyms)

		touched := MapHunks(afterSyms, fs.hunks)
		if fs.change == "added" {
			touched = afterSyms // a new file: every declaration in it is new
		}
		for _, s := range touched {
			if prev, ok := beforeByID[s.ID]; ok {
				if prev.Fingerprint == s.Fingerprint {
					continue // reformatted or moved, but semantically identical (P5)
				}
				s.Change = "modified"
			} else {
				s.Change = "added"
			}
			b.Symbols = append(b.Symbols, s)
		}

		// Deletions are only visible from the old side.
		afterByID := index(afterSyms)
		for _, s := range beforeSyms {
			if _, ok := afterByID[s.ID]; ok {
				continue
			}
			if !touchedByHunk(s, fs.hunks, true) && fs.change != "deleted" {
				continue
			}
			s.Change = "deleted"
			b.Symbols = append(b.Symbols, s)
		}
	}
}

func touchedByHunk(s bundle.Symbol, hunks []Hunk, oldSide bool) bool {
	for _, h := range hunks {
		start, end := h.Start, h.End()
		if oldSide {
			start = h.OldStart
			end = h.OldStart + max(h.OldLines, 1) - 1
		}
		if start <= s.LineEnd && end >= s.LineStart {
			return true
		}
	}
	return false
}

// surface diffs the exported API of every changed file. Signature changes on
// *existing* exports are the highest-signal event this tool produces, because
// they silently break callers nobody looked at (§6.8).
func (e *Extractor) surface(ctx context.Context, b *bundle.Bundle, states map[string]*fileState) {
	for _, fs := range sortedStates(states) {
		if isTestFile(fs.path) {
			continue // tests are not public surface, however exported their names look
		}
		a := e.Reg.For(fs.path)
		if a == nil {
			if isMigration(fs.path) && fs.change != "deleted" {
				b.Surface.Added = append(b.Surface.Added, bundle.SurfaceItem{
					Kind: "migration", Name: filepath.Base(fs.path), File: fs.path,
				})
			}
			continue
		}
		var before, after []bundle.SurfaceItem
		if len(fs.before) > 0 {
			before, _ = a.PublicSurface(fs.path, fs.before)
		}
		if len(fs.after) > 0 {
			after, _ = a.PublicSurface(fs.path, fs.after)
		}
		bm, am := surfaceIndex(before), surfaceIndex(after)
		for k, item := range am {
			prev, ok := bm[k]
			switch {
			case !ok:
				b.Surface.Added = append(b.Surface.Added, item)
			case prev.Signature != item.Signature:
				b.Surface.Modified = append(b.Surface.Modified, bundle.SurfaceMod{
					SurfaceItem: item, Before: prev.Signature, After: item.Signature,
				})
			}
		}
		for k, item := range bm {
			if _, ok := am[k]; !ok {
				b.Surface.Removed = append(b.Surface.Removed, item)
			}
		}
		if isMigration(fs.path) && fs.change == "added" {
			b.Surface.Added = append(b.Surface.Added, bundle.SurfaceItem{
				Kind: "migration", Name: filepath.Base(fs.path), File: fs.path,
			})
		}
	}
	sortSurface(&b.Surface)
}

// edges records call edges out of the changed symbols, resolving unqualified
// callees against the whole changed set. Cross-file resolution is best effort —
// the interface promises no more than that (§4.1).
func (e *Extractor) edges(b *bundle.Bundle, states map[string]*fileState) {
	byName := map[string][]bundle.SymbolID{}
	for _, s := range b.Symbols {
		byName[lastSeg(s.Name)] = append(byName[lastSeg(s.Name)], s.ID)
	}
	changed := map[bundle.SymbolID]bool{}
	for _, s := range b.Symbols {
		changed[s.ID] = true
	}

	seen := map[string]bool{}
	for _, fs := range sortedStates(states) {
		a := e.Reg.For(fs.path)
		if a == nil || len(fs.after) == 0 {
			continue
		}
		edges, _ := a.CallEdges(fs.path, fs.after)
		var beforeEdges map[string]bool
		if len(fs.before) > 0 {
			beforeEdges = map[string]bool{}
			old, _ := a.CallEdges(fs.path, fs.before)
			for _, oe := range old {
				beforeEdges[string(oe.From)+">"+string(oe.To)] = true
			}
		}
		for _, ed := range edges {
			if !changed[ed.From] {
				continue
			}
			if strings.HasPrefix(string(ed.To), "::") {
				name := strings.TrimPrefix(string(ed.To), "::")
				cands := byName[name]
				if len(cands) != 1 {
					continue // ambiguous or external — do not invent an edge
				}
				ed.To = cands[0]
			}
			if ed.From == ed.To {
				continue
			}
			key := string(ed.From) + ">" + string(ed.To)
			if seen[key] {
				continue
			}
			seen[key] = true
			ed.CrossesModule = moduleOf(ed.From.File()) != moduleOf(ed.To.File())
			ed.New = beforeEdges == nil || !beforeEdges[key]
			b.Edges = append(b.Edges, ed)
		}
	}
}

func (e *Extractor) risks(b *bundle.Bundle, states map[string]*fileState) {
	byFile := map[string][]bundle.Symbol{}
	for _, s := range b.Symbols {
		if s.Change != "deleted" {
			byFile[s.File] = append(byFile[s.File], s)
		}
	}
	for _, fs := range sortedStates(states) {
		if isTestFile(fs.path) {
			continue // a predicate firing inside a test says nothing about the code
		}
		a := e.Reg.For(fs.path)
		if a == nil || len(fs.after) == 0 || len(byFile[fs.path]) == 0 {
			continue
		}
		marks, _ := a.RiskMarkers(fs.path, fs.after, byFile[fs.path])
		b.RiskMarkers = append(b.RiskMarkers, marks...)
	}
}

func (e *Extractor) coverage(b *bundle.Bundle, states map[string]*fileState) {
	b.Coverage.TestCommand = e.Cfg.Repo.TestCommand
	testNames := map[string]bool{}
	for path, fs := range states {
		if !isTestFile(path) || len(fs.after) == 0 {
			continue
		}
		if fs.change == "added" {
			b.Coverage.TestFilesNew = append(b.Coverage.TestFilesNew, path)
		}
		testNames[strings.ToLower(string(fs.after))] = true
	}
	haystack := strings.Builder{}
	for k := range testNames {
		haystack.WriteString(k)
	}
	hay := haystack.String()

	for i := range b.Symbols {
		s := &b.Symbols[i]
		if s.Change == "deleted" {
			continue
		}
		if isTestFile(s.File) {
			s.Tested = true // a test is not itself comprehension debt
			continue
		}
		b.Coverage.SymbolCount++
		// A symbol counts as tested when a changed test file mentions it by name.
		// Deliberately shallow: real coverage arrives with traces in M2.
		if hay != "" && strings.Contains(hay, strings.ToLower(lastSeg(s.Name))) {
			s.Tested = true
			b.Coverage.TestedCount++
		} else {
			b.Coverage.Untested = append(b.Coverage.Untested, s.ID)
		}
	}
	sort.Slice(b.Coverage.Untested, func(i, j int) bool { return b.Coverage.Untested[i] < b.Coverage.Untested[j] })
	sort.Strings(b.Coverage.TestFilesNew)
}

// gate decides whether this session deserves the developer's attention (P6).
// Most sessions get one journal line and nothing else.
func (e *Extractor) gate(b *bundle.Bundle) {
	g := e.Cfg.Gating
	var reasons []string
	if n := len(b.Symbols); n >= g.MinSymbolsChanged {
		reasons = append(reasons, plural(n, "symbol")+" changed (threshold "+itoa(g.MinSymbolsChanged)+")")
	}
	if g.NewPublicSurface {
		if n := len(b.Surface.Added); n > 0 {
			reasons = append(reasons, "new public surface: "+plural(n, "item"))
		}
		if n := len(b.Surface.Modified); n > 0 {
			reasons = append(reasons, "signature changed on "+plural(n, "existing export"))
		}
		if n := len(b.Surface.Removed); n > 0 {
			reasons = append(reasons, plural(n, "public item")+" removed")
		}
	}
	if g.NewDependency && len(b.Deps.Added)+len(b.Deps.Upgraded) > 0 {
		reasons = append(reasons, "dependency graph changed")
	}
	if g.MigrationTouched {
		for _, f := range b.Files {
			if f.Migration {
				reasons = append(reasons, "migration touched: "+f.Path)
				break
			}
		}
	}
	if b.Divergence.Score >= g.DivergenceThreshold {
		reasons = append(reasons, "divergence "+ftoa(b.Divergence.Score))
	}
	if g.RiskMarkers > 0 && len(b.RiskMarkers) >= g.RiskMarkers {
		reasons = append(reasons, plural(len(b.RiskMarkers), "risk marker"))
	}
	b.Gate = bundle.Gate{Fired: len(reasons) > 0, Reasons: reasons}
}

func index(syms []bundle.Symbol) map[bundle.SymbolID]bundle.Symbol {
	m := make(map[bundle.SymbolID]bundle.Symbol, len(syms))
	for _, s := range syms {
		m[s.ID] = s
	}
	return m
}

func surfaceIndex(items []bundle.SurfaceItem) map[string]bundle.SurfaceItem {
	m := make(map[string]bundle.SurfaceItem, len(items))
	for _, i := range items {
		m[i.Kind+":"+i.Name] = i
	}
	return m
}

func sortSurface(s *bundle.SurfaceDelta) {
	sort.Slice(s.Added, func(i, j int) bool { return s.Added[i].Name < s.Added[j].Name })
	sort.Slice(s.Removed, func(i, j int) bool { return s.Removed[i].Name < s.Removed[j].Name })
	sort.Slice(s.Modified, func(i, j int) bool { return s.Modified[i].Name < s.Modified[j].Name })
}

func sortedStates(m map[string]*fileState) []*fileState {
	out := make([]*fileState, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

func isBinary(b []byte) bool {
	for i, c := range b {
		if i > 8000 {
			break
		}
		if c == 0 {
			return true
		}
	}
	return false
}

func isMigration(path string) bool {
	p := strings.ToLower(filepath.ToSlash(path))
	return strings.Contains(p, "migration") || strings.Contains(p, "/migrate/") || strings.HasSuffix(p, ".sql")
}

func isTestFile(path string) bool {
	p := strings.ToLower(filepath.ToSlash(path))
	base := filepath.Base(p)
	return strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, "test_") ||
		strings.HasSuffix(base, ".test.ts") || strings.HasSuffix(base, ".test.js") ||
		strings.HasSuffix(base, "_test.py") || strings.HasSuffix(base, ".spec.ts") ||
		strings.Contains(p, "/tests/") || strings.Contains(p, "/testdata/")
}

func moduleOf(path string) string {
	d := filepath.ToSlash(filepath.Dir(path))
	if d == "." {
		return ""
	}
	return d
}

func lastSeg(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
