// Package boardintegration adapts keel's identity, authorization, PostgreSQL,
// SSE and telemetry surfaces to github.com/jixoai/keel-board (change
// keel-board-v1, Phase 1).
//
// The board module owns the domain, the log and the HTTP contract. This package
// owns only the seams keel must supply: who the caller is (Account plus an
// optional Bot), which keel permission key and channel relation authorizes an
// action, which database the board's own migrations and RLS use, and where the
// board's metrics and audit go.
package boardintegration

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/store"

	"github.com/jixoai/keel-board/protocol"
)

// AccountActorPrefix is the stable actor namespace keel maps an Account into.
// The rest of the actor ID is the account UUID, never a username: a rename
// cannot change board ownership.
const AccountActorPrefix = "account:"

// botProfilePrefix is the board profile token namespace. board's profile
// grammar is ^[a-z][a-z0-9-]{0,31}$, while a keel Bot handle may contain
// underscores, dots, colons and uppercase letters, and may be longer. A raw
// Bot handle therefore cannot be a board profile; the token is a stable
// derivation of the Bot ID (see BotProfileToken).
const botProfilePrefix = "bot-"

// BotProfileToken returns the stable board profile token for a keel Bot ID.
//
// The derivation is intentionally one-way and independent of the display name:
// renaming a Bot must not change its board identity, and two Bots must never
// share a token. 24 hex characters (96 bits) leave room inside the 32-character
// board profile grammar even with the "bot-" prefix.
func BotProfileToken(botID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(botID)))
	return botProfilePrefix + hex.EncodeToString(sum[:12])
}

// AccountActorID is the board actor ID of an Account acting as itself.
func AccountActorID(accountID uuid.UUID) string {
	return AccountActorPrefix + accountID.String()
}

// BotActorID is the board actor ID of a Bot acting through its Account:
// "account:<uuid>+bot-<token>".
func BotActorID(accountID uuid.UUID, botID string) string {
	return AccountActorID(accountID) + "+" + BotProfileToken(botID)
}

// Identity resolves a board Actor and tenant from the keel edge principal. It
// never reads an actor or tenant from a client header: the authentication
// middleware (or the local admin path) is the only source, and a request that
// bypassed it fails closed.
type Identity struct {
	// Store resolves a Bot's display name and owning Profile. A nil Store
	// refuses every Bot request (fail closed); human requests still work from
	// the principal's own fields.
	Store *store.Store
}

// Actor implements server.ActorFunc.
func (i *Identity) Actor(r *http.Request) (protocol.Actor, error) {
	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok || principal.TenantID == uuid.Nil || principal.AccountID == uuid.Nil {
		return protocol.Actor{}, unauthorized()
	}
	accountID := principal.AccountID
	accountName := strings.TrimSpace(principal.DisplayName)
	if accountName == "" {
		accountName = strings.TrimSpace(principal.Username)
	}
	if accountName == "" {
		accountName = AccountActorID(accountID)
	}
	accountName = bounded(accountName, 200)

	if principal.IsBot() {
		return i.botActor(r, principal, accountName)
	}

	handle := strings.TrimSpace(principal.Username)
	if handle == "" {
		handle = AccountActorID(accountID)
	}
	name := strings.TrimSpace(principal.DisplayName)
	if name == "" {
		name = handle
	}
	return protocol.Actor{
		ID:        AccountActorID(accountID),
		Principal: AccountActorID(accountID),
		Kind:      protocol.ActorPerson,
		Handle:    bounded(handle, 200),
		Name:      bounded(name, 200),
	}, nil
}

func (i *Identity) botActor(r *http.Request, principal auth.Principal, accountName string) (protocol.Actor, error) {
	botID := strings.TrimSpace(principal.BotID)
	if botID == "" {
		return protocol.Actor{}, unauthorized()
	}
	if i == nil || i.Store == nil {
		return protocol.Actor{}, unavailable("the identity store is not wired")
	}
	ctx := r.Context()
	bot, err := i.Store.GetBot(ctx, principal.TenantID, botID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return protocol.Actor{}, unauthorized()
		}
		return protocol.Actor{}, unavailable("the bot identity could not be read")
	}
	if bot.Archived {
		return protocol.Actor{}, unauthorized()
	}
	// The Bot acts through the Account named by its Profile. The edge already
	// authenticated the key against that binding; re-reading it keeps a forged
	// or stale context from turning into board ownership.
	profile, err := i.Store.GetProfile(ctx, principal.TenantID, bot.ProfileID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return protocol.Actor{}, unauthorized()
		}
		return protocol.Actor{}, unavailable("the bot profile could not be read")
	}
	if profile.AccountID != principal.AccountID {
		return protocol.Actor{}, unauthorized()
	}

	token := BotProfileToken(bot.ID)
	name := strings.TrimSpace(bot.Name)
	if name == "" {
		name = bot.ID
	}
	return protocol.Actor{
		ID:        AccountActorID(principal.AccountID) + "+" + token,
		Principal: AccountActorID(principal.AccountID),
		Profile:   &token,
		Kind:      protocol.ActorAgent,
		Handle:    bounded(bot.ID, 200),
		Name:      bounded(name, 200),
		Via:       &protocol.Via{Name: accountName},
	}, nil
}

// Tenant implements server.TenantFunc. The tenant is the authenticated
// principal's tenant; a request cannot choose another one by header or path.
func (i *Identity) Tenant(r *http.Request) (string, error) {
	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok || principal.TenantID == uuid.Nil {
		return "", unauthorized()
	}
	return principal.TenantID.String(), nil
}

// EdgePrincipal returns the edge-resolved principal for a board request. It is
// the narrow seam the channel-ownership writer uses after a successful create.
func EdgePrincipal(r *http.Request) (auth.Principal, bool) {
	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok || principal.TenantID == uuid.Nil || principal.AccountID == uuid.Nil {
		return auth.Principal{}, false
	}
	return principal, true
}

func unauthorized() *protocol.Error {
	return protocol.NewError(protocol.CodeUnauthorized, "authentication required")
}

func unavailable(detail string) *protocol.Error {
	return protocol.NewError(protocol.CodeUnavailable, detail)
}

// bounded trims a display value to max bytes on a rune boundary. It is display
// metadata only; neither the actor ID nor authorization depends on it.
func bounded(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	cut := max
	for cut > 0 && !utf8Start(value[cut]) {
		cut--
	}
	return strings.TrimSpace(value[:cut])
}

// utf8Start reports whether b is not a UTF-8 continuation byte.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// ValidateTenantUUID parses the board tenant string into keel's tenant UUID.
// A malformed or foreign tenant is reported as not-found by callers so it
// cannot confirm another board's existence.
func ValidateTenantUUID(tenant string) (uuid.UUID, error) {
	parsed, err := uuid.Parse(strings.TrimSpace(tenant))
	if err != nil || parsed == uuid.Nil {
		return uuid.Nil, fmt.Errorf("boardintegration: %q is not a tenant id", tenant)
	}
	return parsed, nil
}
