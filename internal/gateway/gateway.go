// Package gateway is the HTTP front door of mcpgate.
//
// Request flow for POST /mcp/{backend}:
//
//  1. Authenticate the client (X-API-Key or Bearer token).
//  2. Enforce the per-key token-bucket rate limit (429 on exhaustion).
//  3. Look up the backend; unknown backends get a JSON-RPC -32601 error.
//  4. Enforce the backend's tool filter: tools/call for a filtered tool is
//     rejected with a JSON-RPC -32602 error and never reaches the backend.
//  5. Check the breaker; an open breaker fails fast with 503.
//  6. Check the response cache (tools/list, and tools/call for explicitly
//     cacheable tools). Hits return X-Cache: HIT. tools/list responses
//     are filtered on the way out, on both paths. Clients that asked
//     for SSE skip the cache: a live stream is not a snapshot.
//  7. Forward the JSON-RPC request to the backend, record metrics, and
//     update the breaker. Cacheable responses are stored. A client
//     that sent Accept: text/event-stream gets the backend's SSE
//     events relayed live instead of a single JSON response (see
//     serveStream); backends that cannot stream (stdio) answer with
//     one SSE event carrying the single response.
//
// Notifications (messages without an id) take a shorter path: after
// authentication, rate limiting, and the breaker check, they are forwarded
// fire-and-forget and the client gets 202 Accepted. The client asked for
// acceptance, not delivery confirmation, so forward failures are logged
// and counted in metrics but never surfaced to the client.
//
// The gateway also serves GET /healthz and GET /metrics (Prometheus), plus
// GET /readyz (readiness: per-backend breaker state; 503 when no backend
// can serve) and an admin endpoint: POST /admin/cache/invalidate[/{backend}] clears
// backend response caches on demand (API-key authenticated).
// API key values and backend credentials are never logged; logs carry only
// the key name and backend name.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"sort"
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
		auth:       auth.New(cfg.APIKeys, nil, nil),
		limiters:   map[string]*ratelimit.Limiter{},
		backends:   map[string]*backendState{},
		logger:     logger,
		newBackend: factory,
	}
	var verifier *auth.Verifier
	if cfg.OIDC != nil {
		var err error
		verifier, err = auth.NewVerifier(*cfg.OIDC)
		if err != nil {
			g.Close()
			return nil, fmt.Errorf("oidc: %w", err)
		}
	}
	var introspector *auth.Introspector
	if cfg.OAuth != nil {
		introspector = auth.NewIntrospector(*cfg.OAuth)
	}
	g.auth = auth.New(cfg.APIKeys, verifier, introspector)
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
	mux.HandleFunc("/readyz", g.handleReady)
	mux.HandleFunc("/admin/cache/invalidate", g.handleCacheInvalidate)
	mux.HandleFunc("/admin/cache/invalidate/", g.handleCacheInvalidate)
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

// handleReady answers readiness checks: it reports every backend's circuit
// breaker state, so a load balancer or operator can tell whether the
// gateway can currently serve traffic. This is deliberately distinct
// from /healthz (liveness): the process can be alive while every backend
// is down, and that is exactly what readiness must surface.
//
//	GET /readyz → 200 {"status":"ready","backends":{"fake":"closed"}}
//	           → 503 {"status":"degraded","backends":{"fake":"open"}}
//	           (degraded = no backend configured, or every breaker's open)
//
// The state names are the breaker's own ("closed", "open", "half-open");
// a breaker past its cooldown reports "half-open" even before a probe
// arrives. Unauthenticated like /healthz: breaker states carry no secrets.
func (g *Gateway) handleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	states := make(map[string]string, len(g.backends))
	ready := false
	for name, st := range g.backends {
		s := st.breaker.State()
		states[name] = s.String()
		if s != breaker.Open {
			ready = true
		}
	}
	status := "ready"
	code := http.StatusOK
	if !ready {
		status = "degraded"
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   status,
		"backends": states,
	})
}

// handleCacheInvalidate clears backend response caches on demand. POST
// only, authenticated with an API key like every other endpoint.
//
//	POST /admin/cache/invalidate            — every backend's cache
//	POST /admin/cache/invalidate/{backend}  — one backend's cache
//
// Answers 200 with {"status":"ok","backends":[...],"cleared":N}; an unknown
// backend name gets 404. Cache invalidation is an operator action, so it is
// not rate-limited. API key values are never logged.
func (g *Gateway) handleCacheInvalidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	apiKey, ok := g.auth.Authenticate(r)
	if !ok {
		http.Error(w, "missing or invalid API key", http.StatusUnauthorized)
		return
	}
	name := strings.TrimPrefix(
		strings.TrimPrefix(r.URL.Path, "/admin/cache/invalidate"), "/")
	var targets map[string]*backendState
	if name == "" {
		targets = g.backends
	} else {
		st, ok := g.backends[name]
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"unknown backend"}` + "\n"))
			return
		}
		targets = map[string]*backendState{name: st}
	}
	names := make([]string, 0, len(targets))
	cleared := 0
	for n, st := range targets {
		names = append(names, n)
		cleared += st.cache.Clear()
	}
	sort.Strings(names)
	g.logger.Info("cache invalidated",
		"api_key", apiKey.Name, "backends", names, "cleared", cleared)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   "ok",
		"backends": names,
		"cleared":  cleared,
	})
}

// rpcError writes a JSON-RPC error response.
func writeRPCError(w http.ResponseWriter, status int, id any, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	resp := backend.Response{JSONRPC: "2.0", ID: id, Error: &backend.Error{Code: code, Message: message}}
	_ = json.NewEncoder(w).Encode(resp)
}

// wantsSSE reports whether the client asked for a live event stream:
// its Accept header includes text/event-stream. A client that accepts
// both JSON and SSE (the MCP default) gets the stream; a client that
// only accepts JSON keeps the single-response behavior. A bare */*
// does not opt into SSE: it expresses no preference for a stream.
func wantsSSE(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		mt, _, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err == nil && mt == "text/event-stream" {
			return true
		}
	}
	return false
}

// writeSSEData writes one SSE event carrying data as its payload,
// splitting multi-line payloads into one data: line each.
func writeSSEData(w http.ResponseWriter, data []byte) {
	for _, line := range bytes.Split(data, []byte("\n")) {
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(line)
		_, _ = w.Write([]byte("\n"))
	}
	_, _ = w.Write([]byte("\n"))
}

// writeResponse writes one JSON-RPC response, already encoded. When
// sse is true the response goes out as a single SSE data event
// (Content-Type: text/event-stream); otherwise as plain JSON.
// cacheStatus, when non-empty, is reported in X-Cache.
func writeResponse(w http.ResponseWriter, encoded []byte, sse bool, cacheStatus string) {
	if sse {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	if cacheStatus != "" {
		w.Header().Set("X-Cache", cacheStatus)
	}
	if sse {
		writeSSEData(w, encoded)
		return
	}
	_, _ = w.Write(encoded)
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

// toolBlocked reports whether the backend's tool filter bars clients from
// the named tool. Deny wins over Allow; an empty Allow permits every
// tool not denied.
func toolBlocked(f config.ToolFilter, name string) bool {
	for _, d := range f.Deny {
		if d == name {
			return true
		}
	}
	if len(f.Allow) == 0 {
		return false
	}
	for _, a := range f.Allow {
		if a == name {
			return false
		}
	}
	return true
}

// filterToolsList removes blocked tools from a tools/list response result
// in place. Malformed results (no "tools" key, wrong types) are left
// untouched; entries without a recognizable name are kept, never dropped.
func filterToolsList(f config.ToolFilter, resp *backend.Response) {
	raw, ok := resp.Result["tools"]
	if !ok {
		return
	}
	list, ok := raw.([]any)
	if !ok {
		return
	}
	kept := make([]any, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			kept = append(kept, item)
			continue
		}
		name, ok := m["name"].(string)
		if !ok || name == "" {
			// No recognizable name: the filter cannot judge this
			// entry, so keep it. (A missing name used to decode as
			// "" and match no allow entry, so allow lists silently
			// dropped unnamed entries.)
			kept = append(kept, item)
			continue
		}
		if !toolBlocked(f, name) {
			kept = append(kept, item)
		}
	}
	resp.Result["tools"] = kept
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

	// 4. Tool filter. A tools/call for a filtered tool is rejected here,
	// before any backend, cache, or breaker involvement: the tool simply
	// does not exist for this client. tools/list filtering happens on the
	// response path, so cached responses stay filtered too.
	if req.Method == "tools/call" {
		if name, _ := req.Params["name"].(string); name != "" && toolBlocked(st.cfg.ToolFilter, name) {
			metrics.RequestsTotal.WithLabelValues(backendName, req.Method, "rejected").Inc()
			log.Info("request rejected", "reason", "tool_blocked", "tool", name)
			writeRPCError(w, http.StatusForbidden, id, -32602,
				fmt.Sprintf("tool %q is not available", name))
			return
		}
	}

	// 5. Circuit breaker.
	if err := st.breaker.Allow(); err != nil {
		if errors.Is(err, breaker.ErrOpen) {
			metrics.RequestsTotal.WithLabelValues(backendName, req.Method, "rejected").Inc()
			log.Info("request rejected", "reason", "circuit_open")
			writeRPCError(w, http.StatusServiceUnavailable, id, jsonRPCErrorCodeBackendFailure,
				"backend unavailable: circuit breaker open")
			return
		}
	}

	// A client that asked for SSE (Accept: text/event-stream) gets a
	// live stream when the backend can provide one. Streaming skips
	// the cache below: a live stream is not a cacheable snapshot.
	// A backend that cannot stream (stdio) falls through to the
	// normal path, and its single response is encoded as one SSE
	// event instead of plain JSON.
	sse := wantsSSE(r)
	if sse {
		if streamer, ok := st.backend.(backend.Streamer); ok {
			g.serveStream(w, r, log, st, backendName, req, id, streamer)
			return
		}
	}

	// 6. Cache lookup. Cached entries store only the result/error payload;
	// the response is re-wrapped with the current request's id on a hit,
	// because JSON-RPC requires the response id to match the request id.
	// tools/list responses are filtered on the way out, so a cached
	// payload always looks the same as a fresh one.
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
				resp := backend.Response{
					JSONRPC: "2.0", ID: id, Result: payload.Result, Error: payload.Error,
				}
				if req.Method == "tools/list" && resp.Error == nil && resp.Result != nil {
					filterToolsList(st.cfg.ToolFilter, &resp)
				}
				// A cache hit consumed the breaker probe (the breaker
				// check runs before the cache lookup) but proves
				// nothing about the backend: free the probe slot so a
				// half-open breaker is not wedged by the hit.
				st.breaker.ReleaseProbe()
				encoded, err := json.Marshal(resp)
				if err != nil {
					log.Error("failed to encode response", "error", err)
					writeRPCError(w, http.StatusInternalServerError, id, -32603, "internal error")
					return
				}
				writeResponse(w, encoded, sse, "HIT")
				return
			}
			// Corrupt cache entry: fall through to the backend.
			log.Error("cached entry corrupt, refetching", "error", err)
		}
		metrics.CacheMisses.WithLabelValues(backendName, req.Method).Inc()
	}

	// 7. Forward to the backend.
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
	if req.Method == "tools/list" && resp.Error == nil && resp.Result != nil {
		// Hide filtered tools from the client. This runs before caching
		// so the cached payload is already filtered (the filter is static
		// per backend, so cache and fresh paths agree).
		filterToolsList(st.cfg.ToolFilter, resp)
	}
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
	cacheStatus := ""
	if ttl > 0 {
		cacheStatus = "MISS"
	}
	writeResponse(w, encoded, sse, cacheStatus)
}

// serveStream relays a backend's live event stream to a client that
// asked for SSE (Accept: text/event-stream).
//
// Events arrive in backend order: server notifications (progress,
// logging) are relayed verbatim as they arrive, instead of being
// swallowed while the gateway waits for the response. The stream ends
// after the JSON-RPC response matching the request id is relayed —
// post-response server messages are out of scope, matching the
// single-response path, which also stops at the response. Server
// requests (a method with an id) are skipped, not relayed: the
// gateway cannot answer them, so relaying them would leave the
// backend waiting for an answer that never comes. A tools/list
// response is filtered exactly like a single response, so the tool
// filter cannot be bypassed by asking for a stream. Streaming
// responses are never cached: a live stream is not a snapshot.
//
// If the stream ends or fails before the matching response arrives,
// the client gets a final JSON-RPC error event (-32000) and the
// failure counts against the breaker like any other backend failure.
// A client that disconnects mid-stream simply ends the relay: the
// backend request is cancelled and nothing is recorded against the
// breaker, because the backend did not fail.
func (g *Gateway) serveStream(w http.ResponseWriter, r *http.Request, log *slog.Logger, st *backendState, backendName string, req *backend.Request, id any, streamer backend.Streamer) {
	start := time.Now()
	stream, err := streamer.Stream(r.Context(), req)
	if err != nil {
		// The stream never started: no SSE headers were sent yet, so
		// a normal 502 JSON error is still possible.
		metrics.BackendErrors.WithLabelValues(backendName).Inc()
		metrics.RequestsTotal.WithLabelValues(backendName, req.Method, "backend_error").Inc()
		st.breaker.RecordFailure()
		metrics.BreakerState.WithLabelValues(backendName).Set(float64(breakerStateValue(st.breaker.State())))
		log.Error("backend stream failed", "error", err, "latency_ms", time.Since(start).Milliseconds())
		writeRPCError(w, http.StatusBadGateway, id, jsonRPCErrorCodeBackendFailure, "backend error: "+err.Error())
		return
	}
	defer stream.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Unreachable with net/http, which always provides a
		// Flusher — but without one events could not be pushed
		// live, so fail plainly instead of buffering a stream.
		_ = stream.Close()
		writeRPCError(w, http.StatusInternalServerError, id, -32603, "streaming not supported by this server")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // don't let proxies buffer the stream
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// fail ends a stream that produced no matching response: the
	// failure counts against the breaker, and the client gets a
	// final JSON-RPC error event instead of a silent close.
	fail := func(err error) {
		if r.Context().Err() != nil {
			// The client went away mid-stream: nothing to send,
			// and the backend did not fail.
			log.Info("stream ended", "reason", "client_disconnected")
			return
		}
		metrics.BackendErrors.WithLabelValues(backendName).Inc()
		metrics.RequestsTotal.WithLabelValues(backendName, req.Method, "backend_error").Inc()
		st.breaker.RecordFailure()
		metrics.BreakerState.WithLabelValues(backendName).Set(float64(breakerStateValue(st.breaker.State())))
		log.Error("backend stream failed", "error", err, "latency_ms", time.Since(start).Milliseconds())
		errResp := backend.Response{JSONRPC: "2.0", ID: id, Error: &backend.Error{
			Code: jsonRPCErrorCodeBackendFailure, Message: "backend error: " + err.Error(),
		}}
		encoded, encErr := json.Marshal(errResp)
		if encErr != nil {
			return
		}
		writeSSEData(w, encoded)
		flusher.Flush()
	}

	wantID, _ := json.Marshal(id)
	for {
		payload, err := stream.Next()
		if err != nil {
			if err == io.EOF {
				fail(errors.New("SSE stream had no JSON-RPC response matching the request id"))
			} else {
				fail(err)
			}
			return
		}
		// Classify the event. Only well-formed JSON-RPC 2.0
		// messages are relayed; anything else is skipped, like
		// the single-response path skips non-matching events.
		var probe struct {
			JSONRPC string `json:"jsonrpc"`
			Method  string `json:"method"`
			ID      any    `json:"id"`
		}
		if err := json.Unmarshal(payload, &probe); err != nil || probe.JSONRPC != "2.0" {
			continue
		}
		if probe.Method != "" {
			if probe.ID != nil {
				continue // server request: never relayed (see doc)
			}
			// Server notification (progress, logging): relay live.
			writeSSEData(w, payload)
			flusher.Flush()
			continue
		}
		if probe.ID == nil {
			continue // neither a response nor a notification; skip
		}
		gotID, err := json.Marshal(probe.ID)
		if err != nil || string(gotID) != string(wantID) {
			continue // a response to some other request; skip
		}
		var resp backend.Response
		if err := json.Unmarshal(payload, &resp); err != nil || resp.JSONRPC != "2.0" {
			continue // malformed response; keep waiting
		}
		// The response: filter tools/list like the single-response
		// path, re-stamp the client's id, and relay it.
		resp.JSONRPC = "2.0"
		resp.ID = id
		if req.Method == "tools/list" && resp.Error == nil && resp.Result != nil {
			filterToolsList(st.cfg.ToolFilter, &resp)
		}
		encoded, err := json.Marshal(resp)
		if err != nil {
			log.Error("failed to encode response", "error", err)
			fail(fmt.Errorf("failed to encode response: %w", err))
			return
		}
		writeSSEData(w, encoded)
		flusher.Flush()

		// The response arrived: record the outcome like the
		// single-response path. A JSON-RPC error is a valid
		// response, not a transport failure, so it still counts
		// as a breaker success.
		elapsed := time.Since(start)
		metrics.RequestDuration.WithLabelValues(backendName).Observe(elapsed.Seconds())
		st.breaker.RecordSuccess()
		metrics.BreakerState.WithLabelValues(backendName).Set(float64(breakerStateValue(st.breaker.State())))
		status := "ok"
		if resp.Error != nil {
			status = "rpc_error"
		}
		metrics.RequestsTotal.WithLabelValues(backendName, req.Method, status).Inc()
		log.Info("request served", "cache", "stream", "latency_ms", elapsed.Milliseconds(), "status", status)
		return
	}
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
