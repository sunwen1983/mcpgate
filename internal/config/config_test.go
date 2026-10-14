package config_test

import (
	"os"
	"path/filepath"
	"testing"

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
