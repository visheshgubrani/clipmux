#!/bin/sh
# Prints the release notes for TAG: its section of CHANGELOG.md (the lines
# under `## TAG`, up to the next `## ` heading), then how to install it.
# Fails if CHANGELOG.md has no section for TAG, so a release can't go out
# without one (P6-T07).
#
#   release_notes.sh TAG [CHANGELOG]
set -eu
tag=${1:?usage: release_notes.sh TAG [CHANGELOG]}
changelog=${2:-CHANGELOG.md}

notes=$(awk -v tag="$tag" '
	/^## / {
		if (on) exit
		h = substr($0, 4)
		if (h == tag || index(h, tag " ") == 1) { on = 1; next }
	}
	on { print }
' "$changelog")
[ -n "$(printf '%s' "$notes" | tr -d '[:space:]')" ] || {
	echo "release_notes.sh: $changelog has no section for $tag (a heading '## $tag - YYYY-MM-DD')" >&2
	exit 1
}

printf '%s\n' "$notes" | sed -e '/./,$!d'
cat <<EOF

## Install

\`\`\`bash
curl -fsSL https://github.com/visheshgubrani/clipmux/releases/download/$tag/install.sh | sudo sh
\`\`\`

Run it again on an installed server to upgrade in place. The image is
\`ghcr.io/visheshgubrani/clipmux:$tag\` for amd64 and arm64. Every file here
is listed in \`SHA256SUMS\` and has a build provenance attestation:
\`gh attestation verify FILE --repo visheshgubrani/clipmux\`.
EOF
