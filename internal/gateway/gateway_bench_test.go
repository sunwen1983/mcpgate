package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sunwen1983/mcpgate/internal/config"
	"github.com/sunwen1983/mcpgate/internal/gateway"
)

// benchConfig is testConfig with rate limiting raised far above what a
// benchmark can reach, so the numbers measure gateway throughput, not
// the token bucket. Everything else (cache TTLs, breaker, cacheable
// tools) is exactly the test default.
func benchConfig() *config.Config {
	cfg := testConfig()
	cfg.DefaultRateLimit = config.RateLimit{Rate: 1000000, Burst: 1000000}
	return cfg
}

// postBench posts one JSON-RPC request the same way post does, and
// returns the HTTP status. It fails the benchmark on any transport
// error, so a 429 or 500 can never silently pollute the numbers.
func postBench(b *testing.B, srv *httptest.Server, path, apiKey, body string) int {
	b.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		b.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		b.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		b.Fatal(err)
	}
	return resp.StatusCode
}

// BenchmarkToolsCallUncached measures tools/call for a tool that is NOT
// in CacheableTools ("other"), so every measured call reaches the
// backend.
func BenchmarkToolsCallUncached(b *testing.B) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(b, benchConfig(), fake)
	defer srv.Close()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"other","arguments":{}}}`
	if status := postBench(b, srv, "/mcp/fake", "key-alice", body); status != http.StatusOK {
		b.Fatalf("warmup: status = %d, want 200", status)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if status := postBench(b, srv, "/mcp/fake", "key-alice", body); status != http.StatusOK {
			b.Fatalf("status = %d, want 200", status)
		}
	}
	b.StopTimer()
	// Warmup + one backend call per measured iteration: nothing was
	// served from the cache.
	if got := fake.Calls(); got != b.N+1 {
		b.Fatalf("backend calls = %d, want %d (every call should reach the backend)", got, b.N+1)
	}
}

// BenchmarkToolsCallCacheHit measures tools/call for a cacheable tool
// ("echo") with the cache pre-warmed, so every measured call is a hit.
// The backend-call count after the loop proves no measured call
// reached the backend.
func BenchmarkToolsCallCacheHit(b *testing.B) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(b, benchConfig(), fake)
	defer srv.Close()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{}}}`
	if status := postBench(b, srv, "/mcp/fake", "key-alice", body); status != http.StatusOK {
		b.Fatalf("warmup: status = %d, want 200", status)
	}
	if got := fake.Calls(); got != 1 {
		b.Fatalf("warmup: backend calls = %d, want 1", got)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if status := postBench(b, srv, "/mcp/fake", "key-alice", body); status != http.StatusOK {
			b.Fatalf("status = %d, want 200", status)
		}
	}
	b.StopTimer()
	if got := fake.Calls(); got != 1 {
		b.Fatalf("backend calls = %d, want 1 (every measured call should be a cache hit)", got)
	}
}

// BenchmarkToolsList measures tools/list through the same setup. The
// list response is cached for ToolsListTTL after the first call (as in
// production), so the measured calls are cache hits; the backend-call
// count after the loop proves it.
func BenchmarkToolsList(b *testing.B) {
	fake := toolsListBackend()
	srv := gateway.NewTestServer(b, benchConfig(), fake)
	defer srv.Close()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	if status := postBench(b, srv, "/mcp/fake", "key-alice", body); status != http.StatusOK {
		b.Fatalf("warmup: status = %d, want 200", status)
	}
	if got := fake.Calls(); got != 1 {
		b.Fatalf("warmup: backend calls = %d, want 1", got)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if status := postBench(b, srv, "/mcp/fake", "key-alice", body); status != http.StatusOK {
			b.Fatalf("status = %d, want 200", status)
		}
	}
	b.StopTimer()
	if got := fake.Calls(); got != 1 {
		b.Fatalf("backend calls = %d, want 1 (every measured call should be a cache hit)", got)
	}
}
