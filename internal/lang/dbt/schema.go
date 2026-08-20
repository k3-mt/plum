// Package dbt is the adapter for dbt projects.
//
// It needs no SQL parser. A dbt project already publishes its own symbol
// table — models, their columns, their tests, and the DAG that joins them — so
// this reads that rather than deriving it.
//
// Where it reads from matters. `target/manifest.json` is the richest source but
// it is a build artifact and normally gitignored, so it cannot be diffed across
// a commit range: there is no manifest at StartSHA to compare against. The
// committed `schema.yml` files are the *declared* contract, they are diffable,
// and they are what a reviewer actually changes. So the declaration is the
// source of truth here and the manifest, when present, is enrichment.
package dbt

import (
	"strings"
)

// Model is one model's declared contract, as written in a schema file.
type Model struct {
	Name        string
	Description string
	Line        int
	Columns     []Column
	Tests       []string // model-level tests
}

// Column is one declared column: the part of a model other people depend on.
type Column struct {
	Name        string
	Description string
	DataType    string
	Tests       []string
	Line        int
}

// parseSchema reads the subset of a dbt schema file that describes contracts:
// models, their columns, their descriptions and their tests.
//
// It is a scanner, not a YAML implementation. dbt schema files are a narrow,
// highly conventional shape, and a scanner that understands that shape exactly
// is easier to trust than a general parser that understands it approximately.
func parseSchema(src []byte) []Model {
	var models []Model
	var current *Model
	var column *Column
	var listTarget *[]string // where a bare "- item" belongs right now

	closeColumn := func() {
		if current != nil && column != nil {
			current.Columns = append(current.Columns, *column)
			column = nil
		}
	}
	closeModel := func() {
		closeColumn()
		if current != nil {
			models = append(models, *current)
			current = nil
		}
	}

	section := "" // models | columns | tests
	var modelIndent, columnIndent int

	for i, raw := range strings.Split(string(src), "\n") {
		line := strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))

		switch {
		case trimmed == "models:":
			closeModel()
			section, modelIndent = "models", indent
			continue
		case trimmed == "sources:" || trimmed == "seeds:" || trimmed == "snapshots:" || trimmed == "macros:":
			// Only models are contracts other models select from.
			closeModel()
			section = trimmed
			continue
		}
		if section != "models" {
			continue
		}

		// A new model starts at "- name: x" one level inside models:.
		if name, ok := listItemKey(trimmed, "name"); ok && indent <= modelIndent+2 && column == nil {
			closeModel()
			current = &Model{Name: name, Line: i + 1}
			listTarget = nil
			continue
		}
		if current == nil {
			continue
		}

		// Inside a columns: block, "- name: c" starts a column.
		if trimmed == "columns:" {
			closeColumn()
			columnIndent = indent
			listTarget = nil
			continue
		}
		if name, ok := listItemKey(trimmed, "name"); ok && indent > columnIndent && columnIndent > 0 {
			closeColumn()
			column = &Column{Name: name, Line: i + 1}
			listTarget = nil
			continue
		}

		if trimmed == "tests:" || trimmed == "data_tests:" {
			if column != nil {
				listTarget = &column.Tests
			} else {
				listTarget = &current.Tests
			}
			continue
		}
		if strings.HasPrefix(trimmed, "- ") && listTarget != nil {
			if test := testName(strings.TrimPrefix(trimmed, "- ")); test != "" {
				*listTarget = append(*listTarget, test)
			}
			continue
		}

		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(key, "- "))
		value = unquote(strings.TrimSpace(value))
		switch key {
		case "description":
			if column != nil {
				column.Description = value
			} else {
				current.Description = value
			}
			listTarget = nil
		case "data_type":
			if column != nil {
				column.DataType = value
			}
			listTarget = nil
		}
	}
	closeModel()
	return models
}

// listItemKey matches `- name: value`.
func listItemKey(trimmed, want string) (string, bool) {
	if !strings.HasPrefix(trimmed, "- ") {
		return "", false
	}
	key, value, ok := strings.Cut(strings.TrimPrefix(trimmed, "- "), ":")
	if !ok || strings.TrimSpace(key) != want {
		return "", false
	}
	return unquote(strings.TrimSpace(value)), true
}

// testName reads both shapes dbt allows: a bare `- unique` and the configured
// `- relationships:` block that follows with arguments.
func testName(item string) string {
	item = strings.TrimSpace(item)
	if name, _, ok := strings.Cut(item, ":"); ok {
		return strings.TrimSpace(name)
	}
	return unquote(item)
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

// HasTest reports whether a column carries one of the named tests.
func (c Column) HasTest(names ...string) bool {
	for _, t := range c.Tests {
		for _, want := range names {
			if strings.EqualFold(t, want) {
				return true
			}
		}
	}
	return false
}
