// Package config loads and validates mcpgate's YAML configuration.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from a YAML string such as
// "30s", "5m", or "0" (zero disables the feature it configures).
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// Seconds returns the duration in seconds as a float.
func (d Duration) Seconds() float64 { return time.Duration(d).Seconds() }

// RateLimit is a token-bucket configuration: up to Burst tokens may be
// consumed at once, refilling at Rate tokens per second.
type RateLimit struct {
	Rate  float64 `yaml:"requests_per_second"`
	Burst int     `yaml:"burst"`
}

// CacheConfig controls response caching for one backend.
type CacheConfig struct {
	// ToolsListTTL caches tools/list responses. Zero disables.
	ToolsListTTL Duration `yaml:"tools_list_ttl"`
	// ToolsCallTTL caches tools/call responses for the tools named in
	// CacheableTools. Zero disables.
	ToolsCallTTL Duration `yaml:"tools_call_ttl"`
	// CacheableTools lists the tool names whose calls may be cached.
	// Tool calls can have side effects, so nothing is cached unless the
	// tool is explicitly listed here.
	CacheableTools []string `yaml:"cacheable_tools"`
}

// BreakerConfig controls the per-backend circuit breaker.
type BreakerConfig struct {
	// FailureThreshold is the number of consecutive backend failures that
	// trips the breaker open.
	FailureThreshold int `yaml:"failure_threshold"`
	// Cooldown is how long the breaker stays open before allowing a probe.
	Cooldown Duration `yaml:"cooldown"`
}

// BackendAuth carries credentials used to authenticate the gateway to a
// backend. Values are never logged.
type BackendAuth struct {
	// Type is "bearer" or "basic". Empty means no auth.
	Type string `yaml:"type"`
	// Token is the bearer token (type "bearer").
	Token string `yaml:"token"`
	// Username and Password are used for type "basic".
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Backend describes one upstream MCP server.
type Backend struct {
	Name string `yaml:"name"`
	// Transport is "stdio" or "http".
	Transport string `yaml:"transport"`
	// Command is the executable (plus arguments) for stdio backends.
	Command []string `yaml:"command"`
	// Env holds extra environment variables for stdio backends.
	Env map[string]string `yaml:"env"`
	// URL is the endpoint for http backends (Streamable HTTP).
	URL string `yaml:"url"`
	// Auth authenticates the gateway to the backend.
	Auth BackendAuth `yaml:"auth"`
	// Timeout bounds a single backend request. Zero means 10s.
	Timeout Duration `yaml:"timeout"`
	// Cache configures response caching for this backend.
	Cache CacheConfig `yaml:"cache"`
	// Breaker configures the circuit breaker for this backend.
	Breaker BreakerConfig `yaml:"breaker"`
}

// APIKey is one client credential. The Key itself is never logged; only
// Name appears in logs and metrics.
type APIKey struct {
	Key  string `yaml:"key"`
	Name string `yaml:"name"`
	// RateLimit overrides the global default for this key. Nil means
	// use the global default.
	RateLimit *RateLimit `yaml:"rate_limit"`
}

// Config is the whole mcpgate configuration file.
type Config struct {
	// Listen is the address the gateway binds, e.g. ":8080".
	Listen string `yaml:"listen"`
	// APIKeys lists the client credentials the gateway accepts.
	APIKeys []APIKey `yaml:"api_keys"`
	// DefaultRateLimit applies to keys without an override.
	DefaultRateLimit RateLimit `yaml:"default_rate_limit"`
	// Backends lists the upstream MCP servers.
	Backends []Backend `yaml:"backends"`
}

// Default returns a Config with sane production defaults filled in.
func Default() *Config {
	return &Config{
		Listen: ":8080",
		DefaultRateLimit: RateLimit{
			Rate:  10,
			Burst: 20,
		},
	}
}

// Load reads and validates the YAML configuration at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks the configuration for contradictions and missing fields.
func (c *Config) Validate() error {
	if c.Listen == "" {
		return fmt.Errorf("listen must not be empty")
	}
	if len(c.APIKeys) == 0 {
		return fmt.Errorf("at least one api_key is required")
	}
	seenKeys := map[string]bool{}
	for i, k := range c.APIKeys {
		if k.Key == "" {
			return fmt.Errorf("api_keys[%d]: key must not be empty", i)
		}
		if seenKeys[k.Key] {
			return fmt.Errorf("api_keys[%d]: duplicate key", i)
		}
		seenKeys[k.Key] = true
		if k.Name == "" {
			return fmt.Errorf("api_keys[%d]: name must not be empty", i)
		}
		if k.RateLimit != nil {
			if err := validateRateLimit(*k.RateLimit); err != nil {
				return fmt.Errorf("api_keys[%d]: %w", i, err)
			}
		}
	}
	if err := validateRateLimit(c.DefaultRateLimit); err != nil {
		return fmt.Errorf("default_rate_limit: %w", err)
	}
	if len(c.Backends) == 0 {
		return fmt.Errorf("at least one backend is required")
	}
	seenBackends := map[string]bool{}
	for i, b := range c.Backends {
		if b.Name == "" {
			return fmt.Errorf("backends[%d]: name must not be empty", i)
		}
		if seenBackends[b.Name] {
			return fmt.Errorf("backends[%d]: duplicate name %q", i, b.Name)
		}
		seenBackends[b.Name] = true
		switch b.Transport {
		case "stdio":
			if len(b.Command) == 0 {
				return fmt.Errorf("backends[%d] (%s): command is required for stdio transport", i, b.Name)
			}
		case "http":
			if b.URL == "" {
				return fmt.Errorf("backends[%d] (%s): url is required for http transport", i, b.Name)
			}
		default:
			return fmt.Errorf("backends[%d] (%s): transport must be \"stdio\" or \"http\"", i, b.Name)
		}
		if b.Timeout < 0 {
			return fmt.Errorf("backends[%d] (%s): timeout must not be negative", i, b.Name)
		}
		if b.Cache.ToolsListTTL < 0 || b.Cache.ToolsCallTTL < 0 {
			return fmt.Errorf("backends[%d] (%s): cache TTLs must not be negative", i, b.Name)
		}
		if b.Breaker.FailureThreshold < 0 {
			return fmt.Errorf("backends[%d] (%s): breaker failure_threshold must not be negative", i, b.Name)
		}
		if b.Breaker.Cooldown < 0 {
			return fmt.Errorf("backends[%d] (%s): breaker cooldown must not be negative", i, b.Name)
		}
		if b.Auth.Type != "" && b.Auth.Type != "bearer" && b.Auth.Type != "basic" {
			return fmt.Errorf("backends[%d] (%s): auth type must be \"bearer\" or \"basic\"", i, b.Name)
		}
	}
	return nil
}

func validateRateLimit(r RateLimit) error {
	if r.Rate <= 0 {
		return fmt.Errorf("requests_per_second must be positive")
	}
	if r.Burst <= 0 {
		return fmt.Errorf("burst must be positive")
	}
	return nil
}

// FindBackend returns the backend with the given name, or false.
func (c *Config) FindBackend(name string) (Backend, bool) {
	for _, b := range c.Backends {
		if b.Name == name {
			return b, true
		}
	}
	return Backend{}, false
}

// FindAPIKey returns the API key record matching key, or false.
func (c *Config) FindAPIKey(key string) (APIKey, bool) {
	for _, k := range c.APIKeys {
		if k.Key == key {
			return k, true
		}
	}
	return APIKey{}, false
}

// RateLimitFor returns the effective rate limit for an API key.
func (c *Config) RateLimitFor(k APIKey) RateLimit {
	if k.RateLimit != nil {
		return *k.RateLimit
	}
	return c.DefaultRateLimit
}
