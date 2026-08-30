# `bees` command reference

`bees` is a single binary. Most commands need a `bees.toml`; it is found with
`--config`, then `$BEES_CONFIG`, then by searching upwards from the current directory.

Four commands — `bees mail send`, `bees issue create`, `bees issue link` and
`bees done` — are designed to be run **by Claude Code sessions** from inside the
factory (people can use them too). Everything else is for humans.

## Global flags

| Flag | Description |
|---|---|
| `-c, --config <path>` | Path to `bees.toml`. Default: `$BEES_CONFIG`, else search upwards from cwd. |
| `-v, --verbose` | Debug logging. With `run`/`tick`/`exec`, also streams every claude event to stderr. |
| `-h, --help` | Help for any command. |

## Setting up

### `bees init`

Creates `bees.toml` in the current directory (which must be a git clone of the
project), creates the state directory, adds it to the repository's `.gitignore`
(unless `git check-ignore` says it is already ignored, or it lives outside the clone)
and prints a reminder to commit that, and creates the workflow labels in the GitHub
repository. Refuses to overwrite an existing file. `bees.toml` is meant to be committed.

| Flag | Description |
|---|---|
| `--remote name` | Git remote the factory pushes to (default `origin`). |
| `--repo owner/name` | Write `project.repo` and `project.default_branch` as active settings. By default both are derived from the remote at run time and only appear as commented placeholders showing the detected values. |
| `--label <name>` | Visibility label (default `bees`). |
| `--assignee <login>` | Only see items assigned to this login; `@me` for yourself. |
| `--print` | Print the template to stdout instead of writing it. |
| `--no-labels` | Skip creating GitHub labels. |

The generated file lists every option; optional ones are commented out with their
default values (`#max_developers = 1`), so configuring is a matter of uncommenting and
editing lines. See [configuration.md](configuration.md).

```sh
cd ~/src/my-project
bees init
bees init --assignee @me --label kyle-bees
bees init --print > bees.example.toml
```

### `bees labels sync`

Creates or updates every workflow label in the repository (idempotent). Run it after
changing `filter.label`.

### `bees labels list`

Prints the label names and what each one means.

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
model, limits and `enabled` after merging `[global]` with `[roles.<name>]`.

```sh
bees config show
bees config show developer
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
reconciles labels (new issues enter triage, answered questions unblock), hands ready
issues to free developer workers and starts the product manager, project manager and
QA when they have work. Ctrl-C stops polling and waits for running sessions to
finish.

| Flag | Description |
|---|---|
| `--once` | Do one pass and exit when the sessions it started finish. Same as `bees tick`. |
| `--roles a,b` | Only run these roles (aliases accepted: `pm`, `pjm`, `dev`, `reviewer`, `qa`). |

```sh
bees run
bees run --roles dev,reviewer
bees -v run --once
```

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
| `reviewer` | `--issue N` or `--pr N` (the PR's closing issue is used). Moves the issue into review and runs the loop from the review stage. |

```sh
bees exec pjm
bees exec developer --issue 12
bees exec reviewer --pr 34
```

### `bees status [--json]`

Shows the last poll time and PID of the scheduler, queue sizes per workflow state
(plus `feedback` and `features`, the open `bees:feedback` and `bees:feature` issues
owned by the product manager, and `open_prs`), running developer workers (issue, stage, round, and the attempt number while a session is being retried), singleton state and last run, and
unread mail per role. Reads `status.json` from the state directory, so it works while
`bees run` is active in another terminal.

When [`scheduler.work_hours`](configuration.md#work-hours) is configured it also
reports whether the factory is inside the window and when the next GitHub poll is
due (`in_work_hours` and `next_poll` in `--json`):

```
work hours: yes (09:00-18:00 mon-fri, America/New_York)   next GitHub poll in 2m55s
```

The yes/no is computed when you run the command, so it is right even when the
scheduler is stopped; `in_work_hours` in `--json` is the scheduler's own record
from its last pass.

## The mailbox

Roles talk to each other only through the local mailbox in `<state_dir>/mail`. The
scheduler delivers messages by including them in the prompt of the session working on
the referenced issue or PR and marks them read afterwards.

### `bees kill [--dry-run] [--scheduler] [--grace 5s]`

Cleans up after a crash: finds Claude Code sessions started by bees, terminates them
together with their process groups (MCP servers, shells), removes stale pid files,
removes the temporary worktrees bees created under the workspace root, and resets the
worker list in `status.json`.

Sessions are found two ways: the `pid` file each running session keeps in its
`<state_dir>/sessions/<id>/` directory, and a scan of the process table for `claude`
processes carrying the `--name bees-…` argument every session is started with. Pid files
are cross-checked against the process table, so a pid reused by an unrelated process
after a reboot is discarded, never killed.

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
| `--feature` | Feature issue for the product manager (`bees:feature`, no state label). |
| `--ready` | Work item is already detailed: `bees:ready` instead of `bees:triage`. |
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
```

### `bees issue link --parent N --child M`

Attaches existing issue `M` as a sub-issue of feature `N` (for example a bug filed by
QA that turns out to belong to a feature in progress).

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

## Misc

### `bees version`

Prints the version.

### `bees completion <shell>`

Generates shell completion scripts (bash, zsh, fish, powershell).
