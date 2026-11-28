package superagentplugin

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/jixoai/keel/internal/boardplugin"
	"github.com/jixoai/keel/internal/pluginruntime"
	"github.com/jixoai/keel/internal/pluginstore"
	"github.com/jixoai/keel/internal/superagentstore"
)

// PluginID is the superagent plugin's manifest id.
const PluginID = "keel.superagent"

// Capability is the service name the plugin publishes.
const Capability = "superagent"

// DomainStore is the superagent plugin's independent domain store. The
// platform schema creates the superagent_* tables; the adapter proves it can
// reach them.
type DomainStore struct {
	Store *superagentstore.Store
}

var _ pluginstore.DomainStore = DomainStore{}

// Domain names the storage owner.
func (d DomainStore) Domain() string { return "superagent" }

// Migrate is a no-op: the platform schema migration creates the superagent_*
// tables in the same transaction that creates plugin_state/config.
func (d DomainStore) Migrate(context.Context) error { return nil }

// Ready proves the adapter can serve reads.
func (d DomainStore) Ready(ctx context.Context) error {
	if d.Store == nil {
		return errors.New("superagentplugin: superagent store is not configured")
	}
	return d.Store.Ping(ctx)
}

// BuildFunc materializes the supervisor service once the board dependency is
// available. Returning a typed nil is a programming error; return nil plus an
// error for a disabled deployment.
type BuildFunc func(ctx context.Context, board boardplugin.Service) (any, error)

// Options wires the superagent plugin.
type Options struct {
	// BoardCapability is the required service name (normally "board").
	BoardCapability string
	// Build creates the supervisor service. Nil uses Service.
	Build BuildFunc
	// Service is a prebuilt supervisor service (tests, an embedding host).
	Service any
	// Stop releases the supervisor. Nil is allowed.
	Stop func(ctx context.Context) error
	// Writer is the board projection/import loop. Nil disables it.
	Writer *Writer
	// Domain is the plugin's independent storage. Required for a production
	// deployment; a test may pass a memory-backed adapter.
	Domain pluginstore.DomainStore
	// Disabled keeps the plugin PENDING (or stopped) without starting.
	Disabled bool
	Logger   *slog.Logger
}

// NewPlugin builds the superagent service plugin. Its manifest requires the
// board capability; the kernel starts it only after board published it, and a
// missing/disabled/failed board leaves it PENDING instead of half-loaded.
func NewPlugin(opts Options) *pluginruntime.Plugin {
	boardCapability := opts.BoardCapability
	if boardCapability == "" {
		boardCapability = boardplugin.Capability
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	var (
		services *pluginruntime.Services
		cancel   context.CancelFunc
		stopOnce sync.Once
	)
	return &pluginruntime.Plugin{
		Spec: pluginruntime.Spec{
			ID:       PluginID,
			Version:  "1.0.0",
			Kind:     "service",
			Requires: []string{boardCapability},
			Provides: []string{Capability},
		},
		Disabled: opts.Disabled,
		Start: func(ctx context.Context, env *pluginruntime.Env) error {
			board, err := pluginruntime.GetAs[boardplugin.Service](env.Services, boardCapability)
			if err != nil {
				return err
			}
			if opts.Domain != nil {
				if err := opts.Domain.Migrate(ctx); err != nil {
					return err
				}
				if err := opts.Domain.Ready(ctx); err != nil {
					return err
				}
			}
			service := opts.Service
			if opts.Build != nil {
				built, err := opts.Build(ctx, board)
				if err != nil {
					return err
				}
				if built == nil {
					return errors.New("superagentplugin: supervisor builder returned nil")
				}
				service = built
			}
			if service == nil {
				return errors.New("superagentplugin: supervisor service is not configured")
			}
			if err := env.Services.Provide(PluginID, Capability, service); err != nil {
				return err
			}
			services = env.Services
			if opts.Writer != nil {
				var writerCtx context.Context
				writerCtx, cancel = context.WithCancel(ctx)
				go opts.Writer.Run(writerCtx)
			}
			logger.Info("superagent plugin active")
			return nil
		},
		Stop: func(ctx context.Context) error {
			var err error
			stopOnce.Do(func() {
				if cancel != nil {
					cancel()
				}
				if services != nil {
					services.Remove(PluginID, Capability)
				}
				if opts.Stop != nil {
					err = opts.Stop(ctx)
				}
			})
			return err
		},
	}
}
