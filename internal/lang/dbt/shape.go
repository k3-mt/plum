package dbt

import (
	"regexp"
	"strings"
)

// What a model does to its inputs, read off the SQL. A code trace records
// execution; a warehouse run records only that a query ran and what it cost, so
// the shape of the transformation has to come from the statement itself.
//
// This is a scanner, not a parser. It reads the joins, the filters and the
// grouping of a straightforward dbt model — which is what most models are — and
// says nothing rather than guessing when a statement is beyond it. A wrong
// grain on the picture is worse than a blank one (P1).

var (
	joinRe = regexp.MustCompile(
		"(?is)\\b(inner|left(?:\\s+outer)?|right(?:\\s+outer)?|full(?:\\s+outer)?|cross)?\\s*join\\s+" +
			"(\\{\\{[^}]*\\}\\}|`[^`]+`|[a-z0-9_.-]+)" +
			"(?:\\s+(?:as\\s+)?([a-z_]\\w*))?" +
			"(?:\\s+on\\s+([^\n]*(?:\n\\s{4,}[^\n]*)*))?")
	whereRe  = regexp.MustCompile(`(?is)\bwhere\b(.*?)(?:\bgroup\s+by\b|\border\s+by\b|\bhaving\b|\bqualify\b|\blimit\b|\bunion\b|;|$)`)
	groupRe  = regexp.MustCompile(`(?is)\bgroup\s+by\b(.*?)(?:\border\s+by\b|\bhaving\b|\bqualify\b|\blimit\b|\bunion\b|;|$)`)
	fromRe   = regexp.MustCompile(`(?is)\bfrom\s+(\{\{[^}]*\}\}|` + "`" + `[^` + "`" + `]+` + "`" + `|[a-z0-9_.-]+)(?:\s+(?:as\s+)?([a-z_]\w*))?`)
	selectRe = regexp.MustCompile(`(?is)\bselect\b(.*?)\bfrom\b`)
	perRowRe = regexp.MustCompile(`(?i)\bone\s+row\s+per\s+([a-z0-9_-]+(?:\s+[a-z0-9_-]+)?)`)
	// Where a stated grain stops. "One row per order as the source system
	// records it" states a grain of "order"; the rest is a sentence.
	grainStopRe = regexp.MustCompile(`(?i)^(as|with|that|which|where|from|for|in|of|and|plus|per|the|a|an)$`)
	aggRe       = regexp.MustCompile(`(?i)\b(count|sum|avg|min|max|array_agg|string_agg|approx_count_distinct)\s*\(`)
	aliasColRe  = regexp.MustCompile(`(?i)^\s*(.*?)\s+as\s+([a-z_]\w*)\s*$`)
)

// Join is one inbound edge as the SQL writes it: how the rows are matched, and
// on what. The join type is the difference between "rows can vanish here" and
// "rows can multiply here", which is the first thing to know about a mart.
type Join struct {
	Type   string // inner | left | right | full | cross
	Target string // the ref name, or the raw table when the model escaped the DAG
	Alias  string
	On     string // the join condition, as written
	Key    string // the column both sides are matched on, when it is a plain equality
	Line   int
}

// Shape is what one model does: what it reads, how it matches, what it drops,
// and what one output row ends up meaning.
type Shape struct {
	From       string
	Joins      []Join
	Where      string
	GroupBy    string
	Aggregates []string
	// Grain is what one row of the output is. Declared is what the prose says
	// ("one row per order"); Inferred is what the SQL does. They are kept apart
	// on purpose: when they disagree, that is the finding.
	Declared string
	Inferred string
	// Unresolved says why the grain could not be read off the SQL, when there
	// is a reason worth reporting. A model that groups positionally over a
	// select star has a grain that depends on whatever columns upstream has
	// today — which is not a gap in this scanner, it is a fact about the model.
	Unresolved string
}

// ReadShape scans a model's SQL. doc is the model's description or leading
// comment, which is where the grain is usually stated in words.
func ReadShape(sql, doc string) Shape {
	code := stripSQLComments(sql)
	var s Shape

	if m := fromRe.FindStringSubmatch(code); m != nil {
		s.From = refName(m[1])
	}
	for _, m := range joinRe.FindAllStringSubmatchIndex(code, -1) {
		g := func(i int) string {
			if m[2*i] < 0 {
				return ""
			}
			return strings.TrimSpace(code[m[2*i]:m[2*i+1]])
		}
		j := Join{
			Type:   joinType(g(1)),
			Target: refName(g(2)),
			Alias:  g(3),
			On:     oneLineSQL(g(4)),
			Line:   LineOf(code, m[0]),
		}
		j.Key = joinKey(j.On)
		s.Joins = append(s.Joins, j)
	}
	if m := whereRe.FindStringSubmatch(code); m != nil {
		s.Where = oneLineSQL(m[1])
	}
	if m := groupRe.FindStringSubmatch(code); m != nil {
		s.GroupBy = oneLineSQL(m[1])
	}
	for _, m := range aggRe.FindAllStringSubmatch(code, -1) {
		s.Aggregates = appendUnique(s.Aggregates, strings.ToLower(m[1]))
	}

	s.Declared = declaredGrain(doc)
	s.Inferred, s.Unresolved = inferGrain(code, s)
	return s
}

// GrainDivergence reports a doc that promises a grain the SQL does not produce.
// It is a claim about content addressed to content (P5): "one row per order"
// written above a statement that groups by customer_id is wrong the moment the
// group by lands, and nothing else in the pipeline will notice.
func (s Shape) GrainDivergence() string {
	if s.Declared == "" || s.Inferred == "" || s.Declared == s.Inferred {
		return ""
	}
	// Only report when the SQL is definite about it. An inference drawn from a
	// bare select is a guess, and a guess must not contradict a person.
	if s.GroupBy == "" || sameGrain(s.Declared, s.Inferred) {
		return ""
	}
	return "the doc says one row per " + s.Declared +
		"; the SQL groups by " + s.GroupBy + ", giving one row per " + s.Inferred
}

// sameGrain compares two statements of grain written by different hands. Prose
// says "customer", SQL says "customer_id", and treating those as a divergence
// would bury the real ones.
func sameGrain(a, b string) bool { return normaliseGrain(a) == normaliseGrain(b) }

func normaliseGrain(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer("_", " ", "-", " ").Replace(s)
	fields := strings.Fields(s)
	for i, f := range fields {
		f = strings.TrimSuffix(f, "id")
		f = strings.TrimSuffix(strings.TrimSuffix(f, "es"), "s")
		fields[i] = strings.TrimSpace(f)
	}
	var kept []string
	for _, f := range fields {
		if f != "" {
			kept = append(kept, f)
		}
	}
	return strings.Join(kept, " ")
}

// declaredGrain pulls "one row per X" out of prose. dbt authors write it
// constantly, in the model's description and at the top of the file, because it
// is the one thing about a model you cannot recover by looking at the columns.
func declaredGrain(doc string) string {
	m := perRowRe.FindStringSubmatch(doc)
	if m == nil {
		return ""
	}
	var kept []string
	for _, w := range strings.Fields(strings.ToLower(m[1])) {
		w = strings.Trim(w, ",.:;()")
		if w == "" || grainStopRe.MatchString(w) {
			break
		}
		kept = append(kept, w)
	}
	return strings.Join(kept, " ")
}

// inferGrain reads the grain off the statement. A group by names it outright;
// otherwise the grain is whatever the driving table's grain was, which this
// cannot know and so does not claim.
func inferGrain(code string, s Shape) (grain, unresolved string) {
	if s.GroupBy == "" {
		return "", ""
	}
	cols := selectedColumns(code)
	var names []string
	for _, part := range strings.Split(s.GroupBy, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// group by 1, 2 is positional: the numbers index the select list, and
		// resolving them is the only way to say what the grain actually is.
		if n := positional(part); n > 0 {
			if n > len(cols) {
				return "", "groups by position " + part + ", past the end of the select list"
			}
			if cols[n-1] == "*" {
				return "", "groups by position over a select star, so its grain is whatever columns upstream has today"
			}
			names = append(names, cols[n-1])
			continue
		}
		names = append(names, strings.ToLower(lastSegmentSQL(part)))
	}
	if len(names) == 0 {
		return "", ""
	}
	return strings.Join(names, " + "), ""
}

// selectedColumns returns the output names of the select list, in order, so a
// positional group by can be resolved. A `*` cannot be resolved — its width is
// whatever upstream has today — so it stops the list.
func selectedColumns(code string) []string {
	m := selectRe.FindStringSubmatch(code)
	if m == nil {
		return nil
	}
	var out []string
	for _, part := range splitTopLevel(m[1]) {
		part = oneLineSQL(part)
		if part == "" {
			continue
		}
		if strings.HasSuffix(part, "*") {
			out = append(out, "*")
			continue
		}
		if a := aliasColRe.FindStringSubmatch(part); a != nil {
			out = append(out, strings.ToLower(a[2]))
			continue
		}
		out = append(out, strings.ToLower(lastSegmentSQL(part)))
	}
	return out
}

// splitTopLevel splits a select list on commas that are not inside parentheses,
// so cast(x as numeric) / 100 stays one column.
func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

func positional(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// joinKey pulls the matched column out of a plain equality. Anything more
// involved is left alone and shown as written.
func joinKey(on string) string {
	parts := strings.SplitN(on, "=", 2)
	if len(parts) != 2 || strings.Contains(on, " and ") || strings.Contains(on, " or ") {
		return ""
	}
	l, r := lastSegmentSQL(parts[0]), lastSegmentSQL(parts[1])
	if l != "" && strings.EqualFold(l, r) {
		return strings.ToLower(l)
	}
	return ""
}

func joinType(kw string) string {
	kw = strings.ToLower(strings.Join(strings.Fields(kw), " "))
	switch {
	case kw == "":
		return "inner"
	case strings.HasPrefix(kw, "left"):
		return "left"
	case strings.HasPrefix(kw, "right"):
		return "right"
	case strings.HasPrefix(kw, "full"):
		return "full"
	case kw == "cross":
		return "cross"
	}
	return kw
}

// refName turns whatever the SQL wrote — a ref, a source, a backticked
// fully-qualified table — into the name the DAG uses.
func refName(s string) string {
	s = strings.TrimSpace(s)
	if m := refRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	if m := sourceRe.FindStringSubmatch(s); m != nil {
		return m[1] + "." + m[2]
	}
	return strings.Trim(s, "`")
}

func lastSegmentSQL(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(strings.Trim(s, "`\"'"))
}

func oneLineSQL(s string) string { return strings.Join(strings.Fields(s), " ") }

func appendUnique(list []string, s string) []string {
	for _, e := range list {
		if e == s {
			return list
		}
	}
	return append(list, s)
}
