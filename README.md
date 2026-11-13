# user-service

## Purpose and place in the shop

A gRPC service for the shop's accounts: register, log in, resolve a session token, log out. It owns its own Postgres database and creates the schema with embedded migrations at start-up. Passwords are stored as bcrypt hashes; a session is an opaque random token of which only the SHA-256 is stored. It is demo grade (see [Known limits](#known-limits) and [`docs/security.md`](docs/security.md)): no email verification, no password reset, no roles. It calls no other service.

Context diagram and the main flow as sequence diagram: [`docs/architecture.md`](docs/architecture.md).

| | |
|---|---|
| Language / runtime | Go 1.26 (`go.mod`: `go 1.26.0`, `toolchain go1.26.10`), static binary, `CGO_ENABLED=0` |
| Image | built locally only: `platform-demo/user-service:local` (`Dockerfile`, user `10001:10001`) |
| Ports | gRPC `8086` (plaintext), HTTP `9090` (`/metrics`, pprof only with profiling) |
| Health | `grpc.health.v1.Health/Check` on `8086`, `NOT_SERVING` until the migrations are done and whenever the database does not answer a ping (every 5 s) |
| State | Postgres (`users`, `sessions`) via `DATABASE_URL` |
| Depends on | its own Postgres (required); OTLP collector and Pyroscope (optional) |
| Called by | the frontend (accounts pages, header, cart/order identity; see `local/compose.yaml` and the frontend repo) |
| gRPC reflection | enabled |

## Status

No CI pipeline, GitHub repository, Helm chart or Backstage registration exists yet; the service is built and run locally only. Status badges are added when the repo has a pipeline.

## Quick start

In the platform-demo workspace (this repo is built from `apps/user-service` by `local/compose.yaml`; the compose commands were not run for this README):

```bash
local/up.sh                      # builds all images and starts the shop, http://localhost:8080
SKIP_BUILD=1 local/up.sh         # reuse images
local/rebuild.sh user-service  # rebuild and recreate only this service
```

Run the tests (run on 2026-11-13, see Develop):

```bash
GOTOOLCHAIN=auto go vet ./... && GOTOOLCHAIN=auto go test -race -count=1 ./...
```

## API

Package `hipstershop.users`, service `UserService` ([`proto/users.proto`](proto/users.proto)). Full rules, error messages and `grpcurl` examples: [`docs/api.md`](docs/api.md).

| Method | Request | Behaviour | Error codes |
|---|---|---|---|
| `Register` | `email`, `password`, `name` | Creates the account and signs it in (`user`, `token`, `expires_at`) | `INVALID_ARGUMENT`, `ALREADY_EXISTS`, `RESOURCE_EXHAUSTED`, `UNAVAILABLE`, `INTERNAL` |
| `Login` | `email`, `password` | Checks the credentials and opens a new session; several live sessions per user | `UNAUTHENTICATED` (one message `wrong email or password` for every credential failure), `RESOURCE_EXHAUSTED`, `INVALID_ARGUMENT` (empty field), `UNAVAILABLE`, `INTERNAL` |
| `GetSession` | `token` | Returns the `User` of a live session | `UNAUTHENTICATED` (unknown, malformed, expired or logged-out token), `UNAVAILABLE`, `INTERNAL` |
| `Logout` | `token` | Revokes that one session; idempotent (unknown or already revoked token is `OK`) | `UNAVAILABLE`, `INTERNAL` |

Field rules: `email` trimmed and lower-cased, 3-254 characters, plain address, unique ignoring case; `password` 8-72 **bytes** (bcrypt limit; longer is refused, not truncated); `name` 1-100 characters; `token` is 32 random bytes as base64url (43 characters), `expires_at` is login time plus `SESSION_TTL`. All handlers answer `UNAVAILABLE` until the migrations have run; a handler panic becomes `INTERNAL`.

## Configuration

Every variable read by the code (`grep -rn 'Getenv\|getenv'` over `cmd/` and `internal/`, checked 2026-11-13). All configuration is by environment; there is no config file.

| Variable | Default | Required | Meaning |
|---|---|---|---|
| `DATABASE_URL` | none | **yes** | Postgres URL, e.g. `postgres://users:users@user-db:5432/users?sslmode=disable`. The process logs an error and exits 1 if it is unset. A 5 s connect timeout is added unless the URL sets one |
| `PORT` | `8086` | no | gRPC listen port. The `healthcheck` subcommand reads it too |
| `METRICS_PORT` | `9090` | no | HTTP port for `/metrics` (and pprof when profiling is on) |
| `BCRYPT_COST` | `11` | no | bcrypt cost, integer 4-16. Anything else makes the process exit at start-up. Below 10 a warning is logged; the tests use 4 |
| `SESSION_TTL` | `336h` | no | Session lifetime (Go duration, fixed, not sliding). Invalid or non-positive: the process exits |
| `LOGIN_RATE_LIMIT` | `5` | no | Login attempts per e-mail address and window, per process. `0` switches it off (warning). An unparsable value keeps the default and is logged |
| `LOGIN_RATE_WINDOW` | `1m` | no | Window of the login limit (Go duration) |
| `REGISTER_RATE_LIMIT` | `60` | no | Registrations per window for the whole process (a flood brake, not a per-client limit). `0` switches it off |
| `REGISTER_RATE_WINDOW` | `1m` | no | Window of the register limit |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | unset (telemetry off) | no | OTLP/**gRPC** endpoint URL with scheme for traces, metrics and logs |
| `OTEL_SDK_DISABLED` | unset | no | `true` forces telemetry off even with an endpoint |
| `OTEL_SERVICE_NAME` | `user-service` | no | `service.name` resource attribute |
| `OTEL_METRIC_EXPORT_INTERVAL` | `15000` | no | Metric push interval: a number of milliseconds, or a Go duration (`15s`). Invalid values fall back to 15 s |
| `PYROSCOPE_ADDR` | unset (profiling off) | no | Pyroscope server URL. Profiling (push and pprof endpoints) is on only if this is set |
| `ENABLE_PROFILING` | unset | no | `0` forces profiling off even with `PYROSCOPE_ADDR` |
| `ENABLE_TRACING` | unset | no | Legacy: `0` forces telemetry off |
| `COLLECTOR_SERVICE_ADDR` | unset | no | **Legacy, to be removed** (owner decision 2026-11-13): `host:port` of a collector; turns telemetry on without `OTEL_EXPORTER_OTLP_ENDPOINT` |
| `HOSTNAME` | OS hostname | no | Used as `service.instance.id` (the pod name in Kubernetes) |
| `OTEL_RESOURCE_ATTRIBUTES` | unset | no | Read by the OTel SDK resource detector (`resource.WithFromEnv`), not by this code directly |

The OTLP exporters of the OpenTelemetry Go SDK also read their own standard `OTEL_EXPORTER_OTLP_*` variables (headers, TLS, timeout); they are not read by this service's code and were not tested. `OTEL_TRACES_SAMPLER` is **not** honoured: the sampler is fixed to always-on (`otlp.go`). `LOG_LEVEL` does not exist: the log level is logrus' default (info).

Start-up: the gRPC server listens at once (health `NOT_SERVING`); a background loop runs the goose migrations (under goose's Postgres advisory lock), retrying every 2 s until the database is reachable. The service hashes one random password at start-up (the dummy for unknown e-mails), so start-up takes about as long as one login (about 0.2 s at cost 11, as measured by the author).

## Observability

What the service emits today (code read 2026-11-13; the compose observability profile was not run for this README):

| Signal | Today | Not there yet (standards section 6) |
|---|---|---|
| Traces | OTLP/gRPC, only when `OTEL_EXPORTER_OTLP_ENDPOINT` is set. A server span per inbound gRPC call (`otelgrpc`), W3C `traceparent` + `baggage` propagation, sampler always-on | spans for Postgres calls |
| Metrics | Prometheus on `:9090/metrics`: `grpc_server_*` (go-grpc-prometheus with handling-time histogram), Go runtime and process metrics. The same registry is pushed over OTLP when the endpoint is set | `build_info`, DB pool stats, business counters (registrations, logins by outcome, rate-limit rejections) |
| Logs | JSON on stdout, one object per line: `timestamp` (RFC3339Nano), `severity`, `message` plus fields. Mirrored over OTLP when the endpoint is set. Logs contain user ids, never e-mail addresses, passwords, tokens or hashes (a unit test and an integration test fail if a log line contains any of them). | `trace_id` / `span_id` on the stdout line, `service`, `version`, `LOG_LEVEL` |
| Profiles | Pyroscope push (CPU, alloc objects/space, inuse objects/space) and pprof on the metrics port, only with `PYROSCOPE_ADDR` | goroutine / mutex / block profiles, tags `version`, `environment` |

Resource attributes today: `service.name`, `service.instance.id` and the SDK's telemetry attributes (plus whatever `OTEL_RESOURCE_ATTRIBUTES` adds). `service.version`, `service.namespace` and `deployment.environment` are not set.

Example query (not run against a live backend): `sum by (grpc_method, grpc_code) (rate(grpc_server_handled_total{grpc_service="hipstershop.users.UserService"}[5m]))`.

## Data

Postgres, database owned by this service. `users` (`id uuid`, `email`, `name`, `password_hash`, `created_at`) with a unique index on `lower(email)`; `sessions` (`token_hash bytea` primary key, 32 bytes = SHA-256 of the token; `user_id` with `ON DELETE CASCADE`; `created_at`, `expires_at`, `revoked_at`). Check constraints repeat the length limits. The unique index (not a pre-check) makes concurrent registrations of one address race-safe. Expired sessions are deleted opportunistically by a login or registration (at most once a minute per process); revoked sessions stay until they expire.

Migrations: [goose](https://github.com/pressly/goose) SQL files in `internal/users/migrations/`, embedded in the binary and applied at start-up under goose's advisory lock, forward-only (`00001_create_users.sql`). No seed data. Demo database in compose: `postgres:17-alpine`, user/password `users` (demo values only, they live in `local/compose.yaml`).

## Develop

Requirements: Go 1.26 (`GOTOOLCHAIN=auto` fetches it), Docker for integration tests, [`grpcurl`](https://github.com/fullstorydev/grpcurl) for manual calls. Layout: `cmd/server/` (entry point, config, telemetry, health), `internal/users/` (service, validation, tokens, rate limiter, store, migrations), `proto/`, `tests/integration/`.

| Step | Command | Result on 2026-11-13 |
|---|---|---|
| Vet + unit tests | `go vet ./... && go test -race -count=1 ./...` | run, passed |
| Format | `gofmt -l .` | run, no files listed |
| Lint | `golangci-lint run ./...` (config `.golangci.yaml`, v2 default linters) | run with golangci-lint v2.11.4, 0 issues; also with `--build-tags integration`, 0 issues. CI pins v2.14.0: not run |
| Integration (Postgres) | see below | not run today (earlier runs by the author are in `docs/development.md`) |
| Vulnerability / security scan | `govulncheck ./...`, `gosec -exclude-generated ./...` | not run today |
| Image | `docker build -t platform-demo/user-service:local .` | not run today |
| Docs | `mkdocs build --strict` (mkdocs 1.6.1 + mkdocs-techdocs-core 1.7.1, Python 3.12 venv made with uv) | run, passed |

Integration tests (build tag `integration`, each test creates and drops its own database):

```bash
docker run -d --rm --name usvc-test-pg --memory 256m -e POSTGRES_USER=users -e POSTGRES_PASSWORD=users \
  -p 127.0.0.1:55436:5432 postgres:17-alpine
TEST_DATABASE_URL='postgres://users:users@127.0.0.1:55436/postgres?sslmode=disable' \
  go test -race -tags integration ./tests/integration/
docker rm -f usvc-test-pg
```

Regenerate the Go code after editing the proto: `protoc -I proto --go_out=proto --go_opt=paths=source_relative --go-grpc_out=proto --go-grpc_opt=paths=source_relative proto/users.proto` (protoc 3.21.12, protoc-gen-go 1.36.11, protoc-gen-go-grpc 1.6.1; not run today). Proto changes are additive only, server first.

## Operate

- **Health:** `grpc.health.v1` on the service port, for the empty service name and `hipstershop.users.UserService`. `NOT_SERVING` until the migrations are done, and again whenever the Postgres ping fails (every 5 s); handlers answer `UNAVAILABLE` meanwhile. The image `HEALTHCHECK` runs `/app/user-service healthcheck` (exit 0 only on `SERVING`).
- **Shutdown:** on SIGTERM/SIGINT health goes `NOT_SERVING`, `GracefulStop` with a 10 s cap (then `Stop`), the metrics server stops (5 s), telemetry is flushed (5 s), the pool is closed, exit 0.
- **Scaling:** stateless except for Postgres, several replicas are possible (migrations take goose's advisory lock; registration is race-safe through the unique index). The **login and register rate limits are per process**: N replicas allow N times the limit and a restart resets them. bcrypt work is not capped: many concurrent logins use many cores. Multi-replica operation was not run.
- **Image:** Alpine runtime, user `10001:10001`, `EXPOSE 8086 9090`, about 12 MB when last built.
- Failure behaviour and runbook: [`docs/operations.md`](docs/operations.md).

## Known limits

- No email verification and no password reset: a lost password means a lost account. No multi-factor authentication, no account deletion, no listing or revoking of other sessions, no change of password or email.
- `Register` answers `ALREADY_EXISTS` for a known address (user enumeration accepted: the frontend contract needs that message). Only `Login` is hardened.
- The limiters are per process, there is no per-client or per-IP limit; the register limit is one global bucket; when 50,000 distinct keys are inside their window a new key is refused (fail closed).
- Plain gRPC without TLS or caller authentication: anyone who can reach the port can call it (in the local stack it is on the internal network only).
- No spans for database calls; no `trace_id` in logs; no version/commit exposed.
- Not run: OTLP exporters against a real collector and Pyroscope, arm64 build, `govulncheck`, more than one replica, any load or timing measurement. The legacy `COLLECTOR_SERVICE_ADDR` is still read (to be removed).

## License

Apache License 2.0, see [LICENSE](LICENSE).
