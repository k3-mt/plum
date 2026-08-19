package trace

import _ "embed"

// PythonShimSource is the sys.monitoring shim, written into a scratch copy of
// the repository at trace time. It speaks the same JSONL Event schema as the Go
// and Node shims, which is why the core ingests all three identically (§4.2).
//
//go:embed shim_assets/plum_shim.py
var PythonShimSource string

// PythonSiteCustomize is imported automatically by CPython at startup when the
// shim directory is on PYTHONPATH — the attachment point that reaches pytest,
// unittest and plain scripts without any of them knowing plum exists.
//
//go:embed shim_assets/sitecustomize.py
var PythonSiteCustomize string
