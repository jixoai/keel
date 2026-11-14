package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

// RunObserver is the optional metrics seam of a Worker. It keeps the worker
// free of a metrics implementation; *metrics.Set satisfies it. Attempt metrics
// cover every execution; RunFinished is called only for a committed terminal
// state (done/failed).
type RunObserver interface {
	RunStarted()
	RunAttempt(outcome string, duration time.Duration)
	RunFinished(status string)
	FinishFailed()
	LeaseExpired()
}

// RunMirror reads the legacy run-row mirror (status, attempts, cursor). The
// kernel keeps that mirror in sync inside the same transactions it settles with,
// so it is safe to read, but it is not the write path: leases and run state are
// owned by kernel.LeaseManager. *store.Store satisfies it.
type RunMirror interface {
	GetRun(ctx context.Context, tenantID, sessionID uuid.UUID) (store.Run, error)
}

// LoadoutRef identifies the immutable service loadout a run pinned: the
// snapshot header plus the in-process generation that served it. It is run
// metadata (recorded with the settled run), never a routing input.
type LoadoutRef struct {
	ID         string
	Revision   int64
	Generation int64
}

// BoundRun is one run's pinned service generation: the loop/coordinator
// instance the run must use, the loadout it pinned, and the reference that
// keeps a retiring generation alive until the attempt settles.
type BoundRun interface {
	// Runner is the loop+coordinator instance of the pinned generation.
	Runner() *Runner
	// Loadout is the snapshot identity recorded with the run.
	Loadout() LoadoutRef
	// Release drops the pin. It is idempotent and must be called when the
	// attempt settles (or is abandoned).
	Release(ctx context.Context) error
}

// LoadoutPinner records the pinned loadout on the claim, the way the handoff
// audit expects it (kernel.Kernel implements it). It is optional: a store
// without run pins simply leaves the metadata to the settle.
type LoadoutPinner interface {
	PinRunLoadout(ctx context.Context, lease kernel.Lease, loadoutID string, revision int64) error
}

// RunBinding pins a service generation for each run. A hosting process that
// assembles its services through a graph supplies this so a run started before
// a reassembly keeps the instances it began with; a run started afterwards
// gets the new generation. Without a binding the worker uses its fixed Runner.
type RunBinding interface {
	Acquire(ctx context.Context) (BoundRun, error)
}

// DrainGate is the worker's drain seam (internal/handoff's Coordinator
// satisfies it). Claiming() is checked before every claim, so a draining
// runtime stops picking up work without killing the runs already executing;
// RunStarted/RunFinished report the in-flight count so a handoff drain can wait
// for it (bounded by the drain deadline) instead of guessing.
type DrainGate interface {
	Claiming() bool
	RunStarted()
	RunFinished()
}

// RunController is the optional seam that lets an external caller stop a
// locally executing attempt (the product layer's cancel endpoint). The worker
// registers every attempt; the returned context is cancelled by the caller, and
// finish must be called exactly once with the attempt outcome ("done",
// "failed", "cancelled" or "abandoned"). The interface is deliberately one
// method: the kernel side stays unaware of what a cancel means.
type RunController interface {
	BeginRun(parent context.Context, scope kernel.Scope, runID string) (context.Context, func(outcome string))
}

// Worker claims runs through the contract kernel and executes them with a
// bounded concurrency. The bound is what keeps 1000 online sessions from
// turning into 1000 simultaneous model streams.
//
// Claim / Renew / Finish / Release are the kernel's lease verbs: store.ClaimRun,
// store.RenewLease and store.FinishRun are retired from the production path
// (they do not take the session lock and do not check the fencing token; the
// deprecated stubs remain only for legacy readers and tests).
type Worker struct {
	// Leases is the kernel lease seam. Required.
	Leases kernel.LeaseManager
	// Runs reads the run-row mirror for logs and metrics only.
	Runs   RunMirror
	Runner *Runner
	// Binding, when set, pins the service generation per run and supplies the
	// loop instance of that generation. Runner is the fallback for hosts that
	// do not assemble services through a graph.
	Binding RunBinding
	// Owner is the runtime identity written into every claim (the fencing owner
	// and the audit trail). It is stable for the lifetime of the process.
	Owner       string
	Concurrency int
	Lease       time.Duration
	Poll        time.Duration
	MaxAttempts int
	Logger      *slog.Logger
	// Metrics, when set, observes claimed/finished/failed runs and expired
	// leases.
	Metrics RunObserver
	// DrainGate, when set, closes the claim path during a drain and reports
	// in-flight runs. Nil keeps the ungated behavior.
	DrainGate DrainGate
	// Control, when set, registers each executing attempt so an external caller
	// can stop it (the product's cancel endpoint). Nil means no cancel seam.
	Control RunController
	// Committer, when set, records the attempt summary as run.finished under the
	// run's lease right before the settle. Nil keeps the worker summary-free.
	Committer kernel.Committer
}

// NewWorker builds a worker with defaults sized for a 2GB host. leases is the
// contract kernel's lease seam, runs the run-row mirror used for logs/metrics,
// and owner the runtime identity recorded in every claim.
func NewWorker(leases kernel.LeaseManager, runs RunMirror, runner *Runner, owner string, concurrency int, logger *slog.Logger) *Worker {
	if concurrency <= 0 {
		concurrency = 64
	}
	if logger == nil {
		logger = slog.Default()
	}
	if owner == "" {
		hostname, _ := os.Hostname()
		owner = hostname + "-" + strconv.Itoa(os.Getpid())
	}
	lease := 30 * time.Second
	if ms, _ := strconv.Atoi(os.Getenv("KEEL_LEASE_MS")); ms > 0 {
		lease = time.Duration(ms) * time.Millisecond
	}
	return &Worker{
		Leases:      leases,
		Runs:        runs,
		Runner:      runner,
		Owner:       owner,
		Concurrency: concurrency,
		Lease:       lease,
		Poll:        200 * time.Millisecond,
		MaxAttempts: 3,
		Logger:      logger,
	}
}

// Run blocks until ctx is cancelled. Each claim runs in its own goroutine; the
// semaphore is the only queue (the database is the durable queue).
func (w *Worker) Run(ctx context.Context) {
	sem := make(chan struct{}, w.Concurrency)
	ticker := time.NewTicker(w.Poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// A draining runtime stops claiming new work immediately: the gate is
		// the handoff promise ("the old runtime stops taking work while the new
		// one takes over"). Without this check the drain waits forever on runs
		// the draining process itself keeps starting.
		if w.DrainGate != nil && !w.DrainGate.Claiming() {
			continue
		}
		select {
		case sem <- struct{}{}:
		default:
			continue // all slots busy; do not claim more work
		}
		lease, err := w.Leases.Claim(ctx, w.Owner, w.Lease)
		if err != nil {
			<-sem
			if !errors.Is(err, kernel.ErrNoRunnable) && ctx.Err() == nil {
				w.Logger.Warn("claim run", "err", err)
			}
			continue
		}
		if lease.Reclaimed {
			w.countLeaseExpired()
		}
		go w.execute(ctx, sem, lease)
	}
}

func (w *Worker) countLeaseExpired() {
	if w.Metrics != nil {
		w.Metrics.LeaseExpired()
	}
}

// isFenceError reports whether err means the claim is no longer ours: another
// owner claimed the session (stale epoch), the lease expired, or it was
// released. A run that sees this must stop and discard its local results
// instead of retrying — the new owner is responsible for the session.
func isFenceError(err error) bool {
	return errors.Is(err, kernel.ErrStaleEpoch) ||
		errors.Is(err, kernel.ErrLeaseExpired) ||
		errors.Is(err, kernel.ErrLeaseLost)
}

func (w *Worker) execute(ctx context.Context, sem chan struct{}, lease kernel.Lease) {
	defer func() { <-sem }()
	runID := uuid.NewString()
	started := time.Now()
	attempts := w.attemptOf(ctx, lease.Scope)
	if w.Metrics != nil {
		w.Metrics.RunStarted()
	}
	if w.DrainGate != nil {
		w.DrainGate.RunStarted()
		defer w.DrainGate.RunFinished()
	}
	settled := false
	defer func() {
		if w.Metrics != nil && !settled {
			w.Metrics.RunAttempt("abandoned", time.Since(started))
		}
	}()

	// Pin the service generation this run executes under. The run keeps the
	// instances it began with even if the host reassembles services later; a
	// retiring generation stays alive until this attempt settles.
	runner := w.Runner
	var pinned LoadoutRef
	if w.Binding != nil {
		bound, err := w.Binding.Acquire(ctx)
		if err != nil {
			if ctx.Err() != nil {
				w.Logger.Info("run abandoned for shutdown", "run_id", runID,
					"tenant_id", lease.Scope.TenantID, "session_id", lease.Scope.SessionID, "attempt", attempts)
				w.releaseAbandoned(lease)
				return
			}
			settled = true
			w.Logger.Error("pin service loadout", "run_id", runID,
				"tenant_id", lease.Scope.TenantID, "session_id", lease.Scope.SessionID, "err", err)
			if finishErr := w.Leases.Finish(ctx, kernel.FinishRequest{
				Lease:       lease,
				Err:         fmt.Errorf("pin service loadout: %w", err),
				MaxAttempts: w.MaxAttempts,
			}); finishErr != nil {
				w.Logger.Warn("finish unpinned run", "run_id", runID, "err", finishErr)
			}
			if w.Metrics != nil {
				w.Metrics.RunAttempt("finish_error", time.Since(started))
			}
			return
		}
		// Register the pin release before any later step can abandon: a failed
		// pin (fence or error) must not leak the generation reference either.
		defer func() {
			// Release even when the attempt is abandoned: the pin must not
			// outlive the run, and a retiring generation drains on it.
			if err := bound.Release(context.WithoutCancel(ctx)); err != nil {
				w.Logger.Warn("release service loadout", "run_id", runID, "err", err)
			}
		}()
		runner = bound.Runner()
		pinned = bound.Loadout()
		if pinner, ok := w.Leases.(LoadoutPinner); ok && pinned.ID != "" {
			if err := pinner.PinRunLoadout(ctx, lease, pinned.ID, pinned.Revision); err != nil {
				if isFenceError(err) {
					w.Logger.Info("run abandoned, claim lost while pinning loadout", "run_id", runID,
						"tenant_id", lease.Scope.TenantID, "session_id", lease.Scope.SessionID, "err", err)
					w.releaseAbandoned(lease)
					return
				}
				settled = true
				w.Logger.Error("pin run loadout", "run_id", runID,
					"tenant_id", lease.Scope.TenantID, "session_id", lease.Scope.SessionID, "err", err)
				if finishErr := w.Leases.Finish(ctx, kernel.FinishRequest{
					Lease:       lease,
					Err:         fmt.Errorf("pin run loadout: %w", err),
					MaxAttempts: w.MaxAttempts,
				}); finishErr != nil {
					w.Logger.Warn("finish unpinned run", "run_id", runID, "err", finishErr)
				}
				if w.Metrics != nil {
					w.Metrics.RunAttempt("finish_error", time.Since(started))
				}
				return
			}
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Register the attempt with the product's cancel seam (optional). The
	// outcome is reported exactly once, when the attempt is done touching the
	// session — settle, abandon or supersede.
	outcome := "abandoned"
	if w.Control != nil {
		controlled, finishControl := w.Control.BeginRun(runCtx, lease.Scope, runID)
		if controlled != nil {
			runCtx = controlled
		}
		if finishControl != nil {
			defer func() { finishControl(outcome) }()
		}
	}
	// Keep the lease alive while the model/tools run. A crash stops the renewal,
	// so the lease expires and another worker takes over. A rejection means the
	// claim is gone (taken over or expired): stop the run so it cannot write
	// with a stale token.
	go func() {
		t := time.NewTicker(w.Lease / 3)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				if _, err := w.Leases.Renew(runCtx, lease, w.Lease); err != nil && runCtx.Err() == nil {
					if isFenceError(err) {
						w.Logger.Info("lease lost, abandoning run", "run_id", runID,
							"tenant_id", lease.Scope.TenantID, "session_id", lease.Scope.SessionID,
							"token", lease.Token.String(), "err", err)
						cancel()
						return
					}
					w.Logger.Warn("renew lease", "run_id", runID,
						"tenant_id", lease.Scope.TenantID, "session_id", lease.Scope.SessionID, "err", err)
				}
			}
		}
	}()

	result, runErr := runner.RunAttempt(runCtx, lease)
	cursor, readThrough := result.Cursor, result.ReadThrough
	// A cancel from the product seam reaches the attempt as a cancelled runCtx
	// (the parent ctx is untouched). It is a deliberate stop, not a failure: the
	// attempt settles through the same policy (unread input re-queues) and the
	// summary says so.
	// A cancel only matters if it actually cut the attempt short: a cancel that
	// lands after the last answer was committed must not rewrite a completed run
	// as cancelled (the runner returned no error, so the work is done and the
	// cancel seam will report the settlement as such).
	cancelled := ctx.Err() == nil && runCtx.Err() != nil && runErr != nil
	if !cancelled && runErr == nil && result.Summary.Status == kernel.RunSummaryFailed {
		// The loop reports a provider failure as a terminal assistant message
		// (stopReason=error), not as a Go error. The attempt failed all the same:
		// settle it through the failure policy so the input is retried and the
		// run row records why.
		runErr = modelTurnFailure(result.Summary.ErrorMessage)
	}
	if ctx.Err() != nil {
		// Process is shutting down: give the claim back instead of recording a
		// failure. Release clears the lease and returns the run to pending, so
		// the next process can claim it immediately (and the tenant's admission
		// slot is returned; leaving the lease to expire would leak it).
		w.Logger.Info("run abandoned for shutdown", "run_id", runID,
			"tenant_id", lease.Scope.TenantID, "session_id", lease.Scope.SessionID, "attempt", attempts)
		w.releaseAbandoned(lease)
		return
	}
	elapsed := time.Since(started)
	// The attempt summary is committed under the lease, BEFORE the settle: it is
	// a fact about this attempt (the run row only carries the session-level
	// status), so it cannot be written after the lease is cleared, and a later
	// fence failure cannot make it false. A summary failure never changes the
	// run's terminal state — it only costs observability.
	if cancelled {
		result.Summary.Status = kernel.RunSummaryCancelled
		result.Summary.StopReason = agentcore.StopReasonAborted
	}
	if w.Committer != nil {
		if payload, err := json.Marshal(result.Summary); err != nil {
			w.Logger.Warn("marshal run summary", "run_id", runID, "err", err)
		} else if receipt, err := w.Committer.Commit(ctx, kernel.CommitRequest{
			Scope: lease.Scope, Token: lease.Token, Kind: kernel.KindRunFinished, Payload: payload,
			RunID: runID,
		}); err != nil {
			w.Logger.Warn("commit run summary", "run_id", runID, "err", err)
		} else {
			cursor = receipt.Seq
		}
	}
	if err := w.Leases.Finish(ctx, kernel.FinishRequest{
		Lease: lease,
		// The read-through watermark is what makes input that arrived mid-run
		// re-queue the session: the cursor alone has already been pushed past
		// it by this run's own commits.
		Cursor:      cursor,
		ReadThrough: readThrough,
		Err:         runErr,
		MaxAttempts: w.MaxAttempts,
		// A deliberate stop settles the attempt terminally instead of re-queuing
		// the input it did not answer.
		Cancelled: cancelled,
		// The loadout the run pinned is written with the settle, so an audit
		// can answer which revision/generation finished the run. The kernel
		// fills the fencing epoch from the lease token.
		Loadout: kernel.RunLoadout{ID: pinned.ID, Revision: pinned.Revision, Generation: pinned.Generation},
	}); err != nil {
		settled = true
		if isFenceError(err) {
			// Our lease was taken over (or the run already settled): the
			// result is a no-op. Do not touch terminal metrics.
			if w.Metrics != nil {
				w.Metrics.RunAttempt("abandoned", elapsed)
			}
			w.Logger.Info("run attempt superseded", "run_id", runID,
				"tenant_id", lease.Scope.TenantID, "session_id", lease.Scope.SessionID,
				"token", lease.Token.String(), "attempt", attempts,
				"duration_ms", elapsed.Milliseconds(), "err", err)
			return
		}
		if w.Metrics != nil {
			w.Metrics.RunAttempt("finish_error", elapsed)
			w.Metrics.FinishFailed()
		}
		w.Logger.Error("finish run", "run_id", runID,
			"tenant_id", lease.Scope.TenantID, "session_id", lease.Scope.SessionID,
			"duration_ms", elapsed.Milliseconds(), "err", err)
		return
	}
	settled = true
	// The outcome the cancel seam remembers: cancelled is the deliberate stop,
	// a failed attempt is reported as failed, anything else that settled
	// cleanly is done.
	switch {
	case cancelled:
		outcome = kernel.RunSummaryCancelled
	case runErr != nil:
		outcome = kernel.RunSummaryFailed
	default:
		outcome = kernel.RunSummaryDone
	}
	// The kernel settled the run; the run-row mirror carries the committed
	// status (done / pending / failed) for the terminal metrics and the log.
	// A row a newer claim already took over reads as running: that status
	// belongs to the new attempt, not to this one, so it is reported as
	// superseded instead of a bogus "running" attempt.
	status := w.settledStatus(ctx, lease.Scope)
	if status == store.RunRunning {
		status = "superseded"
	}
	if w.Metrics != nil {
		w.Metrics.RunAttempt(status, elapsed)
		if status == store.RunDone || status == store.RunFailed {
			w.Metrics.RunFinished(status)
		}
	}
	level := slog.LevelInfo
	if runErr != nil {
		level = slog.LevelWarn
	}
	w.Logger.Log(ctx, level, "run attempt settled", "run_id", runID,
		"tenant_id", lease.Scope.TenantID, "session_id", lease.Scope.SessionID,
		"cursor", cursor, "attempt", attempts, "duration_ms", elapsed.Milliseconds(),
		"run_status", status, "token", lease.Token.String(), "err", runErr)
}

// modelTurnFailure converts the loop's terminal error message into the settle's
// error. The message comes from the provider and is recorded on the run row;
// an empty message still fails the attempt.
func modelTurnFailure(message string) error {
	if message == "" {
		return errors.New("agent: the model turn ended in an error")
	}
	return fmt.Errorf("agent: model turn failed: %s", message)
}

// releaseAbandoned returns a claim and its admission slot when an attempt is
// abandoned without a settle (shutdown, or a claim lost while pinning). Release
// is fenced: a stale token fails, but the kernel returns the admission slot
// either way, so repeated abandons cannot starve the tenant's run budget.
func (w *Worker) releaseAbandoned(lease kernel.Lease) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Leases.Release(ctx, lease); err != nil && !isFenceError(err) {
		w.Logger.Warn("release abandoned run",
			"tenant_id", lease.Scope.TenantID, "session_id", lease.Scope.SessionID,
			"token", lease.Token.String(), "err", err)
	}
}

// attemptOf reads the run-row mirror for logging. Best effort: a mirror read
// failure must not affect the run.
func (w *Worker) attemptOf(ctx context.Context, scope kernel.Scope) int {
	if w.Runs == nil {
		return 0
	}
	run, err := w.Runs.GetRun(ctx, scope.TenantID, scope.SessionID)
	if err != nil {
		return 0
	}
	return run.Attempts
}

// settledStatus reads the committed run status from the mirror for terminal
// metrics. The work itself is settled by the kernel; this read only observes it.
func (w *Worker) settledStatus(ctx context.Context, scope kernel.Scope) string {
	if w.Runs == nil {
		return "unknown"
	}
	run, err := w.Runs.GetRun(ctx, scope.TenantID, scope.SessionID)
	if err != nil {
		w.Logger.Warn("read settled run status",
			"tenant_id", scope.TenantID, "session_id", scope.SessionID, "err", err)
		return "unknown"
	}
	return run.Status
}
