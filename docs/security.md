# Security

[`sandbox`](configuration.md#sandboxing) decides how much of the machine
running `bees` a session can reach: `none`, `claude` or `container`. This page
states what each mode protects and what it does not, for filesystem, network
and credentials, so choosing a mode is an informed choice rather than a name.

## Credentials reach every session, in every mode

A session carries the bot's own GitHub credentials
([`[github]`](configuration.md#github) login and token) and its role's
configured [`env`](configuration.md#global-and-rolesname) and `shell`,
whatever `sandbox` says. Sandboxing keeps the host's filesystem, network and
*other* checkouts and credentials out of reach; it does not withhold the
bot's own, already-scoped credentials from the session acting on their
behalf. A session that can push to the repository and comment as the bot in
`none` mode can still do both in `claude` and `container` mode: that is the
role's job, not a gap.

What each mode changes is everything else the process can reach: the rest of
the filesystem, other hosts, and credentials that belong to something other
than this session.

## `none`

No sandbox. A session runs with every permission the user running `bees`
has: it reads and writes anywhere that user can, reaches any host the
network allows, and can use any credential on the machine, including ones
that belong to other projects.

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
limited by the network rule above; only the tools each role's prompt
actually offers limit what they are used for.

**Credentials.** The `[github]` token and the role's `env` reach the shell
inside the box, per the shared rule above. `gpg` commit signing does not
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
Nothing else of the host is reachable: no home directory, no other
checkout, no credential store, no keychain. A write outside the three
mounts is refused (`Permission denied`). The mounted state directory holds
every role's mail and notes, not only this session's, and the mounted
`.git` is the repository's own: a session can rewrite refs and hooks there,
as it can in every mode.

**Network.** Open: the session reaches any host the container's network
allows, the same as `none`, but with only the bot's GitHub token and its own
agent credential in hand rather than the host's other secrets. Nothing
filters outbound connections inside the container.

**Credentials.** The container's environment is built from nothing rather
than inherited from the host process: the agent's credential
(`ANTHROPIC_API_KEY`/`CLAUDE_CODE_OAUTH_TOKEN` or the codex equivalents) when
the bees environment or the role's `env` has one, the `[github]` token and
git identity, and the role's own `env`. Values reach the engine by variable
name, never on a command line that a process listing could read. The
session runs unprivileged (the host's uid:gid, or on Linux whatever
`--user` names), with a `HOME` on a tmpfs that is gone when the session
ends.

**Does not hold, or costs something:**

- The network is open: nothing about `container` mode limits which hosts a
  session reaches, unlike `claude`'s allowlist.
- Everything baked into the image is the session's to use; the image is the
  operator's responsibility; bees never pulls or verifies it.
- The mounted state directory is shared across roles, so a container
  session can read another role's mail and notes.
- The `bees` CLI is not in the container: the tools reach it over HTTP
  instead, so a session that shells out to `bees` directly has nothing to
  run.
- `bees kill` does not find the container directly; it stops the `docker
  run` client, which forwards the signal, and a container left behind needs
  `docker rm -f`. See [The container mode](configuration.md#the-container-mode).
- Linux is written for but was not run for this page.

## Choosing a mode

| Mode | Stops a session from | Does not stop a session from |
|---|---|---|
| `none` | nothing | reading or writing anywhere the user can, reaching any host, using any credential on the machine |
| `claude` | writing outside the worktree, state directory and shared `.git`; reaching a host other than GitHub | reading anything the user can read; reaching GitHub with whatever it read; `gh` on macOS reopening the trust daemon |
| `container` | reading or writing anything of the host outside the worktree, `.git` and the state directory; using a credential other than the bot's own and its agent's | reaching any host; another role reading the shared state directory |

A role that only reads the repository and calls the factory's own tools is
no safer in `claude` or `container` than in `none`: the risk sandboxing
addresses is a session's shell commands and file writes, and a role that
never runs untrusted shell commands has little to box. The two modes earn
their cost on a role that runs the product's own build, test or lint
tooling, where a dependency or a generated script is the thing being kept
away from the rest of the machine.
