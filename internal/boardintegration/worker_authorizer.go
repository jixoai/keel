package boardintegration

// WorkerAuthorizer authorizes a board write that originated outside an HTTP
// request (a watcher action or a schedule run). The board module hands the
// actor the watcher/schedule names; this adapter resolves that actor back to a
// keel Account (and optional Bot) principal and then shares the HTTP path's
// channel/relation decision. It never reads a header and never widens scope.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/store"

	"github.com/jixoai/keel-board/protocol"
)

// WorkerAuthorizer implements automation.Authorizer.
type WorkerAuthorizer struct {
	Bridge *accesswire.Bridge
	Store  *store.Store
	// Decision is the shared board authorizer. Required.
	Decision *Authorizer
}

// Authorize resolves actor to a principal and delegates.
func (w *WorkerAuthorizer) Authorize(ctx context.Context, tenant string, actor protocol.Actor, channel string, permission protocol.Permission) error {
	if w == nil || w.Bridge == nil || w.Store == nil || w.Decision == nil {
		return unavailable("the automation authorizer is not wired")
	}
	tenantID, err := ValidateTenantUUID(tenant)
	if err != nil {
		return notFound()
	}
	accountID, botID, err := w.resolveActor(ctx, tenantID, actor)
	if err != nil {
		return unauthorized()
	}
	principal := access.Principal{TenantID: tenantID, AccountID: accountID, BotID: botID}
	if botID != "" {
		permissions, permErr := w.Bridge.BotHarnessPermissions(ctx, tenantID, botID, accountID)
		if permErr != nil {
			return unauthorized()
		}
		principal.HarnessPermissions = permissions
	}
	return w.Decision.authorizePrincipal(ctx, tenantID, principal, actor, channel, permission)
}

// resolveActor parses the board actor namespace back to a keel account and
// verifies a bot token still names a live bot owned by that account.
func (w *WorkerAuthorizer) resolveActor(ctx context.Context, tenantID uuid.UUID, actor protocol.Actor) (uuid.UUID, string, error) {
	principal := strings.TrimSpace(actor.Principal)
	if !strings.HasPrefix(principal, AccountActorPrefix) {
		return uuid.Nil, "", errors.New("actor is not a keel account")
	}
	raw := strings.TrimPrefix(principal, AccountActorPrefix)
	if index := strings.IndexByte(raw, '+'); index >= 0 {
		raw = raw[:index]
	}
	accountID, err := uuid.Parse(raw)
	if err != nil || accountID == uuid.Nil {
		return uuid.Nil, "", errors.New("actor has no usable account")
	}
	if _, err := w.Store.GetAccount(ctx, tenantID, accountID); err != nil {
		return uuid.Nil, "", err
	}
	if actor.Profile == nil || *actor.Profile == "" {
		if actor.Kind == protocol.ActorAgent {
			// An agent actor without a profile is not a keel Bot identity.
			return uuid.Nil, "", errors.New("agent actor has no bot profile")
		}
		return accountID, "", nil
	}
	bots, err := w.Store.ListBots(ctx, tenantID, false, 1000)
	if err != nil {
		return uuid.Nil, "", err
	}
	want := strings.TrimSpace(*actor.Profile)
	for _, bot := range bots {
		if BotProfileToken(bot.ID) != want {
			continue
		}
		profile, err := w.Store.GetProfile(ctx, tenantID, bot.ProfileID)
		if err != nil {
			return uuid.Nil, "", err
		}
		if profile.AccountID != accountID {
			return uuid.Nil, "", fmt.Errorf("bot %s is not owned by the actor", bot.ID)
		}
		return accountID, bot.ID, nil
	}
	return uuid.Nil, "", errors.New("no live bot matches the actor profile")
}
