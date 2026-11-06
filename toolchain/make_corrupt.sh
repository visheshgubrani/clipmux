#!/usr/bin/env bash
# Generates internal/media/testdata/corrupt.mp4: a source that passes the
# probe but can't be encoded. Its MP4 boxes are intact (a 320x240 H.264
# video stream and AAC audio, 2 s), so ffprobe reports a usable video, but
# every byte of sample data in mdat is replaced with a deterministic AES-CTR
# keystream, so the decoder never produces a frame and ffmpeg exits non-zero.
#
# It stands for "a file that kills the encoder": the routing tests and the
# Phase 4 exit test use it to prove such a source never reaches a fallback.
# Rerun only to change the fixture; commit the result.
set -euo pipefail
out="${1:-internal/media/testdata/corrupt.mp4}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

ffmpeg -nostdin -loglevel error -y \
  -f lavfi -i "testsrc2=size=320x240:rate=25" \
  -f lavfi -i "sine=frequency=440:sample_rate=48000" \
  -t 2 -map 0:v -map 1:a \
  -c:v libx264 -preset veryfast -crf 34 -pix_fmt yuv420p -g 25 \
  -c:a aac -b:a 64k -ac 2 \
  -movflags +faststart -map_metadata -1 -fflags +bitexact -flags:v +bitexact -flags:a +bitexact \
  "$tmp/good.mp4"

# Walk the top-level boxes to find mdat's payload (faststart puts it last).
offset=0
size=$(stat -c %s "$tmp/good.mp4")
while [ "$offset" -lt "$size" ]; do
  header=$(od -An -tx1 -j "$offset" -N 8 "$tmp/good.mp4" | tr -d ' \n')
  box=$((16#${header:0:8}))
  type=$(printf '%s' "${header:8:8}" | xxd -r -p)
  if [ "$type" = mdat ]; then break; fi
  offset=$((offset + box))
done
[ "$type" = mdat ] || { echo "make_corrupt: no mdat box" >&2; exit 1; }
payload=$((box - 8))

head -c "$payload" /dev/zero |
  openssl enc -aes-128-ctr -nosalt -K 00000000000000000000000000000000 -iv 00000000000000000000000000000000 >"$tmp/noise"
cp "$tmp/good.mp4" "$out"
dd if="$tmp/noise" of="$out" bs=1 seek=$((offset + 8)) conv=notrunc status=none
sha256sum "$out"
