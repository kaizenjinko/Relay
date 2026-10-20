# syntax=docker/dockerfile:1

# ── build ────────────────────────────────────────────────────────────────────
# The builder is discarded, so its size is irrelevant; only the runtime stage
# below ships.
FROM golang:1.23-alpine AS build

WORKDIR /src

# Dependencies first, so editing source does not re-download the module cache.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Static binary: with CGO off there is no libc to carry into the runtime stage.
#   -trimpath       drops build paths from the binary
#   -s -w           drops the symbol table and DWARF data
#   -buildvcs=false keeps .git out of the build, since it may be absent in CI
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -buildvcs=false \
        -ldflags='-s -w' \
        -o /out/relay .

# ── runtime ──────────────────────────────────────────────────────────────────
# scratch: no shell, no package manager, no CA bundle, no libc. The relay is a
# single static binary, so nothing else is required. This keeps the image under
# 7 MB total.
#
# No CA bundle means the relay cannot verify HTTPS upstreams. That is on
# purpose: this image is built to sit behind a wrapper that terminates TLS and
# points the routes at plain http:// upstreams. If you need the relay to reach
# an https:// upstream directly, add ca-certificates in the builder and copy
# the bundle to /etc/ssl/certs/ca-certificates.crt in this stage.
FROM scratch

COPY --from=build /out/relay /usr/local/bin/relay

# Route scripts are baked in. A deployment that mounts its own config over
# /etc/relay replaces them wholesale.
COPY --chown=10001:10001 scripts/ /etc/relay/scripts/
COPY --chown=10001:10001 deploy/relay.toml.example /etc/relay/relay.toml

# Matches the uid/gid created for systemd deployments, so a bind-mounted config
# owned by 10001 reads without loosening permissions.
USER 10001:10001

# Bind on all interfaces: inside a container 127.0.0.1 is unreachable from the
# host and from peers on the compose network.
EXPOSE 8787

# No HEALTHCHECK directive: scratch has no shell and the check is driven from
# outside (compose runs the relay's own -healthcheck flag). Declaring one here
# would require a second copy of the check logic in the image.
ENTRYPOINT ["/usr/local/bin/relay"]
CMD ["-config", "/etc/relay/relay.toml", "-listen", "0.0.0.0:8787"]
