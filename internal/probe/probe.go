// Package probe is a handle on one thing you are trying to get right.
//
// The rest of plum is addressed to a session: everything that changed between
// two commits, read after the fact. A probe is addressed to a single test, and
// it exists because the loop it serves runs the other way round — you have just
// written something, you want to watch it run, you change it, you want to watch
// it again, and you want that to happen without being asked anything in between.
//
// Naming the target once and passing a short handle around is what makes that
// possible. The handle is what an agent hands back when it writes the test, and
// what the window is pointed at. It is derived from the test's name rather than
// allocated, so probing the same test twice is the same probe rather than two.
//
// Probes live in the repository, not the state dir. A probe describes a test
// that is in the code; it means the same thing for everyone who checks the code
// out, which is exactly what the state dir is not for.
package probe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	dir    = ".plum/probes"
	prefix = "plum:"
)

type Probe struct {
	ID   string `json:"id"`
	Test string `json:"test"`
	// Command runs just this test. Stored rather than derived on the fly so it
	// is visible, editable, and cannot change under you when a default does.
	Command string `json:"command"`
	// Fixture is repo-relative sample data the test reads. plum does not make a
	// test read it — the test has to be written that way — but recording the
	// pairing is what lets the window put the data next to the run.
	Fixture string    `json:"fixture,omitempty"`
	Created time.Time `json:"created"`
}

func (p *Probe) Handle() string { return prefix + p.ID }

// idFor is the test's name, not a counter. Probing the same test twice has to
// land on the same handle, or the one an agent printed last week stops working.
func idFor(test string) string {
	sum := sha256.Sum256([]byte(test))
	return hex.EncodeToString(sum[:])[:4]
}

// Mint records a probe and returns it. Re-probing the same test overwrites in
// place, which keeps a handle that has already been passed around valid.
func Mint(root, test, command, fixture string) (*Probe, error) {
	if strings.TrimSpace(test) == "" {
		return nil, fmt.Errorf("a probe needs a test to point at")
	}
	p := &Probe{
		ID: idFor(test), Test: test, Command: command,
		Fixture: fixture, Created: time.Now().UTC(),
	}
	// A four-character handle is short enough to say out loud and has room for
	// far more probes than a repository will ever hold — but "far more" is not
	// "all", so a collision is detected rather than assumed away.
	if existing, err := load(root, p.ID); err == nil && existing.Test != test {
		p.ID = hex.EncodeToString(sha256Sum(test))[:8]
	}
	return p, p.save(root)
}

func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func (p *Probe) save(root string) error {
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d, p.ID+".json"), append(data, '\n'), 0o644)
}

// Load accepts the handle as printed, with or without its prefix. Somebody will
// paste one of each.
func Load(root, handle string) (*Probe, error) {
	id := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(handle), prefix))
	if id == "" {
		return nil, fmt.Errorf("no probe handle given")
	}
	p, err := load(root, id)
	if err != nil {
		return nil, fmt.Errorf("no probe %q — `plum probe -test <name>` mints one, `plum probe list` shows them", handle)
	}
	return p, nil
}

func load(root, id string) (*Probe, error) {
	data, err := os.ReadFile(filepath.Join(root, dir, id+".json"))
	if err != nil {
		return nil, err
	}
	var p Probe
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func List(root string) ([]*Probe, error) {
	entries, err := os.ReadDir(filepath.Join(root, dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Probe
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if p, err := load(root, strings.TrimSuffix(e.Name(), ".json")); err == nil {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Test < out[j].Test })
	return out, nil
}

// ScopeCommand narrows a suite command to one test, and to the package that
// holds it when the caller knows which one that is.
//
// Narrowing the test alone is not enough to make this loop feel live. `go test
// -run X ./...` still builds every package under instrumentation: measured on
// this repository it was 7.5 seconds against 1.3 for the one package that
// actually holds the test. The difference is what decides whether saving a file
// gives you an answer or a wait.
//
// It knows the runners plum supports and nothing else. When it does not
// recognise one it returns the command untouched and says so, because the
// failure it is avoiding is silent: a "single test" run that quietly ran the
// whole suite would still draw a picture, and the picture would not be of what
// the window says it is showing.
func ScopeCommand(base, test, pkgDir string) (string, bool) {
	fields := strings.Fields(base)
	if len(fields) == 0 || test == "" {
		return base, false
	}
	switch {
	case fields[0] == "go" && len(fields) > 1 && fields[1] == "test":
		// -count=1 defeats the test cache, and without it this whole loop is a
		// lie. Go caches a successful result against the content of the tree it
		// ran on, so running the same probe twice returns "(cached)" in a few
		// milliseconds, executes nothing, records no events, and the window
		// draws an empty picture of a test that passed. Anchored too, or
		// TestCache would also run TestCacheEvictsUnderPressure.
		out := insertAfter(fields, 2, "-count=1", "-run", "^"+test+"$")
		if pkgDir != "" && pkgDir != "." {
			out = strings.Replace(out, "./...", "./"+strings.Trim(pkgDir, "/")+"/", 1)
		}
		return out, true
	case strings.Contains(fields[0], "pytest"), len(fields) > 1 && fields[1] == "pytest":
		if pkgDir != "" {
			return base + " " + pkgDir + " -k " + test, true
		}
		return base + " -k " + test, true
	case strings.Contains(base, "jest"), strings.Contains(base, "vitest"):
		return base + " -t " + test, true
	}
	return base, false
}

func insertAfter(fields []string, at int, extra ...string) string {
	if at > len(fields) {
		at = len(fields)
	}
	out := append([]string{}, fields[:at]...)
	out = append(out, extra...)
	out = append(out, fields[at:]...)
	return strings.Join(out, " ")
}
