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

1. **Confirm green on `main`.**

   ```bash
   go build ./... && go test -race ./...
   ```

2. **Bump the version.** Set `Version` in `cmd/sig/version.go` to the new
   number (e.g. `0.2.0`).

3. **Update the changelog.** In `CHANGELOG.md`, move items out of
   `[Unreleased]` into a new `[X.Y.Z]` section with today's date, and add the
   version-compare links at the bottom.

4. **Commit** the version and changelog changes to `main`.

5. **Tag** the release commit (annotated, `v`-prefixed):

   ```bash
   git tag -a vX.Y.Z -m "vX.Y.Z"
   git push origin vX.Y.Z
   ```

   The `v` prefix on the tag lets `runtime/debug.ReadBuildInfo` and GitHub's
   release UI line up with the changelog's unprefixed version numbers.

6. **Create the GitHub release** from the tag. Use the changelog section as the
   body, or the drafted notes in `docs/release-notes-vX.Y.Z.md` for the
   headline release.

## After the release

- Confirm `sig version` on a fresh build reports the new number.
- Open a new `[Unreleased]` section at the top of `CHANGELOG.md` for ongoing
  work.
