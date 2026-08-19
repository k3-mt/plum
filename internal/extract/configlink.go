package extract

import (
	"context"
	"regexp"
	"strings"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/lang/conf"
	"github.com/kelalaike/plum/internal/vcs"
)

// linkConfig connects code to the configuration it reads.
//
// This is the join that makes a config file worth pulling into the view at all.
// On its own, "server.timeout changed from 30 to 0" is a line in a diff; joined
// to the function that reads it, it is "the function you just changed now waits
// forever". Three bindings are recognised, and each records how it was found so
// a reader can judge it:
//
//   - env: code reads an environment variable whose name is defined in a config
//     or .env file
//   - literal: code contains a string literal equal to a config key or its leaf
//   - filename: code opens a config file by name
//
// Nothing here guesses. An unmatched key stays unmatched.
func (e *Extractor) linkConfig(ctx context.Context, b *bundle.Bundle, states map[string]*fileState) {
	var keys []target
	for _, fs := range sortedStates(states) {
		if !isConfigFile(fs.path) || len(fs.after) == 0 {
			continue
		}
		for _, k := range conf.Parse(fs.path, fs.after) {
			keys = append(keys, target{
				id:   bundle.MakeID(fs.path, k.Path),
				path: k.Path,
				file: fs.path,
			})
		}
	}
	if len(keys) == 0 {
		return
	}

	byLeaf := map[string][]target{}
	byPath := map[string][]target{}
	for _, k := range keys {
		byPath[k.path] = append(byPath[k.path], k)
		byLeaf[leafOf(k.path)] = append(byLeaf[leafOf(k.path)], k)
	}

	// Environment variables named in the public surface are the strongest
	// binding: the code asked for a name, and a config file defines that name.
	envReaders := map[string][]bundle.SymbolID{}
	for _, item := range b.Surface.Added {
		if item.Kind == "env_var" && item.Symbol != "" {
			envReaders[item.Name] = append(envReaders[item.Name], item.Symbol)
		}
	}

	seen := map[string]bool{}
	add := func(from bundle.SymbolID, to target, how string) {
		if from == "" {
			return
		}
		key := string(from) + ">" + string(to.id)
		if seen[key] {
			return
		}
		seen[key] = true
		b.Edges = append(b.Edges, bundle.Edge{
			From: from, To: to.id, Kind: "config:" + how,
			CrossesModule: true, New: true,
		})
	}

	for name, readers := range envReaders {
		for _, k := range append(byPath[name], byLeaf[name]...) {
			for _, r := range readers {
				add(r, k, "env")
			}
		}
	}

	// The code that reads a setting is almost never the code that changed in the
	// same session, so the search runs over the whole tree at EndSHA rather than
	// over the diff. Bounded, and every hit is resolved to its enclosing symbol
	// so the edge points at a function rather than at a line.
	e.searchReaders(ctx, b, keys, add)

	for _, fs := range sortedStates(states) {
		if isConfigFile(fs.path) || len(fs.after) == 0 || e.Reg.For(fs.path) == nil {
			continue
		}
		syms := symbolsIn(b, fs.path)
		if len(syms) == 0 {
			continue
		}
		lines := strings.Split(string(fs.after), "\n")
		for _, sym := range syms {
			for line := sym.LineStart; line <= sym.LineEnd && line <= len(lines); line++ {
				text := lines[line-1]
				for _, lit := range stringLiterals(text) {
					for _, k := range byPath[lit] {
						add(sym.ID, k, "literal")
					}
					if len(byPath[lit]) == 0 {
						for _, k := range byLeaf[lit] {
							add(sym.ID, k, "literal")
						}
					}
					for _, k := range keys {
						if k.file == lit || strings.HasSuffix(k.file, "/"+lit) || baseOf(k.file) == lit {
							add(sym.ID, k, "filename")
						}
					}
				}
			}
		}
	}
}

const (
	maxHitsPerKey = 25
	maxLinkedKeys = 60
)

// searchReaders greps the tree for each changed key's name and binds every hit
// to the symbol that encloses it.
func (e *Extractor) searchReaders(ctx context.Context, b *bundle.Bundle, keys []target, add func(bundle.SymbolID, target, string)) {
	changed := map[bundle.SymbolID]bool{}
	for _, s := range b.Symbols {
		if s.Kind == "config_key" {
			changed[s.ID] = true
		}
	}
	pathspecs := []string{"*.go", "*.py", "*.pyi", "*.ts", "*.tsx", "*.js", "*.jsx", "*.rb", "*.java", "*.sh"}

	linked := 0
	for _, k := range keys {
		if !changed[k.id] || linked >= maxLinkedKeys {
			continue
		}
		linked++
		// Search the leaf first: code says os.environ["AUTH_REALM"] or
		// cfg.get("timeout"), it does not say "server.timeout" in full.
		needles := []string{k.path}
		if leaf := leafOf(k.path); leaf != k.path {
			needles = append(needles, leaf)
		}
		for _, needle := range needles {
			if len(needle) < 3 {
				continue // too short to mean anything; a match would be noise
			}
			hits, err := e.Repo.Grep(ctx, b.Session.EndSHA, needle, pathspecs, maxHitsPerKey)
			if err != nil {
				continue
			}
			for _, hit := range hits {
				if e.Cfg.Excluded(hit.Path) || isConfigFile(hit.Path) {
					continue
				}
				how := "literal"
				if isEnvShaped(needle) && strings.Contains(hit.Text, needle) {
					how = "env"
				}
				if sym := e.enclosing(ctx, b, hit); sym != "" {
					add(sym, k, how)
				}
			}
		}
	}
}

// enclosing resolves a file:line hit to the declaration that contains it.
func (e *Extractor) enclosing(ctx context.Context, b *bundle.Bundle, hit vcs.GrepHit) bundle.SymbolID {
	a := e.Reg.For(hit.Path)
	if a == nil {
		return ""
	}
	src, err := e.Repo.Show(ctx, b.Session.EndSHA, hit.Path)
	if err != nil || src == "" {
		return ""
	}
	syms, err := a.ParseSymbols(hit.Path, []byte(src))
	if err != nil {
		return ""
	}
	var best bundle.SymbolID
	span := 1 << 30
	for _, s := range syms {
		if hit.Line >= s.LineStart && hit.Line <= s.LineEnd && s.LineEnd-s.LineStart < span {
			best, span = s.ID, s.LineEnd-s.LineStart
		}
	}
	if best == "" && len(syms) > 0 {
		return bundle.MakeID(hit.Path, "<module>") // read at import time, still a reader
	}
	return best
}

// isEnvShaped recognises SCREAMING_SNAKE names, the convention every language
// uses for environment variables.
func isEnvShaped(s string) bool {
	if len(s) < 3 {
		return false
	}
	for _, c := range s {
		if c >= 'a' && c <= 'z' {
			return false
		}
	}
	return strings.ContainsAny(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
}

var literalRe = regexp.MustCompile(`["'` + "`" + `]([A-Za-z_][A-Za-z0-9_.\-/]*)["'` + "`" + `]`)

func stringLiterals(line string) []string {
	var out []string
	for _, m := range literalRe.FindAllStringSubmatch(line, -1) {
		out = append(out, m[1])
	}
	return out
}

func symbolsIn(b *bundle.Bundle, file string) []bundle.Symbol {
	var out []bundle.Symbol
	for _, s := range b.Symbols {
		if s.File == file && s.Change != "deleted" {
			out = append(out, s)
		}
	}
	return out
}

func isConfigFile(path string) bool {
	switch strings.ToLower(extOf(path)) {
	case ".yaml", ".yml", ".toml", ".json", ".ini", ".cfg", ".env", ".properties":
		return true
	}
	return strings.HasPrefix(baseOf(path), ".env")
}

func extOf(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i:]
	}
	return ""
}

func baseOf(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

func leafOf(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i+1:]
	}
	return path
}

// target is one configuration key, as a link destination.
type target struct {
	id   bundle.SymbolID
	path string
	file string
}

// ConfigEdges returns the code→config bindings, for the report and the UI.
func ConfigEdges(b *bundle.Bundle) []bundle.Edge {
	var out []bundle.Edge
	for _, e := range b.Edges {
		if strings.HasPrefix(e.Kind, "config:") {
			out = append(out, e)
		}
	}
	return out
}
