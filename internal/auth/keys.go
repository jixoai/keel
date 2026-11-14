// Package auth resolves a request's tenant from an API key.
//
// It is the machine-facing half of the tenancy contract: an AI client presents
// `Authorization: Bearer <key>` or `X-API-Key: <key>`, and the key resolves to
// exactly one tenant plus its role. The browser/passphrase half stays where it
// is (cmd/keel's cookie handler); this package only decides "does this request
// carry a machine credential that names a tenant?" and leaves every other
// request untouched, so a deployment that has no keys behaves exactly as it did
// before.
//
// # What is stored, and what is not
//
// Only a SHA-256 hash of the plaintext key and a display prefix reach the
// database (store.api_keys). The plaintext exists once, in the answer of
// provisioning, and is never recoverable: there is no decrypt path, no "show
// key" endpoint, and nothing in this package logs a key.
//
// # Why the prefix is part of the format
//
// A key always starts with "keel_<role>_". That lets the middleware tell a
// machine credential from a passphrase in O(1) without a database round trip:
// a `Bearer <server passphrase>` request never becomes a key lookup, and a
// deployment with no keys pays nothing for the feature being available.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// Key format constants.
const (
	// KeyScheme prefixes every plaintext key. The trailing underscore of the
	// complete prefix keeps `keel_tenant_x` unambiguous.
	KeyScheme = "keel_"
	// KeyBytes is the entropy of a key. 32 bytes (256 bits) makes an offline
	// guess loop pointless; the hash lookup is the only path.
	KeyBytes = 32
	// KeyPrefixLength is how much of the plaintext is kept for display: the
	// scheme, the role, and eight random characters. Enough to tell two keys
	// apart in a list, useless as a credential (43-8 = 35 characters unknown).
	KeyDisplayChars = 8
)

// Errors. All of them map to 401 with no further detail: a caller must not be
// able to tell "unknown key" from "revoked key" from "wrong tenant format".
var (
	// ErrMissingCredential means the request carried no machine credential and
	// the middleware was configured to require one.
	ErrMissingCredential = errors.New("auth: api key is required")
	// ErrInvalidCredential means the presented credential does not resolve.
	ErrInvalidCredential = errors.New("auth: invalid api key")
)

// Roles a key can carry. They mirror store.APIKeyRole*; this package does not
// import the store so the credential vocabulary cannot drift with persistence.
const (
	RoleTenant = "tenant"
	RoleAdmin  = "admin"
)

// KeyRecord is what a KeyStore resolves a hash to.
type KeyRecord struct {
	TenantID uuid.UUID
	KeyID    uuid.UUID
	Prefix   string
	Role     string
}

// KeyStore is the persistence seam: hash in, live credential out. A missing or
// revoked key must report an error (any error is a 401); the middleware never
// distinguishes the cases.
type KeyStore interface {
	LookupAPIKey(ctx context.Context, hash []byte) (KeyRecord, error)
}

// MintedKey is a freshly generated credential. Plaintext is returned to the
// caller exactly once (provisioning); nothing else in the process keeps it.
type MintedKey struct {
	Plaintext string
	Prefix    string
	Hash      []byte
	Role      string
}

// MintKey generates a new key for role. It fails only when the system random
// source does — there is no weak fallback.
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

// ValidRole reports whether role is a known key role.
func ValidRole(role string) bool { return role == RoleTenant || role == RoleAdmin }

// LooksLikeKey reports whether a credential is in this package's key format.
// It is a pure string test: no allocation beyond a prefix compare, no I/O.
func LooksLikeKey(credential string) bool {
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

// Identity is an authenticated machine caller.
type Identity struct {
	TenantID uuid.UUID
	KeyID    uuid.UUID
	Prefix   string
	Role     string
}

// Admin reports whether the credential may act across tenants.
func (i Identity) Admin() bool { return i.Role == RoleAdmin }

// HeaderTenantID is the header the rest of the stack reads. It is defined here
// because this package is the one that writes it on the authenticated path.
const HeaderTenantID = "X-Tenant-ID"

// Authorizer is the API-key middleware.
//
// Contract:
//   - X-API-Key present: it must be a valid key, otherwise 401. This header is
//     unambiguously a machine credential, so there is no fallthrough.
//   - Authorization: Bearer <credential> where the credential looks like a key
//     (LooksLikeKey): it must resolve, otherwise 401. A malformed or unknown
//     keel_* token is refused here rather than leaking to the passphrase path,
//     which would answer with a different error shape.
//   - Anything else (no credential, a cookie, a passphrase bearer, an
//     unrelated Authorization scheme): passed through untouched, so the
//     existing cookie/passphrase handler keeps deciding.
//
// On success the key's tenant is written to X-Tenant-ID, replacing anything the
// caller sent: a key client never gets to choose its tenant.
type Authorizer struct {
	Keys KeyStore
	// Next handles every request this middleware did not authenticate: no
	// credential, a non-key credential, a cookie session. It is the
	// passphrase/cookie path.
	Next http.Handler
	// Verified receives requests whose credential WAS a valid key. It is
	// normally the bare API mux, not the cookie handler: the cookie handler
	// would refuse a key-only request for lacking a session, which is exactly
	// the deployment shape a machine client has. When nil, Next is used.
	Verified http.Handler
}

// ServeHTTP implements http.Handler.
func (a *Authorizer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	credential, source := machineCredential(r)
	if credential == "" {
		// No machine credential: the passphrase/cookie path still owns this
		// request. This middleware never rejects what it did not authenticate,
		// so a deployment that has no keys behaves exactly as before.
		a.next().ServeHTTP(w, r)
		return
	}
	if a.Keys == nil {
		// No key store: a key-shaped credential cannot be validated, and letting
		// it through would authorize nothing. Fail closed.
		writeUnauthorized(w, source)
		return
	}
	identity, err := a.resolve(r.Context(), credential)
	if err != nil {
		writeUnauthorized(w, source)
		return
	}
	ctx := WithIdentity(r.Context(), identity)
	r = r.WithContext(ctx)
	r.Header.Del(HeaderTenantID)
	r.Header.Set(HeaderTenantID, identity.TenantID.String())
	// The presented credential must not travel further: downstream components
	// (plugin bridges, MCP proxies) must never see a raw key in a header.
	r.Header.Del("Authorization")
	r.Header.Del("X-API-Key")
	next := a.Verified
	if next == nil {
		next = a.next()
	}
	next.ServeHTTP(w, r)
}

func (a *Authorizer) resolve(ctx context.Context, credential string) (Identity, error) {
	record, err := a.Keys.LookupAPIKey(ctx, HashKey(credential))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrInvalidCredential, err)
	}
	if record.TenantID == uuid.Nil || !ValidRole(record.Role) {
		return Identity{}, ErrInvalidCredential
	}
	return Identity{
		TenantID: record.TenantID,
		KeyID:    record.KeyID,
		Prefix:   record.Prefix,
		Role:     record.Role,
	}, nil
}

func (a *Authorizer) next() http.Handler {
	if a.Next == nil {
		return http.NotFoundHandler()
	}
	return a.Next
}

// machineCredential extracts the machine credential, if any. The returned
// source names the header for the WWW-Authenticate answer.
func machineCredential(r *http.Request) (credential, source string) {
	if value := strings.TrimSpace(r.Header.Get("X-API-Key")); value != "" {
		return value, "X-API-Key"
	}
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return "", ""
	}
	scheme, value, ok := strings.Cut(header, " ")
	if !ok {
		return "", ""
	}
	if !strings.EqualFold(scheme, "bearer") {
		return "", ""
	}
	value = strings.TrimSpace(value)
	// Only keel_-shaped bearers are this package's business; a passphrase
	// bearer keeps travelling to the cookie handler (which validates it in
	// constant time). A keel_* token that does not look like a key is still
	// treated as one, so a truncated key is a 401 rather than a passphrase check.
	if !strings.HasPrefix(value, KeyScheme) {
		return "", ""
	}
	return value, "Authorization"
}

// writeUnauthorized answers 401 without disclosing whether the tenant, the key
// or the key's revocation was the problem. The body shape matches the rest of
// the API surface ("error" field).
func writeUnauthorized(w http.ResponseWriter, source string) {
	if source == "X-API-Key" {
		w.Header().Set("WWW-Authenticate", `ApiKey realm="keel"`)
	} else {
		w.Header().Set("WWW-Authenticate", `Bearer realm="keel"`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
}

type identityKey struct{}

// WithIdentity attaches an identity to a context (the middleware does this; it
// is exported so the hosting process can inject one in tests).
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, identity)
}

// IdentityFrom reads the identity the middleware attached.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityKey{}).(Identity)
	return identity, ok
}
