package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sunwen1983/mcpgate/internal/config"
)

// defaultIntrospectionCacheTTL bounds how long a successful
// introspection result is cached when the config does not set one.
const defaultIntrospectionCacheTTL = 60 * time.Second

// introspectionTimeout bounds a single introspection request.
const introspectionTimeout = 10 * time.Second

// Introspection is the parsed result of a successful RFC 7662 token
// introspection: the token is active, and these are the claims the
// gateway uses. Scopes is the space-delimited scope claim split into
// individual scopes; it is parsed here so the per-tool authorization
// roadmap item can consume it without changing this API.
type Introspection struct {
	Subject string
	Scopes  []string
}

// Introspector validates opaque OAuth2 bearer tokens against an
// authorization server's RFC 7662 introspection endpoint.
type Introspector struct {
	url          string
	clientID     string
	clientSecret string
	cacheTTL     time.Duration
	client       *http.Client

	mu    sync.Mutex
	cache map[string]cachedIntrospection // sha256(token) -> result
}

// cachedIntrospection is one cached successful introspection. The raw
// token is never stored — the key is its SHA-256 hex digest.
type cachedIntrospection struct {
	result     Introspection
	validUntil time.Time
}

// NewIntrospector builds an Introspector from config. It performs no
// network I/O: unlike OIDC discovery there is nothing to fetch without
// a token, so a misconfigured or unreachable endpoint surfaces on the
// first introspected request, failing that request closed.
func NewIntrospector(cfg config.OAuthConfig) *Introspector {
	ttl := time.Duration(cfg.CacheTTL)
	if ttl <= 0 {
		ttl = defaultIntrospectionCacheTTL
	}
	return &Introspector{
		url:          cfg.IntrospectionURL,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		cacheTTL:     ttl,
		client:       &http.Client{Timeout: introspectionTimeout},
		cache:        map[string]cachedIntrospection{},
	}
}

// Verify checks an opaque bearer token with the introspection endpoint
// and returns the token's subject and scopes. It fails closed: any
// network error, non-200 response, malformed body, inactive token, or
// missing subject is an error, and the caller must reject the request.
// The returned error is intentionally terse: callers must not leak
// introspection details (or the token) to clients or logs.
func (in *Introspector) Verify(token string) (Introspection, error) {
	errAuth := fmt.Errorf("invalid bearer token")
	key := tokenCacheKey(token)

	in.mu.Lock()
	if cached, ok := in.cache[key]; ok && time.Now().Before(cached.validUntil) {
		result := cached.result
		in.mu.Unlock()
		return result, nil
	}
	in.mu.Unlock()

	result, validUntil, err := in.introspect(token)
	if err != nil {
		return Introspection{}, errAuth
	}
	if time.Until(validUntil) > 0 {
		in.mu.Lock()
		in.cache[key] = cachedIntrospection{result: result, validUntil: validUntil}
		in.mu.Unlock()
	}
	return result, nil
}

// introspect performs one RFC 7662 request and parses the response. The
// token is sent as a form parameter with token_type_hint=access_token,
// authenticated with the gateway's client credentials (HTTP Basic).
func (in *Introspector) introspect(token string) (Introspection, time.Time, error) {
	form := url.Values{
		"token":           {token},
		"token_type_hint": {"access_token"},
	}
	req, err := http.NewRequest(http.MethodPost, in.url, strings.NewReader(form.Encode()))
	if err != nil {
		return Introspection{}, time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(in.clientID, in.clientSecret)

	resp, err := in.client.Do(req)
	if err != nil {
		return Introspection{}, time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Introspection{}, time.Time{}, fmt.Errorf("introspection returned %s", resp.Status)
	}
	var body struct {
		Active bool   `json:"active"`
		Sub    string `json:"sub"`
		Scope  string `json:"scope"`
		Exp    *int64 `json:"exp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Introspection{}, time.Time{}, fmt.Errorf("decode introspection response: %w", err)
	}
	if !body.Active {
		return Introspection{}, time.Time{}, fmt.Errorf("token is not active")
	}
	if body.Sub == "" {
		return Introspection{}, time.Time{}, fmt.Errorf("introspection response has no sub")
	}
	// A cached entry must never outlive the token: the effective TTL
	// is the configured TTL capped by the token's remaining lifetime.
	// A token without exp is cached for the full configured TTL.
	validUntil := time.Now().Add(in.cacheTTL)
	if body.Exp != nil {
		exp := time.Unix(*body.Exp, 0)
		if !time.Now().Before(exp) {
			return Introspection{}, time.Time{}, fmt.Errorf("token is expired")
		}
		if exp.Before(validUntil) {
			validUntil = exp
		}
	}
	result := Introspection{Subject: body.Sub}
	if body.Scope != "" {
		result.Scopes = strings.Fields(body.Scope)
	}
	return result, validUntil, nil
}

// tokenCacheKey derives the cache key for a token without storing the
// token itself.
func tokenCacheKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
