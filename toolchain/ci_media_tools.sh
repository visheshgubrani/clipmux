#!/usr/bin/env bash
# Installs ffmpeg (apt), the pinned Shaka Packager and the pinned
# whisper.cpp for CI jobs, into $1 (default $RUNNER_TEMP/bin), puts that
# directory on GITHUB_PATH, and fetches the speech models by digest into
# $CLIPMUX_TEST_MODELS_DIR (exported through GITHUB_ENV). whisper.cpp builds
# into $CLIPMUX_WHISPER_PREFIX (default $RUNNER_TEMP/whisper), which a
# workflow may cache: build_whisper.sh stops early when its manifest matches.
# Runner apt mirrors sometimes stall without output; each attempt is
# bounded and retried rather than waited out.
set -euo pipefail
dest=${1:-$RUNNER_TEMP/bin}
here=$(cd "$(dirname "$0")" && pwd)
apt_opts=(-o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30)
for try in 1 2 3; do
  if sudo timeout 240 apt-get "${apt_opts[@]}" update -qq &&
     sudo timeout 300 apt-get "${apt_opts[@]}" install -y -qq --no-install-recommends ffmpeg \
       cmake glslc libvulkan-dev spirv-headers libvulkan1 mesa-vulkan-drivers; then
    break
  fi
  [ "$try" = 3 ] && { echo "ci_media_tools: apt failed 3 times" >&2; exit 1; }
  echo "ci_media_tools: apt attempt $try failed; retrying" >&2
  sleep 10
done
mkdir -p "$dest"
"$here/install_packager.sh" "$dest"
prefix=${CLIPMUX_WHISPER_PREFIX:-$RUNNER_TEMP/whisper}
CLIPMUX_BUILD_DIR="${RUNNER_TEMP:-/tmp}/whisper-build" "$here/build_whisper.sh" "$prefix"
ln -sf "$prefix/bin/whisper-cli" "$dest/whisper-cli"
models=${CLIPMUX_TEST_MODELS_DIR:-$prefix/models}
"$here/fetch_speech_models.sh" "$models"
if [ -n "${GITHUB_PATH:-}" ]; then echo "$dest" >> "$GITHUB_PATH"; fi
if [ -n "${GITHUB_ENV:-}" ]; then echo "CLIPMUX_TEST_MODELS_DIR=$models" >> "$GITHUB_ENV"; fi
