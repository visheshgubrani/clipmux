# syntax=docker/dockerfile:1.29
#
# The clipmux image: one binary (serve, the built-in worker, the CLI) with the
# pinned FFmpeg, whisper.cpp, Shaka Packager and speech models from
# toolchain/. Versions, URLs and digests of the tools live only in
# toolchain/versions.env.
#
#   docker build -t clipmux .
#
# ClipMux is AGPL-3.0; FFmpeg in it is a GPLv2+ build run as a separate
# process (D086). /usr/share/doc/clipmux/licenses/ holds every licence and
# SOURCE.md, the written source offer.
#
# Layers keep their bytes across releases (D142). A host that ran the last
# release downloads only what changed, which for a release that changes only
# ClipMux is its binary. So:
#   * every base is pinned by digest, and Dependabot bumps it on purpose;
#   * each tool builds in its own stage, keyed only on its own pins (the
#     pins stage cuts versions.env up per tool) and its own scripts, which
#     come in by bind mount, never COPY toolchain/;
#   * the runtime stage stacks the layers from the one that changes least to
#     the binary, which is last; COPY --link keeps each copied layer the same
#     whatever is beneath it.
# toolchain/check_layers.sh checks it (install.yml, image.yml).

# ── Bases ───────────────────────────────────────────────────────────────────
FROM ubuntu:24.04@sha256:9c7f200c07e3d4a03e7249d2c16bf66a51120fb876aa2242cf31451412543289 AS base
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# ── Each tool's pins, cut out of versions.env ───────────────────────────────
# A stage copies only its own file, so BuildKit keys it on that file's
# content: bumping CUDA no longer rebuilds FFmpeg or fetches the models
# again. Each file must source on its own (no pin may name another tool's).
FROM base AS pins
RUN --mount=type=bind,source=toolchain/versions.env,target=/tmp/versions.env \
    set -eux; \
    mkdir /pins; \
    grep -E '^(FFMPEG|NV_CODEC_HEADERS)_' /tmp/versions.env >/pins/ffmpeg.env; \
    grep -E '^(WHISPER_CPP|WHISPER_CUDA|CUDA)_' /tmp/versions.env >/pins/whisper.env; \
    grep -E '^SHAKA_' /tmp/versions.env >/pins/shaka.env; \
    grep -E '^WHISPER_(VAD_)?MODEL_' /tmp/versions.env >/pins/models.env; \
    for f in /pins/*.env; do (set -u; . "$f"); done

# ── Build tools, shared by FFmpeg and whisper.cpp ───────────────────────────
FROM base AS builddeps
ARG DEBIAN_FRONTEND=noninteractive
RUN --mount=type=bind,source=toolchain/apt-packages.env,target=/tc/apt-packages.env \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    set -eux; \
    rm -f /etc/apt/apt.conf.d/docker-clean; \
    echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' >/etc/apt/apt.conf.d/keep-cache; \
    . /tc/apt-packages.env; \
    apt-get update; \
    apt-get install -y --no-install-recommends "${CLIPMUX_BUILD_PACKAGES[@]}"

# Downloads only: the packager and the models need nothing more than curl.
FROM base AS fetch
ARG DEBIAN_FRONTEND=noninteractive
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends curl ca-certificates

# ── FFmpeg from source ──────────────────────────────────────────────────────
FROM builddeps AS ffmpeg
COPY --from=pins /pins/ffmpeg.env /tc/versions.env
RUN --mount=type=bind,source=toolchain/build_ffmpeg.sh,target=/tc/build_ffmpeg.sh \
    --mount=type=bind,source=toolchain/apt-packages.env,target=/tc/apt-packages.env \
    --mount=type=cache,target=/var/tmp/ffmpeg-build,sharing=locked \
    CLIPMUX_BUILD_DIR=/var/tmp/ffmpeg-build bash /tc/build_ffmpeg.sh /usr/local

# ── whisper.cpp ─────────────────────────────────────────────────────────────
# Its CPU variants and Vulkan, in its own directory (build_whisper.sh). On
# amd64 it also has CUDA (D131), for NVIDIA serverless machines without the
# Vulkan ICD: the toolkit is installed, built against and purged in this one
# step, so only cudart and cuBLAS remain, beside whisper-cli.
FROM builddeps AS whisper
ARG DEBIAN_FRONTEND=noninteractive
COPY --from=pins /pins/whisper.env /tc/versions.env
RUN --mount=type=bind,source=toolchain/build_whisper.sh,target=/tc/build_whisper.sh \
    --mount=type=bind,source=toolchain/install_cuda.sh,target=/tc/install_cuda.sh \
    --mount=type=bind,source=toolchain/whisper-vad-token-times.patch,target=/tc/whisper-vad-token-times.patch \
    --mount=type=cache,target=/var/tmp/whisper-build,sharing=locked \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    set -eux; \
    cuda=0; \
    if [ "$(dpkg --print-architecture)" = amd64 ]; then cuda=1; bash /tc/install_cuda.sh; fi; \
    CLIPMUX_WHISPER_CUDA=$cuda CLIPMUX_BUILD_DIR=/var/tmp/whisper-build bash /tc/build_whisper.sh /usr/local; \
    if [ "$cuda" = 1 ]; then bash /tc/install_cuda.sh --remove; fi

# ── The pinned packager ─────────────────────────────────────────────────────
FROM fetch AS packager
COPY --from=pins /pins/shaka.env /tc/versions.env
RUN --mount=type=bind,source=toolchain/install_packager.sh,target=/tc/install_packager.sh \
    mkdir /out && bash /tc/install_packager.sh /out

# ── The speech models (D118) ────────────────────────────────────────────────
# Baked in, so a serverless machine (a new one for every job) never downloads
# 0.9 GB before it can start. Checked against the sha256 in versions.env.
# A fixed mtime keeps the layer's bytes the same if this step ever runs again
# for the same models.
FROM fetch AS models
COPY --from=pins /pins/models.env /tc/versions.env
RUN --mount=type=bind,source=toolchain/fetch_speech_models.sh,target=/tc/fetch_speech_models.sh \
    bash /tc/fetch_speech_models.sh /models && chmod 0644 /models/* && touch -d @0 /models /models/*

# ── The dashboard (embedded in the binary) ──────────────────────────────────
FROM node:24@sha256:9d266751ca3d7fdbe651c878ffd5ad0a42fed49a18c283509267c97f025b7496 AS web
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
FROM golang:1.27@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build
WORKDIR /src
COPY go.mod go.sum ./
# A layer of its own (no cache mount), so the build cache in the registry
# serves the modules to a fresh runner.
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=web /src/internal/web/dist/ internal/web/dist/
ARG VERSION=dev
# The image this build is pushed as (ghcr.io/visheshgubrani/clipmux:sha-…),
# which serverless providers run so they match this server (D058).
ARG WORKER_IMAGE=
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION} -X main.workerImage=${WORKER_IMAGE}" -o /out/clipmux ./cmd/clipmux

# ── Agent tarball (D061) ─────────────────────────────────────────────────────
# Built only when asked for, and before the runtime stage so a plain build
# still makes the image:
#   docker build --target agent --output type=local,dest=dist .
# The pack stage holds the runtime image's files, plus patchelf.
FROM base AS agent-pack
ARG DEBIAN_FRONTEND=noninteractive
RUN --mount=type=bind,source=toolchain/apt-packages.env,target=/tc/apt-packages.env \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    set -eux; \
    . /tc/apt-packages.env; \
    apt-get update; \
    if [ "$(dpkg --print-architecture)" = amd64 ]; then CLIPMUX_RUNTIME_PACKAGES+=("${CLIPMUX_RUNTIME_PACKAGES_AMD64[@]}"); fi; \
    apt-get install -y --no-install-recommends "${CLIPMUX_RUNTIME_PACKAGES[@]}" patchelf
COPY --from=ffmpeg /usr/local/ /usr/local/
COPY --from=whisper /usr/local/ /usr/local/
COPY --from=packager /out/packager /usr/local/bin/packager
COPY --from=build /out/clipmux /usr/local/bin/clipmux
RUN --mount=type=bind,source=toolchain/pack_agent.sh,target=/tc/pack_agent.sh \
    --mount=type=bind,source=toolchain/licenses.sh,target=/tc/licenses.sh \
    --mount=type=bind,source=toolchain/versions.env,target=/tc/versions.env \
    --mount=type=bind,source=LICENSE,target=/tc/LICENSE \
    ldconfig && bash /tc/pack_agent.sh /out

FROM scratch AS agent
COPY --from=agent-pack /out/ /

# ── Runtime ──────────────────────────────────────────────────────────────────
# Bottom to top, from the layer that changes least to the binary, which
# changes every release.
FROM base
ARG DEBIAN_FRONTEND=noninteractive
RUN --mount=type=bind,source=toolchain/apt-packages.env,target=/tc/apt-packages.env \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    set -eux; \
    . /tc/apt-packages.env; \
    apt-get update; \
    if [ "$(dpkg --print-architecture)" = amd64 ]; then CLIPMUX_RUNTIME_PACKAGES+=("${CLIPMUX_RUNTIME_PACKAGES_AMD64[@]}"); fi; \
    apt-get install -y --no-install-recommends "${CLIPMUX_RUNTIME_PACKAGES[@]}" tini; \
    useradd --system --uid 10001 --home-dir /var/lib/clipmux --create-home --shell /usr/sbin/nologin clipmux; \
    install -d -o clipmux -g clipmux -m 0750 /var/lib/clipmux/data; \
    install -d -o clipmux -g clipmux -m 0700 /var/lib/clipmux-agent
COPY --link --from=models /models/ /usr/share/clipmux/models/
COPY --link --from=ffmpeg /usr/local/ /usr/local/
COPY --link --from=whisper /usr/local/ /usr/local/
COPY --link --from=packager /out/packager /usr/local/bin/packager
# The toolchain must still do what the engine asks of it on the final image.
RUN --mount=type=bind,source=toolchain/verify_toolchain.sh,target=/tc/verify_toolchain.sh \
    --mount=type=bind,source=toolchain/licenses.sh,target=/tc/licenses.sh \
    --mount=type=bind,source=toolchain/versions.env,target=/tc/versions.env \
    --mount=type=bind,source=LICENSE,target=/tc/LICENSE \
    ldconfig \
    && if [ "$(dpkg --print-architecture)" = amd64 ]; then export CLIPMUX_WHISPER_CUDA=1; fi \
    && bash /tc/verify_toolchain.sh /usr/local/bin/ffmpeg /usr/local/bin/ffprobe /usr/local/bin/whisper-cli \
    && packager --version \
    && bash /tc/licenses.sh /usr/share/doc/clipmux/licenses /tc/versions.env /tc/LICENSE
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
# No HEALTHCHECK: the same image runs `worker --once` on Modal and RunPod
# and as a PC agent, which serve no /healthz. deploy/compose.yml and
# compose.release.yml declare `clipmux healthcheck` for serve.
ENTRYPOINT ["tini", "--", "clipmux"]
CMD ["serve"]
# Last, so a release that changes only ClipMux is one small layer.
COPY --link --from=build /out/clipmux /usr/local/bin/clipmux
