// Bot-owned session creation on POST /v1/sessions
// (change keel-bot-identity-access, A4.1).
//
// The generic create route keeps its pre-A4 meaning when no botId is named. A
// botId turns it into a capability-authorized create: the caller needs
// bot.invoke on that Bot, the Bot must be live, and the new session belongs to
// the Bot's Account with the caller recorded as created_by.
package main

import (
	"context"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/httpapi"
	"github.com/jixoai/keel/internal/store"
)

// newSessionOwner returns the serve layer's ownership callback. The caller's
// identity comes from the auth context the edge resolved, never from a header.
func newSessionOwner(st *store.Store, bridge *accesswire.Bridge) httpapi.SessionOwnerFunc {
	return func(ctx context.Context, tenantID, sessionID uuid.UUID, botID string) error {
		if st == nil || bridge == nil {
			return &httpapi.SessionOwnerError{
				Status: 503, Code: "not_wired", Message: "session ownership is not wired in this deployment",
			}
		}
		principal, err := bridge.Principal(ctx, tenantID)
		if err != nil {
			return &httpapi.SessionOwnerError{
				Status: 401, Code: "invalid_session", Message: "authentication required",
			}
		}
		decision, err := bridge.Authorize(ctx, principal, access.KeyBotInvoke, access.Resource{
			TenantID: tenantID, Type: "bot", ID: botID,
		})
		if err != nil {
			return err
		}
		if decision.Denied() {
			return &httpapi.SessionOwnerError{
				Status: 403, Code: "forbidden", Message: "forbidden",
				Reason: decision.Reason.String(),
			}
		}
		owner, archived, err := bridge.BotOwnerAccount(ctx, tenantID, botID)
		if err != nil {
			// A missing Bot is store.ErrNotFound; the HTTP shell maps it to 404.
			return err
		}
		if archived {
			return &httpapi.SessionOwnerError{
				Status: 409, Code: "bot_archived", Message: "an archived bot refuses new sessions",
			}
		}
		return st.CreateOwnedSession(ctx, store.SessionOwnership{
			TenantID: tenantID, SessionID: sessionID, BotID: botID,
			AccountID: owner, CreatedByAccountID: principal.AccountID,
		})
	}
}

// newAccountSessionOwner returns the pre-A4 create path's callback: a session
// with no botId is still created, but it is attributed to the caller and gets
// an owner relation in the same transaction. The caller's identity comes from
// the auth context, never from a header.
func newAccountSessionOwner(st *store.Store, bridge *accesswire.Bridge) httpapi.SessionCreatorFunc {
	return func(ctx context.Context, tenantID, sessionID uuid.UUID) error {
		if st == nil || bridge == nil {
			return &httpapi.SessionOwnerError{
				Status: 503, Code: "not_wired", Message: "session ownership is not wired in this deployment",
			}
		}
		principal, err := bridge.Principal(ctx, tenantID)
		if err != nil {
			return &httpapi.SessionOwnerError{
				Status: 401, Code: "invalid_session", Message: "authentication required",
			}
		}
		return st.CreateAccountOwnedSession(ctx, tenantID, sessionID, principal.AccountID)
	}
}
