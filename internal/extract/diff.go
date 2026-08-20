// Package extract turns a commit range into mechanical evidence. No LLM ever
// runs in here: everything in this package is derivable from git and the AST (P1).
package extract

import (
	"bufio"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/k3-mt/plum/internal/bundle"
)

// Hunk is a contiguous run of changed lines in the *new* file.
type Hunk struct {
	File     string
	OldStart int
	OldLines int
	Start    int // new-file start line, 1-based
	Lines    int
	Added    []string
	Removed  []string
}

// End is the last line the hunk touches. A zero-line hunk (pure deletion) still
// occupies the position it was removed from, so End == Start there.
func (h Hunk) End() int {
	if h.Lines == 0 {
		return h.Start
	}
	return h.Start + h.Lines - 1
}

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// ParseDiff reads `git diff --unified=0` output into per-file hunks. Zero
// context keeps hunks tight, which is what makes symbol mapping precise (§6.3).
func ParseDiff(diff string) []Hunk {
	var out []Hunk
	var file string
	var cur *Hunk
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}
	sc := bufio.NewScanner(strings.NewReader(diff))
	sc.Buffer(make([]byte, 0, 256*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "+++ "):
			flush()
			p := strings.TrimPrefix(line, "+++ ")
			if p == "/dev/null" {
				file = ""
			} else {
				file = strings.TrimPrefix(p, "b/")
			}
		case strings.HasPrefix(line, "--- "):
			if file == "" {
				p := strings.TrimPrefix(line, "--- ")
				if p != "/dev/null" {
					file = strings.TrimPrefix(p, "a/")
				}
			}
		case strings.HasPrefix(line, "@@"):
			flush()
			m := hunkRe.FindStringSubmatch(line)
			if m == nil || file == "" {
				continue
			}
			h := Hunk{File: file, OldStart: atoi(m[1]), OldLines: 1, Start: atoi(m[3]), Lines: 1}
			if m[2] != "" {
				h.OldLines = atoi(m[2])
			}
			if m[4] != "" {
				h.Lines = atoi(m[4])
			}
			cur = &h
		case cur != nil && strings.HasPrefix(line, "+"):
			cur.Added = append(cur.Added, line[1:])
		case cur != nil && strings.HasPrefix(line, "-"):
			cur.Removed = append(cur.Removed, line[1:])
		}
	}
	flush()
	return out
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// HunksByFile groups hunks for symbol mapping.
func HunksByFile(hs []Hunk) map[string][]Hunk {
	m := map[string][]Hunk{}
	for _, h := range hs {
		m[h.File] = append(m[h.File], h)
	}
	return m
}

// MapHunks assigns each diff hunk to its enclosing declarations. Where
// declarations nest, the innermost one wins; where a single hunk spans several
// siblings, every sibling it touches is credited — a rewrite that replaces three
// functions changed three functions (spec §6.3).
//
// This is the highest-value transform in the tool: it turns "lines 40–58" into
// "modified AuthCache.Get", and everything downstream keys off the result.
func MapHunks(decls []bundle.Symbol, hunks []Hunk) []bundle.Symbol {
	touched := map[bundle.SymbolID]bundle.Symbol{}
	for _, h := range hunks {
		var overlapping []bundle.Symbol
		for _, d := range decls {
			if h.Start <= d.LineEnd && h.End() >= d.LineStart {
				overlapping = append(overlapping, d)
			}
		}
		for _, d := range overlapping {
			if !containsAnother(d, overlapping) {
				touched[d.ID] = d
			}
		}
	}
	out := make([]bundle.Symbol, 0, len(touched))
	for _, s := range touched {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LineStart < out[j].LineStart })
	return out
}

// containsAnother reports whether d strictly encloses another declaration the
// same hunk touched, in which case the inner one is the better answer.
func containsAnother(d bundle.Symbol, others []bundle.Symbol) bool {
	for _, o := range others {
		if o.ID == d.ID {
			continue
		}
		if o.LineStart >= d.LineStart && o.LineEnd <= d.LineEnd &&
			(o.LineEnd-o.LineStart) < (d.LineEnd-d.LineStart) {
			return true
		}
	}
	return false
}

// ParseNumstat reads `git diff --numstat` into per-file line counts.
func ParseNumstat(s string) map[string][2]int {
	out := map[string][2]int{}
	for _, l := range strings.Split(s, "\n") {
		f := strings.Split(strings.TrimSpace(l), "\t")
		if len(f) < 3 {
			continue
		}
		path := f[2]
		if i := strings.Index(path, " => "); i >= 0 { // rename: "old => new"
			path = strings.TrimSuffix(path[i+4:], "}")
		}
		add, _ := strconv.Atoi(f[0])
		del, _ := strconv.Atoi(f[1])
		out[path] = [2]int{add, del}
	}
	return out
}

// ParseNameStatus reads `git diff --name-status` into change kinds.
func ParseNameStatus(s string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(s, "\n") {
		f := strings.Split(strings.TrimSpace(l), "\t")
		if len(f) < 2 {
			continue
		}
		kind := "modified"
		switch f[0][0] {
		case 'A':
			kind = "added"
		case 'D':
			kind = "deleted"
		case 'R':
			kind = "renamed"
		}
		out[f[len(f)-1]] = kind
	}
	return out
}
