package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Options configures one load-test run.
type Options struct {
	GoBinary         string        // go toolchain for building the gateway; default "go"
	RepoDir          string        // repo root containing ./cmd/mcpgate; default "."
	BinaryPath       string        // prebuilt mcpgate binary; empty means build it
	Concurrency      int           // parallel workers; default 10
	Requests         int           // total requests to fire; default 1000
	Method           string        // "tools/call" (default) or "tools/list"
	BackendDelay     time.Duration // artificial mock-backend latency; default 0
	ReadinessTimeout time.Duration // how long to wait for /healthz; default 10s
}

func (o Options) withDefaults() Options {
	if o.GoBinary == "" {
		o.GoBinary = "go"
	}
	if o.RepoDir == "" {
		o.RepoDir = "."
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 10
	}
	if o.Requests <= 0 {
		o.Requests = 1000
	}
	if o.Method == "" {
		o.Method = "tools/call"
	}
	if o.ReadinessTimeout <= 0 {
		o.ReadinessTimeout = 10 * time.Second
	}
	return o
}

// loadtestConfig is the gateway config the harness generates: one API
// key, a rate limit far above the load (the harness measures the
// gateway, not the token bucket), no caching, and a single HTTP
// backend pointing at the mock.
func loadtestConfig(gatewayAddr, mockURL string) string {
	return fmt.Sprintf(`listen: %q
api_keys:
  - key: "loadtest-key"
    name: "loadtest"
default_rate_limit:
  requests_per_second: 1000000
  burst: 1000000
backends:
  - name: "mock"
    transport: "http"
    url: %q
    timeout: 10s
`, gatewayAddr, mockURL+"/mcp")
}

// freePort returns an unused loopback port. The port is released
// before the gateway binds it, so a stray process could in theory
// grab it first — acceptable for a local harness; a bind failure
// surfaces as a startup error with the gateway's stderr attached.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// run builds (unless BinaryPath is set) and starts the real mcpgate
// binary against a mock HTTP backend, fires opts.Requests JSON-RPC
// requests at it with opts.Concurrency workers, and returns the
// headline report. The gateway is always stopped before run returns.
func run(ctx context.Context, opts Options) (Report, error) {
	opts = opts.withDefaults()
	if opts.Method != "tools/call" && opts.Method != "tools/list" {
		return Report{}, fmt.Errorf("unsupported method %q (want tools/call or tools/list)", opts.Method)
	}

	workdir, err := os.MkdirTemp("", "mcpgate-loadtest-*")
	if err != nil {
		return Report{}, err
	}
	defer os.RemoveAll(workdir)

	binary := opts.BinaryPath
	if binary == "" {
		binary = filepath.Join(workdir, "mcpgate")
		build := exec.CommandContext(ctx, opts.GoBinary, "build", "-o", binary, "./cmd/mcpgate")
		build.Dir = opts.RepoDir
		var buildErr bytes.Buffer
		build.Stderr = &buildErr
		if err := build.Run(); err != nil {
			return Report{}, fmt.Errorf("build mcpgate: %w: %s", err, buildErr.String())
		}
	}

	mock := newMockBackend(opts.BackendDelay)
	mockSrv := httptest.NewServer(mock)
	defer mockSrv.Close()

	port, err := freePort()
	if err != nil {
		return Report{}, fmt.Errorf("pick gateway port: %w", err)
	}
	gatewayAddr := fmt.Sprintf("127.0.0.1:%d", port)
	cfgPath := filepath.Join(workdir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(loadtestConfig(gatewayAddr, mockSrv.URL)), 0o600); err != nil {
		return Report{}, err
	}

	// Start the real server binary.
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, "-config", cfgPath)
	cmd.Stderr = &stderr
	// Stdout carries the gateway's JSON logs; discard them — the
	// harness reports numbers, not logs.
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		return Report{}, fmt.Errorf("start mcpgate: %w", err)
	}
	// Always stop the gateway before returning. SIGTERM triggers the
	// graceful shutdown in main; SIGKILL is the backstop.
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()

	gatewayURL := "http://" + gatewayAddr
	if err := waitReady(ctx, gatewayURL+"/healthz", opts.ReadinessTimeout); err != nil {
		return Report{}, fmt.Errorf("gateway never became ready: %w\n[DEBUG-loadtest] gateway stderr:\n%s", err, stderr.String())
	}

	return fireLoad(ctx, gatewayURL, opts, mock)
}

// waitReady polls url until it answers 200 or the timeout expires.
func waitReady(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("GET %s: %w", url, err)
			}
			return fmt.Errorf("GET %s: last status %d", url, resp.StatusCode)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// fireLoad runs the worker pool against a ready gateway and returns
// the report. Each worker takes the next request number from a shared
// counter until opts.Requests is reached; latencies are collected
// per-worker and merged at the end, so the harness adds no lock
// contention to the numbers it measures.
func fireLoad(ctx context.Context, gatewayURL string, opts Options, mock *mockBackend) (Report, error) {
	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConnsPerHost: opts.Concurrency,
		},
	}
	var issued atomic.Int64
	var errors atomic.Int64
	latCh := make(chan []time.Duration, opts.Concurrency)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < opts.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var lats []time.Duration
			for {
				n := issued.Add(1)
				if int(n) > opts.Requests {
					break
				}
				lats = append(lats, oneRequest(ctx, client, gatewayURL, opts.Method, int(n), &errors))
			}
			latCh <- lats
		}()
	}
	wg.Wait()
	close(latCh)
	elapsed := time.Since(start)
	var all []time.Duration
	for lats := range latCh {
		all = append(all, lats...)
	}
	rep := summarize(all, int(errors.Load()), elapsed)
	rep.BackendCalls = mock.Calls()
	return rep, nil
}

// oneRequest fires a single JSON-RPC request and returns its latency.
// A transport error, a non-200 status, or a JSON-RPC error object in
// the response counts as an error; the latency is recorded either way.
func oneRequest(ctx context.Context, client *http.Client, gatewayURL, method string, id int, errors *atomic.Int64) time.Duration {
	var body string
	switch method {
	case "tools/list":
		body = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/list"}`, id)
	default: // tools/call for "other": not cacheable, so every call reaches the mock backend
		body = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"other","arguments":{}}}`, id)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/mcp/mock", strings.NewReader(body))
	if err != nil {
		errors.Add(1)
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "loadtest-key")
	start := time.Now()
	resp, err := client.Do(req)
	lat := time.Since(start)
	if err != nil {
		errors.Add(1)
		return lat
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		errors.Add(1)
		return lat
	}
	var out struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Error != nil {
		errors.Add(1)
	}
	return lat
}
