package dbt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/trace"
)

func writeRun(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	manifest := map[string]any{
		"nodes": map[string]any{
			"model.shop.stg_orders": map[string]any{
				"unique_id": "model.shop.stg_orders", "name": "stg_orders", "resource_type": "model",
				"original_file_path": "models/staging/stg_orders.sql",
				"depends_on":         map[string]any{"nodes": []string{}},
				"config":             map[string]any{"materialized": "view"},
			},
			"model.shop.fct_orders": map[string]any{
				"unique_id": "model.shop.fct_orders", "name": "fct_orders", "resource_type": "model",
				"original_file_path": "models/marts/fct_orders.sql",
				"depends_on":         map[string]any{"nodes": []string{"model.shop.stg_orders"}},
				"config":             map[string]any{"materialized": "incremental", "unique_key": "order_id"},
			},
			"test.shop.unique_fct_orders_order_id": map[string]any{
				"unique_id": "test.shop.unique_fct_orders_order_id", "name": "unique_fct_orders_order_id",
				"resource_type": "test", "original_file_path": "models/marts/schema.yml",
				"depends_on": map[string]any{"nodes": []string{"model.shop.fct_orders"}},
			},
		},
	}
	failures := 1204
	results := map[string]any{
		"metadata": map[string]any{"invocation_id": "inv-1"},
		"results": []any{
			map[string]any{"unique_id": "model.shop.stg_orders", "status": "success", "execution_time": 4.0,
				"adapter_response": map[string]any{"rows_affected": 2481003, "bytes_processed": 1181116006}},
			map[string]any{"unique_id": "model.shop.fct_orders", "status": "success", "execution_time": 40.0,
				"adapter_response": map[string]any{"rows_affected": 2481003, "bytes_processed": 19756742246, "slot_ms": 93200}},
			map[string]any{"unique_id": "test.shop.unique_fct_orders_order_id", "status": "fail",
				"execution_time": 6.0, "failures": failures,
				"adapter_response": map[string]any{"bytes_processed": 2362232012}},
		},
		"elapsed_time": 50.0,
	}
	for name, doc := range map[string]any{"manifest.json": manifest, "run_results.json": results} {
		data, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A dbt unique_id has to become the same SymbolID the adapter produces from the
// file, or the run and the code describe two different projects.
func TestRunResolvesToTheSameSymbolIDsAsTheSource(t *testing.T) {
	m, r, err := LoadRun(writeRun(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.symbolID("model.shop.fct_orders"); got != "models/marts/fct_orders.sql::fct_orders" {
		t.Errorf("symbol id = %q", got)
	}
	if got := m.symbolID("model.shop.unknown"); !strings.HasPrefix(string(got), "::") {
		t.Errorf("an unknown node should stay unresolved, got %q", got)
	}
	if len(r.Results) != 3 {
		t.Fatalf("results = %d", len(r.Results))
	}
}

// A model cannot be built before what it selects from, so the lineage is walked
// upstream-first and the landscape reads as a descent.
func TestEventsWalkLineageUpstreamFirst(t *testing.T) {
	m, r, err := LoadRun(writeRun(t))
	if err != nil {
		t.Fatal(err)
	}
	events := Events(m, r)
	if len(events) == 0 {
		t.Fatal("no events")
	}
	if events[0].Kind != "call" || events[0].Symbol != "models/marts/fct_orders.sql::fct_orders" {
		t.Fatalf("first event = %+v, want the root of the lineage", events[0])
	}
	if events[0].Depth != 0 {
		t.Errorf("root depth = %d", events[0].Depth)
	}

	var sawUpstream bool
	for _, e := range events {
		if e.Kind == "call" && e.Symbol == "models/staging/stg_orders.sql::stg_orders" {
			sawUpstream = true
			if e.Depth != 1 {
				t.Errorf("upstream depth = %d, want 1 — it is one step up the lineage", e.Depth)
			}
		}
	}
	if !sawUpstream {
		t.Error("the upstream model never appeared")
	}

	// Every build carries what it actually cost, because in a warehouse that is
	// the number people are asked about.
	for _, e := range events {
		if e.Kind == "return" && e.Symbol == "models/marts/fct_orders.sql::fct_orders" {
			for _, want := range []string{"2,481,003 rows", "GB scanned", "slot-ms"} {
				if !strings.Contains(e.Result, want) {
					t.Errorf("result %q is missing %q", e.Result, want)
				}
			}
		}
	}
}

// dbt builds a node and then tests it, so a test belongs inside that node's
// frame — and a failing test has to unwind, or the landscape shows a clean
// build of a table nobody should trust.
func TestAFailingTestUnwindsIntoTheModelItChecks(t *testing.T) {
	m, r, err := LoadRun(writeRun(t))
	if err != nil {
		t.Fatal(err)
	}
	events := Events(m, r)

	var testCall, testRaise, modelReturn int
	for i, e := range events {
		switch {
		case e.Kind == "call" && strings.Contains(string(e.Symbol), "unique_fct_orders"):
			testCall = i
		case e.Kind == "raise" && strings.Contains(string(e.Symbol), "unique_fct_orders"):
			testRaise = i
			if !strings.Contains(e.Exception, "1204 rows") {
				t.Errorf("failure text = %q, want the row count dbt reported", e.Exception)
			}
		case e.Kind == "return" && e.Symbol == "models/marts/fct_orders.sql::fct_orders":
			modelReturn = i
		}
	}
	if testCall == 0 || testRaise == 0 {
		t.Fatal("the failing test produced no frame")
	}
	if !(testCall < testRaise && testRaise < modelReturn) {
		t.Errorf("ordering call=%d raise=%d modelReturn=%d — the test must run inside the model's frame",
			testCall, testRaise, modelReturn)
	}

	// And it has to survive derivation into a visible cliff.
	b := &bundle.Bundle{Session: bundle.Session{ID: "s"}, Symbols: []bundle.Symbol{
		{ID: "models/marts/fct_orders.sql::fct_orders", Name: "fct_orders", Kind: "model"},
	}}
	l := trace.Derive(events, b)
	var unwinds int
	for _, bar := range l.Barriers {
		if bar.Direction == "unwind" {
			unwinds++
		}
	}
	if unwinds != 1 {
		t.Errorf("got %d unwinds, want the failing test to draw as one cliff", unwinds)
	}
}

// In a warehouse "tested" is declared, not inferred: dbt says which tests cover
// which model, and a failing one is named as failing.
func TestCoverageNamesTheTestsThatCoverEachModel(t *testing.T) {
	m, r, err := LoadRun(writeRun(t))
	if err != nil {
		t.Fatal(err)
	}
	cov := Coverage(m, r)
	tests := cov["models/marts/fct_orders.sql::fct_orders"]
	if len(tests) != 1 {
		t.Fatalf("coverage = %v", cov)
	}
	if !strings.Contains(tests[0], "unique_fct_orders_order_id") || !strings.Contains(tests[0], "failing") {
		t.Errorf("test label = %q, want it named and marked as failing", tests[0])
	}
}

func TestMissingArtifactsSayWhatToDo(t *testing.T) {
	_, _, err := LoadRun(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "dbt compile") {
		t.Errorf("error = %v, want it to say how to produce the manifest", err)
	}
}

// "What did this need?" and "what does this hit?" are opposite walks, and the
// second is the question somebody editing a staging model actually has. A
// descent from a root cannot show it.
func TestDownstreamWalkShowsTheFanOut(t *testing.T) {
	dir := writeRun(t)
	// A second mart that reads the same staging model: now stg_orders fans out.
	addNode(t, dir, "model.shop.dim_customers", map[string]any{
		"unique_id": "model.shop.dim_customers", "name": "dim_customers", "resource_type": "model",
		"original_file_path": "models/marts/dim_customers.sql",
		"depends_on":         map[string]any{"nodes": []string{"model.shop.stg_orders"}},
		"config":             map[string]any{"materialized": "table"},
	}, map[string]any{
		"unique_id": "model.shop.dim_customers", "status": "success", "execution_time": 22.0,
		"adapter_response": map[string]any{"rows_affected": 418772, "bytes_processed": 10307921510},
	})

	m, r, err := LoadRun(dir)
	if err != nil {
		t.Fatal(err)
	}

	events, err := EventsFrom(m, r, "stg_orders", Downstream)
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Symbol != "models/staging/stg_orders.sql::stg_orders" || events[0].Depth != 0 {
		t.Fatalf("the walk must start at the named model: %+v", events[0])
	}
	if events[0].TestID != "impact of stg_orders" {
		t.Errorf("chain label = %q", events[0].TestID)
	}

	// Both consumers must appear, each one step down from the shared model.
	consumers := map[bundle.SymbolID]int{}
	for _, e := range events {
		if e.Kind == "call" && e.Depth == 1 {
			consumers[e.Symbol]++
		}
	}
	for _, want := range []bundle.SymbolID{
		"models/marts/fct_orders.sql::fct_orders",
		"models/marts/dim_customers.sql::dim_customers",
	} {
		if consumers[want] != 1 {
			t.Errorf("%s appeared %d times at depth 1; the fan is not drawn", want, consumers[want])
		}
	}

	// And the upstream walk from the same node must show nothing below it,
	// because staging selects from a source rather than from another model.
	up, err := EventsFrom(m, r, "stg_orders", Upstream)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range up {
		if e.Depth > 0 {
			t.Errorf("walking upstream from staging found %s below it", e.Symbol)
		}
	}
}

// A DAG can reach the same node by two paths, and a cycle would not terminate.
func TestDownstreamWalkTerminates(t *testing.T) {
	dir := writeRun(t)
	addNode(t, dir, "model.shop.loop_a", map[string]any{
		"unique_id": "model.shop.loop_a", "name": "loop_a", "resource_type": "model",
		"original_file_path": "models/loop_a.sql",
		"depends_on":         map[string]any{"nodes": []string{"model.shop.loop_b"}},
		"config":             map[string]any{},
	}, map[string]any{"unique_id": "model.shop.loop_a", "status": "success", "execution_time": 1.0,
		"adapter_response": map[string]any{}})
	addNode(t, dir, "model.shop.loop_b", map[string]any{
		"unique_id": "model.shop.loop_b", "name": "loop_b", "resource_type": "model",
		"original_file_path": "models/loop_b.sql",
		"depends_on":         map[string]any{"nodes": []string{"model.shop.loop_a"}},
		"config":             map[string]any{},
	}, map[string]any{"unique_id": "model.shop.loop_b", "status": "success", "execution_time": 1.0,
		"adapter_response": map[string]any{}})

	m, r, err := LoadRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		events, _ := EventsFrom(m, r, "loop_a", Downstream)
		done <- len(events)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a cycle in the DAG did not terminate")
	}
}

func TestUnknownModelSaysSo(t *testing.T) {
	m, r, err := LoadRun(writeRun(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EventsFrom(m, r, "not_a_model", Downstream); err == nil ||
		!strings.Contains(err.Error(), "no model named") {
		t.Errorf("error = %v", err)
	}
}

// addNode splices one node and its result into the artifacts on disk.
func addNode(t *testing.T, dir, id string, node, result map[string]any) {
	t.Helper()
	for name, key := range map[string]string{"manifest.json": "nodes", "run_results.json": "results"} {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		if key == "nodes" {
			doc["nodes"].(map[string]any)[id] = node
		} else {
			doc["results"] = append(doc["results"].([]any), result)
		}
		out, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Walking one line of lineage hides the others. fct_orders selects two staging
// models; a walk that arrives from one of them must still say the other exists,
// or the picture reads as the whole story when it is one branch of it.
func TestDownstreamWalkNamesTheOtherInflow(t *testing.T) {
	dir := writeRun(t)
	addNode(t, dir, "model.shop.stg_payments", map[string]any{
		"unique_id": "model.shop.stg_payments", "name": "stg_payments", "resource_type": "model",
		"original_file_path": "models/staging/stg_payments.sql",
		"depends_on":         map[string]any{"nodes": []string{}},
		"config":             map[string]any{"materialized": "view"},
	}, map[string]any{
		"unique_id": "model.shop.stg_payments", "status": "success", "execution_time": 3.0,
		"adapter_response": map[string]any{"rows_affected": 900000},
	})
	// The mart now selects both staging models; the walk still arrives from one.
	addNode(t, dir, "model.shop.fct_orders", map[string]any{
		"unique_id": "model.shop.fct_orders", "name": "fct_orders", "resource_type": "model",
		"original_file_path": "models/marts/fct_orders.sql",
		"depends_on": map[string]any{"nodes": []string{
			"model.shop.stg_orders", "model.shop.stg_payments"}},
		"config": map[string]any{"materialized": "incremental", "unique_key": "order_id"},
	}, map[string]any{
		"unique_id": "model.shop.fct_orders", "status": "success", "execution_time": 40.0,
		"adapter_response": map[string]any{"rows_affected": 2481003},
	})

	m, r, err := LoadRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := EventsFrom(m, r, "stg_orders", Downstream)
	if err != nil {
		t.Fatal(err)
	}
	var fct *trace.Event
	for i, e := range events {
		if e.Kind == "call" && e.Symbol == "models/marts/fct_orders.sql::fct_orders" {
			fct = &events[i]
			break
		}
	}
	if fct == nil {
		t.Fatal("no fct_orders frame")
	}
	var in []string
	for _, j := range fct.Joins {
		if j.Dir == "in" {
			in = append(in, string(j.Symbol))
		}
	}
	want := "models/staging/stg_payments.sql::stg_payments"
	if !slicesContains(in, want) {
		t.Errorf("inflows = %v, want %s among them", in, want)
	}
	// Cost travels with the join: an inflow you cannot see is still on the bill.
	for _, j := range fct.Joins {
		if string(j.Symbol) == want && j.Nanos <= 0 {
			t.Error("stg_payments ran in this run, so its cost is known and belongs on the join")
		}
	}
}

// Every root walked its own chain from a zero clock and a zero counter, so two
// chains minted the same invocation IDs at the same timestamps — which is one
// chain as far as the landscape can tell, and it spliced them.
func TestChainsDoNotShareInvocationIDs(t *testing.T) {
	dir := writeRun(t)
	addNode(t, dir, "model.shop.dim_customers", map[string]any{
		"unique_id": "model.shop.dim_customers", "name": "dim_customers", "resource_type": "model",
		"original_file_path": "models/marts/dim_customers.sql",
		"depends_on":         map[string]any{"nodes": []string{"model.shop.stg_orders"}},
		"config":             map[string]any{"materialized": "table"},
	}, map[string]any{
		"unique_id": "model.shop.dim_customers", "status": "success", "execution_time": 22.0,
		"adapter_response": map[string]any{"rows_affected": 418772},
	})
	m, r, err := LoadRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	events := Events(m, r)
	seen := map[string]bool{}
	for _, e := range events {
		if e.Kind != "call" {
			continue
		}
		if seen[e.InvocationID] {
			t.Fatalf("invocation %s minted twice; chains will splice", e.InvocationID)
		}
		seen[e.InvocationID] = true
	}
}

func slicesContains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
