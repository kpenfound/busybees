# `bees` command reference

`bees` is a single binary. Most commands need a `bees.toml`; it is found with
`--config`, then `$BEES_CONFIG`, then by searching upwards from the current directory.

Four commands — `bees mail send`, `bees issue create`, `bees issue link` and
`bees done` — are designed to be run **by Claude Code sessions** from inside the
factory (people can use them too). Sessions normally reach the same operations as
MCP tools rather than as commands, along with the GitHub operations every role
performs: `bees mcp serve` serves them, and every session gets it automatically.
Everything else is for humans.

## Global flags

| Flag | Description |
|---|---|
| `-c, --config <path>` | Path to `bees.toml`. Default: `$BEES_CONFIG`, else search upwards from cwd. |
| `-v, --verbose` | Debug logging (same as `--log-level debug`). With `run`/`tick`/`exec`, also streams every claude event to stderr. |
| `-q, --quiet` | Console shows only session summaries, warnings and errors. Cannot be combined with `-v` or `--log-level debug`. |
| `--log-format <text\|json>` | Console log format. Default `text`; `$BEES_LOG_FORMAT`, then [`logging.format`](configuration.md#logging). |
| `--log-level <debug\|info\|warn\|error>` | Console log level. Default `info`; `$BEES_LOG_LEVEL`, then [`logging.level`](configuration.md#logging). |
| `-h, --help` | Help for any command. |

A flag beats its environment variable, which beats the
[`[logging]` table](configuration.md#logging) in `bees.toml`, which beats the
default. An unknown value is an error naming the valid ones.

## Setting up

### `bees init`

Creates `bees.toml` in the current directory (which must be a git clone of the
project), creates the state directory, adds it to the repository's `.gitignore`
(unless `git check-ignore` says it is already ignored, or it lives outside the clone)
and prints a reminder to commit that, and creates the workflow labels in the GitHub
repository. Refuses to overwrite an existing file. `bees.toml` is meant to be committed.

init validates before it writes: the current directory must be a git clone, and the
configuration it is about to write must parse and resolve to a repository and a default
branch. Values are only written as active settings when you stated them or init really
detected them; a value it could only guess stays a commented placeholder, so init fails
rather than write a default branch nobody confirmed. A failed init leaves no `bees.toml`
behind and the directory exactly as it was, so fixing what the error reports and running
init again works. The one step that can fail after the local files exist is creating the
labels; the error then says to run
`bees labels sync`, not init again.

Last, init runs the **full** [`bees doctor`](#bees-doctor) — the expensive per-role
checks included, since this is where a wrong skill URL or an unreachable MCP server is
worth waiting for — prints the table and points at `bees doctor`. A check that fails
does **not** make init exit non-zero: `bees.toml` and the labels are written by then,
and the table is the list of what is left to set up.

| Flag | Description |
|---|---|
| `--remote name` | Git remote the factory pushes to (default `origin`). |
| `--repo owner/name` | Write `project.repo` as an active setting, and `project.default_branch` too when it could be detected from the remote. By default both are derived from the remote at run time and only appear as commented placeholders showing the detected values. |
| `--default-branch <name>` | Write `project.default_branch` as an active setting, as given: no detection, no check against the remote. Use it when the branch cannot be detected (a remote that cannot be reached), which is otherwise what makes init fail. |
| `--label <name>` | Visibility label (default `bees`). |
| `--assignee <login>` | Only see items assigned to this login; `@me` for yourself. |
| `--print` | Print the template to stdout instead of writing it. Writes nothing, so it works outside a git clone. |
| `--no-labels` | Skip creating GitHub labels. |

The generated file lists every option; optional ones are commented out with their
default values (`#max_developers = 1`), so configuring is a matter of uncommenting and
editing lines. See [configuration.md](configuration.md).

```sh
cd ~/src/my-project
bees init
bees init --assignee @me --label kyle-bees
```

`bees.example.toml` at the repository root is the same template with the placeholders
left in. Regenerate it after changing `internal/config/template.go`:

```sh
go test ./internal/config -update
```

A golden-file test (`TestExampleTOMLInSync`) fails when the two drift, so `dagger check`
enforces the sync.

### `bees doctor`

Runs the preflight checks the factory otherwise only discovers mid-run, and prints
what it found grouped by area:

| Group | Checks |
|---|---|
| `toolchain` | `git` on `PATH`; `gh` on `PATH`, authenticated and holding the `repo` token scope; `claude` (or `$BEES_CLAUDE_BIN`) runnable and new enough. |
| `config` | `bees.toml` loads and validates; `project.repo` and `project.default_branch` are set or derivable; the remote answers; the state directory is ignored by git; the notes directory is writable; every configured `prompt_file` exists. |
| `github` | The repository is readable and writable (`viewerPermission`); every workflow label exists; the visibility filter matches at least one open issue; with `auto_merge` on, what a merge is actually gated on. |
| `workspace` | A worktree can be created under `workspace_root` and removed again. |
| `roles` | Per role: every configured skill URL clones and produces a plugin directory; every configured MCP server starts and answers an `initialize` request within 15s; a configured `shell` can be executed. |

A **failure** (`✗`) means the factory cannot run: a missing tool, a repository it
cannot push to, missing workflow labels. A **warning** (`!`) means something that
will probably bite you but does not stop a session: a state directory that is not
git-ignored (notes and transcripts would be committed), a filter that matches no
open issue (usually a misconfigured label or assignee), a Claude Code older than
bees expects. The filter check tells the two empty cases apart: when nothing matches
the filter but open issues or pull requests carry the base label, it reports both
counts and spells the filter out (`0 match your filter (label=bees AND
assignee=kyle)`) - that is a filter criterion hiding work the factory already owns,
not an empty repository, and the fix is `bees doctor --fix` (below) or unsetting the
criterion in `bees.toml`. The auto-merge check is a warning of the same kind: with
`roles.reviewer.auto_merge` on and no check required on the default branch, bees gates
a merge on whatever checks the pull request reports, which is worth knowing once —
requiring your CI checks in the branch protection rules is the fix, and leaving it as
it is a legitimate choice. bees never enables or edits branch protection itself, and
the check is silent when `auto_merge` is off. Every warning and failure prints the
command that fixes it on the next line; doctor changes nothing unless `--fix` is given.

doctor exits 1 when a check failed and 0 when only warnings are present, so it can
gate a deploy. Checks that need something that is missing are left out rather than
reported twice: without a `bees.toml` only the toolchain checks run, and the GitHub
and workspace checks need a resolved repository.

**Cheap and expensive checks.** The `roles` group is the expensive half: it clones
skill repositories and starts MCP servers, which takes seconds to minutes on a cold
cache. `bees doctor` and `bees init` run it; the `bees run` preflight does not (see
[`bees run`](#bees-run)). Each role reports one line per thing it configures, named
after the role (`developer skills`, `qa mcp`), so the table says which role is broken
rather than that something is. A role that configures none of the three still gets a
line, and a role with `enabled = false` is reported as disabled rather than dropped
silently. The skills are cloned into the cache a session uses (`$BEES_CACHE_DIR`, else
`~/.cache/bees`), so doctor warms it instead of duplicating the work.

```
$ bees doctor
toolchain
  ✓ git                         /usr/bin/git (git version 2.50.1)
  ✓ gh authenticated            logged in as kyle, token scopes: gist, read:org, repo
  ✓ claude runnable             claude 2.1.251 at /usr/local/bin/claude

config
  ✓ bees.toml valid             /home/kyle/src/proj/bees.toml (version 1)
  ✓ project repo                kyle/proj, default branch main (remote "origin")
  ✓ remote reachable            origin answers
  ! state dir ignored           .bees is not ignored by git
      → add "/.bees/" to .gitignore: notes, mail and session transcripts would be committed otherwise
  ✓ notes dir writable          /home/kyle/src/proj/.bees/notes
  ✓ prompt files exist          no prompt_file configured

github
  ✓ repo readable and writable  kyle/proj (ADMIN)
  ✗ workflow labels             2 of 17 missing: bees:size/l, bees:size/xl
      → run `bees labels sync`
  ✓ filter matches issues       12 open issues matching label bees
  ✓ auto_merge check gate       auto_merge is off: people merge pull requests themselves

workspace
  ✓ worktree                    created and removed one under /tmp/bees

roles
  ✓ product_manager             enabled, no skills, MCP servers or shell configured
  ✓ project_manager             enabled, no skills, MCP servers or shell configured
  ✓ developer skills            1 skill ready: https://github.com/acme/skills#skills/tdd
  ✗ developer mcp               sentry: fork/exec /opt/sentry-mcp: no such file or directory
      → start the server by hand or fix [roles.developer.mcp] in /home/kyle/src/proj/bees.toml: a session that cannot reach it loses those tools
  ✓ reviewer                    enabled, no skills, MCP servers or shell configured
  ✓ qa                          disabled (roles.qa.enabled = false)

20 checks: 17 passed, 1 warning, 2 failed
```

| Flag | Description |
|---|---|
| `--json` | Print the results as JSON (`name`, `group`, `status`, `detail`, `remediation`) instead of the table. |
| `--fix` | Apply the repairs doctor knows how to make, then re-run the checks. |

#### `bees doctor --fix`

`--fix` runs the checks, applies the repairs doctor knows how to make for the ones
that did not pass, prints one line per action and then **re-runs every check**, so the
table is what the repository looks like afterwards and the exit code follows the
repair: `--fix` exits non-zero only if a check still fails. Checks doctor cannot
repair are untouched, and their remediation line still says what to do by hand.

Exactly one repair exists today: **the filter check**. It lists the open issues *and*
pull requests carrying the base label (`filter.label`) that do not match the rest of
the filter, and adds `filter.assignee` and, when one is configured, `filter.milestone`
to each. That is the repair for the failure this exists to catch - adding
`assignee = "@me"` to a factory that has been running for weeks takes every issue
nobody ever assigned out of the factory's view in one commit.

```
$ bees doctor --fix
fixing filter matches issues
  assigned issue #92 to kyle
  assigned issue #119 to kyle
  assigned pull request #148 to kyle
  ! issue #131: assign to kyle: gh: HTTP 403 (forbidden)
...
  ✓ filter matches issues       12 open issues matching label bees + assignee kyle
```

What it will not do:

- **It never touches an item that does not carry the base label.** That is the safety
  rule that makes bees usable in a repository shared with people, and it is enforced
  on selection *and* again per item before any write. Selection is on the label alone
  - never on who wrote the issue: a feature issue a person filed with the `bees` label
  and no assignee is adopted exactly like one the factory created.
- **It never adds or removes a label**, and it never edits `bees.toml`. `--fix` moves
  items into the filter; deciding what the filter should be is yours.
- **With `filter.require_label = false` it does nothing at all** and says so in one
  line. Without a base label there is no way to tell the factory's work from everyone
  else's, and "assign every issue in the repository" is not a repair. If you run with
  an assignee-only filter, bring items into it by hand.
- One item it cannot repair is reported on its own line and does not stop the others.

### `bees labels sync`

Creates or updates every workflow label in the repository (idempotent), forcing the
factory's colour and description on the labels that already exist. Run it after
changing `filter.label`. Labels that are merely *missing* need no sync: `bees run`
creates them at start.

### `bees labels list`

Prints the label names and what each one means.

### `bees skills list`

Prints the skill repositories configured for the enabled roles: the cache directory and
the refresh policy on the first line, then one line per reference with its commit (or
`not cached`), how long ago it was fetched, the roles that use it and the reference
itself. Reads the cache only — no session, no GitHub.

```
$ bees skills list
/home/kyle/.cache/bees  (refresh: 24h)
9f1c0aa     3h ago  developer,reviewer  https://github.com/acme/skills#skills/tdd
not cached  -       qa                  https://github.com/acme/qa-skills
```

### `bees skills update`

Clones what is missing and pulls everything else right now, whatever `skills_refresh`
says. With no argument (or `--all`) it updates every configured reference; arguments
must match a configured reference verbatim.

```
$ bees skills update
updated https://github.com/acme/skills#skills/tdd 9f1c0aa → 2b7d431
unchanged https://github.com/acme/qa-skills 4c19e02
```

A reference that fails prints `failed <ref>: <error>` and the command exits non-zero
after trying the rest. Pinned references (`@v1.2.0`) are detached checkouts and cannot
be pulled; that failure is expected.

### `bees config validate`

Loads `bees.toml` and reports errors (missing or unsupported `version`, unknown keys,
bad repo, invalid MCP server, ...).

### `bees config migrate`

Rewrites `bees.toml` to the current format version (see
[`version`](configuration.md#version)), keeping the original as
`bees.toml.v<old>.bak`. Prints "already version N" when nothing needs doing. `bees
run`, `tick`, `exec` and `status` run the same migration automatically on startup.

### `bees config show [role]`

Prints the resolved configuration as JSON: project, filter, scheduler and — for every
role, or the one given — the effective prompt, skills, MCP servers, model, fallback
model, limits and `enabled` after merging `[global]` with `[roles.<name>]`. The
global-only `skills_refresh` is printed under every role, since it governs how each
role's skills are refreshed.

The JSON keys are the `bees.toml` key names, so you can match what is printed against
what you wrote, and durations print as duration strings (`"45m0s"`). The role-specific
keys appear on the role that owns them: the reviewer carries its merge policy
(`auto_merge`, `merge_method`, `checks_wait`, `checks_poll_interval`, `checks_timeout`,
`max_check_fix_rounds`) and the developer its `commit_flags`, `max_size` and
`model_by_size`.

```sh
bees config show
bees config show developer
```

```json
{
  "path": "/src/widgets/bees.toml",
  "version": 1,
  "filter": { "label": "bees", "require_label": true, "assignee": "@me", "milestone": "" },
  "scheduler": { "poll_interval": "5m0s", "max_developers": 1, "max_review_rounds": 3, "...": "" },
  "roles": {
    "reviewer": {
      "name": "reviewer",
      "model": "opus",
      "fallback_model": "sonnet",
      "max_turns": 200,
      "timeout": "45m0s",
      "enabled": true,
      "auto_merge": false,
      "merge_method": "squash",
      "checks_wait": "1m0s",
      "checks_poll_interval": "2m0s",
      "checks_timeout": "30m0s",
      "max_check_fix_rounds": 2
    }
  }
}
```

### `bees prompts show <role> [--rendered]`

Without `--rendered`, prints the role's built-in base prompt (the part busybees ships).
With `--rendered`, prints the full system prompt the role would receive for this
project: common preamble, base prompt and your `bees.toml` additions, with placeholder
values for the worktree and issue.

```sh
bees prompts show reviewer
bees prompts show pm --rendered | less
```

## Running the factory

### `bees run`

Runs the scheduler until interrupted. Every `poll_interval` (default 5m; two API calls
per poll) it lists visible issues and PRs, delivers new human reviews and comments on
factory PRs to the developer as mail (sending an approved issue back to `bees:ready`),
reconciles labels (unlabelled issues go to the product manager, answered questions
unblock), hands ready issues to free developer workers and starts the product manager,
project manager and QA when they have work. It does not wait out the interval for what
happens locally: a finished session wakes it, so a freed developer slot and mail one
role wrote to another are picked up at once, without polling GitHub again. Ctrl-C
stops polling and waits for running sessions to finish.

Before the first poll it runs the **cheap half of [`bees doctor`](#bees-doctor)** —
every check except the `roles` group, which clones skills and starts MCP servers — and
refuses to start when one of them fails: it prints the doctor table and exits non-zero,
having started no session. Warnings do not stop it and are not printed, so a start that
is going to work stays quiet. `--skip-doctor` bypasses the preflight. `bees tick` and
`bees exec` never run it: they are debugging commands and must stay usable on a
half-configured machine.

```
$ bees run
github
  ✗ workflow labels             2 of 19 missing: bees:size/l, bees:size/xl
      → run `bees labels sync`
...
Error: preflight: 1 of 14 checks failed — fix them, run `bees doctor --fix`, or start anyway with `bees run --skip-doctor`
```

At start it lists the repository's labels once and creates any workflow label
that is missing, so a repository set up by an older `bees init` gains the labels
newer versions need without a `bees labels sync`. Labels that already exist are
left untouched, colour and description included. Failing to read or create them
only logs a warning; the run continues.

| Flag | Description |
|---|---|
| `--once` | Do one pass and exit when the sessions it started finish. Same as `bees tick`. |
| `--roles a,b` | Only run these roles (aliases accepted: `pm`, `pjm`, `dev`, `reviewer`, `qa`). |
| `--skip-doctor` | Start without running the doctor preflight. |
| `--no-tui` | Log to the console instead of drawing the terminal UI. Nothing is drawn yet, so today the flag changes nothing; a stdout that is not a terminal turns the UI off on its own. |

```sh
bees run
bees run --roles dev,reviewer
bees -v run --once
bees --log-format json --quiet run
```

Every finished session prints one summary line. In `text` format they are the
message alone, so a run reads as a report:

```
✓ project manager issue #12 done: "refined and moved to ready" (34 turns, $0.61, 3m02s)
✓ developer issue #12 → PR #31 opened (87 turns, $2.41, 11m37s)
✗ reviewer PR #31 changes requested: "tests missing for the error path" (52 turns, $1.18, 6m14s)
✓ developer issue #12 → PR #31 updated (41 turns, $0.98, 5m03s)
✓ reviewer PR #31 approved: "lgtm" (23 turns, $0.47, 2m41s)
⚠ issue #14 escalated to a human: Checks on #33 still fail after 2 fix rounds: go / test
```

With `--log-format json` the same line is an ordinary record carrying its
numbers as fields:

```json
{"time":"2026-08-29T10:14:02Z","level":"INFO","msg":"✓ developer issue #12 → PR #31 opened","summary":true,"role":"developer","issue":12,"pr":31,"outcome":"pr-opened","turns":87,"cost_usd":2.41,"duration":697000000000,"note":""}
```

`--quiet` keeps the summary lines, warnings and errors and drops the rest, so a
service can run the factory and still see what it did.

`run`, `tick` and `exec` also write every record — at debug level, whatever the
console flags say — as JSON to `<state_dir>/bees.log`. It rotates in place at
10 MiB into `bees.log.1` and `bees.log.2`; older generations are dropped. The
log file is a diagnostic, never a reason not to start: if it cannot be opened
(a read-only or full state directory) the run continues with console logging
only, after one warning naming the path and the reason.

### `bees tick [--roles a,b]`

One scheduler pass, then wait for everything it started. Useful for cron-style
operation or for watching a single cycle while tuning prompts.

### `bees exec <role> [--issue N] [--pr N]`

Runs one session for a role right now, outside the polling loop, with the same prompts
and label transitions the scheduler would apply.

| Role | Arguments |
|---|---|
| `pm`, `pjm`, `qa` | None. Polls GitHub, reconciles, then runs the role once. |
| `developer` | `--issue N`. Runs the full developer ↔ reviewer loop for that issue. |
| `reviewer` | `--issue N` or `--pr N` (the PR's closing issue is used). Moves the issue into review and runs the loop from the review stage, through the pre-review checks read. |

```sh
bees exec pjm
bees exec developer --issue 12
bees exec reviewer --pr 34
```

### `bees status [--json]`

Shows the last poll time and PID of the scheduler, queue sizes per workflow state
(plus `feedback` and `features`, the open `bees:feedback` and `bees:feature` issues
owned by the product manager, `proposals`, the subset of `features` still waiting for
a person to approve them, and `open_prs`), running developer workers (issue, [size](workflow.md#sizing), stage, round, and the attempt number while a session is being retried), a row per
role, and unread mail per role. Reads `status.json` from the state directory, so it
works while `bees run` is active in another terminal.

A worker's stage is `develop`, `pre-review checks`, `review` or `checks`. Once the
checks stage knows what it is waiting for, the stage names the gate — `checks (required)`, `checks (reported)`
or `checks (none)` — so a worker sitting in a 30-minute wait says whether it is
waiting on the branch's required checks, on the checks the pull request happens to
report, or on nothing at all. See
[auto-merge](configuration.md#rolesreviewer-only-checks-and-auto-merge).

The `roles:` table covers all five roles with what each is doing (`running` or
`idle`; `-` for the developer and reviewer, whose work is in the workers table
above), when it last ran, and how big its [notes file](roles.md#notes-files) has
grown — a role whose notes are getting long is a candidate for
`bees notes reset`:

```
roles:
  product_manager  idle     last run 12m0s ago    notes 4.2 KB
  project_manager  running  last run 1m0s ago     notes 2.1 KB
  developer        -        last run never        notes 31.4 KB
  reviewer         -        last run never        notes 6.0 KB
  qa               idle     last run 2h0m0s ago   notes -
```

Notes sizes are read from the files when the command runs, not from
`status.json`, so they are right even when the scheduler has never run; `--json`
carries them as `notes_bytes` (role → bytes).

A `no_state` queue counts issues that are visible to the factory but carry no
workflow state label yet — usually ones a person just filed from the GitHub UI. The
scheduler reads them as feedback and gives them `bees:feedback` on its next reconcile,
so the row normally disappears again within the same pass and the `feedback` row grows
by one. A workflow-state queue is omitted while it is
empty (`feedback`, `features`, `proposals` and `open_prs` are always shown).

Under the scheduler line it also reports what the factory has spent since midnight,
summed from the
[session ledger](#bees-cost---since-24h---by-roleissueday---json) (`today` in
`--json`):

```
today: 23 sessions, 412 turns, $8.12
```

With [`scheduler.max_cost_per_day`](configuration.md#cost-budgets) configured, the
scheduler line itself carries the rolling 24-hour spend against that budget — a
different window from `today:` above — and says plainly when the budget has paused
dispatch:

```
scheduler: pid 4711, last poll 12s ago   daily budget: $42.10 / $100.00
scheduler: pid 4711, last poll 12s ago   paused: daily budget ($101.20 / $100.00)
```

While it is paused the scheduler keeps polling and reconciling labels but starts no
new session; the workers already running finish their loop. Both numbers come from
`status.json` (`budget_paused`, `day_spend_usd` and `day_budget_usd` in `--json`), so
they are what the scheduler last computed rather than a fresh sum.

The [claude session limit](configuration.md#the-claude-session-limit) pauses the
factory the same way, and is reported before the budget because it is the harder
stop — it names the time it lifts (`limit_paused_until` in `--json`):

```
scheduler: pid 4711, last poll 12s ago   paused: claude session limit until 23:50 (in 37m)
```

The `ready` queue also carries a breakdown by [size](workflow.md#sizing)
(`ready_sizes` in `--json`); issues the scheduler has not sized yet are
counted as `unsized`:

```
  ready          4  (xs 1, s 2, m 1)
```

A `work hours:` line always follows it. With
[`scheduler.work_hours`](configuration.md#work-hours) configured it reports whether
the factory is inside the window, the window itself, and when the next GitHub poll
is due:

```
work hours: yes (09:00-18:00 mon-fri, America/New_York)   next GitHub poll in 2m55s
```

Without it, the line says so and names the cadence in force instead, because a
missing line would be indistinguishable from a factory that polls around the clock:

```
work hours: not configured — GitHub polled every 5m0s   next GitHub poll in 2m55s
```

When `scheduler.timezone` is unset the window is read in the machine's local time,
which is printed as the abbreviation and offset in force right now rather than the
uninformative `Local`:

```
work hours: no (09:00-18:00 sat,sun, local time (PDT -07:00))
```

`--json` reports the same answer as a `work_hours` object, computed when the
command runs:

```json
"work_hours": {
  "configured": true,
  "in_work_hours": false,
  "window": "09:00-18:00 mon-fri, America/New_York",
  "poll_interval": "1h0m0s",
  "checked_at": "2026-08-29T20:48:00Z"
}
```

`in_work_hours` is omitted when `configured` is `false`, and `poll_interval` is the
cadence in force at `checked_at` (so `off_hours_poll_interval` outside the window).
This is the **live** answer. `status.in_work_hours` and `status.next_poll` next to
it are the **scheduler's own record** from its last pass, and go stale as soon as
the scheduler stops; both are reported so the stale one is never the only one
available.

Ready issues held back by an open [dependency](workflow.md#dependencies) are counted
on the `ready` row and listed below the queues:

```
queues:
  ready          4  (xs 1, s 1, 2 waiting on deps)

waiting on dependencies:
  #40  blocked by #37
  #46  blocked by #44
```

`--json` carries the same information as `waiting_on_deps` (issue number → open
blockers).

### Degraded operations

A factory operation that keeps failing — assigning what a session created, editing a
label, the poll itself — used to leave nothing behind but a warning in the log, so a
half-broken run looked exactly like a healthy one. Every such operation now reports
its outcome under a short, stable name, and the ones failing right now are listed
after the `last error` line:

```
degraded:
  assign           12 consecutive failures over 3h10m   last: GraphQL: Projects (classic) is being deprecated
  label            1 failure   last: gh: HTTP 403 (Resource not accessible by integration)
```

The section is absent entirely when nothing is failing. A single success clears the
operation's streak and removes its line. `--json` carries the same entries as
`status.degraded` (`op`, `count`, `first`, `last`, `last_error`, `escalated`).

Three consecutive failures of one operation also print one line into the run's
output — once per streak, not once per pass:

```
⚠ assign has failed 3 times in a row: GraphQL: Projects (classic) is being deprecated
```

Nothing else happens: the scheduler does not retry, back off or stop, and no issue is
commented on — there is no issue to comment on for a factory-wide operation, and no
role can fix a broken credential or a missing label. This is visibility only.

## The mailbox

Roles talk to each other only through the local mailbox in `<state_dir>/mail`. The
scheduler delivers messages by including them in the prompt of the session working on
the referenced issue or PR and marks them read afterwards.

### `bees kill [--dry-run] [--scheduler] [--grace 5s]`

Cleans up after a crash: finds Claude Code sessions started by bees, terminates them
together with their process groups (MCP servers, shells), removes stale pid files,
removes the temporary worktrees bees created under the workspace root, and resets the
worker list in `status.json`.

Sessions are found two ways: from the `pid` file each running session keeps in its
`<state_dir>/sessions/<id>/` directory, and from the process table, limited to sessions
of this state directory — a `claude` process counts only when it carries the
`--name bees-…` argument every session is started with *and* its command line
references `<state_dir>/sessions/`. Another project's factory running on the same
machine is never touched, whichever config you point `bees kill` at. Pid files are
cross-checked against that scan, so a pid reused by an unrelated process after a reboot
is discarded, never killed.

It refuses to run while a `bees run` scheduler is alive (killing sessions under a running
scheduler would corrupt its state); pass `--scheduler` to stop the scheduler too.

| Flag | Meaning |
|---|---|
| `--dry-run` | Show what would be killed and removed. |
| `--scheduler` | Also stop a running scheduler (found via the pid in `status.json`). |
| `--grace 5s` | Time to wait after SIGTERM before SIGKILL. |

```sh
bees kill --dry-run
bees kill
bees kill --scheduler      # the scheduler itself is hung
```

### `bees mail send` *(sessions)*

| Flag | Description |
|---|---|
| `--to <role>` | Recipient role (required). |
| `--from <name>` | Sender. Default `$BEES_ROLE`; humans must pass one, e.g. `--from human`. |
| `--subject <text>` | Subject line. |
| `--body <text>` | Body. |
| `--body-file <path>` | Read the body from a file; `-` for stdin. |
| `--issue N` | Issue the message is about. Default `$BEES_ISSUE`. |
| `--pr N` | Pull request the message is about. Default `$BEES_PR`. |
| `--in-reply-to <id>` | Id of the message being answered. |

Attach `--issue`/`--pr` whenever possible: that is how the scheduler routes the message
to the right developer session, and how an answer unblocks a `bees:blocked` issue.

`bees mail` talks to the state directory of `$BEES_STATE_DIR` when it is set (that is
how a session reaches its own mailbox), but an explicit `--config` wins over it, so
`bees -c other/bees.toml mail send ...` inside a session reaches the other project.
The confirmation line names the state directory the message landed in:
`sent <id> to <role> (<state dir>)`.

```sh
bees mail send --to project_manager --issue 12 --subject "Which auth scheme?" --body "JWT or sessions?"
bees mail send --to developer --pr 34 --issue 12 --subject "Review round 1" --body-file review.md
bees mail send --from human --to product_manager --subject "Priority" --body "Ship billing before reports."
bees mail send --from human --to developer --issue 12 --body "Keep the CLI flag names as they are."
```

### `bees mail list`

| Flag | Description |
|---|---|
| `--to <role>` / `--from <name>` | Filter by recipient or sender. |
| `--issue N` / `--pr N` | Filter by referenced item. |
| `--unread` | Only undelivered messages. |
| `--full` | Print whole messages instead of one line each. |

Unread messages are marked with `*`.

### `bees mail read <id>`

Prints one message.

## Notes

`<state_dir>/notes/<role>.md` is a role's only memory between sessions: its
contents go into every task prompt and the role updates it before it finishes.
These commands are how a person reads and steers it; see
[Notes files](roles.md#notes-files). Roles accept the usual aliases (`pm`,
`pjm`, `dev`, `review`, `qa`).

### `bees notes show <role>`

Prints the notes file (nothing when the role has never run).

### `bees notes edit <role>`

Opens the notes file in `$VISUAL`, else `$EDITOR`, else `vi`, creating it first
if needed, and exits with the editor's status. It needs a terminal, so it
refuses to run inside a session (`$BEES_SESSION_DIR` set) — sessions edit their
file directly.

### `bees notes reset <role>`

Moves the notes file to `<state_dir>/notes/archive/<role>-<timestamp>.md`,
prints that path and leaves a fresh file behind. Use it when a role has
accumulated advice that no longer applies; nothing is lost, the archive stays.

### `bees notes add <role> [text]`

Appends one bullet to the notes file, creating it when needed:

```
bees notes add developer "Always run dagger check before committing"
bees notes add pm --body-file vision.md
```

Pass `--body-file <path>` (or `--body-file -` to read stdin) instead of the
argument for longer text. A note spanning several lines keeps its line breaks;
every line after the first is indented by two spaces so the whole note stays
inside one bullet. Like `bees mail`, `show`, `reset` and `add` find the
state directory from `$BEES_STATE_DIR` before falling back to `bees.toml`, so a
session can append to its own notes without a config file.

## Creating issues

### `bees issue create` *(sessions, and humans)*

Creates an issue the way the factory wants it. Roles are told to use this instead of
`gh issue create`; it is equally handy for people.

| Flag | Meaning |
|---|---|
| `--title` | Required. |
| `--body` / `--body-file` | Body text, or a file (`-` for stdin). |
| `--parent N` | Make the new issue a native GitHub **sub-issue** of feature `N` and inherit its milestone. |
| `--related N` | Inherit the milestone of issue `N` without attaching (a bug found while working on `N`, a feature distilled from feedback `N`). Exclusive with `--parent`. |
| `--milestone T` | Set the milestone explicitly (overrides inheritance). |
| `--bug` | Bug work item (`bees:bug`). |
| `--feature` | Feature issue for the product manager (`bees:feature` + `bees:proposal`, no state label). |
| `--ready` | Work item is already detailed: `bees:ready` instead of `bees:triage`. |
| `--blocked-by N` | Repeatable. Prefixes the body with a `Blocked by #N` line, so the scheduler does not build the issue while `N` is open (see [Dependencies](workflow.md#dependencies)). No GitHub dependency relationship is created. |
| `--label L` | Extra label (repeatable). |

What it always does: adds the visibility label and, when `filter.assignee` is set, the
assignee; adds the kind label; adds `bees:triage` (or `bees:ready`) to work items — feature
issues get no state label; resolves the milestone as *explicit → parent/related issue's
milestone → `filter.milestone`*; and, with `--parent`, attaches the issue as a sub-issue
(three API calls: parent details, create, attach). Bees never create, edit or close
milestones themselves; inheritance is the only way a milestone gets set by the factory.

```sh
bees issue create --parent 12 --title "Export as CSV" --body-file body.md      # work item under feature #12
bees issue create --bug --related 34 --title "Crash on empty input" --body "…"  # bug in #34's milestone
bees issue create --feature --related 40 --title "Search" --body-file body.md   # feature from feedback #40
bees issue create --title "Fix typo in README" --ready                          # fast-tracked work item
bees issue create --parent 12 --blocked-by 37 --title "Order the queue" --body-file body.md  # waits for #37
```

### `bees issue link --parent N --child M`

Attaches existing issue `M` as a sub-issue of feature `N` (for example a bug filed by
QA that turns out to belong to a feature in progress).

Becoming a sub-issue carries the feature's milestone across, exactly as `--parent` does
on `bees issue create`, so an issue attached after the fact lands in the same release as
one created under the feature. It only ever *fills in* a milestone: an issue that already
has one keeps it, and the command says which milestone it set, if any. Refuses a parent
that is still a proposal.

## Reporting outcomes

### `bees done <status> [-m note] [--pr N] [--issue N]` *(sessions)*

The last command every session runs. Writes `outcome.json` into `$BEES_SESSION_DIR`;
the scheduler reads it to decide what happens next. A session that ends without an
outcome is treated as failed. Statuses are validated against `$BEES_ROLE`:

| Role | Valid statuses |
|---|---|
| `product_manager` | `done`, `idle`, `failed` |
| `project_manager` | `done`, `idle`, `failed` |
| `developer` | `pr-opened --pr N`, `pr-updated --pr N`, `question`, `failed` |
| `reviewer` | `approved`, `changes-requested`, `failed` |
| `qa` | `done`, `failed` |

`pr-opened` and `pr-updated` require a PR number (`--pr` or `$BEES_PR`). `question` and
`changes-requested` are only honoured if the session actually sent the corresponding
mail; otherwise the issue is escalated to a human.

```sh
bees done pr-opened --pr 34
bees done changes-requested
bees done approved -m "Clean implementation, tests cover the edge cases"
bees done failed -m "Could not get the test-suite to run: missing DATABASE_URL"
```

### `bees mcp serve` *(sessions)*

Runs the built-in MCP server on stdio. You never start it yourself: `bees` writes it
into every session's `mcp.json` as the server named `bees`, and claude starts it as
`<bees binary> mcp serve` with the session's `BEES_*` variables. The name `bees` is
reserved — a `[global.mcp.bees]` or `[roles.<role>.mcp.bees]` entry in `bees.toml`
fails validation.

The server is backed by the same code as the commands above, so a tool and its
command do exactly the same thing. Claude Code exposes the tools as
`mcp__bees__<name>`:

| Tool | Arguments | Same as |
|---|---|---|
| `mail_send` | `to`, `subject`, `body`, optional `issue`, `pr`, `in_reply_to` | `bees mail send` |
| `mail_list` | optional `unread`, `issue`, `pr` | `bees mail list --full` |
| `issue_create` | `title`, `body`, optional `parent`, `related`, `milestone`, `bug`, `feature`, `ready`, `labels`, `blocked_by` | `bees issue create` |
| `issue_link` | `parent`, `child` | `bees issue link` |
| `done` | `status`, optional `note`, `pr`, `issue` | `bees done` |

The rest are GitHub operations. They are the same `gh` calls a role would build by
hand, with the factory's rules applied — so a prompt no longer has to restate them:

| Tool | Arguments | Offered to | Does |
|---|---|---|---|
| `issue_view` | optional `number` | every role | Prints an issue: state/kind/size labels, milestone, parent feature, body, then every comment oldest first, marked as a bee's or a person's. |
| `pr_view` | optional `number` | every role | Prints a pull request: title, head → base, draft flag, body, required-check summary with the failed check names, then every review and comment a person left. |
| `comment` | `number`, `body` | every role | Comments on an issue or pull request, appending the role's `<!-- bees:<role> -->` marker unless the body's last line already is it. A marker quoted anywhere else — including as the last line, `> <!-- bees:<role> -->` — does not suppress it, so the marker line is always the poster's. |
| `issue_edit_body` | `number`, `body` | product_manager, project_manager | Replaces an issue body. Refuses a `bees:feature` or `bees:feedback` issue for anyone but the product manager. |
| `issue_set_state` | `number`, `state` (`ready`\|`blocked`), `size` (`xs`…`xl`, required for `ready`) | project_manager | Moves a work item out of `bees:triage` in one label edit, replacing any existing size. Refuses an issue that is in any other state, naming it. |
| `issue_question` | `number`, `waiting` | product_manager | Adds or removes `bees:question`. Refuses anything that is not a feature or feedback issue. |

Every one of them refuses an issue or pull request that does not match the
[filter](workflow.md#what-the-factory-can-see), and every write is a refusal or a
single `gh` call — there is no partial state to clean up.

`issue` and `pr` default to `$BEES_ISSUE`/`$BEES_PR`, so a session rarely passes them
(`issue_view` and `pr_view` default their `number` the same way). The schemas depend on
`$BEES_ROLE`: `done`'s `status` enum is exactly the role's valid outcomes (a developer
sees `pr-opened, pr-updated, question, failed`; a reviewer
`approved, changes-requested, failed`), and an unknown or empty role gets the full tool
set with no enum, so the server is usable by hand.

### `bees mcp tools [role]`

Prints the tools a role's session sees, with the enum of every constrained
parameter — the part that differs between roles:

```
$ bees mcp tools developer
mcp__bees__comment          Comment on an issue or pull request
mcp__bees__done             Report the session outcome
    status: pr-opened | pr-updated | question | failed
mcp__bees__issue_create     Create a factory issue
mcp__bees__issue_link       Attach an issue to a feature
mcp__bees__issue_view       Read an issue
mcp__bees__mail_list        Read the mailbox
mcp__bees__mail_send        Send mail to another role
    to: product_manager | project_manager | developer | reviewer | qa
mcp__bees__pr_view          Read a pull request
```

The tool *set* differs too: the project manager also sees `issue_edit_body` and
`issue_set_state` (`state: ready | blocked`, `size: xs | s | m | l | xl`), and the
product manager `issue_edit_body` and `issue_question`.

Without a role argument it uses `$BEES_ROLE`, and without that it prints the
unconstrained tool set.

## Misc

### `bees cost [--since 24h] [--by role|issue|day] [--json]`

Reports what finished sessions cost, summed from `<state_dir>/ledger.jsonl`: one JSON
line per session, appended when it ends, with its role, issue, PR, turns, cost,
duration and outcome. The numbers are what `claude` reported; nothing is reconciled
against billing.

```
$ bees cost --since 72h --by role
role             sessions    turns       cost
developer              12      214      $6.10
product_manager         1       11      $0.32
reviewer                9       74      $1.70
total                  22      299      $8.12
```

`--since` is a Go duration (default `24h`). `--by issue` groups by issue number and
collects sessions that belong to no issue (the singleton roles) under `-`; `--by day`
groups by local calendar day. `--json` prints the same groups plus the total. An empty
ledger prints `no sessions recorded`.

### `bees version`

Prints `bees <version>`, resolved from the binary itself:

| Build | Output |
|---|---|
| `go install github.com/kpenfound/busybees/cmd/bees@latest` (or `@v0.2.0`) | The module version Go recorded: a tag (`bees v0.2.0`) or, for an untagged module, the pseudo-version `@latest` resolves to (`bees v0.0.0-20260829201307-b24a0605c2a1`). |
| `go build ./cmd/bees` in a clone | The version Go stamps from the checkout — on Go 1.24+ a pseudo-version, with `+dirty` appended when the working tree has uncommitted changes. |
| A build whose module version is `(devel)` but that carries VCS stamps | `bees dev (b24a0605c2a1)` — the 12-character commit, with ` modified` appended when the working tree was dirty. |
| Built with `-ldflags "-X main.version=v1.2.3"` | `bees v1.2.3`. The override wins over everything else. |
| No build information at all | `bees dev`. |

### `bees completion <shell>`

Generates shell completion scripts (bash, zsh, fish, powershell).
