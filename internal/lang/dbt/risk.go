package dbt

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kelalaike/plum/internal/bundle"
)

// RiskMarkers runs the predicates that matter for warehouse work.
//
// The failures these catch are not crashes. A model with `select *` keeps
// running perfectly while silently changing shape underneath everything
// downstream; an incremental model with no partition filter keeps returning the
// right answer while scanning the whole table every night. Nothing goes red.
// That is exactly why they are worth naming.
func (a *Adapter) RiskMarkers(path string, src []byte, syms []bundle.Symbol) ([]bundle.RiskMarker, error) {
	rel := filepath.ToSlash(path)
	changed := map[bundle.SymbolID]bool{}
	for _, s := range syms {
		changed[s.ID] = true
	}
	var out []bundle.RiskMarker
	mark := func(kind string, id bundle.SymbolID, line int, note string) {
		if len(changed) > 0 && !changed[id] {
			return
		}
		out = append(out, bundle.RiskMarker{Kind: kind, Symbol: id, File: rel, Line: line, Note: note})
	}

	if isSchemaFile(path) {
		for _, model := range parseSchema(src) {
			a.contractRisks(rel, model, mark)
		}
		return out, nil
	}
	if filepath.Ext(path) != ".sql" {
		return nil, nil
	}

	name := strings.TrimSuffix(filepath.Base(rel), ".sql")
	id := bundle.MakeID(rel, name)
	sql := string(src)
	code := stripSQLComments(sql)
	cfg := Config(sql)

	if m := selectStarRe.FindStringIndex(code); m != nil {
		mark("select_star", id, LineOf(code, m[0]),
			"select * — the column list is whatever upstream happens to have today, so a new or renamed column changes this model's shape without anything failing")
	}
	if m := crossJoinRe.FindStringIndex(code); m != nil {
		mark("cross_join", id, LineOf(code, m[0]),
			"cross join — row count multiplies rather than matches; if that is intended it is worth a comment saying so")
	}
	for _, m := range hardcodedRe.FindAllStringSubmatchIndex(code, -1) {
		table := strings.Trim(code[m[2]:m[3]], "`")
		mark("hardcoded_table", id, LineOf(code, m[0]),
			"selects from "+table+" directly instead of ref() or source() — dbt cannot see this dependency, so it will not build it first and it will not appear in lineage")
	}

	incremental := cfg["materialized"] == "incremental"
	if incremental {
		if !incrementalRe.MatchString(code) {
			mark("incremental_without_guard", id, 1,
				"materialized as incremental but nothing calls is_incremental() — every run rebuilds the whole table at full cost")
		}
		if m := nowRe.FindStringIndex(code); m != nil {
			mark("nondeterministic_incremental", id, LineOf(code, m[0]),
				"an incremental model that reads the clock cannot be rebuilt to the same result — a backfill will not match the rows it replaces")
		}
		if cfg["partition_by"] == "" {
			mark("incremental_without_partition", id, 1,
				"incremental with no partition_by — the merge has to scan the whole destination table on every run, which is where the bill comes from")
		}
		if cfg["unique_key"] == "" {
			mark("incremental_without_unique_key", id, 1,
				"incremental with no unique_key — rows are appended rather than merged, so a re-run of the same window duplicates them")
		}
	}
	if !incremental && cfg["materialized"] == "table" && cfg["partition_by"] == "" {
		if strings.Contains(strings.ToLower(rel), "/marts/") || strings.Contains(strings.ToLower(rel), "/fct") {
			mark("unpartitioned_table", id, 1,
				"a table in the mart layer with no partition_by — every downstream query scans all of it")
		}
	}
	return out, nil
}

var keyColumnRe = regexp.MustCompile(`(?i)(^id$|_id$|_key$|_sk$)`)

// contractRisks looks at what a model promises, rather than at how it is built.
func (a *Adapter) contractRisks(rel string, model Model, mark func(string, bundle.SymbolID, int, string)) {
	modelID := bundle.MakeID(rel, model.Name+" (contract)")
	if model.Description == "" {
		mark("undescribed_model", modelID, model.Line,
			model.Name+" has no description — what it is for is not written down anywhere a consumer can read")
	}

	var hasKeyTest bool
	for _, col := range model.Columns {
		colID := bundle.MakeID(rel, model.Name+"."+col.Name)
		if !keyColumnRe.MatchString(col.Name) {
			continue
		}
		if col.HasTest("unique") && col.HasTest("not_null") {
			hasKeyTest = true
			continue
		}
		missing := []string{}
		if !col.HasTest("unique") {
			missing = append(missing, "unique")
		}
		if !col.HasTest("not_null") {
			missing = append(missing, "not_null")
		}
		mark("untested_key", colID, col.Line, fmt.Sprintf(
			"%s looks like a key but has no %s test — nothing would notice if the grain of this model changed",
			col.Name, strings.Join(missing, " or ")))
	}
	if !hasKeyTest && len(model.Columns) > 0 {
		mark("no_grain_test", modelID, model.Line,
			model.Name+" has no column with both unique and not_null — its grain is undefended, so a duplicating join upstream would pass unnoticed")
	}

	for _, col := range model.Columns {
		if col.DataType == "" {
			continue
		}
		colID := bundle.MakeID(rel, model.Name+"."+col.Name)
		if strings.EqualFold(col.DataType, "float64") && looksMonetary(col.Name) {
			mark("float_money", colID, col.Line,
				col.Name+" holds money in FLOAT64 — binary floating point cannot represent decimal amounts exactly, so sums drift; NUMERIC is exact")
		}
	}
}

func looksMonetary(name string) bool {
	lower := strings.ToLower(name)
	for _, hint := range []string{"amount", "price", "total", "cost", "revenue", "value", "pence", "cents", "fee", "balance"} {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}
