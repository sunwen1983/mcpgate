package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/sunwen1983/mcpgate/internal/config"
)

// Authenticator checks client credentials against the configured keys,
// falling back to OIDC JWT verification when configured.
type Authenticator struct {
	keys []config.APIKey
	oidc *Verifier // nil when OIDC client auth is not configured
}

// New builds an Authenticator from the configured API keys. oidc may be
// nil, in which case API keys are the only accepted credentials.
func New(keys []config.APIKey, oidc *Verifier) *Authenticator {
	return &Authenticator{keys: keys, oidc: oidc}
}

// Authenticate extracts the client credential and returns the matching
// identity. API keys come from the X-API-Key header or a Bearer token and
// are compared in constant time. When OIDC is configured, a Bearer token
// that matches no API key is tried as a JWT; on success the identity is
// "oidc:<sub>". The second return value is false when no credential was
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
		if sub, err := a.oidc.Verify(bearer); err == nil {
			return config.APIKey{Name: "oidc:" + sub}, true
		}
	}
	return config.APIKey{}, false
}
