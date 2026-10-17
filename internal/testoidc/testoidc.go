// Package testoidc is a minimal OpenID provider for tests: it serves an
// OIDC discovery document and a JWKS endpoint backed by generated RSA
// keys, and can sign test tokens. Test-only; never imported by
// non-test code.
package testoidc

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Provider is a test OIDC issuer.
type Provider struct {
	srv    *httptest.Server
	issuer string

	mu   sync.Mutex
	keys map[string]*rsa.PrivateKey // kid -> private key
}

// New starts a Provider serving discovery + JWKS with one RSA key
// ("key-1").
func New(t *testing.T) *Provider {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{keys: map[string]*rsa.PrivateKey{"key-1": priv}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": p.srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		keys := []any{}
		for kid, priv := range p.keys {
			pub := &priv.PublicKey
			keys = append(keys, map[string]string{
				"kty": "RSA",
				"kid": kid,
				"use": "sig",
				"alg": "RS256",
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	})
	p.srv = httptest.NewServer(mux)
	p.issuer = p.srv.URL
	t.Cleanup(p.srv.Close)
	return p
}

// Issuer returns the provider's issuer URL.
func (p *Provider) Issuer() string { return p.issuer }

// Rotate replaces the served key set with a single fresh key and returns
// its kid ("key-2").
func (p *Provider) Rotate(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.keys = map[string]*rsa.PrivateKey{"key-2": priv}
	p.mu.Unlock()
	return "key-2"
}

// Claims returns a valid claim set for audience aud, with overrides
// applied (a nil override deletes the claim).
func (p *Provider) Claims(aud string, overrides map[string]any) jwt.MapClaims {
	c := jwt.MapClaims{
		"iss": p.issuer,
		"aud": aud,
		"sub": "user-123",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	for k, v := range overrides {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

// PrivateKey returns the test private key for kid.
func (p *Provider) PrivateKey(t *testing.T, kid string) *rsa.PrivateKey {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	priv, ok := p.keys[kid]
	if !ok {
		t.Fatalf("no test key %q", kid)
	}
	return priv
}

// Sign issues an RS256 token for kid with the given claims.
func (p *Provider) Sign(t *testing.T, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(p.PrivateKey(t, kid))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
