package bundle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Lookup returns the symbol with the given ID. The zero Symbol is returned when
// the ID is unknown, so callers can render a placeholder frame rather than nil-check.
func (b *Bundle) Lookup(id SymbolID) Symbol {
	for i := range b.Symbols {
		if b.Symbols[i].ID == id {
			return b.Symbols[i]
		}
	}
	return Symbol{ID: id, Name: shortName(id), File: fileOf(id)}
}

// Has reports whether the ID names a symbol captured in this session.
func (b *Bundle) Has(id SymbolID) bool {
	for i := range b.Symbols {
		if b.Symbols[i].ID == id {
			return true
		}
	}
	return false
}

func (b *Bundle) HasRisk(id SymbolID) bool {
	for _, r := range b.RiskMarkers {
		if r.Symbol == id {
			return true
		}
	}
	return false
}

func (b *Bundle) RisksFor(id SymbolID) []RiskMarker {
	var out []RiskMarker
	for _, r := range b.RiskMarkers {
		if r.Symbol == id {
			out = append(out, r)
		}
	}
	return out
}

func (b *Bundle) EdgesFrom(id SymbolID) []Edge {
	var out []Edge
	for _, e := range b.Edges {
		if e.From == id {
			out = append(out, e)
		}
	}
	return out
}

func (b *Bundle) EdgesTo(id SymbolID) []Edge {
	var out []Edge
	for _, e := range b.Edges {
		if e.To == id {
			out = append(out, e)
		}
	}
	return out
}

// JournalFor returns journal entries whose tool call touched the symbol's file.
// File-level is the finest granularity a tool-call hook can honestly claim.
func (b *Bundle) JournalFor(id SymbolID) []JournalEntry {
	f := fileOf(id)
	var out []JournalEntry
	for _, j := range b.Journal {
		if j.File == f || strings.HasSuffix(j.File, "/"+f) {
			out = append(out, j)
		}
	}
	return out
}

// CallSiteComment returns the contiguous comment block immediately preceding the
// call to callee inside caller, or "" if the call is unannotated (spec §9.4).
// An unannotated expensive barrier is a first-class finding.
func (b *Bundle) CallSiteComment(caller, callee SymbolID) string {
	s := b.Lookup(caller)
	for _, cs := range s.CallSites {
		if cs.Callee == callee {
			return cs.Rationale
		}
	}
	// Fall back to matching on the bare name: cross-file resolution is best effort,
	// so a call recorded as "Get" may trace back as "internal/x.go::Cache.Get".
	want := shortName(callee)
	for _, cs := range s.CallSites {
		if cs.CalleeRaw == want || shortName(cs.Callee) == want {
			return cs.Rationale
		}
	}
	return ""
}

// Fingerprints maps every captured symbol to its fingerprint, for staleness (P5).
func (b *Bundle) Fingerprints() map[SymbolID]string {
	m := make(map[SymbolID]string, len(b.Symbols))
	for _, s := range b.Symbols {
		m[s.ID] = s.Fingerprint
	}
	return m
}

func (b *Bundle) Sort() {
	sort.Slice(b.Symbols, func(i, j int) bool {
		if b.Symbols[i].File != b.Symbols[j].File {
			return b.Symbols[i].File < b.Symbols[j].File
		}
		return b.Symbols[i].LineStart < b.Symbols[j].LineStart
	})
	sort.Slice(b.Files, func(i, j int) bool { return b.Files[i].Path < b.Files[j].Path })
	sort.Slice(b.Edges, func(i, j int) bool {
		if b.Edges[i].From != b.Edges[j].From {
			return b.Edges[i].From < b.Edges[j].From
		}
		return b.Edges[i].To < b.Edges[j].To
	})
	sort.Slice(b.RiskMarkers, func(i, j int) bool {
		if b.RiskMarkers[i].File != b.RiskMarkers[j].File {
			return b.RiskMarkers[i].File < b.RiskMarkers[j].File
		}
		return b.RiskMarkers[i].Line < b.RiskMarkers[j].Line
	})
}

// MakeID builds the canonical join key for a declaration.
func MakeID(relpath, qualified string) SymbolID {
	return SymbolID(filepath.ToSlash(relpath) + "::" + qualified)
}

func fileOf(id SymbolID) string {
	if i := strings.Index(string(id), "::"); i >= 0 {
		return string(id)[:i]
	}
	return ""
}

func shortName(id SymbolID) string {
	s := string(id)
	if i := strings.Index(s, "::"); i >= 0 {
		s = s[i+2:]
	}
	return s
}

// File returns the repo-relative path component of a SymbolID.
func (id SymbolID) File() string { return fileOf(id) }

// Qualified returns the "Type.Method" component of a SymbolID.
func (id SymbolID) Qualified() string { return shortName(id) }

func Write(path string, b *Bundle) error {
	b.SchemaVersion = SchemaVersion
	b.Sort()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func Read(path string) (*Bundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if maj(b.SchemaVersion) > maj(SchemaVersion) {
		return nil, fmt.Errorf("%s: bundle schema %s is newer than this binary (%s)", path, b.SchemaVersion, SchemaVersion)
	}
	return &b, nil
}

func maj(v string) string {
	if i := strings.Index(v, "."); i >= 0 {
		return v[:i]
	}
	return v
}
