# busybees — notes for Claude Code

busybees (`bees`) is a Go CLI that orchestrates a staff of headless Claude Code
sessions (product manager, project manager, developers, reviewers, QA) building a
GitHub repository. Read `docs/architecture.md` before changing the scheduler.

## Build and test

- Everything builds and tests through Dagger. Always export
  `DAGGER_X_RELEASE=v1.0.0-beta.11` before running `dagger`.
- `dagger check` runs `go:lint-all`, `go:test-all` and `go:generate-all` (from the
  official `github.com/dagger/go` module in `dagger.toml`). Run it before committing.
- `go build ./... && go test ./...` works locally too and is faster while iterating.
- Tests must never call the real `claude` or `gh`. `gh` is faked through
  `github.Client.Exec`; `claude` is faked by the test binary itself (see `TestMain` in
  `internal/scheduler/scheduler_test.go`) or a shell script (`internal/session`).
  Git is real: tests create a bare origin with `internal/testutil.SetupRepos`.

## Layout

- `cmd/bees` — cobra CLI (`init`, `run`, `tick`, `exec`, `status`, `kill`, `mail`, `issue`, `done`, `config`, `prompts`, `labels`, `version`).
- `internal/config` — `bees.toml` schema (commented-defaults template in `template.go`), global/role merging, labels, repo/branch derivation from the git remote (`resolve.go`).
- `internal/scheduler` — the loop: poll → deliver human PR feedback (`humans.go`) → reconcile labels → dispatch developer workers (`developer.go`: develop → review → checks stages) → dispatch singletons (`singletons.go`).
- `internal/issues` — `bees issue create/link`: visible, labelled, sub-issue of a feature, milestone inherited.
- `internal/procs` — pid files + `ps` scan to find and kill orphaned sessions (`bees kill`).
- `internal/testutil` — local bare git remote + clone for tests.
- `internal/session` — runs one `claude -p` session; outcome file written by `bees done`.
- `internal/prompts` — embedded role prompts (`system/*.md`, `task/*.md`) rendered with `text/template`.
- `internal/mail` — local JSON mailbox; the only channel between roles.
- `internal/github` — thin `gh` wrapper. `internal/workspace` — git worktrees. `internal/skills` — skills by git URL → `--plugin-dir`.
- `internal/state` — state dir layout (`mail/`, `notes/`, `sessions/`, `issues/`, `status.json`).

## Conventions

- Roles never talk to each other through GitHub comments; the mailbox is the only
  channel. Comments on GitHub are for people: every comment a bee posts ends with
  `<!-- bees:<role> -->` so the orchestrator can tell bee and human comments apart
  (they share one `gh` account). The orchestrator itself only writes the
  `needs-human` escalation comment.
- Milestones are managed by people. Bees never create, edit or close them; new issues
  inherit them via `bees issue create --parent/--related`.
- Workflow label transitions happen in the scheduler, except the ones prompts
  explicitly delegate: the project manager moves triage → ready / blocked, the
  product manager adds `bees:question`, and `bees issue create` sets the initial
  state label.
- Keep prompts in `internal/prompts/*/` in sync with `docs/roles.md` and `docs/workflow.md`.
- Every new bees.toml key needs: struct tag, default, validation, `template.go`,
  `docs/configuration.md`, and a test in `internal/config`.
