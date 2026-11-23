// Runtime admission gate: the one place a native run is admitted into a group
// before it is created. The gate resolves an unplaced session through the
// configured strategy, then asks AdmitRun to reserve the group/tenant slot and
// write the pending run row in one database transaction.
//
// The gate is a façade over Service on purpose: Service stays the pure policy
// layer (it refuses an unplaced session with a typed conflict), while the
// runtime needs the automatic placement step first. Keeping them separate means
// a caller cannot accidentally "admit" without a group and a test can assert
// both halves independently.
package placement

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/kernel"
)

// SessionStateFunc answers whether the tenant owns the session and whether an
// external backend owns its state. The runtime supplies it (the placement
// package owns no session state); a missing or external session must never get
// a placement row or a run row.
type SessionStateFunc func(ctx context.Context, tenantID, sessionID uuid.UUID) (exists, external bool, err error)

// Gate errors that are not placement conflicts.
var (
	// ErrSessionNotFound reports that the tenant does not own the session. The
	// HTTP edge maps it to 404 without leaking existence.
	ErrSessionNotFound = errors.New("placement: session not found")
	// ErrExternalSession reports a session whose state an external backend owns:
	// it never enters the native placement/run flow.
	ErrExternalSession = errors.New("placement: session is owned by an external backend")
)

// GateOptions wires the runtime gate.
type GateOptions struct {
	// Service is the placement policy/service (required).
	Service *Service
	// WorkerID names this runtime process in a placement created by the gate.
	// It is the lease holder for the automatic placement; required.
	WorkerID string
	// Strategy is the placement policy for an unplaced session. Empty uses the
	// service's configured default strategy.
	Strategy Strategy
	// TTL bounds the automatic placement lease; zero uses the service default.
	TTL time.Duration
	// SessionState is the session existence/owner check (required). The gate
	// fails closed: without it an unplaced session could get a run row that no
	// native worker may ever claim.
	SessionState SessionStateFunc
	// Logger reports settlement-hook failures (duration observation, queue
	// promotion). Nil uses slog.Default.
	Logger *slog.Logger
}

// EnsureRequest creates a placement when the session has none. It never steals
// a live lease: an existing placement is returned as-is, whatever its holder.
type EnsureRequest struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	// GroupID, when set, is the group to create the placement in. Nil resolves
	// the strategy (pack/user/pin).
	GroupID *uuid.UUID
	// Strategy overrides the service default for this call.
	Strategy Strategy
	// WorkerID is the lease holder of the created placement (required when a
	// placement is created).
	WorkerID string
	// TTL bounds the created lease; zero uses the service default.
	TTL time.Duration
}

// EnsurePlacement returns the session's current placement, creating it from the
// strategy when absent. The read-then-create is a compare-and-set: two processes
// racing for one session produce one placement, and the loser reads the winner
// instead of returning a conflict (the caller is not asserting authority; it
// only needs the group).
func (s *Service) EnsurePlacement(ctx context.Context, req EnsureRequest) (Placement, error) {
	if err := validatePlacementIDs(req.TenantID, req.SessionID); err != nil {
		return Placement{}, err
	}
	current, err := s.store.GetPlacement(ctx, req.TenantID, req.SessionID)
	if err == nil {
		return current, nil
	}
	if !errors.Is(err, ErrPlacementNotFound) {
		return Placement{}, err
	}
	if req.WorkerID == "" {
		return Placement{}, errors.New("placement: ensuring a placement requires a worker id")
	}
	groupID := req.GroupID
	if groupID == nil {
		id, err := s.resolveStrategyGroup(ctx, req.TenantID, req.SessionID, s.strategyFor(req.Strategy))
		if err != nil {
			return Placement{}, err
		}
		groupID = &id
	}
	created, err := s.store.ClaimPlacement(ctx, claimWrite{
		tenantID:      req.TenantID,
		sessionID:     req.SessionID,
		groupID:       groupID,
		expectedEpoch: 0,
		workerID:      req.WorkerID,
		ttl:           s.ttl(req.TTL),
		moveReason:    ReasonLeaseHeld,
	})
	if err == nil {
		return created, nil
	}
	// A concurrent creator won the insert. Its placement is the answer; a stale
	// epoch here is a race, not a decision, so the caller must not see a 409.
	var conflict *Conflict
	if errors.As(err, &conflict) && conflict.Reason == ReasonEpochMismatch {
		if winner, getErr := s.store.GetPlacement(ctx, req.TenantID, req.SessionID); getErr == nil {
			return winner, nil
		}
	}
	return Placement{}, err
}

// Gate is the runtime's admission façade: ensure a placement, then reserve the
// run slot. It is safe for concurrent use.
type Gate struct {
	service      *Service
	workerID     string
	strategy     Strategy
	ttl          time.Duration
	sessionState SessionStateFunc
	logger       *slog.Logger
}

// NewGate validates the options and builds the gate.
func NewGate(opts GateOptions) (*Gate, error) {
	if opts.Service == nil {
		return nil, errors.New("placement: gate requires a service")
	}
	if opts.WorkerID == "" {
		return nil, errors.New("placement: gate requires a worker id")
	}
	if opts.SessionState == nil {
		return nil, errors.New("placement: gate requires a session state check")
	}
	strategy := opts.Strategy
	if strategy == "" {
		strategy = opts.Service.strategy
	}
	switch strategy {
	case Pack, User, Pin:
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidStrategy, strategy)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Gate{
		service:      opts.Service,
		workerID:     opts.WorkerID,
		strategy:     strategy,
		ttl:          opts.TTL,
		sessionState: opts.SessionState,
		logger:       logger,
	}, nil
}

// Service exposes the wired service for diagnostics and tests.
func (g *Gate) Service() *Service { return g.service }

// Strategy is the policy used for an unplaced session.
func (g *Gate) Strategy() Strategy { return g.strategy }

// QueueState reads the session's durable queue entry (Queued=false when the run
// is not waiting). The HTTP edge reads it after a commit so a queued admission
// answers 202 with its position and ETA.
func (g *Gate) QueueState(ctx context.Context, tenantID, sessionID uuid.UUID) (QueueState, error) {
	return g.service.QueueState(ctx, tenantID, sessionID)
}

// RunSettled is the worker's settlement observer (agent.SettlementObserver).
// A settled attempt frees a group slot, so the queue head is promoted now
// instead of waiting for the recovery sweep; the observed duration is the ETA
// history. Failures never affect the settled run: they are logged and the
// sweep converges.
func (g *Gate) RunSettled(ctx context.Context, scope kernel.Scope, status string, duration time.Duration) {
	if scope.IsZero() {
		return
	}
	if err := g.service.ObserveRunDuration(ctx, scope.TenantID, scope.SessionID, duration); err != nil {
		g.logger.Warn("observe run duration for the placement ETA",
			"tenant_id", scope.TenantID, "session_id", scope.SessionID,
			"status", status, "duration_ms", duration.Milliseconds(), "err", err)
	}
	promoted, err := g.service.PromoteQueued(ctx, scope.TenantID)
	if err != nil {
		g.logger.Warn("promote the placement queue after a settled run",
			"tenant_id", scope.TenantID, "session_id", scope.SessionID, "status", status, "err", err)
		return
	}
	if len(promoted) > 0 {
		g.logger.Info("promoted queued runs after a settled run",
			"tenant_id", scope.TenantID, "session_id", scope.SessionID,
			"status", status, "promoted", len(promoted))
	}
}

// SweepQueues is the crash-recovery pass: queued rows written by a previous
// process are promoted (FIFO, capacity-guarded) even though no settlement in
// this process triggered it. It returns what it promoted.
func (g *Gate) SweepQueues(ctx context.Context) ([]Promotion, error) {
	return g.service.PromoteAllQueued(ctx)
}

// Admit places an unplaced session (strategy pack by default) and admits its
// run: a slot when the group has room, otherwise a durable FIFO queue entry
// with position and ETA. The tenant quota is a typed *CapacityError (hard
// refusal); a draining/retired group, a foreign session or a live lease move is
// a typed *Conflict; a memory-bounded group without an estimate is a typed
// *UnsupportedError. Nothing is silently dropped: a refused admission writes no
// run row, and a queued one is never claimable before promotion.
func (g *Gate) Admit(ctx context.Context, tenantID, sessionID uuid.UUID) (Admission, error) {
	if err := validatePlacementIDs(tenantID, sessionID); err != nil {
		return Admission{}, err
	}
	exists, external, err := g.sessionState(ctx, tenantID, sessionID)
	if err != nil {
		return Admission{}, fmt.Errorf("placement: resolve session: %w", err)
	}
	if !exists {
		return Admission{}, ErrSessionNotFound
	}
	if external {
		return Admission{}, ErrExternalSession
	}

	_, err = g.service.store.GetPlacement(ctx, tenantID, sessionID)
	switch {
	case err == nil:
	case errors.Is(err, ErrPlacementNotFound):
		if _, err := g.service.EnsurePlacement(ctx, EnsureRequest{
			TenantID: tenantID, SessionID: sessionID, Strategy: g.strategy,
			WorkerID: g.workerID, TTL: g.ttl,
		}); err != nil {
			return Admission{}, err
		}
	default:
		return Admission{}, err
	}

	admission, err := g.service.AdmitRun(ctx, AdmitRequest{TenantID: tenantID, SessionID: sessionID})
	if err == nil {
		return admission, nil
	}
	// The placement can be removed by a drain/leave between the read and the
	// admission. Re-ensure once: the session is still valid, so the run should
	// land in the strategy's current group rather than fail with a stale 409.
	var conflict *Conflict
	if errors.As(err, &conflict) && conflict.Reason == ReasonPlacementNotFound {
		if _, ensureErr := g.service.EnsurePlacement(ctx, EnsureRequest{
			TenantID: tenantID, SessionID: sessionID, Strategy: g.strategy,
			WorkerID: g.workerID, TTL: g.ttl,
		}); ensureErr != nil {
			return Admission{}, ensureErr
		}
		return g.service.AdmitRun(ctx, AdmitRequest{TenantID: tenantID, SessionID: sessionID})
	}
	return Admission{}, err
}

// Release frees an admitted-but-abandoned reservation (the input never
// committed, so the pending/queued row would otherwise execute an empty
// attempt) and promotes the next FIFO queue head. Only an unstarted row with no
// unread input is released; a running run is settled by the kernel.
func (g *Gate) Release(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	return g.service.ReleaseRun(ctx, tenantID, sessionID)
}
