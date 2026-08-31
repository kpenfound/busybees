# 🐝 busybees

**busybees** is a lightweight software factory: a Go CLI (`bees`) that runs a staff of
headless [Claude Code](https://claude.com/claude-code) sessions — product manager,
project manager, developers, reviewers and QA — against a single GitHub repository.

Humans steer it through GitHub. Create and label issues, comment, merge pull requests;
the bees do the rest. Every role runs in its own temporary git worktree, talks to the
other roles through a local mailbox, and is configured by one file: `bees.toml`.

```
        ┌──────────────────┐ features → sub-issue work items┌──────────────────┐
        │  product manager │ ─────────────────────────────▶ │  project manager │
        │   (singleton)    │ ◀───────── questions ───────── │   (singleton)    │
        └──────────────────┘                                └──────────────────┘
                 ▲                                             │  refined issues
                 │ QA reports                                  ▼  (bees:ready)
        ┌──────────────────┐                     ┌──────────────────────────────────┐
        │        QA        │                     │ developer workers (max_developers)│
        │   (singleton)    │                     │  developer ─▶ PR ─▶ reviewer ─┐   │
        │  tests `main`    │                     │      ▲                        │   │
        └──────────────────┘                     │      └──── feedback (mail) ◀──┘   │
                 ▲                               └──────────────────────────────────┘
                 │ merged PRs                                  │ bees:approved
                 └────────────── humans merge ◀────────────────┘
```

## Quick start

Prerequisites: Go 1.25+, [`gh`](https://cli.github.com/) 2.50.0 or newer
(authenticated), `git`, and Claude Code (`claude`) 2.1.76 or newer, logged in.
`bees` checks the `gh` and `claude` versions on startup; see
[Requirements](docs/configuration.md#requirements).

```sh
# install
go install github.com/kpenfound/busybees/cmd/bees@latest

# inside a clone of the project you want the bees to build
cd ~/src/my-project
bees init                # writes bees.toml, creates .bees/ and the GitHub labels
$EDITOR bees.toml        # pick models, add skills, set filter/scheduler options
bees doctor              # check the toolchain, the config, GitHub access, worktrees and the roles

# run the factory
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
goes straight to the project manager. Milestones stay yours: bees never
create or change them, but every issue they create inherits the milestone of the issue
it grew out of.

`bees status` shows queues, workers and unread mail at any time, and `bees cost`
answers what the factory spent, by role, by issue or by day. `bees tick` runs a
single pass, `bees exec developer --issue 12` runs one session by hand, `bees notes`
reads and edits a role's notes file — its only memory between sessions — and
`bees kill` cleans up leftover sessions and worktrees after a crash.

## How it works

- **GitHub is the workflow.** Issues and PRs move through labels
  (`bees:triage → bees:ready → bees:in-progress → bees:review → bees:approved`, plus
  `bees:blocked` and `bees:needs-human`). Only items matching the configured filter —
  a label, an assignee, a milestone, or a combination — are visible to the factory, so
  it coexists with normal work in the same repo. See [docs/workflow.md](docs/workflow.md).
- **Roles are Claude Code sessions.** Each session is a fresh `claude -p` run in a
  temporary worktree with a role-specific system prompt, your extra instructions,
  skills (by git URL), MCP servers, a model and a fallback model for when the primary
  hits its usage limit. Roles keep a notes file as their memory between sessions.
  See [docs/roles.md](docs/roles.md).
- **Sequential where it matters, parallel where it helps.** The product manager,
  project manager and QA are singletons. Developers form a pool (`max_developers`),
  and each developer worker runs a strictly sequential developer ↔ reviewer loop for
  its issue.
- **Role-to-role messaging stays local.** Roles ask each other questions and exchange
  review feedback through a mailbox in the state directory, never through GitHub
  comments. Questions park an issue in `bees:blocked` until the answer arrives. Bees do
  comment on GitHub *to people* — the developer replies to your PR review, the product
  manager replies to feedback and feature issues and asks its questions there — and
  every such comment ends with an invisible `<!-- bees:<role> -->` marker. The
  orchestrator itself writes exactly one kind of comment: the escalation when it needs
  a human.
- **Humans talk to the product manager on GitHub.** `bees:feedback` issues are its
  inbox and `bees:feature` issues are its own: both stay outside the workflow state
  machine and wake the product manager when a person creates or comments on them. It
  breaks features into work items — native GitHub sub-issues, so progress is tracked
  where you already look — replies on the issue, asks you with `bees:question` when only
  a person can decide, and closes the feature when its sub-issues are done. Milestones
  are managed by people only; bees inherit them onto everything they create.
- **Humans review on GitHub.** Review a bees PR like any other: the orchestrator
  delivers your reviews and comments to the developer, who addresses them and replies
  on GitHub (bee comments end with an invisible `<!-- bees:developer -->` marker).
  Feedback on an approved PR sends the issue back through the developer and reviewer.
- **One config file.** `bees.toml` holds project settings, the visibility filter,
  scheduler limits, and global + per-role prompt/skills/MCP/model settings.
  See [docs/configuration.md](docs/configuration.md) and
  [bees.example.toml](bees.example.toml).

## Configuration at a glance

```toml
# [project] repo and default_branch are derived from the git remote (origin).

[filter]
label = "bees"          # only issues/PRs with this label are visible
assignee = "@me"        # ...and assigned to you (optional)

[scheduler]
max_developers = 2      # concurrent developer <-> reviewer loops
max_review_rounds = 3

[global]
model = "opus"
fallback_model = "sonnet"
skills = ["https://github.com/anthropics/skills#skills/webapp-testing"]

[roles.developer]
prompt = "Follow the conventions in CONTRIBUTING.md. Prefer table-driven tests."
```

## Documentation

These pages are also published at
**<https://kpenfound.github.io/busybees/>**.

| Document | What it covers |
|---|---|
| [docs/workflow.md](docs/workflow.md) | The GitHub-centred workflow: filter, label state machine, questions, review loop, escalation, QA |
| [docs/roles.md](docs/roles.md) | Each role's responsibilities, inputs, outcomes, and how to customise or disable it |
| [docs/configuration.md](docs/configuration.md) | Complete `bees.toml` reference |
| [docs/cli.md](docs/cli.md) | Every `bees` command |
| [docs/architecture.md](docs/architecture.md) | Internals, state directory, testing strategy |

## Development

busybees builds and tests with [Dagger](https://dagger.io). Set the release before
running it:

```sh
export DAGGER_X_RELEASE=v1.0.0-beta.11
dagger check            # go:lint-all, go:test-all, go:generate-all
```

`go build ./... && go test ./...` also works locally. Tests never call the real
`claude` or `gh`: GitHub is faked in-process and the test binary stands in for
`claude`, but git is real (tests push to a local bare remote).

## Status

Early. The orchestrator, roles, mailbox, worktree and skill/MCP wiring are in place and
covered by tests; the prompts will keep improving with use. Run it on a project you can
afford to babysit, keep the reviewer's `auto_merge` off, and watch `bees status` and `.bees/sessions/`.

## Acknowledgements

Inspired by [swarms](https://github.com/kyegomez/swarms), scaled down to one job: a
software factory driven by GitHub.

## License

[MIT](LICENSE)
