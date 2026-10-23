package gateway_test

import (
	"bufio"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sunwen1983/mcpgate/internal/backend"
	"github.com/sunwen1983/mcpgate/internal/config"
	"github.com/sunwen1983/mcpgate/internal/gateway"
)

// stdioConfig builds a config with the named backends and no API keys:
// stdio mode is a local trust boundary (no header auth is possible).
func stdioConfig(names ...string) *config.Config {
	cfg := &config.Config{
		Listen:           ":0",
		DefaultRateLimit: config.RateLimit{Rate: 100, Burst: 100},
	}
	for _, n := range names {
		cfg.Backends = append(cfg.Backends, config.Backend{
			Name:      n,
			Transport: "http",
			URL:       "http://localhost:1/unused",
			Timeout:   config.Duration(5 * time.Second),
			Cache: config.CacheConfig{
				ToolsListTTL:   config.Duration(time.Minute),
				ToolsCallTTL:   config.Duration(time.Minute),
				CacheableTools: []string{"echo", "add"},
			},
			Breaker: config.BreakerConfig{FailureThreshold: 5, Cooldown: config.Duration(time.Minute)},
		})
	}
	return cfg
}

// toolsListFake returns a fake backend serving the given tool names.
func toolsListFake(tools ...string) *fakeBackend {
	list := make([]any, 0, len(tools))
	for _, name := range tools {
		list = append(list, map[string]any{"name": name})
	}
	return &fakeBackend{resp: func(req *backend.Request) *backend.Response {
		if req.Method == "tools/list" {
			return &backend.Response{JSONRPC: "2.0", ID: req.ID,
				Result: map[string]any{"tools": list}}
		}
		return &backend.Response{JSONRPC: "2.0", ID: req.ID,
			Result: map[string]any{"ok": true}}
	}}
}

// runStdio feeds lines to ServeStdio and parses every output line as a
// JSON-RPC response — which also proves stdout carries nothing but
// protocol messages.
func runStdio(t *testing.T, g *gateway.Gateway, input string) []backend.Response {
	t.Helper()
	var out strings.Builder
	if err := g.ServeStdio(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}
	var resps []backend.Response
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var resp backend.Response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("stdout line is not JSON-RPC: %q: %v", line, err)
		}
		resps = append(resps, resp)
	}
	return resps
}

func TestStdioInitialize(t *testing.T) {
	g := gateway.NewTestGateway(t, stdioConfig("fake"),
		func(c config.Backend) (backend.Backend, error) { return toolsListFake("echo"), nil })

	resps := runStdio(t, g,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`+"\n")
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	r := resps[0]
	if r.Error != nil {
		t.Fatalf("initialize errored: %v", r.Error)
	}
	if r.Result["protocolVersion"] != "2025-03-26" {
		t.Errorf("protocolVersion = %v, want echoed 2025-03-26", r.Result["protocolVersion"])
	}
	info, ok := r.Result["serverInfo"].(map[string]any)
	if !ok || info["name"] != "mcpgate" {
		t.Errorf("serverInfo = %v, want name mcpgate", r.Result["serverInfo"])
	}
}

func TestStdioToolsListAggregatesBackends(t *testing.T) {
	fakes := map[string]*fakeBackend{
		"one": toolsListFake("echo"),
		"two": toolsListFake("add", "mul"),
	}
	g := gateway.NewTestGateway(t, stdioConfig("one", "two"),
		func(c config.Backend) (backend.Backend, error) { return fakes[c.Name], nil })

	resps := runStdio(t, g, `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`+"\n")
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("tools/list: %+v", resps)
	}
	tools, ok := resps[0].Result["tools"].([]any)
	if !ok || len(tools) != 3 {
		t.Fatalf("tools = %v, want 3 merged tools", resps[0].Result["tools"])
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"echo", "add", "mul"} {
		if !names[want] {
			t.Errorf("merged tools/list missing %q: %v", want, names)
		}
	}
}

func TestStdioToolsListDedupesCollidingToolNames(t *testing.T) {
	fakes := map[string]*fakeBackend{
		"one": toolsListFake("echo", "shared"),
		"two": toolsListFake("shared", "add"),
	}
	g := gateway.NewTestGateway(t, stdioConfig("one", "two"),
		func(c config.Backend) (backend.Backend, error) { return fakes[c.Name], nil })

	input := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"shared"}}` + "\n"
	resps := runStdio(t, g, input)
	if len(resps) != 2 || resps[1].Error != nil {
		t.Fatalf("responses: %+v", resps)
	}
	tools, ok := resps[0].Result["tools"].([]any)
	if !ok || len(tools) != 3 {
		t.Fatalf("tools = %v, want 3 (echo, shared once, add)", resps[0].Result["tools"])
	}
	names := map[string]int{}
	for _, tool := range tools {
		names[tool.(map[string]any)["name"].(string)]++
	}
	if names["shared"] != 1 {
		t.Errorf("shared listed %d times, want 1", names["shared"])
	}
	// The colliding tool routes to the first owner ("one"): "one"
	// served tools/list + tools/call, "two" only tools/list.
	if got := fakes["one"].Calls(); got != 2 {
		t.Errorf("backend one calls = %d, want 2 (tools/list + tools/call)", got)
	}
	if got := fakes["two"].Calls(); got != 1 {
		t.Errorf("backend two calls = %d, want 1 (tools/list only)", got)
	}
}

func TestStdioToolsCallRoutesToOwner(t *testing.T) {
	fakes := map[string]*fakeBackend{
		"one": toolsListFake("echo"),
		"two": toolsListFake("add"),
	}
	g := gateway.NewTestGateway(t, stdioConfig("one", "two"),
		func(c config.Backend) (backend.Backend, error) { return fakes[c.Name], nil })

	input := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"add","arguments":{"a":1,"b":2}}}` + "\n"
	resps := runStdio(t, g, input)
	if len(resps) != 2 || resps[1].Error != nil {
		t.Fatalf("responses: %+v", resps)
	}
	// Both backends were listed once; only "two" got the extra tools/call.
	if got := fakes["one"].Calls(); got != 1 {
		t.Errorf("backend one calls = %d, want 1 (tools/list only)", got)
	}
	if got := fakes["two"].Calls(); got != 2 {
		t.Errorf("backend two calls = %d, want 2 (tools/list + tools/call)", got)
	}
}

func TestStdioToolsCallUnknownTool(t *testing.T) {
	g := gateway.NewTestGateway(t, stdioConfig("fake"),
		func(c config.Backend) (backend.Backend, error) { return toolsListFake("echo"), nil })

	resps := runStdio(t, g,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nope"}}`+"\n")
	if len(resps) != 1 || resps[0].Error == nil {
		t.Fatalf("want one error response, got %+v", resps)
	}
	if resps[0].Error.Code != -32602 {
		t.Errorf("error code = %d, want -32602", resps[0].Error.Code)
	}
}

func TestStdioNotificationProducesNoOutput(t *testing.T) {
	fake := toolsListFake("echo")
	g := gateway.NewTestGateway(t, stdioConfig("fake"),
		func(c config.Backend) (backend.Backend, error) { return fake, nil })

	resps := runStdio(t, g,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	if len(resps) != 0 {
		t.Fatalf("notification produced %d responses, want 0", len(resps))
	}
	if fake.NotificationCount() != 1 {
		t.Errorf("backend notifications = %d, want 1 forwarded", fake.NotificationCount())
	}
}

func TestStdioUnsupportedMethod(t *testing.T) {
	g := gateway.NewTestGateway(t, stdioConfig("fake"),
		func(c config.Backend) (backend.Backend, error) { return toolsListFake("echo"), nil })

	resps := runStdio(t, g, `{"jsonrpc":"2.0","id":9,"method":"resources/list"}`+"\n")
	if len(resps) != 1 || resps[0].Error == nil || resps[0].Error.Code != -32601 {
		t.Fatalf("want -32601 error, got %+v", resps)
	}
}

func TestStdioParseError(t *testing.T) {
	g := gateway.NewTestGateway(t, stdioConfig("fake"),
		func(c config.Backend) (backend.Backend, error) { return toolsListFake("echo"), nil })

	resps := runStdio(t, g, "this is not json\n")
	if len(resps) != 1 || resps[0].Error == nil || resps[0].Error.Code != -32700 {
		t.Fatalf("want -32700 parse error, got %+v", resps)
	}
}
