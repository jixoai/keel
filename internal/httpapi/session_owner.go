// Session ownership on the generic session-create path
// (change keel-bot-identity-access, A4.1).
//
// POST /v1/sessions predates Bot ownership and still creates an unowned tenant
// session when the body names no bot. When it names one, the serve layer's
// SessionOwner callback authorizes bot.invoke, derives the owner account from
// the Bot's Profile and writes the ownership columns plus the owner relation.
// The callback returns a status-carrying error so a denial is 403 (with the
// engine's reason), an archived bot is 409, and so on — the shell never guesses
// an authorization status.
package httpapi

import (
	"context"

	"github.com/google/uuid"
)

// SessionOwnerFunc writes a new session's Bot ownership after authorizing the
// caller for the Bot. It must create the session row (or fail); the shell does
// not write an unowned row first, because that would leave a session nobody
// can authorize.
type SessionOwnerFunc func(ctx context.Context, tenantID, sessionID uuid.UUID, botID string) error

// SessionCreatorFunc creates a session with no Bot and attributes it to the
// caller's account: the session row and the account's owner relation are
// written in one transaction. It closes the "create without botId" bypass — the
// old unowned row had no relation, so every single-resource session route would
// deny, but any handler that skipped the relation check would expose it.
type SessionCreatorFunc func(ctx context.Context, tenantID, sessionID uuid.UUID) error

// SessionOwnerError carries the exact HTTP status and machine code of an
// ownership refusal. Reason is the engine's stable denial reason, when present.
type SessionOwnerError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"error"`
	Reason  string `json:"reason,omitempty"`
}

// Error renders the message for logs.
func (e *SessionOwnerError) Error() string { return e.Message }
