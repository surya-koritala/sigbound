# Release guide

Maintainer notes for cutting a Sigbound release. Releases are cut from `main`
only — never from a feature branch.

## Versioning policy

Sigbound follows [Semantic Versioning](https://semver.org/). While the project
is pre-1.0:

- **Minor** (`0.x.0`) — new features; small, backward-compatible behavior
  changes are acceptable.
- **Patch** (`0.0.x`) — bug fixes only.

The **public surface** for versioning purposes is the CLI: the `sig`
subcommands and their flags, and the `SIGBOUND_*` environment variables passed
to the planner / agent / resolver / repair commands. Changes to that surface
drive the version bump. Internal packages (`cell`, `internal/...`) are not part
of the public API before 1.0.

## Cutting a release

Pushing a `vX.Y.Z` tag triggers `.github/workflows/release.yml`, which runs
[GoReleaser](https://goreleaser.com) (config: `.goreleaser.yaml`) to
cross-compile `sig`, publish a GitHub release with checksummed archives, and
push a Homebrew formula to the tap (see below). The manual steps are just the
version bump, the changelog, and the tag:

1. **Confirm green on `main`.**

   ```bash
   go build ./... && go test -race ./...
   ```

2. **Bump the version.** Set `Version` in `cmd/sig/version.go` to the new
   number (e.g. `0.2.0`). This keeps `go install` and local `go build`
   binaries accurate; the tagged release build overrides it anyway via
   `-ldflags -X main.Version=...`.

3. **Update the changelog.** In `CHANGELOG.md`, move items out of
   `[Unreleased]` into a new `[X.Y.Z]` section with today's date, and add the
   version-compare links at the bottom.

4. **Commit** the version and changelog changes to `main`.

5. **Tag** the release commit (annotated, `v`-prefixed) and push the tag:

   ```bash
   git tag -a vX.Y.Z -m "vX.Y.Z"
   git push origin vX.Y.Z
   ```

   The `v` prefix on the tag lets `runtime/debug.ReadBuildInfo` and GitHub's
   release UI line up with the changelog's unprefixed version numbers. The
   tag push starts the release workflow — watch it under the repo's Actions
   tab.

## Homebrew tap

`.goreleaser.yaml` publishes a `sig` cask to
`github.com/surya-koritala/homebrew-tap` on every release. Two prerequisites,
both one-time setup:

- The `homebrew-tap` repository must exist under the `surya-koritala` org/user
  (GoReleaser pushes a commit to it; it does not create the repo).
- A `HOMEBREW_TAP_TOKEN` repo secret must be set on `sigbound` — a personal
  access token with write access to `homebrew-tap` (the default
  `GITHUB_TOKEN` only has permissions on the repo the workflow runs in).

If either is missing, `HOMEBREW_TAP_TOKEN` is empty at release time and
`.goreleaser.yaml`'s `skip_upload` template resolves to `true`: the Homebrew
push is skipped and the rest of the release (binaries, checksums, GitHub
release) still succeeds.

## Validating the config locally

Before relying on the pipeline, or after editing `.goreleaser.yaml`:

```bash
go install github.com/goreleaser/goreleaser/v2@latest
goreleaser check
goreleaser release --snapshot --clean --skip=publish,homebrew
./dist/sig_<os>_<arch>*/sig version
```

`--snapshot` builds locally without touching GitHub or the tap; `--skip`
leaves out the two publish steps that need real credentials.

## After the release

- Confirm the release workflow run succeeded and the GitHub release has the
  expected archives + `checksums.txt`.
- Confirm `sig version` on a fresh `go install .../cmd/sig@vX.Y.Z` (or a
  downloaded archive) reports the new number.
- Open a new `[Unreleased]` section at the top of `CHANGELOG.md` for ongoing
  work.
