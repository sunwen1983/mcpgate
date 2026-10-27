package gateway

// Stdio transport for the gateway.
//
// ServeStdio lets mcpgate itself be launched as a stdio MCP server —
// the mode stdio-only clients (Claude Desktop, IDE extensions, MCP
// inspectors and directory checkers such as Glama) expect. The client
// speaks newline-delimited JSON-RPC on the gateway's stdin/stdout, and
// the gateway aggregates every configured backend behind that one
// stream:
//
//   - initialize / ping are answered by the gateway itself.
//   - tools/list merges the (filtered) tool lists of all backends.
//     A tool name claimed by more than one backend is listed once:
//     calls route to the first owner only (backends are visited in
//     sorted name order), so later duplicates are dropped.
//   - tools/call routes to the backend that owns the named tool
//     (first backend in sorted name order wins on a name collision).
//   - notifications are forwarded to every backend fire-and-forget.
//
// Per backend, the same pipeline as the HTTP path applies: tool
// filter, circuit breaker, response cache, metrics. Two HTTP concerns
// deliberately do not apply: there is no API-key auth and no rate
// limiting, because stdio is a local trust boundary — whoever can
// spawn the process already controls it, exactly like any other stdio
// MCP server. Callers must keep stdout reserved for protocol traffic:
// in stdio mode the process logger writes to stderr (see cmd/mcpgate).

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/sunwen1983/mcpgate/internal/backend"
	"github.com/sunwen1983/mcpgate/internal/breaker"
	"github.com/sunwen1983/mcpgate/internal/metrics"
)

// stdioServerVersion is reported as serverInfo.version in initialize
// responses. Kept in step with the release version in the README.
const stdioServerVersion = "0.1.0"

// defaultProtocolVersion is answered to initialize requests that do
// not name a protocol version.
const defaultProtocolVersion = "2025-03-26"

// ServeStdio serves MCP over the given streams until EOF on in or the
// context ends. Each input line is one JSON-RPC message; each response
// is written as one JSON line. It returns nil on a clean EOF.
func (g *Gateway) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	s := &stdioSession{g: g, enc: json.NewEncoder(out)}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64<<10), 10<<20)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		s.handleLine(ctx, line)
	}
	return scanner.Err()
}

// stdioSession is the per-stream state of one ServeStdio call.
type stdioSession struct {
	g         *Gateway
	enc       *json.Encoder
	toolOwner map[string]string // tool name -> backend name, built by tools/list
}

// handleLine processes one raw JSON-RPC message.
func (s *stdioSession) handleLine(ctx context.Context, line []byte) {
	g := s.g
	log := g.logger.With("request_id", newRequestID(), "api_key", "stdio")
	req, id, isNotification, err := parseBody(line)
	if err != nil {
		log.Info("request rejected", "reason", "parse_error", "error", err)
		s.write(&backend.Response{JSONRPC: "2.0", ID: nil,
			Error: &backend.Error{Code: -32700, Message: "parse error: invalid JSON-RPC request"}})
		return
	}
	if isNotification {
		for _, name := range g.backendNames() {
			g.forwardNotification(ctx, log, g.backends[name], name, req)
		}
		return
	}
	log = log.With("method", req.Method)
	if resp := s.dispatch(ctx, log, req, id); resp != nil {
		s.write(resp)
	}
}

// write emits one response line. Encoding cannot fail for the response
// shapes built here; a broken output stream surfaces on the next read
// or at EOF handling in ServeStdio.
func (s *stdioSession) write(resp *backend.Response) {
	_ = s.enc.Encode(resp)
}

// dispatch routes one request. It returns the response to write.
func (s *stdioSession) dispatch(ctx context.Context, log *slog.Logger, req *backend.Request, id any) *backend.Response {
	switch req.Method {
	case "initialize":
		pv, _ := req.Params["protocolVersion"].(string)
		if pv == "" {
			pv = defaultProtocolVersion
		}
		return &backend.Response{JSONRPC: "2.0", ID: id, Result: map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "mcpgate", "version": stdioServerVersion},
		}}
	case "ping":
		return &backend.Response{JSONRPC: "2.0", ID: id, Result: map[string]any{}}
	case "tools/list":
		tools, errResp := s.aggregateTools(ctx, log, req)
		if errResp != nil {
			errResp.ID = id
			return errResp
		}
		return &backend.Response{JSONRPC: "2.0", ID: id, Result: map[string]any{"tools": tools}}
	case "tools/call":
		return s.callTool(ctx, log, req, id)
	default:
		return &backend.Response{JSONRPC: "2.0", ID: id,
			Error: &backend.Error{Code: -32601,
				Message: fmt.Sprintf("method %q is not supported in stdio mode", req.Method)}}
	}
}

// aggregateTools fetches every backend's filtered tool list, merges
// them, and (re)builds the tool->backend routing index. A tool name
// claimed by more than one backend appears only once in the merged
// list: calls can only route to the first owner, so listing a later
// backend's copy would promise a tool the client cannot actually
// reach. A backend that is down or whose breaker is open is skipped
// and logged; only if every configured backend fails does the call
// return an error response. Returns the merged tools (never nil).
func (s *stdioSession) aggregateTools(ctx context.Context, log *slog.Logger, req *backend.Request) ([]any, *backend.Response) {
	g := s.g
	merged := []any{}
	s.toolOwner = map[string]string{}
	failures := 0
	for _, name := range g.backendNames() {
		st := g.backends[name]
		tools, err := s.fetchTools(ctx, log, st, name, req.Params)
		if err != nil {
			failures++
			log.Info("tools/list skipped backend", "backend", name, "error", err)
			continue
		}
		for _, tool := range tools {
			if m, ok := tool.(map[string]any); ok {
				if toolName, _ := m["name"].(string); toolName != "" {
					if _, taken := s.toolOwner[toolName]; taken {
						// Already listed by an earlier backend:
						// calls route to the first owner only,
						// so drop the duplicate from the list.
						continue
					}
					s.toolOwner[toolName] = name
				}
			}
			merged = append(merged, tool)
		}
	}
	if len(g.backends) > 0 && failures == len(g.backends) {
		return nil, &backend.Response{JSONRPC: "2.0",
			Error: &backend.Error{Code: jsonRPCErrorCodeBackendFailure,
				Message: "all backends unavailable"}}
	}
	return merged, nil
}

// fetchTools returns one backend's tool list with the gateway pipeline
// applied: breaker check, response cache, tool filter. A JSON-RPC
// error from the backend is returned as a Go error so the caller can
// skip the backend.
func (s *stdioSession) fetchTools(ctx context.Context, log *slog.Logger, st *backendState, name string, params map[string]any) ([]any, error) {
	g := s.g
	if err := st.breaker.Allow(); err != nil {
		return nil, err
	}
	listReq := &backend.Request{JSONRPC: "2.0", Method: "tools/list", Params: params}
	ttl := g.cacheTTL(st, listReq)
	var key string
	if ttl > 0 {
		key = cacheKey(name, "tools/list", params)
		if cached, hit := st.cache.Get(key); hit {
			metrics.CacheHits.WithLabelValues(name, "tools/list").Inc()
			var payload cachedPayload
			if err := json.Unmarshal(cached, &payload); err == nil && payload.Error == nil {
				resp := &backend.Response{Result: payload.Result}
				filterToolsList(st.cfg.ToolFilter, resp)
				// Cache hit consumed the breaker probe but proves
				// nothing about the backend: free the slot (see
				// Breaker.ReleaseProbe) so a half-open breaker is not wedged.
				st.breaker.ReleaseProbe()
				return toolsOf(resp), nil
			}
			// Corrupt or errored entry: fall through to the backend.
		}
		metrics.CacheMisses.WithLabelValues(name, "tools/list").Inc()
	}
	resp, err := st.backend.Call(ctx, listReq)
	if err != nil {
		st.breaker.RecordFailure()
		metrics.BackendErrors.WithLabelValues(name).Inc()
		return nil, err
	}
	st.breaker.RecordSuccess()
	if resp.Error != nil {
		return nil, resp.Error
	}
	if resp.Result == nil {
		return []any{}, nil
	}
	filterToolsList(st.cfg.ToolFilter, resp)
	if ttl > 0 {
		if payload, err := json.Marshal(cachedPayload{Result: resp.Result}); err == nil {
			st.cache.Set(key, payload, ttl)
		}
	}
	return toolsOf(resp), nil
}

// toolsOf extracts the tools array from a tools/list response result.
func toolsOf(resp *backend.Response) []any {
	if resp == nil || resp.Result == nil {
		return []any{}
	}
	tools, _ := resp.Result["tools"].([]any)
	if tools == nil {
		return []any{}
	}
	return tools
}

// callTool routes a tools/call to the owning backend through the
// breaker, tool filter, and cache, mirroring the HTTP path.
func (s *stdioSession) callTool(ctx context.Context, log *slog.Logger, req *backend.Request, id any) *backend.Response {
	g := s.g
	name, _ := req.Params["name"].(string)
	if name == "" {
		return &backend.Response{JSONRPC: "2.0", ID: id,
			Error: &backend.Error{Code: -32602, Message: "tools/call requires a tool name"}}
	}
	if s.toolOwner == nil {
		// Route index not built yet (client called a tool without
		// listing first): build it from a tools/list round.
		if _, errResp := s.aggregateTools(ctx, log, req); errResp != nil {
			errResp.ID = id
			return errResp
		}
	}
	owner, ok := s.toolOwner[name]
	if !ok {
		return &backend.Response{JSONRPC: "2.0", ID: id,
			Error: &backend.Error{Code: -32602, Message: fmt.Sprintf("unknown tool %q", name)}}
	}
	st := g.backends[owner]
	log = log.With("backend", owner, "tool", name)

	if toolBlocked(st.cfg.ToolFilter, name) {
		metrics.RequestsTotal.WithLabelValues(owner, req.Method, "rejected").Inc()
		log.Info("request rejected", "reason", "tool_blocked")
		return &backend.Response{JSONRPC: "2.0", ID: id,
			Error: &backend.Error{Code: -32602, Message: fmt.Sprintf("tool %q is not available", name)}}
	}
	if err := st.breaker.Allow(); err != nil {
		if errors.Is(err, breaker.ErrOpen) {
			metrics.RequestsTotal.WithLabelValues(owner, req.Method, "rejected").Inc()
			log.Info("request rejected", "reason", "circuit_open")
			return &backend.Response{JSONRPC: "2.0", ID: id,
				Error: &backend.Error{Code: jsonRPCErrorCodeBackendFailure,
					Message: "backend unavailable: circuit breaker open"}}
		}
	}

	ttl := g.cacheTTL(st, req)
	var key string
	if ttl > 0 {
		key = cacheKey(owner, req.Method, req.Params)
		if cached, hit := st.cache.Get(key); hit {
			metrics.CacheHits.WithLabelValues(owner, req.Method).Inc()
			var payload cachedPayload
			if uerr := json.Unmarshal(cached, &payload); uerr == nil {
				metrics.RequestsTotal.WithLabelValues(owner, req.Method, "ok").Inc()
				log.Info("request served", "cache", "hit")
				// Cache hit consumed the breaker probe but proves
				// nothing about the backend: free the slot (see
				// Breaker.ReleaseProbe) so a half-open breaker is not wedged.
				st.breaker.ReleaseProbe()
				return &backend.Response{JSONRPC: "2.0", ID: id,
					Result: payload.Result, Error: payload.Error}
			} else {
				log.Error("cached entry corrupt, refetching", "error", uerr)
			}
		}
		metrics.CacheMisses.WithLabelValues(owner, req.Method).Inc()
	}

	start := time.Now()
	resp, err := st.backend.Call(ctx, req)
	elapsed := time.Since(start)
	metrics.RequestDuration.WithLabelValues(owner).Observe(elapsed.Seconds())
	if err != nil {
		metrics.BackendErrors.WithLabelValues(owner).Inc()
		metrics.RequestsTotal.WithLabelValues(owner, req.Method, "backend_error").Inc()
		st.breaker.RecordFailure()
		metrics.BreakerState.WithLabelValues(owner).Set(float64(breakerStateValue(st.breaker.State())))
		log.Error("backend call failed", "error", err, "latency_ms", elapsed.Milliseconds())
		return &backend.Response{JSONRPC: "2.0", ID: id,
			Error: &backend.Error{Code: jsonRPCErrorCodeBackendFailure,
				Message: "backend error: " + err.Error()}}
	}
	st.breaker.RecordSuccess()
	metrics.BreakerState.WithLabelValues(owner).Set(float64(breakerStateValue(st.breaker.State())))

	status := "ok"
	if resp.Error != nil {
		// A JSON-RPC error from the backend is forwarded verbatim and
		// never cached, exactly as on the HTTP path.
		status = "rpc_error"
		ttl = 0
	}
	metrics.RequestsTotal.WithLabelValues(owner, req.Method, status).Inc()
	resp.JSONRPC = "2.0"
	resp.ID = id // echo the client's id, not the backend's
	if ttl > 0 {
		if payload, err := json.Marshal(cachedPayload{Result: resp.Result, Error: resp.Error}); err == nil {
			st.cache.Set(key, payload, ttl)
		}
	}
	log.Info("request served", "cache", "miss", "latency_ms", elapsed.Milliseconds(), "status", status)
	return resp
}

// backendNames returns the configured backend names in sorted order so
// aggregation and notifications are deterministic.
func (g *Gateway) backendNames() []string {
	names := make([]string, 0, len(g.backends))
	for name := range g.backends {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
