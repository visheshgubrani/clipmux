#!/bin/sh
# Uploads the built site (site/out) to HOST as a new release and switches
# /srv/clipmux-site/current to it in one rename, so a reader never sees half
# a deploy. Keeps the five newest releases. Run through `make site-deploy`.
#
#   SITE_HOST=user@host [SITE_SSH="ssh -i key"] make site-deploy
set -eu

host=${1:?usage: deploy.sh user@host}
ssh=${SITE_SSH:-ssh}
dir=/srv/clipmux-site
release=$(git rev-parse --short HEAD)
git diff --quiet HEAD -- site || release="$release-dirty"

test -f site/out/index.html || { echo "deploy.sh: build the site first (make site-check)" >&2; exit 1; }

$ssh "$host" "mkdir -p $dir/releases"
rsync -a --delete --exclude _headers --exclude _redirects -e "$ssh" site/out/ "$host:$dir/releases/$release/"
$ssh "$host" "set -e
	ln -sfn releases/$release $dir/current.new
	mv -Tf $dir/current.new $dir/current
	cd $dir/releases && ls -1t | grep -vx '$release' | tail -n +5 | xargs -r rm -rf"
echo "deploy.sh: $host now serves release $release"
