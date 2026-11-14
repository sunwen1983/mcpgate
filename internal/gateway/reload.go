// Config reload and runtime status for the admin API.
//
// POST /admin/reload re-reads the gateway's config file, validates it
// with the same strict checks as startup, and applies the reloadable
// parts live — API keys, per-tool scopes, rate limits, and per-backend
// tool filters, schema rewrites, cache settings, breaker settings, and
// health-check settings. Everything else (listen address, TLS, Redis,
// OIDC/OAuth endpoints, and a backend's connection identity —
// transport, command, URL, endpoints, auth, headers, timeout) cannot
// change under a running gateway and is reported in requires_restart
// instead of being silently ignored: the operator restarts to pick
// those up. An invalid config is rejected whole — the running config
// keeps serving untouched.
//
// GET /admin/status reports the runtime state an operator needs after
// a reload: uptime, when the config was last loaded, the API key names,
// and every backend's breaker and health state. Key values and backend
// credentials never appear in either response.
package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/sunwen1983/mcpgate/internal/auth"
	"github.com/sunwen1983/mcpgate/internal/breaker"
	"github.com/sunwen1983/mcpgate/internal/config"
	"github.com/sunwen1983/mcpgate/internal/metrics"
)

// configDiff is the result of comparing the running config with a newly
// loaded one.
type configDiff struct {
	reloaded        []string
	requiresRestart []string
	// backendUpdates holds the new config for every backend whose
	// reloadable fields changed (keyed by backend name).
	backendUpdates map[string]config.Backend
	// apiKeysChanged / defaultRateLimitChanged tell the applier that
	// the authenticator and the affected rate limiters need rebuilding.
	apiKeysChanged          bool
	defaultRateLimitChanged bool
}

// diffConfigs compares the running config with a newly loaded one and
// sorts each change into "applied live on reload" or "needs a restart".
// It is pure: it changes nothing, so tests can drive it directly.
func diffConfigs(old, new *config.Config) configDiff {
	d := configDiff{
		reloaded:        []string{},
		requiresRestart: []string{},
		backendUpdates:  map[string]config.Backend{},
	}
	if !reflect.DeepEqual(old.APIKeys, new.APIKeys) {
		d.reloaded = append(d.reloaded, "api_keys")
		d.apiKeysChanged = true
	}
	if !reflect.DeepEqual(old.ToolScopes, new.ToolScopes) {
		d.reloaded = append(d.reloaded, "tool_scopes")
	}
	if !reflect.DeepEqual(old.DefaultRateLimit, new.DefaultRateLimit) {
		d.reloaded = append(d.reloaded, "default_rate_limit")
		d.defaultRateLimitChanged = true
	}
	if old.Listen != new.Listen {
		d.requiresRestart = append(d.requiresRestart, "listen")
	}
	if !reflect.DeepEqual(old.TLS, new.TLS) {
		d.requiresRestart = append(d.requiresRestart, "tls")
	}
	if !reflect.DeepEqual(old.Redis, new.Redis) {
		d.requiresRestart = append(d.requiresRestart, "redis")
	}
	if !reflect.DeepEqual(old.OIDC, new.OIDC) {
		d.requiresRestart = append(d.requiresRestart, "oidc")
	}
	if !reflect.DeepEqual(old.OAuth, new.OAuth) {
		d.requiresRestart = append(d.requiresRestart, "oauth")
	}
	oldByName := map[string]config.Backend{}
	for _, b := range old.Backends {
		oldByName[b.Name] = b
	}
	newByName := map[string]config.Backend{}
	for _, b := range new.Backends {
		newByName[b.Name] = b
	}
	for name := range oldByName {
		if _, ok := newByName[name]; !ok {
			d.requiresRestart = append(d.requiresRestart, "backend "+name+" removed")
		}
	}
	for name, nb := range newByName {
		ob, ok := oldByName[name]
		if !ok {
			d.requiresRestart = append(d.requiresRestart, "backend "+name+" added")
			continue
		}
		if changed := backendIdentityChanges(ob, nb); len(changed) > 0 {
			d.requiresRestart = append(d.requiresRestart,
				"backend "+name+": "+strings.Join(changed, ", ")+" changed")
		}
		if !reflect.DeepEqual(ob.ToolFilter, nb.ToolFilter) {
			d.reloaded = append(d.reloaded, "backends."+name+".tool_filter")
			d.backendUpdates[name] = nb
		}
		if !reflect.DeepEqual(ob.SchemaRewrites, nb.SchemaRewrites) {
			d.reloaded = append(d.reloaded, "backends."+name+".schema_rewrites")
			d.backendUpdates[name] = nb
		}
		if !reflect.DeepEqual(ob.Cache, nb.Cache) {
			d.reloaded = append(d.reloaded, "backends."+name+".cache")
			d.backendUpdates[name] = nb
		}
		if !reflect.DeepEqual(ob.Breaker, nb.Breaker) {
			d.reloaded = append(d.reloaded, "backends."+name+".breaker")
			d.backendUpdates[name] = nb
		}
		if !reflect.DeepEqual(ob.HealthCheck, nb.HealthCheck) {
			d.reloaded = append(d.reloaded, "backends."+name+".health_check")
			d.backendUpdates[name] = nb
		}
	}
	sort.Strings(d.reloaded)
	sort.Strings(d.requiresRestart)
	return d
}

// backendIdentityChanges names the connection-identity fields that
// differ between two configs for the same backend: the backend client
// is built from these once at startup (child process, HTTP client,
// auth headers), so a change here cannot take effect without a
// restart. An empty return means the backend's identity is unchanged.
func backendIdentityChanges(old, new config.Backend) []string {
	var changed []string
	if old.Transport != new.Transport {
		changed = append(changed, "transport")
	}
	if !reflect.DeepEqual(old.Command, new.Command) {
		changed = append(changed, "command")
	}
	if !reflect.DeepEqual(old.Env, new.Env) {
		changed = append(changed, "env")
	}
	if old.URL != new.URL {
		changed = append(changed, "url")
	}
	if !reflect.DeepEqual(old.Endpoints, new.Endpoints) {
		changed = append(changed, "endpoints")
	}
	if old.Strategy != new.Strategy {
		changed = append(changed, "strategy")
	}
	if !reflect.DeepEqual(old.Auth, new.Auth) {
		changed = append(changed, "auth")
	}
	if !reflect.DeepEqual(old.Headers, new.Headers) {
		changed = append(changed, "headers")
	}
	if old.Timeout != new.Timeout {
		changed = append(changed, "timeout")
	}
	return changed
}

// reload applies a newly loaded config: the reloadable parts take
// effect immediately and the rest is reported as requiring a restart.
// It takes the write lock, so a request in flight keeps the snapshot
// it started with and the next request sees the new config whole —
// never a half-applied mix.
func (g *Gateway) reload(newCfg *config.Config) (reloaded, requiresRestart []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := diffConfigs(g.cfg, newCfg)
	old := g.cfg

	if d.apiKeysChanged {
		// The OIDC verifier and OAuth introspector are reused: their
		// own configuration did not change (a change there is
		// restart-required), and rebuilding them would drop the JWKS
		// and introspection caches for no reason.
		g.auth = auth.New(newCfg.APIKeys, g.oidc, g.oauth)
	}
	if d.apiKeysChanged || d.defaultRateLimitChanged {
		g.resetLimitersLocked(old, newCfg)
	}
	for name, nb := range d.backendUpdates {
		st, ok := g.backends[name]
		if !ok {
			continue // unreachable: diff only lists backends in both configs
		}
		if !reflect.DeepEqual(st.cfg.Breaker, nb.Breaker) {
			// A new breaker starts closed: the old breaker's state
			// belonged to the old thresholds, so carrying it over
			// would be meaningless.
			st.breaker = breaker.New(nb.Breaker.FailureThreshold, time.Duration(nb.Breaker.Cooldown))
			metrics.BreakerState.WithLabelValues(name).Set(0)
			g.logger.Info("breaker recreated by config reload", "backend", name)
		}
		if !reflect.DeepEqual(st.cfg.HealthCheck, nb.HealthCheck) {
			g.restartHealthCheckerLocked(st, nb)
		}
		st.cfg.ToolFilter = nb.ToolFilter
		st.cfg.SchemaRewrites = nb.SchemaRewrites
		st.cfg.Cache = nb.Cache
		st.cfg.Breaker = nb.Breaker
		st.cfg.HealthCheck = nb.HealthCheck
	}
	g.cfg = newCfg
	g.loadedAt = time.Now()
	return d.reloaded, d.requiresRestart
}

// resetLimitersLocked drops the cached rate limiter for every API key
// whose effective limit changed or that no longer exists, so the next
// request builds a limiter from the new config. Limiters for
// unchanged keys keep their token balance: a reload must not hand
// every client a fresh burst. Callers must hold g.mu; the limiters
// map has its own mutex because request handling touches it too.
func (g *Gateway) resetLimitersLocked(old, new *config.Config) {
	oldLimits := map[string]config.RateLimit{}
	for _, k := range old.APIKeys {
		oldLimits[k.Name] = old.RateLimitFor(k)
	}
	newLimits := map[string]config.RateLimit{}
	for _, k := range new.APIKeys {
		newLimits[k.Name] = new.RateLimitFor(k)
	}
	g.limitersMu.Lock()
	defer g.limitersMu.Unlock()
	for name := range g.limiters {
		nl, ok := newLimits[name]
		if !ok || nl != oldLimits[name] {
			delete(g.limiters, name)
		}
	}
}

// restartHealthCheckerLocked stops the backend's current prober (if
// any) and starts one built from the new health-check config, or
// leaves probing disabled when the new interval is zero. Callers must
// hold g.mu.
func (g *Gateway) restartHealthCheckerLocked(st *backendState, nb config.Backend) {
	if st.health != nil {
		st.health.Stop()
		st.health = nil
	}
	if nb.HealthCheck.Interval > 0 {
		st.health = g.newHealthChecker(st.backend, nb)
		st.health.Start()
	}
	g.logger.Info("health checker restarted by config reload", "backend", nb.Name)
}

// handleReload re-reads the config file, validates it, and applies the
// reloadable parts live. POST only, authenticated with an API key like
// every other admin endpoint, and not rate-limited: it is an operator
// action, not client traffic.
//
//	POST /admin/reload → 200 {"status":"ok","reloaded":[...],
//	    "requires_restart":[...],"loaded_at":"..."}
//	                   → 400 {"error":"invalid config: ..."} (running config untouched)
//	                   → 401 without a valid API key
func (g *Gateway) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	g.mu.RLock()
	authn := g.auth
	configPath := g.configPath
	g.mu.RUnlock()
	apiKey, ok := authn.Authenticate(r)
	if !ok {
		http.Error(w, "missing or invalid API key", http.StatusUnauthorized)
		return
	}
	if configPath == "" {
		metrics.ConfigReloads.WithLabelValues("rejected").Inc()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"config reload is unavailable: gateway was not started from a config file"}` + "\n"))
		return
	}
	newCfg, err := config.Load(configPath)
	if err != nil {
		metrics.ConfigReloads.WithLabelValues("rejected").Inc()
		g.logger.Info("config reload rejected", "api_key", apiKey.Name, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": fmt.Sprintf("invalid config: %v", err),
		})
		return
	}
	reloaded, requiresRestart := g.reload(newCfg)
	metrics.ConfigReloads.WithLabelValues("applied").Inc()
	g.logger.Info("config reloaded", "api_key", apiKey.Name,
		"reloaded", reloaded, "requires_restart", requiresRestart)
	g.mu.RLock()
	loadedAt := g.loadedAt
	g.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":           "ok",
		"reloaded":         reloaded,
		"requires_restart": requiresRestart,
		"loaded_at":        loadedAt.Format(time.RFC3339),
	})
}

// handleAdminStatus reports the gateway's runtime state: uptime, when
// the config was last loaded, the API key names, and every backend's
// breaker and health state. GET only, authenticated with an API key.
// Key values and backend credentials never appear in the response.
//
//	GET /admin/status → 200 {"status":"ok","uptime_seconds":123,
//	    "loaded_at":"...","api_keys":["alice"],
//	    "backends":[{"name":"fake","transport":"http",
//	    "breaker":"closed","health":"disabled"}]}
func (g *Gateway) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	g.mu.RLock()
	authn := g.auth
	g.mu.RUnlock()
	if _, ok := authn.Authenticate(r); !ok {
		http.Error(w, "missing or invalid API key", http.StatusUnauthorized)
		return
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	keyNames := make([]string, 0, len(g.cfg.APIKeys))
	for _, k := range g.cfg.APIKeys {
		keyNames = append(keyNames, k.Name)
	}
	backends := make([]map[string]any, 0, len(g.backends))
	for name, st := range g.backends {
		healthState := "disabled"
		if st.health != nil {
			healthState = "healthy"
			if !st.health.Healthy() {
				healthState = "unhealthy"
			}
		}
		backends = append(backends, map[string]any{
			"name":      name,
			"transport": st.cfg.Transport,
			"breaker":   st.breaker.State().String(),
			"health":    healthState,
		})
	}
	sort.Slice(backends, func(i, j int) bool {
		return backends[i]["name"].(string) < backends[j]["name"].(string)
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         "ok",
		"uptime_seconds": int64(time.Since(g.startedAt).Seconds()),
		"loaded_at":      g.loadedAt.Format(time.RFC3339),
		"api_keys":       keyNames,
		"backends":       backends,
	})
}
