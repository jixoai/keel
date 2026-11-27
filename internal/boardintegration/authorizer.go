package boardintegration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/auth"

	"github.com/jixoai/keel-board/log"
	"github.com/jixoai/keel-board/protocol"
)

// BoardChannelResourceType is the resource_relations type a board channel owns.
// It reuses keel's existing viewer/editor/owner vocabulary; no second relation
// table exists.
const BoardChannelResourceType = "board_channel"

// adminChannel is the board's operations channel: `manage` asked at "*" is a
// tenant-wide administration decision, not a channel relation.
const adminChannel = "*"

// channelCreateKey marks the one board route that may create a channel. The
// authorizer normally needs a relation before it allows a read or a post; the
// create route asks read+post before the channel exists, so the handler marks
// the request and the authorizer's bootstrap applies only there.
type channelCreateKey struct{}

func withChannelCreate(ctx context.Context) context.Context {
	return context.WithValue(ctx, channelCreateKey{}, true)
}

func channelCreateAllowed(ctx context.Context) bool {
	allowed, _ := ctx.Value(channelCreateKey{}).(bool)
	return allowed
}

// Authorizer implements server.Authorizer over keel's access engine.
//
// Mapping (keel-board design §11):
//
//	board.read    + channel viewer  -> read
//	board.write   + channel editor  -> post/react/label/sync
//	board.write   + channel owner   -> task owner operations (manage)
//	board.manage  at tenant scope   -> channel "*" administration
//
// board.manage implies board.read/board.write on the board surface. It never
// bypasses a channel relation: a tenant administrator without a relation on a
// channel can administer metadata but cannot read or write that channel's
// content.
//
// Channel relations follow keel-board's most-specific-wins inheritance: the
// exact path is checked first, then each ancestor up to the root. The first
// path where the principal holds any relation is the effective one, so a
// viewer tuple on work/secret narrows an editor tuple on work.
type Authorizer struct {
	// Bridge is the edge-resolved identity and grant source. Required.
	Bridge *accesswire.Bridge
	// Relations reads resource_relations. Required.
	Relations access.RelationSource
	// Board answers whether a channel exists, for the create-first-channel
	// bootstrap. Nil treats every channel as existing (no bootstrap).
	Board *log.Board
	// Logger receives unexpected failures. Nil uses slog.Default().
	Logger *slog.Logger
}

var _ interface {
	Authorize(ctx context.Context, tenant string, actor protocol.Actor, channel string, permission protocol.Permission) error
	ValidateRequest(ctx context.Context, tenant string, actor protocol.Actor) error
	AuthorizeAnywhere(ctx context.Context, tenant string, actor protocol.Actor, permission protocol.Permission) error
} = (*Authorizer)(nil)

// NewAuthorizer builds the authorizer. Production and tests share this path:
// both pass the access bridge the edge already resolved the caller through.
func NewAuthorizer(bridge *accesswire.Bridge, relations access.RelationSource, board *log.Board, logger *slog.Logger) (*Authorizer, error) {
	if bridge == nil || bridge.Store == nil {
		return nil, errors.New("boardintegration: an access bridge with a store is required")
	}
	if relations == nil {
		source, err := accesswire.NewSource(bridge.Store)
		if err != nil {
			return nil, err
		}
		relations = source
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Authorizer{Bridge: bridge, Relations: relations, Board: board, Logger: logger}, nil
}

// ValidateRequest rejects an actor that does not match the edge principal
// before a handler can turn a mismatch into an empty result.
func (a *Authorizer) ValidateRequest(ctx context.Context, tenant string, actor protocol.Actor) error {
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.TenantID == uuid.Nil || principal.AccountID == uuid.Nil {
		return unauthorized()
	}
	tenantID, err := ValidateTenantUUID(tenant)
	if err != nil || tenantID != principal.TenantID {
		return notFound()
	}
	if actor.Principal != AccountActorID(principal.AccountID) {
		return unauthorized()
	}
	if principal.BotID != "" {
		if actor.Profile == nil || actor.ID != BotActorID(principal.AccountID, principal.BotID) {
			return unauthorized()
		}
		return nil
	}
	if actor.Profile != nil {
		return unauthorized()
	}
	return nil
}

// Authorize implements server.Authorizer.
func (a *Authorizer) Authorize(ctx context.Context, tenant string, actor protocol.Actor, channel string, permission protocol.Permission) error {
	if a == nil || a.Bridge == nil || a.Relations == nil {
		return unavailable("the board authorizer is not wired")
	}
	tenantID, err := ValidateTenantUUID(tenant)
	if err != nil {
		return notFound()
	}
	principal, err := a.Bridge.Principal(ctx, tenantID)
	if err != nil {
		if errors.Is(err, accesswire.ErrNoPrincipal) {
			return unauthorized()
		}
		return unavailable("authorization is unavailable")
	}
	if actor.Principal != AccountActorID(principal.AccountID) {
		return unauthorized()
	}
	if principal.BotID != "" {
		if actor.Profile == nil || actor.ID != BotActorID(principal.AccountID, principal.BotID) {
			return unauthorized()
		}
	} else if actor.Profile != nil {
		return unauthorized()
	}
	return a.authorizePrincipal(ctx, tenantID, principal, actor, channel, permission)
}

// authorizePrincipal is the shared decision path. HTTP requests reach it after
// the edge principal was authenticated; the automation workers reach it through
// WorkerAuthorizer, which resolves the same principal from the actor the
// watcher/schedule names. actor is used for audit only; the principal is the
// authority.
func (a *Authorizer) authorizePrincipal(ctx context.Context, tenantID uuid.UUID, principal access.Principal, actor protocol.Actor, channel string, permission protocol.Permission) error {
	if a == nil || a.Bridge == nil || a.Relations == nil {
		return unavailable("the board authorizer is not wired")
	}
	if actor.Principal == "" {
		actor.Principal = AccountActorID(principal.AccountID)
	}

	effective, err := a.Bridge.Effective(ctx, principal)
	if err != nil {
		return unavailable("authorization is unavailable")
	}

	if channel == adminChannel {
		// Channel "*" has no content relation. Only board.manage at tenant (or
		// all) scope is the administrative decision; the super-admin effective
		// set already carries every key at scope all.
		scope, held := effective.ScopeOf(access.KeyBoardManage)
		if !held || (scope != access.ScopeTenant && scope != access.ScopeAll) {
			return a.deny(ctx, principal, access.KeyBoardManage, scope,
				access.ReasonMissingGrant,
				"channel * needs board.manage at tenant scope",
				resource(tenantID, adminChannel), "board.manage")
		}
		return nil
	}

	path := normalizeChannel(channel)
	if path == "" {
		return notFound()
	}

	// Resolve the most specific relation the principal holds on this channel
	// or its nearest ancestor.
	evaluation, chosen, err := a.channelRelation(ctx, principal, tenantID, path)
	if err != nil {
		return err
	}
	if chosen == "" {
		// No relation anywhere. The channel-create route is the one exception:
		// it asks read then post before the channel exists. board.manage may
		// administer channel metadata (including an idempotent create) without
		// a content relation; board.write may create a channel that does not
		// exist yet. The create itself records the creator as owner
		// (channelOwners), so this bootstrap never grants access to existing
		// content.
		if channelCreateAllowed(ctx) && (permission == protocol.PermissionRead || permission == protocol.PermissionPost) {
			// board.manage administers channel metadata, including an
			// idempotent create of an existing channel.
			if scope, held := effective.ScopeOf(access.KeyBoardManage); held &&
				(scope == access.ScopeTenant || scope == access.ScopeAll) {
				return nil
			}
			// A writer may create a channel that does not exist yet. It must not
			// use the create route to probe an existing channel it cannot read:
			// board's 200 created=false would confirm existence.
			exists, existsErr := a.channelExists(tenantID.String(), path)
			if existsErr != nil {
				return existsErr
			}
			if !exists {
				if _, _, _, held := boardKey(effective, protocol.PermissionPost); held {
					return nil
				}
			}
		}
		// The caller gets a 404 so it cannot confirm the channel exists, but the
		// denial is still an authorization fact: record it in auth_events
		// (permission.denied) before hiding it.
		key, scope, reason, held := boardKey(effective, permission)
		reasonCode := access.ReasonMissingRelation
		detail := fmt.Sprintf("channel %q has no relation for the caller", path)
		if !held {
			reasonCode = access.ReasonMissingGrant
			detail = reason
		}
		a.recordDenial(ctx, principal, key, scope, reasonCode, detail, resource(tenantID, path))
		return notFound()
	}

	required := relationForPermission(permission)
	if required == access.RelationNone {
		return notFound()
	}
	key, scope, reason, held := boardKey(effective, permission)
	if !held {
		detail := fmt.Sprintf("no grant for %s", key)
		if reason != "" {
			detail = reason
		}
		return a.deny(ctx, principal, key, scope, access.ReasonMissingGrant, detail,
			resource(tenantID, chosen), key)
	}
	if !relationHeld(evaluation, required, scope) {
		detail := fmt.Sprintf("channel %q has no %s relation reachable at scope %q", chosen, required, scope)
		return a.deny(ctx, principal, key, scope, access.ReasonMissingRelation, detail,
			resource(tenantID, chosen), "channel:"+string(required))
	}
	return nil
}

// tenantManager reports whether the caller holds board.manage at tenant or all
// scope. It is the adoption gate for a relation-free channel: board.manage
// administers metadata but never bypasses an existing content relation.
func (a *Authorizer) tenantManager(ctx context.Context, principal auth.Principal) bool {
	if a == nil || a.Bridge == nil || principal.TenantID == uuid.Nil {
		return false
	}
	effectivePrincipal, err := a.Bridge.Principal(ctx, principal.TenantID)
	if err != nil {
		return false
	}
	effective, err := a.Bridge.Effective(ctx, effectivePrincipal)
	if err != nil {
		return false
	}
	scope, held := effective.ScopeOf(access.KeyBoardManage)
	return held && (scope == access.ScopeTenant || scope == access.ScopeAll)
}

// AuthorizeAnywhere implements server.AnywhereAuthorizer: a board write action
// that names no channel yet (an attachment upload) is allowed when the caller
// may write somewhere. The channel relation is checked when the message names
// the channel.
func (a *Authorizer) AuthorizeAnywhere(ctx context.Context, tenant string, actor protocol.Actor, permission protocol.Permission) error {
	if a == nil || a.Bridge == nil {
		return unavailable("the board authorizer is not wired")
	}
	tenantID, err := ValidateTenantUUID(tenant)
	if err != nil {
		return notFound()
	}
	principal, err := a.Bridge.Principal(ctx, tenantID)
	if err != nil {
		if errors.Is(err, accesswire.ErrNoPrincipal) {
			return unauthorized()
		}
		return unavailable("authorization is unavailable")
	}
	if actor.Principal != AccountActorID(principal.AccountID) {
		return unauthorized()
	}
	effective, err := a.Bridge.Effective(ctx, principal)
	if err != nil {
		return unavailable("authorization is unavailable")
	}
	if _, _, _, held := boardKey(effective, permission); held {
		return nil
	}
	key := access.KeyBoardWrite
	return a.deny(ctx, principal, key, "", access.ReasonMissingGrant,
		"no board write grant", resource(tenantID, ""), key)
}

// channelRelation walks the channel path from most specific to least. A path
// where any relation is held wins even when that relation is too weak for the
// requested action: that is the "explicit viewer narrows an inherited editor"
// rule. Unresolved tuples fail closed instead of falling through to a wider
// ancestor.
func (a *Authorizer) channelRelation(ctx context.Context, principal access.Principal, tenantID uuid.UUID, path string) (access.RelationEvaluation, string, error) {
	candidate := path
	for {
		evaluation, err := access.EvaluateRelations(ctx, a.Relations, principal,
			resource(tenantID, candidate))
		if err != nil {
			return access.RelationEvaluation{}, "", unavailable("channel access could not be evaluated")
		}
		if evaluation.Unresolved {
			return access.RelationEvaluation{}, "", a.deny(ctx, principal, access.KeyBoardRead, "",
				access.ReasonUnresolvedSubject,
				"a channel relation names an unresolvable subject",
				resource(tenantID, candidate), "channel relation")
		}
		if len(evaluation.Held) > 0 {
			return evaluation, candidate, nil
		}
		index := strings.LastIndex(candidate, "/")
		if index < 0 {
			return access.RelationEvaluation{}, "", nil
		}
		candidate = candidate[:index]
	}
}

// channelExists asks the board projection whether the path names a channel.
// A store failure is a 503, never "not found".
func (a *Authorizer) channelExists(tenant, path string) (bool, error) {
	if a.Board == nil {
		return true, nil
	}
	exists := false
	if err := a.Board.Read(tenant, func(snapshot *log.Snapshot) error {
		exists = snapshot.Channel(path) != nil
		return nil
	}); err != nil {
		return false, unavailable("the board projection could not be read")
	}
	return exists, nil
}

// deny records the denial in keel's auth_events (permission.denied) and
// returns the board's 403. The audit detail carries identifiers and the missing
// key/relation only, never message text or a claim token.
func (a *Authorizer) deny(ctx context.Context, principal access.Principal, key string, scope access.Scope, reason access.Reason, detail string, res access.Resource, missing string) error {
	a.recordDenial(ctx, principal, key, scope, reason, detail, res)
	message := "permission denied"
	if missing != "" {
		message = "permission denied: missing " + missing
	}
	return protocol.NewError(protocol.CodeForbidden, message).WithHint(missing)
}

// recordDenial appends one permission.denied fact to keel's auth_events. It is
// best effort, like every denial audit: a refusal or a hidden 404 must not turn
// into a 500 because the audit stream is down.
func (a *Authorizer) recordDenial(ctx context.Context, principal access.Principal, key string, scope access.Scope, reason access.Reason, detail string, res access.Resource) {
	if a.Bridge == nil {
		return
	}
	a.Bridge.Denied(ctx, principal, access.Decision{
		Allow: false, Reason: reason, Key: key, Scope: scope, Detail: detail,
	}, res)
}

// boardKey picks the key that authorizes permission and its effective scope.
// board.manage implies read/write on the board surface; the specific key wins
// when both are held so audit detail names the narrower authority.
func boardKey(effective access.Grants, permission protocol.Permission) (string, access.Scope, string, bool) {
	keys := keysFor(permission)
	for _, key := range keys {
		if scope, held := effective.ScopeOf(key); held {
			return key, scope, "", true
		}
	}
	// Report the specific key as the missing one; board.manage is the
	// administration alternative, not the expected default for a channel read.
	missing := keys[0]
	return missing, "", "no grant for " + missing, false
}

func keysFor(permission protocol.Permission) []string {
	switch permission {
	case protocol.PermissionRead:
		return []string{access.KeyBoardRead, access.KeyBoardManage}
	case protocol.PermissionPost, protocol.PermissionReact, protocol.PermissionLabel, protocol.PermissionSync,
		protocol.PermissionManage:
		return []string{access.KeyBoardWrite, access.KeyBoardManage}
	default:
		return []string{access.KeyBoardWrite, access.KeyBoardManage}
	}
}

func relationForPermission(permission protocol.Permission) access.Relation {
	switch permission {
	case protocol.PermissionRead:
		return access.RelationViewer
	case protocol.PermissionManage:
		return access.RelationOwner
	case protocol.PermissionPost, protocol.PermissionReact, protocol.PermissionLabel, protocol.PermissionSync:
		return access.RelationEditor
	default:
		return access.RelationNone
	}
}

// relationHeld applies the relation lattice plus the grant's scope reach:
// an own-scope grant reaches only a relation held directly by the account, a
// team/tenant/all-scope grant reaches a relation held through a group or team
// bot too.
func relationHeld(evaluation access.RelationEvaluation, required access.Relation, scope access.Scope) bool {
	if scope == access.ScopeOwn {
		return relationSatisfies(evaluation.Direct, required)
	}
	return relationSatisfies(evaluation.Held, required)
}

// relationSatisfies is keel's fixed relation lattice: owner includes editor
// and viewer, editor includes viewer, and a team member can see. Viewer never
// implies editor, and no relation implies owner.
func relationSatisfies(held map[access.Relation]bool, required access.Relation) bool {
	for relation := range held {
		switch relation {
		case access.RelationOwner:
			return true
		case access.RelationEditor:
			if required == access.RelationEditor || required == access.RelationViewer {
				return true
			}
		case access.RelationViewer, access.RelationTeamMember:
			if required == access.RelationViewer {
				return true
			}
		}
	}
	return false
}

func resource(tenantID uuid.UUID, id string) access.Resource {
	return access.Resource{TenantID: tenantID, Type: BoardChannelResourceType, ID: id}
}

func normalizeChannel(channel string) string {
	return strings.Trim(strings.TrimSpace(channel), "/")
}

func notFound() *protocol.Error {
	return protocol.NewError(protocol.CodeNotFound, "not found")
}
