# Setting up plum — a runbook

**This document is addressed to an agent.** Point one at it — "read SETUP.md and
set plum up in this repository" — and it has everything it needs: the commands,
what each one should print, what to do when it does not, and where it must stop
and ask.

A human can follow it too. It is ordered, every step is verifiable, and no step
depends on a judgement the document does not supply.

---

## Before you start

Check these. Do not proceed past a failure.

```sh
cd <the repository>               # every command below runs from the repo root
git rev-parse --show-toplevel     # must succeed — plum reads git, it is not optional
go version                        # only if installing from source; 1.23+
```

**plum needs a git repository.** It derives everything from a commit range. If
`git rev-parse` fails, stop and tell the user: `git init` is their decision, not
yours.

**Nothing else is required.** plum is one static binary with no third-party
dependencies. Python and Node are needed only to *trace* repositories in those
languages, and only the interpreter the repo already uses.

---

## Step 1 — install

Pick the first that applies.

```sh
# A Go toolchain is present
go install github.com/k3-mt/plum/cmd/plum@latest

# No Go toolchain: fetch the release binary
curl -fsSL https://raw.githubusercontent.com/k3-mt/plum/main/install.sh | sh
```

Verify:

```sh
plum version        # -> plum v0.1.0 (or later)
```

**If `plum: command not found`:** the binary is installed but not on `PATH`.
`go install` writes to `$(go env GOPATH)/bin`; `install.sh` writes to
`~/.local/bin` unless `PREFIX` says otherwise. Add that directory to `PATH` in
the user's shell profile — and tell them you changed their profile, do not do it
silently.

**If `install.sh` says no prebuilt binary for your platform:** use
`go install`. Releases cover macOS and Linux on amd64/arm64, and Windows on
amd64.

---

## Step 2 — initialise

```sh
cd "$(git rev-parse --show-toplevel)"
plum init
```

Expect:

```
wrote .plum/config.toml
wrote .plum/.gitignore

next: plum run -- <your agent command>
```

It creates `.plum/` containing those two files plus empty `journal/` and
`sessions/` directories. It does not touch source, does not commit, and does not
run anything.

`config.toml` is longer than the two keys below — `[gating]`, `[conventions]`,
`[synthesis]`, `[trace]`, `[auto]`, `[ask]`. **Every one has a working default.
Change nothing there during setup.** They are tuning knobs for once the loop is
running, and each is commented in place.

### Configure it for this repository

Open `.plum/config.toml` and set two keys. **Read the repository to determine
them — do not guess.**

```toml
[repo]
languages    = ["go"]           # go | python | javascript | typescript | dbt
test_command = "go test ./..."
```

How to determine `languages` — the file that exists decides it:

| If the repo has | set `languages` to |
|---|---|
| `go.mod` | `["go"]` |
| `pyproject.toml`, `setup.py`, `requirements.txt` | `["python"]` |
| `package.json` | `["javascript"]` or `["typescript"]` if `tsconfig.json` |
| `dbt_project.yml` | `["dbt"]` |

List several if several apply: `languages = ["go", "typescript"]`. Aliases are
accepted — `golang`, `py`, `js`, `ts`, `node`, `sql`.

Do **not** add an entry for configuration files. YAML, TOML, JSON and `.env` are
always analysed, whatever `languages` says, because a changed default is a
behaviour change that no compiler and no test signature announces.

How to determine `test_command`: use the command the repository already
documents. **Do not invent one.** If you cannot find it, ask the user — running
the wrong command wastes their time and may have side effects.

Prefer the *invocation* a contributor would type over the raw script behind it:

| Where you found it | Use |
|---|---|
| `package.json` → `scripts.test` | `npm test` — not the script's contents |
| `Makefile` → a `test:` target | `make test` |
| `pyproject.toml` / `tox.ini` | `pytest`, or the documented runner |
| a CI workflow | the same line the workflow runs |

The wrapper is the right answer because it is the one the repository maintains.
A raw script often contains a shell glob (`src/**/*.test.js`) that behaves
differently under a different shell.

Then confirm it works before plum depends on it:

```sh
<the test command>          # must pass on a clean tree
```

**If the suite fails on a clean tree, stop.** plum traces a *passing* suite to
learn what the code does. Report the failure; do not work around it.

---

## Step 3 — commit the configuration

```sh
git add .plum/config.toml .plum/.gitignore
git commit -m "plum: add comprehension-debt configuration"
```

`.plum/config.toml` is repo truth and belongs in git. `.plum/.gitignore` keeps
the machine-specific parts (traces, journal, current session) out. Sessions
themselves *are* committed — they are the reviewable artifact.

---

## Step 4 — automatic capture (recommended)

```sh
plum hooks install
plum hooks              # verify
```

Expect:

```
Claude Code Stop hook    installed (Stop)
git post-commit hook     installed (post-commit)
auto capture enabled     true
runs when gate fires     nothing (capture only)
```

This writes `.claude/settings.json` and `.git/hooks/post-commit`. **Tell the
user you modified these.** If `.claude/settings.json` already exists, plum
merges into it rather than replacing it, but the user should still know.

`.claude/settings.json` contains an **absolute path to the plum binary**, which
is specific to this machine. Do not commit it without saying so — on somebody
else's checkout that path will not exist. `.git/hooks/` is never committed by
git at all, so each contributor who wants the hook runs `plum hooks install`
themselves.

To undo the whole step:

```sh
plum hooks uninstall
```

### The post-commit hook and manual boundaries

Once the hook is installed, committing captures a session on its own. If you are
also using `plum mark start` / `end`, expect two sessions when you commit inside
a marked window — the marked one, and the post-commit one. That is not a fault;
sessions tile by SHA. Use one mechanism or the other for a given change unless
you want both.

Capture is milliseconds, so it always runs. Tracing and synthesis are not, and
running them after every prompt is how a tool stops being read. Nothing heavier
runs unless the gate fires and `[auto] on_gate` names it:

```toml
[auto]
on_gate = ["trace"]        # or ["trace", "synth"]
```

Leave `on_gate` empty unless the user asks for more. Note that Claude Code loads
hooks at startup — an already-running session picks this up after `/hooks` or a
restart.

---

## Step 5 — prove the loop on a real change

Do not hand back an untested setup. Make a change, or use one already in flight.

```sh
plum mark start
# ... the change happens: an agent edits code, or you do ...
plum note "why this approach" -rejected "the approach not taken"
plum mark end
```

`plum mark end` prints the session id and where the bundle went:

```
session 2026-08-20-8cf8 → .plum/sessions/2026-08-20-8cf8/bundle.json
```

Then:

```sh
plum report        # mechanical evidence: what changed, what it exports now
plum trace         # runs the suite with the changed symbols instrumented
plum explain       # what the run actually did, in plain language
```

Then commit the session — this is the step that makes it reviewable:

```sh
git add .plum/sessions
git commit -m "plum: session for <what changed>"
```

`traces/` inside the session is gitignored already; the bundle and landscape are
small and are the artifact a reviewer reads.

`plum trace` writes to a scratch copy. **It never writes to the repository.**

**Order matters here, and getting it wrong is quiet.** `plum trace` instruments
the symbol set recorded in the *session bundle*, which `plum mark end` fixed at
that moment. If you edit code after `mark end` and then trace, you are tracing
the new code against the old symbol table — renamed or newly nested functions
will not match, and they will be drawn as unchanged surrounding code rather than
as your change. Nothing errors.

If you have edited since closing the session, capture again before tracing:

```sh
plum range HEAD~1..HEAD     # or: plum mark start; plum mark end
plum trace
```

Verify the evidence is real, not empty:

```sh
plum tests         # must list tests by name, each with the frames it reached
```

Three outcomes, and only one is success:

- **Named tests, each with frames.** Good — carry on.
- **`no events: the changed symbols were never called by the test command`.**
  Nothing in the suite exercises the change. That is a finding, not a failure.
  Report it to the user in those terms.
- **Everything under `(no test)`.** The run was recorded but the test runner is
  not one plum can label, so "which test reaches this change" cannot be
  answered. The landscape is still valid. Report this too — the evidence is
  thinner than it looks.

Then check that `plum explain` names a symbol `plum report` said you changed:

```sh
plum explain -brief
```

If it narrates a function you did not touch, say so rather than passing it on.

**If `plum trace` says "no events: the changed symbols were never called":**
that is a finding, not a failure. Nothing in the suite exercises the change.
Report it to the user in exactly those terms.

---

## Step 6 — hand it over

```sh
plum export -o plum-review.html
```

One file, opens with nothing running, safe to attach to a pull request or send
to somebody who does not have plum installed.

Written to the working tree, it will show as untracked. It is a rendering of the
session, not a source of truth — attach it and delete it, or write it outside
the repository (`plum export -o /tmp/review.html`). With no `-o` it goes inside
the session directory, which is gitignored for traces but not for this. Do not
commit it without asking.

For an interactive read:

```sh
plum explore                       # serves on localhost, opens a browser
plum explore -test "TestName"      # narrow to one test's path
```

Drag the canvas to move, scroll to zoom, double-click to fit. Clicking a frame
copies its assembled evidence to the clipboard — source, recorded arguments and
returns, neighbours, risks, rationale — which is designed to be pasted straight
into an agent.

---

## If you are asked to wire it into CI

```sh
plum claims verify        # exit 1 on a failing claim
plum stale                # exit 1 when a claim no longer addresses its code
```

Two behaviours to know before you rely on the exit code:

- `plum claims verify` exits **1 when the session has no claims at all**, saying
  `no claims for <session> — run plum synth first`. That is deliberate: a CI step
  that passes because nothing was checked is worse than one that fails. So
  `plum synth` has to have run for the session under test.
- A claim printed as `SKIP` — no test body was written for it — **does not**
  fail the run. Only a claim that ran and did not hold does.

## Warehouses (dbt)

A dbt project uses the same loop with one substitution: **`plum ingest` replaces
`plum trace`.**

```toml
[repo]
languages = ["dbt"]
```

```sh
plum ingest        # reads target/manifest.json and target/run_results.json
plum flow          # the build as a DAG: join types, grain, cost per model
```

`plum ingest` **never runs dbt.** In a warehouse every execution scans billed
bytes, and a tool that re-runs your project in order to look at it shows up on
the invoice. It reads artifacts a run already wrote.

**If it says the manifest is missing:** the user must run `dbt build` or
`dbt compile` themselves first. Do not run it for them without asking — it
costs money.

---

## What you must ask the user about, not decide

- **`git init`** if the directory is not a repository.
- **The test command**, if the repository does not document one.
- **Running `dbt build`**, ever. It is billed.
- **Editing shell profiles** to add a directory to `PATH`. Do it if they agree,
  but say what you changed.
- **`[auto] on_gate`** beyond the default. It makes every gated session slower.
- **Committing `.claude/settings.json`**, which holds a machine-specific path.
- **Which form of the test command** to use, if the repository documents more
  than one and they are not equivalent.

## What you must report, even when nothing broke

- Which files you created or modified, especially `.claude/settings.json` and
  `.git/hooks/post-commit`.
- A suite that fails on a clean tree.
- A trace that recorded no events.
- A trace you ran against a session captured before the code was edited.
- A `plum tests` listing where everything is `(no test)` — the run is recorded
  but nothing is attributed, so test-level questions cannot be answered.
- A `plum explain` that names symbols other than the ones `plum report` said
  changed.
- Any artifact you left untracked in the working tree.
- Anything you had to guess.

---

## The commands this runbook uses

| Command | What it does |
|---|---|
| `plum init` | write `.plum/config.toml` — the only setup step that is required |
| `plum hooks install` | capture on agent stop and on commit |
| `plum mark start` / `end` | manual session boundaries |
| `plum run -- <cmd>` | wrap an agent invocation; boundaries are automatic |
| `plum range HEAD~3..HEAD` | a session from any commit range, after the fact |
| `plum note "why" -rejected "..."` | record rationale while it is still in your head |
| `plum report` | the evidence, ordered by what a reader needs first |
| `plum trace` | run the suite with the changed set instrumented |
| `plum ingest` | read a dbt run that already happened |
| `plum explain` | what the run did, in sentences |
| `plum explore` | the interactive picture |
| `plum export` | one HTML file, needs nothing running |
| `plum synth` | seams doc plus executable claims |
| `plum stale` | exit 1 when a claim no longer addresses its code |
| `plum claims verify` | exit 1 on a failing claim — for CI |
| `plum quiz` | interrogate yourself, graded against recorded execution |

`plum` on its own lists every command, including ones this runbook does not
use: `ls`, `show`, `context`, `ask`, `interpret`, `landscape`, `hooks
uninstall`.

Every command takes an optional session reference — an id, a commit, or nothing
for the most recent, which is what this runbook relies on throughout:

```sh
plum report                    # the latest session
plum report 2026-08-20-daa7    # a named one
plum report HEAD~2             # whichever session covers that commit
```

Flags may be written before or after it.
