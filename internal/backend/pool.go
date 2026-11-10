// Load balancing: one backend name, several endpoints.
//
// A pooled backend is an http backend configured with `endpoints`
// instead of a single `url`. The pool keeps one httpBackend per URL
// and spreads requests across them round-robin, skipping endpoints
// that are currently down. The gateway needs no changes: the pool
// satisfies the same Backend (and Streamer) interfaces as a single
// endpoint, so the breaker, cache, health checker, and SSE relay all
// see one logical backend.
//
// Failure handling is deliberately conservative about retries. A
// failed call is NOT retried on another endpoint: tools/call may have
// side effects, so retrying could execute the same call twice. Instead
// the failed endpoint is taken out of rotation after
// failureThreshold consecutive failures, so the *next* request goes
// elsewhere, and the failed request itself returns the error (the
// per-backend breaker records it as usual). A JSON-RPC error inside
// an otherwise well-formed response is not a failure — the endpoint
// served — so it does not count toward the threshold.
//
// Recovery: a down endpoint becomes eligible again after cooldown.
// While every endpoint is down the pool degrades to plain
// round-robin over all of them instead of failing every request
// outright, so a recovered endpoint gets traffic again and a success
// brings it back into rotation.
package backend

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sunwen1983/mcpgate/internal/config"
	"github.com/sunwen1983/mcpgate/internal/metrics"
)

// Defaults governing endpoint failure tracking. The constructor takes
// them as arguments so tests can use short values; New passes these.
const (
	// poolFailureThreshold is the consecutive failures after which an
	// endpoint is taken out of rotation.
	poolFailureThreshold = 3
	// poolCooldown is how long a failed endpoint stays out of rotation
	// before it is tried again.
	poolCooldown = 30 * time.Second
)

// poolEndpoint is one URL inside a pool, with its failure state.
type poolEndpoint struct {
	backend          Backend
	url              string
	failureThreshold int
	cooldown         time.Duration

	mu sync.Mutex
	// consecutiveFailures counts transport failures since the last
	// success. A JSON-RPC error response is a success here: the
	// endpoint answered.
	consecutiveFailures int
	// down is true once the endpoint was taken out of rotation; it
	// clears on the next success.
	down bool
	// downUntil is when a down endpoint becomes eligible again.
	downUntil time.Time
}

// eligible reports whether the endpoint may receive traffic now: it
// is up, or it was down but its cooldown has elapsed.
func (e *poolEndpoint) eligible(now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.down || !now.Before(e.downUntil)
}

// isDown reports whether the endpoint is currently out of rotation
// (regardless of whether its cooldown has elapsed).
func (e *poolEndpoint) isDown() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.down
}

// recordSuccess resets the failure count and clears the down state.
// It reports whether the endpoint was down: the pool uses that to
// refresh the healthy-endpoint metric.
func (e *poolEndpoint) recordSuccess() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.consecutiveFailures = 0
	wasDown := e.down
	e.down = false
	e.downUntil = time.Time{}
	return wasDown
}

// recordFailure counts one transport failure. Once the threshold is
// reached the endpoint is taken out of rotation until cooldown has
// passed. It reports whether the endpoint just went down.
func (e *poolEndpoint) recordFailure(now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.consecutiveFailures++
	if e.consecutiveFailures < e.failureThreshold {
		return false
	}
	if e.down {
		// Already down: a failure on a retried endpoint just
		// extends the cooldown.
		e.downUntil = now.Add(e.cooldown)
		return false
	}
	e.down = true
	e.downUntil = now.Add(e.cooldown)
	return true
}

// poolBackend spreads requests across several http endpoints of one
// backend name.
type poolBackend struct {
	name      string
	endpoints []*poolEndpoint
	next      uint64 // round-robin cursor
}

// newPoolBackend builds a pool of one httpBackend per URL. The
// per-endpoint backends share the rest of cfg (auth, headers, timeout)
// and differ only in URL. failureThreshold and cooldown override the
// defaults when positive; New passes the defaults.
func newPoolBackend(cfg config.Backend, urls []string, failureThreshold int, cooldown time.Duration) (*poolBackend, error) {
	if len(urls) == 0 {
		return nil, fmt.Errorf("pool backend %s: at least one endpoint is required", cfg.Name)
	}
	if failureThreshold <= 0 {
		failureThreshold = poolFailureThreshold
	}
	if cooldown <= 0 {
		cooldown = poolCooldown
	}
	p := &poolBackend{
		name: cfg.Name,
	}
	for _, u := range urls {
		epCfg := cfg
		epCfg.URL = u
		epCfg.Endpoints = nil
		p.endpoints = append(p.endpoints, &poolEndpoint{
			backend:          newHTTPBackend(epCfg),
			url:              u,
			failureThreshold: failureThreshold,
			cooldown:         cooldown,
		})
	}
	metrics.EndpointsHealthy.WithLabelValues(cfg.Name).Set(float64(len(urls)))
	return p, nil
}

func (p *poolBackend) Name() string { return p.name }

// pickEndpoint returns the next endpoint in round-robin order,
// skipping endpoints that are down. When every endpoint is down it
// falls back to plain round-robin over all of them, so a recovered
// endpoint gets traffic again instead of every request failing.
func (p *poolBackend) pickEndpoint() *poolEndpoint {
	now := time.Now()
	n := len(p.endpoints)
	start := int(atomic.AddUint64(&p.next, 1)-1) % n
	for i := 0; i < n; i++ {
		ep := p.endpoints[(start+i)%n]
		if ep.eligible(now) {
			return ep
		}
	}
	return p.endpoints[start]
}

// reportHealth refreshes the healthy-endpoint gauge for this backend.
func (p *poolBackend) reportHealth() {
	healthy := 0
	for _, ep := range p.endpoints {
		if !ep.isDown() {
			healthy++
		}
	}
	metrics.EndpointsHealthy.WithLabelValues(p.name).Set(float64(healthy))
}

// Call forwards the request to the next endpoint in rotation. A
// transport failure takes that endpoint a step toward being marked
// down; the request itself is not retried elsewhere (see the package
// comment).
func (p *poolBackend) Call(ctx context.Context, req *Request) (*Response, error) {
	ep := p.pickEndpoint()
	resp, err := ep.backend.Call(ctx, req)
	if err != nil {
		if ep.recordFailure(time.Now()) {
			p.reportHealth()
		}
		return nil, err
	}
	if ep.recordSuccess() {
		p.reportHealth()
	}
	return resp, nil
}

// Notify forwards the notification to the next endpoint in rotation,
// with the same failure accounting as Call.
func (p *poolBackend) Notify(ctx context.Context, method string, params map[string]any) error {
	ep := p.pickEndpoint()
	if err := ep.backend.Notify(ctx, method, params); err != nil {
		if ep.recordFailure(time.Now()) {
			p.reportHealth()
		}
		return err
	}
	if ep.recordSuccess() {
		p.reportHealth()
	}
	return nil
}

// Stream opens a live event stream from the next endpoint in
// rotation. Failures to establish the stream count toward taking the
// endpoint down; errors mid-stream cannot be attributed after the
// stream is handed out, so they do not.
func (p *poolBackend) Stream(ctx context.Context, req *Request) (EventStream, error) {
	ep := p.pickEndpoint()
	streamer, ok := ep.backend.(Streamer)
	if !ok {
		return nil, fmt.Errorf("pool backend %s: endpoint %s does not support streaming", p.name, ep.url)
	}
	stream, err := streamer.Stream(ctx, req)
	if err != nil {
		if ep.recordFailure(time.Now()) {
			p.reportHealth()
		}
		return nil, err
	}
	if ep.recordSuccess() {
		p.reportHealth()
	}
	return stream, nil
}

// Close releases every endpoint's resources.
func (p *poolBackend) Close() error {
	var firstErr error
	for _, ep := range p.endpoints {
		if err := ep.backend.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
