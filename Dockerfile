# syntax=docker/dockerfile:1.7
#
# The clipmux image: one binary (serve, the built-in worker, the CLI) with the
# pinned FFmpeg and Shaka Packager from toolchain/. Versions, URLs and digests
# live only in toolchain/versions.env.
#
#   docker build -t clipmux .
#
# Not published until the licence is decided (docs/decisions.md D005, D006).

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
# (toolchain/build_whisper.sh). The models are fetched by workers, not baked in.
RUN --mount=type=cache,target=/var/tmp/whisper-build,sharing=locked \
    CLIPMUX_BUILD_DIR=/var/tmp/whisper-build bash /tmp/clipmux-toolchain/build_whisper.sh /usr/local

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
COPY toolchain/apt-packages.env toolchain/pack_agent.sh /tmp/clipmux-toolchain/
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
COPY toolchain/apt-packages.env toolchain/verify_toolchain.sh /tmp/clipmux-toolchain/
RUN set -eux; \
    . /tmp/clipmux-toolchain/apt-packages.env; \
    apt-get update; \
    if [ "$(dpkg --print-architecture)" = amd64 ]; then CLIPMUX_RUNTIME_PACKAGES+=("${CLIPMUX_RUNTIME_PACKAGES_AMD64[@]}"); fi; \
    apt-get install -y --no-install-recommends "${CLIPMUX_RUNTIME_PACKAGES[@]}" tini; \
    rm -rf /var/lib/apt/lists/*
COPY --from=toolchain /usr/local/ /usr/local/
# The toolchain must still do what the engine asks of it on the final image.
RUN ldconfig && bash /tmp/clipmux-toolchain/verify_toolchain.sh /usr/local/bin/ffmpeg /usr/local/bin/ffprobe /usr/local/bin/whisper-cli \
    && packager --version && rm -rf /tmp/clipmux-toolchain
COPY --from=build /out/clipmux /usr/local/bin/clipmux

RUN useradd --system --uid 10001 --home-dir /var/lib/clipmux --create-home --shell /usr/sbin/nologin clipmux \
    && install -d -o clipmux -g clipmux -m 0750 /var/lib/clipmux/data \
    && install -d -o clipmux -g clipmux -m 0700 /var/lib/clipmux-agent
USER 10001
# NVENC needs the driver's video capability and whisper's Vulkan backend its
# graphics one (the Vulkan ICD), which NVIDIA's container runtime injects
# only when asked (Modal, RunPod, docker --gpus).
ENV CLIPMUX_DATA_DIR=/var/lib/clipmux/data \
    CLIPMUX_LISTEN=:8080 \
    NVIDIA_DRIVER_CAPABILITIES=compute,video,utility,graphics
VOLUME /var/lib/clipmux/data
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s CMD ["clipmux", "healthcheck"]
ENTRYPOINT ["tini", "--", "clipmux"]
CMD ["serve"]
