// Production manual-compaction adapter (change keel-checkpoint): the seam
// behind POST /v1/sessions/{id}/compact.
//
// Manual compaction is an owner-scoped write: it appends a compaction event and
// a checkpoint projection under the session's run lease, exactly like the
// automatic path. The adapter therefore claims the session first. A session
// without a runnable row is queued once (EnqueueRun) so the claim can succeed;
// a live run answers "busy" instead of racing it. The lease is renewed while
// the summary request is in flight, and settled afterwards: success finishes the
// run (unread input re-queues through the store's normal probe), failure
// releases it so the worker still sees any owed turn.
package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/harnessapi"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

// manualCompactor owns the claim/settle lifecycle around one manual compaction.
// It is safe for concurrent calls: each call claims the run lease, so only one
// compacts a session at a time.
type manualCompactor struct {
	binding  agent.RunBinding
	store    *store.Store
	kernel   *kernel.Kernel
	logger   *slog.Logger
	owner    string
	leaseTTL time.Duration
}

// newManualCompactor builds the HTTP seam. binding supplies the live runner of
// the current generation; store and kernel are the durable queue and fencing
// seams. Every dependency is required.
func newManualCompactor(binding agent.RunBinding, st *store.Store, kern *kernel.Kernel, logger *slog.Logger) harnessapi.CompactSessionFunc {
	if logger == nil {
		logger = slog.Default()
	}
	owner := runtimeID() + "/manual-compact"
	compactor := &manualCompactor{
		binding:  binding,
		store:    st,
		kernel:   kern,
		logger:   logger,
		owner:    owner,
		leaseTTL: 30 * time.Second,
	}
	return compactor.compact
}

func (m *manualCompactor) compact(ctx context.Context, tenantID, sessionID uuid.UUID, requestID string) (harnessapi.CompactResult, error) {
	scope := kernel.Scope{TenantID: tenantID, SessionID: sessionID}
	// A completed requestId replays without touching the lease: the projection
	// row is the durable idempotency fact.
	if checkpoint, found, err := m.kernel.CheckpointByRequest(ctx, scope, requestID); err != nil {
		return harnessapi.CompactResult{}, err
	} else if found {
		return harnessapi.CompactResult{
			CheckpointID:  checkpoint.UptoSeq,
			Replayed:      true,
			TokensBefore:  checkpoint.TokensBefore,
			TokensAfter:   checkpoint.TokensAfter,
			KeptFromSeq:   checkpoint.KeptFromSeq,
			PolicyVersion: checkpoint.PolicyVersion,
			Reason:        "replayed",
		}, nil
	}

	bound, err := m.binding.Acquire(ctx)
	if err != nil {
		return harnessapi.CompactResult{}, err
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if releaseErr := bound.Release(releaseCtx); releaseErr != nil {
			m.logger.Warn("manual compact: release generation pin", "err", releaseErr)
		}
	}()

	lease, err := m.claim(ctx, scope)
	if err != nil {
		return harnessapi.CompactResult{}, err
	}
	stopKeepAlive := m.keepAlive(ctx, lease)
	result, compactErr := bound.Runner().CompactNow(ctx, lease, requestID)
	stopKeepAlive()
	m.settle(ctx, lease, compactErr)

	if compactErr != nil {
		switch {
		case errors.Is(compactErr, agent.ErrManualCompactNothing):
			// No safe cut: the context is untouched; this is a successful no-op.
			return harnessapi.CompactResult{
				TokensBefore:  result.TokensBefore,
				TokensAfter:   result.TokensAfter,
				PolicyVersion: result.PolicyVersion,
				Reason:        "nothing-to-compact",
			}, nil
		case errors.Is(compactErr, agent.ErrManualCompactDisabled):
			return harnessapi.CompactResult{}, harnessapi.ErrCompactDisabled
		default:
			return harnessapi.CompactResult{}, compactErr
		}
	}
	m.logger.Info("manual compaction completed",
		"tenant_id", tenantID, "session_id", sessionID, "request_id", requestIDLabel(requestID),
		"checkpoint_seq", result.CheckpointID, "kept_from_seq", result.KeptFromSeq,
		"tokens_before", result.TokensBefore, "tokens_after", result.TokensAfter,
		"policy_version", result.PolicyVersion)
	return harnessapi.CompactResult{
		CheckpointID:  result.CheckpointID,
		Performed:     result.Performed,
		Replayed:      result.Replayed,
		TokensBefore:  result.TokensBefore,
		TokensAfter:   result.TokensAfter,
		KeptFromSeq:   result.KeptFromSeq,
		PolicyVersion: result.PolicyVersion,
		Reason:        result.Reason,
	}, nil
}

// claim takes the session's run lease. A session with no runnable run is
// queued once so a quiet session can still be compacted; the queue row is the
// same mechanism a new input uses, and settling it as done is a no-op attempt.
func (m *manualCompactor) claim(ctx context.Context, scope kernel.Scope) (kernel.Lease, error) {
	lease, err := m.kernel.ClaimScope(ctx, scope, m.owner, m.leaseTTL)
	if err == nil {
		return lease, nil
	}
	switch {
	case errors.Is(err, kernel.ErrLeaseHeld):
		return kernel.Lease{}, harnessapi.ErrCompactBusy
	case errors.Is(err, kernel.ErrNotFound):
		return kernel.Lease{}, harnessapi.ErrSessionNotFound
	case errors.Is(err, kernel.ErrQuota):
		return kernel.Lease{}, harnessapi.ErrCompactQuota
	case !errors.Is(err, kernel.ErrNoRunnable):
		return kernel.Lease{}, err
	}
	// No claimable run: queue the session once and retry. EnqueueRun refuses a
	// missing session and an external-backed one with its own sentinels.
	if err := m.store.EnqueueRun(ctx, scope.TenantID, scope.SessionID); err != nil {
		switch {
		case errors.Is(err, store.ErrNoSession):
			return kernel.Lease{}, harnessapi.ErrSessionNotFound
		case errors.Is(err, store.ErrExternalSession):
			return kernel.Lease{}, harnessapi.ErrExternalSession
		default:
			return kernel.Lease{}, err
		}
	}
	lease, err = m.kernel.ClaimScope(ctx, scope, m.owner, m.leaseTTL)
	if err != nil {
		switch {
		case errors.Is(err, kernel.ErrLeaseHeld):
			return kernel.Lease{}, harnessapi.ErrCompactBusy
		case errors.Is(err, kernel.ErrNoRunnable):
			// The run stays placement-queued or is no longer claimable: another
			// actor owns the next move, so this is honest backpressure.
			return kernel.Lease{}, harnessapi.ErrCompactBusy
		case errors.Is(err, kernel.ErrQuota):
			return kernel.Lease{}, harnessapi.ErrCompactQuota
		case errors.Is(err, kernel.ErrNotFound):
			return kernel.Lease{}, harnessapi.ErrSessionNotFound
		default:
			return kernel.Lease{}, err
		}
	}
	return lease, nil
}

// keepAlive renews the lease while a summary request is in flight. The stop
// function is idempotent from the caller's side (called once).
func (m *manualCompactor) keepAlive(ctx context.Context, lease kernel.Lease) func() {
	done := make(chan struct{})
	interval := m.leaseTTL / 3
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				_, err := m.kernel.Renew(renewCtx, lease, m.leaseTTL)
				cancel()
				if err != nil {
					m.logger.Warn("manual compact: renew lease", "session_id", lease.Scope.SessionID, "err", err)
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

// settle gives the claim back. Success (and a safe no-op) finishes the run: the
// store's ordinary moreInput probe re-queues unread input, so a manual
// compaction never consumes an owed turn. A real failure releases the run so
// the worker still picks it up.
func (m *manualCompactor) settle(ctx context.Context, lease kernel.Lease, compactErr error) {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if compactErr == nil || errors.Is(compactErr, agent.ErrManualCompactNothing) {
		err := m.kernel.Finish(settleCtx, kernel.FinishRequest{
			Lease: lease, MaxAttempts: 3,
		})
		if err != nil {
			m.logger.Warn("manual compact: finish run", "session_id", lease.Scope.SessionID, "err", err)
		}
		return
	}
	if err := m.kernel.Release(settleCtx, lease); err != nil {
		m.logger.Warn("manual compact: release run", "session_id", lease.Scope.SessionID, "err", err)
	}
}

// requestIDLabel bounds a requestId before it enters a log line. The full id is
// an opaque client value; the log only needs a short correlation hint.
func requestIDLabel(requestID string) string {
	requestID = strings.TrimSpace(requestID)
	if len(requestID) <= 24 {
		return requestID
	}
	return requestID[:24] + "…"
}
