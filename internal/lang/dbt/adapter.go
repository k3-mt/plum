package dbt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"

	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/trace"
)

// Adapter reads a dbt project.
type Adapter struct {
	root string

	once   sync.Once
	models map[string]string // model name -> repo-relative path of its .sql
}

func New(root string) *Adapter { return &Adapter{root: root} }

func (a *Adapter) Name() string { return "dbt" }

func (a *Adapter) Extensions() []string { return []string{".sql", ".yml", ".yaml"} }

// index maps model names to files once, so a ref() can be resolved to the model
// it points at.
func (a *Adapter) index() map[string]string {
	a.once.Do(func() {
		a.models = map[string]string{}
		_ = filepath.WalkDir(a.root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "target", "dbt_packages", "logs", ".plum":
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != ".sql" {
				return nil
			}
			rel, err := filepath.Rel(a.root, path)
			if err != nil {
				return nil
			}
			name := strings.TrimSuffix(filepath.Base(path), ".sql")
			a.models[name] = filepath.ToSlash(rel)
			return nil
		})
	})
	return a.models
}

func isSchemaFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yml" || ext == ".yaml"
}

// ParseSymbols returns a model for each .sql file, and a symbol per declared
// column for each schema file.
//
// A column is a symbol in its own right because it is the unit other people
// depend on. A dropped column breaks every downstream model that selects it,
// and nothing fails to compile — which makes it exactly the kind of change this
// tool exists to put in front of a reader.
func (a *Adapter) ParseSymbols(path string, src []byte) ([]bundle.Symbol, error) {
	rel := filepath.ToSlash(path)
	if isSchemaFile(path) {
		return a.schemaSymbols(rel, src), nil
	}
	if filepath.Ext(path) != ".sql" {
		return nil, nil
	}
	return a.modelSymbols(rel, src), nil
}

func (a *Adapter) modelSymbols(rel string, src []byte) []bundle.Symbol {
	name := strings.TrimSuffix(filepath.Base(rel), ".sql")
	sql := string(src)
	cfg := Config(sql)

	materialized := cfg["materialized"]
	if materialized == "" {
		materialized = "view" // dbt's default, worth stating rather than leaving blank
	}
	signature := fmt.Sprintf("model %s (%s)", name, materialized)
	if by := cfg["partition_by"]; by != "" {
		signature += " partitioned by " + by
	}

	sym := bundle.Symbol{
		ID:          bundle.MakeID(rel, name),
		Kind:        "model",
		Name:        name,
		File:        rel,
		LineStart:   1,
		LineEnd:     strings.Count(sql, "\n") + 1,
		ByteStart:   0,
		ByteEnd:     len(src),
		Signature:   signature,
		Doc:         leadingComment(sql),
		Exported:    true, // every model is something another model may select from
		Fingerprint: hash(Normalise(sql)),
	}
	sym.Comments = sqlComments(sql)
	sym.CallSites = a.refCallSites(rel, sql)
	return []bundle.Symbol{sym}
}

// schemaSymbols turns declared columns into symbols. Their line numbers are the
// lines in the schema file, because that is where the contract is written and
// where a reviewer will change it.
func (a *Adapter) schemaSymbols(rel string, src []byte) []bundle.Symbol {
	var out []bundle.Symbol
	for _, model := range parseSchema(src) {
		out = append(out, bundle.Symbol{
			ID:          bundle.MakeID(rel, model.Name+" (contract)"),
			Kind:        "contract",
			Name:        model.Name + " (contract)",
			File:        rel,
			LineStart:   model.Line,
			LineEnd:     model.Line,
			Signature:   fmt.Sprintf("%s declares %s", model.Name, plural(len(model.Columns), "column")),
			Doc:         model.Description,
			Exported:    true,
			Fingerprint: hash(model.Name + "\x00" + columnSummary(model)),
		})
		for _, col := range model.Columns {
			dataType := col.DataType
			if dataType == "" {
				dataType = "type not declared"
			}
			tests := "no tests"
			if len(col.Tests) > 0 {
				tests = strings.Join(col.Tests, ", ")
			}
			out = append(out, bundle.Symbol{
				ID:        bundle.MakeID(rel, model.Name+"."+col.Name),
				Kind:      "column",
				Name:      model.Name + "." + col.Name,
				File:      rel,
				LineStart: col.Line,
				LineEnd:   col.Line,
				Signature: fmt.Sprintf("%s %s [%s]", col.Name, dataType, tests),
				Doc:       col.Description,
				Exported:  true,
				// The fingerprint covers the type and the tests, not the
				// description: retyping a column or dropping its test changes
				// what downstream models can rely on, rewording its description
				// does not.
				Fingerprint: hash(col.Name + "\x00" + dataType + "\x00" + strings.Join(col.Tests, ",")),
			})
		}
	}
	return out
}

func columnSummary(m Model) string {
	parts := make([]string, 0, len(m.Columns))
	for _, c := range m.Columns {
		parts = append(parts, c.Name+":"+c.DataType)
	}
	return strings.Join(parts, ",")
}

// refCallSites treats each ref() as a call, with the comment above it as its
// rationale — the same shape as a call site in code.
func (a *Adapter) refCallSites(rel, sql string) []bundle.CallSite {
	lines := strings.Split(sql, "\n")
	var out []bundle.CallSite
	for i, line := range lines {
		for _, m := range refRe.FindAllStringSubmatch(stripSQLComments(line), -1) {
			cs := bundle.CallSite{CalleeRaw: m[1], Line: i + 1}
			if path, ok := a.index()[m[1]]; ok {
				cs.Callee = bundle.MakeID(path, m[1])
			} else {
				cs.Callee = bundle.SymbolID("::" + m[1])
			}
			cs.Rationale = commentAbove(lines, i)
			out = append(out, cs)
		}
		for _, m := range sourceRe.FindAllStringSubmatch(stripSQLComments(line), -1) {
			out = append(out, bundle.CallSite{
				Callee:    bundle.SymbolID("::source." + m[1] + "." + m[2]),
				CalleeRaw: "source(" + m[1] + ", " + m[2] + ")",
				Line:      i + 1,
				Rationale: commentAbove(lines, i),
			})
		}
	}
	return out
}

// PublicSurface is the declared contract: every model, and every column of it.
func (a *Adapter) PublicSurface(path string, src []byte) ([]bundle.SurfaceItem, error) {
	rel := filepath.ToSlash(path)
	var out []bundle.SurfaceItem
	if isSchemaFile(path) {
		for _, model := range parseSchema(src) {
			for _, col := range model.Columns {
				dataType := col.DataType
				if dataType == "" {
					dataType = "type not declared"
				}
				tests := "untested"
				if len(col.Tests) > 0 {
					tests = strings.Join(col.Tests, "+")
				}
				out = append(out, bundle.SurfaceItem{
					Kind: "column", Name: model.Name + "." + col.Name, File: rel,
					Signature: dataType + " [" + tests + "]",
					Symbol:    bundle.MakeID(rel, model.Name+"."+col.Name),
				})
			}
		}
		return out, nil
	}
	if filepath.Ext(path) != ".sql" {
		return nil, nil
	}
	name := strings.TrimSuffix(filepath.Base(rel), ".sql")
	cfg := Config(string(src))
	materialized := cfg["materialized"]
	if materialized == "" {
		materialized = "view"
	}
	out = append(out, bundle.SurfaceItem{
		Kind: "model", Name: name, File: rel,
		Signature: materialized, Symbol: bundle.MakeID(rel, name),
	})
	return out, nil
}

// CallEdges is the DAG, as each model declares it through ref().
func (a *Adapter) CallEdges(path string, src []byte) ([]bundle.Edge, error) {
	if filepath.Ext(path) != ".sql" || isSchemaFile(path) {
		return nil, nil
	}
	rel := filepath.ToSlash(path)
	from := bundle.MakeID(rel, strings.TrimSuffix(filepath.Base(rel), ".sql"))
	seen := map[string]bool{}
	var out []bundle.Edge
	for _, cs := range a.refCallSites(rel, string(src)) {
		if seen[string(cs.Callee)] {
			continue
		}
		seen[string(cs.Callee)] = true
		out = append(out, bundle.Edge{From: from, To: cs.Callee, Kind: "ref"})
	}
	return out, nil
}

func (a *Adapter) Comments(path string, src []byte) ([]bundle.Comment, error) {
	if isSchemaFile(path) {
		return nil, nil
	}
	return sqlComments(string(src)), nil
}

func (a *Adapter) Normalise(src []byte) ([]byte, error) {
	return []byte(Normalise(string(src))), nil
}

// ShimSpec: a dbt run reports its own execution, so there is nothing to
// instrument. Ingesting run_results.json is a separate piece of work.
func (a *Adapter) ShimSpec(syms []bundle.SymbolID) (trace.ShimSpec, error) {
	return trace.ShimSpec{Language: "dbt", Mode: "none"}, nil
}

// ---------------------------------------------------------------- helpers

func sqlComments(sql string) []bundle.Comment {
	var out []bundle.Comment
	for i, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			out = append(out, bundle.Comment{
				Text:      strings.TrimSpace(strings.TrimPrefix(trimmed, "--")),
				LineStart: i + 1, LineEnd: i + 1,
			})
		}
	}
	return out
}

// leadingComment is the block at the top of a model file: the closest thing SQL
// has to a declaration doc.
func leadingComment(sql string) string {
	var block []string
	for _, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" && len(block) == 0 {
			continue
		}
		if !strings.HasPrefix(trimmed, "--") {
			break
		}
		block = append(block, strings.TrimSpace(strings.TrimPrefix(trimmed, "--")))
	}
	return strings.Join(block, "\n")
}

func commentAbove(lines []string, i int) string {
	var block []string
	for j := i - 1; j >= 0; j-- {
		trimmed := strings.TrimSpace(lines[j])
		if !strings.HasPrefix(trimmed, "--") {
			break
		}
		block = append([]string{strings.TrimSpace(strings.TrimPrefix(trimmed, "--"))}, block...)
	}
	return strings.Join(block, "\n")
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
