#!/bin/sh
# Installs ClipMux on a Linux host with Docker: the app, Postgres and Caddy
# from the release image, in /opt/clipmux (docs/plans/P6-launch.md,
# "One-line install").
#
#   curl -fsSL https://clipmux.com/install.sh | sudo sh
#
# clipmux.com/install.sh redirects to this file on a GitHub Release, where the
# release workflow has written its tag in.
#
# Run again, it upgrades in place: it keeps .env (and so the data, keys and
# sessions), moves CLIPMUX_VERSION to the new release and pulls.
#
# Options (after `sh -s --`):
#   --version vX.Y.Z   install that release (default: the one this script came from, else the latest)
#   --dir DIR          install there instead of /opt/clipmux
#   --no-gpu           skip GPU auto-detection and run on the CPU
#   --restore          on a new host: bring ClipMux back from its latest backup. Put the saved
#                      .env in DIR first; it names the backup target and holds the secrets key
#   --restore-only     restore into a fresh Postgres for a recovery drill, leaving ClipMux stopped
#   --uninstall        stop and remove the stack; the volumes and .env stay
#   --purge            with --uninstall: delete the volumes and DIR too (every video, key and setting)
#
# Environment, for testing and mirrors:
#   CLIPMUX_RELEASE_URL   where the release's files are (default: its GitHub Release)
#   CLIPMUX_IMAGE         the image without its tag (default: ghcr.io/visheshgubrani/clipmux)
#   CLIPMUX_GPU           set to off to skip GPU auto-detection (same as --no-gpu)
#   CLIPMUX_HTTP_PORT, CLIPMUX_HTTPS_PORT, CLIPMUX_ADMIN_PORT   host ports, saved on first install
#   CLIPMUX_TRACE_URL     where the public address is asked (default: clipmux.com's /cdn-cgi/trace)
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
	restore=
	restore_only=
	no_gpu=
	gpu_mode=
	gpu_generated=
	while [ $# -gt 0 ]; do
		case $1 in
		--version) version=${2:?--version needs a tag}; shift ;;
		--version=*) version=${1#*=} ;;
		--dir) dir=${2:?--dir needs a directory}; shift ;;
		--dir=*) dir=${1#*=} ;;
		--no-gpu) no_gpu=1 ;;
		--restore) restore=1 ;;
		--restore-only) restore=1; restore_only=1 ;;
		--uninstall) uninstall=1 ;;
		--purge) purge=1 ;;
		-h | --help) usage; exit 0 ;;
		*) die "unknown option $1 (see --help)" ;;
		esac
		shift
	done
	[ -z "$purge" ] || [ -n "$uninstall" ] || die "--purge goes with --uninstall"
	[ -z "$restore" ] || [ -z "$uninstall" ] || die "--restore can't go with --uninstall"

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
	[ -z "$restore" ] || check_restore_env
	check_disk
	# A restore is a new host's first start, whatever its .env says.
	[ -n "$restore_only" ] || { [ -n "$upgrade" ] && [ -z "$restore" ]; } || choose_ports

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
		if [ -n "$restore" ] && [ -z "$restore_only" ]; then
			for p in CLIPMUX_HTTP_PORT CLIPMUX_HTTPS_PORT CLIPMUX_ADMIN_PORT; do
				eval "v=\${$p:-}"
				[ -z "$v" ] || setenv "$dir/.env" "$p" "$v"
			done
		fi
	else
		write_env "$dir/.env" "$version"
	fi
	cp "$tmp/compose.yml" "$tmp/Caddyfile" "$dir/"
	chmod 0644 "$dir/compose.yml" "$dir/Caddyfile"
	configure_gpu

	if [ -n "$restore" ]; then
		restore_stack
		return
	fi

	say "Starting ClipMux (pulling the image takes a few minutes the first time)"
	compose pull --quiet
	start_stack

	if [ -n "$upgrade" ]; then
		say ""
		say "ClipMux upgraded from ${from:-an earlier release} to $version (${gpu_mode:-CPU}). The data, keys and sessions are kept."
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
	suffix=
	[ -z "$port" ] || [ "$port" = 80 ] || suffix=":$port"
	https=$(sed -n 's/^CLIPMUX_HTTPS_PORT=//p' "$dir/.env")
	admin=$(sed -n 's/^CLIPMUX_ADMIN_PORT=//p' "$dir/.env")
	open=$(open_lines "$suffix" "${port:-80} and ${https:-443}")
	key=$(sed -n 's/^CLIPMUX_SECRETS_KEY=//p' "$dir/.env")
	alt_note=
	if [ "${port:-80}" != 80 ] || [ "${https:-443}" != 443 ]; then
		alt_note="
  Note: ports 80/443 were in use, so ClipMux is on HTTP ${port:-80} and HTTPS ${https:-443}.
  Automatic TLS requires ports 80 and 443. If another reverse proxy already
  owns them, point it at http://127.0.0.1:${admin:-8080} instead:
  https://clipmux.com/docs/self-host/install-options/reverse-proxy
"
	fi
	cat <<DONE

  ClipMux $version is running (${gpu_mode:-CPU}).

$open
  Setup token:  ${token:-(not found; run: cd $dir && sudo docker compose logs app | grep setup_token)}

  Create the owner with the token, then set the public URL and the media
  URL; Caddy gets a certificate for each hostname when it is first visited.
${alt_note}
  Secrets key:  $key
  Store this off the host, with your database backups. Without it, the
  stored secrets and every video are lost; see
  https://clipmux.com/docs/self-host/operate/backups
  It is also in $dir/.env.

  Upgrade:      run this installer again
  Uninstall:    curl -fsSL https://clipmux.com/install.sh | sudo sh -s -- --uninstall

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

compose() {
	if [ -f "$dir/compose.gpu.yml" ]; then
		docker compose --project-directory "$dir" -f "$dir/compose.yml" -f "$dir/compose.gpu.yml" "$@"
	else
		docker compose --project-directory "$dir" -f "$dir/compose.yml" "$@"
	fi
}

is_generated_gpu() {
	[ -f "$dir/compose.gpu.yml" ] &&
		sed -n '1p' "$dir/compose.gpu.yml" 2>/dev/null | grep -q '^# Generated by install\.sh'
}

set_compose_file() {
	cur=$(envval COMPOSE_FILE)
	if [ -z "$cur" ] || [ "$cur" = "compose.yml:compose.gpu.yml" ]; then
		setenv "$dir/.env" COMPOSE_FILE "compose.yml:compose.gpu.yml"
	fi
}

unset_compose_file() {
	[ -f "$dir/.env" ] || return 0
	if grep -q '^COMPOSE_FILE=compose\.yml:compose\.gpu\.yml$' "$dir/.env"; then
		grep -v '^COMPOSE_FILE=compose\.yml:compose\.gpu\.yml$' "$dir/.env" >"$dir/.env.new" || true
		chmod 0600 "$dir/.env.new"
		mv "$dir/.env.new" "$dir/.env"
	fi
}

clear_generated_gpu() {
	if is_generated_gpu; then
		rm -f "$dir/compose.gpu.yml"
	fi
	unset_compose_file
}

configure_gpu() {
	if [ -n "$no_gpu" ] || [ "${CLIPMUX_GPU:-}" = "off" ]; then
		clear_generated_gpu
		return
	fi
	if [ -f "$dir/compose.gpu.yml" ] && ! is_generated_gpu; then
		gpu_mode="custom compose.gpu.yml"
		return
	fi
	if [ -e /dev/nvidiactl ] || [ -e /dev/nvidia0 ]; then
		if docker info --format '{{range $k, $v := .Runtimes}}{{$k}} {{end}}' 2>/dev/null | grep -qw nvidia ||
			docker info --format '{{json .}}' 2>/dev/null | grep -q 'nvidia\.com/gpu'; then
			cat >"$dir/compose.gpu.yml" <<'GPU'
# Generated by install.sh (nvidia)
services:
  app:
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
              capabilities: [gpu]
GPU
			chmod 0644 "$dir/compose.gpu.yml"
			set_compose_file
			gpu_mode=nvidia
			gpu_generated=1
			say "GPU detected: NVIDIA (passing GPU to container)"
			return
		fi
		say "note: NVIDIA GPU detected, but the NVIDIA Container Toolkit isn't configured in Docker; falling back to CPU unless /dev/dri is usable (install nvidia-container-toolkit and re-run install.sh to enable NVIDIA)"
	fi
	dri_node=
	for n in /dev/dri/renderD*; do
		if [ -c "$n" ]; then
			dri_node=$n
			break
		fi
	done
	if [ -n "$dri_node" ]; then
		gid=$(stat -c '%g' "$dri_node" 2>/dev/null || getent group render 2>/dev/null | awk -F: '{print $3}' || true)
		{
			say "# Generated by install.sh (dri)"
			say "services:"
			say "  app:"
			say "    devices:"
			say "      - /dev/dri:/dev/dri"
			case $gid in
			[1-9]*)
				case $gid in
				*[!0-9]*) ;;
				*)
					say "    group_add:"
					say "      - \"$gid\""
					;;
				esac
				;;
			esac
		} >"$dir/compose.gpu.yml"
		chmod 0644 "$dir/compose.gpu.yml"
		set_compose_file
		gpu_mode="dri ($dri_node)"
		gpu_generated=1
		say "GPU detected: $dri_node (passing /dev/dri to container)"
		return
	fi
	clear_generated_gpu
}

start_stack() {
	if compose up -d --wait --wait-timeout 600; then
		return 0
	fi
	if [ -n "$gpu_generated" ]; then
		say "note: starting with GPU passthrough failed; falling back to CPU" >&2
		clear_generated_gpu
		gpu_generated=
		gpu_mode=
		if compose up -d --wait --wait-timeout 600; then
			return 0
		fi
	fi
	compose ps >&2
	compose logs --tail 50 app >&2
	die "ClipMux didn't become healthy; the logs are above"
}

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

# check_restore_env checks that the saved .env holds what a restore needs,
# before anything is downloaded or started.
check_restore_env() {
	[ -f "$dir/.env" ] || die "--restore needs the .env you saved from the old server in $dir: copy it there first"
	for k in POSTGRES_PASSWORD CLIPMUX_SECRETS_KEY CLIPMUX_BACKUP_URL; do
		[ -n "$(envval "$k")" ] || die "$dir/.env has no $k, which a restore needs (see https://clipmux.com/docs/self-host/operate/backups)"
	done
	case $(envval CLIPMUX_BACKUP_URL) in
	s3://*) [ -n "$(envval CLIPMUX_BACKUP_ACCESS_KEY_ID)" ] && [ -n "$(envval CLIPMUX_BACKUP_SECRET_ACCESS_KEY)" ] ||
		die "$dir/.env names an s3 backup target but not its CLIPMUX_BACKUP_ACCESS_KEY_ID and CLIPMUX_BACKUP_SECRET_ACCESS_KEY" ;;
	esac
}

# envval prints the value of KEY in $dir/.env, the last one if it is set twice.
envval() {
	sed -n "s/^$1=//p" "$dir/.env" | tail -n 1
}

# restore_stack brings ClipMux back from the latest backup on the target the
# saved .env names: Postgres first, then the restore in a one-off app container
# (serve isn't running, so nothing else uses the database), then the whole
# stack, then the report of what storage holds that the backup doesn't know.
restore_stack() {
	[ -z "$(compose ps --status running -q app 2>/dev/null)" ] ||
		die "ClipMux is running on this host; --restore is for a new one. To roll this one back, see https://clipmux.com/docs/self-host/operate/backups"
	say "Pulling the recovery images"
	compose pull --quiet
	say "Starting Postgres"
	compose up -d --wait --wait-timeout 300 postgres || {
		compose logs --tail 50 postgres >&2
		die "Postgres didn't become healthy; the logs are above"
	}
	if [ -n "$restore_only" ]; then
		tables=$(compose exec -T postgres psql -U clipmux -d clipmux -Atqc "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'") ||
			die "couldn't check that the recovery database is empty"
		[ "$tables" = 0 ] || die "--restore-only requires an empty recovery database"
	fi
	say "Restoring the latest backup from $(envval CLIPMUX_BACKUP_URL | sed 's/?.*//')"
	docker compose --project-directory "$dir" -f "$dir/compose.yml" run --rm -T --no-deps app admin restore --latest --yes || {
		die "the restore failed; only Postgres is running. Fix the error above (the secrets key and backup settings are in $dir/.env) and run this again"
	}
	if [ -n "$restore_only" ]; then
		say "ClipMux $version is restored from the backup. Only Postgres is running."
		return
	fi
	say "Starting ClipMux"
	start_stack
	say ""
	compose exec -T app clipmux admin restore report || say "(the report couldn't be read; run it again below)"
	cat <<DONE

  ClipMux $version is restored from the backup and running.

  The restore fence is up: until you lift it, nothing is deleted from storage,
  so the videos uploaded after the backup stay safe. Read the report above,
  then lift the fence:

      cd $dir && docker compose exec app clipmux admin restore finish --yes

  Next:
    - Point DNS for the public and media hostnames at this server. Caddy gets
      a certificate for each when it is first visited.
    - Play a video, and run: docker compose exec app clipmux admin setup-test --wait
    - Backups carry on to the same target, from this host.

DONE
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

port_in_use() {
	case $2 in
	tcp) ss -Htln "sport = :$1" 2>/dev/null | grep -q . ;;
	udp) ss -Huln "sport = :$1" 2>/dev/null | grep -q . ;;
	esac
}

port_in_list() {
	case " $2 " in
	*" $1 "*) return 0 ;;
	esac
	return 1
}

port_busy() {
	port_in_list "$1" "$used_ports" && return 0
	case $2 in
	tcp+udp) port_in_use "$1" tcp || port_in_use "$1" udp ;;
	*) port_in_use "$1" "$2" ;;
	esac
}

explicit_port() {
	eval "v=\${$1:-}"
	if [ -z "$v" ] && [ -n "$upgrade" ] && [ -f "$dir/.env" ]; then
		v=$(envval "$1")
	fi
	case $v in
	"") ;;
	*[!0-9]*) die "$1=$v is not a valid port" ;;
	*)
		[ "$v" -ge 1 ] && [ "$v" -le 65535 ] || die "$1=$v is not a valid port"
		;;
	esac
	printf '%s\n' "$v"
}

pick_port() {
	var=$1 def=$2 start=$3 end=$4 proto=$5 label=$6
	exp=$(explicit_port "$var")
	if [ -n "$exp" ]; then
		if port_busy "$exp" "$proto"; then
			die "port $exp ($label) is in use; set another $var"
		fi
		used_ports="$used_ports $exp"
		eval "$var=\$exp"
		return 0
	fi
	if ! port_in_list "$def" "$reserved_ports" && ! port_busy "$def" "$proto"; then
		used_ports="$used_ports $def"
		return 0
	fi
	cand=$start
	while [ "$cand" -le "$end" ]; do
		if ! port_in_list "$cand" "$reserved_ports" && ! port_busy "$cand" "$proto"; then
			used_ports="$used_ports $cand"
			eval "$var=\$cand"
			say "note: port $def is in use; using port $cand for $label"
			return 0
		fi
		cand=$((cand + 1))
	done
	die "port $def is in use and no free port found in $start-$end (set $var manually)"
}

choose_ports() {
	command -v ss >/dev/null 2>&1 || {
		say "note: can't check ports (no ss); continuing"
		return
	}
	used_ports=""
	reserved_ports=""
	for p in CLIPMUX_HTTP_PORT CLIPMUX_HTTPS_PORT CLIPMUX_ADMIN_PORT; do
		ep=$(explicit_port "$p")
		[ -z "$ep" ] && continue
		if port_in_list "$ep" "$reserved_ports"; then
			die "ports for ClipMux can't share port $ep"
		fi
		reserved_ports="$reserved_ports $ep"
	done
	pick_port CLIPMUX_HTTP_PORT 80 8088 8188 tcp "HTTP"
	pick_port CLIPMUX_HTTPS_PORT 443 8443 8543 tcp+udp "HTTPS"
	pick_port CLIPMUX_ADMIN_PORT 8080 18080 18180 tcp "loopback admin"
}

# open_lines SUFFIX PORTS prints where to open the dashboard, each address
# with SUFFIX (the port), and that PORTS must be reachable. Behind NAT or on a cloud VM the host's own address is private,
# so the address the internet sees comes first; a home server whose router
# doesn't forward the ports is reached at its own address instead.
open_lines() {
	own=$(own_address)
	pub=$(public_address)
	if [ -n "$pub" ] && [ "$pub" != "$own" ]; then
		say "  Open:         http://$pub$1"
		[ -z "$own" ] || say "                http://$own$1 from the server's own network"
	elif [ -n "$own" ]; then
		say "  Open:         http://$own$1"
		if [ -z "$pub" ] && private "$own"; then
			say "                That is a private address: from outside its network, use the"
			say "                server's public address, or a DNS name that points at it."
		fi
	else
		say "  Open:         http://THIS-SERVER$1 (its public address, or a DNS name that points at it)"
	fi
	say "                Ports $2 must be open to it: check the cloud firewall"
	say "                or security group, if there is one."
	say ""
}

# own_address is the address the host reaches the internet from: the
# source of its default route, not the first of hostname -I, which can be
# a Docker bridge.
own_address() {
	a=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "src") {print $(i + 1); exit}}')
	[ -n "$a" ] || a=$(hostname -I 2>/dev/null | awk '{print $1}')
	printf '%s\n' "$a"
}

# public_address is the IPv4 address the internet sees this host at, as
# reported by clipmux.com's CDN (Cloudflare's trace endpoint, on the host
# this script came from); empty if it can't be reached in 5 s.
public_address() {
	curl -4 -fsS --max-time 5 "${CLIPMUX_TRACE_URL:-https://clipmux.com/cdn-cgi/trace}" 2>/dev/null |
		sed -n 's/^ip=\([0-9.]*\)$/\1/p' || true
}

# private says whether an IPv4 address is private, shared (CGNAT),
# link-local or loopback.
private() {
	case $1 in
	10.* | 127.* | 169.254.* | 192.168.*) return 0 ;;
	172.1[6-9].* | 172.2[0-9].* | 172.3[01].*) return 0 ;;
	100.6[4-9].* | 100.[7-9][0-9].* | 100.1[01][0-9].* | 100.12[0-7].*) return 0 ;;
	esac
	return 1
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
