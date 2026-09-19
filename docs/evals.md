# Evals

`bees eval` runs the whole factory against a fixture repository and grades the
result with the fixture's own test, the way SWE-bench Lite grades a coding
agent. Use it to compare a prompt, model or profile change against a baseline
run.

Evals run real agent sessions and cost money. They are not part of
`dagger check`.

## Running evals

From a directory with an `evals/` directory in it (the busybees repository
ships one):

```sh
bees eval                          # every case under evals/
bees eval --case hello             # only evals/hello
bees eval --profile fast           # every role on the profile named fast
```

The output is one row per case, then what failed:

```
CASE   RESULT  STOP  COST   TURNS  DURATION  PROFILE
hello  pass    done  $0.84  41     6m12s     default (bees.toml)

report: /home/me/src/busybees/.bees/evals/20260918-190412/report.json
```

`bees eval` exits non-zero when any case fails. `STOP` says why the run of a
case ended:

| Stop | Meaning |
|---|---|
| `done` | Every seeded issue is closed or carries `bees:needs-human`. |
| `timeout` | The case's `timeout` ran out. The running sessions were stopped. |
| `budget` | The sessions cost the case's `max_cost`. The running sessions were stopped. |
| `invalid` | The test passed on the fixture before the run, so the case proves nothing. No session ran. |
| `interrupted` | You stopped `bees eval`. |
| `error` | The case could not be set up, or the scheduler failed. The error follows the table. |

A `COST` ending in `+?` counts sessions that reported no cost.

### Which profile the sessions run on

`--profile <name>` runs every role on that profile. bees looks it up in the
`[profiles]` of `bees.toml` first, then in `~/.config/bees/config.toml`.

Without `--profile`, the eval uses the profile selection `bees.toml` makes
([profiles](configuration.md#profilesname)): `[global]` and each
role's `profile` and `profile_by_size`, and the reviewer's `brief_profile`,
`judge_profile` and `angle_profiles`. With no `bees.toml`, it uses the
`provider` and `model` of `~/.config/bees/config.toml`, and without that file
the built-in profile.

The eval takes nothing else from `bees.toml`: no prompts, skills, MCP
servers, limits or timings, and every role runs. Two runs differ only by
their profiles. A fixture can carry its own `bees/prompts/`, as any project
can. A profile whose sandbox is `container` or `sbx` is refused: those
sessions cannot reach the fake GitHub described below.

## What a run does

For each case, in its own directory under
`<state_dir>/evals/<timestamp>/<case>/`:

1. It builds the fixture: a bare `origin.git` with one commit on `main`, and a
   clone of it in `project/`.
2. It runs the case's test on the fixture, in `before/`. A test that passes
   ends the case as `invalid`.
3. It writes `project/bees.toml` and seeds an in-memory GitHub with the case's
   issues, each carrying the `bees` label, and the mailbox with its mail.
4. It runs scheduler passes. Each pass waits for the work it started, which
   takes a developer's issue up to its approval. Between passes, it merges
   every pull request labelled `bees:approved` into its base branch on the
   origin, as a person would, and closes the issues its body names with a
   closing keyword (`Closes #1`).
5. It stops when every seeded issue is closed or carries `bees:needs-human`,
   or at the case's timeout or budget.
6. It grades the result.

The directory keeps everything: `state/` with the sessions' transcripts and
`bees.log`, the fixture, and the test output in `before.log` and `after.log`.

### The fake GitHub

The scheduler talks to the in-memory GitHub directly. A session, and the
`bees` MCP server it starts, runs `gh`, and the eval puts a `gh` of its own
first on their `PATH` (`bin/gh` in the case directory). It forwards each call
to the eval, which answers it from the same in-memory GitHub. Nothing a session
runs as `gh` reaches github.com.

That GitHub answers the calls the factory makes: listing, viewing, creating
and editing issues and pull requests, labels, comments, reviews, sub-issues
and `pr diff`. It ignores `--jq` and `--search`, reports no checks on a pull
request, and fails any call it does not know with an error the session sees.
A session that runs a `gh` by its full path, not the one on its `PATH`, is not
stopped from reaching github.com.

## Grading

A case passes when every check passes:

| Check | Passes when |
|---|---|
| the test fails on the fixture | The test fails before the run. |
| `#N closed` | Seeded issue N is closed. |
| `#N has a pull request` | A pull request's head is the issue's branch (`bees/issue-N`), or its body closes the issue. |
| the test passes | The test passes on `main` after the run. |

The test runs in a fresh clone of `main`, with the case's `grade/` files
copied over it, as `sh -c "<test>"`. A test gets 10 minutes.

## Writing a case

A case is a directory under `evals/` with a `case.toml`:

```
evals/hello/
  case.toml       the issues to seed, the test and the limits
  repo/           the fixture's files, committed as they are
  grade/          optional: files copied over the checkout before the test runs
```

Instead of `repo/`, a case can have a `setup.sh`, which bees runs with `sh` in
an empty clone and then commits. A case has one or the other.

Put the files that grade the case in `grade/` as well as in `repo/`. The
developer can run the tests in `repo/`, but a session that edits them does not
change what grades it.

`evals/hello/case.toml`:

```toml
description = "greet.sh leaves out the comma and the exclamation mark"
test = "sh test.sh"
timeout = "30m"
max_cost = 5

[[issues]]
number = 1
title = "greet.sh prints the greeting without punctuation"
labels = ["bees:ready", "bees:size/xs"]
body = """
`sh greet.sh Ada` prints `Hello Ada`. It should print `Hello, Ada!`.
"""
```

| Key | Meaning |
|---|---|
| `description` | What the case is about. |
| `test` | Required. The command that grades the case, run with `sh -c` in a checkout of `main`. It must fail on the fixture. |
| `timeout` | How long the factory runs. Default `1h`. |
| `max_cost` | The budget in USD. `0` is no limit. Default `10`. |
| `[[issues]]` | The issues to seed, at least one: `number`, `title`, `body`, `labels`, `author` (default `human`) and `[[issues.comments]]` with `author` and `body`. |
| `[[mail]]` | Messages in a role's mailbox when the run starts: `from` (default `human`), `to` (a role), `subject`, `body`, and `issue`, a seeded issue it is about. |

bees adds the `bees` label to every seeded issue. Give a work item
`bees:ready` and a size to send it straight to a developer, or `bees:triage`
to have the project manager refine it first.

Pick issues the way SWE-bench Lite does: self-contained, touching a few files,
with no links to anything outside the fixture, and graded by a test that fails
before the change and passes after it.

A directory under `evals/` named after a role (`evals/developer/`) is not a
case. Those names are reserved for per-role cases.
