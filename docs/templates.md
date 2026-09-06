# Config templates

A config template is a named set of decisions about how the factory runs: which
of the five roles are enabled, whether the reviewer merges the pull requests it
approves, whether a feature a bee writes waits for your approval, and whether
the reviewer reviews pull requests other people open.
`bees init --template <name>` writes those decisions as active settings in
`bees.toml`, under a comment naming the template.

A template decides nothing else. The repository, the branch, the filter, the
models, the budgets, the intervals and the prompts are what
[`bees init`](cli.md#bees-init) writes either way. Nothing reads the name back
afterwards, so the file is yours to edit: change a line and the factory follows
the line, not the template it started from.

## The templates

| Template | What it does | Roles that run | Auto-merge | What waits for you |
|---|---|---|---|---|
| [contributor](#contributor) | one contributor among many: builds the issues in its filter, a person merges | project manager, developer, reviewer | off | every issue, every merge |
| [issue-driven](#issue-driven) | a person writes feature issues, the full staff builds them, a person merges | all five | off | each proposed feature, every merge |
| [slop-factory](#slop-factory) | the full staff with auto-merge on and no approval gate on proposed features | all five | on | nothing |
| [reviewer](#reviewer) | reviews pull requests other people open and builds nothing | reviewer | off | what to do with each review |
| [planner](#planner) | the two managers scope and break down issues, nobody builds | product manager, project manager | off | each proposed feature, and everything after `bees:ready` |

`bees templates show <name>` prints the whole file a template writes, so
anything the table leaves out is one command away.

## contributor

bees is one contributor among many on the project. It takes the issues in its
filter, sizes them, builds each one on a branch and opens a pull request that a
person reviews and merges. The product manager and QA are off, so nothing
proposes features or files bugs. The project manager is on, so an issue you
file is triaged into a ready work item without any further help.

The pull request is the gate, and it is the only one that matters here: nothing
merges without you, and nothing enters the factory that you did not file.

```sh
bees init --template contributor
```

Day to day you put the `bees` label on an issue and, a while later, review the
pull request that answers it.

## issue-driven

A person writes feature issues and the full staff takes them from there: the
product manager breaks each one into work items, the project manager triages
them, developers and reviewers build them, and QA tests the default branch
after every merge. A person approves each feature a bee proposes and merges
each approved pull request.

Two things wait for you. A feature the product manager writes carries
[`bees:proposal`](workflow.md#feature-issues) until you remove the label, so
the factory cannot grow its own roadmap. And the reviewer approves rather than
merges, so every change is yours to land.

```sh
bees init --template issue-driven
```

Day to day you file a feature issue, approve or drop the proposals that come
back, and merge the pull requests the reviewer approved.

## slop-factory

The full staff, and nothing waits for a person: the reviewer merges an approved
pull request as soon as its checks are green, and a feature the product manager
designs from QA's reports goes straight into the queue without a person's
approval. A person writes the first issues and watches.

There is no gate. The reviewer merges what it approves, and a feature the
product manager writes carries no `bees:proposal`, so the factory's own reports
become its input: QA files a bug, the product manager turns it into work, a
developer builds it, the reviewer merges it. Watch `bees status`, and set
`scheduler.max_cost_per_day` before you walk away.

```sh
bees init --template slop-factory
```

Day to day you file the first issues, then read what landed on the default
branch.

## reviewer

Nothing is built. The reviewer reviews the pull requests other people open and
submits each review on GitHub: every open pull request in the filter that the
factory did not write, which with `filter.assignee` set means the ones assigned
to the bee. `filter.assignee` is a login, so no template sets it. Pass it to
`bees init` yourself.

The verdict is a GitHub review and nothing else. No label moves, nobody is
mailed, no branch is pushed, so what happens to the pull request is entirely
yours. See
[Asking for a review of any pull request](workflow.md#asking-for-a-review-of-any-pull-request).

```sh
bees init --template reviewer --assignee my-bot-login
```

Day to day you assign a pull request to that login and read the review the
factory leaves on it.

## planner

Only the two managers run: the product manager turns feature issues into work
items and the project manager triages them until they are ready to build, and
there it stops. No developer picks anything up, nothing is reviewed and nothing
is tested. Use it to have the backlog scoped before deciding who builds it.

Everything past `bees:ready` waits for you, because no role is enabled that
could act on it, and a feature the product manager writes still waits for you
to remove `bees:proposal`.

```sh
bees init --template planner
```

Day to day you file a feature issue and read the work items and the questions
that come back on it.

## The commands

Four commands work with templates, and each links to its full reference.

[`bees templates list`](cli.md#bees-templates-list) prints the names and
summaries.

```
$ bees templates list
contributor    one contributor among many: builds the issues in its filter, a person merges
issue-driven   a person writes feature issues, the full staff builds them, a person merges
slop-factory   the full staff with auto-merge on and no approval gate on proposed features
reviewer       reviews pull requests other people open and builds nothing
planner        the two managers scope and break down issues, nobody builds
```

[`bees templates show`](cli.md#bees-templates-show-name) prints the `bees.toml`
a template writes, headed by the paragraph explaining when to use it. It reads
nothing, so you can run it before you have a project.

```
$ bees templates show planner | head -7
# Template: planner
#
# Only the two managers run: the product manager turns feature issues into work
# items and the project manager triages them until they are ready to build, and
# there it stops. No developer picks anything up, nothing is reviewed and
# nothing is tested. Use it to have the backlog scoped before deciding who
# builds it.
```

`bees init --template <name>` writes that file into the current directory, and
is otherwise the same command as a plain [`bees init`](cli.md#bees-init).

[`bees templates diff`](cli.md#bees-templates-diff-name) reports where this
project's config and a template disagree. With no name it picks the template
the config is closest to, which places a project set up before the templates
existed.

```
$ bees templates diff
/src/widgets/bees.toml vs issue-driven (closest of 5 templates)

  roles.reviewer.auto_merge   true  (issue-driven: false)
```

The comparison is on the resolved values rather than the file text, so a key
left commented out compares equal to a template that sets it to the same
default. Only the settings a template decides are compared. A config that makes
all of them the same way prints one line.

```
$ bees templates diff contributor
/src/widgets/bees.toml matches contributor.
```
