# Configuration

Every variable read by the code (`grep -rn 'Getenv\|getenv'` over `cmd/` and `internal/`, checked 2026-11-22). All configuration is by environment; there is no config file.

| Variable | Default | Required | Meaning |
|---|---|---|---|
| `DATABASE_URL` | none | **yes** | Postgres URL, e.g. `postgres://users:users@user-db:5432/users?sslmode=disable`. The process logs an error and exits 1 if it is unset. A 5 s connect timeout is added unless the URL sets one |
| `PORT` | `8086` | no | gRPC listen port. The `healthcheck` subcommand reads it too |
| `BCRYPT_COST` | `11` | no | bcrypt cost, integer 4-16. Anything else makes the process exit at start-up. Below 10 a warning is logged; the tests use 4. Cost 16 takes about 6 s per hash on the development host and bcrypt ignores `REQUEST_TIMEOUT` (see Known limits) |
| `SESSION_TTL` | `336h` | no | Session lifetime (Go duration, fixed, not sliding). Invalid or non-positive: the process exits |
| `LOGIN_RATE_LIMIT` | `5` | no | Login attempts per e-mail address and window, per process. `0` switches it off (warning). An unparsable value keeps the default and is logged |
| `LOGIN_RATE_WINDOW` | `1m` | no | Window of the login limit (Go duration) |
| `REGISTER_RATE_LIMIT` | `60` | no | Registrations per window for the whole process (a flood brake, not a per-client limit). `0` switches it off |
| `REGISTER_RATE_WINDOW` | `1m` | no | Window of the register limit |
| `REQUEST_TIMEOUT` | `5s` | no | Server-side bound of every unary call (Go duration, at least `100ms`; `0` switches the bound off). A call without a deadline gets this one, a longer caller deadline is shortened to it; the context reaches every database query. A value that does not parse keeps `5s` with a warning |
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
