#!/usr/bin/env bash
# action/install.sh — download and install the sig binary onto this GitHub
# Actions runner's PATH. Fetches the release matching SIG_VERSION from this
# repo's GitHub Releases (Linux only — see docs/USAGE.md's GitHub Action
# section), verifies the archive against that release's checksums.txt before
# extracting, then appends the install directory to GITHUB_PATH so later
# steps can just run `sig`. Pure bash + curl + sha256sum: no third-party
# actions.
#
# Required env: INSTALL_DIR.
# Optional env: SIG_VERSION ("latest" or e.g. "0.2.0"/"v0.2.0", default
# "latest"), SIGBOUND_REPO (default surya-koritala/sigbound), RUNNER_ARCH_IN
# (runner.arch: X64|ARM64, default X64), RUNNER_OS_IN (runner.os, default
# Linux), GITHUB_PATH (appended to when set).
set -euo pipefail

# resolve_tag sets $tag (e.g. "v0.2.0") and $ver (e.g. "0.2.0") in the
# caller's scope for the given version input ("latest" or a version string).
# "latest" is the only branch that touches the network.
resolve_tag() {
  local version="$1" repo="$2"
  if [ "$version" = "latest" ]; then
    tag="$(curl -sSL --fail "https://api.github.com/repos/$repo/releases/latest" | jq -r '.tag_name')"
    if [ -z "$tag" ] || [ "$tag" = "null" ]; then
      echo "sigbound-action: could not resolve the latest release tag for $repo" >&2
      return 1
    fi
  else
    tag="v${version#v}"
  fi
  ver="${tag#v}"
}

# arch_for prints the release-archive arch name (amd64/arm64) for a GitHub
# Actions runner.arch value, or fails on anything else.
arch_for() {
  case "$1" in
    X64) echo amd64 ;;
    ARM64) echo arm64 ;;
    *) return 1 ;;
  esac
}

main() {
  local repo="${SIGBOUND_REPO:-surya-koritala/sigbound}"
  local version="${SIG_VERSION:-latest}"
  local install_dir="${INSTALL_DIR:?INSTALL_DIR is required}"
  local runner_arch="${RUNNER_ARCH_IN:-X64}"
  local runner_os="${RUNNER_OS_IN:-Linux}"

  if [ "$runner_os" != "Linux" ]; then
    echo "sigbound-action: only Linux runners are supported (got runner.os=$runner_os); use ubuntu-latest or a self-hosted Linux runner" >&2
    exit 1
  fi

  local arch
  if ! arch="$(arch_for "$runner_arch")"; then
    echo "sigbound-action: unsupported runner.arch '$runner_arch' (only X64 and ARM64 are supported)" >&2
    exit 1
  fi

  local tag ver
  resolve_tag "$version" "$repo" || exit 1

  local asset="sigbound_${ver}_linux_${arch}.tar.gz"
  local base_url="https://github.com/$repo/releases/download/$tag"

  # Deliberately NOT `local`: the EXIT trap fires after main() has already
  # returned (as the whole script exits), by which point a local var's
  # binding is gone — under `set -u` that would make the trap itself fail
  # with "unbound variable" on every run, local or not.
  work="$(mktemp -d)"
  trap 'rm -rf "$work"' EXIT

  echo "sigbound-action: installing $asset ($tag)" >&2
  curl -sSL --fail -o "$work/$asset" "$base_url/$asset"
  curl -sSL --fail -o "$work/checksums.txt" "$base_url/checksums.txt"

  # sha256sum is what ubuntu runners have; shasum is the macOS/local-testing
  # fallback so this same logic is exercisable off-CI too.
  local sha_tool=(sha256sum -c -)
  command -v sha256sum >/dev/null 2>&1 || sha_tool=(shasum -a 256 -c -)
  ( cd "$work" && grep " ${asset}\$" checksums.txt | "${sha_tool[@]}" )

  mkdir -p "$install_dir"
  tar -xzf "$work/$asset" -C "$install_dir" sig
  chmod +x "$install_dir/sig"

  if [ -n "${GITHUB_PATH:-}" ]; then
    echo "$install_dir" >>"$GITHUB_PATH"
  fi

  echo "sigbound-action: installed $("$install_dir/sig" version | head -1) to $install_dir/sig" >&2
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
