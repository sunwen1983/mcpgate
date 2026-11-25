package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	var opts Options
	var maxErrorRate float64
	flag.StringVar(&opts.BinaryPath, "binary", "", "prebuilt mcpgate binary (default: build from -repo)")
	flag.StringVar(&opts.RepoDir, "repo", ".", "repo root containing ./cmd/mcpgate")
	flag.IntVar(&opts.Concurrency, "concurrency", 10, "parallel workers")
	flag.IntVar(&opts.Requests, "requests", 1000, "total requests to fire")
	flag.StringVar(&opts.Method, "method", "tools/call", "JSON-RPC method: tools/call or tools/list")
	flag.DurationVar(&opts.BackendDelay, "backend-delay", 0, "artificial mock-backend latency, e.g. 50ms")
	flag.Float64Var(&maxErrorRate, "max-error-rate", 0, "fail when the error rate exceeds this fraction")
	flag.Parse()

	rep, err := run(context.Background(), opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadtest: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("loadtest: %d requests, %d concurrent, %s\n", rep.Requests, opts.Concurrency, opts.Method)
	fmt.Printf("  QPS: %.1f  P50: %s  P99: %s  errors: %d (%.1f%%)  elapsed: %s\n",
		rep.QPS, rep.P50.Round(time.Microsecond), rep.P99.Round(time.Microsecond),
		rep.Errors, rep.ErrorRate()*100, rep.Elapsed.Round(time.Millisecond))
	fmt.Printf("  backend calls: %d (of %d requests reached the mock backend)\n", rep.BackendCalls, rep.Requests)
	if rep.ErrorRate() > maxErrorRate {
		fmt.Fprintf(os.Stderr, "loadtest: error rate %.1f%% exceeds -max-error-rate %.1f%%\n",
			rep.ErrorRate()*100, maxErrorRate*100)
		os.Exit(1)
	}
}
