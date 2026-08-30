# Roles

A busybees staff has five roles. Each session is a fresh `claude -p` run with
a role-specific system prompt, so a role has no memory beyond its notes file
and what is visible in GitHub. This page describes what each role is given,
what it may do, and how to shape it.

## Concurrency model

| Role | Instances | Runs when |
|---|---|---|
| `product_manager` | singleton | unread mail, a fresh `bees:feedback` or `bees:feature` issue (a person created or commented on it since the PM last replied), or `product_manager_interval` elapsed (first run immediately) |
| `project_manager` | singleton | issues in `bees:triage`, or unread mail |
| `developer` | pool of `scheduler.max_developers` workers | a `bees:ready` issue is waiting (or an in-progress/review issue needs resuming); a ready issue whose PR came back — human feedback, a conflict with the default branch — goes before new work |
| `reviewer` | one per developer worker, in sequence | the worker's developer session opened or updated a PR; with `auto_merge`, also when a required check fails after approval |
| `qa` | singleton | `qa_interval` elapsed and something was merged (first run immediately; the merged-PR check runs at most once per `qa_interval`) |

A developer worker owns one issue at a time and runs a strictly sequential
developer → reviewer → developer … loop for it. Reviewer concurrency therefore
follows developer concurrency: with `max_developers = 3` there can be at most
three reviewer sessions running, never two for the same PR. A singleton never
runs two sessions at once, and no singleton re-runs faster than
`scheduler.poll_interval` (default 5m — polling is deliberately infrequent to
stay well inside GitHub's API limits; see the API budget in
[configuration.md](configuration.md#api-budget)).

Roles report how a session ended with the `done` tool; the orchestrator
treats a session that ends without an outcome, times out, or exits with an
error as `failed`.

## Common ground

Every role's system prompt starts with the same preamble (see
`internal/prompts/system/common.md`): what busybees is (and that the product's
purpose lives in the repository, not the prompt), the visibility filter, the label
table, how the mailbox works, how to report an outcome, the notes file, the
`gh` CLI, environment details (repository, working directory, branch, state
directory, session directory), how to learn the project, the comment marker
rule, and the ground rules: stay in your role, do not merge, do not push to the
default branch, do not remove the `bees` label, write for a human reader, and
be honest in the outcome note.

**Learning the project.** busybees tells roles nothing about how to build,
test or run the product; that knowledge belongs to the repository. Roles read
its README, CONTRIBUTING, CLAUDE.md, Makefile and CI config, and record the
commands and gotchas in their notes file.

**Comment marker.** Humans and bees share one GitHub account, so every comment
a role posts on GitHub must end with the line `<!-- bees:<role> -->` (invisible
when rendered). The orchestrator uses it to tell bee comments from human ones
when it collects PR feedback for the developer.

Roles interact with GitHub through the already-authenticated `gh` CLI, and
with each other only through the local mailbox (the `mail_send` tool). Every
issue or PR a role creates must match the filter: issues are created with the
`issue_create` tool, which applies the filter label and assignee itself, and
for `gh pr create` the prompts include the exact flags to pass (`--label "bees"`
plus `--assignee` when configured).

`mail_send`, `mail_list`, `issue_create`, `issue_link` and `done` are MCP tools
served to every session by the built-in `bees` server, so a role calls them with
arguments instead of composing a command line; the equivalent `bees` commands
(`bees mail send`, `bees issue create`, `bees issue link`, `bees done`) still
exist and behave identically. See [cli.md](cli.md#bees-mcp-serve-sessions).

### Notes files

`<state_dir>/notes/<role>.md` is a role's only long-term memory. Its current
contents are included in every task prompt and the role is told to update it
before finishing: decisions, conventions, gotchas, what it tested. The file is
created on the role's first run with a `# <role> notes` heading and the four
sections roles are asked to keep their notes under: **Project facts** (how to
build, test and run the project), **Conventions**, **Decisions** and **Open
questions**. Anything that does not fit goes under a heading of the role's
choosing.

Nothing else curates the file, so the scheduler asks the role to do it: every
`scheduler.notes_consolidate_every` sessions (default 10), or earlier once the
file grows past `scheduler.notes_max_bytes` (default 32768), the task prompt
gains a paragraph asking the session to rewrite its notes into those sections —
merge duplicates, drop what is stale or contradicted, keep decisions, commands
and gotchas — on top of its normal work. Nothing is truncated or rewritten
behind the role's back, and the counters live in `<state_dir>/<role>.json`.

**Editing a notes file is the most direct way to steer a role.** Write the
product vision into `notes/product_manager.md`, coding conventions into
`notes/developer.md`, or "always run the e2e suite" into `notes/reviewer.md`,
and the next session reads it. Developer workers share a single
`notes/developer.md`.

[`bees notes`](cli.md#notes) is the way to do that without hunting for the
file: `bees notes show <role>` prints it, `bees notes edit <role>` opens it in
your editor, `bees notes add <role> "..."` appends one bullet, and
`bees notes reset <role>` archives it and starts a fresh one when a role has
accumulated advice that no longer applies. `bees status` shows how big each
file has grown.

## product_manager

Owns the *what* and the *why*: vision and feature issues, and turns them into
work items for the project manager. Milestones are not its job (see below).

**Given:** open milestones, read only (number, title, open/closed counts,
description); the **fresh feature issues** (full body and every comment); a
table of *all* open feature issues (milestone, sub-issue **progress** as
`completed/total` from GitHub's `sub_issues_summary`, whether it is waiting on
a person, title);
every open **work item** (state, kind, milestone, title — feature and
feedback issues are excluded from this table); open PRs; the **fresh
`bees:feedback` issues** (full body and every comment); unread mail; its
notes. It works from a detached checkout of the default branch and is told to
read the codebase and README to understand what exists.

**Does on GitHub:** creates feature issues with `issue_create` (`feature: true`
→ `bees` + `bees:feature` + `bees:proposal`, no state label; `related: <feedback issue>` when it
comes from feedback, so the milestone is inherited), describing user-visible
outcomes rather than implementation; creates work items as sub-issues with
`issue_create` (`parent: <feature>`); attaches existing issues with
`issue_link`; comments on feature and feedback issues; adds
`bees:question`; closes feature issues that are done or no longer make sense,
with a comment. It **never creates, edits or closes milestones** — people
manage those; the product manager reads them as a priority signal and, if it
thinks one is wrong, says so in a reply instead of acting. It is told to
search before creating and to keep the backlog small.

**Feature issues:** a `bees:feature` issue is the product manager's from idea
to shipped, whether it wrote it or a person filed it. Feature issues never
enter the workflow state machine. For each fresh one it:

1. makes sure the issue is detailed enough to be broken down;
2. if only a person can decide something, posts the question as a comment
   (ending with the `<!-- bees:product_manager -->` marker), adds the
   `bees:question` label, and stops working on that feature;
3. otherwise breaks it into work items — one issue per pull-request-sized
   piece, created with `issue_create` (`parent: <feature>`, `bug: true` for
   bugs), which makes each a native GitHub **sub-issue** of the feature with
   `bees` + `bees:triage` and the feature's milestone (it may pre-size one
   with `--label "bees:size/s"`, a hint the project manager confirms during
   triage — see [Sizing](workflow.md#sizing)); ordered, with
   dependencies expressed as `blocked_by: [<issue>]` (a `Blocked by #N` line the
   scheduler honours, see [Dependencies](workflow.md#dependencies)) rather than
   prose — then comments the list of work items on the feature
   issue (with the marker) so it is not re-presented until something changes;
4. closes the feature issue when all its sub-issues are closed (the progress
   column in its prompt shows this), or when it no longer makes sense.

A feature issue is *fresh* when a person created or commented on it after
the product manager's last marker comment (`github.Issue.AwaitingBee`). When a
person answers a `bees:question`, the orchestrator removes the label and the
issue comes back as fresh; a fresh feature or feedback issue triggers a
product manager run regardless of `product_manager_interval`.

**Feedback from people:** issues labelled `bees:feedback` are the product
manager's inbox (feature ideas, product feedback, bug reports from humans);
they never enter the workflow state machine. For each fresh one the product
manager decides and acts (feature issues, bug work items, or a reasoned no), then must **reply on the feedback issue** with a comment saying
what it did and linking created issues, ending with the marker. It closes the
issue when fully actioned, or asks the person a question (comment +
`bees:question`) and leaves it open. Freshness works exactly as for feature
issues.

**Mail:** receives questions from the project manager and reports from QA;
may send to `project_manager` only. It is told to be decisive because the
project manager is blocked until it answers.

**Outcomes:** `done` (with a summary), `idle`, `failed`. The orchestrator
records the run time (which starts the `product_manager_interval` clock) and
marks the delivered mail read. `failed` logs an error and backs the role off
for five poll intervals.

## project_manager

Turns ideas into work a developer can execute without guessing, and keeps
developers unblocked.

**Given:** up to `scheduler.triage_batch_size` (default 5) **work items** in
`bees:triage`, oldest first, with full bodies and comments; unread mail; a
table of all other visible issues; open PRs; its notes. Work items usually
come from the product manager breaking a feature issue down; they are GitHub
sub-issues, and each triage item's **parent feature** (number and title,
looked up with one GraphQL call) is shown in the prompt. The project manager
is told to read the parent feature for context. The prompt also shows the open
[blockers](workflow.md#dependencies) each work item declares — on the triage
header line and as a **Blocked by** column of the other-issues table.

**Does on GitHub:** rewrites triage work items (context, scope, acceptance
criteria, pointers to code, testing expectations) with `gh issue edit`,
keeping the author's intent; splits oversized work items with
`issue_create` (`ready: true`, `parent: <feature>`, or `related: <original>` when
there is no parent feature), closing the original with a comment; moves
refined work items `bees:triage` →
`bees:ready` with exactly one **size label** (`bees:size/xs` … `bees:size/l`)
added in the same edit, splitting anything that sizes up above
`roles.developer.max_size` (default `l`) instead of labelling it — the
orchestrator sends such an issue straight back to `bees:triage`; moves a work
item to `bees:blocked` when it has asked the product manager; closes invalid or duplicate work items with a comment. It
never edits feature or feedback issues — those belong to the product
manager — and never touches milestones. It is the only role besides the
orchestrator that moves state labels. It is told to declare dependencies with a
`Blocked by #N` line and move the item to `bees:ready` anyway rather than
parking it in triage: the scheduler holds it back until the blocker closes.

**Mail:** receives developer questions; may send to `product_manager`
(product decisions) and `developer` (answers, always with `issue`). Prompts
tell it to answer mail first, since developers are blocked on it, and to give
decisions rather than options.

**Outcomes:** `done`, `idle`, `failed`. The orchestrator marks its mail read
and records the run. Answers it sent take effect on the next poll: an issue
in `bees:blocked` with unread developer mail is relabelled `bees:ready`.

## developer

Implements exactly one issue on a dedicated branch and opens a pull request.

**Given:** the issue (body, labels, milestone, comments) and its parent
feature (number and title) when it is a sub-issue, the existing PR if this is
a later review round, unread mail addressed to the developer about
this issue or PR (project manager answers, reviewer feedback, feedback
from people who reviewed the PR on GitHub, delivered as mail from `human`
with comment ids and the exact `gh` reply commands, and — from
`orchestrator` — a request to bring the branch up to date when the PR
[conflicts with the default branch](workflow.md#conflicts-with-the-default-branch)),
the round number and limit, its notes. It runs in a worktree on `bees/issue-N` (prefix
from `project.branch_prefix`), already based on the default branch. The session
environment carries `push.autoSetupRemote=true` and `push.default=current`
(via `GIT_CONFIG_*` variables, so the clone's own git config is untouched) and a
plain `git push` just works.

**Does on GitHub:** pushes the branch; opens the PR with `gh pr create` (body
must contain `Closes #N`, a summary, and how it was tested) or updates the
existing one; files out-of-scope bugs with
`issue_create` (`bug: true`, `related: <issue>` — a `bees:bug` work item in triage,
in the same milestone as the issue it was working on); replies
on GitHub to every human review comment it addresses (`gh api
repos/<repo>/pulls/<pr>/comments/<id>/replies` for inline comments, `gh pr
comment` for reviews and conversation comments), ending each reply with
`<!-- bees:developer -->`. When a human's request conflicts with the issue or
the reviewer, the human wins. It must not change labels, must not push to the
default branch, and must not fix unrelated bugs.

**Mail:** may send to `project_manager` only. If the issue is too vague it is
told to ask one precise question and stop rather than guess.

**Outcomes and what the orchestrator does:**

| Status | Orchestrator |
|---|---|
| `pr-opened` (with `pr`) | Locates the PR (by number, else by branch), labels it `bees` (+ assignee), records it, moves the issue to `bees:review`, runs the reviewer. If the PR cannot be found: escalate. |
| `pr-updated` (with `pr`) | Same as above; used after addressing review feedback. |
| `question` | Verifies a message to the project manager was actually sent during the session, then labels the issue `bees:blocked` and frees the worker. No message: escalate. |
| `failed` (or no outcome / timeout / error) | Escalates to `bees:needs-human` with the note. |

If the reviewer role is disabled, `pr-opened`/`pr-updated` go straight to
approved (and, with `roles.reviewer.auto_merge`, into the checks stage).

## reviewer

Reviews one pull request and decides whether it is mergeable. It also owns
merging: `auto_merge` and its companions live under `[roles.reviewer]` (see
[configuration.md](configuration.md#rolesreviewer-only-auto-merge)).

**Given:** the PR (title, body, branch, author), the linked issue, the
issue's **size** and a sentence on the scrutiny it warrants (see
[Sizing](workflow.md#sizing)), its own
feedback from previous rounds, the round number and limit, its notes. It runs
in the same worktree as the developer for that issue, fast-forwarded to the
latest push, so it can run the tests and exercise the change.

**Does on GitHub:** reads (`gh pr diff`); files unrelated bugs with
`issue_create` (`bug: true`, `related: <issue>`, inheriting the issue's milestone).
It does **not** submit a GitHub review, push to the branch, or change labels.

**Mail:** may send to `developer` only, with `pr` and `issue`, one
consolidated message per round listing every point with file/line and the
expected change.

**Outcomes and what the orchestrator does:**

| Status | Orchestrator |
|---|---|
| `approved` | Labels the PR and the issue `bees:approved`. Without `auto_merge` the worker is freed and a human merges. With `auto_merge` the worker enters the checks stage (below). |
| `changes-requested` | Verifies feedback mail to the developer was sent during the session (none: escalate). If the round limit is reached: escalate. Otherwise increments the round, moves the issue back to `bees:in-progress` and runs the developer with the feedback. |
| `failed` (or no outcome / timeout / error) | Escalates to `bees:needs-human`. |

The reviewer is told when it is on the final round so it still requests
changes honestly and lets the orchestrator escalate.

### Checks mode (`auto_merge = true`)

After approval the orchestrator waits `checks_wait`, then polls the PR's
required checks. If they all pass (or there are none) it merges with
`merge_method` and deletes the branch. If any fails, the reviewer gets a
second kind of session, rendered from `task/reviewer_checks.md` with
`BEES_REVIEW_MODE=checks` in its environment.

**Given:** the PR, the issue, the list of failing required checks (name,
workflow, bucket, description, details link), the fix round and its limit,
its notes.

**Does:** finds the cause without assuming a CI system — follows the details
link, runs `gh pr checks`, reads the repository's docs and its own notes, uses
`gh run view --log-failed` only when the link is a GitHub Actions run, and
otherwise reproduces the failure locally on the branch. It records how to
read this project's CI in its notes. It then distils the main error, its
cause and a reproducing command into one mail to the developer.

| Status | Orchestrator |
|---|---|
| `changes-requested` | Verifies the mail was sent (none: escalate), then runs the developer, whose `pr-updated` leads straight back to polling the checks — no second full review. |
| `approved` | Means "I re-ran the check; wait again": the orchestrator waits `checks_wait` and polls once more. |
| `failed` (or no outcome / timeout / error) | Escalates to `bees:needs-human`. |

Fix rounds are counted in `<state_dir>/issues/<n>.json` (`check_fix_rounds`)
and capped by `max_check_fix_rounds`; checks still pending at
`checks_timeout`, or a merge GitHub refuses, also escalate.

## qa

Tests the product as a user would, from the default branch, and turns what it
finds into bug reports and product feedback.

**Given:** when it last ran, the PRs merged since then (title, body, closing
issues), the open `bees:bug` issues (to avoid duplicates), its notes. It runs
in a detached checkout of the default branch and works out from the
repository's documentation (and its notes) how to install, test and launch
the application.

**Does on GitHub:** files bug issues with
`issue_create` (`bug: true`, `related: <issue the merged PR closed>` → `bees` +
`bees:bug` + `bees:triage`, in that issue's milestone; `related` is omitted
when the bug is not tied to a recent change), with reproduction steps,
expected vs actual behaviour and severity, after searching for existing
reports; comments on an existing report rather than duplicating it.

**Mail:** may send to `product_manager` only: one report per session (what
was tested, what works, bugs filed, product-level observations), and nothing
if there is nothing to say.

**Outcomes:** `done` (with a summary), `failed` (could not test, with why).
The orchestrator records the run time either way; `failed` backs QA off for
five poll intervals.

## Customising a role

Everything is in `bees.toml`. `[global]` applies to every role and
`[roles.<name>]` layers on top of it:

```toml
[global]
prompt = "Follow CONVENTIONS.md. Prefer small PRs."
skills = ["https://github.com/acme/skills#skills/repo-conventions"]
model = "opus"
fallback_model = "sonnet"
shell = "/bin/bash"
[global.env]
CI = "true"

[roles.reviewer]
prompt = "Block any PR that lowers test coverage."
model = "sonnet"
max_turns = 60

[roles.developer]
skills = ["https://github.com/acme/skills#skills/tdd"]
commit_flags = "--gpg-sign --signoff"
[roles.developer.mcp.playwright]
command = "npx"
args = ["-y", "@playwright/mcp"]

[roles.qa]
prompt_file = "docs/qa-checklist.md"
```

- **prompt / prompt_file** are appended to the role's base prompt under an
  "Additional instructions from bees.toml" heading, in the order global
  prompt, global file, role prompt, role file. `bees prompts show <role>`
  prints the base prompt; `--rendered` shows the whole thing for this project.
- **skills** are unioned (global first) and exposed to the session as plugin
  directories.
- **mcp** servers are unioned; a role's server replaces a global one with the
  same name.
- **model / fallback_model / effort / max_turns / timeout /
  allowed_tools / disallowed_tools** fall back to `[global]`, then to the
  built-in defaults. `fallback_model` is used automatically when the primary
  model has reached its usage limit.
- **commit_flags** (developer only) are extra flags for every `git commit`
  the developer makes, e.g. `"--gpg-sign --signoff"`. They are appended to
  the developer's system prompt as "When creating git commits, always use the
  following extra flags: `--gpg-sign --signoff`." Setting the key on another
  role or `[global]` is a validation error. GPG or SSH signing runs inside a
  headless session on the machine running `bees`, so the signing key and agent
  must work there without prompting.

See [configuration.md](configuration.md) for every key.

## Disabling a role

```toml
[roles.qa]
enabled = false
```

A disabled role is never dispatched. The effects worth knowing:

- **reviewer disabled:** a PR is treated as approved the moment the developer
  reports `pr-opened`/`pr-updated`.
- **project_manager disabled:** issues stay in `bees:triage` (and blocked
  developer questions are never answered) unless humans label issues
  `bees:ready` and answer mail themselves.
- **product_manager disabled:** no feature issues or work items are created
  from features and feedback;
  QA reports and project manager questions pile up unread in the mailbox.
- **developer disabled:** nothing is built; the reviewer is never run either.

`bees run --roles developer,reviewer` restricts a single run in the same way
without editing the file.
