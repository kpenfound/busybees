# Contributing

## Package layout

```
cmd/bees/            the cobra CLI: every `bees` command
internal/config/     bees.toml: schema, defaults, validation, global/role merging, labels, the init template
internal/doctor/     the checks `bees doctor` runs
internal/github/     thin wrapper around the gh CLI: issues, PRs, labels, milestones, sub-issues, checks, merge, activity
internal/issues/     `bees issue create/link`: filter labels, kind and state labels, sub-issue of --parent, inherited milestone
internal/logging/    console and file logging; bees.log rotation
internal/mail/       the local mailbox: JSON messages under <state_dir>/mail/<role>/
internal/mcpserver/  the built-in MCP server (`bees mcp serve`): the factory's operations as tools, filtered by role
internal/procs/      finding and stopping sessions: `bees kill`, and one at a time from the live view
internal/prompts/    role prompts embedded in the binary (system/*.md, task/*.md), the project's own bees/prompts/ files, the renderer
internal/scheduler/  the loop: poll, human feedback, merge state, reconcile, developer workers, singleton roles, the event stream
internal/session/    one headless `claude -p` session: arguments, environment, transcript, result and outcome
internal/skills/     skill repositories by git URL, exposed as claude plugin directories
internal/state/      the state directory: notes, per-issue and per-role bookkeeping, status.json, the ledger
internal/testutil/   test helpers: a local bare git remote and a clone
internal/text/       small English renderings shared by every package
internal/tui/        the live view `bees run` draws in a terminal
internal/versions/   version checks for the tools bees drives (gh, claude)
internal/workspace/  temporary git worktrees created from the main clone
```

Dependency direction is strictly downwards: `cmd/bees` → `tui` → `scheduler`
→ everything else, and `scheduler` is the only package that knows about all
the others. `github` and `session` execute external programs (`gh`, `claude`)
and both expose an override point (`Client.Exec`, `Runner.ClaudeBin`) so tests
never need the real ones. Read [docs/architecture.md](docs/architecture.md)
before changing the scheduler: it describes the loop, the developer worker's
stages and the state directory, and it is kept in step with the code.

## Building and testing

Iterate with:

```sh
dagger check
```

The gate before committing is Dagger:

```sh
dagger check
```

`dagger check` runs `go:lint-all`, `go:test-all` and `go:generate-all` from the
`github.com/dagger/go` module installed by `dagger.toml`. The repository pins
the Dagger release with `DAGGER_X_RELEASE=v1.0.0-beta.11`; set it in your
shell. Without the export there are no checks to run: `dagger check` prints a
few green lines and exits 0, having checked nothing. `dagger check -l` lists
the checks, and `dagger check go:test-all` runs one of them.

### Testing rules

- Tests never call the real `claude` or `gh`. `gh` is faked through
  `github.Client.Exec`: the scheduler tests replace it with an in-memory
  implementation that understands the `gh` invocations the wrapper makes
  (`issue list`, including `--state all --search` for the visibility
  backstop; `issue view/edit/comment`; `pr list/view/merge/checks`, with a
  queue of scripted check results; the `api` calls for milestones, issue
  details, the parent lookup and human PR activity) and records label
  history, comments and merges for assertions. `claude` is faked by the
  test binary itself: `TestMain` in `internal/scheduler/scheduler_test.go`
  checks `FAKE_CLAUDE=1` (not `BEES_FAKE_CLAUDE`: the runner strips every
  inherited `BEES_*` variable, so a flag in that namespace would never reach
  the fake) and, when set, runs a scripted role (a developer commits, pushes
  and reports `pr-opened`; a reviewer mails feedback once, then approves;
  the singletons report `done`), writes `outcome.json` and prints a
  stream-json `result` line; `Runner.ClaudeBin` is set to `os.Args[0]`. The
  session tests fake `claude` with a shell script the same way. Git is real:
  tests create a bare origin and a clone with one commit with
  `internal/testutil.SetupRepos`, and the workspace and scheduler tests
  create real worktrees, push to it, then assert the branch history and
  that no worktree is left behind.
- `skills.Manager.Git` is replaced with a copy of a fixture directory, so
  every supported repository layout is exercised offline.
- `install.sh` is checked with `shellcheck -s sh install.sh`. No test runs it,
  by design, because it must not reach the network; `cmd/bees/release_test.go`
  reads its text to pin the asset names it parses.
- A test that reads a repository file — a fixture, a doc page, anything that
  is not a Go source — must list that file in `dagger.toml`'s
  `includeExtraFiles`, or it passes locally and fails under `dagger check`:
  the check container mounts only Go sources.
- `bees.example.toml` at the repository root is a golden file: the `bees init`
  template with the placeholders left in. Never edit it by hand. After
  changing `internal/config/template.go`, regenerate it with
  `go test ./internal/config -update`; `TestExampleTOMLInSync` fails when the
  two drift.

### The QA playground

`.dagger/modules/qa-playground` is a Dagger module, written in dang, that
gives a shell to try `bees` in without a factory, a GitHub token or an
Anthropic key anywhere near it:

```sh
dagger call qa-playground playground terminal
```

The shell opens in `/work/playground`, a one-commit Go module whose `origin`
is `https://github.com/busybees-sandbox/playground.git`, a repository that
does not exist and is never fetched. On the PATH is a `bees` built from your
working tree, and the project already has the `bees.toml` and `.bees/` that
`bees init --repo busybees-sandbox/playground --default-branch main` writes.
`bees status`, `bees doctor` and the other commands run as they would
anywhere.

`gh` and `claude` on that PATH are stubs, not the real programs. Each answers
`--version` with a plausible version, so the toolchain checks have something
to read, and refuses everything else with one line naming the playground:

```
$ gh pr list
gh: this is a stub in the bees QA playground, not the real gh; it reaches nothing
```

Expect `bees doctor` to report what that implies: every GitHub check fails
against the stub, `remote reachable` fails because the remote does not
exist, and `worktree` fails because there is no `origin/main`. `bees init`
itself hits the stub too, at its `gh label create` step, which is why the
playground tolerates that command's exit code and then checks that
`bees.toml` was written.

To try bees against a project of your own, pass its directory. It gets a git
repository with one commit if it has none, and the sandbox `origin` if it has
no remote called that:

```sh
dagger call qa-playground playground --project ../my-project terminal
```

A headless session has no terminal to attach to. `script` runs a shell
script in the playground instead and returns its combined stdout and
stderr, with the exit status appended, so a failing probe is still
readable; the call itself succeeds even when the script does not:

```sh
dagger call qa-playground script --contents "$(cat probe.sh)"
dagger call qa-playground script --project ../my-project --contents 'bees status'
```

The module contributes no checks: `dagger check` runs the three Go checks
and nothing else. It is registered in `dagger.toml` as `qa-playground`
without the `dang` SDK helper module (`github.com/dagger/dang-sdk`), which
`dagger module init dang` installs for scaffolding and which would add a
`generate` check of its own. The module runs on the engine's built-in dang
runtime and needs no generated files.

### Go and markdown style

Match the style of the surrounding code. For markdown, follow the rules in
[CLAUDE.md](CLAUDE.md#documentation).

## Contributor workflow

1. Branch, make your change, and keep `dagger check` green as you go.
2. Open a pull request. There is deliberately no `push` or `pull_request`
   workflow: `.github/workflows/release.yml` is the only one, and its only
   trigger is a `v*` tag. PR #116 added a CI-on-push workflow and PR #121
   reverted it sixteen minutes later. All dagger checks run automatically
   on pull requests with Dagger Cloud Checks.

3. A reviewer goes through the PR. Address feedback and keep `dagger check`
   green until it is approved and merged.

Maintainers cutting a release should read [docs/releasing.md](docs/releasing.md).
