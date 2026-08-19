// Package config reads .plum/config.toml. The parser is a deliberately small
// TOML subset (tables, strings, bools, numbers, string arrays) so that the whole
// tool keeps zero third-party dependencies and CGO_ENABLED=0 stays trivially true.
package config

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// Value is one scalar or list read from the file.
type Value struct {
	Str  string
	Bool bool
	Num  float64
	List []string
	Kind string // string | bool | number | list
	Line int
}

// Doc is a parsed TOML document keyed by "table.key".
type Doc map[string]Value

func ParseTOML(src string) (Doc, error) {
	doc := Doc{}
	sc := bufio.NewScanner(strings.NewReader(src))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	table := ""
	ln := 0
	for sc.Scan() {
		ln++
		line := strings.TrimSpace(stripComment(sc.Text()))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			table = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			return nil, fmt.Errorf("line %d: expected key = value, got %q", ln, line)
		}
		key := strings.TrimSpace(line[:eq])
		raw := strings.TrimSpace(line[eq+1:])
		v, err := parseValue(raw)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", ln, err)
		}
		v.Line = ln
		full := key
		if table != "" {
			full = table + "." + key
		}
		doc[full] = v
	}
	return doc, sc.Err()
}

// stripComment removes a trailing # comment that is not inside a string literal.
func stripComment(s string) string {
	inStr := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inStr = !inStr
		case '#':
			if !inStr {
				return s[:i]
			}
		}
	}
	return s
}

func parseValue(raw string) (Value, error) {
	switch {
	case strings.HasPrefix(raw, "["):
		if !strings.HasSuffix(raw, "]") {
			return Value{}, fmt.Errorf("multi-line arrays are not supported: %q", raw)
		}
		inner := strings.TrimSpace(raw[1 : len(raw)-1])
		v := Value{Kind: "list"}
		if inner == "" {
			return v, nil
		}
		for _, part := range splitTop(inner) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			s, err := unquote(part)
			if err != nil {
				return Value{}, err
			}
			v.List = append(v.List, s)
		}
		return v, nil
	case strings.HasPrefix(raw, `"`):
		s, err := unquote(raw)
		return Value{Kind: "string", Str: s}, err
	case raw == "true" || raw == "false":
		return Value{Kind: "bool", Bool: raw == "true"}, nil
	default:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return Value{}, fmt.Errorf("unsupported value %q", raw)
		}
		return Value{Kind: "number", Num: f}, nil
	}
}

func splitTop(s string) []string {
	var out []string
	depth, inStr, start := 0, false, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inStr = !inStr
		case '[':
			if !inStr {
				depth++
			}
		case ']':
			if !inStr {
				depth--
			}
		case ',':
			if !inStr && depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

func unquote(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strconv.Unquote(s)
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return s[1 : len(s)-1], nil
	}
	return "", fmt.Errorf("expected quoted string, got %q", s)
}
