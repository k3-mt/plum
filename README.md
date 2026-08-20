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
| `plum ask` | grounded answer | Context assembled from the bundle, routed to your agent session. A question the evidence cannot answer is itself a finding. |
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

| | Parsing | Tracing | Notes |
|---|---|---|---|
| **Go** | native `go/ast` | source-rewritten scratch copy | exact everything |
| **Python** | the interpreter's own `ast` + `tokenize` | `sys.monitoring` shim | needs python3 on PATH; falls back to a line-based adapter without it |
| **Config** | YAML, TOML, JSON, `.env`, INI | n/a — config is read, not executed | see below |
| **JavaScript / TypeScript** | structural scanner (brace depth, class bodies, comment state) | `--require` preload hook | CommonJS only; native ES modules are not traced |

JavaScript has no parser to borrow — Node exposes no AST to userland — so its
adapter is a structural scanner that tracks brace depth, class bodies and
comment state rather than matching lines in isolation. That is enough to name
every function and method exactly, which is what the SymbolIDs and the
instrumentation set need. Braces inside strings, template literals and comments
do not move it.

Python gets the same treatment Go does: exact signatures including defaults,
keyword-only markers and annotations; docstrings; the comment above a call site;
module-level state (but only when something can actually write it); mutable
default arguments; `except: pass` however many lines it spans; and real recorded
arguments and return values from `sys.monitoring`. The shim attaches through
`sitecustomize.py` on `PYTHONPATH`, so pytest, unittest and plain scripts all
work without knowing plum exists.

### Adding a language

The engine contains no language-specific instrumentation. An adapter declares
how it attaches, and the collector honours it:

```go
func (a *Adapter) ShimSpec(syms []bundle.SymbolID) (trace.ShimSpec, error) {
    return trace.ShimSpec{
        Mode:     "env",                     // write these files, set this environment
        Dir:      ".plum-shim-ruby",
        Files:    map[string]string{"shim.rb": rubyShimSource},
        Env:      map[string]string{"RUBYOPT": "-r${SHIM_DIR}/shim.rb",
                                    "PLUM_SYMBOLS": "${SYMBOLS}"},
        PathVars: []string{"RUBYLIB"},       // prepended to, never replaced
    }, nil
}
```

`${SHIM_DIR}` expands to the shim's absolute path in the scratch copy and
`${SYMBOLS}` to the instrumentation set. A language that cannot be attached this
way — Go, where a probe has to go *inside* a function — implements
`trace.Rewriter` instead and is handed the scratch copy to rewrite. Either way
the engine never learns the language. There is a test that instruments a
made-up Ruby adapter end to end to keep that honest.

## Configuration files are part of the code

A YAML key is not code, but changing one changes behaviour exactly as surely as
editing a function — and it does so invisibly, because no compiler and no test
signature moves. So config keys become symbols with the same SymbolID shape as
everything else:

```
config/app.yaml::server.timeout
```

which means they carry fingerprints, appear in the public-surface diff as
**value changed**, can be claimed about, go stale, and — the part that matters —
get linked to the code that reads them:

```
- modified `config/app.yaml::auth.AUTH_REALM` — `auth.AUTH_REALM = prod`
    - comment: the suffix appended to every issued token
    - read by `app/cache.py::Cache.decorate` (matched by env)
```

Three bindings are recognised, and each records how it was found so you can
judge it: `env` (code reads an environment variable the config defines),
`literal` (code contains a string equal to a key or its leaf), and `filename`
(code opens the config file by name). The search runs over the whole tree at
`EndSHA`, not over the diff — the code that reads a setting is almost never the
code that changed in the same session. Settings with no reader anywhere are
reported as such rather than quietly omitted.

Config also carries its own risk predicates: hardcoded secrets (a `${VAR}`
reference is the fix, not an instance), disabled verification, `0.0.0.0` binds,
`timeout: 0`, debug left on. Secret *values* are redacted before they reach
bundle.json — the key stays visible, the value does not.

## Asking questions from the landscape

Click a frame in `plum explore`, type a question, and it goes to the Claude Code
session already running in a tmux pane:

```
browser ──ask──▶ plum ──writes .plum/ask/<id>.md──▶ tmux send-keys ──▶ Claude Code
                                                                          │
        browser ◀──polls /api/ask/<id>──── plum ◀── .plum/ask/<id>.answer.md
```

What travels with the question is the whole point. It is not a search result: it
is the exact declaration text, the real recorded arguments and return values for
that frame, its callers and callees, its risk markers, whatever rationale was
journalled, and any claims about it — assembled mechanically from the bundle.
The prompt tells the agent to answer only from that, and to say what is missing
rather than guess.

Configure the route in `.plum/config.toml`:

```toml
[ask]
route           = "tmux"   # tmux | api | context-only
tmux_target     = ""       # empty to auto-detect the agent pane
timeout_seconds = 300
```

The pane is found by inspecting the processes actually running in it, not by
tmux's `pane_current_command` — a real Claude Code session reports its version
as the command and the hostname as its title, so the obvious check finds nothing.

`tmux` needs no API key and uses the session you already have open. `api` calls
the Anthropic API directly with `ANTHROPIC_API_KEY`. `context-only` returns the
assembled evidence and nothing else — which is still useful, because it is
exactly what a model would have been given.

Same thing from a terminal or a tmux popup:

```sh
plum ask -symbol 'app/cache.py::Cache.get' "what does this return when the key is absent?"
plum ask -pending                     # questions still waiting for an answer
```

### Keeping an answer

An answer nobody keeps is a chat message. Three buttons appear under an answer,
and each turns it into something durable:

| Keep as | Writes | Why it survives |
|---|---|---|
| **rationale** | a journal entry against that file | appears in every future report — this is the P3 loss, recovered |
| **a claim** | `claims.yaml`, fingerprinted at write time | goes stale automatically when the symbol changes (P5) |
| **a comment** | `.plum/patches/<id>.diff` | a reviewable unified diff in that language's comment syntax |

Source is never edited in place. The comment option writes a patch you read and
then `git apply` — or delete. A tool that silently rewrites your code is a tool
you stop trusting.

Replies arrive as markdown, usually opening with a heading and a restatement of
the question. Only the first substantive paragraph is kept, with the scaffolding
and the markup stripped — a claim that restates the question asserts nothing.

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
