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
plum version        # -> plum v0.1.1 (or later)
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
| none of the above | the source extensions decide it — `.py` → python, `.go` → go, `.ts`/`.js` → typescript |

A repository with no manifest at all is common; reading the extensions is not a
guess, it is the same evidence one step further down.

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

### When capture does nothing

Capture is deduplicated on tree content, so a commit that changes nothing plum
had not already recorded produces no new session. That is correct, not broken.
Run by hand it says which of those it was:

```
plum: nothing captured — the tree is byte-for-byte what the last capture
already recorded (-force overrides)
```

From a hook it stays silent, because a message after every agent turn is how a
hook gets uninstalled. **So do not conclude the hook is broken from the absence
of a session** — run `plum auto` yourself and read the reason.

### The post-commit hook and manual boundaries

Once the hook is installed, committing captures a session **if the commit
contains something the last capture had not already seen.** If you ran `plum
mark end` first, it usually has — so committing right afterwards produces no
second session, and that is the deduplication above rather than a failure.

Use one mechanism or the other for a given change. Sessions tile by SHA, so
mixing them is safe, it is just harder to read.

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

Then commit the change **and** the session together — a session bundle
describing source that is still uncommitted is a record of nothing anyone else
can see:

```sh
git add <the files you changed> .plum/sessions
git commit -m "<what changed>"
```

`traces/` inside the session is gitignored already; the bundle and landscape are
small and are the artifact a reviewer reads.

`plum trace` instruments a **scratch copy**. **It never modifies your source.**
It does write into `.plum/sessions/<id>/` — the trace, the landscape — and says
so in its own output.

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
- **A mix: some named tests and a `(no test)` bucket.** Partial attribution.
  Judge it by size — if most frames sit under `(no test)`, treat it as the case
  above and report it. A small remainder is usually setup or teardown running
  outside any test, and is not worth raising.

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
plum export -o /tmp/plum-review.html
```

One file, opens with nothing running, safe to attach to a pull request or send
to somebody who does not have plum installed.

**Write it outside the repository**, as above. It is a rendering of the session,
not a source of truth, and inside the tree it becomes an untracked file somebody
has to decide about. With no `-o` at all it lands in the session directory,
which is *not* gitignored for this file. Do not commit it without asking.

For an interactive read:

```sh
plum explore                       # serves on localhost and OPENS A BROWSER
plum explore -no-open              # serve only, and print the URL
plum explore -test "TestName"      # narrow to one test's path
```

`plum explore` opens a browser window on the user's machine and then **blocks
until interrupted**. If you are working unattended, use `-no-open` and hand them
the URL, or skip it — `plum export` gives the same content without taking over
their screen.

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
| `plum tests` | the tests that ran, and what each one reached |
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
