// Package conf makes configuration a first-class citizen of the bundle.
//
// A YAML key is not code, but changing one changes behaviour exactly as surely
// as editing a function — and it does so invisibly, because no compiler and no
// test signature moves. So config keys become symbols with the same SymbolID
// shape as everything else ("config/app.yaml::server.timeout"), which means they
// can carry fingerprints, appear in the public-surface diff, be claimed about,
// go stale, and be linked to the code that reads them.
//
// The parsers here are deliberately small and structural: enough to find keys,
// values, line spans and the comment written above them. They are not full YAML
// or TOML implementations and do not pretend to be.
package conf

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/trace"
)

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return "config" }

func (a *Adapter) Extensions() []string {
	return []string{".yaml", ".yml", ".toml", ".json", ".ini", ".cfg", ".env", ".properties"}
}

// Key is one setting: its dotted path, its value as written, where it lives,
// and the comment block above it.
type Key struct {
	Path    string
	Value   string
	Line    int
	Comment string
	Secret  bool
}

// Parse dispatches on extension. An unparseable file yields no keys rather than
// an error: a config file this pass cannot read is not a reason to fail a session.
func Parse(path string, src []byte) []Key {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return parseJSON(src)
	case ".env", ".properties":
		return parseEnvFile(src)
	case ".ini", ".cfg":
		return parseINI(src)
	default: // .yaml, .yml, .toml
		if strings.EqualFold(filepath.Ext(path), ".toml") {
			return parseTOMLish(src)
		}
		return parseYAML(src)
	}
}

func (a *Adapter) ParseSymbols(path string, src []byte) ([]bundle.Symbol, error) {
	rel := filepath.ToSlash(path)
	offsets := lineOffsets(src)
	var out []bundle.Symbol
	for _, k := range Parse(path, src) {
		value := k.Value
		if k.Secret {
			value = "<redacted>"
		}
		out = append(out, bundle.Symbol{
			ID:        bundle.MakeID(rel, k.Path),
			Kind:      "config_key",
			Name:      k.Path,
			File:      rel,
			LineStart: k.Line,
			LineEnd:   k.Line,
			ByteStart: offsetOf(offsets, k.Line-1),
			ByteEnd:   offsetOf(offsets, k.Line),
			Signature: k.Path + " = " + value,
			Doc:       k.Comment,
			Exported:  true, // every setting is somebody's interface
			// The value is the whole content of a key: if it changes, every
			// claim about the behaviour it configures is suspect.
			Fingerprint: hash(k.Path + "\x00" + k.Value),
		})
	}
	return out, nil
}

// PublicSurface treats every key as surface. A changed default is a silent
// behaviour change, and it is exactly the kind nobody reviews.
func (a *Adapter) PublicSurface(path string, src []byte) ([]bundle.SurfaceItem, error) {
	rel := filepath.ToSlash(path)
	var out []bundle.SurfaceItem
	for _, k := range Parse(path, src) {
		value := k.Value
		if k.Secret {
			value = "<redacted>"
		}
		out = append(out, bundle.SurfaceItem{
			Kind: "config_key", Name: filepath.Base(rel) + ":" + k.Path, File: rel,
			Signature: value, Symbol: bundle.MakeID(rel, k.Path),
		})
	}
	return out, nil
}

func (a *Adapter) RiskMarkers(path string, src []byte, syms []bundle.Symbol) ([]bundle.RiskMarker, error) {
	rel := filepath.ToSlash(path)
	changed := map[bundle.SymbolID]bool{}
	for _, s := range syms {
		changed[s.ID] = true
	}
	var out []bundle.RiskMarker
	for _, k := range Parse(path, src) {
		id := bundle.MakeID(rel, k.Path)
		if len(changed) > 0 && !changed[id] {
			continue
		}
		for _, m := range predicates(k) {
			out = append(out, bundle.RiskMarker{Kind: m.kind, Symbol: id, File: rel, Line: k.Line, Note: m.note})
		}
	}
	return out, nil
}

type marker struct{ kind, note string }

func predicates(k Key) []marker {
	var out []marker
	lower := strings.ToLower(k.Path)
	value := strings.Trim(strings.ToLower(k.Value), `"'`)

	if k.Secret && value != "" && !isReference(k.Value) {
		out = append(out, marker{"hardcoded_secret",
			"a literal value under a key named " + k.Path + " — if this is a real credential it is now in git history"})
	}
	if (strings.Contains(lower, "debug") || strings.Contains(lower, "verbose")) && (value == "true" || value == "1" || value == "on") {
		out = append(out, marker{"debug_enabled",
			k.Path + " is on — debug paths log more, check less, and are rarely what you want deployed"})
	}
	if value == "*" || value == "0.0.0.0" || strings.Contains(value, "://*") {
		out = append(out, marker{"wildcard_binding",
			k.Path + " is " + k.Value + " — this permits or listens to everything, not something"})
	}
	if strings.Contains(lower, "verify") && (value == "false" || value == "off" || value == "no") {
		out = append(out, marker{"verification_disabled",
			k.Path + " turns off verification — TLS and signature checks fail open from here"})
	}
	if (strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline")) && (value == "0" || value == "none" || value == "null") {
		out = append(out, marker{"timeout_disabled",
			k.Path + " is " + k.Value + " — a call configured this way waits forever"})
	}
	return out
}

// isReference spots a value that defers to somewhere else (${VAR}, vault paths,
// env interpolation) — those are not hardcoded secrets, they are the fix for one.
func isReference(v string) bool {
	v = strings.Trim(v, `"'`)
	return strings.HasPrefix(v, "${") || strings.HasPrefix(v, "$") ||
		strings.HasPrefix(v, "vault:") || strings.HasPrefix(v, "secret:") ||
		strings.Contains(v, "{{") || v == "" || strings.EqualFold(v, "changeme")
}

// CallEdges: configuration calls nothing. The edges that matter run the other
// way, from code to config, and are built by the extractor's linking pass.
func (a *Adapter) CallEdges(path string, src []byte) ([]bundle.Edge, error) { return nil, nil }

func (a *Adapter) Comments(path string, src []byte) ([]bundle.Comment, error) {
	var out []bundle.Comment
	for i, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			text := strings.TrimSpace(strings.TrimLeft(trimmed, "#/"))
			out = append(out, bundle.Comment{Text: text, LineStart: i + 1, LineEnd: i + 1})
		}
	}
	return out, nil
}

func (a *Adapter) Normalise(src []byte) ([]byte, error) {
	var kept []string
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		kept = append(kept, strings.Join(strings.Fields(trimmed), " "))
	}
	return []byte(strings.Join(kept, "\n")), nil
}

func (a *Adapter) ShimSpec(syms []bundle.SymbolID) (trace.ShimSpec, error) {
	// Configuration is read, not executed. There is nothing to instrument.
	return trace.ShimSpec{Language: "config", Mode: "none"}, nil
}

// ---------------------------------------------------------------- parsers

// parseYAML walks indentation to build dotted key paths. Enough for the shape
// of real config files: nested maps, scalars, and list items counted by index.
func parseYAML(src []byte) []Key {
	var out []Key
	var stack []frame
	var comment []string
	listIdx := map[int]int{}

	for i, raw := range strings.Split(string(src), "\n") {
		line := strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "---" {
			comment = nil
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			comment = append(comment, strings.TrimSpace(strings.TrimPrefix(trimmed, "#")))
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		for k := range listIdx {
			if k > indent {
				delete(listIdx, k)
			}
		}

		if strings.HasPrefix(trimmed, "- ") {
			idx := listIdx[indent]
			listIdx[indent] = idx + 1
			item := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
			path := join(stack, fmt.Sprintf("[%d]", idx))
			if key, value, ok := splitKV(item); ok {
				out = append(out, mkKey(join(stack, fmt.Sprintf("[%d]", idx))+"."+key, value, i+1, comment))
			} else {
				out = append(out, mkKey(path, item, i+1, comment))
			}
			comment = nil
			continue
		}

		key, value, ok := splitKV(trimmed)
		if !ok {
			comment = nil
			continue
		}
		if value == "" {
			stack = append(stack, frame{indent: indent, name: key})
			comment = nil
			continue
		}
		out = append(out, mkKey(join(stack, key), value, i+1, comment))
		comment = nil
	}
	return out
}

// parseTOMLish reads [table] headers and key = value pairs.
func parseTOMLish(src []byte) []Key {
	var out []Key
	table := ""
	var comment []string
	for i, raw := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			comment = nil
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			comment = append(comment, strings.TrimSpace(strings.TrimPrefix(trimmed, "#")))
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			table = strings.Trim(trimmed, "[]")
			comment = nil
			continue
		}
		if key, value, ok := splitKV(stripInlineComment(trimmed)); ok {
			path := key
			if table != "" {
				path = table + "." + key
			}
			out = append(out, mkKey(path, value, i+1, comment))
		}
		comment = nil
	}
	return out
}

func parseINI(src []byte) []Key { return parseTOMLish(src) }

// parseEnvFile reads KEY=value lines, the shape of .env and .properties.
func parseEnvFile(src []byte) []Key {
	var out []Key
	var comment []string
	for i, raw := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			comment = nil
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			comment = append(comment, strings.TrimSpace(strings.TrimPrefix(trimmed, "#")))
			continue
		}
		trimmed = strings.TrimPrefix(trimmed, "export ")
		if key, value, ok := strings.Cut(trimmed, "="); ok {
			out = append(out, mkKey(strings.TrimSpace(key), strings.TrimSpace(value), i+1, comment))
		}
		comment = nil
	}
	return out
}

// parseJSON walks the decoded document, then finds each leaf's line by search.
// JSON has no comments, so nothing is lost by decoding properly.
func parseJSON(src []byte) []Key {
	var doc any
	if err := json.Unmarshal(src, &doc); err != nil {
		return nil
	}
	lines := strings.Split(string(src), "\n")
	var out []Key
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch t := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sortStrings(keys)
			for _, k := range keys {
				walk(joinPath(prefix, k), t[k])
			}
		case []any:
			for i, item := range t {
				walk(fmt.Sprintf("%s[%d]", prefix, i), item)
			}
		default:
			leaf := prefix
			if i := strings.LastIndex(prefix, "."); i >= 0 {
				leaf = prefix[i+1:]
			}
			line := 0
			for i, l := range lines {
				if strings.Contains(l, `"`+leaf+`"`) {
					line = i + 1
					break
				}
			}
			out = append(out, mkKey(prefix, fmt.Sprintf("%v", v), line, nil))
		}
	}
	walk("", doc)
	return out
}

// ---------------------------------------------------------------- helpers

func mkKey(path, value string, line int, comment []string) Key {
	return Key{
		Path:    strings.TrimPrefix(path, "."),
		Value:   strings.TrimSpace(value),
		Line:    line,
		Comment: strings.Join(comment, "\n"),
		Secret:  looksSecret(path),
	}
}

func looksSecret(path string) bool {
	lower := strings.ToLower(path)
	for _, hint := range []string{"password", "passwd", "secret", "token", "api_key", "apikey", "private_key", "credential", "access_key"} {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

func splitKV(s string) (string, string, bool) {
	key, value, ok := strings.Cut(s, ":")
	if !ok || strings.Contains(key, "=") {
		key, value, ok = strings.Cut(s, "=")
	}
	if !ok {
		return "", "", false
	}
	key = strings.TrimSpace(strings.Trim(strings.TrimSpace(key), `"'`))
	if key == "" || strings.ContainsAny(key, " \t") && !strings.Contains(key, "_") {
		return "", "", false
	}
	return key, strings.TrimSpace(stripInlineComment(value)), true
}

func stripInlineComment(s string) string {
	inStr := byte(0)
	for i := 0; i < len(s); i++ {
		switch {
		case inStr != 0 && s[i] == inStr:
			inStr = 0
		case inStr == 0 && (s[i] == '"' || s[i] == '\''):
			inStr = s[i]
		case inStr == 0 && s[i] == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return strings.TrimSpace(s[:i])
		}
	}
	return s
}

// frame is one level of YAML nesting: how far it is indented, and its key.
type frame struct {
	indent int
	name   string
}

func join(stack []frame, key string) string {
	parts := make([]string, 0, len(stack)+1)
	for _, f := range stack {
		parts = append(parts, f.name)
	}
	parts = append(parts, key)
	return strings.Join(parts, ".")
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func lineOffsets(src []byte) []int {
	offs := []int{0}
	for i, b := range src {
		if b == '\n' {
			offs = append(offs, i+1)
		}
	}
	return append(offs, len(src))
}

func offsetOf(offs []int, line int) int {
	if line < 0 {
		return 0
	}
	if line >= len(offs) {
		return offs[len(offs)-1]
	}
	return offs[line]
}
