// Command mcpgate is a production gateway for MCP servers.
//
// It authenticates clients by API key (optionally also by OIDC JWT),
// rate-limits per key, caches safe responses, circuit-breaks failing
// backends, and exposes Prometheus metrics — the production layer that
// raw MCP servers don't ship with.
//
// Usage:
//
//	mcpgate -config examples/config.yaml          # HTTP gateway
//	mcpgate -config examples/config.yaml -stdio   # stdio MCP server
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sunwen1983/mcpgate/internal/config"
	"github.com/sunwen1983/mcpgate/internal/gateway"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the YAML configuration file")
	stdioMode := flag.Bool("stdio", false,
		"serve MCP over stdin/stdout, aggregating all configured backends (for stdio-only clients and directories); logs go to stderr")
	flag.Parse()

	// In stdio mode stdout is the MCP protocol channel, so logs must
	// go to stderr — a single log line on stdout corrupts the stream.
	logDest := os.Stdout
	if *stdioMode {
		logDest = os.Stderr
	}
	logger := slog.New(slog.NewJSONHandler(logDest, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	gw, err := gateway.New(cfg, logger)
	if err != nil {
		logger.Error("failed to start gateway", "error", err)
		os.Exit(1)
	}
	defer gw.Close()

	if *stdioMode {
		logger.Info("mcpgate serving over stdio",
			"backends", len(cfg.Backends),
		)
		ctx, stop := signal.NotifyContext(context.Background(),
			syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := gw.ServeStdio(ctx, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "stdio server error: %v\n", err)
			os.Exit(1)
		}
		logger.Info("mcpgate stopped")
		return
	}

	server := &http.Server{
		Addr:         cfg.Listen,
		Handler:      gw,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	// Graceful shutdown on SIGINT / SIGTERM.
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-shutdown
		logger.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	logger.Info("mcpgate listening",
		"addr", cfg.Listen,
		"backends", len(cfg.Backends),
		"api_keys", len(cfg.APIKeys),
	)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
	logger.Info("mcpgate stopped")
}
