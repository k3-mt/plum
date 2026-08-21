package probe

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/lang"
)

// Test is one runnable test the window can be pointed at.
type Test struct {
	Name    string `json:"name"`
	File    string `json:"file"`
	Package string `json:"package"`
	// Command runs this test alone. Empty means plum could not narrow the
	// configured test command to it, which is said rather than papered over: a
	// run that quietly executed the whole suite would still draw a picture.
	Command string `json:"command"`
	// Handle is the probe already minted for this test, when there is one.
	Handle string `json:"handle,omitempty"`
	// Doc is the test's own comment: what it says it is checking. A name has to
	// be a name, and the sentence above it is usually the thing you are actually
	// scanning for when you are looking for the test that covers something.
	Doc string `json:"doc,omitempty"`
}

// Discover finds the tests in a repository by parsing its test files.
//
// It reads the working tree rather than a session bundle, because the test you
// want to watch is usually the one just written, and no capture has seen that.
//
// What counts as a test is a naming convention, and the conventions it knows are
// the declaration-shaped ones: Go's TestXxx, Python's test_xxx, and exported
// test functions in JavaScript and TypeScript. Tests declared by calling into a
// framework — `it("...")`, `describe(...)`, table entries — are not declarations
// and will not be found. That is a real limit, and the window says so rather
// than presenting a short list as a complete one.
func Discover(root string, reg *lang.Registry, testCommand string) ([]Test, error) {
	var out []Test
	seen := map[string]bool{}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // an unreadable corner is not a reason to find nothing
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		if info.IsDir() {
			if skipDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !bundle.IsTestPath(rel) || info.Size() > 4<<20 {
			return nil
		}
		a := reg.For(rel)
		if a == nil {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		syms, rerr := a.ParseSymbols(rel, src)
		if rerr != nil {
			return nil
		}
		for _, sym := range syms {
			if !isTestName(a.Name(), sym) || seen[sym.Name] {
				continue
			}
			seen[sym.Name] = true
			dir := filepath.Dir(rel)
			cmd, _ := ScopeCommand(testCommand, sym.Name, dir)
			out = append(out, Test{
				Name: sym.Name, File: rel, Package: dir, Command: cmd,
				Doc: firstSentence(sym.Doc),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Pair each with its handle, if one has been minted. A test you have watched
	// before is one you are likely to want again.
	if ps, perr := List(root); perr == nil {
		byTest := map[string]string{}
		for _, p := range ps {
			byTest[p.Test] = p.Handle()
		}
		for i := range out {
			out[i].Handle = byTest[out[i].Name]
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Package != out[j].Package {
			return out[i].Package < out[j].Package
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func skipDir(name string) bool {
	switch name {
	case "node_modules", "vendor", "dist", "build", "target", "__pycache__", ".git":
		return true
	}
	return strings.HasPrefix(name, ".") && name != "."
}

// isTestName applies the convention of the language the file is in. A shared
// rule would be wrong in both directions: Go's TestingHelper is not a test, and
// Python's testHelper is not one either, but for different reasons.
func isTestName(language string, sym bundle.Symbol) bool {
	if sym.Kind != "func" && sym.Kind != "method" {
		return false
	}
	name := sym.Name
	switch language {
	case "go":
		// The shapes `go test` will actually run. A method is never one of them.
		if sym.Kind == "method" || strings.Contains(name, ".") {
			return false
		}
		for _, prefix := range []string{"Test", "Benchmark", "Fuzz"} {
			rest, ok := strings.CutPrefix(name, prefix)
			// TestingHelper is not a test: the character after the prefix has to
			// start a new word.
			if ok && (rest == "" || rest[0] < 'a' || rest[0] > 'z') {
				return true
			}
		}
		return false
	case "python":
		return strings.HasPrefix(name, "test_") || strings.HasPrefix(name, "test")
	case "javascript", "typescript":
		return strings.HasPrefix(name, "test") || strings.HasPrefix(name, "should")
	}
	return false
}

// firstSentence is as much of a doc comment as belongs on one row. The rest is
// in the code, which is one click away.
func firstSentence(doc string) string {
	doc = strings.Join(strings.Fields(doc), " ")
	if i := strings.Index(doc, ". "); i >= 0 {
		doc = doc[:i+1]
	}
	if len(doc) > 150 {
		doc = doc[:149] + "\u2026"
	}
	return doc
}
