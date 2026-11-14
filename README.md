# user-service

## Purpose and place in the shop

A gRPC service for the shop's accounts: register, log in, resolve a session token, log out. It owns its own Postgres database and creates the schema with embedded migrations at start-up. Passwords are stored as bcrypt hashes; a session is an opaque random token of which only the SHA-256 is stored. It is demo grade (see [Known limits](#known-limits) and [`docs/security.md`](docs/security.md)): no email verification, no password reset, no roles. It calls no other service.

Context diagram and the main flow as sequence diagram: [`docs/architecture.md`](docs/architecture.md).

| | |
|---|---|
| Language / runtime | Go 1.26 (`go.mod`: `go 1.26.0`, `toolchain go1.26.10`), static binary, `CGO_ENABLED=0` |
| Image | built locally only: `platform-demo/user-service:local` (`Dockerfile`, user `10001:10001`) |
| Ports | gRPC `8086` (plaintext), HTTP `9090` (`/metrics`, scraped by Alloy; pprof only on its own `PPROF_ADDR` listener) |
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
| `BCRYPT_COST` | `11` | no | bcrypt cost, integer 4-16. Anything else makes the process exit at start-up. Below 10 a warning is logged; the tests use 4 |
| `SESSION_TTL` | `336h` | no | Session lifetime (Go duration, fixed, not sliding). Invalid or non-positive: the process exits |
| `LOGIN_RATE_LIMIT` | `5` | no | Login attempts per e-mail address and window, per process. `0` switches it off (warning). An unparsable value keeps the default and is logged |
| `LOGIN_RATE_WINDOW` | `1m` | no | Window of the login limit (Go duration) |
| `REGISTER_RATE_LIMIT` | `60` | no | Registrations per window for the whole process (a flood brake, not a per-client limit). `0` switches it off |
| `REGISTER_RATE_WINDOW` | `1m` | no | Window of the register limit |
| `HOSTNAME` | OS hostname | no | Used as `service.instance.id` (the pod name in Kubernetes) |
| `METRICS_PORT` | `9090` | no | Port of the Prometheus `/metrics` endpoint (scraped by Alloy). A value that is not a port falls back to `9090` with a warning |
| `PPROF_ADDR` | unset (no pprof) | no | `host:port` of a separate pprof listener, e.g. `127.0.0.1:6060`. Off unless set; never expose it. pprof is never served on the metrics port |
| `LOG_LEVEL` | `info` | no | `debug`, `info`, `warn` or `error`; anything else falls back to `info` with a warning |
| `SERVICE_VERSION` | build info, else `dev` | no | `service.version`, the `version` log field and the `build_info` label. Fallbacks in order: ldflags `-X <module>/internal/obs.Version=...` (and `.Commit=`, `.BuildTime=`), the VCS revision Go embeds in the binary (12 characters, `-dirty` if modified), `dev` |
| `DEPLOYMENT_ENVIRONMENT` | `local` | no | `deployment.environment` on every signal and the Pyroscope tag `environment` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | unset (telemetry off) | no | OTLP/**gRPC** endpoint for **traces, metrics and logs**, e.g. `http://alloy:4317`. Telemetry is on only if this is a valid absolute http(s) URL; empty or malformed means off (one warning, never a crash). The legacy `COLLECTOR_SERVICE_ADDR` and `ENABLE_TRACING` are no longer read |
| `OTEL_SDK_DISABLED` | unset | no | `true` (trimmed, case-insensitive) forces everything off: OTLP export and profiling, even if an endpoint or `PYROSCOPE_ADDR` is set |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `grpc` | no | Documented knob; only gRPC is implemented, the value is not read |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | SDK default (`parentbased_always_on`) | no | Standard OpenTelemetry sampler variables, read by the SDK (tested for `always_on` and `always_off`) |
| `OTEL_RESOURCE_ATTRIBUTES` | unset | no | Extra resource attributes (`k=v,...`); applied after the defaults, so they can override them |
| `OTEL_SERVICE_NAME` | `user-service` | no | `service.name`; keep the default, dashboards depend on it |
| `OTEL_METRIC_EXPORT_INTERVAL` | `15000` | no | Milliseconds between metric pushes (also accepts durations such as `15s`) |
| `PYROSCOPE_ADDR` | unset (profiling off) | no | Pyroscope URL (SDK push protocol, not OTLP). The only switch: no default address, `ENABLE_PROFILING=1` alone starts nothing |
| `ENABLE_PROFILING` | unset | no | `0` forces profiling off even with `PYROSCOPE_ADDR`; any other value changes nothing |

The OTLP exporters of the OpenTelemetry Go SDK also read their own standard `OTEL_EXPORTER_OTLP_*` variables (headers, TLS, timeout); they are not read by this service's code and were not tested. The sampler is the SDK's (`OTEL_TRACES_SAMPLER`, default `parentbased_always_on`).

Start-up: the gRPC server listens at once (health `NOT_SERVING`); a background loop runs the goose migrations (under goose's Postgres advisory lock), retrying every 2 s until the database is reachable. The service hashes one random password at start-up (the dummy for unknown e-mails), so start-up takes about as long as one login (about 0.2 s at cost 11, as measured by the author).

## Observability

| Signal | Implementation | Destination |
|---|---|---|
| **Traces** | OpenTelemetry (`otelgrpc` handlers), OTLP/gRPC exporter (batch processor, queue bounded at 2048), W3C TraceContext + Baggage propagated to every downstream call. Sampler from `OTEL_TRACES_SAMPLER` (SDK default `parentbased_always_on`) | Alloy `:4317` -> Tempo |
| **Metrics** | Prometheus default registry: `grpc_server_*` (go-grpc-prometheus with handling-time histogram), Go runtime and process metrics. Served as Prometheus text on `:9090/metrics` (`METRICS_PORT`) and, because the same registry is bridged into OpenTelemetry, also pushed over OTLP every 15 s (metric names identical). Plus `build_info{service,version,commit} 1` | Alloy scrapes `:9090` -> Mimir |
| **Logs** | `logrus`, one JSON object per line on stdout, plus an OTLP/gRPC hook (`otellogrus`) | stdout -> Alloy -> Loki; OTLP -> Alloy -> Loki |
| **Profiles** | Pyroscope Go SDK (CPU, alloc/in-use objects and space, goroutines, mutex, block), only with `PYROSCOPE_ADDR`; tags `service_name`, `version`, `environment` | Pyroscope |

**Telemetry contract** (the code is `internal/obs`, the same package in every Go service): telemetry is on only if `OTEL_EXPORTER_OTLP_ENDPOINT` is a valid absolute http(s) URL; `OTEL_SDK_DISABLED=true` forces everything off (profiling too); profiling only with `PYROSCOPE_ADDR`. Start never waits for the collector (the exporters connect in the background) and on SIGTERM the flush is cut off after 4 s, so a blackholed collector cannot hold the process (unit-tested with a listener that never answers: start and shutdown both return within the bound).

**Resource attributes** on every signal: `service.name` (`OTEL_SERVICE_NAME`, default `user-service`), `service.version` (`SERVICE_VERSION`, else build info, else `dev`), `service.namespace=everly`, `deployment.environment` (`DEPLOYMENT_ENVIRONMENT`, default `local`), `service.instance.id` (hostname). `OTEL_RESOURCE_ATTRIBUTES` is applied last.

**Log shape** (one shape for all services): `timestamp` (RFC 3339, UTC, milliseconds), `severity` (`debug`, `info`, `warning`, `error`), `message`, `service`, `version`, `trace_id` and `span_id` (hex, only while a valid span is active, never empty or all zeros), `request_id` (when the caller sent one), `error` (string, on errors), plus the call's own fields (for example `order_id`). Level from `LOG_LEVEL`. `trace_id`/`span_id` come from the context of the log call: handlers and clients log with `log.WithContext(ctx)`, the formatter reads the span from it. Calls without a context (startup, background work) have no ids. No card data, tokens or e-mail addresses are logged.

**Startup and version**: the first line is `message: "starting"` with `version`, `commit` and `environment`; `version`/`commit` come from `SERVICE_VERSION`, ldflags `-X <module>/internal/obs.Version=` / `.Commit=`, or the VCS data Go embeds (no Dockerfile change was made: without any of them a container image reports `version: "dev"`, `commit: "unknown"`). Panics are logged as one JSON line (`message: "panic"`, `error`) before the process dies; a startup error is one JSON error line, exit 1.

**pprof** is off. `PPROF_ADDR=127.0.0.1:6060` starts a separate listener; it is never on the metrics port.

**Graceful shutdown** (SIGTERM or SIGINT): health goes `NOT_SERVING`, `GracefulStop` with a 5 s cap (then `Stop`), then the telemetry flush runs, cut off after 4 s, the pool is closed: the process exits within 9 s even when the collector is blackholed. Run against a listener that never answers (OTLP and Pyroscope both pointed at it, database unreachable) the binary exited 0 about 4 s after SIGTERM (smoke run, not part of the unit tests; with a reachable database not re-measured).

**Not there yet** (standards section 6): spans for Postgres calls, DB pool stats and business counters (registrations, logins by outcome, rate-limit rejections); `build_info` is there. Logs contain user ids (`user_id` field), never e-mail addresses, passwords, tokens or hashes (a unit test and an integration test fail if a log line contains any of them; the new log-shape test checks the e-mail address, the password and the token again). Background work (the expired-session clean-up) has no request, so its lines carry no trace ids.

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
- **Shutdown:** on SIGTERM/SIGINT health goes `NOT_SERVING`, `GracefulStop` with a 5 s cap (then `Stop`), the pool is closed, telemetry is flushed (cut off after 4 s), the metrics listener stops, exit 0. At most 9 s in total (5 s drain + 4 s flush) even with a blackholed collector.
- **Scaling:** stateless except for Postgres, several replicas are possible (migrations take goose's advisory lock; registration is race-safe through the unique index). The **login and register rate limits are per process**: N replicas allow N times the limit and a restart resets them. bcrypt work is not capped: many concurrent logins use many cores. Multi-replica operation was not run.
- **Image:** Alpine runtime, user `10001:10001`, `EXPOSE 8086 9090`, about 12 MB when last built.
- Failure behaviour and runbook: [`docs/operations.md`](docs/operations.md).

## Known limits

- No email verification and no password reset: a lost password means a lost account. No multi-factor authentication, no account deletion, no listing or revoking of other sessions, no change of password or email.
- `Register` answers `ALREADY_EXISTS` for a known address (user enumeration accepted: the frontend contract needs that message). Only `Login` is hardened.
- The limiters are per process, there is no per-client or per-IP limit; the register limit is one global bucket; when 50,000 distinct keys are inside their window a new key is refused (fail closed).
- Plain gRPC without TLS or caller authentication: anyone who can reach the port can call it (in the local stack it is on the internal network only).
- No spans for database calls, no DB pool stats or business counters. The version/commit is logged at start and in `build_info`, but the image build sets no ldflags, so a container reports `dev` unless `SERVICE_VERSION` is set.
- Not run: OTLP exporters against a real collector and Pyroscope, arm64 build, `govulncheck`, more than one replica, any load or timing measurement.

## License

Apache License 2.0, see [LICENSE](LICENSE).
