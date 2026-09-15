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
environment. The caller selects profiles and fallback overrides before running.

The caller also supplies:

- `EnvironmentPrefix` to strip stale inherited variables, and the request's
  `Env` for session context and credentials. An empty prefix strips nothing.
- `ValidOutcomes` on each request and when calling `agent.Report`. A nil set
  accepts any status; an empty, non-nil set accepts none. Validation errors list
  the supplied statuses. Workflow-specific requirements belong to the caller.
- Backend name prefixes, container labels, writable mounts, and optional
  `HostMCP` launch details for a server that runs on the host during a container
  session. A container session without `HostMCP` starts no host server.
- `SandboxDomains` for Claude's network permissions. `VCSAccess` controls whether
  the runner mounts a linked worktree's shared git directory. It does not remove
  VCS executables or restrict an unsandboxed host's filesystem.
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
starting Docker. Busybees adapter tests use these same fakes for its environment,
identity and MCP contracts.

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

- `ClassifyFailure`, `RetryPolicy.Decide`, `SelectModel` and `Sleep` preserve
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
skipped-source reasons pass through to the brief. `R` is the caller's reference
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
