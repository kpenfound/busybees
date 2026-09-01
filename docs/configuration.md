# Configuring busybees: `bees.toml`

Everything a busybees factory does is driven by one file, `bees.toml`, which lives in
the root of a git clone of the project being built. `bees init` writes a starter file in
the style of a classic Unix config: every option is listed, and the optional ones are
commented out showing their default value — uncomment a line to change it. Only
`filter.label` (plus `filter.assignee` when you pass `--assignee`, `project.repo` when you
pass `--repo`, and `project.default_branch` when you pass `--default-branch` or init
detected the branch from the remote) start out active. A value init could only guess is
never written as a setting: it stays a commented placeholder and init fails instead.
`bees config validate` checks the file; `bees config show` prints the resolved settings
for every role after merging.

The file starts with a `version` key, followed by seven top-level tables:

| Table | Purpose |
|---|---|
| `[project]` | The git remote, repository, default branch and state directory |
| `[filter]` | Which GitHub issues and pull requests the factory can see |
| `[github]` | The GitHub account the factory itself acts as |
| `[scheduler]` | Concurrency, polling and review-loop limits |
| `[logging]` | Console log format and level |
| `[global]` | Prompt, skills, MCP servers, model, shell and environment settings applied to every role |
| `[roles.<name>]` | Per-role overrides for `product_manager`, `project_manager`, `developer`, `reviewer`, `qa` |

Unknown keys are an error, so typos are caught at load time.

## `version`

```toml
version = 1
```

The format version of `bees.toml` (not of bees itself); the first key in the file,
written by `bees init`. A file without one is version 0, the format that predates the
key.

- A file **newer** than the running bees understands is refused ("upgrade bees").
- An **older** file is migrated on load — step by step through the migration table in
  `internal/config` — and `bees run`, `tick`, `exec` and `status` then write the
  migrated file back, keeping the original as `bees.toml.v<old>.bak` and logging what
  happened. `bees config migrate` does the same explicitly (handy for reviewing the
  diff with git before starting the factory); `bees config validate` only reports that
  a migration is pending. Migrations rewrite the file's text, so comments and the
  commented-out defaults survive.

Adding optional keys never bumps the version; renaming or removing keys, or changing
what an existing key means, does — the release notes of a bees version that bumps it
say what changed. The current version is `1`; the only migration so far (0 → 1) adds
the key.

Tightening validation does not bump it either, even though a file that loaded before
may now be refused. Such a file fails to load with an error naming the offending key
and what to change — for example the MCP server name `bees`, reserved for the built-in
server (see [MCP servers](#mcp-servers)). No migration is attempted, because the value
is the user's own and bees cannot guess the fix: a loud error is better than silently
rewriting or dropping the setting.

## `[project]`

| Key | Type | Default | Description |
|---|---|---|---|
| `remote` | string | `"origin"` | Git remote the factory fetches from and pushes to. |
| `repo` | string | derived | GitHub repository as `owner/name`. When unset it is parsed from the remote's URL (https, `ssh://` and `git@github.com:` forms). Set it only if the remote URL is not a github.com URL. |
| `default_branch` | string | derived | Branch developers branch from, reviewers diff against and QA tests. When unset it is read from the remote's HEAD (`refs/remotes/<remote>/HEAD`, or `git ls-remote --symref`). |
| `state_dir` | string | `".bees"` | Where mail, notes, session logs and scheduler state live. Relative paths resolve against the directory containing `bees.toml`. `bees init` adds it to the repository's `.gitignore` when it lives inside the clone (commit that). |
| `branch_prefix` | string | `"bees/"` | Prefix for developer branches, e.g. `bees/issue-12`. |

There is deliberately no product description and no build/test/run commands here.
What the product is, how to install dependencies, run the test-suite and launch the
application all belong in the repository's own documentation (README, CONTRIBUTING, CLAUDE.md, Makefile, CI
config). Every role is told to read those, and to record what it learns in its
notes file so later sessions start faster.

The clone that holds `bees.toml` is the "main" checkout: every session runs in a
temporary `git worktree` created from it, so it must have the configured `remote`
pointing at the GitHub repository.

## `[filter]`

The filter decides which issues and pull requests the factory is allowed to see and
touch. All configured criteria are ANDed.

| Key | Type | Default | Description |
|---|---|---|---|
| `label` | string | `"bees"` | The factory's label. It is always the base name of the workflow state labels (`bees:triage`, `bees:ready`, ...) and, when `require_label` is true, the visibility gate. Must not contain spaces or colons. |
| `require_label` | bool | `true` | When true, only items carrying `label` are visible. Set to false to let `assignee` and/or `milestone` alone define visibility. The factory still applies `label` to everything it creates or manages. |
| `assignee` | string | `""` | Only see items assigned to this GitHub login. `"@me"` resolves to the authenticated `gh` user at startup. Everything the factory creates is assigned to this user so it stays visible, pull requests included. |
| `milestone` | string | `""` | Only see items in this milestone (by title). Also the fallback milestone for issues the factory creates when neither `--parent` nor `--related` supplies one (see `bees issue create`), and the milestone put on the pull requests the factory opens so they stay visible. Milestones themselves are managed by people, never by bees. |

Setting `require_label = false` without `assignee` or `milestone` is rejected, because it
would make every open issue in the repository visible.

### Use case: "everything assigned to me" in a shared repository

One person can run busybees for their own share of a team project. Teammates assign
issues to them as usual; nothing needs a special label:

```toml
[filter]
label = "bees"          # still used for the bees:* state labels
require_label = false
assignee = "@me"
```

The factory picks up any issue assigned to you, adds `bees` and `bees:feedback` to
it on first sight — an issue with no state label and neither `bees:feature` nor
`bees:feedback` is read as feedback for the product manager, and you label it
`bees:triage` or `bees:ready` yourself to have it built — and assigns every PR or
bug it creates back to you. Because state labels are prefixed with `label`, several
people can each run their own factory in the same repository with different labels
(`kyle-bees`, `sam-bees`) without interfering.

### Workflow labels

Every label below is derived from `filter.label` (shown here for `bees`). `bees init`
and `bees labels sync` create them in GitHub.

| Label | Meaning |
|---|---|
| `bees` | Visible to the factory |
| `bees:feature` | Feature issue owned by the product manager, which makes it detailed enough and breaks it into work items (outside the state machine) |
| `bees:bug` | Bug work item (filed by the developer, reviewer, QA or a human) |
| `bees:feedback` | Feature idea, product feedback or bug report for the product manager (outside the state machine) |
| `bees:question` | The product manager is waiting for a person to answer on a feature or feedback issue; removed by the orchestrator when they reply |
| `bees:proposal` | A feature issue a bee wrote; it sits next to `bees:feature`, and a person removes the label to approve it |
| `bees:planning` | A person and the product manager are still agreeing a feature or feedback issue: the product manager only discusses it and breaks nothing down. Not a state label; only a person sets or removes it. See [Planning with the product manager](workflow.md#planning-with-the-product-manager) |
| `bees:planned` | A person ended planning: the scope is agreed, and the product manager breaks the issue down on its next run without re-opening it. Not a state label; only a person sets or removes it |
| `bees:priority` | A person wants this next: dispatched before the rest of the `bees:ready` queue. Not a state label; people set it, the project manager may add it to a work item that unblocks the factory itself, and the product manager carries one from a feedback issue onto the work item it creates from it |
| `bees:triage` | Needs refinement by the project manager |
| `bees:ready` | Detailed enough for a developer to pick up |
| `bees:in-progress` | A developer worker owns it |
| `bees:blocked` | Waiting on an answer to a question |
| `bees:review` | Pull request open and under review |
| `bees:approved` | Reviewer approved; waiting for a human to merge |
| `bees:needs-human` | The factory gave up, or a person is holding the issue; either way a person must step in |

An issue also carries at most one **size label**, independently of its state.
The project manager sets it when it moves a work item to `bees:ready`; the
orchestrator adds `bees:size/m` to any ready issue that has none. See
[Sizing](workflow.md#sizing).

| Label | Meaning |
|---|---|
| `bees:size/xs` | One file, obvious change, no design |
| `bees:size/s` | A few files, clear approach, existing tests cover it |
| `bees:size/m` | A coherent feature slice touching several packages, needs new tests |
| `bees:size/l` | Crosses subsystems or needs a design decision; near the limit for one PR |
| `bees:size/xl` | Too big for one pull request — split it instead of labelling it |

## `[github]`

By default the factory uses whatever account the machine's `gh` is logged in with, so
every issue, comment and label edit it makes looks like it came from the person
running it. `[github]` gives the orchestrator a login of its own.

| Key | Type | Default | Description |
|---|---|---|---|
| `login` | string | `""` | GitHub login the factory acts as. It is what `bees init` and `bees doctor` verify the token against and what `bees status` reports; it is not itself a credential. It also decides which comments the factory reads as its own, so it must be the login GitHub reports as the author of what the token writes — with a `[bot]` suffix exactly when GitHub uses one. |
| `token` | string | `""` | A token for `login`, passed as `GH_TOKEN` to every `gh` call the orchestrator makes and to every session it runs. A `"$VAR"` or `"${VAR}"` value is expanded from the environment bees runs in, so the secret itself stays out of `bees.toml`. A reference that expands to nothing is rejected at load time, naming the variable. |
| `git_name` | string | `""` | Name for commits made by developer sessions, given to them as `GIT_AUTHOR_NAME` and `GIT_COMMITTER_NAME`. |
| `git_email` | string | `""` | Email for those commits, as `GIT_AUTHOR_EMAIL` and `GIT_COMMITTER_EMAIL`. |

`login` and `token` are set together: either alone is rejected with an error naming
the key. A login on its own would make bees report an account it does not act as, and
a token on its own leaves nothing to report without asking GitHub who it belongs to.
`git_name` and `git_email` are an identity rather than a credential, so they are
accepted on their own, and either may be set without the other — whichever is unset
leaves that half of the commit identity to the machine's own git configuration.

With the table unset — the default, and what every `bees.toml` written before it looks
like — nothing is injected and the factory behaves exactly as it always has.

```toml
[github]
login = "busybees-bot"
token = "$BEES_GITHUB_TOKEN"
```

**What the token covers.** Everything the factory does on GitHub. That is every call
bees makes with its own code — polling for issues and pull requests, the label edits
it makes, the review requests and escalation comments it posts, `bees init`'s label
creation, `bees doctor`'s repository checks, and everything `bees issue`, `bees mail`
and the built-in MCP tools do — and, since the sessions get the token too, the `gh` a
Claude session runs itself: its `gh pr create`, its `gh api` calls and the pushes it
makes. Its commits are the bot's as well, when `git_name` and `git_email` are set.

The [comment marker](roles.md#common-ground) stays necessary all the same, because
`[github]` is optional: with the table unset every comment still arrives under your
own login. Where a comment does come from the bot, the orchestrator reads its author
as well as its marker — anything that login posted is the factory's, including the
escalation comment, which carries no marker.

**What sessions get.** A session runs with `GH_TOKEN` set to the token, with
`GIT_AUTHOR_NAME`/`GIT_AUTHOR_EMAIL` and `GIT_COMMITTER_NAME`/`GIT_COMMITTER_EMAIL`
from `git_name`/`git_email`, and with `credential.helper=!gh auth git-credential`
configured for git — reset first, so a credential helper of your own does not answer a
push before gh's does. Your stored credentials are neither read nor written. The
helper only steers **https** remotes: on an `ssh://` or `git@github.com:` remote the
commits are still the factory's, but the push authenticates with the machine's own ssh
key. When `token` is a `"$VAR"` reference the session is given that variable as well,
holding the same resolved token, because the `bees` commands a session runs load
`bees.toml` themselves and a reference that expands to nothing is a load error; it
goes into the session's environment and never into a file. A session started with
`[github]` unset gets none of these and behaves exactly as it always has. `bees` sets
git configuration through `GIT_CONFIG_COUNT` and friends, and leaves the whole block
alone when you have set `GIT_CONFIG_COUNT` yourself.

**What the token needs.** A fine-grained personal access token, scoped to the one
repository with **write** access to *Issues*, *Pull requests* and *Contents*, and
**read** access to *Metadata*. Issues and pull requests cover the labels, comments,
milestones and reviews; contents covers the pushes developer sessions make; metadata
is required by GitHub alongside the rest. A classic token works too, with the `repo`
scope. Either way the token has to belong to an account that has a login — a bot user
account or your own — because `login` is compared with the account GitHub says the
token authenticates as. `bees init` checks the token before the factory ever uses it:
that GitHub accepts it, that it belongs to `login`, and that it can read the
repository. `bees doctor` asks the same two identity questions of whatever token is
configured at the time, and two more that `bees init` does not: that the account can
actually **write** issues, and that it can actually **push branches**. Repository
permission does not imply either — a fine-grained token's per-resource permissions sit
on top of the repository role, so a token can read the repository as `ADMIN` and still
be refused every issue, comment and label the factory writes, or every branch a
developer session pushes.

### `filter.assignee = "@me"` still means you

`"@me"` says whose work the factory picks up, which is the person's, not the bot's. It
is resolved to a login with the machine owner's own `gh` authentication before any
token is used — in the orchestrator, in the MCP server and in `bees doctor` alike — so
setting `[github]` never changes which issues are visible. To have the factory pick up
the bot's issues instead, write the bot's login out in full:

```toml
[filter]
assignee = "busybees-bot"
```

## `[scheduler]`

| Key | Type | Default | Description |
|---|---|---|---|
| `poll_interval` | duration | `"5m"` | How often GitHub is polled for work. Each poll costs two API calls (`gh issue list`, `gh pr list`); everything else is gated on what those lists report (see [API budget](#api-budget)). Also the minimum gap between two runs of the same singleton role. Keep it infrequent. |
| `rate_limit_backoff` | duration | `"15m"` | How long to pause polling after a poll fails with a GitHub rate-limit error (a message containing "rate limit", "secondary rate" or "abuse detection"), instead of retrying after `poll_interval`. It is also how long the whole factory pauses when a session hits the account-wide claude session limit and no usable reset time came with it (see [The claude session limit](#the-claude-session-limit)). |
| `max_developers` | int | `1` | Number of concurrent developer workers. Each worker owns one issue and runs a sequential developer → reviewer → developer loop, so reviewer concurrency follows developer concurrency. Must be ≥ 1. |
| `max_review_rounds` | int | `3` | Developer/reviewer iterations before an issue is escalated with `bees:needs-human`. |
| `retries` | int | `1` | Extra attempts a session gets when it failed for **infrastructure** reasons — it timed out, ran out of turns, hit an API error or rate limit, or `claude` crashed. A session that ran and reported (with `bees done`, including `failed`) is never retried, and neither is one that hit [the claude session limit](#the-claude-session-limit) — every attempt would hit the same wall. `0` disables retrying; must be between 0 and 5. See [Escalation](workflow.md#escalation-beesneeds-human). |
| `retry_delay` | duration | `"10m"` | How long to wait before an attempt is repeated. `"0s"` retries immediately. |
| `retry_with_fallback` | bool | `true` | Run the retry with the role's `fallback_model` as its primary model. Roles without a fallback model simply rerun. |
| `triage_batch_size` | int | `5` | Maximum number of issues handed to the project manager in one session. |
| `notes_consolidate_every` | int | `10` | Sessions a role runs between two passes in which it is also asked to consolidate its [notes file](roles.md#notes-files) into the standard sections. `0` means the default; must be ≥ 0. |
| `notes_max_bytes` | int | `32768` | Ask for consolidation early, whatever the session count, once a notes file has grown past this many bytes. `0` means the default; must be ≥ 0. |
| `dispatch_order` | string | `"small-first"` | Which `bees:ready` issue a free developer takes next: `small-first` (smallest size first), `oldest` (whatever the size) or `large-first`. Ties are broken by age, oldest first; an issue without a size ranks as `m`. Issues already `bees:in-progress` or `bees:review`, an issue in `bees:approved` whose post-approval checks were interrupted, and `bees:ready` issues that already have an open pull request, are resumed first and are never reordered. Issues carrying `bees:priority` come before everything else in the ready queue, whatever this key says. See [Sizing](workflow.md#size-decides-what-gets-built-next). |
| `max_large_in_flight` | int | `1` | How many `bees:size/l` issues developer workers may hold at once. A larger issue over the cap is skipped and the free worker takes the next issue that fits. `0` means no cap; must be ≥ 0. |
| `pr_fix_conflicts` | bool | `true` | Hand an open pull request that **conflicts** with the default branch back to its developer: the developer is mailed (from `orchestrator`) to merge the default branch, resolve, test and push, and an approved issue goes back to `bees:ready` ahead of new work. See [Conflicts with the default branch](workflow.md#conflicts-with-the-default-branch). |
| `pr_keep_updated` | bool | `false` | Do the same when a pull request is merely **behind** the default branch (it would merge cleanly, but was not tested against what is on the default branch now). |
| `notify` | list of strings | `[]` | GitHub logins and/or `org/team` slugs the factory turns to when it needs a person. They are mentioned in the `bees:needs-human` escalation comment and in the product manager's `bees:question` comments, and asked to review a pull request the reviewer moved to `bees:approved`. Entries carry no leading `@` and hold at most one `/`. Empty (the default) mentions nobody and requests no reviewer. See [Notifying a person](#notifying-a-person). |
| `product_manager_interval` | duration | `"1h"` | Minimum time between product manager runs. Unread mail in the PM's inbox triggers an earlier run. |
| `qa_interval` | duration | `"30m"` | Minimum time between QA runs. QA only runs when something was merged since its last run (the first run always happens); mail in the QA inbox triggers an earlier run. The merged-PR query itself runs at most once per `qa_interval` (tracked as `last_check` in `<state_dir>/qa.json`), not on every poll. |
| `max_cost_per_issue` | float | `0` | USD one work item may cost across every session run for it — developer, reviewer, retries, check fixes. Checked between a developer worker's stages, never mid session, so the session that passes it finishes and its work stays on the branch; the issue is then escalated with what it spent. `0` (the default) is unlimited; must be ≥ 0. See [Cost budgets](#cost-budgets). |
| `max_cost_per_day` | float | `0` | USD the whole factory may spend over a rolling 24 hours. At or over it no new session is dispatched — no developer worker, no singleton — while the sessions already running finish normally. `0` (the default) is unlimited; must be ≥ 0. |
| `max_cost_per_session` | float | `0` | USD a single session may cost. `claude -p` cannot be stopped on cost while it runs, so this is checked once it has finished: an over-budget session is treated as failed, and two in a row for the same work item escalate it. `0` (the default) is unlimited; must be ≥ 0. |
| `keep_workspaces` | bool | `false` | Leave temporary worktrees on disk after a session (debugging). |
| `workspace_root` | string | `""` | Directory temporary worktrees are created under. Empty means `$TMPDIR/bees`. |
| `work_hours` | string | `""` | Daily window during which GitHub is polled every `poll_interval`, as `"HH:MM-HH:MM"` on a 24-hour clock. Empty (the default) disables the feature — GitHub is polled around the clock and the three keys below are ignored. See [Work hours](#work-hours). |
| `off_hours_poll_interval` | duration | `"1h"`, or `poll_interval` when that is longer | How often GitHub is polled outside `work_hours`. Must be ≥ `poll_interval`. Only used when `work_hours` is set. |
| `work_days` | list of strings | `["mon","tue","wed","thu","fri"]` | Days the window applies to, as lowercase three-letter names (`mon tue wed thu fri sat sun`). At least one is required when `work_hours` is set. |
| `timezone` | string | `""` | IANA name the window is read in (`"America/New_York"`). Empty means the machine's local time. |

Durations use Go syntax: `"30s"`, `"5m"`, `"1h30m"`.

### Notifying a person

By default the factory and the people it works for share one GitHub account,
so nothing the factory writes notifies anybody: the comment author *is* you,
and GitHub sends no mail for your own comments. (With [`[github]`](#github)
configured the orchestrator's own comments come from the bot instead, and a
mention in one does reach you — but the factory does not rely on that.)
`scheduler.notify` says who to reach:

```toml
[scheduler]
notify = ["kpenfound", "myorg/bees-team"]
```

Each entry is a GitHub login or an `org/team` slug, with no leading `@` and at
most one `/`. Where they are used:

- the `bees:needs-human` [escalation comment](workflow.md#escalation-beesneeds-human)
  starts with `@kpenfound @myorg/bees-team`;
- the product manager starts a `bees:question` comment with the same line;
- a pull request the reviewer moved to `bees:approved` is waiting for a person
  to merge it, so a review is requested from every entry.

The review request is best effort — a failure is logged and the pull request
still reaches `bees:approved`. **GitHub refuses to request a review from a pull
request's own author**, and with a shared account the configured login usually
*is* the author, so a login often gets the mention but not the review request.
Teams are always accepted, so list one if you want the request as well.

With `notify` unset (the default) nobody is mentioned and no reviewer is
requested.

### Work hours

Most polling exists to notice *human* activity: new issues, feedback, PR
reviews, merges, hand-backs from `bees:needs-human`. Outside working hours that
activity is rare, so `work_hours` lets the factory poll GitHub less often then:

```toml
[scheduler]
poll_interval = "5m"            # inside work hours
off_hours_poll_interval = "1h"  # outside work hours
work_hours = "09:00-18:00"
work_days = ["mon", "tue", "wed", "thu", "fri"]
timezone = "America/New_York"
```

Only the **GitHub polling cadence** changes. The scheduler keeps ticking every
`poll_interval`, and a finished session wakes it in between; a tick that is not
due for a poll runs a *local pass* that reuses the last poll's issue and PR
lists (see [The scheduler loop](architecture.md#the-scheduler-loop)).
Everything driven by
the local mailbox — the developer ↔ reviewer loop, answered questions moving
`bees:blocked` back to `bees:ready`, the checks stage — runs at full speed at
every hour of the day. `bees tick` and `bees exec` ignore the window and always
do a full pass.

`bees status` always prints a `work hours:` line, whether or not the window is
configured, with the cadence in force and when the next poll is due — that is the
quickest way to check the feature is on and doing what you meant. See
[`bees status`](cli.md#bees-status---json).

**The work day starts on time.** The poll before the window opens is scheduled
for the moment it opens, not a whole `off_hours_poll_interval` later, so the
first poll of the day is at `09:00` rather than up to an interval late.

**A rate limit never speeds polling up.** `rate_limit_backoff` is a floor on the
wait, not a replacement for it: after a rate-limited poll the next one is due
after whichever of the backoff and the interval in force is longer. Off hours
with `off_hours_poll_interval = "8h"` that is 8h, not 15m.

**Overnight windows.** When the start is later than the end, the window wraps
midnight and belongs to the day its **start** falls on: `work_hours =
"22:00-06:00"` with `work_days = ["fri"]` covers Friday 22:00 through Saturday
06:00, and nothing else. A window whose start equals its end is rejected.

Invalid values are rejected when `bees.toml` is loaded: a window that is not
`"HH:MM-HH:MM"` on a 24-hour clock, an unknown or empty `work_days`, a timezone
`time.LoadLocation` does not know, or an `off_hours_poll_interval` shorter than
`poll_interval`. `off_hours_poll_interval` is only defaulted when it is unset,
and then never below `poll_interval`, so a file that sets only a long
`poll_interval` cannot fail on a key it does not contain.

`bees status` shows the window, whether the factory is inside it right now, and
when the next GitHub poll is due.

### API budget

Several parts of busybees talk to the GitHub API through `gh`, and the sessions
themselves call `gh` freely on top of that, so the orchestrator is deliberately
frugal:

| What | Cost | When |
|---|---|---|
| A poll | 2 calls (`gh issue list`, `gh pr list`) | every `poll_interval`, or every `off_hours_poll_interval` outside `work_hours` |
| Human PR feedback | 3 calls per PR (reviews, review comments, comments) | only for PRs whose `updatedAt` moved since the last look |
| Human issue comments | 1 call per issue | only for issues in `bees:in-progress`, `bees:review`, `bees:approved` or `bees:blocked` whose `updatedAt` moved since the last look; none until the issue has a clock — a pass that sees an issue in `bees:triage` or `bees:ready` records the time and fetches nothing, and so does the first pass that sees an issue with no clock in one of those states |
| Product-manager has-work check | 1 `issue view` per feedback/feature issue | only for issues whose `updatedAt` is newer than the PM's last run; the check for a feature whose sub-issues have all closed adds none — it compares the sub-issue numbers recorded on the last PM run with the issues the poll found open |
| Product-manager run | 1 `issue view` per open feedback/feature issue, plus 1 REST call per open feature (sub-issue progress) and 1 GraphQL call per open work item (parent feature) | every PM run (not gated by `updatedAt`) |
| Planning mode | 1 extra `issue view` per `bees:planned` issue the freshness check did not already fetch the comments of | every PM run, while an issue is agreed and not yet acted on |
| QA merged-PR check | 1 call | at most once per `qa_interval` |
| Checks (auto-merge) | 1 call per poll of the checks stage, 2 when the branch requires no check | every `roles.reviewer.checks_poll_interval` while waiting |
| Visibility backstop | 2 list calls | after every session |
| Feature progress | 1 REST call per open feature issue (`sub_issues_summary`) | per product manager run |
| Parent feature lookup | 1 GraphQL call per triage item, 1 per open work item, 1 per developer session, and 1 per review round with a `product-fit` stage configured (none by default) | per project manager run / product manager run / developer session / reviewer session |
| `bees issue create --parent` | 3 calls (parent details, create, attach as sub-issue); `--related` 2; plain 1 | whenever a role files an issue |
| Worker stage transitions | a handful of `issue view` / `pr view` / `issue edit` calls | per transition |

An idle factory therefore costs two calls per poll. If GitHub does rate-limit
the process, polling pauses for `rate_limit_backoff` before trying again.

What the factory spends on the Anthropic API is a separate budget, capped by
the three `max_cost_*` keys below.

### Cost budgets

Nothing limits what a factory spends by default: all three budgets are `0`,
which means unlimited. They are spent against the
[session ledger](cli.md#bees-cost---since-24h---by-roleissueday---json) — the
same numbers `bees cost` reports — so a retried session counts like any other,
and setting one is enough to make it bite:

```toml
[scheduler]
max_cost_per_issue = 25.00   # every session run for one work item
max_cost_per_day = 100.00    # whole factory, rolling 24 hours
max_cost_per_session = 10.00 # a single session
```

Each is enforced at the only moment the factory can act on it. A running
session is never interrupted on cost:

- **Per issue** — checked between the stages of a developer worker. The
  session that took the issue over its budget finishes and its work stays on
  the branch; the worker then stops and the issue is escalated with what it
  spent (*"Issue #12 has cost $26.40 across 7 sessions, over the
  `max_cost_per_issue` budget of $25.00"*).
- **Per day** — summed over the last 24 hours before anything is dispatched.
  At or over the budget the scheduler keeps polling and reconciling labels but
  starts no new session; workers already running finish their loop. The pause
  is logged once, and `bees status` says so on its scheduler line.
- **Per session** — checked after the session ended. An over-budget session is
  treated as failed whatever it reported, which means it is retried once (with
  the role's `fallback_model` when `retry_with_fallback` is on, usually the
  cheaper one). Two over-budget sessions in a row for the same work item
  escalate it: that is a signal that the role's `max_turns` or `timeout` are
  the wrong shape for this work, not that one session went astray.

Budgets are about money, not about turns: `max_turns` already caps how long a
single session may go on for.

### The claude session limit

The Anthropic account has limits of its own, and every role shares the
account: when one session runs out of capacity, so has the whole factory.
`claude` reports it in two ways and either is enough — a `rate_limit_event`
in the session's stream whose status is neither `allowed` nor
`allowed_warning`, or, from a session that failed without reporting an
outcome, a result text naming a session or usage limit ("You've hit your
session limit · resets 11:50pm (America/Detroit)"). The result text is only
read this way when the session reported nothing: it is the session's own
prose, and a bee whose work is the limit itself writes those words.

A session that ends that way is **not** retried: every attempt would hit the
same wall. Its issue is not escalated either — the limit says nothing about
the work — so it keeps its state label and is picked up again afterwards.
The scheduler pauses **all** dispatch, developers and singletons alike,
until the limit resets. Sessions already running finish on their own, as
with the daily cost budget; polling and label reconciliation carry on, so
the pause costs nothing and `bees status` stays honest.

How long it pauses comes from the reset time the event carried, with two
sanity limits: one that is missing or already in the past falls back to
`rate_limit_backoff` (default 15m), and one more than 8 hours ahead is
clamped to 8 hours, so a wrong clock or a weekly window cannot park the
factory for days. The pause is logged when it starts and again when it
lifts, and `bees status` names the time it lifts. It is held in memory only:
restarting `bees run` clears it and the first session re-learns the limit.

## `[logging]`

How `bees` logs to the console. It is a top-level table, not a role setting:
logging is a property of the `bees` process, so it applies to every command.

```toml
[logging]
format = "text"   # text | json
level = "info"    # debug | info | warn | error
```

| Key | Type | Default | Description |
|---|---|---|---|
| `format` | string | `text` | Console log format: `text` or `json`. |
| `level` | string | `info` | Console log level: `debug`, `info`, `warn` or `error`. |

The table exists for running `bees run` as a long-lived service, where the
natural place to say "always log JSON at info" is the project, not a systemd
unit. It is the lowest-priority source: **a flag beats an environment variable,
which beats `bees.toml`, which beats the built-in default.** So
`bees run --log-format text` still gives you a readable terminal in a project
whose file says `json`, and `-v` still wins over `level = "info"`.

Commands that never read `bees.toml` — `bees version`, `bees done`, and any
command run against a file that fails to load — log with the flag, environment
and default settings only.

There is no `quiet` key: `--quiet` is a shorthand for one invocation, not a way
to run the factory. `level = "warn"` is the service-shaped equivalent (it also
drops the one-line session summaries, which `--quiet` keeps).

`bees run` in a terminal draws the live view instead of logging to the
console, and silences console logging while it is up; `--no-tui`, a
redirected stdout, and every other command log as this table says.

The `bees.log` file in the state directory is not configurable here: it always
gets every record at debug level, in JSON. See
[`bees run`](cli.md#bees-run).

## `[global]` and `[roles.<name>]`

`[global]` and each `[roles.<name>]` table accept the same keys. The role name must be
one of `product_manager`, `project_manager`, `developer`, `reviewer`, `qa` (the CLI
accepts aliases such as `pm`, `pjm`, `dev`, but the TOML keys must be the full names).

| Key | Type | Default | Description |
|---|---|---|---|
| `prompt` | string | `""` | Text appended to the role's built-in base prompt. |
| `prompt_file` | string | `""` | Path (relative to `bees.toml`) whose contents are appended after `prompt`. Must exist. |
| `skills` | string list | `[]` | Skills by git URL (see below). |
| `skills_refresh` | string | `"24h"` | **Global only.** How stale a skill clone may get before it is pulled when a session needs it: `never`, `always` or a duration. See [Skills](#skills). |
| `mcp.<name>` | table | — | MCP servers keyed by name (see below). |
| `model` | string | `"opus"` | Claude model alias or full id passed as `claude --model`. |
| `fallback_model` | string | `"sonnet"` | Passed as `claude --fallback-model`. Claude Code switches to it automatically when `model` has reached the account's usage limit. Omitted when equal to `model`. |
| `effort` | string | `""` | Passed as `claude --effort`. One of `low`, `medium`, `high`, `max`. |
| `max_turns` | int | `200` | Agentic turns per session (`claude --max-turns`). |
| `timeout` | duration | `"45m"` | Wall-clock limit for one session. The claude process group is killed when it expires. |
| `allowed_tools` | string list | `[]` | Passed as `claude --allowedTools`. |
| `disallowed_tools` | string list | `[]` | Passed as `claude --disallowedTools`. |
| `shell` | string | the shell bees runs under | Exported into sessions as `$SHELL`. Claude Code has no setting to force its Bash tool's shell; it uses the system default, which it discovers from `$SHELL`, so this is the lever available — but not a hard guarantee. Must be an existing executable. |
| `env` | table | `{}` | Environment variables exported into every session: inherited by `claude`, its Bash tool, MCP servers and git. `$VAR` references are expanded from the bees process environment when the session starts. `[roles.<name>.env]` entries are merged over `[global.env]` (role wins per key). bees' own `BEES_*` variables always win. `BEES_*` variables are always set by bees for each session and are never inherited from the process that started it, so a session started from inside another one (a nested `bees run` or `bees exec`) never sees a stale issue, PR or branch; the only `BEES_*` name bees puts back after that strip is the one a `"$VAR"` [`github.token`](#github) reads, and it carries the value bees itself resolved. |
| `enabled` | bool | `true` | **Roles only.** `false` takes the role out of the rotation. Disabling `reviewer` makes developer PRs count as approved as soon as they are opened (and, with `auto_merge`, go straight to the checks stage). |

### `[roles.reviewer]` only: checks and auto-merge

The reviewer owns the checks and merging. These keys are accepted **only** under
`[roles.reviewer]`; setting them on `[global]` or another role is a validation error.

| Key | Type | Default | Description |
|---|---|---|---|
| `auto_merge` | bool | `false` | Merge a pull request the reviewer approved once its checks are green — the required checks if the branch has any, otherwise every check the pull request reports; with no checks at all it merges and says so. Off by default: humans merge. |
| `merge_method` | string | `"squash"` | `squash`, `merge` or `rebase` (`gh pr merge --<method> --delete-branch`). |
| `checks_wait` | duration | `"1m"` | How long to wait after approval before polling the checks, because some take a moment to report they have started. |
| `checks_poll_interval` | duration | `"2m"` | How often the checks are polled while waiting (one API call each, two when the branch requires nothing). |
| `checks_timeout` | duration | `"30m"` | How long to wait for the checks to finish before escalating with `bees:needs-human`. |
| `max_check_fix_rounds` | int | `2` | Reviewer-diagnoses / developer-fixes iterations allowed when checks fail, before escalating. |
| `pre_review_checks` | bool | `true` | Read the pull request's checks **before** the first review, so the reviewer starts from a green pull request (or is told it is not). Independent of `auto_merge`. |
| `pre_review_checks_timeout` | duration | `"10m"` | How long that pre-review read waits for pending checks before reviewing anyway. |

With `pre_review_checks` (on by default), a developer worker reads the pull
request's checks between opening it and the first review, once —
`checks_wait`, then a poll every `checks_poll_interval`, under the same gate
rules as after approval, but bounded by `pre_review_checks_timeout`. Green → the
review starts and the reviewer's prompt lists the checks. A failure → the
checks-mode reviewer and a developer fix round first, sharing `check_fix_rounds`
and `max_check_fix_rounds` with the post-approval stage; the review happens only
once the pull request is green. Still pending at the timeout, no check reported
at all, or a read that fails outright → the review happens anyway and the
reviewer is told nothing was verified for it, and to say so in its outcome
note. Later review rounds go straight to the reviewer: no second read, and no
checks section describing a head the developer has replaced. `bees status`
shows the worker in the `pre-review checks` stage while it waits.
`pre_review_checks = false` goes straight from the developer to the reviewer.

With `auto_merge = true`, after approval the worker waits `checks_wait`, then polls
the pull request's checks every `checks_poll_interval`. All green → merge. Any check
fails → the reviewer gets a checks-mode session to find the main error and mail it to
the developer, the developer pushes a fix, and the checks are polled again — up to
`max_check_fix_rounds`. Still pending at `checks_timeout`, or a merge that GitHub
refuses (for example branch protection that needs a human review) →
`bees:needs-human`. See [workflow.md](workflow.md#merging).

**Which checks are the gate.** `gh pr checks --required` decides whenever the default
branch requires anything: those checks, and only those. A repository with no branch
protection requires nothing, and gating on nothing would merge with nothing green — so
in that case every check the pull request reports (`gh pr checks`) is the gate instead,
a failing one blocks the merge and a pending one is waited for. To take a check out of
the gate, mark the ones that must block a merge as required in the branch protection
rules of the default branch; bees never reads those rules to change them, and never
enables or edits protection. With no check reported at all — a repository with no CI —
it merges after two consecutive empty polls and logs that no check was reported, not
that the checks passed. `bees doctor` says which of the three is in force, and
`bees status` shows it in the worker stage (`checks (required)`, `checks (reported)`,
`checks (none)`).

### `[roles.reviewer]` only: the review stages

The reviewer reviews in ordered stages, each with its own focus, its own source
of truth and its own verdict. `stages` is accepted **only** under
`[roles.reviewer]`; setting it on `[global]` or another role is a validation
error, like the checks keys above.

| Key | Type | Default | Description |
|---|---|---|---|
| `stages` | string list | `["implementation", "completeness", "cleanliness", "style"]` | The review stages to run, in order. One or more of `implementation`, `completeness`, `cleanliness`, `style`, `product-fit`. |

| Stage | Question it answers | Source of truth |
|---|---|---|
| `style` | Does it follow the repository's formatting and lint conventions? | the repository's conventions, CLAUDE.md, the linter |
| `cleanliness` | Is it clear, small, free of dead code and needless abstraction? | the diff |
| `implementation` | Is it correct? Error handling, edge cases, tests, security. | the diff |
| `completeness` | Does it deliver the work item's acceptance criteria? | the issue |
| `product-fit` | Does it fit the parent feature and the product direction? | the parent feature, the README and the docs |

Every configured stage runs — the reviewer is told not to stop at the first one
that finds something to block on — and each ends with a verdict line of its
own. Requesting changes still sends one message to the developer, its points
grouped by stage in the stages' order. An approval means every configured stage
passed.

**`product-fit` is off by default.** A work item the project manager already
scoped is not the place to re-open the product decision, and leaving it off is
what keeps the default review's scope the same as the single-pass reviewer it
replaced. It is also the only stage that reads the work item's parent feature,
so the orchestrator makes that lookup — one GraphQL call per review round —
only when the stage is configured.

Both mistakes are load errors that name the key, the bad value and the valid
set: a stage that is not one of the five, and an empty `stages = []`, which
means a misconfiguration rather than "review nothing". See
[roles.md](roles.md#review-stages-rolesreviewerstages) for why the default is
what it is.

### `[roles.developer]` only: commit flags, max size and per-size models

These three keys describe the developer specifically, so they are accepted **only**
under `[roles.developer]`; setting one on `[global]` or another role is a
validation error.

| Key | Type | Default | Description |
|---|---|---|---|
| `commit_flags` | string | `""` | Extra flags for every `git commit` the developer makes, for example `"--gpg-sign --signoff"`. Appended verbatim to the developer's system prompt as "When creating git commits, always use the following extra flags: `--gpg-sign --signoff`." |
| `max_size` | string | `"l"` | The largest work item a developer takes: `xs`, `s`, `m`, `l` or `xl`. A `bees:ready` issue sized above it is never dispatched — the orchestrator moves it back to `bees:triage` and the project manager splits it. The project manager is told the limit in its prompt. See [Sizing](workflow.md#size-decides-what-gets-built-next). |
| `model_by_size` | table | `{}` | The model to run a developer session with, per work item size. Keys are the sizes `xs`, `s`, `m`, `l`, `xl`; an unknown key or an empty value is a validation error. A size with no entry — and an issue with no size label — uses `model`. |

```toml
[roles.developer]
commit_flags = "--gpg-sign --signoff"
max_size = "m"          # anything bigger goes back to triage to be split

[roles.developer.model_by_size]
xs = "sonnet"           # a typo fix does not need the strongest model
s = "sonnet"
```

`model_by_size` is read once per session, from the size label the issue carries when
the developer picks it up: `bees:size/xs` above runs that session as `--model sonnet`,
everything else as the developer's `model`. `fallback_model` is unchanged, and a retry
that runs with it (`scheduler.retry_with_fallback`) still overrides the size's choice.
Only the developer has the key; the reviewer, which is told the size too, always runs
its own `model`.

Signing (`--gpg-sign` / `-S`) happens inside a headless Claude Code session on the
machine running `bees`, so a working signing key and agent (gpg-agent, or an SSH
signing key with `gpg.format = ssh`) must be available to that user without a prompt.
`--signoff` needs `user.name` / `user.email` configured, as any commit does.

### How global and role settings merge

For each role the effective settings are computed from `[global]` and
`[roles.<name>]`:

| Setting | Rule |
|---|---|
| `prompt` / `prompt_file` | Concatenated in this order, separated by blank lines: global `prompt`, global `prompt_file`, role `prompt`, role `prompt_file`. The result is appended to the role's built-in base prompt under an "Additional instructions from bees.toml" heading, and the repository's own [project prompt files](#project-prompt-files) after that. |
| `skills` | Union, order preserved, global first, duplicates dropped. |
| `commit_flags`, `max_size`, `model_by_size` | Developer only; not merged from `[global]`. |
| `env` | union; the role wins on a name conflict |
| `mcp` | Union by name. A role server with the same name as a global one replaces it. |
| `model`, `fallback_model`, `effort`, `max_turns`, `timeout` | Role value if set, else global value, else the built-in default. |
| `allowed_tools`, `disallowed_tools` | Global list followed by role list. |
| `enabled` | Role only. |
| `auto_merge`, `merge_method`, `checks_wait`, `checks_poll_interval`, `checks_timeout`, `max_check_fix_rounds`, `pre_review_checks`, `pre_review_checks_timeout` | `roles.reviewer` only; they form the checks and merge policy `bees config show reviewer` prints. |
| `stages` | `roles.reviewer` only; not merged from `[global]`. `bees config show reviewer` prints the resolved list. |

`bees config show <role>` prints the result.

Only the *contents* of `prompt_file` are re-read for every session. The rest of this
table — `prompt` included — comes from the `bees.toml` that was loaded when `bees
run` started, so an edit to it reaches no session until the scheduler is restarted.
The **base** role prompts need more than a restart: they are compiled into the
`bees` binary, so a change to `internal/prompts/*/*.md` reaches no session until
`bees` is rebuilt and `bees run` restarted. `bees status` names the build the
running scheduler was started from, so it can be told from the one the repository
has, and [`bees doctor`](cli.md#bees-doctor) warns outright when that build is
behind the commit the repository has checked out.

### Project prompt files

A project can keep its role instructions in the repository instead of in `bees.toml`,
so they are versioned, reviewed in a pull request like code, and a branch can carry
experimental instructions the reviewer sees in the diff.

If the repository contains `bees/prompts/common.md`, its text is appended to every
role's system prompt; `bees/prompts/<role>.md` is appended to that role's. Nothing has
to be configured — the files are the whole convention, and a repository with no
`bees/prompts/` directory (every repository that has never used the feature) renders
exactly the prompt it rendered before.

```
bees/prompts/common.md            every role
bees/prompts/developer.md         the developer
bees/prompts/product_manager.md   the product manager
```

The four sources are appended to a role's built-in base prompt in this order, each
under a heading naming where it came from:

1. `[global]` `prompt` / `prompt_file`
2. `[roles.<name>]` `prompt` / `prompt_file`
3. `bees/prompts/common.md`
4. `bees/prompts/<role>.md`

`bees.toml` comes first so a machine-specific override still wins over what the
repository says.

The directory is `bees/`, without a dot, on purpose: `bees init` adds `/.bees/` to
`.gitignore`, so files under `.bees/prompts/` would be untracked by default — the
opposite of instructions reviewed like code — whatever `state_dir` points at.

Sessions read the files from **their own worktree**, so the instructions that reach a
session are the ones on the branch it is working on, not the ones on the default branch.
They are read at session start, which is also what makes them the exception to the
rebuild above: an edit to `bees/prompts/*.md` takes effect on the next session without
rebuilding or restarting anything. `bees prompts show <role> --rendered` has no worktree
and reads the checkout `bees.toml` sits in; it says so when it finds any.

A file bees cannot use — unreadable, or larger than 64 KiB — never takes a session
down: the session warns, skips that file and runs with the rest. `bees doctor` is
where those fail loudly, together with a file no role will ever read (a misspelled
role name such as `bees/prompts/develloper.md`).

> These files are instructions to an agent, taken from the repository and applied to
> the branch a session is building. Anyone who can land a commit on a branch can
> change what the sessions working on that branch are told to do. That is the point of
> the feature rather than a defect in it, but it is the same trust boundary as a CI
> configuration in the repository: review changes to `bees/prompts/` as you would
> review changes to a workflow file.

### Skills

Skills are referenced by git URL and cloned into a cache directory
(`~/.cache/bees/repos`, override with `BEES_CACHE_DIR`). They are exposed to Claude
Code as plugin directories (`claude --plugin-dir`), so the project worktree is never
modified.

URL syntax: `<git-url>[@<ref>][#<sub/dir>]`

```toml
skills = [
  "https://github.com/acme/skills",                 # whole repo
  "https://github.com/acme/skills#skills/tdd",      # one directory inside it
  "https://github.com/acme/my-plugin@v1.2.0",       # pinned tag or branch
  "git@github.com:acme/private-skills.git",         # ssh works too
]
```

Three repository layouts are recognised, checked in this order on the selected
directory:

1. **Claude Code plugin** — has `.claude-plugin/plugin.json`. Used as-is.
2. **Single skill** — has `SKILL.md` at the root (or in the `#sub/dir`). Wrapped in a
   generated plugin exposing that one skill.
3. **Skills collection** — has a `skills/` directory. Wrapped in a generated plugin
   exposing every skill in it.

Anything else is an error. Clones and generated wrappers are reused: a wrapper is only
rebuilt when it is missing or points somewhere else (the reference gained, lost or
changed its `#sub/dir`). Sessions run with `--plugin-dir` pointing at the wrapper, so
rebuilding one that a concurrent session is using would break it.

A clone is refreshed (`git pull --ff-only`) when a session needs it and it was last
fetched more than `skills_refresh` ago — `24h` by default. `skills_refresh = "always"`
pulls before every session, `"never"` never pulls. The fetch time is the mtime of a
`<clone>.fetched` file next to the clone in the cache. A failed pull is logged as a
warning and never stops a session: a reference pinned with `@tag` is a detached
checkout and cannot be pulled, which is the point of pinning it.

`bees skills list` shows what the cache holds and `bees skills update` refreshes it now,
whatever the policy says (see [cli.md](cli.md#bees-skills-list)).

### MCP servers

Servers are written to a per-session `--mcp-config` file and loaded with
`--strict-mcp-config`, so a session sees exactly the servers configured here plus the
built-in one, and none of the user's own.

**`bees` is reserved.** Every session automatically gets a server called `bees`
(`bees mcp serve`) carrying the factory's own tools — see
[cli.md](cli.md#bees-mcp-serve-sessions). It needs no configuration and cannot be
turned off; defining `[global.mcp.bees]` or `[roles.<role>.mcp.bees]` fails validation
with *mcp server name "bees" is reserved for the built-in server*.

| Key | Description |
|---|---|
| `type` | `stdio` (default when `command` is set), `http` (default when only `url` is set) or `sse`. |
| `command` | Executable for stdio servers. |
| `args` | Arguments for `command`. |
| `env` | Environment variables for the server process. `$VAR` and `${VAR}` are expanded from the `bees` process environment. |
| `url` | Endpoint for `http`/`sse` servers. |
| `headers` | HTTP headers for remote servers. `$VAR` expansion applies. |

Either `command` or `url` is required.

```toml
[global.mcp.github]
command = "npx"
args = ["-y", "@modelcontextprotocol/server-github"]
env = { GITHUB_PERSONAL_ACCESS_TOKEN = "$GITHUB_TOKEN" }

[roles.qa.mcp.browser]
type = "http"
url = "https://mcp.example.com/browser"
headers = { Authorization = "Bearer $BROWSER_MCP_TOKEN" }
```

## Defaults at a glance

| Setting | Default |
|---|---|
| `project.remote` | `origin` |
| `project.repo` | derived from the remote URL |
| `project.default_branch` | derived from the remote HEAD |
| `project.state_dir` | `.bees` |
| `project.branch_prefix` | `bees/` |
| `filter.label` | `bees` |
| `filter.require_label` | `true` |
| `github.login` | `""` (the machine's own gh account) |
| `github.token` | `""` |
| `scheduler.poll_interval` | `5m` |
| `scheduler.rate_limit_backoff` | `15m` |
| `scheduler.max_developers` | `1` |
| `scheduler.max_review_rounds` | `3` |
| `scheduler.retries` | `1` |
| `scheduler.retry_delay` | `10m` |
| `scheduler.retry_with_fallback` | `true` |
| `scheduler.triage_batch_size` | `5` |
| `scheduler.notes_consolidate_every` | `10` |
| `scheduler.notes_max_bytes` | `32768` |
| `scheduler.dispatch_order` | `small-first` |
| `scheduler.max_large_in_flight` | `1` |
| `scheduler.pr_fix_conflicts` | `true` |
| `scheduler.pr_keep_updated` | `false` |
| `scheduler.notify` | `[]` (nobody is mentioned) |
| `scheduler.product_manager_interval` | `1h` |
| `scheduler.qa_interval` | `30m` |
| `scheduler.max_cost_per_issue` | `0` (unlimited) |
| `scheduler.max_cost_per_day` | `0` (unlimited) |
| `scheduler.max_cost_per_session` | `0` (unlimited) |
| `scheduler.work_hours` | `""` (poll around the clock) |
| `scheduler.off_hours_poll_interval` | `1h`, or `poll_interval` when that is longer |
| `scheduler.work_days` | `["mon","tue","wed","thu","fri"]` |
| `scheduler.timezone` | `""` (the machine's local time) |
| `logging.format` | `text` |
| `logging.level` | `info` |
| `model` | `opus` |
| `fallback_model` | `sonnet` |
| `max_turns` | `200` |
| `timeout` | `45m` |
| `roles.reviewer.auto_merge` | `false` |
| `roles.reviewer.merge_method` | `squash` |
| `roles.developer.commit_flags` | `""` (none) |
| `roles.developer.max_size` | `l` |
| `roles.developer.model_by_size` | `{}` (every size uses `model`) |
| `skills_refresh` | `24h` |
| `roles.reviewer.checks_wait` | `1m` |
| `roles.reviewer.checks_poll_interval` | `2m` |
| `roles.reviewer.checks_timeout` | `30m` |
| `roles.reviewer.max_check_fix_rounds` | `2` |
| `roles.reviewer.pre_review_checks` | `true` |
| `roles.reviewer.pre_review_checks_timeout` | `10m` |
| `roles.reviewer.stages` | `["implementation", "completeness", "cleanliness", "style"]` |

## Examples

### Solo project, two developers

```toml
# repo and default_branch are derived from the origin remote.

[filter]
label = "bees"

[scheduler]
poll_interval = "5m"
max_developers = 2
max_review_rounds = 3
qa_interval = "1h"

[global]
prompt = """
Use conventional commits. Never add dependencies without a comment explaining why.
"""
model = "opus"
fallback_model = "sonnet"
max_turns = 200
timeout = "45m"

[roles.developer]
commit_flags = "--signoff"

[roles.reviewer]
model = "sonnet"
prompt = "Be strict about error handling and test coverage."
# Merge approved PRs once CI is green; hand check failures back to the developer.
auto_merge = true
merge_method = "squash"
checks_wait = "1m"
checks_poll_interval = "2m"
checks_timeout = "20m"
max_check_fix_rounds = 2
pre_review_checks_timeout = "10m"

[roles.qa]
skills = ["https://github.com/anthropics/skills#skills/webapp-testing"]
timeout = "30m"
```

### Team repository, only work assigned to me

```toml
[project]
remote = "upstream"        # my clone's origin is a fork; the team repo is "upstream"

[filter]
label = "kyle-bees"       # bees:* labels are namespaced to me
require_label = false
assignee = "@me"

[scheduler]
max_developers = 1
product_manager_interval = "4h"

[global]
prompt_file = "docs/engineering-conventions.md"
model = "opus"
fallback_model = "sonnet"

[roles.product_manager]
enabled = false            # the team's real PM owns the roadmap

[roles.developer]
prompt = "Only touch files under services/billing unless the issue says otherwise."
```

## Requirements

`bees run`, `tick`, `exec`, `status` and `init` check the tools they drive before doing
anything and refuse to start when one is missing or too old:

| Tool | Minimum | Why |
|---|---|---|
| [`gh`](https://cli.github.com/) | 2.50.0 | `gh pr checks --json` (2.50.0) and `gh api --slurp` (2.49.0). |
| Claude Code (`claude`) | 2.1.76 | `claude --name` (2.1.76); `--append-system-prompt-file`, `--effort`, `--plugin-dir`, `--strict-mcp-config` and `--fallback-model` are older. |

The minimums live in `internal/versions`. Set `BEES_SKIP_VERSION_CHECK=1` to run
with an unsupported version anyway.

## Environment variables

### Honoured by the `bees` command

| Variable | Effect |
|---|---|
| `BEES_CONFIG` | Path to `bees.toml` when `--config` is not given. Set automatically inside sessions. |
| `BEES_CLAUDE_BIN` | Path of the `claude` executable to run. Default `claude` on `PATH`. |
| `BEES_CACHE_DIR` | Cache directory for skill clones and generated plugins. Default `~/.cache/bees`. |
| `BEES_SKIP_VERSION_CHECK` | When non-empty, skip the `gh` / `claude` version checks (see [Requirements](#requirements)). |
| `BEES_STATE_DIR` | When set, `bees mail` uses this state directory directly instead of loading `bees.toml`, unless `--config` is passed explicitly (then that config's state dir wins). Set automatically inside sessions. |
| `BEES_SESSION_DIR` | Where `bees done` writes `outcome.json`; `bees done` refuses to run without it. Set automatically inside sessions. |
| `BEES_ROLE` | Default `--from` for `bees mail send`, and the role whose `bees done` statuses are validated. Set automatically inside sessions. |
| `BEES_ISSUE`, `BEES_PR` | Defaults for the `--issue` / `--pr` flags of `bees mail send` and `bees done`. Set automatically inside sessions. |
| `BEES_LOG_FORMAT`, `BEES_LOG_LEVEL` | Fallbacks for `--log-format` / `--log-level`. They sit between the flags and [`[logging]`](#logging): a flag beats them, and they beat `bees.toml`. |

The variables marked *set automatically inside sessions* are the only ones that
reach a session. The rest — `BEES_CLAUDE_BIN`, `BEES_CACHE_DIR`,
`BEES_SKIP_VERSION_CHECK` and the `BEES_LOG_FORMAT` / `BEES_LOG_LEVEL` fallbacks
of `--log-format` / `--log-level` — configure the `bees` process you start and
are **not** inherited by the sessions it spawns (see [Exported into every
session](#exported-into-every-session)), so a `bees` command a session runs
itself sees their defaults. Every `BEES_*` variable a session sees is one bees set
for it, with one exception: when [`github.token`](#github) is a `"$VAR"`
reference, bees sets that variable again after the strip, so the `bees` commands a
session runs itself can still resolve it. To give sessions one of these knobs, put
it in [`[global.env]`](#global-and-rolesname) instead.

### Exported into every session

Sessions run with the `bees` process environment, minus every `BEES_*` variable
it inherited, plus:

| Variable | Value |
|---|---|
| `BEES_ROLE` | The role name (`developer`, `reviewer`, ...). Default sender for `bees mail send`. |
| `BEES_SESSION_DIR` | This session's directory (prompts, transcript, `outcome.json`). Required by `bees done`. |
| `BEES_STATE_DIR` | The state directory (mail, notes, logs). |
| `BEES_CONFIG` | Path to `bees.toml`. |
| `BEES_REPO` | `owner/name`. |
| `BEES_LABEL` | `filter.label`. |
| `BEES_ISSUE` | Issue number the session is working on, when any. Default for `--issue` flags. |
| `BEES_PR` | Pull request number, when any. Default for `--pr` flags. |
| `BEES_BRANCH` | Checked-out branch, when any. |
| `BEES_NOTES_FILE` | The role's notes file. |
| `BEES_BIN` | Path of the `bees` executable. Its directory is also prepended to `PATH` so sessions can run `bees mail` and `bees done`. |
| `BEES_REVIEW_MODE` | `checks` for the reviewer's checks-mode sessions (diagnosing failed checks); unset otherwise. |
| `SHELL` | The configured `shell`, when set. |
| *configured `env`* | Every `[global.env]` / `[roles.<name>.env]` entry, `$VAR`-expanded from the bees environment. Set before the `BEES_*` variables, so those always win. |
| `GH_TOKEN` | [`github.token`](#github), when one is configured, so the session's own `gh` acts as the factory. Set with the `BEES_*` variables, after the configured `env`, so a role cannot give itself another identity. |
| *the variable `github.token` names* | When [`github.token`](#github) is a `"$VAR"` reference, that variable, holding the token bees resolved. Sessions load `bees.toml` themselves — the built-in MCP server behind every `bees` tool does it on each call — and a reference that expands to nothing is a load error, so the name has to survive the `BEES_*` strip. It is set in the session's environment only: the MCP server inherits it, and nothing writes it into the session directory. |
| `GIT_AUTHOR_NAME`, `GIT_COMMITTER_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_EMAIL` | [`github.git_name`](#github) and `github.git_email`, when set, so a session's commits are the factory's. |
| `GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_n`/`VALUE_n` | `push.autoSetupRemote=true` and `push.default=current`, so a plain `git push` works on a fresh branch without touching the clone's git config; plus an empty `credential.helper` and `credential.helper=!gh auth git-credential` when `github.token` is set, so an https push authenticates as the factory rather than through your stored credentials. `GIT_CONFIG_COUNT` is derived from the entries. Only set when `GIT_CONFIG_COUNT` is not already in the bees environment. |
