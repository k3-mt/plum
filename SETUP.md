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
```

This writes two files and nothing else. It does not touch source, does not
commit, and does not run anything.

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
documents. Look in `Makefile`, `package.json` `scripts.test`, `justfile`,
`CONTRIBUTING.md`, or CI workflow files. **Do not invent one.** If you cannot
find it, ask the user — running the wrong command wastes their time and may
have side effects.

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

`plum trace` writes to a scratch copy. **It never writes to the repository.**

Verify the evidence is real, not empty:

```sh
plum tests         # must list tests, each with the frames it reached
```

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

## What you must report, even when nothing broke

- Which files you created or modified, especially `.claude/settings.json` and
  `.git/hooks/post-commit`.
- A suite that fails on a clean tree.
- A trace that recorded no events.
- Anything you had to guess.

---

## Commands, in one place

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

Every command takes an optional session reference — an id, a commit, or nothing
for the most recent. Flags may be written before or after it.
