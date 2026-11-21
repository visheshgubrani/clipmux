#!/usr/bin/env bash
# Compares the layers of two pushed images, for one platform: which layers
# NEW shares with OLD (a host that has OLD downloads none of them) and which
# it adds. A release that changes only ClipMux should add one layer, its
# binary, on top of all of OLD's others (D142).
#
#   check_layers.sh [--strict] [--platform linux/amd64] OLD NEW
#
# OLD and NEW are registry references (tag or digest). Prints a table, and
# appends it to $GITHUB_STEP_SUMMARY when that is set. With --strict it fails
# unless the two have the same number of layers and every one but the top is
# the same blob. Without it, it only reports. The platform defaults to this
# machine's.
#
# Reads manifests only, never layers: `docker buildx imagetools inspect --raw`,
# or plain HTTP for a registry on localhost (install.yml's).
set -euo pipefail

strict=0
platform=""
while [ $# -gt 0 ]; do
  case $1 in
    --strict) strict=1; shift ;;
    --platform) platform=${2:?--platform needs a value}; shift 2 ;;
    -h|--help) sed -n '2,16p' "$0"; exit 0 ;;
    -*) echo "check_layers: unknown option $1" >&2; exit 2 ;;
    *) break ;;
  esac
done
[ $# -eq 2 ] || { echo "usage: check_layers.sh [--strict] [--platform OS/ARCH] OLD NEW" >&2; exit 2; }
old=$1 new=$2
if [ -z "$platform" ]; then
  case $(uname -m) in
    x86_64) platform=linux/amd64 ;;
    aarch64|arm64) platform=linux/arm64 ;;
    *) echo "check_layers: name a --platform for $(uname -m)" >&2; exit 2 ;;
  esac
fi
os=${platform%/*} arch=${platform#*/}

# repo REF: the reference without its tag or digest.
repo() {
  local r=${1%@*} last
  last=${r##*/}
  case $last in *:*) r=${r%:*} ;; esac
  printf '%s' "$r"
}
# ref_of REF: the tag or digest the registry is asked for.
ref_of() {
  case $1 in
    *@*) printf '%s' "${1#*@}" ;;
    *) local last=${1##*/}; case $last in *:*) printf '%s' "${last##*:}" ;; *) printf latest ;; esac ;;
  esac
}
# raw REPO REF: the manifest (or index) REF names, as JSON.
raw() {
  local repo=$1 ref=$2 host=${1%%/*}
  case $host in
    localhost:*|127.0.0.1:*)
      curl -fsS \
        -H 'Accept: application/vnd.oci.image.index.v1+json' \
        -H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json' \
        -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
        -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \
        "http://$host/v2/${repo#*/}/manifests/$ref" ;;
    *)
      case $ref in
        sha256:*) docker buildx imagetools inspect --raw "$repo@$ref" ;;
        *) docker buildx imagetools inspect --raw "$repo:$ref" ;;
      esac ;;
  esac
}
# layers IMAGE: "digest size" per layer, bottom first, for the platform.
layers() {
  local image=$1 r m
  r=$(repo "$image")
  m=$(raw "$r" "$(ref_of "$image")") || return 1
  if jq -e '.manifests' >/dev/null <<<"$m"; then
    local d
    d=$(jq -r --arg os "$os" --arg arch "$arch" \
      '[.manifests[] | select(.platform.os == $os and .platform.architecture == $arch)][0].digest // empty' <<<"$m")
    [ -n "$d" ] || { echo "check_layers: $image has no $platform image" >&2; return 1; }
    m=$(raw "$r" "$d") || return 1
  fi
  jq -r '.layers[] | "\(.digest) \(.size)"' <<<"$m"
}

if ! old_layers=$(layers "$old"); then
  echo "check_layers: couldn't read $old" >&2
  [ "$strict" = 1 ] && exit 1
  exit 0
fi
new_layers=$(layers "$new") || { echo "check_layers: couldn't read $new" >&2; exit 1; }
mapfile -t o <<<"$old_layers"
mapfile -t n <<<"$new_layers"

mb() { awk -v b="$1" 'BEGIN { printf "%.1f", b / 1000000 }'; }
table="| # | layer | size MB | in $old |"$'\n'"|---|---|---|---|"$'\n'
shared=0 added=0 changed_below_top=0
declare -A have=()
for l in "${o[@]}"; do have[${l% *}]=1; done
for i in "${!n[@]}"; do
  d=${n[$i]% *} size=${n[$i]#* }
  if [ -n "${have[$d]:-}" ]; then
    mark=same; shared=$((shared + size))
  else
    mark=new; added=$((added + size))
  fi
  # Same blob at the same height: what a host's layer store reuses.
  if [ "$i" -lt $((${#n[@]} - 1)) ] && [ "${o[$i]:-}" != "${n[$i]}" ]; then
    changed_below_top=$((changed_below_top + 1))
  fi
  table+="| $i | \`${d:7:12}\` | $(mb "$size") | $mark |"$'\n'
done
summary="$new on $platform: $(mb "$shared") MB in layers $old already has, $(mb "$added") MB new"

printf '%s\n%s' "$summary" "$table"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  printf '### Layers\n\n%s\n\n%s\n' "$summary" "$table" >>"$GITHUB_STEP_SUMMARY"
fi

if [ "$strict" = 1 ]; then
  if [ "${#o[@]}" != "${#n[@]}" ]; then
    echo "check_layers: $old has ${#o[@]} layers and $new ${#n[@]}" >&2
    exit 1
  fi
  if [ "$changed_below_top" -gt 0 ]; then
    echo "check_layers: $changed_below_top layers below the top differ from $old; only the binary's should" >&2
    exit 1
  fi
  echo "check_layers: only the top layer differs"
fi
