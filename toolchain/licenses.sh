#!/usr/bin/env bash
# Writes the licences and the written source offer that ship with every
# binary distribution of ClipMux (D086): the image (Dockerfile, runtime
# stage) and the agent tarball (pack_agent.sh, which adds the copyright
# files of the libraries it bundles under packages/).
#
#   licenses.sh OUT_DIR VERSIONS_ENV CLIPMUX_LICENSE
#
# OUT_DIR gets:
#   README          what is licensed how
#   SOURCE.md       the source offer: pinned sources and their digests
#   clipmux/        ClipMux's own licence (AGPL-3.0)
#   ffmpeg/         FFmpeg's licence, the GPLv2 and LGPLv2.1, its NOTICE and
#                   build manifest (toolchain/build_ffmpeg.sh)
#   whisper/        whisper.cpp's licence and build manifest
#   models/         where the speech models come from and their licences
#                   (the image bakes them in, D118; an agent fetches them)
#   packages.txt    every Debian package in the stage, with its source package
#                   and version, for the libraries FFmpeg links against
#
# Reads what build_ffmpeg.sh and build_whisper.sh installed under /usr/local.
set -euo pipefail
out=${1:?usage: licenses.sh OUT_DIR VERSIONS_ENV CLIPMUX_LICENSE}
versions=${2:?usage: licenses.sh OUT_DIR VERSIONS_ENV CLIPMUX_LICENSE}
license=${3:?usage: licenses.sh OUT_DIR VERSIONS_ENV CLIPMUX_LICENSE}
# shellcheck source=/dev/null
. "$versions"

mkdir -p "$out/clipmux"
cp "$license" "$out/clipmux/LICENSE"
cp -r /usr/local/share/doc/clipmux-ffmpeg "$out/ffmpeg"
cp /usr/local/share/clipmux/toolchain.json "$out/ffmpeg/"
cp -r /usr/local/share/doc/clipmux-whisper "$out/whisper"
cp /usr/local/share/clipmux/whisper.json "$out/whisper/"
mkdir -p "$out/models"
cat >"$out/models/NOTICE" <<NOTICE
The speech models, pinned in toolchain/versions.env. The image carries them
in /usr/share/clipmux/models (D118); a PC agent downloads them at start.

${WHISPER_MODEL_NAME}: ${WHISPER_MODEL_URL}
  sha256 ${WHISPER_MODEL_SHA256}
  OpenAI's Whisper large-v3-turbo weights in whisper.cpp's ggml format,
  released by OpenAI under the MIT licence (Copyright (c) 2022 OpenAI;
  https://github.com/openai/whisper/blob/main/LICENSE).

${WHISPER_VAD_MODEL_NAME}: ${WHISPER_VAD_MODEL_URL}
  sha256 ${WHISPER_VAD_MODEL_SHA256}
  Silero VAD in ggml format, released under the MIT licence
  (Copyright (c) 2020-present Silero Team;
  https://github.com/snakers4/silero-vad/blob/master/LICENSE).

The MIT licence: Permission is hereby granted, free of charge, to any person
obtaining a copy of this software and associated documentation files (the
"Software"), to deal in the Software without restriction, including without
limitation the rights to use, copy, modify, merge, publish, distribute,
sublicense, and/or sell copies of the Software, and to permit persons to whom
the Software is furnished to do so, subject to the following conditions: The
above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software. THE SOFTWARE IS PROVIDED "AS
IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT
LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE
AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE
LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF
CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE
SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
NOTICE
dpkg-query -W -f '${Package} ${Version} source=${source:Package} ${source:Version}\n' | sort >"$out/packages.txt"

cat >"$out/README" <<'README'
ClipMux (clipmux/) is licensed under the GNU Affero General Public License
v3.0. FFmpeg here is built from source under the GPL v2 or later (see
ffmpeg/: its licence, the GPLv2 and LGPLv2.1 texts, the exact sources and
configure flags) and runs as a separate process, never linked into
ClipMux. Shaka Packager is BSD-licensed. whisper.cpp is MIT-licensed (see
whisper/); its speech models are MIT too (see models/). Every other
library comes from a Debian package listed in packages.txt, under that
package's licence (its copyright file is in /usr/share/doc/PACKAGE/ in
the image, and in packages/ in the agent tarball).

SOURCE.md says where the corresponding source is, and offers it in writing.
README

cat >"$out/SOURCE.md" <<SOURCE
# Corresponding source

This distribution of ClipMux contains programs licensed under the GNU GPL
(FFmpeg, built with \`--enable-gpl\`, linked against libx264 and libx265) and
the GNU AGPL (ClipMux itself). Their complete corresponding source is:

| Component | Source | Digest |
| --- | --- | --- |
| ClipMux | https://github.com/visheshgubrani/clipmux at the tag \`clipmux version\` prints (a \`dev\` build: the commit it names) | the git tag |
| FFmpeg ${FFMPEG_VERSION} | ${FFMPEG_URL} | sha256 \`${FFMPEG_SHA256}\` |
| nv-codec-headers ${NV_CODEC_HEADERS_VERSION} | ${NV_CODEC_HEADERS_URL} | sha256 \`${NV_CODEC_HEADERS_SHA256}\` |
| whisper.cpp ${WHISPER_CPP_VERSION} | ${WHISPER_CPP_URL} | sha256 \`${WHISPER_CPP_SHA256}\` |
| Shaka Packager ${SHAKA_VERSION} | ${SHAKA_RELEASE_URL} (a release binary; source at https://github.com/shaka-project/shaka-packager/tree/${SHAKA_VERSION}) | as released |
| Debian and Ubuntu libraries | the Ubuntu ${TARGET_UBUNTU_VERSION} (${TARGET_UBUNTU_CODENAME}) archive: \`apt-get source PACKAGE=VERSION\` for each source package in packages.txt | the archive's signed indexes |

The scripts that build these, with their exact flags and patches, are in the
same ClipMux tag: \`Dockerfile\`, \`toolchain/versions.env\` (every pin),
\`toolchain/apt-packages.env\`, \`toolchain/build_ffmpeg.sh\`,
\`toolchain/build_whisper.sh\`, \`toolchain/whisper-vad-token-times.patch\`,
\`toolchain/install_packager.sh\` and \`toolchain/pack_agent.sh\`. The
configure line FFmpeg was built with is in ffmpeg/toolchain.json.

## Written offer

For at least three years from the date you received this distribution, and
for as long as we offer spare parts or support for it, the ClipMux
maintainers will give anyone who asks a complete machine-readable copy of
the corresponding source code of the GPL-licensed components above, on a
medium customarily used for software interchange, for a charge no more than
the cost of physically performing the distribution. Ask by opening an issue
at https://github.com/visheshgubrani/clipmux/issues naming the ClipMux
version you received.
SOURCE
