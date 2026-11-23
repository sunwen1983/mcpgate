package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestPercentile pins the latency math before any harness exists: the
// headline P50/P99 numbers are only trustworthy if this is right.
func TestPercentile(t *testing.T) {
	lat := []time.Duration{
		10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond,
		40 * time.Millisecond, 50 * time.Millisecond, 60 * time.Millisecond,
		70 * time.Millisecond, 80 * time.Millisecond, 90 * time.Millisecond,
		100 * time.Millisecond,
	}
	if got := percentile(lat, 50); got != 50*time.Millisecond {
		t.Errorf("p50 = %v, want 50ms", got)
	}
	if got := percentile(lat, 99); got != 100*time.Millisecond {
		t.Errorf("p99 = %v, want 100ms", got)
	}
	if got := percentile(lat, 100); got != 100*time.Millisecond {
		t.Errorf("p100 = %v, want 100ms", got)
	}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("p50 of empty = %v, want 0", got)
	}
}

func TestSummarize(t *testing.T) {
	lat := make([]time.Duration, 100)
	for i := range lat {
		lat[i] = time.Duration(i+1) * time.Millisecond
	}
	rep := summarize(lat, 2, 100*time.Millisecond)
	if rep.Requests != 100 {
		t.Errorf("requests = %d, want 100", rep.Requests)
	}
	if rep.Errors != 2 {
		t.Errorf("errors = %d, want 2", rep.Errors)
	}
	if rep.QPS != 1000 {
		t.Errorf("qps = %v, want 1000 (100 requests in 100ms)", rep.QPS)
	}
	if rep.P50 != 50*time.Millisecond {
		t.Errorf("p50 = %v, want 50ms", rep.P50)
	}
}

// TestMockBackend checks the mock MCP backend answers tools/list and
// tools/call as JSON-RPC 2.0 with the request's id echoed back —
// exactly what the gateway's HTTP backend requires (id match,
// application/json).
func TestMockBackend(t *testing.T) {
	mock := newMockBackend(0)
	srv := httptest.NewServer(mock)
	defer srv.Close()

	for _, tc := range []struct {
		method string
		params map[string]any
	}{
		{"tools/list", nil},
		{"tools/call", map[string]any{"name": "other", "arguments": map[string]any{}}},
	} {
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 7, "method": tc.method, "params": tc.params,
		})
		resp, err := http.Post(srv.URL+"/mcp", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", tc.method, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("%s: content-type = %q, want application/json", tc.method, ct)
		}
		var out struct {
			JSONRPC string         `json:"jsonrpc"`
			ID      int            `json:"id"`
			Result  map[string]any `json:"result"`
			Error   *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("%s: decode: %v", tc.method, err)
		}
		if out.JSONRPC != "2.0" || out.ID != 7 || out.Error != nil || out.Result == nil {
			t.Fatalf("%s: bad response: %+v", tc.method, out)
		}
	}
	if got := mock.Calls(); got != 2 {
		t.Errorf("mock calls = %d, want 2", got)
	}
}

// TestRunEndToEnd is the failing feedback loop for the whole seam:
// build the real mcpgate binary, start it against the mock backend,
// fire concurrent load at it, and require zero errors. Small numbers
// keep it fast; the headline run uses the binary directly.
func TestRunEndToEnd(t *testing.T) {
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	rep, err := run(context.Background(), Options{
		GoBinary:    goBin,
		RepoDir:     "../..",
		Concurrency: 2,
		Requests:    20,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Requests != 20 {
		t.Errorf("requests = %d, want 20", rep.Requests)
	}
	if rep.Errors != 0 {
		t.Errorf("errors = %d, want 0", rep.Errors)
	}
	if rep.QPS <= 0 {
		t.Errorf("qps = %v, want > 0", rep.QPS)
	}
	if rep.P50 <= 0 {
		t.Errorf("p50 = %v, want > 0", rep.P50)
	}
}
