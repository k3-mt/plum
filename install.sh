#!/bin/sh
# Install plum: fetch the release binary for this machine and put it on PATH.
#
# Deliberately a POSIX shell script with no dependencies beyond curl or wget and
# tar, because a tool whose whole claim is "one static binary, no supply chain"
# should not need a package manager to install.
#
#   curl -fsSL https://raw.githubusercontent.com/k3-mt/plum/main/install.sh | sh
#   PREFIX=/usr/local sh install.sh          # somewhere else
#   VERSION=v0.1.0 sh install.sh             # a specific release
set -eu

REPO="k3-mt/plum"
PREFIX="${PREFIX:-$HOME/.local}"
BINDIR="$PREFIX/bin"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "plum: no prebuilt binary for $arch — install with: go install github.com/k3-mt/plum/cmd/plum@latest" >&2; exit 1 ;;
esac
case "$os" in
  darwin|linux) ;;
  *) echo "plum: no prebuilt binary for $os — install with: go install github.com/k3-mt/plum/cmd/plum@latest" >&2; exit 1 ;;
esac

fetch() {
  if command -v curl >/dev/null 2>&1; then curl -fsSL "$1"
  elif command -v wget >/dev/null 2>&1; then wget -qO- "$1"
  else echo "plum: need curl or wget" >&2; exit 1
  fi
}

VERSION="${VERSION:-}"
if [ -z "$VERSION" ]; then
  VERSION=$(fetch "https://api.github.com/repos/$REPO/releases/latest" |
    sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
fi
if [ -z "$VERSION" ]; then
  echo "plum: no release published yet — install with: go install github.com/k3-mt/plum/cmd/plum@latest" >&2
  exit 1
fi

url="https://github.com/$REPO/releases/download/$VERSION/plum-$os-$arch.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "plum: fetching $VERSION for $os/$arch"
fetch "$url" > "$tmp/plum.tar.gz" || {
  echo "plum: could not download $url" >&2; exit 1; }
tar -xzf "$tmp/plum.tar.gz" -C "$tmp"

mkdir -p "$BINDIR"
install -m 0755 "$tmp/plum" "$BINDIR/plum"
echo "plum: installed $BINDIR/plum"

case ":$PATH:" in
  *":$BINDIR:"*) ;;
  *) echo "plum: $BINDIR is not on your PATH — add it, or move the binary somewhere that is" ;;
esac

"$BINDIR/plum" version 2>/dev/null || true
