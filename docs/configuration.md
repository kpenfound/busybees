# Configuring busybees: `bees.toml`

One file configures a factory: `bees.toml`, in the root of a git clone of the
project being built, or elsewhere with `project.dir` naming the clone (see
[`[project]`](#project) below). Without `--template`, `bees init` writes it with every
option listed and the optional ones commented out at their default, so
configuring is uncommenting and editing lines; see
[config templates](templates.md) for the alternative. `bees config validate`
checks the file and `bees config show [role]` prints the settings each role
ends up with.

The file starts with a `version` key, followed by these tables:

| Table | Purpose |
|---|---|
| `[project]` | The git remote, repository, default branch and state directory |
| `[filter]` | Which GitHub issues and pull requests the factory can see |
| `[github]` | The GitHub account the factory acts as |
| `[scheduler]` | Concurrency, polling, retries, budgets and the review loop |
| `[logging]` | Console log format and level |
| `[notes]` | Where a role's notes live: files, or Neo4j Agent Memory |
| `[global]` | Prompt, skills, MCP servers, profile selection and environment for every role |
| `[roles.<name>]` | The same keys per role, plus a few that only one role takes |
| `[profiles.<name>]` | Agent, model, effort and sandbox settings selected by `[global]` or a role |

An unknown key anywhere in the file is a load error, so a typo cannot pass as
a default. Every validation error names the key and what to change.

Durations are written the way Go reads them: `"30s"`, `"5m"`, `"1h30m"`.

`bees review` reads none of this file. Its settings are
`~/.config/bees/config.toml` and the project's `context.toml`, both described
in [Reviewing a pull request](review.md#configuration).

One bees process managing several projects reads a
[machine config](#machine-config-several-projects) instead, which lists the
`bees.toml` of each project.

## Machine config: several projects

A machine config is a separate file that lists the `bees.toml` files of the
projects one bees process manages, and holds the settings that span them:

```toml
projects = [
  "~/src/foo/bees.toml",
  "../bar/bees.toml",
  "/srv/factories/baz.toml",
]
max_developers = 3
retention_period = "72h"
```

- A relative path is relative to the directory of the machine config. A
  leading `~/` is your home directory.
- Every entry must name an existing file that loads as a valid `bees.toml`. A
  missing file, a directory, an empty entry, a project listed twice, or a
  project file that fails to load is an error naming the entry, for example
  `projects[1] = "../bar/bees.toml": ...`.
- The list must not be empty, and any key besides `projects`,
  `max_developers` and `retention_period` is a load error. Project settings stay in each project's
  `bees.toml`, so each project keeps its own state directory, mailbox and
  notes.
- `max_developers` caps the developer slots in use across every project
  listed: the total of what their
  [`scheduler.max_developers`](#scheduler) pools have out at once, a
  requested review and every attempt of a fan-out counted like a worker.
  Each project keeps its own `max_developers`; the machine's is on top of
  them. Unset or `0` is no cap across projects, and a negative value is an
  error. When the cap is reached and a slot frees up, it goes to the project
  that has waited longest for one, not to the project that just gave it up,
  so a busy project cannot starve the others. See
  [Several projects in one process](architecture.md#several-projects-in-one-process).
- `retention_period` is [`scheduler.retention_period`](#scheduler) for every
  listed project whose `bees.toml` does not set it. A project that sets its
  own keeps it. Unset leaves those projects on `"24h"`; zero, a negative
  value or a string that is not a duration is an error.
- The file has no `version` key.

bees finds a machine config the same way it finds `bees.toml`: `--config`,
then `$BEES_CONFIG`, then a file named `bees.toml` in the working directory
or a parent. It reads the file's keys to tell the two kinds apart: a file with
a top-level `projects` key is a machine config. `bees config validate` checks
either kind. A command that works on one project refuses a machine config
with an error saying which kind of file it found.

You edit the file by hand. `bees run --config /path/to/machine.toml` runs
every listed project. Add `-d` to detach; SIGHUP reloads the project list.
See [Running in the background](cli.md#running-in-the-background).

## `version`

```toml
version = 4
```

The format version of the file, not of bees. `bees init` writes the current
one, `4`. A file without the key is version 0.

- A file newer than the running bees understands is refused with `upgrade
  bees`.
- An older file is migrated on load, one version at a time. `bees run`,
  `tick`, `exec`, `status`, `issue create` and `issue link` then write the
  migrated file back, keeping the original as `bees.toml.v<old>.bak` and
  logging that they did. `bees config migrate` does the same and nothing
  else, so the diff can be read before the factory starts; `bees config
  validate` only reports that a migration is pending.
- Migrations rewrite the text of the file, so comments and the commented-out
  defaults survive. The migration from 0 to 1 adds the `version` key and
  changes nothing else. The migration from 1 to 2 removes
  `roles.reviewer.stages` and leaves a comment in its place: no stage maps
  onto a review angle, so the reviewer gets the default `angles`. The migration
  from 2 to 3 moves `agent`, `model`, `fallback_model`, `sandbox`, `effort` and
  `model_by_size` into named `[profiles.<name>]` tables and replaces them with
  `profile` and `profile_by_size`; scopes without those settings use the
  implicit built-in profile. The migration from 3 to 4 converts reviewer
  phase model overrides to named phase profiles, cloning the ordinary
  reviewer fallback profile's five fields and replacing its model. Equal
  profiles are reused; size-specific fallback still applies to phases with
  no override.

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
| `dir` | string | `bees.toml`'s directory | The git clone the factory works in, when it differs from the directory holding `bees.toml`. A relative path is resolved against the directory holding `bees.toml`. |

What the product is and how to build, test and run it are not configuration.
Every role reads the repository's own README, CONTRIBUTING and CLAUDE.md, and
keeps what it learns in its notes file.

The main checkout is `dir` when set, else the clone holding `bees.toml`.
Every session runs in a temporary `git worktree` cut from it, so `remote` has
to point at the GitHub repository.

## `[filter]`

The filter decides which issues and pull requests the factory sees and touches.
Configured criteria are ANDed, and everything the factory creates stays
visible: it is made to match `label`, `assignee` and `milestone`, and the
account the factory acts as always passes `creator`.

| Key | Type | Default | Description |
|---|---|---|---|
| `label` | string | `"bees"` | The factory's label. It is the base name of every workflow label (`bees:triage`, `bees:ready`, ...) and, while `require_label` is true, the visibility gate. Spaces and colons are rejected. |
| `require_label` | bool | `true` | With `false`, `assignee`, `milestone` and/or `creator` alone decide visibility. The factory still puts `label` on everything it creates. |
| `assignee` | string | `""` | Only see items assigned to this GitHub login. `"@me"` is resolved to the machine's own `gh` login at startup, even with [`[github]`](#github) set. Everything the factory creates is assigned to this login so it stays visible. |
| `milestone` | string | `""` | Only see items in this milestone, by title. Also the milestone for issues the factory creates when neither `--parent` nor `--related` gives one, and the one put on the pull requests it opens. People manage milestones; bees inherit them, or pick among them for the features of an agreed design. |
| `creator` | string | `""` | Only see items opened by this GitHub login or by the account the factory acts as ([`github.login`](#github), else your own `gh` login). An issue or pull request the factory opens is authored by that account, so it is visible whatever `creator` says. |

`require_label = false` without `assignee`, `milestone` or `creator` is
rejected: it would make every open issue in the repository visible.

A factory acting as a bot that should pick up only the issues one person
files, plus the ones it opens itself:

```toml
[filter]
creator = "kyle"
[github]
login = "busybees-bot"
token = "$BEES_GITHUB_TOKEN"
```

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
developer sessions make. A classic token with the `repo` scope also works.
With [`scheduler.report_factory_errors`](#scheduler) on, the same token files
the factory-error reports against `kpenfound/busybees`, so a fine-grained
token scoped to your repository alone leaves them in the queue: give it issue
write on `kpenfound/busybees` too, or leave the key off. The
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
| `retries` | int | `1` | Extra attempts a session gets after failing for infrastructure reasons: it timed out, ran out of turns, hit an API error or rate limit, or the agent crashed. A session that ran and reported with `bees done`, `failed` included, is not retried, and neither is one that hit the claude session limit. `0` disables retrying; `0` to `5`. See [Escalation](workflow.md#escalation-beesneeds-human). |
| `retry_delay` | duration | `"10m"` | Wait before a retry. `"0s"` retries at once; a negative value is rejected. |
| `retry_with_fallback` | bool | `true` | Run the retry with the fallback model from the role's resolved profile as its primary model. A profile without one reruns as it was. |
| `triage_batch_size` | int | `5` | Most issues handed to the project manager in one session. `0` means the default. |
| `notes_consolidate_every` | int | `10` | Sessions a role runs between two in which it is also asked to consolidate its [notes](roles.md#notes-files). `0` means the default; a negative value is rejected. |
| `notes_max_bytes` | int | `32768` | Ask for consolidation early, whatever the session count, once a role's notes are larger than this. Measured in the backend [`notes.backend`](#notes) names. `0` means the default; a negative value is rejected. |
| `dispatch_order` | string | `"small-first"` | Which `bees:ready` issue a free developer takes next: `small-first`, `oldest` or `large-first`. Ties go to the older issue, and an issue without a size ranks as `m`. Issues carrying `bees:priority` come first whatever this says, and issues already in flight (`bees:in-progress`, `bees:review`, a `bees:ready` issue with an open pull request, a `bees:approved` issue whose checks were interrupted) are resumed before new work. See [Sizing](workflow.md#size-decides-what-gets-built-next). |
| `max_large_in_flight` | int | `1` | How many `bees:size/l` issues developer workers may hold at once. A large issue over the cap is skipped and the worker takes the next issue that fits. `0` means no cap; a negative value is rejected. |
| `pr_fix_conflicts` | bool | `true` | Hand an open pull request that conflicts with the default branch back to its developer: the developer is mailed from `orchestrator` to merge, resolve, test and push, and an approved issue goes back to `bees:ready` ahead of new work. See [Conflicts with the default branch](workflow.md#conflicts-with-the-default-branch). |
| `pr_keep_updated` | bool | `false` | Do the same when a pull request is merely behind the default branch. |
| `stacked_prs` | bool | `false` | Build a `blocked_by` chain of work items under the same feature as a stack of branches and pull requests, each depending on its predecessor's, instead of independently. `false` keeps every work item branching from the default branch on its own, as it does today. See [Stacked features](workflow.md#stacked-features). |
| `review_assigned_prs` | bool | `false` | Ask the reviewer for one pass over every open pull request in the filter whose head branch does not start with `project.branch_prefix`, without waiting for `bees:review-requested`. With `filter.assignee` set, that is the pull requests assigned to the factory. A new push to one earns another review. See [Asking for a review of any pull request](workflow.md#asking-for-a-review-of-any-pull-request). |
| `report_factory_errors` | bool | `false` | Let a role record a draft issue about an error the factory itself caused: a tool that misbehaved, a prompt that contradicted the code, an orchestrator mislabel it had to work around. The `report_factory_error` tool writes the draft, scrubbed by the role of repository names, tokens, paths and people, to `<state_dir>/feedback/<id>.json`, and every full pass files the queue against the busybees repository: a report that looks like an issue already there is a comment on it, one that looks like nothing is a new issue with no label and no assignee. Off, the tool records nothing, says so, and nothing is filed. |
| `feature_proposals` | bool | `true` | A feature issue a bee creates is a proposal: it carries `bees:proposal` and a person approves it by removing the label. `false` lets a bee break its own features down without a person's approval: `bees issue create --feature` writes no `bees:proposal`, and `--parent` and `bees issue link` do not refuse a parent for carrying one. See [Feature issues](workflow.md#feature-issues). |
| `notify` | string list | `[]` | GitHub logins and `org/team` slugs the factory turns to when it needs a person. No leading `@`, at most one `/`. See [Notifying a person](#notifying-a-person). |
| `product_manager_interval` | duration | `"1h"` | Minimum time between product manager runs. Unread mail in its inbox starts one earlier. |
| `qa_interval` | duration | `"30m"` | Minimum time between QA runs. QA runs when something was merged since its last run (the first run always happens); mail in its inbox starts one earlier. The merged-PR query runs at most once per interval. |
| `max_cost_per_issue` | float | `0` | USD one work item may cost across every session run for it. `0` is unlimited; a negative value is rejected. See [Cost budgets](#cost-budgets). |
| `max_cost_per_day` | float | `0` | USD the whole factory may spend over a rolling 24 hours. `0` is unlimited; a negative value is rejected. |
| `max_cost_per_day_resume_percent` | float | `100` | How far the rolling 24 hours has to fall back before dispatch starts again, as a percentage of `max_cost_per_day`: at `80` a factory paused at $100.00 stays paused until the window is under $80.00, rather than resuming at $99.99 and going straight over again. `100` (the default) resumes as soon as the window is under budget, `0` means that default, and a value outside 0-100 is rejected. See [Cost budgets](#cost-budgets). |
| `max_cost_per_session` | float | `0` | USD one session may cost. `0` is unlimited; a negative value is rejected. |
| `keep_workspaces` | bool | `false` | Leave temporary worktrees on disk after a session, for debugging. |
| `workspace_root` | string | `""` | Directory temporary worktrees are created under. Empty means `bees` under the system temp directory. |
| `retention_period` | duration | `"24h"` | How long the state directory keeps data after it goes stale: the session directories and `issues/work-<hash>.json` of a closed issue, counted from its pull request merging or, without one, the issue closing, and `ledger.jsonl` lines older than this, never within the last 24 hours, which `max_cost_per_day` reads. Retention is always on: zero or a negative value is rejected. Unset, the [machine config](#machine-config-several-projects)'s `retention_period` applies when there is one. |
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
| Human issue comments | 1 call per issue | only for issues whose `updatedAt` moved since the last look: `bees:in-progress`, `bees:review`, `bees:approved` and `bees:blocked` unconditionally, and, once `[github]` names an account to mention, `bees:triage`, `bees:ready`, `bees:feature` and `bees:feedback` too, to look for an `@`-mention. A pass with no `[github]` login configured, or one that sees an issue with no recorded time, records the time and fetches nothing |
| Product-manager has-work check | 1 `issue view` per feedback or feature issue | only for issues whose `updatedAt` is newer than the last product manager run. Noticing that a feature's sub-issues have all closed costs nothing: it compares the numbers recorded on the last run with what the poll found open |
| Product-manager run | 1 `issue view` per open feedback or feature issue, 1 REST call per open feature (sub-issue progress) and 1 GraphQL call per open work item (parent feature) | every run |
| Planning mode | 1 extra `issue view` per `bees:planned` issue the has-work check did not already fetch | every product manager run while an issue is agreed and not yet acted on |
| QA merged-PR check | 1 call | at most once per `qa_interval` |
| Checks | 1 call per poll of a checks stage, 2 when the branch requires no check | every `roles.reviewer.checks_poll_interval` while waiting |
| Visibility backstop | 2 list calls | after every session |
| Refreshing what a session changed | 1 `issue view` per issue | after a session, for each issue it created or relabelled through the MCP server; none when it changed no issue |
| The marker audit | 1 comment read per issue or pull request | after a session, for its issue, the pull request it was given or opened, and each issue it changed, only with `[github]` set, and nothing at all without it |
| Parent feature lookup | 1 GraphQL call per triage item, per open work item and per developer session | per project manager run, product manager run, developer session |
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
  failed whatever it reported, so it is retried once, with the resolved
  profile's fallback model when `retry_with_fallback` is on. Two over-budget sessions
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

## `[notes]`

```toml
[notes]
backend = "file"   # file | neo4j
# neo4j_url = "https://memory.neo4jlabs.com/v1"
# neo4j_api_key = "$BEES_NEO4J_API_KEY"
```

| Key | Type | Default | Description |
|---|---|---|---|
| `backend` | string | `"file"` | Where the `notes_read` and `notes_write` tools keep a role's notes: `file` (the state directory) or `neo4j` (Neo4j Agent Memory). |
| `neo4j_url` | string | `""` | The base URL of the Neo4j Agent Memory REST API, including its version segment: the hosted service's (`https://memory.neo4jlabs.com/v1`) or a self-hosted deployment's. Required, and must be an `http(s)` URL, with `backend = "neo4j"`. |
| `neo4j_api_key` | string | `""` | An API key for `neo4j_url`, written as a `"$VAR"` reference so the secret is not in this file. Required with `backend = "neo4j"`; a reference that expands to nothing fails to load, naming the variable. Redacted as written in `bees config show`. |

`bees` neither runs nor embeds Neo4j Agent Memory; with `backend = "neo4j"`
the notes tools talk to the REST API at `neo4j_url`, and so do `bees status`
and the [`scheduler`](#scheduler) consolidation trigger when they measure a
role's notes. `neo4j_url` and `neo4j_api_key` are ignored with
`backend = "file"`, so they can stay in the file across a switch.
`bees notes show|edit|reset|add` are the exception: they always act on the
state directory's file, whatever `backend` is.

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
| `profile` | string | `""` | Profile name. Under `[global]`, the default profile for every role; under a role, that role's profile override. An empty value uses the implicit built-in profile. |
| `profile_by_size` | table | `{}` | Profile name per work item size, keyed by `xs`, `s`, `m`, `l` or `xl`. Accepted under `[global]` and every role. |
| `max_turns` | int | `200` | Agentic turns per session (`claude --max-turns`). `0` means the default. Codex and opencode have no such limit and a `codex` or `opencode` role ignores it. |
| `timeout` | duration | `"45m"` | Wall-clock limit for one session; the session's process group is killed when it expires. `"0s"` means the default. |
| `allowed_tools` | string list | `[]` | Passed as `claude --allowedTools`. A `codex` or `opencode` role ignores it. |
| `disallowed_tools` | string list | `[]` | Passed as `claude --disallowedTools`. A `codex` or `opencode` role ignores it. |
| `shell` | string | the shell bees runs under | Exported into sessions as `$SHELL`. Claude Code discovers its Bash tool's shell from `$SHELL`, so this is the lever, without being a guarantee. Must be an existing file. |
| `sandbox_image` | string | `""` | The image a `container` session runs in: it must hold the role's agent, `git` and `gh`. A `container` role without one or `container_use_environment` is refused at `bees run`. See [The container mode](#the-container-mode). |
| `container_use_environment` | string | `""` | Path, relative to the project repository root, to a `dagger/container-use` environment definition to build the `container` profile's image from, instead of `sandbox_image`. Requires the resolved profile's `sandbox = "container"` and is a load error together with `sandbox_image` on the same resolved role. See [Building from container-use](#building-from-container-use). |
| `env` | table | `{}` | Environment variables exported into every session: the agent, its shell tool and git see them, and so do MCP servers under `claude` and `opencode` (codex starts a server with only the variables its entry names). A `$VAR` value is expanded from the bees process environment when the session starts. A name may not be empty or contain `=` or a space. See [Exported into every session](#exported-into-every-session) for how it meets the variables bees sets itself. |
| `enabled` | bool | `true` | Roles only. `false` takes a role out of the rotation. Disabling `reviewer` makes a developer's pull request count as approved the moment it is opened, and with `auto_merge` it goes straight to the checks stage. Under `[global]` the key is an error. A named set of these decisions is a [config template](templates.md). |

## `[profiles.<name>]`

A profile bundles the agent, model, fallback model, effort and sandbox for a
session. Profiles are named globally, then selected by `[global]` or a role.
Values omitted from a profile use its built-in defaults; a profile configured
for `codex` or `opencode` has no default model or fallback model.

```toml
[global]
profile = "bar"

[profiles.foo]
agent = "claude"
model = "fable"
fallback_model = "opus"
effort = "max"
sandbox = "claude"

[profiles.bar]
agent = "opencode"
model = "ollama/qwen3.8:27b-mlx"
effort = "high"

[roles.developer]
profile = "foo"
profile_by_size = { xs = "bar", s = "bar", l = "foo", xl = "foo" }
```

| Key | Type | Default | Description |
|---|---|---|---|
| `agent` | string | `"claude"` | CLI a session runs as: `claude` (`claude -p`), `codex` (`codex exec`) or `opencode`. An unknown value is a load error. See [Running a session](architecture.md#running-a-session). |
| `model` | string | `"opus"` for `claude`, `""` otherwise | Model alias or full id, passed to the selected agent (`provider/model` for opencode). An empty value lets codex or opencode use its own configured model. |
| `fallback_model` | string | `"sonnet"` for `claude`, `""` otherwise | Passed as `claude --fallback-model` when it differs from `model`. Codex and opencode have no fallback-model flag. |
| `effort` | string | `""` | Passed as `claude --effort` when set: `low`, `medium`, `high` or `max`. Codex receives it as `model_reasoning_effort`; `max` maps to `high`. Opencode receives it as the default `build` agent's `variant`; variants are names the model defines, not levels. |
| `sandbox` | string | `"none"` | How much of the machine a session can reach: `none`, `claude` or `container`. See [Sandboxing](#sandboxing). |

The effective profile follows this order for a work item size:
role `profile_by_size[size]`, role `profile`, global
`profile_by_size[size]`, global `profile`, then the implicit built-in profile.
An unknown profile name or size is a load error.

### `[roles.reviewer]` only: checks and auto-merge

The reviewer owns the checks and the merge. These keys are accepted only under
`[roles.reviewer]`; under `[global]` or another role they are a load error.

| Key | Type | Default | Description |
|---|---|---|---|
| `auto_merge` | bool | `false` | Merge a pull request the reviewer approved once its checks are green. Off means people merge. A named set of these decisions is a [config template](templates.md). |
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

### `[roles.reviewer]` only: the review pipeline

The reviewer's review runs a brief, then one session per angle, then a
judge that merges what they found; these keys choose angles and execution
profiles for each step.
Like the checks keys, they are accepted only under `[roles.reviewer]`.

| Key | Type | Default | Description |
|---|---|---|---|
| `angles.<size>` | string list | the built-in list for that size | The angles a pull request of that size is reviewed from. `<size>` is `xs`, `s`, `m`, `l` or `xl`; one or more of `quick_general`, `general`, `docs`, `test_coverage`, `acceptance_criteria`, `side_effects`. An unknown size or angle, or an empty list, is a load error. A size this does not name keeps the built-in list. |
| `brief_profile` | profile name | size-resolved reviewer profile | Execution profile for the brief. Must use Claude or Codex; sandbox is ignored. |
| `angle_profiles.<angle>` | profile name | size-resolved reviewer profile | Execution profile for that angle. Must use Claude or Codex; sandbox is ignored. Unknown profile or angle names fail loading with the complete key path. |
| `judge_profile` | profile name | size-resolved reviewer profile | All five execution fields, including sandbox, for the reviewer session that posts findings and decides the verdict. |

| Size | Built-in angles |
|---|---|
| `xs`, `s` | `quick_general`, `docs` |
| `m`, `l` | `general`, `docs`, `test_coverage`, `acceptance_criteria` |
| `xl` | `general`, `docs`, `test_coverage`, `acceptance_criteria`, `side_effects` |

Phase overrides resolve through `[profiles.*]` after ordinary role and
`profile_by_size` selection. An unspecified angle uses the size-resolved
reviewer profile. Brief and angle sessions select `agent`, `model`,
`fallback_model` and `effort`, subject to backend support (fallback model is
Claude-only; Codex maps `max` effort to `high`). They ignore the profile's
`sandbox` and use the host adapter's mandatory read-only checkout policy:
no commands, writes, web/network tools, MCP, factory identity, or writable
or shared VCS access. Claude and Codex are the supported host agents.

The judge applies all five profile fields, including `sandbox`, through an
ordinary factory reviewer session. Its prompt, tools, permissions and ability
to post the verdict belong to the reviewer workflow. Profiles contain no
prompts, MCP, skills, environment, sandbox image, container environment,
shell, timeout, turn limit or VCS settings; those remain role/phase-owned.

An angle that fails is named in the judge's task and the rest are judged;
a review where the brief or every angle failed escalates the issue instead.
The standalone `bees review` configuration is separate and model-based; see
[Review configuration](review.md).

```toml
[profiles.review_quick]
agent = "claude"
model = "haiku"
effort = "low"

[profiles.review_judge]
agent = "claude"
model = "sonnet"
sandbox = "claude"

[roles.reviewer]
angles.xs = ["quick_general"]
brief_profile = "review_quick"
angle_profiles.docs = "review_quick"
judge_profile = "review_judge"
```

See [Review pipeline](roles.md#review-pipeline-rolesreviewerangles) for what
each angle looks at.

### `[roles.product_manager]` only: minimum issue size

This key describes the product manager, so it is accepted only under
`[roles.product_manager]`.

| Key | Type | Default | Description |
|---|---|---|---|
| `min_issue_size` | string | `""` | The smallest work item the product manager aims for when it breaks a feature down: `xs`, `s`, `m`, `l` or `xl`. Unset, it splits at whatever boundaries the work has. See [Sizing](workflow.md#sizing). |

```toml
[roles.product_manager]
min_issue_size = "m"    # fewer, larger work items per feature
```

It is a hint on the split, not a rule: nothing checks the size of an issue
that gets created, and the project manager still sizes a work item as it
finds it during triage. Work that is genuinely smaller still gets its own
issue, whoever files it, so a bug found mid-implementation is unaffected.

### `[roles.developer]` only: commit flags and max size

These keys describe the developer, so they are accepted only under
`[roles.developer]`.

| Key | Type | Default | Description |
|---|---|---|---|
| `commit_flags` | string | `""` | Extra flags for every `git commit` the developer makes, appended to its prompt verbatim. |
| `max_size` | string | `"l"` | The largest work item a developer takes: `xs`, `s`, `m`, `l` or `xl`. A `bees:ready` issue sized above it is moved back to `bees:triage` for the project manager to split; the project manager is told the limit. See [Sizing](workflow.md#size-decides-what-gets-built-next). |

```toml
[roles.developer]
commit_flags = "--gpg-sign --signoff"
max_size = "m"          # anything bigger goes back to triage to be split
```

Signing (`--gpg-sign`, `-S`) happens inside a headless session on the machine
running `bees`, so a signing key and agent must work for that user without a
prompt. `--signoff` needs `user.name` and `user.email`, as any
commit does.

### `[roles.developer]` only: best of N

Best of N runs several developer sessions on the same issue, each on its own
branch, and has an assembler session pick the pull request that goes to review.
It is off until a size names a count.

| Key | Type | Default | Description |
|---|---|---|---|
| `best_of_n_by_size` | table | `{}` | Attempts per work item size, keyed by `xs`, `s`, `m`, `l`, `xl`. An unknown key, or a value below `1`, is a load error. A size with no entry, and an issue with no size label, gets one attempt. |
| `best_of_n_model` | string | `""` | The model every attempt runs when a size fans out. Empty: an attempt resolves its model the way a single session does, through its selected `profile_by_size` or `profile`. |
| `best_of_n_prompt` | string | `""` | The prompt every attempt runs with. Empty: the developer's own `prompt`. |
| `assembler_model` | string | `""` | The model the assembler session runs. Empty: the developer's resolved profile model. |
| `assembler_prompt` | string | `""` | The prompt the assembler session runs with. Empty: the developer's own `prompt`. |

```toml
[roles.developer]
best_of_n_by_size = { l = 3 }   # three attempts on a large issue

best_of_n_prompt = """
Do not read the other attempts. Solve the issue your own way.
"""
```

An entry of `1` is one attempt, the same as leaving the size out, so a table
that names no size above `1` is best of N off. A size above `1` runs that many
developer sessions at once for the issue's first round, each on the branch
`<branch_prefix>issue-<n>-attempt-<i>` and each in a `max_developers` slot of
its own, so the count is clamped to `max_developers`. When every attempt has
ended, one assembler session runs on the issue's own branch with the attempt
branches listed in its task, puts the result there (one attempt as it stands,
or a synthesis) and opens the pull request from it, which goes to review as
any developer's does; the attempt branches are then deleted. An attempt that
pushed no commits is listed as not a candidate; when none did, the issue is
handed to a person, `bees:needs-human`.

### `[roles.developer]` only: mixture of experts

A mixture of experts runs several developer sessions on the same issue, each
one to a brief of its own, and has an assembler session combine their work
into the pull request that goes to review. It is off until a size names its
experts.

| Key | Type | Default | Description |
|---|---|---|---|
| `moe_experts_by_size` | table | `{}` | The experts a work item of that size fans out to, keyed by `xs`, `s`, `m`, `l`, `xl`, each an ordered list of names from `moe_experts`. An unknown size, an empty list, a name `moe_experts` does not define, or a size that is also in `best_of_n_by_size`, is a load error. A size with no entry, and an issue with no size label, runs a plain developer round. |
| `moe_experts` | table | `{}` | The experts themselves, one sub-table per name. `prompt` is what that expert's session runs with in place of the developer's own `prompt`, `model` the model it runs. Both are optional: an expert that names neither runs the developer's prompt and resolves its model the way a single session does, through its selected `profile_by_size` or `profile`. |
| `moe_assembler_model` | string | `""` | The model the assembler session runs. Empty: the developer's resolved profile model. |
| `moe_assembler_prompt` | string | `""` | The prompt the assembler session runs with. Empty: the developer's own `prompt`. |

```toml
[roles.developer]
moe_experts_by_size = { xl = ["backend", "frontend"] }
moe_assembler_model = "opus"

[roles.developer.moe_experts.backend]
prompt = """
Solve it in the server code.
"""
model = "opus"

[roles.developer.moe_experts.frontend]
prompt = """
Solve it in the interface.
"""
```

A size that names two or more experts runs that many developer sessions at
once for the issue's first develop round, one per name and in the order the
list gives them: expert `i` works on the branch
`<branch_prefix>issue-<n>-attempt-<i>`, with that expert's prompt and model,
in a `max_developers` slot of its own, so the count is clamped to
`max_developers` and the experts past the clamp do not run. A size that names
a single expert runs the plain developer session that leaving the size out
would run, so that expert's prompt and model go unused.

When every expert has ended, one assembler session runs on the issue's own
branch. Its task lists each expert branch and the expert it came from; it
combines their work into one implementation on that branch and opens the pull
request from it, which goes to review as any developer's does. The expert
branches are then deleted. An expert that pushed no commits is listed as not
a candidate; when none did, the issue is handed to a person,
`bees:needs-human`.

### Sandboxing

`sandbox` in a profile says how much of the machine a session can reach. Select
different profiles when roles that run untrusted code need a harder box than
roles that only read the repository:

```toml
[profiles.unboxed]
sandbox = "none"

[profiles.boxed]
sandbox = "container"

[roles.developer]
profile = "boxed"
```

| Mode | What a session can reach |
|---|---|
| `none` | Everything the user running `bees` can: the home directory, credentials, the network and every other checkout on the machine. |
| `claude` | Claude Code's own sandbox: writes to the worktree and the state directory, network to GitHub. See [The claude mode](#the-claude-mode) and [Security](security.md#claude). |
| `container` | A container holding the worktree, the repository's `.git` and the state directory, and nothing else of the host. See [The container mode](#the-container-mode) and [Security](security.md#container). |

`none` is the default. `bees run` checks before it starts that every role in
the rotation can have the box it asks for (the programs a `claude` box needs
on Linux, an agent that runs under it, and a `container` role's image,
credentials and engine) and refuses to start while one cannot, naming the
role: a factory that fell back to running that role unboxed would give it
exactly what it was configured to be kept away from. `bees exec` and
`bees tick` refuse the same session for the same reason.

`bees config show` prints the resolved mode per role, and
[`bees status`](cli.md#bees-status---json) the mode of the session each worker
is running right now.

See [Security](security.md) for what each mode protects and what it does not,
filesystem, network and credentials in turn, including what a sandboxed
session's own GitHub credentials mean for it.

#### The claude mode

A session in `claude` mode runs under
[Claude Code's sandbox](https://code.claude.com/docs/en/sandboxing), with a
settings block bees writes for it. Every shell command, and every process it
starts, runs inside a box the operating system enforces: Seatbelt on macOS,
bubblewrap on Linux. The built-in file and web tools run in the `claude`
process itself, outside that box, so Claude Code's permission rules hold them
to the same boundary, and anything those rules would ask a person about is
refused instead.

What the session can reach:

- **Writes**: the worktree, the state directory, the session's own temporary
  directory and the repository's shared `.git` directory, so `git commit`
  works in a linked worktree. Anything else is refused, `~/.zshrc` and a
  toolchain's cache under the home directory included, and Claude Code
  protects its own configuration inside the writable directories too
  (`.claude/`, `.mcp.json`, `.git/hooks`, `.git/config`), so a session cannot
  loosen its own box.
- **Reads**: everything the user running `bees` can read, credentials
  included. The mode fences writes and the network, not reads.
- **Network**: `github.com` and `*.github.com` only, from shell commands and
  from the `WebFetch` tool alike, so `gh` and `git push` over https work.
  Every other host is refused: a module proxy, a package registry, an ssh
  remote (no host name resolves inside the box), Docker and Dagger.
- **Tools**: the built-in `bees` MCP server and every MCP server of the role
  run on the host, outside the box, so the mail, issue, outcome and
  `gh`-backed tools work unchanged. The `[github]` token and the role's
  `env` reach the shell inside the box. `WebSearch` is refused.

On macOS nothing needs installing. On Linux the box needs `bubblewrap` and
`socat` on `PATH`, which `bees run` checks before it starts. The box is
Claude Code's, so a role whose resolved profile's `agent` is `codex` or
`opencode` cannot use it: `bees run` refuses to start, naming the role.
Commit signing
through `gpg` does not work inside the box, because `gpg` writes under
`~/.gnupg`.

To widen the box for one project, commit a `.claude/settings.json` to the
repository: Claude Code merges its `sandbox` lists with the ones bees writes.
A Go project on macOS needs its build cache and module proxy, for example:

```json
{
  "sandbox": {
    "filesystem": { "allowWrite": ["~/Library/Caches/go-build"] },
    "network": { "allowedDomains": ["proxy.golang.org", "sum.golang.org"] }
  }
}
```

The settings of the user running `bees` (`~/.claude/settings.json`) merge the
same way, and an `excludedCommands` entry there runs that command outside the
box.

#### The container mode

A `container` session is the agent's command line, unchanged, run inside
`docker run`. It needs `docker` on `PATH` with a daemon that answers, and the
image `sandbox_image` names on the machine: pull or build it yourself, bees
never pulls. `bees run` checks all three ahead of the doctor, whatever
`--skip-doctor` says, and refuses to start naming the role that fails. A role
with `container_use_environment` instead of `sandbox_image` has its image
built at session start, and `docker build` pulls the definition's
`base_image` then; see
[Building from container-use](#building-from-container-use).

The container sees three things of the host, each bind-mounted at its host
path so every path in the prompts and the environment means the same inside:
the worktree, the repository's `.git` (a linked worktree's `.git` file points
into it, and commits write there) and the state directory (mail, notes, the
session directory). A role with `skills` also gets the skills cache,
read-only. Nothing else: no home directory, no other checkout, no credential
store. The session runs as the user running `bees` (claude refuses to skip
permissions as root), with a `HOME` of its own on a tmpfs that is gone when
the session ends; a settings directory baked into the image is reached by
setting `CLAUDE_CONFIG_DIR` in the role's `env`.

Its environment is built from nothing rather than from the one `bees` runs
in, and holds, in this order: the agent's credential forwarded from the bees
environment when it is set there (`ANTHROPIC_API_KEY` or
`CLAUDE_CODE_OAUTH_TOKEN` for claude, `OPENAI_API_KEY` or `CODEX_API_KEY` for
codex), the role's `env` (which may name the credential itself), `SHELL`, the
`BEES_*` variables, the [`[github]`](#github) token and git identity, the git
configuration every session runs with plus `safe.directory = *` and an
`insteadOf` that pushes an ssh remote over https (the container has no ssh
keys), and `HOME`. Values reach the engine by variable name, never on a
command line. A `container` role therefore needs `[github]` (or `GH_TOKEN` in
its `env`) and a credential for its agent; `bees run` refuses the role
without them, because inside the container there is no other gh login and no
keychain.

The `bees` binary is not in the container. The built-in MCP server runs on the
host as `bees mcp serve --listen`, with the environment a session on the host
would have started it with, and the session reaches it over HTTP at
`host.docker.internal` with a bearer token of its own, passed through the
container environment. The backend configuration references that variable;
the token value stays out of configuration files and command arguments. The
tools work unchanged; the session's prompt does not offer the `bees` commands.
A stdio MCP server configured in `bees.toml` starts inside the container, so
its command must be in the image; a remote one is
reached as configured (`host.docker.internal` reaches a server on the host).

The image must hold the agent, `git` and `gh`. A Dockerfile that does:

```dockerfile
FROM node:22-bookworm
RUN apt-get update \
    && apt-get install -y --no-install-recommends git curl ca-certificates \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
       -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) \
       signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] \
       https://cli.github.com/packages stable main" \
       > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*
RUN npm install -g @anthropic-ai/claude-code
```

A role that runs the product (QA, most often) names an image carrying the
product's toolchain too, under `[roles.qa]`.

While the session runs, `<session>/container-id` holds the container's id
and `<session>/mcp-server-pid` the pid of the built-in MCP server bees
started on the host for it, and the container is named
`bees-<session>-<random>` and labelled
`bees.session=<session directory>`, so `docker ps --filter
label=bees.session` lists a factory's sessions. Stopping the session (its
timeout, or `bees run` stopping) removes the container. So does stopping it
from outside: [`bees kill`](cli.md#bees-kill---dry-run---scheduler---grace-5s)
and the live view's `k` key find a container session through that label and
remove its container, which is what stops the agent: it runs in the
container, not on the host, and killing the `docker run` client alone
leaves it running. They stop the MCP server too, from its pid file: it runs
on the host in a process group of its own, so a crash that takes bees down
would otherwise leave it holding its port and serving the factory's tools.

What the box holds: the session cannot read or write anything of the host
outside the three mounts, and cannot reach the host's credentials or its
other checkouts. What it does not: the session has the network, the bot's
GitHub token, the Neo4j Agent Memory API key with the `neo4j` notes backend,
its agent credential, and everything in the image; and the mounted state
directory holds every role's mail and notes, not only its own.
On Linux the container runs as the user running bees so what it writes stays
theirs, and reaches the host at the docker bridge gateway; the mode has been
exercised on macOS with Docker Desktop.

#### Building from container-use

`sandbox_image` is the way to run a `container` session: point it at an
image you built and published yourself, and bees runs it unchanged. This is
a niche opt-in for a project that already keeps a declarative, buildable
environment definition in the
[`dagger/container-use` format](https://github.com/dagger/container-use), and
would rather bees build from that than from a hand-built image. Most
projects should keep using `sandbox_image` instead.

Setting `container_use_environment` to a path builds the image from
`<path>/.container-use/environment.json` in the project repository, at
session start, instead of requiring `sandbox_image`. bees reads only that
file's format: it does not depend on `container-use`'s Go package, the
Dagger SDK, or a running Dagger engine. Of the definition's fields, bees
uses:

- `base_image`: the image the build starts `FROM`.
- `env`: `KEY=VALUE` entries, set before any command runs.
- `setup_commands`, then `install_commands`: one `RUN` each, in that order.
  `container-use` itself runs `setup_commands`, copies its environment's
  source tree in, then runs `install_commands`, so an install command there
  can see a checked-in file such as `package.json`. bees has no such copy
  step. The worktree is bind-mounted when the container runs, not baked into
  the image, so the two lists run back to back. An install command that
  depends on source files being present must tolerate their absence, or move
  to a role's own session-start mechanism instead.

`workdir`, `secrets` and `services` are read and ignored, without an error.
The container always runs at the worktree's host path regardless of
`workdir`. `secrets` names Dagger secret-provider URIs bees has no pipeline
to resolve, and `services` describes more than one container, which the
sandbox does not support.

The built image must still hold the agent, `git` and `gh`, the same
requirement a hand-built `sandbox_image` has. Providing them is the
definition's `setup_commands`/`install_commands` job. The image is tagged
after a hash of the resolved build inputs (`base_image`, `env`,
`setup_commands`, `install_commands`, in that order), so a definition whose
formatting changes but whose build inputs do not reuses the image already
built for it, and a change to any of those inputs builds a new one.
`docker build` runs once per definition, the first time a session needs it.
A missing or malformed `environment.json`, or a failing build, fails that
session with an error naming the path or the build command, not a
`bees.toml` load error.

### How global and role settings merge

| Setting | Rule |
|---|---|
| `prompt`, `prompt_file` | Concatenated, separated by blank lines: global `prompt`, global `prompt_file`, role `prompt`, role `prompt_file`. The result is appended to the role's built-in prompt under an "Additional instructions from bees.toml" heading, followed by the repository's [project prompt files](#project-prompt-files). |
| `skills` | Union, global first, order kept, duplicates dropped. |
| `mcp` | Union by name; a role server replaces a global one of the same name. |
| `env` | Union by name; the role wins. |
| `max_turns`, `timeout`, `shell`, `sandbox_image`, `container_use_environment` | Role value if set, else global, else the built-in default. |
| `profile`, `profile_by_size` | For a work item size: role `profile_by_size[size]`, role `profile`, global `profile_by_size[size]`, global `profile`, then the implicit built-in profile. |
| `allowed_tools`, `disallowed_tools` | Global list followed by the role list. |
| `enabled` | Role only. |
| `skills_refresh` | Global only. |
| `min_issue_size` | `roles.product_manager` only. |
| `commit_flags`, `max_size`, `best_of_n_by_size`, `best_of_n_model`, `best_of_n_prompt`, `assembler_model`, `assembler_prompt`, `moe_experts_by_size`, `moe_experts`, `moe_assembler_model`, `moe_assembler_prompt` | `roles.developer` only. |
| `auto_merge`, `merge_method`, `checks_wait`, `checks_poll_interval`, `checks_timeout`, `max_check_fix_rounds`, `pre_review_checks`, `pre_review_checks_timeout`, `angles`, `brief_profile`, `angle_profiles`, `judge_profile` | `roles.reviewer` only. `bees config show reviewer` prints the resolved policy. |

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
--rendered` has no worktree and reads the main checkout (`project.dir`, else
the checkout `bees.toml` sits in); it says so when it finds any.

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

A `claude` session is given the servers in a per-session `--mcp-config` file
loaded with `--strict-mcp-config`, so it sees the servers configured here plus
the built-in one, and none of your own. A `codex` session is given the same
servers as `-c mcp_servers.<name>.<key>=<value>` overrides on its command
line, next to whatever `~/.codex/config.toml` configures. Codex filters a
stdio server's inherited environment. Bees forwards the built-in server's
GitHub credentials and configured notes API key through `env_vars`, which
names variables to read from the session's environment. Their values stay
out of the generated MCP configuration and command arguments. Configured
servers receive their entry's `env` values. An `opencode` session is given
the same servers as the `mcp` table of a per-session `opencode.json`, handed to it
through `OPENCODE_CONFIG`, next to whatever its global configuration and
the project's own `opencode.json` name.

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
version = 4
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

[profiles.reviewer]
model = "sonnet"

[roles.developer]
commit_flags = "--signoff"

[roles.reviewer]
profile = "reviewer"
prompt = "Be strict about error handling and test coverage."
auto_merge = true
checks_timeout = "20m"

[roles.qa]
skills = ["https://github.com/anthropics/skills#skills/webapp-testing"]
timeout = "30m"
```

### Team repository, only work assigned to me

```toml
version = 4

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

The Claude Code check only runs when at least one enabled role resolves to an
agent profile whose `agent` is `"claude"`, the default (see
[`[profiles.<name>]`](#profilesname)). A factory where every enabled role
resolves to `"codex"` or `"opencode"` never runs it, so `claude` does not
need to be installed.

Set `BEES_SKIP_VERSION_CHECK=1` to run with an unsupported version anyway.

## Environment variables

### Honoured by the `bees` command

| Variable | Effect |
|---|---|
| `BEES_CONFIG` | Path of `bees.toml` when `--config` is not given. Set inside sessions. |
| `BEES_CLAUDE_BIN` | The `claude` executable to run. Default `claude` on `PATH`. |
| `BEES_CODEX_BIN` | The `codex` executable to run for a role whose `agent` is `codex`. Default `codex` on `PATH`. |
| `BEES_OPENCODE_BIN` | The `opencode` executable to run for a role whose `agent` is `opencode`. Default `opencode` on `PATH`. |
| `BEES_CACHE_DIR` | Cache directory for skill clones and generated plugins. Default `~/.cache/bees` on Linux, `~/Library/Caches/bees` on macOS. |
| `BEES_SKIP_VERSION_CHECK` | When non-empty, skip the `gh` and `claude` version checks. |
| `BEES_STATE_DIR` | `bees mail` and `bees notes` use this state directory without loading `bees.toml`, unless `--config` is given, in which case that file's state directory wins. Set inside sessions. |
| `BEES_SESSION_DIR` | Where `bees done` writes `outcome.json`; `bees done` refuses to run without it. Set inside sessions. |
| `BEES_ROLE` | Default `--from` of `bees mail send`, and the role whose `bees done` statuses are validated. Set inside sessions. |
| `BEES_ISSUE`, `BEES_PR` | Defaults for the `--issue` and `--pr` flags of `bees mail send` and `bees done`. Set inside sessions. |
| `BEES_LOG_FORMAT`, `BEES_LOG_LEVEL` | Fallbacks for `--log-format` and `--log-level`. A flag beats them, and they beat [`[logging]`](#logging). |

The variables marked *set inside sessions* are the only ones a session
inherits. `BEES_CLAUDE_BIN`, `BEES_CODEX_BIN`, `BEES_OPENCODE_BIN`,
`BEES_CACHE_DIR`, `BEES_SKIP_VERSION_CHECK`, `BEES_LOG_FORMAT` and
`BEES_LOG_LEVEL` configure
the `bees` process you start
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
| `BEES_BIN` | Path of the `bees` executable. Its directory is also prepended to `PATH`, so a session can run `bees mail` and `bees done`. Not set in a `container` session, which has no `bees` binary. |
| `BEES_REVIEW_MODE` | `checks` in a reviewer session that diagnoses failed checks; unset otherwise. |
| `SHELL` | The configured `shell`, when set. |
| `GH_TOKEN` | [`github.token`](#github), when set, so the session's `gh` acts as the factory. |
| *the variable `github.token` names* | When `github.token` is a `"$VAR"` reference, that variable, holding the resolved token. The `bees` commands a session runs load `bees.toml` themselves, and a reference that expands to nothing is a load error, so the name has to survive the `BEES_*` strip. It is set in the environment only; nothing writes it into the session directory. |
| *the variable `notes.neo4j_api_key` names* | The same, for a `"$VAR"` `notes.neo4j_api_key`, with `notes.backend = "neo4j"`: the value the scheduler resolved, so the session's own `notes_read` and `notes_write` can load `[notes]`. |
| `GIT_AUTHOR_NAME`, `GIT_COMMITTER_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_EMAIL` | `github.git_name` and `github.git_email`, when set. |
| `GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_n`, `GIT_CONFIG_VALUE_n` | `push.autoSetupRemote=true` and `push.default=current`, so a plain `git push` works on a fresh branch without touching the clone's git config; with `github.token` set, also an empty `credential.helper` followed by `credential.helper=!gh auth git-credential`, so an https push authenticates as the factory rather than through your stored credentials. Left alone when `GIT_CONFIG_COUNT` is already in the bees environment. |

`BEES_*` variables are always set by bees for each session and never inherited,
so a session started from inside another one, by a nested `bees run` or `bees
exec`, never sees a stale issue, PR or branch. The `BEES_*` names bees puts
back after the strip are the ones a `"$VAR"` `github.token` and a `"$VAR"`
`notes.neo4j_api_key` read.
