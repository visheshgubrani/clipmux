#!/usr/bin/env bash
# Fetches the pinned speech models (versions.env) into DIR under their
# upstream file names, verifying each sha256: for the image, which bakes
# them in (Dockerfile, models stage; D118), for CI, and for running the
# speech tests by hand (CLIPMUX_TEST_MODELS_DIR).
#
#   fetch_speech_models.sh DIR
set -euo pipefail
dir=${1:?usage: fetch_speech_models.sh DIR}
here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=versions.env disable=SC1091
. "$here/versions.env"
mkdir -p "$dir"
fetch() {
  local url=$1 sha=$2 out="$dir/$(basename "$1")"
  if [ -f "$out" ] && [ "$(sha256sum "$out" | cut -d' ' -f1)" = "$sha" ]; then
    echo "fetch_speech_models: have $(basename "$out")"
    return
  fi
  curl -fsSL --retry 3 --retry-delay 5 -o "$out.part" "$url"
  [ "$(sha256sum "$out.part" | cut -d' ' -f1)" = "$sha" ] || { echo "fetch_speech_models: sha256 mismatch for $url" >&2; exit 1; }
  mv "$out.part" "$out"
  echo "fetch_speech_models: got $(basename "$out")"
}
fetch "$WHISPER_MODEL_URL" "$WHISPER_MODEL_SHA256"
fetch "$WHISPER_VAD_MODEL_URL" "$WHISPER_VAD_MODEL_SHA256"
