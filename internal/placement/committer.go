// GatedCommitter wires the placement admission gate into the kernel's edge
// commit seam. It is the runtime's single admission point for native input:
// every user.message commit that will wake a run pays for one placement
// admission first, and the kernel's own append is unchanged.
//
// Why a committer wrapper instead of a call in each HTTP handler: a native run
// is created by exactly one event (the kernel's user.message wake), so gating
// the commit covers the input endpoint, the raw event endpoint, retries, the
// MCP app bridge and product-initiated turns without a second accounting path.
// Non-user events (tool results, run.finished, ...) pass through untouched.
//
// The wrapper is for the EDGE only. The run loop's optional read-marker and
// checkpoint capabilities are derived from the concrete committer it receives,
// so the service host keeps the raw kernel as the worker/runner Committer and
// passes this wrapper to the HTTP edge and the product turn starters.
package placement

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/kernel"
)

// releaseTimeout bounds the cleanup write when a commit fails after admission.
const releaseTimeout = 5 * time.Second

// GatedCommitter decorates a kernel.Committer with placement admission.
type GatedCommitter struct {
	edge   kernel.Committer
	gate   *Gate
	logger *slog.Logger
}

var _ kernel.Committer = (*GatedCommitter)(nil)

// NewGatedCommitter builds the wrapper. Both seams are required: an unwired
// gate would silently restore the pre-placement behavior (no group slot, no
// cross-group quota), which is exactly the gap this wiring closes.
func NewGatedCommitter(edge kernel.Committer, gate *Gate, logger *slog.Logger) (*GatedCommitter, error) {
	if edge == nil {
		return nil, errors.New("placement: gated committer requires a kernel committer")
	}
	if gate == nil {
		return nil, errors.New("placement: gated committer requires a gate")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &GatedCommitter{edge: edge, gate: gate, logger: logger}, nil
}

// Commit admits the run a native user.message will wake, then commits the
// event. The admission writes the pending run row or the durable queue entry
// (the slot reservation) before the input exists; if the commit fails, a
// reservation this call created is released so the queue does not hold an empty
// attempt. A queued admission still commits: the input is durable, and the run
// starts when promotion (FIFO) gives it a slot.
func (c *GatedCommitter) Commit(ctx context.Context, req kernel.CommitRequest) (kernel.Receipt, error) {
	if req.Kind != kernel.KindUserMessage || !req.NativeEdge || req.Scope.IsZero() {
		return c.edge.Commit(ctx, req)
	}
	// A replay of an already-committed requestId must not buy (or be refused)
	// a new slot: the event is durable and the run it woke was settled by the
	// first commit. Lookup only decides whether admission is needed; the commit
	// seam stays the dedupe authority, so a same-key/different-payload retry
	// still returns its key-conflict instead of silently reusing the receipt.
	if req.RequestID != "" {
		if _, found, err := c.edge.Lookup(ctx, req.Scope,
			kernel.KeyRef{Kind: kernel.KeyRequest, Value: req.RequestID}); err != nil {
			return kernel.Receipt{}, err
		} else if found {
			return c.edge.Commit(ctx, req)
		}
	}
	admission, err := c.gate.Admit(ctx, req.Scope.TenantID, req.Scope.SessionID)
	if err != nil {
		return kernel.Receipt{}, mapGateError(err)
	}
	receipt, err := c.edge.Commit(ctx, req)
	if err != nil {
		// Only the reservation this call created is ours to release. An
		// already-active or already-queued run belongs to an earlier input and
		// must keep its slot/place.
		if admission.Created {
			c.release(ctx, req.Scope.TenantID, req.Scope.SessionID)
		}
		return receipt, err
	}
	return receipt, nil
}

// Lookup delegates the idempotency read; it never admits.
func (c *GatedCommitter) Lookup(ctx context.Context, scope kernel.Scope, key kernel.KeyRef) (kernel.Receipt, bool, error) {
	return c.edge.Lookup(ctx, scope, key)
}

// release frees an admission whose input did not commit. It runs with a fresh
// bounded context: the commit error may be a request cancellation, and a
// cancelled context must not strand the reservation. The store's ReleaseRun is
// guarded (pending, no unread input), so a racing successful commit wins.
func (c *GatedCommitter) release(ctx context.Context, tenantID, sessionID uuid.UUID) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if err := c.gate.Release(cleanup, tenantID, sessionID); err != nil {
		c.logger.Warn("release placement admission after failed commit",
			"tenant_id", tenantID, "session_id", sessionID, "err", err)
	}
}

// mapGateError keeps the kernel's error vocabulary at the seam: a missing
// session is the kernel's ErrNotFound, an external session its
// ErrExternalSession. Capacity and placement conflicts are new decisions and
// travel as their typed errors for the edge to map onto 429/409.
func mapGateError(err error) error {
	switch {
	case errors.Is(err, ErrSessionNotFound):
		return kernel.ErrNotFound
	case errors.Is(err, ErrExternalSession):
		return kernel.ErrExternalSession
	default:
		return err
	}
}
