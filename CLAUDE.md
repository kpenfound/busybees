# busybees — notes for Claude Code

busybees (`bees`) is a Go CLI that orchestrates a staff of headless coding-agent
sessions (product manager, project manager, developers, reviewers, QA) building a
GitHub repository. Read `docs/architecture.md` before changing the scheduler.

## Build and test

- Everything builds and tests through Dagger.
- `dagger check` runs `go:lint-all`, `go:test-all` and `go:generate-all` (from the
  official `github.com/dagger/go` module in `dagger.toml`) for both the root and
  `core/` modules. Run it before committing.
  `dagger check` is the only validation a pull request may report as done.
- `dagger call qa-playground playground terminal` opens a shell with `bees` built
  from the working tree, a test project and stubbed `gh` and `claude`: the QA
  playground, a dang module in `.dagger/modules/qa-playground` described in
  `CONTRIBUTING.md`. It contributes no checks.
- Tests must never call the real `claude`, `codex`, `opencode`, `gh` or `docker`. `gh` is faked through
  `github.Client.Exec`; `claude`, `codex` and `opencode` are faked by the test binary itself (see `TestMain` in
  `internal/scheduler/scheduler_test.go`) or a shell script (`core/agent/agenttest`), and so is
  `docker` (`fakeDocker` in `core/agent` and, separately, in `internal/review`).
  `core/agent/agentbin.Resolve`, which every session and review agent goes through, refuses any other executable
  from a test binary (`testing.Testing()`), so a test that forgets its fake, or a test binary re-executed as
  `bees.test run`, fails with `ErrRealAgent` instead of running the real `claude` on the host. `bees run`, `tick`
  and `exec` refuse to start inside a session (`BEES_SESSION_DIR` set) for the same reason.
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

## Validation

- Run `dagger check` before declaring a change complete. Use the pinned experimental release:

  ```sh
  DAGGER_X_RELEASE=v1.0.0-beta.13 dagger check
  ```

- The factory exports `DAGGER_X_RELEASE` for its sessions.
- Never run `go test` on the host, in any role and for any purpose: iterating, a single test, a mutation check and reproducing a flake included. Tests start processes that leak onto the machine they run on, so they run only inside Dagger. `gofmt`, `go build` and `go vet` are fine on the host; `go vet ./...` type-checks test files without running them.
- Run one package or one test inside a Dagger container:

  ```sh
  DAGGER_X_RELEASE=v1.0.0-beta.13 dagger core container from --address golang:1.26-bookworm \
    with-directory --path /src --source . --exclude .git,.bees \
    with-workdir --path /src \
    with-exec --args=go,test,-count=1,-run,'TestA|TestB',-v,./internal/service \
    combined-output
  ```

  The arguments after `--args=` are the `go test` command line, separated by commas. Add `-count=5` there to reproduce a flake and `-race` to match the race detector.
- Add meaningful tests for changed behavior and regressions, especially state transitions, recovery, owner gates and execution boundaries. Use temporary directories and local repositories for filesystem and VCS tests.
- Tests must use fake agents, GitHub clients, providers and container engines. Never launch real model sessions, the live factory, remote pushes or pull requests from tests.
- Report the checks actually run and their results. If validation is blocked, state the exact blocker; do not report success.
- `dagger check` runs automatically on pull requests using Dagger Native CI

## Layout

- `cmd/bees` — cobra CLI (`init`, `run`, `tick`, `exec`, `status`, `cost`, `machine`, `kill`, `mail`, `notes`, `issue`, `done`, `mcp`, `doctor`, `config`, `skills`, `prompts`, `labels`, `templates`, `review`, `version`).
- `internal/config` — `bees.toml` schema, and the machine config that lists the `bees.toml` files of several projects and caps the developer slots across them with `max_developers` and sets `retention_period` for the projects that do not (`machine.go`: `LoadMachine`, and `DetectKind`, which tells the two apart by the machine config's top-level `projects` key; `Load` refuses a machine config with `ErrMachineConfig`) (commented-defaults template in `template.go`, the named config templates and the `setting` func that activates their keys in `templates.go`), global/role merging, labels, repo/branch derivation from the git remote (`resolve.go`).
- `internal/scheduler` — the loop (`reload.go`: `Reload` hands it a `bees.toml` read again, in force from the start of the next pass, refused when it changes a key the scheduler or its collaborators were built from, `fixedKeys`, which `docs/cli.md` lists beside the live view's keys): poll → deliver human feedback (`humans.go`: comments on an in-flight issue, an `@`-mention of the factory's login on any other issue, then reviews and comments on a PR) → reconcile labels → trim `ledger.jsonl` to `max(scheduler.retention_period, 24h)` (`ledger.go`) → dispatch developer workers (`developer.go`: develop → review → checks stages; `review.go`: the review itself, brief → angles → judge through `internal/review`'s `Runner` built from `roles.reviewer`, the brief and angle sessions read-only `CLIAgent` sessions with no MCP server, their named `brief_profile` and `angle_profiles` selecting execution fields after the role's size selection with sandbox ignored, `judge_profile` selecting all five fields for the ordinary reviewer session with reviewer-owned tools and permissions, the diff read from and the brief and angles run in an independent local clone of the worker's checkout (`cloneOf`, its base reference the merge base with the base branch), the artifact kept under the state directory's `reviews/`, followed by one factory session, the judge session, that posts every finding with `submit_review` and decides the verdict, a later round of the loop running no pipeline and verifying the first round's findings instead (`verifyReview`); `bestofn.go`: the first develop round as N concurrent attempts, one slot each, then one assembler session on the issue's branch that opens the pull request, after which the attempt branches are deleted; `shared.go`: the `core/ops.SharedPool` alias, the machine config's `max_developers`, one pool the schedulers of a daemon's projects take a slot of on top of their own for every worker, attempt and requested review, the refused queued and served in order so a freed slot goes round the waiting projects) → dispatch requested reviews (`reviewrequests.go`: the same review, then a reviewer session that posts it, for a pull request a person labelled `bees:review-requested`, or with `scheduler.review_assigned_prs` one the factory did not write, full passes only) → dispatch singletons (`singletons.go`) → file the factory-error drafts against `kpenfound/busybees` (`factoryerrors.go`: `drainFeedbackQueue`, gated on `scheduler.report_factory_errors`, full passes only). `events.go` adapts the core non-blocking event stream (`Subscribe`) for views, alongside `status.json`.
- `internal/daemon` — several projects' schedulers in one process: `Daemon.Run` starts each `Project` on its own goroutine and waits for all of them (while `Reload` is open, it stays alive until cancellation and reconciles replacement lists by project path; after it closes, it returns when active projects finish), a project that fails to start, errors or panics logged and returned as its own `ProjectError` while the others run on; `HardStop` reaches every started loop. It knows nothing of how a scheduler is built: `cmd/bees`' `machineDaemon` builds one per project a machine config lists, the way a single-project run builds its own, each with its own `<state_dir>/bees.log` through `logging.Logger.Tee` and all of them on one `scheduler.SharedPool` when the machine config caps `max_developers`, and `runMachineWithTUI` draws the live view over it (`machineViews`: one `tui.Project` per project, wired before the project's scheduler exists — its events forwarded into the view's channel once it has started, its status.json and mailbox read from its state directory, and its `Kill` and `Send` refusing until then). `bees run` selects this path for a machine config; SIGHUP reloads its project list and live selector, retaining removed sources through their drain, and hands every running project its `bees.toml` read again (`cmd/bees/reload.go`: `machineReloader`, all or nothing, the same reload the live view's `r` key runs; `machineRuntime` keeps the running loops by config path for it). `-d`/`--daemon` detaches either mode (`background.go`: `MachinePIDFile` next to the machine config or `ProjectPIDFile` under the project state dir, a lock inherited by the child, and appended console logs beside it).
- `internal/issues` — `bees issue create/link`: visible, labelled, sub-issue of a feature, milestone inherited.
- `internal/duplicates` — `Find`: the existing issues a title and body would duplicate, scored by local word overlap over every issue in the repository, open and closed, whatever its labels (`github.Client.ListAllIssues` ignores `[filter]` on purpose). Explicitly invoked by the callers that file bugs (QA's `file_bug`, the upstream feedback loop's `drainFeedbackQueue`), never by `issues.Create`: a triage split and a feature's work items look alike on purpose. `internal/review` also supplies `Score` and its threshold to `core/review` for finding deduplication and reviewer-note matching.
- `core/ops` — retry decisions, ledger storage and budget signals, capacity and
  degraded episodes, the event bus, fair shared slots and tick/wake primitives.
  Busybees supplies configuration, work mapping, logging and escalation policy;
  `internal/state` migrates schema before delegating ledger operations.
- `core/agent/agentbin` — `Resolve`, the one place a session (`internal/session`) or a review session (`internal/review`'s `CLIAgent`) turns an agent name into the executable it runs, and the guard that keeps a test binary from running a real one.
- `core/agent/procs` — PID files, process-table inspection and container cleanup.
  Busybees supplies its command markers and `bees.session` container label.
- `internal/testutil` — local bare git remote + clone for tests.
- `core/agent` — standalone Go module's headless session runner: backend commands
  and streams, sandbox/container execution, timeout/cancellation, result/outcome
  files and interruption inspection. `grants.go` is the capability contract every
  request carries (`Grants`: env allowlist, tools, ro/rw mounts, VCS) and the
  `Boundary` that verifies it before launch (`HostBoundary`, `ContainerBoundary`). `confine.go` is the host's
  confined mode (`Profile.Confine`): the operating system holds the process to its mounts and `SystemPaths` through a
  `Confiner`, Landlock on Linux (`confine_linux.go`), Seatbelt on macOS (`confine_seatbelt.go`: the profile and the
  `sandbox-exec` start, untagged so the gate tests them; `confine_darwin.go` selects it), `ErrUnsupported` where there
  is none; the Landlock enforcement tests skip on a kernel without Landlock and run in the VM `CONTRIBUTING.md`
  describes, and Seatbelt enforcement is checked by hand with the recipe there. `enforce.go` is the runner an embedder
  takes a held turn from (`Enforcer`: `NewHostNone`, `NewHostClaude`, `NewContainer`; `Prepare(ctx, grants)` → `Session`
  with `Policy`, `Run` and `Release`): host kinds are confined turns, a container session binds stand-ins over its
  image's VCS executables (`containermask.go`, `ContainerBoundary.Masks`), and `Run` refuses a turn the reported policy
  does not describe (`ErrPolicyChanged`). Busybees' own sessions go through `Runner.Run`. `agenttest` supplies fake
  agents, Docker and a host MCP server, and `agenttest/enforcertest` an `Enforcer` that starts no process; `agentbin`
  guards all agent and engine launches in tests.
- `internal/session` — busybees adapter: `ProfileForRole` projects a resolved role
  into execution settings; `grants` turns role policy into `agent.Grants`
  (`HostEnv`, `ProviderEnv`, `VCSEnv`, mounts by sandbox); `Runner` supplies the `BEES_*` context, GitHub and git
  identity, built-in MCP entry and outcome policy. Session work markers use
  core's opaque identity; GitHub touched-issue artifacts stay here. Skills acquisition stays in `internal/skills`, behind
  core's `SkillPreparer` interface.
- `internal/prompts` — role prompts embedded in the binary (`system/*.md`, `task/*.md`) rendered with `text/template`, so a prompt change reaches no session until `bees` is rebuilt and `bees run` restarted; `project.go` appends the project repository's own `bees/prompts/common.md` and `bees/prompts/<role>.md`, read from the session's worktree at session start, which need no rebuild.
- `core/mcphost` — generic MCP server metadata, typed role-scoped registration,
  in-memory clients, stdio/HTTP lifecycle, per-turn endpoints (`lease.go`:
  `Start`/`StartOn` → `Endpoint` and `Lease`, `StartMemory` for tests) and
  caller-supplied outcome reporting.
- `internal/mcpserver` — busybees tool registration and policy on core/mcphost: the built-in MCP server (`bees mcp serve`) added to every session as `bees`, backed by the same code as the CLI: `mail_send`, `mail_list`, `issue_create`, `issue_link`, `issue_view`, `pr_view`, `comment`, `report_factory_error` (a draft under `<state_dir>/feedback/` when `scheduler.report_factory_errors` is on, otherwise a result saying nothing was recorded), `notes_read`, `notes_write` (a role's notes as one text, read whole and replaced whole, through the Notes backend `cmd/bees/mcp.go` wires from `notes.backend`: the file under `<state_dir>/notes/`, or Neo4j Agent Memory through `internal/nams`; the prompt does not carry them) and `done` go to every role; role-scoped are `issue_edit_body` (both managers), `issue_set_state` (project manager), `issue_question` (product manager), `submit_review` (reviewer: its judge session posts the review's findings with it, as a comment review on a developer's pull request and with the verdict as the event on a requested one) and `file_bug` (QA: `issue_create` for a bug report, refused when `internal/duplicates` finds the bug already filed). The name `bees` is reserved in bees.toml.
- `internal/tui` — the live view `bees run` draws in a terminal (bubbletea + lipgloss): the Now, Recent, Needs human, Approved PRs and Queues panels, fed by `scheduler.Subscribe` and `status.json`, plus its keys (↑/↓ select, `enter` watches the selected session, `o` opens on GitHub, `k` stops the selected session, `r` reloads the configuration from disk through `Deps.Reload`, `p` pauses or resumes dispatch across the whole factory (`Deps.SetPaused`: `Scheduler.SetPaused`, `Daemon.SetPaused`), `q`/ctrl-c stops the factory — the work in flight finishes, a second press stops the running sessions too) and a session view (`session.go`) that tails one session's `transcript.jsonl` and queues a message from `human` for the next session on that work item. A daemon's view (`RunMachine`, `Deps.Projects`: one `Project` per project, each with its own event stream, status.json, mailbox, `Kill` and `Send`) adds a selector the ←/→ keys cycle through (`project.go`: all, then each project) that filters every panel to one project, or shows every project's rows together with a project column and the Queues panel summed; a single-project view has neither. Drawn only when stdout is a terminal and `--no-tui` was not given (`tuiMode` in `cmd/bees`), and it silences console logging while it is up. `theme.go` is the only file in the package that names a colour (rows are painted by role and by outcome class, panels by their title and border) and `TestOnlyTheThemeNamesColours` keeps it that way.
- `core/review` — standalone review pipeline over supplied context and diff:
  distillation, size-based concurrent angles, deterministic judge, findings,
  reviewer-note filtering and artifact persistence. The caller supplies a
  reference type (display, URL and opaque rule scope), agent implementations
  and profiles, comparison policy and working directories. Busybees keeps
  GitHub acquisition/publication, configured CLI agents, notes storage and triage
  in `internal/review`; its reference type preserves the existing artifact JSON.
- `internal/review` — `bees review`, the pull request review tool, which works on any GitHub pull request and reads no factory state; the factory's reviewer runs the same `Runner` from `roles.reviewer` (`internal/scheduler`'s `review.go`). Its configuration surface is two files, neither of them `bees.toml`, neither versioned or migrated, each loading as the defaults when it is not there and failing with an error naming the key when it has one the schema does not or a value outside its enum: the global `~/.config/bees/config.toml` (`config.go`: provider/model, per-size angles and per-step models (`angles`, `brief_model`, `angle_models`, and `judge_model`, which has no effect because the judge is not a session), reviewer notes and review artifact paths, default output mode, GitHub token) and the project's `context.toml` (`project.go`: which angles run, extra style sources, category severity overrides, which context sources are gathered, `generated` patterns). `ref.go` turns a URL, `owner/name#123` or a bare number into the pull request under review; `context.go` gathers the sources `context.toml` enables into a `Bundle` of raw items, each source a `Collector` bound to its name; `sources.go` implements the built-in ones (`diff`, `pr_body`, `linked_issues`, `style_files`, `callers`) and the file-gathering source a project declares itself; the `diff` source reads the diff from the checkout `Gather` makes under the artifact directory (`Input.Diff`, `checkoutDiff`: the head against its merge base with the base branch, with no limit on changed files) and through `gh pr diff` when there is none, and without its generated files (`generated.go`, `excludeGenerated` in `Input.Diff`: a `// Code generated ... DO NOT EDIT.` line in the file's head read from the checkout, `linguist-generated` in the checkout's `.gitattributes` through `git check-attr`, or a `generated` pattern in `context.toml`; without a checkout the patterns alone, and `Bundle.Skipped` says so; the files taken out are `Bundle.Excluded` with their line counts, carried into `Brief.Excluded` for every session to read, and `core/review`'s `Exclude` drops a finding anchored in one, in the runner and in `Queue.Ask`). A source that could not read something records it in `Bundle.Skipped` rather than failing the review. `core/review/distill.go` runs the first session of a review, the distiller, over that bundle: it produces the `Brief` in `core/review/brief.go` (`WriteBrief`/`ReadBrief`), which is what every angle session reads, with the diff, instead of the raw bundle. `agent.go` is the seam those sessions run through, `CLIAgent` running `claude -p` or `codex exec` read-only: no factory session runner, no MCP server, no tool that writes, runs or fetches anything. `core/review/angles.go` fans out the angle sessions after the distiller, one per angle the brief's size calls for (`sizeAngles`: `xs` and `s` two, `m` and `l` four, `xl` five, unless `config.toml`'s `angles` replaces a size's list; `angle_models` gives an angle its own model) that `context.toml` enables (`quick_general`, `general`, `docs`, `test_coverage`, `acceptance_criteria`, `side_effects`), all at once through that same seam, each told its own instructions from `core/review/prompts/angles/`, the brief and the diff; what each came to is an `AngleRun` persisted as `angles/<angle>.json` in the artifact directory (`WriteAngleRun`/`ReadAngleRuns`), enough for `Angles.Resume` to reopen the session with a follow-up question: the agent that ran it, its session id and the directory it ran in. That directory is the one `checkout.go` makes as the context is gathered (`Pipeline.Checkout`), once per review: a container of its own (Alpine with git, built with `docker build`, nothing of `internal/session` imported) clones `refs/pull/<number>/head` of the pull request's repository over HTTPS into the artifact's `checkout/`, fetches the base branch with history beside it and points `CheckoutBaseRef` at the merge base (the base branch's tip, recorded in `Bundle.Skipped`, when there is none), and exits, `github.token` reaching it as an environment variable and never as an argument; when `docker` is missing or the build or clone fails the diff is read through gh, the angles run in the machine's checkout of the repository when there is one, else in the empty `scratch/`, and the review says so and goes on. `core/review/findings.go` is the findings schema (`Finding`: id, angle, session id, category, severity, file, line range, side, title, body, suggestion, evidence, sources) and `ParseFindings`, which reads one angle's answer into it; `core/review/judge.go` is the merge step, deterministic code and not a session (`Judge` over the angle runs, `Merge` over any findings): severities normalised to the four the schema has and pinned by `context.toml`'s category overrides, a category pinned `off` dropped, the same finding from two angles kept once (same place and same category, or alike by `internal/duplicates.Score`), most severe first, ids a hash of what places a finding. `artifact.go` keeps reference-based directory naming and aliases core artifact types; it documents the review artifact directory, one per review under the global file's `storage_path` (`ArtifactDir`, `LatestArtifactDir`): `brief.json`, `angles/`, `findings.json` and `triage.json`, written as the review goes and read back whole by `ReadArtifact`. `notes.go` is the reviewer notes, the global markdown file dismissals accumulate in (`AppendDismissal`, one `[repo] [angle] [category] reason` line per dismissal) and `Notes.Consolidate` turns the patterns that repeat into rules, written between two markers by `bees review consolidate` (`cmd/bees/review.go`) and never deleted or reworded there; `core/review/noise.go` is the filter those rules are for, `Filter` dropping or ranking down a finding a rule is about before triage sees it and recording it as `Silenced`, and the same rules telling an angle session what has been dismissed from its angle before (`Angles.Rules`). `triage.go` is the triage queue (`Queue` over an `Artifact`): `Pending`, the findings nothing has been decided about, and the four actions, `Select` (with the comment text as written or edited), `Dismiss` (the reason appended to the reviewer notes first), `Defer` (recorded nowhere but the artifact) and `Ask` (`Angles.Resume` on the angle that found it, the answer recorded beside the finding, a finding the answer turns up merged, filtered and queued); every action is written into `triage.json` before it returns and the latest select, dismiss or defer on a finding is the one in force, so a triage stopped is picked up where it left off. `console.go` drives a queue from an input and an output one line at a time, with the comment text edited in `$VISUAL` or `$EDITOR`: what `bees review triage <pr>` (`cmd/bees/review.go`) runs over the latest review of a pull request at a stdout that is not a terminal, or with `--no-tui`; at a terminal it opens `internal/reviewtui`'s screen over the same queue instead. `factory.go` is factory mode, `AgentTriage` driving the same queue from an agent session instead of a terminal (`--agent` and `--instructions` on `bees review <pr>` and `bees review triage <pr>`): rounds of new read-only sessions, each told the findings still undecided, the answers to its asks and what its last answer could not do, and answering with the decisions to take and, when the command leaves the end to ask, how the review ends. `run.go` is `Runner`, the acquisition adapter around `core/review.Runner` for one pull request (`NewRunner` from the global file, `Run` gathering, distilling, running the angles, judging, filtering and writing the artifact as it goes, an angle that failed logged and skipped, every angle failing an error, a `Progress` hook told from each angle's goroutine as its session starts and ends, which `Angles.Progress` is), which `bees review <pr>` runs before triage; what that command shows while the runner runs is `cmd/bees/reviewprogress.go`: at a terminal (`tuiMode`) a small bubbletea view drawn in place with no alternate screen, the runner's log lines with one row per angle, a spinner while its session runs and a mark when it ends, fed by `Log` and `Progress`, and otherwise the same lines on stdout plus one per angle as it starts and ends; its colours are in `cmd/bees/theme.go`, the only file in that package that names one, and `TestOnlyTheThemeNamesColours` there keeps it that way. `output.go` is the end of a review, one of the five output modes the global file's `output` key or `--post`/`--report` on either command choose, and `Console.Choose` asks for when it is `ask`, or the agent chooses in factory mode: `Post` submits what triage selected as one review in one call (`github.Client.PostReview`: a comment on the lines the pull request's diff has, `Anchors`, with a suggestion block on the new side, the rest folded into the summary) for the three modes that post, `Report` renders the same selections as markdown, and discard does nothing. `diffview.go` is the diff a person reads beside a finding, data for a screen and nothing drawn: `NewDiffView` sections a unified diff by file, then by hunk, then by line (its kind and its number on each side), through `walkDiff`, the one diff reader in the package, which `ParseAnchors` walks too; it marks a finding's lines only when `Anchors.Has` says the diff has them on its side, says so in `DiffView.InDiff` when it does not, and puts a suggestion on the last marked line only on the new side.
- `internal/reviewtui` — the triage screen (bubbletea + lipgloss): the diff, sectioned by `review.NewDiffView` with the finding's lines marked, beside the finding, driving `review.Queue` the way the console does, with the console's keys (`s`, `e`, `d`, `f`, `a`, `n`, `q`, `?`) each acting the moment it is pressed and every decision written into the artifact before the next finding is shown; edit takes the comment text on screen, ask runs `Queue.Ask` off the screen's goroutine and shows the answer under the finding. `Run(ctx, diff, queue)` is given the diff as text and fetches nothing. It depends on `internal/review` and not on `internal/tui`; `theme.go` is the only file in the package that names a colour (its own palette: diff lines, severities) and `TestOnlyTheThemeNamesColours` keeps it that way.
- `internal/nams` — the Neo4j Agent Memory REST client behind the notes tools with `notes.backend = "neo4j"`: one conversation per role (`userId` `bees-notes-<role>`), each write a message holding the whole notes text, a read the newest message; no Neo4j driver, no SDK, nothing deleted. Faked in its tests with an `httptest` server (`fakeNAMS`).
- `internal/mail` — local JSON mailbox; the only channel between roles. `internal/feedback` — the queue of factory-error drafts `report_factory_error` writes (`Draft`, `Queue.Add`/`List`/`Remove`); the scheduler files them against `kpenfound/busybees` and removes them (`factoryerrors.go`).
- `internal/github` — thin `gh` wrapper. `internal/workspace` — git worktrees. `internal/skills` — skills by git URL → `--plugin-dir`.
- `core/vcs` — workspace directory, optional VCS mounts and provider lifecycle.
  `internal/workspace` implements it with git worktrees; core does no git discovery.
- `core/work` — opaque work keys, caller tags and collision-resistant filenames.
- `internal/ghwork` — busybees GitHub key/tag mapping; `internal/mailfmt` supplies
  GitHub display fields to the generic mailbox formatter.
- `internal/statemigrate` — locked, restart-safe runtime state upgrade; state access
  runs it before reading or writing, with the schema marker published last.
- `internal/state` — state dir layout (`mail/`, `notes/`, `sessions/`, `reviews/`, `issues/`, `status.json`).
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

## Documentation

- Docs describe the current state. They never describe a change from previous
  behaviour: no "now", no "no longer", no "as of v0.2".
- Docs instruct, they do not sell. No marketing language, no piled-up adjectives.
- Prefer an example over an extended explanation.
- `README.md` holds a brief overview of the project, installation, and a list of
  key features, each one a brief description plus a link to where it is
  explained. Nothing else.
- `CONTRIBUTING.md` holds testing instructions, style rules and the contributor
  workflow.
- `docs/` is product documentation for people using bees. It does not describe
  the development of this project. `docs/releasing.md` is the exception and
  stays where it is, because `cmd/bees/release_test.go` pins the asset names it
  documents.
- The `unslop` writing skill applies to every markdown file.
