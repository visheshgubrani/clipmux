#!/usr/bin/env bash
# Packs the self-contained Linux agent (D061): clipmux, the pinned FFmpeg
# with every shared library it needs except glibc's, the VAAPI drivers
# (radeonsi, and iHD on amd64) with theirs, the pinned Shaka Packager, the
# licences, and checksums. Runs inside the Dockerfile's agent-pack stage,
# which holds exactly the runtime image's files.
#
#   pack_agent.sh OUT_DIR
#
# Layout of clipmux-agent-linux-{arch}.tar.gz:
#   clipmux-agent/clipmux            launcher: points clipmux at the tools below
#   clipmux-agent/libexec/           clipmux, ffmpeg, ffprobe, packager, lib/, dri/,
#                                    whisper/ (whisper-cli, its libraries and backends)
#   clipmux-agent/licenses/          GPL notices, the toolchain manifest, copyrights
#   clipmux-agent/SHA256SUMS
#
# The tools and libraries keep everything in libexec/ (found through
# $ORIGIN rpaths), because the media sandbox lets a tool read its own
# directory and the system's, nothing else.
set -euo pipefail
out=${1:?usage: pack_agent.sh OUT_DIR}
arch=$(dpkg --print-architecture)
root=$(mktemp -d)/clipmux-agent
lx="$root/libexec"
mkdir -p "$lx/lib" "$lx/dri" "$root/licenses"

cp /usr/local/bin/clipmux /usr/local/bin/ffmpeg /usr/local/bin/ffprobe /usr/local/bin/packager "$lx/"

# glibc and the loader stay the host's: they must match its kernel ABI, and
# every Linux has them. Everything else ffmpeg or a driver loads is bundled.
system_lib() {
  case "$(basename "$1")" in
    ld-linux*|libc.so*|libm.so*|libpthread.so*|libdl.so*|librt.so*|libresolv.so*|libutil.so*|libanl.so*|libmvec.so*) return 0 ;;
  esac
  return 1
}
bundle_deps() {
  ldd "$1" | awk '/=> \// { print $3 }' | while read -r lib; do
    system_lib "$lib" && continue
    [ -e "$lx/lib/$(basename "$lib")" ] || cp -L "$lib" "$lx/lib/"
  done
}

bundle_deps "$lx/ffmpeg"
bundle_deps "$lx/ffprobe"
# whisper.cpp keeps its own directory: ggml loads its backends (the CPU
# variants, Vulkan) from beside the executable, so their dependencies go
# there too. Vulkan drivers stay the host's (mesa's or NVIDIA's ICD); a host
# without one transcribes on the CPU.
cp -a /usr/local/libexec/clipmux-whisper "$lx/whisper"
for f in "$lx"/whisper/*; do
  [ -L "$f" ] && continue
  ldd "$f" | awk '/=> \// { print $3 }' | while read -r lib; do
    system_lib "$lib" && continue
    [ -e "$lx/whisper/$(basename "$lib")" ] || cp -L "$lib" "$lx/whisper/"
  done
done
# The VAAPI drivers libva loads at run time: radeonsi (AMD) everywhere,
# iHD (Intel) where Ubuntu ships it.
for drv in radeonsi_drv_video.so iHD_drv_video.so; do
  path=$(find /usr/lib -name "$drv" -print -quit)
  if [ -z "$path" ]; then
    echo "pack_agent: no $drv on $arch; skipped" >&2
    continue
  fi
  cp -L "$path" "$lx/dri/"
  bundle_deps "$lx/dri/$drv"
done

# Each binary finds its libraries beside it. DT_RPATH (--force-rpath), not
# RUNPATH, so a library's own dependencies resolve the same way.
patchelf --force-rpath --set-rpath '$ORIGIN/lib' "$lx/ffmpeg" "$lx/ffprobe"
for f in "$lx"/lib/*.so*; do patchelf --force-rpath --set-rpath '$ORIGIN' "$f"; done
for f in "$lx"/dri/*.so; do patchelf --force-rpath --set-rpath '$ORIGIN/../lib' "$f"; done
find "$lx/whisper" -type f -exec patchelf --force-rpath --set-rpath '$ORIGIN' {} \;

cat >"$root/clipmux" <<'LAUNCHER'
#!/bin/sh
# The ClipMux agent with its own FFmpeg, packager and VAAPI drivers.
here=$(cd "$(dirname "$(readlink -f "$0")")" && pwd)
export CLIPMUX_FFMPEG="${CLIPMUX_FFMPEG:-$here/libexec/ffmpeg}"
export CLIPMUX_FFPROBE="${CLIPMUX_FFPROBE:-$here/libexec/ffprobe}"
export CLIPMUX_PACKAGER="${CLIPMUX_PACKAGER:-$here/libexec/packager}"
export CLIPMUX_WHISPER="${CLIPMUX_WHISPER:-$here/libexec/whisper/whisper-cli}"
export LIBVA_DRIVERS_PATH="${LIBVA_DRIVERS_PATH:-$here/libexec/dri}"
exec "$here/libexec/clipmux" "$@"
LAUNCHER
chmod 0755 "$root/clipmux"

# Licences: FFmpeg's GPL build notices and manifest (toolchain/build_ffmpeg.sh),
# and the copyright file of every Ubuntu package a bundled file came from.
cp -r /usr/local/share/doc/clipmux-ffmpeg "$root/licenses/ffmpeg"
cp /usr/local/share/clipmux/toolchain.json "$root/licenses/ffmpeg/"
cp -r /usr/local/share/doc/clipmux-whisper "$root/licenses/whisper"
cp /usr/local/share/clipmux/whisper.json "$root/licenses/whisper/"
cat >"$root/licenses/README" <<'README'
FFmpeg here is built from source under the GPL (see ffmpeg/: its licence,
the GPLv2 and LGPLv2.1 texts, and the exact sources it was built from).
Shaka Packager is BSD-licensed. whisper.cpp is MIT-licensed (see whisper/;
its models are fetched at run time and are MIT too). Every other library
comes from an Ubuntu package, whose copyright file is in packages/.
README
mkdir -p "$root/licenses/packages"
for f in "$lx"/lib/*.so* "$lx"/dri/*.so "$lx"/whisper/*.so*; do
  src=$(find /usr/lib /lib -name "$(basename "$f")" -print -quit 2>/dev/null || true)
  [ -n "$src" ] || continue
  pkg=$(dpkg -S "$(readlink -f "$src")" 2>/dev/null | head -1 | cut -d: -f1 || true)
  [ -n "$pkg" ] && [ -f "/usr/share/doc/$pkg/copyright" ] && cp "/usr/share/doc/$pkg/copyright" "$root/licenses/packages/$pkg"
done

(cd "$root" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum >SHA256SUMS)
mkdir -p "$out"
tar -C "$(dirname "$root")" -czf "$out/clipmux-agent-linux-$arch.tar.gz" clipmux-agent
(cd "$out" && sha256sum "clipmux-agent-linux-$arch.tar.gz" >"clipmux-agent-linux-$arch.tar.gz.sha256")
echo "pack_agent: $(du -h "$out/clipmux-agent-linux-$arch.tar.gz" | cut -f1) clipmux-agent-linux-$arch.tar.gz"
