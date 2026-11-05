#!/usr/bin/env bash
# Installs ffmpeg (apt) and the pinned Shaka Packager for CI jobs, into
# $1 (default $RUNNER_TEMP/bin), and puts that directory on GITHUB_PATH.
# Runner apt mirrors sometimes stall without output; each attempt is
# bounded and retried rather than waited out.
set -euo pipefail
dest=${1:-$RUNNER_TEMP/bin}
here=$(cd "$(dirname "$0")" && pwd)
apt_opts=(-o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30)
for try in 1 2 3; do
  if sudo timeout 240 apt-get "${apt_opts[@]}" update -qq &&
     sudo timeout 300 apt-get "${apt_opts[@]}" install -y -qq --no-install-recommends ffmpeg; then
    break
  fi
  [ "$try" = 3 ] && { echo "ci_media_tools: apt failed 3 times" >&2; exit 1; }
  echo "ci_media_tools: apt attempt $try failed; retrying" >&2
  sleep 10
done
mkdir -p "$dest"
"$here/install_packager.sh" "$dest"
if [ -n "${GITHUB_PATH:-}" ]; then echo "$dest" >> "$GITHUB_PATH"; fi
