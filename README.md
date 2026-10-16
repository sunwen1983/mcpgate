# mcpgate

[![CI](https://github.com/sunwen1983/mcpgate/actions/workflows/ci.yml/badge.svg)](https://github.com/sunwen1983/mcpgate/actions/workflows/ci.yml)

**MCP servers exist everywhere. The production layer doesn't.**

MCP (Model Context Protocol) servers are easy to write and easy to expose —
and that's the problem. A raw MCP server has no authentication, no rate
limiting, no caching, no circuit breaking, and no observability. Put one
behind your product and the first abusive client, the first slow backend,
or the first retry storm takes it down.

mcpgate is the production layer: a gateway that sits in front of your MCP
servers and adds auth, per-key rate limits, response caching, circuit
breakers, structured logs, and Prometheus metrics — without touching the
servers themselves.

```
                    ┌─────────────────────────────────────────────┐
  MCP client ──────▶│  mcpgate                                    │
  (any MCP client)  │                                             │
                    │  ┌─────────┐  ┌──────────┐  ┌───────────┐  │
                    │  │ API key │─▶│ token    │─▶│ cache     │  │
                    │  │ auth    │  │ bucket   │  │ (TTL)     │  │
                    │  └─────────┘  └──────────┘  └─────┬─────┘  │
                    │  ┌───────────┐  ┌───────────┐     │        │
                    │  │ circuit   │─▶│ backend   │◀────┘        │
                    │  │ breaker   │  │ stdio /   │              │
                    │  └───────────┘  │ HTTP      │              │
                    │  /metrics /healthz  └───────────┘           │
                    └─────────────────────────────────────────────┘
                                        │                │
                                   stdio child       Streamable HTTP
                                   MCP server        MCP server
```

## Quickstart

```bash
# Build
go build -o mcpgate ./cmd/mcpgate
go build -o echo-backend ./examples/echo-backend

# Run (examples/config.yaml uses the demo key "demo-key-123" — rotate it)
./mcpgate -config examples/config.yaml
```

List tools through the gateway:

```bash
curl -X POST http://localhost:8080/mcp/echo \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: demo-key-123' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

Call a tool:

```bash
curl -X POST http://localhost:8080/mcp/echo \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: demo-key-123' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"add","arguments":{"a":2,"b":3}}}'
# {"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"5"}]}}
```

Or run the full guided demo (auth failure, cache MISS/HIT, rate-limit 429s,
health + metrics):

```bash
bash demo/demo.sh
```

## How it works

`POST /mcp/{backend}` runs one JSON-RPC 2.0 request through this pipeline:

1. **Auth** — `X-API-Key` header or `Authorization: Bearer <key>`.
   Missing/invalid keys get `401`. Key *values* are never logged.
2. **Rate limit** — token bucket per API key. Exhausted buckets get `429`
   with a JSON-RPC error. Per-key overrides in config.
3. **Backend lookup** — unknown backends get `404` / JSON-RPC `-32601`.
4. **Circuit breaker** — an open breaker fails fast with `503` and never
   touches the backend. After the cooldown, one probe is allowed; success
   closes the breaker, failure re-opens it.
5. **Cache** — `tools/list` responses are cached per backend. `tools/call`
   responses are cached **only** for tools explicitly listed in
   `cacheable_tools`, because tool calls can have side effects. Cache hits
   return `X-Cache: HIT`; the cached payload is re-wrapped with the current
   request's `id`, because JSON-RPC requires the response id to match.
6. **Forward** — the request goes to the backend (stdio child process or
   Streamable HTTP POST). Transport failures return `502` and count toward
   the breaker. JSON-RPC error objects from the backend are forwarded
   verbatim and are *not* cached.

Notifications (messages without an `id`) are forwarded to the backend
fire-and-forget and the client gets `202 Accepted` — the client asked for
acceptance, not delivery confirmation, so a backend that can't be reached
still gets a `202`, and the failure is logged and counted instead of
surfacing to the client. An open circuit breaker drops notifications
without attempting the forward.

## Configuration

```yaml
listen: ":8080"

api_keys:
  - key: "demo-key-123"      # X-API-Key or Bearer token
    name: "demo"              # appears in logs/metrics, never the key itself
    rate_limit:               # optional per-key override
      requests_per_second: 5
      burst: 10

default_rate_limit:
  requests_per_second: 10
  burst: 20

backends:
  - name: "echo"
    transport: "stdio"        # or "http"
    command: ["./echo-backend"]  # stdio: executable + args
    env: {}                   # stdio: extra env vars for the child
    # url: "http://localhost:9000/mcp"  # http: Streamable HTTP endpoint
    # auth: {type: "bearer", token: "..."}  # http: gateway -> backend auth
    timeout: 10s              # per-request backend timeout (default 10s)
    cache:
      tools_list_ttl: 60s
      tools_call_ttl: 30s     # 0 disables tools/call caching
      cacheable_tools: ["echo", "add"]
    breaker:
      failure_threshold: 5    # consecutive failures to trip open (0 = never)
      cooldown: 30s           # open duration before a probe is allowed
```

| Field | Required | Notes |
|---|---|---|
| `listen` | yes | Address the gateway binds, e.g. `:8080` |
| `api_keys` | yes | At least one; keys must be unique |
| `default_rate_limit` | yes | Applies to keys without an override |
| `backends` | yes | At least one; names must be unique |
| `backends[].transport` | yes | `stdio` or `http` |
| `backends[].command` | stdio | Executable + args for the child process |
| `backends[].url` | http | Streamable HTTP endpoint |
| `backends[].auth` | no | `bearer` (token) or `basic` (username/password) |
| `backends[].timeout` | no | Default `10s` |
| `backends[].cache.tools_list_ttl` | no | `0` disables |
| `backends[].cache.tools_call_ttl` | no | `0` disables; only listed tools cached |
| `backends[].breaker.failure_threshold` | no | `0` disables tripping |
| `backends[].breaker.cooldown` | no | Time before a probe is allowed |

## Observability

- `GET /healthz` → `{"status":"ok"}` (liveness).
- `GET /metrics` → Prometheus exposition:

| Metric | Labels | Meaning |
|---|---|---|
| `mcpgate_requests_total` | backend, method, status | Requests by outcome (`ok`, `rpc_error`, `backend_error`, `rejected`) |
| `mcpgate_request_duration_seconds` | backend | Backend call latency histogram |
| `mcpgate_cache_hits_total` / `mcpgate_cache_misses_total` | backend, method | Cache behavior |
| `mcpgate_backend_errors_total` | backend | Transport failures |
| `mcpgate_breaker_state` | backend | `0` closed, `1` half-open, `2` open |
| `mcpgate_rate_limit_rejections_total` | key_name | Rejected requests per API key name |
| `mcpgate_notifications_total` | backend, method, status | Notifications by outcome (`forwarded`, `dropped`, `failed`) |

Logs are structured JSON (via `log/slog`) with `request_id`, `api_key`
(name only), `backend`, `method`, cache outcome, and latency. Backend
credentials and API key values never appear in logs.

## Development

```bash
gofmt -l .          # must be clean
go build ./...
go vet ./...
go test ./...       # unit tests: rate limiter, cache, breaker, config,
                    # backend transports (incl. stdio against echo-backend),
                    # gateway routing (auth, rate limit, cache, breaker)
```

CI (GitHub Actions) runs format, build, vet, and test on every push/PR.

## Roadmap — what's NOT in v0.1

Honest gaps, in rough priority order:

- **SSE streaming**: HTTP backends that answer with `text/event-stream`
  have the stream read and the JSON-RPC response with the matching
  request id returned; events are not relayed to the client as a live
  stream.
- **OAuth / OIDC**: client auth is API keys only; no token introspection,
  scopes, or per-tool authorization.
- **Request/response transformation**: no header injection, tool filtering,
  or schema rewriting between client and backend.
- **Backend health checks / load balancing**: one backend per name, no
  pooling, no active probing — the breaker is the only failure signal.
- **Distributed rate limiting / shared cache**: both are in-process only.
  Multi-replica deployments need Redis-backed implementations.
- **TLS termination**: run behind a TLS-terminating reverse proxy.
- **Admin API**: no runtime config reload or cache invalidation endpoint;
  restart to pick up config changes.

## License

MIT. See [LICENSE](LICENSE).
