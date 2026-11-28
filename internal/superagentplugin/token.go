package superagentplugin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// SecretEnv names the deployment secret the deterministic mirrored claim token
// is derived from. It is never logged, stored in board_links or written to a
// board event: only its HMAC output (and the board's SHA-256 of that output)
// travel.
const SecretEnv = "KEEL_SECRET_KEY"

// ErrSecretMissing is the fail-closed answer when mirroring is requested
// without KEEL_SECRET_KEY. The adapter neither mirrors nor claims in that
// deployment: a predictable token would let any board reader forge the claim.
var ErrSecretMissing = errors.New("superagentplugin: KEEL_SECRET_KEY is required to derive mirrored claim tokens")

// TokenSource derives the deterministic claim token of one mirrored assignment.
// The derivation is stable across restarts by design: the process can rebuild
// the token it never stored, and the board keeps only its hash.
type TokenSource struct {
	secret []byte
}

// NewTokenSource validates the deployment secret. A blank secret fails closed.
func NewTokenSource(secret string) (*TokenSource, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, ErrSecretMissing
	}
	return &TokenSource{secret: []byte(secret)}, nil
}

// ClaimToken returns the mirrored claim token:
//
//	base64url(HMAC-SHA256(secret, "board-claim:v1:<tenant>:<assignment>:<session>"))
//
// It is 32 bytes, so 43 base64url characters, the board's claimed-token
// grammar.
func (t *TokenSource) ClaimToken(tenantID, assignmentID, sessionID uuid.UUID) (string, error) {
	if t == nil || len(t.secret) == 0 {
		return "", ErrSecretMissing
	}
	if tenantID == uuid.Nil || assignmentID == uuid.Nil || sessionID == uuid.Nil {
		return "", fmt.Errorf("superagentplugin: tenant, assignment and session ids are required for a claim token")
	}
	return t.derive("board-claim:v1", tenantID, assignmentID, sessionID)
}

// SettlementToken returns the deterministic token of the service claim the
// mirror takes to project an already terminal Ticket's final state. It is a
// different derivation from an assignment claim so the two can never collide:
//
//	base64url(HMAC-SHA256(secret, "board-settle:v1:<tenant>:<ticket>:<ticket>"))
func (t *TokenSource) SettlementToken(tenantID, ticketID uuid.UUID) (string, error) {
	if t == nil || len(t.secret) == 0 {
		return "", ErrSecretMissing
	}
	if tenantID == uuid.Nil || ticketID == uuid.Nil {
		return "", fmt.Errorf("superagentplugin: tenant and ticket ids are required for a settlement token")
	}
	return t.derive("board-settle:v1", tenantID, ticketID, ticketID)
}

func (t *TokenSource) derive(purpose string, tenantID, left, right uuid.UUID) (string, error) {
	mac := hmac.New(sha256.New, t.secret)
	message := purpose + ":" + tenantID.String() + ":" + left.String() + ":" + right.String()
	if _, err := mac.Write([]byte(message)); err != nil {
		return "", fmt.Errorf("superagentplugin: derive claim token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
