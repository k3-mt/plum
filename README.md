# plum

[![release](https://img.shields.io/github/v/release/k3-mt/plum?label=release&color=6f8f7a)](https://github.com/k3-mt/plum/releases/latest)
[![ci](https://github.com/k3-mt/plum/actions/workflows/ci.yml/badge.svg)](https://github.com/k3-mt/plum/actions/workflows/ci.yml)
[![licence](https://img.shields.io/badge/licence-MIT-6f8f7a)](LICENSE)

**A comprehension-debt system for agent-written code.**

When an agent builds a feature, the code may work while your mental model of the
repository silently goes stale. `plum` extracts mechanical evidence about what
changed, synthesises it without inherited context, lets you **explore** the
result as a navigable energy landscape, and only then interrogates you against
recorded execution.

The deliverable is not documentation. The deliverable is a correctly updated
model in the developer's head.

Works on Go, Python, JavaScript/TypeScript, and dbt/SQL warehouses.

---

## Install

```sh
# Go toolchain
go install github.com/k3-mt/plum/cmd/plum@latest

# or a prebuilt binary, no toolchain needed
curl -fsSL https://raw.githubusercontent.com/k3-mt/plum/main/install.sh | sh

# or from source
git clone https://github.com/k3-mt/plum && cd plum && make install
```

All three give you the current release — **v0.2.2** at the time of writing;
`plum version` tells you what you actually have. Binaries for macOS, Linux and
Windows on amd64 and arm64, with `checksums.txt`, are attached to every
[release](https://github.com/k3-mt/plum/releases). To pin one:

```sh
go install github.com/k3-mt/plum/cmd/plum@v0.2.2
VERSION=v0.2.2 sh install.sh
```

**Setting it up in a repository?** [`SETUP.md`](SETUP.md) is a runbook written to
be handed to an agent — *"read SETUP.md and set plum up in this repository"* —
with the commands, the expected output, what to do when it differs, and where it
must stop and ask you rather than decide.

**No third-party dependencies.** One static binary, `CGO_ENABLED=0`, Go 1.23+
and `git` on PATH. That is deliberate: a tool that reads your repository should
not bring a supply chain with it. `go.mod` has no `require` block, and `make
cross` failing is the acceptance test — if it ever does, a cgo dependency has
crept in and cross-compilation is gone.

## The loop

```sh
plum init                       # write .plum/config.toml
plum run -- claude              # wrap the session; emits bundle.json + report
plum note "why I did it this way" -rejected "the thing I didn't do"
plum report                     # mechanical evidence, read-first ordering
plum synth                      # seams doc in a fresh context, claims.yaml
plum trace                      # run the suite with the changed set instrumented
plum explore                    # meet the code as a landscape — no score, no timer
plum watch                      # or leave one window open and let it follow every session
plum export                     # one HTML file, opens with nothing running
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
| `plum watch` | a window that stays | The same page with a longer life: a stable address, its own frame, a debt meter, and it follows each session as it lands. |
| `plum context` | evidence on stdout | Deterministic given a commit range. Pipe it into any tool. |
| `plum ask` | grounded answer | Context assembled from the bundle, routed to your agent session. A question the evidence cannot answer is itself a finding. |
| `plum quiz` | terminal Q&A | Every question comes from a recorded invocation. Misses accumulate in your state dir. |
| `plum claims verify` | exit 1 on failure | A failing claim means the doc is wrong or the code is. Both worth knowing. |
| `plum export` | one `.html` file | The same page with the evidence folded in. Opens from `file://` with no network. Attach it to the PR. |
| `plum ingest` | `flow.json` | Reads a dbt run that already happened. Never triggers one — in a warehouse every run is billed. |
| `plum flow` | terminal DAG | Build order, join types, grain, cost per model. |

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
| **JavaScript / TypeScript** | structural scanner (brace depth, class bodies, comment state) | `--require` preload for CommonJS, `module.register()` load hook for ESM | `const`-bound arrow exports in ESM cannot be traced, and say so |

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

### CommonJS and ESM

The two module systems need different attachment points, and both are wired.
CommonJS is hooked at load and its exports object wrapped afterwards. ES modules
have no object to mutate — their exports are live bindings resolved at link time
— so a `load` hook appends instrumentation to the module source before it is
evaluated. Nothing already in the file moves, so line numbers stay honest.

Both paths share one runtime, so a call that crosses from CJS into ESM is one
stack at one set of depths, not two traces.

One thing genuinely cannot be traced: `export const f = () => {}`. A `const`
binding cannot be rebound, and there is no exports object to reach around it.
Rather than quietly recording nothing, the shim reports it and plum subtracts it
from the instrumented count:

```
instrumented 3 symbols (typescript), recorded 10 events
  skipped: src/cache.js::shorten: Assignment to constant variable.
```

Declare it as `export function f() {}` and it traces.

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

## Letting it happen on its own

```sh
plum hooks install     # Claude Code Stop hook + git post-commit
plum hooks status
plum agent install     # teach Claude Code and Codex to offer plum, in every repo
```

`plum hooks` is per repository. `plum agent` is per machine: it adds one marked
block to `~/.claude/CLAUDE.md` and `~/.codex/AGENTS.md` telling the agent to
offer `plum init` in a repository that lacks it, to ask *"want to see what
changed, visualised?"* when it finishes work, and never to run anything slow
unprompted. Your own text in those files is kept as it is; `plum agent
uninstall` removes the block exactly.

Two attachment points, because a session ends in two different ways:

| Hook | Fires | Catches |
|---|---|---|
| Claude Code `Stop` | the agent finishes a turn | agent work, still uncommitted |
| git `post-commit` | a commit lands | anything, whoever made it |

Both call `plum auto`, which captures **only if something actually changed**.
The fingerprint is the id of the tree git would write right now — not `git
status`, which reports only *which* paths changed and so cannot see a second
edit to a file already listed as modified.

Consecutive captures tile rather than overlap: each turn's session covers that
turn's work, not everything since the last commit.

```
turn 1  → session 2026-08-20-3b91  [Validate]
turn 2  → (nothing changed, silent)
turn 3  → session 2026-08-20-d4cb  [Normalise]
```

The Stop hook is installed `async`, so a capture never delays the end of a turn,
and `plum auto` exits 0 whatever happens — a hook that breaks a commit or an
agent session is a hook that gets uninstalled within the hour.

### Capture always, analyse only when it matters

Capture is milliseconds: a commit range and an AST pass. Tracing and synthesis
are not, and running them after every prompt is exactly how a tool stops being
read (P6). So they are opt-in, per session, and only for one that fired the gate:

```toml
[auto]
enabled  = true
on_gate  = []          # e.g. ["trace"] or ["trace", "synth"]
notify   = true        # one line in the agent's own UI when the gate fires
```

### Choosing a commit to analyse later

Each capture attaches its analysis to the commit as a **git note** — bound after
the fact, without rewriting history and without touching the commit message:

```sh
git notes --ref=refs/notes/plum show HEAD
  plum session 2026-08-20-9e61
  gate FIRED — new public surface: 1 item
  1 symbols, 1 files
```

So every read-only command takes a commit wherever it takes a session id:

```sh
plum report HEAD
plum report HEAD~3
plum explore 9e61
```

## The test is the unit

A test is the only artifact that is simultaneously named, executable, committed,
and about one intention. So every recorded frame is attributed to the test that
produced it, and the landscape can be drawn per test rather than per heuristic:

```sh
plum tests
  get and put                    2 frames  2 symbols  depth 2
  verify throws through frames   3 frames  3 symbols  depth 3  ⚠ raised

plum explore -test "verify throws through frames"
plum landscape -test "verify throws through frames"
```

Attribution works differently in each runtime, and all three reach 100%:

| | How the test is identified |
|---|---|
| Go | `Test*` functions get a label probe injected into the scratch copy; every frame on that goroutine inherits it |
| Python | `sys.monitoring` sees test functions and tracks them as roots — tracked, never emitted |
| Node | `node:test` is a builtin, so patching its `test`/`it` exports in the preload is seen by `require` **and** by `import`; `AsyncLocalStorage` keeps concurrent tests from mislabelling each other |

The test frame itself is never drawn. The test names the recording; it is not a
frame in it.

### Change, drawn inside the system it perturbs

A change is only legible in context, but recording the whole system as deeply as
the change costs more and says less. So tracing happens in rings:

| Ring | What | Recorded | Drawn |
|---|---|---|---|
| 1 | symbols this session changed | arguments, returns, exceptions | solid |
| 2 | surrounding code the test walks through | entering and leaving only | thin, `(in parentheses)` |
| 3 | `node_modules`, vendored, stdlib | never | not drawn |

Ring 2 is cheap precisely because capturing arguments is most of what tracing
spends. Without it, a landscape shows the change floating free:

```
[decorate]
  ↓ 21µs
  [realm]
```

With it, the change sits where it actually lives:

```
(Cache.get) ·context
  ↓ 18µs
  (Cache.lookup) ·context
↑ 17µs
(Cache.get) (resumed) ·context
  ↓ 165µs  “(unexplained)”
  [decorate] ·undocumented
    ↓ 21µs
    [realm] ·undocumented
```

Scope it in `.plum/config.toml`:

```toml
[trace]
context = "file"   # off | file | dir
```

`file` (the default) records the other declarations in the files this session
changed. `dir` widens to their neighbours. `off` returns to changed symbols only.

Ring membership needs nothing from the shims — the bundle already knows what
changed. And documentation debt is scoped to the session: an undocumented
function this session never touched is not this session's to answer for.

### What this buys

**"Untested" becomes exact.** It used to mean *a changed test file mentions this
name*. With traces it means *no test's execution ever entered this symbol*:

```
## Untested new symbols
1 of 2 changed code symbols were never entered by any test's execution.
- `a.go::NeverRun`
```

Without traces the report says so, rather than passing a name match off as
coverage.

**And you get the map that grows with the suite:**

```
## Which tests reach this change
- **verify throws through frames** reaches 3 changed symbols
    - `src/cache.js::Cache.check`
    - `src/cache.js::Cache.verify`
    - `src/cache.js::mustGet`
    - `plum explore -test "verify throws through frames"`
```

Each test contributes a named path. The union over a suite is a map of the
covered system, assembled from real execution rather than static analysis — and
code no test reaches is visible by its absence, which is itself the finding.

## Two panes

The explore page is a view of files that change while you are looking at them,
so it watches them. Work in one pane and read in the other:

| You do this | The page does this |
|---|---|
| `plum trace` | reloads the landscape |
| `plum interpret` | the reading appears at the top |
| an agent edits the source | the reading's **stale** badge lights up |
| `plum synth` | claims refresh in the rail |

No dependency and no build step: a digest of the watched files' size and
modification time on a short timer, pushed over server-sent events. The reader's
selection and scroll position survive a reload, and a view narrowed with `-test`
stays narrowed. `plum explore -no-watch` turns it off.

### A window, rather than a tab

`plum explore` opens one recording and is finished when you close it. `plum
watch` is the same picture with a different lifetime — the one to leave on a
second screen while an agent works:

```sh
plum watch          # start it once; it is still the right window tomorrow
```

Three things make that possible.

It **follows the session**. It starts even when the repository has none yet, and
when capture writes one — the Stop hook firing, a commit landing — the window
moves to it. You never go and reopen it against the session you just made.

It has a **stable address**, derived from the repository's path rather than from
whatever port was free: `http://127.0.0.1:17…`, the same one next week. So a
bookmark keeps working, and so does a docked editor pane pointed at it. A second
`plum watch` in the same repository raises the window that is already running
rather than starting a rival on another port — one window per repository, the way
opening a project twice does in an editor. If something else has the port, it
says so and takes a temporary one.

It opens a **frame, not a tab**: a Chromium-based browser started with `--app=`,
so there is no tab strip and no address bar, it gets its own entry in the dock,
and — because it also gets a profile of its own — it remembers where you put it.
No Electron and no dependency; it is a different `exec`. If no such browser is
installed it falls back to a tab, and `-tab` asks for one deliberately.

Clicking "I have met this code" still unlocks `plum quiz`, but no longer closes
anything: meeting the code ends an explore, not a window you left open for the
next session.

It is also **installable**. The page ships a web app manifest, so Chrome offers
to install it and it gets a real icon and a real dock entry rather than a
borrowed favicon. An export carries neither — the manifest and its icons are
stripped on the way out, because a file you open from `file://` has nothing to
install and no server to fetch them from.

### The number on it

The window carries a debt meter, which is the number all of this exists to show:

```
▲3  14  ████████░░░░  unmet of 37 · 6 changed since you read it
```

**Unmet** is how many of the symbols this session changed you have not seen at
the version they are in now. It is keyed on the same AST fingerprint that drives
staleness, not on a boolean — having read `Get` last week says nothing about the
`Get` that exists today, and that difference *is* the debt. A symbol you had read
before and that has since changed under you is counted separately, because code
that moved while you were not looking is a different problem from code that is
simply new.

It moves in both directions on its own terms. An agent working pushes it up.
Opening a frame pulls it down by one — the brief fetch is the moment the code is
actually in front of you, as opposed to being a shape on a picture with a name on
it. "I have met this code" clears the changed set in one go: that is a claim you
are making about yourself, taken at face value.

**`plum quiz` is where that claim is checked, so it is also where the meter is
corrected.** Answering from the recording confirms the symbol was met. Missing it
says the claim did not hold, and the debt goes back on — a meter that only ever
went down would be measuring clicks rather than comprehension. The quiz prints
where the number stands when it finishes. Frames you have not met are drawn hollow, so the picture shows the same
thing the number does.

What you have met lives in the state dir next to the explore telemetry, never in
git — it describes you against this codebase, and two people on one repository
have different debts. An export has no reader and so shows no meter at all;
zero would read as *you have met all of this*, which is a different claim and an
untrue one.

### A list you can work through

The landscape draws one chain out of however many were recorded — on a real
session here, six of eighty-six unmet symbols were on the picture. A meter that
counts eighty-six while the window offers a way to meet six of them is a number
you can read but not act on.

So the window also lists what you owe, in the order `plum report` reads in — what
could break other people first, source order never:

```
what you have not met — read-first order, not source order

 1. Server.Serve   · internal/server/server.go  signature changed on an existing export
 2. handleSymbol   · internal/server/server.go  risk marker
 3. Set            · internal/met/met.go        new public surface
 …
 … and 61 more, held back so this stays a list you finish
```

Clicking an entry opens its brief, which is the same act as clicking a frame:
the debt goes down by one and the entry leaves the list. Test code sits at the
bottom — it is changed code and belongs on the list, but an exported `Test`
function outranking a changed signature would be the order backwards. What does
not fit is counted and named rather than silently cut.

### Which way it is going

A number on its own does not say whether it is rising, and that is most of what a
glance from across a room is for: fourteen unmet reads very differently depending
on whether it was four ten minutes ago or forty. So the meter carries an arrow —
how far it has moved over the last half hour, counting both the unmet symbols and
the ones being written right now.

It is movement across that window, not the step since the last reading. A step
would flicker: every symbol you opened would flash *down 1* and settle, telling
you about your own last click rather than about the session.

The history lives in memory for the life of the window and is never written down.
A trend is a live signal about what is happening now; carrying it across restarts
would buy a file, a format and a trimming policy in exchange for a number nobody
reads the following morning. A restarted window has no trend yet, and says so by
showing no arrow rather than a flat one.

### It moves while the agent is still typing

A bundle is a photograph. Between the agent writing a line and the Stop hook
capturing it there is a window — often the most interesting minute of the whole
session — in which a meter measured only against the capture would sit perfectly
still.

So the window also asks the files. For every file the capture named, the working
tree is re-parsed and its symbols' fingerprints compared with the recorded ones.
What has moved is code that exists and has never been captured, let alone read:

```
14  ████████░░░░  unmet of 37 · 6 changed since you read it · 3 being written now
```

This is the same comparison `plum stale` makes for claims, run over the changed
set instead of over `claims.yaml`, and it inherits the same property: the
fingerprint is over the normalised subtree, so **reformatting is not drift**. An
agent running a formatter does not make the number twitch.

It is reported beside the unmet count rather than added to it. One is measured
against a recording and the other against the disk this second; a number that
quietly mixed the two could not be checked against anything.

The work is bounded and the bound is visible. A first capture in a repository can
name every file in it, and re-parsing all of them on a watcher tick is a build
rather than a meter — past the budget the window says *drift not measured* and
why, because a silent zero would read as "nothing is being written", which is
both wrong and reassuring. The comparison is memoised on the same size-and-mtime
digest the watcher uses, so asking for the meter is cheap however often the page
asks.

### Two reading distances

Dragged narrow, the window collapses to its meter: the number at the size you
can read from across a room, and everything that is not the number out of the
way. Opened out, it is the full page again. Width decides this, not focus — a
window that blanked whenever you looked elsewhere would be unusable, and dragging
an edge is a control everyone already knows how to work. Only `plum watch` does
this; a narrow `plum explore` tab showing nothing but a number would simply be
broken.

Collapsing is also what makes leaving it open cheap. A window showing only its
meter does not fetch a landscape it is not drawing — that payload carries every
changed symbol in the session — so it asks for the few hundred bytes of the
number instead. A window behind other windows draws nothing at all. Either way
the page remembers that it owes itself a redraw, and pays it on the way back
rather than sitting there showing something out of date.

A click is not just navigation. Clicking a **frame** copies its whole assembled
brief to the clipboard — source, recorded arguments and returns, neighbours with
their code, risks, journalled rationale, claims — and says so:

```
evidence copied — undocumented, paste it and ask for the doc · 2,228 chars
```

Clicking a **transition** copies that call site instead, headed with its cost and
with whether anything explains it. An unannotated expensive call is the thing you
most want to hand to an agent, and it lives on the transition rather than on the
frame. The copied brief is byte-identical to what `plum context` prints, with a
test pinning the two together.

## Two layers, kept apart

There are two different kinds of statement about a change, and mixing them is
how a tool stops being trusted.

**What happened** — `plum explain`. Composed from the recording: the arguments
that went in, the values that came back, the comment above the call, the cost of
each step. No model, no network, identical every run, and where the evidence is
silent it says so:

```
Cache.Get was called with key = "user:42" and opts = nothing. It returned
"tok@prod" and no error. Its own description: "Get returns the token for a key,
or ErrMiss.". Flagged: parameter opts is typed as any/interface{} — the compiler
stops helping callers.
    Cache.Get then called Cache.decorate, which took 27µs. The code says why:
    "the realm suffix is applied on the way out so callers never see a raw token".
        ⚠ no description was written for this function, so what it is for is
          not recorded anywhere
```

**What it is for** — `plum interpret`. Purpose is not recoverable from a trace;
it lives in a head, a ticket, or a comment nobody wrote. So this one asks a model
— routed to your agent session over the same tmux bridge, or to the API — and
everything about it is arranged so the answer stays honest:

- the brief leads with the mechanical narration, **labelled as established**, and
  the prompt forbids contradicting it
- the reading must separate *"the recording shows"* from *"my inference"*
- it must include a section naming what the evidence does **not** settle, and
  what would settle each thing
- it is stored with the fingerprint of every frame it describes, so it goes
  stale the moment one changes — the same mechanism that keeps claims honest
- it is labelled everywhere it appears: *a reading, not a record*

```sh
plum interpret                     # the session
plum interpret -test TestGetPut    # one test's path
plum interpret -symbol 'a.go::F'   # one frame
plum interpret -show               # the stored reading, without asking again
plum interpret -route print        # just print the prompt, ask nobody
plum stale                         # claims AND readings that no longer apply
```

In the UI the reading gets its own panel, bordered and titled *"what this is for
— a reading, not a record"*, kept visually apart from the landscape and the
narration above it. Prose that reads like a record but was inferred is worse
than no prose at all.

## Feeding the evidence to an LLM

`plum context` prints the assembled evidence to stdout, so it pipes anywhere:

```sh
plum context                                   # the whole session brief
plum context -symbol 'src/cache.js::Cache.get' # one frame, in full
plum context -symbol Cache.get -json           # structured, for indexing
plum context -diff                             # brief plus the diff
```

What comes out is the exact declaration source, the real recorded arguments and
return values, callers and callees *with their code*, risk markers, journalled
rationale and claims — assembled from the bundle rather than retrieved by a
search, so it does not depend on a model guessing which files to open.

### Is it deterministic?

Mostly, and precisely where it matters. Measured over repeated runs of the same
commit range:

| Artifact | Deterministic | Notes |
|---|---|---|
| `bundle.json` | **yes** | byte-identical apart from `session.id`, `started_at`, `ended_at` |
| `plum context` output | **yes** | same input, same bytes |
| `synthesis.md`, `claims.yaml` (offline provider) | **yes** | composed mechanically from the bundle |
| `synthesis.md` (API provider) | no | it is a model call |
| landscape **structure** | **yes** | same frames, same depths, same order |
| landscape **timings** | no | measured durations; one barrier moved 21µs → 1.9ms between runs on a warm vs cold JIT |

So: cache it, diff it in a PR, feed it to a model and get a reproducible answer
about structure. Do not treat a barrier height as a stable number — it is a
measurement, and it is labelled as one.

## dbt

A dbt project publishes its own symbol table, so the adapter reads it rather
than parsing SQL. Set `languages = ["dbt"]` and the same loop applies.

| PLUM concept | dbt equivalent |
|---|---|
| symbol | a model, and **each declared column** |
| public surface | a column's **type and its tests** |
| fingerprint | normalised SQL, comments and whitespace stripped |
| edges | the DAG, from `ref()` and `source()` |
| call-site rationale | the `--` comment above a `ref()` |

Columns are symbols in their own right because a column is the unit other people
depend on. A dropped column breaks every downstream model that selects it and
**nothing fails to compile**, which is exactly the change this tool exists to put
in front of a reader:

```
- **column changed** `fct_orders.order_total`
    - before: `NUMERIC [untested]`
    - after:  `FLOAT64 [untested]`
    - every downstream query that selects this column keeps running and returns something else
- **removed** column `fct_orders.customer_id`
    - every downstream model that selects it breaks at run time, not at compile time
```

The contract comes from the committed `schema.yml`, not from `target/manifest.json`.
The manifest is richer but it is a build artifact and normally gitignored, so
there is no manifest at `StartSHA` to diff against — the declared contract is
what a reviewer actually changes, and it is what can be compared across a range.

### A warehouse build is not a call stack

The landscape draws code: a path that descends into a call and comes back, where
closure is the shape you read it by. SQL has no returns. `fct_orders` does not
call `stg_orders` and get control back — the warehouse builds `stg_orders`,
builds `stg_payments`, then builds `fct_orders` by reading both. So dbt gets its
own picture, layered in build order with data moving one way:

```
$ plum ingest        # reads target/manifest.json + run_results.json. Never runs dbt.

build order · 1m24.1s · 37.1 GB scanned · 7,282,893 rows written · 1 test failing

layer 1
  stg_payments                       view · 1,902,115 rows · 819.2 MB · 3.2s
      one row per capture attempt (declared)
      ← shop_raw.payments                              from — the driving table
      where captured_at is not null                  drops rows that do not match

layer 2
  fct_orders                         incremental on order_id · 18.4 GB · 41.7s
      one row per order, says the doc — but the SQL groups by position over a
      select star, so its grain is whatever columns upstream has today
      ← stg_orders                     2,481,003 rows  from — the driving table
      ← stg_payments                   1,902,115 rows  left join on order_id —
                    unmatched rows are kept, and rows multiply if the key repeats
      ← shop-prod-1234.shop_raw.refunds   left join — written into the SQL,
                                                             invisible to dbt
      ✗ unique_fct_orders_order_id       1,204 rows  failed
```

Every arrow carries the two facts a call arrow cannot hold and a warehouse
reader asks for first: **how the rows were matched**, and **how many came
through**. Read `left join … rows multiply if the key repeats` next to
`✗ unique(order_id) — 1,204 rows failed` and that is not two facts, it is one
diagnosis.

Three things it can say that the manifest alone cannot:

- **Join type, with what it does to your rows.** A run records that a query took
  41.7s and scanned 18.4 GB. Only the statement says it left-joined on
  `order_id`. Nothing else in the pipeline knows the difference between rows
  being dropped, kept, or multiplied.
- **Grain, split into what is promised and what is true.** *Declared* is the
  author's prose ("one row per order"); *inferred* is what the SQL does. They
  are kept apart on purpose: when they disagree that is a finding, and it is one
  nothing else catches, because a doc is never run. `customer` and `customer_id`
  are normalised to the same grain, and a statement with no `group by` is not
  allowed to contradict a person. A model that groups by position over a
  `select *` is reported as **unresolvable** rather than guessed at — a wrong
  grain on the picture is worse than a blank one.
- **The edge dbt cannot see.** A fully-qualified table written straight into the
  SQL will not be built first and will not appear in lineage. It is drawn as a
  node *outside* the DAG.

`plum ingest -from stg_orders` walks the other way: what selects this model, and
what that costs when you change it.

### Predicates that earn their place

The failures these catch are not crashes. A model with `select *` keeps running
perfectly while silently changing shape; an incremental model with no partition
filter keeps returning the right answer while scanning the whole table nightly.
Nothing goes red, which is why they are worth naming.

`select_star` (including `o.*`) · `hardcoded_table` — a fully-qualified table
instead of `ref()`, so dbt cannot see the dependency · `incremental_without_guard`
· `incremental_without_partition` · `incremental_without_unique_key` — appends
instead of merges, so a re-run duplicates · `nondeterministic_incremental` —
reads the clock, so a backfill cannot reproduce · `cross_join` · `untested_key`
· `no_grain_test` · `float_money` — decimal amounts do not round-trip through
binary floating point.

### Reading a run

```sh
plum ingest            # reads target/manifest.json and target/run_results.json
```

`plum trace` runs a suite under instrumentation. That is the wrong shape here:
dbt records its own execution in detail, so there is nothing to instrument, and
every run scans billed bytes — a tool that re-runs your project in order to look
at it shows up on the invoice. `plum ingest` reads what a run already wrote and
**never triggers one**.

A build is not a call stack, but the lineage of a model is a tree, and that is
what gets walked. Entering a node means its upstream had to be built first;
returning means it finished, carrying what it cost:

```
ingested 6 nodes from target
  59.5s elapsed, 26.3 GB scanned, 6,864,121 rows written
  1 failed, 0 skipped because something upstream did

[fct_orders] ·risk
  ↓ 1ms
  (stg_orders) ·context
↑ 4.1s io
[fct_orders] (resumed) ·risk
  ↓ 1ms
  (unique_fct_orders_order_id) ·context
⇊ 6.4s raise
[fct_orders] (resumed) ·risk
```

dbt builds a node and then tests it, so tests are drawn **inside** the node's
frame — whether a model came out right is part of building it. A failing test
unwinds as a cliff, because a table whose grain test just failed is not a clean
build.

`plum explain` reads the same run in sentences, with what it cost as recorded
data rather than as prose:

> Running "build fct_orders", the run went through 6 nodes: fct_orders →
> stg_orders → stg_payments → unique_fct_orders_order_id → … 1 of those this
> session changed; 5 it merely passed through. Everything entered came back, but
> 1 step failed: unique_fct_orders_order_id.
>
> `fct_orders` was called with `materialized = "incremental"`. It returned
> `"2,481,003 rows, 18.4 GB scanned, 93,200 slot-ms"`. Flagged: materialized as
> incremental but nothing calls is_incremental() — every run rebuilds the whole
> table at full cost.

Coverage is declared rather than inferred: dbt says which tests cover which
model, and a failing one is named as failing.

## Reading it when nothing is running

`plum explore` is a server because it answers questions and watches the tree.
Reading is not that. Reading happens in a pull request, in a chat thread, on
somebody else's laptop six months from now — and none of those can run a binary
from your machine.

```sh
plum export -o review.html
```

One file. The same markup, stylesheet and scripts the server serves, with the
evidence folded in: the picture, every symbol's brief, the narration, the
findings, the recorded arguments and returns. It opens from a `file://` URL and
makes no network request of any kind. Nothing is regenerated for the export, so
the artifact cannot drift from the tool.

The parts only a running plum can do — asking an agent a question, recording
that you met the code — are removed rather than left to fail quietly, and the
file says plainly that it is a snapshot.

The canvas pans and zooms in both: drag to move, scroll to zoom, double-click to
fit. A DAG with thirty models is wider than any window.

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

Built to a written specification, which is not published here. The deviations
are recorded anyway, because a tool about comprehension debt should not run one
of its own.

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

Every behaviour described above has a test, and the tests are written to state
the failure they prevent rather than to describe the code. A release is cut by
tagging: `git tag v0.2.0 && git push --tags` builds and attaches the binaries.

## Licence

MIT. See [LICENSE](LICENSE).
