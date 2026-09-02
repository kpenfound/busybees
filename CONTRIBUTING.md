# Contributing

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
`github.com/dagger/go` module. Without the export there are no checks to run:
`dagger check` prints a few green lines and exits 0, having checked nothing.

### Testing rules

- Tests never call the real `claude` or `gh`. `gh` is faked through
  `github.Client.Exec`; `claude` is faked by the test binary itself (see
  `TestMain` in `internal/scheduler/scheduler_test.go`) or by a shell script
  (`internal/session`). Git is real: tests create a bare origin with
  `internal/testutil.SetupRepos`.
- `install.sh` is checked with `shellcheck -s sh install.sh`. No test runs it,
  by design, because it must not reach the network; `cmd/bees/release_test.go`
  reads its text to pin the asset names it parses.
- A test that reads a repository file — a fixture, a doc page, anything that
  is not a Go source — must list that file in `dagger.toml`'s
  `includeExtraFiles`, or it passes locally and fails under `dagger check`:
  the check container mounts only Go sources.

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
