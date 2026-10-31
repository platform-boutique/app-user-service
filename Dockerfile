# --- Stage 1: Build ---
# BuildKit sets TARGETOS/TARGETARCH from --platform, so the same Dockerfile builds amd64 and arm64.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
ARG TARGETOS TARGETARCH

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -trimpath -ldflags="-s -w" \
    -o /user-service \
    ./cmd/server

# --- Stage 2: Run ---
FROM alpine:3.24

WORKDIR /app

COPY --from=builder /user-service .

ENV GOTRACEBACK=single

# numeric non-root user (no entry in /etc/passwd needed); matches the charts' runAsUser
USER 10001:10001

EXPOSE 8086 9090

HEALTHCHECK --interval=5s --timeout=4s --start-period=10s --retries=3 CMD ["/app/user-service", "healthcheck"]

ENTRYPOINT ["/app/user-service"]
