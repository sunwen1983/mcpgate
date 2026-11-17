package gateway

import (
	"net/http"
	"strings"
)

// CORS for browser callers.
//
// Browser runtimes (for example the Agent Bazaar site) call the gateway
// cross-origin: POST /mcp/{backend} with X-API-Key and
// Content-Type: application/json. A request like that triggers a CORS
// preflight (OPTIONS), and without CORS headers the browser blocks the
// call before it ever reaches the handler.
//
// Only the browser-facing paths are opened: the MCP proxy and the public
// health endpoint. Admin endpoints and /metrics stay same-origin; a
// browser page has no business calling them, and opening them would widen
// the attack surface for no benefit.
//
// The middleware runs before routing, so a preflight is answered here
// with 200 instead of falling through to handleMCP (which requires POST)
// or to the authenticator (which would 401 a keyless OPTIONS).
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isCORSPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "X-API-Key, Authorization, Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isCORSPath reports whether a path is reachable from a browser page on
// another origin.
func isCORSPath(path string) bool {
	return path == "/healthz" || strings.HasPrefix(path, "/mcp/")
}
