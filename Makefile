.PHONY: db-up db-down test test-db run build fmt

COMPOSE := docker compose -f deploy/compose.dev.yml

db-up:
	$(COMPOSE) up -d --wait

db-down:
	$(COMPOSE) down

test:
	go vet ./...
	go test ./...

test-db:
	TEST_DATABASE_URL=postgres://clipmux:clipmux@localhost:5444/clipmux_test?sslmode=disable go test ./...

run:
	DATABASE_URL=$${DATABASE_URL:-postgres://clipmux:clipmux@localhost:5444/clipmux?sslmode=disable} go run ./cmd/clipmux serve

build:
	go build -o bin/clipmux ./cmd/clipmux

fmt:
	gofmt -w .
