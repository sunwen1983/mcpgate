// Package auth authenticates gateway clients.
//
// Two credential types are supported:
//
//   - API keys: presented as the X-API-Key header or an
//     "Authorization: Bearer <key>" header, compared in constant time
//     against the configured keys.
//   - OIDC JWTs (optional): when an OIDC issuer is configured, a Bearer
//     token that is not an API key is tried as a JWT signed by the
//     issuer. The token's sub claim becomes the client identity
//     ("oidc:<sub>") used in logs, metrics, and rate limiting.
//
// Key values and token contents are never logged; callers log only the
// identity name.
package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/sunwen1983/mcpgate/internal/config"
)

// defaultClockSkew is the tolerated clock difference for exp/nbf checks
// when the config does not set one.
const defaultClockSkew = 60 * time.Second

// jwksRefreshInterval bounds how often the JWKS document is re-fetched in
// the background of normal verification. Unknown kids trigger an immediate
// refresh regardless of this interval, so key rotation is picked up fast.
const jwksRefreshInterval = 10 * time.Minute

// discoveryTimeout bounds OIDC discovery and JWKS fetches.
const discoveryTimeout = 10 * time.Second

// validMethods is the allowlist of JWS signing algorithms. Only asymmetric
// algorithms are accepted: "none" and every HMAC algorithm are rejected,
// so a token can never validate against a symmetric secret.
var validMethods = []string{
	"RS256", "RS384", "RS512",
	"ES256", "ES384", "ES512",
	"PS256", "PS384", "PS512",
}

// Verifier validates OIDC JWT bearer tokens against an issuer's JWKS.
type Verifier struct {
	issuer   string
	audience string
	jwksURL  string
	skew     time.Duration
	client   *http.Client

	mu      sync.RWMutex
	keys    map[string]crypto.PublicKey // kid -> public key
	fetched time.Time
}

// NewVerifier builds a Verifier from config. It resolves the JWKS endpoint
// (via OIDC discovery unless jwks_url is set) and fetches the key set
// once, so a misconfigured or unreachable issuer fails fast at startup.
func NewVerifier(cfg config.OIDCConfig) (*Verifier, error) {
	jwksURL := cfg.JWKSURL
	if jwksURL == "" {
		var err error
		jwksURL, err = discoverJWKS(cfg.Issuer)
		if err != nil {
			return nil, fmt.Errorf("oidc discovery: %w", err)
		}
	}
	skew := time.Duration(cfg.ClockSkew)
	if skew <= 0 {
		skew = defaultClockSkew
	}
	v := &Verifier{
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		jwksURL:  jwksURL,
		skew:     skew,
		client:   &http.Client{Timeout: discoveryTimeout},
		keys:     map[string]crypto.PublicKey{},
	}
	if err := v.refresh(); err != nil {
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	return v, nil
}

// Verify checks a JWT bearer token and returns the subject (sub claim) on
// success. It verifies the signature against the issuer's JWKS and checks
// the iss, aud, exp, and nbf claims. The returned error is intentionally
// terse: callers must not leak verification details to clients.
func (v *Verifier) Verify(tokenString string) (string, error) {
	errAuth := errors.New("invalid bearer token")
	parser := jwt.NewParser(
		jwt.WithValidMethods(validMethods),
		jwt.WithLeeway(v.skew),
		jwt.WithExpirationRequired(),
	)
	token, err := parser.ParseWithClaims(tokenString, jwt.MapClaims{}, v.keyFunc())
	if err != nil {
		return "", errAuth
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return "", errAuth
	}
	// iss must match the configured issuer exactly; aud must contain the
	// configured audience (it may be a string or a list).
	iss, err := claims.GetIssuer()
	if err != nil || iss != v.issuer {
		return "", errAuth
	}
	audienceOK := false
	if aud, err := claims.GetAudience(); err == nil {
		for _, a := range aud {
			if a == v.audience {
				audienceOK = true
				break
			}
		}
	}
	if !audienceOK {
		return "", errAuth
	}
	sub, err := claims.GetSubject()
	if err != nil || sub == "" {
		return "", errAuth
	}
	return sub, nil
}

// keyFunc returns a jwt.Keyfunc resolving the token's kid to a public key.
// A stale key set or an unknown kid triggers one JWKS refresh before
// giving up, so key rotation is picked up without waiting for the
// periodic refresh. If the refresh itself fails, a previously cached key
// for the kid is still honored: verification fails closed only when no
// key is known at all.
func (v *Verifier) keyFunc() jwt.Keyfunc {
	return func(token *jwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		if kid == "" {
			return nil, fmt.Errorf("missing kid")
		}
		v.mu.RLock()
		key, ok := v.keys[kid]
		stale := time.Since(v.fetched) > jwksRefreshInterval
		v.mu.RUnlock()
		if ok && !stale {
			return key, nil
		}
		if err := v.refresh(); err != nil {
			if ok {
				return key, nil // refresh failed; trust the cached key
			}
			return nil, err
		}
		v.mu.RLock()
		key, ok = v.keys[kid]
		v.mu.RUnlock()
		if !ok {
			return nil, fmt.Errorf("unknown kid %q", kid)
		}
		return key, nil
	}
}

// refresh re-fetches the JWKS document and replaces the key set.
func (v *Verifier) refresh() error {
	keys, err := fetchJWKS(v.client, v.jwksURL)
	if err != nil {
		return err
	}
	v.mu.Lock()
	v.keys = keys
	v.fetched = time.Now()
	v.mu.Unlock()
	return nil
}

// discoverJWKS fetches the OIDC discovery document and returns its
// jwks_uri.
func discoverJWKS(issuer string) (string, error) {
	u := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	client := &http.Client{Timeout: discoveryTimeout}
	resp, err := client.Get(u)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discovery returned %s", resp.Status)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", fmt.Errorf("decode discovery document: %w", err)
	}
	if doc.JWKSURI == "" {
		return "", fmt.Errorf("discovery document has no jwks_uri")
	}
	return doc.JWKSURI, nil
}

// fetchJWKS downloads a JWKS document and extracts RSA/EC public keys.
// Keys of any other type are skipped.
func fetchJWKS(client *http.Client, url string) (map[string]crypto.PublicKey, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks fetch returned %s", resp.Status)
	}
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode jwks: %w", err)
	}
	keys := make(map[string]crypto.PublicKey, len(doc.Keys))
	for _, raw := range doc.Keys {
		var meta struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil || meta.Kid == "" {
			continue
		}
		var key crypto.PublicKey
		switch meta.Kty {
		case "RSA":
			key, err = parseRSAKey(raw)
		case "EC":
			key, err = parseECKey(raw)
		default:
			continue // symmetric ("oct") and unknown key types are unusable
		}
		if err != nil {
			continue // skip malformed keys rather than failing the whole set
		}
		keys[meta.Kid] = key
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("jwks document contains no usable RSA/EC keys")
	}
	return keys, nil
}

// parseRSAKey builds an *rsa.PublicKey from a JWK's n/e parameters.
func parseRSAKey(raw json.RawMessage) (*rsa.PublicKey, error) {
	var jwk struct {
		N string `json:"n"`
		E string `json:"e"`
	}
	if err := json.Unmarshal(raw, &jwk); err != nil {
		return nil, err
	}
	nBytes, err := base64.RawURLEncoding.DecodeString(jwk.N)
	if err != nil {
		return nil, fmt.Errorf("bad n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(jwk.E)
	if err != nil {
		return nil, fmt.Errorf("bad e: %w", err)
	}
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() {
		return nil, fmt.Errorf("exponent out of range")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: int(e.Int64())}, nil
}

// parseECKey builds an *ecdsa.PublicKey from a JWK's crv/x/y parameters.
func parseECKey(raw json.RawMessage) (*ecdsa.PublicKey, error) {
	var jwk struct {
		Crv string `json:"crv"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}
	if err := json.Unmarshal(raw, &jwk); err != nil {
		return nil, err
	}
	var curve elliptic.Curve
	switch jwk.Crv {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	case "P-521":
		curve = elliptic.P521()
	default:
		return nil, fmt.Errorf("unsupported curve %q", jwk.Crv)
	}
	x, err := jwkCoord(jwk.X)
	if err != nil {
		return nil, fmt.Errorf("bad x: %w", err)
	}
	y, err := jwkCoord(jwk.Y)
	if err != nil {
		return nil, fmt.Errorf("bad y: %w", err)
	}
	if !curve.IsOnCurve(x, y) {
		return nil, fmt.Errorf("point not on curve")
	}
	return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
}

// jwkCoord decodes one base64url JWK coordinate.
func jwkCoord(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(b), nil
}
