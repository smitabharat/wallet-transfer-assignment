# Wallet Transfer Service — Solution

A Go service implementing idempotent, concurrency-safe wallet-to-wallet
transfers with a double-entry ledger on PostgreSQL.

- Design note (schema, idempotency, concurrency, failure modes): [`docs/DESIGN.md`](docs/DESIGN.md)

## Layout

```text
cmd/server             entrypoint (config, wiring, graceful shutdown)
internal/handler       HTTP transport: decoding, validation, error -> status mapping
internal/service       business workflow: transfer orchestration, idempotency
internal/repository    PostgreSQL persistence + schema.sql
internal/domain        entities, state machine, validation rules, errors
```

## Requirements

- Go 1.24+
- PostgreSQL 13+ (`make db-up` starts one in Docker on `localhost:55432`)

## Run

```bash
make db-up                   # PostgreSQL 16 via docker compose
make run                     # listens on :8080
# or: DATABASE_URL=postgres://user:pass@host:5432/db?sslmode=disable go run ./cmd/server
# or: go run ./cmd/server -addr :8080 -database-url postgres://...
```

The schema in `internal/repository/schema.sql` is applied on startup.

```bash
curl -X POST localhost:8080/wallets -d '{"id":"wallet_1","initialBalance":1000}'
curl -X POST localhost:8080/wallets -d '{"id":"wallet_2","initialBalance":0}'
curl -i -X POST localhost:8080/transfers \
  -d '{"idempotencyKey":"abc123","fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":100}'
curl localhost:8080/wallets/wallet_1
curl localhost:8080/transfers/<transfer-id>
```

Sending the same transfer again returns the same body and status with the
header `Idempotent-Replayed: true`, and the balance does not change.

## Test, lint, format

Tests that touch the database need `TEST_DATABASE_URL`; each test creates and
drops its own schema, so any database you can create schemas in will do. They
are skipped when it is unset. `make test` points it at the `make db-up`
database. `-race` needs cgo (a C compiler).

```bash
make test        # go test -race -count=1 ./...
make lint        # golangci-lint run ./...
make fmt-check   # gofmt -l .
```

Suggested CI repository variables: `LINT_CMD=golangci-lint run ./...`,
`FORMAT_CHECK_CMD=make fmt-check`, `TEST_CMD=go test -race -count=1 ./...`,
`TEST_DATABASE_URL` pointing at a PostgreSQL service container, and
`CGO_ENABLED=1` (for `-race`).

---

# Wallet Transfer Assignment Repository

This repository is a reusable coding assignment template for evaluating backend engineers on wallet transfers, idempotency, concurrency control, and double-entry ledger design.

## Included

- `ASSIGNMENT.md` - candidate-facing prompt
- `.github/pull_request_template.md` - required PR structure
- `.github/workflows/ci.yml` - lint, format, test placeholder workflow
- `.github/workflows/sonarqube.yml` - SonarQube pull request analysis
- `.github/copilot-instructions.md` - repository-level Copilot review guidance
- `evaluation_guide.md` - reviewer rubric
- `branch-protection-checklist.md` - GitHub setup checklist

## Intended use

1. Mark this repository as a GitHub template repository.
2. Create one private repository per candidate from the template.
3. Add the candidate as a collaborator.
4. Ask them to submit via a pull request into `main`.
5. Enable required checks, SonarQube, and Copilot review in GitHub.

## Notes

- Copilot automatic pull request review is configured in GitHub repository or organization settings, not purely through files in the repo.
- The `copilot-instructions.md` file included here provides repository-specific review guidance once Copilot review is enabled.
- The CI workflow is language-agnostic by default and expects you to set the `LINT_CMD`, `FORMAT_CHECK_CMD`, and `TEST_CMD` repository variables or replace the commands directly.

## How to Submit Assignment

1. **Fork this repository** to your own GitHub account.
2. Complete the assignment described in [`ASSIGNMENT.md`](./ASSIGNMENT.md).
3. **Raise a Pull Request** back to this repository (`main` branch) with your full solution.

Your PR branch should be named: `solution/<your-name>` (e.g., `solution/jane-doe`).
