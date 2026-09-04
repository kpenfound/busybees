# Roles

A busybees staff has five roles. Each session is a fresh `claude -p` run with
a role's system prompt, so a role remembers nothing between sessions beyond
its notes file and what is visible on GitHub. This page says what each role
is: what it reads, what it may do, what it reports, and which `roles.<name>.*`
keys shape it. What happens to an issue on its way through the factory, the
labels, the queues and the review loop, is on [workflow.md](workflow.md).

## Concurrency model

| Role | Instances | Runs when |
|---|---|---|
| `product_manager` | singleton | unread mail; `scheduler.product_manager_interval` (default 1h) elapsed since its last run, the first run being immediate; a person approved a proposal by removing `bees:proposal`; a feature whose recorded sub-issues have all closed; a fresh `bees:feedback` or `bees:feature` issue, one a person created or commented on since the product manager last replied (on a `bees:proposal` or `bees:planning` issue only a comment counts) |
| `project_manager` | singleton | an issue in `bees:triage`, or unread mail |
| `developer` | pool of `scheduler.max_developers` workers (default 1) | a `bees:ready` issue is waiting, or an issue in `bees:in-progress`, `bees:review` or `bees:approved` has a worker to resume. A ready issue that already has a pull request goes before new work |
| `reviewer` | one per developer worker, in turn with it; one per requested review, in a developer slot | the worker's developer session opened or updated a pull request; a check on that pull request failed, before the first review or, with `auto_merge`, after approval; a person put `bees:review-requested` on a pull request |
| `qa` | singleton | unread mail; or `scheduler.qa_interval` (default 30m) elapsed and something was merged since its last run, the first run being immediate |

A developer worker owns one issue at a time and runs developer, reviewer,
developer, and so on for it, one session at a time. A review a person asks for
with `bees:review-requested` runs outside any worker and takes a slot from the
same `scheduler.max_developers` pool, so that number bounds every reviewer
session too.
A singleton runs one session at a time and waits at least
`scheduler.poll_interval` (default 5m) after a session ends before starting
the next; after a session that reported `failed` it waits five poll intervals.

Every role reports how a session ended with the `done` tool. A session that
ends without an outcome, runs past the role's `timeout` or exits with an error
counts as `failed`. What the orchestrator does with a failure is under
[Escalation](workflow.md#escalation-beesneeds-human); which ready issue a free
developer takes is under [Sizing](workflow.md#sizing) and
[Priority](workflow.md#priority-do-this-next).

## Common ground

Every role's system prompt starts with the same preamble: what busybees is,
and that the product's purpose lives in the repository rather than in the
prompt; the [visibility filter](workflow.md#what-the-factory-can-see); the
[label state machine](workflow.md#the-label-state-machine); the tools; the
mailbox; how to report an outcome; the notes file; the environment
(repository, working directory, branch, state directory, session directory);
how to learn the project; and the ground rules. `bees prompts show <role>
--rendered` prints the whole system prompt a role receives for this project.

**People outrank the prompt.** Anything a person writes, in an issue, on a
pull request or in mail from `human`, is authoritative for every role. Mail
from `human` is a direction, not a question: the role follows it even where
its prompt says otherwise, and says in its outcome what it did about it. How
your comments reach a session is under
[Giving the developer feedback](workflow.md#giving-the-developer-feedback) and
[Commenting on the issue](workflow.md#commenting-on-the-issue).

**Learning the project.** busybees tells a role nothing about how to build,
test or run the product. The role reads the repository's README, CONTRIBUTING,
CLAUDE.md, Makefile and CI configuration, and records the commands and gotchas
in its notes file. The prompt adds that documentation which is missing or
wrong is worth an issue.

**The comment marker.** People and bees share one GitHub account unless
[`[github]`](configuration.md#github) gives the factory its own, so every
comment a role posts ends with the line `<!-- bees:<role> -->`, invisible when
rendered. The `comment` tool appends it; a role posting through `gh` (the
product manager closing an issue, the developer replying to an inline review
comment) writes it itself. The orchestrator reads the marker to tell a bee's
comment from a person's, and with `[github]` set it also counts a comment by
the configured login as a bee's. The marker is required either way.

**Ground rules.** Stay in your role. Do not merge pull requests. Do not push
to the default branch. Do not remove the `bees` label from anything. Write for
a person who has not seen the conversation. Say so in the outcome note when
you could not finish.

**Tools.** The factory's own operations and the GitHub actions a role performs
are MCP tools, served to every session by the built-in `bees` server. Eight go
to every role: `mail_send`, `mail_list`, `issue_create`, `issue_link`,
`issue_view`, `pr_view`, `comment` and `done`. Three are role-scoped:
`issue_edit_body` (both managers), `issue_set_state` (project manager) and
`issue_question` (product manager). The tools enforce the factory's rules:
nothing outside the filter can be read or written, `comment` appends the
marker, `issue_edit_body` refuses a feature or feedback issue for anyone but
the product manager, and `issue_set_state` only moves an issue out of
`bees:triage`. A tool a role is not offered is one it may not use, and the
prompt tells it not to reach for `gh` instead. `gh`, already authenticated, is
for everything without a tool: `gh pr create`, `gh pr diff`, `gh issue close`,
`gh api`. Issues are always created with `issue_create`, which applies the
filter labels and assignee, and pull requests with the `gh pr create` flags
the prompt spells out. `bees mcp tools <role>` prints a role's tool set; the
arguments and the matching `bees` commands are under
[`bees mcp serve`](cli.md#bees-mcp-serve-sessions).

**Mail.** Roles talk to each other only through the local mailbox, never
through GitHub comments. Every message carries the issue and/or pull request
it is about, so it reaches the session working on that item. The unread mail
addressed to a role is rendered into its task prompt and marked read when the
session ends. `bees mail list` reads the mailbox and `bees mail send --from
human --to <role>` writes to a role. Who each role may write to is under the
role below.

### Notes files

`<state_dir>/notes/<role>.md` is a role's only long-term memory. Its contents
are rendered into every task prompt, and the role is told to update it before
finishing: decisions, conventions, gotchas, what it tested. The file is
created on the role's first run with a `# <role> notes` heading and the four
sections roles keep their notes under: **Project facts** (how to build, test
and run the project), **Conventions**, **Decisions** and **Open questions**.
Anything else goes under a heading of the role's choosing. Developer workers
share one `notes/developer.md`.

Nothing curates the file behind the role's back. Every
`scheduler.notes_consolidate_every` sessions (default 10), or sooner once the
file is larger than `scheduler.notes_max_bytes` (default 32768), the task
prompt asks the session to rewrite its notes into those sections on top of its
normal work: merge duplicates, drop what is stale or contradicted, keep
decisions, commands and gotchas. The counters live in
`<state_dir>/<role>.json`.

Editing a notes file is the most direct way to steer a role. Write the product
vision into `notes/product_manager.md`, coding conventions into
`notes/developer.md`, or "always run the e2e suite" into `notes/reviewer.md`,
and the next session reads it. [`bees notes`](cli.md#notes) does it without
hunting for the file, and `bees status` shows how big each file is:

```sh
bees notes show reviewer
bees notes add reviewer "Always run the e2e suite before approving."
bees notes edit developer
bees notes reset qa        # archive the file and start a fresh one
```

## product_manager

Owns the *what* and the *why*: the product vision, the feature issues, and the
answers to the project manager's product questions. It runs against a detached
checkout of the default branch and is told to read the codebase and the README
to understand what exists.

**Reads.** The open milestones (number, title, open and closed counts,
description), read only. Every open feature issue in a table: milestone,
sub-issue progress as `completed/total`, whether it is a proposal, whether it
is waiting on a person. The fresh feature issues in full, with every comment.
Its own proposals awaiting a person's approval, the issues in planning with a
person, and the ones a person has agreed, each group in a section of its own.
The features whose work is done, every sub-issue closed since the last run.
Every open work item: state, kind, the parent feature it is a sub-issue of
(`-` when none), milestone, title. The open pull requests. The fresh feedback
issues in full. Unread mail. Its notes. Each list is a snapshot taken through
the filter when the session started, and the prompt tells it to confirm with
`issue_view` or `gh` before creating, closing or commenting on anything.

**Does.** Creates feature issues with `issue_create` (`feature: true`, plus
`related: <feedback issue>` when the feature comes from feedback), describing
a user-visible outcome rather than an implementation. Breaks a feature into
work items with `issue_create` (`parent: <feature>`; `bug: true` for a bug;
`blocked_by` for an order; a size label in `labels` as a hint the project
manager confirms), then comments the list on the feature. Attaches loose work
items to their feature with `issue_link`, once a pass, reading the `Parent`
column of its task. Rewrites feature and feedback bodies with
`issue_edit_body`, which no other role may. Asks a person a question on the
issue with `comment` and `issue_question` (`waiting: true`), starting the
comment with the mentions from
[`scheduler.notify`](configuration.md#notifying-a-person) when it is set; the
orchestrator removes the label when the person answers, and `waiting: false`
only withdraws a question. Replies on every feedback issue it acts on, and
routes a ready-to-build ask straight to a work item (`issue_create`,
`related: <feedback issue>`, carrying `bees:priority` over with `gh issue edit
--add-label` when a person put it there) rather than writing a feature around
it. Closes feature and feedback issues with `gh issue close`, the one action
with no tool, writing the marker into the closing comment by hand. Never
creates, edits or closes a milestone: it reads them as a priority signal and
says in a reply when it thinks one is wrong. It searches before it creates and
keeps the backlog small; a full ready queue is a reason to create less.

Proposals, planning mode, questions, feedback and finished features are the
product manager's half of
[Talking to the product manager](workflow.md#talking-to-the-product-manager)
and
[Features, sub-issues and milestones](workflow.md#features-sub-issues-and-milestones).
In short: a feature it writes is a proposal it may refine but not break down
until a person removes `bees:proposal`, and `issue_create` and `issue_link`
refuse the breakdown while the label is there. An issue in `bees:planning` is
a conversation it replies to on the issue and creates nothing from.
`bees:planned` is an agreement it writes into the body as a `## Decisions`
section and then acts on without reopening it. A feature whose work is done is
one yes-or-no decision: close it, or say on the issue what is missing and
create work items for exactly that.

**Mail.** Receives the project manager's product questions, QA's reports and
mail from `human`. Writes to `project_manager` only, and only when the answer
changes what the project manager does, because unread mail on its own starts a
project manager session.

**Outcomes.** `done` with a one-line summary, `idle` when the pass found
nothing to do, `failed` when it could not run the pass at all. The
orchestrator marks the delivered mail read and records the run time, which
restarts the `product_manager_interval` clock. A run woken by the clock rather
than by an event still works the agreed section and the loose-work-item check
before it reports `idle`.

## project_manager

Turns work items into work a developer can execute without guessing, and
answers developers' questions. It runs against a detached checkout of the
default branch.

**Reads.** The first `scheduler.triage_batch_size` (default 5) work items in
`bees:triage` in full, each with its parent feature and the open issues it is
blocked by. The rest of the triage queue in a table of its own, which the
prompt says is triage work too. A table of every other visible issue: state,
kind, blockers, milestone, title. The open pull requests. Unread mail. Its
notes.

**Does.** Rewrites a work item's body with `issue_edit_body`: context, scope
in and out, acceptance criteria, pointers to code, testing expectations,
keeping the author's meaning. Splits an item too big for one pull request with
`issue_create` (`ready: true`, `parent: <feature>`, or `related: <original>`
when there is no parent; `blocked_by` for the order) and closes the original
with a comment listing the parts. Moves a refined item to `bees:ready` with
`issue_set_state` (`state: ready`, `size: <xs|s|m|l|xl>`), which sets both
labels in one edit. The size is required, and an item that would size above
`roles.developer.max_size` is split instead of labelled. Asks the product
manager for a product decision by mail and moves the item to `bees:blocked`
with the same tool; a work item that is really a direction rather than a piece
of work goes the same way, instead of getting invented acceptance criteria.
Closes an invalid or duplicate item with a comment naming its replacement,
closing the one the work is not attached to. Checks a bug still happens on the
default branch before moving it to `bees:ready`. Writes `Blocked by #N` into
an item that depends on another and still moves it to `bees:ready`; the
scheduler holds it. Adds `bees:priority` in one case only, a work item that
unblocks the factory itself, with `gh issue edit --add-label`. It never edits
feature or feedback issues (`issue_edit_body` refuses them), never touches
milestones, and never moves a `bees:ready` issue back to `bees:triage` to
reorder the queue.

The states it moves, how sizes are read and how a blocked question comes back
are under [The label state machine](workflow.md#the-label-state-machine),
[Sizing](workflow.md#sizing), [Dependencies](workflow.md#dependencies) and
[Questions](workflow.md#questions).

**Mail.** Receives developers' questions, a person's comments on an issue it
blocked out of triage (delivered as mail from `human`), and any other mail
from `human`. Writes to `product_manager` for product decisions and to
`developer` for answers, always with `issue`. The prompt puts mail first,
since developers are blocked on it, and asks for a decision rather than
options. A direction from `human` that holds work back keeps the items in
`bees:triage`, with the hold written into the body.

**Outcomes.** `done`, `idle`, `failed`. The orchestrator marks its mail read
and records the run. An answer it sent lifts `bees:blocked` on the pass its
finished session wakes, and a question it sent must carry the issue number or
the issue waits for ever.

## developer

Implements one issue on a dedicated branch and opens a pull request for it.
`scheduler.max_developers` workers run at once, each owning one issue.

**Reads.** The issue with its labels, milestone and every comment; the parent
feature's number and title when the issue is a sub-issue; the existing pull
request on a later round; the round number and `scheduler.max_review_rounds`;
unread mail about the issue or the pull request; its notes. The mail is where
every direction arrives: the project manager's answers, the reviewer's
feedback, a person's review of the pull request (from `human`, with comment
ids and the exact `gh` reply commands), a person's comment on the issue while
it is in flight (from `human` too), and the orchestrator's request to bring
the branch up to date when the pull request
[conflicts with the default branch](workflow.md#conflicts-with-the-default-branch).

It runs in a git worktree on `bees/issue-N` (the prefix is
`project.branch_prefix`), based on the default branch. The session environment
sets `push.autoSetupRemote=true` and `push.default=current` through
`GIT_CONFIG_*` variables, so a plain `git push` works and the clone's own git
configuration is untouched. With [`[github]`](configuration.md#github) set it
also carries the factory's token, commit identity and a git credential helper,
so what it commits and opens is the factory's rather than the machine owner's,
and an https push authenticates as the factory too. `commit_flags` tells it to
add flags to every `git commit` it makes.

**Does.** Explores the codebase, implements on the branch in small commits,
adds or updates tests and runs the test-suite the way the repository
documents. Before handing over it checks the change the way the reviewer will:
runs the repository's own lint and test commands and records them in its
notes, undoes its fix to confirm the test it added fails without it, and greps
for every claim the change makes false rather than for the sentence it edited.
It merges the default branch into the branch before every push, on every
round, and runs the tests again afterwards. It pushes and opens the pull
request with `gh pr create` (body with `Closes #N`, a summary and how it was
tested), or pushes to the existing one and rewrites its description. Where the
issue leaves a choice it makes one, implements it and records it in the pull
request under a heading of its own, for the reviewer to rule on.

On a review round it addresses every point or says in the pull request why
not; the pull request is the conversation, never mail back to the reviewer. A
person's review comment gets a reply on GitHub, an inline one through `gh api
.../replies` with the marker written by hand, a review or conversation comment
through `comment`. When a person's request conflicts with the issue or the
reviewer, the person wins and the pull request says so. A person's comment on
the issue is answered on the issue with `comment`. Bugs outside the issue's
scope are filed with `issue_create` (`bug: true`, `related: <issue>`), never
fixed in passing. It never changes labels and never pushes to the default
branch.

**A session that was interrupted.** When the session working the issue never
finished, because the scheduler died with it or a person stopped it (`bees
kill`, the live view's `k`, or a second ctrl-c), the next session is told so
first: how far the interrupted one got, where its transcript is, and whether
it was stopped on purpose. The developer is told the branch may carry commits,
uncommitted edits or a pull request the interrupted session never reported,
and to carry on from them. A reviewer in the same position is told its round
reported no verdict and starts over, and that the interrupted session may
already have sent mail or posted on GitHub. Only the interrupted role's first
session is told, and `bees status` marks the worker `resumed`. See
[The developer worker](architecture.md#the-developer-worker).

**Mail.** Writes to `project_manager` only, and only when no reading of the
issue is safe: the issue contradicts itself about something the repository
cannot settle, or the wrong choice would throw the implementation away. It
then sends one message, stops without implementing and reports `question`.
Everything else is a choice it makes and records in the pull request.

**Outcomes.**

| Status | What the orchestrator does |
|---|---|
| `pr-opened`, `pr-updated` (with `pr`) | Locates the pull request by number, else by branch; makes it match the filter (the `bees` label, plus the assignee and milestone when configured); moves the issue to `bees:review`; reads the checks ([pre-review checks](#pre-review-checks-pre_review_checks-on-by-default)) and runs the reviewer. No such pull request: escalate. |
| `question` | Checks that a message to the project manager was sent during the session, then labels the issue `bees:blocked` and frees the worker. No message: escalate. |
| `failed`, or no outcome | Escalates with the note, posted on the issue as the comment a person reads. The prompt reserves it for when neither a partial pull request nor a question can move the issue forward. |

With the reviewer [disabled](#disabling-a-role), `pr-opened` and `pr-updated`
approve the pull request at once, and with `auto_merge` the worker goes on to
the checks stage.

**Configuration.** `roles.developer` takes the common keys and three of its
own: `commit_flags`, `max_size` (default `l`, the largest work item a
developer takes) and `model_by_size` (a model per size label). Set anywhere
else, they are a load error. See
[configuration.md](configuration.md#rolesdeveloper-only-commit-flags-max-size-and-per-size-models).

## reviewer

Reviews one pull request and decides whether it is ready to merge. It also
owns the checks and the merge: `auto_merge` and its companions are
`roles.reviewer` keys.

A review a person asks for by putting `bees:review-requested` on any visible
pull request is a session of the same role with no issue behind it: it reads
the pull request, the mail addressed to `reviewer` about it and its notes,
runs in a read-only checkout of the head branch, and reports `approved` or
`changes-requested` with a note. See
[Asking for a review of any pull request](workflow.md#asking-for-a-review-of-any-pull-request).

**Reads.** The pull request (title, body, branches, author); the issue with
its body; the [review stages](#review-stages-rolesreviewerstages) to run; the
issue's size with a sentence on the scrutiny it warrants (an `xs` change is
checked for correctness and completeness and not asked to restructure; an `l`
one is judged on its design too); the pull request's checks as read just
before the first review, or a line saying nothing was verified; its own
feedback from previous rounds; unread mail addressed to `reviewer` about the
issue or the pull request; the round number and `scheduler.max_review_rounds`;
its notes. With `product-fit` configured it also reads the work item's parent
feature. It runs in the developer's worktree for that issue, brought up to the
latest push, so it reads the change in context.

**Does.** Reads the diff with `gh pr diff` and the pull request with
`pr_view`, where what a person already said outranks the issue and the prompt.
Judges the change from the code: verifying that it builds and passes is CI's
job, and the prompt tells it not to re-run the repository's test-suite.
Whatever the stage, it looks for correctness first, then the same defect shape
at sibling sites the pull request did not touch, then tests that would fail
without the change, then unhandled errors, security and scope creep. What the
formatter, the linter and the checks enforce is not a review point. It raises
only what it can show, the input and the wrong result, never a "might" or a
"consider". It files unrelated bugs with `issue_create` (`bug: true`,
`related: <issue>`) rather than blocking on them. It does not submit a GitHub
review, comment on the pull request, push to the branch or change labels.
Nothing it writes reaches the person who merges except its outcome note, so
the note carries the stages that ran and how each came out, what it chose not
to block on, and, when no check was reported, that nothing was verified for
it.

**Mail.** Writes to `developer` only: one message per round, with `pr` and
`issue`, its points grouped by stage in the stages' order, each group headed
by that stage's verdict line, each point with the file and line and what it
expects instead. An approval sends no mail, because the developer's work on
the issue is over and no session is left to read it: anything the developer
should know goes in the note, anything worth doing in an issue. It receives
mail addressed to `reviewer`, in practice from a person (`bees mail send
--from human --to reviewer`), in a review session and a checks-mode session
alike, and a copy of any comment a person writes on the issue while it is in
`bees:review`.

**Outcomes.**

| Status | What the orchestrator does |
|---|---|
| `approved` | Every stage passed. Labels the pull request and the issue `bees:approved` and requests a review from the people in `scheduler.notify`. Without `auto_merge` the worker is freed and a person merges; with it, the worker enters the checks stage ([Checks mode](#checks-mode-a-failing-check)). |
| `changes-requested` | Checks that feedback was mailed to the developer during the session (none: escalate). On the last round: escalate. Otherwise moves the issue back to `bees:in-progress` and runs the developer with the feedback in its mail. |
| `failed`, or no outcome | Escalates. |

The reviewer is told when it is on the final round, so it still requests
changes honestly and lets the orchestrator escalate. The loop as a whole,
`max_review_rounds` and what the person merging sees are under
[Review](workflow.md#review) and [Merging](workflow.md#merging).

### Review stages (`roles.reviewer.stages`)

A reviewer session reviews in ordered stages, each a section of its task
prompt with its own focus, its own source of truth and its own verdict. The
word "stage" also names the phases of a developer worker (develop, pre-review
checks, review, checks), which are separate sessions; the two are unrelated.

| Stage | Question it answers | Source of truth |
|---|---|---|
| `implementation` | Is it correct? Error handling, edge cases, concurrency, security, the inputs and states the issue never mentioned, and whether the tests would fail without the change. | the diff |
| `completeness` | Does it deliver the acceptance criteria, one at a time? A deviation the pull request declares is a judgement call; one it is silent about is not delivered. | the issue |
| `cleanliness` | Is it clear, small and free of dead code? Needless abstraction, a helper with one caller, a copy of something that exists, changes the issue did not ask for. | the diff |
| `style` | Does it follow the repository's conventions? Only what the tooling does not already enforce. | the repository's conventions, CLAUDE.md, the linter |
| `product-fit` | Does it pull the product somewhere the feature and the documentation do not go? The work item's own scope was settled before it reached the developer and is not reopened here. | the parent feature, the README and the docs |

The default is `["implementation", "completeness", "cleanliness", "style"]`,
in that order, so the reviewer spends its first attention on correctness and
reaches formatting last. `product-fit` is off by default: a work item the
project manager already scoped is not the place to reopen the product
decision. It is the one stage that reads the parent feature, so the
orchestrator looks the parent up only when the stage is configured; a work
item that belongs to no feature gets the stage anyway, judged against the
README and the docs, and the reviewer says so in the verdict.

Every configured stage runs, even after one has found something to block on:
the developer fixes one round of feedback at a time, so a skipped stage costs
a whole extra round when its findings arrive. Each stage ends with a verdict
line, `<stage>: pass` or `<stage>: fail` with one line saying why, in the
stages' order. Approval means every stage passed; one failed stage is
`changes-requested` whatever the others said.

```toml
[roles.reviewer]
stages = ["implementation", "completeness", "product-fit"]
```

A name outside the five above, an empty list, or `stages` anywhere but
`[roles.reviewer]` is a load error. See
[configuration.md](configuration.md#rolesreviewer-only-the-review-stages).

### Pre-review checks (`pre_review_checks`, on by default)

Before the first review, the developer worker reads the pull request's checks,
waiting `checks_wait`, polling every `checks_poll_interval` and giving up
after `pre_review_checks_timeout` (default 10 minutes). Green, and the
reviewer's prompt lists the checks under `## Required checks` and says CI is
green. Still pending at the timeout, or no check reported at all, and the
review happens anyway, with the reviewer told nothing was verified for it and
to say so in its note. A failing check goes to
[checks mode](#checks-mode-a-failing-check) before any review, and the
reviewer sees the pull request once it is green. The read happens once per
pull request: a later round has no checks section, because the checks that
were read describe a head the developer has since replaced. Where the stage
sits in the review loop is under
[Before the review](workflow.md#before-the-review-the-checks).

```toml
[roles.reviewer]
pre_review_checks = false    # straight from the developer to the reviewer
```

### Checks mode (a failing check)

A failing check, before the first review or after approval with `auto_merge`,
gets the reviewer a different kind of session: a diagnosis, not a review. The
session runs with `BEES_REVIEW_MODE=checks` in its environment and no review
stages.

**Reads.** The pull request, the issue, the failing checks (name, workflow,
bucket, description, details link), the fix round and `max_check_fix_rounds`,
unread mail, its notes.

**Does.** Finds out what failed without assuming a CI system: from each
check's details link and description, from `gh pr checks`, from the
repository's documentation and its own notes; with `gh run view --log-failed`
when the link is a GitHub Actions run; and, only when no log is reachable, by
reproducing the failure on the branch, to name the error rather than to verify
the change. It records how to read this project's CI in its notes. Then it
distils the main error, its cause, the file and line where possible, and a
command that reproduces it into one mail to the developer.

| Status | What the orchestrator does |
|---|---|
| `changes-requested` | Checks the mail was sent (none: escalate), then runs the developer, whose `pr-updated` goes straight back to the stage that found the failure, pre-review or post-approval. No review round is spent. |
| `approved` | "Wait again": the checks were already green when the reviewer looked, or it re-ran a failure it judged unrelated (infrastructure, flakiness), which it does at most once. The orchestrator waits `checks_wait` and polls once more. |
| `failed`, or no outcome | Escalates. |

Fix rounds are counted in `<state_dir>/issues/<n>.json` and capped by
`max_check_fix_rounds` (default 2). Pre-review and post-approval rounds share
the counter, and neither counts against `max_review_rounds`. The reviewer is
told when it is on the last fix round. What the post-approval stage polls,
when it merges and when it escalates is under [Merging](workflow.md#merging);
the keys are under
[configuration.md](configuration.md#rolesreviewer-only-checks-and-auto-merge).

```toml
[roles.reviewer]
auto_merge = true
merge_method = "squash"
max_check_fix_rounds = 3
```

## qa

Tests the product as a user would, from the default branch, and turns what it
finds into bug reports and a report to the product manager. It runs against a
detached checkout of the default branch, so it only sees merged work.

**Reads.** When it last ran, and the pull requests merged since then (title,
body, closing issues); the open `bees:bug` issues, to avoid duplicates; unread
mail addressed to `qa`; its notes. The first run looks back seven days.

**Does.** Works out from the repository's documentation and its notes how to
install dependencies, run the test-suite and exercise the product, and records
which commands are safe. What "exercise it" means follows from the product: a
service is launched and driven, a command-line tool or a library is run the
way its documentation tells a user to. It never starts anything that acts on
the real world for it, such as a deploy, a job runner or a command that spends
money or writes to the live project the product manages, and uses a sandbox, a
throwaway configuration or a dry-run flag instead. It verifies each merged
pull request against its issue and explores around it for regressions. When
the merged list is long it spends its time on what a user touches and says in
its report which entries it only skimmed.

QA judges what the product does, not how the code is written; that is the
reviewer's job. A clean batch is a good result: QA files nothing and says so.
It files only what it saw the product do wrong itself, with the command it ran
and the output it got, as `issue_create` (`bug: true`, `related: <issue the
merged pull request closed>`, omitted when the bug is not tied to a recent
change), with reproduction steps, expected against actual behaviour, and
severity. Before filing it searches the existing issues, closed as well as
open: it comments on an open duplicate with `comment`, opens a new bug linking
to a closed one it has reproduced again, and files a broken environment once
however many merged pull requests it spoils. What it may file directly is a
bug report or a small work item within the existing design. Anything asking
for new scope goes to the product manager by mail, and QA never opens a
feature issue itself.

**Mail.** Writes to `product_manager` only: one report per session saying what
was tested, what works, the bugs filed and product-level observations, sent
even when nothing was found and skipped only when QA could not test at all.
Receives mail addressed to `qa`, in practice from a person (`bees mail send
--from human --to qa`); unread mail starts a QA run on the next pass whatever
`qa_interval` says.

**Outcomes.** `done` with a one-line summary, `failed` when it could not test,
with why. The orchestrator records the run time either way. When QA runs and
what it looks at is under [QA](workflow.md#qa).

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
  prompt, global file, role prompt, role file. The repository's own
  [project prompt files](configuration.md#project-prompt-files),
  `bees/prompts/common.md` and `bees/prompts/<role>.md`, read from the
  session's worktree, come after them. `bees prompts show <role>` prints the
  base prompt, and `--rendered` the whole system prompt for this project. A
  `prompt_file`'s contents are re-read for each session; `prompt`, like every
  other key in `bees.toml`, is read when `bees run` starts, so changing it
  needs a restart. The base prompt is compiled into the `bees` binary, so
  changing it needs a rebuild and a restart. `bees status` names the build the
  running scheduler was started from, and `bees doctor` warns when it is
  behind the repository.
- **skills** are unioned, global first, and exposed to the session as plugin
  directories.
- **mcp** servers are unioned; a role's server replaces a global one with the
  same name. The name `bees` is reserved for the built-in server.
- **model / fallback_model / effort / max_turns / timeout / allowed_tools /
  disallowed_tools / shell / env** fall back to `[global]`, then to the
  built-in defaults. `fallback_model` is what Claude Code switches to when
  `model` has reached its usage limit.
- **commit_flags, max_size, model_by_size** are `roles.developer` only
  ([developer](#developer)). **auto_merge, merge_method, checks_wait,
  checks_poll_interval, checks_timeout, max_check_fix_rounds,
  pre_review_checks, pre_review_checks_timeout, stages** are `roles.reviewer`
  only ([reviewer](#reviewer)). Set anywhere else, each is a load error.

`bees config show <role>` prints the result of the merge. See
[configuration.md](configuration.md#global-and-rolesname) for every key.

## Disabling a role

```toml
[roles.qa]
enabled = false
```

A disabled role is never dispatched. `bees run --roles developer,reviewer`
restricts one run the same way without editing the file. What each one costs:

- **reviewer disabled:** a pull request is approved the moment the developer
  reports `pr-opened` or `pr-updated`; with `auto_merge` it goes straight to
  the checks stage.
- **project_manager disabled:** issues stay in `bees:triage` and developers'
  questions go unanswered, unless you move issues to `bees:ready` and answer
  mail yourself.
- **product_manager disabled:** no feature is broken down and no feedback
  issue is answered; QA's reports and the project manager's questions pile up
  unread in the mailbox.
- **developer disabled:** nothing is built, and the reviewer never runs
  either.

`enabled` is a role key; under `[global]` it is a load error.
