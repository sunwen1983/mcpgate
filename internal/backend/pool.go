// Load balancing: one backend name, several endpoints.
//
// A pooled backend is an http backend configured with `endpoints`
// instead of a single `url`. The pool keeps one httpBackend per URL
// and spreads requests across them; the strategy (see
// config.Strategy*) selects how: plain round-robin, smooth weighted
// round-robin, or least-connections. The gateway needs no changes:
// the pool satisfies the same Backend (and Streamer) interfaces as a
// single endpoint, so the breaker, cache, health checker, and SSE
// relay all see one logical backend.
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
	// weight is this endpoint's share of traffic under the weighted
	// round-robin strategy; 1 means an equal share. Ignored by the
	// other strategies.
	weight int
	// currentWeight is the smooth weighted round-robin accumulator
	// (nginx algorithm). Guarded by poolBackend.mu, not by mu.
	currentWeight int
	// inFlight counts requests currently being served by this
	// endpoint (calls, notifications, and open streams). The
	// least-connections strategy reads it to pick the least busy
	// endpoint.
	inFlight atomic.Int64

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

// begin marks one request in flight on this endpoint; end releases
// it. Callers must pair them, including for streams, where end runs
// when the stream is closed rather than when it is opened.
func (e *poolEndpoint) begin() { e.inFlight.Add(1) }
func (e *poolEndpoint) end()   { e.inFlight.Add(-1) }

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
	strategy  string
	next      uint64 // round-robin cursor
	// mu serializes weighted round-robin picks: the smooth algorithm
	// updates every candidate's currentWeight as one step, so two
	// concurrent picks must not interleave.
	mu sync.Mutex
}

// newPoolBackend builds a pool of one httpBackend per URL. The
// per-endpoint backends share the rest of cfg (auth, headers, timeout)
// and differ only in URL. failureThreshold and cooldown override the
// defaults when positive; New passes the defaults. The strategy comes
// from cfg (empty means round-robin); cfg.Weights, when set, must
// line up one-to-one with urls and defaults to 1 per endpoint.
func newPoolBackend(cfg config.Backend, urls []string, failureThreshold int, cooldown time.Duration) (*poolBackend, error) {
	if len(urls) == 0 {
		return nil, fmt.Errorf("pool backend %s: at least one endpoint is required", cfg.Name)
	}
	strategy := cfg.Strategy
	if strategy == "" {
		strategy = config.StrategyRoundRobin
	}
	switch strategy {
	case config.StrategyRoundRobin, config.StrategyWeightedRoundRobin, config.StrategyLeastConnections:
	default:
		return nil, fmt.Errorf("pool backend %s: unknown load balancing strategy %q", cfg.Name, strategy)
	}
	weights := cfg.Weights
	if len(weights) == 0 {
		weights = make([]int, len(urls))
		for i := range weights {
			weights[i] = 1
		}
	}
	if len(weights) != len(urls) {
		return nil, fmt.Errorf("pool backend %s: %d weights for %d endpoints", cfg.Name, len(weights), len(urls))
	}
	if failureThreshold <= 0 {
		failureThreshold = poolFailureThreshold
	}
	if cooldown <= 0 {
		cooldown = poolCooldown
	}
	p := &poolBackend{
		name:     cfg.Name,
		strategy: strategy,
	}
	for i, u := range urls {
		if weights[i] < 1 {
			return nil, fmt.Errorf("pool backend %s: weight for endpoint %s must be at least 1", cfg.Name, u)
		}
		epCfg := cfg
		epCfg.URL = u
		epCfg.Endpoints = nil
		p.endpoints = append(p.endpoints, &poolEndpoint{
			backend:          newHTTPBackend(epCfg),
			url:              u,
			weight:           weights[i],
			failureThreshold: failureThreshold,
			cooldown:         cooldown,
		})
	}
	metrics.EndpointsHealthy.WithLabelValues(cfg.Name).Set(float64(len(urls)))
	return p, nil
}

// EndpointStatus is one pooled endpoint's rotation state, as reported
// to /readyz: Down is true while the endpoint is out of rotation
// after consecutive transport failures. It clears on the next success,
// so a down endpoint that has recovered but not yet been tried again
// still reports down until traffic proves it.
type EndpointStatus struct {
	URL  string
	Down bool
}

// EndpointReporter is implemented by backends that spread traffic
// across several endpoints (pools). The gateway reports it in /readyz
// so an operator can see degraded redundancy without Prometheus;
// single-endpoint backends do not implement it.
type EndpointReporter interface {
	EndpointsStatus() []EndpointStatus
}

// EndpointsStatus snapshots every endpoint's rotation state, in
// configured order.
func (p *poolBackend) EndpointsStatus() []EndpointStatus {
	out := make([]EndpointStatus, 0, len(p.endpoints))
	for _, ep := range p.endpoints {
		out = append(out, EndpointStatus{URL: ep.url, Down: ep.isDown()})
	}
	return out
}

func (p *poolBackend) Name() string { return p.name }

// pickEndpoint returns the next endpoint under the pool's strategy,
// skipping endpoints that are down. When every endpoint is down it
// falls back to round-robin over all of them, so a recovered endpoint
// gets traffic again instead of every request failing.
func (p *poolBackend) pickEndpoint() *poolEndpoint {
	switch p.strategy {
	case config.StrategyWeightedRoundRobin:
		return p.pickWeighted()
	case config.StrategyLeastConnections:
		return p.pickLeastConnections()
	default:
		return p.pickRoundRobin()
	}
}

// eligibleEndpoints returns the endpoints that may receive traffic
// now: up, or down but past their cooldown.
func (p *poolBackend) eligibleEndpoints(now time.Time) []*poolEndpoint {
	var out []*poolEndpoint
	for _, ep := range p.endpoints {
		if ep.eligible(now) {
			out = append(out, ep)
		}
	}
	return out
}

// pickRoundRobin returns the next endpoint in turn, skipping
// endpoints that are down.
func (p *poolBackend) pickRoundRobin() *poolEndpoint {
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

// pickWeighted returns the next endpoint by smooth weighted
// round-robin (the nginx algorithm): each eligible endpoint's current
// weight grows by its weight, the highest is picked, and the picker's
// current weight drops by the total — so a 2:1 pool serves a, b, a,
// a, b, a… instead of bursting a, a, b.
func (p *poolBackend) pickWeighted() *poolEndpoint {
	p.mu.Lock()
	defer p.mu.Unlock()
	candidates := p.eligibleEndpoints(time.Now())
	if len(candidates) == 0 {
		candidates = p.endpoints
	}
	total := 0
	var best *poolEndpoint
	for _, ep := range candidates {
		total += ep.weight
		ep.currentWeight += ep.weight
		if best == nil || ep.currentWeight > best.currentWeight {
			best = ep
		}
	}
	best.currentWeight -= total
	return best
}

// pickLeastConnections returns the eligible endpoint with the fewest
// requests currently in flight. Ties break in round-robin order
// starting from the cursor, so an idle pool still spreads evenly
// instead of pinning every request to the first endpoint.
func (p *poolBackend) pickLeastConnections() *poolEndpoint {
	now := time.Now()
	n := len(p.endpoints)
	start := int(atomic.AddUint64(&p.next, 1)-1) % n
	bestIdx := -1
	var bestInFlight int64
	for i := 0; i < n; i++ {
		ep := p.endpoints[(start+i)%n]
		if !ep.eligible(now) {
			continue
		}
		inFlight := ep.inFlight.Load()
		if bestIdx == -1 || inFlight < bestInFlight {
			bestIdx = (start + i) % n
			bestInFlight = inFlight
		}
	}
	if bestIdx == -1 {
		return p.endpoints[start]
	}
	return p.endpoints[bestIdx]
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
	ep.begin()
	defer ep.end()
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
	ep.begin()
	defer ep.end()
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
// stream is handed out, so they do not. The stream counts as in
// flight on its endpoint until it is closed, so least-connections
// sees long-lived SSE streams as load.
func (p *poolBackend) Stream(ctx context.Context, req *Request) (EventStream, error) {
	ep := p.pickEndpoint()
	ep.begin()
	streamer, ok := ep.backend.(Streamer)
	if !ok {
		ep.end()
		return nil, fmt.Errorf("pool backend %s: endpoint %s does not support streaming", p.name, ep.url)
	}
	stream, err := streamer.Stream(ctx, req)
	if err != nil {
		ep.end()
		if ep.recordFailure(time.Now()) {
			p.reportHealth()
		}
		return nil, err
	}
	if ep.recordSuccess() {
		p.reportHealth()
	}
	return &countingStream{EventStream: stream, ep: ep}, nil
}

// countingStream releases its endpoint's in-flight slot when the
// stream is closed. Close may be called more than once; the slot is
// released only on the first call.
type countingStream struct {
	EventStream
	ep   *poolEndpoint
	once sync.Once
}

func (s *countingStream) Close() error {
	s.once.Do(s.ep.end)
	return s.EventStream.Close()
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
