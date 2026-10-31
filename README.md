# user-service

A gRPC service for shop accounts: register, log in, resolve a session token, log out. It owns its own Postgres database and creates the schema with embedded migrations at start-up. Passwords are stored as bcrypt hashes; a session is an opaque random token of which only the SHA-256 is stored. It is demo grade (see [Security notes](#security-notes)): no email verification, no password reset, no roles.

## At a glance

| | |
|---|---|
| **Language / runtime** | Go 1.26 (static binary, `CGO_ENABLED=0`) |
| **Image** | built locally only: `user-service:local` (about 12 MB) |
| **Protocol / port** | gRPC (HTTP/2, plaintext) on `8086` |
| **Metrics** | Prometheus `/metrics` on `9090`; pushed over OTLP/gRPC only when an endpoint is set |
| **Health** | Standard `grpc.health.v1.Health/Check` on `8086`: `NOT_SERVING` until the migrations are done and whenever the database does not answer a ping (checked every 5 s) |
| **State** | Postgres (`users`, `sessions`), via `DATABASE_URL` |
| **Depends on** | Its own Postgres (optional: OTLP collector, Pyroscope) |
| **Called by** | the frontend (accounts pages, header, cart/order identity); nothing else |
| **gRPC reflection** | Enabled: `grpcurl` works without `-proto` |

## API

Package `hipstershop.users`, service `UserService` ([`proto/users.proto`](proto/users.proto), copied verbatim from the slice-5 contract). The package is a sub-package of `hipstershop` on purpose, like `hipstershop.orders`: a client that also compiles `demo.proto` gets no name clashes.

| Method | Request | Behaviour | Error codes |
|---|---|---|---|
| `Register` | `email`, `password`, `name` | Creates the account and signs it in (same response as `Login`: `user`, `token`, `expires_at`). | `INVALID_ARGUMENT` (message starts with the field name, e.g. `password must be 8-72 bytes (got 5)`), `ALREADY_EXISTS` (`an account with this email already exists`), `RESOURCE_EXHAUSTED`, `UNAVAILABLE`, `INTERNAL` |
| `Login` | `email`, `password` | Checks the credentials and opens a new session. Several sessions per user can be live at once (one per browser). | `UNAUTHENTICATED` with the ONE message `wrong email or password` for an unknown email, a wrong password and a password that cannot be right (over 72 bytes); `RESOURCE_EXHAUSTED` (`too many attempts, try again in N seconds`); `INVALID_ARGUMENT` only when email or password is empty; `UNAVAILABLE`, `INTERNAL` |
| `GetSession` | `token` | Returns the `User` of a live session. | `UNAUTHENTICATED` (`invalid or expired session`) for an unknown, malformed, expired or logged-out token; `UNAVAILABLE`, `INTERNAL` |
| `Logout` | `token` | Revokes that one session. Idempotent: an unknown, malformed, empty or already revoked token is `OK` and changes nothing (a second call does not move `revoked_at`). | `UNAVAILABLE`, `INTERNAL` |

All handlers answer `UNAVAILABLE` (`database not ready`) until the migrations have run; a database outage later gives `UNAVAILABLE` (`database unavailable`) without SQL details (those are logged); a handler panic becomes `INTERNAL` through a recovery interceptor.

Field rules:

- `email`: trimmed and lower-cased by the service; 3-254 characters; a plain address (`net/mail`, no display name, no `<...>`, no control characters). Unique ignoring case.
- `password`: 8-72 **bytes** (bcrypt ignores everything after byte 72, so a longer password is refused instead of silently truncated); not trimmed, not changed. At `Login` a password over 72 bytes is just a failed login.
- `name`: trimmed; 1-100 characters; no control characters.
- `id` is a UUID v4; `token` is 32 random bytes from `crypto/rand` as base64url without padding (43 characters), returned only by `Register`/`Login`; `expires_at` is login time plus `SESSION_TTL` (default 14 days, fixed, not sliding).

Examples (against the container, see below):

```bash
grpcurl -plaintext -d '{"email":"ada@example.com","password":"correct horse","name":"Ada"}' localhost:8086 hipstershop.users.UserService/Register
grpcurl -plaintext -d '{"email":"ADA@example.com","password":"correct horse"}' localhost:8086 hipstershop.users.UserService/Login
grpcurl -plaintext -d '{"token":"<token>"}' localhost:8086 hipstershop.users.UserService/GetSession
grpcurl -plaintext -d '{"token":"<token>"}' localhost:8086 hipstershop.users.UserService/Logout
```

## Configuration

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | **required** | Postgres URL, e.g. `postgres://users:users@user-db:5432/users?sslmode=disable`. The process exits if it is unset. A 5 s connect timeout is added unless the URL sets one |
| `PORT` | `8086` | gRPC listen port |
| `METRICS_PORT` | `9090` | Prometheus (and, with profiling, pprof) HTTP port |
| `BCRYPT_COST` | `11` | bcrypt cost, integer 4-16. Anything else makes the process exit at start-up (a security setting must not fall back silently). Below 10 a warning is logged; the unit and integration tests use 4 |
| `SESSION_TTL` | `336h` | Session lifetime (Go duration). Invalid or non-positive: the process exits |
| `LOGIN_RATE_LIMIT` | `5` | Login attempts per e-mail address and window, per process. `0` switches it off (logged as a warning). An unparsable value keeps the default and is logged |
| `LOGIN_RATE_WINDOW` | `1m` | Window of the login limit (Go duration) |
| `REGISTER_RATE_LIMIT` | `60` | Registrations per window for the whole process (a flood brake, not a per-client limit). `0` switches it off |
| `REGISTER_RATE_WINDOW` | `1m` | Window of the register limit |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | unset (telemetry off) | OTLP/**gRPC** endpoint for traces, metrics and logs (scheme required). Legacy fallback: `COLLECTOR_SERVICE_ADDR` (`host:port`). Telemetry is on only if one of them is set and non-empty |
| `OTEL_SDK_DISABLED` | - | `true` forces telemetry off even with an endpoint (the legacy `ENABLE_TRACING=0` does the same) |
| `OTEL_METRIC_EXPORT_INTERVAL` | `15000` | Milliseconds between metric pushes |
| `OTEL_SERVICE_NAME` | `user-service` | `service.name` |
| `PYROSCOPE_ADDR` | unset (profiling off) | Profiling (Pyroscope push **and** the pprof endpoints on `METRICS_PORT`) is on only if this is set and `ENABLE_PROFILING` is not `0` |
| `ENABLE_PROFILING` | - | `0` forces profiling off |

Logs are JSON on stdout (logrus, fields `timestamp`, `severity`, `message`). Start-up: the gRPC server listens at once (health `NOT_SERVING`); a background loop runs the goose migrations (under goose's Postgres session advisory lock, so replicas starting together apply each migration once), retrying every 2 s until the database is reachable. SIGTERM/SIGINT: health goes `NOT_SERVING`, `GracefulStop` (10 s cap), then the pool is closed and the process exits 0. The service hashes one random password at start-up (the dummy for unknown e-mails), so start-up takes about as long as one login (about 0.2 s at cost 11).

## Security notes

What it does:

- **Passwords**: bcrypt at `BCRYPT_COST`, random salt per hash, never logged, never returned. Only the hash is in the database.
- **Sessions**: the token is 256 random bits; the database stores only `SHA-256(token)` (plus `user_id`, `created_at`, `expires_at`, `revoked_at`), so a leaked table or backup cannot be replayed as cookies. A fast hash is enough because there is nothing to brute-force. `GetSession` checks expiry and revocation in the query; a token that is not 43 base64url characters is refused without a database call.
- **No user enumeration at login**: an unknown address, a wrong password, a malformed address and a password over 72 bytes all take the same path and give the same code and message. For an unknown address the service compares against a dummy bcrypt hash made at the configured cost, so a failed login costs exactly one bcrypt comparison either way (a unit test counts the comparisons for every failure case; response times were not measured). If `BCRYPT_COST` is changed later, accounts hashed at the old cost still compare at the old cost.
- **Login rate limit**: sliding window per normalised e-mail address (default 5 per minute), in the memory of one process (same style as the review-service limiter), applied to known and unknown addresses alike. The attempt is counted before the password is checked, so parallel guesses cannot all slip through while bcrypt runs. A success resets the count. Refused attempts are not counted, so a block ends one window after the last counted attempt: someone who keeps failing for an address cannot lock its owner out for good, only for up to one window at a time.
- **Race-safe registration**: uniqueness is the unique index on `lower(email)`, not a pre-check. A violation maps to `ALREADY_EXISTS`; of N concurrent registrations of one address exactly one wins (tested with 30 goroutines on Postgres, mixed case).
- **Expired sessions** are deleted opportunistically: a login or registration starts a background `DELETE ... WHERE expires_at <= now`, at most once a minute per process. No cron. Revoked sessions stay until they expire.
- **Errors** never contain SQL text, the e-mail or the password; logs contain user ids, never e-mail addresses, passwords, tokens, hashes (a unit test and an integration test fail if a log line contains any of them, with a passing control that proves they were looking at real output).

What it does not do (accepted for the demo, say so before using it for anything real):

- **No email verification and no password reset.** A lost password means a lost account.
- **User enumeration on `Register` is accepted**: `ALREADY_EXISTS` tells the caller that an address has an account. The frontend contract needs that message. Only `Login` is hardened.
- The limiters are **per process**: with N replicas the effective limit is N times higher and a restart resets them. There is no per-client or per-IP limit (the frontend is the only caller and the service sees one address). The register limit is one global bucket, so it protects the CPU and the table from floods but also lets a flood block other registrations for a minute. When 50,000 distinct keys are inside their window a new key is refused (fail closed), which would block logins of addresses not seen before; a flood of 50,000 distinct addresses in one minute is the price.
- bcrypt work is not capped: many concurrent logins use many cores.
- No multi-factor authentication, no account deletion, no listing or revoking of other sessions, no change of password or email.
- Plain gRPC without TLS or authentication of the caller: anyone who can reach the port can call it (in the local stack it is on the internal network only).
- A failed `Register` after the bcrypt step leaves nothing behind (the account and its first session are one transaction); `Login` after a successful password check and a failed session insert returns an error and the user logs in again.

## Storage

`users` (`id uuid`, `email`, `name`, `password_hash`, `created_at`) with a unique index on `lower(email)`, and `sessions` (`token_hash bytea` primary key, 32 bytes; `user_id` with `ON DELETE CASCADE`; `created_at`, `expires_at`, `revoked_at`). Check constraints repeat the length limits. One goose migration, embedded in the binary.

## Container image

Multi-stage [`Dockerfile`](Dockerfile) (same as order-service): `golang:1.26-alpine` builder (`--platform=$BUILDPLATFORM`, `GOARCH=$TARGETARCH`), `alpine:3.24` runtime with the single binary at `/app/user-service`.

- **User:** `10001:10001` (numeric, non-root).
- **HEALTHCHECK:** `/app/user-service healthcheck` calls the gRPC health service on `127.0.0.1:$PORT` and exits 0 only on `SERVING`.
- **EXPOSE:** `8086`, `9090`. Built locally on amd64 (`docker build -t user-service:local .`); arm64 was not built.

## Not deployed anywhere yet

No Helm chart, pipeline, Backstage entry or GitHub repository exists, and the service is not in the local compose stack yet (the integrator wires `user-db` + `user-service`); it was only built and run on its own.

## Running locally

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

## Project structure

```
├── cmd/server/
│   ├── main.go            # entrypoint: telemetry, profiling, gRPC server, DB watcher, graceful shutdown
│   ├── config.go          # BCRYPT_COST, SESSION_TTL, rate limit settings
│   ├── otlp.go            # OTLP traces/metrics/logs (same as order-service)
│   └── healthcheck.go     # `user-service healthcheck` for the container HEALTHCHECK
├── internal/users/
│   ├── service.go         # gRPC handlers, login failure path, error mapping, session clean-up
│   ├── validate.go        # email / password / name rules
│   ├── token.go           # token generation, SHA-256 hash, shape check
│   ├── ratelimit.go       # sliding-window limiter (as in review-service) + env parsing
│   ├── store.go           # Postgres store (pgx/v5)
│   ├── migrate.go         # goose migrations, Setup
│   └── migrations/        # embedded SQL migrations
├── proto/                 # users.proto + generated Go
├── tests/integration/     # tests against a real Postgres (build tag `integration`)
└── Dockerfile
```

## Dependencies

Same pins as order-service, plus `golang.org/x/crypto v0.58.0` (bcrypt; released 2026-10-09) and `github.com/google/uuid v1.6.0` (user ids; was already an indirect dependency).
