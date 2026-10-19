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
		"headers on stdio backend": `
listen: ":8080"
api_keys: [{key: "k", name: "n"}]
backends: [{name: "b", transport: "stdio", command: ["./x"], headers: {"X-Gateway": "mcpgate"}}]
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
