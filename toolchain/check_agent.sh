#!/usr/bin/env bash
# Checks the agent tarball (D061) in a clean container with no FFmpeg of
# its own: checksums, every bundled library resolved, the toolchain's
# capability checks against the bundled FFmpeg and whisper.cpp, and
# `clipmux worker doctor --offline` (tools, sandbox probe, test encodes).
#
#   check_agent.sh TARBALL [IMAGE]      # IMAGE defaults to ubuntu:24.04
set -euo pipefail
tarball=$(readlink -f "${1:?usage: check_agent.sh TARBALL [IMAGE]}")
image=${2:-ubuntu:24.04}
here=$(cd "$(dirname "$0")" && pwd)

docker run --rm \
  -v "$(dirname "$tarball"):/in:ro" -v "$here:/toolchain:ro" \
  -e TARBALL="$(basename "$tarball")" \
  "$image" bash -c '
set -euo pipefail
if command -v ffmpeg >/dev/null; then echo "check_agent: the image has its own ffmpeg; use a clean one" >&2; exit 1; fi
cd /tmp && tar -xzf "/in/$TARBALL" && cd clipmux-agent
sha256sum --quiet -c SHA256SUMS
missing=0
for f in libexec/ffmpeg libexec/ffprobe libexec/dri/*.so libexec/whisper/whisper-cli libexec/whisper/*.so; do
  if ldd "$f" | grep "not found"; then echo "check_agent: $f has unresolved libraries" >&2; missing=1; fi
done
[ "$missing" = 0 ]
./clipmux version
bash /toolchain/verify_toolchain.sh libexec/ffmpeg libexec/ffprobe libexec/whisper/whisper-cli
./clipmux worker doctor --offline
'
echo "check_agent: $(basename "$tarball") works in a clean $image"
