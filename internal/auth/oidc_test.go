package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/sunwen1983/mcpgate/internal/auth"
	"github.com/sunwen1983/mcpgate/internal/config"
	"github.com/sunwen1983/mcpgate/internal/testoidc"
)

func newTestVerifier(t *testing.T, idp *testoidc.Provider, skew time.Duration) *auth.Verifier {
	t.Helper()
	v, err := auth.NewVerifier(config.OIDCConfig{
		Issuer:    idp.Issuer(),
		Audience:  "mcpgate-test",
		ClockSkew: config.Duration(skew),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

func TestVerifyValid(t *testing.T) {
	idp := testoidc.New(t)
	v := newTestVerifier(t, idp, 0)
	sub, err := v.Verify(idp.Sign(t, "key-1", idp.Claims("mcpgate-test", nil)))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if sub != "user-123" {
		t.Errorf("sub = %q, want %q", sub, "user-123")
	}
}

func TestVerifyRejects(t *testing.T) {
	idp := testoidc.New(t)
	v := newTestVerifier(t, idp, 0)

	tampered := idp.Sign(t, "key-1", idp.Claims("mcpgate-test", nil))
	parts := strings.Split(tampered, ".")
	// Flip a character in the signature part.
	parts[2] = "A" + parts[2][1:]
	tampered = strings.Join(parts, ".")

	// Manually crafted tokens: {"alg":"none"} and {"alg":"HS256"}.
	noneHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"key-1"}`))
	hsHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","kid":"key-1"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"x","aud":"y","sub":"z","exp":9999999999}`))
	unsignedNone := noneHeader + "." + payload + "."
	hsToken := hsHeader + "." + payload + "." + base64.RawURLEncoding.EncodeToString([]byte("fakesig"))

	// A token signed by a key the IdP never served (unknown kid).
	otherPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rogue := jwt.NewWithClaims(jwt.SigningMethodRS256, idp.Claims("mcpgate-test", nil))
	rogue.Header["kid"] = "rogue-key"
	rogueSigned, err := rogue.SignedString(otherPriv)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		token string
	}{
		{"garbage", "not.a.jwt"},
		{"expired", idp.Sign(t, "key-1", idp.Claims("mcpgate-test", map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}))},
		{"no exp", idp.Sign(t, "key-1", idp.Claims("mcpgate-test", map[string]any{"exp": nil}))},
		{"wrong audience", idp.Sign(t, "key-1", idp.Claims("someone-else", nil))},
		{"wrong issuer", idp.Sign(t, "key-1", idp.Claims("mcpgate-test", map[string]any{"iss": "https://evil.example.com"}))},
		{"missing sub", idp.Sign(t, "key-1", idp.Claims("mcpgate-test", map[string]any{"sub": nil}))},
		{"tampered signature", tampered},
		{"none algorithm", unsignedNone},
		{"hmac algorithm", hsToken},
		{"unknown kid", rogueSigned},
		{"missing kid", func() string {
			tok := jwt.NewWithClaims(jwt.SigningMethodRS256, idp.Claims("mcpgate-test", nil))
			s, err := tok.SignedString(idp.PrivateKey(t, "key-1"))
			if err != nil {
				t.Fatal(err)
			}
			return s
		}()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := v.Verify(tc.token); err == nil {
				t.Error("Verify accepted a token it must reject")
			}
		})
	}
}

func TestVerifyRotation(t *testing.T) {
	idp := testoidc.New(t)
	v := newTestVerifier(t, idp, 0)

	// Rotate the IdP's keys. A token from the new key has an unknown kid,
	// which must trigger a JWKS refresh and then verify.
	kid2 := idp.Rotate(t)
	sub, err := v.Verify(idp.Sign(t, kid2, idp.Claims("mcpgate-test", nil)))
	if err != nil {
		t.Fatalf("Verify after rotation: %v", err)
	}
	if sub != "user-123" {
		t.Errorf("sub = %q, want %q", sub, "user-123")
	}

	// A token from the retired key must now fail: the refresh already ran
	// and the old kid is gone.
	old := jwt.NewWithClaims(jwt.SigningMethodRS256, idp.Claims("mcpgate-test", nil))
	old.Header["kid"] = "key-1"
	retiredPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	oldSigned, err := old.SignedString(retiredPriv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(oldSigned); err == nil {
		t.Error("Verify accepted a token with a retired kid")
	}
}

func TestVerifyClockSkew(t *testing.T) {
	idp := testoidc.New(t)
	v := newTestVerifier(t, idp, 60*time.Second)

	// Expired 30s ago: within the 60s skew, must verify.
	tok := idp.Sign(t, "key-1", idp.Claims("mcpgate-test", map[string]any{"exp": time.Now().Add(-30 * time.Second).Unix()}))
	if _, err := v.Verify(tok); err != nil {
		t.Errorf("Verify rejected a token within clock skew: %v", err)
	}

	// Expired 5 minutes ago: outside the skew, must fail.
	tok = idp.Sign(t, "key-1", idp.Claims("mcpgate-test", map[string]any{"exp": time.Now().Add(-5 * time.Minute).Unix()}))
	if _, err := v.Verify(tok); err == nil {
		t.Error("Verify accepted a token outside clock skew")
	}
}

func TestNewVerifierJWKSURL(t *testing.T) {
	idp := testoidc.New(t)
	// Bypass discovery: point straight at the JWKS endpoint.
	v, err := auth.NewVerifier(config.OIDCConfig{
		Issuer:   idp.Issuer(),
		Audience: "mcpgate-test",
		JWKSURL:  idp.Issuer() + "/jwks",
	})
	if err != nil {
		t.Fatalf("NewVerifier with jwks_url: %v", err)
	}
	if _, err := v.Verify(idp.Sign(t, "key-1", idp.Claims("mcpgate-test", nil))); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestNewVerifierFailures(t *testing.T) {
	idp := testoidc.New(t)

	t.Run("unreachable issuer", func(t *testing.T) {
		_, err := auth.NewVerifier(config.OIDCConfig{
			Issuer:   "http://127.0.0.1:1",
			Audience: "mcpgate-test",
		})
		if err == nil {
			t.Error("NewVerifier succeeded against an unreachable issuer")
		}
	})

	t.Run("issuer without jwks_uri", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{})
		}))
		defer srv.Close()
		_, err := auth.NewVerifier(config.OIDCConfig{Issuer: srv.URL, Audience: "x"})
		if err == nil {
			t.Error("NewVerifier succeeded with a discovery doc lacking jwks_uri")
		}
	})

	t.Run("jwks with no usable keys", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{}})
		}))
		defer srv.Close()
		_, err := auth.NewVerifier(config.OIDCConfig{
			Issuer: idp.Issuer(), Audience: "x", JWKSURL: srv.URL,
		})
		if err == nil {
			t.Error("NewVerifier succeeded with an empty JWKS")
		}
	})
}
