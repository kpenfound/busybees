# Releasing

A release of busybees is a set of prebuilt `bees` binaries attached to a
GitHub release, built by `.github/workflows/release.yml` when a person pushes
a `v*` tag. That tag is the only thing in the repository that triggers a
workflow: the project's own gate is `dagger check`, run by hand (see
[Architecture](architecture.md#testing)).

## Cutting a release

1. Make sure `main` is the commit you want to ship and that it is green:

   ```sh
   git checkout main && git pull
   DAGGER_X_RELEASE=v1.0.0-beta.11 dagger check
   ```

2. Tag it and push the tag. The tag name is the version — it is what the
   binaries report and what the asset names carry, so it is the one string to
   get right:

   ```sh
   git tag -a v0.2.0 -m "v0.2.0"
   git push origin v0.2.0
   ```

3. Watch the run (`gh run watch` or the Actions tab). It builds the four
   platforms in parallel, then a second job writes `checksums.txt`, creates
   the release and uploads everything to it. Release notes are generated from
   the commits since the previous tag.

There is nothing to do by hand afterwards. To ship a fix, tag again: the
workflow creates one release per tag and never touches an existing one.

## What a release contains

One gzipped tarball per platform, each holding a single executable named
`bees`, plus a `checksums.txt`:

| Asset | Platform |
|---|---|
| `bees_<version>_darwin_amd64.tar.gz` | macOS, Intel |
| `bees_<version>_darwin_arm64.tar.gz` | macOS, Apple silicon |
| `bees_<version>_linux_amd64.tar.gz` | Linux, x86-64 |
| `bees_<version>_linux_arm64.tar.gz` | Linux, arm64 |
| `checksums.txt` | SHA-256 of every tarball above, in `sha256sum` format |

`<version>` is the tag exactly as pushed, **including the leading `v`** — the
assets for `v0.2.0` are `bees_v0.2.0_linux_amd64.tar.gz` and friends.

This naming scheme is a stable interface, not an implementation detail: the
install script parses it to pick the right asset. Changing it breaks every
installer already in the wild, so treat it like a public API.

Installing by hand is three commands:

```sh
curl -fsSLO https://github.com/kpenfound/busybees/releases/download/v0.2.0/bees_v0.2.0_linux_amd64.tar.gz
tar -xzf bees_v0.2.0_linux_amd64.tar.gz
sudo mv bees /usr/local/bin/
```

To verify a download first, fetch `checksums.txt` alongside it and run
`sha256sum --check --ignore-missing checksums.txt` (`shasum -a 256 -c` on
macOS).

## The version a binary reports

The workflow builds with `-ldflags "-X main.version=<tag>"`, so a released
binary reports the tag it was built from:

```
$ bees version
bees v0.2.0
```

A local `go build ./cmd/bees` passes no such flag and reports `dev` (or
`dev (<commit>)` when Go's VCS stamps are available) instead. The full set of
cases is in the [`bees version`](cli.md#bees-version) reference.

## Actions permissions

The workflow creates the release with the automatic `GITHUB_TOKEN` and
declares `permissions: contents: write` on its `publish` job. Settings →
Actions → General → Workflow permissions sets the *default* a job's token
starts from; an explicit `permissions:` block raises it, so the workflow
works whether that setting is "Read repository contents and packages
permissions" or "Read and write permissions". (The "Allow GitHub Actions to
create and approve pull requests" checkbox next to it is about pull
requests, not releases.)

If `gh release create` ever fails with a 403, set that setting to "Read and
write permissions" and re-push the tag.
