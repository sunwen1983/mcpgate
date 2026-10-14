package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sunwen1983/mcpgate/internal/backend"
	"github.com/sunwen1983/mcpgate/internal/config"
	"github.com/sunwen1983/mcpgate/internal/gateway"
)

// fakeBackend is a scriptable Backend for gateway tests.
type fakeBackend struct {
	mu    sync.Mutex
	calls int
	err   error
	resp  func(req *backend.Request) *backend.Response
}

func (f *fakeBackend) Name() string { return "fake" }

func (f *fakeBackend) Call(_ context.Context, req *backend.Request) (*backend.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.resp != nil {
		return f.resp(req), nil
	}
	return &backend.Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"ok": true}}, nil
}

func (f *fakeBackend) Close() error { return nil }

func (f *fakeBackend) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func testConfig() *config.Config {
	return &config.Config{
		Listen: ":0",
		APIKeys: []config.APIKey{
			{Key: "key-alice", Name: "alice"},
			{Key: "key-bob", Name: "bob", RateLimit: &config.RateLimit{Rate: 1, Burst: 1}},
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
}

func post(t *testing.T, srv *httptest.Server, path, apiKey, body string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data
}

func parseRPC(t *testing.T, data []byte) backend.Response {
	t.Helper()
	var resp backend.Response
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("parse response %s: %v", data, err)
	}
	return resp
}

func TestAuthRequired(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	status, _, _ := post(t, srv, "/mcp/fake", "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if status != http.StatusUnauthorized {
		t.Errorf("no key: status = %d, want 401", status)
	}
	status, _, _ = post(t, srv, "/mcp/fake", "wrong-key", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if status != http.StatusUnauthorized {
		t.Errorf("wrong key: status = %d, want 401", status)
	}
	if fake.Calls() != 0 {
		t.Errorf("unauthorized requests reached backend: %d calls", fake.Calls())
	}
}

func TestBearerAuth(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp/fake", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer key-alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("bearer auth: status = %d, want 200", resp.StatusCode)
	}
}

func TestUnknownBackend(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	status, _, body := post(t, srv, "/mcp/nope", "key-alice", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
	if resp := parseRPC(t, body); resp.Error == nil || resp.Error.Code != -32601 {
		t.Errorf("error = %+v, want -32601", resp.Error)
	}
}

func TestRateLimit(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	// bob's limiter: burst 1, refill 1/sec.
	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	status, _, _ := post(t, srv, "/mcp/fake", "key-bob", body)
	if status != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", status)
	}
	status, _, _ = post(t, srv, "/mcp/fake", "key-bob", body)
	if status != http.StatusTooManyRequests {
		t.Errorf("second request: status = %d, want 429", status)
	}
}

func TestCacheHitReturnsCurrentID(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	body1 := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	status, hdr, _ := post(t, srv, "/mcp/fake", "key-alice", body1)
	if status != http.StatusOK {
		t.Fatalf("first: status = %d", status)
	}
	if hdr.Get("X-Cache") != "MISS" {
		t.Errorf("first: X-Cache = %q, want MISS", hdr.Get("X-Cache"))
	}
	// Same logical request, different id: must hit the cache and echo id 2.
	body2 := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	status, hdr, data := post(t, srv, "/mcp/fake", "key-alice", body2)
	if status != http.StatusOK {
		t.Fatalf("second: status = %d", status)
	}
	if hdr.Get("X-Cache") != "HIT" {
		t.Errorf("second: X-Cache = %q, want HIT", hdr.Get("X-Cache"))
	}
	resp := parseRPC(t, data)
	if resp.ID != float64(2) {
		t.Errorf("cached response id = %v, want 2 (must echo the current request id)", resp.ID)
	}
	if fake.Calls() != 1 {
		t.Errorf("backend calls = %d, want 1 (second request should be cached)", fake.Calls())
	}
}

func TestNonCacheableToolCallNotCached(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dangerous","arguments":{}}}`
	for i := 0; i < 2; i++ {
		status, hdr, _ := post(t, srv, "/mcp/fake", "key-alice", body)
		if status != http.StatusOK {
			t.Fatalf("request %d: status = %d", i+1, status)
		}
		if hdr.Get("X-Cache") == "HIT" {
			t.Errorf("request %d: unlisted tool must not be cached", i+1)
		}
	}
	if fake.Calls() != 2 {
		t.Errorf("backend calls = %d, want 2", fake.Calls())
	}
}

func TestCircuitBreakerOpens(t *testing.T) {
	fake := &fakeBackend{err: errors.New("backend down")}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	// Two failures trip the breaker (threshold 2).
	for i := 0; i < 2; i++ {
		status, _, _ := post(t, srv, "/mcp/fake", "key-alice", body)
		if status != http.StatusBadGateway {
			t.Errorf("failure %d: status = %d, want 502", i+1, status)
		}
	}
	// Third request fails fast without touching the backend.
	status, _, _ := post(t, srv, "/mcp/fake", "key-alice", body)
	if status != http.StatusServiceUnavailable {
		t.Errorf("breaker open: status = %d, want 503", status)
	}
	if fake.Calls() != 2 {
		t.Errorf("backend calls = %d, want 2 (third request must fail fast)", fake.Calls())
	}
}

func TestNotificationGets202(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	status, _, _ := post(t, srv, "/mcp/fake", "key-alice", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if status != http.StatusAccepted {
		t.Errorf("notification: status = %d, want 202", status)
	}
	if fake.Calls() != 0 {
		t.Errorf("notifications are not forwarded in v0.1: %d calls", fake.Calls())
	}
}

func TestParseError(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	status, _, body := post(t, srv, "/mcp/fake", "key-alice", `not json`)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if resp := parseRPC(t, body); resp.Error == nil || resp.Error.Code != -32700 {
		t.Errorf("error = %+v, want -32700", resp.Error)
	}
}

func TestHealthAndMetrics(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	health, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(health, []byte(`"status":"ok"`)) {
		t.Errorf("healthz = %d %s, want 200 ok", resp.StatusCode, health)
	}

	// Generate one request so metrics are non-empty.
	_, _, _ = post(t, srv, "/mcp/fake", "key-alice", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	resp, err = http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metricsBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d", resp.StatusCode)
	}
	for _, want := range []string{"mcpgate_requests_total", "mcpgate_cache_hits_total", "mcpgate_breaker_state"} {
		if !bytes.Contains(metricsBody, []byte(want)) {
			t.Errorf("metrics missing %s", want)
		}
	}
}
