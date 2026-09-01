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
- `.github/workflows/release.yml` is the only workflow, and its only trigger is a
  `v*` tag (see `docs/releasing.md`). There is deliberately no `push` or
  `pull_request` workflow: `dagger check` is the gate, and a CI-on-push workflow
  was added and reverted by a person on purpose. Do not add one.
- `install.sh` (repository root) downloads a release. It parses the workflow's asset
  names (`bees_<version>_<os>_<arch>.tar.gz` + `checksums.txt`), so those names are a
  public interface: `cmd/bees/release_test.go` pins them against `docs/releasing.md`.
  It is POSIX `sh`, has no test in the suite by design (it must not reach the
  network), and is checked with `shellcheck -s sh install.sh`.

## Layout

- `cmd/bees` — cobra CLI (`init`, `run`, `tick`, `exec`, `status`, `cost`, `kill`, `mail`, `notes`, `issue`, `done`, `mcp`, `doctor`, `config`, `skills`, `prompts`, `labels`, `version`).
- `internal/config` — `bees.toml` schema (commented-defaults template in `template.go`), global/role merging, labels, repo/branch derivation from the git remote (`resolve.go`).
- `internal/scheduler` — the loop: poll → deliver human feedback (`humans.go`: comments on an in-flight issue, then reviews and comments on a PR) → reconcile labels → dispatch developer workers (`developer.go`: develop → review → checks stages) → dispatch singletons (`singletons.go`). `events.go` publishes a non-blocking event stream (`Subscribe`) for views, alongside `status.json`.
- `internal/issues` — `bees issue create/link`: visible, labelled, sub-issue of a feature, milestone inherited.
- `internal/procs` — pid files + `ps` scan to find and kill orphaned sessions (`bees kill`, and the live view's `k` key through `Scheduler.KillSession`).
- `internal/testutil` — local bare git remote + clone for tests.
- `internal/session` — runs one `claude -p` session; outcome file written by `bees done`; `CheckInterrupted` says whether a session directory belongs to a session that is running, one that finished, or one a killed scheduler left unfinished.
- `internal/prompts` — role prompts embedded in the binary (`system/*.md`, `task/*.md`) rendered with `text/template`, so a prompt change reaches no session until `bees` is rebuilt and `bees run` restarted; `project.go` appends the project repository's own `bees/prompts/common.md` and `bees/prompts/<role>.md`, read from the session's worktree at session start, which need no rebuild.
- `internal/mcpserver` — the built-in MCP server (`bees mcp serve`) added to every session as `bees`, backed by the same code as the CLI: `mail_send`, `mail_list`, `issue_create`, `issue_link`, `issue_view`, `pr_view`, `comment` and `done` go to every role; role-scoped are `issue_edit_body` (both managers), `issue_set_state` (project manager) and `issue_question` (product manager). The name `bees` is reserved in bees.toml.
- `internal/tui` — the live view `bees run` draws in a terminal (bubbletea + lipgloss): the Now, Recent, Needs human, Approved PRs and Queues panels, fed by `scheduler.Subscribe` and `status.json`, plus its keys (arrows select, `enter` watches the selected session, `o` opens on GitHub, `k` stops the selected session, `q`/ctrl-c drains) and a session view (`session.go`) that tails one session's `transcript.jsonl` and queues a message from `human` for the next session on that work item. Drawn only when stdout is a terminal and `--no-tui` was not given (`tuiMode` in `cmd/bees`), and it silences console logging while it is up. `theme.go` is the only file in the package that names a colour (rows are painted by role and by outcome class, panels by their title and border) and `TestOnlyTheThemeNamesColours` keeps it that way.
- `internal/mail` — local JSON mailbox; the only channel between roles.
- `internal/github` — thin `gh` wrapper. `internal/workspace` — git worktrees. `internal/skills` — skills by git URL → `--plugin-dir`.
- `internal/state` — state dir layout (`mail/`, `notes/`, `sessions/`, `issues/`, `status.json`).
- `internal/text` — small English renderings shared by every package; `text.Count(n, noun)` is the one plural helper (regular plurals only).

## Conventions

- Roles never talk to each other through GitHub comments; the mailbox is the only
  channel. Comments on GitHub are for people: every comment a bee posts ends with
  `<!-- bees:<role> -->` so the orchestrator can tell bee and human comments apart
  (they share one `gh` account). With `[github]` set the orchestrator also reads a
  comment by the login it acts as as a bee's — the marker is emitted either way.
  The orchestrator itself only writes the `needs-human` escalation comment (which
  carries no marker).
- Milestones are managed by people. Bees never create, edit or close them; new issues
  inherit them via `bees issue create --parent/--related`.
- Workflow label transitions happen in the scheduler, except the ones prompts
  explicitly delegate: the project manager moves triage → ready / blocked, the
  product manager adds `bees:question`, and `bees issue create` sets the initial
  state label.
- Keep prompts in `internal/prompts/*/` in sync with `docs/roles.md` and `docs/workflow.md`.
- Every new bees.toml key needs: struct tag, default, validation, `template.go`,
  `docs/configuration.md`, and a test in `internal/config`.
- bees.toml carries a `version` key (missing = 0). Adding optional keys is not a
  breaking change. Renaming/removing keys or changing their meaning is: bump
  `config.CurrentVersion`, add a `migrations[old]` step that rewrites the file
  *text* (so comments survive; also fix the commented-out defaults), test it, and
  update the `version` section of `docs/configuration.md`. Loading migrates in
  memory; `Config.Rewrite` writes it back. Tightening validation (a newly reserved
  name or rejected value) is not a migration either: fail to load with an actionable
  error naming the key and what to change.
