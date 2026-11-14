# --- Stage 1: Build ---
# BuildKit sets TARGETOS/TARGETARCH from --platform, so the same Dockerfile builds amd64 and arm64.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
ARG TARGETOS TARGETARCH
# Build identity, linked into internal/obs (read by the service at start)
ARG SERVICE_VERSION=dev
ARG SERVICE_COMMIT=unknown

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# declared late on purpose: a new value must not invalidate the dependency layers above (empty = build time not set)
ARG BUILD_TIME
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags="-s -w -X user-service/internal/obs.Version=${SERVICE_VERSION} -X user-service/internal/obs.Commit=${SERVICE_COMMIT} -X user-service/internal/obs.BuildTime=${BUILD_TIME}" \
    -o /user-service \
    ./cmd/server

# --- Stage 2: Run ---
FROM alpine:3.24
ARG SERVICE_VERSION=dev
ARG SERVICE_COMMIT=unknown

LABEL org.opencontainers.image.title="user-service" \
      org.opencontainers.image.source="https://github.com/platform-boutique/app-user-service" \
      org.opencontainers.image.revision="${SERVICE_COMMIT}" \
      org.opencontainers.image.version="${SERVICE_VERSION}" \
      org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /app

COPY --from=builder /user-service .

ENV GOTRACEBACK=single \
    SERVICE_VERSION=${SERVICE_VERSION} \
    SERVICE_COMMIT=${SERVICE_COMMIT}

# numeric non-root user (no entry in /etc/passwd needed); matches the charts' runAsUser
USER 10001:10001

EXPOSE 8086 9090

HEALTHCHECK --interval=5s --timeout=4s --start-period=10s --retries=3 CMD ["/app/user-service", "healthcheck"]

ENTRYPOINT ["/app/user-service"]
