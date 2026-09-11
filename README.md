# 🐝 busybees

**busybees** is a lightweight software factory: a Go CLI (`bees`) that runs a staff of
headless coding-agent sessions — product manager, project manager, developers,
reviewers and QA — against a single GitHub repository.

Humans steer it through GitHub. Create and label issues, comment, merge pull requests;
the bees do the rest. Every role runs in its own temporary git worktree, talks to the
other roles through a local mailbox, and is configured by one file: `bees.toml`.

<img width="964" height="575" alt="image" src="https://github.com/user-attachments/assets/599cba11-910a-4f22-8ec3-c78272417c35" />


Contributing to busybees itself? See [CONTRIBUTING.md](CONTRIBUTING.md).

## Install

macOS and Linux, on amd64 (x86-64) and arm64:

```sh
curl -fsSL https://raw.githubusercontent.com/kpenfound/busybees/main/install.sh | sh
```

The script downloads the latest [release](https://github.com/kpenfound/busybees/releases),
checks it against the SHA-256 sums published with it, and installs the `bees` binary in
`/usr/local/bin` — using `sudo` only if that directory is not writable. Re-running it
upgrades an existing install in place. `BEES_VERSION` picks a release and
`BEES_INSTALL_DIR` where it goes; both go on `sh`, not on `curl`:

```sh
curl -fsSL https://raw.githubusercontent.com/kpenfound/busybees/main/install.sh |
    BEES_VERSION=v0.2.0 BEES_INSTALL_DIR="$HOME/.local/bin" sh
```

Anywhere the script does not cover — Windows, another architecture, or a build of
unreleased `main` — build from source with Go 1.25+ instead:

```sh
go install github.com/kpenfound/busybees/cmd/bees@latest
```

[Releasing](docs/releasing.md) describes what a release contains and how to install one
by hand.

## Quick start

You need [`gh`](https://cli.github.com/) (authenticated), `git`, and Claude Code
(`claude`), logged in. Inside a clone of the repository you want built:

```sh
bees init --template issue-driven   # writes bees.toml, creates .bees/ and the GitHub labels
bees run                            # draws a live view; --no-tui logs instead
```

Then open an issue describing a feature, label it `bees`, and watch:
the product manager breaks it into work items, the project manager refines them, a
developer builds each one and opens a pull request, the reviewer approves it, and QA
tests the default branch after every merge. Two things wait for you: a feature a bee
proposes carries `bees:proposal` until you remove the label, and every approved pull
request is yours to merge. [Config templates](docs/templates.md) describes the other
ways to run the factory.

## Key features

- **GitHub is the workflow.** Issues and pull requests move through labels
  (`bees:triage → bees:ready → bees:in-progress → bees:review → bees:approved`), and
  only items matching your configured filter are visible, so bees coexists with
  ordinary work in the same repository. See [Workflow](docs/workflow.md).
- **A role per session.** The product manager, project manager, developers,
  reviewers and QA each run as a fresh headless session in its own worktree, with a
  role-specific prompt and its own model. A role runs as
  [Claude Code](https://claude.com/claude-code) — with skills and a fallback model
  for when the primary hits its usage limit — or as
  [Codex](https://github.com/openai/codex). See [Roles](docs/roles.md).
- **Features become GitHub sub-issues.** The product manager turns a feedback or
  feature issue into work items tracked as native sub-issues, and can discuss a
  feature with you under `bees:planning` before breaking it down. See
  [Filing work](docs/workflow.md#filing-work).
- **A developer → reviewer loop.** A developer implements a work item and opens a
  pull request; a reviewer reviews it, and the two iterate until it is approved. See
  [developer](docs/roles.md#developer) and [reviewer](docs/roles.md#reviewer).
- **A review on request.** Put `bees:review-requested` on any pull request the
  factory can see, yours included, and the reviewer submits one GitHub review on
  it. See [Asking for a review of any pull request](docs/workflow.md#asking-for-a-review-of-any-pull-request).
- **A review of any pull request, without a factory.** `bees review <pr>`
  reviews a pull request from six angles in read-only sessions, lets you
  select, dismiss, defer or ask about each finding, and posts what you
  selected as one GitHub review. See [Reviewing a pull request](docs/review.md).
- **A local mailbox, not GitHub comments.** Roles ask each other questions and
  exchange review feedback through a mailbox in the state directory; every comment a
  bee posts on GitHub is to a person, and ends with an invisible marker. See
  [The mailbox](docs/architecture.md#the-mailbox).
- **One config file.** `bees.toml` holds project settings, the visibility filter,
  scheduler limits, and global and per-role prompt/skills/MCP/model settings. See
  [Configuration](docs/configuration.md) and [bees.example.toml](bees.example.toml).
- **A CLI for status, cost and control.** `bees status`, `bees cost`, `bees tick`,
  `bees exec`, `bees notes`, and `bees kill`. See [CLI reference](docs/cli.md).
- **It stops itself rather than overspend.** `scheduler.max_cost_per_day` pauses
  new sessions once the rolling 24-hour spend reaches it; two companion budgets
  cap a single issue and a single session, and all three are unlimited by
  default. The claude session limit pauses the whole factory the same way when
  the account runs out of capacity, and `bees status` reports the daily spend
  against the budget and says when either pause is in force. See
  [Cost budgets](docs/configuration.md#cost-budgets) and
  [The claude session limit](docs/configuration.md#the-claude-session-limit).

## Documentation

These pages are also published at
**<https://kpenfound.github.io/busybees/>**.

| Document | What it covers |
|---|---|
| [docs/workflow.md](docs/workflow.md) | The GitHub-centred workflow: filter, label state machine, questions, review loop, escalation, QA |
| [docs/roles.md](docs/roles.md) | Each role's responsibilities, inputs, outcomes, and how to customise or disable it |
| [docs/configuration.md](docs/configuration.md) | Complete `bees.toml` reference |
| [docs/security.md](docs/security.md) | What each sandbox mode protects and what it does not: filesystem, network, credentials |
| [docs/templates.md](docs/templates.md) | The named ways to run the factory: which roles run, what merges on its own, what waits for a person |
| [docs/cli.md](docs/cli.md) | Every `bees` command |
| [docs/review.md](docs/review.md) | `bees review`: how a review runs, its findings, reviewer notes, and its two configuration files |
| [docs/architecture.md](docs/architecture.md) | How `bees run` works: the scheduler loop, the developer worker, sessions, the mailbox, the state directory, crash recovery |
| [docs/releasing.md](docs/releasing.md) | Cutting a release: the tag, the workflow, the assets it publishes |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Building, testing and the contributor workflow |

## Acknowledgements

Inspired by [swarms](https://github.com/kyegomez/swarms), scaled down to one job: a
software factory driven by GitHub.

## License

[MIT](LICENSE)
