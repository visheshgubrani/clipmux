#!/bin/sh
# Installs ClipMux on a Linux host with Docker: the app, Postgres and Caddy
# from the release image, in /opt/clipmux (docs/plans/P6-launch.md,
# "One-line install").
#
#   curl -fsSL https://github.com/visheshgubrani/clipmux/releases/latest/download/install.sh | sudo sh
#
# Run again, it upgrades in place: it keeps .env (and so the data, keys and
# sessions), moves CLIPMUX_VERSION to the new release and pulls.
#
# Options (after `sh -s --`):
#   --version vX.Y.Z   install that release (default: the one this script came from, else the latest)
#   --dir DIR          install there instead of /opt/clipmux
#   --uninstall        stop and remove the stack; the volumes and .env stay
#   --purge            with --uninstall: delete the volumes and DIR too (every video, key and setting)
#
# Environment, for testing and mirrors:
#   CLIPMUX_RELEASE_URL   where the release's files are (default: its GitHub Release)
#   CLIPMUX_IMAGE         the image without its tag (default: ghcr.io/visheshgubrani/clipmux)
#   CLIPMUX_HTTP_PORT, CLIPMUX_HTTPS_PORT, CLIPMUX_ADMIN_PORT   host ports, saved on first install
#
# It downloads compose.yml and Caddyfile from the release and checks each
# against the release's SHA256SUMS; it never runs anything it downloads.
# Everything is inside main, so a truncated download runs nothing.

main() {
	repo=visheshgubrani/clipmux
	# The release workflow writes its tag here.
	version="@CLIPMUX_VERSION@"
	dir=/opt/clipmux
	uninstall=
	purge=
	while [ $# -gt 0 ]; do
		case $1 in
		--version) version=${2:?--version needs a tag}; shift ;;
		--version=*) version=${1#*=} ;;
		--dir) dir=${2:?--dir needs a directory}; shift ;;
		--dir=*) dir=${1#*=} ;;
		--uninstall) uninstall=1 ;;
		--purge) purge=1 ;;
		-h | --help) usage; exit 0 ;;
		*) die "unknown option $1 (see --help)" ;;
		esac
		shift
	done
	[ -z "$purge" ] || [ -n "$uninstall" ] || die "--purge goes with --uninstall"

	need docker
	docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is needed (the 'docker compose' plugin)"
	docker info >/dev/null 2>&1 || die "can't reach Docker: run as root (curl … | sudo sh) or as a user in the docker group"

	if [ -n "$uninstall" ]; then
		remove
		return
	fi

	case $(uname -m) in
	x86_64 | amd64 | aarch64 | arm64) ;;
	*) die "ClipMux images are built for amd64 and arm64, not $(uname -m)" ;;
	esac
	need curl
	need awk

	case $version in
	@*@ | "" | latest) version=$(latest) ;;
	esac
	case $version in
	v[0-9]*) ;;
	*) die "$version isn't a release tag (vX.Y.Z)" ;;
	esac
	base=${CLIPMUX_RELEASE_URL:-https://github.com/$repo/releases/download/$version}

	upgrade=
	[ -f "$dir/.env" ] && upgrade=1
	mkdir -p "$dir" 2>/dev/null || die "can't create $dir: run as root (curl … | sudo sh), or pass --dir"
	[ -w "$dir" ] || die "can't write $dir: run as root (curl … | sudo sh), or pass --dir"
	check_disk
	[ -n "$upgrade" ] || check_ports

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	say "Downloading ClipMux $version"
	fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS"
	for f in compose.yml Caddyfile; do
		fetch "$base/$f" "$tmp/$f"
		verify "$tmp" "$f"
	done

	if [ -n "$upgrade" ]; then
		from=$(sed -n 's/^CLIPMUX_VERSION=//p' "$dir/.env")
		setenv "$dir/.env" CLIPMUX_VERSION "$version"
	else
		write_env "$dir/.env" "$version"
	fi
	cp "$tmp/compose.yml" "$tmp/Caddyfile" "$dir/"
	chmod 0644 "$dir/compose.yml" "$dir/Caddyfile"

	say "Starting ClipMux (pulling the image takes a few minutes the first time)"
	compose pull --quiet
	compose up -d --wait --wait-timeout 600 || {
		compose ps >&2
		compose logs --tail 50 app >&2
		die "ClipMux didn't become healthy; the logs are above"
	}

	if [ -n "$upgrade" ]; then
		say ""
		say "ClipMux upgraded from ${from:-an earlier release} to $version. The data, keys and sessions are kept."
		return
	fi
	token=
	i=0
	while [ $i -lt 30 ] && [ -z "$token" ]; do
		token=$(compose logs --no-log-prefix app 2>/dev/null | sed -n 's/.*"setup_token":"\([A-Z2-7]*\)".*/\1/p' | tail -n 1)
		[ -n "$token" ] || sleep 2
		i=$((i + 1))
	done
	port=$(sed -n 's/^CLIPMUX_HTTP_PORT=//p' "$dir/.env")
	url="http://$(host_address)"
	[ -z "$port" ] || [ "$port" = 80 ] || url="$url:$port"
	key=$(sed -n 's/^CLIPMUX_SECRETS_KEY=//p' "$dir/.env")
	cat <<DONE

  ClipMux $version is running.

  Open:         $url
  Setup token:  ${token:-(not found; run: cd $dir && docker compose logs app | grep setup_token)}

  Create the owner with the token, then set the public URL and the media
  URL; Caddy gets a certificate for each hostname when it is first visited.

  Secrets key:  $key
  Store this off the host, with your database backups. Without it, the
  stored secrets and every video are lost; see
  https://clipmux.com/docs/self-host/operate/backups
  It is also in $dir/.env.

  Upgrade:      run this installer again
  Uninstall:    curl -fsSL https://github.com/$repo/releases/latest/download/install.sh | sudo sh -s -- --uninstall

DONE
}

usage() {
	sed -n '2,/^# Everything is inside main/p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//' ||
		echo "see https://github.com/visheshgubrani/clipmux/blob/main/deploy/install.sh"
}

say() { printf '%s\n' "$*"; }
die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is needed"; }

compose() { docker compose --project-directory "$dir" -f "$dir/compose.yml" "$@"; }

# latest is the newest release's tag, from where /releases/latest redirects.
latest() {
	u=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest") ||
		die "can't find the latest release; pass --version"
	t=${u##*/}
	case $t in
	v[0-9]*) printf '%s\n' "$t" ;;
	*) die "can't find the latest release; pass --version" ;;
	esac
}

fetch() {
	curl -fsSL --retry 3 --proto '=https,http' -o "$2" "$1" || die "can't download $1"
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		openssl dgst -sha256 -r "$1" | awk '{print $1}'
	fi
}

# verify checks DIR/NAME against DIR/SHA256SUMS.
verify() {
	want=$(awk -v f="$2" '$2 == f || $2 == "*" f {print $1}' "$1/SHA256SUMS")
	[ -n "$want" ] || die "SHA256SUMS doesn't list $2"
	got=$(sha256 "$1/$2")
	[ "$got" = "$want" ] || die "$2 doesn't match SHA256SUMS (got $got, want $want)"
}

random_hex() {
	if command -v openssl >/dev/null 2>&1; then
		openssl rand -hex "$1"
	else
		od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'
	fi
}

write_env() {
	umask 077
	{
		say "# ClipMux settings, written by install.sh on $(date -u +%Y-%m-%d). Keep this file."
		say "CLIPMUX_VERSION=$2"
		[ -z "${CLIPMUX_IMAGE:-}" ] || say "CLIPMUX_IMAGE=$CLIPMUX_IMAGE"
		say "POSTGRES_PASSWORD=$(random_hex 24)"
		say "# Seals the secrets kept in Postgres. Back it up off the host, with the database."
		say "CLIPMUX_SECRETS_KEY=v1:$(random_hex 32)"
		for p in CLIPMUX_HTTP_PORT CLIPMUX_HTTPS_PORT CLIPMUX_ADMIN_PORT; do
			eval "v=\${$p:-}"
			[ -z "$v" ] || say "$p=$v"
		done
		say "# Optional: pin the URLs instead of choosing them at first boot."
		say "# CLIPMUX_PUBLIC_URL=https://video.example.com"
		say "# CLIPMUX_MEDIA_URL=https://media.example.net"
	} >"$1"
	umask 022
}

# setenv sets KEY=VALUE in FILE, keeping every other line and the mode.
setenv() {
	if grep -q "^$2=" "$1"; then
		sed "s|^$2=.*|$2=$3|" "$1" >"$1.new"
	else
		{ cat "$1"; say "$2=$3"; } >"$1.new"
	fi
	chmod 0600 "$1.new"
	mv "$1.new" "$1"
}

check_disk() {
	root=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || true)
	for d in "$dir" "${root:-/var/lib/docker}"; do
		[ -d "$d" ] || continue
		free=$(df -Pk "$d" | awk 'NR == 2 {print int($4 / 1048576)}')
		[ "$free" -ge 4 ] || die "only ${free} GiB free under $d; ClipMux needs at least 4 GiB, and room for your videos"
		[ "$free" -ge 20 ] || say "note: ${free} GiB free under $d; encoded videos need room"
	done
}

check_ports() {
	command -v ss >/dev/null 2>&1 || {
		say "note: can't check ports 80 and 443 (no ss); continuing"
		return
	}
	for p in "${CLIPMUX_HTTP_PORT:-80}" "${CLIPMUX_HTTPS_PORT:-443}"; do
		if ss -Htln "sport = :$p" 2>/dev/null | grep -q .; then
			die "port $p is in use; ClipMux's Caddy serves on 80 and 443 (set CLIPMUX_HTTP_PORT and CLIPMUX_HTTPS_PORT to use others)"
		fi
	done
}

host_address() {
	a=$(hostname -I 2>/dev/null | awk '{print $1}')
	[ -n "$a" ] || a=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "src") print $(i + 1)}')
	printf '%s\n' "${a:-THIS-HOST}"
}

remove() {
	[ -f "$dir/compose.yml" ] || die "no ClipMux in $dir"
	if [ -n "$purge" ]; then
		say "Removing ClipMux, its volumes and $dir: every video, key and setting"
		compose down --volumes --remove-orphans
		rm -rf "$dir"
		say "ClipMux is gone."
	else
		compose down --remove-orphans
		say "ClipMux is stopped and removed. Its volumes and $dir/.env are kept:"
		say "run the installer again to bring it back, or --uninstall --purge to delete them."
	fi
}

set -eu
main "$@"
