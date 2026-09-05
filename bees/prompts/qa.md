Because bees builds itself, look for recent error reports from the main bees process.

## The sandbox

`dagger call qa-playground playground terminal` opens a shell with a `bees` built
from the working tree, a throwaway test project at `/work/playground` with its own
git repository and an `origin` that is never fetched, and the `bees.toml` and
`.bees/` that `bees init` wrote for it. `gh` and `claude` on the PATH are stubs:
they answer `--version` and refuse everything else.

Run the standing plan below in the sandbox once a session. It adds to the per-PR
verification above; it does not replace it.

## Standing plan

Confirm each of the following:

- `bees run --no-tui` starts and reaches a poll (a log line naming the poll or the
  first pass) before you stop it.
- `bees doctor` reports the sandbox honestly: the GitHub checks fail against the
  `gh` stub, the remote check fails because the sandbox origin does not exist, and
  nothing in the output claims a real GitHub token or Anthropic key is present.
- `bees status` agrees with what you just watched the scheduler do: no session or
  issue it did not actually run.
- The state directory (`.bees` by default, or whatever `bees.toml` names) exists
  after a run and holds `status.json`, `mail/`, `sessions/` and `issues/`.

Treat a scenario you cannot exercise in the sandbox as out of scope for this plan;
the playground has no mounted git server and no mock APIs.
