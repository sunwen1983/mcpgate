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
                    │  /metrics /healthz /readyz └───────────┘   │
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

### Stdio mode

With `-stdio`, mcpgate itself runs as a stdio MCP server, aggregating
every configured backend behind one stdin/stdout stream — for
stdio-only clients (Claude Desktop, IDE extensions, MCP inspectors)
and server directories:

```bash
./mcpgate -config examples/config.yaml -stdio
```

The gateway answers `initialize`/`ping` itself, merges all backends'
tool lists into one `tools/list` (a tool name claimed by more than
one backend is listed once — calls can only route to the first
owner, in sorted backend-name order), and routes each `tools/call` to the
backend that owns the tool. Per backend, the tool filter, circuit breaker, and
response cache apply exactly as on the HTTP path. Two HTTP concerns do
not apply: stdio is a local trust boundary, so there is no API-key auth
or rate limiting — whoever can spawn the process already controls it.
Logs go to stderr; stdout carries protocol traffic only.

## How it works

`POST /mcp/{backend}` runs one JSON-RPC 2.0 request through this pipeline:

1. **Auth** — `X-API-Key` header or `Authorization: Bearer <key>` for API
   keys; when `oidc` is configured, a Bearer JWT signed by the issuer is
   also accepted (identity `oidc:<sub>`); when `oauth` is configured, an
   opaque Bearer token is checked with the authorization server's
   introspection endpoint (identity `oauth:<sub>`). Missing/invalid
   credentials get `401`. Key *values* and token contents are never logged.
2. **Rate limit** — token bucket per API key. Exhausted buckets get `429`
   with a JSON-RPC error. Per-key overrides in config.
3. **Backend lookup** — unknown backends get `404` / JSON-RPC `-32601`.
4. **Tool filter** — a backend's `tool_filter` hides tools: denied tools
   are removed from `tools/list` results, and `tools/call` for a filtered
   tool is rejected with `403` / JSON-RPC `-32602` before touching the
   backend, cache, or breaker. A backend's `schema_rewrites` also apply
   here: `tools/list` shows renamed/hidden arguments, and `tools/call`
   arguments are translated back to the backend's names before forwarding.
5. **Circuit breaker** — an open breaker fails fast with `503` and never
   touches the backend. After the cooldown, one probe is allowed; success
   closes the breaker, failure re-opens it.
6. **Cache** — `tools/list` responses are cached per backend. `tools/call`
   responses are cached **only** for tools explicitly listed in
   `cacheable_tools`, because tool calls can have side effects. Cache hits
   return `X-Cache: HIT`; the cached payload is re-wrapped with the current
   request's `id`, because JSON-RPC requires the response id to match.
   Operators can drop cached entries without restarting the gateway:
   `POST /admin/cache/invalidate` clears every backend's cache,
   `POST /admin/cache/invalidate/{backend}` clears one (API key required,
   answered with `{"status":"ok","backends":[...],"cleared":N}`). They can
   also reload the config without restarting: `POST /admin/reload`
   re-reads the config file, validates it with the same strict checks as
   startup, and applies the reloadable parts live — API keys, per-tool
   scopes, rate limits, and per-backend tool filters, schema rewrites,
   cache settings, breaker settings, and health-check settings. Anything
   else (listen address, TLS, Redis, OIDC/OAuth endpoints, and a
   backend's connection identity — transport, command, URL, endpoints,
   auth, headers, timeout — plus adding or removing a backend) is
   reported in `requires_restart` and needs a restart; an invalid config
   is rejected whole with `400` and the running config keeps serving.
   `GET /admin/status` reports uptime, when the config was last loaded,
   the API key names, and every backend's breaker and health state
   (API key required; key values never appear in the response).
7. **Forward** — the request goes to the backend (stdio child process or
   Streamable HTTP POST). Configured static `headers` are injected into
   HTTP backend requests. Transport failures return `502` and count toward
   the breaker. JSON-RPC error objects from the backend are forwarded
   verbatim and are *not* cached. A Streamable HTTP backend that answers
   with `text/event-stream` has its stream parsed incrementally: the
   JSON-RPC response matching the request id is returned as soon as its
   event arrives, instead of waiting for the stream to close.

Clients that send `Accept: text/event-stream` get the backend's SSE
events relayed live: progress notifications and other server messages
arrive as `data:` events as the backend sends them, and the stream
ends after the JSON-RPC response matching the request id. Streaming
responses are never cached, and the per-request timeout bounds the
whole stream. A backend that cannot stream (stdio) answers with a
single SSE event carrying the response. Clients that only accept JSON
keep the single-response behavior.

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
    # scopes: ["tools:read"]  # scopes this key holds (see "Per-tool authorization")
    rate_limit:               # optional per-key override
      requests_per_second: 5
      burst: 10

default_rate_limit:
  requests_per_second: 10
  burst: 20

# Optional: share rate-limit budgets and the response cache across
# replicas via Redis (see "Distributed rate limiting / shared cache
# (Redis)"). Without it, each replica enforces its own per-key budget
# and keeps its own cache.
# redis:
#   addr: "localhost:6379"
#   # password: "..."   # AUTH password; never logged
#   # db: 0             # logical database
#   # key_prefix: "mcpgate:"  # namespace for mcpgate's keys

# Optional: gate individual tools behind scopes (see "Per-tool authorization").
# tool_scopes:
#   drop_table: ["tools:admin"]   # any one listed scope grants the tool
#   truncate: ["tools:admin", "tools:write"]

# Optional: accept OIDC JWT bearer tokens as well as API keys.
# oidc:
#   issuer: "https://accounts.example.com"  # discovery via /.well-known/openid-configuration
#   audience: "mcpgate"                      # required aud claim
#   # jwks_url: "https://accounts.example.com/jwks"  # override discovery
#   # clock_skew: 60s                        # exp/nbf tolerance (default 60s)

# Optional: accept opaque OAuth2 bearer tokens via RFC 7662 introspection.
# oauth:
#   introspection_url: "https://auth.example.com/oauth/introspect"
#   client_id: "gateway"                     # gateway credential at the endpoint
#   client_secret: "s3cret"                  # never logged; rotate before real use
#   # cache_ttl: 60s                         # successful-introspection cache (default 60s, never past exp)

backends:
  - name: "echo"
    transport: "stdio"        # or "http"
    command: ["./echo-backend"]  # stdio: executable + args
    env: {}                   # stdio: extra env vars for the child
    # url: "http://localhost:9000/mcp"  # http: Streamable HTTP endpoint
    # endpoints:                        # http: pool of endpoints instead of one url
    #   - "http://localhost:9000/mcp"   #   (mutually exclusive with url; see "Load balancing")
    #   - "http://localhost:9001/mcp"
    # strategy: "round_robin"           # load-balancing strategy (only round_robin so far)
    # auth: {type: "bearer", token: "..."}  # http: gateway -> backend auth
    # headers:                            # http: static headers injected into
    #   X-Gateway: "mcpgate"              #   every request to this backend
    timeout: 10s              # per-request backend timeout (default 10s)
    tool_filter:              # which tools clients may see and call
      # allow: ["echo", "add"]  # non-empty = only these tools are exposed
      deny: ["drop_table"]     # hidden from tools/list, rejected on tools/call
    schema_rewrites:          # reshape tool schemas for clients (see "Schema rewriting")
      - tool: "add"
        rename_arguments: {a: "first", b: "second"}  # client names; translated back on tools/call
        hide_arguments: ["debug"]                    # removed from the schema, stripped from calls
    cache:
      tools_list_ttl: 60s
      tools_call_ttl: 30s     # 0 disables tools/call caching
      cacheable_tools: ["echo", "add"]
    breaker:
      failure_threshold: 5    # consecutive failures to trip open (0 = never)
      cooldown: 30s           # open duration before a probe is allowed
    health_check:             # active probing (see "Backend health checks")
      interval: 30s           # 0 disables (the default)
      # timeout: 5s           # per-probe timeout (default 5s)
      # failure_threshold: 3  # consecutive failed probes to mark unhealthy (default 3)
```

| Field | Required | Notes |
|---|---|---|
| `listen` | yes | Address the gateway binds, e.g. `:8080` |
| `api_keys` | yes | At least one; keys must be unique |
| `api_keys[].scopes` | no | Scopes this key holds for per-tool authorization |
| `default_rate_limit` | yes | Applies to keys without an override |
| `redis.addr` | no | Redis address (`host:port`); enables distributed rate limiting |
| `redis.password` | no | Redis AUTH password; never logged |
| `redis.db` | no | Redis logical database (default `0`) |
| `redis.key_prefix` | no | Namespace for mcpgate's keys (default `mcpgate:`) |
| `tool_scopes` | no | Tool name → scopes; holding any listed scope grants the tool |
| `oidc.issuer` | with `oidc` | OIDC issuer URL; JWKS discovered via `/.well-known/openid-configuration` |
| `oidc.audience` | with `oidc` | Required `aud` claim value |
| `oidc.jwks_url` | no | Overrides the discovered JWKS endpoint |
| `oidc.clock_skew` | no | `exp`/`nbf` tolerance; default `60s` |
| `oauth.introspection_url` | with `oauth` | RFC 7662 introspection endpoint |
| `oauth.client_id` | with `oauth` | Gateway credential at the introspection endpoint (HTTP Basic) |
| `oauth.client_secret` | with `oauth` | Gateway credential at the introspection endpoint; never logged |
| `oauth.cache_ttl` | no | Successful-introspection cache; default `60s`, never past the token's `exp` |
| `backends` | yes | At least one; names must be unique |
| `backends[].transport` | yes | `stdio` or `http` |
| `backends[].command` | stdio | Executable + args for the child process |
| `backends[].url` | http | Absolute `http(s)` URL of the Streamable HTTP endpoint |
| `backends[].endpoints` | no | http only: pool of endpoint URLs instead of `url` (mutually exclusive; see "Load balancing") |
| `backends[].strategy` | no | Load-balancing strategy across `endpoints`; only `round_robin` (the default) |
| `backends[].auth` | no | `bearer` (token) or `basic` (username/password) |
| `backends[].headers` | no | http only: static headers injected into every backend request; backend auth wins on name clashes |
| `backends[].tool_filter.allow` | no | Non-empty: only these tools are exposed |
| `backends[].tool_filter.deny` | no | Hidden from `tools/list`, rejected on `tools/call` (deny wins) |
| `backends[].schema_rewrites` | no | Per-tool argument renames and hidden arguments (see "Schema rewriting") |
| `backends[].timeout` | no | Default `10s` |
| `backends[].cache.tools_list_ttl` | no | `0` disables |
| `backends[].cache.tools_call_ttl` | no | `0` disables; only listed tools cached |
| `backends[].breaker.failure_threshold` | no | `0` disables tripping |
| `backends[].breaker.cooldown` | no | Time before a probe is allowed |
| `backends[].health_check.interval` | no | Probe interval; `0` disables health checks |
| `backends[].health_check.timeout` | no | Per-probe timeout; default `5s` |
| `backends[].health_check.failure_threshold` | no | Consecutive failed probes to mark unhealthy; default `3` |

### Per-tool authorization (scopes)

`tool_scopes` gates individual tools behind OAuth-style scopes. A tool
named there may be called only by a client holding at least one of its
listed scopes; holding any one suffices. A gated tool is rejected on
`tools/call` with a JSON-RPC `-32602` error (HTTP 403) before the
backend is touched, and hidden from `tools/list` for clients that lack
a granting scope — exactly like the `tool_filter` deny list. Tools not
named in `tool_scopes` stay open to any authenticated client, so adding
the section never breaks existing callers.

Scopes come from the client's credential:

- **API keys**: the `scopes` list on the key in config.
- **OIDC JWTs**: the `scope` claim (space-delimited string) or the `scp`
  claim (array of strings).
- **OAuth2 introspected tokens**: the `scope` claim from the
  introspection response.

Scope names are single tokens (no whitespace); a gated tool must list at
least one scope — a tool nobody may call belongs in `tool_filter.deny`,
not here. Scope filtering happens per request at output time, so a
cached `tools/list` response never leaks one client's view of gated
tools to another.

### Schema rewriting

`schema_rewrites` reshapes one backend tool's input schema as clients
see it, per backend:

```yaml
backends:
  - name: "echo"
    schema_rewrites:
      - tool: "add"
        rename_arguments: {a: "first", b: "second"}
        hide_arguments: ["debug"]
```

- **Rename** — the tool's `inputSchema` is exposed with the renamed
  arguments (in `properties` and in `required`), and a `tools/call`
  that passes the client-facing names is translated back to the
  backend's names before forwarding. Use it to present a consistent
  naming style across backends (e.g. a backend's `snake_case`
  arguments as `camelCase`), or to give a confusing argument a
  clearer name.
- **Hide** — the argument disappears from the exposed schema, and a
  client that sends it anyway has it stripped from the call before
  forwarding. Use it for arguments clients should never set (an
  internal flag, a server-side default).

Tool names are never rewritten: the tool filter, per-tool
authorization, and cache keys all keep using the backend's tool name.
A rewrite may name an argument the backend does not expose — that
part is simply a no-op, not an error. Renames must be unambiguous:
two arguments may not rename to the same client-facing name, and a
rename target may not also be a rename source (no chains). An
argument may not be both renamed and hidden. Rewrites are static per
backend, so a rewritten `tools/list` response is cached exactly like
a filtered one, and the same rewriting applies on the HTTP path, the
SSE stream, and stdio mode.

Do not hide an argument the backend requires unless the backend
tolerates its absence: the gateway does not inject a value for it,
so the call would fail at the backend.

### Backend health checks

The circuit breaker only learns a backend is down from real client
traffic failing against it. Active health checks learn it sooner:
when configured, the gateway probes the backend with a plain
`tools/list` call — the lightest request that exercises exactly the
path real traffic needs — and marks the backend unhealthy after a
configurable number of consecutive failed probes. A probe fails on a
transport error, a timeout, or a JSON-RPC error response.

```yaml
backends:
  - name: "remote"
    transport: "http"
    url: "http://localhost:9000/mcp"
    health_check:
      interval: 30s           # 0 disables health checks (the default)
      timeout: 5s             # per-probe timeout (default 5s)
      failure_threshold: 3    # consecutive failed probes to mark unhealthy (default 3)
```

While a backend is unhealthy, requests to it fail fast with `503`
(`backend unavailable: health check failing`) and notifications are
dropped, instead of timing out against a backend that is already
known to be down. One successful probe brings the backend back and
traffic resumes automatically. A backend is healthy before its first
probe, so enabling health checks never blocks startup, and probe
failures never touch the circuit breaker — the breaker measures real
traffic, health measures the backend, and the two signals stay
independent. `GET /readyz` reports each backend's health
(`healthy`, `unhealthy`, or `disabled`), and the
`mcpgate_backend_health` gauge (`1` healthy, `0` unhealthy) exposes
it to Prometheus.

### Load balancing

One backend name can front several identical endpoints: configure
`endpoints` instead of a single `url` and requests spread across
them round-robin. The pool is per backend name, so the breaker,
cache, health checks, and SSE relay all keep seeing one logical
backend; only the transport fans out.

```yaml
backends:
  - name: "remote"
    transport: "http"
    endpoints:
      - "http://10.0.0.1:9000/mcp"
      - "http://10.0.0.2:9000/mcp"
    strategy: "round_robin"   # the only strategy so far; may be omitted
```

An endpoint that fails is taken out of rotation after 3 consecutive
transport failures and tried again after a 30s cooldown; a success
brings it straight back. JSON-RPC errors inside a well-formed
response do not count — the endpoint answered. While every endpoint
is down the pool degrades to plain round-robin over all of them
instead of failing every request outright, so a recovered endpoint
gets traffic again. A failed call is never retried on another
endpoint: `tools/call` may have side effects, so retrying could
execute the same call twice — the failed endpoint is skipped from
the *next* request on. The `mcpgate_backend_endpoints_healthy`
gauge reports how many endpoints of a backend are currently in
rotation. Pooling is http-only (`stdio` backends are single
processes); `url` and `endpoints` are mutually exclusive, and
existing single-`url` configurations behave exactly as before.

### Distributed rate limiting / shared cache (Redis)

By default each replica enforces its own per-key token budget, so a
client can spend N× the limit against N replicas. Point the gateway
at a shared Redis and every replica enforces one budget per key:

```yaml
redis:
  addr: "localhost:6379"
  # password: "..."  # AUTH password; never logged
  # db: 0
  # key_prefix: "mcpgate:"  # namespace when Redis is shared with other apps
```

The bucket is a Lua script (atomic refill + consume), so concurrent
replicas cannot overspend it; idle buckets expire once a full refill
is guaranteed. Per-key `rate_limit` overrides and the default limit
apply exactly as before — only the storage moves to Redis.

Failure behavior is availability-first: a Redis that is unreachable
at startup logs a warning and the gateway falls back to in-process
limiters and an in-process response cache until the next restart; a
Redis error on a single request fails open (the request is allowed,
a cache lookup misses) and is counted in
`mcpgate_rate_limit_errors_total` / `mcpgate_cache_errors_total`.

The same `redis:` block also shares the response cache: with Redis
configured, a cached `tools/list` or cacheable `tools/call` response
is visible to every replica, and `POST /admin/cache/invalidate`
clears the shared entries. Without Redis, caching stays in-process
exactly as before. The cache keys are namespaced under
`<key_prefix>cache:`, apart from the rate limiter's `ratelimit:` keys,
so invalidation never touches token budgets.

### TLS termination

By default the gateway serves plain HTTP — put a TLS-terminating
reverse proxy in front of it. To terminate TLS in the gateway itself
(direct HTTPS on the listen address, e.g. for a single-host deploy
with no proxy), add a `tls:` block:

```yaml
tls:
  cert_file: "tls/cert.pem"   # PEM chain, leaf first
  key_file: "tls/key.pem"     # PEM private key; never logged
  # min_version: "1.2"        # "1.2" (default) or "1.3"
```

The pair is loaded at startup, so a missing file, a mismatched key,
or an unsupported `min_version` fails fast with a clear error instead
of at first serve. Only the client-facing listener is affected —
backends are reached exactly as configured, and the `tls:` block has
no effect in stdio mode.

## Observability

- `GET /healthz` → `{"status":"ok"}` (liveness).
- `GET /readyz` → `{"status":"ready","backends":{"demo":"closed"},"health":{"demo":"healthy"}}` (readiness:
  per-backend circuit-breaker state and health-check state;
  `503 {"status":"degraded",...}` when no backend can serve — use this
  for load-balancer health checks, not `/healthz`, which stays green
  while every backend is down).
- `GET /metrics` → Prometheus exposition:

| Metric | Labels | Meaning |
|---|---|---|
| `mcpgate_requests_total` | backend, method, status | Requests by outcome (`ok`, `rpc_error`, `backend_error`, `rejected`) |
| `mcpgate_request_duration_seconds` | backend | Backend call latency histogram |
| `mcpgate_cache_hits_total` / `mcpgate_cache_misses_total` | backend, method | Cache behavior |
| `mcpgate_cache_errors_total` | — | Redis-backed cache operations that errored and failed open |
| `mcpgate_backend_errors_total` | backend | Transport failures |
| `mcpgate_breaker_state` | backend | `0` closed, `1` half-open, `2` open |
| `mcpgate_backend_health` | backend | `1` healthy, `0` unhealthy (only for backends with health checks enabled) |
| `mcpgate_backend_endpoints_healthy` | backend | Endpoints currently in rotation (only for backends with `endpoints` configured) |
| `mcpgate_rate_limit_rejections_total` | key_name | Rejected requests per API key name |
| `mcpgate_rate_limit_errors_total` | — | Redis-backed rate-limit checks that errored and failed open |
| `mcpgate_notifications_total` | backend, method, status | Notifications by outcome (`forwarded`, `dropped`, `failed`) |
| `mcpgate_config_reloads_total` | result | Config reload attempts by outcome (`applied`, `rejected`) |

Logs are structured JSON (via `log/slog`) with `request_id`, `api_key`
(name only), `backend`, `method`, cache outcome, and latency. Backend
credentials and API key values never appear in logs.

## Development

```bash
gofmt -l .          # must be clean
go build ./...
go vet ./...
go test ./...       # unit tests: rate limiter, cache, breaker, config,
                    # health checks, load balancing, backend transports (incl. stdio against echo-backend),
                    # gateway routing (auth, rate limit, cache, breaker, health, admin reload/status)
```

CI (GitHub Actions) runs format, build, vet, and test on every push/PR.

## Roadmap — what's NOT in v0.1

Honest gaps, in rough priority order:

- **SSE streaming (live relay)**: supported — a client that sends
  `Accept: text/event-stream` gets the backend's SSE events relayed
  live (progress notifications arrive before the response, instead of
  being swallowed), and the stream ends after the matching JSON-RPC
  response. Streaming responses are never cached; stdio backends
  answer with a single SSE event. Still missing: relaying events that
  arrive after the response (the stream closes at the response,
  matching the single-response path).
- **OAuth / OIDC**: OIDC JWT bearer-token client auth is supported
  (RS/ES/PS signature validation against the issuer's JWKS via discovery,
  strict `iss`/`aud`/`exp` checks; JWKS refreshes periodically and on key
  rotation). OAuth2 token introspection (RFC 7662) is supported too:
  opaque Bearer tokens are checked with the authorization server
  (HTTP Basic client auth, `active`/`sub`/`exp` enforced, successful
  results cached no longer than the token's remaining lifetime).
  Scopes and per-tool authorization are supported too: `tool_scopes`
  gates individual tools behind scopes drawn from an API key's
  configured scopes, a JWT's `scope`/`scp` claim, or an introspected
  token's `scope` claim (see "Per-tool authorization").
- **Request/response transformation**: per-backend `tool_filter`
  (allow/deny lists: hidden from `tools/list`, rejected on `tools/call`),
  static `headers` injection for HTTP backends, and per-tool schema
  rewriting are supported: `schema_rewrites` renames arguments as
  clients see them (translated back on `tools/call`) and hides
  arguments clients should never set (see "Schema rewriting"). Still
  missing: rewriting of response payloads (result content).
- **Backend health checks / load balancing**: active health checks are
  supported — a configured `health_check` probes each backend with
  `tools/list`, marks it unhealthy after consecutive failed probes, and
  the gateway fails fast (503) until a probe succeeds again; `GET
  /readyz` and the `mcpgate_backend_health` gauge report the state (see
  "Backend health checks"). Load balancing is supported too: an http
  backend configured with `endpoints` instead of a single `url` spreads
  requests round-robin across them, takes endpoints out of rotation
  after consecutive transport failures, and brings them back after a
  cooldown (see "Load balancing"). Still missing: other balancing
  strategies (weighted, least-connections), and the breaker remains the
  only failure signal when health checks are disabled.
- **Distributed rate limiting / shared cache**: rate limiting is
  distributed — a configured `redis:` shares one token budget per API
  key across replicas (see "Distributed rate limiting / shared cache
  (Redis)").
  Response caching is shared the same way: with `redis:` configured,
  cached responses live in Redis and are visible to every replica,
  and `POST /admin/cache/invalidate` clears the shared entries;
  without Redis, caching stays in-process.
- **TLS termination**: supported — a `tls:` block (certificate chain +
  private key, minimum TLS 1.2 or 1.3) makes the gateway serve HTTPS
  directly; without it, plain HTTP behind a reverse proxy (see "TLS
  termination").
- **Admin API**: `POST /admin/cache/invalidate[/{backend}]` clears backend
  response caches at runtime (API-key authenticated); `POST /admin/reload`
  re-reads the config file and applies the reloadable parts live
  (API keys, tool scopes, rate limits, per-backend tool filters, schema
  rewrites, cache, breaker, and health-check settings), reporting the
  rest as requiring a restart; `GET /admin/status` reports uptime,
  last-loaded time, API key names, and per-backend breaker/health state.
  Still missing: reloading a backend's connection identity (URL, auth,
  command) without a restart, and SIGHUP-triggered reload.

## License

MIT. See [LICENSE](LICENSE).
