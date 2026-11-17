# Show HN draft — mcpgate

Submit URL: https://github.com/sunwen1983/mcpgate

Title: Show HN: mcpgate – a production gateway for MCP servers

First comment:

MCP servers exist everywhere. The production layer doesn't.

A raw MCP server has no auth, no rate limits, no caching, no circuit breaking, no observability. mcpgate sits in front of your MCP servers and adds that layer, without touching the servers themselves. Written in Go, it ships as a single binary with config in one YAML file.

v0.1.0 includes:

- Auth: API keys, OIDC JWT, OAuth2 token introspection, scopes + per-tool authorization
- Traffic: per-key token-bucket rate limiting (Redis-backed), TTL response caching (Redis-backed shared cache), per-backend circuit breakers, load balancing
- Protocol: HTTP and stdio backends, live SSE relay (including post-response notifications)
- Shaping: request/response transformation, schema rewriting, per-backend tool filtering, header injection
- Ops: Prometheus metrics, /healthz and /readyz, backend health checks, admin API (config reload, runtime status), TLS termination

Quickstart:

```
go build -o mcpgate ./cmd/mcpgate
go build -o echo-backend ./examples/echo-backend
./mcpgate -config examples/config.yaml
```

Demo — a 5-act terminal walkthrough recorded against the real gateway (401 without a key, cache MISS then HIT, a tools/call, rate-limit 429s, health/metrics): https://muse.ai/s/mcpgate-demo-txp6xztxixlxhxb6xd/DSaM8_eTR__8HX4slasuNSk_PjI1iugGPxbJyA_ePt8

Release: https://github.com/sunwen1983/mcpgate/releases/tag/v0.1.0

Also listed in [awesome-mcp-servers](https://github.com/punkpeye/awesome-mcp-servers) (Aggregators) and on Glama.

Built because I kept needing this layer in front of MCP servers I run. Feedback welcome — especially on what the gateway is still missing for your setup.
