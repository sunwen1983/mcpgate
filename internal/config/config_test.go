package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sunwen1983/mcpgate/internal/config"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const validYAML = `
listen: ":8080"
api_keys:
  - key: "secret-1"
    name: "alice"
  - key: "secret-2"
    name: "bob"
    rate_limit: {requests_per_second: 2, burst: 4}
default_rate_limit: {requests_per_second: 10, burst: 20}
backends:
  - name: "echo"
    transport: "stdio"
    command: ["./echo-backend"]
    timeout: 10s
    cache:
      tools_list_ttl: 60s
      tools_call_ttl: 30s
      cacheable_tools: ["echo"]
    breaker:
      failure_threshold: 5
      cooldown: 30s
  - name: "remote"
    transport: "http"
    url: "http://localhost:9000/mcp"
    auth: {type: "bearer", token: "backend-token"}
`

func TestLoadValid(t *testing.T) {
	cfg, err := config.Load(writeTempConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("Listen = %q, want :8080", cfg.Listen)
	}
	if len(cfg.APIKeys) != 2 {
		t.Fatalf("APIKeys = %d, want 2", len(cfg.APIKeys))
	}
	if cfg.APIKeys[0].Name != "alice" {
		t.Errorf("APIKeys[0].Name = %q, want alice", cfg.APIKeys[0].Name)
	}
	b, ok := cfg.FindBackend("echo")
	if !ok {
		t.Fatal("FindBackend(echo) not found")
	}
	if b.Cache.ToolsListTTL.Seconds() != 60 {
		t.Errorf("ToolsListTTL = %v, want 60s", b.Cache.ToolsListTTL)
	}
	if len(b.Cache.CacheableTools) != 1 || b.Cache.CacheableTools[0] != "echo" {
		t.Errorf("CacheableTools = %v, want [echo]", b.Cache.CacheableTools)
	}
	if b.Breaker.FailureThreshold != 5 {
		t.Errorf("FailureThreshold = %d, want 5", b.Breaker.FailureThreshold)
	}
	remote, ok := cfg.FindBackend("remote")
	if !ok {
		t.Fatal("FindBackend(remote) not found")
	}
	if remote.Auth.Type != "bearer" || remote.Auth.Token != "backend-token" {
		t.Errorf("remote auth = %+v, want bearer", remote.Auth)
	}
	// Per-key override wins; otherwise the default applies.
	if got := cfg.RateLimitFor(cfg.APIKeys[1]); got.Rate != 2 || got.Burst != 4 {
		t.Errorf("RateLimitFor(bob) = %+v, want {2 4}", got)
	}
	if got := cfg.RateLimitFor(cfg.APIKeys[0]); got.Rate != 10 || got.Burst != 20 {
		t.Errorf("RateLimitFor(alice) = %+v, want {10 20}", got)
	}
}

func TestLoadValidToolScopes(t *testing.T) {
	cfg, err := config.Load(writeTempConfig(t, `
listen: ":8080"
api_keys:
  - {key: "k1", name: "alice", scopes: ["tools:read"]}
  - {key: "k2", name: "bob"}
tool_scopes:
  drop_table: ["tools:admin"]
  truncate: ["tools:admin", "tools:write"]
backends: [{name: "b", transport: "http", url: "http://x"}]
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.APIKeys[0].Scopes; len(got) != 1 || got[0] != "tools:read" {
		t.Errorf("APIKeys[0].Scopes = %v, want [tools:read]", got)
	}
	if len(cfg.APIKeys[1].Scopes) != 0 {
		t.Errorf("APIKeys[1].Scopes = %v, want none", cfg.APIKeys[1].Scopes)
	}
	if got := cfg.ToolScopes["drop_table"]; len(got) != 1 || got[0] != "tools:admin" {
		t.Errorf("ToolScopes[drop_table] = %v, want [tools:admin]", got)
	}
	if got := cfg.ToolScopes["truncate"]; len(got) != 2 {
		t.Errorf("ToolScopes[truncate] = %v, want 2 scopes", got)
	}
}

func TestLoadValidOAuth(t *testing.T) {
	cfg, err := config.Load(writeTempConfig(t, `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
oauth:
  introspection_url: "https://auth.example.com/oauth/introspect"
  client_id: "gateway"
  client_secret: "s3cret"
  cache_ttl: 5m
backends: [{name: "b", transport: "http", url: "http://x"}]
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OAuth == nil {
		t.Fatal("OAuth = nil, want configured")
	}
	if cfg.OAuth.IntrospectionURL != "https://auth.example.com/oauth/introspect" {
		t.Errorf("IntrospectionURL = %q", cfg.OAuth.IntrospectionURL)
	}
	if cfg.OAuth.ClientID != "gateway" || cfg.OAuth.ClientSecret != "s3cret" {
		t.Errorf("client credentials = %q / %q", cfg.OAuth.ClientID, cfg.OAuth.ClientSecret)
	}
	if time.Duration(cfg.OAuth.CacheTTL) != 5*time.Minute {
		t.Errorf("CacheTTL = %v, want 5m", time.Duration(cfg.OAuth.CacheTTL))
	}
}

func TestLoadValidRedis(t *testing.T) {
	cfg, err := config.Load(writeTempConfig(t, `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
redis:
  addr: "redis.internal:6379"
  password: "s3cret"
  db: 2
  key_prefix: "prod:"
backends: [{name: "b", transport: "http", url: "http://x"}]
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Redis == nil {
		t.Fatal("Redis = nil, want configured")
	}
	if cfg.Redis.Address() != "redis.internal:6379" {
		t.Errorf("Address = %q", cfg.Redis.Address())
	}
	if cfg.Redis.Password != "s3cret" {
		t.Errorf("Password = %q", cfg.Redis.Password)
	}
	if cfg.Redis.DB != 2 {
		t.Errorf("DB = %d, want 2", cfg.Redis.DB)
	}
	if cfg.Redis.Prefix() != "prod:" {
		t.Errorf("Prefix = %q, want prod:", cfg.Redis.Prefix())
	}
}

func TestLoadRedisDefaults(t *testing.T) {
	cfg, err := config.Load(writeTempConfig(t, `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
redis: {}
backends: [{name: "b", transport: "http", url: "http://x"}]
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Redis == nil {
		t.Fatal("Redis = nil, want configured")
	}
	if cfg.Redis.Address() != "localhost:6379" {
		t.Errorf("Address = %q, want localhost:6379", cfg.Redis.Address())
	}
	if cfg.Redis.Prefix() != "mcpgate:" {
		t.Errorf("Prefix = %q, want mcpgate:", cfg.Redis.Prefix())
	}
}

func TestLoadInvalidRedis(t *testing.T) {
	cases := map[string]string{
		"redis addr without port": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
redis: {addr: "localhost"}
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"redis negative db": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
redis: {db: -1}
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
	}
	for name, yaml := range cases {
		if _, err := config.Load(writeTempConfig(t, yaml)); err == nil {
			t.Errorf("%s: Load succeeded, want error", name)
		}
	}
}

func TestLoadInvalid(t *testing.T) {
	cases := map[string]string{
		"empty listen": `
listen: ""
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"no api keys": `
listen: ":8080"
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"duplicate api key": `
listen: ":8080"
api_keys: [{key: "k", name: "a"}, {key: "k", name: "b"}]
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"duplicate api key name": `
listen: ":8080"
api_keys: [{key: "k1", name: "a"}, {key: "k2", name: "a"}]
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"no backends": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
`,
		"duplicate backend": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends:
  - {name: "b", transport: "http", url: "http://x"}
  - {name: "b", transport: "http", url: "http://y"}
`,
		"bad transport": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "grpc", url: "http://x"}]
`,
		"stdio without command": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "stdio"}]
`,
		"http without url": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http"}]
`,
		"http url without scheme": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", url: "localhost:9000/mcp"}]
`,
		"http url wrong scheme": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", url: "ftp://localhost:9000/mcp"}]
`,
		"http url without host": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", url: "http:///mcp"}]
`,
		"zero rate": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
default_rate_limit: {requests_per_second: 0, burst: 1}
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"bad duration": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", url: "http://x", timeout: "soon"}]
`,
		"bad auth type": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", url: "http://x", auth: {type: "oauth"}}]
`,
		"oidc missing issuer": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
oidc: {audience: "mcpgate"}
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"oidc missing audience": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
oidc: {issuer: "https://accounts.example.com"}
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"oidc non-https issuer": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
oidc: {issuer: "http://accounts.example.com", audience: "mcpgate"}
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"oidc relative issuer": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
oidc: {issuer: "accounts.example.com", audience: "mcpgate"}
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"oidc negative clock skew": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
oidc: {issuer: "https://accounts.example.com", audience: "mcpgate", clock_skew: "-5s"}
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"oauth missing introspection_url": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
oauth: {client_id: "gateway", client_secret: "s3cret"}
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"oauth missing client_id": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
oauth: {introspection_url: "https://auth.example.com/introspect", client_secret: "s3cret"}
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"oauth missing client_secret": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
oauth: {introspection_url: "https://auth.example.com/introspect", client_id: "gateway"}
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"oauth non-https introspection_url": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
oauth: {introspection_url: "http://auth.example.com/introspect", client_id: "gateway", client_secret: "s3cret"}
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"oauth negative cache ttl": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
oauth: {introspection_url: "https://auth.example.com/introspect", client_id: "gateway", client_secret: "s3cret", cache_ttl: "-5s"}
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"headers on stdio backend": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "stdio", command: ["./x"], headers: {"X-Gateway": "mcpgate"}}]
`,
		"tool_scopes empty scope list": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
tool_scopes: {drop_table: []}
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"tool_scopes scope with whitespace": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
tool_scopes: {drop_table: ["tools: admin"]}
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"api key scope with whitespace": `
listen: ":8080"
api_keys: [{key: "k", name: "n", scopes: ["tools: read"]}]
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"api key empty scope": `
listen: ":8080"
api_keys: [{key: "k", name: "n", scopes: [""]}]
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"schema rewrite without tool": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", url: "http://x", schema_rewrites: [{rename_arguments: {a: first}}]}]
`,
		"schema rewrite duplicate tool": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", url: "http://x", schema_rewrites: [{tool: "add"}, {tool: "add"}]}]
`,
		"schema rewrite rename to itself": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", url: "http://x", schema_rewrites: [{tool: "add", rename_arguments: {a: a}}]}]
`,
		"schema rewrite duplicate rename target": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", url: "http://x", schema_rewrites: [{tool: "add", rename_arguments: {a: x, b: x}}]}]
`,
		"schema rewrite rename chain": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", url: "http://x", schema_rewrites: [{tool: "add", rename_arguments: {a: b, b: c}}]}]
`,
		"schema rewrite renamed and hidden": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", url: "http://x", schema_rewrites: [{tool: "add", rename_arguments: {a: first}, hide_arguments: [a]}]}]
`,
		"schema rewrite duplicate hidden": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", url: "http://x", schema_rewrites: [{tool: "add", hide_arguments: [debug, debug]}]}]
`,
		"negative health_check interval": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", url: "http://x", health_check: {interval: "-1s"}}]
`,
		"negative health_check timeout": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", url: "http://x", health_check: {interval: "30s", timeout: "-1s"}}]
`,
		"negative health_check failure_threshold": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", url: "http://x", health_check: {interval: "30s", failure_threshold: -1}}]
`,
		"url and endpoints together": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", url: "http://x", endpoints: ["http://y", "http://z"]}]
`,
		"endpoints with bad url": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", endpoints: ["http://y", "localhost:9000"]}]
`,
		"endpoints with duplicate url": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", endpoints: ["http://y", "http://y"]}]
`,
		"endpoints on stdio backend": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "stdio", command: ["./x"], endpoints: ["http://y"]}]
`,
		"strategy on stdio backend": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "stdio", command: ["./x"], strategy: "round_robin"}]
`,
		"unknown strategy": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "http", endpoints: ["http://y", "http://z"], strategy: "least_connections"}]
`,
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := config.Load(writeTempConfig(t, yaml)); err == nil {
				t.Errorf("Load(%s) = nil error, want an error", name)
			}
		})
	}
}

func TestLoadUnknownField(t *testing.T) {
	cases := map[string]string{
		"unknown top-level field": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
listenn: ":9090"
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
		"typo in backend tool filter": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends:
  - name: "b"
    transport: "http"
    url: "http://x"
    tool_fiter:
      deny: ["dangerous"]
`,
		"typo in rate limit": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
default_rate_limit: {requests_per_second: 10, brust: 20}
backends: [{name: "b", transport: "http", url: "http://x"}]
`,
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := config.Load(writeTempConfig(t, yaml)); err == nil {
				t.Errorf("Load(%s) = nil error, want an error for unknown field", name)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := config.Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("Load(missing) = nil error, want an error")
	}
}

func TestLoadValidToolFilterAndHeaders(t *testing.T) {
	yaml := `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends:
  - name: "b"
    transport: "http"
    url: "http://localhost:9000/mcp"
    headers:
      X-Gateway: "mcpgate"
      X-Tenant: "acme"
    tool_filter:
      allow: ["echo", "add"]
      deny: ["add"]
`
	cfg, err := config.Load(writeTempConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b, ok := cfg.FindBackend("b")
	if !ok {
		t.Fatal("FindBackend(b) not found")
	}
	if b.Headers["X-Gateway"] != "mcpgate" || b.Headers["X-Tenant"] != "acme" {
		t.Errorf("Headers = %v, want injected gateway headers", b.Headers)
	}
	if len(b.ToolFilter.Allow) != 2 || b.ToolFilter.Allow[0] != "echo" {
		t.Errorf("ToolFilter.Allow = %v, want [echo add]", b.ToolFilter.Allow)
	}
	if len(b.ToolFilter.Deny) != 1 || b.ToolFilter.Deny[0] != "add" {
		t.Errorf("ToolFilter.Deny = %v, want [add]", b.ToolFilter.Deny)
	}
}

func TestLoadValidSchemaRewrites(t *testing.T) {
	yaml := `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends:
  - name: "b"
    transport: "http"
    url: "http://localhost:9000/mcp"
    schema_rewrites:
      - tool: "add"
        rename_arguments: {a: "first", b: "second"}
        hide_arguments: ["debug"]
      - tool: "echo"
        rename_arguments: {text: "message"}
`
	cfg, err := config.Load(writeTempConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b, ok := cfg.FindBackend("b")
	if !ok {
		t.Fatal("FindBackend(b) not found")
	}
	if len(b.SchemaRewrites) != 2 {
		t.Fatalf("SchemaRewrites = %d, want 2", len(b.SchemaRewrites))
	}
	rw := b.SchemaRewrites[0]
	if rw.Tool != "add" || rw.RenameArguments["a"] != "first" || rw.RenameArguments["b"] != "second" {
		t.Errorf("SchemaRewrites[0] = %+v, want add with a->first, b->second", rw)
	}
	if len(rw.HideArguments) != 1 || rw.HideArguments[0] != "debug" {
		t.Errorf("HideArguments = %v, want [debug]", rw.HideArguments)
	}
}

func TestLoadValidHealthCheck(t *testing.T) {
	yaml := `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends:
  - name: "b"
    transport: "http"
    url: "http://localhost:9000/mcp"
    health_check:
      interval: 30s
      timeout: 5s
      failure_threshold: 3
  - name: "c"
    transport: "http"
    url: "http://localhost:9001/mcp"
`
	cfg, err := config.Load(writeTempConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b, ok := cfg.FindBackend("b")
	if !ok {
		t.Fatal("FindBackend(b) not found")
	}
	if b.HealthCheck.Interval != config.Duration(30*time.Second) ||
		b.HealthCheck.Timeout != config.Duration(5*time.Second) ||
		b.HealthCheck.FailureThreshold != 3 {
		t.Errorf("HealthCheck = %+v, want 30s/5s/3", b.HealthCheck)
	}
	// A backend without health_check stays zero-valued (disabled).
	c, ok := cfg.FindBackend("c")
	if !ok {
		t.Fatal("FindBackend(c) not found")
	}
	if c.HealthCheck.Interval != 0 {
		t.Errorf("HealthCheck.Interval = %v, want 0 (disabled)", c.HealthCheck.Interval)
	}
}

func TestLoadValidEndpoints(t *testing.T) {
	yaml := `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends:
  - name: "pooled"
    transport: "http"
    endpoints:
      - "http://localhost:9000/mcp"
      - "http://localhost:9001/mcp"
    strategy: "round_robin"
  - name: "single"
    transport: "http"
    url: "http://localhost:9002/mcp"
`
	cfg, err := config.Load(writeTempConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b, ok := cfg.FindBackend("pooled")
	if !ok {
		t.Fatal("FindBackend(pooled) not found")
	}
	if len(b.Endpoints) != 2 || b.Endpoints[0] != "http://localhost:9000/mcp" || b.Endpoints[1] != "http://localhost:9001/mcp" {
		t.Errorf("Endpoints = %v, want [http://localhost:9000/mcp http://localhost:9001/mcp]", b.Endpoints)
	}
	if b.Strategy != config.StrategyRoundRobin {
		t.Errorf("Strategy = %q, want %q", b.Strategy, config.StrategyRoundRobin)
	}
	// A backend without endpoints keeps the single-url behavior.
	s, ok := cfg.FindBackend("single")
	if !ok {
		t.Fatal("FindBackend(single) not found")
	}
	if len(s.Endpoints) != 0 || s.URL != "http://localhost:9002/mcp" {
		t.Errorf("single backend = url %q endpoints %v, want url only", s.URL, s.Endpoints)
	}
}

func TestToolFilterDisabled(t *testing.T) {
	var empty config.ToolFilter
	if !empty.Disabled() {
		t.Error("empty ToolFilter should be Disabled")
	}
	cfg := config.ToolFilter{Allow: []string{"echo"}}
	if cfg.Disabled() {
		t.Error("ToolFilter with allow list should not be Disabled")
	}
	cfg = config.ToolFilter{Deny: []string{"drop"}}
	if cfg.Disabled() {
		t.Error("ToolFilter with deny list should not be Disabled")
	}
}

func TestLoadValidOIDC(t *testing.T) {
	yaml := `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
default_rate_limit: {requests_per_second: 10, burst: 20}
oidc:
  issuer: "https://accounts.example.com"
  audience: "mcpgate"
  clock_skew: 30s
backends: [{name: "b", transport: "http", url: "http://x"}]
`
	cfg, err := config.Load(writeTempConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OIDC == nil {
		t.Fatal("OIDC = nil, want configured")
	}
	if cfg.OIDC.Issuer != "https://accounts.example.com" || cfg.OIDC.Audience != "mcpgate" {
		t.Errorf("OIDC = %+v, want issuer/audience set", cfg.OIDC)
	}
	if time.Duration(cfg.OIDC.ClockSkew) != 30*time.Second {
		t.Errorf("ClockSkew = %v, want 30s", cfg.OIDC.ClockSkew)
	}
}

func TestLoadOIDCLoopbackHTTPAllowed(t *testing.T) {
	yaml := `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
default_rate_limit: {requests_per_second: 10, burst: 20}
oidc:
  issuer: "http://127.0.0.1:8081"
  audience: "mcpgate"
backends: [{name: "b", transport: "http", url: "http://x"}]
`
	if _, err := config.Load(writeTempConfig(t, yaml)); err != nil {
		t.Errorf("Load with loopback http issuer: %v", err)
	}
}
