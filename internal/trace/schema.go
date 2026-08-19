// Package trace defines the shim contract and the landscape derivation.
// Shims are separate processes speaking JSONL; the core orchestrates and never
// absorbs them (spec §4.2). Treat Event as a public API.
package trace

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"sort"

	"github.com/kelalaike/plum/internal/bundle"
)

const SchemaVersion = "1.0"

type Event struct {
	SchemaVersion string            `json:"schema_version"`
	Kind          string            `json:"event"` // call | return | raise
	Symbol        bundle.SymbolID   `json:"symbol_id"`
	InvocationID  string            `json:"invocation_id"`
	ParentID      string            `json:"parent_invocation_id"`
	TSNanos       int64             `json:"ts_ns"`
	Depth         int               `json:"depth"`
	Args          map[string]string `json:"args"`
	Result        string            `json:"result"`
	Exception     string            `json:"exception"`
	TestID        string            `json:"test_id"`
}

// ShimSpec tells the collector how to instrument one language for a given
// symbol set. Only symbols present in Bundle.Symbols are ever instrumented —
// the AST pass determines the instrumentation set (spec §4.2).
type ShimSpec struct {
	Language string
	// Mode is how the shim attaches: "rewrite" (source instrumentation in a
	// scratch copy), "env" (preload/require hook), or "none".
	Mode    string
	Env     map[string]string
	Command []string
	Symbols []bundle.SymbolID
}

// ReadJSONL ingests a shim's event stream. Malformed lines are skipped rather
// than fatal: a partially written trace is still evidence.
func ReadJSONL(r io.Reader) ([]Event, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
	var out []Event
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

func ReadFile(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadJSONL(f)
}

func WriteFile(path string, events []Event) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, e := range events {
		if e.SchemaVersion == "" {
			e.SchemaVersion = SchemaVersion
		}
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return w.Flush()
}

// SortByTime orders events as they were observed. Shims may interleave writes
// from several goroutines, so the file order is not authoritative.
func SortByTime(ev []Event) {
	sort.SliceStable(ev, func(i, j int) bool { return ev[i].TSNanos < ev[j].TSNanos })
}

// For returns up to max recorded invocations of a symbol, call events first.
func For(ev []Event, sym bundle.SymbolID, max int) []Event {
	var out []Event
	for _, e := range ev {
		if e.Symbol == sym {
			out = append(out, e)
			if len(out) >= max {
				break
			}
		}
	}
	return out
}
