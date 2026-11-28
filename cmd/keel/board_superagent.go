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
	"github.com/jixoai/keel/internal/boardplugin"
	"github.com/jixoai/keel/internal/pluginruntime"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/superagent"
	"github.com/jixoai/keel/internal/superagentplugin"
	"github.com/jixoai/keel/internal/superagentstore"
)

// boardService is the board plugin's public task/channel/thread service. The
// superagent plugin reaches the board only through this interface; no board
// table or board store type enters the dependent's code path.
func boardService(surface *boardintegration.Surface) boardplugin.Service {
	if surface == nil {
		return nil
	}
	return &boardplugin.LogBoard{Board: surface.Board()}
}

// newSuperagentBoardWriter builds the superagent plugin's board projection.
// It is append-only on the board side: the ticket is the supervisor truth and
// the board thread is a projection. A deployment without KEEL_SECRET_KEY keeps
// the projection fail-closed while both plugins still load.
func newSuperagentBoardWriter(
	ctx context.Context,
	tenantID uuid.UUID,
	st *store.Store,
	domain *superagentstore.Store,
	surface *boardintegration.Surface,
	service boardplugin.Service,
	workerBot string,
	ownerSessionID uuid.UUID,
	dispatch func(ctx context.Context, ticketID uuid.UUID) error,
	log *slog.Logger,
) *superagentplugin.Writer {
	if st == nil || domain == nil || surface == nil || service == nil || tenantID == uuid.Nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	configReader := superagentplugin.StoreConfigReader{Store: st}
	secret := strings.TrimSpace(os.Getenv(superagentplugin.SecretEnv))
	tokens, tokenErr := superagentplugin.NewTokenSource(secret)
	if tokenErr != nil {
		// Only a tenant that asked for the projection needs the warning.
		if config, configErr := configReader.SuperagentConfig(ctx, tenantID); configErr == nil && config.Mirror {
			log.Warn("super-agent board projection is fail-closed: set KEEL_SECRET_KEY to enable it",
				"reason", superagentplugin.ErrSecretMissing.Error())
		}
		tokens = nil
	}
	importOptions := superagentplugin.ImportOptions{
		Store: domain, Links: domain, TenantID: tenantID,
		Requester: superagent.Actor{ID: "board:" + tenantID.String(), Display: "Board"},
		WorkerBot: workerBot,
		// The orchestrator session is the imported ticket's owner: the Host's
		// Assign gate only admits a member of the work item.
		OwnerSessionID: ownerSessionID,
		Dispatch:       dispatch,
	}
	writer, err := superagentplugin.NewWriter(&superagentplugin.Writer{
		TenantID: tenantID,
		Store:    domain,
		Board:    service,
		Links:    domain,
		Config:   configReader,
		Secret:   tokens,
		ACL:      boardChannelACL{store: st},
		Enabled:  surface.BoardEnabled,
		Import: func(ctx context.Context) error {
			config, err := configReader.SuperagentConfig(ctx, tenantID)
			if err != nil {
				return err
			}
			_, err = superagentplugin.ImportChannel(ctx, service, importOptions, config.Channel, 200)
			return err
		},
		Logger: log,
		Now:    func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		log.Warn("super-agent board projection is not wired", "err", err)
		return nil
	}
	return writer
}

// activeSupervisor is the product exposure gate. The plugin's Start can build
// the supervisor and still fail a later step (service publication, writer
// start), so a non-nil pointer alone does not mean the queue is serving. Only
// an ACTIVE kernel state may expose the supervisor to the HTTP surface and the
// model-facing report/ask tools.
func activeSupervisor(runtime *pluginruntime.Runtime, candidate *superagent.Orchestrator) *superagent.Orchestrator {
	if runtime == nil || !runtime.Active(superagentplugin.PluginID) {
		return nil
	}
	return candidate
}

// boardChannelACL grants the projected channel's operator relation. The writer
// appends through the board log, not as a request principal, so the channel has
// no creator relation; without this tuple even an operator cannot see the
// queue. tenant-admin and bot-operator are the builtin groups that operate the
// super-agent queue; owner includes editor and viewer, and is what the UI's
// channel-level check requires. A deployment with its own groups can replace
// this seam.
type boardChannelACL struct {
	store *store.Store
}

// EnsureOperator implements superagentplugin.ChannelACL.
func (a boardChannelACL) EnsureOperator(ctx context.Context, tenantID uuid.UUID, channel string) error {
	if a.store == nil || tenantID == uuid.Nil || strings.TrimSpace(channel) == "" {
		return nil
	}
	for _, name := range []string{"tenant-admin", "bot-operator"} {
		groupID := access.BuiltinGroupID(tenantID, name)
		if _, err := a.store.PutResourceRelation(ctx, store.ResourceRelation{
			TenantID:     tenantID,
			ResourceType: boardintegration.BoardChannelResourceType,
			ResourceID:   channel,
			Relation:     store.RelationOwner,
			SubjectType:  store.RelationSubjectGroup,
			SubjectID:    groupID.String(),
			CreatedBy:    tenantID,
		}); err != nil {
			return err
		}
	}
	return nil
}
