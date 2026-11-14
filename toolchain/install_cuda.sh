#!/usr/bin/env bash
# install_cuda.sh: the CUDA toolkit pieces whisper.cpp's CUDA backend builds
# against (D131), from NVIDIA's apt repository, at the exact versions in
# versions.env. amd64 Ubuntu 24.04 only: the image's toolchain stage.
#
#   install_cuda.sh            install them
#   install_cuda.sh --remove   purge them again, once build_whisper.sh has
#                              copied the runtime libraries beside whisper-cli
#
# The toolkit never reaches the image: only cudart and cuBLAS do, beside the
# backend that loads them.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
die() { printf 'install_cuda: ERROR: %s\n' "$*" >&2; exit 1; }

# shellcheck source=versions.env disable=SC1091
. "$SCRIPT_DIR/versions.env"
: "${CUDA_VERSION:?versions.env must set CUDA_VERSION}"
: "${CUDA_KEYRING_SHA256:?versions.env must set CUDA_KEYRING_SHA256}"
v="${CUDA_VERSION/./-}"
packages=(
  "cuda-nvcc-$v=$CUDA_NVCC_VERSION"
  "cuda-cudart-$v=$CUDA_CUDART_VERSION"
  "cuda-cudart-dev-$v=$CUDA_CUDART_VERSION"
  "cuda-cccl-$v=$CUDA_CCCL_VERSION"
  "cuda-driver-dev-$v=$CUDA_DRIVER_DEV_VERSION"
  "libcublas-$v=$CUDA_CUBLAS_VERSION"
  "libcublas-dev-$v=$CUDA_CUBLAS_VERSION"
)

if [ "${1:-}" = --remove ]; then
  DEBIAN_FRONTEND=noninteractive apt-get purge -y --auto-remove "${packages[@]%%=*}" cuda-keyring
  rm -rf /usr/local/cuda /usr/local/cuda-"$CUDA_VERSION" /var/lib/apt/lists/*
  exit 0
fi

[ "$(dpkg --print-architecture)" = amd64 ] || die "the CUDA backend is built on amd64 only"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL --retry 3 --retry-delay 5 -o "$tmp/cuda-keyring.deb" "$CUDA_KEYRING_URL"
echo "$CUDA_KEYRING_SHA256  $tmp/cuda-keyring.deb" | sha256sum -c - >/dev/null || die "sha256 mismatch for $CUDA_KEYRING_URL"
dpkg -i "$tmp/cuda-keyring.deb"
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "${packages[@]}"
"/usr/local/cuda-$CUDA_VERSION/bin/nvcc" --version
