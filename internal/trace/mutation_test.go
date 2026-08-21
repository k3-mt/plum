package trace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Arguments are recorded on the way in, which says what a function was given and
// nothing about what it did to it. A handler that writes to a ResponseWriter
// returns nothing at all: before this, its entire behaviour was invisible.
//
// This drives the real shim rather than reimplementing its rules, because the
// question is what the shipped code records, not what a copy of its logic would.
func TestTheShimRecordsWhatACallChangedAboutItsArguments(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "plumtrace")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "shim.go"), []byte(GoShimSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module mutcheck\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

import "mutcheck/plumtrace"

type box struct{ Items []string }

// fill writes through the pointer: the caller's box is different afterwards.
func fill(b *box) {
	defer plumtrace.Enter("m.go::fill", plumtrace.KV{K: "b", V: b})()
	b.Items = append(b.Items, "added")
}

// count reads and returns; it changes nothing its caller can see.
func count(b *box, n int) int {
	defer plumtrace.Enter("m.go::count", plumtrace.KV{K: "b", V: b}, plumtrace.KV{K: "n", V: n})()
	return len(b.Items) + n
}

func main() {
	b := &box{}
	fill(b)
	count(b, 1)
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "run", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PLUM_TRACE=1", "PLUM_TRACE_OUT="+out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("running the traced program: %v\n%s", err, b)
	}

	events, err := ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	returns := map[string]Event{}
	for _, e := range events {
		if e.Kind == "return" {
			returns[string(e.Symbol)] = e
		}
	}

	filled, ok := returns["m.go::fill"]
	if !ok {
		t.Fatalf("no return recorded for fill; got %d events", len(events))
	}
	after, ok := filled.ArgsOut["b"]
	if !ok {
		t.Fatal("fill appended to the caller's slice and the trace did not say so")
	}
	if !strings.Contains(after, "added") {
		t.Errorf("after = %q, want the appended element", after)
	}

	// The other half, and the one that keeps this readable: an argument that
	// came back the same must say nothing. Reporting every argument on every
	// return would bury the ones that actually changed.
	counted := returns["m.go::count"]
	if len(counted.ArgsOut) != 0 {
		t.Errorf("count changed nothing but reported %v", counted.ArgsOut)
	}
}
