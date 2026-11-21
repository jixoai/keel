package access

import (
	"errors"
	"strings"

	"github.com/google/uuid"
)

// Principal is the identity an action is authorized for. A human or service
// account is (TenantID, AccountID, IsSuperAdmin). A Bot carries the same
// account identity plus its BotID and its Harness permission narrowing: a Bot
// never becomes a second permission subject, it acts through its Account
// (L2/L11/L14).
//
// HarnessPermissions is only meaningful when BotID is set. For a Bot an empty
// (or nil) HarnessPermissions list narrows the effective permission set to
// nothing, exactly as botteam documents for Harness.context.permissions. It is
// not "inherit everything".
type Principal struct {
	// TenantID is the tenant the account belongs to.
	TenantID uuid.UUID
	// AccountID is the login/permission subject.
	AccountID uuid.UUID
	// BotID is set when the principal acts as a Bot. It is the bot handle, not
	// an account id.
	BotID string
	// HarnessPermissions is the Bot's own permission narrowing. The engine
	// checks membership directly; callers that need a materialized set use
	// NarrowToHarness.
	HarnessPermissions []string
	// IsSuperAdmin is the deployment-level authority. It is never delegated to a
	// Bot (spec: super admin authority MUST NOT be delegated to a Bot).
	IsSuperAdmin bool
	// Disabled marks a disabled account. A2 rejects authentication for a
	// disabled account; the engine denies it too, as defense in depth.
	Disabled bool
}

// Validate checks the identity fields an authorization decision depends on.
// It does not consult a store; an account that does not exist simply has no
// grants and is denied later.
func (p Principal) Validate() error {
	switch {
	case p.TenantID == uuid.Nil:
		return errors.New("tenant id is required")
	case p.AccountID == uuid.Nil:
		return errors.New("account id is required")
	case p.Disabled:
		return errors.New("account is disabled")
	case p.BotID != "" && strings.ContainsAny(p.BotID, " \t\r\n"):
		return errors.New("bot id must be a single token without whitespace")
	case p.IsSuperAdmin && p.BotID != "":
		return errors.New("super admin authority cannot be delegated to a bot")
	}
	return nil
}

// harnessAllows reports whether the Bot's harness narrowing includes key. The
// caller only invokes it for a Bot principal.
func harnessAllows(harness []string, key string) bool {
	for _, candidate := range harness {
		if strings.TrimSpace(candidate) == key {
			return true
		}
	}
	return false
}
