// Package auth resolves and mints credentials.
//
// This file is the retired tenant api_keys surface: `keel admin tenant/key` and
// the operator provisioning API still mint `keel_tenant_` / `keel_admin_` keys
// for pre-A2 tooling, but the serving path no longer accepts them (see
// cmd/keel/auth.go, which rejects every non-bot `keel_*` bearer and looks only at
// bot_api_keys). Browser login is the password/session service in service.go;
// machine login is the bot key in botkeys.go.
//
// Only a SHA-256 hash of a minted key and a display prefix ever reach the
// database. The plaintext exists once, in the answer of provisioning, and is
// never recoverable: there is no decrypt path, no "show key" endpoint, and
// nothing in this package logs a key.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Key format constants.
const (
	// KeyScheme prefixes every legacy plaintext key. The trailing underscore of
	// the complete prefix keeps `keel_tenant_x` unambiguous.
	KeyScheme = "keel_"
	// KeyBytes is the entropy of a key. 32 bytes (256 bits) makes an offline
	// guess loop impossible; the hash lookup is the only path.
	KeyBytes = 32
	// KeyDisplayChars is how much of the plaintext is kept for display: the
	// scheme, the role, and eight random characters.
	KeyDisplayChars = 8
)

// Legacy roles a tenant api_key can carry. They mirror store.APIKeyRole*; this
// package does not import the store so the credential vocabulary cannot drift
// with persistence.
const (
	RoleTenant = "tenant"
	RoleAdmin  = "admin"
)

// MintedKey is a freshly generated credential. Plaintext is returned to the
// caller exactly once (provisioning); nothing else in the process keeps it.
type MintedKey struct {
	Plaintext string
	Prefix    string
	Hash      []byte
	Role      string
}

// MintKey generates a new legacy key for role. It fails only when the system
// random source does — there is no weak fallback.
func MintKey(role string) (MintedKey, error) {
	if role == "" {
		role = RoleTenant
	}
	if !ValidRole(role) {
		return MintedKey{}, fmt.Errorf("auth: unknown key role %q", role)
	}
	buf := make([]byte, KeyBytes)
	if _, err := rand.Read(buf); err != nil {
		return MintedKey{}, fmt.Errorf("auth: generate key: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	plaintext := KeyScheme + role + "_" + token
	prefixLen := len(KeyScheme) + len(role) + 1 + KeyDisplayChars
	if prefixLen > len(plaintext) {
		prefixLen = len(plaintext)
	}
	return MintedKey{
		Plaintext: plaintext,
		Prefix:    plaintext[:prefixLen],
		Hash:      HashKey(plaintext),
		Role:      role,
	}, nil
}

// HashKey is the lookup identity of a plaintext key.
func HashKey(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// ValidRole reports whether role is a known legacy key role.
func ValidRole(role string) bool { return role == RoleTenant || role == RoleAdmin }

// LooksLikeKey reports whether a credential is in the legacy key format. It is
// a pure string test: no allocation beyond a prefix compare, no I/O. The
// serving middleware uses it to reject retired `keel_tenant_`/`keel_admin_`
// bearers explicitly instead of letting them reach a handler.
func LooksLikeKey(credential string) bool {
	credential = strings.TrimSpace(credential)
	if !strings.HasPrefix(credential, KeyScheme) {
		return false
	}
	rest := credential[len(KeyScheme):]
	role, token, ok := strings.Cut(rest, "_")
	if !ok || token == "" {
		return false
	}
	return ValidRole(role)
}

// Identity is an operator identity attached to a request context by the legacy
// provisioning surface. It is not produced by the serving auth middleware any
// more; agentapi still reads it for `whoami` and its own tests.
type Identity struct {
	TenantID uuid.UUID
	KeyID    uuid.UUID
	Prefix   string
	Role     string
}

// Admin reports whether the credential may act across tenants.
func (i Identity) Admin() bool { return i.Role == RoleAdmin }

type identityKey struct{}

// WithIdentity attaches an identity to a context.
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, identity)
}

// IdentityFrom reads the identity a caller attached.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityKey{}).(Identity)
	return identity, ok
}
