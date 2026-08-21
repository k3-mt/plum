package gopkg

import (
	"os"
	"testing"

	"github.com/k3-mt/plum/internal/bundle"
)

// These are the three cases a text search gets wrong, taken from real code in
// this repository. Clicking each of them deserves a different answer, and only
// a parse can tell them apart.
const resolveSrc = `package demo

import (
	"fmt"
	"strings"
)

// Threshold is how many before it stops.
const Threshold = 10

func renderContext(pc PromptContext) string {
	var w strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&w, f+"\n", a...) }
	sym := pc.Symbol_

	kind := sym.Kind
	if kind == "" {
		kind = "symbol"
	}
	p("# %s", pc.Symbol)
	for _, cs := range sym.CallSites {
		cutoff := len(cs.Callee) - 1
		if cutoff > Threshold {
			p("%d", cutoff)
		}
	}
	return w.String()
}
`

func resolve(t *testing.T, name string, line int) bundle.Resolution {
	t.Helper()
	_ = os.WriteFile
	got, err := New().ResolveIdentifier("demo.go", []byte(resolveSrc), line, name)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A local assigned from a parameter. "First appears at line 14" was the old
// answer and it is not what anybody wants to know: where it came from is.
func TestALocalReportsWhatItWasDerivedFrom(t *testing.T) {
	r := resolve(t, "sym", 14)
	if r.Kind != "local" {
		t.Errorf("kind = %q, want local", r.Kind)
	}
	if r.DerivedFrom != "pc.Symbol_" {
		t.Errorf("derived from %q, want pc.Symbol_ — the expression is the answer", r.DerivedFrom)
	}
	if r.DeclaredAt != 14 {
		t.Errorf("declared at %d, want 14", r.DeclaredAt)
	}
}

// A closure. A text search sees `p(` and reports a call that leaves the
// recorded set, when it is declared two lines up.
func TestAClosureIsALocalAndNotACall(t *testing.T) {
	r := resolve(t, "p", 13)
	if r.Kind != "local function" {
		t.Errorf("kind = %q, want local function", r.Kind)
	}
	if r.DeclaredAt != 13 {
		t.Errorf("declared at %d, want 13", r.DeclaredAt)
	}
}

// A package qualifier. A text search reports a local named strings.
func TestAPackageIsAPackage(t *testing.T) {
	r := resolve(t, "strings", 12)
	if r.Kind != "package" {
		t.Errorf("kind = %q, want package", r.Kind)
	}
	if r.Type != "strings" {
		t.Errorf("type = %q, want the import path", r.Type)
	}
}

func TestAParameterCarriesItsType(t *testing.T) {
	r := resolve(t, "pc", 14)
	if r.Kind != "parameter" || r.Type != "PromptContext" {
		t.Errorf("got %s/%s, want parameter/PromptContext", r.Kind, r.Type)
	}
}

// Reassignment is the thing a flat "used at" list hides, and it is exactly what
// you need to see when following a value through a function.
func TestWritesAreSeparatedFromReads(t *testing.T) {
	r := resolve(t, "kind", 16)
	if r.DeclaredAt != 16 {
		t.Errorf("declared at %d, want 16", r.DeclaredAt)
	}
	// The declaration is itself a write, and listing it is the point: "written
	// at 16 and 18" says at a glance that this name is set twice, which is the
	// thing a flat list of uses hides.
	if len(r.Writes) != 2 || r.Writes[0] != 16 || r.Writes[1] != 18 {
		t.Errorf("writes = %v, want the declaration at 16 and the reassignment at 18", r.Writes)
	}
	if len(r.Reads) == 0 {
		t.Error("the comparison at 17 is a read and should be listed")
	}
}

// A binding a text search cannot see is a binding at all.
func TestRangeBindingsAreFound(t *testing.T) {
	r := resolve(t, "cs", 21)
	if r.Kind != "range value" {
		t.Errorf("kind = %q, want range value", r.Kind)
	}
	if r.DerivedFrom != "sym.CallSites" {
		t.Errorf("derived from %q, want sym.CallSites", r.DerivedFrom)
	}
}

func TestAConstantIsFoundWithItsDoc(t *testing.T) {
	r := resolve(t, "Threshold", 23)
	if r.Kind != "constant" {
		t.Errorf("kind = %q, want constant", r.Kind)
	}
	if r.DerivedFrom != "10" {
		t.Errorf("derived from %q, want 10", r.DerivedFrom)
	}
	if r.Doc == "" {
		t.Error("the constant's own comment should travel with it")
	}
}

// The one the user asked for by name.
func TestCutoffReportsTheExpressionItComesFrom(t *testing.T) {
	r := resolve(t, "cutoff", 22)
	if r.Kind != "local" {
		t.Errorf("kind = %q", r.Kind)
	}
	if r.DerivedFrom != "len(cs.Callee) - 1" {
		t.Errorf("derived from %q, want the whole expression", r.DerivedFrom)
	}
}

// A name it cannot resolve must say so rather than be filed under something
// plausible. A confident wrong answer is worse than an admitted gap.
func TestAnUnknownNameSaysSo(t *testing.T) {
	r := resolve(t, "nowhere", 14)
	if r.Kind != "unknown" || r.Note == "" {
		t.Errorf("got %s/%q, want unknown with a reason", r.Kind, r.Note)
	}
}

// A name declared twice in one function is two variables, and Go says so with
// block scope. Reporting them as one puts writes from a stranger's scope in
// your list — which for a reader following a value is worse than saying nothing.
const shadowSrc = `package demo

func render(items []string) string {
	kind := "outer"
	if kind == "" {
		kind = "symbol"
	}
	for _, it := range items {
		kind := "inner"
		if it != "" {
			kind = "set"
		}
		_ = kind
	}
	return kind
}
`

func resolveIn(t *testing.T, src, name string, line int) bundle.Resolution {
	t.Helper()
	got, err := New().ResolveIdentifier("demo.go", []byte(src), line, name)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestTwoVariablesSharingANameAreTwoVariables(t *testing.T) {
	outer := resolveIn(t, shadowSrc, "kind", 4)
	if outer.DeclaredAt != 4 || outer.DerivedFrom != `"outer"` {
		t.Errorf("outer: declared %d from %q", outer.DeclaredAt, outer.DerivedFrom)
	}
	for _, ln := range append(outer.Writes, outer.Reads...) {
		if ln >= 9 && ln <= 14 {
			t.Errorf("outer kind claims line %d, which belongs to the inner one", ln)
		}
	}

	inner := resolveIn(t, shadowSrc, "kind", 9)
	if inner.DeclaredAt != 9 || inner.DerivedFrom != `"inner"` {
		t.Errorf("inner: declared %d from %q, want 9 from \"inner\"", inner.DeclaredAt, inner.DerivedFrom)
	}
	if len(inner.Writes) != 2 || inner.Writes[0] != 9 || inner.Writes[1] != 11 {
		t.Errorf("inner writes = %v, want its own declaration and reassignment", inner.Writes)
	}
	// And a line inside the loop that only reads it resolves to the same one.
	if mid := resolveIn(t, shadowSrc, "kind", 13); mid.DeclaredAt != 9 {
		t.Errorf("a read inside the loop resolved to the declaration at %d", mid.DeclaredAt)
	}
}
