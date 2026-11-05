#!/usr/bin/env bash
# Install the pinned Shaka Packager binary for this platform, verified against
# the published SHA-256 in versions.env.
#
#   ./install_packager.sh [destination-dir]      # default /usr/local/bin
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
set -a; . "$here/versions.env"; set +a
dest=${1:-/usr/local/bin}

case "$(uname -s)/$(uname -m)" in
  Linux/x86_64)          asset=packager-linux-x64;   sha=$SHAKA_LINUX_X64_SHA256 ;;
  Linux/aarch64|Linux/arm64) asset=packager-linux-arm64; sha=$SHAKA_LINUX_ARM64_SHA256 ;;
  Darwin/x86_64)         asset=packager-osx-x64;     sha=$SHAKA_OSX_X64_SHA256 ;;
  Darwin/arm64)          asset=packager-osx-arm64;   sha=$SHAKA_OSX_ARM64_SHA256 ;;
  *) echo "install_packager: unsupported platform $(uname -s)/$(uname -m)" >&2; exit 1 ;;
esac

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
curl -fsSL --connect-timeout 20 --max-time 300 --retry 5 --retry-delay 10 --retry-all-errors -o "$tmp" "$SHAKA_RELEASE_URL/$asset"
if command -v sha256sum >/dev/null; then got=$(sha256sum "$tmp" | cut -d' ' -f1); else got=$(shasum -a 256 "$tmp" | cut -d' ' -f1); fi
if [ "$got" != "$sha" ]; then
  echo "install_packager: $asset failed SHA-256 verification (got $got, want $sha)" >&2
  exit 1
fi
install -m 0755 "$tmp" "$dest/packager"
"$dest/packager" --version
