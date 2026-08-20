package dbt

import (
	"regexp"
	"strings"
)

// The SQL side of the adapter is a scanner too. It answers three questions —
// what does this model depend on, what is risky about how it is written, and
// has it changed — and none of those need a parse tree.

var (
	refRe    = regexp.MustCompile(`\{\{\s*ref\(\s*['"]([^'"]+)['"]\s*\)\s*\}\}`)
	sourceRe = regexp.MustCompile(`\{\{\s*source\(\s*['"]([^'"]+)['"]\s*,\s*['"]([^'"]+)['"]\s*\)\s*\}\}`)
	// config( ... ) is found by scanning for the matching paren rather than by
	// regex: a partition_by={...} argument contains braces of its own, and a
	// pattern that stops at the first one silently reads the whole block as
	// empty — which made every partitioned model look like a view.
	configOpenRe = regexp.MustCompile(`\{\{\s*config\s*\(`)
	// A fully-qualified table written straight into the SQL, which is how a
	// model escapes the DAG: dbt cannot know about it, so it will not be built
	// first and will not appear in lineage.
	hardcodedRe = regexp.MustCompile("(?i)\\b(?:from|join)\\s+(`[a-z0-9_-]+\\.[a-z0-9_]+\\.[a-z0-9_]+`|[a-z0-9_-]+\\.[a-z0-9_]+\\.[a-z0-9_]+)")
	// `select *` and `select o.*` are the same hazard: the column list becomes
	// whatever upstream happens to have today.
	selectStarRe  = regexp.MustCompile(`(?i)select\s+(?:[a-z_]\w*\s*\.\s*)?\*`)
	crossJoinRe   = regexp.MustCompile(`(?i)\bcross\s+join\b`)
	nowRe         = regexp.MustCompile(`(?i)\b(current_timestamp|current_date|now)\s*\(`)
	incrementalRe = regexp.MustCompile(`(?i)\bis_incremental\s*\(`)
)

// stripSQLComments blanks comments so a scanner does not read a commented-out
// `select *` as a real one.
func stripSQLComments(sql string) string {
	var out strings.Builder
	for i := 0; i < len(sql); i++ {
		if sql[i] == '-' && i+1 < len(sql) && sql[i+1] == '-' {
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			out.WriteByte('\n')
			continue
		}
		if sql[i] == '/' && i+1 < len(sql) && sql[i+1] == '*' {
			i += 2
			for i+1 < len(sql) && !(sql[i] == '*' && sql[i+1] == '/') {
				if sql[i] == '\n' {
					out.WriteByte('\n')
				}
				i++
			}
			i++
			continue
		}
		out.WriteByte(sql[i])
	}
	return out.String()
}

// Refs returns the models and sources this model selects from — the DAG, as the
// model itself declares it.
func Refs(sql string) (models []string, sources []string) {
	code := stripSQLComments(sql)
	for _, m := range refRe.FindAllStringSubmatch(code, -1) {
		models = append(models, m[1])
	}
	for _, m := range sourceRe.FindAllStringSubmatch(code, -1) {
		sources = append(sources, m[1]+"."+m[2])
	}
	return models, sources
}

// Config reads the model's own {{ config(...) }} block. Materialization decides
// what a change costs to apply, so it belongs in the report.
func Config(sql string) map[string]string {
	out := map[string]string{}
	code := stripSQLComments(sql)
	loc := configOpenRe.FindStringIndex(code)
	if loc == nil {
		return out
	}
	args := balanced(code[loc[1]-1:])
	if args == "" {
		return out
	}
	for _, part := range splitArgs(args) {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `'"`)
	}
	return out
}

// balanced returns the contents of the parenthesised group starting at s[0],
// respecting nested brackets and quotes.
func balanced(s string) string {
	if len(s) == 0 || s[0] != '(' {
		return ""
	}
	depth, quote := 0, byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return s[1:i]
			}
		}
	}
	return ""
}

// splitArgs splits a config argument list on commas that are not inside
// brackets or quotes.
func splitArgs(s string) []string {
	var out []string
	depth, quote, start := 0, byte(0), 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '[' || c == '(' || c == '{':
			depth++
		case c == ']' || c == ')' || c == '}':
			depth--
		case c == ',' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// Normalise strips comments and collapses whitespace, so reformatting a query
// does not invalidate what has been claimed about it, but changing what it
// selects does.
func Normalise(sql string) string {
	return strings.Join(strings.Fields(stripSQLComments(sql)), " ")
}

// LineOf returns the 1-based line a match sits on, for a marker's location.
func LineOf(sql string, index int) int {
	if index < 0 || index > len(sql) {
		return 0
	}
	return strings.Count(sql[:index], "\n") + 1
}
