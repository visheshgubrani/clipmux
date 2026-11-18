.PHONY: db-up db-down test test-db run build web worker worker-check site site-check fmt lint sqlc sqlc-check

COMPOSE := docker compose -f deploy/compose.dev.yml

db-up:
	$(COMPOSE) up -d --wait

db-down:
	$(COMPOSE) down

test:
	go vet ./...
	go test ./...

# Media tests run when ffmpeg, ffprobe and packager are on PATH (or
# CLIPMUX_PACKAGER points at Shaka); toolchain/install_packager.sh installs
# the pinned packager.
# The S3 variables point the storage contract tests at the dev SeaweedFS.
S3_TEST_ENV := CLIPMUX_TEST_S3_ENDPOINT=http://127.0.0.1:8333 CLIPMUX_TEST_S3_ACCESS_KEY=clipmuxdev CLIPMUX_TEST_S3_SECRET_KEY=clipmuxdevsecret0123456789
test-db:
	TEST_DATABASE_URL=postgres://clipmux:clipmux@localhost:5444/clipmux_test?sslmode=disable $(S3_TEST_ENV) go test ./...

run:
	DATABASE_URL=$${DATABASE_URL:-postgres://clipmux:clipmux@localhost:5444/clipmux?sslmode=disable} go run ./cmd/clipmux serve

# The dashboard, built into internal/web/dist for the binary to embed.
web:
	pnpm install --frozen-lockfile
	pnpm --filter "@clipmux/dashboard..." run build

# The Cloudflare Worker, bundled into internal/cloudflare for the binary to
# embed and deploy. The bundle is committed; CI checks it is current.
worker:
	pnpm install --frozen-lockfile
	pnpm --filter @clipmux/worker run build
	cp worker/dist/worker.js internal/cloudflare/worker.js

worker-check: worker
	git diff --exit-code internal/cloudflare/worker.js

# clipmux.com: the landing page and the docs (site/, D133).
site:
	pnpm install --frozen-lockfile
	pnpm --filter @clipmux/site run dev

# The typecheck, the static build, the docs lints (frontmatter, internal IDs,
# routes against the OpenAPI spec, settings, links and anchors, llms.txt and
# the Markdown twins) with their tests, and Biome.
site-check:
	pnpm install --frozen-lockfile
	pnpm --filter @clipmux/site run typecheck
	pnpm --filter @clipmux/site run build
	pnpm --filter @clipmux/site run test
	pnpm --filter @clipmux/site run lint:docs
	pnpm exec biome ci site

build: web
	go build -o bin/clipmux ./cmd/clipmux

fmt:
	gofmt -w .

# staticcheck with every check on, and govulncheck (go.dev/security).
lint:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 -checks all ./...
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

# sqlc runs from its container, so nobody needs the cgo build. Generated code
# is committed; CI checks it is current.
SQLC := docker run --rm -u $$(id -u):$$(id -g) -v $(CURDIR):/src -w /src sqlc/sqlc:1.30.0

sqlc:
	$(SQLC) generate

sqlc-check:
	$(SQLC) diff
