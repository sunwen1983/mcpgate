package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/sunwen1983/mcpgate/internal/config"
)

// Identity is what a verified bearer token proves about its holder:
// the subject and the scopes the token carries. Scopes are the
// currency of per-tool authorization (see config.ToolScopes).
type Identity struct {
	Subject string
	Scopes  []string
}

// Authenticator checks client credentials against the configured keys,
// falling back to OIDC JWT verification and OAuth2 token introspection
// when configured.
type Authenticator struct {
	keys  []config.APIKey
	oidc  *Verifier     // nil when OIDC client auth is not configured
	oauth *Introspector // nil when OAuth2 introspection is not configured
}

// New builds an Authenticator from the configured API keys. oidc and
// oauth may be nil, in which case API keys are the only accepted
// credentials.
func New(keys []config.APIKey, oidc *Verifier, oauth *Introspector) *Authenticator {
	return &Authenticator{keys: keys, oidc: oidc, oauth: oauth}
}

// Authenticate extracts the client credential and returns the matching
// identity. API keys come from the X-API-Key header or a Bearer token and
// are compared in constant time. When OIDC is configured, a Bearer token
// that matches no API key is tried as a JWT; on success the identity is
// "oidc:<sub>" carrying the token's scopes. When OAuth2 introspection is
// configured, a Bearer token that is neither an API key nor a valid JWT
// is introspected; on success the identity is "oauth:<sub>" carrying the
// token's scopes. An API key identity carries the scopes configured for
// that key. The second return value is false when no credential was
// presented or none verified.
func (a *Authenticator) Authenticate(r *http.Request) (config.APIKey, bool) {
	var bearer string
	key := r.Header.Get("X-API-Key")
	if key == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			bearer = strings.TrimPrefix(auth, "Bearer ")
			key = bearer
		}
	}
	if key != "" {
		for _, k := range a.keys {
			if subtle.ConstantTimeCompare([]byte(k.Key), []byte(key)) == 1 {
				return k, true
			}
		}
	}
	// OIDC fallback: only Bearer tokens are tried as JWTs. API keys are
	// opaque secrets; X-API-Key values are never interpreted as tokens.
	if a.oidc != nil && bearer != "" {
		if ident, err := a.oidc.Verify(bearer); err == nil {
			return config.APIKey{Name: "oidc:" + ident.Subject, Scopes: ident.Scopes}, true
		}
	}
	// OAuth2 introspection fallback: only Bearer tokens are introspected,
	// after the JWT check, so a valid JWT never costs an introspection
	// round-trip. X-API-Key values are never introspected either.
	if a.oauth != nil && bearer != "" {
		if result, err := a.oauth.Verify(bearer); err == nil {
			return config.APIKey{Name: "oauth:" + result.Subject, Scopes: result.Scopes}, true
		}
	}
	return config.APIKey{}, false
}
