package backend_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/sunwen1983/mcpgate/internal/backend"
	"github.com/sunwen1983/mcpgate/internal/config"
)

// buildEchoBackend compiles examples/echo-backend into a temp binary.
func buildEchoBackend(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "echo-backend")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	// Locate the repo root from this test file's directory.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	srcDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "examples", "echo-backend")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build echo-backend: %v\n%s", err, out)
	}
	return bin
}

func stdioConfig(bin string) config.Backend {
	return config.Backend{
		Name:      "echo",
		Transport: "stdio",
		Command:   []string{bin},
		Timeout:   config.Duration(5 * time.Second),
	}
}

func TestStdioBackendEcho(t *testing.T) {
	bin := buildEchoBackend(t)
	b, err := backend.New(stdioConfig(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer b.Close()

	ctx := context.Background()
	// initialize
	resp, err := b.Call(ctx, &backend.Request{JSONRPC: "2.0", ID: 1, Method: "initialize", Params: map[string]any{}})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("initialize: rpc error: %v", resp.Error)
	}
	if resp.Result["protocolVersion"] == nil {
		t.Errorf("initialize: missing protocolVersion in %v", resp.Result)
	}

	// tools/list
	resp, err = b.Call(ctx, &backend.Request{JSONRPC: "2.0", ID: 2, Method: "tools/list"})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	tools, ok := resp.Result["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools/list: tools = %v, want 2 tools", resp.Result["tools"])
	}

	// tools/call add
	resp, err = b.Call(ctx, &backend.Request{
		JSONRPC: "2.0", ID: 3, Method: "tools/call",
		Params: map[string]any{"name": "add", "arguments": map[string]any{"a": 2.0, "b": 3.0}},
	})
	if err != nil {
		t.Fatalf("tools/call add: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("tools/call add: rpc error: %v", resp.Error)
	}
	content, _ := resp.Result["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "5" {
		t.Errorf("tools/call add: content = %v, want text 5", resp.Result["content"])
	}
}

func TestStdioBackendUnknownTool(t *testing.T) {
	bin := buildEchoBackend(t)
	b, err := backend.New(stdioConfig(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer b.Close()

	resp, err := b.Call(context.Background(), &backend.Request{
		JSONRPC: "2.0", ID: 1, Method: "tools/call",
		Params: map[string]any{"name": "nope"},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("Error = %v, want code -32602", resp.Error)
	}
}

func TestStdioBackendRestartsAfterCrash(t *testing.T) {
	bin := buildEchoBackend(t)
	b, err := backend.New(stdioConfig(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer b.Close()

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Next call must restart the child transparently.
	resp, err := b.Call(context.Background(), &backend.Request{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	if err != nil {
		t.Fatalf("Call after Close: %v", err)
	}
	if resp.Result["tools"] == nil {
		t.Errorf("tools/list after restart: %v", resp.Result)
	}
}

func TestHTTPBackend(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var req backend.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(backend.Response{
			JSONRPC: "2.0", ID: req.ID,
			Result: map[string]any{"echo": req.Method},
		})
	}))
	defer srv.Close()

	cfg := config.Backend{
		Name:      "remote",
		Transport: "http",
		URL:       srv.URL,
		Auth:      config.BackendAuth{Type: "bearer", Token: "backend-secret"},
		Timeout:   config.Duration(5 * time.Second),
	}
	b, err := backend.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := b.Call(context.Background(), &backend.Request{JSONRPC: "2.0", ID: 7, Method: "tools/list"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Result["echo"] != "tools/list" {
		t.Errorf("Result = %v", resp.Result)
	}
	if gotAuth != "Bearer backend-secret" {
		t.Errorf("Authorization = %q, want bearer token", gotAuth)
	}
}

func TestHTTPBackendErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	b, err := backend.New(config.Backend{Name: "bad", Transport: "http", URL: srv.URL, Timeout: config.Duration(5 * time.Second)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = b.Call(context.Background(), &backend.Request{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	if err == nil {
		t.Error("Call against 500: want error")
	}
}

func TestHTTPBackendRejectsUnknownContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>nope</html>"))
	}))
	defer srv.Close()

	b, err := backend.New(config.Backend{Name: "html", Transport: "http", URL: srv.URL, Timeout: config.Duration(5 * time.Second)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = b.Call(context.Background(), &backend.Request{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	if err == nil {
		t.Error("HTML response: want error (unsupported content type)")
	}
}

func TestHTTPBackendSSESingleResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"tools\":[]}}\n\n"))
	}))
	defer srv.Close()

	b, err := backend.New(config.Backend{Name: "sse", Transport: "http", URL: srv.URL, Timeout: config.Duration(5 * time.Second)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := b.Call(context.Background(), &backend.Request{JSONRPC: "2.0", ID: 7, Method: "tools/list"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Result["tools"] == nil {
		t.Errorf("Result = %v, want tools list", resp.Result)
	}
}

func TestHTTPBackendSSESkipsNonMatchingEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		// A progress notification, a heartbeat comment, the real answer
		// (with a string id), then a trailing event for another request.
		_, _ = w.Write([]byte("" +
			"event: message\n" +
			"data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n" +
			"\n" +
			": heartbeat\n" +
			"\n" +
			"data: {\"jsonrpc\":\"2.0\",\"id\":\"abc\",\"result\":{\"ok\":true}}\n" +
			"\n" +
			"data: {\"jsonrpc\":\"2.0\",\"id\":999,\"result\":{\"nope\":true}}"))
	}))
	defer srv.Close()

	b, err := backend.New(config.Backend{Name: "sse", Transport: "http", URL: srv.URL, Timeout: config.Duration(5 * time.Second)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := b.Call(context.Background(), &backend.Request{JSONRPC: "2.0", ID: "abc", Method: "tools/call"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Result["ok"] != true {
		t.Errorf("Result = %v, want {\"ok\":true}", resp.Result)
	}
}

func TestHTTPBackendSSEMultiLineData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// A data field split across two lines is joined with a newline.
		// A JSON payload split this way would be invalid, but a plain
		// multi-line data event must not crash the parser; here the
		// response id arrives in the second event.
		_, _ = w.Write([]byte("" +
			"data: not json at all\n" +
			"data: still not json\n" +
			"\n" +
			"data: {\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{\"v\":1}}\n\n"))
	}))
	defer srv.Close()

	b, err := backend.New(config.Backend{Name: "sse", Transport: "http", URL: srv.URL, Timeout: config.Duration(5 * time.Second)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := b.Call(context.Background(), &backend.Request{JSONRPC: "2.0", ID: 3, Method: "tools/list"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Result["v"] != float64(1) {
		t.Errorf("Result = %v, want {\"v\":1}", resp.Result)
	}
}

func TestHTTPBackendSSENoMatchingID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"id\":999,\"result\":{}}\n\n"))
	}))
	defer srv.Close()

	b, err := backend.New(config.Backend{Name: "sse", Transport: "http", URL: srv.URL, Timeout: config.Duration(5 * time.Second)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = b.Call(context.Background(), &backend.Request{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	if err == nil {
		t.Error("SSE stream without matching id: want error")
	}
}

func TestHTTPBackendSSEEmptyStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": just a heartbeat\n\n"))
	}))
	defer srv.Close()

	b, err := backend.New(config.Backend{Name: "sse", Transport: "http", URL: srv.URL, Timeout: config.Duration(5 * time.Second)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = b.Call(context.Background(), &backend.Request{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	if err == nil {
		t.Error("SSE stream with no data events: want error")
	}
}

func TestUnknownTransport(t *testing.T) {
	_, err := backend.New(config.Backend{Name: "x", Transport: "grpc"})
	if err == nil {
		t.Error("New(grpc): want error")
	}
}

func TestStdioNotify(t *testing.T) {
	bin := buildEchoBackend(t)
	b, err := backend.New(stdioConfig(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer b.Close()

	ctx := context.Background()
	// The echo backend answers nothing to notifications; a subsequent
	// request must see a clean stream (no desync).
	if err := b.Notify(ctx, "notifications/initialized", map[string]any{"protocolVersion": "2025-03-26"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	resp, err := b.Call(ctx, &backend.Request{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	if err != nil {
		t.Fatalf("Call after Notify: %v", err)
	}
	if tools, ok := resp.Result["tools"].([]any); !ok || len(tools) != 2 {
		t.Errorf("tools/list after Notify: %v", resp.Result)
	}
}

func TestHTTPNotify(t *testing.T) {
	var gotMethod string
	var gotID any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req backend.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		gotMethod = req.Method
		gotID = req.ID
		w.WriteHeader(http.StatusAccepted) // MCP servers answer notifications with 202
	}))
	defer srv.Close()

	cfg := config.Backend{
		Name:      "remote",
		Transport: "http",
		URL:       srv.URL,
		Timeout:   config.Duration(5 * time.Second),
	}
	b, err := backend.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Notify(context.Background(), "notifications/initialized", map[string]any{"x": 1}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotMethod != "notifications/initialized" {
		t.Errorf("method = %q, want notifications/initialized", gotMethod)
	}
	if gotID != nil {
		t.Errorf("id = %v, want nil (notifications carry no id)", gotID)
	}
}

func TestHTTPNotifyErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	b, err := backend.New(config.Backend{Name: "bad", Transport: "http", URL: srv.URL, Timeout: config.Duration(5 * time.Second)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Notify(context.Background(), "notifications/initialized", nil); err == nil {
		t.Error("Notify against 502: want error")
	}
}

func TestHTTPBackendInjectsHeaders(t *testing.T) {
	var callHeaders, notifyHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req backend.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		if req.ID == nil {
			notifyHeaders = r.Header.Clone()
			w.WriteHeader(http.StatusAccepted)
			return
		}
		callHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(backend.Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"ok": true}})
	}))
	defer srv.Close()

	cfg := config.Backend{
		Name:      "remote",
		Transport: "http",
		URL:       srv.URL,
		Timeout:   config.Duration(5 * time.Second),
		Headers:   map[string]string{"X-Gateway": "mcpgate", "X-Tenant": "acme"},
	}
	b, err := backend.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := b.Call(context.Background(), &backend.Request{JSONRPC: "2.0", ID: 1, Method: "tools/list"}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if err := b.Notify(context.Background(), "notifications/initialized", nil); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	for name, hdrs := range map[string]http.Header{"Call": callHeaders, "Notify": notifyHeaders} {
		if hdrs.Get("X-Gateway") != "mcpgate" || hdrs.Get("X-Tenant") != "acme" {
			t.Errorf("%s: injected headers missing: X-Gateway=%q X-Tenant=%q",
				name, hdrs.Get("X-Gateway"), hdrs.Get("X-Tenant"))
		}
	}
}

func TestHTTPBackendAuthWinsOverInjectedHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(backend.Response{JSONRPC: "2.0", ID: 1, Result: map[string]any{"ok": true}})
	}))
	defer srv.Close()

	cfg := config.Backend{
		Name:      "remote",
		Transport: "http",
		URL:       srv.URL,
		Auth:      config.BackendAuth{Type: "bearer", Token: "backend-secret"},
		Headers:   map[string]string{"Authorization": "injected-value"},
		Timeout:   config.Duration(5 * time.Second),
	}
	b, err := backend.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := b.Call(context.Background(), &backend.Request{JSONRPC: "2.0", ID: 1, Method: "tools/list"}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if gotAuth != "Bearer backend-secret" {
		t.Errorf("Authorization = %q, want backend auth to win", gotAuth)
	}
}
