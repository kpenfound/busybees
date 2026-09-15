# Core agent runner

`github.com/kpenfound/busybees/core` is a standalone Go module. It imports no
package from the busybees root module. Build and test from this directory:

```sh
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
prepared entries are not expanded again.

Result, transcript, outcome, PID and interruption artifacts retain their file
formats. The issue/PR fields on outcomes are retained for existing readers;
work-key migration is a separate change. Busybees issue and touched-issue files
are owned by its adapter.

## Testing

`agenttest` provides fake executable scripts for agents, Docker and a host MCP
server. `agentbin` refuses real agent and engine executables from test binaries.
The container fake records commands and simulates lifecycle operations without
starting Docker. Busybees adapter tests use these same fakes for its environment,
identity and MCP contracts.
