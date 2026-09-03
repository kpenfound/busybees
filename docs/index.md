# busybees

**busybees** is a lightweight software factory: a Go CLI (`bees`) that runs a
staff of headless [Claude Code](https://claude.com/claude-code) sessions —
product manager, project manager, developers, reviewers and QA — against a
single GitHub repository.

Humans steer it through GitHub. Create and label issues, comment, merge pull
requests; the bees do the rest. Every role runs in its own temporary git
worktree, talks to the other roles through a local mailbox, and is configured
by one file: `bees.toml`.

## Quick start

Prerequisites: [`gh`](https://cli.github.com/) 2.50.0 or newer (authenticated), `git`,
and Claude Code (`claude`) 2.1.76 or newer, logged in — plus Go 1.25+ if you build from
source rather than installing a release. `bees` checks the `gh` and `claude` versions on
startup; see [Requirements](configuration.md#requirements).

```sh
# inside a clone of the project you want the bees to build
cd ~/src/my-project
bees init                # writes bees.toml, creates .bees/ and the GitHub labels
$EDITOR bees.toml        # pick models, add skills, set filter/scheduler options
bees doctor              # check the toolchain, the config, GitHub access, worktrees and the roles

# run the factory (in a terminal it draws a live view; --no-tui logs instead)
bees run
```

Then open a feature issue in the repository, label it `bees` + `bees:feature`, and watch:

1. the **product manager** makes it detailed enough — asking you on the issue
   (`bees:question`) if only you can decide something — and breaks it into work items:
   GitHub sub-issues of the feature (`bees issue create --parent N`), so the feature's
   progress shows on GitHub;
2. the **project manager** refines each work item and moves it to `bees:ready`;
3. a **developer** picks it up, implements it on `bees/issue-N`, and opens a PR;
4. the **reviewer** reviews the PR; the developer addresses feedback; repeat until approved;
5. you **merge** the PR (or set `auto_merge = true` on the reviewer, which waits for the
   required checks, hands failures back to the developer, and merges when green);
6. **QA** tests `main`, files bugs, and reports to the **product manager**, who closes
   the feature when its sub-issues are done and plans the next ones.

Have an idea, feedback, or a bug you'd rather have weighed than fixed verbatim? Open an
issue with `bees` + `bees:feedback`: it goes to the **product manager**, which turns it
into feature issues, replies on it, and closes it when done. An issue you label only
`bees` goes there too; label it `bees:triage` or `bees:ready` yourself to have it built
without that hop. A concrete bug can skip all that: `bees` + `bees:bug` + `bees:triage`
goes straight to the project manager. For anything non-trivial, add `bees:planning`
first: the product manager then only *discusses* the issue — questions, options, a
draft to react to — until you swap the label for `bees:planned`, which is your
agreement for it to break the work down. Milestones stay yours: bees never
create or change them, but every issue they create inherits the milestone of the issue
it grew out of.

`bees status` shows queues, workers and unread mail at any time, and `bees cost`
answers what the factory spent, by role, by issue or by day. `bees tick` runs a
single pass, `bees exec developer --issue 12` runs one session by hand, `bees notes`
reads and edits a role's notes file — its only memory between sessions — and
`bees kill` cleans up leftover sessions and worktrees after a crash.

## Documentation

| Document | What it covers |
|---|---|
| [Architecture](architecture.md) | How `bees run` works: the scheduler loop, the developer worker, sessions, the mailbox, the state directory, crash recovery |
| [Roles](roles.md) | Each role's responsibilities, inputs, outcomes, and how to customise or disable it |
| [Workflow](workflow.md) | The GitHub-centred workflow: filter, label state machine, questions, review loop, escalation, QA |
| [Configuration](configuration.md) | Complete `bees.toml` reference |
| [CLI](cli.md) | Every `bees` command |
| [Releasing](releasing.md) | Cutting a release: the tag, the workflow, the assets it publishes |
