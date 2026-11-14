package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sunwen1983/mcpgate/internal/config"
	"github.com/sunwen1983/mcpgate/internal/gateway"
)

// reloadTestConfig is a complete config file for reload tests: one fake
// http backend (the test factory ignores the URL) and two API keys.
const reloadTestConfig = `listen: ":0"
api_keys:
  - key: key-alice
    name: alice
  - key: key-bob
    name: bob
default_rate_limit:
  requests_per_second: 100
  burst: 100
backends:
  - name: fake
    transport: http
    url: http://localhost:1/unused
    timeout: 5s
    cache:
      tools_list_ttl: 1m
      tools_call_ttl: 1m
      cacheable_tools: [echo]
    breaker:
      failure_threshold: 2
      cooldown: 1m
`

// newReloadTestServer writes content as the gateway's config file,
// loads it, and serves a gateway that reloads from that file. It
// returns the server and a rewrite function for the next config.
func newReloadTestServer(t *testing.T, content string) (*httptest.Server, func(string)) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	fake := &fakeBackend{}
	srv := gateway.NewTestServerWithConfigPath(t, cfg, fake, path)
	rewrite := func(next string) {
		if err := os.WriteFile(path, []byte(next), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return srv, rewrite
}

// get issues a GET with an optional API key.
func get(t *testing.T, srv *httptest.Server, path, apiKey string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// reload posts to /admin/reload and decodes the JSON body. Non-JSON
// error bodies (401/405) come back under "raw" instead of failing
// the decode.
func reload(t *testing.T, srv *httptest.Server, apiKey string) (int, map[string]any) {
	t.Helper()
	status, _, data := post(t, srv, "/admin/reload", apiKey, `{}`)
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		body = map[string]any{"raw": string(data)}
	}
	return status, body
}

// strList pulls a []string out of a decoded JSON body field.
func strList(t *testing.T, body map[string]any, field string) []string {
	t.Helper()
	raw, ok := body[field].([]any)
	if !ok {
		t.Fatalf("body[%q] = %v, want a list", field, body[field])
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("body[%q] entry %v is not a string", field, v)
		}
		out = append(out, s)
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestAdminStatus(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	// Unauthenticated: 401, like every other admin endpoint.
	status, _ := get(t, srv, "/admin/status", "")
	if status != http.StatusUnauthorized {
		t.Errorf("GET without key: status = %d, want 401", status)
	}
	// Wrong method: 405.
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/admin/status", nil)
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
		t.Errorf("POST: status = %d, want 405", resp.StatusCode)
	}

	status, data := get(t, srv, "/admin/status", "key-alice")
	if status != http.StatusOK {
		t.Fatalf("GET with key: status = %d, want 200", status)
	}
	var body struct {
		Status        string   `json:"status"`
		UptimeSeconds int64    `json:"uptime_seconds"`
		LoadedAt      string   `json:"loaded_at"`
		APIKeys       []string `json:"api_keys"`
		Backends      []struct {
			Name      string `json:"name"`
			Transport string `json:"transport"`
			Breaker   string `json:"breaker"`
			Health    string `json:"health"`
		} `json:"backends"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("parse status: %v (%s)", err, data)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if body.UptimeSeconds < 0 {
		t.Errorf("uptime_seconds = %d, want >= 0", body.UptimeSeconds)
	}
	if _, err := time.Parse(time.RFC3339, body.LoadedAt); err != nil {
		t.Errorf("loaded_at = %q, want RFC3339: %v", body.LoadedAt, err)
	}
	if len(body.APIKeys) != 2 || body.APIKeys[0] != "alice" || body.APIKeys[1] != "bob" {
		t.Errorf("api_keys = %v, want [alice bob]", body.APIKeys)
	}
	if len(body.Backends) != 1 || body.Backends[0].Name != "fake" ||
		body.Backends[0].Transport != "http" || body.Backends[0].Breaker != "closed" ||
		body.Backends[0].Health != "disabled" {
		t.Errorf("backends = %+v, want one fake/http/closed/disabled", body.Backends)
	}
}

func TestReloadAuthRequired(t *testing.T) {
	srv, _ := newReloadTestServer(t, reloadTestConfig)
	defer srv.Close()

	status, _ := reload(t, srv, "")
	if status != http.StatusUnauthorized {
		t.Errorf("no key: status = %d, want 401", status)
	}
	status, _ = reload(t, srv, "wrong-key")
	if status != http.StatusUnauthorized {
		t.Errorf("wrong key: status = %d, want 401", status)
	}
}

func TestReloadAPIKeys(t *testing.T) {
	srv, rewrite := newReloadTestServer(t, reloadTestConfig)
	defer srv.Close()
	mcpBody := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	// Add a key: the new key works, the old ones keep working.
	rewrite(strings.Replace(reloadTestConfig,
		"  - key: key-bob\n    name: bob\n",
		"  - key: key-bob\n    name: bob\n  - key: key-carol\n    name: carol\n", 1))
	status, body := reload(t, srv, "key-alice")
	if status != http.StatusOK {
		t.Fatalf("reload: status = %d, want 200 (%v)", status, body)
	}
	if !contains(strList(t, body, "reloaded"), "api_keys") {
		t.Errorf("reloaded = %v, want api_keys", body["reloaded"])
	}
	for _, key := range []string{"key-alice", "key-bob", "key-carol"} {
		status, _, _ := post(t, srv, "/mcp/fake", key, mcpBody)
		if status != http.StatusOK {
			t.Errorf("key %s after reload: status = %d, want 200", key, status)
		}
	}

	// Remove a key: it stops working immediately, the others are unaffected.
	rewrite(strings.Replace(reloadTestConfig,
		"  - key: key-alice\n    name: alice\n", "", 1))
	status, body = reload(t, srv, "key-bob")
	if status != http.StatusOK {
		t.Fatalf("reload: status = %d, want 200 (%v)", status, body)
	}
	status, _, _ = post(t, srv, "/mcp/fake", "key-alice", mcpBody)
	if status != http.StatusUnauthorized {
		t.Errorf("removed key: status = %d, want 401", status)
	}
	status, _, _ = post(t, srv, "/mcp/fake", "key-bob", mcpBody)
	if status != http.StatusOK {
		t.Errorf("kept key: status = %d, want 200", status)
	}
}

func TestReloadRejectsInvalidConfig(t *testing.T) {
	srv, rewrite := newReloadTestServer(t, reloadTestConfig)
	defer srv.Close()
	mcpBody := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	rewrite("listen: :0\napi_keys: []\nbackends: []\n") // fails validation
	status, body := reload(t, srv, "key-alice")
	if status != http.StatusBadRequest {
		t.Fatalf("reload invalid: status = %d, want 400 (%v)", status, body)
	}
	if errMsg, _ := body["error"].(string); !strings.Contains(errMsg, "invalid config") {
		t.Errorf("error = %q, want invalid-config message", errMsg)
	}
	// The running config is untouched.
	status, _, _ = post(t, srv, "/mcp/fake", "key-alice", mcpBody)
	if status != http.StatusOK {
		t.Errorf("after rejected reload: status = %d, want 200", status)
	}

	// A syntactically broken file is rejected the same way.
	rewrite("this is: [not valid yaml\n")
	status, _ = reload(t, srv, "key-alice")
	if status != http.StatusBadRequest {
		t.Errorf("reload broken yaml: status = %d, want 400", status)
	}
}

func TestReloadToolFilter(t *testing.T) {
	srv, rewrite := newReloadTestServer(t, reloadTestConfig)
	defer srv.Close()
	callBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{}}}`

	status, _, _ := post(t, srv, "/mcp/fake", "key-alice", callBody)
	if status != http.StatusOK {
		t.Fatalf("before reload: status = %d, want 200", status)
	}
	rewrite(strings.Replace(reloadTestConfig,
		"    timeout: 5s\n",
		"    timeout: 5s\n    tool_filter:\n      deny: [echo]\n", 1))
	status, body := reload(t, srv, "key-alice")
	if status != http.StatusOK {
		t.Fatalf("reload: status = %d, want 200 (%v)", status, body)
	}
	if !contains(strList(t, body, "reloaded"), "backends.fake.tool_filter") {
		t.Errorf("reloaded = %v, want backends.fake.tool_filter", body["reloaded"])
	}
	status, _, _ = post(t, srv, "/mcp/fake", "key-alice", callBody)
	if status != http.StatusForbidden {
		t.Errorf("after reload: status = %d, want 403", status)
	}
}

func TestReloadRequiresRestart(t *testing.T) {
	srv, rewrite := newReloadTestServer(t, reloadTestConfig)
	defer srv.Close()
	mcpBody := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	// A backend URL change cannot take effect live.
	rewrite(strings.Replace(reloadTestConfig,
		"url: http://localhost:1/unused", "url: http://localhost:2/unused", 1))
	status, body := reload(t, srv, "key-alice")
	if status != http.StatusOK {
		t.Fatalf("reload: status = %d, want 200 (%v)", status, body)
	}
	restart := strList(t, body, "requires_restart")
	if !contains(restart, "backend fake: url changed") {
		t.Errorf("requires_restart = %v, want backend fake: url changed", restart)
	}
	if len(strList(t, body, "reloaded")) != 0 {
		t.Errorf("reloaded = %v, want empty", body["reloaded"])
	}
	// The gateway keeps serving with the old backend settings.
	status, _, _ = post(t, srv, "/mcp/fake", "key-alice", mcpBody)
	if status != http.StatusOK {
		t.Errorf("after reload: status = %d, want 200", status)
	}

	// Adding a backend needs a restart too.
	rewrite(strings.Replace(reloadTestConfig,
		"backends:\n",
		"backends:\n  - name: second\n    transport: http\n    url: http://localhost:3/unused\n    timeout: 5s\n", 1))
	status, body = reload(t, srv, "key-alice")
	if status != http.StatusOK {
		t.Fatalf("reload: status = %d, want 200 (%v)", status, body)
	}
	if !contains(strList(t, body, "requires_restart"), "backend second added") {
		t.Errorf("requires_restart = %v, want backend second added", body["requires_restart"])
	}
}

func TestReloadNoChange(t *testing.T) {
	srv, _ := newReloadTestServer(t, reloadTestConfig)
	defer srv.Close()

	status, body := reload(t, srv, "key-alice")
	if status != http.StatusOK {
		t.Fatalf("reload: status = %d, want 200 (%v)", status, body)
	}
	if len(strList(t, body, "reloaded")) != 0 || len(strList(t, body, "requires_restart")) != 0 {
		t.Errorf("reloaded = %v, requires_restart = %v, want both empty",
			body["reloaded"], body["requires_restart"])
	}
}

func TestReloadHealthCheckToggle(t *testing.T) {
	srv, rewrite := newReloadTestServer(t, reloadTestConfig)
	defer srv.Close()

	// Enable probing: the prober starts (the fake answers tools/list,
	// so the backend reports healthy).
	rewrite(strings.Replace(reloadTestConfig,
		"    breaker:\n",
		"    health_check:\n      interval: 50ms\n    breaker:\n", 1))
	status, body := reload(t, srv, "key-alice")
	if status != http.StatusOK {
		t.Fatalf("reload: status = %d, want 200 (%v)", status, body)
	}
	if !contains(strList(t, body, "reloaded"), "backends.fake.health_check") {
		t.Errorf("reloaded = %v, want backends.fake.health_check", body["reloaded"])
	}
	_, data := get(t, srv, "/admin/status", "key-alice")
	var st struct {
		Backends []struct {
			Health string `json:"health"`
		} `json:"backends"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Backends) != 1 || st.Backends[0].Health != "healthy" {
		t.Errorf("after enabling health checks: health = %+v, want healthy", st.Backends)
	}

	// Disable probing again: the checker stops and status reports disabled.
	rewrite(reloadTestConfig)
	status, _ = reload(t, srv, "key-alice")
	if status != http.StatusOK {
		t.Fatalf("reload: status = %d, want 200", status)
	}
	_, data = get(t, srv, "/admin/status", "key-alice")
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Backends) != 1 || st.Backends[0].Health != "disabled" {
		t.Errorf("after disabling health checks: health = %+v, want disabled", st.Backends)
	}
}
