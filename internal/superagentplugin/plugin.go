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

// Built is one materialized supervisor. Stop releases everything Start
// acquired (the orchestrator goroutine, its writer lease, the writer ticker).
type Built struct {
	Service any
	Writer  *Writer
	Stop    func(ctx context.Context) error
}

// BuildFunc materializes the supervisor once the board dependency is
// available. It runs inside Start, after the board plugin published `board`;
// returning a typed nil is a programming error. A deployment whose mode is
// disabled must set Options.Disabled instead of returning a nil service.
type BuildFunc func(ctx context.Context, board boardplugin.Service) (*Built, error)

// Options wires the superagent plugin.
type Options struct {
	// BoardCapability is the required service name (normally "board").
	BoardCapability string
	// Build creates the supervisor service and its board writer. Nil uses
	// Service/Writer (tests, an embedding host).
	Build BuildFunc
	// Service is a prebuilt supervisor service (tests, an embedding host).
	Service any
	// Writer is a prebuilt board projection loop (tests, an embedding host).
	Writer *Writer
	// Stop releases a prebuilt supervisor. Nil is allowed.
	Stop func(ctx context.Context) error
	// Domain is the plugin's independent storage. Required for a production
	// deployment; a test may pass a memory-backed adapter.
	Domain pluginstore.DomainStore
	// Disabled keeps the plugin in the graph without starting it. A missing or
	// failed board is not "disabled": the plugin must stay PENDING so the
	// dependency truth is visible instead of collapsing to Stopped.
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
		mu       sync.Mutex
		running  bool
		services *pluginruntime.Services
		release  func(context.Context) error
		stopRun  context.CancelFunc
	)
	// stopLocked detaches the per-run state and returns what Stop must release.
	// Re-arming on Start makes a reload (Unload then Load) start a second
	// generation instead of leaving the first one's goroutine and services
	// behind; a second Stop without a Start is a no-op.
	stopLocked := func() (context.CancelFunc, *pluginruntime.Services, func(context.Context) error) {
		if !running {
			return nil, nil, nil
		}
		running = false
		cancel, svc, rel := stopRun, services, release
		stopRun, services, release = nil, nil, nil
		return cancel, svc, rel
	}
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
			// The run context covers the supervisor goroutine and the writer:
			// stopping the plugin cancels both, so a reload cannot leak the
			// previous generation.
			runCtx, cancel := context.WithCancel(ctx)
			service := opts.Service
			writer := opts.Writer
			stop := opts.Stop
			if opts.Build != nil {
				built, err := opts.Build(runCtx, board)
				if err != nil {
					cancel()
					return err
				}
				if built == nil || built.Service == nil {
					cancel()
					return errors.New("superagentplugin: supervisor builder returned nil")
				}
				service = built.Service
				writer = built.Writer
				stop = built.Stop
			}
			if service == nil {
				cancel()
				return errors.New("superagentplugin: supervisor service is not configured")
			}
			if err := env.Services.Provide(PluginID, Capability, service); err != nil {
				cancel()
				return err
			}
			if writer != nil {
				go writer.Run(runCtx)
			}
			mu.Lock()
			running = true
			services = env.Services
			release = stop
			stopRun = cancel
			mu.Unlock()
			logger.Info("superagent plugin active")
			return nil
		},
		Stop: func(ctx context.Context) error {
			mu.Lock()
			cancel, svc, rel := stopLocked()
			mu.Unlock()
			if cancel == nil {
				return nil
			}
			cancel()
			if svc != nil {
				svc.Remove(PluginID, Capability)
			}
			if rel != nil {
				return rel(ctx)
			}
			return nil
		},
	}
}
