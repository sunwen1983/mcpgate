// Package metrics exposes Prometheus metrics for the gateway.
//
// Metric names:
//   - mcpgate_requests_total{backend,method,status} — requests by outcome
//     (status: ok, rpc_error, backend_error, rejected)
//   - mcpgate_request_duration_seconds{backend} — backend call latency
//   - mcpgate_cache_hits_total{backend,method} / mcpgate_cache_misses_total
//   - mcpgate_backend_errors_total{backend}
//   - mcpgate_breaker_state{backend} — 0 closed, 1 half-open, 2 open
//   - mcpgate_rate_limit_rejections_total{key_name}
//   - mcpgate_notifications_total{backend,method,status} — notifications by
//     outcome (status: forwarded, dropped, failed)
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mcpgate_requests_total",
		Help: "Total gateway requests by backend, method, and outcome status.",
	}, []string{"backend", "method", "status"})

	RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mcpgate_request_duration_seconds",
		Help:    "Backend call latency in seconds.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"backend"})

	CacheHits = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mcpgate_cache_hits_total",
		Help: "Cache hits by backend and method.",
	}, []string{"backend", "method"})

	CacheMisses = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mcpgate_cache_misses_total",
		Help: "Cache misses by backend and method.",
	}, []string{"backend", "method"})

	BackendErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mcpgate_backend_errors_total",
		Help: "Backend transport errors by backend.",
	}, []string{"backend"})

	BreakerState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mcpgate_breaker_state",
		Help: "Circuit breaker state by backend: 0 closed, 1 half-open, 2 open.",
	}, []string{"backend"})

	RateLimitRejections = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mcpgate_rate_limit_rejections_total",
		Help: "Rate-limited requests by API key name.",
	}, []string{"key_name"})

	NotificationsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mcpgate_notifications_total",
		Help: "Notifications by backend, method, and outcome status (forwarded, dropped, failed).",
	}, []string{"backend", "method", "status"})
)
