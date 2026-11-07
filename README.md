# rpc-filter

A method-aware JSON-RPC proxy that sits between Caddy and geth.

## Why this exists

geth's `--http.api` is **namespace-granular and all-or-nothing**. The PulseChain
relays need `debug_traceBlockByNumber` against the archive node at
`rpc.vouch.run`, but the `debug` namespace also contains:

- `debug_writeMemProfile`, `debug_writeBlockProfile`, `debug_writeMutexProfile`
- `debug_startCPUProfile`, `debug_stopCPUProfile`, `debug_goTrace`

These accept a caller-supplied path and write to it **as root**. On 2026-11-06
that combination was used to write ~60 files into `/blockchain`, overwrite
`jwt.hex`, `menu.sh`, `start_execution.sh`, `start_consensus.sh` and the
`Caddyfile`, and corrupt the pebble database.

Caddy cannot inspect JSON-RPC bodies, so it cannot separate the trace methods
from the file writers. This proxy can.

## What it does

```
443 (Caddy) -> rpc-filter 127.0.0.1:8547 -> geth 127.0.0.1:8545
```

- Forwards the `eth_`, `net_`, `web3_`, `txpool_` and `rpc_` namespaces in full.
- Forwards an explicit allowlist of read-only `debug_` methods (see
  [`policy.go`](policy.go)).
- Rejects everything else with a JSON-RPC `-32601` error, matching geth's own
  "does not exist / is not available" wording so clients behave identically.
- **Inspects every element of a batch.** If any element is disallowed, the
  whole batch is rejected. A batch is the obvious way to smuggle a forbidden
  method past a naive filter.

Nothing is rewritten: allowed requests are forwarded byte-for-byte, so
`eth_call` results, trace payloads and error semantics are unchanged.

## Deliberate limits

- The filter protects the **public edge**. It does not make `127.0.0.1:8545`
  safe: anything on the host that can open a socket can still call the write
  methods directly. Keep geth bound to loopback and treat host access as root.
- Fail-closed: if the filter is down, the public RPC is down. That is the
  intended behaviour.

## Configuration

| Flag | Default | Purpose |
| --- | --- | --- |
| `-listen` | `127.0.0.1:8547` | JSON-RPC listener |
| `-upstream` | `http://127.0.0.1:8545` | geth endpoint |
| `-metrics-listen` | `127.0.0.1:8548` | Prometheus text endpoint, empty disables |
| `-max-body` | `1048576` | request body cap in bytes |
| `-max-batch` | `100` | maximum calls in a batch |
| `-debug-concurrency` | `4` | concurrent `debug_` calls allowed |
| `-timeout` | `120s` | upstream request timeout |

## Build and test

```sh
go vet ./...
go test -race ./...
go build -o rpc-filter .
```

Image: `ghcr.io/vouchrun/rpc-filter` (public), built by GitHub Actions on push
to `main`, tagged `main`, `sha-<short>` and semver on `v*` tags.

## Verifying a deployment

```sh
# Allowed: ordinary calls and traces.
curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}' \
  https://rpc.vouch.run

curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"debug_traceBlockByNumber","params":["0x1",{"tracer":"callTracer"}]}' \
  https://rpc.vouch.run

# Denied: must return -32601 and must not reach geth.
curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"debug_writeMemProfile","params":["/tmp/pwn"]}' \
  https://rpc.vouch.run

# Denied: smuggling inside a batch.
curl -s -X POST -H 'Content-Type: application/json' \
  --data '[{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]},{"jsonrpc":"2.0","id":2,"method":"debug_setHead","params":["0x1"]}]' \
  https://rpc.vouch.run
```

Denials are logged as `DENIED method="…" client=… batch=…` and counted at
`http://127.0.0.1:8548/metrics` as `rpc_filter_denied_total`.
