# Develop and test

Requirements: Go 1.26 (`GOTOOLCHAIN=auto` fetches the toolchain from `go.mod`), Docker for integration tests, `grpcurl`, `golangci-lint` v2.

Commands run on 2026-11-13 for the standards change (host: Go 1.22 with `GOTOOLCHAIN=auto` selecting 1.26.10):

| Command | Result |
|---|---|
| `go vet ./... && go test -race -count=1 ./...` | passed (2 packages with tests) |
| `golangci-lint run ./...` and `golangci-lint run --build-tags integration ./...` (golangci-lint v2.11.4, `.golangci.yaml`) | 0 issues. The shared pipeline pins v2.14.0: not run |
| `gofmt -l .` | no files listed |
| `mkdocs build --strict` (mkdocs 1.6.1, mkdocs-techdocs-core 1.7.1, Python 3.12 venv made with uv) | passed |

## What the earlier author ran (recorded, not re-run for this page)

Commands and results below come from the README before the standards change; they were not repeated on 2026-11-13.

Requirements: Go 1.26 (`GOTOOLCHAIN=auto` fetches it), Docker, [`grpcurl`](https://github.com/fullstorydev/grpcurl).

What was actually run (all on the final tree):

```bash
# unit tests (validation, rate limiter, token/hash helpers, service on an in-memory store, config, no secrets in logs)
go vet ./... && gofmt -l . && go test -race ./...

# integration tests against a throwaway Postgres (each test creates and drops its own database)
docker run -d --rm --name usvc-test-$RANDOM --memory 256m -e POSTGRES_USER=users -e POSTGRES_PASSWORD=users \
  -p 127.0.0.1:55436:5432 postgres:17-alpine
TEST_DATABASE_URL='postgres://users:users@127.0.0.1:55436/postgres?sslmode=disable' \
  go test -race -tags integration ./tests/integration/

# container smoke run: image + a Postgres with its own database and user on one docker network
docker build -t user-service:local .
docker network create usvc-smoke-net
docker run -d --name usvc-smoke-db --memory 256m --network usvc-smoke-net --network-alias user-db -e POSTGRES_DB=users -e POSTGRES_USER=users -e POSTGRES_PASSWORD=users postgres:17-alpine
docker run -d --name usvc-smoke-svc --memory 128m --network usvc-smoke-net -p 127.0.0.1:58086:8086 \
  -e DATABASE_URL='postgres://users:users@user-db:5432/users?sslmode=disable' user-service:local
```

Results: `go vet`, `gofmt -l` (empty), `go test -race ./...` and the integration tests with `-race` pass; `golangci-lint run` (default linters, also with `-tags integration`) and `gosec -exclude-generated ./...` report no issues. The integration tests cover: migrations idempotent and six concurrent starts apply exactly one; register -> login -> get session -> logout -> get session `UNAUTHENTICATED`; only the SHA-256 and the bcrypt hash in the tables; duplicate e-mail in several cases (and the constraint itself with a direct insert); 30 concurrent registrations of one address (3 rounds) give exactly one account; wrong password and unknown e-mail give the same code and message; expired and revoked sessions; the rate limit; expired sessions deleted by a login; check constraints; database down gives `UNAVAILABLE` without details; the whole flow over a real gRPC connection; no secrets in the logs.

The container smoke run checked with `grpcurl` (reflection, no `-proto`): service list; register (e-mail lower-cased), duplicate in another case `ALREADY_EXISTS`, short password `INVALID_ARGUMENT`; `GetSession`; wrong password and unknown e-mail both `UNAUTHENTICATED` / `wrong email or password`; login in other case; logout twice `OK`; the logged-out token `UNAUTHENTICATED` while the register token still works; the 6th failed login for one address `RESOURCE_EXHAUSTED`; health `SERVING` and Docker health `healthy`; `/metrics` shows `grpc_server_handled_total` for the four methods; a new container on the same database still logs the user in (`BCRYPT_COST=10`, `SESSION_TTL=1h` honoured); health `NOT_SERVING` and `UNAVAILABLE` after disconnecting the database from the network, `SERVING` and a working login after reconnecting it; uid `10001`; exit code 0 on `docker stop`; no password, token or hash in the container log.

Not run: the OTLP exporters against a real collector and Pyroscope (the code is the order-service code; only the "off by default / forced off" switches are unit-tested), an arm64 build, `govulncheck`, the service inside the compose stack, any load or timing measurement.

Regenerate the Go code after editing the proto: `protoc -I proto --go_out=proto --go_opt=paths=source_relative --go-grpc_out=proto --go-grpc_opt=paths=source_relative proto/users.proto` (protoc 3.21.12, protoc-gen-go 1.36.11, protoc-gen-go-grpc 1.6.1). Proto changes are additive only.

## Build the docs

```bash
uv venv docs-venv && uv pip install --python docs-venv/bin/python 'mkdocs==1.6.1' 'mkdocs-techdocs-core==1.7.1'
docs-venv/bin/mkdocs build --strict
```

Mermaid diagrams are plain fenced code blocks for `mkdocs`; rendering them in Backstage needs the TechDocs Mermaid add-on (not verified here).
