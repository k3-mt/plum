// Package stale re-fingerprints claims against the working tree. Docs are
// addressed to content, not files: when a subject's AST fingerprint changes, the
// claim about it is automatically suspect (P5).
package stale

import (
	"os"
	"path/filepath"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/claims"
	"github.com/kelalaike/plum/internal/config"
	"github.com/kelalaike/plum/internal/lang"
)

type Finding struct {
	ID     string
	Claim  string
	Symbol bundle.SymbolID
	Reason string
}

// Check reads claims.yaml and compares each claim's recorded fingerprint with
// the symbol's fingerprint in the working tree right now.
func Check(cfg *config.Config, reg *lang.Registry, claimsPath string) ([]Finding, error) {
	cs, err := claims.Load(claimsPath)
	if err != nil {
		return nil, err
	}
	current := map[bundle.SymbolID]string{}
	parsed := map[string]bool{}
	for _, c := range cs {
		file := c.Symbol.File()
		if file == "" || parsed[file] {
			continue
		}
		parsed[file] = true
		a := reg.For(file)
		if a == nil {
			continue
		}
		src, err := os.ReadFile(filepath.Join(cfg.Root, file))
		if err != nil {
			continue
		}
		syms, err := a.ParseSymbols(file, src)
		if err != nil {
			continue
		}
		for _, s := range syms {
			current[s.ID] = s.Fingerprint
		}
	}

	var out []Finding
	for _, c := range cs {
		if c.Fingerprint == "" {
			continue
		}
		fp, ok := current[c.Symbol]
		switch {
		case !ok:
			out = append(out, Finding{c.ID, c.Claim, c.Symbol, "symbol no longer exists at this path"})
		case fp != c.Fingerprint:
			out = append(out, Finding{c.ID, c.Claim, c.Symbol, "fingerprint moved since the claim was written"})
		}
	}
	return out, nil
}
