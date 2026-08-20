// Package claims reads and writes claims.yaml — every assertion synthesis makes,
// tagged executable or not. A non-executable claim is a trust-me assertion, and
// that is exactly where attention should go (spec §8.2, §12).
package claims

import (
	"fmt"
	"os"
	"strings"

	"github.com/k3-mt/plum/internal/bundle"
)

type Claim struct {
	ID          string          `json:"id"`
	Claim       string          `json:"claim"`
	Symbol      bundle.SymbolID `json:"symbol"`
	Executable  bool            `json:"executable"`
	Test        string          `json:"test"`
	Fingerprint string          `json:"fingerprint"` // captured at write time — drives staleness (P5)
}

// Parse reads the small YAML subset the spec uses: a top-level sequence of
// mappings with scalar values and one block scalar (`test: |`). Hand-rolled so
// the binary keeps zero dependencies.
func Parse(src string) ([]Claim, error) {
	var out []Claim
	var cur *Claim
	var block *strings.Builder
	var blockIndent int

	flush := func() {
		if cur != nil {
			if block != nil {
				cur.Test = strings.TrimRight(block.String(), "\n")
				block = nil
			}
			out = append(out, *cur)
			cur = nil
		}
	}

	for _, raw := range strings.Split(src, "\n") {
		if block != nil {
			indent := len(raw) - len(strings.TrimLeft(raw, " "))
			if strings.TrimSpace(raw) != "" && indent < blockIndent {
				cur.Test = strings.TrimRight(block.String(), "\n")
				block = nil
			} else {
				if len(raw) >= blockIndent {
					raw = raw[blockIndent:]
				} else {
					raw = strings.TrimSpace(raw)
				}
				block.WriteString(raw + "\n")
				continue
			}
		}
		line := strings.TrimRight(raw, " ")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") {
			flush()
			cur = &Claim{}
			trimmed = strings.TrimPrefix(trimmed, "- ")
		}
		if cur == nil {
			continue
		}
		key, val, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if i := strings.Index(val, " #"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		switch key {
		case "id":
			cur.ID = unquote(val)
		case "claim":
			cur.Claim = unquote(val)
		case "symbol":
			cur.Symbol = bundle.SymbolID(unquote(val))
		case "fingerprint":
			cur.Fingerprint = unquote(val)
		case "executable":
			cur.Executable = val == "true"
		case "test":
			if val == "|" || val == "|-" {
				block = &strings.Builder{}
				blockIndent = len(line) - len(strings.TrimLeft(line, " ")) + 2
			} else {
				cur.Test = unquote(val)
			}
		}
	}
	flush()
	return out, nil
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

func Render(cs []Claim) string {
	var w strings.Builder
	w.WriteString("# Claims extracted by `plum synth`. Fingerprints are captured at write\n")
	w.WriteString("# time: when a subject's fingerprint moves, its claims go stale (P5).\n")
	for _, c := range cs {
		fmt.Fprintf(&w, "- id: %s\n", c.ID)
		fmt.Fprintf(&w, "  claim: %q\n", c.Claim)
		fmt.Fprintf(&w, "  symbol: %q\n", string(c.Symbol))
		fmt.Fprintf(&w, "  fingerprint: %q\n", c.Fingerprint)
		fmt.Fprintf(&w, "  executable: %t\n", c.Executable)
		if strings.TrimSpace(c.Test) != "" {
			w.WriteString("  test: |\n")
			for _, l := range strings.Split(strings.TrimRight(c.Test, "\n"), "\n") {
				w.WriteString("    " + l + "\n")
			}
		}
	}
	return w.String()
}

func Load(path string) ([]Claim, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(string(data))
}

func Save(path string, cs []Claim) error {
	return os.WriteFile(path, []byte(Render(cs)), 0o644)
}
