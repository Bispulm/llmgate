// Package auth identifies the caller of a request. Team identity only ever comes
// from here, never from a request header or body.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// ErrInvalidCredential means the credential is missing or unknown.
var ErrInvalidCredential = errors.New("auth: invalid credential")

// Principal is an authenticated caller.
type Principal struct {
	TeamID string
	KeyID  string
	Limits TeamLimits
}

// TeamLimits are a team's limits that aren't about money. Zero means no limit.
type TeamLimits struct {
	RequestsPerMinute int64 `json:"requests_per_minute"`
	TokensPerMinute   int64 `json:"tokens_per_minute"`
	MaxInFlight       int64 `json:"max_in_flight"`
}

// Authenticator resolves a bearer token to its principal.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (Principal, error)
}

// Identifiers for the development principal.
const (
	devTeamID = "dev"
	devKeyID  = "dev"
)

// DevKey accepts a single shared key, for local development only. Configuration
// refuses to pair it with a listener other than loopback. Tenant keys replace it
// in a later milestone.
type DevKey struct {
	digest [sha256.Size]byte
}

// NewDevKey returns an authenticator for the given key.
func NewDevKey(key string) *DevKey {
	return &DevKey{digest: sha256.Sum256([]byte(key))}
}

// Authenticate implements Authenticator. Comparing fixed-size digests keeps the
// comparison's timing independent of the key's length and contents.
func (d *DevKey) Authenticate(_ context.Context, token string) (Principal, error) {
	got := sha256.Sum256([]byte(token))
	if token == "" || subtle.ConstantTimeCompare(got[:], d.digest[:]) != 1 {
		return Principal{}, ErrInvalidCredential
	}
	return Principal{TeamID: devTeamID, KeyID: devKeyID}, nil
}

// BearerToken extracts the token from an Authorization header, or returns "" if
// there isn't exactly one well-formed token.
func BearerToken(header http.Header) string {
	scheme, token, ok := strings.Cut(header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.ContainsRune(token, ' ') {
		return ""
	}
	return token
}
