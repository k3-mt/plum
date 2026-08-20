package server

import (
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/k3-mt/plum/internal/bundle"
)

// The meter is measured against a bundle, and a bundle is a photograph. Between
// the agent writing a line and the Stop hook capturing it there is a window —
// often the most interesting minute of the whole session — in which the number
// would otherwise sit perfectly still.
//
// So the window also asks the files. For every file the capture named, the
// working tree is re-parsed and its symbols' fingerprints compared with the
// ones recorded. What has moved is code that exists and has never been captured,
// let alone read. It is reported separately from the unmet count rather than
// added to it: one is measured against a recording and the other against the
// disk this second, and a number that quietly mixed the two could not be checked
// against anything.
//
// This is the same comparison `plum stale` makes for claims, over the changed
// set instead of over claims.yaml.

// driftFileBudget bounds the work. A first capture in a repository names every
// file in it, and re-parsing all of them on a watcher tick is not a meter, it is
// a build. Past the budget the drift is reported as unmeasured rather than as
// zero — no silent caps.
const driftFileBudget = 60

type driftCache struct {
	mu     sync.Mutex
	digest string
	count  int
	why    string
}

// drift counts symbols the working tree no longer agrees with the capture about.
// The caller holds at least a read lock on the session state.
func (s *Server) drift() (int, string) {
	if s.Adapters == nil || s.Bundle == nil {
		return 0, ""
	}
	files := map[string]bool{}
	for _, sym := range s.Bundle.Symbols {
		if sym.File != "" && sym.Fingerprint != "" {
			files[sym.File] = true
		}
	}
	if len(files) == 0 {
		return 0, ""
	}
	if len(files) > driftFileBudget {
		return 0, "the capture names too many files to re-read on a timer"
	}

	paths := make([]string, 0, len(files))
	for f := range files {
		paths = append(paths, filepath.Join(s.Cfg.Root, f))
	}
	sort.Strings(paths)

	// Size and modification time, the same cheap question the watcher asks. The
	// page requests the meter on every click and on every reload; parsing the
	// changed set each time would make reading a symbol cost more than drawing
	// the landscape did.
	key := s.digest(paths)
	s.drifted.mu.Lock()
	defer s.drifted.mu.Unlock()
	if key == s.drifted.digest {
		return s.drifted.count, s.drifted.why
	}

	current := map[bundle.SymbolID]string{}
	for f := range files {
		a := s.Adapters.For(f)
		if a == nil {
			continue // a language plum cannot parse cannot be said to have drifted
		}
		src, err := os.ReadFile(filepath.Join(s.Cfg.Root, f))
		if err != nil {
			continue
		}
		syms, err := a.ParseSymbols(f, src)
		if err != nil {
			continue
		}
		for _, sym := range syms {
			current[sym.ID] = sym.Fingerprint
		}
	}

	count := 0
	for _, sym := range s.Bundle.Symbols {
		if sym.Fingerprint == "" {
			continue
		}
		fp, ok := current[sym.ID]
		if !ok {
			continue // deleted, renamed, or in a file nothing could parse
		}
		if fp != sym.Fingerprint {
			count++
		}
	}
	s.drifted.digest, s.drifted.count, s.drifted.why = key, count, ""
	return count, ""
}
