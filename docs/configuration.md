# Configuration

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
