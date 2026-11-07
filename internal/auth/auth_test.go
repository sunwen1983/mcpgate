package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sunwen1983/mcpgate/internal/auth"
	"github.com/sunwen1983/mcpgate/internal/config"
	"github.com/sunwen1983/mcpgate/internal/testoidc"
)

func testKeys() []config.APIKey {
	return []config.APIKey{
		{Key: "secret-alice", Name: "alice"},
		{Key: "secret-bob", Name: "bob"},
	}
}

func requestWith(headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/mcp/echo", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestAuthenticateAPIKey(t *testing.T) {
	a := auth.New(testKeys(), nil, nil)

	cases := []struct {
		name    string
		headers map[string]string
		wantOK  bool
		want    string // expected identity name
	}{
		{"x-api-key match", map[string]string{"X-API-Key": "secret-alice"}, true, "alice"},
		{"bearer api key match", map[string]string{"Authorization": "Bearer secret-bob"}, true, "bob"},
		{"wrong key", map[string]string{"X-API-Key": "nope"}, false, ""},
		{"wrong bearer", map[string]string{"Authorization": "Bearer nope"}, false, ""},
		{"no credentials", map[string]string{}, false, ""},
		{"empty bearer", map[string]string{"Authorization": "Bearer "}, false, ""},
		{"basic auth ignored", map[string]string{"Authorization": "Basic c2VjcmV0"}, false, ""},
		// X-API-Key takes precedence: a wrong X-API-Key is not rescued by
		// a valid Bearer token.
		{
			"x-api-key precedence",
			map[string]string{"X-API-Key": "wrong", "Authorization": "Bearer secret-alice"},
			false, "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, ok := a.Authenticate(requestWith(tc.headers))
			if ok != tc.wantOK {
				t.Fatalf("Authenticate ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && key.Name != tc.want {
				t.Errorf("identity = %q, want %q", key.Name, tc.want)
			}
		})
	}
}

// TestAuthenticateCarriesScopes verifies that the identity returned by
// Authenticate carries the credential's scopes, whichever credential
// type presented them: API keys from config, OIDC JWTs from the
// scope/scp claims, OAuth tokens from the introspection scope claim.
func TestAuthenticateCarriesScopes(t *testing.T) {
	t.Run("api key scopes from config", func(t *testing.T) {
		keys := []config.APIKey{{Key: "secret-scoped", Name: "scoped", Scopes: []string{"tools:read"}}}
		a := auth.New(keys, nil, nil)
		key, ok := a.Authenticate(requestWith(map[string]string{"X-API-Key": "secret-scoped"}))
		if !ok {
			t.Fatal("Authenticate rejected a valid scoped API key")
		}
		if !equalStrings(key.Scopes, []string{"tools:read"}) {
			t.Errorf("scopes = %v, want [tools:read]", key.Scopes)
		}
	})

	t.Run("oidc jwt scopes from scope claim", func(t *testing.T) {
		idp := testoidc.New(t)
		v, err := auth.NewVerifier(config.OIDCConfig{
			Issuer:   idp.Issuer(),
			Audience: "mcpgate-test",
		})
		if err != nil {
			t.Fatalf("NewVerifier: %v", err)
		}
		a := auth.New(testKeys(), v, nil)
		token := idp.Sign(t, "key-1", idp.Claims("mcpgate-test", map[string]any{"scope": "tools:read tools:admin"}))
		key, ok := a.Authenticate(requestWith(map[string]string{"Authorization": "Bearer " + token}))
		if !ok {
			t.Fatal("Authenticate rejected a valid JWT")
		}
		if key.Name != "oidc:user-123" {
			t.Errorf("identity = %q, want %q", key.Name, "oidc:user-123")
		}
		if !equalStrings(key.Scopes, []string{"tools:read", "tools:admin"}) {
			t.Errorf("scopes = %v, want [tools:read tools:admin]", key.Scopes)
		}
	})

	t.Run("oauth token scopes from introspection", func(t *testing.T) {
		f := newFakeIntrospectionServer(t, map[string]map[string]any{
			"tok-scoped": {
				"active": true,
				"sub":    "user-456",
				"scope":  "tools:read",
				"exp":    time.Now().Add(time.Hour).Unix(),
			},
		})
		in := introspectorFor(t, f, "")
		a := auth.New(testKeys(), nil, in)
		key, ok := a.Authenticate(requestWith(map[string]string{"Authorization": "Bearer tok-scoped"}))
		if !ok {
			t.Fatal("Authenticate rejected an active introspected token")
		}
		if key.Name != "oauth:user-456" {
			t.Errorf("identity = %q, want %q", key.Name, "oauth:user-456")
		}
		if !equalStrings(key.Scopes, []string{"tools:read"}) {
			t.Errorf("scopes = %v, want [tools:read]", key.Scopes)
		}
	})
}

// TestAuthenticateOIDCFallback verifies that a Bearer token which matches
// no API key is tried as an OIDC JWT, and that X-API-Key values are never
// interpreted as JWTs.
func TestAuthenticateOIDCFallback(t *testing.T) {
	idp := testoidc.New(t)
	v, err := auth.NewVerifier(config.OIDCConfig{
		Issuer:   idp.Issuer(),
		Audience: "mcpgate-test",
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	a := auth.New(testKeys(), v, nil)

	valid := idp.Sign(t, "key-1", idp.Claims("mcpgate-test", nil))

	t.Run("valid jwt authenticates as oidc identity", func(t *testing.T) {
		key, ok := a.Authenticate(requestWith(map[string]string{"Authorization": "Bearer " + valid}))
		if !ok {
			t.Fatal("Authenticate rejected a valid JWT")
		}
		if key.Name != "oidc:user-123" {
			t.Errorf("identity = %q, want %q", key.Name, "oidc:user-123")
		}
		if key.Key != "" {
			t.Error("JWT identity must not carry a key value")
		}
	})

	t.Run("garbage bearer rejected", func(t *testing.T) {
		_, ok := a.Authenticate(requestWith(map[string]string{"Authorization": "Bearer not.a.jwt"}))
		if ok {
			t.Error("Authenticate accepted a garbage Bearer token")
		}
	})

	t.Run("x-api-key never tried as jwt", func(t *testing.T) {
		_, ok := a.Authenticate(requestWith(map[string]string{"X-API-Key": valid}))
		if ok {
			t.Error("Authenticate interpreted an X-API-Key value as a JWT")
		}
	})

	t.Run("api keys still work with oidc configured", func(t *testing.T) {
		key, ok := a.Authenticate(requestWith(map[string]string{"X-API-Key": "secret-alice"}))
		if !ok || key.Name != "alice" {
			t.Errorf("API key auth broke with OIDC configured: ok=%v name=%q", ok, key.Name)
		}
	})

	t.Run("no verifier means no jwt fallback", func(t *testing.T) {
		plain := auth.New(testKeys(), nil, nil)
		_, ok := plain.Authenticate(requestWith(map[string]string{"Authorization": "Bearer " + valid}))
		if ok {
			t.Error("accepted a JWT with no OIDC verifier configured")
		}
	})
}
