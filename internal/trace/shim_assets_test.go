package trace

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Each shim exists twice: once under shims/ where it is meant to be read
// and edited, and once under internal/trace/shim_assets/ because go:embed cannot
// reach outside its own package directory. Nothing in the compiler stops those
// two copies drifting, and a drifted copy is the worst kind: the shipped binary
// would use a stale shim while the readable one looks current.
func TestEmbeddedShimsMatchTheReadableOnes(t *testing.T) {
	for _, tc := range []struct {
		readable string
		embedded string
	}{
		{"../../shims/python/plum_shim.py", PythonShimSource},
		{"../../shims/python/sitecustomize.py", PythonSiteCustomize},
		{"../../shims/node/plum-shim.cjs", NodeShimSource},
		{"../../shims/node/plum-loader.mjs", NodeLoaderSource},
	} {
		want, err := os.ReadFile(tc.readable)
		if err != nil {
			t.Fatal(err)
		}
		if string(want) != tc.embedded {
			t.Errorf("%s has drifted from the embedded copy.\nrun: cp %s internal/trace/shim_assets/", tc.readable, tc.readable)
		}
	}
}

// The Go shim is a string constant, so `go build` never looks at it: a syntax
// error or a missing import in it compiles perfectly well here and fails at
// trace time, in a scratch tree, as somebody else's build error. This compiles
// it as what it is.
func TestTheGoShimCompiles(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "plumtrace")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "shim.go"), []byte(GoShimSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module shimcheck\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the shim does not compile:\n%s", out)
	}
}
