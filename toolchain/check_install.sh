#!/usr/bin/env bash
# The one-line installers end to end (P6-T08), against two release
# candidates served from this machine: deploy/install.sh installs the first,
# passes the setup test through Caddy, re-runs to upgrade to the second
# keeping the data, keys and the owner's session, then uninstalls (volumes
# kept) and purges; deploy/install-agent.sh, given a tarball, installs the
# agent and pairs it with a code from the dashboard's API.
#
#   check_install.sh IMAGE1 IMAGE2 [AGENT_TARBALL]
#
# IMAGE1 and IMAGE2 are repository:tag references in a registry the Docker
# daemon can pull from (the Install workflow runs registry:2 on localhost),
# both of the same repository, built with VERSION set to their tags. The
# agent check installs a system unit as root, or a user unit otherwise.
#
# Environment: CLIPMUX_HTTP_PORT, CLIPMUX_HTTPS_PORT and CLIPMUX_ADMIN_PORT
# (default 80, 443, 8080) and COMPOSE_PROJECT_NAME (default clipmux) pick
# where the stack runs, so a developer machine can run it beside others.
set -euo pipefail
image1=${1:?usage: check_install.sh IMAGE1 IMAGE2 [AGENT_TARBALL]}
image2=${2:?usage: check_install.sh IMAGE1 IMAGE2 [AGENT_TARBALL]}
tarball=${3:-}
repo=${image1%:*}
v1=${image1##*:}
v2=${image2##*:}
[ "${image2%:*}" = "$repo" ] || { echo "check_install: both images must be in one repository" >&2; exit 2; }
here=$(cd "$(dirname "$0")/.." && pwd)
http=${CLIPMUX_HTTP_PORT:-80}
export CLIPMUX_HTTP_PORT=$http CLIPMUX_HTTPS_PORT=${CLIPMUX_HTTPS_PORT:-443} CLIPMUX_ADMIN_PORT=${CLIPMUX_ADMIN_PORT:-8080}
export COMPOSE_PROJECT_NAME=${COMPOSE_PROJECT_NAME:-clipmux}
export CLIPMUX_IMAGE=$repo
base=http://localhost
[ "$http" = 80 ] || base=$base:$http

work=$(mktemp -d)
opt=$work/opt
server_pid=
cleanup() {
	[ -z "$server_pid" ] || kill "$server_pid" 2>/dev/null || true
	if [ -f "$opt/compose.yml" ]; then
		docker compose --project-directory "$opt" -f "$opt/compose.yml" down --volumes --remove-orphans >/dev/null 2>&1 || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT
fail() {
	echo "check_install: $*" >&2
	[ ! -f "$opt/compose.yml" ] || docker compose --project-directory "$opt" -f "$opt/compose.yml" logs --tail 40 >&2 || true
	exit 1
}
step() { printf '\n== %s\n' "$*"; }
dc() { docker compose --project-directory "$opt" -f "$opt/compose.yml" "$@"; }

step "installer scripts pass sh -n"
sh -n "$here/deploy/install.sh" "$here/deploy/install-agent.sh"

# The operator compose is the dev compose with the image in place of the
# build: anything else that differs is drift.
step "deploy/compose.release.yml matches deploy/compose.yml"
norm() {
	POSTGRES_PASSWORD=x CLIPMUX_SECRETS_KEY=x CLIPMUX_VERSION=x docker compose -f "$1" config --format json |
		jq 'del(.name, .services.app.build, .services.app.image) | walk(if type == "object" then del(.source) else . end)'
}
diff <(norm "$here/deploy/compose.yml") <(norm "$here/deploy/compose.release.yml") || fail "the two compose files drifted apart"

step "deploy/compose.proxy.yml takes Caddy out of either stack and nothing else"
for f in compose.yml compose.release.yml; do
	services=$(POSTGRES_PASSWORD=x CLIPMUX_SECRETS_KEY=x CLIPMUX_VERSION=x docker compose -f "$here/deploy/$f" -f "$here/deploy/compose.proxy.yml" config --services | sort | tr '\n' ' ')
	[ "$services" = "app postgres " ] || fail "$f with the proxy override runs: $services"
done

step "deploy/compose.nvidia.yml and deploy/compose.dri.yml merge cleanly with either stack"
for f in compose.yml compose.release.yml; do
	for g in compose.nvidia.yml compose.dri.yml; do
		POSTGRES_PASSWORD=x CLIPMUX_SECRETS_KEY=x CLIPMUX_VERSION=x docker compose -f "$here/deploy/$f" -f "$here/deploy/$g" config >/dev/null ||
			fail "$f with $g failed compose config"
	done
done

# Each release as its GitHub Release would hold it.
release() {
	local v=$1 d=$work/releases/$1
	mkdir -p "$d"
	cp "$here/deploy/compose.release.yml" "$d/compose.yml"
	cp "$here/deploy/Caddyfile" "$here/deploy/install.sh" "$here/deploy/install-agent.sh" "$d/"
	sed -i "s/@CLIPMUX_VERSION@/$v/" "$d/install.sh" "$d/install-agent.sh"
	[ -z "$tarball" ] || cp "$tarball" "$d/"
	(cd "$d" && sha256sum -- * >SHA256SUMS)
}
release "$v1"
release "$v2"
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
python3 -m http.server "$port" --bind 127.0.0.1 --directory "$work/releases" >/dev/null 2>&1 &
server_pid=$!
for _ in $(seq 50); do curl -fs -o /dev/null "http://127.0.0.1:$port/" && break; sleep 0.1; done

step "A tampered compose.yml is refused"
mkdir -p "$work/bad" && cp -r "$work/releases/$v1/." "$work/bad/"
echo "# tampered" >>"$work/bad/compose.yml"
mv "$work/bad" "$work/releases/bad"
if CLIPMUX_RELEASE_URL=http://127.0.0.1:$port/bad sh "$work/releases/$v1/install.sh" --dir "$opt" >"$work/bad.log" 2>&1; then
	fail "installed a compose.yml that doesn't match SHA256SUMS"
fi
grep -q "doesn't match SHA256SUMS" "$work/bad.log" || { cat "$work/bad.log" >&2; fail "the refusal didn't say why"; }
[ ! -f "$opt/compose.yml" ] || fail "a refused install left compose.yml behind"
rm -rf "$opt"

step "Install $v1"
CLIPMUX_RELEASE_URL=http://127.0.0.1:$port/$v1 sh "$work/releases/$v1/install.sh" --dir "$opt" | tee "$work/install1.log"
token=$(sed -n 's/^ *Setup token: *\([A-Z2-7]\{8,\}\)$/\1/p' "$work/install1.log")
[ -n "$token" ] || fail "the installer printed no setup token"
grep -q "Store this off the host" "$work/install1.log" || fail "the installer didn't print the secrets key"
[ "$(stat -c %a "$opt/.env")" = 600 ] || fail ".env isn't 0600"
[ "$(dc exec -T app clipmux version)" = "$v1" ] || fail "the app isn't $v1"
key1=$(sed -n 's/^CLIPMUX_SECRETS_KEY=//p' "$opt/.env")

step "The owner registers through Caddy and the setup test passes"
jar=$work/cookies
curl -fsS -c "$jar" -H 'Content-Type: application/json' -H "Origin: $base" \
	-d "{\"setup_token\":\"$token\",\"email\":\"owner@example.test\",\"password\":\"a long test password\"}" \
	"$base/v1/setup/owner" >/dev/null || fail "creating the owner failed"
curl -fsS -b "$jar" "$base/v1/session" >/dev/null || fail "the owner's session doesn't work"
# The URLs step of first boot, as the dashboard saves it.
curl -fsS -b "$jar" -X PUT -H 'Content-Type: application/json' -H "Origin: $base" \
	-d "{\"public_url\":\"$base\",\"media_url\":\"$base\"}" "$base/v1/settings/urls" >/dev/null || fail "saving the URLs failed"
dc exec -T app clipmux admin setup-test --wait --timeout 15m || fail "the setup test failed"
dc exec -T app clipmux admin create-key --app Installed --allow-loopback >"$work/key.txt" || fail "create-key failed"

if [ -n "$tarball" ]; then
	step "An agent installs and pairs from the dashboard's command"
	inv=$(curl -fsS -b "$jar" -X POST -H 'Content-Type: application/json' -H "Origin: $base" -d '{}' "$base/v1/agents/pairings") ||
		fail "creating a pairing code failed"
	code=$(jq -r .code <<<"$inv")
	# Not `| grep -q`: under pipefail, grep leaving early fails the pipe.
	grep -q -- "--pair $code" <<<"$(jq -r .install_command <<<"$inv")" || fail "the invitation has no install command: $inv"
	CLIPMUX_RELEASE_URL=http://127.0.0.1:$port/$v1 sh "$work/releases/$v1/install-agent.sh" --server "$base" --pair "$code" --name install-check |
		tee "$work/agent.log"
	grep -q "This PC is connected" "$work/agent.log" || fail "the agent didn't report a connection"
	online=
	for _ in $(seq 60); do
		online=$(curl -fsS -b "$jar" "$base/v1/agents" | jq -r '.data[] | select(.name == "install-check") | .online')
		[ "$online" = true ] && break
		sleep 2
	done
	[ "$online" = true ] || fail "the agent never showed online"
	if [ "$(id -u)" = 0 ]; then unit=(systemctl); else unit=(systemctl --user); fi
	if grep -q -- "--pair" <<<"$("${unit[@]}" cat clipmux-agent.service)"; then fail "the spent code is still in the unit"; fi
	CLIPMUX_RELEASE_URL=http://127.0.0.1:$port/$v1 sh "$work/releases/$v1/install-agent.sh" --uninstall --purge
fi

step "Re-running the installer upgrades to $v2"
CLIPMUX_RELEASE_URL=http://127.0.0.1:$port/$v2 sh "$work/releases/$v2/install.sh" --dir "$opt" | tee "$work/install2.log"
grep -q "upgraded from $v1 to $v2" "$work/install2.log" || fail "the installer didn't report the upgrade"
[ "$(dc exec -T app clipmux version)" = "$v2" ] || fail "the app isn't $v2 after the upgrade"
[ "$(sed -n 's/^CLIPMUX_SECRETS_KEY=//p' "$opt/.env")" = "$key1" ] || fail "the upgrade changed the secrets key"
curl -fsS -b "$jar" "$base/v1/session" >/dev/null || fail "the upgrade ended the owner's session"
keys=$(dc exec -T app clipmux admin list-keys --app Installed 2>&1) || fail "list-keys failed: $keys"
grep -q "^  key_.*revoked -$" <<<"$keys" || fail "the upgrade lost the app's API key: $keys"
[ "$(dc exec -T app clipmux admin setup-test --wait --timeout 15m >/dev/null && echo ok)" = ok ] || fail "the setup test failed after the upgrade"

step "--uninstall keeps the volumes; --purge deletes them"
sh "$work/releases/$v2/install.sh" --dir "$opt" --uninstall
[ -z "$(docker ps -aq --filter "label=com.docker.compose.project=$COMPOSE_PROJECT_NAME")" ] || fail "containers are left after --uninstall"
docker volume inspect "${COMPOSE_PROJECT_NAME}_pgdata" "${COMPOSE_PROJECT_NAME}_data" >/dev/null || fail "--uninstall deleted the volumes"
[ -f "$opt/.env" ] || fail "--uninstall deleted .env"
sh "$work/releases/$v2/install.sh" --dir "$opt" --uninstall --purge
if docker volume inspect "${COMPOSE_PROJECT_NAME}_pgdata" >/dev/null 2>&1; then fail "--purge kept the database volume"; fi
[ ! -e "$opt" ] || fail "--purge kept $opt"

echo
echo "check_install: install, setup test, upgrade $v1 → $v2, ${tarball:+agent pairing, }uninstall and purge all passed"
