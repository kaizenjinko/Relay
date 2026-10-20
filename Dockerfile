# syntax=docker/dockerfile:1

# ── build ───────────────────────────────────────────────────────────────────
FROM golang:1.23-alpine AS build

WORKDIR /src

# Dependencies first so a source-only change reuses this layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# -trimpath keeps local paths out of the binary; -s -w drops the symbol table.
# CGO is off so the result runs on a scratch-style base with no libc.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION:-dev}" \
    -o /out/relay .

# ── runtime ─────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

# Runs as uid 65532; nothing in the image is writable, so a compromise cannot
# persist. The config and scripts are mounted read-only at runtime.
COPY --from=build /out/relay /relay

USER nonroot:nonroot

EXPOSE 8787

# distroless has no shell, so the healthcheck is the relay's own /healthz.
HEALTHCHECK --interval=10s --timeout=2s --start-period=3s --retries=3 \
    CMD ["/relay", "-healthcheck", "-listen", "127.0.0.1:8787"]

ENTRYPOINT ["/relay"]
CMD ["-config", "/etc/relay/relay.toml"]
