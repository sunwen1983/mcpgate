package health_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sunwen1983/mcpgate/internal/backend"
	"github.com/sunwen1983/mcpgate/internal/config"
	"github.com/sunwen1983/mcpgate/internal/health"
)

// scriptableBackend answers probes with a scripted error (nil = success)
// and counts calls, so tests can drive probe outcomes deterministically.
type scriptableBackend struct {
	mu     sync.Mutex
	err    error
	rpcErr *backend.Error
	calls  int
}

func (b *scriptableBackend) Call(_ context.Context, _ *backend.Request) (*backend.Response, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.err != nil {
		return nil, b.err
	}
	return &backend.Response{JSONRPC: "2.0", ID: "health-check", Error: b.rpcErr}, nil
}

func (b *scriptableBackend) Notify(_ context.Context, _ string, _ map[string]any) error { return nil }
func (b *scriptableBackend) Name() string                                               { return "fake" }
func (b *scriptableBackend) Close() error                                               { return nil }

func (b *scriptableBackend) setErr(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
}

func (b *scriptableBackend) setRPCErr(err *backend.Error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rpcErr = err
}

func (b *scriptableBackend) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func testConfig(interval time.Duration, threshold int) config.HealthCheckConfig {
	return config.HealthCheckConfig{
		Interval:         config.Duration(interval),
		Timeout:          config.Duration(time.Second),
		FailureThreshold: threshold,
	}
}

func TestProbeOnceHealthyOnSuccess(t *testing.T) {
	be := &scriptableBackend{}
	c := health.New(be, testConfig(time.Minute, 3), testLogger(), nil)
	if !c.Healthy() {
		t.Fatal("new checker should start healthy")
	}
	if !c.ProbeOnce(context.Background()) {
		t.Fatal("ProbeOnce on a working backend should stay healthy")
	}
	if be.callCount() != 1 {
		t.Errorf("calls = %d, want 1", be.callCount())
	}
	st := c.Status()
	if !st.Healthy || st.ConsecutiveFailures != 0 || st.LastError != "" || st.LastCheck.IsZero() {
		t.Errorf("status = %+v, want healthy with a recorded check", st)
	}
}

func TestProbeOnceUnhealthyAfterThreshold(t *testing.T) {
	be := &scriptableBackend{}
	c := health.New(be, testConfig(time.Minute, 3), testLogger(), nil)
	be.setErr(errors.New("connection refused"))

	// Below the threshold the backend is still considered healthy: a
	// single failed probe may be a blip, not an outage.
	if !c.ProbeOnce(context.Background()) {
		t.Fatal("1 failure should not mark the backend unhealthy (threshold 3)")
	}
	if !c.ProbeOnce(context.Background()) {
		t.Fatal("2 failures should not mark the backend unhealthy (threshold 3)")
	}
	if c.Status().ConsecutiveFailures != 2 {
		t.Fatalf("consecutive failures = %d, want 2", c.Status().ConsecutiveFailures)
	}
	// The third consecutive failure trips it.
	if c.ProbeOnce(context.Background()) {
		t.Fatal("3 consecutive failures should mark the backend unhealthy")
	}
	if c.Status().LastError == "" {
		t.Error("status should record the probe error")
	}
}

func TestProbeOnceRecoversOnSuccess(t *testing.T) {
	be := &scriptableBackend{}
	c := health.New(be, testConfig(time.Minute, 1), testLogger(), nil)
	be.setErr(errors.New("boom"))
	if c.ProbeOnce(context.Background()) {
		t.Fatal("failed probe should mark the backend unhealthy (threshold 1)")
	}
	be.setErr(nil)
	if !c.ProbeOnce(context.Background()) {
		t.Fatal("a successful probe should recover the backend")
	}
	if c.Status().ConsecutiveFailures != 0 {
		t.Errorf("consecutive failures = %d, want 0 after recovery", c.Status().ConsecutiveFailures)
	}
}

func TestProbeOnceRPCErrorsCountAsFailure(t *testing.T) {
	be := &scriptableBackend{}
	c := health.New(be, testConfig(time.Minute, 1), testLogger(), nil)
	// The server answered, but tools/list itself errored: the backend
	// cannot serve through the gateway, so the probe must fail.
	be.setRPCErr(&backend.Error{Code: -32601, Message: "method not found"})
	if c.ProbeOnce(context.Background()) {
		t.Fatal("a JSON-RPC error response should mark the backend unhealthy (threshold 1)")
	}
	if c.Status().LastError == "" {
		t.Error("status should record the RPC error")
	}
}

func TestProbeUsesToolsList(t *testing.T) {
	be := &probeCapturingBackend{resp: &backend.Response{JSONRPC: "2.0", ID: "health-check"}}
	c := health.New(be, testConfig(time.Minute, 1), testLogger(), nil)
	c.ProbeOnce(context.Background())
	if be.method != "tools/list" {
		t.Errorf("probe method = %q, want tools/list", be.method)
	}
	if be.id != "health-check" {
		t.Errorf("probe id = %v, want health-check", be.id)
	}
}

// probeCapturingBackend records the request the probe sends.
type probeCapturingBackend struct {
	resp   *backend.Response
	method string
	id     any
}

func (b *probeCapturingBackend) Call(_ context.Context, req *backend.Request) (*backend.Response, error) {
	b.method = req.Method
	b.id = req.ID
	return b.resp, nil
}

func (b *probeCapturingBackend) Notify(_ context.Context, _ string, _ map[string]any) error {
	return nil
}
func (b *probeCapturingBackend) Name() string { return "fake" }
func (b *probeCapturingBackend) Close() error { return nil }

func TestReportCalledAfterProbe(t *testing.T) {
	be := &scriptableBackend{}
	var reported []bool
	c := health.New(be, testConfig(time.Minute, 1), testLogger(), func(healthy bool) {
		reported = append(reported, healthy)
	})
	be.setErr(errors.New("boom"))
	c.ProbeOnce(context.Background())
	c.ProbeOnce(context.Background())
	be.setErr(nil)
	c.ProbeOnce(context.Background())
	want := []bool{false, false, true}
	if len(reported) != len(want) {
		t.Fatalf("reported = %v, want %v", reported, want)
	}
	for i, w := range want {
		if reported[i] != w {
			t.Errorf("reported[%d] = %v, want %v", i, reported[i], w)
		}
	}
}

func TestStartStopProbesOnInterval(t *testing.T) {
	be := &scriptableBackend{}
	c := health.New(be, testConfig(10*time.Millisecond, 3), testLogger(), nil)
	c.Start()
	defer c.Stop()

	// The loop probes immediately and then every 10ms: within a
	// second it must have probed several times.
	deadline := time.Now().Add(time.Second)
	for be.callCount() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if be.callCount() < 3 {
		t.Fatalf("calls = %d after 1s, want >= 3", be.callCount())
	}
	if !c.Healthy() {
		t.Fatal("backend should stay healthy while probes succeed")
	}

	c.Stop()
	afterStop := be.callCount()
	time.Sleep(50 * time.Millisecond)
	if be.callCount() != afterStop {
		t.Errorf("calls grew after Stop: %d -> %d", afterStop, be.callCount())
	}
}

func TestStartDisabledDoesNotProbe(t *testing.T) {
	be := &scriptableBackend{}
	// Zero interval: probing is disabled.
	c := health.New(be, testConfig(0, 3), testLogger(), nil)
	c.Start()
	defer c.Stop()
	time.Sleep(50 * time.Millisecond)
	if be.callCount() != 0 {
		t.Errorf("calls = %d, want 0 with health checks disabled", be.callCount())
	}
	if !c.Healthy() {
		t.Error("disabled health checks should stay healthy")
	}
}
