// Package health actively probes backends so the gateway can fail fast
// on a backend that is down, instead of sending client traffic into it
// and timing out.
//
// A probe is a plain tools/list call against the backend: the lightest
// request that exercises exactly the path real client traffic needs, so
// a backend that answers the probe can serve. A probe fails on a
// transport error, a timeout, or a JSON-RPC error response — a server
// that cannot answer tools/list cannot serve through this gateway
// anyway. Probe failures never touch the circuit breaker: the breaker
// measures real traffic, health measures the backend, and the two
// signals stay independent.
//
// A backend starts healthy (optimistic: no probe has run yet, and the
// breaker still guards real traffic) and turns unhealthy after
// FailureThreshold consecutive failed probes. One successful probe
// brings it back. Probing is opt-in — a zero interval disables it —
// and a Checker with probing disabled simply stays healthy.
package health

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sunwen1983/mcpgate/internal/backend"
	"github.com/sunwen1983/mcpgate/internal/config"
)

// Defaults for the probe settings a config leaves at zero.
const (
	// DefaultProbeTimeout bounds one probe when health_check.timeout is 0.
	DefaultProbeTimeout = 5 * time.Second
	// DefaultFailureThreshold is the consecutive-failure count that
	// marks a backend unhealthy when health_check.failure_threshold is 0.
	DefaultFailureThreshold = 3
)

// healthCheckID is the JSON-RPC id every probe carries, so a probe
// response is recognizable in backend logs.
const healthCheckID = "health-check"

// Status is a snapshot of a backend's health.
type Status struct {
	// Healthy is false once FailureThreshold consecutive probes failed,
	// until a probe succeeds again. True before the first probe.
	Healthy bool
	// LastCheck is when the most recent probe ran (zero value if none).
	LastCheck time.Time
	// ConsecutiveFailures counts failed probes since the last success.
	ConsecutiveFailures int
	// LastError is the most recent probe failure ("" after a success).
	LastError string
}

// Checker probes one backend on a schedule and tracks its health.
type Checker struct {
	backend backend.Backend
	cfg     config.HealthCheckConfig
	logger  *slog.Logger
	// report, when set, is called after every probe with the resulting
	// health. The gateway uses it to update the health metric.
	report func(healthy bool)

	mu                  sync.Mutex
	healthy             bool
	consecutiveFailures int
	lastCheck           time.Time
	lastErr             error

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
}

// New builds a Checker for a backend. Probing does not start until
// Start is called; until then the backend is reported healthy.
func New(be backend.Backend, cfg config.HealthCheckConfig, logger *slog.Logger, report func(bool)) *Checker {
	return &Checker{
		backend: be,
		cfg:     cfg,
		logger:  logger,
		report:  report,
		healthy: true,
	}
}

// Interval returns the configured probe interval (zero disables probing).
func (c *Checker) Interval() time.Duration { return time.Duration(c.cfg.Interval) }

// probeTimeout returns the configured probe timeout, defaulting to 5s.
func (c *Checker) probeTimeout() time.Duration {
	if c.cfg.Timeout > 0 {
		return time.Duration(c.cfg.Timeout)
	}
	return DefaultProbeTimeout
}

// failureThreshold returns the configured consecutive-failure count
// that marks the backend unhealthy, defaulting to 3.
func (c *Checker) failureThreshold() int {
	if c.cfg.FailureThreshold > 0 {
		return c.cfg.FailureThreshold
	}
	return DefaultFailureThreshold
}

// ProbeOnce runs a single probe synchronously and returns whether the
// backend is healthy afterwards. Tests drive state transitions through
// this; the background loop calls it on every tick.
func (c *Checker) ProbeOnce(ctx context.Context) bool {
	timeout := c.probeTimeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req := &backend.Request{JSONRPC: "2.0", ID: healthCheckID, Method: "tools/list"}
	resp, err := c.backend.Call(ctx, req)
	if err == nil && resp != nil && resp.Error != nil {
		// The server answered, but tools/list itself errored: the
		// backend cannot serve through this gateway.
		err = resp.Error
	}

	c.mu.Lock()
	c.lastCheck = time.Now()
	if err == nil {
		c.consecutiveFailures = 0
		c.lastErr = nil
		if !c.healthy {
			c.logger.Info("backend recovered", "backend", c.backend.Name())
			c.healthy = true
		}
	} else {
		c.consecutiveFailures++
		c.lastErr = err
		if c.consecutiveFailures >= c.failureThreshold() && c.healthy {
			c.logger.Info("backend unhealthy",
				"backend", c.backend.Name(),
				"consecutive_failures", c.consecutiveFailures,
				"error", err)
			c.healthy = false
		}
	}
	healthy := c.healthy
	c.mu.Unlock()

	if c.report != nil {
		c.report(healthy)
	}
	return healthy
}

// Healthy reports whether the backend is currently considered healthy:
// true before the first probe, and false once FailureThreshold
// consecutive probes failed, until a probe succeeds again.
func (c *Checker) Healthy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.healthy
}

// Status returns a snapshot of the checker's state.
func (c *Checker) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	lastErr := ""
	if c.lastErr != nil {
		lastErr = c.lastErr.Error()
	}
	return Status{
		Healthy:             c.healthy,
		LastCheck:           c.lastCheck,
		ConsecutiveFailures: c.consecutiveFailures,
		LastError:           lastErr,
	}
}

// Start begins background probing: one probe immediately, then one per
// interval, until Stop is called. A zero interval disables probing:
// Start returns immediately and the backend stays healthy. Start is
// safe to call more than once; only the first call starts the loop.
func (c *Checker) Start() {
	if c.Interval() <= 0 {
		return
	}
	c.startOnce.Do(func() {
		c.stop = make(chan struct{})
		c.done = make(chan struct{})
		go c.loop()
	})
}

// loop probes immediately and then on every tick until stopped.
func (c *Checker) loop() {
	defer close(c.done)
	c.ProbeOnce(context.Background())
	ticker := time.NewTicker(c.Interval())
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.ProbeOnce(context.Background())
		}
	}
}

// Stop halts background probing and waits for an in-flight probe to
// finish. A no-op when probing never started, and safe to call more
// than once.
func (c *Checker) Stop() {
	if c.stop == nil {
		return
	}
	c.stopOnce.Do(func() {
		close(c.stop)
		<-c.done
	})
}
