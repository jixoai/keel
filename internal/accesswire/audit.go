// Audit writes for the access surface (change keel-bot-identity-access, A4.1).
//
// Every grant, revoke, denial, super-admin action, relation change, invocation
// and bot-key lifecycle write lands in auth_events — the same append-only
// stream A2 writes login facts to. Grant/invoke/relation/bot-key/super-admin
// writes return the append error: a business write whose audit row could not be
// recorded is reported to the caller (500), never hidden behind a log warning
// (A4.2a §7). Denials stay best effort: a refused request must not become a 500
// because the audit stream is down (the failure is logged). Detail maps carry
// identifiers, keys, scopes and reasons, never a credential or a secret value.
package accesswire

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/store"
)

// Auditor appends access facts to the auth audit stream. A nil *Auditor is a
// no-op, so a deployment/test without one keeps working.
type Auditor struct {
	Store  *store.Store
	Logger *slog.Logger
}

// NewAuditor builds the writer.
func NewAuditor(st *store.Store, log *slog.Logger) *Auditor {
	if log == nil {
		log = slog.Default()
	}
	return &Auditor{Store: st, Logger: log}
}

// record writes one audit row. A configured auditor that cannot write returns
// the error: callers decide whether the business write stands (it already
// committed) but never report an unaudited success. A nil auditor is a no-op
// (a deployment/test without one).
func (a *Auditor) record(ctx context.Context, event store.AuthEvent) error {
	if a == nil || a.Store == nil {
		return nil
	}
	_, err := a.Store.RecordAuthEvent(ctx, event)
	return err
}

// Denied records one authorization denial with its stable reason. The detail is
// what a security review needs and what a 403 body must not reveal: the key,
// the reason, the resource type/handle, and the acting bot if any.
func (a *Auditor) Denied(ctx context.Context, principal access.Principal, decision access.Decision, resource access.Resource) {
	if a == nil {
		return
	}
	detail := map[string]any{
		"reason":     decision.Reason.String(),
		"key":        decision.Key,
		"tenant_id":  principal.TenantID.String(),
		"account_id": principal.AccountID.String(),
	}
	if decision.Scope != "" {
		detail["scope"] = string(decision.Scope)
	}
	if resource.Type != "" {
		detail["resource_type"] = resource.Type
	}
	if resource.ID != "" {
		detail["resource_id"] = resource.ID
	}
	if principal.BotID != "" {
		detail["bot_id"] = principal.BotID
	}
	if decision.Detail != "" {
		detail["detail"] = decision.Detail
	}
	// Denials are best effort: a refused request must not become a 500 because
	// the audit stream is unavailable, and the failure is still logged.
	if err := a.record(ctx, store.AuthEvent{
		TenantID: principal.TenantID, AccountID: uuidPtr(principal.AccountID),
		ActorAccountID: uuidPtr(principal.AccountID),
		Kind:           store.AuthEventPermissionDenied, Result: store.AuthResultDenied,
		Detail: detail,
	}); err != nil && a.Logger != nil {
		a.Logger.Warn("access denial audit write failed", "kind", store.AuthEventPermissionDenied, "err", err)
	}
}

// Grant records a grant or revoke write. action is "grant" or "revoke"; target
// names the account or group, via names the storage surface.
func (a *Auditor) Grant(ctx context.Context, tenantID, actor, target uuid.UUID, action, via, key, scope, result string, extra map[string]any) error {
	if a == nil {
		return nil
	}
	detail := map[string]any{
		"action": action, "via": via, "key": key, "scope": scope,
		"tenant_id": tenantID.String(),
	}
	if target != uuid.Nil {
		// A membership grant targets an account; a group-grant write targets a
		// group and leaves account_id NULL (the group id rides in the extra map).
		detail["target_id"] = target.String()
	}
	if actor != uuid.Nil {
		detail["actor_account_id"] = actor.String()
	}
	for name, value := range extra {
		detail[name] = value
	}
	kind := store.AuthEventPermissionGrant
	if action == "revoke" {
		kind = store.AuthEventPermissionRevoke
	}
	if result == "" {
		result = store.AuthResultOK
	}
	return a.record(ctx, store.AuthEvent{
		TenantID: tenantID, AccountID: uuidPtr(target), ActorAccountID: uuidPtr(actor),
		Kind: kind, Result: result, Detail: detail,
	})
}

// Relation records a resource-relation write or delete.
func (a *Auditor) Relation(ctx context.Context, tenantID, actor uuid.UUID, action, resourceType, resourceID, relation, subjectType, subjectID string) error {
	if a == nil {
		return nil
	}
	return a.record(ctx, store.AuthEvent{
		TenantID: tenantID, ActorAccountID: uuidPtr(actor),
		Kind: store.AuthEventRelationChanged,
		Detail: map[string]any{
			"action": action, "resource_type": resourceType, "resource_id": resourceID,
			"relation": relation, "subject_type": subjectType, "subject_id": subjectID,
		},
	})
}

// SuperAdmin records a super-admin action. The caller passes the action name
// and non-sensitive detail; the row always marks the actor.
func (a *Auditor) SuperAdmin(ctx context.Context, principal access.Principal, action string, detail map[string]any) error {
	if a == nil || !principal.IsSuperAdmin {
		return nil
	}
	merged := map[string]any{"action": action, "super_admin": true, "tenant_id": principal.TenantID.String()}
	for name, value := range detail {
		merged[name] = value
	}
	return a.record(ctx, store.AuthEvent{
		TenantID: principal.TenantID, AccountID: uuidPtr(principal.AccountID),
		ActorAccountID: uuidPtr(principal.AccountID),
		Kind:           store.AuthEventSuperAdmin, Detail: merged,
	})
}

// BotKey records a bot credential issue or revoke. The plaintext and hash are
// never part of the row; the prefix is the display fragment.
func (a *Auditor) BotKey(ctx context.Context, tenantID, actor, accountID uuid.UUID, botID, action, keyID, prefix, label string) error {
	if a == nil {
		return nil
	}
	kind := store.AuthEventBotKeyIssued
	if action == "revoke" {
		kind = store.AuthEventBotKeyRevoked
	}
	return a.record(ctx, store.AuthEvent{
		TenantID: tenantID, AccountID: uuidPtr(accountID), ActorAccountID: uuidPtr(actor),
		Kind: kind,
		Detail: map[string]any{
			"key_id": keyID, "key_prefix": prefix, "bot_id": botID,
			"label": label, "action": action,
		},
	})
}

// Invoke records one bot invocation: caller, callee, session and result. The
// spec treats bot.invoke as a high-risk capability, so every accepted (and
// refused) call is traceable.
func (a *Auditor) Invoke(ctx context.Context, tenantID uuid.UUID, caller access.Principal, botID string, sessionID uuid.UUID, result string) error {
	if a == nil {
		return nil
	}
	detail := map[string]any{
		"bot_id": botID, "caller_account_id": caller.AccountID.String(),
		"result": result,
	}
	if caller.BotID != "" {
		detail["caller_bot_id"] = caller.BotID
	}
	if sessionID != uuid.Nil {
		detail["session_id"] = sessionID.String()
	}
	return a.record(ctx, store.AuthEvent{
		TenantID: tenantID, AccountID: uuidPtr(caller.AccountID),
		ActorAccountID: uuidPtr(caller.AccountID),
		Kind:           store.AuthEventBotInvoked, Detail: detail,
	})
}

func uuidPtr(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	copied := id
	return &copied
}
