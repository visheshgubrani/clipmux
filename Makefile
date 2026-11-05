.PHONY: db-up db-down test test-db run build fmt lint sqlc sqlc-check

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
test-db:
	TEST_DATABASE_URL=postgres://clipmux:clipmux@localhost:5444/clipmux_test?sslmode=disable go test ./...

run:
	DATABASE_URL=$${DATABASE_URL:-postgres://clipmux:clipmux@localhost:5444/clipmux?sslmode=disable} go run ./cmd/clipmux serve

build:
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
