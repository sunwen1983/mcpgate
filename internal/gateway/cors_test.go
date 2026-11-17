package gateway_test

import (
	"net/http"
	"testing"

	"github.com/sunwen1983/mcpgate/internal/gateway"
)

func TestCORSPreflightAnsweredBeforeAuth(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodOptions, srv.URL+"/mcp/fake", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Origin", "https://agent-bazaar.pages.dev")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "X-API-Key, Content-Type")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("preflight status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, "*")
	}
	if got := resp.Header.Get("Access-Control-Allow-Methods"); got == "" {
		t.Errorf("Access-Control-Allow-Methods missing")
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); got == "" {
		t.Errorf("Access-Control-Allow-Headers missing")
	}
	if fake.Calls() != 0 {
		t.Errorf("preflight reached backend: %d calls", fake.Calls())
	}
}

func TestCORSHeadersOnMCPResponse(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	status, header, _ := post(t, srv, "/mcp/fake", "key-alice", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if status != http.StatusOK {
		t.Fatalf("POST /mcp/fake status = %d, want 200", status)
	}
	if got := header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, "*")
	}
}

func TestCORSHeadersOnHealthz(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, "*")
	}
}

func TestCORSNotOnAdmin(t *testing.T) {
	fake := &fakeBackend{}
	srv := gateway.NewTestServer(t, testConfig(), fake)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodOptions, srv.URL+"/admin/status", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Origin", "https://agent-bazaar.pages.dev")
	req.Header.Set("Access-Control-Request-Method", "GET")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS /admin/status: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q on admin path, want empty", got)
	}
	if resp.StatusCode == http.StatusOK {
		t.Errorf("OPTIONS /admin/status status = 200, want the middleware to leave it alone")
	}
}
