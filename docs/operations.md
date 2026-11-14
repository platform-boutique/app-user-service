# Operations

## Health

- `grpc.health.v1.Health/Check` on the gRPC port (`8086`), for the empty service name and for `hipstershop.users.UserService`.
- `NOT_SERVING` from start until the migrations are done, and again whenever the Postgres ping fails (checked every 5 s). While not ready the handlers answer `UNAVAILABLE` (`database not ready`).
- Container probe: `/app/user-service healthcheck` calls the local health service (`127.0.0.1:$PORT`) and exits 0 only on `SERVING` (Docker `HEALTHCHECK`, interval 5 s, start period 10 s).

```bash
grpcurl -plaintext localhost:8086 grpc.health.v1.Health/Check
```

## Shutdown

SIGTERM or SIGINT: health goes `NOT_SERVING` so probes fail first; `GracefulStop` with a 5 s cap, then `Stop`; the pool is closed; telemetry and profiles are flushed (cut off after 4 s) and the metrics listener stops; exit 0. At most 9 s in total, inside the 10 s of CODE-04.

Measured: with OTLP and Pyroscope pointed at a listener that never answers (and the database unreachable) the binary exited 0 about 4.0 s after SIGTERM. Earlier, by the author: `docker stop` exit code 0 (image smoke run). Not measured again in a container or with a reachable database.

## Scaling and replicas

- Postgres is the only state. Several replicas are possible in principle: migrations take goose's advisory lock, registration uniqueness is the unique index on `lower(email)`.
- The login and register rate limits are **per process**: a restart resets them, N replicas allow N times the limit.
- bcrypt work is not capped: many concurrent logins use many cores (cost 11 takes about 0.2 s per hash on the author's machine).
- Running more than one replica was not tested.
- Resource use in the local stack: `mem_limit: 128m` (workspace `local/compose.yaml`).

## Failure behaviour

| Failure | Behaviour |
|---|---|
| Postgres down at start | server listens, health `NOT_SERVING`, migration retried every 2 s |
| Postgres goes away while running | health `NOT_SERVING` within about 5 s, handlers answer `UNAVAILABLE` (`database unavailable`, no SQL details), `SERVING` again after it returns |
| Invalid `BCRYPT_COST`, `SESSION_TTL` | process exits at start-up (a security setting must not fall back silently) |
| Handler panic | turned into `INTERNAL` by a recovery interceptor; the process keeps running |
| Collector unreachable | telemetry export fails in the background (bounded queues, spans and logs are dropped); start-up does not depend on it; the flush on SIGTERM is cut off after 4 s |

## Runbook

| Symptom | Check |
|---|---|
| Health stays `NOT_SERVING` | logs for `database not ready (attempt N)`: wrong `DATABASE_URL`, network, credentials |
| Users get `RESOURCE_EXHAUSTED` on login | the per e-mail limit (`LOGIN_RATE_LIMIT` per `LOGIN_RATE_WINDOW`); it clears one window after the last counted attempt |
| Registrations refused | the global register limit (`REGISTER_RATE_LIMIT`) or 50,000 live limiter keys |
| Sessions end early or late | `SESSION_TTL` (fixed, not sliding); expired rows are deleted lazily |
| Process exits at start | `DATABASE_URL is not set`, or an invalid `BCRYPT_COST` / `SESSION_TTL` / rate-limit value |

There is no dashboard or alert for this service yet.
