package gateway

// Test hooks. This file is compiled only under `go test`.

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/sunwen1983/mcpgate/internal/backend"
	"github.com/sunwen1983/mcpgate/internal/config"
)

// NewTestServer builds a Gateway whose backend factory always returns fake,
// and serves it on an httptest server.
func NewTestServer(t *testing.T, cfg *config.Config, fake backend.Backend) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	g, err := newWithBackendFactory(cfg, logger, func(c config.Backend) (backend.Backend, error) {
		return fake, nil
	})
	if err != nil {
		t.Fatalf("newWithBackendFactory: %v", err)
	}
	t.Cleanup(g.Close)
	return httptest.NewServer(g)
}

// NewTestGateway builds a Gateway with a caller-supplied backend factory,
// for tests that drive the Gateway directly (e.g. the stdio transport).
func NewTestGateway(t *testing.T, cfg *config.Config, factory func(config.Backend) (backend.Backend, error)) *Gateway {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	g, err := newWithBackendFactory(cfg, logger, factory)
	if err != nil {
		t.Fatalf("newWithBackendFactory: %v", err)
	}
	t.Cleanup(g.Close)
	return g
}
