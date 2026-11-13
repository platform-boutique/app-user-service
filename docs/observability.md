# Observability

What the service emits today (code read 2026-11-13; the compose observability profile was not run for this README):

| Signal | Today | Not there yet (standards section 6) |
|---|---|---|
| Traces | OTLP/gRPC, only when `OTEL_EXPORTER_OTLP_ENDPOINT` is set. A server span per inbound gRPC call (`otelgrpc`), W3C `traceparent` + `baggage` propagation, sampler always-on | spans for Postgres calls |
| Metrics | Prometheus on `:9090/metrics`: `grpc_server_*` (go-grpc-prometheus with handling-time histogram), Go runtime and process metrics. The same registry is pushed over OTLP when the endpoint is set | `build_info`, DB pool stats, business counters (registrations, logins by outcome, rate-limit rejections) |
| Logs | JSON on stdout, one object per line: `timestamp` (RFC3339Nano), `severity`, `message` plus fields. Mirrored over OTLP when the endpoint is set. Logs contain user ids, never e-mail addresses, passwords, tokens or hashes (a unit test and an integration test fail if a log line contains any of them). | `trace_id` / `span_id` on the stdout line, `service`, `version`, `LOG_LEVEL` |
| Profiles | Pyroscope push (CPU, alloc objects/space, inuse objects/space) and pprof on the metrics port, only with `PYROSCOPE_ADDR` | goroutine / mutex / block profiles, tags `version`, `environment` |

Resource attributes today: `service.name`, `service.instance.id` and the SDK's telemetry attributes (plus whatever `OTEL_RESOURCE_ATTRIBUTES` adds). `service.version`, `service.namespace` and `deployment.environment` are not set.

Example query (not run against a live backend): `sum by (grpc_method, grpc_code) (rate(grpc_server_handled_total{grpc_service="hipstershop.users.UserService"}[5m]))`.

## Switches

Telemetry is off unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set; `OTEL_SDK_DISABLED=true` forces it off; profiling needs `PYROSCOPE_ADDR`. The legacy `COLLECTOR_SERVICE_ADDR` still turns telemetry on in the current code and is to be removed. Details: [configuration](configuration.md).
