# Configuring busybees: `bees.toml`

Everything a busybees factory does is driven by one file, `bees.toml`, which lives in
the root of a git clone of the project being built. `bees init` writes a starter file in
the style of a classic Unix config: every option is listed, and the optional ones are
commented out showing their default value — uncomment a line to change it. Only
`filter.label` (plus `filter.assignee` when you pass `--assignee`, and `project.repo` /
`project.default_branch` when you pass `--repo`) start out active; `bees config validate` checks it; `bees config show` prints
the resolved settings for every role after merging.

The file starts with a `version` key, followed by five top-level tables:

| Table | Purpose |
|---|---|
| `[project]` | The git remote, repository, default branch and state directory |
| `[filter]` | Which GitHub issues and pull requests the factory can see |
| `[scheduler]` | Concurrency, polling and review-loop limits |
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
| `assignee` | string | `""` | Only see items assigned to this GitHub login. `"@me"` resolves to the authenticated `gh` user at startup. Everything the factory creates is assigned to this user so it stays visible. |
| `milestone` | string | `""` | Only see items in this milestone (by title). Also the fallback milestone for issues the factory creates when neither `--parent` nor `--related` supplies one (see `bees issue create`). Milestones themselves are managed by people, never by bees. |

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

The factory picks up any issue assigned to you, adds `bees` and `bees:triage` to it on
first sight, and assigns every PR or bug it creates back to you. Because state labels
are prefixed with `label`, several people can each run their own factory in the same
repository with different labels (`kyle-bees`, `sam-bees`) without interfering.

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
| `bees:triage` | Needs refinement by the project manager |
| `bees:ready` | Detailed enough for a developer to pick up |
| `bees:in-progress` | A developer worker owns it |
| `bees:blocked` | Waiting on an answer to a question |
| `bees:review` | Pull request open and under review |
| `bees:approved` | Reviewer approved; waiting for a human to merge |
| `bees:needs-human` | The factory gave up; a person must step in |

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

## `[scheduler]`

| Key | Type | Default | Description |
|---|---|---|---|
| `poll_interval` | duration | `"5m"` | How often GitHub is polled for work. Each poll costs two API calls (`gh issue list`, `gh pr list`); everything else is gated on what those lists report (see [API budget](#api-budget)). Also the minimum gap between two runs of the same singleton role. Keep it infrequent. |
| `rate_limit_backoff` | duration | `"15m"` | How long to pause polling after a poll fails with a GitHub rate-limit error (a message containing "rate limit", "secondary rate" or "abuse detection"), instead of retrying after `poll_interval`. |
| `max_developers` | int | `1` | Number of concurrent developer workers. Each worker owns one issue and runs a sequential developer → reviewer → developer loop, so reviewer concurrency follows developer concurrency. Must be ≥ 1. |
| `max_review_rounds` | int | `3` | Developer/reviewer iterations before an issue is escalated with `bees:needs-human`. |
| `retries` | int | `1` | Extra attempts a session gets when it failed for **infrastructure** reasons — it timed out, ran out of turns, hit an API error or rate limit, or `claude` crashed. A session that ran and reported (with `bees done`, including `failed`) is never retried. `0` disables retrying; must be between 0 and 5. See [Escalation](workflow.md#escalation-beesneeds-human). |
| `retry_delay` | duration | `"10m"` | How long to wait before an attempt is repeated. `"0s"` retries immediately. |
| `retry_with_fallback` | bool | `true` | Run the retry with the role's `fallback_model` as its primary model. Roles without a fallback model simply rerun. |
| `triage_batch_size` | int | `5` | Maximum number of issues handed to the project manager in one session. |
| `dispatch_order` | string | `"small-first"` | Which `bees:ready` issue a free developer takes next: `small-first` (smallest size first), `oldest` (whatever the size) or `large-first`. Ties are broken by age, oldest first; an issue without a size ranks as `m`. Issues already `bees:in-progress` or `bees:review`, and `bees:ready` issues that already have an open pull request, are resumed first and are never reordered. See [Sizing](workflow.md#size-decides-what-gets-built-next). |
| `max_large_in_flight` | int | `1` | How many `bees:size/l` issues developer workers may hold at once. A larger issue over the cap is skipped and the free worker takes the next issue that fits. `0` means no cap; must be ≥ 0. |
| `pr_fix_conflicts` | bool | `true` | Hand an open pull request that **conflicts** with the default branch back to its developer: the developer is mailed (from `orchestrator`) to merge the default branch, resolve, test and push, and an approved issue goes back to `bees:ready` ahead of new work. See [Conflicts with the default branch](workflow.md#conflicts-with-the-default-branch). |
| `pr_keep_updated` | bool | `false` | Do the same when a pull request is merely **behind** the default branch (it would merge cleanly, but was not tested against what is on the default branch now). |
| `product_manager_interval` | duration | `"1h"` | Minimum time between product manager runs. Unread mail in the PM's inbox triggers an earlier run. |
| `qa_interval` | duration | `"30m"` | Minimum time between QA runs. QA only runs when something was merged since its last run (the first run always happens). The merged-PR query itself runs at most once per `qa_interval` (tracked as `last_check` in `<state_dir>/qa.json`), not on every poll. |
| `keep_workspaces` | bool | `false` | Leave temporary worktrees on disk after a session (debugging). |
| `workspace_root` | string | `""` | Directory temporary worktrees are created under. Empty means `$TMPDIR/bees`. |
| `work_hours` | string | `""` | Daily window during which GitHub is polled every `poll_interval`, as `"HH:MM-HH:MM"` on a 24-hour clock. Empty (the default) disables the feature — GitHub is polled around the clock and the three keys below are ignored. See [Work hours](#work-hours). |
| `off_hours_poll_interval` | duration | `"1h"` | How often GitHub is polled outside `work_hours`. Must be ≥ `poll_interval`. Only used when `work_hours` is set. |
| `work_days` | list of strings | `["mon","tue","wed","thu","fri"]` | Days the window applies to, as lowercase three-letter names (`mon tue wed thu fri sat sun`). At least one is required when `work_hours` is set. |
| `timezone` | string | `""` | IANA name the window is read in (`"America/New_York"`). Empty means the machine's local time. |

Durations use Go syntax: `"30s"`, `"5m"`, `"1h30m"`.

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
`poll_interval`; a tick that is not due for a poll runs a *local pass* that
reuses the last poll's issue and PR lists (see
[The scheduler loop](architecture.md#the-scheduler-loop)). Everything driven by
the local mailbox — the developer ↔ reviewer loop, answered questions moving
`bees:blocked` back to `bees:ready`, the checks stage — runs at full speed at
every hour of the day. `bees tick` and `bees exec` ignore the window and always
do a full pass.

**Overnight windows.** When the start is later than the end, the window wraps
midnight and belongs to the day its **start** falls on: `work_hours =
"22:00-06:00"` with `work_days = ["fri"]` covers Friday 22:00 through Saturday
06:00, and nothing else. A window whose start equals its end is rejected.

Invalid values are rejected when `bees.toml` is loaded: a window that is not
`"HH:MM-HH:MM"` on a 24-hour clock, an unknown or empty `work_days`, a timezone
`time.LoadLocation` does not know, or an `off_hours_poll_interval` shorter than
`poll_interval`.

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
| Product-manager has-work check | 1 `issue view` per feedback/feature issue | only for issues whose `updatedAt` is newer than the PM's last run |
| Product-manager run | 1 `issue view` per open feedback/feature issue, plus 1 REST call per open feature (sub-issue progress) | every PM run (not gated by `updatedAt`) |
| QA merged-PR check | 1 call | at most once per `qa_interval` |
| Required checks (auto-merge) | 1 call per poll of the checks stage | every `roles.reviewer.checks_poll_interval` while waiting |
| Label backstop | 2 list calls | after every session |
| Feature progress | 1 REST call per open feature issue (`sub_issues_summary`) | per product manager run |
| Parent feature lookup | 1 GraphQL call per triage item, and 1 per developer session | per project manager run / developer session |
| `bees issue create --parent` | 3 calls (parent details, create, attach as sub-issue); `--related` 2; plain 1 | whenever a role files an issue |
| Worker stage transitions | a handful of `issue view` / `pr view` / `issue edit` calls | per transition |

An idle factory therefore costs two calls per poll. If GitHub does rate-limit
the process, polling pauses for `rate_limit_backoff` before trying again.

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
| `env` | table | `{}` | Environment variables exported into every session: inherited by `claude`, its Bash tool, MCP servers and git. `$VAR` references are expanded from the bees process environment when the session starts. `[roles.<name>.env]` entries are merged over `[global.env]` (role wins per key). bees' own `BEES_*` variables always win. `BEES_*` variables are always set by bees for each session and are never inherited from the process that started it, so a session started from inside another one (a nested `bees run` or `bees exec`) never sees a stale issue, PR or branch. |
| `enabled` | bool | `true` | **Roles only.** `false` takes the role out of the rotation. Disabling `reviewer` makes developer PRs count as approved as soon as they are opened (and, with `auto_merge`, go straight to the checks stage). |

### `[roles.reviewer]` only: auto-merge

The reviewer owns merging. These keys are accepted **only** under `[roles.reviewer]`;
setting them on `[global]` or another role is a validation error.

| Key | Type | Default | Description |
|---|---|---|---|
| `auto_merge` | bool | `false` | Merge a pull request the reviewer approved once its required checks are green. Off by default: humans merge. |
| `merge_method` | string | `"squash"` | `squash`, `merge` or `rebase` (`gh pr merge --<method> --delete-branch`). |
| `checks_wait` | duration | `"1m"` | How long to wait after approval before polling required checks, because some take a moment to report they have started. |
| `checks_poll_interval` | duration | `"2m"` | How often `gh pr checks --required` is polled while waiting for checks (one API call each). |
| `checks_timeout` | duration | `"30m"` | How long to wait for required checks to finish before escalating with `bees:needs-human`. |
| `max_check_fix_rounds` | int | `2` | Reviewer-diagnoses / developer-fixes iterations allowed when required checks fail, before escalating. |

With `auto_merge = true`, after approval the worker waits `checks_wait`, then polls
`gh pr checks --required` every `checks_poll_interval`. All green (or no required
checks at all) → merge. Any check fails → the reviewer gets a checks-mode session to
find the main error and mail it to the developer, the developer pushes a fix, and the
checks are polled again — up to `max_check_fix_rounds`. Still pending at
`checks_timeout`, or a merge that GitHub refuses (for example branch protection that
needs a human review) → `bees:needs-human`. See [workflow.md](workflow.md#merging).

### `[roles.developer]` only: commit flags and max size

These two keys describe the developer specifically, so they are accepted **only**
under `[roles.developer]`; setting either on `[global]` or another role is a
validation error.

| Key | Type | Default | Description |
|---|---|---|---|
| `commit_flags` | string | `""` | Extra flags for every `git commit` the developer makes, for example `"--gpg-sign --signoff"`. Appended verbatim to the developer's system prompt as "When creating git commits, always use the following extra flags: `--gpg-sign --signoff`." |
| `max_size` | string | `"l"` | The largest work item a developer takes: `xs`, `s`, `m`, `l` or `xl`. A `bees:ready` issue sized above it is never dispatched — the orchestrator moves it back to `bees:triage` and the project manager splits it. The project manager is told the limit in its prompt. See [Sizing](workflow.md#size-decides-what-gets-built-next). |

```toml
[roles.developer]
commit_flags = "--gpg-sign --signoff"
max_size = "m"          # anything bigger goes back to triage to be split
```

Signing (`--gpg-sign` / `-S`) happens inside a headless Claude Code session on the
machine running `bees`, so a working signing key and agent (gpg-agent, or an SSH
signing key with `gpg.format = ssh`) must be available to that user without a prompt.
`--signoff` needs `user.name` / `user.email` configured, as any commit does.

### How global and role settings merge

For each role the effective settings are computed from `[global]` and
`[roles.<name>]`:

| Setting | Rule |
|---|---|
| `prompt` / `prompt_file` | Concatenated in this order, separated by blank lines: global `prompt`, global `prompt_file`, role `prompt`, role `prompt_file`. The result is appended to the role's built-in base prompt under an "Additional instructions from bees.toml" heading. |
| `skills` | Union, order preserved, global first, duplicates dropped. |
| `commit_flags`, `max_size` | Developer only; not merged from `[global]`. |
| `env` | union; the role wins on a name conflict |
| `mcp` | Union by name. A role server with the same name as a global one replaces it. |
| `model`, `fallback_model`, `effort`, `max_turns`, `timeout` | Role value if set, else global value, else the built-in default. |
| `allowed_tools`, `disallowed_tools` | Global list followed by role list. |
| `enabled` | Role only. |
| `auto_merge`, `merge_method`, `checks_wait`, `checks_poll_interval`, `checks_timeout`, `max_check_fix_rounds` | `roles.reviewer` only; `bees config show` does not list them, they form the merge policy. |

`bees config show <role>` prints the result.

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

Anything else is an error. Generated wrappers are rebuilt on every session; clones are
reused.

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
| `scheduler.poll_interval` | `5m` |
| `scheduler.rate_limit_backoff` | `15m` |
| `scheduler.max_developers` | `1` |
| `scheduler.max_review_rounds` | `3` |
| `scheduler.retries` | `1` |
| `scheduler.retry_delay` | `10m` |
| `scheduler.retry_with_fallback` | `true` |
| `scheduler.triage_batch_size` | `5` |
| `scheduler.dispatch_order` | `small-first` |
| `scheduler.max_large_in_flight` | `1` |
| `scheduler.pr_fix_conflicts` | `true` |
| `scheduler.pr_keep_updated` | `false` |
| `scheduler.product_manager_interval` | `1h` |
| `scheduler.qa_interval` | `30m` |
| `scheduler.work_hours` | `""` (poll around the clock) |
| `scheduler.off_hours_poll_interval` | `1h` |
| `scheduler.work_days` | `["mon","tue","wed","thu","fri"]` |
| `scheduler.timezone` | `""` (the machine's local time) |
| `model` | `opus` |
| `fallback_model` | `sonnet` |
| `max_turns` | `200` |
| `timeout` | `45m` |
| `roles.reviewer.auto_merge` | `false` |
| `roles.reviewer.merge_method` | `squash` |
| `roles.developer.commit_flags` | `""` (none) |
| `roles.developer.max_size` | `l` |
| `skills_refresh` | `24h` |
| `roles.reviewer.checks_wait` | `1m` |
| `roles.reviewer.checks_poll_interval` | `2m` |
| `roles.reviewer.checks_timeout` | `30m` |
| `roles.reviewer.max_check_fix_rounds` | `2` |

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

### Exported into every session

Sessions run with the `bees` process environment plus:

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
| `BEES_REVIEW_MODE` | `checks` for the reviewer's checks-mode sessions (diagnosing failed required checks); unset otherwise. |
| `SHELL` | The configured `shell`, when set. |
| *configured `env`* | Every `[global.env]` / `[roles.<name>.env]` entry, `$VAR`-expanded from the bees environment. Set before the `BEES_*` variables, so those always win. |
| `GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_0`/`VALUE_0`, `GIT_CONFIG_KEY_1`/`VALUE_1` | `push.autoSetupRemote=true` and `push.default=current`, so a plain `git push` works on a fresh branch without touching the clone's git config. Only set when `GIT_CONFIG_COUNT` is not already in the bees environment. |
