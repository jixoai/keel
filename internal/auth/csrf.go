// CSRF double-submit and the signed session cookie (A2).
//
// The CSRF token is not a random cookie value stored server-side: it is
// HMAC(key, subject || nonce), where the subject is the session id (or
// "anonymous" before login). The cookie carries the token and the client echoes
// it in X-Keel-CSRF. A cross-site request can make the browser send the cookie
// but cannot read it, so it cannot produce the header; a subdomain that can set
// a cookie cannot forge the signature.
//
// The session cookie likewise carries only the session id plus an HMAC. The
// signing key is derived from KEEL_SECRET_KEY, never from a password, so an old
// passphrase cookie fails the format/signature check and a revoked session is
// rejected by the server-side row even if the signature still verifies.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const (
	// SessionCookieName is the browser session cookie.
	SessionCookieName = "keel_session"
	// CSRFCookieName is the double-submit token cookie (readable by JS).
	CSRFCookieName = "keel_csrf"
	// CSRFHeaderName is the required header on writes.
	CSRFHeaderName = "X-Keel-CSRF"
	// AnonymousCSRFSubject is the subject before a session exists (login,
	// bootstrap and the unauthenticated session probe).
	AnonymousCSRFSubject = "anonymous"
)

// ErrBadCookie means a session cookie is absent, malformed, tampered with or
// from an older format. Every case answers 401, exactly like an expired session.
var ErrBadCookie = errors.New("auth: invalid session cookie")

// SessionCookie signs and verifies the session-id cookie.
type SessionCookie struct {
	key []byte
}

// NewSessionCookie derives the cookie key from the deployment secret. The
// secret must already be a 32-byte key (store.SecretKeyFromEnv/ParseSecretKey
// enforces that); this constructor does not accept a passphrase.
func NewSessionCookie(secret []byte) (*SessionCookie, error) {
	if len(secret) != 32 {
		return nil, fmt.Errorf("auth: cookie key must be 32 bytes, got %d", len(secret))
	}
	return &SessionCookie{key: deriveKey(secret, "keel-session-cookie-v1")}, nil
}

// Sign returns "v1.<base64url(session uuid)>.<base64url(hmac)>".
func (c *SessionCookie) Sign(sessionID uuid.UUID) string {
	payload := sessionID[:]
	return "v1." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(c.mac(payload))
}

// Verify parses and authenticates a cookie value. It never returns a partial
// result: any error means "not a live session".
func (c *SessionCookie) Verify(value string) (uuid.UUID, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return uuid.Nil, ErrBadCookie
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) != 16 {
		return uuid.Nil, ErrBadCookie
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return uuid.Nil, ErrBadCookie
	}
	if subtle.ConstantTimeCompare(signature, c.mac(payload)) != 1 {
		return uuid.Nil, ErrBadCookie
	}
	sessionID, err := uuid.FromBytes(payload)
	if err != nil || sessionID == uuid.Nil {
		return uuid.Nil, ErrBadCookie
	}
	return sessionID, nil
}

func (c *SessionCookie) mac(payload []byte) []byte {
	mac := hmac.New(sha256.New, c.key)
	mac.Write(payload)
	return mac.Sum(nil)
}

// CSRF issues and verifies double-submit tokens bound to a subject.
type CSRF struct {
	key []byte
}

// NewCSRF derives the CSRF key from the same deployment secret. It is a
// different subkey from the cookie so a token cannot be replayed as a cookie.
func NewCSRF(secret []byte) (*CSRF, error) {
	if len(secret) != 32 {
		return nil, fmt.Errorf("auth: csrf key must be 32 bytes, got %d", len(secret))
	}
	return &CSRF{key: deriveKey(secret, "keel-csrf-v1")}, nil
}

// Issue returns a fresh token for subject. The nonce makes every issue unique,
// so a rotated session never keeps an old token valid.
func (c *CSRF) Issue(subject string) (string, error) {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("auth: generate csrf nonce: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(nonce)
	return body + "." + base64.RawURLEncoding.EncodeToString(c.mac(subject, body)), nil
}

// Verify checks that token (the header) was issued for subject and equals the
// cookie value. The caller compares header and cookie in constant time as well.
func (c *CSRF) Verify(subject, token string) bool {
	body, signature, ok := strings.Cut(token, ".")
	if !ok || body == "" || signature == "" {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(decoded, c.mac(subject, body)) == 1
}

// Equal compares a header token to a cookie token in constant time.
func Equal(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func (c *CSRF) mac(subject, body string) []byte {
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(subject))
	mac.Write([]byte{0})
	mac.Write([]byte(body))
	return mac.Sum(nil)
}

// deriveKey is HMAC-SHA256(secret, label): 32 bytes of independent key material
// per purpose.
func deriveKey(secret []byte, label string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(label))
	return mac.Sum(nil)
}
