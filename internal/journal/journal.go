// Package journal captures rationale live. What was considered and rejected is
// unrecoverable from the diff (P3): journal during the run or lose it.
package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kelalaike/plum/internal/bundle"
)

// Append writes one entry as JSONL. Called by an agent hook, an editor
// integration, or a human typing `plum note`.
func Append(dir string, e bundle.JournalEntry) error {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, e.TS.Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(e)
}

// Harvest collects entries written at or after since. Best effort by design:
// a session with no journal still produces a bundle, just a degraded one (P7).
func Harvest(dir string, since time.Time) ([]bundle.JournalEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []bundle.JournalEntry
	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var e bundle.JournalEntry
			if json.Unmarshal([]byte(line), &e) != nil {
				continue
			}
			if e.TS.Before(since) {
				continue
			}
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out, nil
}
