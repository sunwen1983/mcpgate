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
	"github.com/sunwen1983/mcpgate/internal/testoidc"
)

// fakeBackend is a scriptable Backend for gateway tests.
type fakeBackend struct {
	mu    sync.Mutex
	calls int
	err   error
	resp  func(req *backend.Request) *backend.Response

	notifyErr  error
	notified   []notif
	notifCount int
}

type notif struct {
	method string
	params map[string]any
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

func (f *fakeBackend) Notify(_ context.Context, method string, params map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notifCount++
	if f.notifyErr != nil {
		return f.notifyErr
	}
	f.notified = append(f.notified, notif{method: method, params: params})
	return nil
}

func (f *fakeBackend) Close() error { return nil }

func (f *fakeBackend) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeBackend) Notifications() []notif {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notif(nil), f.notified...)
}

func (f *fakeBackend) NotificationCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.notifCount
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

// A cache hit served while the breaker is half-open consumes the probe
// slot (the breaker check runs before the cache lookup). If the hit does
// not free that slot, the breaker wedges: every later request fails fast
// with 503 even though the backend is back.
func TestCacheHitDoesNotWedgeHalfOpenBreaker(t *testing.T) {
	cfg := testConfig()
	cfg.Backends[0].Breaker.Cooldown = config.Duration(20 * time.Millisecond)
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, cfg, fake)
	defer srv.Close()

	// Prime the tools/list cache.
	status, _, _ := post(t, srv, "/mcp/fake", "key-alice",
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if status != http.StatusOK {
		t.Fatalf("prime: status = %d", status)
	}

	// Trip the breaker with two backend failures (non-cacheable calls).
	fake.err = errors.New("backend down")
	callBody := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"dangerous","arguments":{}}}`
	for i := 0; i < 2; i++ {
		status, _, _ := post(t, srv, "/mcp/fake", "key-alice", callBody)
		if status != http.StatusBadGateway {
			t.Fatalf("failure %d: status = %d, want 502", i+1, status)
		}
	}
	fake.err = nil

	// Cooldown passes; the next request is a cache hit, not a probe.
	time.Sleep(50 * time.Millisecond)
	status, hdr, _ := post(t, srv, "/mcp/fake", "key-alice",
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if status != http.StatusOK || hdr.Get("X-Cache") != "HIT" {
		t.Fatalf("cache hit: status = %d, X-Cache = %q, want 200/HIT", status, hdr.Get("X-Cache"))
	}

	// The backend must be reachable again: the cache hit must not have
	// wedged the breaker.
	status, _, _ = post(t, srv, "/mcp/fake", "key-alice", callBody)
	if status != http.StatusOK {
		t.Fatalf("after cache hit: status = %d, want 200 (breaker wedged)", status)
	}
	if fake.Calls() != 4 {
		t.Errorf("backend calls = %d, want 4 (prime + 2 failures + probe)", fake.Calls())
	}
}

func TestNotificationForwarded(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	body := `{"jsonrpc":"2.0","method":"notifications/initialized","params":{"protocolVersion":"2025-03-26"}}`
	status, _, _ := post(t, srv, "/mcp/fake", "key-alice", body)
	if status != http.StatusAccepted {
		t.Errorf("notification: status = %d, want 202", status)
	}
	notified := fake.Notifications()
	if len(notified) != 1 {
		t.Fatalf("notifications forwarded = %d, want 1", len(notified))
	}
	if notified[0].method != "notifications/initialized" {
		t.Errorf("method = %q, want notifications/initialized", notified[0].method)
	}
	if notified[0].params["protocolVersion"] != "2025-03-26" {
		t.Errorf("params = %v, want protocolVersion forwarded", notified[0].params)
	}
	if fake.Calls() != 0 {
		t.Errorf("notification must not go through Call: %d calls", fake.Calls())
	}
}

func TestNotificationForwardFailureStill202(t *testing.T) {
	// The client asked for acceptance, not delivery confirmation: a backend
	// that cannot be reached still gets a 202, and the failure is logged
	// and counted rather than surfaced.
	fake := &fakeBackend{notifyErr: errors.New("backend down")}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	status, _, _ := post(t, srv, "/mcp/fake", "key-alice", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if status != http.StatusAccepted {
		t.Errorf("notification: status = %d, want 202", status)
	}
	if fake.NotificationCount() != 1 {
		t.Errorf("forward attempts = %d, want 1", fake.NotificationCount())
	}
}

func TestNotificationDroppedWhenBreakerOpen(t *testing.T) {
	// Two notification-forward failures trip the breaker (threshold 2).
	// The third notification is dropped without touching the backend.
	fake := &fakeBackend{notifyErr: errors.New("backend down")}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	body := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	for i := 0; i < 2; i++ {
		if status, _, _ := post(t, srv, "/mcp/fake", "key-alice", body); status != http.StatusAccepted {
			t.Fatalf("attempt %d: status = %d, want 202", i+1, status)
		}
	}
	if status, _, _ := post(t, srv, "/mcp/fake", "key-alice", body); status != http.StatusAccepted {
		t.Errorf("breaker open: status = %d, want 202 (dropped, not surfaced)", status)
	}
	if fake.NotificationCount() != 2 {
		t.Errorf("forward attempts = %d, want 2 (third must fail fast)", fake.NotificationCount())
	}
}

func TestNotificationStillNeedsAuth(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	status, _, _ := post(t, srv, "/mcp/fake", "", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if status != http.StatusUnauthorized {
		t.Errorf("no key: status = %d, want 401", status)
	}
	if fake.NotificationCount() != 0 {
		t.Errorf("unauthorized notification reached backend: %d forwards", fake.NotificationCount())
	}
}

func TestMalformedNotificationRejected(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	// No method: not a valid notification, so 400, not 202.
	status, _, body := post(t, srv, "/mcp/fake", "key-alice", `{"jsonrpc":"2.0"}`)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if resp := parseRPC(t, body); resp.Error == nil || resp.Error.Code != -32700 {
		t.Errorf("error = %+v, want -32700", resp.Error)
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

func TestReadyz(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	get := func() (int, map[string]any) {
		t.Helper()
		resp, err := http.Get(srv.URL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("readyz body %q: %v", body, err)
		}
		return resp.StatusCode, got
	}

	// Healthy backend: 200, breaker closed.
	status, got := get()
	if status != http.StatusOK || got["status"] != "ready" {
		t.Errorf("readyz = %d %v, want 200 ready", status, got)
	}
	if backends, _ := got["backends"].(map[string]any); backends["fake"] != "closed" {
		t.Errorf("backends = %v, want fake=closed", got["backends"])
	}

	// Trip the breaker (testConfig threshold is 2) via real failures.
	fake.mu.Lock()
	fake.err = errors.New("boom")
	fake.mu.Unlock()
	_, _, _ = post(t, srv, "/mcp/fake", "key-alice", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	_, _, _ = post(t, srv, "/mcp/fake", "key-alice", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)

	status, got = get()
	if status != http.StatusServiceUnavailable || got["status"] != "degraded" {
		t.Errorf("readyz after trip = %d %v, want 503 degraded", status, got)
	}
	if backends, _ := got["backends"].(map[string]any); backends["fake"] != "open" {
		t.Errorf("backends = %v, want fake=open", got["backends"])
	}

	// Non-GET is rejected.
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST readyz = %d, want 405", resp.StatusCode)
	}
}

func TestOIDCAuthEndToEnd(t *testing.T) {
	idp := testoidc.New(t)
	cfg := testConfig()
	cfg.OIDC = &config.OIDCConfig{Issuer: idp.Issuer(), Audience: "mcpgate-test"}
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, cfg, fake)
	defer srv.Close()

	postBearer := func(token string) int {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp/fake",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := postBearer(idp.Sign(t, "key-1", idp.Claims("mcpgate-test", nil))); code != http.StatusOK {
		t.Errorf("valid JWT: status = %d, want 200", code)
	}
	if code := postBearer("not.a.jwt"); code != http.StatusUnauthorized {
		t.Errorf("invalid JWT: status = %d, want 401", code)
	}
	if fake.Calls() != 1 {
		t.Errorf("backend calls = %d, want 1 (only the valid request may pass auth)", fake.Calls())
	}
}

// toolsListBackend returns a backend whose tools/list exposes three tools.
func toolsListBackend() *fakeBackend {
	return &fakeBackend{resp: func(req *backend.Request) *backend.Response {
		if req.Method == "tools/list" {
			return &backend.Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
				"tools": []any{
					map[string]any{"name": "echo", "description": "echo tool"},
					map[string]any{"name": "add", "description": "add tool"},
					map[string]any{"name": "drop_table", "description": "dangerous"},
				},
			}}
		}
		return &backend.Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"ok": true}}
	}}
}

func toolFilterConfig(filter config.ToolFilter) *config.Config {
	cfg := testConfig()
	cfg.Backends[0].ToolFilter = filter
	return cfg
}

func toolNames(t *testing.T, resp backend.Response) []string {
	t.Helper()
	raw, ok := resp.Result["tools"].([]any)
	if !ok {
		t.Fatalf("result.tools = %v (%T), want a list", resp.Result["tools"], resp.Result["tools"])
	}
	names := make([]string, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("tool entry = %v (%T), want an object", item, item)
		}
		name, _ := m["name"].(string)
		names = append(names, name)
	}
	return names
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestToolCallDeniedToolRejected(t *testing.T) {
	fake := toolsListBackend()
	cfg := toolFilterConfig(config.ToolFilter{Deny: []string{"drop_table"}})
	srv := gateway.NewTestServer(t, cfg, fake)
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"drop_table"}}`
	status, _, data := post(t, srv, "/mcp/fake", "key-alice", body)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	resp := parseRPC(t, data)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("error = %+v, want JSON-RPC -32602", resp.Error)
	}
	if fake.Calls() != 0 {
		t.Errorf("backend calls = %d, want 0 (blocked tool must not reach the backend)", fake.Calls())
	}

	// An unfiltered tool still goes through.
	body = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo"}}`
	status, _, _ = post(t, srv, "/mcp/fake", "key-alice", body)
	if status != http.StatusOK {
		t.Errorf("allowed tool: status = %d, want 200", status)
	}
	if fake.Calls() != 1 {
		t.Errorf("backend calls = %d, want 1", fake.Calls())
	}
}

func TestToolCallAllowList(t *testing.T) {
	fake := toolsListBackend()
	cfg := toolFilterConfig(config.ToolFilter{Allow: []string{"echo"}})
	srv := gateway.NewTestServer(t, cfg, fake)
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add"}}`
	status, _, data := post(t, srv, "/mcp/fake", "key-alice", body)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if resp := parseRPC(t, data); resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("error = %+v, want JSON-RPC -32602", resp.Error)
	}

	// Deny wins over Allow when a tool is in both lists.
	cfg2 := toolFilterConfig(config.ToolFilter{Allow: []string{"echo"}, Deny: []string{"echo"}})
	srv2 := gateway.NewTestServer(t, cfg2, fake)
	defer srv2.Close()
	body = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo"}}`
	status, _, _ = post(t, srv2, "/mcp/fake", "key-alice", body)
	if status != http.StatusForbidden {
		t.Errorf("deny-over-allow: status = %d, want 403", status)
	}
}

func TestToolsListFilteredOnMissAndHit(t *testing.T) {
	fake := toolsListBackend()
	cfg := toolFilterConfig(config.ToolFilter{Deny: []string{"drop_table"}})
	srv := gateway.NewTestServer(t, cfg, fake)
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	status, hdr, data := post(t, srv, "/mcp/fake", "key-alice", body)
	if status != http.StatusOK {
		t.Fatalf("miss: status = %d", status)
	}
	if hdr.Get("X-Cache") != "MISS" {
		t.Errorf("miss: X-Cache = %q, want MISS", hdr.Get("X-Cache"))
	}
	if got := toolNames(t, parseRPC(t, data)); !equalStrings(got, []string{"echo", "add"}) {
		t.Errorf("miss: tools = %v, want [echo add]", got)
	}

	// The cached response must be filtered identically.
	body = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	status, hdr, data = post(t, srv, "/mcp/fake", "key-alice", body)
	if status != http.StatusOK {
		t.Fatalf("hit: status = %d", status)
	}
	if hdr.Get("X-Cache") != "HIT" {
		t.Errorf("hit: X-Cache = %q, want HIT", hdr.Get("X-Cache"))
	}
	if got := toolNames(t, parseRPC(t, data)); !equalStrings(got, []string{"echo", "add"}) {
		t.Errorf("hit: tools = %v, want [echo add]", got)
	}
	if fake.Calls() != 1 {
		t.Errorf("backend calls = %d, want 1", fake.Calls())
	}
}

func TestToolsListAllowList(t *testing.T) {
	fake := toolsListBackend()
	cfg := toolFilterConfig(config.ToolFilter{Allow: []string{"add"}})
	srv := gateway.NewTestServer(t, cfg, fake)
	defer srv.Close()

	_, _, data := post(t, srv, "/mcp/fake", "key-alice", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if got := toolNames(t, parseRPC(t, data)); !equalStrings(got, []string{"add"}) {
		t.Errorf("tools = %v, want [add]", got)
	}
}

func TestAdminInvalidateRequiresAuth(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	status, _, _ := post(t, srv, "/admin/cache/invalidate", "", "")
	if status != http.StatusUnauthorized {
		t.Errorf("no key: status = %d, want 401", status)
	}
	status, _, _ = post(t, srv, "/admin/cache/invalidate/fake", "wrong-key", "")
	if status != http.StatusUnauthorized {
		t.Errorf("wrong key: status = %d, want 401", status)
	}
}

func TestAdminInvalidateMethodNotAllowed(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/admin/cache/invalidate", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Key", "key-alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", resp.StatusCode)
	}
}

func TestAdminInvalidateUnknownBackend(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	status, _, body := post(t, srv, "/admin/cache/invalidate/nope", "key-alice", "")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
	if !strings.Contains(string(body), "unknown backend") {
		t.Errorf("body = %q, want unknown-backend error", body)
	}
}

func toolsList(t *testing.T, srv *httptest.Server, key string) (int, http.Header) {
	t.Helper()
	status, hdr, _ := post(t, srv, "/mcp/fake", key,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	return status, hdr
}

func parseAdminInvalidate(t *testing.T, body []byte) (backends []string, cleared int) {
	t.Helper()
	var v struct {
		Status   string   `json:"status"`
		Backends []string `json:"backends"`
		Cleared  int      `json:"cleared"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("parse admin response %s: %v", body, err)
	}
	return v.Backends, v.Cleared
}

func TestAdminInvalidateOneBackend(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	// Populate the cache: first tools/list is a MISS, the second a HIT.
	if _, hdr := toolsList(t, srv, "key-alice"); hdr.Get("X-Cache") != "MISS" {
		t.Fatalf("first tools/list: X-Cache = %q, want MISS", hdr.Get("X-Cache"))
	}
	if _, hdr := toolsList(t, srv, "key-alice"); hdr.Get("X-Cache") != "HIT" {
		t.Fatalf("second tools/list: X-Cache = %q, want HIT", hdr.Get("X-Cache"))
	}

	status, _, body := post(t, srv, "/admin/cache/invalidate/fake", "key-alice", "")
	if status != http.StatusOK {
		t.Fatalf("invalidate: status = %d, want 200", status)
	}
	backends, cleared := parseAdminInvalidate(t, body)
	if len(backends) != 1 || backends[0] != "fake" {
		t.Errorf("backends = %v, want [fake]", backends)
	}
	if cleared != 1 {
		t.Errorf("cleared = %d, want 1", cleared)
	}

	// The next tools/list must go back to the backend.
	if _, hdr := toolsList(t, srv, "key-alice"); hdr.Get("X-Cache") != "MISS" {
		t.Errorf("after invalidate: X-Cache = %q, want MISS", hdr.Get("X-Cache"))
	}
}

func twoBackendConfig() *config.Config {
	cfg := testConfig()
	cfg.Backends = append(cfg.Backends, config.Backend{
		Name:      "fake2",
		Transport: "http",
		URL:       "http://localhost:1/unused",
		Timeout:   config.Duration(5 * time.Second),
		Cache:     cfg.Backends[0].Cache,
		Breaker:   cfg.Backends[0].Breaker,
	})
	return cfg
}

func TestAdminInvalidateAllBackends(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, twoBackendConfig(), fake)
	defer srv.Close()

	// Populate both backends' caches via the two /mcp paths.
	for _, b := range []string{"fake", "fake2"} {
		status, _, _ := post(t, srv, "/mcp/"+b, "key-alice",
			`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		if status != http.StatusOK {
			t.Fatalf("/mcp/%s: status = %d, want 200", b, status)
		}
	}

	status, _, body := post(t, srv, "/admin/cache/invalidate", "key-alice", "")
	if status != http.StatusOK {
		t.Fatalf("invalidate all: status = %d, want 200", status)
	}
	backends, cleared := parseAdminInvalidate(t, body)
	if len(backends) != 2 {
		t.Errorf("backends = %v, want 2 entries", backends)
	}
	if cleared != 2 {
		t.Errorf("cleared = %d, want 2", cleared)
	}

	// Both backends' caches are gone: the next tools/list is a MISS again.
	for _, b := range []string{"fake", "fake2"} {
		status, hdr, _ := post(t, srv, "/mcp/"+b, "key-alice",
			`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		if status != http.StatusOK || hdr.Get("X-Cache") != "MISS" {
			t.Errorf("/mcp/%s after invalidate: status = %d, X-Cache = %q, want 200/MISS",
				b, status, hdr.Get("X-Cache"))
		}
	}
}
