#!/bin/sh
# Sigbound installer: downloads the prebuilt `sig` binary for this platform
# from the latest GitHub release, verifies its SHA-256 against the release's
# checksums.txt, and installs it. No Go toolchain required.
#
#   curl -fsSL https://raw.githubusercontent.com/surya-koritala/sigbound/main/install.sh | sh
#
# Options (environment variables):
#   SIG_INSTALL_DIR  install directory (default: ~/.local/bin)
#   SIG_VERSION      tag to install, e.g. v1.0.0 (default: latest release)
set -eu

REPO="surya-koritala/sigbound"

fail() { echo "install.sh: $*" >&2; exit 1; }

os=$(uname -s)
case "$os" in
  Darwin) goos=darwin ;;
  Linux)  goos=linux ;;
  *) fail "unsupported OS '$os' — Windows and others: grab the archive from https://github.com/$REPO/releases or use 'go install github.com/$REPO/cmd/sig@latest'" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64)  goarch=amd64 ;;
  arm64|aarch64) goarch=arm64 ;;
  *) fail "unsupported architecture '$arch' — see https://github.com/$REPO/releases" ;;
esac

if [ "${SIG_VERSION:-}" != "" ]; then
  tag="$SIG_VERSION"
else
  # tag_name from the latest-release API, no jq dependency
  tag=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
    grep -m1 '"tag_name"' | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')
  [ -n "$tag" ] || fail "could not determine the latest release tag"
fi
ver=${tag#v}

archive="sigbound_${ver}_${goos}_${goarch}.tar.gz"
base="https://github.com/$REPO/releases/download/$tag"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $archive ($tag)..."
curl -fsSL -o "$tmp/$archive" "$base/$archive" || fail "download failed: $base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || fail "download failed: checksums.txt"

want=$(grep " $archive\$" "$tmp/checksums.txt" | awk '{print $1}')
[ -n "$want" ] || fail "$archive not found in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$archive" | awk '{print $1}')
else
  got=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')
fi
[ "$want" = "$got" ] || fail "checksum mismatch for $archive (want $want, got $got)"
echo "Checksum verified."

tar -xzf "$tmp/$archive" -C "$tmp" sig

dir="${SIG_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$dir" || fail "cannot create $dir (set SIG_INSTALL_DIR to a writable directory)"
install -m 0755 "$tmp/sig" "$dir/sig"

echo "Installed: $dir/sig ($("$dir/sig" version | head -1))"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "NOTE: $dir is not on your PATH — add:  export PATH=\"$dir:\$PATH\"" ;;
esac
echo "Next: run 'sig doctor' in a git repo to check your setup."
