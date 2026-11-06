package auth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sunwen1983/mcpgate/internal/auth"
	"github.com/sunwen1983/mcpgate/internal/config"
)

// fakeIntrospectionServer is a test RFC 7662 endpoint. It answers from
// the active map (token -> response) and counts requests. expectedAuth
// is the Basic-auth credential it requires ("client-id:secret").
type fakeIntrospectionServer struct {
	server       *httptest.Server
	active       map[string]map[string]any
	expectedAuth string
	requests     atomic.Int32
	// lastForm records the form body of the most recent request.
	lastForm url.Values
}

func newFakeIntrospectionServer(t *testing.T, active map[string]map[string]any) *fakeIntrospectionServer {
	t.Helper()
	f := &fakeIntrospectionServer{active: active}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		user, pass, ok := r.BasicAuth()
		if !ok || user+":"+pass != f.expectedAuth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.lastForm = r.Form
		resp, ok := f.active[r.Form.Get("token")]
		if !ok {
			resp = map[string]any{"active": false}
		}
		// tok-short's exp is computed per request: a static Unix-second
		// exp only 150ms out can already be past by the time the test
		// verifies it (Unix() truncates to whole seconds).
		if r.Form.Get("token") == "tok-short" {
			resp = map[string]any{
				"active": true,
				"sub":    "user-short",
				"exp":    time.Now().Add(1200 * time.Millisecond).Unix(),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func introspectorFor(t *testing.T, f *fakeIntrospectionServer, ttl string) *auth.Introspector {
	t.Helper()
	cfg := config.OAuthConfig{
		IntrospectionURL: f.server.URL,
		ClientID:         "gateway",
		ClientSecret:     "s3cret",
	}
	if ttl != "" {
		d, err := time.ParseDuration(ttl)
		if err != nil {
			t.Fatalf("parse ttl: %v", err)
		}
		cfg.CacheTTL = config.Duration(d)
	}
	f.expectedAuth = "gateway:s3cret"
	return auth.NewIntrospector(cfg)
}

func TestIntrospectorVerify(t *testing.T) {
	f := newFakeIntrospectionServer(t, map[string]map[string]any{
		"tok-active": {
			"active": true,
			"sub":    "user-456",
			"scope":  "tools:read tools:call",
			"exp":    time.Now().Add(time.Hour).Unix(),
		},
		"tok-inactive": {"active": false},
		"tok-no-sub":   {"active": true},
		"tok-expired": {
			"active": true,
			"sub":    "user-456",
			"exp":    time.Now().Add(-time.Hour).Unix(),
		},
	})
	in := introspectorFor(t, f, "")

	result, err := in.Verify("tok-active")
	if err != nil {
		t.Fatalf("Verify(active) = %v", err)
	}
	if result.Subject != "user-456" {
		t.Errorf("Subject = %q, want %q", result.Subject, "user-456")
	}
	if len(result.Scopes) != 2 || result.Scopes[0] != "tools:read" || result.Scopes[1] != "tools:call" {
		t.Errorf("Scopes = %v, want [tools:read tools:call]", result.Scopes)
	}
	// The request used Basic auth with the gateway's client credentials
	// and the RFC 7662 form fields.
	if got := f.lastForm.Get("token"); got != "tok-active" {
		t.Errorf("form token = %q, want %q", got, "tok-active")
	}
	if got := f.lastForm.Get("token_type_hint"); got != "access_token" {
		t.Errorf("form token_type_hint = %q, want %q", got, "access_token")
	}

	cases := []struct {
		name  string
		token string
	}{
		{"inactive token", "tok-inactive"},
		{"unknown token", "tok-unknown"},
		{"active without sub", "tok-no-sub"},
		{"expired token", "tok-expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := in.Verify(tc.token); err == nil {
				t.Errorf("Verify(%q) = nil error, want an error", tc.token)
			}
		})
	}
}

func TestIntrospectorServerErrors(t *testing.T) {
	// Non-200 responses and malformed bodies fail closed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.FormValue("token") {
		case "tok-500":
			w.WriteHeader(http.StatusInternalServerError)
		case "tok-garbage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{not json"))
		}
	}))
	defer srv.Close()
	in := auth.NewIntrospector(config.OAuthConfig{
		IntrospectionURL: srv.URL,
		ClientID:         "gateway",
		ClientSecret:     "s3cret",
	})
	for _, token := range []string{"tok-500", "tok-garbage"} {
		if _, err := in.Verify(token); err == nil {
			t.Errorf("Verify(%q) = nil error, want an error", token)
		}
	}

	// Wrong client credentials (server rejects with 401) fail closed.
	f := newFakeIntrospectionServer(t, map[string]map[string]any{
		"tok-active": {"active": true, "sub": "user-456"},
	})
	f.expectedAuth = "someone-else:other-secret"
	badCreds := auth.NewIntrospector(config.OAuthConfig{
		IntrospectionURL: f.server.URL,
		ClientID:         "gateway",
		ClientSecret:     "wrong",
	})
	if _, err := badCreds.Verify("tok-active"); err == nil {
		t.Error("Verify with wrong client credentials = nil error, want an error")
	}

	// An unreachable endpoint fails closed instead of hanging.
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closed.Close()
	unreachable := auth.NewIntrospector(config.OAuthConfig{
		IntrospectionURL: closed.URL,
		ClientID:         "gateway",
		ClientSecret:     "s3cret",
	})
	if _, err := unreachable.Verify("tok-active"); err == nil {
		t.Error("Verify against an unreachable endpoint = nil error, want an error")
	}
}

func TestIntrospectorCache(t *testing.T) {
	f := newFakeIntrospectionServer(t, map[string]map[string]any{
		"tok-cached": {
			"active": true,
			"sub":    "user-789",
			"exp":    time.Now().Add(time.Hour).Unix(),
		},
		// "tok-short" is answered dynamically by the fake server (see
		// above): exp is ~1.2s out at each request.
	})
	in := introspectorFor(t, f, "50ms")

	// The second Verify of the same token is served from cache: no
	// second request reaches the server.
	if _, err := in.Verify("tok-cached"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if _, err := in.Verify("tok-cached"); err != nil {
		t.Fatalf("Verify (cached): %v", err)
	}
	if got := f.requests.Load(); got != 1 {
		t.Errorf("server requests = %d, want 1 (second Verify must be cached)", got)
	}

	// After the TTL lapses the entry is re-introspected.
	time.Sleep(60 * time.Millisecond)
	if _, err := in.Verify("tok-cached"); err != nil {
		t.Fatalf("Verify (after TTL): %v", err)
	}
	if got := f.requests.Load(); got != 2 {
		t.Errorf("server requests = %d, want 2 (TTL expiry must re-introspect)", got)
	}

	// A token whose exp is sooner than the configured TTL is cached
	// only until exp, not for the full TTL. This needs an introspector
	// with a long TTL: with the 50ms one above, the TTL — not exp —
	// would be what expires the entry.
	inLong := introspectorFor(t, f, "")
	before := f.requests.Load()
	if _, err := inLong.Verify("tok-short"); err != nil {
		t.Fatalf("Verify(short exp): %v", err)
	}
	if _, err := inLong.Verify("tok-short"); err != nil {
		t.Fatalf("Verify(short exp, cached): %v", err)
	}
	if got := f.requests.Load(); got != before+1 {
		t.Errorf("server requests = %d, want %d (short-exp token must cache once)", got, before+1)
	}
	time.Sleep(1300 * time.Millisecond)
	if _, err := inLong.Verify("tok-short"); err != nil {
		t.Fatalf("Verify(short exp, after exp): %v", err)
	}
	if got := f.requests.Load(); got != before+2 {
		t.Errorf("server requests = %d, want %d (expired cache entry must re-introspect)", got, before+2)
	}
}

// TestAuthenticateOAuthFallback verifies that a Bearer token which is
// neither an API key nor a valid JWT is introspected, and that X-API-Key
// values are never sent to the introspection endpoint.
func TestAuthenticateOAuthFallback(t *testing.T) {
	f := newFakeIntrospectionServer(t, map[string]map[string]any{
		"opaque-123": {
			"active": true,
			"sub":    "user-456",
			"exp":    time.Now().Add(time.Hour).Unix(),
		},
	})
	in := introspectorFor(t, f, "")
	a := auth.New(testKeys(), nil, in)

	t.Run("valid opaque token authenticates as oauth identity", func(t *testing.T) {
		key, ok := a.Authenticate(requestWith(map[string]string{"Authorization": "Bearer opaque-123"}))
		if !ok {
			t.Fatal("Authenticate rejected a valid introspected token")
		}
		if key.Name != "oauth:user-456" {
			t.Errorf("identity = %q, want %q", key.Name, "oauth:user-456")
		}
		if key.Key != "" {
			t.Error("introspected identity must not carry a key value")
		}
	})

	t.Run("inactive token rejected", func(t *testing.T) {
		_, ok := a.Authenticate(requestWith(map[string]string{"Authorization": "Bearer opaque-999"}))
		if ok {
			t.Error("Authenticate accepted an inactive token")
		}
	})

	t.Run("x-api-key never introspected", func(t *testing.T) {
		before := f.requests.Load()
		_, ok := a.Authenticate(requestWith(map[string]string{"X-API-Key": "opaque-123"}))
		if ok {
			t.Error("Authenticate interpreted an X-API-Key value as an opaque token")
		}
		if got := f.requests.Load(); got != before {
			t.Errorf("introspection requests = %d, want %d (X-API-Key must not reach the endpoint)", got, before)
		}
	})

	t.Run("api keys still work with introspection configured", func(t *testing.T) {
		key, ok := a.Authenticate(requestWith(map[string]string{"X-API-Key": "secret-alice"}))
		if !ok || key.Name != "alice" {
			t.Errorf("API key auth broke with introspection configured: ok=%v name=%q", ok, key.Name)
		}
	})

	t.Run("no introspector means no opaque-token fallback", func(t *testing.T) {
		plain := auth.New(testKeys(), nil, nil)
		_, ok := plain.Authenticate(requestWith(map[string]string{"Authorization": "Bearer opaque-123"}))
		if ok {
			t.Error("accepted an opaque token with no introspector configured")
		}
	})
}

// TestIntrospectorSendsTokenNotLogged is a smoke check that Verify
// never includes the token in its error: the error must be the same
// generic string for every failure mode.
func TestIntrospectorErrorIsGeneric(t *testing.T) {
	f := newFakeIntrospectionServer(t, map[string]map[string]any{
		"tok-inactive": {"active": false},
	})
	in := introspectorFor(t, f, "")
	_, errInactive := in.Verify("tok-inactive")
	_, errUnknown := in.Verify("tok-unknown")
	if errInactive == nil || errUnknown == nil {
		t.Fatal("expected errors for inactive/unknown tokens")
	}
	if errInactive.Error() != errUnknown.Error() {
		t.Errorf("errors differ by failure mode: %q vs %q (must be generic)", errInactive, errUnknown)
	}
	if got := errInactive.Error(); got != "invalid bearer token" {
		t.Errorf("error = %q, want %q", got, "invalid bearer token")
	}
}
