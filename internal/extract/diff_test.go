package extract

import (
	"testing"

	"github.com/kelalaike/plum/internal/bundle"
)

const zeroContextDiff = `diff --git a/internal/auth/cache.go b/internal/auth/cache.go
index 1111111..2222222 100644
--- a/internal/auth/cache.go
+++ b/internal/auth/cache.go
@@ -12,3 +12,5 @@
-	old line
+	new line one
+	new line two
@@ -40,0 +42,2 @@
+	added at 42
+	added at 43
`

func TestParseDiff(t *testing.T) {
	hs := ParseDiff(zeroContextDiff)
	if len(hs) != 2 {
		t.Fatalf("got %d hunks, want 2", len(hs))
	}
	if hs[0].File != "internal/auth/cache.go" {
		t.Errorf("file = %q", hs[0].File)
	}
	if hs[0].Start != 12 || hs[0].Lines != 5 || hs[0].End() != 16 {
		t.Errorf("hunk 0 = start %d lines %d end %d", hs[0].Start, hs[0].Lines, hs[0].End())
	}
	if len(hs[0].Added) != 2 || len(hs[0].Removed) != 1 {
		t.Errorf("hunk 0 added %d removed %d", len(hs[0].Added), len(hs[0].Removed))
	}
	if hs[1].Start != 42 || hs[1].End() != 43 {
		t.Errorf("hunk 1 = %d..%d", hs[1].Start, hs[1].End())
	}
}

func TestParseDiffPureDeletion(t *testing.T) {
	// A hunk with no new lines still occupies the position it was removed from.
	hs := ParseDiff("+++ b/a.go\n@@ -10,3 +9,0 @@\n-gone\n")
	if len(hs) != 1 {
		t.Fatalf("got %d hunks", len(hs))
	}
	if hs[0].Lines != 0 || hs[0].End() != hs[0].Start {
		t.Errorf("zero-line hunk: start %d lines %d end %d", hs[0].Start, hs[0].Lines, hs[0].End())
	}
}

func decls() []bundle.Symbol {
	return []bundle.Symbol{
		{ID: "f.go::Outer", Name: "Outer", LineStart: 1, LineEnd: 100},
		{ID: "f.go::A", Name: "A", LineStart: 10, LineEnd: 20},
		{ID: "f.go::B", Name: "B", LineStart: 30, LineEnd: 40},
		{ID: "f.go::C", Name: "C", LineStart: 50, LineEnd: 60},
	}
}

func TestMapHunksInnermostWins(t *testing.T) {
	got := MapHunks(decls(), []Hunk{{Start: 12, Lines: 2}})
	if len(got) != 1 || got[0].ID != "f.go::A" {
		t.Fatalf("got %v, want only f.go::A — the innermost enclosing declaration", ids(got))
	}
}

func TestMapHunksCreditsEverySiblingItSpans(t *testing.T) {
	// A rewrite replacing three functions changed three functions, not one.
	got := MapHunks(decls(), []Hunk{{Start: 10, Lines: 51}})
	want := map[bundle.SymbolID]bool{"f.go::A": true, "f.go::B": true, "f.go::C": true}
	if len(got) != 3 {
		t.Fatalf("got %v, want A, B and C", ids(got))
	}
	for _, s := range got {
		if !want[s.ID] {
			t.Errorf("unexpected %s", s.ID)
		}
	}
}

func TestMapHunksUntouchedFileYieldsNothing(t *testing.T) {
	if got := MapHunks(decls(), nil); len(got) != 0 {
		t.Fatalf("got %v, want none", ids(got))
	}
}

func TestMapHunksOutsideAnyDeclaration(t *testing.T) {
	// An import block change belongs to no declaration and must not be
	// misattributed to whichever function happens to be nearest.
	got := MapHunks([]bundle.Symbol{{ID: "f.go::A", LineStart: 10, LineEnd: 20}}, []Hunk{{Start: 3, Lines: 1}})
	if len(got) != 0 {
		t.Fatalf("got %v, want none", ids(got))
	}
}

func ids(ss []bundle.Symbol) []bundle.SymbolID {
	var out []bundle.SymbolID
	for _, s := range ss {
		out = append(out, s.ID)
	}
	return out
}

func TestParseNumstatHandlesRenames(t *testing.T) {
	m := ParseNumstat("3\t1\told.go => new.go\n5\t0\tplain.go\n")
	if v, ok := m["new.go"]; !ok || v != [2]int{3, 1} {
		t.Errorf("rename target: %v %v", v, ok)
	}
	if m["plain.go"] != [2]int{5, 0} {
		t.Errorf("plain: %v", m["plain.go"])
	}
}
