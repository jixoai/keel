package boardplugin

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jixoai/keel/internal/pluginruntime"
	"github.com/jixoai/keel/internal/pluginstore"
)

// PluginID is the board plugin's manifest id.
const PluginID = "keel.board"

// Capability is the capability name superagent declares in `requires`.
const Capability = "board"

// Options wires the board service plugin. The host builds the board surface
// (keel serve: boardintegration.Surface) and hands it in; this package owns the
// plugin's lifecycle and its published service, not the HTTP mount.
type Options struct {
	// Service is the task/channel/thread substrate. Required for an enabled
	// board; a disabled deployment passes nil and Disabled=true.
	Service Service
	// Domain is the board's independent domain store adapter (board pgadapter
	// behind Surface). Required: board is one of the two plugins allowed a
	// domain schema.
	Domain pluginstore.DomainStore
	// Close releases the host's board surface (pools, workers). Nil is allowed
	// in tests.
	Close func()
	// Disabled keeps the plugin in the graph but starts nothing, so a
	// dependent's status is PENDING rather than silently half-loaded.
	Disabled bool
	Logger   *slog.Logger
}

// NewPlugin builds the board service plugin.
func NewPlugin(opts Options) *pluginruntime.Plugin {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	var services *pluginruntime.Services
	return &pluginruntime.Plugin{
		Spec: pluginruntime.Spec{
			ID:       PluginID,
			Version:  "1.0.0",
			Kind:     "service",
			Provides: []string{Capability},
		},
		Disabled: opts.Disabled,
		Start: func(ctx context.Context, env *pluginruntime.Env) error {
			if opts.Service == nil {
				return errors.New("boardplugin: board service is not configured")
			}
			if opts.Domain == nil {
				return errors.New("boardplugin: board domain store is not configured")
			}
			if err := opts.Domain.Migrate(ctx); err != nil {
				return err
			}
			if err := opts.Domain.Ready(ctx); err != nil {
				return err
			}
			if err := env.Services.Provide(PluginID, Capability, opts.Service); err != nil {
				return err
			}
			services = env.Services
			logger.Info("board plugin active", "domain", opts.Domain.Domain())
			return nil
		},
		Stop: func(context.Context) error {
			if services != nil {
				services.Remove(PluginID, Capability)
			}
			// The service was published through Env.Services, which the runtime
			// keeps; the host closes the surface here. A nil Close is a test or
			// a host that owns the surface elsewhere.
			if opts.Close != nil {
				opts.Close()
			}
			return nil
		},
	}
}
