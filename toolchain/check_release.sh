#!/usr/bin/env bash
# A published release's one-line install on a clean machine (P6 exit check 1,
# D136): run as root on a fresh Ubuntu 24.04 or Debian 12 VM with a public
# IPv4 address and ports 80 and 443 open. It
#   - installs Docker from Docker's apt repository if it is missing;
#   - runs the one-liner exactly as the landing page shows it;
#   - registers the owner, sets the public and media URLs to sslip.io names
#     of the VM's address, so Caddy gets real certificates, and checks both
#     over verified HTTPS;
#   - runs the setup test, which plays the sample through delivery;
#   - re-runs the one-liner and checks that the secrets key, the owner's
#     session and an API key survive it;
#   - uninstalls (the volumes stay), installs again on the same data, and
#     uninstalls with --purge.
#
#   check_release.sh [INSTALL_URL]     (default https://clipmux.com/install.sh)
#
# Never run it on a machine with a ClipMux you want: --purge deletes it.
set -euo pipefail
url=${1:-https://clipmux.com/install.sh}
opt=/opt/clipmux
work=$(mktemp -d)
step() { printf '\n== %s\n' "$*"; }
fail() {
	printf 'check_release: FAIL: %s\n' "$*" >&2
	exit 1
}
dc() { docker compose --project-directory "$opt" -f "$opt/compose.yml" "$@"; }
oneliner() { curl -fsSL "$url" | sh -s -- "$@"; }

[ "$(id -u)" = 0 ] || fail "run as root"
[ ! -e "$opt" ] || fail "$opt exists: this is for a clean machine"
# shellcheck source=/dev/null
. /etc/os-release
step "On $PRETTY_NAME ($(uname -m))"

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
command -v jq >/dev/null || { apt-get update -qq && apt-get install -y -qq jq >/dev/null; }
docker compose version

step "Install with the one-liner"
oneliner | tee "$work/install.log"
token=$(sed -n 's/^ *Setup token: *\([A-Z2-7]\{8,\}\)$/\1/p' "$work/install.log")
[ -n "$token" ] || fail "no setup token"
ip=$(sed -n 's|^ *Open: *http://\([0-9.]*\).*|\1|p' "$work/install.log")
[ -n "$ip" ] || fail "no address after Open:"
version=$(sed -n 's/^CLIPMUX_VERSION=//p' "$opt/.env")
[ "$(dc exec -T app clipmux version)" = "$version" ] || fail "the app isn't $version"
key1=$(sed -n 's/^CLIPMUX_SECRETS_KEY=//p' "$opt/.env")
echo "installed $version; open http://$ip"

dashed=${ip//./-}
public=https://video.$dashed.sslip.io
media=https://media.$dashed.sslip.io
step "The owner registers at http://$ip and sets $public and $media"
jar=$work/cookies
curl -fsS -c "$jar" -H 'Content-Type: application/json' -H "Origin: http://$ip" \
	-d "{\"setup_token\":\"$token\",\"email\":\"owner@example.test\",\"password\":\"a long test password\"}" \
	"http://$ip/v1/setup/owner" >/dev/null || fail "creating the owner failed"
curl -fsS -b "$jar" -X PUT -H 'Content-Type: application/json' -H "Origin: http://$ip" \
	-d "{\"public_url\":\"$public\",\"media_url\":\"$media\"}" "http://$ip/v1/settings/urls" >/dev/null || fail "saving the URLs failed"
for u in "$public" "$media"; do
	ok=
	for _ in $(seq 30); do
		curl -fsS --max-time 10 "$u/healthz" >/dev/null 2>&1 && { ok=1; break; }
		sleep 4
	done
	[ -n "$ok" ] || fail "$u/healthz isn't served over verified HTTPS"
	echo "$u: certificate $(echo | openssl s_client -connect "${u#https://}:443" -servername "${u#https://}" 2>/dev/null | openssl x509 -noout -issuer)"
done
# The session, from here on, is the one on the public URL.
curl -fsS -c "$jar" -H 'Content-Type: application/json' -H "Origin: $public" \
	-d '{"email":"owner@example.test","password":"a long test password"}' "$public/v1/session" >/dev/null ||
	fail "signing in at $public failed"

step "The setup test plays the sample"
dc exec -T app clipmux admin setup-test --wait --timeout 20m || fail "the setup test failed"
dc exec -T app clipmux admin create-key --app Installed >"$work/key.txt" || fail "create-key failed"
apikey=$(grep -o 'cmx_[A-Za-z0-9_]*' "$work/key.txt" | head -n 1)
[ -n "$apikey" ] || fail "create-key printed no key"

step "Re-run the one-liner: an upgrade in place"
oneliner | tee "$work/upgrade.log"
grep -q "upgraded" "$work/upgrade.log" || fail "the re-run didn't upgrade"
[ "$(sed -n 's/^CLIPMUX_SECRETS_KEY=//p' "$opt/.env")" = "$key1" ] || fail "the re-run changed the secrets key"
curl -fsS -b "$jar" "$public/v1/session" >/dev/null || fail "the owner's session didn't survive"
curl -fsS -H "Authorization: Bearer $apikey" "$public/v1/app" >/dev/null || fail "the API key didn't survive"
dc exec -T app clipmux admin setup-test --wait --timeout 20m || fail "the setup test failed after the re-run"

step "--uninstall keeps the volumes"
oneliner --uninstall
[ -f "$opt/.env" ] || fail "--uninstall removed .env"
docker volume ls --format '{{.Name}}' | grep -q '^clipmux_pgdata$' || fail "--uninstall removed the database volume"
oneliner >/dev/null
curl -fsS -b "$jar" "$public/v1/session" >/dev/null || fail "reinstalling lost the owner's session"

step "--uninstall --purge deletes everything"
oneliner --uninstall --purge
[ ! -e "$opt" ] || fail "--purge left $opt"
if docker volume ls --format '{{.Name}}' | grep -q '^clipmux_'; then fail "--purge left volumes"; fi

printf '\ncheck_release: PASS: %s on %s\n' "$version" "$PRETTY_NAME"
