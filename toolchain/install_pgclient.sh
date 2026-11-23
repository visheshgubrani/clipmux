#!/usr/bin/env bash
# Install the pinned PostgreSQL client tools backups need (pg_dump, and
# pg_restore for restores) and their libpq, from PGDG's .deb files, each
# verified against the SHA-256 in versions.env. Only those two binaries and
# libpq.so.5 are installed, never the packages, so nothing else comes with them.
# The libraries libpq itself links (Kerberos, LDAP, OpenSSL) are the runtime
# image's, from toolchain/apt-packages.env.
#
#   ./install_pgclient.sh [prefix]      # default /usr/local: bin/ and lib/
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
set -a; . "$here/versions.env"; set +a
dest=${1:-/usr/local}

case "$(uname -m)" in
  x86_64)        arch=amd64; client_sha=$PGCLIENT_AMD64_SHA256; libpq_sha=$PGCLIENT_LIBPQ_AMD64_SHA256 ;;
  aarch64|arm64) arch=arm64; client_sha=$PGCLIENT_ARM64_SHA256; libpq_sha=$PGCLIENT_LIBPQ_ARM64_SHA256 ;;
  *) echo "install_pgclient: unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# fetch NAME VERSION SHA: download one .deb and check it.
fetch() {
  local name=$1 version=$2 want=$3 file="$tmp/$1.deb" got
  curl -fsSL --connect-timeout 20 --max-time 300 --retry 5 --retry-delay 10 --retry-all-errors \
    -o "$file" "$PGCLIENT_POOL_URL/${name}_${version}_${arch}.deb"
  if command -v sha256sum >/dev/null; then got=$(sha256sum "$file" | cut -d' ' -f1); else got=$(shasum -a 256 "$file" | cut -d' ' -f1); fi
  if [ "$got" != "$want" ]; then
    echo "install_pgclient: ${name}_${version}_${arch}.deb failed SHA-256 verification (got $got, want $want)" >&2
    exit 1
  fi
  dpkg-deb -x "$file" "$tmp/$name"
}
fetch postgresql-client-"$PGCLIENT_MAJOR" "$PGCLIENT_VERSION" "$client_sha"
fetch libpq5 "$PGCLIENT_LIBPQ_VERSION" "$libpq_sha"

install -d "$dest/bin" "$dest/lib"
for tool in pg_dump pg_restore; do
  install -m 0755 "$tmp/postgresql-client-$PGCLIENT_MAJOR/usr/lib/postgresql/$PGCLIENT_MAJOR/bin/$tool" "$dest/bin/$tool"
done
cp -a "$tmp"/libpq5/usr/lib/*-linux-gnu/libpq.so.5* "$dest/lib/"
echo "installed pg_dump and pg_restore $PGCLIENT_VERSION to $dest"
