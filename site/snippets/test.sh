#!/bin/sh
# The Python, Go and PHP code the docs show, against the vector files the
# server, the SDK and the Worker share (internal/webhooks and
# internal/delivery testdata). `make snippets-test`; CI's `snippets` job.
# PHP runs in a container when php or composer isn't installed.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)

echo "== Python"
venv=${CLIPMUX_SNIPPETS_VENV:-${XDG_CACHE_HOME:-$HOME/.cache}/clipmux-snippets/venv}
[ -x "$venv/bin/python" ] || python3 -m venv "$venv"
"$venv/bin/pip" install --quiet --disable-pip-version-check -r "$here/python/requirements.txt"
(cd "$here/python" && PYTHONDONTWRITEBYTECODE=1 "$venv/bin/python" -m unittest test_snippets)

echo "== Go"
(cd "$here/go" && go vet ./... && go test ./...)

echo "== PHP"
if command -v php >/dev/null && command -v composer >/dev/null; then
	(cd "$here/php" && composer install --no-interaction --quiet && php test.php)
else
	docker run --rm -v "$here/php":/app -w /app -u "$(id -u):$(id -g)" -e COMPOSER_HOME=/tmp/composer \
		composer:2 install --no-interaction --quiet
	docker run --rm -v "$repo":/repo -w /repo/site/snippets/php php:8.4-cli php test.php
fi
