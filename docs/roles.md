# Roles

A busybees staff has five roles. Each session is a fresh `claude -p` run with
a role-specific system prompt, so a role has no memory beyond its notes file
and what is visible in GitHub. This page describes what each role is given,
what it may do, and how to shape it.

## Concurrency model

| Role | Instances | Runs when |
|---|---|---|
| `product_manager` | singleton | unread mail, a fresh `bees:feedback` or `bees:feature` issue (a person created or commented on it since the PM last replied — on a `bees:proposal` or `bees:planning` issue only a comment counts), a feature whose sub-issues have all closed, or `product_manager_interval` elapsed (first run immediately) |
| `project_manager` | singleton | issues in `bees:triage`, or unread mail |
| `developer` | pool of `scheduler.max_developers` workers | a `bees:ready` issue is waiting (or an in-progress/review issue needs resuming); a ready issue whose PR came back — human feedback, a conflict with the default branch — goes before new work |
| `reviewer` | one per developer worker, in sequence | the worker's developer session opened or updated a PR; with `auto_merge`, also when a required check fails after approval |
| `qa` | singleton | unread mail, or `qa_interval` elapsed and something was merged (first run immediately; the merged-PR check runs at most once per `qa_interval`) |

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
purpose lives in the repository, not the prompt), the visibility filter, the
label table, that anything a person writes — in an issue, in a pull request or
in mail from `human` — is authoritative and outranks the prompt, how the
mailbox works, how to report an outcome, the notes file, the `gh` CLI,
environment details (repository, working directory, branch, state directory,
session directory), how to learn the project, the comment marker rule, and the
ground rules: stay in your role, do not merge, do not push to the
default branch, do not remove the `bees` label, write for a human reader, and
be honest in the outcome note.

**Learning the project.** busybees tells roles nothing about how to build,
test or run the product; that knowledge belongs to the repository. Roles read
its README, CONTRIBUTING, CLAUDE.md, Makefile and CI config, and record the
commands and gotchas in their notes file.

**Comment marker.** Humans and bees share one GitHub account unless
[`[github]`](configuration.md#github) gives the factory one of its own, so
every comment a role posts on GitHub must end with the line
`<!-- bees:<role> -->` (invisible when rendered). The orchestrator uses it to
tell bee comments from human ones when it collects PR feedback for the
developer, and where the factory does have its own login it counts a comment
by that login as a bee's too — sessions carry that login's token, so their
own `gh` posts as it as well. The marker is required either way: with no
`[github]` account it is the only signal there is.

Roles interact with GitHub through MCP tools where there is one, and through
the already-authenticated `gh` CLI for everything else; with each other, only
through the local mailbox (the `mail_send` tool). Every issue or PR a role
creates must match the filter: issues are created with the `issue_create`
tool, which applies the filter label and assignee itself, and for
`gh pr create` the prompts include the exact flags to pass (`--label "bees"`
plus `--assignee` when configured).

The built-in `bees` MCP server serves the tools. `mail_send`, `mail_list`,
`issue_create`, `issue_link`, `done`, `issue_view`, `pr_view` and `comment` go
to every session; `issue_edit_body` to the two managers, `issue_set_state` to
the project manager and `issue_question` to the product manager. A role calls
them with arguments instead of composing a command line, and **the tools
enforce the rules the prompts used to only state**: nothing outside the filter
can be read or written, `comment` appends the role's marker, `issue_edit_body`
refuses a feature or feedback issue for anyone but the product manager, and
`issue_set_state` only moves an issue that is in `bees:triage`. The equivalent
`bees` commands (`bees mail send`, `bees issue create`, `bees issue link`,
`bees done`) still exist and behave identically. See
[cli.md](cli.md#bees-mcp-serve-sessions).

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
a person, title); the features **whose work is done** — every sub-issue closed
since the last run — in a section of their own; every open **work item**
(state, kind, the **parent** feature it is a sub-issue of or `-`, milestone,
title — feature and feedback issues are excluded from this table); open PRs;
the **fresh `bees:feedback` issues** (full body and every comment); the
issues a person put in **planning** with it and the ones they have **agreed**,
each in a section of its own (full body and every comment); unread
mail; its notes. It works from a detached
checkout of the default branch and is told to read the codebase and README to
understand what exists.

**Does on GitHub:** creates feature issues with `issue_create` (`feature: true`
→ `bees` + `bees:feature` + `bees:proposal`, no state label; `related: <feedback issue>` when it
comes from feedback, so the milestone is inherited), describing user-visible
outcomes rather than implementation; creates work items as sub-issues with
`issue_create` (`parent: <feature>`); attaches existing issues with
`issue_link`; rewrites feature and feedback bodies with `issue_edit_body` (the
only role that may); comments with `comment`; adds `bees:question` with
`issue_question` (`waiting: true`) and uses `waiting: false` only to withdraw a
question — the orchestrator is what clears the label once a person answers;
closes feature and feedback issues with `gh issue close`, which is the one
action here with no tool, so the marker goes on that comment by hand. It
**never creates, edits or closes milestones** — people manage those; the
product manager reads them as a priority signal and, if it thinks one is
wrong, says so in a reply instead of acting. It is told to search before
creating and to keep the backlog small.

**Feature issues:** a `bees:feature` issue is the product manager's from idea
to shipped, whether it wrote it or a person filed it. Feature issues never
enter the workflow state machine. For each fresh one it:

1. makes sure the issue is detailed enough to be broken down;
2. if only a person can decide something, posts the question with `comment`
   (which appends the `<!-- bees:product_manager -->` marker), starting it with
   the mentions from [`scheduler.notify`](configuration.md#notifying-a-person)
   when it is set — the factory comments under your own account, so nothing
   else tells the people who can answer — adds the `bees:question` label with
   `issue_question` (`waiting: true`), and stops working on that feature;
3. otherwise breaks it into work items — one issue per pull-request-sized
   piece, created with `issue_create` (`parent: <feature>`, `bug: true` for
   bugs), which makes each a native GitHub **sub-issue** of the feature with
   `bees` + `bees:triage` and the feature's milestone (it may pre-size one
   with `labels: ["bees:size/s"]`, a hint the project manager confirms during
   triage — see [Sizing](workflow.md#sizing)); ordered, with
   dependencies expressed as `blocked_by: [<issue>]` (a `Blocked by #N` line the
   scheduler honours, see [Dependencies](workflow.md#dependencies)) rather than
   prose — then comments the list of work items on the feature
   issue (with the marker) so it is not re-presented until something changes;
4. closes the feature issue when all its sub-issues are closed, or when it no
   longer makes sense.

**Features whose work is done.** The last sub-issue of a feature closing is an
event nobody would otherwise report: the work items are gone from the queues
and the feature sits open until the product manager next runs for another
reason. The orchestrator notices it without asking GitHub. Each product manager
run records, per feature, the numbers of its open sub-issues (it already looks
each open work item's parent up for the `Parent` column); every later pass
checks those numbers against the issues the poll still finds open, which is a
purely local test and adds no call to the polling path. When they have all
closed, the feature wakes the product manager and is presented in a section of
its own, framed as one yes/no: is the feature's *original intent* complete? If
it is, the product manager closes it; if it is not, it says on the issue what
is missing and creates work items for exactly that — a finished feature is not
an invitation to widen it. It is reported **once**: a feature the product
manager looked at and deliberately left open is not raised again until it gains
a sub-issue and that one closes too. A feature whose children all closed before
the scheduler ever recorded them, or that no product manager run has seen, has
nothing recorded and is picked up on the next run for any other reason, which
`product_manager_interval` guarantees.

Once a pass it also **attaches loose work items**. Only the issues the product
manager creates itself get a `parent`: a bug a developer, reviewer or QA files,
or a split the project manager makes during triage, has none, so its feature
looks further along than it is and it inherits no milestone. The scheduler
looks each open work item's parent up on GitHub (one `ParentIssue` GraphQL
query apiece) and renders it in the `Parent` column of the work-item table, so
a loose item shows as `-`; the prompt tells the product manager to read that
column and attach what belongs to a feature with `issue_link`. Becoming a
sub-issue carries the feature's milestone across, so an attached work item
lands in the same release as one created under the feature — but only when it
is in no milestone already, because a milestone a person set is never
overwritten or cleared.

**Proposals:** a feature issue the product manager creates itself is labelled
`bees:proposal` as well as `bees:feature`. It writes, refines and asks
questions on such an issue as usual, but does **not** break it into work items
until a person removes the label — and it cannot: `issue_create`
(`parent: <proposal>`) and `issue_link` refuse while the label is there.
Removing the label is the approval, and the scheduler notices it (it is a label
edit, so it leaves no comment) and hands the feature back to the product manager
on its next run. A feature issue a person filed carries no proposal label: it is
already approved and handled exactly as before. The product manager's prompt
lists its proposals in a section of their own, never among the features it is
told to break down, and marks them in the `Proposal` column of the feature
table, because bees and people share one GitHub account and the author is no
signal.

**Planning mode:** a person may put a feature or feedback issue in
`bees:planning` to agree it with the product manager before anything is built.
While the label is there the issue is a conversation: it is presented in a
section of its own that lists no breakdown step, the product manager replies to
each fresh comment with questions, options or a draft, and it creates nothing —
`issue_create` (`parent:`) and `issue_link` refuse a planning issue, as they do
a proposal. The person ends planning by swapping `bees:planning` for
`bees:planned`, which the product manager treats as **agreed**: it does not
re-open the scope, writes what was settled into the issue body as a short
`## Decisions` section, records the outcome in its notes, and breaks the issue
down. A feature is presented as agreed only while it has no sub-issues, so the
breakdown happens once. Both labels are a person's: the product manager never
adds or removes either. See
[Planning with the product manager](workflow.md#planning-with-the-product-manager).

A feature issue is *fresh* when the human side had the last word on it: a
person created or commented on it, and the product manager has not commented
since (`github.Client.AwaitingBee`). When a person answers a `bees:question`,
the orchestrator removes the label and the issue comes back as fresh; a fresh
feature or feedback issue triggers a product manager run regardless of
`product_manager_interval` — except a proposal or a planning issue, where only
a comment counts, since nobody has commented on one the moment it is labelled
and it would otherwise wake the product manager on every poll.

**Feedback from people:** issues labelled `bees:feedback` are the product
manager's inbox (feature ideas, product feedback, bug reports from humans) —
the usual channel, not the only one, since a person can also write to it by
mail; they never enter the workflow state machine. Not all of it is
paragraph-sized: an issue a person files with no state label and neither
`bees:feature` nor `bees:feedback` — a bare `bees` issue, or a `bees:bug`
report — lands here too (the orchestrator labels it `bees:feedback`, see
[Filing work](workflow.md#filing-work)), so some feedback is a small,
already-well-formed ask. For each fresh one the product manager decides and
acts (feature issues, bug work items, or a reasoned no), then must **reply
on the feedback issue** with `comment`, saying what it did and linking
created issues. A ready-to-build ask becomes a work item with `issue_create`
(`related: <feedback issue>`) rather than a feature written around it,
carrying `bees:priority` over if a person put it there. It closes the issue
when fully actioned, or asks the person a question (`comment` +
`issue_question`) and leaves it open. Freshness works exactly as for feature
issues.

**Mail:** receives questions from the project manager and reports from QA;
may send to `project_manager` only. It is told to be decisive because the
project manager is blocked until it answers — and to reply only when the answer
changes what the project manager does, because unread mail on its own is enough
to start a project manager session. Mail from `human` is treated as a direction
rather than a question: it is followed literally even where it contradicts the
prompt, and what was done about it is said in the outcome. It needs no feedback
issue to hang a reply on.

**Outcomes:** `done` (with a summary), `idle`, `failed`. A run with no fresh
feature, no fresh feedback, no mail, no unanswered comment on a proposal or a
planning issue, and no
completed feature was woken by `product_manager_interval` rather than by an
event; the prompt tells the
product manager to run the loose-work-item check and then report `idle` rather
than look for work to invent. Proposals, planning issues and completed features
are the wake conditions that are easy to miss: all three are partitioned out of
the fresh features into a section of their own, so a person questioning a
proposal, a person's comment on an issue in planning, or a
feature finishing its last work item, produces a task whose other sections are
empty. An issue carrying `bees:planned` is not a wake condition at all — it
waits in its own section for whichever run comes next — so a clock-woken run
still has that section to work before it reports `idle`. The orchestrator
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
is told to read the parent feature for context.

The batch size is a per-pass reading limit, not the size of the queue.
Anything in `bees:triage` beyond it gets a table of its own ("Also in
`bees:triage`, bodies not shown"), saying it is triage work the session may
take if it has room, and is left out of the other-issues table so the two
never overlap. The prompt also shows the open
[blockers](workflow.md#dependencies) each work item declares — on the triage
header line and as a **Blocked by** column of both tables.

**Does on GitHub:** rewrites triage work items (context, scope, acceptance
criteria, pointers to code, testing expectations) with `issue_edit_body`,
keeping the author's intent; splits oversized work items with
`issue_create` (`ready: true`, `parent: <feature>`, or `related: <original>` when
there is no parent feature), closing the original with a comment; moves
refined work items `bees:triage` →
`bees:ready` with `issue_set_state`, which requires exactly one **size**
(`xs` … `l`) and applies both labels in one edit, splitting anything that sizes
up above `roles.developer.max_size` (default `l`) instead of labelling it — the
orchestrator sends such an issue straight back to `bees:triage`; moves a work
item to `bees:blocked` (the same tool) when it has asked the product manager;
closes invalid or duplicate work items with a comment. It
never edits feature or feedback issues — those belong to the product manager,
and `issue_edit_body` refuses them — and never touches milestones. It is the
only role besides the orchestrator that moves state labels. It is told to
declare dependencies with a `Blocked by #N` line — written for it by
`issue_create`'s `blocked_by` on an issue it creates in a split — and to move
the item to `bees:ready` anyway rather than parking it in triage: the
scheduler holds it back until the blocker closes.

Three judgements the prompt makes for it, rather than leaving to the session:

- A work item that is really a **direction** rather than a piece of work goes
  to the product manager and is blocked, like any other product decision; the
  project manager does not invent acceptance criteria for it.
- When it **dedupes**, it closes the issue the work is *not* attached to, and
  it re-checks that a bug still happens on the default branch before moving it
  to `bees:ready` — a bug that waited through a merge wave is often already
  fixed.
- It may add [`bees:priority`](workflow.md#priority-do-this-next) — a label
  that is otherwise a person's to set — to a work item that
  unblocks the factory itself: the default branch does not build, every pull
  request's checks are red for the same reason, or the orchestrator cannot
  run. That is the only ordering it controls; it never moves `bees:ready`
  issues back to `bees:triage` to make another one the oldest.

**Mail:** receives developer questions; may send to `product_manager`
(product decisions) and `developer` (answers, always with `issue`). Prompts
tell it to answer mail first, since developers are blocked on it, and to give
decisions rather than options. Mail from `human` is treated as a direction
rather than a question: it is followed literally even where it contradicts the
prompt, and work it holds back stays in `bees:triage` with the hold written
into the body.

**Outcomes:** `done`, `idle`, `failed`. The orchestrator marks its mail read
and records the run. Answers it sent take effect on the pass its finished
session wakes: an issue in `bees:blocked` with unread developer mail is
relabelled `bees:ready`. The
same mechanism returns its own questions: an issue it blocked goes back to
`bees:triage` once there is unread mail for the project manager about it, so
the question it sends must carry the issue number.

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
environment carries `push.autoSetupRemote=true` and `push.default=current` (via
`GIT_CONFIG_*` variables, so the clone's own git config is untouched) and a
plain `git push` just works. With [`[github]`](configuration.md#github) set the
session also carries the factory's `GH_TOKEN`, its commit identity and a git
credential helper of gh's, so what it pushes, commits and opens is the bot's
rather than the machine owner's.

**Does on GitHub:** pushes the branch; opens the PR with `gh pr create` (body
must contain `Closes #N`, a summary, and how it was tested) or updates the
existing one; files out-of-scope bugs with
`issue_create` (`bug: true`, `related: <issue>` — a `bees:bug` work item in triage,
in the same milestone as the issue it was working on); replies
on GitHub to every human review comment it addresses (`gh api
repos/<repo>/pulls/<pr>/comments/<id>/replies` for inline comments, ending
that reply with `<!-- bees:developer -->` itself; the `comment` tool, which
appends the marker, for reviews and conversation comments). When a human's request conflicts with the issue or
the reviewer, the human wins. It must not change labels, must not push to the
default branch, and must not fix unrelated bugs.

**A session that was interrupted:** when a scheduler was killed while a
session was working this issue, the session that takes over is told so before
anything else — how far the interrupted one got, where its transcript is,
whether `bees kill` stopped it, and that the branch may already carry commits,
uncommitted edits or a pull request it never reported, to carry on from rather
than start over. Only the role that was interrupted is told, and only its first
session; `bees status` marks the worker `resumed`. See
[crash recovery](architecture.md#the-developer-worker).

**Self-check:** before pushing, the developer checks the change the way the
reviewer will. It runs the repository's own lint and test commands — the ones
its README, CONTRIBUTING, CLAUDE.md, Makefile or CI configuration document —
fixes what they report, and records the commands in its notes so later sessions
do not have to find them again. It also undoes its own fix to confirm the test
it added fails without it, and greps for every claim the change makes false
(docs, code comments, other prompts) by searching for the claim rather than for
the sentence it edited.

**Keeping up with the default branch:** the developer merges the default branch
into its own before every push, on every round, and re-runs the tests after the
merge — a merge git reports as conflict-free can still break the build, because
it resolves by context. The `orchestrator` mail about a
[conflicting or behind PR](workflow.md#conflicts-with-the-default-branch) is the
backstop for when the branch falls behind anyway, not the first time the
developer is asked.

**Mail:** may send to `project_manager` only. Where the issue leaves a choice it
is told to make it, implement that reading and write the choice into the pull
request under a heading of its own, so the reviewer rules on it. It asks only
when no reading is safe — the issue contradicts itself about something the
repository cannot settle, or a wrong choice would throw the implementation away
— because a question parks the issue and restarts the work in a later session
with none of this one's context.

**Outcomes and what the orchestrator does:**

| Status | Orchestrator |
|---|---|
| `pr-opened` (with `pr`) | Locates the PR (by number, else by branch), makes it match the filter (`bees` label, plus the assignee and milestone when configured), records it, moves the issue to `bees:review`, reads the pull request's checks (`pre_review_checks`) and runs the reviewer. If the PR cannot be found: escalate. |
| `pr-updated` (with `pr`) | Same as above; used after addressing review feedback. |
| `question` | Verifies a message to the project manager was actually sent during the session, then labels the issue `bees:blocked` and frees the worker. No message: escalate. |
| `failed` (or no outcome / timeout / error) | Escalates to `bees:needs-human` with the note, which is posted on the issue as the comment a person reads. The prompt tells the developer to use it only when neither a partial pull request nor a question can move the issue forward. |

If the reviewer role is disabled, `pr-opened`/`pr-updated` go straight to
approved (and, with `roles.reviewer.auto_merge`, into the checks stage).

## reviewer

Reviews one pull request and decides whether it is mergeable. It also owns
merging: `auto_merge` and its companions live under `[roles.reviewer]` (see
[configuration.md](configuration.md#rolesreviewer-only-checks-and-auto-merge)).

**Given:** the PR (title, body, branch, author), the linked issue, the
**review stages** to run (below), the issue's **size** and a sentence on the
scrutiny it warrants (see [Sizing](workflow.md#sizing)), the status of the pull
request's checks as read just before the review (unless
`pre_review_checks = false`), its own feedback from previous rounds, unread
mail addressed to `reviewer` about the issue or the pull request (in practice
from a human), the round number and limit, its notes. With a `product-fit`
stage configured it is also given the work item's **parent feature**, the only
thing that stage judges against, and when a scheduler was killed during a
reviewer session for the issue, [what that session left
behind](#developer). It runs in the same worktree as the developer
for that issue, fast-forwarded to the latest push, so it reads the change in
its context.

**Verifying is CI's job.** The prompt tells the reviewer to judge the change
from the code and not to spend the session re-running the repository's
test-suite to repeat what the checks already report; the checks section of its
prompt says what CI found, and when nothing was reported it is told to say so in
its outcome note. Its "look for" list is ordered — correctness, the same defect
shape at sibling sites, tests that would fail without the change, then
everything else — and it must be able to show a finding (the input, the wrong
result) rather than hedge it.

**Does on GitHub:** reads (`gh pr diff`, and `pr_view` for what people have
already said on the pull request — which outranks the issue and its prompt);
files unrelated bugs with `issue_create` (`bug: true`, `related: <issue>`,
inheriting the issue's milestone). It does **not** submit a GitHub review,
comment on the pull request, push to the branch, or change labels: nothing it
writes reaches the person who merges except its outcome note, which is why the
note has to stand on its own.

**Mail:** may send to `developer` only, with `pr` and `issue`, one
consolidated message per round, its points grouped by stage, each listing the
file/line and the expected change. It also *receives* mail addressed to
`reviewer` — in practice from a human
(`bees mail send --from human --to reviewer`) — about the issue or the pull
request: it is delivered to the next reviewer session, in review mode and in
checks mode alike, and marked read afterwards. Mail from `human` is a
direction it follows literally, even where its prompt says otherwise.

**Outcomes and what the orchestrator does:**

| Status | Orchestrator |
|---|---|
| `approved` | Every configured stage passed. Labels the PR and the issue `bees:approved`. Without `auto_merge` the worker is freed and a human merges. With `auto_merge` the worker enters the checks stage (below). |
| `changes-requested` | Verifies feedback mail to the developer was sent during the session (none: escalate). If the round limit is reached: escalate. Otherwise increments the round, moves the issue back to `bees:in-progress` and runs the developer with the feedback. |
| `failed` (or no outcome / timeout / error) | Escalates to `bees:needs-human`. |

The reviewer is told when it is on the final round so it still requests
changes honestly and lets the orchestrator escalate.

### Review stages (`roles.reviewer.stages`)

These are stages *within* one reviewer session — sections of its prompt — not
worker stages like the pre-review checks and checks stages below, which are
separate sessions.

A review is not one judgement. Asking for all of them at once produced reviews
that commented on formatting while missing that the feature was half
implemented, or blocked on product fit when the issue had been explicit — so
the review runs as **ordered stages**, each with its own focus, its own source
of truth and its own verdict.

| Stage | Question it answers | Source of truth |
|---|---|---|
| `style` | Does it follow the repository's formatting and lint conventions? | the repository's conventions, CLAUDE.md, the linter |
| `cleanliness` | Is it clear, small, free of dead code and needless abstraction? | the diff |
| `implementation` | Is it correct? Error handling, edge cases, tests, security. | the diff |
| `completeness` | Does it deliver the work item's acceptance criteria? | the issue |
| `product-fit` | Does it fit the parent feature and the product direction? | the parent feature, the README and the docs |

**The default is `["implementation", "completeness", "cleanliness", "style"]`**,
in that order: the most valuable judgement first, so the reviewer spends its
best attention on correctness and reaches formatting last. `product-fit` is
**off by default** — a work item the project manager already scoped is not the
place to re-open the product decision, and leaving it off is also what keeps
the default review's scope the same as the single-pass reviewer it replaced.
It is the one stage that needs the work item's parent feature, so the
orchestrator looks the parent up only when the stage is configured; a work item
that belongs to no feature gets the stage anyway, judged against the README and
the docs, and the reviewer is told to say so.

**No early exit.** Every configured stage runs, even after one of them has
already found something to block on. The developer fixes one round of feedback
at a time, so a stage skipped because an earlier one failed costs a whole extra
round when its findings finally arrive.

**Feedback and approval.** Each stage ends with a verdict line of its own
(`<stage>: pass` / `<stage>: fail`). Requesting changes still sends exactly one
message to the developer, its points **grouped by stage** in the stages' order,
each group headed by that stage's verdict. An approval means every configured
stage passed: one failed stage is `changes-requested`, whatever the others
said. The outcome note — the only thing that reaches the person who merges —
carries the stages that ran and how each came out.

**One session, several sections.** All the stages run in one reviewer session,
as sections of its task prompt, rather than one session per stage: a session
per stage would multiply the cost and each one would have to re-read the diff
and the issue from scratch. Per-stage sessions remain a possible follow-up if
the sections turn out to bleed into each other — the stage list is already the
seam they would be split along.

The list is validated at load: a stage that is not one of the five above, an
empty list, and `stages` set anywhere but `[roles.reviewer]` are all load
errors. See
[configuration.md](configuration.md#rolesreviewer-only-the-review-stages).

### Pre-review checks (`pre_review_checks`, on by default)

The worker reads the pull request's checks before the first review, bounded by
`pre_review_checks_timeout` (default 10 minutes), so a CI failure costs a check
fix round instead of a whole review round. Green → the reviewer's prompt lists
them under `## Required checks` and says CI is green. Still pending at the
timeout, or a repository that reports no checks → the review happens anyway and
the reviewer is told nothing was verified for it, and to say so in its note. A
failing check → checks mode below, *before* any review; the fix rounds are the
same counter, and the reviewer only sees the pull request once it is green. The
read is made once per pull request: a later review round has no checks section,
because the checks that were read describe a head the developer has since
replaced.

### Checks mode (a failing check)

After approval, with `auto_merge = true`, the orchestrator waits `checks_wait`,
then polls the PR's checks — the required checks if the branch has any,
otherwise every check the pull request reports; with no checks at all it merges
and says so. If they all pass it merges with `merge_method` and deletes the
branch. If any fails — here or in the pre-review read above — the reviewer gets
a second kind of session, rendered from `task/reviewer_checks.md` with
`BEES_REVIEW_MODE=checks` in its environment.

**Given:** the PR, the issue, the list of failing checks (name,
workflow, bucket, description, details link), the fix round and its limit,
its notes.

**Does:** finds the cause without assuming a CI system — follows the details
link, runs `gh pr checks`, reads the repository's docs and its own notes, uses
`gh run view --log-failed` only when the link is a GitHub Actions run, and — as
a last resort, when no log is reachable — reproduces the failure locally on the
branch, to name the error rather than to verify the change. It records how to
read this project's CI in its notes. It then distils the main error, its
cause and a reproducing command into one mail to the developer.

| Status | Orchestrator |
|---|---|
| `changes-requested` | Verifies the mail was sent (none: escalate), then runs the developer, whose `pr-updated` leads straight back to the stage that found the failure (pre-review or post-approval checks) — no extra review round. |
| `approved` | Means "wait again": the reviewer re-ran the check, or found it already green. The orchestrator waits `checks_wait` and polls once more. |
| `failed` (or no outcome / timeout / error) | Escalates to `bees:needs-human`. |

Fix rounds are counted in `<state_dir>/issues/<n>.json` (`check_fix_rounds`)
and capped by `max_check_fix_rounds` — pre-review and post-approval rounds share
that counter, and neither counts against `max_review_rounds`. Checks still
pending at `checks_timeout` (after approval), or a merge GitHub refuses, also
escalate.

## qa

Tests the product as a user would, from the default branch, and turns what it
finds into bug reports and product feedback.

**Given:** when it last ran, the PRs merged since then (title, body, closing
issues), the open `bees:bug` issues (to avoid duplicates), unread mail
addressed to `qa` (in practice from a human), its notes. It runs in a detached
checkout of the default branch and works out from the repository's
documentation (and its notes) how to install, test and exercise the product.

**Product defects, not code critique.** QA judges what the product does — what
it does that it should not, or fails to do at all. How the code is written is
the reviewer's job, on the pull request. What "exercise it" means follows from
what the product is: a service is launched and driven, a command-line tool or a
library is run the way its documentation tells a user to. The prompt tells QA
never to start anything that acts on the real world for it — a deploy, a
scheduler or job runner, a command that spends money or writes to the live
project the product manages — and to use a sandbox, a throwaway configuration
or a dry-run flag instead. When the merged list is long QA is told not to give
every entry equal time, and to say in its report which ones it only skimmed.

**Filing is not the goal; the report is.** A clean batch is a good result: QA
files nothing and says so. It files only defects it reproduced itself.

**Does on GitHub:** files bug issues with
`issue_create` (`bug: true`, `related: <issue the merged PR closed>` → `bees` +
`bees:bug` + `bees:triage`, in that issue's milestone; `related` is omitted
when the bug is not tied to a recent change), with reproduction steps, expected
vs actual behaviour, severity and the command it ran with the output it got.
Before opening anything it searches the existing issues, **closed as well as
open** (the list in its task is the open bugs only), and comments on an
**open** report rather than duplicating it; a closed one is context, and a
failure it has reproduced again becomes a new bug linking to it. A broken
environment is one issue however many merged pull requests it spoils.

**Stays in its lane:** what QA may file directly is a bug report, or a small
work item within the existing design. Anything that asks for new scope — a new
capability, a different way of working — goes to the product manager by mail
instead; the product manager decides whether to drop it or turn it into a
proposal a person approves. QA never opens feature issues itself.

**Mail:** may send to `product_manager` only: one report per session (what was
tested, what works, bugs filed, product-level observations), sent even when
nothing was found, and skipped only when QA could not test at all. It also
*receives* mail addressed to `qa` — in practice from a human
(`bees mail send --from human --to qa`): unread mail starts a QA run on the
next pass, whatever `qa_interval` is, and is marked read there. Mail from
`human` is a direction QA follows literally, even where its prompt says
otherwise.

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
- **model_by_size** (developer only) picks the model per work item size, e.g.
  `model_by_size = { xs = "sonnet", s = "sonnet" }`. A size with no entry, and
  an issue with no size label, uses `model`. Setting the key on another role or
  `[global]` is a validation error.

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
