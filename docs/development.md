# Develop and test

Requirements: Go 1.26 (`GOTOOLCHAIN=auto` fetches the toolchain from `go.mod`), Docker for integration tests, `grpcurl`, `golangci-lint` v2.

Final checks of 2026-11-22 (branch `final/user-service`, host: Go 1.22 with `GOTOOLCHAIN=auto` selecting 1.26.11). The same results are in the `## Develop` table of the README.

| Command | Result |
|---|---|
| `gofmt -l .`, `go vet ./...`, `go vet -tags integration ./...` | clean |
| `go test -race -count=1 ./...` | passed (3 packages with tests; coverage `cmd/server` 17.8%, `internal/users` 73.7%, `internal/obs` 89.6%); `TestGRPCRequestPathLogsCarryTraceIDs` with `-count=50`: passed |
| `golangci-lint run ./...` and `golangci-lint run --build-tags integration ./...` (golangci-lint v2.14.0 in `golangci/golangci-lint:v2.14.0`, `.golangci.yaml`) | 0 issues both times |
| Integration tests | 16 integration tests and all unit tests, `go test -race -count=1 -tags integration -v ./tests/integration/ ./...`, throwaway `postgres:17-alpine` (`--memory 256m`): passed |
| `govulncheck ./...`, `gosec -exclude-generated ./...` | `No vulnerabilities found`; gosec 0 issues (20 files, 2269 lines) |
| `gitleaks git --redact` over the full history (37 commits scanned) | no leaks found |
| `docker build --pull --platform linux/amd64 ...`, `trivy image --severity CRITICAL,HIGH --exit-code 1` (0.76.1) | OK, 12.1 MB content (44 MB disk usage); 0 vulnerabilities, exit 0 |
| whole shop: `local/up.sh` (`SKIP_BUILD=1`), `local/smoke.sh` three times | `SMOKE PASSED`, 87 PASS lines each time; no error log lines of this service afterwards |
| `mkdocs build --strict` (mkdocs 1.6.1, mkdocs-techdocs-core 1.7.1) and the link check | passed |

The container run (image built with `--build-arg SERVICE_VERSION=final-test`, throwaway `postgres:17-alpine` on a private docker network, `grpcurl` with reflection): Docker health `healthy`, uid:gid `10001:10001`; `Register` (address lower-cased), the same address in another case `ALREADY_EXISTS`, `GetSession` returns the user, a wrong password and an unknown address both `UNAUTHENTICATED` / `wrong email or password`, `Login` with the address in another case OK, `Logout` OK, the logged-out token `UNAUTHENTICATED` while the register token still works, the 6th failed login in a row `RESOURCE_EXHAUSTED`; the `starting` log line carries `version: final-test` and the commit; no log line contains the address, the password or a token; `/metrics` shows `build_info`, `db_pool_connections_max 12` and `grpc_server_handled_total` per method and code; with the database disconnected health went `NOT_SERVING` and `GetSession` answered `UNAVAILABLE` (`database not ready`), after reconnecting `SERVING` again; 0 error log lines; `docker stop` exit code 0 after about 0.4 s.

bcrypt timing (one hash each, development host with 12 cores, a throwaway program using the module's `golang.org/x/crypto`): cost 11 0.18 s, cost 14 1.5 s, cost 16 5.9 s (see ADR 3 for what that means for `REQUEST_TIMEOUT`).

Not run: OTLP exporters against a real collector and Pyroscope, an arm64 build, several replicas against one database, `verify-signals.py`, any load or timing measurement beyond the bcrypt hashes above.

## What the earlier author ran (recorded, not re-run for this page)

Commands and results below come from the README before the standards change; they were not repeated for this page; the current results are in the table above.

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

Observability step 3 (2026-11-14): `go vet`, `go test ./...` and the integration tests against a throwaway Postgres 17 container pass; `golangci-lint run` (the repo's `.golangci.yaml`, also with `-tags integration`) reports 0 issues; `internal/obs` (the same package as in checkout-service) has the env-matrix, log-shape, resource-attribute and blackholed-collector tests; not repeated: `-race`, `gosec`.

Not run: the OTLP exporters against a real collector and Pyroscope (only the env switches, the bounded start and the bounded shutdown against a listener that never answers are tested), an arm64 build, any load or timing measurement (`govulncheck` and the compose stack were run on 2026-11-22, see above).

Regenerate the Go code after editing the proto: `protoc -I proto --go_out=proto --go_opt=paths=source_relative --go-grpc_out=proto --go-grpc_opt=paths=source_relative proto/users.proto` (protoc 3.21.12, protoc-gen-go 1.36.11, protoc-gen-go-grpc 1.6.1). Proto changes are additive only.

## Build the docs

```bash
uv venv docs-venv && uv pip install --python docs-venv/bin/python 'mkdocs==1.6.1' 'mkdocs-techdocs-core==1.7.1'
docs-venv/bin/mkdocs build --strict
```

Mermaid diagrams are plain fenced code blocks for `mkdocs`; rendering them in Backstage needs the TechDocs Mermaid add-on (not verified here).
