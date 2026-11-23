package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// mockBackend is a minimal Streamable HTTP MCP server: it answers
// tools/list and tools/call as JSON-RPC 2.0 with the request's id
// echoed back and Content-Type application/json — exactly what the
// gateway's HTTP backend requires (id match, JSON content type).
// Anything else gets a -32601 method-not-found error. Calls counts
// every request served, so the harness can prove each load request
// reached the backend instead of being served from a cache.
type mockBackend struct {
	delay time.Duration
	mu    sync.Mutex
	calls int
}

func newMockBackend(delay time.Duration) *mockBackend {
	return &mockBackend{delay: delay}
}

// Calls returns how many requests the mock has served.
func (m *mockBackend) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *mockBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if m.delay > 0 {
		time.Sleep(m.delay)
	}
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()

	var req struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      any            `json:"id"`
		Method  string         `json:"method"`
		Params  map[string]any `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}

	switch req.Method {
	case "tools/list":
		writeRPCResult(w, req.ID, map[string]any{
			"tools": []map[string]any{
				{
					"name":        "echo",
					"description": "Return the input text unchanged.",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"text": map[string]any{"type": "string"}},
						"required":   []string{"text"},
					},
				},
				{
					"name":        "other",
					"description": "A tool that is never cached.",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{},
					},
				},
			},
		})
	case "tools/call":
		writeRPCResult(w, req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}},
		})
	default:
		writeRPCError(w, req.ID, -32601, "method not found")
	}
}

func writeRPCResult(w http.ResponseWriter, id any, result map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func writeRPCError(w http.ResponseWriter, id any, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
}

var _ http.Handler = (*mockBackend)(nil)
