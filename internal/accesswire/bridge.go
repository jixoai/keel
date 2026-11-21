// Bridge: the product layer's single authorization entry point
// (change keel-bot-identity-access, A4.1).
//
// The pure engine (internal/access) decides; this bridge supplies the identity
// the serving edge resolved, loads the Bot Harness narrowing for bot keys,
// records denials, and exposes the two checks a handler needs: Authorize for
// one resource and Visible for a bounded list page.
package accesswire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/store"
)

// ErrNoPrincipal marks a request with no edge-resolved identity, or an identity
// that no longer matches the store (a vanished account, an archived bot). The
// HTTP layer answers 401 and must not fall back to a header.
var ErrNoPrincipal = errors.New("accesswire: no authenticated principal")

// Bridge owns the authorizer, the store-backed sources and the audit writer.
type Bridge struct {
	Store      *store.Store
	Authorizer *access.Authorizer
	Audit      *Auditor
	Logger     *slog.Logger
}

// NewBridge builds the bridge over a store. It fails if the sources cannot be
// constructed: a half-armed authorizer must fail at boot, not at the first 403.
func NewBridge(st *store.Store, log *slog.Logger) (*Bridge, error) {
	source, err := NewSource(st)
	if err != nil {
		return nil, err
	}
	authorizer, err := access.NewAuthorizer(source, source)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	return &Bridge{Store: st, Authorizer: authorizer, Audit: NewAuditor(st, log), Logger: log}, nil
}

// Principal builds the engine principal for the edge-resolved identity. The
// request's tenant must equal the identity's tenant: a bot key never chooses
// its tenant by header, and a cookie session is pinned at login.
func (b *Bridge) Principal(ctx context.Context, tenantID uuid.UUID) (access.Principal, error) {
	edge, ok := auth.PrincipalFromContext(ctx)
	if !ok || edge.AccountID == uuid.Nil || edge.TenantID == uuid.Nil {
		return access.Principal{}, ErrNoPrincipal
	}
	if tenantID != uuid.Nil && edge.TenantID != tenantID {
		return access.Principal{}, ErrNoPrincipal
	}
	principal := access.Principal{
		TenantID:     edge.TenantID,
		AccountID:    edge.AccountID,
		IsSuperAdmin: edge.IsSuperAdmin && !edge.IsBot(),
	}
	if edge.IsBot() {
		principal.BotID = edge.BotID
		permissions, err := b.BotHarnessPermissions(ctx, edge.TenantID, edge.BotID, edge.AccountID)
		if err != nil {
			return access.Principal{}, err
		}
		principal.HarnessPermissions = permissions
	}
	return principal, nil
}

// BotHarnessPermissions reads the Bot's Harness narrowing. A bot key whose Bot
// is missing, archived or owned by another account is not a usable principal;
// an unreadable Harness document narrows to nothing (fail closed).
func (b *Bridge) BotHarnessPermissions(ctx context.Context, tenantID uuid.UUID, botID string, accountID uuid.UUID) ([]string, error) {
	if b == nil || b.Store == nil {
		return nil, errors.New("accesswire: bridge is not configured")
	}
	bot, err := b.Store.GetBot(ctx, tenantID, botID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNoPrincipal
		}
		return nil, err
	}
	if bot.Archived {
		return nil, ErrNoPrincipal
	}
	profile, err := b.Store.GetProfile(ctx, tenantID, bot.ProfileID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNoPrincipal
		}
		return nil, err
	}
	if profile.AccountID != accountID {
		return nil, ErrNoPrincipal
	}
	var document struct {
		Context struct {
			Permissions []string `json:"permissions"`
		} `json:"context"`
	}
	if len(bot.Harness) > 0 {
		if err := json.Unmarshal(bot.Harness, &document); err != nil {
			return nil, nil
		}
	}
	return document.Context.Permissions, nil
}

// Authorize runs the single-resource pipeline and audits a denial. An
// infrastructure failure is returned as an error; the caller must answer 5xx,
// never "allow".
func (b *Bridge) Authorize(ctx context.Context, principal access.Principal, key string, resource access.Resource) (access.Decision, error) {
	if b == nil || b.Authorizer == nil {
		return access.Decision{}, errors.New("accesswire: bridge is not configured")
	}
	decision, err := b.Authorizer.Authorize(ctx, principal, key, resource)
	if err != nil {
		return access.Decision{}, err
	}
	if decision.Denied() {
		b.Audit.Denied(ctx, principal, decision, resource)
	}
	return decision, nil
}

// Visible is the list-page filter. A denied key-level gate is audited, so a
// client cannot probe list endpoints without a trail.
func (b *Bridge) Visible(ctx context.Context, principal access.Principal, key, resourceType string, candidateIDs []string) (access.VisibleResult, error) {
	if b == nil || b.Authorizer == nil {
		return access.VisibleResult{}, errors.New("accesswire: bridge is not configured")
	}
	result, err := b.Authorizer.VisibleResources(ctx, principal, key, resourceType, candidateIDs)
	if err != nil {
		return access.VisibleResult{}, err
	}
	if !result.Allowed() {
		b.Audit.Denied(ctx, principal, result.Decision, access.Resource{TenantID: principal.TenantID, Type: resourceType})
	}
	return result, nil
}

// Effective is the caller's materialized permission set: the union of direct
// and group grants, widest scope per key, narrowed by the Bot Harness for a bot
// principal. A deployment super admin gets every key at scope all.
func (b *Bridge) Effective(ctx context.Context, principal access.Principal) (access.Grants, error) {
	if b == nil || b.Store == nil {
		return nil, errors.New("accesswire: bridge is not configured")
	}
	if err := principal.Validate(); err != nil {
		return nil, err
	}
	if principal.IsSuperAdmin {
		all := make(access.Grants, len(access.Keys()))
		for _, key := range access.Keys() {
			all[key] = access.ScopeAll
		}
		return all, nil
	}
	source, err := NewSource(b.Store)
	if err != nil {
		return nil, err
	}
	effective, err := access.EffectiveGrants(ctx, source, principal.TenantID, principal.AccountID)
	if err != nil {
		return nil, err
	}
	if principal.BotID != "" {
		return access.NarrowToHarness(effective, principal.HarnessPermissions)
	}
	return effective, nil
}

// CanGrant enforces "授予不得超过自己" for one proposed tuple set. The super
// admin short circuit lives in the engine; an unknown key or a scope the key
// does not allow is rejected for everyone.
func (b *Bridge) CanGrant(ctx context.Context, principal access.Principal, proposed []access.Grant) error {
	if b == nil {
		return errors.New("accesswire: bridge is not configured")
	}
	grantor := access.Grantor{SuperAdmin: principal.IsSuperAdmin}
	if !principal.IsSuperAdmin {
		effective, err := b.Effective(ctx, principal)
		if err != nil {
			return err
		}
		grantor.Effective = effective
	}
	return access.CanGrant(grantor, proposed)
}

// BotOwnerAccount resolves the account a Bot belongs to (through its Profile)
// and reports whether the Bot is archived. A missing Bot is store.ErrNotFound.
func (b *Bridge) BotOwnerAccount(ctx context.Context, tenantID uuid.UUID, botID string) (uuid.UUID, bool, error) {
	if b == nil || b.Store == nil {
		return uuid.Nil, false, errors.New("accesswire: bridge is not configured")
	}
	bot, err := b.Store.GetBot(ctx, tenantID, botID)
	if err != nil {
		return uuid.Nil, false, err
	}
	profile, err := b.Store.GetProfile(ctx, tenantID, bot.ProfileID)
	if err != nil {
		return uuid.Nil, false, err
	}
	return profile.AccountID, bot.Archived, nil
}

// String renders the bridge for logs without leaking configuration.
func (b *Bridge) String() string {
	if b == nil || b.Store == nil {
		return "accesswire.Bridge(unconfigured)"
	}
	return fmt.Sprintf("accesswire.Bridge(%d keys)", len(access.Keys()))
}
