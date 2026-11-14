#!/bin/sh
# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0

# Installs the setdrift CLI from the latest GitHub release (or SETDRIFT_VERSION):
#
#   curl -fsSL https://raw.githubusercontent.com/setdrift/setdrift-engine/main/scripts/install.sh | sh
#
# It downloads the archive for this OS and CPU, checks it against the
# release's checksums.txt, and installs the binary to SETDRIFT_INSTALL_DIR,
# else /usr/local/bin when writable, else ~/.local/bin. macOS and Linux;
# on Windows, use scripts/install.ps1 from PowerShell.
set -eu

repo=setdrift/setdrift-engine

say() { printf '%s\n' "$*" >&2; }
fail() { say "setdrift install: $*"; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "needs $1"; }

need uname
case $(uname -s) in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  MINGW* | MSYS* | CYGWIN*)
    fail "on Windows, install from PowerShell: irm https://raw.githubusercontent.com/$repo/main/scripts/install.ps1 | iex" ;;
  *) fail "unsupported OS $(uname -s): download a release from https://github.com/$repo/releases" ;;
esac
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "unsupported CPU $(uname -m): download a release from https://github.com/$repo/releases" ;;
esac
need curl
need tar

version=${SETDRIFT_VERSION:-}
if [ -z "$version" ]; then
  # The latest release's tag, from where /releases/latest redirects.
  version=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest" | sed 's#.*/tag/##')
  case $version in v*) ;; *) fail "no release found at https://github.com/$repo/releases" ;; esac
fi
case $version in v*) ;; *) version=v$version ;; esac

archive="setdrift_${version#v}_${os}_${arch}.tar.gz"
# SETDRIFT_DOWNLOAD_URL: a mirror of the release assets (and for testing).
base=${SETDRIFT_DOWNLOAD_URL:-"https://github.com/$repo/releases/download/$version"}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say "Downloading setdrift $version for $os/$arch"
curl -fsSL -o "$tmp/$archive" "$base/$archive" || fail "download failed: $base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || fail "download failed: $base/checksums.txt"

want=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || fail "$archive is not in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$archive" | awk '{ print $1 }')
else
  need shasum
  got=$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')
fi
[ "$got" = "$want" ] || fail "checksum mismatch for $archive: got $got, want $want"

tar -xzf "$tmp/$archive" -C "$tmp" setdrift

dir=${SETDRIFT_INSTALL_DIR:-}
if [ -z "$dir" ]; then
  if [ -w /usr/local/bin ]; then dir=/usr/local/bin; else dir="$HOME/.local/bin"; fi
fi
mkdir -p "$dir"
install -m 0755 "$tmp/setdrift" "$dir/setdrift" 2>/dev/null || { cp "$tmp/setdrift" "$dir/setdrift" && chmod 0755 "$dir/setdrift"; }

say "Installed $("$dir/setdrift" version) to $dir/setdrift"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) say "Add $dir to your PATH, e.g.: export PATH=\"$dir:\$PATH\"" ;;
esac
say "Next: setdrift export   (reads your current kubeconfig context; changes nothing)"
