package trace

import (
	"os"
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
