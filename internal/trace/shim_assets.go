package trace

import _ "embed"

// The shim sources are embedded so a single binary carries everything it needs.
// Each also exists under shims/ where it is meant to be read and edited;
// go:embed cannot reach outside its own package directory, so the copies here
// are kept in step by TestEmbeddedShimsMatchTheReadableOnes and `make shims`.

// PythonShimSource is the sys.monitoring shim.
//
//go:embed shim_assets/plum_shim.py
var PythonShimSource string

// PythonSiteCustomize is imported automatically by CPython at startup when the
// shim directory is on PYTHONPATH — the attachment point that reaches pytest,
// unittest and plain scripts without any of them knowing plum exists.
//
//go:embed shim_assets/sitecustomize.py
var PythonSiteCustomize string

// NodeShimSource is the --require preload: it wraps CommonJS exports, publishes
// the tracing runtime on globalThis, and registers the ESM loader.
//
//go:embed shim_assets/plum-shim.cjs
var NodeShimSource string

// NodeLoaderSource is the ESM module-customization hook. ES exports are live
// bindings with no object to mutate, so instrumentation is appended to the
// module source before it is evaluated.
//
//go:embed shim_assets/plum-loader.mjs
var NodeLoaderSource string
