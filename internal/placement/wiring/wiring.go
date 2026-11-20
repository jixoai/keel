// Package wiring assembles the placement admission gate for a runtime process.
//
// It is the one place the production edge gets its gate: the placement store,
// the strategy, the tenant run-quota resolver and the kernel edge committer are
// bound together here, so `cmd/runtime` and `cmd/keel serve` cannot drift into
// two different admission semantics. Tests build the same Runtime over a test
// database, which is why the HTTP end-to-end covers the shipped wiring rather
// than a parallel test-only path.
package wiring

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/placement"
	"github.com/jixoai/keel/internal/quota"
	"github.com/jixoai/keel/internal/store"
)

// Config wires one runtime's placement gate.
type Config struct {
	// Store is the process store (required): placement uses its pool and the
	// gate reads the session owner through the same tenant boundary.
	Store *store.Store
	// Kernel is the edge committer the gate decorates (required).
	Kernel kernel.Committer
	// Quota resolves the tenant run-concurrency limit; nil means unlimited. It
	// is the same quota document the kernel used, so the durable gate and the
	// configuration speak one vocabulary.
	Quota *quota.Limits
	// WorkerID names this runtime in a placement it creates (required).
	WorkerID string
	// Strategy is the default placement policy (pack when empty).
	Strategy placement.Strategy
	// PinGroupID is the configured pin target (pin strategy only).
	PinGroupID *uuid.UUID
	// DefaultMaxConcurrentRuns / DefaultMemoryLimitBytes configure groups the
	// automatic strategies create on demand. Zero means unlimited.
	DefaultMaxConcurrentRuns int
	DefaultMemoryLimitBytes  int64
	// LeaseTTL bounds the placement lease; zero uses the placement default.
	LeaseTTL time.Duration
	Logger   *slog.Logger
}

// Runtime is the wired admission surface.
type Runtime struct {
	// Gate admits a run (placement + slot) and releases abandoned reservations.
	Gate *placement.Gate
	// Service exposes the placement policy for diagnostics and tests.
	Service *placement.Service
	// Edge is the gated kernel committer the HTTP edge and the product turn
	// starters must use for native input.
	Edge *placement.GatedCommitter
}

// New validates the config and builds the gate.
func New(cfg Config) (*Runtime, error) {
	if cfg.Store == nil {
		return nil, errors.New("placement wiring: store is required")
	}
	if cfg.Kernel == nil {
		return nil, errors.New("placement wiring: kernel committer is required")
	}
	if strings.TrimSpace(cfg.WorkerID) == "" {
		return nil, errors.New("placement wiring: worker id is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	// Pin never guesses a group: a deployment that selects it without naming one
	// would refuse every first run with a 409, so fail the boot instead.
	if cfg.Strategy == placement.Pin && cfg.PinGroupID == nil {
		return nil, errors.New("placement wiring: the pin strategy requires a group id")
	}
	var tenantLimit func(context.Context, uuid.UUID) (int, error)
	if cfg.Quota != nil {
		limits := cfg.Quota
		tenantLimit = func(ctx context.Context, tenantID uuid.UUID) (int, error) {
			quota, err := limits.Quota(ctx, tenantID)
			if err != nil {
				// Fail closed: a quota that cannot be resolved must not admit.
				return 0, fmt.Errorf("placement wiring: resolve tenant quota: %w", err)
			}
			return quota.RunConcurrency, nil
		}
	}
	service, err := placement.New(placement.Options{
		Store:                    placement.NewPGStore(cfg.Store.Pool()),
		Strategy:                 cfg.Strategy,
		PinGroupID:               cfg.PinGroupID,
		DefaultMaxConcurrentRuns: cfg.DefaultMaxConcurrentRuns,
		DefaultMemoryLimitBytes:  cfg.DefaultMemoryLimitBytes,
		TenantRunLimit:           tenantLimit,
	})
	if err != nil {
		return nil, err
	}
	gate, err := placement.NewGate(placement.GateOptions{
		Service:      service,
		WorkerID:     cfg.WorkerID,
		Strategy:     cfg.Strategy,
		TTL:          cfg.LeaseTTL,
		SessionState: sessionState(cfg.Store),
	})
	if err != nil {
		return nil, err
	}
	edge, err := placement.NewGatedCommitter(cfg.Kernel, gate, cfg.Logger)
	if err != nil {
		return nil, err
	}
	return &Runtime{Gate: gate, Service: service, Edge: edge}, nil
}

// sessionState is the gate's session check over the store's tenant boundary.
// It is deliberately a function: the placement package must not import the
// store, and an external session must never get a placement or run row.
func sessionState(st *store.Store) placement.SessionStateFunc {
	return func(ctx context.Context, tenantID, sessionID uuid.UUID) (bool, bool, error) {
		owner, exists, err := st.SessionOwner(ctx, tenantID, sessionID)
		if err != nil {
			return false, false, err
		}
		return exists, exists && owner == store.StateOwnerExternal, nil
	}
}
