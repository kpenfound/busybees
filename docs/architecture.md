# Architecture

Internals for people working on busybees itself. For the user-facing view see
[workflow.md](workflow.md), [roles.md](roles.md), [configuration.md](configuration.md)
and [cli.md](cli.md).

## Package layout

```
cmd/bees/            CLI (cobra): init, run, tick, exec, status, mail, issue, done, config, prompts, labels
internal/config/     bees.toml schema, defaults, validation, global/role merging, label names, init template
internal/github/     thin wrapper around the gh CLI (issues, PRs, labels, milestones (read), sub-issues, required checks, merge, comment, PR activity)
internal/issues/     creates issues the factory way: filter labels/assignee, kind + state labels, sub-issue of --parent, inherited milestone
internal/mail/       the local mailbox: JSON messages under <state_dir>/mail/<role>/
internal/workspace/  temporary git worktrees created from the main clone
internal/skills/     clones skill repos by git URL and exposes them as claude plugin dirs
internal/session/    runs one headless `claude -p` session and collects its result and outcome
internal/prompts/    embedded base prompts (system/*.md, task/*.md) and their renderer
internal/state/      state directory: notes, per-issue bookkeeping, singleton run times, status.json
internal/scheduler/  the orchestrator: poll, human feedback, reconcile, developer worker pool, singleton roles
internal/testutil/   test helpers (local bare git remote + clone)
```

Dependency direction is strictly downwards: `cmd/bees` → `scheduler` →
everything else; `scheduler` is the only package that knows about all the
others. `github` and `session` execute external programs (`gh`, `claude`) and
both expose an override point (`Client.Exec`, `Runner.ClaudeBin`) so tests
never need the real ones.

## The scheduler loop

`scheduler.Run` initialises the state directory, prunes stale worktrees, then
repeats a **pass** every `scheduler.poll_interval` (default 5m) until the
context is cancelled. Ctrl-C stops polling and waits for running sessions to
finish. If a pass fails with a rate-limit error (`isRateLimited`: the message
contains "rate limit", "secondary rate" or "abuse detection") the next pass
waits `scheduler.rate_limit_backoff` (default 15m) instead.

A pass is:

1. **poll** – `gh issue list` and `gh pr list` with the filter's query (label,
   assignee, milestone). Issues are bucketed by state label (`triage`, `ready`,
   `in-progress`, `blocked`, `review`, `approved`, `needs-human`, or `""` for
   none) and sorted oldest first. Issues carrying `bees:feedback` or
   `bees:feature` are set aside in `snapshot.feedback` / `snapshot.features`
   instead of being bucketed, so they never get a state label — they belong
   to the product manager. Queue sizes (including `feedback` and `features`)
   go into `status.json`.
2. **human feedback** (`humans.go`) – for every open PR whose closing issue is
   visible, if `pr.updatedAt` is later than the issue's `human_seen_at` (or
   the PR's creation time), fetch its reviews, inline review comments and
   conversation comments with `gh api --paginate` (`github.Client.PRActivity`).
   Items containing `github.BeesMarker` (`<!-- bees:`) — comments written by
   bees, which share the human's `gh` account — and empty approvals are
   dropped. The rest are mailed to the developer as one message from `human`
   (`issue == N`, `pr == M`) whose body carries each item's id and the `gh`
   command to reply to it; `human_seen_at` is advanced to the newest item.
   If the issue was `approved`, it is relabelled `ready` and `bees:approved`
   is removed from the PR, so a developer worker picks it up on step 3.
3. **reconcile** – label transitions driven by local state:
   - an issue with no state label gets `bees:triage` (and the `bees` label if
     the filter did not require it);
   - a `bees:blocked` issue with unread developer mail about it becomes
     `bees:ready`; with unread project-manager mail it becomes `bees:triage`.
4. **dispatch developers** – candidates are unowned `in-progress` and `review`
   issues (resume after a restart) followed by `ready` issues. For each, a
   slot is taken from a buffered channel sized `max_developers`; when none is
   free the pass stops dispatching. A goroutine runs `workIssue` and returns
   the slot when done.
5. **dispatch singletons** – project manager (has triage issues or unread
   mail), product manager (unread mail, first run, or interval elapsed), QA
   (first run, or interval elapsed and something merged since — checked at
   most once per `qa_interval`). Each runs in
   its own goroutine, guarded by a `running` flag so at most one session per
   role exists.

   The product manager's "has work" test also looks at `snapshot.feedback`
   and `snapshot.features`: `freshIssues` fetches comments (`gh issue view`)
   only for issues whose `updatedAt` is later than the product manager's last
   run, and keeps those where `Issue.AwaitingBee()` is true — the latest
   human activity (creation or a comment without the `<!-- bees:` marker) is
   newer than the latest bee comment. A fresh issue that carries
   `bees:question` has that label removed on the spot (a person answered).
   Any fresh issue triggers a run outside `product_manager_interval`.
   `runProductManager` passes fresh feedback issues as `Data.Feedback`, fresh
   feature issues as `Data.FreshFeatures`, every open feature issue as
   `Data.Features` with its sub-issue progress in `Data.Progress` (one REST
   `gh api repos/../issues/N` per open feature, reading `sub_issues_summary`),
   and only work items (neither feature nor feedback) as `Data.Issues`.
   `runProjectManager` looks up each triage item's parent feature with one
   GraphQL query (`ParentIssue`) into `Data.Parents`; the developer worker
   does the same for its issue into `Data.Parent`.

   **Sub-issues and milestones.** Work items are native GitHub sub-issues of
   their feature. Roles create issues through `bees issue create`
   (`internal/issues`): it labels for the filter and kind/state, resolves the
   milestone as explicit → parent/related issue's milestone (`GetIssueDetails`)
   → `filter.milestone`, creates with `gh issue create`, and for `--parent`
   attaches the child with `POST repos/../issues/<parent>/sub_issues`
   (`AddSubIssue`, which needs the child's database id from
   `GetIssueDetails`). The factory never creates, edits or closes milestones;
   people do, and the bees inherit.

**Backoff.** A singleton is never dispatched again sooner than one poll
interval after it finishes, and a failing singleton or developer issue waits
five poll intervals. This stops a broken session from burning tokens in a tight
loop.

**API budget.** Every poll costs exactly two `gh` calls (`issue list`,
`pr list`); everything else is gated on what those lists report so an idle
factory stays at two calls per poll. Human PR feedback is fetched (3 calls)
only for PRs whose `updatedAt` moved past `human_seen_at`; product-manager
feedback/feature comments (1 `issue view` each) only for issues whose
`updatedAt` is newer than the PM's last run; QA's merged-PR query runs at most
once per `qa_interval`, recorded as `last_check` in `<state_dir>/qa.json` so
an elapsed interval with nothing merged does not re-query on every poll; the
checks stage polls `gh pr checks` every `roles.reviewer.checks_poll_interval`
(default 2m), not every poll; the label backstop makes two list calls after
each session; and worker stage transitions make a handful of `issue view` /
`pr view` / `issue edit` calls. Sessions call `gh` on their own on top of
this, which busybees does not meter.

**Once mode.** `Once = true` (`bees tick`, `bees run --once`) performs a single
pass and then waits for everything it started. `OnlyRoles` (`--roles`)
restricts dispatch to the named roles; a role with `enabled = false` in
`bees.toml` is skipped regardless.

`status.json` is rewritten after every pass and whenever a worker or
singleton starts or stops; `bees status` just reads it.

## The developer worker

`workIssue` owns one issue from claim to approval — or, with
`roles.reviewer.auto_merge`, to merge — (or escalation) and runs a
small state machine with two stages:

```mermaid
stateDiagram-v2
    [*] --> develop
    [*] --> review: resumed with an open PR and label bees:review
    develop --> review: pr-opened / pr-updated (PR found)
    develop --> [*]: question (issue → blocked)
    develop --> [*]: failed / no PR (escalate)
    review --> [*]: approved, auto_merge off
    review --> checks: approved, auto_merge on
    review --> develop: changes-requested, round < max
    review --> [*]: changes-requested, round == max (escalate)
    review --> [*]: failed (escalate)
    checks --> [*]: required checks pass → gh pr merge
    checks --> [*]: pending at checks_timeout / merge refused (escalate)
    checks --> develop: a check failed, reviewer (checks mode) mailed a fix request
    checks --> checks: reviewer re-ran the check (approved)
    checks --> [*]: fix rounds exhausted / reviewer failed (escalate)
    develop --> checks: pr-updated while fixing checks (afterDevelop = checks)
```

- **Workspace.** `git fetch`, then one worktree for the issue on
  `<branch_prefix>issue-N`: created from `<project.remote>/<default_branch>` if new,
  checked out tracking the remote if it exists there, or reused if it exists
  only locally. The same worktree serves both developer and reviewer sessions
  for that issue and is removed when the worker exits (unless
  `keep_workspaces`). Before each reviewer session the worktree is
  fast-forwarded to the developer's latest push.
- **Resume.** On start the worker looks for an open PR whose head is the
  branch. If one exists and the issue is labelled `bees:review` it starts in
  the review stage; otherwise in develop. This is how work survives a restart
  of `bees run`.
- **Rounds.** `<state_dir>/issues/<n>.json` records the review round, PR
  number, branch, `check_fix_rounds` and `human_seen_at`. The round is
  incremented on every `changes-requested` and compared with
  `scheduler.max_review_rounds`; human feedback rounds do not count against
  the limit. `check_fix_rounds` is incremented each time the reviewer is asked
  to diagnose failing checks and compared with
  `roles.reviewer.max_check_fix_rounds`.
- **Checks stage** (`auto_merge`). `approve` only labels the PR and issue
  `bees:approved`; merging happens in the `checks` stage. `awaitChecks` sleeps
  `checks_wait`, then calls `github.RequiredChecks` (`gh pr checks --required
  --json …`; gh's non-zero exits for pending/failing checks and its "no
  required checks" / "no checks reported" messages are handled, an empty list
  counts as passed) every `checks_poll_interval` until `Summarize` returns
  passed or failed, or `checks_timeout` elapses. Passed → `MergePR` with `merge_method`
  and `--delete-branch` (a refusal escalates). Failed → a reviewer session
  with `task: "reviewer_checks"` and `BEES_REVIEW_MODE=checks`, given
  `github.Failed(checks)`. Its `changes-requested` sets `afterDevelop =
  "checks"` so the next developer `pr-updated` returns to the checks stage
  instead of review; `approved` re-polls. If the reviewer role is disabled
  the developer's PR goes straight from develop to checks.
- **Verification.** Outcomes that imply a side effect are checked: `pr-opened`
  must correspond to an open PR on the branch (looked up by the reported
  number, else by branch), `question` must have produced mail to the project
  manager during the session, and `changes-requested` mail to the developer.
  A claim without the side effect is escalated rather than trusted.
- **Escalation** (`escalate`) sets `bees:needs-human` and posts a comment;
  it is the only GitHub comment the orchestrator itself writes. Roles comment on
  GitHub to people (developer PR replies, product-manager replies and questions
  on feedback/feature issues), always with the `<!-- bees:<role> -->` marker.

Singleton roles share `runSingleton`: detached worktree on the default branch,
one session, mark delivered mail read, record `LastRun` in
`<state_dir>/<role>.json`.

## Running a session

`session.Runner.Run` executes, inside the worktree:

```
claude -p \
  --output-format stream-json --verbose \
  --dangerously-skip-permissions \
  --append-system-prompt-file <session>/system-prompt.md \
  --model <model> [--fallback-model <fallback>] [--effort <level>] \
  --max-turns <n> --name bees-<session name> \
  --add-dir <state_dir> \
  [--allowedTools ...] [--disallowedTools ...] \
  [--mcp-config <session>/mcp.json --strict-mcp-config] \
  [--plugin-dir <skill plugin dir> ...]
```

The task prompt is written to stdin. Each line of stream-json is appended to
`<session>/transcript.jsonl`; the final `result` event supplies the result
text, `is_error`, subtype, turn count, cost and claude session id. stderr is
saved to `stderr.log` when non-empty, and `result.json` summarises the run.

- **Outcome.** The session ends by running `bees done <status> [-m note]
  [--pr N]`, which writes `<session>/outcome.json`. The runner reads it after
  claude exits; a missing file is reported as `HasOutcome = false` and the
  scheduler treats it as `failed`.
- **Environment.** The configured `env` entries first (`$VAR`-expanded) and
  `SHELL` when `shell` is set; then `BEES_ROLE`, `BEES_SESSION_DIR`,
  `BEES_STATE_DIR`, `BEES_CONFIG`, `BEES_REPO`, `BEES_LABEL`, `BEES_BIN`, plus
  `BEES_NOTES_FILE`, `BEES_ISSUE`, `BEES_PR`, `BEES_BRANCH` when they apply and
  `BEES_REVIEW_MODE=checks` for the reviewer's checks-mode sessions; and, unless
  `GIT_CONFIG_COUNT` is already set, `GIT_CONFIG_*` entries for
  `push.autoSetupRemote=true` / `push.default=current`. The directory holding
  the `bees` binary is prepended to `PATH` so `bees mail`, `bees issue` and
  `bees done` resolve inside the session.
- **Prompts.** `prompts.System` renders `system/common.md` + `system/<role>.md`
  and appends the role's custom text from `bees.toml`; `prompts.Task` renders
  `task/<role>.md`. Both take a single `prompts.Data` struct (project, filter,
  labels, workspace, notes, inbox, issue, PR, lists, round).
- **Skills.** `skills.Manager.Prepare` clones each URL (`<url>[@ref][#subdir]`)
  under `~/.cache/bees/repos/` and returns a plugin directory: the repo itself
  if it has `.claude-plugin/plugin.json`, otherwise a generated wrapper under
  `~/.cache/bees/plugins/<name>/` whose `skills/` symlinks to the skill or
  skills collection. Each becomes a `--plugin-dir`, so the project worktree is
  never modified.
- **MCP.** Servers from the resolved role are written to `mcp.json`
  (`$VAR` in `env` and `headers` expanded from the bees process environment)
  and passed with `--strict-mcp-config`, so sessions see exactly the servers
  in `bees.toml` and nothing from the user's own settings.
- **Timeout.** The role's `timeout` bounds the command context; claude runs in
  its own process group and on expiry the whole group is `SIGKILL`ed so MCP
  servers die with it. The result is marked `TimedOut`.

Unless `GIT_CONFIG_COUNT` is already set, the runner also exports
`GIT_CONFIG_COUNT=2` with `push.autoSetupRemote=true` and `push.default=current`,
so a session can run a plain `git push` on a branch the workspace created with
`git worktree add --no-track -b …` without busybees ever editing the clone's git
configuration.

## The mailbox

A message is one JSON file at `<state_dir>/mail/<to-role>/<id>.json`:

```json
{
  "id": "20260829T151201-9f3a2b1c",
  "from": "reviewer",
  "to": "developer",
  "subject": "Review round 1",
  "body": "...",
  "issue": 12,
  "pr": 34,
  "created_at": "2026-08-29T15:12:01Z",
  "read_at": null,
  "in_reply_to": ""
}
```

Messages are addressed to a **role**, not a session. Delivery rules:

- A developer session for issue N with PR M receives unread developer mail
  where `issue == N` or `pr == M`.
- A reviewer session receives its own earlier feedback for the PR
  (`from: reviewer, to: developer, pr == M`) as "previous rounds".
- Singleton sessions receive all unread mail addressed to their role.
- Mail is marked read (`read_at` set) after the session that received it
  finishes, so a crashed session sees it again.
- Reconcile uses *unread* mail to relabel blocked issues; the scheduler's
  `sentSince` check uses creation time to verify a session really sent what it
  claimed.
- Human PR feedback enters the mailbox as messages from `human` (see the
  scheduler loop); people can also send mail by hand with
  `bees mail send --from human`.

**Label backstop.** After every session (`runSession` in `sessions.go`) the
scheduler calls `adoptCreated`: `github.Client.ListCreatedSince` lists issues
and PRs matching `author:@me created:>=<session start>` regardless of labels,
and anything carrying a `<label>:*` label but missing the base label (or the
configured `filter.assignee`) is labelled/assigned so it stays visible. Items
with no factory label at all are left alone.

Writes are atomic (temp file + rename), IDs embed a timestamp so listing sorts
oldest first, and `bees mail` works from any directory because sessions get
`BEES_STATE_DIR`.

## State directory

```
<state_dir>/                     default .bees/ next to bees.toml
  README.md
  mail/<role>/*.json             mailbox
  notes/<role>.md                role memory
  sessions/<ts>-<name>-<rand>/   system-prompt.md, prompt.md, mcp.json, transcript.jsonl,
                                 stderr.log, outcome.json, result.json
  issues/<n>.json                {number, round, pr, branch, check_fix_rounds, human_seen_at, updated_at}
  product_manager.json           {last_run}
  qa.json                        {last_run, last_check}
  status.json                    live scheduler status for `bees status` (queues, workers, singletons, last_poll, last_error)
```

`bees init` makes sure the directory is ignored by git: if `git check-ignore`
does not already ignore it (and it lives inside the clone), `/.bees/` is
appended to the repository's `.gitignore` — commit that change. `bees.toml`
itself is meant to be committed. Worktrees live under `$TMPDIR/bees/` (or
`scheduler.workspace_root`) and are removed after each worker or singleton
run; the skills cache lives under `~/.cache/bees/` (`BEES_CACHE_DIR`).

## Testing

Nothing in the test-suite talks to GitHub or runs Claude Code.

- **Fake gh.** `github.Client.Exec` is a function field; the scheduler tests
  replace it with an in-memory implementation that understands the `gh`
  invocations the wrapper makes — `issue list` (including `--state all
  --search` for the label backstop), `issue view/edit/comment` (labels and
  `--add-assignee`), `pr list/view/merge/checks` (a queue of scripted check
  results; merge arguments are recorded), `api .../milestones`, `api
  repos/…/issues/N` (REST details: id, milestone, sub-issue summary), `api
  graphql` (parent lookup) and `api …/pulls/N/reviews|comments` and
  `issues/N/comments` (human PR activity) — and records label history,
  comments and merges for assertions.
- **Fake claude.** The scheduler test binary doubles as `claude`: `TestMain`
  checks `BEES_FAKE_CLAUDE=1` and, when set, runs a scripted role
  (`developer` commits and pushes then reports `pr-opened`; `reviewer` mails
  feedback once then approves; singletons report `done`), writes
  `outcome.json` and prints a stream-json `result` line. `Runner.ClaudeBin` is
  set to `os.Args[0]`. The session tests use small shell scripts the same way.
- **Real git.** `testutil.SetupRepos` creates a bare "origin" and a clone with
  one commit; workspace and scheduler tests create real worktrees and push to
  it, then assert the branch history and that no worktrees are left behind.
- **Skills.** `skills.Manager.Git` is replaced with a copy of a fixture
  directory so every supported repository layout is exercised offline.

Run locally:

```
go test ./...
```

Or through Dagger, which is the CI entrypoint. The workspace (`dagger.toml`)
installs the official `github.com/dagger/go` module, providing the checks
`go:lint-all`, `go:test-all` and `go:generate-all`:

```
DAGGER_X_RELEASE=v1.0.0-beta.11 dagger check
DAGGER_X_RELEASE=v1.0.0-beta.11 dagger check -l        # list checks
DAGGER_X_RELEASE=v1.0.0-beta.11 dagger check go:test-all
```

The `DAGGER_X_RELEASE` variable pins the Dagger release used for this
repository; set it in your shell.
