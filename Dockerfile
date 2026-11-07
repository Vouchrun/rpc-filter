# Build stage: static binary, no cgo, no external modules.
FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/rpc-filter .

# Runtime stage: scratch. The binary is static, so there is nothing else to
# ship, and there is no shell for an attacker to use if it is ever compromised.
FROM scratch

COPY --from=build /out/rpc-filter /rpc-filter

# Bind to 127.0.0.1 only; the proxy must never be reachable except via Caddy.
USER 65534:65534

HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
	CMD ["/rpc-filter", "-healthcheck"]

ENTRYPOINT ["/rpc-filter"]
