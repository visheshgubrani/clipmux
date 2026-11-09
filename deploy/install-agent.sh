#!/bin/sh
# Installs the ClipMux agent on a Linux PC and connects it to a server
# (docs/plans/P6-launch.md, "One-line install"): the self-contained tarball
# (D061) for this machine's architecture, checked against the release's
# SHA256SUMS, and a systemd unit that starts it at boot. The Workers page
# shows the command with a fresh pairing code:
#
#   curl -fsSL https://github.com/visheshgubrani/clipmux/releases/download/vX.Y.Z/install-agent.sh | sh -s -- --server URL --pair CODE
#
# As root it installs to /opt/clipmux-agent with a system unit that runs as
# the clipmux-agent user (in the video and render groups, for GPUs). As
# anyone else it installs to ~/.local/share/clipmux-agent with a user unit,
# and asks systemd to keep it running while you're logged out. Run again with
# a new release to upgrade; the connection is kept.
#
# Options (after `sh -s --`):
#   --server URL       the ClipMux server
#   --pair CODE        a pairing code from its Workers page (not needed to upgrade)
#   --name NAME        what the Workers page calls this PC (default: its hostname)
#   --version vX.Y.Z   that release (default: the one this script came from, else the latest)
#   --uninstall        stop and remove the agent; its connection is kept unless --purge
#   --purge            with --uninstall: forget the connection too
#
# Environment, for testing and mirrors:
#   CLIPMUX_RELEASE_URL   where the release's files are (default: its GitHub Release)
#
# macOS and Windows run the agent in Docker; the Workers page shows that
# command too. Everything is inside main, so a truncated download runs nothing.

main() {
	repo=visheshgubrani/clipmux
	version="@CLIPMUX_VERSION@"
	server=
	code=
	name=
	groups=
	uninstall=
	purge=
	while [ $# -gt 0 ]; do
		case $1 in
		--server) server=${2:?--server needs a URL}; shift ;;
		--server=*) server=${1#*=} ;;
		--pair) code=${2:?--pair needs a code}; shift ;;
		--pair=*) code=${1#*=} ;;
		--name) name=${2:?--name needs a name}; shift ;;
		--name=*) name=${1#*=} ;;
		--version) version=${2:?--version needs a tag}; shift ;;
		--version=*) version=${1#*=} ;;
		--uninstall) uninstall=1 ;;
		--purge) purge=1 ;;
		-h | --help) usage; exit 0 ;;
		*) die "unknown option $1 (see --help)" ;;
		esac
		shift
	done
	[ -z "$purge" ] || [ -n "$uninstall" ] || die "--purge goes with --uninstall"
	[ "$(uname -s)" = Linux ] || die "this installer is for Linux; on macOS and Windows, run the Docker command from the Workers page"

	if [ "$(id -u)" = 0 ]; then
		system=1
		prefix=/opt/clipmux-agent
		bindir=/usr/local/bin
		state=/var/lib/clipmux-agent
		unit=/etc/systemd/system/clipmux-agent.service
	else
		system=
		prefix=${XDG_DATA_HOME:-$HOME/.local/share}/clipmux-agent
		bindir=$HOME/.local/bin
		state=${XDG_CONFIG_HOME:-$HOME/.config}/clipmux
		unit=${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/clipmux-agent.service
	fi
	if [ -n "$uninstall" ]; then
		remove
		return
	fi

	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "the agent is built for amd64 and arm64, not $(uname -m)" ;;
	esac
	check_glibc
	need curl
	need tar
	need awk
	if [ ! -f "$state/agent.json" ] && { [ -z "$server" ] || [ -z "$code" ]; }; then
		die "give --server URL and --pair CODE (the Workers page shows this command with both)"
	fi

	case $version in
	@*@ | "" | latest) version=$(latest) ;;
	esac
	case $version in
	v[0-9]*) ;;
	*) die "$version isn't a release tag (vX.Y.Z)" ;;
	esac
	base=${CLIPMUX_RELEASE_URL:-https://github.com/$repo/releases/download/$version}
	tarball=clipmux-agent-linux-$arch.tar.gz

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	say "Downloading the ClipMux agent $version for linux/$arch"
	fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS"
	fetch "$base/$tarball" "$tmp/$tarball"
	verify "$tmp" "$tarball"
	mkdir -p "$tmp/x"
	tar -xzf "$tmp/$tarball" -C "$tmp/x"
	[ -x "$tmp/x/clipmux-agent/clipmux" ] || die "$tarball has no clipmux-agent/clipmux"

	# Swap the whole directory, so an upgrade never mixes two releases.
	mkdir -p "$(dirname "$prefix")" "$bindir"
	rm -rf "$prefix.new" "$prefix.old"
	mv "$tmp/x/clipmux-agent" "$prefix.new"
	[ ! -d "$prefix" ] || mv "$prefix" "$prefix.old"
	mv "$prefix.new" "$prefix"
	rm -rf "$prefix.old"
	ln -sf "$prefix/clipmux" "$bindir/clipmux"
	say "Installed $("$prefix/clipmux" version) in $prefix"

	if ! has_systemd; then
		say ""
		say "No systemd here, so nothing starts the agent at boot. Run it with:"
		say "  $bindir/clipmux worker${server:+ --server $server}${code:+ --pair $code}${name:+ --name $name}"
		return
	fi
	[ -z "$system" ] || ensure_user
	paired=
	[ -f "$state/agent.json" ] && paired=1
	write_unit "$code"
	ctl daemon-reload
	ctl enable clipmux-agent.service >/dev/null 2>&1
	ctl restart clipmux-agent.service
	if [ -z "$system" ]; then
		# A user unit stops at logout unless the user lingers.
		loginctl enable-linger "$(id -un)" 2>/dev/null ||
			say "note: couldn't enable lingering; the agent runs only while you're logged in (sudo loginctl enable-linger $(id -un))"
	fi

	if [ -z "$paired" ]; then
		say "Connecting to $server"
		i=0
		while [ ! -f "$state/agent.json" ] && [ $i -lt 60 ]; do
			sleep 1
			i=$((i + 1))
		done
		if [ ! -f "$state/agent.json" ]; then
			logs >&2
			die "the agent didn't connect: the code may have expired or been used (create a new one on the Workers page); its log is above"
		fi
		# The code is spent; take it out of the unit.
		write_unit ""
		ctl daemon-reload
	fi
	sleep 2
	ctl is-active --quiet clipmux-agent.service || {
		logs >&2
		die "the agent isn't running; its log is above"
	}
	say ""
	if [ -n "$paired" ]; then
		say "The ClipMux agent is upgraded to $version and running."
	else
		say "This PC is connected. It shows on the Workers page and takes jobs now."
	fi
	if [ -n "$system" ]; then
		say "Logs: journalctl -u clipmux-agent -f    Check: sudo -u clipmux-agent $bindir/clipmux worker doctor"
	else
		say "Logs: journalctl --user -u clipmux-agent -f    Check: clipmux worker doctor"
	fi
}

usage() {
	sed -n '2,/^# command too/p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//' ||
		echo "see https://github.com/visheshgubrani/clipmux/blob/main/deploy/install-agent.sh"
}

say() { printf '%s\n' "$*"; }
die() {
	printf 'install-agent.sh: %s\n' "$*" >&2
	exit 1
}
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is needed"; }

has_systemd() { [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; }

ctl() {
	if [ -n "$system" ]; then systemctl "$@"; else systemctl --user "$@"; fi
}

logs() {
	if [ -n "$system" ]; then
		journalctl -u clipmux-agent -n 30 --no-pager 2>/dev/null
	else
		journalctl --user -u clipmux-agent -n 30 --no-pager 2>/dev/null
	fi
}

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
	else
		openssl dgst -sha256 -r "$1" | awk '{print $1}'
	fi
}

verify() {
	want=$(awk -v f="$2" '$2 == f || $2 == "*" f {print $1}' "$1/SHA256SUMS")
	[ -n "$want" ] || die "SHA256SUMS doesn't list $2"
	got=$(sha256 "$1/$2")
	[ "$got" = "$want" ] || die "$2 doesn't match SHA256SUMS (got $got, want $want)"
}

# The tarball's FFmpeg and libraries are built on Ubuntu 24.04 (glibc 2.39).
check_glibc() {
	v=$(getconf GNU_LIBC_VERSION 2>/dev/null | awk '{print $2}')
	[ -n "$v" ] || die "the agent needs glibc (not musl); run the Docker command from the Workers page instead"
	major=${v%%.*}
	minor=${v#*.}
	minor=${minor%%.*}
	if [ "$major" -lt 2 ] || { [ "$major" = 2 ] && [ "$minor" -lt 39 ]; }; then
		die "the agent needs glibc 2.39 or newer (Ubuntu 24.04, Debian 13, Fedora 40); this has $v. Run the Docker command from the Workers page instead"
	fi
}

ensure_user() {
	id clipmux-agent >/dev/null 2>&1 ||
		useradd --system --home-dir "$state" --no-create-home --shell /usr/sbin/nologin clipmux-agent
	groups=
	for g in video render; do
		getent group "$g" >/dev/null 2>&1 && groups="$groups $g"
	done
	groups=${groups# }
}

# write_unit writes the unit; CODE, while it is unspent, pairs on first start
# (the worker ignores a spent code once agent.json exists).
write_unit() {
	args="worker"
	[ -z "$server" ] || args="$args --server $server"
	[ -z "$1" ] || args="$args --pair $1"
	[ -z "$name" ] || args="$args --name '$name'"
	mkdir -p "$(dirname "$unit")"
	if [ -n "$system" ]; then
		cat >"$unit" <<UNIT
# Written by install-agent.sh. Re-run it to upgrade.
[Unit]
Description=ClipMux agent
Wants=network-online.target
After=network-online.target

[Service]
User=clipmux-agent
Group=clipmux-agent
${groups:+SupplementaryGroups=$groups}
Environment=CLIPMUX_AGENT_DIR=$state
StateDirectory=clipmux-agent
StateDirectoryMode=0700
ExecStart=$prefix/clipmux $args --work-dir $state/work
Restart=always
RestartSec=5
# The agent hands a running job back on SIGTERM.
TimeoutStopSec=40
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=full

[Install]
WantedBy=multi-user.target
UNIT
	else
		cat >"$unit" <<UNIT
# Written by install-agent.sh. Re-run it to upgrade.
[Unit]
Description=ClipMux agent
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=$prefix/clipmux $args
Restart=always
RestartSec=5
TimeoutStopSec=40

[Install]
WantedBy=default.target
UNIT
	fi
}

remove() {
	if has_systemd && [ -f "$unit" ]; then
		ctl disable --now clipmux-agent.service 2>/dev/null || true
		rm -f "$unit"
		ctl daemon-reload
	fi
	if [ -L "$bindir/clipmux" ] && [ "$(readlink "$bindir/clipmux")" = "$prefix/clipmux" ]; then
		rm -f "$bindir/clipmux"
	fi
	rm -rf "$prefix"
	if [ -n "$purge" ]; then
		rm -f "$state/agent.json"
		[ -z "$system" ] || rm -rf "$state"
		say "The ClipMux agent is removed, and this PC's connection forgotten. Revoke it on the Workers page too."
	else
		say "The ClipMux agent is removed. Its connection ($state/agent.json) is kept for a reinstall; --uninstall --purge forgets it."
	fi
}

set -eu
main "$@"
