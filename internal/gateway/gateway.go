// Package gateway is the HTTP front door of mcpgate.
//
// Request flow for POST /mcp/{backend}:
//
//  1. Authenticate the client (X-API-Key or Bearer token).
//  2. Enforce the per-key token-bucket rate limit (429 on exhaustion).
//  3. Look up the backend; unknown backends get a JSON-RPC -32601 error.
//  4. Check the breaker; an open breaker fails fast with 503.
//  5. Check the response cache (tools/list, and tools/call for explicitly
//     cacheable tools). Hits return X-Cache: HIT.
//  6. Forward the JSON-RPC request to the backend, record metrics, and
//     update the breaker. Cacheable responses are stored.
//
// Notifications (messages without an id) take a shorter path: after
// authentication, rate limiting, and the breaker check, they are forwarded
// fire-and-forget and the client gets 202 Accepted. The client asked for
// acceptance, not delivery confirmation, so forward failures are logged
// and counted in metrics but never surfaced to the client.
//
// The gateway also serves GET /healthz and GET /metrics (Prometheus).
// API key values and backend credentials are never logged; logs carry only
// the key name and backend name.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sunwen1983/mcpgate/internal/auth"
	"github.com/sunwen1983/mcpgate/internal/backend"
	"github.com/sunwen1983/mcpgate/internal/breaker"
	"github.com/sunwen1983/mcpgate/internal/cache"
	"github.com/sunwen1983/mcpgate/internal/config"
	"github.com/sunwen1983/mcpgate/internal/metrics"
	"github.com/sunwen1983/mcpgate/internal/ratelimit"
)

// jsonRPCErrorCodeForBackendFailure is the JSON-RPC error code returned
// when the gateway cannot reach the backend at all.
const jsonRPCErrorCodeBackendFailure = -32000

// backendState bundles everything the gateway holds per backend.
type backendState struct {
	cfg     config.Backend
	backend backend.Backend
	breaker *breaker.Breaker
	cache   *cache.Cache
}

// Gateway is an http.Handler for the MCP proxy.
type Gateway struct {
	cfg        *config.Config
	auth       *auth.Authenticator
	limiters   map[string]*ratelimit.Limiter // key name -> limiter
	limitersMu sync.Mutex
	backends   map[string]*backendState
	handler    http.Handler
	logger     *slog.Logger
	newBackend func(config.Backend) (backend.Backend, error) // injectable for tests
}

// New builds a Gateway from config. It starts every configured backend.
func New(cfg *config.Config, logger *slog.Logger) (*Gateway, error) {
	return newWithBackendFactory(cfg, logger, backend.New)
}

func newWithBackendFactory(cfg *config.Config, logger *slog.Logger, factory func(config.Backend) (backend.Backend, error)) (*Gateway, error) {
	g := &Gateway{
		cfg:        cfg,
		auth:       auth.New(cfg.APIKeys, nil),
		limiters:   map[string]*ratelimit.Limiter{},
		backends:   map[string]*backendState{},
		logger:     logger,
		newBackend: factory,
	}
	if cfg.OIDC != nil {
		verifier, err := auth.NewVerifier(*cfg.OIDC)
		if err != nil {
			g.Close()
			return nil, fmt.Errorf("oidc: %w", err)
		}
		g.auth = auth.New(cfg.APIKeys, verifier)
	}
	for _, bcfg := range cfg.Backends {
		be, err := factory(bcfg)
		if err != nil {
			g.Close()
			return nil, fmt.Errorf("backend %s: %w", bcfg.Name, err)
		}
		st := &backendState{
			cfg:     bcfg,
			backend: be,
			breaker: breaker.New(bcfg.Breaker.FailureThreshold, time.Duration(bcfg.Breaker.Cooldown)),
			cache:   cache.New(),
		}
		g.backends[bcfg.Name] = st
		metrics.BreakerState.WithLabelValues(bcfg.Name).Set(0)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/", g.handleMCP)
	mux.HandleFunc("/healthz", g.handleHealth)
	mux.Handle("/metrics", promhttp.Handler())
	g.handler = mux
	return g, nil
}

// ServeHTTP implements http.Handler.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.handler.ServeHTTP(w, r)
}

// Close shuts down all backends.
func (g *Gateway) Close() {
	for _, st := range g.backends {
		_ = st.backend.Close()
	}
}

// limiterFor returns (creating if needed) the rate limiter for an API key.
func (g *Gateway) limiterFor(key config.APIKey) *ratelimit.Limiter {
	g.limitersMu.Lock()
	defer g.limitersMu.Unlock()
	if l, ok := g.limiters[key.Name]; ok {
		return l
	}
	rl := g.cfg.RateLimitFor(key)
	l := ratelimit.New(rl.Rate, rl.Burst)
	g.limiters[key.Name] = l
	return l
}

// newRequestID generates a random hex request ID for log correlation.
func newRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// handleHealth answers liveness checks.
func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
}

// rpcError writes a JSON-RPC error response.
func writeRPCError(w http.ResponseWriter, status int, id any, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	resp := backend.Response{JSONRPC: "2.0", ID: id, Error: &backend.Error{Code: code, Message: message}}
	_ = json.NewEncoder(w).Encode(resp)
}

// parseBody reads and parses one JSON-RPC message. It returns the request,
// its raw id (for error responses), and whether the body held a
// notification (no id, no response expected).
func parseBody(body []byte) (*backend.Request, any, bool, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, false, err
	}
	var req backend.Request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, nil, false, err
	}
	// Notifications are validated like requests: a notification without a
	// method is a malformed message, not a notification.
	if req.JSONRPC != "2.0" || req.Method == "" {
		return nil, nil, false, fmt.Errorf("invalid json-rpc request")
	}
	if _, hasID := raw["id"]; !hasID {
		return &req, nil, true, nil // notification
	}
	var id any
	_ = json.Unmarshal(raw["id"], &id)
	return &req, id, false, nil
}

// cacheKey builds a cache key from backend, method, and canonical params.
// encoding/json sorts map keys, so marshaling the params map is canonical.
func cacheKey(backendName, method string, params map[string]any) string {
	paramsJSON, _ := json.Marshal(params)
	return backendName + "|" + method + "|" + string(paramsJSON)
}

// cacheTTL returns the TTL for a cacheable request, or 0 if the request
// must not be cached.
func (g *Gateway) cacheTTL(st *backendState, req *backend.Request) time.Duration {
	switch req.Method {
	case "tools/list":
		return time.Duration(st.cfg.Cache.ToolsListTTL)
	case "tools/call":
		if time.Duration(st.cfg.Cache.ToolsCallTTL) <= 0 {
			return 0
		}
		name, _ := req.Params["name"].(string)
		for _, t := range st.cfg.Cache.CacheableTools {
			if t == name {
				return time.Duration(st.cfg.Cache.ToolsCallTTL)
			}
		}
		return 0
	default:
		return 0
	}
}

// handleMCP proxies one JSON-RPC request to the backend named in the path.
func (g *Gateway) handleMCP(w http.ResponseWriter, r *http.Request) {
	requestID := newRequestID()
	log := g.logger.With("request_id", requestID)
	w.Header().Set("X-Request-ID", requestID)

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	backendName := strings.TrimPrefix(r.URL.Path, "/mcp/")
	if backendName == "" || strings.Contains(backendName, "/") {
		writeRPCError(w, http.StatusNotFound, nil, -32601, "unknown backend")
		return
	}

	// 1. Authenticate.
	apiKey, ok := g.auth.Authenticate(r)
	if !ok {
		log.Info("request rejected", "reason", "unauthorized", "backend", backendName)
		writeRPCError(w, http.StatusUnauthorized, nil, -32001, "missing or invalid API key")
		return
	}
	log = log.With("api_key", apiKey.Name, "backend", backendName)

	// 2. Rate limit.
	if !g.limiterFor(apiKey).Allow(apiKey.Name) {
		metrics.RateLimitRejections.WithLabelValues(apiKey.Name).Inc()
		log.Info("request rejected", "reason", "rate_limited")
		writeRPCError(w, http.StatusTooManyRequests, nil, -32002, "rate limit exceeded")
		return
	}

	// 3. Backend lookup.
	st, ok := g.backends[backendName]
	if !ok {
		log.Info("request rejected", "reason", "unknown_backend")
		writeRPCError(w, http.StatusNotFound, nil, -32601, "unknown backend: "+backendName)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		writeRPCError(w, http.StatusBadRequest, nil, -32700, "failed to read request body")
		return
	}
	req, id, isNotification, err := parseBody(body)
	if err != nil {
		log.Info("request rejected", "reason", "parse_error", "error", err)
		writeRPCError(w, http.StatusBadRequest, nil, -32700, "parse error: invalid JSON-RPC request")
		return
	}
	if isNotification {
		// Notifications carry no id and get no response body (202 Accepted
		// per MCP spec). They are forwarded fire-and-forget after auth,
		// rate limiting, and the breaker check; failures are logged and
		// counted, never surfaced to the client.
		g.forwardNotification(r.Context(), log, st, backendName, req)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	log = log.With("method", req.Method)

	// 4. Circuit breaker.
	if err := st.breaker.Allow(); err != nil {
		if errors.Is(err, breaker.ErrOpen) {
			metrics.RequestsTotal.WithLabelValues(backendName, req.Method, "rejected").Inc()
			log.Info("request rejected", "reason", "circuit_open")
			writeRPCError(w, http.StatusServiceUnavailable, id, jsonRPCErrorCodeBackendFailure,
				"backend unavailable: circuit breaker open")
			return
		}
	}

	// 5. Cache lookup. Cached entries store only the result/error payload;
	// the response is re-wrapped with the current request's id on a hit,
	// because JSON-RPC requires the response id to match the request id.
	ttl := g.cacheTTL(st, req)
	var key string
	if ttl > 0 {
		key = cacheKey(backendName, req.Method, req.Params)
		if cached, hit := st.cache.Get(key); hit {
			metrics.CacheHits.WithLabelValues(backendName, req.Method).Inc()
			metrics.RequestsTotal.WithLabelValues(backendName, req.Method, "ok").Inc()
			var payload cachedPayload
			if err := json.Unmarshal(cached, &payload); err == nil {
				log.Info("request served", "cache", "hit")
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Cache", "HIT")
				_ = json.NewEncoder(w).Encode(backend.Response{
					JSONRPC: "2.0", ID: id, Result: payload.Result, Error: payload.Error,
				})
				return
			}
			// Corrupt cache entry: fall through to the backend.
			log.Error("cached entry corrupt, refetching", "error", err)
		}
		metrics.CacheMisses.WithLabelValues(backendName, req.Method).Inc()
	}

	// 6. Forward to the backend.
	start := time.Now()
	resp, err := st.backend.Call(r.Context(), req)
	elapsed := time.Since(start)
	metrics.RequestDuration.WithLabelValues(backendName).Observe(elapsed.Seconds())
	if err != nil {
		metrics.BackendErrors.WithLabelValues(backendName).Inc()
		metrics.RequestsTotal.WithLabelValues(backendName, req.Method, "backend_error").Inc()
		st.breaker.RecordFailure()
		metrics.BreakerState.WithLabelValues(backendName).Set(float64(breakerStateValue(st.breaker.State())))
		log.Error("backend call failed", "error", err, "latency_ms", elapsed.Milliseconds())
		writeRPCError(w, http.StatusBadGateway, id, jsonRPCErrorCodeBackendFailure,
			"backend error: "+err.Error())
		return
	}
	st.breaker.RecordSuccess()
	metrics.BreakerState.WithLabelValues(backendName).Set(float64(breakerStateValue(st.breaker.State())))

	status := "ok"
	if resp.Error != nil {
		// A JSON-RPC error from the backend is a valid response, forwarded
		// verbatim. It is not a transport failure, so the breaker stays
		// untouched and the response is not cached.
		status = "rpc_error"
		ttl = 0
	}
	metrics.RequestsTotal.WithLabelValues(backendName, req.Method, status).Inc()

	resp.JSONRPC = "2.0"
	resp.ID = id // echo the client's id, not the backend's
	if ttl > 0 {
		// Cache only the payload, not the id (see the lookup path above).
		if payload, err := json.Marshal(cachedPayload{Result: resp.Result, Error: resp.Error}); err == nil {
			st.cache.Set(key, payload, ttl)
		}
	}
	encoded, err := json.Marshal(resp)
	if err != nil {
		log.Error("failed to encode response", "error", err)
		writeRPCError(w, http.StatusInternalServerError, id, -32603, "internal error")
		return
	}
	log.Info("request served", "cache", "miss", "latency_ms", elapsed.Milliseconds(), "status", status)
	w.Header().Set("Content-Type", "application/json")
	if ttl > 0 {
		w.Header().Set("X-Cache", "MISS")
	}
	_, _ = w.Write(encoded)
}

// forwardNotification sends a notification to the backend fire-and-forget.
// The client always gets 202 Accepted from handleMCP regardless of the
// outcome here: the client asked for acceptance, not delivery
// confirmation. An open breaker drops the notification (nothing is
// attempted). A forward failure counts against the breaker like any other
// backend failure, and is logged and counted in metrics.
func (g *Gateway) forwardNotification(ctx context.Context, log *slog.Logger, st *backendState, backendName string, req *backend.Request) {
	log = log.With("method", req.Method)
	if err := st.breaker.Allow(); err != nil {
		if errors.Is(err, breaker.ErrOpen) {
			metrics.NotificationsTotal.WithLabelValues(backendName, req.Method, "dropped").Inc()
			log.Info("notification dropped", "reason", "circuit_open")
		}
		return
	}
	if err := st.backend.Notify(ctx, req.Method, req.Params); err != nil {
		st.breaker.RecordFailure()
		metrics.BreakerState.WithLabelValues(backendName).Set(float64(breakerStateValue(st.breaker.State())))
		metrics.NotificationsTotal.WithLabelValues(backendName, req.Method, "failed").Inc()
		log.Error("notification forward failed", "error", err)
		return
	}
	st.breaker.RecordSuccess()
	metrics.BreakerState.WithLabelValues(backendName).Set(float64(breakerStateValue(st.breaker.State())))
	metrics.NotificationsTotal.WithLabelValues(backendName, req.Method, "forwarded").Inc()
	log.Info("notification forwarded")
}

// cachedPayload is what the response cache stores: the result/error of a
// backend response, without the request id.
type cachedPayload struct {
	Result map[string]any `json:"result,omitempty"`
	Error  *backend.Error `json:"error,omitempty"`
}

// breakerStateValue maps a breaker state to its Prometheus gauge value.
func breakerStateValue(s breaker.State) int {
	switch s {
	case breaker.Open:
		return 2
	case breaker.HalfOpen:
		return 1
	default:
		return 0
	}
}
