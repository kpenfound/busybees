# Core factory components

`github.com/kpenfound/busybees/core` is a standalone Go module. It imports no
package from the busybees root module. Build and test from this directory:

```sh
go build ./...
go test ./...
```

The repository's `dagger check` builds, lints, generates and tests both modules.
The root module consumes this one through its `replace ... => ./core` entry.

## Execution boundary

`agent.Runner.Run` takes an `agent.Request` with an execution-only `Profile`:
backend, models, effort, tools, prepared MCP entries, sandbox, shell and
environment. The caller selects profiles before running; `Profile.Fallback`
is the profile the session runs on instead when it has no capacity, agent
included, with its own fallback after it: `ops.SelectProfile` walks the
chain for a retry, and the claude backend passes a claude fallback's model as
`--fallback-model` so claude switches to it within the session.

The caller also supplies:

- `EnvironmentPrefix` to strip stale inherited variables, even granted ones, and
  the request's `Env` for session context and credentials. An empty prefix
  strips nothing.
- `ValidOutcomes` on each request and when calling `agent.Report`. A nil set
  accepts any status; an empty, non-nil set accepts none. Validation errors list
  the supplied statuses. Workflow-specific requirements belong to the caller.
- Backend name prefixes, container labels, writable mounts, and optional
  `HostMCP` launch details for a server that runs on the host during a container
  session. A container session without `HostMCP` starts no host server.
- `SandboxDomains` for Claude's network permissions. `VCSAccess` asks for the
  workspace's VCS mounts and the request's `VCSEnv` / `VCSContainerEnv` identity,
  credentials and configuration; the request's `Grants.VCS` must allow it.
  `ContainerEnv` and explicit caller mounts remain caller-controlled.
- `Grants`, the session's complete capabilities (below). A request without
  them does not run.
- An optional `SkillPreparer` and read-only skill cache mounts. Acquisition,
  caching and configuration policy stay with the caller.

MCP entries contain public context in `Env` and credential names in `EnvVars`.
Pass credential values in the request environment. `MCPEntries` can expand
configured environment references before entries are passed to the runner;
prepared entries are not expanded again. `BearerTokenEnv` names the process
variable holding a remote server's bearer token; each backend writes its native
environment reference. `HostMCP` uses this field for its generated token.

Codex receives the session directory through
`-c shell_environment_policy.set.<prefix>SESSION_DIR=...` even without MCP.
Orphan scans use `procs.CodexMarker(prefix)` with the same environment prefix;
`LegacyCodex` can also match a caller's older MCP-based marker. With the default
empty prefix, `procs.Find` needs no marker overrides.

Discovery deletes the stale pid, container id and server pid files it reads,
the cleanup a caller stopping sessions wants. A caller that only inspects
asks through `procs.Finder{ReadOnly: true}`, which reports the same sessions
and leaves every file in place.

A pi session (`AgentPi`) has no MCP support of its own: the runner loads
`PiMCPAdapter` (`npm:pi-mcp-adapter`) and then `Profile.PiPackages` with
`-e`, writes the MCP entries to `pi-mcp.json` in the session directory, and
passes it as the adapter's `--mcp-config` with `PI_MCP_CONFIG_MODE=exclusive`.

## Grants

`agent.Grants` lists everything a session may have:

```go
req.Grants = &agent.Grants{
	Env:    []string{"PATH", "HOME", "ANTHROPIC_API_KEY", "LC_*"},
	Tools:  []string{"Read", "Edit", "mcp__tools"},
	Mounts: []agent.Mount{{Path: "/", Access: agent.ReadOnly}, {Path: work, Access: agent.ReadWrite}},
	VCS:    false,
}
```

- `Env` is the complete allowlist of variable names; `NAME*` is a prefix. A
  host variable is inherited only when listed, and a variable the profile or
  request sets must be listed. Without `VCS`, an entry that could match a gh,
  git or SSH-agent variable (`GH_*`, `GITHUB_*`, `GIT_*`, `GCM_*`,
  `SSH_AUTH_SOCK`, ...) is refused.
- `Tools` are built-in tool names, or `agent.ToolsAll`, and `mcp__<server>`
  for each MCP server. The profile's `AllowedTools`, `MCP` and `HostMCP` may
  name less, never more; `DisallowedTools` only narrows. Claude is started
  with `--tools` when not every built-in tool is granted; codex, opencode and
  pi cannot restrict their built-in tools and need `ToolsAll`.
- `Mounts` are absolute, clean, existing paths, `ReadOnly` or `ReadWrite`,
  judged after their symbolic links are resolved; `Within` confines them all.
  The working directory must lie inside one. Without `VCS`, a writable mount
  inside or holding `.git`, `.hg`, `.jj` or `.svn` is refused.
- `VCS` grants version control. Without it, a host session finds `gh`, `git`,
  `hg`, `jj` and `svn` shadowed on `PATH` by stand-ins that exit 126, and
  Claude's sandbox settings deny them.
- `DaggerEngine` grants a Dagger engine, `unix://<socket>` or
  `tcp://<host>:<port>`, to a `SandboxSbx` session whose `Profile.Dagger`
  asks for that engine. Every boundary refuses it otherwise: granted to a
  profile that does not ask (`ErrUnsupported`), asked for without the grant
  or with another engine granted (`ErrNotGranted`), or in any other mode
  (`ErrUnsupported`).

`Runner.Verify(req)` checks a request without starting anything and returns
the `Turn` it would run: its environment, tools and resolved mounts. `Run`
calls it first. `HostBoundary`, `ContainerBoundary` and `SandboxBoundary`
implement the `Boundary` interface. The host enforces nothing of the mounts by itself, so
it refuses what it cannot enforce (`ErrUnsupported`):

| Sandbox | Mounts it needs |
|---|---|
| `none` | `/` read-write, and `VCS`: nothing keeps an unsandboxed process out of VCS metadata |
| `claude` | `/` read-only and the working directory read-write; every other read-write mount and every `Runner.AddDirs` entry, which must be granted read-write, is passed with `--add-dir` |

### Confined host sessions

`Profile.Confine` has the operating system hold a host session, `none` or
`claude`, to its mounts. Neither row of the table applies to it:

```go
req.Profile.Confine = true
req.Grants = &agent.Grants{
	Env:   []string{"PATH", "HOME", "TMPDIR", "ANTHROPIC_API_KEY"},
	Tools: []string{"Read", "Grep", "Bash"},
	Mounts: []agent.Mount{
		{Path: pinned, Access: agent.ReadOnly}, // the working directory
		{Path: sessionDir, Access: agent.ReadWrite},
		{Path: agentHome, Access: agent.ReadWrite},
	},
}
```

- The session reads, writes and executes inside its mounts, by their access,
  and reads the system paths. Everything else is refused by the kernel, for
  the agent and every process it starts, the MCP servers it launches
  included: grant what they need. A read-only mount inside a read-write one
  stays read-only.
- The working directory may be `ReadOnly`.
- The system paths are `HostBoundary.SystemPaths` (`Runner.SystemPaths`), or
  `agent.DefaultSystemPaths()`. On Linux: `/usr` and the directories linked
  into it, the files under `/etc` a program needs to load libraries,
  resolve names and verify certificates, `/proc`, the CPU and cgroup
  information under `/sys`, and `/dev/null`, `/dev/zero`, `/dev/full`,
  `/dev/random`, `/dev/urandom` and `/dev/tty`. On macOS: `/usr`, `/bin`,
  `/sbin`, `/System`, the files under `/etc` a program needs to resolve
  names, verify certificates and know the time, the time zone data, and
  `/dev/null`, `/dev/zero`, `/dev/random`, `/dev/urandom`, `/dev/tty` and
  `/dev/dtracehelper`. No home directory, no temporary directory, and
  nothing of `/opt` or Homebrew: grant the ones the agent needs and point
  `HOME` and `TMPDIR` at them. The runner adds the agent's executable, and
  nothing else it loads (a `node` the executable runs, for one).
- Without `VCS`, the files `gh`, `git`, `hg`, `jj` and `svn` resolve to, in
  the session's `PATH` and in the usual system directories, cannot be read
  or executed: by name, by absolute path, from a shell, through a symbolic
  link or a hard link beside them, or as a copy. Git's directory of
  subcommand programs goes with `git`. A mount that is one of them, or a
  hard link to one, is refused (`ErrNotGranted`). A hard link to one in any
  other directory of a mount or a system path is not found, and neither is
  a VCS executable outside the directories searched. The stand-ins on
  `PATH` stay. With `VCS` nothing is denied, and `/` is not needed for it.
- The session directory (or `Runner.SessionsDir` before it exists) and, for
  a profile with skills, `Runner.SkillMountDirs` must lie inside a mount.
- Under Landlock, a directory that holds a denied executable, or a
  read-only mount inside a read-write one, is allowed entry by entry
  instead of as a whole. It can be listed, and nothing can be created at
  its own level.
- Under Seatbelt, the metadata of any file that is not denied can be read
  (`stat(2)`, as under Landlock), and so can the entries of `/`, which the
  loader lists before any program starts; nothing below `/` comes with it.
  A denied path, and a hard link to a denied executable, cannot even be
  `stat`ed. Hard links are found in every directory above the denied
  executable, up to the mount or system path that allows it.

`Confiner` is what enforces it: `Check` says whether it can, `Start` starts
the process under it. `HostBoundary.Confiner` (`Runner.Confiner`) replaces
the platform's: Landlock on Linux, Seatbelt on macOS (a profile
`/usr/bin/sandbox-exec` applies before it executes the agent) and nothing
anywhere else. A session nothing can confine is refused with
`ErrUnsupported` and never run with less. When `sandbox-exec` cannot apply
the profile it exits with an error and the agent never starts.

| Platform | Confined `none` | Confined `claude` |
|---|---|---|
| Linux, Landlock version 3 or later (kernel 6.2) | enforced | refused: Claude's box is bubblewrap, which needs `mount(2)`, and Landlock refuses every mount |
| Linux without Landlock, or an earlier version, which cannot refuse `truncate(2)` | refused | refused |
| macOS | enforced | enforced: Seatbelt holds `claude` and its box; macOS refuses to apply a second profile inside the first, so Claude's own box may not start |
| macOS without `/usr/bin/sandbox-exec` | refused | refused |
| everything else | refused | refused |

`CONTRIBUTING.md` has the manual check of macOS enforcement, which no test
runs.

`ContainerBoundary` verifies the environment, tools and mounts the same way,
and the container gets its grants and nothing else:

- Binds: every granted mount at its real path and at the path it was
  granted by, `--mount ...,readonly` for `ReadOnly`. `/` is refused, and so
  is a path the engine's `--mount` cannot take (a comma, quote or newline).
- Paths the runner uses must lie inside a grant, or the request is refused
  with `ErrNotGranted`:
  - read-write: `Runner.MountDirs` and the workspace's `VCS()` mounts;
  - any access: the working directory, which is bound `readonly` when it is
    granted `ReadOnly`, the session directory (or `Runner.SessionsDir`
    before it exists) and, for a profile with skills,
    `Runner.SkillMountDirs`.
- Environment: the agent's credential (`AgentCredentials`) from the host
  when granted, the variables the request sets, `ContainerEnv` and, with
  `VCS`, `VCSContainerEnv`, each of which must be granted, then `HOME`. No
  other host variable is inherited, granted or not.
- Without `VCS`, the command runs through `/bin/sh -c` with the stand-ins'
  directory in front of the image's `PATH`. That denies a VCS executable by
  its name. `ContainerBoundary.Masks` are read-only binds laid over paths of
  the image for such a turn; `Runner.Run` sets none, and a `NewContainer`
  session (below) sets one over each VCS executable its image holds.

`SandboxBoundary` (`sandbox = "sbx"`, a Docker Sandbox the `sbx` CLI
creates for the profile's agent, from `SbxTemplates[agent]` unless
`SandboxImage` names a template) binds what `ContainerBoundary` binds, each bind a
workspace of `sbx create` at its destination (`:ro` for `ReadOnly`). `/`
is refused, and so is a destination holding a colon, which sbx would read
as the access; the source is never passed to sbx, and commas and quotes
are accepted. It builds the environment the same way with two
differences: no agent credential is forwarded, because the sandbox's proxy
injects the one stored with `sbx secret set`, and no `HOME` of its own is
set, because the sandbox has one; a `HOME` the request sets is passed by
value. Without `VCS` the command runs behind the same `/bin/sh -c`
stand-in wrapper; nothing masks a VCS executable of the template reached
by its path. The runner creates the sandbox before the host server starts,
runs the command through `sbx exec --interactive` with the variables by
name, removes the sandbox with `sbx rm --force` when the session ends, and
records its name in `procs.SandboxNameFile` meanwhile. With
`Profile.Dagger`, it installs the Dagger CLI at `Dagger.Version` with a
setup `sbx exec` once the sandbox exists, forwards a socket engine from a
port on `127.0.0.1` for the session's lifetime, and sets
`EnvDaggerRunnerHost` to the engine's address: at `host.docker.internal`
for a socket or a loopback TCP engine, and as written for a TCP engine on
any other host. No `Enforcer` prepares this kind.

## Enforced turns

`Runner.Run` verifies a request and runs it under whatever its profile asks
for, which on the host may be nothing. An `Enforcer` runs only turns that
something other than the agent holds to their grants. There is one
constructor per sandbox kind, each taking the `Runner` whose fields it uses:

```go
// or NewHostClaude(runner), NewContainer(runner, image)
enforcer := agent.NewHostNone(runner)
session, err := enforcer.Prepare(ctx, grants)
if err != nil {
	return err // wraps ErrUnsupported where nothing enforces the kind
}
defer session.Release(ctx)
p := session.Policy()
if p.Reads("/home") || p.Writes(pinned) || p.Runs("/usr/bin/git") {
	return errors.New("not the policy this role was meant to have")
}
result, err := session.Run(ctx, request)
```

- `Prepare` checks the grants on their own, with no request, and asks the
  platform whether it can enforce them. `NewHostNone` and `NewHostClaude`
  are confined host sessions (above): the same `Confiner`, the same system
  paths, and the platforms of the table above, so `NewHostClaude` prepares
  on macOS and, on Linux, only with a `Runner.Confiner` that can hold it.
  `NewContainer` binds the mounts as `ContainerBoundary` does. Without
  `VCS` it also runs the image once (`docker run --rm --network none
  --entrypoint /bin/sh <image> -c <script>`, no mounts) to find the files
  `gh`, `git`, `hg`, `jj` and `svn` resolve to in the image's `PATH` and the
  usual system directories, and git's directory of subcommand programs, and
  binds a stand-in that exits 126 over each file and an empty directory over
  that directory. A VCS executable is then denied by absolute path and from
  a shell too. One anywhere else in the image, a hard link to one, or one
  the turn downloads is not found. The stand-ins are the runner's own files
  under the temporary directory, which the engine must be able to bind;
  `Release` removes them. An image the engine cannot look into is not
  prepared.
- `Policy` is what the session enforces: the sandbox kind, the environment
  allowlist, the built-in tools the agent is started with (`nil` is all) and
  the MCP servers it may be given, the mounts, `System` (a host session's
  system paths and the executables of the agents the runner names), `VCS`,
  the names shadowed on `PATH`, the `Denied` paths, and a container's image
  and `Binds`. It holds names and paths, never a variable's value.
  `Reads(path)`, `Writes(path)`, `Runs(path)` and `Allows(tool)` answer for
  one path or tool: the innermost mount decides, a host session's system
  paths add to it, and a denied path, or on the host a hard link to a denied
  file in a directory that holds a denied path, is not reached. A turn gets
  no more than they say, and under Landlock less in one place: nothing is
  created, removed or renamed at the level of a directory Landlock goes
  around. They do not judge metadata (`stat(2)`) or, under Seatbelt, the
  entries of `/`. A container's `Reads` and `Writes` are judged by its
  mounts. Its `Runs` takes a path as the container sees it and judges it as
  written: cleaned, with no link followed on the host or in the image, it
  runs unless it lies under `Denied`. A name the image links to a denied
  path (`/bin/git` where `/bin` links to `usr/bin`) is said to run, and the
  stand-in over the path it leads to refuses it all the same.
- `Run` takes an ordinary `Request`. The grants, the sandbox, the image and
  the confinement are the session's: `Grants` may be nil or equal to the
  prepared ones, the profile's `Sandbox` and `SandboxImage` may be empty or
  name the session's, and `ContainerUseEnvironment` is refused. A host
  session has no image, so it refuses a profile that names one. A host turn
  runs confined whatever `Profile.Confine` says. Granted `VCS` is the
  turn's, and a profile that asks for `VCS` that was not granted is refused.
  `agent.Admit` is that step alone.
- Before anything starts, `Run` compares the verified turn with the policy:
  tools, mounts, system paths, denied names and paths, binds. A difference
  is refused with `ErrPolicyChanged`. It happens when what the policy was
  read from has changed since `Prepare`: the request sets another `PATH`
  with another `git` on it, a granted symbolic link points elsewhere, an
  agent was installed. A container turn adds only a granted mount again,
  under the name a symbolic link gives a directory of the request.
- Tools are held by the agent's own flags, not by the prompt: `claude` is
  started with `--tools` and `--strict-mcp-config`, and a request for codex,
  opencode or pi with anything less than `ToolsAll` is refused.
- A session runs any number of turns. After `Release`, `Run` returns
  `ErrReleased`.

## Workspaces

`agent.Request.Workspace` implements `vcs.Workspace`: `Directory()` is where the
agent runs and `VCS()` optionally describes additional writable container mounts
at host paths, which must be granted read-write. `vcs.Directory(path)` supplies a directory without VCS metadata.
The runner performs no git discovery. A denied profile never reads `VCS()`.

`vcs.Provider` acquires a workspace from caller-defined name/ref/branch values,
releases it, and prunes stale metadata. The caller owns this lifetime: a workspace
may span several sessions and retries. Release it on success, errors and
cancellation, using `context.WithoutCancel` for cleanup after cancellation.
Providers own retention policy and interpret refs; core does neither.

Busybees implements this contract in `internal/workspace`, serializing git
operations on its main clone and supplying its shared git directory as a mount.
Its scheduler adds fetch, commits-ahead and branch deletion operations at the
busybees boundary. Its session adapter supplies git/GitHub identity and push
configuration through the VCS environment fields. Busybees profiles allow VCS
access; another caller can supply a plain directory and deny it.

## Work identity

`work.Ref` carries an opaque string `Key` and caller-defined string `Tags`.
Core stores and compares these values without interpreting tracker identity.
`agent.Outcome.Work` records the reported work, and `agent.WriteWork` and
`agent.ReadWork` persist it in a session's `work.json` marker. Keys have a
bounded, case-safe hashed filename representation for bookkeeping stores.

Busybees supplies GitHub mapping and a one-time upgrade of its existing state
directories. Transcript, PID and interruption artifacts keep their formats;
GitHub environment parameters and touched-issue files stay in its adapter.

## Testing

`agenttest` provides fake executable scripts for agents, Docker and a host MCP
server. `agentbin` refuses real agent and engine executables from test binaries.
The container fake records commands and simulates lifecycle operations without
starting Docker. It answers a `NewContainer` session's look into its image
from `image-vcs.txt` beside the script (`f <path>` or `d <path>` a line, no
VCS executables without the file), records that command in
`docker-probe.txt`, and fails it when a file named `fail-probe` is there.
Busybees adapter tests use these same fakes for its environment, identity and
MCP contracts.

`agenttest/enforcertest` is an `agent.Enforcer` that starts no process. It
checks grants and requests with `agent.NewPolicy` and `agent.Admit`, the code
the real enforcers use, and hands each turn to a Go function that plays the
agent:

```go
e := &enforcertest.Enforcer{Sandbox: agent.SandboxNone}
e.Agent = func(ctx context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
	err := turn.WriteFile(filepath.Join(pinned, "x"), nil)
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("the reviewer wrote its pinned revision: %v", err)
	}
	if err := turn.Exec("git"); err == nil {
		t.Error("the reviewer ran git")
	}
	return &agent.Result{ResultText: "done"}, nil
}
```

`Turn.ReadFile`, `WriteFile`, `Exec` and `UseTool` are judged by the
session's policy, and a refusal wraps `fs.ErrPermission`. The fake has no
platform: its policy has no system paths and no denied paths, a denied name
is denied under every path that ends in it, and `Exec` runs nothing.
`PrepareErr` stands in for a platform that cannot enforce the kind, and
`Sessions`, `Session.Requests` and `Session.Released` say what the caller did.
It is a package of its own because it imports `agent`, whose tests import
`agenttest`.

## MCP host

`mcphost.NewRegistry` accepts arbitrary role names and separate policies for an
empty role and an unknown role: `RejectRole`, `AllTools` or `NoTools`.
`mcphost.AddTool` registers an SDK typed handler, metadata and optional role
scope. With no scope, every known role gets the tool. Duplicate tool names and
unknown scopes panic during registration; hidden and unknown tools receive the
SDK's unknown-tool error. `SchemaFor` adds enums to an inferred input schema.

`Registry.NewServer` takes a role and SDK implementation metadata and returns an
independent server snapshot. Registration copies metadata and scopes. Handlers
receive independently decoded inputs; callers synchronize mutable collaborators.
`Connect` and `Tools` use an in-memory client. `Run` accepts SDK transports,
including stdio, and treats ordinary client closure as success. `ServeHTTP` owns
an already-bound listener, requires a bearer token and closes on cancellation.
The caller owns address announcements and environment variable names.

`Start` serves one server to one turn and returns an `Endpoint` (URL, bound
host and port, bearer token) and a `Lease`. Each call opens its own loopback
port with its own token, so one turn cannot reach another's server.
`Endpoint.Via("host.docker.internal")` is the URL a container uses under
Docker Desktop; on Linux, where a container reaches the host only on the
bridge gateway, use `StartOn` with that address and port 0. `Lease.Close`
stops the server and frees the port; `Lease.Connect` opens a client with the
token. `StartMemory` has the same shape (`StartFunc`) and opens no listener:
its `memory://` endpoint is reached only through `Lease.Connect`, for tests.

```go
ep, lease, err := mcphost.Start(ctx, srv)
if err != nil {
	return err
}
defer lease.Close()
// Point the agent's MCP config at ep.URL, with ep.Token in the turn's environment.
```

`AddDone` registers a generic outcome tool with status and note inputs.
`DoneOptions` supplies the advertised statuses, description, default `work.Ref`
and a reporting callback that validates and records `agent.Outcome`. Empty status
lists leave the schema unrestricted; the callback remains responsible for policy.
`AddDoneTool` accepts a custom typed input and decoder for an existing wire
contract. Each call receives a copy of the default work tags. Busybees uses this
adapter to keep issue/PR fields and their defaults, state migration and PR outcome
requirements outside core.

## Operational primitives

`ops` contains reusable pieces for caller-owned reconcile loops. It does not
poll a tracker, choose a workflow, log, or escalate work:

- `ClassifyFailure`, `RetryPolicy.Decide`, `SelectProfile` and `Sleep` preserve
  reported-outcome precedence, retry counts, delays and fallback selection.
- `Ledger` appends, reads and atomically trims JSONL accounting. Share one
  instance per file to serialize append/trim. `Now` supplies timestamps for
  entries without one; reads skip malformed lines, trims preserve their bytes,
  and a read error never returns partial totals. Schema migration is external.
- `Spend` selects opaque work keys and an inclusive time cutoff. `OverBudget`
  uses a strict threshold with nonpositive limits unlimited. `EvaluateWindow`
  returns reached/crossed/released signals with caller-supplied hysteresis and
  window. `Streaks` counts consecutive crossings per comparable subject.
- `PauseUntil` and `CapacityPause` calculate reset/backoff, extend episodes
  without shortening them and report release once. `Degraded` resets on success
  and reports one threshold crossing per failure streak, with sorted snapshots.
- `Bus` stamps events with an injected clock and drops new events for a full
  subscriber buffer. Work keys and tags, event kinds, roles, phases and outcomes
  carry caller meaning only; each subscriber receives independent work tags.
- `SharedPool` gives all-or-none claims to queued members in FIFO order. Call
  `Pass` after each dispatch pass and `Leave` when a loop stops; release active
  claims normally. Claims must fit the pool. Wake callbacks must not block or
  call back into the pool because they run under its lock.
- `NewWake` creates one coalescing wake per loop. `Wait` services local work
  without resetting the caller's tick source and stops on cancellation. A ready
  tick consumes a pending wake; `Drain` also lets an immediate full pass consume
  it. The caller owns timers, full passes, and how many loops exist.

Busybees keeps one loop per project. Its adapter chooses the 24-hour budget
window, retention floor, retry configuration, eight-hour reported-reset cap,
two-session budget streak and three-failure degraded threshold. It renders
status and logs, maps work to GitHub and decides what operational signals do.

## Review pipeline

`review.Runner[R].Run` takes an artifact directory, `review.Bundle[R]` and a
supplied diff. Context items retain their source names, content and order;
skipped-source reasons and the generated files the caller took out of the
diff (`Bundle.Excluded`) pass through to the brief, and `Exclude` drops a
finding anchored in one of those files after the judge. `R` is the caller's reference
type, with display text, a URL and an opaque scope for reviewer-note rules.
Its JSON representation is preserved in `brief.json`; it must support decoding
when artifacts are read back. Core does not interpret tracker identity.

Supply `Distiller.Agent` and `Angles.Agent` through the small `review.Agent`
interface. `Angles.AgentFor` can select a prepared agent and recorded model for
each angle; timeout, turn-limit and backend settings belong to those agents.
`Settings` enables angles and pins category severities. `Angles.Sized` overrides
size selection. The judge merges findings deterministically, without a session.

`Runner.Compare` supplies text comparison for both duplicate findings and note
rules. Nil disables text matching; overlapping findings in the same file, side
and category still merge. Rule scopes are opaque strings with `*` and empty
values matching any scope. `Runner.Rules` filters findings; `Angles.Rules`
provides the rules included in angle prompts.

The caller owns directory naming and acquisition. `Angles.Prepare` optionally
supplies a working directory; otherwise angles use `Angles.Dir` or an artifact
scratch directory. Core writes the diff only into a directory the review owns,
never into `Angles.Dir`. Before the brief is persisted, an error removes the
artifact directory and acquired files in it. Later errors retain the completed
stages for inspection. One failed angle is non-fatal when another succeeds.

Artifacts keep `brief.json`, `angles/<angle>.json`, `findings.json` and
`triage.json`. `ReadArtifact[R]` reads partial artifacts after the brief; the
caller owns interactive triage and publication. Progress callbacks run on the
angle goroutines and must be concurrency-safe. Runs preserve reported session
IDs, turns and costs; the brief preserves its session ID and cost.
