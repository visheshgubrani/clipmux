#!/usr/bin/env bash
# Generates the setup test's sample clip, internal/setuptest/sample.mp4: 11 s
# of a synthetic test pattern with real speech, 854x480 so the ladder makes
# two video rungs plus audio. The speech is internal/media/testdata/speech/
# jfk.ogg (public domain), so a setup test can prove subtitles: "Americans"
# is heard from 1.22 s to 1.98 s. It is embedded in the binary, so the
# control plane never runs ffmpeg itself. Rerun only to change the sample;
# commit the result.
set -euo pipefail
out="${1:-internal/setuptest/sample.mp4}"
speech="$(dirname "$0")/../internal/media/testdata/speech/jfk.ogg"
ffmpeg -nostdin -loglevel error -y \
  -f lavfi -i "testsrc2=size=854x480:rate=25" \
  -i "$speech" \
  -t 11 -map 0:v -map 1:a \
  -c:v libx264 -preset veryfast -crf 34 -pix_fmt yuv420p -g 50 \
  -c:a aac -b:a 64k -ac 2 -ar 48000 \
  -movflags +faststart -map_metadata -1 -fflags +bitexact -flags:v +bitexact -flags:a +bitexact \
  "$out"
sha256sum "$out"
