package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sunwen1983/mcpgate/internal/config"
)

// poolTestServer is a fake MCP endpoint over Streamable HTTP. It
// answers every request with {"server": id} and counts calls; when
// failing is set it answers 500 instead, which the httpBackend treats
// as a transport failure.
type poolTestServer struct {
	srv     *httptest.Server
	id      string
	calls   atomic.Int32
	failing atomic.Bool
}

func newPoolTestServer(t *testing.T, id string) *poolTestServer {
	t.Helper()
	s := &poolTestServer{id: id}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		var req Request
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if s.failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Response{
			JSONRPC: "2.0", ID: req.ID,
			Result: map[string]any{"server": id},
		})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *poolTestServer) URL() string    { return s.srv.URL }
func (s *poolTestServer) Calls() int     { return int(s.calls.Load()) }
func (s *poolTestServer) setFail(f bool) { s.failing.Store(f) }

func poolConfig(urls ...string) config.Backend {
	return config.Backend{
		Name:      "pooled",
		Transport: "http",
		Endpoints: urls,
		Timeout:   config.Duration(5 * time.Second),
	}
}

// mustPool builds a pool for the given endpoint URLs, failing the test
// on error.
func mustPool(t *testing.T, threshold int, cooldown time.Duration, urls ...string) *poolBackend {
	t.Helper()
	p, err := newPoolBackend(poolConfig(urls...), urls, threshold, cooldown)
	if err != nil {
		t.Fatalf("newPoolBackend: %v", err)
	}
	return p
}

// callServer issues one tools/list call and returns which endpoint
// answered it.
func callServer(t *testing.T, b Backend) (string, error) {
	t.Helper()
	resp, err := b.Call(context.Background(), &Request{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	if err != nil {
		return "", err
	}
	server, _ := resp.Result["server"].(string)
	return server, nil
}

func TestPoolRoundRobin(t *testing.T) {
	a := newPoolTestServer(t, "a")
	b := newPoolTestServer(t, "b")
	p := mustPool(t, 3, time.Minute, a.URL(), b.URL())
	defer p.Close()

	var got []string
	for i := 0; i < 6; i++ {
		server, err := callServer(t, p)
		if err != nil {
			t.Fatalf("Call %d: %v", i, err)
		}
		got = append(got, server)
	}
	want := []string{"a", "b", "a", "b", "a", "b"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d served by %q, want %q (sequence %v)", i, got[i], want[i], got)
		}
	}
	if a.Calls() != 3 || b.Calls() != 3 {
		t.Errorf("calls = a:%d b:%d, want a:3 b:3", a.Calls(), b.Calls())
	}
}

func TestPoolSkipsFailingEndpoint(t *testing.T) {
	a := newPoolTestServer(t, "a")
	b := newPoolTestServer(t, "b")
	b.setFail(true)
	// Cooldown is long so b stays down for the whole test once marked.
	p := mustPool(t, 3, time.Hour, a.URL(), b.URL())
	defer p.Close()

	// Round-robin alternates a, b, a, b, ... — b fails on its turns
	// and is marked down after its third failure (call 6).
	for i := 0; i < 6; i++ {
		_, _ = callServer(t, p) // errors from b's turns are expected
	}
	if b.Calls() != 3 {
		t.Fatalf("b calls = %d, want 3 (its three failures)", b.Calls())
	}
	// Ten more calls must all go to a: b is out of rotation.
	for i := 0; i < 10; i++ {
		server, err := callServer(t, p)
		if err != nil {
			t.Fatalf("Call after b down: %v", err)
		}
		if server != "a" {
			t.Errorf("call served by %q, want a (b is down)", server)
		}
	}
	if b.Calls() != 3 {
		t.Errorf("b calls = %d after being down, want 3", b.Calls())
	}
}

func TestPoolRecoversAfterCooldown(t *testing.T) {
	a := newPoolTestServer(t, "a")
	b := newPoolTestServer(t, "b")
	b.setFail(true)
	p := mustPool(t, 2, 50*time.Millisecond, a.URL(), b.URL())
	defer p.Close()

	// b fails twice (calls 2 and 4) and goes down.
	for i := 0; i < 4; i++ {
		_, _ = callServer(t, p)
	}
	if b.Calls() != 2 {
		t.Fatalf("b calls = %d, want 2", b.Calls())
	}
	// While b is down, traffic goes to a only.
	for i := 0; i < 2; i++ {
		if server, err := callServer(t, p); err != nil || server != "a" {
			t.Fatalf("call while b down: server=%q err=%v", server, err)
		}
	}
	// Heal b, wait out the cooldown, and it must rejoin rotation.
	b.setFail(false)
	time.Sleep(100 * time.Millisecond)
	seenB := false
	for i := 0; i < 4; i++ {
		server, err := callServer(t, p)
		if err != nil {
			t.Fatalf("Call after recovery: %v", err)
		}
		if server == "b" {
			seenB = true
		}
	}
	if !seenB {
		t.Error("b never served a call after its cooldown elapsed")
	}
	if p.endpoints[1].isDown() {
		t.Error("b still marked down after serving successfully")
	}
}

func TestPoolAllDownFallsBack(t *testing.T) {
	a := newPoolTestServer(t, "a")
	b := newPoolTestServer(t, "b")
	a.setFail(true)
	b.setFail(true)
	p := mustPool(t, 2, time.Hour, a.URL(), b.URL())
	defer p.Close()

	// Both endpoints fail: the call errors, but the pool does not
	// refuse with a "no endpoints" error — it still attempts one.
	for i := 0; i < 4; i++ {
		if _, err := callServer(t, p); err == nil {
			t.Fatalf("Call %d: want error, got success", i)
		}
	}
	// Heal a: the next calls must succeed again, through the
	// all-down fallback round-robin.
	a.setFail(false)
	succeeded := false
	for i := 0; i < 4; i++ {
		if server, err := callServer(t, p); err == nil && server == "a" {
			succeeded = true
		}
	}
	if !succeeded {
		t.Error("no call succeeded after a recovered (all-down fallback broken)")
	}
}

// TestPoolRPCErrorDoesNotMarkDown covers a server that answers every
// request with a JSON-RPC error: it is serving, so its failures must
// not count toward being taken out of rotation.
func TestPoolRPCErrorDoesNotMarkDown(t *testing.T) {
	a := newPoolTestServer(t, "a")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Request
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Response{
			JSONRPC: "2.0", ID: req.ID,
			Error: &Error{Code: -32601, Message: "method not found"},
		})
	}))
	t.Cleanup(srv.Close)
	// The erroring server would be marked down after 2 transport
	// failures if RPC errors counted; with threshold 2 and many calls
	// it must keep receiving traffic.
	p := mustPool(t, 2, time.Hour, a.URL(), srv.URL)
	defer p.Close()

	rpcErrors := 0
	for i := 0; i < 10; i++ {
		resp, err := p.Call(context.Background(), &Request{JSONRPC: "2.0", ID: i, Method: "tools/call"})
		if err != nil {
			t.Fatalf("Call %d: transport error %v", i, err)
		}
		if resp.Error != nil {
			rpcErrors++
		}
	}
	if rpcErrors == 0 {
		t.Error("no JSON-RPC error responses seen; the erroring server got no traffic")
	}
	if p.endpoints[1].isDown() {
		t.Error("erroring server marked down: JSON-RPC errors must not count as transport failures")
	}
}

func TestPoolNotifyRoundRobin(t *testing.T) {
	a := newPoolTestServer(t, "a")
	b := newPoolTestServer(t, "b")
	p := mustPool(t, 3, time.Minute, a.URL(), b.URL())
	defer p.Close()

	for i := 0; i < 4; i++ {
		if err := p.Notify(context.Background(), "notifications/cancelled", nil); err != nil {
			t.Fatalf("Notify %d: %v", i, err)
		}
	}
	if a.Calls() != 2 || b.Calls() != 2 {
		t.Errorf("notify calls = a:%d b:%d, want a:2 b:2", a.Calls(), b.Calls())
	}
}

func TestPoolStream(t *testing.T) {
	a := newPoolTestServer(t, "a")
	b := newPoolTestServer(t, "b")
	p := mustPool(t, 3, time.Minute, a.URL(), b.URL())
	defer p.Close()

	// Streams alternate endpoints like calls do.
	for i, want := range []string{"a", "b"} {
		stream, err := p.Stream(context.Background(), &Request{JSONRPC: "2.0", ID: i, Method: "tools/list"})
		if err != nil {
			t.Fatalf("Stream %d: %v", i, err)
		}
		data, err := stream.Next()
		_ = stream.Close()
		if err != nil {
			t.Fatalf("Stream %d Next: %v", i, err)
		}
		var resp Response
		if err := json.Unmarshal(data, &resp); err != nil {
			t.Fatalf("Stream %d: parse event: %v", i, err)
		}
		if resp.Result["server"] != want {
			t.Errorf("Stream %d served by %v, want %s", i, resp.Result["server"], want)
		}
	}

	// A stream that fails to establish counts toward the endpoint
	// going down: fail b twice with threshold 2, then it is skipped.
	b.setFail(true)
	p2 := mustPool(t, 2, time.Hour, a.URL(), b.URL())
	defer p2.Close()
	for i := 0; i < 4; i++ {
		_, _ = p2.Stream(context.Background(), &Request{JSONRPC: "2.0", ID: i, Method: "tools/list"})
	}
	if b.Calls() != 3 { // 1 ok stream above + 2 failing ones
		t.Fatalf("b calls = %d, want 3 (1 ok + 2 failing)", b.Calls())
	}
	if !p2.endpoints[1].isDown() {
		t.Error("b not marked down after 2 failed Stream calls")
	}
}

func TestBackendNewWithEndpoints(t *testing.T) {
	a := newPoolTestServer(t, "a")
	b := newPoolTestServer(t, "b")
	be, err := New(poolConfig(a.URL(), b.URL()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer be.Close()
	if be.Name() != "pooled" {
		t.Errorf("Name = %q, want pooled", be.Name())
	}
	if _, ok := be.(Streamer); !ok {
		t.Error("pooled backend does not implement Streamer")
	}
	if _, err := callServer(t, be); err != nil {
		t.Fatalf("Call: %v", err)
	}
}

func TestNewPoolBackendNoEndpoints(t *testing.T) {
	if _, err := newPoolBackend(poolConfig(), nil, 3, time.Minute); err == nil {
		t.Error("newPoolBackend with no endpoints: want error, got nil")
	}
}
