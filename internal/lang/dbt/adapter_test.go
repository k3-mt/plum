package dbt

import (
	"strings"
	"testing"

	"github.com/k3-mt/plum/internal/bundle"
)

const model = `-- Order facts: one row per order.
{{ config(
    materialized='incremental',
    unique_key='order_id',
    partition_by={'field': 'ordered_at', 'data_type': 'date'}
) }}

select
    o.order_id,
    o.order_total
-- the mart layer decides what counts as a real order
from {{ ref('stg_orders') }} o
left join {{ ref('stg_payments') }} p on p.order_id = o.order_id
{% if is_incremental() %}
where o.ordered_at >= (select max(ordered_at) from {{ this }})
{% endif %}
`

// A partition_by argument contains braces of its own. A config pattern that
// stops at the first closing brace reads the whole block as empty, which made
// every partitioned model look like an unconfigured view.
func TestConfigSurvivesNestedBraces(t *testing.T) {
	cfg := Config(model)
	for key, want := range map[string]string{
		"materialized": "incremental",
		"unique_key":   "order_id",
	} {
		if cfg[key] != want {
			t.Errorf("config[%q] = %q, want %q", key, cfg[key], want)
		}
	}
	if cfg["partition_by"] == "" {
		t.Error("partition_by was lost; the model would be reported as unpartitioned")
	}
}

func TestRefsAreTheDAG(t *testing.T) {
	models, sources := Refs(model)
	if len(models) != 2 || models[0] != "stg_orders" || models[1] != "stg_payments" {
		t.Errorf("refs = %v", models)
	}
	if len(sources) != 0 {
		t.Errorf("sources = %v", sources)
	}
	// A ref inside a comment is not a dependency.
	if got, _ := Refs("-- see {{ ref('old_model') }}\nselect 1"); len(got) != 0 {
		t.Errorf("a commented-out ref was counted: %v", got)
	}
}

func TestNormaliseIgnoresFormattingButNotContent(t *testing.T) {
	base := "select a, b from {{ ref('x') }}"
	reformatted := "select\n    a,\n    b\n-- a new comment\nfrom {{ ref('x') }}"
	changed := "select a, c from {{ ref('x') }}"

	if Normalise(base) != Normalise(reformatted) {
		t.Error("reformatting a query must not change its fingerprint")
	}
	if Normalise(base) == Normalise(changed) {
		t.Error("selecting a different column must change its fingerprint")
	}
}

const schema = `version: 2

models:
  - name: fct_orders
    description: One row per order.
    columns:
      - name: order_id
        description: The order's identifier.
        data_type: STRING
        tests:
          - unique
          - not_null
      - name: order_total
        data_type: NUMERIC
      - name: status
        data_type: STRING
        tests:
          - accepted_values:
              values: ['pending', 'paid']
`

func TestSchemaReadsTheDeclaredContract(t *testing.T) {
	models := parseSchema([]byte(schema))
	if len(models) != 1 {
		t.Fatalf("got %d models", len(models))
	}
	m := models[0]
	if m.Name != "fct_orders" || m.Description == "" {
		t.Errorf("model = %+v", m)
	}
	if len(m.Columns) != 3 {
		t.Fatalf("columns = %+v", m.Columns)
	}
	if !m.Columns[0].HasTest("unique") || !m.Columns[0].HasTest("not_null") {
		t.Errorf("order_id tests = %v", m.Columns[0].Tests)
	}
	// The configured shape `- accepted_values:` names a test too.
	if !m.Columns[2].HasTest("accepted_values") {
		t.Errorf("status tests = %v", m.Columns[2].Tests)
	}
	if m.Columns[1].Tests != nil {
		t.Errorf("order_total has no tests declared, got %v", m.Columns[1].Tests)
	}
}

// A column is the unit other people depend on, so it has to be a symbol in its
// own right — a dropped column is the change this whole tool exists to surface.
func TestColumnsAreSymbolsWithTypeAndTestsInTheSignature(t *testing.T) {
	a := New(t.TempDir())
	syms, err := a.ParseSymbols("models/marts/schema.yml", []byte(schema))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[bundle.SymbolID]bundle.Symbol{}
	for _, s := range syms {
		byID[s.ID] = s
	}
	col, ok := byID["models/marts/schema.yml::fct_orders.order_id"]
	if !ok {
		t.Fatalf("no symbol for the column: %v", byID)
	}
	if col.Kind != "column" {
		t.Errorf("kind = %q", col.Kind)
	}
	if !strings.Contains(col.Signature, "STRING") || !strings.Contains(col.Signature, "unique") {
		t.Errorf("signature = %q — the type and the tests are what downstream relies on", col.Signature)
	}
}

// Rewording a description must not stale a claim; retyping a column or dropping
// its test must.
func TestColumnFingerprintTracksTheContractNotTheProse(t *testing.T) {
	a := New(t.TempDir())
	fp := func(src string) string {
		syms, err := a.ParseSymbols("s.yml", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range syms {
			if strings.HasSuffix(string(s.ID), "fct_orders.order_id") {
				return s.Fingerprint
			}
		}
		t.Fatal("column not found")
		return ""
	}
	reworded := strings.Replace(schema, "The order's identifier.", "Unique id of the order.", 1)
	retyped := strings.Replace(schema, "data_type: STRING\n        tests:", "data_type: INT64\n        tests:", 1)
	untested := strings.Replace(schema, "          - unique\n", "", 1)

	if fp(schema) != fp(reworded) {
		t.Error("rewording a description changed the contract fingerprint")
	}
	if fp(schema) == fp(retyped) {
		t.Error("retyping a column must change its fingerprint")
	}
	if fp(schema) == fp(untested) {
		t.Error("dropping a test changes what downstream can rely on")
	}
}

func TestWarehouseRiskPredicates(t *testing.T) {
	a := New(t.TempDir())
	risky := `{{ config(materialized='incremental', unique_key='id') }}
select o.*, current_timestamp() as built_at
from ` + "`proj.raw.orders`" + ` o
cross join {{ ref('dim_date') }}
`
	syms, _ := a.ParseSymbols("models/marts/f.sql", []byte(risky))
	marks, err := a.RiskMarkers("models/marts/f.sql", []byte(risky), syms)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, m := range marks {
		kinds[m.Kind] = true
	}
	for _, want := range []string{
		"select_star",                   // o.* is the same hazard as *
		"hardcoded_table",               // escapes the DAG
		"cross_join",                    // row count multiplies
		"nondeterministic_incremental",  // cannot be rebuilt to the same result
		"incremental_without_guard",     // rebuilds everything every night
		"incremental_without_partition", // scans the whole destination
	} {
		if !kinds[want] {
			t.Errorf("missing %q (got %v)", want, keysOf(kinds))
		}
	}

	// A well-formed model should be quiet.
	clean := `{{ config(materialized='view') }}
select id, total from {{ ref('stg_x') }}
`
	cleanSyms, _ := a.ParseSymbols("models/staging/stg_y.sql", []byte(clean))
	cleanMarks, _ := a.RiskMarkers("models/staging/stg_y.sql", []byte(clean), cleanSyms)
	if len(cleanMarks) != 0 {
		t.Errorf("a clean model produced %d markers: %+v", len(cleanMarks), cleanMarks)
	}
}

func TestUntestedKeyAndFloatMoney(t *testing.T) {
	a := New(t.TempDir())
	src := `version: 2
models:
  - name: fct_x
    description: Facts.
    columns:
      - name: order_id
        data_type: STRING
      - name: revenue_total
        data_type: FLOAT64
`
	syms, _ := a.ParseSymbols("models/marts/schema.yml", []byte(src))
	marks, _ := a.RiskMarkers("models/marts/schema.yml", []byte(src), syms)
	kinds := map[string]bool{}
	for _, m := range marks {
		kinds[m.Kind] = true
	}
	if !kinds["untested_key"] {
		t.Error("a key column with no unique/not_null test should be flagged")
	}
	if !kinds["float_money"] {
		t.Error("money in FLOAT64 should be flagged: decimal amounts do not round-trip")
	}
	if !kinds["no_grain_test"] {
		t.Error("a model with no defended grain should be flagged")
	}
}

func keysOf(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
