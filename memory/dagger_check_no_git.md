---
name: dagger-check-no-git
description: dagger check/list/workspace commands fail silently in a mason view because they require a .git repo; use direct dagger -m <module> call ... instead
metadata:
  type: project
---

In a mason workspace for the busybees project, `dagger check`, `dagger check -l`, `dagger list checks`, and `dagger workspace ls`/`config-file` all return empty/no-op (exit 0, no output) because Dagger's workspace-config discovery (`dagger.toml`) depends on the directory being a Git repository, and a mason's view has no `.git` and no git tool at all ("git: not granted to this session").

**Why:** Masons hold no version control tool by design (the service records work via other means), so the Dagger CLI's git-based workspace resolution can never succeed here, independent of whether `dagger.toml`/`dagger.lock` are present and valid.

**How to apply:** Do not spend time debugging `dagger check` returning nothing in a mason view — it is an environment constraint, not a misconfiguration. Instead, replicate what `dagger check`'s `go:test-all` would run via a direct module call against `busybees-dev`'s `test-runtime`, e.g.:

```sh
dagger -m .dagger/modules/busybees-dev call test-runtime \
    with-directory --path /src --source . --exclude .git,.bees \
    with-workdir --path /src/core \
    with-exec --args=go,test,-count=1,./... \
    combined-output
```

(Use `/src` as workdir for the root module, `/src/core` for the standalone `core/` module.) Pair this with `gofmt -l` and `go build ./...`/`go vet ./...` on the host (explicitly allowed by AGENTS.md; only `go test` on the host is forbidden). Report in the unit's summary that true `dagger check` was blocked by the missing git repo and that this direct-call equivalent was run instead, per AGENTS.md's instruction to state the exact blocker rather than claim `dagger check` succeeded.
