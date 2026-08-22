package bundle

import "testing"

// Four packages had grown their own copy of this and they had drifted: one knew
// about .test.js and the others did not, so the same file counted as a test in
// one place and as production code in another.
func TestIsTestPathKnowsEveryConventionThePackagesHadSeparately(t *testing.T) {
	for _, path := range []string{
		"internal/server/server_test.go", "server_test.go",
		"tests/test_cache.py", "cache_test.py",
		"src/cache.test.ts", "src/cache.test.js",
		"src/cache.spec.ts", "src/cache.spec.js",
		`windows\path\thing_test.go`,
	} {
		if !IsTestPath(path) {
			t.Errorf("IsTestPath(%q) = false", path)
		}
	}
	for _, path := range []string{
		"internal/server/server.go", "cache.py", "src/cache.ts",
		"contest.go", "latest.js", "protest_helper.go",
		// A compiled test in __pycache__ is not the test — narrowing a probe to
		// it collects nothing.
		"tests/__pycache__/test_cache.cpython-313-pytest-9.1.1.pyc",
		"tests/__pycache__/test_cache.cpython-313.pyc",
	} {
		if IsTestPath(path) {
			t.Errorf("IsTestPath(%q) = true", path)
		}
	}
}
