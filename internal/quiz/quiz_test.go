package quiz

import "testing"

func TestGradeRejectsAccidentalSubstrings(t *testing.T) {
	q := Question{Expected: "auth: no token for absent"}
	// "b" appears inside the recorded value. Accepting it would make the whole
	// exercise theatre, which is exactly what P4 exists to prevent.
	if Grade(q, "b") {
		t.Error("a single letter must not count as knowing the answer")
	}
	if !Grade(q, "auth: no token for absent") {
		t.Error("the exact recorded value must count")
	}
	if !Grade(q, `"auth: no token for absent."`) {
		t.Error("punctuation and quoting must not matter")
	}
	if Grade(q, "cache miss") {
		t.Error("a different value must not count")
	}
	if Grade(q, "") {
		t.Error("an empty answer is not correct")
	}
}

func TestGradeMultipleChoiceIsExact(t *testing.T) {
	q := Question{Expected: "Cache.lookup", Options: []string{"Cache.lookup", "Cache.decorate"}}
	if !Grade(q, "cache.lookup") {
		t.Error("case should not matter")
	}
	if Grade(q, "lookup") {
		t.Error("a partial option must not count when options were offered")
	}
}
