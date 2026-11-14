# syntax=docker/dockerfile:1.7
#
# The clipmux image: one binary (serve, the built-in worker, the CLI) with the
# pinned FFmpeg and Shaka Packager from toolchain/. Versions, URLs and digests
# live only in toolchain/versions.env.
#
#   docker build -t clipmux .
#
# ClipMux is AGPL-3.0; FFmpeg in it is a GPLv2+ build run as a separate
# process (D086). /usr/share/doc/clipmux/licenses/ holds every licence and
# SOURCE.md, the written source offer.

ARG BASE=ubuntu:24.04

# ── FFmpeg from source and the pinned packager ───────────────────────────────
FROM ${BASE} AS toolchain
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
ARG DEBIAN_FRONTEND=noninteractive
COPY toolchain/ /tmp/clipmux-toolchain/
RUN --mount=type=cache,target=/var/tmp/ffmpeg-build,sharing=locked \
    set -eux; \
    . /tmp/clipmux-toolchain/apt-packages.env; \
    apt-get update; \
    apt-get install -y --no-install-recommends "${CLIPMUX_BUILD_PACKAGES[@]}" curl ca-certificates xz-utils; \
    CLIPMUX_BUILD_DIR=/var/tmp/ffmpeg-build bash /tmp/clipmux-toolchain/build_ffmpeg.sh /usr/local
# Its own layer: a failed download (GitHub releases can 504) keeps FFmpeg cached.
RUN bash /tmp/clipmux-toolchain/install_packager.sh /usr/local/bin
# whisper.cpp with its CPU variants and Vulkan, in its own directory
# (toolchain/build_whisper.sh). On amd64 it also has CUDA (D131), for NVIDIA
# serverless machines without the Vulkan ICD: the toolkit is installed, built
# against and purged in this one step, so only cudart and cuBLAS remain,
# beside whisper-cli. The models come from the models stage.
RUN --mount=type=cache,target=/var/tmp/whisper-build,sharing=locked \
    set -eux; \
    cuda=0; \
    if [ "$(dpkg --print-architecture)" = amd64 ]; then cuda=1; bash /tmp/clipmux-toolchain/install_cuda.sh; fi; \
    CLIPMUX_WHISPER_CUDA=$cuda CLIPMUX_BUILD_DIR=/var/tmp/whisper-build bash /tmp/clipmux-toolchain/build_whisper.sh /usr/local; \
    if [ "$cuda" = 1 ]; then bash /tmp/clipmux-toolchain/install_cuda.sh --remove; fi

# ── The speech models (D118) ────────────────────────────────────────────────
# Baked in, so a serverless machine (a new one for every job) never downloads
# 0.9 GB before it can start. Checked against the sha256 in versions.env; the
# layer depends only on the pins, so it is rebuilt only when they change.
FROM ${BASE} AS models
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates && rm -rf /var/lib/apt/lists/*
COPY toolchain/versions.env toolchain/fetch_speech_models.sh /tmp/clipmux-toolchain/
RUN bash /tmp/clipmux-toolchain/fetch_speech_models.sh /models && chmod 0644 /models/*

# ── The dashboard (embedded in the binary) ──────────────────────────────────
FROM node:24 AS web
WORKDIR /src
RUN corepack enable
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml tsconfig.base.json ./
COPY packages/ packages/
COPY dashboard/ dashboard/
# Every workspace member the lockfile names, so a frozen install matches it.
COPY e2e/web/package.json e2e/web/
COPY worker/package.json worker/
RUN --mount=type=cache,target=/root/.local/share/pnpm/store \
    pnpm install --frozen-lockfile --filter "@clipmux/dashboard..." \
    && mkdir -p internal/web/dist && pnpm --filter "@clipmux/dashboard..." run build

# ── The Go binary ────────────────────────────────────────────────────────────
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=web /src/internal/web/dist/ internal/web/dist/
ARG VERSION=dev
# The image this build is pushed as (ghcr.io/visheshgubrani/clipmux:sha-…),
# which serverless providers run so they match this server (D058).
ARG WORKER_IMAGE=
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION} -X main.workerImage=${WORKER_IMAGE}" -o /out/clipmux ./cmd/clipmux

# ── Agent tarball (D061) ─────────────────────────────────────────────────────
# Built only when asked for, and before the runtime stage so a plain build
# still makes the image:
#   docker build --target agent --output type=local,dest=dist .
# The pack stage holds the runtime image's files, plus patchelf.
FROM ${BASE} AS agent-pack
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
ARG DEBIAN_FRONTEND=noninteractive
COPY toolchain/apt-packages.env toolchain/pack_agent.sh toolchain/licenses.sh toolchain/versions.env /tmp/clipmux-toolchain/
COPY LICENSE /tmp/clipmux-toolchain/LICENSE
RUN set -eux; \
    . /tmp/clipmux-toolchain/apt-packages.env; \
    apt-get update; \
    if [ "$(dpkg --print-architecture)" = amd64 ]; then CLIPMUX_RUNTIME_PACKAGES+=("${CLIPMUX_RUNTIME_PACKAGES_AMD64[@]}"); fi; \
    apt-get install -y --no-install-recommends "${CLIPMUX_RUNTIME_PACKAGES[@]}" patchelf; \
    rm -rf /var/lib/apt/lists/*
COPY --from=toolchain /usr/local/ /usr/local/
COPY --from=build /out/clipmux /usr/local/bin/clipmux
RUN ldconfig && bash /tmp/clipmux-toolchain/pack_agent.sh /out

FROM scratch AS agent
COPY --from=agent-pack /out/ /

# ── Runtime ──────────────────────────────────────────────────────────────────
FROM ${BASE}
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
ARG DEBIAN_FRONTEND=noninteractive
COPY toolchain/apt-packages.env toolchain/verify_toolchain.sh toolchain/licenses.sh toolchain/versions.env /tmp/clipmux-toolchain/
COPY LICENSE /tmp/clipmux-toolchain/LICENSE
RUN set -eux; \
    . /tmp/clipmux-toolchain/apt-packages.env; \
    apt-get update; \
    if [ "$(dpkg --print-architecture)" = amd64 ]; then CLIPMUX_RUNTIME_PACKAGES+=("${CLIPMUX_RUNTIME_PACKAGES_AMD64[@]}"); fi; \
    apt-get install -y --no-install-recommends "${CLIPMUX_RUNTIME_PACKAGES[@]}" tini; \
    rm -rf /var/lib/apt/lists/*
COPY --from=toolchain /usr/local/ /usr/local/
# The toolchain must still do what the engine asks of it on the final image.
RUN ldconfig \
    && if [ "$(dpkg --print-architecture)" = amd64 ]; then export CLIPMUX_WHISPER_CUDA=1; fi \
    && bash /tmp/clipmux-toolchain/verify_toolchain.sh /usr/local/bin/ffmpeg /usr/local/bin/ffprobe /usr/local/bin/whisper-cli \
    && packager --version \
    && bash /tmp/clipmux-toolchain/licenses.sh /usr/share/doc/clipmux/licenses /tmp/clipmux-toolchain/versions.env /tmp/clipmux-toolchain/LICENSE \
    && rm -rf /tmp/clipmux-toolchain
COPY --from=build /out/clipmux /usr/local/bin/clipmux
COPY --from=models /models/ /usr/share/clipmux/models/

RUN useradd --system --uid 10001 --home-dir /var/lib/clipmux --create-home --shell /usr/sbin/nologin clipmux \
    && install -d -o clipmux -g clipmux -m 0750 /var/lib/clipmux/data \
    && install -d -o clipmux -g clipmux -m 0700 /var/lib/clipmux-agent
LABEL org.opencontainers.image.source=https://github.com/visheshgubrani/clipmux \
      org.opencontainers.image.description="ClipMux: self-hosted video infrastructure" \
      org.opencontainers.image.licenses="AGPL-3.0-only AND GPL-2.0-or-later"
USER 10001
# NVENC needs the driver's video capability, whisper's CUDA backend compute
# and its Vulkan backend graphics (the Vulkan ICD), which NVIDIA's container
# runtime injects only when asked (Modal, RunPod, docker --gpus).
ENV CLIPMUX_DATA_DIR=/var/lib/clipmux/data \
    CLIPMUX_LISTEN=:8080 \
    CLIPMUX_MODELS_DIR=/var/lib/clipmux-agent/models \
    CLIPMUX_BAKED_MODELS_DIR=/usr/share/clipmux/models \
    NVIDIA_DRIVER_CAPABILITIES=compute,video,utility,graphics
VOLUME /var/lib/clipmux/data
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s CMD ["clipmux", "healthcheck"]
ENTRYPOINT ["tini", "--", "clipmux"]
CMD ["serve"]
