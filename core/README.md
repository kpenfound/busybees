# Core agent runner

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
