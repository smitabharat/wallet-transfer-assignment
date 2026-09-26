.PHONY: run build test lint fmt fmt-check db-up db-down

# PostgreSQL started by `make db-up` (docker compose). Override either
# variable to point at another server.
DATABASE_URL ?= postgres://postgres:postgres@localhost:55432/wallet?sslmode=disable
TEST_DATABASE_URL ?= $(DATABASE_URL)
export DATABASE_URL TEST_DATABASE_URL

db-up:
	docker compose up -d --wait postgres

db-down:
	docker compose down

run:
	go run ./cmd/server

build:
	go build -o bin/wallet-server ./cmd/server

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -w .

# Fails if any file is not gofmt-formatted.
fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "Unformatted files:"; echo "$$out"; exit 1; fi
