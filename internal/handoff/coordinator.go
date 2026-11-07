// Coordinator: one runtime instance's handoff state machine.
//
// It owns four things:
//
//   - the runtime identity (persistent id + loadout pin), persisted in
//     runtime_instances and used as the claim owner;
//   - the claim gate (Claiming) a worker consults before claiming more work;
//   - the drain state: mode, deadline, in-flight run count;
//   - the handoff audit rows (runtime-level drains and per-session adoptions).
//
// Everything durable goes through a Registry; everything about leases goes
// through the Kernel slice. A Coordinator without a Kernel is audit-only: it
// can register, drain and report, but Adopt fails with ErrNoKernel.
package handoff

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
)

// ErrDrainTimeout reports that a drain hit its deadline with work still in
// flight. The audit row records the timeout; the successor may force a
// takeover once the lease expires, and the stale owner's commits are rejected
// by the kernel's fencing.
var ErrDrainTimeout = errors.New("handoff: drain deadline exceeded")

// ErrStopped reports a claim/adoption attempt on a stopped coordinator.
var ErrStopped = errors.New("handoff: runtime is stopped")

// Config wires a Coordinator.
type Config struct {
	// Identity names this runtime instance (persistent id + loadout pin).
	Identity Identity
	// Registry persists runtime and handoff rows. Required.
	Registry Registry
	// Kernel is the contract-kernel slice the protocol drives (claims, fencing,
	// log reads). Optional: without it the coordinator is audit-only.
	Kernel Kernel
	// LeaseTTL is the lease a claim is issued with (default 30s).
	LeaseTTL time.Duration
	// Logger receives drain/handoff narration.
	Logger *slog.Logger
	// Clock overrides the wall clock (tests).
	Clock func() time.Time
	// OnDrain is invoked exactly once when a drain begins, before referencing
	// runtimes can observe the state flip: the runtime uses it to wake its SSE
	// drainer and schedule the stop of in-flight work. It must not block.
	OnDrain func(mode Mode, deadline time.Duration)
}

// Coordinator is the handoff state machine of one runtime instance. All
// methods are safe for concurrent use.
type Coordinator struct {
	cfg    Config
	logger *slog.Logger
	now    func() time.Time

	startedAt time.Time
	readyAt   time.Time

	claiming atomic.Bool
	inFlight atomic.Int64
	draining atomic.Bool

	mu        sync.Mutex
	mode      Mode
	deadline  time.Time
	drainAt   time.Time
	drainID   string
	reason    string
	stopped   bool
	stoppedAt time.Time
	timedOut  bool
	// drainInitiated is the atomic check-and-set guard of BeginDrain (M1):
	// concurrent triggers (admin POST × SIGTERM) serialize on it, so exactly
	// one drain row and one OnDrain wake ever happen.
	drainInitiated bool
	// drainFinished guards the drain audit row against a second finish (Stop
	// after WaitDrain, or two Stop calls).
	drainFinished bool
}

// New builds a Coordinator.
func New(cfg Config) (*Coordinator, error) {
	if cfg.Identity.ID == "" {
		return nil, errors.New("handoff: runtime id is required")
	}
	if cfg.Registry == nil {
		return nil, ErrNoRegistry
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 30 * time.Second
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Coordinator{
		cfg:       cfg,
		logger:    logger,
		now:       now,
		startedAt: now(),
	}, nil
}

// Identity returns the runtime identity.
func (c *Coordinator) Identity() Identity { return c.cfg.Identity }

// RuntimeID returns the persistent runtime id (the claim owner).
func (c *Coordinator) RuntimeID() string { return c.cfg.Identity.ID }

// LoadoutID returns the loadout id this runtime runs under.
func (c *Coordinator) LoadoutID() string { return c.cfg.Identity.LoadoutID }

// LoadoutRevision returns the pinned loadout revision.
func (c *Coordinator) LoadoutRevision() int64 { return c.cfg.Identity.Revision }

// Claiming reports whether this runtime may claim new work. It is the worker's
// claim gate: false from the moment a drain begins.
func (c *Coordinator) Claiming() bool { return c.claiming.Load() && !c.Draining() }

// Draining reports whether a drain is in progress.
func (c *Coordinator) Draining() bool { return c.draining.Load() }

// DrainMode returns the active drain mode ("" when not draining).
func (c *Coordinator) DrainMode() Mode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode
}

// DrainDeadline returns the drain deadline (zero when not draining).
func (c *Coordinator) DrainDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline
}

// AdmitsEdgeWrites reports whether the runtime keeps accepting edge-scoped
// input. True only during a handoff drain: shutdown rejects new writes and
// lets the client retry against the new runtime.
func (c *Coordinator) AdmitsEdgeWrites() bool {
	if !c.Draining() {
		return true
	}
	return c.DrainMode().AdmitsEdgeWrites()
}

// InFlight reports how many runs this runtime is executing.
func (c *Coordinator) InFlight() int { return int(c.inFlight.Load()) }

// RunStarted reports the start of one in-flight run (the worker's drain gate).
func (c *Coordinator) RunStarted() { c.inFlight.Add(1) }

// RunFinished reports the end of one in-flight run.
func (c *Coordinator) RunFinished() { c.inFlight.Add(-1) }

// StartedAt returns when the coordinator was created.
func (c *Coordinator) StartedAt() time.Time { return c.startedAt }

// Register records the runtime as starting. Durable before anything is served.
func (c *Coordinator) Register(ctx context.Context) error {
	rec := RuntimeRecord{
		RuntimeID: c.cfg.Identity.ID,
		LoadoutID: c.cfg.Identity.LoadoutID,
		Revision:  c.cfg.Identity.Revision,
		Status:    RuntimeStarting,
		StartedAt: c.startedAt,
	}
	if err := c.cfg.Registry.SaveRuntime(ctx, rec); err != nil {
		return fmt.Errorf("handoff: register runtime: %w", err)
	}
	return nil
}

// Ready marks the runtime ready and opens the claim gate. A runtime that is
// already draining never becomes claimable again.
func (c *Coordinator) Ready(ctx context.Context) error {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return ErrStopped
	}
	c.readyAt = c.now()
	rec := RuntimeRecord{
		RuntimeID: c.cfg.Identity.ID,
		LoadoutID: c.cfg.Identity.LoadoutID,
		Revision:  c.cfg.Identity.Revision,
		Status:    RuntimeReady,
		StartedAt: c.startedAt,
		ReadyAt:   c.readyAt,
	}
	stopped := c.stopped
	c.mu.Unlock()
	if stopped {
		return ErrStopped
	}
	if err := c.cfg.Registry.SaveRuntime(ctx, rec); err != nil {
		return fmt.Errorf("handoff: ready: %w", err)
	}
	c.claiming.Store(true)
	c.logger.Info("runtime ready", "runtime_id", c.cfg.Identity.ID,
		"loadout_id", c.cfg.Identity.LoadoutID, "loadout_revision", c.cfg.Identity.Revision)
	return nil
}

// BeginDrain starts the runtime's drain exactly once.
//
// shutdown: the claim gate closes; the runtime rejects new writes (the HTTP
// layer answers 503) and in-flight runs may be abandoned. handoff: the claim
// gate closes, but edge-scoped input keeps landing and in-flight runs drain to
// a safe point or the deadline. Both modes persist a runtime-level handoff row
// so the two are distinguishable in the audit.
func (c *Coordinator) BeginDrain(ctx context.Context, mode Mode, deadline time.Duration) (Status, error) {
	if !mode.Valid() {
		return c.Status(), fmt.Errorf("handoff: unknown drain mode %q", mode)
	}
	if deadline <= 0 {
		deadline = 30 * time.Second
	}

	// The durable record and the state flip are ordered so that a persistence
	// failure can never leave a half-drained zombie: the audit row is written
	// first (with a context that survives the caller disconnecting), and only
	// then does the runtime stop claiming and start draining. The initiation
	// itself is a single check-and-set under the mutex, so two triggers cannot
	// both write a row or wake the drainer twice.
	now := c.now()
	deadlineAt := now.Add(deadline)
	drainID := uuid.NewString()
	startedAt := now
	inFlight := c.InFlight()

	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return c.Status(), ErrStopped
	}
	if c.drainInitiated {
		c.mu.Unlock()
		return c.Status(), ErrAlreadyDraining
	}
	c.drainInitiated = true
	wasClaiming := c.claiming.Load()
	c.mu.Unlock()

	// Stop claiming immediately: the operator asked for a drain, and a claim
	// that lands between the request and the durable write would be one the
	// successor has to take over.
	c.claiming.Store(false)

	// The durable write must not be cancelled by the HTTP caller hanging up; a
	// bounded background budget is enough for two small upserts.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	record := HandoffRecord{
		ID:          drainID,
		Mode:        mode,
		FromRuntime: c.cfg.Identity.ID,
		LoadoutID:   c.cfg.Identity.LoadoutID,
		Revision:    c.cfg.Identity.Revision,
		Status:      HandoffStarted,
		InFlight:    inFlight,
		StartedAt:   startedAt,
		Detail:      fmt.Sprintf("drain %s begun with %d runs in flight", mode, inFlight),
	}
	if err := c.cfg.Registry.SaveHandoff(persistCtx, record); err != nil {
		c.rollbackDrainInitiation(wasClaiming, drainID, false)
		return c.Status(), fmt.Errorf("handoff: record drain: %w", err)
	}
	if err := c.cfg.Registry.SaveRuntime(persistCtx, RuntimeRecord{
		RuntimeID: c.cfg.Identity.ID,
		LoadoutID: c.cfg.Identity.LoadoutID,
		Revision:  c.cfg.Identity.Revision,
		Status:    RuntimeDraining,
		Mode:      mode,
		Deadline:  deadlineAt,
		InFlight:  inFlight,
		StartedAt: c.startedAt,
		ReadyAt:   c.readyAt,
		DrainAt:   startedAt,
	}); err != nil {
		c.rollbackDrainInitiation(wasClaiming, drainID, true)
		return c.Status(), fmt.Errorf("handoff: record drain state: %w", err)
	}

	// The durable audit exists: flip the runtime state and wake the runtime
	// (SSE drain, worker stop).
	c.mu.Lock()
	c.mode = mode
	c.deadline = deadlineAt
	c.drainAt = startedAt
	c.drainID = drainID
	c.draining.Store(true)
	c.mu.Unlock()
	c.logger.Info("drain begin", "runtime_id", c.cfg.Identity.ID, "mode", mode,
		"deadline", deadlineAt, "in_flight", inFlight, "handoff_id", drainID)

	if c.cfg.OnDrain != nil {
		c.cfg.OnDrain(mode, deadline)
	}
	return c.Status(), nil
}

// rollbackDrainInitiation undoes a drain whose durable record could not be
// written: the runtime keeps claiming (it never actually drained) and a retry
// is allowed. The audit row, when it was already written, is closed instead of
// being left dangling as "started".
func (c *Coordinator) rollbackDrainInitiation(wasClaiming bool, drainID string, recordWritten bool) {
	c.claiming.Store(wasClaiming)
	c.mu.Lock()
	c.drainInitiated = false
	c.mu.Unlock()
	if !recordWritten {
		return
	}
	// Best effort: a row that was already written must not stay "started".
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.cfg.Registry.FinishHandoff(ctx, drainID, HandoffTimedOut, c.InFlight(),
		"drain initiation failed: the runtime row could not be written", c.now()); err != nil {
		c.logger.Warn("close failed drain row", "err", err, "handoff_id", drainID)
	}
}

// WaitDrain blocks until every in-flight run has finished or the drain
// deadline passes. A deadline with work still in flight is recorded as a
// timeout (the spec's "旧 runtime 不排水" scenario): the run's lease expires,
// the successor force-takes-over and the old owner's commits are fenced.
// WaitDrain returns nil for a clean drain and ErrDrainTimeout otherwise.
func (c *Coordinator) WaitDrain(ctx context.Context) error {
	c.mu.Lock()
	if !c.draining.Load() {
		c.mu.Unlock()
		return ErrNotDraining
	}
	deadline := c.deadline
	drainID := c.drainID
	c.mu.Unlock()

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		if c.InFlight() == 0 {
			return c.finishDrain(ctx, drainID, HandoffCompleted, "in-flight runs drained")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			inFlight := c.InFlight()
			c.mu.Lock()
			c.timedOut = true
			c.mu.Unlock()
			c.logger.Warn("drain deadline exceeded", "runtime_id", c.cfg.Identity.ID,
				"mode", c.DrainMode(), "in_flight", inFlight, "handoff_id", drainID)
			return c.finishDrain(ctx, drainID, HandoffTimedOut,
				fmt.Sprintf("deadline exceeded with %d runs in flight; leases expire and the successor force-takes-over", inFlight))
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// finishDrain closes the drain: the drain audit row gets its final status, the
// runtime row becomes stopped, and only the first caller does it (a Stop after
// WaitDrain, or a second WaitDrain, is a no-op on the audit).
func (c *Coordinator) finishDrain(ctx context.Context, drainID, status, detail string) error {
	c.mu.Lock()
	if c.drainFinished || c.stopped {
		c.mu.Unlock()
		return drainOutcome(status)
	}
	c.drainFinished = true
	inFlight := c.InFlight()
	rec := RuntimeRecord{
		RuntimeID: c.cfg.Identity.ID,
		LoadoutID: c.cfg.Identity.LoadoutID,
		Revision:  c.cfg.Identity.Revision,
		Status:    RuntimeStopped,
		Mode:      c.mode,
		Deadline:  c.deadline,
		InFlight:  inFlight,
		StartedAt: c.startedAt,
		ReadyAt:   c.readyAt,
		DrainAt:   c.drainAt,
		StoppedAt: c.now(),
		Reason:    detail,
	}
	mode := c.mode
	c.mu.Unlock()

	// The audit writes must survive the caller: a drained runtime is expected to
	// exit right after, and these rows are the only record of the handoff.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := c.cfg.Registry.FinishHandoff(writeCtx, drainID, status, inFlight, detail, c.now()); err != nil {
		c.logger.Warn("finish drain audit", "err", err, "handoff_id", drainID)
	}
	if err := c.cfg.Registry.SaveRuntime(writeCtx, rec); err != nil {
		c.logger.Warn("finish drain runtime row", "err", err)
	}
	c.logger.Info("drain complete", "runtime_id", c.cfg.Identity.ID, "mode", mode,
		"status", status, "in_flight", inFlight, "detail", detail)
	return drainOutcome(status)
}

// drainOutcome maps the closing status onto WaitDrain's error contract.
func drainOutcome(status string) error {
	if status == HandoffTimedOut {
		return ErrDrainTimeout
	}
	return nil
}

// Stop marks the runtime stopped. It also closes the drain audit row when the
// runtime never ran WaitDrain to completion — the SIGTERM/shutdown path exits
// through here, and a row left at "started" with no finished_at hides how the
// drain actually ended.
func (c *Coordinator) Stop(ctx context.Context, reason string) error {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return nil
	}
	c.stopped = true
	c.stoppedAt = c.now()
	c.reason = reason
	drainID := c.drainID
	closeDrain := c.drainInitiated && !c.drainFinished && drainID != ""
	if closeDrain {
		c.drainFinished = true
	}
	timedOut := c.timedOut
	inFlight := c.InFlight()
	rec := RuntimeRecord{
		RuntimeID: c.cfg.Identity.ID,
		LoadoutID: c.cfg.Identity.LoadoutID,
		Revision:  c.cfg.Identity.Revision,
		Status:    RuntimeStopped,
		Mode:      c.mode,
		Deadline:  c.deadline,
		InFlight:  inFlight,
		StartedAt: c.startedAt,
		ReadyAt:   c.readyAt,
		DrainAt:   c.drainAt,
		StoppedAt: c.stoppedAt,
		Reason:    reason,
	}
	c.mu.Unlock()
	c.claiming.Store(false)
	if closeDrain {
		status := HandoffCompleted
		if timedOut {
			status = HandoffTimedOut
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := c.cfg.Registry.FinishHandoff(writeCtx, drainID, status, inFlight, reason, c.now()); err != nil {
			c.logger.Warn("close drain row on stop", "err", err, "handoff_id", drainID)
		}
	}
	c.logger.Info("runtime stopped", "runtime_id", c.cfg.Identity.ID, "reason", reason)
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return c.cfg.Registry.SaveRuntime(writeCtx, rec)
}

// TimedOut reports whether the last drain exceeded its deadline.
func (c *Coordinator) TimedOut() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.timedOut
}

// Status is the runtime's state snapshot.
func (c *Coordinator) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Status{
		Identity:     c.cfg.Identity,
		Draining:     c.draining.Load(),
		Mode:         c.mode,
		Deadline:     c.deadline,
		InFlight:     c.InFlight(),
		Claiming:     c.Claiming(),
		StartedAt:    c.startedAt,
		ReadyAt:      c.readyAt,
		DrainStarted: c.drainAt,
		StoppedAt:    c.stoppedAt,
		Reason:       c.reason,
	}
}

// Adoption is the successor side of the protocol (see handoff.go). Adopt
// claims the session for this runtime, waits (bounded by wait) for the previous
// owner's lease to clear, pins the loadout, rebuilds the resume state from the
// committed log and records the handoff audit row.
func (c *Coordinator) Adopt(ctx context.Context, scope kernel.Scope, wait time.Duration) (Adoption, error) {
	if c.cfg.Kernel == nil {
		return Adoption{}, ErrNoKernel
	}
	if scope.IsZero() {
		return Adoption{}, fmt.Errorf("handoff: tenant and session are required")
	}
	c.mu.Lock()
	stopped := c.stopped
	c.mu.Unlock()
	if stopped {
		return Adoption{}, ErrStopped
	}

	started := c.now()
	var previous kernel.OwnerToken
	if state, err := c.cfg.Kernel.LeaseState(ctx, scope); err == nil {
		previous = state.Lease.Token
	} else if !errors.Is(err, kernel.ErrNotFound) {
		return Adoption{}, fmt.Errorf("handoff: lease state: %w", err)
	}

	deadline := started.Add(wait)
	var lease kernel.Lease
	for {
		var err error
		lease, err = c.cfg.Kernel.ClaimScope(ctx, scope, c.cfg.Identity.ID, c.cfg.LeaseTTL)
		if err == nil {
			break
		}
		switch {
		case errors.Is(err, kernel.ErrNoRunnable):
			return Adoption{}, fmt.Errorf("handoff: adopt %s: %w", scope, ErrNothingToAdopt)
		case errors.Is(err, kernel.ErrLeaseHeld), errors.Is(err, kernel.ErrLeaseExpired), errors.Is(err, kernel.ErrLeaseLost):
			// The previous owner has not released yet. Wait for release or
			// expiry; a cancelled context stops the wait.
			if wait <= 0 || !c.now().Before(deadline) {
				return Adoption{}, fmt.Errorf("handoff: adopt %s: %w", scope, err)
			}
			select {
			case <-ctx.Done():
				return Adoption{}, ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
		default:
			return Adoption{}, fmt.Errorf("handoff: adopt %s: %w", scope, err)
		}
	}
	waited := c.now().Sub(started)

	if err := c.cfg.Kernel.PinRunLoadout(ctx, lease, c.cfg.Identity.LoadoutID, c.cfg.Identity.Revision); err != nil {
		// A claim without its pin is not usable: give it back instead of
		// leaving a dangling owner behind.
		if relErr := c.cfg.Kernel.Release(ctx, lease); relErr != nil {
			c.logger.Warn("release after failed pin", "err", relErr, "scope", scope.String())
		}
		return Adoption{}, fmt.Errorf("handoff: pin loadout: %w", err)
	}

	// From here on the claim is ours: a failure must give it back, or the
	// session stays owned by a runtime that never adopted it until the lease
	// expires.
	release := func(cause error) (Adoption, error) {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if relErr := c.cfg.Kernel.Release(releaseCtx, lease); relErr != nil {
			c.logger.Warn("release after failed adoption", "err", relErr, "scope", scope.String(), "cause", cause)
		}
		return Adoption{}, cause
	}
	rebuild, err := c.Rebuild(ctx, scope)
	if err != nil {
		return release(err)
	}

	adoption := Adoption{Lease: lease, Previous: previous, Rebuild: rebuild, Waited: waited}
	adoption.FirstClaim = previous.IsZero()
	if !adoption.FirstClaim && previous.Owner != c.cfg.Identity.ID {
		record := HandoffRecord{
			ID:          uuid.NewString(),
			TenantID:    scope.TenantID,
			SessionID:   scope.SessionID,
			Mode:        ModeHandoff,
			FromRuntime: previous.Owner,
			ToRuntime:   c.cfg.Identity.ID,
			FromEpoch:   previous.Epoch,
			ToEpoch:     lease.Token.Epoch,
			LoadoutID:   c.cfg.Identity.LoadoutID,
			Revision:    c.cfg.Identity.Revision,
			Status:      HandoffCompleted,
			StartedAt:   started,
			FinishedAt:  c.now(),
			Detail: fmt.Sprintf("adopted session: epoch %d -> %d, %d events replayed, %d unresolved tool calls, waited %s",
				previous.Epoch, lease.Token.Epoch, rebuild.Events, len(rebuild.Unresolved), waited),
		}
		if err := c.cfg.Registry.SaveHandoff(ctx, record); err != nil {
			return release(fmt.Errorf("handoff: record adoption: %w", err))
		}
		adoption.HandoffID = record.ID
		c.logger.Info("session adopted", "runtime_id", c.cfg.Identity.ID, "scope", scope.String(),
			"from_runtime", previous.Owner, "from_epoch", previous.Epoch, "to_epoch", lease.Token.Epoch,
			"loadout_revision", c.cfg.Identity.Revision, "rebuild", rebuild.String(), "handoff_id", record.ID)
	} else {
		c.logger.Info("session claimed", "runtime_id", c.cfg.Identity.ID, "scope", scope.String(),
			"epoch", lease.Token.Epoch, "rebuild", rebuild.String())
	}
	return adoption, nil
}

// Rebuild reconstructs the resume state from the committed event log. Only
// committed events are visible: a model partial that was never committed does
// not exist here, which is the "不出现半条消息" guarantee.
func (c *Coordinator) Rebuild(ctx context.Context, scope kernel.Scope) (Rebuild, error) {
	if c.cfg.Kernel == nil {
		return Rebuild{}, ErrNoKernel
	}
	var out Rebuild
	var after int64
	for {
		events, err := c.cfg.Kernel.LoadEvents(ctx, scope, after, 500)
		if err != nil {
			return Rebuild{}, fmt.Errorf("handoff: rebuild: %w", err)
		}
		if len(events) == 0 {
			break
		}
		for _, ev := range events {
			out.Events++
			out.LastKind = ev.Kind
			out.Cursor = ev.Seq
			if edgeKind(ev.Kind) {
				out.LastInputSeq = ev.Seq
			}
		}
		after = events[len(events)-1].Seq
		if len(events) < 500 {
			break
		}
	}
	unresolved, err := c.cfg.Kernel.UnresolvedToolCalls(ctx, scope)
	if err != nil {
		return Rebuild{}, fmt.Errorf("handoff: rebuild unresolved calls: %w", err)
	}
	out.Unresolved = unresolved
	// Whether the next attempt owes a turn is a fact of the run row, never a
	// guess from the log tail: the tail is an assistant message whenever the
	// previous attempt committed an answer, even one that covers only input
	// committed BEFORE its read point (adversarial review N9/B5).
	pin, err := c.cfg.Kernel.RunPin(ctx, scope)
	switch {
	case err == nil:
		out.ReadWatermark = pin.ReadWatermark
		switch {
		case out.LastKind == kernel.KindUserMessage:
			// The log ends with input: no answer was committed for it, whatever
			// the read watermark says (an attempt read it and died mid-answer).
			out.PendingUserInput = true
		case pin.ReadWatermark > 0:
			out.PendingUserInput = out.LastInputSeq > pin.ReadWatermark
		default:
			// No attempt has proven a read point yet: the edge-written flag is
			// the only evidence, and it is never cleared by a claim.
			out.PendingUserInput = pin.PendingInput
		}
	case errors.Is(err, kernel.ErrNoRunnable):
		// No run row (an external session, or a session with nothing queued):
		// no attempt owes a turn; the tail still tells an operator what the log
		// ends with.
		out.PendingUserInput = edgeKind(out.LastKind)
	default:
		return Rebuild{}, fmt.Errorf("handoff: rebuild run row: %w", err)
	}
	return out, nil
}

// Release gives a claim back without settling the run. A drained runtime uses
// it to hand the session over immediately instead of waiting for the lease to
// expire.
func (c *Coordinator) Release(ctx context.Context, lease kernel.Lease) error {
	if c.cfg.Kernel == nil {
		return ErrNoKernel
	}
	if err := c.cfg.Kernel.Release(ctx, lease); err != nil {
		return fmt.Errorf("handoff: release: %w", err)
	}
	return nil
}

// SyncRuntimeRow rewrites the runtime row with the current in-flight count.
// The runtime calls it when the count changes in a way an operator cares about
// (a busy drain, for example).
func (c *Coordinator) SyncRuntimeRow(ctx context.Context) error {
	c.mu.Lock()
	rec := RuntimeRecord{
		RuntimeID: c.cfg.Identity.ID,
		LoadoutID: c.cfg.Identity.LoadoutID,
		Revision:  c.cfg.Identity.Revision,
		Status:    RuntimeReady,
		InFlight:  c.InFlight(),
		StartedAt: c.startedAt,
		ReadyAt:   c.readyAt,
	}
	if c.draining.Load() {
		rec.Status = RuntimeDraining
		rec.Mode = c.mode
		rec.Deadline = c.deadline
		rec.DrainAt = c.drainAt
	}
	c.mu.Unlock()
	return c.cfg.Registry.SaveRuntime(ctx, rec)
}
