#!/usr/bin/env bash
# The disaster-recovery drill on a fresh VM (P6 exit check 10, T29.10): lose
# the server, bring ClipMux back from its backup with the saved .env and the
# one-line installer's --restore, and time every phase. Run as root on a fresh
# Ubuntu 24.04 or Debian 12 VM with a public IPv4 address and ports 80 and 443
# open. Two modes:
#
#   check_restore.sh [INSTALL_URL]
#       The whole drill, self-contained. Needs a throwaway S3-compatible bucket
#       and a key that can read, write and list it, plus a separate throwaway
#       S3 media bucket. Set:
#           CLIPMUX_DRILL_BACKUP_URL=s3://bucket/prefix?endpoint=...&region=...
#           CLIPMUX_DRILL_BACKUP_ACCESS_KEY_ID=...
#           CLIPMUX_DRILL_BACKUP_SECRET_ACCESS_KEY=...
#           CLIPMUX_DRILL_MEDIA_CONFIG_FILE=/path/to/storage.json
#       The JSON file is a POST /v1/storage body for the media bucket. It
#       must support the storage setup probe, including deletes and CORS.
#       The drill backs up after the first upload and before the second.
#       It then purges the install and starts it from the saved .env.
#       The first video must play; the second must be in the restore report.
#
#   check_restore.sh --env FILE --yes [INSTALL_URL]
#       Only the restore, from an existing install's saved .env (its recovery
#       kit): the drill of a production backup on a throwaway VM (T29.12).
#       Substitute the off-host read-only backup key in the copy of .env.
#       Starts only Postgres. It verifies the exact restored backup and
#       reads the storage report. The local copy is purged on exit.
#
# Phase times are printed as they happen and in a table at the end. The VM's
# ClipMux images from the first install are removed before restoring, so the
# times include pulling them. Never run it on a machine with a ClipMux you want:
# the drill purges /opt/clipmux and its volumes.
set -euo pipefail
url=https://clipmux.com/install.sh
envfile=
yes=
while [ $# -gt 0 ]; do
	case $1 in
	--env)
		[ $# -ge 2 ] || { echo "check_restore: --env needs a file" >&2; exit 2; }
		envfile=$2
		shift
		;;
	--yes) yes=1 ;;
	-*)
		echo "check_restore: unknown option $1" >&2
		exit 2
		;;
	*) url=$1 ;;
	esac
	shift
done
opt=/opt/clipmux

step() { printf '\n== %s\n' "$*"; }
fail() {
	printf 'check_restore: FAIL: %s\n' "$*" >&2
	exit 1
}
dc() { docker compose --project-directory "$opt" -f "$opt/compose.yml" "$@"; }
oneliner() { sh "$work/install.sh" "$@"; }
purge() {
	if [ -f "$opt/compose.yml" ]; then
		oneliner --uninstall --purge
	elif [ -d "$opt" ]; then
		rm -f "$opt/.env" && rmdir "$opt"
	fi
}
# timed NAME CMD...: runs CMD and records how many seconds it took.
timed() {
	local name=$1 t0=$SECONDS rc=0
	shift
	"$@" || rc=$?
	printf '%s\t%s\n' "$name" "$((SECONDS - t0))" >>"$timings"
	printf '[%s: %ss]\n' "$name" "$((SECONDS - t0))"
	return "$rc"
}
# stamped prefixes every line with the seconds since the phase began, so the
# installer's own steps are timed.
stamped() {
	local t0=$SECONDS l
	while IFS= read -r l; do printf '[+%3ss] %s\n' "$((SECONDS - t0))" "$l"; done
}

[ "$(id -u)" = 0 ] || fail "run as root"
[ ! -e "$opt" ] || fail "$opt exists: this is for a clean machine"
if [ -n "$envfile" ]; then
	[ -f "$envfile" ] || fail "$envfile isn't a file"
	[ -n "$yes" ] || fail "--env creates and purges a local copy; pass --yes (see the header)"
else
	[ -z "$yes" ] || fail "--yes goes with --env"
	for v in CLIPMUX_DRILL_BACKUP_URL CLIPMUX_DRILL_BACKUP_ACCESS_KEY_ID CLIPMUX_DRILL_BACKUP_SECRET_ACCESS_KEY CLIPMUX_DRILL_MEDIA_CONFIG_FILE; do
		[ -n "${!v:-}" ] || fail "set $v (see the header)"
	done
	[ -f "$CLIPMUX_DRILL_MEDIA_CONFIG_FILE" ] || fail "the media config file is missing"
fi
umask 077
work=$(mktemp -d)
trap 'rc=$?; purge || rc=1; rm -rf "$work"; exit "$rc"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
export COMPOSE_PROJECT_NAME="clipmux-drill-$(basename "$work" | tr '[:upper:]' '[:lower:]' | tr -cd '[:alnum:]')"
timings=$work/timings.txt
: >"$timings"
t_start=$SECONDS
# shellcheck source=/dev/null
. /etc/os-release
step "On $PRETTY_NAME ($(uname -m)), $(nproc) CPUs"

if ! docker compose version >/dev/null 2>&1; then
	step "Docker from Docker's apt repository"
	apt-get update -qq
	apt-get install -y -qq ca-certificates curl jq >/dev/null
	install -m 0755 -d /etc/apt/keyrings
	curl -fsSL "https://download.docker.com/linux/$ID/gpg" -o /etc/apt/keyrings/docker.asc
	echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/$ID $VERSION_CODENAME stable" >/etc/apt/sources.list.d/docker.list
	apt-get update -qq
	apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-compose-plugin >/dev/null
fi
docker compose version
if [ -z "$envfile" ]; then
	for cmd in jq ffmpeg; do
		command -v "$cmd" >/dev/null || fail "install $cmd for the self-contained drill"
	done
fi
curl -fsSL "$url" -o "$work/install.sh"

jar=$work/cookies
kit=$work/recovery-kit.env
public=
apikey=

if [ -z "$envfile" ]; then
	step "Install with the one-liner, with the drill's backup target"
	timed install oneliner >"$work/install.log" 2>&1 || fail "the initial install failed"
	token=$(sed -n 's/^ *Setup token: *\([A-Z2-7]\{8,\}\)$/\1/p' "$work/install.log")
	[ -n "$token" ] || fail "no setup token"
	ip=$(sed -n 's|^ *Open: *http://\([0-9.]*\).*|\1|p' "$work/install.log")
	[ -n "$ip" ] || fail "no address after Open:"
	{
		printf 'CLIPMUX_BACKUP_URL=%s\n' "$CLIPMUX_DRILL_BACKUP_URL"
		printf 'CLIPMUX_BACKUP_ACCESS_KEY_ID=%s\n' "$CLIPMUX_DRILL_BACKUP_ACCESS_KEY_ID"
		printf 'CLIPMUX_BACKUP_SECRET_ACCESS_KEY=%s\n' "$CLIPMUX_DRILL_BACKUP_SECRET_ACCESS_KEY"
		printf 'CLIPMUX_BACKUP_INTERVAL=0\n'
	} >>"$opt/.env"
	dc up -d --wait --wait-timeout 300 || fail "ClipMux didn't come up with the backup target"

	dashed=${ip//./-}
	public=https://video.$dashed.sslip.io
	media=https://media.$dashed.sslip.io
	step "The owner registers and sets $public and $media"
	curl -fsS -c "$jar" -H 'Content-Type: application/json' -H "Origin: http://$ip" \
		-d "{\"setup_token\":\"$token\",\"email\":\"owner@example.test\",\"password\":\"a long test password\"}" \
		"http://$ip/v1/setup/owner" >/dev/null || fail "creating the owner failed"
	curl -fsS -b "$jar" -X PUT -H 'Content-Type: application/json' -H "Origin: http://$ip" \
		-d "{\"public_url\":\"$public\",\"media_url\":\"$media\"}" "http://$ip/v1/settings/urls" >/dev/null || fail "saving the URLs failed"
	waitfor() {
		for _ in $(seq 40); do
			curl -fsS --max-time 10 "$1/healthz" >/dev/null 2>&1 && return 0
			sleep 4
		done
		fail "$1/healthz isn't served over verified HTTPS"
	}
	waitfor "$public"
	curl -fsS -c "$jar" -H 'Content-Type: application/json' -H "Origin: $public" \
		-d '{"email":"owner@example.test","password":"a long test password"}' "$public/v1/session" >/dev/null ||
		fail "signing in at $public failed"

	step "Register the external media bucket and upload the first video"
	curl -fsS -b "$jar" -H "Origin: $public" -H 'Content-Type: application/json' \
		--data-binary "@$CLIPMUX_DRILL_MEDIA_CONFIG_FILE" "$public/v1/storage" >"$work/store.json"
	store=$(jq -er '.id' "$work/store.json")
	curl -fsS -b "$jar" -H "Origin: $public" -X POST "$public/v1/storage/$store/default" >/dev/null
	dc exec -T app clipmux admin create-key --app Drill --scopes uploads:write,assets:read >"$work/key.txt" || fail "create-key failed"
	apikey=$(grep -o 'cmx_[A-Za-z0-9_]*' "$work/key.txt" | head -n 1)
	[ -n "$apikey" ] || fail "create-key printed no key"
	printf 'Authorization: Bearer %s\n' "$apikey" >"$work/auth"
	export CLIPMUX_API_KEY=$apikey
	upload() {
		local file="/var/lib/clipmux/data/$1.mp4"
		dc exec -T app ffmpeg -nostdin -loglevel error -f lavfi -i testsrc2=size=320x180:rate=25 \
			-f lavfi -i sine=frequency=440 -t 4 -c:v libx264 -c:a aac "$file" || return
		dc exec -T -e CLIPMUX_API_KEY -e CLIPMUX_PUSH_STATE_DIR=/tmp/drill-push app \
			clipmux push --server "$public" --policy public --wait --quiet "$file"
	}
	play() {
		timeout 60 ffmpeg -nostdin -v error -xerror -protocol_whitelist http,https,tcp,tls,crypto,data \
			-i "$media/p/$1/master.m3u8" -map 0:v:0 -map 0:a:0 -f framemd5 - |
			awk -F, '/^0,/ {video++} /^1,/ {audio++} END {exit !(video >= 50 && audio > 0)}'
	}
	first_upload=$(upload before) || fail "the first upload failed"
	IFS=$'\t' read -r _ first_pid first_status _ <<<"$first_upload"
	[ "$first_status" = ready ] && [ -n "$first_pid" ] || fail "the first upload never became playable"
	play "$first_pid" || fail "the first video didn't play before the disaster"

	step "Back up, and check the backup"
	timed backup dc exec -T app clipmux admin backup now || fail "backup now failed"
	timed verify_before dc exec -T app clipmux admin backup verify || fail "the backup didn't verify before the disaster"
	dc exec -T app clipmux admin backup check || fail "backup check failed"

	step "Upload a second video after the backup"
	second_upload=$(upload after) || fail "the second upload failed"
	IFS=$'\t' read -r second second_pid second_status _ <<<"$second_upload"
	[ "$second_status" = ready ] && [ -n "$second_pid" ] || fail "the second upload never became playable"
	play "$second_pid" || fail "the second video didn't play before the disaster"

	step "The recovery kit: .env, saved off the server"
	install -m 0600 "$opt/.env" "$kit"

	step "The disaster: --uninstall --purge"
	dc config --images >"$work/images"
	purge
	[ ! -e "$opt" ] || fail "--purge left $opt"
	[ -z "$(docker volume ls -q --filter "label=com.docker.compose.project=$COMPOSE_PROJECT_NAME")" ] ||
		fail "--purge left the drill's volumes"
	while IFS= read -r img; do docker image rm "$img" >/dev/null; done <"$work/images"
else
	install -m 0600 "$envfile" "$kit"
fi

step "A new host: Docker and the recovery kit"
mkdir -p "$opt"
install -m 0600 "$kit" "$opt/.env"
mode=--restore
[ -z "$envfile" ] || mode=--restore-only
step "install.sh $mode"
restore_started=$SECONDS
timed restore oneliner "$mode" 2>&1 | stamped | tee "$work/restore.log" ||
	fail "install.sh $mode failed; the log is above"
restored=$(sed -n 's/.*] backup \(bak_[[:alnum:]]*\):.*/\1/p' "$work/restore.log")
[ -n "$restored" ] || fail "the installer didn't identify the restored backup"

step "Checks on the restored install"
if [ -z "$envfile" ]; then
	waitfor "$public"
	timed play play "$first_pid" || fail "the first video doesn't play after the restore"
	printf 'first_playback\t%s\n' "$((SECONDS - restore_started))" >>"$timings"
	curl -fsS -b "$jar" "$public/v1/session" >/dev/null || fail "the owner's session didn't survive the restore"
	curl -fsS -H "@$work/auth" "$public/v1/app" >/dev/null || fail "the API key didn't survive the restore"
	dc exec -T app clipmux admin restore report >"$work/report.txt" || fail "restore report failed"
else
	[ "$(dc ps --status running --services)" = postgres ] || fail "expected only Postgres to be running"
	dc run --rm -T --no-deps app admin restore report >"$work/report.txt" || fail "restore report failed"
fi
grep -q '^restore fence: up' "$work/report.txt" || fail "the restored database isn't fenced"
cat "$work/report.txt"
timed verify_after dc run --rm -T --no-deps app admin backup verify --id "$restored" | tee "$work/verify.log"
grep -Fq 'every content key version it needs loads' "$work/verify.log" || fail "the backup didn't pass a full restore verification"
if [ -z "$envfile" ]; then
	grep -Fq "media/$second/" "$work/report.txt" || fail "the newer output isn't in the media bucket"
	grep -Fq "uploads/$second/source" "$work/report.txt" || fail "the newer source isn't in the media bucket"
	dc exec -T app clipmux admin backup now || fail "taking a backup after recovery failed"
	dc exec -T app clipmux admin backup check || fail "backups don't carry on after the restore"
	dc exec -T app clipmux admin restore finish --yes || fail "restore finish failed"
fi

step "Clean up: --uninstall --purge"
purge

total=$((SECONDS - t_start))
printf '\nPhase times (seconds):\n'
awk -F'\t' '{ printf "  %-14s %6d\n", $1, $2 }' "$timings"
printf '  %-14s %6d\n' total "$total"
printf '\ncheck_restore: PASS on %s (%s, %s CPUs). Record the times as a decision.\n' "$PRETTY_NAME" "$(uname -m)" "$(nproc)"
