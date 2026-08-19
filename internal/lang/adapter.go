// Package lang is the seam that lets someone add Ruby without touching the
// engine (spec §4.1).
package lang

import (
	"path/filepath"
	"strings"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/trace"
)

type Adapter interface {
	Name() string
	Extensions() []string

	// ParseSymbols returns every declaration in the file with line spans,
	// signatures, docs, comments, call sites and normalised fingerprints.
	ParseSymbols(path string, src []byte) ([]bundle.Symbol, error)

	// PublicSurface reports exports, routes, env vars, CLI flags.
	PublicSurface(path string, src []byte) ([]bundle.SurfaceItem, error)

	// RiskMarkers runs language-specific AST predicates.
	RiskMarkers(path string, src []byte, syms []bundle.Symbol) ([]bundle.RiskMarker, error)

	// CallEdges resolves intra-file calls; cross-file resolution is best effort.
	CallEdges(path string, src []byte) ([]bundle.Edge, error)

	// Comments returns every comment with its line span, so the extractor can
	// bind comments to declarations and call sites (spec §9.4).
	Comments(path string, src []byte) ([]bundle.Comment, error)

	// Normalise strips comments and collapses whitespace while preserving
	// identifiers, so reformatting does not invalidate a fingerprint but a
	// rename or a logic change does (spec §6.4).
	Normalise(src []byte) ([]byte, error)

	// ShimSpec describes how to instrument this language for tracing (M2).
	ShimSpec(syms []bundle.SymbolID) (trace.ShimSpec, error)
}

// Registry maps file extensions to adapters.
type Registry struct {
	adapters []Adapter
}

func NewRegistry(as ...Adapter) *Registry { return &Registry{adapters: as} }

func (r *Registry) For(path string) Adapter {
	ext := strings.ToLower(filepath.Ext(path))
	for _, a := range r.adapters {
		for _, e := range a.Extensions() {
			if e == ext {
				return a
			}
		}
	}
	return nil
}

func (r *Registry) ByName(name string) Adapter {
	for _, a := range r.adapters {
		if a.Name() == name {
			return a
		}
	}
	return nil
}

func (r *Registry) All() []Adapter { return r.adapters }

// Language names the adapter that would handle a path, or "" when unsupported.
func (r *Registry) Language(path string) string {
	if a := r.For(path); a != nil {
		return a.Name()
	}
	return ""
}
