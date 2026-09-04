# Configuring busybees: `bees.toml`

One file configures a factory: `bees.toml`, in the root of a git clone of the
project being built. `bees init` writes it with every option listed and the
optional ones commented out at their default, so configuring is uncommenting
and editing lines. `bees config validate` checks the file and `bees config
show [role]` prints the settings each role ends up with.

The file starts with a `version` key, followed by these tables:

| Table | Purpose |
|---|---|
| `[project]` | The git remote, repository, default branch and state directory |
| `[filter]` | Which GitHub issues and pull requests the factory can see |
| `[github]` | The GitHub account the factory acts as |
| `[scheduler]` | Concurrency, polling, retries, budgets and the review loop |
| `[logging]` | Console log format and level |
| `[global]` | Prompt, skills, MCP servers, model and environment for every role |
| `[roles.<name>]` | The same keys per role, plus a few that only one role takes |

An unknown key anywhere in the file is a load error, so a typo cannot pass as
a default. Every validation error names the key and what to change.

Durations are written the way Go reads them: `"30s"`, `"5m"`, `"1h30m"`.

## `version`

```toml
version = 1
```

The format version of the file, not of bees. `bees init` writes the current
one, `1`. A file without the key is version 0.

- A file newer than the running bees understands is refused with `upgrade
  bees`.
- An older file is migrated on load, one version at a time. `bees run`,
  `tick`, `exec`, `status`, `issue create` and `issue link` then write the
  migrated file back, keeping the original as `bees.toml.v<old>.bak` and
  logging that they did. `bees config migrate` does the same and nothing
  else, so the diff can be read before the factory starts; `bees config
  validate` only reports that a migration is pending.
- Migrations rewrite the text of the file, so comments and the commented-out
  defaults survive. The one migration, 0 to 1, adds the `version` key and
  changes nothing else.

Adding an optional key never bumps the version. Renaming or removing a key, or
changing what one means, does, and the release notes of the bees version that
bumps it say what changed.

A bees release can also start refusing a value it accepted before, without a
migration. The file then fails to load with an error naming the key, because
the value is yours and bees cannot guess the replacement. The MCP server name
`bees` is one such value (see [MCP servers](#mcp-servers)).

## `[project]`

| Key | Type | Default | Description |
|---|---|---|---|
| `remote` | string | `"origin"` | Git remote the factory fetches from and pushes to. A name; spaces and `/` are rejected. |
| `repo` | string | derived | GitHub repository as `owner/name`. Derived from the remote's URL when unset (https, `ssh://` and `git@github.com:` forms). Set it when the URL is not a github.com one. |
| `default_branch` | string | derived | Branch developers branch from, reviewers diff against and QA tests. Read from the remote's HEAD when unset. |
| `state_dir` | string | `".bees"` | Where mail, notes, session logs and scheduler state live. A relative path is resolved against the directory holding `bees.toml`. `bees init` adds it to `.gitignore` when it is inside the clone. |
| `branch_prefix` | string | `"bees/"` | Prefix of developer branches: `bees/issue-12`. |

What the product is and how to build, test and run it are not configuration.
Every role reads the repository's own README, CONTRIBUTING and CLAUDE.md, and
keeps what it learns in its notes file.

The clone holding `bees.toml` is the main checkout. Every session runs in a
temporary `git worktree` cut from it, so `remote` has to point at the GitHub
repository.

## `[filter]`

The filter decides which issues and pull requests the factory sees and touches.
Configured criteria are ANDed, and everything the factory creates is made to
match them.

| Key | Type | Default | Description |
|---|---|---|---|
| `label` | string | `"bees"` | The factory's label. It is the base name of every workflow label (`bees:triage`, `bees:ready`, ...) and, while `require_label` is true, the visibility gate. Spaces and colons are rejected. |
| `require_label` | bool | `true` | With `false`, `assignee` and `milestone` alone decide visibility. The factory still puts `label` on everything it creates. |
| `assignee` | string | `""` | Only see items assigned to this GitHub login. `"@me"` is resolved to the machine's own `gh` login at startup, even with [`[github]`](#github) set. Everything the factory creates is assigned to this login so it stays visible. |
| `milestone` | string | `""` | Only see items in this milestone, by title. Also the milestone for issues the factory creates when neither `--parent` nor `--related` gives one, and the one put on the pull requests it opens. People manage milestones; bees only inherits them. |

`require_label = false` without `assignee` or `milestone` is rejected: it would
make every open issue in the repository visible.

One person running busybees for their share of a team repository needs no
special label on the issues:

```toml
[filter]
label = "kyle-bees"     # still the base of the kyle-bees:* labels
require_label = false
assignee = "@me"
```

An issue assigned to you with no state label and neither `kyle-bees:feature`
nor `kyle-bees:feedback` gets `kyle-bees` and `kyle-bees:feedback` on first
sight and goes to the product manager as feedback; label it `kyle-bees:triage`
or `kyle-bees:ready` yourself to have it built. Because every label carries
the prefix, two people can run two factories in one repository with two
labels.

### Workflow labels

Every label is derived from `filter.label`, shown here for `bees`. `bees init`
and `bees labels sync` create them in GitHub.

| Label | Meaning |
|---|---|
| `bees` | Visible to the factory |
| `bees:feature` | Feature issue owned by the product manager, which makes it detailed enough and breaks it into work items. Outside the state machine |
| `bees:bug` | Bug work item, filed by a developer, the reviewer, QA or a person. It says what the issue is, not where it goes |
| `bees:feedback` | Feature idea, product feedback or bug report for the product manager. Outside the state machine |
| `bees:question` | The product manager is waiting for a person to answer on a feature or feedback issue; removed by the orchestrator when they reply |
| `bees:proposal` | A feature issue a bee wrote. It sits next to `bees:feature`, and a person removes it to approve the breakdown |
| `bees:planning` | A person and the product manager are still agreeing a feature or feedback issue; the product manager discusses and breaks nothing down. Not a state label; only a person sets or removes it. See [Planning with the product manager](workflow.md#planning-with-the-product-manager) |
| `bees:planned` | A person ended planning: the scope is agreed and the product manager breaks the issue down on its next run. Not a state label; only a person sets or removes it |
| `bees:priority` | A person wants this next: dispatched before the rest of the `bees:ready` queue. Not a state label. People set it; the project manager may add it to a work item that unblocks the factory itself, and the product manager carries one from a feedback issue onto the work item it creates from it |
| `bees:review-requested` | On a pull request, not an issue: a person asks the reviewer for one review pass, whoever opened the pull request. Not a state label; the orchestrator removes it as the review starts. See [Asking for a review of any pull request](workflow.md#asking-for-a-review-of-any-pull-request) |
| `bees:triage` | Needs refinement by the project manager |
| `bees:ready` | Detailed enough for a developer |
| `bees:in-progress` | A developer worker owns it |
| `bees:blocked` | Waiting on an answer to a question |
| `bees:review` | Pull request open and under review |
| `bees:approved` | Reviewer approved; waiting for a person to merge |
| `bees:needs-human` | The factory gave up, or a person is holding the issue. A person may add it on top of another state label to hold the issue where it is; it wins while it is there |

An issue also carries at most one size label, independent of its state. The
project manager sets it when it moves a work item to `bees:ready`, and the
orchestrator adds `bees:size/m` to a ready issue that has none. See
[Sizing](workflow.md#sizing).

| Label | Meaning |
|---|---|
| `bees:size/xs` | One file, obvious change, no design |
| `bees:size/s` | A few files, clear approach, existing tests cover it |
| `bees:size/m` | A feature slice across several packages, needs new tests |
| `bees:size/l` | Crosses subsystems or needs a design decision |
| `bees:size/xl` | Too big for one pull request; split it instead |

## `[github]`

With the table unset, the factory acts as whatever account the machine's `gh`
is logged in with, and everything it writes on GitHub looks like it came from
the person running it. `[github]` gives it an account of its own.

```toml
[github]
login = "busybees-bot"
token = "$BEES_GITHUB_TOKEN"
git_name = "busybees"
git_email = "busybees@example.com"
```

| Key | Type | Default | Description |
|---|---|---|---|
| `login` | string | `""` | The account the factory acts as. `bees init` and `bees doctor` check the token belongs to it, `bees status` reports it, and the orchestrator reads every comment that login posts as the factory's own. It must be the login GitHub reports as the token's user. |
| `token` | string | `""` | A token for `login`, passed as `GH_TOKEN` to every `gh` call the orchestrator makes and to every session. A `"$VAR"` or `"${VAR}"` value is read from the environment bees runs in, so the secret stays out of the file. A reference that expands to nothing is a load error naming the variable. |
| `git_name` | string | `""` | Author and committer name for the commits developer sessions make (`GIT_AUTHOR_NAME`, `GIT_COMMITTER_NAME`). |
| `git_email` | string | `""` | Author and committer email for those commits (`GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_EMAIL`). |

`login` and `token` go together: either alone is rejected. `git_name` and
`git_email` are an identity rather than a credential, so each may be set alone,
and whichever is unset leaves that half of the commit identity to the machine's
own git configuration.

The token is a fine-grained personal access token belonging to a user account,
a bot account or your own, scoped to the one repository: read and write on
Issues, Pull requests and Contents, read on Metadata. Issues and pull requests
cover labels, comments, milestones and reviews; contents covers the pushes
developer sessions make. A classic token with the `repo` scope also works. The
token has to authenticate as a user, because `login` is compared with the
account GitHub says the token belongs to.

`bees init` checks the token before the factory uses it: GitHub accepts it, it
belongs to `login`, and it can read the repository. `bees doctor` asks the same
questions of whatever token is configured, and two more: that the account can
write an issue and that it can push a branch. Repository access does not
imply either. A fine-grained token's permissions sit on top of the repository
role, so a token can read the repository as an admin and still be refused
every label edit or every push.

The token covers everything the factory does on GitHub: polling, label edits,
review requests, the escalation comment, `bees init`'s label creation, `bees
doctor`'s checks, `bees issue`, and the built-in MCP tools. Sessions get it
too, so a session's own `gh pr create`, `gh api` and `git push` act as the
factory. With `git_name` and `git_email` set, its commits are the factory's as
well.

Comments the factory posts still end with the
[comment marker](roles.md#common-ground), because `[github]` is optional and
with it unset every comment arrives under your own login. With it set, the
orchestrator reads a comment as the factory's when it carries the marker or
when `login` wrote it, so the escalation comment, which carries no marker, is
not mistaken for a person's.

A session runs with `GH_TOKEN` set to the token, the `GIT_AUTHOR_*` and
`GIT_COMMITTER_*` variables from `git_name` and `git_email`, and git configured
to answer https pushes through `gh auth git-credential`. Your own credential
helper is reset first so it cannot answer the push, and your stored credentials
are neither read nor written. The helper only steers https remotes: on an
`ssh://` or `git@github.com:` remote the commits are the factory's but the push
authenticates with the machine's ssh key. When `token` is a `"$VAR"` reference
the session is also given that variable, holding the resolved token, because
the `bees` commands a session runs load `bees.toml` themselves. It goes into
the session's environment and never into a file. The full list is under
[Exported into every session](#exported-into-every-session).

`filter.assignee = "@me"` still means you. It says whose work the factory picks
up, and is resolved with the machine's own `gh` login before any token is used,
in the orchestrator, the MCP server and `bees doctor` alike. To pick up the
bot's issues instead, write its login out:

```toml
[filter]
assignee = "busybees-bot"
```

## `[scheduler]`

| Key | Type | Default | Description |
|---|---|---|---|
| `poll_interval` | duration | `"5m"` | How often GitHub is polled. A poll costs two API calls (`gh issue list`, `gh pr list`); see [API budget](#api-budget). Also the minimum gap between two runs of one singleton role. |
| `rate_limit_backoff` | duration | `"15m"` | How long to pause polling after a poll fails with a GitHub rate-limit error, instead of retrying after `poll_interval`. Also how long the whole factory pauses when a session hits the claude session limit and no usable reset time came with it; see [The claude session limit](#the-claude-session-limit). |
| `max_developers` | int | `1` | Concurrent developer workers. Each owns one issue and runs its developer, reviewer and checks stages one after another, so reviewer concurrency follows developer concurrency. `0` means the default; a negative value is rejected. |
| `max_review_rounds` | int | `3` | Developer and reviewer rounds before an issue is escalated with `bees:needs-human`. `0` means the default; a negative value is rejected. |
| `retries` | int | `1` | Extra attempts a session gets after failing for infrastructure reasons: it timed out, ran out of turns, hit an API error or rate limit, or `claude` crashed. A session that ran and reported with `bees done`, `failed` included, is not retried, and neither is one that hit the claude session limit. `0` disables retrying; `0` to `5`. See [Escalation](workflow.md#escalation-beesneeds-human). |
| `retry_delay` | duration | `"10m"` | Wait before a retry. `"0s"` retries at once; a negative value is rejected. |
| `retry_with_fallback` | bool | `true` | Run the retry with the role's `fallback_model` as its primary model. A role without one reruns as it was. |
| `triage_batch_size` | int | `5` | Most issues handed to the project manager in one session. `0` means the default. |
| `notes_consolidate_every` | int | `10` | Sessions a role runs between two in which it is also asked to consolidate its [notes file](roles.md#notes-files). `0` means the default; a negative value is rejected. |
| `notes_max_bytes` | int | `32768` | Ask for consolidation early, whatever the session count, once a notes file is larger than this. `0` means the default; a negative value is rejected. |
| `dispatch_order` | string | `"small-first"` | Which `bees:ready` issue a free developer takes next: `small-first`, `oldest` or `large-first`. Ties go to the older issue, and an issue without a size ranks as `m`. Issues carrying `bees:priority` come first whatever this says, and issues already in flight (`bees:in-progress`, `bees:review`, a `bees:ready` issue with an open pull request, a `bees:approved` issue whose checks were interrupted) are resumed before new work. See [Sizing](workflow.md#size-decides-what-gets-built-next). |
| `max_large_in_flight` | int | `1` | How many `bees:size/l` issues developer workers may hold at once. A large issue over the cap is skipped and the worker takes the next issue that fits. `0` means no cap; a negative value is rejected. |
| `pr_fix_conflicts` | bool | `true` | Hand an open pull request that conflicts with the default branch back to its developer: the developer is mailed from `orchestrator` to merge, resolve, test and push, and an approved issue goes back to `bees:ready` ahead of new work. See [Conflicts with the default branch](workflow.md#conflicts-with-the-default-branch). |
| `pr_keep_updated` | bool | `false` | Do the same when a pull request is merely behind the default branch. |
| `notify` | string list | `[]` | GitHub logins and `org/team` slugs the factory turns to when it needs a person. No leading `@`, at most one `/`. See [Notifying a person](#notifying-a-person). |
| `product_manager_interval` | duration | `"1h"` | Minimum time between product manager runs. Unread mail in its inbox starts one earlier. |
| `qa_interval` | duration | `"30m"` | Minimum time between QA runs. QA runs when something was merged since its last run (the first run always happens); mail in its inbox starts one earlier. The merged-PR query runs at most once per interval. |
| `max_cost_per_issue` | float | `0` | USD one work item may cost across every session run for it. `0` is unlimited; a negative value is rejected. See [Cost budgets](#cost-budgets). |
| `max_cost_per_day` | float | `0` | USD the whole factory may spend over a rolling 24 hours. `0` is unlimited; a negative value is rejected. |
| `max_cost_per_day_resume_percent` | float | `100` | How far the rolling 24 hours has to fall back before dispatch starts again, as a percentage of `max_cost_per_day`: at `80` a factory paused at $100.00 stays paused until the window is under $80.00, rather than resuming at $99.99 and going straight over again. `100` (the default) resumes as soon as the window is under budget, `0` means that default, and a value outside 0-100 is rejected. See [Cost budgets](#cost-budgets). |
| `max_cost_per_session` | float | `0` | USD one session may cost. `0` is unlimited; a negative value is rejected. |
| `keep_workspaces` | bool | `false` | Leave temporary worktrees on disk after a session, for debugging. |
| `workspace_root` | string | `""` | Directory temporary worktrees are created under. Empty means `bees` under the system temp directory. |
| `work_hours` | string | `""` | Daily window during which GitHub is polled every `poll_interval`, as `"HH:MM-HH:MM"` on a 24-hour clock. Empty polls around the clock, and the three keys below are ignored. See [Work hours](#work-hours). |
| `off_hours_poll_interval` | duration | `"1h"`, or `poll_interval` when that is longer | How often GitHub is polled outside `work_hours`. Must be at least `poll_interval`. |
| `work_days` | string list | `["mon","tue","wed","thu","fri"]` | Days the window applies to, as lowercase three-letter names from `mon` to `sun`. At least one, and each must be a known day. |
| `timezone` | string | `""` | IANA name the window is read in, such as `"America/New_York"`. Empty means the machine's local time. |

### Notifying a person

By default the factory and the people it works for share one GitHub account, so
nothing it writes notifies anybody: you wrote the comment, and GitHub sends no
mail for your own. `notify` says who to reach:

```toml
[scheduler]
notify = ["kpenfound", "myorg/bees-team"]
```

- The `bees:needs-human` escalation comment starts with `@kpenfound
  @myorg/bees-team`. See [Escalation](workflow.md#escalation-beesneeds-human).
- The product manager's `bees:question` comments start with the same line.
- A pull request the reviewer moved to `bees:approved` is waiting for a person
  to merge it, so a review is requested from every entry.

The review request is best effort: a failure is logged and the pull request
still reaches `bees:approved`. GitHub refuses to request a review from a pull
request's own author, and with a shared account the configured login usually
is the author, so a login often gets the mention and not the request. Teams
are always accepted; list one if you want the request too.

### Work hours

Most polling exists to notice what people did: new issues, feedback, reviews,
merges. Outside working hours that is rare, so `work_hours` polls GitHub less
often then:

```toml
[scheduler]
poll_interval = "5m"            # inside the window
off_hours_poll_interval = "1h"  # outside it
work_hours = "09:00-18:00"
work_days = ["mon", "tue", "wed", "thu", "fri"]
timezone = "America/New_York"
```

Only the GitHub polling cadence changes. The scheduler still ticks every
`poll_interval`, and a finished session wakes it in between; a tick that is not
due for a poll runs a local pass over the last poll's issue and PR lists (see
[The scheduler loop](architecture.md#the-scheduler-loop)). Everything driven by
the local mailbox, which is the developer and reviewer loop, the checks stage
and an answered question moving `bees:blocked` back to `bees:ready`, runs at
full speed at every hour. `bees tick` and `bees exec` ignore the window.

The poll before the window opens is scheduled for the moment it opens, so the
first poll of the day is at `09:00`, not an interval later.

A rate limit never speeds polling up. After a rate-limited poll the next one is
due after the longer of `rate_limit_backoff` and the interval in force, so off
hours with `off_hours_poll_interval = "8h"` that is 8h.

A window whose start is later than its end wraps midnight and belongs to the
day its start falls on: `"22:00-06:00"` with `work_days = ["fri"]` covers
Friday 22:00 through Saturday 06:00 and nothing else. A window whose start
equals its end is rejected, as is a window that is not `"HH:MM-HH:MM"`, an
unknown or empty `work_days`, a timezone the zoneinfo database does not know,
and an `off_hours_poll_interval` shorter than `poll_interval`.

`bees status` prints a `work hours:` line either way, with the cadence in force
and when the next poll is due. See [`bees status`](cli.md#bees-status---json).

### API budget

The orchestrator is frugal with the GitHub API, because the sessions call `gh`
freely on top of it:

| What | Cost | When |
|---|---|---|
| A poll | 2 calls (`gh issue list`, `gh pr list`) | every `poll_interval`, or every `off_hours_poll_interval` outside `work_hours` |
| Human PR feedback | 3 calls per PR (reviews, review comments, comments) | only for PRs whose `updatedAt` moved since the last look |
| Human issue comments | 1 call per issue | only for issues in `bees:in-progress`, `bees:review`, `bees:approved` or `bees:blocked` whose `updatedAt` moved since the last look. A pass that sees an issue in `bees:triage` or `bees:ready` records the time and fetches nothing, and so does the first pass that sees an issue with no recorded time in one of the four states |
| Product-manager has-work check | 1 `issue view` per feedback or feature issue | only for issues whose `updatedAt` is newer than the last product manager run. Noticing that a feature's sub-issues have all closed costs nothing: it compares the numbers recorded on the last run with what the poll found open |
| Product-manager run | 1 `issue view` per open feedback or feature issue, 1 REST call per open feature (sub-issue progress) and 1 GraphQL call per open work item (parent feature) | every run |
| Planning mode | 1 extra `issue view` per `bees:planned` issue the has-work check did not already fetch | every product manager run while an issue is agreed and not yet acted on |
| QA merged-PR check | 1 call | at most once per `qa_interval` |
| Checks | 1 call per poll of a checks stage, 2 when the branch requires no check | every `roles.reviewer.checks_poll_interval` while waiting |
| Visibility backstop | 2 list calls | after every session |
| Parent feature lookup | 1 GraphQL call per triage item, per open work item, per developer session, and per review round with a `product-fit` stage configured | per project manager run, product manager run, developer session, reviewer session |
| `bees issue create --parent` | 3 calls (parent details, create, attach as sub-issue); `--related` 2; plain 1 | whenever a role files an issue |
| Worker stage transitions | a few `issue view`, `pr view` and `issue edit` calls | per transition |

An idle factory costs two calls per poll. When GitHub does rate-limit the
process, polling pauses for `rate_limit_backoff` and tries again.

What the factory spends on Anthropic is a separate budget, capped by the three
`max_cost_*` keys.

### Cost budgets

All three budgets are `0` by default, which is unlimited. They are spent
against the session ledger, the numbers
[`bees cost`](cli.md#bees-cost---since-24h---by-roleissueday---json) reports,
so a retried session counts like any other:

```toml
[scheduler]
max_cost_per_issue = 25.00   # every session run for one work item
max_cost_per_day = 100.00    # whole factory, rolling 24 hours
max_cost_per_session = 10.00 # a single session
```

A running session is never interrupted on cost. Each budget is enforced at the
first moment the factory can act on it:

- Per issue, between the stages of a developer worker. The session that took
  the issue over budget finishes and its work stays on the branch; the worker
  then stops and the issue is escalated with what it spent ("Issue #12 has
  cost $26.40 across 7 sessions, over the `max_cost_per_issue` budget of
  $25.00").
- Per day, before anything is dispatched. At or over the budget the scheduler
  keeps polling and reconciling labels but starts no session; workers already
  running finish their loop. Dispatch starts again once the window has fallen
  back to `max_cost_per_day_resume_percent` of the budget, by default the
  budget itself, so at `80` a factory paused at $100.00 resumes under $80.00
  rather than at $99.99. The pause is logged once, the release names the
  threshold it crossed, and `bees status` names the pause while it lasts.
- Per session, after the session ended. An over-budget session is treated as
  failed whatever it reported, so it is retried once, with the role's
  `fallback_model` when `retry_with_fallback` is on. Two over-budget sessions
  in a row for one work item escalate it: the role's `max_turns` or `timeout`
  is the wrong shape for that work.

Budgets are money, not turns: `max_turns` already caps how long one session may
go on.

### The claude session limit

Every role shares one Anthropic account, so when one session runs out of
capacity the whole factory has. `claude` reports it two ways and either counts:
a `rate_limit_event` in the session's stream whose status is neither `allowed`
nor `allowed_warning`, or, from a session that failed without reporting an
outcome, a result text naming a session or usage limit ("You've hit your
session limit · resets 11:50pm (America/Detroit)"). The text is only read this
way when the session reported nothing, because it is the session's own prose.

A session that ends that way is not retried, since every attempt would hit the
same wall, and its issue is not escalated, since the limit says nothing about
the work: the issue keeps its state label and is picked up afterwards. The
scheduler pauses all dispatch, developers and singletons alike, until the limit
resets. Running sessions finish on their own, and polling and label
reconciliation carry on.

The pause lasts until the reset time the event carried, with two limits: a
reset that is missing or already past falls back to `rate_limit_backoff`, and
one more than 8 hours ahead is clamped to 8 hours. The pause is logged when it
starts and when it lifts, and `bees status` names the time it lifts. It is held
in memory only, so restarting `bees run` clears it and the first session
re-learns the limit.

## `[logging]`

```toml
[logging]
format = "text"   # text | json
level = "info"    # debug | info | warn | error
```

| Key | Type | Default | Description |
|---|---|---|---|
| `format` | string | `"text"` | Console log format: `text` or `json`. |
| `level` | string | `"info"` | Console log level: `debug`, `info`, `warn` or `error`. |

Logging is a property of the `bees` process, so the table is top-level and
applies to every command. It is the lowest-priority source: a flag beats an
environment variable, which beats `bees.toml`, which beats the default. So
`bees run --log-format text` gives a readable terminal in a project whose file
says `json`, and `-v` wins over `level = "info"`.

Commands that never read `bees.toml`, such as `bees version` and `bees done`,
and any command run against a file that fails to load, use the flag,
environment and default settings only.

There is no `quiet` key. `--quiet` is a shorthand for one invocation; `level =
"warn"` is the service-shaped equivalent, and it also drops the one-line
session summaries that `--quiet` keeps.

`bees run` in a terminal draws the live view instead of logging and silences
console logging while it is up; `--no-tui`, a redirected stdout and every
other command log as this table says. The `bees.log` file in the state
directory is not configurable: it gets every record at debug level, in JSON.
See [`bees run`](cli.md#bees-run).

## `[global]` and `[roles.<name>]`

`[global]` and each `[roles.<name>]` table take the same keys. The role name
is one of `product_manager`, `project_manager`, `developer`, `reviewer`, `qa`.
The CLI accepts aliases such as `pm` and `dev`; the TOML keys do not.

| Key | Type | Default | Description |
|---|---|---|---|
| `prompt` | string | `""` | Text appended to the role's built-in prompt. |
| `prompt_file` | string | `""` | Path, relative to `bees.toml`, whose contents are appended after `prompt`. The file must exist when the file loads. |
| `skills` | string list | `[]` | Skills by git URL. See [Skills](#skills). |
| `skills_refresh` | string | `"24h"` | `[global]` only. How stale a skill clone may get before it is pulled when a session needs it: `never`, `always` or a duration. |
| `mcp.<name>` | table | | MCP servers keyed by name. See [MCP servers](#mcp-servers). |
| `model` | string | `"opus"` | Claude model alias or full id, passed as `claude --model`. |
| `fallback_model` | string | `"sonnet"` | Passed as `claude --fallback-model`, which Claude Code switches to when `model` has reached its usage limit. Not passed when it equals `model`. |
| `effort` | string | `""` | Passed as `claude --effort` when set: `low`, `medium`, `high` or `max`. |
| `max_turns` | int | `200` | Agentic turns per session (`claude --max-turns`). `0` means the default. |
| `timeout` | duration | `"45m"` | Wall-clock limit for one session; the claude process group is killed when it expires. `"0s"` means the default. |
| `allowed_tools` | string list | `[]` | Passed as `claude --allowedTools`. |
| `disallowed_tools` | string list | `[]` | Passed as `claude --disallowedTools`. |
| `shell` | string | the shell bees runs under | Exported into sessions as `$SHELL`. Claude Code discovers its Bash tool's shell from `$SHELL`, so this is the lever, without being a guarantee. Must be an existing file. |
| `env` | table | `{}` | Environment variables exported into every session: `claude`, its Bash tool, MCP servers and git see them. A `$VAR` value is expanded from the bees process environment when the session starts. A name may not be empty or contain `=` or a space. See [Exported into every session](#exported-into-every-session) for how it meets the variables bees sets itself. |
| `enabled` | bool | `true` | Roles only. `false` takes a role out of the rotation. Disabling `reviewer` makes a developer's pull request count as approved the moment it is opened, and with `auto_merge` it goes straight to the checks stage. Under `[global]` the key is an error. |

### `[roles.reviewer]` only: checks and auto-merge

The reviewer owns the checks and the merge. These keys are accepted only under
`[roles.reviewer]`; under `[global]` or another role they are a load error.

| Key | Type | Default | Description |
|---|---|---|---|
| `auto_merge` | bool | `false` | Merge a pull request the reviewer approved once its checks are green. Off means people merge. |
| `merge_method` | string | `"squash"` | `squash`, `merge` or `rebase` (`gh pr merge --<method> --delete-branch`). |
| `checks_wait` | duration | `"1m"` | Wait after approval before polling the checks, because some take a moment to report they started. |
| `checks_poll_interval` | duration | `"2m"` | How often the checks are polled while waiting. One API call each, two when the branch requires nothing. |
| `checks_timeout` | duration | `"30m"` | How long to wait for the checks before escalating with `bees:needs-human`. |
| `max_check_fix_rounds` | int | `2` | Rounds of the reviewer diagnosing and the developer fixing a failed check before escalating. |
| `pre_review_checks` | bool | `true` | Read the pull request's checks before the first review, so the reviewer starts from a green pull request or is told it is not. Independent of `auto_merge`. |
| `pre_review_checks_timeout` | duration | `"10m"` | How long that pre-review read waits for pending checks before reviewing anyway. |

```toml
[roles.reviewer]
auto_merge = true
merge_method = "squash"
checks_timeout = "20m"
```

With `pre_review_checks` on, a developer worker reads the pull request's
checks once, between opening it and the first review: `checks_wait`, then a
poll every `checks_poll_interval`, bounded by `pre_review_checks_timeout`.
Green, and the review starts with the checks listed in the reviewer's prompt.
A failure goes to the reviewer in checks mode and then to the developer for a
fix round first, sharing `max_check_fix_rounds` with the post-approval stage,
and the review happens once the pull request is green. Still pending at the
timeout, no check reported at all, or a read that fails, and the review
happens anyway with the reviewer told nothing was verified. Later review
rounds go straight to the reviewer with no second read. `bees status` shows
the worker in the `pre-review checks` stage while it waits.

With `auto_merge` on, after approval the worker waits `checks_wait` and polls
the checks every `checks_poll_interval`. All green merges. A failing check
gets the reviewer a checks-mode session to find the main error and mail it to
the developer, who pushes a fix, and the checks are polled again, up to
`max_check_fix_rounds`. Still pending at `checks_timeout`, or a merge GitHub
refuses (branch protection that needs a human review, say), escalates with
`bees:needs-human`. See [Merging](workflow.md#merging).

Which checks are the gate: the required checks (`gh pr checks --required`)
whenever the default branch requires any, and only those. A repository with no
branch protection requires nothing, and gating on nothing would merge with
nothing green, so there every check the pull request reports is the gate: a
failing one blocks and a pending one is waited for. To take a check out of the
gate, mark the ones that must block as required in the default branch's
protection rules; bees never reads those rules to change them. With no check
reported at all, it merges after two consecutive empty polls and logs that no
check was reported. `bees doctor` says which of the three is in force, and
`bees status` shows it in the worker stage (`checks (required)`, `checks
(reported)`, `checks (none)`).

### `[roles.reviewer]` only: the review stages

The reviewer reviews in ordered stages, each with its own focus, source of
truth and verdict. Like the checks keys, `stages` is accepted only under
`[roles.reviewer]`.

| Key | Type | Default | Description |
|---|---|---|---|
| `stages` | string list | `["implementation", "completeness", "cleanliness", "style"]` | The stages to run, in order. One or more of `implementation`, `completeness`, `cleanliness`, `style`, `product-fit`. An unknown name or an empty list is a load error. |

| Stage | Question it answers | Source of truth |
|---|---|---|
| `implementation` | Is it correct? Error handling, edge cases, tests, security. | the diff |
| `completeness` | Does it deliver the work item's acceptance criteria? | the issue |
| `cleanliness` | Is it clear, small, free of dead code and needless abstraction? | the diff |
| `style` | Does it follow the repository's formatting and lint conventions? | the repository's conventions, CLAUDE.md, the linter |
| `product-fit` | Does it fit the parent feature and the product direction? | the parent feature, the README and the docs |

Every configured stage runs, and each ends with a verdict line. Requesting
changes still sends one message to the developer, grouped by stage in the
configured order. An approval means every stage passed.

`product-fit` is off by default: a work item the project manager already
scoped is not the place to reopen the product decision. It is the only stage
that reads the work item's parent feature, and the orchestrator makes that
lookup, one GraphQL call per review round, only when the stage is configured.

```toml
[roles.reviewer]
stages = ["implementation", "completeness", "cleanliness", "style", "product-fit"]
```

See [Review stages](roles.md#review-stages-rolesreviewerstages) for what each
stage looks at.

### `[roles.developer]` only: commit flags, max size and per-size models

These keys describe the developer, so they are accepted only under
`[roles.developer]`.

| Key | Type | Default | Description |
|---|---|---|---|
| `commit_flags` | string | `""` | Extra flags for every `git commit` the developer makes, appended to its prompt verbatim. |
| `max_size` | string | `"l"` | The largest work item a developer takes: `xs`, `s`, `m`, `l` or `xl`. A `bees:ready` issue sized above it is moved back to `bees:triage` for the project manager to split; the project manager is told the limit. See [Sizing](workflow.md#size-decides-what-gets-built-next). |
| `model_by_size` | table | `{}` | The model per work item size, keyed by `xs`, `s`, `m`, `l`, `xl`. An unknown key or an empty value is a load error. A size with no entry, and an issue with no size label, uses `model`. |

```toml
[roles.developer]
commit_flags = "--gpg-sign --signoff"
max_size = "m"          # anything bigger goes back to triage to be split

[roles.developer.model_by_size]
xs = "sonnet"           # a typo fix does not need the strongest model
s = "sonnet"
```

`model_by_size` is read once per session from the size label the issue carries
when the developer picks it up: `bees:size/xs` above runs as `--model sonnet`,
every other size as the developer's `model`. `fallback_model` is unchanged, and
a retry that runs with it still overrides the size's choice. The reviewer is
told the size too and always runs its own `model`.

Signing (`--gpg-sign`, `-S`) happens inside a headless Claude Code session on
the machine running `bees`, so a signing key and agent must work for that user
without a prompt. `--signoff` needs `user.name` and `user.email`, as any
commit does.

### How global and role settings merge

| Setting | Rule |
|---|---|
| `prompt`, `prompt_file` | Concatenated, separated by blank lines: global `prompt`, global `prompt_file`, role `prompt`, role `prompt_file`. The result is appended to the role's built-in prompt under an "Additional instructions from bees.toml" heading, followed by the repository's [project prompt files](#project-prompt-files). |
| `skills` | Union, global first, order kept, duplicates dropped. |
| `mcp` | Union by name; a role server replaces a global one of the same name. |
| `env` | Union by name; the role wins. |
| `model`, `fallback_model`, `effort`, `max_turns`, `timeout`, `shell` | Role value if set, else global, else the built-in default. |
| `allowed_tools`, `disallowed_tools` | Global list followed by the role list. |
| `enabled` | Role only. |
| `skills_refresh` | Global only. |
| `commit_flags`, `max_size`, `model_by_size` | `roles.developer` only. |
| `auto_merge`, `merge_method`, `checks_wait`, `checks_poll_interval`, `checks_timeout`, `max_check_fix_rounds`, `pre_review_checks`, `pre_review_checks_timeout`, `stages` | `roles.reviewer` only. `bees config show reviewer` prints the resolved policy. |

`bees config show <role>` prints the result.

Only the contents of `prompt_file` are re-read for every session. Everything
else, `prompt` included, comes from the `bees.toml` that `bees run` loaded when
it started, so an edit reaches no session until the scheduler is restarted.
The built-in role prompts are compiled into the `bees` binary and need a
rebuild as well as a restart. `bees status` names the build the running
scheduler was started from, and [`bees doctor`](cli.md#bees-doctor) warns when
that build is behind the commit the repository has checked out.

### Project prompt files

A project can keep role instructions in the repository instead of in
`bees.toml`, so they are versioned and reviewed like code, and a branch can
carry its own.

```
bees/prompts/common.md            every role
bees/prompts/developer.md         the developer
bees/prompts/product_manager.md   the product manager
```

`bees/prompts/common.md` is appended to every role's prompt and
`bees/prompts/<role>.md` to that role's. Nothing is configured; a repository
without the directory renders the prompt it would render anyway. The sources
land on the built-in prompt in this order, each under a heading naming where it
came from, so a `bees.toml` override still wins over what the repository says:

1. `[global]` `prompt` and `prompt_file`
2. `[roles.<name>]` `prompt` and `prompt_file`
3. `bees/prompts/common.md`
4. `bees/prompts/<role>.md`

The directory is `bees/`, without a dot. `bees init` adds `/.bees/` to
`.gitignore`, so files under `.bees/prompts/` would be untracked, the opposite
of instructions reviewed like code.

Sessions read the files from their own worktree at session start, so a session
sees the files on the branch it is working on, and an edit takes effect on the
next session with no rebuild and no restart. `bees prompts show <role>
--rendered` has no worktree and reads the checkout `bees.toml` sits in; it says
so when it finds any.

A file bees cannot use, unreadable or larger than 64 KiB, never stops a
session: the session warns, skips that file and runs with the rest. `bees
doctor` fails on such a file, and on a file no role reads (a misspelled name
such as `bees/prompts/develloper.md`).

Anyone who can land a commit on a branch can change what the sessions on that
branch are told to do. That is the point of the feature, and it is the same
trust boundary as a CI configuration in the repository: review changes to
`bees/prompts/` as you would review a workflow file.

### Skills

Skills are referenced by git URL and cloned into a cache directory,
`~/.cache/bees/repos` on Linux (`~/Library/Caches/bees/repos` on macOS)
unless `BEES_CACHE_DIR` says otherwise. They reach Claude
Code as plugin directories (`claude --plugin-dir`), so the worktree is never
modified. Skills in the repository's `.claude/skills/` and in
`~/.claude/skills/` are available without being listed.

```toml
skills = [
  "https://github.com/acme/skills",                 # whole repository
  "https://github.com/acme/skills#skills/tdd",      # one directory inside it
  "https://github.com/acme/my-plugin@v1.2.0",       # pinned tag or branch
  "git@github.com:acme/private-skills.git",         # ssh works too
]
```

The URL is `<git-url>[@<ref>][#<sub/dir>]`. The selected directory is one of
three layouts, checked in this order:

1. A Claude Code plugin, with `.claude-plugin/plugin.json`. Used as-is.
2. A single skill, with `SKILL.md` at its root. Wrapped in a generated plugin
   exposing that skill.
3. A skills collection, with a `skills/` directory. Wrapped in a generated
   plugin exposing every skill in it.

Anything else is an error. Clones and wrappers are reused; a wrapper is
rebuilt only when it is missing or its `#sub/dir` changed, because a session
may be running with it.

A clone is pulled (`git pull --ff-only`) when a session needs it and it was
last fetched more than `skills_refresh` ago, `24h` by default. `"always"` pulls
before every session, `"never"` never pulls. A failed pull is logged as a
warning and never stops a session; a reference pinned with `@tag` is a
detached checkout and cannot be pulled, which is the point of pinning.

[`bees skills list`](cli.md#bees-skills-list) shows the cache and
`bees skills update` pulls everything now, whatever the policy says.

### MCP servers

Servers are written to a per-session `--mcp-config` file and loaded with
`--strict-mcp-config`, so a session sees the servers configured here plus the
built-in one, and none of your own.

`bees` is reserved. Every session gets a server called `bees` carrying the
factory's own tools; see [`bees mcp serve`](cli.md#bees-mcp-serve-sessions).
It needs no configuration and cannot be turned off. Defining
`[global.mcp.bees]` or `[roles.<role>.mcp.bees]` fails to load with
`mcp server name "bees" is reserved for the built-in server`.

| Key | Description |
|---|---|
| `type` | `stdio`, `http` or `sse`. Defaults to `stdio` when `command` is set, `http` when only `url` is. |
| `command` | Executable of a stdio server. |
| `args` | Arguments for `command`. |
| `env` | Environment of the server process. `$VAR` and `${VAR}` are expanded from the `bees` process environment. |
| `url` | Endpoint of an `http` or `sse` server. |
| `headers` | HTTP headers for a remote server; `$VAR` expansion applies. |

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

## Examples

### Solo project, two developers

```toml
version = 1
# repo and default_branch are derived from the origin remote.

[filter]
label = "bees"

[scheduler]
max_developers = 2
qa_interval = "1h"

[global]
prompt = """
Use conventional commits. Never add a dependency without a comment saying why.
"""

[roles.developer]
commit_flags = "--signoff"

[roles.reviewer]
model = "sonnet"
prompt = "Be strict about error handling and test coverage."
auto_merge = true
checks_timeout = "20m"

[roles.qa]
skills = ["https://github.com/anthropics/skills#skills/webapp-testing"]
timeout = "30m"
```

### Team repository, only work assigned to me

```toml
version = 1

[project]
remote = "upstream"        # my origin is a fork; the team repository is upstream

[filter]
label = "kyle-bees"        # the kyle-bees:* labels are mine
require_label = false
assignee = "@me"

[scheduler]
product_manager_interval = "4h"

[global]
prompt_file = "docs/engineering-conventions.md"

[roles.product_manager]
enabled = false            # the team's own product manager owns the roadmap

[roles.developer]
prompt = "Only touch files under services/billing unless the issue says otherwise."
```

## Requirements

`bees run`, `tick`, `exec`, `status`, `issue create` and `issue link` check
the tools they drive before doing anything and refuse to start when one is
missing or too old; `bees init` checks `gh`.

| Tool | Minimum | Why |
|---|---|---|
| [`gh`](https://cli.github.com/) | 2.50.0 | `gh pr checks --json` (2.50.0) and `gh api --slurp` (2.49.0). |
| Claude Code (`claude`) | 2.1.76 | `claude --name` (2.1.76); `--append-system-prompt-file`, `--effort`, `--plugin-dir`, `--strict-mcp-config` and `--fallback-model` are older. |

Set `BEES_SKIP_VERSION_CHECK=1` to run with an unsupported version anyway.

## Environment variables

### Honoured by the `bees` command

| Variable | Effect |
|---|---|
| `BEES_CONFIG` | Path of `bees.toml` when `--config` is not given. Set inside sessions. |
| `BEES_CLAUDE_BIN` | The `claude` executable to run. Default `claude` on `PATH`. |
| `BEES_CACHE_DIR` | Cache directory for skill clones and generated plugins. Default `~/.cache/bees` on Linux, `~/Library/Caches/bees` on macOS. |
| `BEES_SKIP_VERSION_CHECK` | When non-empty, skip the `gh` and `claude` version checks. |
| `BEES_STATE_DIR` | `bees mail` and `bees notes` use this state directory without loading `bees.toml`, unless `--config` is given, in which case that file's state directory wins. Set inside sessions. |
| `BEES_SESSION_DIR` | Where `bees done` writes `outcome.json`; `bees done` refuses to run without it. Set inside sessions. |
| `BEES_ROLE` | Default `--from` of `bees mail send`, and the role whose `bees done` statuses are validated. Set inside sessions. |
| `BEES_ISSUE`, `BEES_PR` | Defaults for the `--issue` and `--pr` flags of `bees mail send` and `bees done`. Set inside sessions. |
| `BEES_LOG_FORMAT`, `BEES_LOG_LEVEL` | Fallbacks for `--log-format` and `--log-level`. A flag beats them, and they beat [`[logging]`](#logging). |

The variables marked *set inside sessions* are the only ones a session
inherits. `BEES_CLAUDE_BIN`, `BEES_CACHE_DIR`, `BEES_SKIP_VERSION_CHECK`,
`BEES_LOG_FORMAT` and `BEES_LOG_LEVEL` configure the `bees` process you start
and are not passed on, so a `bees` command a session runs itself sees their
defaults. To give sessions one of them, put it in
[`[global.env]`](#global-and-rolesname).

### Exported into every session

A session runs with the `bees` process environment, minus every `BEES_*`
variable that process inherited, plus:

| Variable | Value |
|---|---|
| *configured `env`* | Every `[global.env]` and `[roles.<name>.env]` entry, `$VAR`-expanded. Set first, so everything below wins over it. |
| `BEES_ROLE` | The role name (`developer`, `reviewer`, ...). |
| `BEES_SESSION_DIR` | This session's directory: prompts, transcript, `outcome.json`. |
| `BEES_STATE_DIR` | The state directory. |
| `BEES_CONFIG` | Path of `bees.toml`. |
| `BEES_REPO` | `owner/name`. |
| `BEES_LABEL` | `filter.label`. |
| `BEES_ISSUE` | The issue the session works on, when any. |
| `BEES_PR` | The pull request, when any. |
| `BEES_BRANCH` | The checked-out branch, when any. |
| `BEES_NOTES_FILE` | The role's notes file. |
| `BEES_BIN` | Path of the `bees` executable. Its directory is also prepended to `PATH`, so a session can run `bees mail` and `bees done`. |
| `BEES_REVIEW_MODE` | `checks` in a reviewer session that diagnoses failed checks; unset otherwise. |
| `SHELL` | The configured `shell`, when set. |
| `GH_TOKEN` | [`github.token`](#github), when set, so the session's `gh` acts as the factory. |
| *the variable `github.token` names* | When `github.token` is a `"$VAR"` reference, that variable, holding the resolved token. The `bees` commands a session runs load `bees.toml` themselves, and a reference that expands to nothing is a load error, so the name has to survive the `BEES_*` strip. It is set in the environment only; nothing writes it into the session directory. |
| `GIT_AUTHOR_NAME`, `GIT_COMMITTER_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_EMAIL` | `github.git_name` and `github.git_email`, when set. |
| `GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_n`, `GIT_CONFIG_VALUE_n` | `push.autoSetupRemote=true` and `push.default=current`, so a plain `git push` works on a fresh branch without touching the clone's git config; with `github.token` set, also an empty `credential.helper` followed by `credential.helper=!gh auth git-credential`, so an https push authenticates as the factory rather than through your stored credentials. Left alone when `GIT_CONFIG_COUNT` is already in the bees environment. |

`BEES_*` variables are always set by bees for each session and never inherited,
so a session started from inside another one, by a nested `bees run` or `bees
exec`, never sees a stale issue, PR or branch. The one `BEES_*` name bees puts
back after the strip is the one a `"$VAR"` `github.token` reads.
