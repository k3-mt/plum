# plum

**A comprehension-debt system for agent-written code.**

When an agent builds a feature, the code may work while your mental model of the
repository silently goes stale. `plum` extracts mechanical evidence about what
changed, synthesises it without inherited context, lets you **explore** the
result as a navigable energy landscape, and only then interrogates you against
recorded execution.

The deliverable is not documentation. The deliverable is a correctly updated
model in the developer's head.

Built to the specification in [`BUILD (1).md`](<BUILD (1).md>).

---

## Install

```sh
make build            # -> bin/plum, a single static binary, CGO_ENABLED=0
make install          # -> ~/.local/bin/plum
```

No third-party dependencies. Go 1.23+ and `git` on PATH is the whole story.

## The loop

```sh
plum init                       # write .plum/config.toml
plum run -- claude              # wrap the session; emits bundle.json + report
plum note "why I did it this way" -rejected "the thing I didn't do"
plum report                     # mechanical evidence, read-first ordering
plum synth                      # seams doc in a fresh context, claims.yaml
plum trace                      # run the suite with the changed set instrumented
plum explore                    # meet the code as a landscape — no score, no timer
plum quiz                       # only after exploring, graded on real traces
plum claims verify              # executable claims, for CI
```

`plum run` not an option (GUI editor, work already done)? Two other doors:

```sh
plum mark start && plum mark end    # manual boundaries
plum range HEAD~3..HEAD             # any commit range, after the fact
```

## What each command actually gives you

| Command | Output | The point |
|---|---|---|
| `plum run` / `mark` / `range` | `bundle.json` | The contract. Everything downstream reads only this. |
| `plum report` | markdown | Signature changes on existing exports first — the highest-signal event the tool produces. |
| `plum synth` | `synthesis.md`, `claims.yaml` | Assumptions, invariants, failure modes. Each claim tagged executable or trust-me. |
| `plum stale` | exit 1 on drift | Claims are addressed to AST fingerprints, not files. Reformatting is not drift; a logic change is. |
| `plum trace` | `traces/`, `landscape.json` | Only the changed symbols are instrumented, in a scratch copy. Your repo is never written to. |
| `plum landscape` | terminal round trip | Descent is entering a call, ascent is returning, a panic is a cliff. |
| `plum explore` | localhost UI | Click a frame, see its real recorded arguments, ask a question grounded in them. |
| `plum quiz` | terminal Q&A | Every question comes from a recorded invocation. Misses accumulate in your state dir. |
| `plum claims verify` | exit 1 on failure | A failing claim means the doc is wrong or the code is. Both worth knowing. |

## Where things live

```
target-repo/
└── .plum/
    ├── config.toml                    # committed
    └── sessions/2026-08-19-a3f2/
        ├── bundle.json                # committed — reviewable in PRs
        ├── synthesis.md               # committed
        ├── claims.yaml                # committed
        ├── landscape.json             # committed (small)
        └── traces/                    # gitignored — large, machine-specific
```

Exploration telemetry and prediction misses live in
`~/.local/state/plum/<repo-hash>/`. They describe *you against this codebase*,
not the codebase, and never enter git.

## Gating, for hooks and popups

Most sessions deserve nothing but a journal line. `plum gate` exits non-zero only
when this one deserves your attention:

```sh
plum gate || tmux display-popup -E -w 80% -h 80% "plum report"
```

## Languages

Go is native (`go/ast`): exact symbols, signatures, docs, call-site comments,
fingerprints, risk predicates, and full tracing. Python and TypeScript/JavaScript
run on a line-based fallback adapter — useful for symbols, surface and comments,
honest about what it cannot resolve — plus standalone trace shims under `shims/`.
Swapping in tree-sitter-under-wazero is a drop-in behind `lang.Adapter`.

## Deviations from the spec, and why

| Spec said | Built | Why |
|---|---|---|
| `spf13/cobra`, `BurntSushi/toml`, `bubbletea` | stdlib `flag`, a small TOML subset parser, plain terminal output | The spec offers "or stdlib `flag` if you want zero deps". Zero dependencies makes `CGO_ENABLED=0` and cross-compilation unconditional, and the gate popup needs no TUI framework — `plum gate` exits non-zero and `tmux display-popup` does the rest. |
| tree-sitter as WASM under wazero | native `go/ast`; a line-based fallback for Python and TS | The spec itself says a Go-only M0 defers this. The `lang.Adapter` seam is unchanged, so the wazero spike remains a drop-in when a non-Go repo justifies its day. |
| `git stash create` for uncommitted work | a commit built against a throwaway `GIT_INDEX_FILE` | `git stash create` refuses to run once intent-to-add entries exist, and cannot see untracked files at all. The replacement touches neither the index nor the working tree, and does capture untracked files. Covered by a test. |
| `audit`, `.audit/` | `plum`, `.plum/` | The product is PLUM. |

Two things the spec left open were forced during the build and are now flags
rather than assumptions:

- **Representative chain** (spec §14.3): `plum trace -chain hottest|slowest|raising`,
  re-derivable from stored events with `plum landscape -chain ...` — no re-run needed.
- **Divergence baseline** (spec §14.6): both are shipped. Every finding records
  whether it came from `config` or from the `empirical` sample at `StartSHA`, so
  the false-positive rates can be compared rather than argued about.

Neither the report nor the landscape truncates silently. A capped section says
how many entries it held back and how to see them.

## Development

```sh
make test     # go test ./... -race
make golden   # regenerate testdata/fixtures/*/golden.json
make lint     # gofmt + go vet
make cross    # darwin/linux/windows, amd64 + arm64
```
