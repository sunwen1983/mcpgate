package gateway

import (
	"testing"
	"time"

	"github.com/sunwen1983/mcpgate/internal/config"
)

// diffConfigs is unexported, so its table test lives in this internal
// test file; the HTTP-level reload tests live in reload_test.go.
func TestDiffConfigs(t *testing.T) {
	base := &config.Config{
		Listen: ":0",
		APIKeys: []config.APIKey{
			{Key: "key-alice", Name: "alice"},
			{Key: "key-bob", Name: "bob"},
		},
		DefaultRateLimit: config.RateLimit{Rate: 100, Burst: 100},
		Backends: []config.Backend{
			{
				Name:      "fake",
				Transport: "http",
				URL:       "http://localhost:1/unused",
				Timeout:   config.Duration(5 * time.Second),
				Cache: config.CacheConfig{
					ToolsListTTL:   config.Duration(time.Minute),
					ToolsCallTTL:   config.Duration(time.Minute),
					CacheableTools: []string{"echo"},
				},
				Breaker: config.BreakerConfig{FailureThreshold: 2, Cooldown: config.Duration(time.Minute)},
			},
		},
	}
	with := func(mut func(*config.Config)) *config.Config {
		c := *base
		c.APIKeys = append([]config.APIKey(nil), base.APIKeys...)
		c.Backends = append([]config.Backend(nil), base.Backends...)
		mut(&c)
		return &c
	}
	containsStr := func(list []string, want string) bool {
		for _, s := range list {
			if s == want {
				return true
			}
		}
		return false
	}
	cases := []struct {
		name        string
		new         *config.Config
		wantReload  []string
		wantRestart []string
	}{
		{"no change", with(func(*config.Config) {}), nil, nil},
		{"listen change", with(func(c *config.Config) { c.Listen = ":9090" }),
			nil, []string{"listen"}},
		{"api key added", with(func(c *config.Config) {
			c.APIKeys = append(c.APIKeys, config.APIKey{Key: "key-carol", Name: "carol"})
		}), []string{"api_keys"}, nil},
		{"tool scopes change", with(func(c *config.Config) {
			c.ToolScopes = map[string][]string{"echo": {"tools:read"}}
		}), []string{"tool_scopes"}, nil},
		{"default rate limit change", with(func(c *config.Config) {
			c.DefaultRateLimit = config.RateLimit{Rate: 50, Burst: 50}
		}), []string{"default_rate_limit"}, nil},
		{"tls change", with(func(c *config.Config) {
			c.TLS = &config.TLSConfig{CertFile: "cert.pem", KeyFile: "key.pem"}
		}), nil, []string{"tls"}},
		{"redis change", with(func(c *config.Config) {
			c.Redis = &config.RedisConfig{Addr: "localhost:6379"}
		}), nil, []string{"redis"}},
		{"backend added", with(func(c *config.Config) {
			c.Backends = append(c.Backends, config.Backend{Name: "second", Transport: "http", URL: "http://localhost:2"})
		}), nil, []string{"backend second added"}},
		{"backend removed", with(func(c *config.Config) {
			c.Backends = nil
		}), nil, []string{"backend fake removed"}},
		{"backend timeout change", with(func(c *config.Config) {
			c.Backends[0].Timeout = config.Duration(10 * time.Second)
		}), nil, []string{"backend fake: timeout changed"}},
		{"backend url change", with(func(c *config.Config) {
			c.Backends[0].URL = "http://localhost:2/unused"
		}), nil, []string{"backend fake: url changed"}},
		{"backend auth change", with(func(c *config.Config) {
			c.Backends[0].Auth = config.BackendAuth{Type: "bearer", Token: "x"}
		}), nil, []string{"backend fake: auth changed"}},
		{"backend tool filter change", with(func(c *config.Config) {
			c.Backends[0].ToolFilter = config.ToolFilter{Deny: []string{"echo"}}
		}), []string{"backends.fake.tool_filter"}, nil},
		{"backend breaker change", with(func(c *config.Config) {
			c.Backends[0].Breaker.FailureThreshold = 5
		}), []string{"backends.fake.breaker"}, nil},
		{"backend cache change", with(func(c *config.Config) {
			c.Backends[0].Cache.ToolsListTTL = config.Duration(2 * time.Minute)
		}), []string{"backends.fake.cache"}, nil},
		{"backend health check change", with(func(c *config.Config) {
			c.Backends[0].HealthCheck.Interval = config.Duration(time.Second)
		}), []string{"backends.fake.health_check"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := diffConfigs(base, tc.new)
			for _, want := range tc.wantReload {
				if !containsStr(d.reloaded, want) {
					t.Errorf("reloaded = %v, want %q", d.reloaded, want)
				}
			}
			for _, want := range tc.wantRestart {
				if !containsStr(d.requiresRestart, want) {
					t.Errorf("requires_restart = %v, want %q", d.requiresRestart, want)
				}
			}
			if len(tc.wantReload) == 0 && len(d.reloaded) != 0 {
				t.Errorf("reloaded = %v, want empty", d.reloaded)
			}
			if len(tc.wantRestart) == 0 && len(d.requiresRestart) != 0 {
				t.Errorf("requires_restart = %v, want empty", d.requiresRestart)
			}
		})
	}
}
