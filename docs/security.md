# Security

[`sandbox`](configuration.md#sandboxing) decides how much of the machine
running `bees` a session can reach: `none`, `claude`, `container` or `sbx`.
This page
states what each mode protects and what it does not, for filesystem, network
and credentials, so choosing a mode is an informed choice rather than a name.

## Credentials reach every session, in every mode

A session carries the bot's own GitHub credentials
([`[github]`](configuration.md#github) login and token), the Neo4j Agent
Memory API key ([`notes.neo4j_api_key`](configuration.md#notes)) when the
notes backend is `neo4j`, and its role's configured
[`env`](configuration.md#global-and-rolesname) and `shell`, whatever
`sandbox` says. Sandboxing keeps the host's filesystem, network and
*other* checkouts and credentials out of reach; it does not withhold the
bot's own, already-scoped credentials from the session acting on their
behalf. A session that can push to the repository and comment as the bot in
`none` mode can still do both in `claude`, `container` and `sbx` mode: that
is the role's job, not a gap.

What each mode changes is everything else the process can reach: the rest of
the filesystem, other hosts, and credentials that belong to something other
than this session.

## `none`

No sandbox. A session runs with every permission the user running `bees`
has: it reads and writes anywhere that user can, reaches any host the
network allows, and can use any credential stored in a file or keychain on
the machine, including ones that belong to other projects. Its environment
holds only the variables listed in
[Exported into every session](configuration.md#exported-into-every-session),
not every variable of the process that started `bees`.

Choose it when the repository's own tooling is already trusted the way a
person's shell is, or when `claude`'s or `container`'s gaps below rule them
out for a role.

## `claude`

Claude Code's own sandbox, described in
[The claude mode](configuration.md#the-claude-mode). Measured on macOS 15,
Claude Code 2.1.263, 2026-09-06; Linux is written to the same contract but
was not run.

**Filesystem.** Shell commands, and everything they start, cannot write
outside the worktree, the state directory, the session's own temporary
directory and, for a linked worktree, the repository's shared `.git` (its
`hooks` and `config` stay read-only). The built-in Write and Edit tools are
held to the same boundary by the permission layer, and nothing inside the
session can widen it: `.claude/`, `.mcp.json` and the settings block bees
passes are protected from both the box and the tools, and the block itself
is never written to a file the session could edit.

Reads are not fenced. A sandboxed shell and the Read tool read anything the
user running `bees` can, `~/.ssh` and `~/.aws` included. Combined with the
network below, that is a path from a file the box cannot write to a host the
box does allow.

**Network.** Shell commands, their children and `WebFetch` reach
`github.com` and `*.github.com` only; every other host is refused, with no
retry outside the box. Neither platform inspects TLS, so domain fronting
through an allowed host is not ruled out. On macOS, `gh` needs the system
trust daemon to verify TLS, which the box hides by default;
`enableWeakerNetworkIsolation` reopens it so `gh` works, and Claude Code's
own docs name that daemon as a possible exfiltration channel. It is the one
hole this mode accepts, on macOS only.

MCP servers, including the built-in one, run on the host rather than inside
the box, so `mail_send`, `pr_view` and the rest work unchanged and are not
limited by the network rule above; with the `neo4j` notes backend that is
also how `notes_read` and `notes_write` reach `notes.neo4j_url`. Only the
tools each role's prompt actually offers limit what they are used for.

**Credentials.** The `[github]` token, the Neo4j Agent Memory API key with
the `neo4j` notes backend, and the role's `env` reach the shell inside the
box, per the shared rule above. `gpg` commit signing does not
work (`~/.gnupg` is not writable), and neither does ssh (no host resolves)
or a toolchain's own credential store outside the writable directories.

**Does not hold, or costs something:**

- Reads reach the whole filesystem the user can read, not only the box's
  writable paths.
- `enableWeakerNetworkIsolation` reopens a channel Claude Code's own
  documentation calls a possible exfiltration risk (macOS only; without it
  `gh` cannot authenticate at all).
- The user's `~/.claude/settings.json` and the project's
  `.claude/settings.json` still load and merge with the block bees writes:
  `allowWrite` and `allowedDomains` there widen the box, and an
  `excludedCommands` entry runs that command outside it entirely. That is
  the operator's and the project's lever, and their responsibility.
- Toolchain caches, a module proxy, `ssh` remotes, a nested `git init` or
  `git clone`, and Docker's or Dagger's sockets are all refused until a
  project widens the box for them.
- Linux needs `bubblewrap` and `socat` on `PATH`, checked before a session
  starts; it was not run for this page.

## `container`

The agent's command line run unchanged inside `docker run`, described in
[The container mode](configuration.md#the-container-mode). Measured on
macOS 26 with Docker Desktop 28.4.0, Claude Code 2.1.263 in
`node:22-bookworm`, 2026-09-06; Linux is written to the same contract
(`--user`, `--add-host`, the bridge gateway) but was not run.

**Filesystem.** The container sees three things of the host, each mounted at
its host path: the worktree, the repository's shared `.git`, and the state
directory. A role with `skills` also gets the skills cache, read-only.
These are the session's grants, and bees refuses to start the container
when a path it would mount is not granted with the access it is mounted
with. Nothing else of the host is reachable: no home directory, no other
checkout, no credential store, no keychain. A write outside the three
mounts is refused (`Permission denied`). The mounted state directory holds
every role's mail and notes, not only this session's, and the mounted
`.git` is the repository's own: a session can rewrite refs and hooks there,
as it can in every mode.

**Network.** Open: the session reaches any host the container's network
allows, the same as `none`, but with only the bot's GitHub token, the Neo4j
Agent Memory API key with the `neo4j` notes backend, and its own agent
credential in hand rather than the host's other secrets. Nothing filters
outbound connections inside the container.

**Credentials.** The container's environment is built from nothing rather
than inherited from the host process: the agent's credential
(`ANTHROPIC_API_KEY`/`CLAUDE_CODE_OAUTH_TOKEN` or the codex equivalents) when
the bees environment or the role's `env` has one, the `[github]` token and
git identity, the Neo4j Agent Memory API key with the `neo4j` notes backend,
and the role's own `env`. Values reach the engine by variable
name, never on a command line that a process listing could read. The
session runs unprivileged (the host's uid:gid, or on Linux whatever
`--user` names), with a `HOME` on a tmpfs that is gone when the session
ends.

**Does not hold, or costs something:**

- The network is open: nothing about `container` mode limits which hosts a
  session reaches, unlike `claude`'s allowlist.
- Everything baked into the image is the session's to use; the image is the
  operator's responsibility; bees never pulls or verifies a `sandbox_image`.
  A `container_use_environment` is built from the worktree's copy of the
  definition, so a branch can change what its own sessions run in, and
  `docker build` pulls its `base_image`.
- The mounted state directory is shared across roles, so a container
  session can read another role's mail and notes.
- The `bees` CLI is not in the container: the tools reach it over HTTP
  instead, so a session that shells out to `bees` directly has nothing to
  run.
- Linux is written for but was not run for this page.

## `sbx`

The agent's command line run unchanged inside a Docker Sandbox, described in
[The sbx mode](configuration.md#the-sbx-mode). Written to the sbx CLI
reference (sbx 0.42) and the Docker Sandboxes documentation; not run for
this page.

**Filesystem.** The sandbox is a microVM with its own kernel and filesystem.
It sees three things of the host, each mounted at its host path as a
workspace: the worktree, the repository's shared `.git`, and the state
directory. A role with `skills` also gets the skills cache, read-only.
These are the session's grants, and bees refuses to create the sandbox when
a path it would mount is not granted with the access it is mounted with.
Nothing else of the host is reachable: no home directory, no other
checkout, no credential store, no keychain, no host Docker daemon, and not
the shared skills store sbx mounts by default, which bees switches off.
Docker documents that a symbolic link pointing out of a workspace is not
followed. Inside the sandbox the session has `sudo` and a private Docker
daemon, and everything it installs or builds is deleted with the sandbox
when the session ends. The mounted state directory holds every role's mail
and notes, and the mounted `.git` is the repository's own, as in
`container` mode.

**Network.** Held by the sbx network policy: every outbound connection
goes through a proxy on the host that allows a destination only when a
rule matches it, and the policy is the machine's (or the organisation's),
not the session's to change. The built-in MCP server needs a rule allowing
`localhost`, which lets a session reach every service listening on the
host's loopback, not only the server: the port is picked per session, so
the rule cannot be narrower. Within the policy the session has the bot's
GitHub token and the role's `env`, and nothing else of the host's secrets.

**Credentials.** The sandbox's environment is built from nothing rather
than inherited: the `[github]` token and git identity, the role's own
`env`, and the `BEES_*` variables. Values reach `sbx` by variable name,
never on a command line. The agent's own credential is not handed in at
all: the sandbox's proxy injects the one stored with `sbx secret set`
(`anthropic`, `openai` or another provider's) into requests to that
provider's API, and a compromised session cannot read it. That proxy injects a stored `github` secret the same way,
which would replace the bot's token with the person's: do not store one on
a machine that runs the factory.

**Dagger.** A role with [`sandbox_dagger_engine`](configuration.md#dagger-in-the-sandbox)
reaches the host's Dagger engine from the sandbox, and nothing else of the
sort: bees refuses to run a session that is granted the engine without its
profile asking for it, or in any mode but `sbx`. What the session hands the
engine runs outside the sandbox: its containers reach the network the
engine can, not what the sbx policy allows, and they share the engine's
cache with every other client of that engine. The Dagger CLI is installed
from `dl.dagger.io` into each sandbox as root.

**Does not hold, or costs something:**

- The `localhost` network rule opens every service on the host's loopback
  to the session, for as long as the rule stands.
- With `sandbox_dagger_engine`, the session runs containers outside the
  sandbox's network policy, through the engine. The engine's forward is
  open on the host's loopback while the session runs, so another sandbox
  allowed `localhost` can reach it too.
- The template is the operator's responsibility; sbx pulls it, and bees
  does not verify it.
- A `github` secret stored with `sbx secret set` overrides the bot's
  identity on every request to GitHub.
- The mounted state directory is shared across roles, so a session can read
  another role's mail and notes.
- The `bees` CLI is not in the sandbox: the tools reach it over HTTP
  instead, so a session that shells out to `bees` directly has nothing to
  run.
- A sandbox a crash left behind persists on the machine until removed by
  hand: `<session>/sandbox-name` says which; `bees kill` does not remove
  it.
- Nothing on this page was run; the mode is written to the documented CLI.

## Choosing a mode

| Mode | Stops a session from | Does not stop a session from |
|---|---|---|
| `none` | inheriting a variable outside its grants | reading or writing anywhere the user can, reaching any host, using any credential stored on the machine |
| `claude` | writing outside the worktree, state directory and shared `.git`; reaching a host other than GitHub | reading anything the user can read; reaching GitHub with whatever it read; `gh` on macOS reopening the trust daemon |
| `container` | reading or writing anything of the host outside the worktree, `.git` and the state directory; using a credential other than the bot's own (GitHub, and Neo4j Agent Memory with the `neo4j` notes backend) and its agent's | reaching any host; another role reading the shared state directory |
| `sbx` | reading or writing anything of the host outside the worktree, `.git` and the state directory; reaching a host the sbx policy does not allow; reading its agent's credential at all; using a credential other than the bot's own | reaching any service on the host's loopback once `localhost` is allowed; another role reading the shared state directory |

A role that only reads the repository and calls the factory's own tools is
no safer in `claude`, `container` or `sbx` than in `none`: the risk
sandboxing addresses is a session's shell commands and file writes, and a
role that never runs untrusted shell commands has little to box. The boxed
modes earn their cost on a role that runs the product's own build, test or lint
tooling, where a dependency or a generated script is the thing being kept
away from the rest of the machine.
