#!/usr/bin/env bash
#
# Build the pinned whisper.cpp (speech to text, P5-T11) into a self-contained
# directory.
#
#   ./build_whisper.sh [install-prefix]          # default /usr/local
#   CLIPMUX_BUILD_DIR=/var/tmp/whisper-build ./build_whisper.sh /usr/local
#   JOBS=8 ./build_whisper.sh --force /usr/local
#
# Installs:
#   <prefix>/libexec/clipmux-whisper/whisper-cli   the tool
#   <prefix>/libexec/clipmux-whisper/lib*.so*      libwhisper, libggml and every
#                                                  backend, beside the tool
#   <prefix>/bin/whisper-cli                       a symlink to it
#   <prefix>/share/clipmux/whisper.json            build manifest
#   <prefix>/share/doc/clipmux-whisper/            LICENSE and NOTICE
#
# Why one directory: ggml loads its backends (the CPU variants and Vulkan) at
# run time from the executable's own directory, and the sandbox lets a tool
# read its own directory (internal/sandbox), so the tool, its libraries and
# its backends travel together, with $ORIGIN rpaths, into the image and the
# agent tarball alike.
#
# Backends:
#   * CPU, every variant ggml ships for the architecture
#     (GGML_CPU_ALL_VARIANTS): the best one the CPU supports is chosen at run
#     time, so one build runs well on any machine instead of being tuned to
#     the builder's.
#   * Vulkan: AMD (radv), Intel (anv) and NVIDIA (the driver's own ICD, which
#     the container runtime injects with `graphics` in
#     NVIDIA_DRIVER_CAPABILITIES). A machine without a Vulkan driver simply
#     doesn't load it and runs on the CPU.
#   * CUDA, with CLIPMUX_WHISPER_CUDA=1 (the amd64 image, D131): NVIDIA
#     cards where the Vulkan ICD isn't offered, as under Modal's gVisor.
#     Needs nvcc (install_cuda.sh); cudart and cuBLAS are copied beside the
#     backend, and the driver's libcuda.so.1 is the container runtime's.
#
# Built without libcurl (WHISPER_CURL=OFF) and without SDL: the tool never
# fetches a model or opens a device of its own; workers download models by
# digest (internal/worker) and run whisper-cli in the sandbox, where CPU
# tools can't open a socket.
#
# One patch is applied (whisper-vad-token-times.patch): with --vad,
# whisper-cli's JSON wrote token times on the speech-only timeline VAD
# makes, while segment times were mapped back to the audio, so every word
# after a removed silence was early. The library already maps token times
# (whisper_full_get_token_t0/t1); the patch makes the CLI use them. Drop it
# once upstream's CLI does.
#
# Idempotent: the manifest records the version, the CMake line and hashes of
# this script and the patch; a match stops the script before downloading
# anything.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

die() { printf 'build_whisper: ERROR: %s\n' "$*" >&2; exit 1; }
log() { printf 'build_whisper: %s\n' "$*"; }

FORCE=0
PREFIX="/usr/local"
for arg in "$@"; do
  case "$arg" in
    --force) FORCE=1 ;;
    -h|--help) sed -n '2,40p' "${BASH_SOURCE[0]}"; exit 0 ;;
    -*) die "unknown option: $arg" ;;
    *) PREFIX="$arg" ;;
  esac
done
PREFIX="$(readlink -f -- "$PREFIX" 2>/dev/null || printf '%s' "$PREFIX")"

[ -f "$SCRIPT_DIR/versions.env" ] || die "missing $SCRIPT_DIR/versions.env"
# shellcheck source=versions.env disable=SC1091
. "$SCRIPT_DIR/versions.env"
: "${WHISPER_CPP_VERSION:?versions.env must set WHISPER_CPP_VERSION}"
: "${WHISPER_CPP_SHA:?versions.env must set WHISPER_CPP_SHA}"
: "${WHISPER_CPP_SHA256:?versions.env must set WHISPER_CPP_SHA256}"
: "${WHISPER_CPP_URL:?versions.env must set WHISPER_CPP_URL}"
[ "${#WHISPER_CPP_SHA}" -eq 40 ] || die "WHISPER_CPP_SHA is not a 40-char commit sha"
[ "${#WHISPER_CPP_SHA256}" -eq 64 ] || die "WHISPER_CPP_SHA256 is not a sha256"

BUILD_DIR="${CLIPMUX_BUILD_DIR:-/tmp/clipmux-whisper-build}"
JOBS="${JOBS:-$(nproc)}"
[[ "$JOBS" =~ ^[1-9][0-9]*$ ]] || die "JOBS must be a positive integer (got '$JOBS')"
SRC="$BUILD_DIR/whisper.cpp-$WHISPER_CPP_SHA"
OUT="$BUILD_DIR/build"
DOWNLOADS="$BUILD_DIR/downloads"
DEST="$PREFIX/libexec/clipmux-whisper"
MANIFEST="$PREFIX/share/clipmux/whisper.json"
DOC_DIR="$PREFIX/share/doc/clipmux-whisper"

# The CMake line is the contract: verify_toolchain.sh checks what it promises.
CMAKE_ARGS=(
  -DCMAKE_BUILD_TYPE=Release
  -DBUILD_SHARED_LIBS=ON
  # Backends as loadable modules, so the CPU variants and Vulkan are chosen
  # at run time from what the machine has.
  -DGGML_BACKEND_DL=ON
  -DGGML_NATIVE=OFF
  -DGGML_CPU_ALL_VARIANTS=ON
  -DGGML_VULKAN=ON
  # ggml's own thread pool: no libgomp to carry.
  -DGGML_OPENMP=OFF
  -DWHISPER_CURL=OFF
  -DWHISPER_SDL2=OFF
  -DWHISPER_FFMPEG=OFF
  -DWHISPER_BUILD_TESTS=OFF
  -DWHISPER_BUILD_SERVER=OFF
  -DWHISPER_BUILD_EXAMPLES=ON
  '-DCMAKE_BUILD_RPATH=$ORIGIN'
  -DCMAKE_BUILD_WITH_INSTALL_RPATH=ON
  '-DCMAKE_INSTALL_RPATH=$ORIGIN'
)
CUDA="${CLIPMUX_WHISPER_CUDA:-0}"
CUDA_HOME=""
if [ "$CUDA" = 1 ]; then
  : "${WHISPER_CUDA_ARCHITECTURES:?versions.env must set WHISPER_CUDA_ARCHITECTURES}"
  NVCC="${CUDACXX:-$(command -v nvcc || echo "/usr/local/cuda-${CUDA_VERSION:-}/bin/nvcc")}"
  [ -x "$NVCC" ] || die "CLIPMUX_WHISPER_CUDA=1 needs nvcc (toolchain/install_cuda.sh)"
  CUDA_HOME="$(dirname "$(dirname "$(readlink -f "$NVCC")")")"
  CMAKE_ARGS+=(
    -DGGML_CUDA=ON
    "-DCMAKE_CUDA_COMPILER=$NVCC"
    "-DCMAKE_CUDA_ARCHITECTURES=$WHISPER_CUDA_ARCHITECTURES"
    -DGGML_CUDA_NCCL=OFF
  )
fi
PATCH="$SCRIPT_DIR/whisper-vad-token-times.patch"
[ -f "$PATCH" ] || die "missing $PATCH"
# The CMake line is part of the recipe: CUDA on or off, and its targets.
RECIPE_SHA256="$( { cat "${BASH_SOURCE[0]}" "$PATCH"; printf '%s\n' "${CMAKE_ARGS[*]}"; } | sha256sum | cut -d' ' -f1)"

if [ "$FORCE" -eq 0 ] && [ -f "$MANIFEST" ] && [ -x "$DEST/whisper-cli" ] \
  && grep -q "\"recipe_sha256\": \"$RECIPE_SHA256\"" "$MANIFEST" \
  && grep -q "\"commit\": \"$WHISPER_CPP_SHA\"" "$MANIFEST"; then
  log "already built: $DEST/whisper-cli ($WHISPER_CPP_VERSION); use --force to rebuild"
  exit 0
fi

for tool in curl sha256sum tar cmake make glslc patch; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done

mkdir -p "$DOWNLOADS"
TARBALL="$DOWNLOADS/whisper.cpp-$WHISPER_CPP_SHA.tar.gz"
if [ -f "$TARBALL" ] && [ "$(sha256sum "$TARBALL" | cut -d' ' -f1)" = "$WHISPER_CPP_SHA256" ]; then
  log "cached + verified $(basename "$TARBALL")"
else
  log "downloading $WHISPER_CPP_URL"
  rm -f "$TARBALL"
  curl -fsSL --retry 3 --retry-delay 5 -o "$TARBALL" "$WHISPER_CPP_URL" || die "download failed: $WHISPER_CPP_URL"
  got="$(sha256sum "$TARBALL" | cut -d' ' -f1)"
  [ "$got" = "$WHISPER_CPP_SHA256" ] || die "sha256 mismatch for $WHISPER_CPP_URL
  expected $WHISPER_CPP_SHA256
  got      $got"
fi

rm -rf "$SRC" "$OUT"
mkdir -p "$SRC"
tar -xzf "$TARBALL" -C "$SRC" --strip-components=1
patch -d "$SRC" -p1 --forward --batch < "$PATCH" > "$BUILD_DIR/patch.log" 2>&1 \
  || { cat "$BUILD_DIR/patch.log" >&2; die "the patch no longer applies to $WHISPER_CPP_VERSION"; }
# ggml's Linux arm64 variants end at armv9.2 with SME, which GCC 13 (Ubuntu
# 24.04) can't compile. Without a compiler that accepts +sme, those two
# variants are dropped; armv8.0 to armv8.6 with SVE2 still cover every arm64
# CPU, and SME machines are rare on Linux today.
if [ "$(uname -m)" = aarch64 ] && ! echo 'int x;' | "${CC:-cc}" -march=armv9.2-a+sme -x c -c -o /dev/null - 2>/dev/null; then
  cpu_cmake="$SRC/ggml/src/CMakeLists.txt"
  sed -i '/ggml_add_cpu_backend_variant(armv9\.2_[12] /d' "$cpu_cmake"
  ! grep -q 'ggml_add_cpu_backend_variant(armv9\.2_' "$cpu_cmake" || die "couldn't drop the SME variants from $cpu_cmake"
  log "the compiler lacks +sme: building without ggml's armv9.2 (SME) CPU variants"
fi
log "configuring whisper.cpp $WHISPER_CPP_VERSION"
cmake -S "$SRC" -B "$OUT" "${CMAKE_ARGS[@]}" > "$BUILD_DIR/cmake.log" 2>&1 \
  || { tail -n 60 "$BUILD_DIR/cmake.log" >&2; die "cmake failed (full log: $BUILD_DIR/cmake.log)"; }
log "building with $JOBS jobs"
cmake --build "$OUT" --config Release -j "$JOBS" --target whisper-cli > "$BUILD_DIR/build.log" 2>&1 \
  || { tail -n 60 "$BUILD_DIR/build.log" >&2; die "build failed (full log: $BUILD_DIR/build.log)"; }

# Everything the tool needs sits beside it.
rm -rf "$DEST"
mkdir -p "$DEST" "$PREFIX/bin" "$DOC_DIR" "$(dirname "$MANIFEST")"
cp "$OUT/bin/whisper-cli" "$DEST/"
find "$OUT" \( -name 'libwhisper.so*' -o -name 'libggml*.so*' \) -exec cp -P {} "$DEST/" \;
ln -sfn ../libexec/clipmux-whisper/whisper-cli "$PREFIX/bin/whisper-cli"
ls "$DEST"/libggml-cpu-*.so >/dev/null 2>&1 || die "no CPU backend variants were built"
[ -f "$DEST/libggml-vulkan.so" ] || die "the Vulkan backend wasn't built"
"$DEST/whisper-cli" --help >/dev/null 2>&1 || die "the built whisper-cli doesn't run"

cp "$SRC/LICENSE" "$DOC_DIR/LICENSE"
if [ "$CUDA" = 1 ]; then
  [ -f "$DEST/libggml-cuda.so" ] || die "the CUDA backend wasn't built"
  # cudart and cuBLAS go beside the backend, where its $ORIGIN rpath finds
  # them and the sandbox lets the tool read; libcuda.so.1 stays the driver's.
  LD_LIBRARY_PATH="$CUDA_HOME/lib64" ldd "$DEST/libggml-cuda.so" | awk '/libcudart|libcublas/ { print $3 }' | while read -r lib; do
    [ -f "$lib" ] || die "the CUDA backend needs $lib, which isn't there"
    cp -L "$lib" "$DEST/"
  done
  for want in libcudart.so libcublas.so libcublasLt.so; do
    ls "$DEST/$want".* >/dev/null 2>&1 || die "no $want beside the CUDA backend"
  done
  # Redistributed under NVIDIA's CUDA EULA; its terms travel with them.
  mkdir -p "$DOC_DIR/NVIDIA-CUDA"
  for notice in "$CUDA_HOME/EULA.txt" /usr/share/doc/cuda-cudart-"${CUDA_VERSION/./-}"/copyright /usr/share/doc/libcublas-"${CUDA_VERSION/./-}"/copyright; do
    if [ -f "$notice" ]; then
      cp "$notice" "$DOC_DIR/NVIDIA-CUDA/$(basename "$(dirname "$notice")")-$(basename "$notice")"
    fi
  done
  ls "$DOC_DIR/NVIDIA-CUDA"/* >/dev/null 2>&1 || die "found no NVIDIA licence text to ship with the CUDA libraries"
fi
cat > "$DOC_DIR/NOTICE" <<NOTICE
whisper.cpp $WHISPER_CPP_VERSION (MIT), built by toolchain/build_whisper.sh.
Source: $WHISPER_CPP_URL
sha256: $WHISPER_CPP_SHA256
Patched with toolchain/whisper-vad-token-times.patch (token times with VAD).
The speech models it runs, and their licences: models/NOTICE in the
licences directory beside this one (toolchain/licenses.sh).
NOTICE

{
  printf '{\n'
  printf '  "tool": "whisper.cpp",\n'
  printf '  "version": "%s",\n' "$WHISPER_CPP_VERSION"
  printf '  "commit": "%s",\n' "$WHISPER_CPP_SHA"
  printf '  "source_sha256": "%s",\n' "$WHISPER_CPP_SHA256"
  printf '  "recipe_sha256": "%s",\n' "$RECIPE_SHA256"
  printf '  "cmake": "%s",\n' "${CMAKE_ARGS[*]}"
  printf '  "backends": [%s]\n' "$(cd "$DEST" && ls libggml-*.so | sed 's/^libggml-//; s/\.so$//' | sed 's/.*/"&"/' | paste -sd, -)"
  printf '}\n'
} > "$MANIFEST"
log "installed $DEST ($(cd "$DEST" && ls libggml-*.so | tr '\n' ' '))"
