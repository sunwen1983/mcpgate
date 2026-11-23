// Command loadtest is the live load-test harness for mcpgate (v0.3
// benchmarking, Day 2): it starts the real mcpgate binary against a
// mock HTTP MCP backend, fires concurrent JSON-RPC load at it, and
// reports the headline numbers — QPS, P50/P99 latency, error rate.
//
// Usage:
//
//	loadtest -concurrency 10 -requests 1000   # tools/call, uncached
//	loadtest -method tools/list -requests 500
//	loadtest -backend-delay 50ms              # simulate a slow backend
//	loadtest -binary ./mcpgate                # skip rebuilding
//
// The harness measures the gateway, not the token bucket: the generated
// config sets the rate limit far above what the load can reach (the
// same rationale as the Day 1 in-process benchmarks), and caching is
// off, so every tools/call reaches the mock backend — Report.BackendCalls
// proves it. Exit status is non-zero when the error rate exceeds
// -max-error-rate (default: any error fails).
package main

import (
	"math"
	"sort"
	"time"
)

// Report is the headline result of one load-test run.
type Report struct {
	Requests     int
	Errors       int
	BackendCalls int // requests the mock backend actually served
	Elapsed      time.Duration
	QPS          float64
	P50          time.Duration
	P99          time.Duration
}

// ErrorRate is Errors / Requests as a fraction in [0, 1].
func (r Report) ErrorRate() float64 {
	if r.Requests == 0 {
		return 0
	}
	return float64(r.Errors) / float64(r.Requests)
}

// percentile returns the nearest-rank percentile of latencies: the
// smallest value v such that at least p percent of samples are <= v.
// It sorts a copy, so callers may pass unsorted samples. An empty
// sample returns 0.
func percentile(latencies []time.Duration, p float64) time.Duration {
	if len(latencies) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	// Nearest rank: ceil(p/100 * n), 1-based, clamped into range.
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// summarize turns raw per-request latencies into a Report. elapsed is
// the wall time of the load phase; QPS is requests / elapsed. Every
// issued request contributes one latency sample (even a failed one —
// a 500 is still a fast answer), so Requests is len(latencies) and
// Errors is the subset that failed.
func summarize(latencies []time.Duration, errors int, elapsed time.Duration) Report {
	rep := Report{
		Requests: len(latencies),
		Errors:   errors,
		Elapsed:  elapsed,
		P50:      percentile(latencies, 50),
		P99:      percentile(latencies, 99),
	}
	if elapsed > 0 {
		rep.QPS = float64(rep.Requests) / elapsed.Seconds()
	}
	return rep
}
