package dbt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kelalaike/plum/internal/bundle"
)

// A warehouse build is not a call stack.
//
// The landscape draws code: a path that descends into a call and comes back
// with a value, and closure is the shape you read it by. SQL has no returns.
// fct_orders does not call stg_orders and get control back — the warehouse
// builds stg_orders, builds stg_payments, then builds fct_orders by reading
// both. Drawing that as a path invents resumes that never happened and buries
// the only structural fact that matters, which is that two tables met.
//
// So dbt gets its own picture: layered by dependency depth, data moving one
// way, every arrow carrying how the rows were matched and what came through.

type Flow struct {
	SessionID    string     `json:"session_id"`
	Nodes        []FlowNode `json:"nodes"`
	Links        []FlowLink `json:"links"`
	ElapsedNanos int64      `json:"elapsed_ns"`
	BytesScanned int64      `json:"bytes_scanned"`
	RowsWritten  int64      `json:"rows_written"`
	Failing      int        `json:"failing"`
	Findings     []string   `json:"findings,omitempty"`
}

type FlowNode struct {
	Symbol bundle.SymbolID `json:"symbol"`
	Name   string          `json:"name"`
	File   string          `json:"file"`
	// Layer is the longest path from a source, so everything a node reads is
	// strictly to its left. That is the build order, and it is why the picture
	// can be read without following arrows backwards.
	Layer        int    `json:"layer"`
	Kind         string `json:"kind"` // source | model
	Materialized string `json:"materialized,omitempty"`
	UniqueKey    string `json:"unique_key,omitempty"`
	Rows         int64  `json:"rows,omitempty"`
	Bytes        int64  `json:"bytes,omitempty"`
	Nanos        int64  `json:"nanos,omitempty"`
	Status       string `json:"status"` // success | fail | error | skipped | not-run
	Changed      bool   `json:"changed"`
	Doc          string `json:"doc,omitempty"`

	// Grain is what one row of this table means. GrainFrom says who says so:
	// "declared" is the author's prose, "inferred" is what the SQL does. The
	// distinction is the whole point — one is a promise, the other is evidence.
	Grain      string `json:"grain,omitempty"`
	GrainFrom  string `json:"grain_from,omitempty"`
	Unresolved string `json:"unresolved,omitempty"`

	Filter     string     `json:"filter,omitempty"`
	Aggregates []string   `json:"aggregates,omitempty"`
	Tests      []FlowTest `json:"tests,omitempty"`
	Risks      []string   `json:"risks,omitempty"`
}

type FlowTest struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Failures int    `json:"failures,omitempty"`
	Nanos    int64  `json:"nanos,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
}

// FlowLink is one table meeting another. How they were matched is the fact a
// call arrow cannot carry: an inner join drops rows that do not match, a left
// join keeps them, and a join on a non-unique key multiplies them.
type FlowLink struct {
	From     bundle.SymbolID `json:"from"`
	To       bundle.SymbolID `json:"to"`
	FromName string          `json:"from_name"`
	ToName   string          `json:"to_name"`
	// Relation is how the target reads this input:
	//   from   the driving table of the select
	//   inner | left | right | full | cross   the join that brought it in
	//   ref    declared in the DAG but not found in the statement
	Relation string `json:"relation"`
	Key      string `json:"key,omitempty"` // the matched column, when it is a plain equality
	On       string `json:"on,omitempty"`  // the condition as written
	Rows     int64  `json:"rows,omitempty"`
	// InDAG is false for a table written straight into the SQL. dbt does not
	// know about it, will not build it first, and will not show it in lineage —
	// so it is drawn, in the one place that can see it.
	InDAG bool `json:"in_dag"`
	// Note is what this edge does to the row count, in words.
	Note string `json:"note,omitempty"`
}

// BuildFlow reads a dbt run and the SQL behind it. root is the project
// directory, used to read the statements themselves — the run says what a query
// cost, only the statement says what it did.
func BuildFlow(m *Manifest, r *RunResults, root string, changed map[bundle.SymbolID]bool, risks map[bundle.SymbolID][]string) *Flow {
	byID := map[string]result{}
	for _, res := range r.Results {
		byID[res.UniqueID] = res
	}
	f := &Flow{ElapsedNanos: int64(r.ElapsedTime * float64(time.Second))}

	shapes := map[string]Shape{}
	index := map[string]bundle.SymbolID{} // model name → symbol

	// Nodes: every model and source the manifest knows, whether or not this run
	// touched it. A model that did not rebuild is still upstream of one that did.
	var ids []string
	for id, n := range m.Nodes {
		if n.ResourceType == "test" {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		n := m.Nodes[id]
		sym := m.symbolID(id)
		index[n.Name] = sym
		node := FlowNode{
			Symbol: sym, Name: displayName(id, n), File: n.OriginalFilePath,
			Kind: n.ResourceType, Materialized: n.Config.Materialized,
			UniqueKey: n.Config.UniqueKey, Doc: n.Description,
			Changed: changed[sym], Risks: risks[sym],
			Status: "not-run",
		}
		if res, ran := byID[id]; ran {
			node.Status = res.Status
			node.Rows = res.Adapter.RowsAffected
			node.Bytes = res.Adapter.BytesProcessed
			node.Nanos = int64(res.ExecutionTime * float64(time.Second))
			f.BytesScanned += node.Bytes
			f.RowsWritten += node.Rows
		}
		if n.ResourceType == "model" && n.OriginalFilePath != "" {
			if src, err := os.ReadFile(filepath.Join(root, n.OriginalFilePath)); err == nil {
				sh := ReadShape(string(src), n.Description+"\n"+leadingComment(string(src)))
				shapes[id] = sh
				node.Filter = sh.Where
				node.Aggregates = sh.Aggregates
				node.Unresolved = sh.Unresolved
				switch {
				case sh.Inferred != "":
					node.Grain, node.GrainFrom = sh.Inferred, "inferred"
				case sh.Declared != "":
					node.Grain, node.GrainFrom = sh.Declared, "declared"
				}
				if d := sh.GrainDivergence(); d != "" {
					f.Findings = append(f.Findings, n.Name+": "+d)
				}
				if sh.Unresolved != "" {
					f.Findings = append(f.Findings, n.Name+": "+sh.Unresolved)
				}
			}
		}
		f.Nodes = append(f.Nodes, node)
	}

	// Tests hang off the model they check. A test is a statement about a table,
	// not a step in the build, and drawing it as a frame was always a fiction.
	nodeAt := map[bundle.SymbolID]int{}
	for i, n := range f.Nodes {
		nodeAt[n.Symbol] = i
	}
	for id, n := range m.Nodes {
		if n.ResourceType != "test" {
			continue
		}
		res, ran := byID[id]
		if !ran {
			continue
		}
		t := FlowTest{Name: n.Name, Status: res.Status, Nanos: int64(res.ExecutionTime * float64(time.Second)), Bytes: res.Adapter.BytesProcessed}
		if res.Failures != nil {
			t.Failures = *res.Failures
		}
		if t.Status == "fail" || t.Status == "error" {
			f.Failing++
		}
		// A test is a query. It scans billed bytes like any other, so leaving it
		// out of the total would understate what the run cost.
		f.BytesScanned += t.Bytes
		for _, dep := range n.DependsOn.Nodes {
			if i, ok := nodeAt[m.symbolID(dep)]; ok {
				f.Nodes[i].Tests = append(f.Nodes[i].Tests, t)
			}
		}
	}
	for i := range f.Nodes {
		sort.Slice(f.Nodes[i].Tests, func(a, b int) bool { return f.Nodes[i].Tests[a].Name < f.Nodes[i].Tests[b].Name })
	}

	// Links: the DAG dbt declares, annotated with what the statement does. A
	// dependency the statement does not mention is still drawn — it is real —
	// but it is marked as declared rather than read.
	for _, id := range ids {
		n := m.Nodes[id]
		to := m.symbolID(id)
		sh := shapes[id]
		matched := map[string]bool{}
		for _, dep := range n.DependsOn.Nodes {
			depNode, ok := m.Nodes[dep]
			if !ok {
				continue
			}
			from := m.symbolID(dep)
			link := FlowLink{
				From: from, To: to, FromName: displayName(dep, depNode), ToName: n.Name,
				Relation: "ref", InDAG: true, Rows: byID[dep].Adapter.RowsAffected,
			}
			switch {
			case sh.From != "" && sameTarget(sh.From, depNode.Name):
				link.Relation = "from"
				link.Note = "the driving table"
				matched[sh.From] = true
			default:
				for _, j := range sh.Joins {
					if sameTarget(j.Target, depNode.Name) {
						link.Relation, link.Key, link.On = j.Type, j.Key, j.On
						link.Note = joinNote(j)
						matched[j.Target] = true
						break
					}
				}
			}
			f.Links = append(f.Links, link)
		}
		// A table written straight into the SQL is read by this model and is
		// invisible to dbt. It is the one edge the manifest cannot tell you
		// about, so it is exactly the one worth drawing.
		for _, j := range sh.Joins {
			if matched[j.Target] || index[j.Target] != "" || !strings.Contains(j.Target, ".") {
				continue
			}
			outside := bundle.SymbolID("::" + j.Target)
			if _, seen := nodeAt[outside]; !seen {
				nodeAt[outside] = len(f.Nodes)
				f.Nodes = append(f.Nodes, FlowNode{
					Symbol: outside, Name: j.Target, Kind: "outside", Status: "not-run",
					Risks: []string{"written into the SQL, so dbt will not build it first or track it in lineage"},
				})
			}
			f.Links = append(f.Links, FlowLink{
				From: outside, To: to, FromName: j.Target, ToName: n.Name,
				Relation: j.Type, Key: j.Key, On: j.On, Note: joinNote(j), InDAG: false,
			})
			f.Findings = append(f.Findings, n.Name+": reads "+j.Target+" directly, outside the DAG")
		}
	}

	// Layer is the longest path from a node with nothing upstream, so an arrow
	// never points backwards and the build order reads left to right.
	inbound := map[bundle.SymbolID][]bundle.SymbolID{}
	for _, l := range f.Links {
		inbound[l.To] = append(inbound[l.To], l.From)
	}
	var depth func(bundle.SymbolID, map[bundle.SymbolID]bool) int
	depth = func(s bundle.SymbolID, seen map[bundle.SymbolID]bool) int {
		if seen[s] {
			return 0 // a cycle dbt would have rejected; do not spin on it
		}
		seen[s] = true
		defer delete(seen, s)
		best := 0
		for _, up := range inbound[s] {
			if d := depth(up, seen) + 1; d > best {
				best = d
			}
		}
		return best
	}
	for i := range f.Nodes {
		f.Nodes[i].Layer = depth(f.Nodes[i].Symbol, map[bundle.SymbolID]bool{})
	}
	sort.SliceStable(f.Nodes, func(a, b int) bool {
		if f.Nodes[a].Layer != f.Nodes[b].Layer {
			return f.Nodes[a].Layer < f.Nodes[b].Layer
		}
		return f.Nodes[a].Name < f.Nodes[b].Name
	})
	sort.Strings(f.Findings)
	return f
}

// displayName is what to call a node on the picture. A source is written into
// the SQL as schema.table, and naming it by the table alone drops the half that
// says which system it came out of.
func displayName(id string, n node) string {
	if n.ResourceType != "source" {
		return n.Name
	}
	parts := strings.Split(id, ".")
	if len(parts) >= 2 {
		return strings.Join(parts[len(parts)-2:], ".")
	}
	return n.Name
}

// joinNote says what the join does to the row count, which is the question
// somebody reading a mart actually has.
func joinNote(j Join) string {
	switch j.Type {
	case "inner":
		return "rows without a match on both sides are dropped"
	case "left":
		return "unmatched rows are kept, and rows multiply if the key repeats"
	case "right":
		return "rows on this side are kept, the other side's unmatched rows are dropped"
	case "full":
		return "both sides are kept, unmatched columns come back null"
	case "cross":
		return "every row meets every row"
	}
	return ""
}

// sameTarget compares a name written in SQL with a name from the manifest. A
// source is written shop_raw.orders and named orders.
func sameTarget(sqlName, nodeName string) bool {
	if strings.EqualFold(sqlName, nodeName) {
		return true
	}
	return strings.EqualFold(lastSegmentSQL(sqlName), nodeName)
}

func (f *Flow) Save(path string) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func LoadFlow(path string) (*Flow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Flow
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parsing flow: %w", err)
	}
	return &f, nil
}
