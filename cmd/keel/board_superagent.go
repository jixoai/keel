package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/boardintegration"
	"github.com/jixoai/keel/internal/harnessapi"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/superagentboard"
)

// newSuperagentBoardBridge builds the pass-through bridge the board handler is
// mounted behind. It is attached to the supervisor after the agent runtime
// exists; until then every request goes straight to the board.
func newSuperagentBoardBridge(surface *boardintegration.Surface, log *slog.Logger) *superagentboard.Bridge {
	if surface == nil {
		return nil
	}
	return &superagentboard.Bridge{
		Next:       surface,
		Identity:   surface.Identity(),
		Authorizer: surface.Authorizer(),
		Logger:     log,
	}
}

// attachSuperagentBoard wires the mirror and command bridge. A deployment
// without a supervisor keeps a pass-through bridge; a missing KEEL_SECRET_KEY
// keeps both directions fail-closed (no mirror writes, no mirrored claim) while
// the board itself keeps serving.
func attachSuperagentBoard(
	ctx context.Context,
	tenantID uuid.UUID,
	st *store.Store,
	surface *boardintegration.Surface,
	bridge *superagentboard.Bridge,
	service harnessapi.SuperagentService,
	log *slog.Logger,
) *superagentboard.Mirror {
	if bridge == nil || surface == nil || st == nil || service == nil || tenantID == uuid.Nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	secret := strings.TrimSpace(os.Getenv(superagentboard.SecretEnv))
	tokens, tokenErr := superagentboard.NewTokenSource(secret)
	configReader := superagentboard.StoreConfigReader{Store: st}
	if tokenErr != nil {
		// Only a tenant that asked for the mirror needs the warning: every other
		// deployment runs without KEEL_SECRET_KEY and must stay quiet.
		if config, configErr := configReader.SuperagentConfig(ctx, tenantID); configErr == nil && config.Mirror {
			log.Warn("super-agent board mirror is fail-closed: set KEEL_SECRET_KEY to enable it",
				"reason", superagentboard.ErrSecretMissing.Error())
		}
		tokens = nil
	}
	board := &superagentboard.LogBoard{Board: surface.Board()}
	mirror, err := superagentboard.NewMirror(&superagentboard.Mirror{
		TenantID: tenantID,
		Store:    st,
		Board:    board,
		Links:    st,
		Config:   configReader,
		Secret:   tokens,
		ACL:      boardChannelACL{store: st},
		Logger:   log,
		Now:      func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		log.Warn("super-agent board mirror is not wired", "err", err)
		return nil
	}
	commands, err := superagentboard.NewSupervisorCommands(tenantID, st, service)
	if err != nil {
		log.Warn("super-agent board command bridge is not wired", "err", err)
		return nil
	}
	bridge.Attach(st, configReader, mirror, commands, board)
	go mirror.Run(ctx)
	return mirror
}

// boardChannelACL grants the mirrored channel's read relation. The mirror
// writes through the board log, not as a request principal, so the channel has
// no creator relation; without this tuple a board.read holder cannot see the
// queue. tenant-admin and bot-operator are the builtin groups that operate the
// super-agent queue; a deployment with its own groups can replace this seam.
type boardChannelACL struct {
	store *store.Store
}

// EnsureViewer implements superagentboard.ChannelACL.
func (a boardChannelACL) EnsureViewer(ctx context.Context, tenantID uuid.UUID, channel string) error {
	if a.store == nil || tenantID == uuid.Nil || strings.TrimSpace(channel) == "" {
		return nil
	}
	for _, name := range []string{"tenant-admin", "bot-operator"} {
		groupID := access.BuiltinGroupID(tenantID, name)
		if _, err := a.store.PutResourceRelation(ctx, store.ResourceRelation{
			TenantID:     tenantID,
			ResourceType: boardintegration.BoardChannelResourceType,
			ResourceID:   channel,
			Relation:     store.RelationViewer,
			SubjectType:  store.RelationSubjectGroup,
			SubjectID:    groupID.String(),
			CreatedBy:    tenantID,
		}); err != nil {
			return err
		}
	}
	return nil
}
