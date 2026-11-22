// Trigger Bot ownership (change keel-bot-identity-access, A4.2b3a follow-up).
//
// A trigger fires as exactly one Bot: the fired session is created with that
// Bot's ownership in the same transaction as the session row, and the attempt
// then resolves the Bot's prompt/tools/model from the session's ownership.
// The engine names two narrow seams (resolve + write); this file binds them to
// the product store, so harnessext never imports the identity tables.
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/store"
)

// triggerBotOwner implements harnessext.TriggerBotResolver and
// harnessext.TriggerSessionOwner over the product store.
type triggerBotOwner struct {
	store *store.Store
}

// ResolveTriggerBot reads the Bot and its Profile's owner account in the tenant
// the trigger (or caller) names. Missing and archived Bots are distinct
// refusals: a fire answers 403 for the former and 409 for the latter.
func (o triggerBotOwner) ResolveTriggerBot(ctx context.Context, tenantID uuid.UUID, botID string) (harnessext.TriggerBot, error) {
	if o.store == nil {
		return harnessext.TriggerBot{}, harnessext.ErrTriggerBotUnavailable
	}
	botID = strings.TrimSpace(botID)
	if botID == "" {
		return harnessext.TriggerBot{}, fmt.Errorf("%w: botId is required", harnessext.ErrTriggerBotMissing)
	}
	bot, err := o.store.GetBot(ctx, tenantID, botID)
	if errors.Is(err, store.ErrNotFound) {
		return harnessext.TriggerBot{}, fmt.Errorf("%w: bot %q", harnessext.ErrTriggerBotMissing, botID)
	}
	if err != nil {
		return harnessext.TriggerBot{}, err
	}
	if bot.Archived {
		return harnessext.TriggerBot{}, fmt.Errorf("%w: bot %q", harnessext.ErrTriggerBotArchived, botID)
	}
	profile, err := o.store.GetProfile(ctx, tenantID, bot.ProfileID)
	if errors.Is(err, store.ErrNotFound) {
		return harnessext.TriggerBot{}, fmt.Errorf("%w: bot %q has no profile", harnessext.ErrTriggerBotMissing, botID)
	}
	if err != nil {
		return harnessext.TriggerBot{}, err
	}
	if profile.AccountID == uuid.Nil {
		return harnessext.TriggerBot{}, fmt.Errorf("%w: bot %q has no owner account", harnessext.ErrTriggerBotMissing, botID)
	}
	return harnessext.TriggerBot{
		TenantID:  tenantID,
		BotID:     bot.ID,
		AccountID: profile.AccountID,
	}, nil
}

// CreateOwnedSession writes the fired session, its Bot/account/created-by
// columns and the owner relation in one transaction. store.CreateOwnedSession
// re-checks the Bot's archived state and Profile owner, so a Bot archived
// between resolution and the write still refuses the session.
func (o triggerBotOwner) CreateOwnedSession(ctx context.Context, ownership harnessext.TriggerSessionOwnership) error {
	if o.store == nil {
		return harnessext.ErrTriggerBotUnavailable
	}
	return o.store.CreateOwnedSession(ctx, store.SessionOwnership{
		TenantID:           ownership.TenantID,
		SessionID:          ownership.SessionID,
		BotID:              ownership.BotID,
		AccountID:          ownership.AccountID,
		CreatedByAccountID: ownership.CreatedByAccountID,
		TeamBotID:          ownership.TeamBotID,
	})
}
