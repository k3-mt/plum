# Go shim

The Go shim is not a file you install — it is generated. Go cannot be
instrumented by dropping in a file and setting an environment variable, so its
adapter implements `trace.Rewriter` (see `internal/lang/gopkg/instrument.go`):
`plum trace` hands it a scratch copy of the tree, it writes the `plumtrace`
runtime package into it (source in `internal/trace/shim_go.go`), and injects one
deferred probe at the top of each **changed** function:

```go
defer plumtrace.Enter("internal/auth/cache.go::Cache.Get",
    plumtrace.KV{"key", key})(&token, &err)
```

Three properties matter:

- **Only the changed set is instrumented.** The AST pass already named it.
- **The repository is never written to.** Instrumentation is a property of a
  trace run, not of the code under audit.
- **Panics are observed, not swallowed.** The deferred half calls `recover()`,
  emits a `raise` event, and re-panics with the same value.

Unnamed results are given generated names (`plumR0`) so the return value the
caller actually saw can be recorded. Naming a result never changes behaviour.
