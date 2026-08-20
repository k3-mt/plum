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
//
// It is declarative on purpose. Everything an "env" shim needs — the files to
// drop into the scratch copy and the environment that attaches them — travels
// in this struct, so the collector can honour a new language without knowing
// anything about it. That is what makes the language seam real rather than
// decorative: adding Ruby means writing an adapter, not editing the engine.
type ShimSpec struct {
	Language string
	// Mode is how the shim attaches:
	//   env      write Files into Dir and set Env for the test command
	//   rewrite  the adapter rewrites the scratch copy itself (see Rewriter)
	//   none     nothing to instrument (configuration, for instance)
	Mode    string
	Symbols []bundle.SymbolID

	// Dir is the scratch subdirectory Files are written into, relative to the
	// scratch root. Its absolute path substitutes for ${SHIM_DIR} in Env.
	Dir string
	// Files maps a name inside Dir to its content.
	Files map[string]string
	// Env is set for the test command. Values may contain ${SHIM_DIR},
	// ${SYMBOLS} for the deep instrumentation set, and ${CONTEXT_SYMBOLS} for
	// the surrounding code recorded for structure only.
	Env map[string]string
	// PathVars are environment variables with path-list semantics (PYTHONPATH,
	// NODE_PATH) that Dir is prepended to, preserving any existing value.
	PathVars []string
}

// Instrumenter is the slice of a language adapter the collector needs. Defined
// here rather than imported from internal/lang so the dependency runs one way:
// adapters know about traces, the tracer does not know about adapters.
type Instrumenter interface {
	Name() string
	Extensions() []string
	ShimSpec(syms []bundle.SymbolID) (ShimSpec, error)
}

// Rewriter is implemented by adapters whose instrumentation cannot be expressed
// as files plus environment — Go, where a probe is injected into the source of
// a scratch copy. The rewriting itself lives in the adapter, next to the parser
// that understands the language.
type Rewriter interface {
	// Instrument rewrites files under scratchRoot in place, returning what it
	// managed to instrument and what it had to skip.
	//
	// ids is the changed set, recorded in full: arguments, returns, exceptions.
	// context is the surrounding code the test may walk through, recorded for
	// structure only — entering and leaving, nothing captured. Capturing
	// arguments is most of the cost, and the surrounding code is there to give
	// the change a shape to sit in, not to be inspected.
	Instrument(scratchRoot string, ids, context []bundle.SymbolID) (Instrumented, error)
}

type Instrumented struct {
	Done    []bundle.SymbolID
	Skipped []string
	Env     map[string]string
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
