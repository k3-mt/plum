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
	"github.com/kelalaike/plum/internal/trace"
)

// dbt records its own execution, so there is nothing to instrument. This reads
// what a run already wrote and turns it into the same event stream the language
// shims emit, which is what lets a warehouse build draw as a landscape.
//
// It never triggers a run. In a warehouse every execution scans bytes and every
// byte is billed, so a tool that re-runs your project to look at it is a tool
// that shows up on the invoice.

// Manifest is the slice of target/manifest.json this needs.
type Manifest struct {
	Nodes   map[string]node `json:"nodes"`
	Sources map[string]node `json:"sources"`
}

type node struct {
	UniqueID         string     `json:"unique_id"`
	Name             string     `json:"name"`
	ResourceType     string     `json:"resource_type"`
	OriginalFilePath string     `json:"original_file_path"`
	Description      string     `json:"description"`
	DependsOn        dependsOn  `json:"depends_on"`
	Config           nodeConfig `json:"config"`
}

type dependsOn struct {
	Nodes []string `json:"nodes"`
}

type nodeConfig struct {
	Materialized string `json:"materialized"`
	UniqueKey    string `json:"unique_key"`
}

// RunResults is the slice of target/run_results.json this needs.
type RunResults struct {
	Metadata struct {
		GeneratedAt  string `json:"generated_at"`
		InvocationID string `json:"invocation_id"`
	} `json:"metadata"`
	Results     []result `json:"results"`
	ElapsedTime float64  `json:"elapsed_time"`
}

type result struct {
	UniqueID      string  `json:"unique_id"`
	Status        string  `json:"status"`
	ExecutionTime float64 `json:"execution_time"`
	Message       string  `json:"message"`
	Failures      *int    `json:"failures"`
	Adapter       struct {
		RowsAffected   int64  `json:"rows_affected"`
		BytesProcessed int64  `json:"bytes_processed"`
		BytesBilled    int64  `json:"bytes_billed"`
		SlotMS         int64  `json:"slot_ms"`
		JobID          string `json:"job_id"`
	} `json:"adapter_response"`
}

// LoadRun reads a dbt target directory.
func LoadRun(targetDir string) (*Manifest, *RunResults, error) {
	var m Manifest
	data, err := os.ReadFile(filepath.Join(targetDir, "manifest.json"))
	if err != nil {
		return nil, nil, fmt.Errorf("reading manifest: %w (run `dbt compile` or `dbt build` first)", err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, nil, fmt.Errorf("parsing manifest: %w", err)
	}

	var r RunResults
	data, err = os.ReadFile(filepath.Join(targetDir, "run_results.json"))
	if err != nil {
		return &m, nil, fmt.Errorf("reading run results: %w (nothing has been built yet)", err)
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return &m, nil, fmt.Errorf("parsing run results: %w", err)
	}
	return &m, &r, nil
}

// symbolID resolves a dbt unique_id to the SymbolID the rest of the tool uses.
func (m *Manifest) symbolID(uniqueID string) bundle.SymbolID {
	n, ok := m.Nodes[uniqueID]
	if !ok {
		if n, ok = m.Sources[uniqueID]; !ok {
			return bundle.SymbolID("::" + uniqueID)
		}
	}
	if n.OriginalFilePath == "" || n.Name == "" {
		return bundle.SymbolID("::" + uniqueID)
	}
	return bundle.MakeID(filepath.ToSlash(n.OriginalFilePath), n.Name)
}

// Events turns one dbt run into the event stream the landscape is derived from.
//
// A build is not a call stack, but the lineage of a model is a tree, and that is
// what gets walked: entering a node means its upstream had to be built first,
// and returning means it finished. Depth is how far up the lineage you are
// rather than how deep the call stack is, which is the honest reading of what
// dbt actually did.
func Events(m *Manifest, r *RunResults) []trace.Event {
	byID := map[string]result{}
	for _, res := range r.Results {
		byID[res.UniqueID] = res
	}

	// Tests are not frames. A test is a statement about a model, so it is
	// recorded against the model it covers — which is what makes "which tests
	// reach this change" mean something in a warehouse.
	testsFor := map[string][]result{}
	var buildable []string
	for id, res := range byID {
		n := m.Nodes[id]
		if n.ResourceType == "test" {
			for _, dep := range n.DependsOn.Nodes {
				testsFor[dep] = append(testsFor[dep], res)
			}
			continue
		}
		buildable = append(buildable, id)
	}
	sort.Strings(buildable)

	// A root is a node nothing else in this run depends on: the tip of a
	// lineage, and the thing somebody actually asked for.
	depended := map[string]bool{}
	for _, id := range buildable {
		for _, dep := range m.Nodes[id].DependsOn.Nodes {
			if _, inRun := byID[dep]; inRun {
				depended[dep] = true
			}
		}
	}
	var roots []string
	for _, id := range buildable {
		if !depended[id] {
			roots = append(roots, id)
		}
	}
	sort.Strings(roots)

	var events []trace.Event
	clock := int64(0)
	counter := 0

	var walk func(id string, depth int, parent string, testID string)
	walk = func(id string, depth int, parent string, testID string) {
		res, ran := byID[id]
		n := m.Nodes[id]
		counter++
		invocation := fmt.Sprintf("%s-%d", r.Metadata.InvocationID, counter)
		sym := m.symbolID(id)

		args := map[string]string{}
		if n.Config.Materialized != "" {
			args["materialized"] = n.Config.Materialized
		}
		if n.Config.UniqueKey != "" {
			args["unique_key"] = n.Config.UniqueKey
		}
		if !ran {
			args["not in this run"] = "true"
		}

		clock += int64(time.Millisecond)
		events = append(events, trace.Event{
			SchemaVersion: trace.SchemaVersion,
			Kind:          "call", Symbol: sym, InvocationID: invocation,
			ParentID: parent, Depth: depth, TSNanos: clock, Args: args, TestID: testID,
		})

		// Upstream first: a model cannot be built before what it selects from.
		deps := append([]string(nil), n.DependsOn.Nodes...)
		sort.Strings(deps)
		for _, dep := range deps {
			if _, inRun := byID[dep]; !inRun {
				continue
			}
			if m.Nodes[dep].ResourceType == "test" {
				continue
			}
			walk(dep, depth+1, invocation, testID)
		}

		// dbt builds a node and then runs the tests attached to it, so the tests
		// belong inside the node's frame: whether this model came out right is
		// part of building it, and a failing test is what makes the answer
		// untrustworthy rather than a separate event afterwards.
		for _, test := range testsFor[id] {
			counter++
			clock += int64(time.Millisecond)
			testInvocation := fmt.Sprintf("%s-t%d", r.Metadata.InvocationID, counter)
			testSym := m.symbolID(test.UniqueID)
			events = append(events, trace.Event{
				SchemaVersion: trace.SchemaVersion, Kind: "call", Symbol: testSym,
				InvocationID: testInvocation, ParentID: invocation, Depth: depth + 1,
				TSNanos: clock, TestID: testID,
				Args: map[string]string{"tests": n.Name},
			})
			clock += int64(test.ExecutionTime * float64(time.Second))
			if test.Status == "fail" || test.Status == "error" {
				events = append(events, trace.Event{
					SchemaVersion: trace.SchemaVersion, Kind: "raise", Symbol: testSym,
					InvocationID: testInvocation, ParentID: invocation, Depth: depth + 1,
					TSNanos: clock, Exception: testFailure(m, test), TestID: testID,
				})
				continue
			}
			events = append(events, trace.Event{
				SchemaVersion: trace.SchemaVersion, Kind: "return", Symbol: testSym,
				InvocationID: testInvocation, Depth: depth + 1, TSNanos: clock,
				Result: "passed" + scanned(test), TestID: testID,
			})
		}

		// The node's own execution time is what the transition back up costs.
		clock += int64(res.ExecutionTime * float64(time.Second))
		if res.ExecutionTime == 0 {
			clock += int64(time.Millisecond)
		}

		switch {
		case !ran:
			events = append(events, trace.Event{
				SchemaVersion: trace.SchemaVersion, Kind: "return", Symbol: sym,
				InvocationID: invocation, Depth: depth, TSNanos: clock,
				Result: "not selected in this run", TestID: testID,
			})
		case res.Status == "error" || res.Status == "fail" || res.Status == "runtime error":
			events = append(events, trace.Event{
				SchemaVersion: trace.SchemaVersion, Kind: "raise", Symbol: sym,
				InvocationID: invocation, Depth: depth, TSNanos: clock,
				Exception: failureText(res), TestID: testID,
			})
		case res.Status == "skipped":
			events = append(events, trace.Event{
				SchemaVersion: trace.SchemaVersion, Kind: "return", Symbol: sym,
				InvocationID: invocation, Depth: depth, TSNanos: clock,
				Result: "skipped — an upstream node failed", TestID: testID,
			})
		default:
			events = append(events, trace.Event{
				SchemaVersion: trace.SchemaVersion, Kind: "return", Symbol: sym,
				InvocationID: invocation, Depth: depth, TSNanos: clock,
				Result: outcome(res), TestID: testID,
			})
		}
	}

	for _, root := range roots {
		walk(root, 0, "", "build "+m.Nodes[root].Name)
	}
	return events
}

// scanned reports what a test cost to run, since a test in a warehouse is a
// query and queries are billed.
func scanned(res result) string {
	if res.Adapter.BytesProcessed == 0 {
		return ""
	}
	return " (" + humanBytes(res.Adapter.BytesProcessed) + " scanned)"
}

func outcome(res result) string {
	var parts []string
	if res.Adapter.RowsAffected > 0 {
		parts = append(parts, fmt.Sprintf("%s rows", commas(res.Adapter.RowsAffected)))
	}
	if res.Adapter.BytesProcessed > 0 {
		parts = append(parts, humanBytes(res.Adapter.BytesProcessed)+" scanned")
	}
	if res.Adapter.BytesBilled > 0 && res.Adapter.BytesBilled != res.Adapter.BytesProcessed {
		parts = append(parts, humanBytes(res.Adapter.BytesBilled)+" billed")
	}
	if res.Adapter.SlotMS > 0 {
		parts = append(parts, fmt.Sprintf("%s slot-ms", commas(res.Adapter.SlotMS)))
	}
	if len(parts) == 0 {
		return res.Status
	}
	return strings.Join(parts, ", ")
}

func failureText(res result) string {
	if res.Message != "" {
		return strings.Join(strings.Fields(res.Message), " ")
	}
	return res.Status
}

func testFailure(m *Manifest, res result) string {
	name := m.Nodes[res.UniqueID].Name
	if name == "" {
		name = res.UniqueID
	}
	if res.Failures != nil {
		return fmt.Sprintf("test %s failed on %d rows", name, *res.Failures)
	}
	return "test " + name + " failed"
}

// Coverage maps each model to the dbt tests that cover it. In a warehouse this
// is what "tested" actually means, and it is declared rather than inferred.
func Coverage(m *Manifest, r *RunResults) map[bundle.SymbolID][]string {
	out := map[bundle.SymbolID][]string{}
	for _, res := range r.Results {
		n, ok := m.Nodes[res.UniqueID]
		if !ok || n.ResourceType != "test" {
			continue
		}
		for _, dep := range n.DependsOn.Nodes {
			sym := m.symbolID(dep)
			label := n.Name
			if res.Status == "fail" || res.Status == "error" {
				label += " (failing)"
			}
			out[sym] = append(out[sym], label)
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func commas(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
