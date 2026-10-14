// Package auth authenticates gateway clients by API key.
//
// Credentials are compared in constant time. The key value itself is never
// logged; callers should log only the key's Name.
package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/sunwen1983/mcpgate/internal/config"
)

// Authenticator checks client credentials against the configured keys.
type Authenticator struct {
	keys []config.APIKey
}

// New builds an Authenticator from the configured API keys.
func New(keys []config.APIKey) *Authenticator {
	return &Authenticator{keys: keys}
}

// Authenticate extracts the API key from the X-API-Key header or a Bearer
// token and returns the matching key record. The second return value is
// false when no credential was presented or none matched.
func (a *Authenticator) Authenticate(r *http.Request) (config.APIKey, bool) {
	key := r.Header.Get("X-API-Key")
	if key == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			key = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	if key == "" {
		return config.APIKey{}, false
	}
	for _, k := range a.keys {
		if subtle.ConstantTimeCompare([]byte(k.Key), []byte(key)) == 1 {
			return k, true
		}
	}
	return config.APIKey{}, false
}
