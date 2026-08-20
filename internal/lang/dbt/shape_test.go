package dbt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k3-mt/plum/internal/bundle"
)

const fctSQL = `-- Order facts: one row per order, with what was actually paid against it.
{{ config(materialized='incremental', unique_key='order_id') }}

select
    o.*,
    coalesce(sum(p.payment_total), 0) as amount_paid
from {{ ref('stg_orders') }} o
left join {{ ref('stg_payments') }} p
    on p.order_id = o.order_id
left join ` + "`shop-prod-1234.shop_raw.refunds`" + ` r
    on r.order_id = o.order_id
group by 1, 2, 3, 4, 5
`

// A join type is the difference between rows being dropped, kept, or
// multiplied. Nothing in a run's artifacts records it; only the statement does.
func TestShapeReadsJoinTypeAndKey(t *testing.T) {
	s := ReadShape(fctSQL, "")
	if s.From != "stg_orders" {
		t.Errorf("driving table = %q", s.From)
	}
	if len(s.Joins) != 2 {
		t.Fatalf("joins = %+v", s.Joins)
	}
	if s.Joins[0].Type != "left" || s.Joins[0].Target != "stg_payments" || s.Joins[0].Key != "order_id" {
		t.Errorf("first join = %+v", s.Joins[0])
	}
	// A backticked fully-qualified table is the one edge the manifest cannot
	// see, so failing to read it here means it is invisible everywhere.
	if s.Joins[1].Target != "shop-prod-1234.shop_raw.refunds" {
		t.Errorf("hardcoded join target = %q", s.Joins[1].Target)
	}
}

// A model that groups by position over a select star has a grain that depends
// on whatever columns upstream has today. That is not a gap in this scanner, it
// is a fact about the model — and guessing a grain would be worse than saying so.
func TestPositionalGroupByOverSelectStarIsUnresolvable(t *testing.T) {
	s := ReadShape(fctSQL, "")
	if s.Inferred != "" {
		t.Errorf("inferred grain = %q, want none — it cannot be known", s.Inferred)
	}
	if !strings.Contains(s.Unresolved, "select star") {
		t.Errorf("Unresolved = %q, want it to say why", s.Unresolved)
	}
	// And with a real column list, the positions resolve.
	ok := ReadShape("select customer_id, count(*) as n from {{ ref('stg_orders') }} group by 1", "")
	if ok.Inferred != "customer_id" || ok.Unresolved != "" {
		t.Errorf("inferred = %q unresolved = %q", ok.Inferred, ok.Unresolved)
	}
}

// The grain is usually stated in prose and never checked against the SQL. When
// they disagree the prose is what people trust, so the disagreement is the find.
func TestGrainDivergenceIsReportedButNotInvented(t *testing.T) {
	div := ReadShape(
		"select customer_id, count(*) as n from {{ ref('stg_orders') }} group by 1",
		"One row per order, with its total.")
	if !strings.Contains(div.GrainDivergence(), "one row per order") {
		t.Errorf("divergence = %q", div.GrainDivergence())
	}

	// customer vs customer_id is the same grain written by two hands. Reporting
	// it would bury the real ones.
	same := ReadShape(
		"select customer_id, count(*) as n from {{ ref('stg_orders') }} group by 1",
		"One row per customer, with their lifetime order behaviour.")
	if d := same.GrainDivergence(); d != "" {
		t.Errorf("false divergence: %s", d)
	}

	// A statement with no group by says nothing about grain, and a guess must
	// not be allowed to contradict a person.
	quiet := ReadShape("select * from {{ ref('stg_orders') }}", "One row per order.")
	if d := quiet.GrainDivergence(); d != "" {
		t.Errorf("a bare select cannot contradict the doc: %s", d)
	}
}

// "One row per order as the source system records it" states a grain of order.
// The rest is a sentence, and carrying it into the picture makes the grain
// unreadable at a glance.
func TestDeclaredGrainStopsAtTheGrain(t *testing.T) {
	for doc, want := range map[string]string{
		"One row per order as the source system records it.":                   "order",
		"Payments as the processor reports them, one row per capture attempt.": "capture attempt",
		"One row per customer, with their lifetime order behaviour.":           "customer",
		"Nothing about grain here.":                                            "",
	} {
		if got := declaredGrain(doc); got != want {
			t.Errorf("declaredGrain(%q) = %q, want %q", doc, got, want)
		}
	}
}

func TestFilterAndAggregatesAreRead(t *testing.T) {
	s := ReadShape("select id, sum(x) as total from {{ ref('a') }} where captured_at is not null group by 1", "")
	if s.Where != "captured_at is not null" {
		t.Errorf("where = %q", s.Where)
	}
	if len(s.Aggregates) != 1 || s.Aggregates[0] != "sum" {
		t.Errorf("aggregates = %v", s.Aggregates)
	}
}

// A commented-out join is not a join.
func TestCommentedSQLIsNotRead(t *testing.T) {
	s := ReadShape("select 1 from {{ ref('a') }}\n-- left join {{ ref('b') }} on b.id = a.id\n", "")
	if len(s.Joins) != 0 {
		t.Errorf("joins = %+v, want none", s.Joins)
	}
}

// The flow is the picture a warehouse actually needs, so the facts it carries
// have to survive the trip from the manifest and the statement into the graph.
func TestBuildFlowLayersTheDAGAndAnnotatesTheArrows(t *testing.T) {
	dir := writeRun(t)
	root := t.TempDir()
	write := func(rel, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("models/staging/stg_orders.sql", "-- One row per order.\nselect id as order_id from {{ source('shop_raw', 'orders') }}")
	write("models/marts/fct_orders.sql", fctSQL)

	m, r, err := LoadRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := BuildFlow(m, r, root, map[bundle.SymbolID]bool{"models/marts/fct_orders.sql::fct_orders": true}, nil)

	at := map[string]FlowNode{}
	for _, n := range f.Nodes {
		at[n.Name] = n
	}
	// Build order reads left to right: a table sits past everything it reads.
	if at["stg_orders"].Layer >= at["fct_orders"].Layer {
		t.Errorf("stg_orders layer %d, fct_orders layer %d — the arrow points backwards",
			at["stg_orders"].Layer, at["fct_orders"].Layer)
	}
	if !at["fct_orders"].Changed {
		t.Error("the changed model must be marked as changed")
	}
	// A test is a statement about a table, not a step in the build.
	if len(at["fct_orders"].Tests) != 1 || f.Failing != 1 {
		t.Errorf("tests = %+v, failing = %d", at["fct_orders"].Tests, f.Failing)
	}
	// A test is a query and is billed, so its bytes are part of what the run cost.
	if f.BytesScanned <= 19756742246 {
		t.Errorf("bytes = %d, want the test's scan counted too", f.BytesScanned)
	}

	var fromLink, outside *FlowLink
	for i, l := range f.Links {
		switch {
		case l.FromName == "stg_orders" && l.ToName == "fct_orders":
			fromLink = &f.Links[i]
		case !l.InDAG:
			outside = &f.Links[i]
		}
	}
	if fromLink == nil || fromLink.Relation != "from" {
		t.Errorf("driving-table link = %+v", fromLink)
	}
	// The table written straight into the SQL is the one edge dbt cannot show,
	// so it is exactly the one this has to draw.
	if outside == nil || outside.FromName != "shop-prod-1234.shop_raw.refunds" {
		t.Fatalf("the hardcoded table never made it onto the graph: %+v", f.Links)
	}
	if outside.Relation != "left" || outside.Key != "order_id" {
		t.Errorf("outside link = %+v", outside)
	}
	var reported bool
	for _, note := range f.Findings {
		if strings.Contains(note, "outside the DAG") {
			reported = true
		}
	}
	if !reported {
		t.Error("reading a table outside the DAG has to be said, not only drawn")
	}
}

// A model that did not rebuild is still upstream of one that did, so it belongs
// on the picture — marked as not rebuilt, not omitted.
func TestFlowKeepsModelsTheRunDidNotTouch(t *testing.T) {
	dir := writeRun(t)
	addNode(t, dir, "model.shop.dim_customers", map[string]any{
		"unique_id": "model.shop.dim_customers", "name": "dim_customers", "resource_type": "model",
		"original_file_path": "models/marts/dim_customers.sql",
		"depends_on":         map[string]any{"nodes": []string{"model.shop.stg_orders"}},
		"config":             map[string]any{"materialized": "table"},
	}, map[string]any{"unique_id": "model.shop.nothing", "status": "success"})

	m, r, err := LoadRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := BuildFlow(m, r, t.TempDir(), nil, nil)
	for _, n := range f.Nodes {
		if n.Name == "dim_customers" {
			if n.Status != "not-run" {
				t.Errorf("status = %q, want it marked as not rebuilt", n.Status)
			}
			return
		}
	}
	t.Error("a model the run skipped vanished from the DAG")
}
