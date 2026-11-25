# Benchmarks

Measured numbers for mcpgate — no tuning, no cherry-picking. v0.3 is
measure-only: these numbers are the baseline any later optimization has
to beat, on the same harness, on comparable hardware.

## Environment

All numbers below were measured on 2026-11-24 on a Linux VM (AMD EPYC
9D25, shared vCPUs, 4 CPUs visible to the benchmark process), Go 1.27.2,
from `bench/performance` at commit `9510cb6` (Day 2 harness) plus the
in-process benchmarks from `bf07f6f` (Day 1). The mock backend answers
with zero artificial delay, so the numbers measure gateway overhead,
not backend time. A real backend's own latency adds on top.

Treat these as relative, not absolute: a different machine, CPU count,
or backend will move them. Re-run the commands below before quoting
any number.

## In-process benchmarks (Day 1)

`go test ./internal/gateway/ -run '^$' -bench 'BenchmarkTools(Call|List)' -benchtime 3s -count 1`

One gateway against an in-process fake backend over `httptest`, serial
requests, rate limit raised far above the load so the token bucket is
not what is measured. The backend-call count asserted after each loop
proves what was actually measured (every call reached the backend, or
none did).

| Benchmark | ns/op | Allocs | What it measures |
| --- | --- | --- | --- |
| `BenchmarkToolsCallUncached` | ~80,000 | 172 allocs, ~11.8 KB | `tools/call`, non-cacheable tool: every call reaches the backend |
| `BenchmarkToolsCallCacheHit` | ~93,000 | 192 allocs, ~12.6 KB | `tools/call`, cacheable tool, cache pre-warmed: no call reaches the backend |
| `BenchmarkToolsList` | ~94,000 | 207 allocs, ~13.8 KB | `tools/list`, list cached after the first call |

Two honest observations, not marketing:

- A cache hit is not faster than an uncached call at this payload size
  (~93µs vs ~80µs). The cache's win is backend calls avoided, not
  gateway latency saved — a cached response still goes through the
  full JSON-RPC round-trip. Do not claim "caching makes calls faster"
  from these numbers.
- `tools/list` costs about the same as a call: the list payload is
  small here (one fake backend, few tools). A backend with a large
  tool catalog will cost more to serialize.

## Live load test (Day 2)

`go run ./cmd/loadtest -method tools/call -concurrency 10 -requests 5000 -max-error-rate 0`
`go run ./cmd/loadtest -method tools/list -concurrency 10 -requests 1000 -max-error-rate 0`

The harness in `cmd/loadtest` builds the real `mcpgate` binary, starts
it against a mock HTTP MCP backend, and fires concurrent JSON-RPC load
at it. Rate limiting is raised far above the load and caching is off,
so every request reaches the mock — the reported backend-call count
proves it. The harness fails when the error rate exceeds
`-max-error-rate`.

| Method | Concurrency | Requests | QPS | P50 | P99 | Errors |
| --- | --- | --- | --- | --- | --- | --- |
| `tools/call` | 10 | 5000 | ~4,000 | ~1.1 ms | ~53 ms | 0 |
| `tools/list` | 10 | 1000 | ~3,600 | ~1.1 ms | ~54 ms | 0 |
| `tools/call` (serial control) | 1 | 500 | ~3,400 | ~260 µs | ~790 µs | 0 |

Reproduced 2026-11-24 (same commands, same VM): `tools/call`
4,024 QPS / P50 1.14 ms / P99 53.3 ms; `tools/list` 3,615 QPS /
P50 1.14 ms / P99 54.2 ms; serial control P50 259 µs / P99 793 µs.
Day 2 (2026-11-23) measured ~3,994 QPS / P50 ~1.0 ms / P99 ~57 ms
for `tools/call` and ~3,821 QPS / P50 ~1.1 ms / P99 ~58 ms for
`tools/list` — the two runs agree within noise.

Reading the numbers:

- **P50 ~1.1 ms under concurrency vs ~260 µs serial.** Most of the
  gap is concurrency itself: 10 workers sharing 4 vCPUs queue behind
  each other. The serial number is the closer estimate of one
  request's own cost through the gateway.
- **P99 ~50 ms appears only under concurrency.** The serial control
  has P99 under 1 ms, so the tail is not a slow code path — it is
  almost certainly VM scheduling jitter on shared CPUs (a worker
  descheduled mid-request). Do not "fix" the P99 by tuning gateway
  code until it reproduces on dedicated hardware.
- **Zero errors at 4,000 QPS** with every request reaching the
  backend. The gateway is not the bottleneck at this load on this VM;
  the mock backend answers instantly, so this is a ceiling for
  gateway throughput, not a claim about real deployments.

## How to re-run

```sh
# In-process (Day 1 numbers)
go test ./internal/gateway/ -run '^$' -bench 'BenchmarkTools(Call|List)' -benchtime 3s -count 1

# Live load test (Day 2 numbers)
go run ./cmd/loadtest -method tools/call -concurrency 10 -requests 5000 -max-error-rate 0
go run ./cmd/loadtest -method tools/list -concurrency 10 -requests 1000 -max-error-rate 0
```

To test with a slower backend, pass `-backend-delay 50ms` to the
harness. To change the load shape, pass `-concurrency` / `-requests`.
