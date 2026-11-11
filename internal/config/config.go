// Package config loads and validates mcpgate's YAML configuration.
package config

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
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

// RedisConfig configures the shared Redis store behind distributed rate
// limiting. When it is set, every gateway replica enforces one shared
// token budget per API key instead of a per-process budget; when it is
// absent, rate limiting stays in-process (the previous behavior), so a
// single-replica deployment needs no Redis at all.
//
// A Redis that is configured but unreachable at startup does not stop
// the gateway: it logs a warning and falls back to in-process limiters
// until the next restart. A Redis error on an individual request fails
// open (the request is allowed) and is counted in the
// mcpgate_rate_limit_errors_total metric, so a Redis outage degrades
// limiting instead of taking the gateway down.
type RedisConfig struct {
	// Addr is the Redis address, e.g. "localhost:6379". Empty means
	// "localhost:6379".
	Addr string `yaml:"addr"`
	// Password authenticates to Redis (AUTH). Empty means no password.
	// Never logged.
	Password string `yaml:"password,omitempty"`
	// DB is the Redis logical database. Zero means database 0.
	DB int `yaml:"db"`
	// KeyPrefix namespaces mcpgate's keys in a Redis shared with other
	// applications. Empty means "mcpgate:".
	KeyPrefix string `yaml:"key_prefix"`
}

// Address returns the Redis address, defaulting to localhost:6379.
func (r *RedisConfig) Address() string {
	if r.Addr == "" {
		return "localhost:6379"
	}
	return r.Addr
}

// Prefix returns the key prefix, defaulting to "mcpgate:".
func (r *RedisConfig) Prefix() string {
	if r.KeyPrefix == "" {
		return "mcpgate:"
	}
	return r.KeyPrefix
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

// HealthCheckConfig controls active health probing for one backend.
// The gateway probes the backend with a plain tools/list call — the
// lightest request that exercises exactly the path real client traffic
// needs — and marks the backend unhealthy after FailureThreshold
// consecutive failed probes. An unhealthy backend fails fast (503)
// until a probe succeeds again, so client traffic stops going to a
// backend that is down instead of timing out against it.
//
// Probing is opt-in: a zero Interval disables it (the default), so
// existing configurations behave exactly as before.
type HealthCheckConfig struct {
	// Interval is how often the gateway probes the backend. Zero
	// disables health checks.
	Interval Duration `yaml:"interval"`
	// Timeout bounds one probe. Zero means 5s.
	Timeout Duration `yaml:"timeout"`
	// FailureThreshold is the number of consecutive failed probes that
	// marks the backend unhealthy. Zero means 3.
	FailureThreshold int `yaml:"failure_threshold"`
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

// ToolFilter restricts which tools of a backend clients can see and call.
// Deny takes precedence over Allow: a tool named in both is blocked. An
// empty Allow list means every tool is exposed unless it is denied.
type ToolFilter struct {
	// Allow lists the only tools clients may see and call. Empty means
	// every tool is allowed unless it is in Deny.
	Allow []string `yaml:"allow"`
	// Deny lists tools hidden from tools/list and rejected on tools/call.
	Deny []string `yaml:"deny"`
}

// Disabled reports whether the filter has no effect at all.
func (f ToolFilter) Disabled() bool { return len(f.Allow) == 0 && len(f.Deny) == 0 }

// SchemaRewrite reshapes one backend tool's input schema as exposed to
// clients, and translates tools/call arguments back to the backend's
// names. Tool names are never rewritten: the tool filter, per-tool
// authorization, and cache keys all keep using the backend's tool
// name.
//
//   - RenameArguments maps a backend argument name to the name clients
//     see in tools/list (properties and required). On tools/call the
//     client-facing name is translated back before forwarding.
//   - HideArguments lists backend arguments removed from the exposed
//     schema; a client that sends one anyway has it stripped from the
//     call before forwarding.
//
// A rewrite may name an argument the backend does not expose; that
// part is a no-op, not an error — backends can serve slightly
// different schemas. Do not hide an argument the backend requires
// unless the backend tolerates its absence: the gateway does not
// inject a value for it, so the call would fail at the backend.
type SchemaRewrite struct {
	// Tool is the backend tool name this rewrite applies to.
	Tool string `yaml:"tool"`
	// RenameArguments maps backend argument name -> client-facing name.
	RenameArguments map[string]string `yaml:"rename_arguments,omitempty"`
	// HideArguments lists backend argument names hidden from clients.
	HideArguments []string `yaml:"hide_arguments,omitempty"`
}

// StrategyRoundRobin is the only load-balancing strategy so far:
// requests are spread across a backend's endpoints in turn, skipping
// endpoints currently marked down. Empty means round_robin.
const StrategyRoundRobin = "round_robin"

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
	// Endpoints lists the endpoints of a pooled http backend. When
	// set, URL must be empty and requests are load-balanced across
	// these endpoints (see Strategy). Only valid for transport
	// "http". Empty means a single endpoint from URL.
	Endpoints []string `yaml:"endpoints,omitempty"`
	// Strategy selects the load-balancing strategy across Endpoints.
	// Only "round_robin" is supported; empty means round_robin.
	Strategy string `yaml:"strategy,omitempty"`
	// Auth authenticates the gateway to the backend.
	Auth BackendAuth `yaml:"auth"`
	// Headers are static headers injected into every request sent to an
	// http backend (e.g. "X-Gateway": "mcpgate"). Backend auth always
	// wins over a configured header with the same name. Only valid for
	// transport "http".
	Headers map[string]string `yaml:"headers"`
	// ToolFilter hides backend tools from clients: denied tools are
	// removed from tools/list results and rejected on tools/call.
	ToolFilter ToolFilter `yaml:"tool_filter"`
	// SchemaRewrites reshapes tool input schemas as exposed to
	// clients: rename arguments, hide arguments. See SchemaRewrite.
	SchemaRewrites []SchemaRewrite `yaml:"schema_rewrites,omitempty"`
	// Timeout bounds a single backend request. Zero means 10s.
	Timeout Duration `yaml:"timeout"`
	// Cache configures response caching for this backend.
	Cache CacheConfig `yaml:"cache"`
	// Breaker configures the circuit breaker for this backend.
	Breaker BreakerConfig `yaml:"breaker"`
	// HealthCheck configures active health probing for this backend.
	// A zero Interval disables probing.
	HealthCheck HealthCheckConfig `yaml:"health_check"`
}

// APIKey is one client credential. The Key itself is never logged; only
// Name appears in logs and metrics.
//
// Scopes restricts which scope-gated tools (see Config.ToolScopes) this
// key may call. An empty list means the key carries no scopes: it may
// still call every tool that is not scope-gated, exactly as before.
type APIKey struct {
	Key  string `yaml:"key"`
	Name string `yaml:"name"`
	// Scopes lists the OAuth-style scopes this key holds. Scope names
	// are single tokens (no whitespace).
	Scopes []string `yaml:"scopes,omitempty"`
	// RateLimit overrides the global default for this key. Nil means
	// use the global default.
	RateLimit *RateLimit `yaml:"rate_limit"`
}

// OIDCConfig enables JWT bearer-token client authentication against an
// OpenID Connect issuer, as an alternative to API keys. Optional; when
// absent, API keys are the only accepted client credentials.
//
// A request carrying "Authorization: Bearer <jwt>" is accepted when the
// token's signature verifies against the issuer's JWKS and its iss, aud,
// exp, and nbf claims check out. The token's sub claim becomes the
// identity used in logs, metrics, and rate limiting ("oidc:<sub>").
type OIDCConfig struct {
	// Issuer is the OIDC issuer URL, e.g. "https://accounts.example.com".
	// The gateway fetches "<issuer>/.well-known/openid-configuration" to
	// discover the JWKS endpoint, unless JWKSURL is set.
	Issuer string `yaml:"issuer"`
	// Audience is the required "aud" claim value (the gateway's client ID
	// as registered with the issuer).
	Audience string `yaml:"audience"`
	// JWKSURL overrides the JWKS endpoint discovered from the issuer.
	// Useful for providers without a discovery document, and for tests.
	JWKSURL string `yaml:"jwks_url,omitempty"`
	// ClockSkew is the tolerated clock difference when checking exp/nbf.
	// Zero means 60s.
	ClockSkew Duration `yaml:"clock_skew,omitempty"`
}

// OAuthConfig enables OAuth2 token introspection (RFC 7662) for client
// authentication, as an alternative to API keys and OIDC JWTs. Optional;
// when absent, opaque Bearer tokens are not accepted.
//
// A request carrying "Authorization: Bearer <token>" is accepted when the
// authorization server reports the token active at the introspection
// endpoint. The token's sub claim becomes the identity used in logs,
// metrics, and rate limiting ("oauth:<sub>"). The scope claim is parsed
// and returned with the introspection result; scope enforcement
// (per-tool authorization) is configured with Config.ToolScopes.
type OAuthConfig struct {
	// IntrospectionURL is the RFC 7662 introspection endpoint of the
	// authorization server, e.g. "https://auth.example.com/oauth/introspect".
	IntrospectionURL string `yaml:"introspection_url"`
	// ClientID and ClientSecret are the gateway's credentials at the
	// introspection endpoint (HTTP Basic authentication, RFC 7662 §2.1).
	ClientID string `yaml:"client_id"`
	// ClientSecret is the gateway's credential at the introspection
	// endpoint. Like API keys, it is never logged.
	ClientSecret string `yaml:"client_secret"`
	// CacheTTL bounds how long a successful introspection result is
	// cached. The cache entry never outlives the token's exp claim.
	// Zero means 60s.
	CacheTTL Duration `yaml:"cache_ttl,omitempty"`
}

// Config is the whole mcpgate configuration file.
type Config struct {
	// Listen is the address the gateway binds, e.g. ":8080".
	Listen string `yaml:"listen"`
	// APIKeys lists the client credentials the gateway accepts.
	APIKeys []APIKey `yaml:"api_keys"`
	// DefaultRateLimit applies to keys without an override.
	DefaultRateLimit RateLimit `yaml:"default_rate_limit"`
	// Redis optionally enables distributed rate limiting backed by
	// Redis. Nil means rate limiting is in-process per replica.
	Redis *RedisConfig `yaml:"redis,omitempty"`
	// OIDC optionally enables JWT bearer-token client authentication.
	// Nil means API keys are the only accepted client credentials.
	OIDC *OIDCConfig `yaml:"oidc,omitempty"`
	// OAuth optionally enables OAuth2 token introspection for opaque
	// Bearer tokens. Nil means opaque Bearer tokens are not accepted.
	OAuth *OAuthConfig `yaml:"oauth,omitempty"`
	// ToolScopes gates individual tools behind OAuth-style scopes:
	// a tool named here may be called only by a client holding at
	// least one of the listed scopes; holding any one suffices.
	// Scopes come from the client's credential — an API key's
	// configured scopes, a JWT's scope/scp claim, or an
	// introspected token's scope claim. Tools not named here are
	// open to any authenticated client, so existing configs keep
	// working unchanged. A gated tool is also hidden from
	// tools/list for clients that lack a granting scope.
	ToolScopes map[string][]string `yaml:"tool_scopes,omitempty"`
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
//
// Decoding is strict: an unknown field is an error, not a silently
// ignored typo. That matters here because the security-relevant settings
// (tool_filter, rate limits, backend auth) all live in this file — a
// misspelled key that quietly fell back to its default could, for
// example, leave a tool exposed that the operator meant to deny.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
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
	seenNames := map[string]bool{}
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
		// Names must be unique too: rate limiters, logs, and metrics
		// are all keyed by name, so two keys sharing a name would
		// silently share one limiter and be indistinguishable in logs.
		if seenNames[k.Name] {
			return fmt.Errorf("api_keys[%d]: duplicate name %q", i, k.Name)
		}
		seenNames[k.Name] = true
		if k.RateLimit != nil {
			if err := validateRateLimit(*k.RateLimit); err != nil {
				return fmt.Errorf("api_keys[%d]: %w", i, err)
			}
		}
		if err := validateScopes(k.Scopes); err != nil {
			return fmt.Errorf("api_keys[%d]: %w", i, err)
		}
	}
	if err := validateToolScopes(c.ToolScopes); err != nil {
		return fmt.Errorf("tool_scopes: %w", err)
	}
	if err := validateRateLimit(c.DefaultRateLimit); err != nil {
		return fmt.Errorf("default_rate_limit: %w", err)
	}
	if c.Redis != nil {
		if err := validateRedis(c.Redis); err != nil {
			return fmt.Errorf("redis: %w", err)
		}
	}
	if c.OIDC != nil {
		if err := validateOIDC(c.OIDC); err != nil {
			return fmt.Errorf("oidc: %w", err)
		}
	}
	if c.OAuth != nil {
		if err := validateOAuth(c.OAuth); err != nil {
			return fmt.Errorf("oauth: %w", err)
		}
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
			if len(b.Endpoints) > 0 {
				return fmt.Errorf("backends[%d] (%s): endpoints are only valid for http transport", i, b.Name)
			}
			if b.Strategy != "" {
				return fmt.Errorf("backends[%d] (%s): strategy is only valid for http transport", i, b.Name)
			}
		case "http":
			if b.URL != "" && len(b.Endpoints) > 0 {
				return fmt.Errorf("backends[%d] (%s): url and endpoints are mutually exclusive", i, b.Name)
			}
			if b.URL == "" && len(b.Endpoints) == 0 {
				return fmt.Errorf("backends[%d] (%s): url or endpoints is required for http transport", i, b.Name)
			}
			if b.URL != "" {
				if err := validateBackendURL(b.URL); err != nil {
					return fmt.Errorf("backends[%d] (%s): %w", i, b.Name, err)
				}
			}
			seenEndpoints := map[string]bool{}
			for j, ep := range b.Endpoints {
				if err := validateBackendURL(ep); err != nil {
					return fmt.Errorf("backends[%d] (%s): endpoints[%d]: %w", i, b.Name, j, err)
				}
				if seenEndpoints[ep] {
					return fmt.Errorf("backends[%d] (%s): duplicate endpoint %q", i, b.Name, ep)
				}
				seenEndpoints[ep] = true
			}
			switch b.Strategy {
			case "", StrategyRoundRobin:
			default:
				return fmt.Errorf("backends[%d] (%s): unknown load balancing strategy %q", i, b.Name, b.Strategy)
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
		if b.HealthCheck.Interval < 0 {
			return fmt.Errorf("backends[%d] (%s): health_check interval must not be negative", i, b.Name)
		}
		if b.HealthCheck.Timeout < 0 {
			return fmt.Errorf("backends[%d] (%s): health_check timeout must not be negative", i, b.Name)
		}
		if b.HealthCheck.FailureThreshold < 0 {
			return fmt.Errorf("backends[%d] (%s): health_check failure_threshold must not be negative", i, b.Name)
		}
		if b.Auth.Type != "" && b.Auth.Type != "bearer" && b.Auth.Type != "basic" {
			return fmt.Errorf("backends[%d] (%s): auth type must be \"bearer\" or \"basic\"", i, b.Name)
		}
		if len(b.Headers) > 0 && b.Transport != "http" {
			return fmt.Errorf("backends[%d] (%s): headers are only valid for http transport", i, b.Name)
		}
		if err := validateSchemaRewrites(b.SchemaRewrites); err != nil {
			return fmt.Errorf("backends[%d] (%s): schema_rewrites: %w", i, b.Name, err)
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

// validateRedis requires a host:port address and a non-negative DB.
func validateRedis(r *RedisConfig) error {
	if r.Addr != "" {
		if _, _, err := net.SplitHostPort(r.Addr); err != nil {
			return fmt.Errorf("addr %q must be host:port: %w", r.Addr, err)
		}
	}
	if r.DB < 0 {
		return fmt.Errorf("db must not be negative")
	}
	return nil
}

// validateScopes requires every scope to be a single token: empty
// scopes and scopes containing whitespace are rejected, because scope
// matching is exact and a stray space would silently never match.
func validateScopes(scopes []string) error {
	for _, s := range scopes {
		if s == "" {
			return fmt.Errorf("scope must not be empty")
		}
		if strings.ContainsAny(s, " \t\r\n") {
			return fmt.Errorf("scope %q must not contain whitespace", s)
		}
	}
	return nil
}

// validateSchemaRewrites requires every rewrite to name a tool (once),
// and every rename to be unambiguous in both directions: a rename
// target must not collide with another rename's source (a chain such
// as a->b, b->c would make the client-facing name "b" mean two
// different backend arguments), and a rename's target must be unique.
// An argument cannot be both renamed and hidden. What the backend
// actually exposes is not checked here — a rewrite may name an
// argument the backend does not serve, and that part is a no-op.
func validateSchemaRewrites(rewrites []SchemaRewrite) error {
	seenTools := map[string]bool{}
	for _, rw := range rewrites {
		if rw.Tool == "" {
			return fmt.Errorf("tool must not be empty")
		}
		if seenTools[rw.Tool] {
			return fmt.Errorf("duplicate rewrite for tool %q", rw.Tool)
		}
		seenTools[rw.Tool] = true
		seenTargets := map[string]bool{}
		for old, new := range rw.RenameArguments {
			if old == "" || new == "" {
				return fmt.Errorf("tool %q: rename argument names must not be empty", rw.Tool)
			}
			if old == new {
				return fmt.Errorf("tool %q: rename %q to itself is a no-op", rw.Tool, old)
			}
			if seenTargets[new] {
				return fmt.Errorf("tool %q: duplicate rename target %q", rw.Tool, new)
			}
			seenTargets[new] = true
		}
		for _, target := range rw.RenameArguments {
			if _, isSource := rw.RenameArguments[target]; isSource {
				return fmt.Errorf("tool %q: rename target %q is also a rename source (rename chains are ambiguous)", rw.Tool, target)
			}
		}
		seenHidden := map[string]bool{}
		for _, h := range rw.HideArguments {
			if h == "" {
				return fmt.Errorf("tool %q: hidden argument names must not be empty", rw.Tool)
			}
			if seenHidden[h] {
				return fmt.Errorf("tool %q: duplicate hidden argument %q", rw.Tool, h)
			}
			seenHidden[h] = true
			if _, renamed := rw.RenameArguments[h]; renamed {
				return fmt.Errorf("tool %q: argument %q cannot be both renamed and hidden", rw.Tool, h)
			}
		}
	}
	return nil
}

// validateToolScopes requires every gated tool to name at least one
// scope, and every scope to be a single token. A tool with an empty
// scope list would be callable by nobody, which is what tool_filter's
// deny list is for — so it is a configuration error here.
func validateToolScopes(m map[string][]string) error {
	for tool, scopes := range m {
		if tool == "" {
			return fmt.Errorf("tool name must not be empty")
		}
		if len(scopes) == 0 {
			return fmt.Errorf("tool %q must list at least one scope", tool)
		}
		if err := validateScopes(scopes); err != nil {
			return fmt.Errorf("tool %q: %w", tool, err)
		}
	}
	return nil
}

// validateOIDC checks the OIDC client-auth configuration. Issuer and
// jwks_url must be absolute URLs; plain http is only accepted for loopback
// hosts (tests and local IdPs), production issuers must use https.
func validateOIDC(o *OIDCConfig) error {
	if o.Issuer == "" {
		return fmt.Errorf("issuer must not be empty")
	}
	if err := validateHTTPURL(o.Issuer, "issuer"); err != nil {
		return err
	}
	if o.Audience == "" {
		return fmt.Errorf("audience must not be empty")
	}
	if o.JWKSURL != "" {
		if err := validateHTTPURL(o.JWKSURL, "jwks_url"); err != nil {
			return err
		}
	}
	if o.ClockSkew < 0 {
		return fmt.Errorf("clock_skew must not be negative")
	}
	return nil
}

// validateOAuth checks the OAuth2 token-introspection configuration.
// The introspection URL must be absolute; plain http is only accepted
// for loopback hosts (tests and local authorization servers), production
// endpoints must use https — the token and the gateway's client secret
// travel in the request.
func validateOAuth(o *OAuthConfig) error {
	if o.IntrospectionURL == "" {
		return fmt.Errorf("introspection_url must not be empty")
	}
	if err := validateHTTPURL(o.IntrospectionURL, "introspection_url"); err != nil {
		return err
	}
	if o.ClientID == "" {
		return fmt.Errorf("client_id must not be empty")
	}
	if o.ClientSecret == "" {
		return fmt.Errorf("client_secret must not be empty")
	}
	if o.CacheTTL < 0 {
		return fmt.Errorf("cache_ttl must not be negative")
	}
	return nil
}

// validateHTTPURL requires an absolute http(s) URL, with https enforced
// for non-loopback hosts.
func validateHTTPURL(raw, field string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return fmt.Errorf("%s must be an absolute URL", field)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("%s must use http or https", field)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return fmt.Errorf("%s must use https (http is only allowed for loopback hosts)", field)
	}
	return nil
}

// validateBackendURL requires an absolute http(s) URL for an http
// backend. Unlike OIDC endpoints, backends commonly run over plaintext
// http on a private network, so https is not enforced here — but a
// missing scheme or host would fail opaquely at request time, so the
// config check fails fast with a clear message instead.
func validateBackendURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return fmt.Errorf("url must be an absolute URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("url must use http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("url must include a host")
	}
	return nil
}

// isLoopback reports whether host is a loopback name or address.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
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
