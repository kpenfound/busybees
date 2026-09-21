# Evals

`bees eval` runs the whole factory against a fixture repository and grades the
result with the fixture's own test, the way SWE-bench Lite grades a coding
agent. `bees eval <role>` runs one role instead, against cases that grade that
role's session on its own. Use either to compare a prompt, model or profile
change against a baseline run.

Evals run real agent sessions and cost money. They are not part of
`dagger check`.

## Running evals

From a directory with an `evals/` directory in it (the busybees repository
ships one):

```sh
bees eval                          # every case under evals/
bees eval --case hello             # only evals/hello
bees eval --profile fast           # every role on the profile named fast
bees eval developer                # every case under evals/developer/
bees eval qa --case broken-greeting
```

The output is one row per case, then what failed:

```
CASE   RESULT  SCORE  STOP  COST   TURNS  DURATION  PROFILE
hello  pass    -      done  $0.84  41     6m12s     default (bees.toml)

report: /home/me/src/busybees/.bees/evals/20260918-190412/report.json
```

`SCORE` is the mean of the case's graded checks, and `-` for a case with
none.

`bees eval` exits non-zero when any case fails. `STOP` says why the run of a
case ended:

| Stop | Meaning |
|---|---|
| `done` | Every seeded issue is closed or carries `bees:needs-human`; for a per-role run, the role's session finished. |
| `timeout` | The case's `timeout` ran out. The running sessions were stopped. |
| `budget` | The sessions cost the case's `max_cost`. The running sessions were stopped. |
| `invalid` | The test passed on the fixture before the run, so the case proves nothing. No session ran. Whole-factory cases only. |
| `interrupted` | You stopped `bees eval`. |
| `error` | The case could not be set up, or the scheduler failed. The error follows the table. |

A `COST` ending in `+?` counts sessions that reported no cost.

### The shipped cases

The busybees repository ships these cases. The `todo-*` cases share one
fixture, `evals/fixtures/todo`: a small Go module with a to-do list library
and a `todo` command. Their test is `go test ./...`, so they need Go on the
machine that runs `bees eval`.

| Case | Kind | The issue asks | The grading test checks |
|---|---|---|---|
| `hello` | bug | `greet.sh` prints `Hello, Ada!`, not `Hello Ada` | `test.sh` compares the output of `sh greet.sh Ada` |
| `todo-done-number` | bug | `todo done <n>` marks item `n`, counting from 1, not the one after it | `List.Complete` marks the numbered item and refuses 0, negative and out-of-range numbers; `todo done 1` marks the first line |
| `todo-priority-order` | bug | `List.ByPriority` puts the items without a priority last, not before `(A)` | `ByPriority` orders `(A)` to `(Z)`, then the items without one, each group in list order, and leaves the list as it was |
| `todo-overdue` | feature | a `List.Overdue(today)` method: the pending items due before today's date | `Overdue` compares dates, not times, and leaves out done items, items due today or later and items with no due date |
| `todo-count` | feature, seeded for triage | a `todo count` command printing `2 pending, 1 done` | the line printed for a list and for a missing file, and an error for an extra argument |

The `todo-*` grading tests are only in each case's `grade/`, so the sessions
do not see them.

These are the per-role cases:

| Case | The role is given | The case checks |
|---|---|---|
| `developer/done-number` | one ready issue on the shared `todo` fixture | the outcome `pr-opened`, a pull request for the issue, `bees:ready` gone from it, and a rubric on the change and the pull request's description |
| `developer/ask-first` | a ready issue whose acceptance criteria contradict each other about the numbering | the outcome `question`, `bees:blocked` on the issue, mail to the project manager, no issue created, and rubrics on the question and on having built nothing |
| `developer/out-of-scope-bug` | a ready issue in a file that holds a second, unrelated fault | the outcome, a pull request for the issue, one issue created, and rubrics on the fault being filed rather than fixed and on the change itself |
| `developer/review-feedback` | an open pull request on the shared `todo` fixture and the reviewer's mail raising three findings | the outcome `pr-updated`, and rubrics on each finding and on the rewritten pull request description |
| `project_manager/thin-issue` | a triage queue of two, a blocked issue and the developer's question about it | the outcome, `bees:triage` → `bees:ready` on the thin issue, the invalid one closed, mail to the developer, and rubrics on the refined issue and the answer |
| `project_manager/split-in-two` | one triage item that is two pull requests | the outcome, two issues created, the original closed, and rubrics on the split and on not rewriting the original instead |
| `project_manager/escalate` | a triage item whose first deliverable is a product decision, and one to refine | the outcome, `bees:blocked` on it, mail to the product manager, no issue created, the other item ready, and rubrics on the question and on inventing no criteria |
| `qa/broken-greeting` | a default branch whose `test.sh` fails, and one bug already filed | the outcome, one issue created, the report mailed to the product manager, and a rubric on the bug report |
| `qa/clean-pass` | a default branch that does what its README says, and a bug report that does not reproduce | the outcome, no issue created, the report mailed to the product manager, and a rubric on reporting a clean pass rather than inventing a defect |
| `qa/already-filed` | a default branch with one defect, already reported as #1 | the outcome, no issue created, the report mailed to the product manager, and a rubric on commenting on #1 after `file_bug` refuses the duplicate |
| `qa/wrong-run-command` | a default branch whose README documents a command that is not there | the outcome, one issue created, the report mailed to the product manager, and a rubric on filing the documentation defect and saying so in the outcome |
| `product_manager/feedback-idea` | one `bees:feedback` issue holding a vague idea | the outcome, one issue created, the feedback issue closed, and a rubric on the feature issue and the reply |
| `product_manager/break-down` | an approved feature with three outcomes in it | the outcome, three issues created, `bees:question` not added, and rubrics on the split and on leaving milestones alone |
| `product_manager/ask-a-person` | a feature whose scope turns on a decision only a person can make | the outcome, `bees:question` on the feature, no issue created, and a rubric on the question |
| `product_manager/answer-a-question` | the project manager's question by mail, and nothing on GitHub to do | the outcome, mail back about the issue, no issue created, and a rubric on the answer and on leaving GitHub alone |

### Which profile the sessions run on

`--profile <name>` runs every role on that profile. bees looks it up in the
`[profiles]` of `bees.toml` first, then in `~/.config/bees/config.toml`.

Without `--profile`, the eval uses the profile selection `bees.toml` makes
([profiles](configuration.md#profilesname)): `[global]` and each
role's `profile` and `profile_by_size`, and the reviewer's `brief_profile`,
`judge_profile` and `angle_profiles`. With no `bees.toml`, it uses the
`provider` and `model` of `~/.config/bees/config.toml`, and without that file
the built-in profile.

The eval takes nothing else from `bees.toml`: no prompts, skills, MCP
servers, limits or timings, and every role is configured — a per-role run
scopes the scheduler to one role rather than configuring the rest away. Two
runs differ only by their profiles. A fixture can carry its own `bees/prompts/`, as any project
can. A profile whose sandbox is `container` or `sbx` is refused: those
sessions cannot reach the fake GitHub described below.

## What a whole-factory run does

For each case, in its own directory under
`<state_dir>/evals/<timestamp>/<case>/`:

1. It builds the fixture: a bare `origin.git` with one commit on `main`, a
   branch for each pull request the case seeds, and a clone of it in
   `project/`.
2. It runs the case's test on the fixture, in `before/`. A test that passes
   ends the case as `invalid`.
3. It writes `project/bees.toml` and seeds an in-memory GitHub with the case's
   issues and pull requests, each carrying the `bees` label, and the mailbox
   with its mail.
4. It runs scheduler passes. Each pass waits for the work it started, which
   takes a developer's issue up to its approval. Between passes, it merges
   every pull request labelled `bees:approved` into its base branch on the
   origin, as a person would, and closes the issues its body names with a
   closing keyword (`Closes #1`).
5. It stops when every seeded issue is closed or carries `bees:needs-human`,
   or at the case's timeout or budget.
6. It grades the result.

The directory keeps everything: `state/` with the sessions' transcripts and
`bees.log`, the fixture, and the test output in `before.log` and `after.log`.

A per-role run does steps 1, 3 and 6, and in place of 4 and 5 runs one
session for the role (see [Per-role cases](#per-role-cases) below).

### The fake GitHub

The scheduler talks to the in-memory GitHub directly. A session, and the
`bees` MCP server it starts, runs `gh`, and the eval puts a `gh` of its own
first on their `PATH` (`bin/gh` in the case directory). It forwards each call
to the eval, which answers it from the same in-memory GitHub. Nothing a session
runs as `gh` reaches github.com.

That GitHub answers the calls the factory makes: listing, viewing, creating
and editing issues and pull requests, labels, comments, reviews, sub-issues
and `pr diff`. It ignores `--jq` and `--search`, reports no checks on a pull
request, and fails any call it does not know with an error the session sees.
A session that runs a `gh` by its full path, not the one on its `PATH`, is not
stopped from reaching github.com.

## Grading

A whole-factory case passes when every check passes:

| Check | Passes when |
|---|---|
| the test fails on the fixture | The test fails before the run. |
| `#N closed` | Seeded issue N is closed. |
| `#N has a pull request` | A pull request's head is the issue's branch (`bees/issue-N`), or its body closes the issue. |
| the test passes | The test passes on `main` after the run. |

The test runs in a fresh clone of `main`, with the case's `grade/` files
copied over it, as `sh -c "<test>"`. A test gets 10 minutes.

## Writing a case

A whole-factory case is a directory under `evals/` with a `case.toml`, and a
per-role one a directory under `evals/<role>/`. Both hold the same fixture:

```
evals/hello/
  case.toml       the issues to seed, the test and the limits
  repo/           the fixture's files, committed as they are
  grade/          optional: files copied over the checkout before the test runs
  pr/<branch>/    optional: the working tree of a seeded pull request's branch
```

Instead of `repo/`, a case can have a `setup.sh`, which bees runs with `sh` in
an empty clone and then commits. A case has one or the other.

Cases can share a fixture. Put it under `evals/fixtures/<name>/` and have
each case's `setup.sh` copy it in:

```sh
cp -R "$(dirname "$0")/../fixtures/todo/." .
```

`evals/fixtures/` is not a case.

Put the files that grade the case in `grade/`, where a session that edits
the fixture's tests does not change what grades it. A copy in `repo/` as
well lets the developer run them, as in `hello`, where the test is a shell
script: a Go test that fails on the fixture cannot be copied into `repo/`.
Left out of `repo/`, as in the `todo-*` cases, they stay hidden the way
SWE-bench hides its tests, and the issue has to state the behaviour they
check. Give them names the developer is unlikely to pick
(`eval_count_test.go`, `TestEvalCount`): a file copied over the checkout
replaces one with the same path, and a Go test declared twice does not
build.

A fixture written in Go has to build, be `gofmt`-clean and pass its own
`go test ./...`, and so does the tree of every branch the case seeds. The
defect the case is about belongs in the hidden tests under `grade/`, or in
what the seeded branch's diff shows: a fixture that ships a failing test
grades whoever runs `go test` rather than the role the case is about. The
suite assembles each shipped case and checks this.

`evals/hello/case.toml`:

```toml
description = "greet.sh leaves out the comma and the exclamation mark"
test = "sh test.sh"
timeout = "30m"
max_cost = 5

[[issues]]
number = 1
title = "greet.sh prints the greeting without punctuation"
labels = ["bees:ready", "bees:size/xs"]
body = """
`sh greet.sh Ada` prints `Hello Ada`. It should print `Hello, Ada!`.
"""
```

| Key | Meaning |
|---|---|
| `description` | What the case is about. |
| `test` | Required. The command that grades the case, run with `sh -c` in a checkout of `main`. It must fail on the fixture. |
| `timeout` | How long the factory runs. Default `1h`. |
| `max_cost` | The budget in USD. `0` is no limit. Default `10`. |
| `[[issues]]` | The issues to seed, at least one: `number`, `title`, `body`, `labels`, `author` (default `human`) and `[[issues.comments]]` with `author` and `body`. |
| `[[mail]]` | Messages in a role's mailbox when the run starts: `from` (default `human`), `to` (a role), `subject`, `body`, and `issue`, a seeded issue it is about. |
| `[[pull_requests]]` | The pull requests to seed, each on a branch of its own in the fixture. See [Seeding a pull request](#seeding-a-pull-request). |

bees adds the `bees` label to every seeded issue. Give a work item
`bees:ready` and a size to send it straight to a developer, or `bees:triage`
to have the project manager refine it first.

Pick issues the way SWE-bench Lite does: self-contained, touching a few files,
with no links to anything outside the fixture, and graded by a test that fails
before the change and passes after it.

A directory under `evals/` named after a role (`evals/developer/`) is not a
whole-factory case: it holds that role's cases, which only `bees eval <role>`
takes.

## Per-role cases

`bees eval <role>` runs one role against the cases under `evals/<role>/`, the
way `bees exec <role>` runs one session: the scheduler is scoped to that
role, so nothing else runs, and the seeded GitHub state and mailbox stand in
for the rest of the factory. The run ends when the session does. Nothing is
merged and no second pass runs.

The rest of the factory is still *configured*, and that is deliberate: the
scheduler routes a new issue by the roles the configuration has, so a run
that configured four roles away would label an unlabelled issue somewhere
the real factory never would, and the case would be measuring the role
against a workflow that does not exist.

A per-role case has no `test`. It is graded by what it declares under
`[expect]`, and it passes when every one of those checks passes.

`evals/project_manager/thin-issue/case.toml`, cut down:

```toml
description = "a thin bug report to refine and a developer's question to answer"

[[issues]]
number = 1
title = "todo done marks the wrong item"
labels = ["bees:triage"]
body = "I typed `todo done 1` and it crossed off the second thing on my list."

[[issues]]
number = 2
title = "todo list should show due dates"
labels = ["bees:blocked", "bees:size/s"]
body = "`todo list` prints the raw line."

[[mail]]
from = "developer"
to = "project_manager"
subject = "Question about #2"
body = "What should `todo list` print for an item with no due date?"
issue = 2

[expect]
outcome = "done"

[[expect.labels]]
issue = 1
has = ["bees:ready"]
missing = ["bees:triage"]

[[expect.mail]]
to = "developer"
issue = 2
```

The keys `[[issues]]`, `[[mail]]`, `description`, `timeout` and `max_cost`
mean what they mean for a whole-factory case. These are the rest:

| Key | Meaning |
|---|---|
| `issue`, `pr` | What the session is about, as `bees exec --issue`/`--pr`. A developer or reviewer case needs one; the three singleton roles take neither, since their session is about the whole repository. `pr` names a seeded pull request. |
| `[[pull_requests]]` | The pull requests to seed: what a reviewer case reviews, and what a developer case has review feedback on. See [Seeding a pull request](#seeding-a-pull-request). |
| `[expect]` | How the case is graded. At least one check, or the case grades nothing. |

### The checks a case can declare

| Check | Passes when |
|---|---|
| `outcome = "done"` | The role's session reported that status with `done`. It has to be one the role may report. `failed` is declarable for a developer or reviewer case, where the factory hands the issue to a person and the run still ends as `done`; for the other three roles a failed session ends the run as `error`, which fails the case whatever it declared. |
| `issues_created = 1` | That many issues exist at the end that the case did not seed. |
| `issues_closed = [3]` | Each of those seeded issues is closed. |
| `pull_requests = [1]` | Each of those issues has a pull request: one on its branch, or one whose body closes it. |
| `[[expect.labels]]` | The issue carries every label under `has` and none under `missing`. One check per label. |
| `[[expect.mail]]` | The role sent the role under `to` a message, about the issue under `issue` when it names one. |
| `[[expect.graded]]` | A grader session scored the rubric at or above its pass score. |

### Rubrics and grader sessions

A check that needs judgement is a rubric, scored by a session of its own,
following Inspect AI's model-graded scorer pattern. The grader is given the
rubric, the transcript of the role's session and the state the run left
behind — the issues as they stand, the comments posted on them, the pull
requests opened and the mail the role sent — and answers with a score
between 0 and 1 and the reasons for it. Both go in the report; the score also
fills the table's `SCORE` column.

```toml
[[expect.graded]]
name = "#1 is detailed enough for a developer to build"
pass = 0.7
rubric = """
#1 as it now stands names where the bug is and says what the fixed behaviour
is. It carries acceptance criteria a test could be written from. Judge the
issue's body as it is at the end, not how it was written.
"""
```

`name` is what the report calls the check, and `pass` the score it passes at:
0.7 when the rubric leaves it out, and `pass = 0` to record the score in the
report without ever failing the case. Write the rubric as what the grader
should see, and say what not to judge: a rubric that leaves the bar to the
grader's taste scores differently from run to run, which is the one thing an
eval must not do.

The grader is **not** the agent `--profile` selects. It is the read-only
session agent from `~/.config/bees/config.toml` — the one `bees review` runs
— and it stays the same whatever profile the roles run on. Two runs of a case
are compared by their scores, so what does the scoring has to hold still
while what is being scored changes.

## Seeding a pull request

`[[pull_requests]]` gives a case a pull request a session sees the way it
would on GitHub: a branch in the fixture with the change on it, and the pull
request itself with its body, comments and reviews.

The branch's working tree is a directory in the case, `pr/<head>` by default:

```
evals/developer/address-feedback/
  case.toml
  repo/                        the fixture, committed on main
  pr/bees/issue-1/             the working tree of the head branch
```

```toml
pr = 2                         # what the session is about

[[pull_requests]]
number = 2
title = "todo list: show due dates"
body = "Closes #1"
head = "bees/issue-1"
base = "main"

[[pull_requests.comments]]
author = "human"
body = "The dates are still raw."

[[pull_requests.reviews]]
author = "kpenfound"
state = "CHANGES_REQUESTED"
body = "Print `no due date` for an item with none."
```

| Key | Meaning |
|---|---|
| `number` | Required. GitHub numbers issues and pull requests together, so it cannot be a seeded issue's number. |
| `title`, `body` | Required title, and the body. A body with a closing keyword (`Closes #1`) is what ties the pull request to its issue. |
| `head` | Required. The branch the change is on, branched off `base`. |
| `base` | The branch it is against. Default `main`. |
| `files` | The directory holding the head branch's working tree, relative to the case directory. Default `pr/<head>`. It has to be there, whether it is named or defaulted. |
| `labels` | Labels beside the `bees` label, which bees adds. |
| `author` | Who opened it. Default `human`. |
| `[[pull_requests.comments]]` | Comments on the conversation: `author` (default `human`) and `body`. |
| `[[pull_requests.reviews]]` | Reviews: `author` (default `human`), `body` and `state`, one of `APPROVED`, `CHANGES_REQUESTED` and `COMMENTED` (default `COMMENTED`). |

Once the fixture is committed on `main`, bees branches `head` off `base`,
copies the tree over it and pushes it. A file the tree leaves out is
unchanged from `base`, so the directory carries only what the pull request
touches. A pull request with no tree directory is a load error: a pull
request with no change is nothing for a session to read.

The tree's own tests pass. A reviewer reads the branch's diff for the
defect, so a test that already fails on the branch hands the answer to
anything that runs the suite instead.

A developer case gives its pull request the branch of the issue it names
(`bees/issue-<n>`): that is the branch the developer's session works on, and
the pull request it is handed to update.
