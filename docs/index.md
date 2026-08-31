# busybees

**busybees** is a lightweight software factory: a Go CLI (`bees`) that runs a
staff of headless [Claude Code](https://claude.com/claude-code) sessions —
product manager, project manager, developers, reviewers and QA — against a
single GitHub repository.

Humans steer it through GitHub. Create and label issues, comment, merge pull
requests; the bees do the rest. Every role runs in its own temporary git
worktree, talks to the other roles through a local mailbox, and is configured
by one file: `bees.toml`.

The [project README](https://github.com/kpenfound/busybees#readme) has the
quick start; these pages are the reference.

## Documentation

| Document | What it covers |
|---|---|
| [Architecture](architecture.md) | Internals, state directory, testing strategy |
| [Roles](roles.md) | Each role's responsibilities, inputs, outcomes, and how to customise or disable it |
| [Workflow](workflow.md) | The GitHub-centred workflow: filter, label state machine, questions, review loop, escalation, QA |
| [Configuration](configuration.md) | Complete `bees.toml` reference |
| [CLI](cli.md) | Every `bees` command |
