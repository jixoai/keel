// Handoff succession evidence: the proof that a handoff cycle actually changed
// shifts on durable state, rather than merely parking a drain request.
//
// The runtime writes independent audit trails while a handoff happens:
//
//   - handoffs (session_id IS NULL, mode='handoff'): the predecessor's
//     runtime-level drain, closed with completed/timed-out and a finished_at;
//   - runtime_instances: the predecessor leaves draining for stopped with
//     drain_started_at/stopped_at, the successor registers ready with ready_at;
//   - sessions.owner / sessions.owner_epoch: the lease authority itself, which
//     is what "a different runtime took the session over" ultimately means.
//
// handoffs rows with a session_id are per-session adoptions; they are written
// by the coordinator's Adopt path, which this harness's runtime does not use
// (the worker claims through kernel.Claim), so their count is informational
// here and not a proof condition.
//
// A cycle counts as a succession only when the audit agrees that a *different*
// runtime process took the sessions over at a strictly higher epoch, after the
// predecessor's drain began. A 200/409 drain answer proves none of this.
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jixoai/keel/internal/handoff"
)

// successionWait bounds how long one cycle waits for its durable proof. A
// working handoff claims every ~200ms and finishDrain writes its row as soon
// as the in-flight runs settle, so the evidence normally lands in well under a
// second; the bound only matters when the handoff failed and the cycle is
// about to be reported as unproven.
const successionWait = 15 * time.Second

// successionEvidence is one handoff cycle's audit snapshot. Everything here
// comes from the database (never from the drain HTTP response), so a report
// reader can re-run the queries and get the same answer.
type successionEvidence struct {
	Cycle            int               `json:"cycle"`
	OldRuntime       string            `json:"oldRuntime"`
	SuccessorRuntime string            `json:"successorRuntime,omitempty"`
	FinalRuntime     string            `json:"finalRuntime,omitempty"`
	DrainStatus      string            `json:"drainStatus,omitempty"`
	DrainFinishedAt  string            `json:"drainFinishedAt,omitempty"`
	OldRuntimeStatus string            `json:"oldRuntimeStatus,omitempty"`
	OldStoppedAt     string            `json:"oldStoppedAt,omitempty"`
	NewRuntimeStatus string            `json:"newRuntimeStatus,omitempty"`
	NewReadyAt       string            `json:"newReadyAt,omitempty"`
	EpochBefore      map[string]int64  `json:"epochBefore,omitempty"`
	EpochAfter       map[string]int64  `json:"epochAfter,omitempty"`
	OwnerAfter       map[string]string `json:"ownerAfter,omitempty"`
	// Adoptions counts per-session adoption rows naming the new runtime. It is
	// informational: the worker's claim path does not write them (see the file
	// header), so 0 here does not weaken the evidence.
	Adoptions int      `json:"adoptions"`
	Proven    bool     `json:"proven"`
	Problems  []string `json:"problems,omitempty"`
}

// runtimeAudit is the subset of a runtime_instances row the evidence uses.
type runtimeAudit struct {
	Status     string
	DrainMode  string
	HasDrain   bool
	DrainAt    time.Time
	HasStopped bool
	StoppedAt  time.Time
	HasReady   bool
	ReadyAt    time.Time
}

// drainAudit is the runtime-level drain row (handoffs, session_id IS NULL).
type drainAudit struct {
	Status     string
	Finished   bool
	FinishedAt time.Time
}

// sessionLeases snapshots the lease authority per session: the owner runtime
// id and the fencing epoch. owner_epoch only moves forward (Claim increments
// it), so before/after ordering is meaningful even across several claims.
func (h *harness) sessionLeases(ctx context.Context, sessions []*sessionState) (map[string]int64, map[string]string, error) {
	epochs := make(map[string]int64, len(sessions))
	owners := make(map[string]string, len(sessions))
	for _, s := range sessions {
		var owner string
		var epoch int64
		if err := h.pool.QueryRow(ctx,
			`SELECT COALESCE(owner, ''), owner_epoch FROM sessions
			 WHERE tenant_id = $1 AND session_id = $2`, s.tenant, s.session).
			Scan(&owner, &epoch); err != nil {
			return nil, nil, fmt.Errorf("session %s: %w", shortID(s.session), err)
		}
		epochs[s.session] = epoch
		owners[s.session] = owner
	}
	return epochs, owners, nil
}

// runtimeAudit reads one runtime_instances row.
func (h *harness) runtimeAudit(ctx context.Context, runtimeID string) (runtimeAudit, error) {
	var a runtimeAudit
	err := h.pool.QueryRow(ctx,
		`SELECT status, COALESCE(drain_mode, ''),
		        drain_started_at IS NOT NULL, COALESCE(drain_started_at, to_timestamp(0)),
		        stopped_at IS NOT NULL, COALESCE(stopped_at, to_timestamp(0)),
		        ready_at IS NOT NULL, COALESCE(ready_at, to_timestamp(0))
		 FROM runtime_instances WHERE runtime_id = $1`, runtimeID).
		Scan(&a.Status, &a.DrainMode, &a.HasDrain, &a.DrainAt,
			&a.HasStopped, &a.StoppedAt, &a.HasReady, &a.ReadyAt)
	if err != nil {
		return runtimeAudit{}, err
	}
	return a, nil
}

// drainAudit reads the newest runtime-level handoff drain row of a runtime.
// found=false means the runtime never wrote one (the drain never began).
func (h *harness) drainAudit(ctx context.Context, runtimeID string) (drainAudit, bool, error) {
	var a drainAudit
	err := h.pool.QueryRow(ctx,
		`SELECT status, finished_at IS NOT NULL, COALESCE(finished_at, to_timestamp(0))
		 FROM handoffs
		 WHERE from_runtime = $1 AND session_id IS NULL AND mode = $2
		 ORDER BY started_at DESC LIMIT 1`, runtimeID, string(handoff.ModeHandoff)).
		Scan(&a.Status, &a.Finished, &a.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return drainAudit{}, false, nil
	}
	if err != nil {
		return drainAudit{}, false, err
	}
	return a, true, nil
}

// adoptionCount counts the per-session adoption rows that name the new runtime
// as the taker and record an epoch jump.
func (h *harness) adoptionCount(ctx context.Context, runtimeID string, since time.Time) (int, error) {
	if runtimeID == "" {
		return 0, nil
	}
	var n int
	err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM handoffs
		 WHERE to_runtime = $1 AND session_id IS NOT NULL
		   AND to_epoch > from_epoch AND started_at >= $2`, runtimeID, since).Scan(&n)
	return n, err
}

// checkSuccession returns the unmet proof conditions of one cycle, filling in
// every piece of evidence it could read. An empty slice means the shift change
// is proven.
func (h *harness) checkSuccession(ctx context.Context, ev *successionEvidence, sessions []*sessionState, started time.Time) []string {
	var problems []string

	// 1. The predecessor actually executed the handoff drain to its end.
	old, oldErr := h.runtimeAudit(ctx, ev.OldRuntime)
	if oldErr != nil {
		problems = append(problems, fmt.Sprintf("predecessor audit row: %v", oldErr))
	} else {
		ev.OldRuntimeStatus = old.Status
		if old.HasStopped {
			ev.OldStoppedAt = auditTime(old.StoppedAt)
		}
		switch {
		case old.Status != handoff.RuntimeStopped:
			problems = append(problems, fmt.Sprintf("predecessor %s status %q after the drain (want %q)", ev.OldRuntime, old.Status, handoff.RuntimeStopped))
		case old.DrainMode != string(handoff.ModeHandoff):
			problems = append(problems, fmt.Sprintf("predecessor drain mode %q (want %q)", old.DrainMode, handoff.ModeHandoff))
		case !old.HasDrain:
			problems = append(problems, "predecessor has no drain_started_at (the drain never began)")
		}
	}
	drain, found, err := h.drainAudit(ctx, ev.OldRuntime)
	switch {
	case err != nil:
		problems = append(problems, fmt.Sprintf("drain audit row: %v", err))
	case !found:
		problems = append(problems, "no runtime-level handoff drain row for the predecessor")
	default:
		ev.DrainStatus = drain.Status
		if drain.Finished {
			ev.DrainFinishedAt = auditTime(drain.FinishedAt)
		}
		switch {
		case drain.Status != handoff.HandoffCompleted && drain.Status != handoff.HandoffTimedOut:
			problems = append(problems, fmt.Sprintf("drain row status %q (want %q or %q)", drain.Status, handoff.HandoffCompleted, handoff.HandoffTimedOut))
		case !drain.Finished:
			problems = append(problems, "drain row has no finished_at (the drain never closed)")
		}
	}

	// 2. A different process became ready afterwards.
	if ev.FinalRuntime == "" {
		problems = append(problems, "no fresh runtime was started")
	} else if neu, err := h.runtimeAudit(ctx, ev.FinalRuntime); err != nil {
		problems = append(problems, fmt.Sprintf("successor audit row: %v", err))
	} else {
		ev.NewRuntimeStatus = neu.Status
		if neu.HasReady {
			ev.NewReadyAt = auditTime(neu.ReadyAt)
		}
		switch {
		case neu.Status != handoff.RuntimeReady:
			problems = append(problems, fmt.Sprintf("successor %s status %q (want %q)", ev.FinalRuntime, neu.Status, handoff.RuntimeReady))
		case !neu.HasReady:
			problems = append(problems, "successor has no ready_at")
		case oldErr == nil && old.HasDrain && !neu.ReadyAt.After(old.DrainAt):
			problems = append(problems, fmt.Sprintf("successor became ready at %s, not after the predecessor's drain began at %s",
				auditTime(neu.ReadyAt), auditTime(old.DrainAt)))
		}
	}

	// 3. Every session is now owned by the new runtime at a higher epoch.
	if len(ev.EpochBefore) != len(sessions) {
		problems = append(problems, fmt.Sprintf("no complete pre-cycle epoch snapshot (%d of %d sessions)", len(ev.EpochBefore), len(sessions)))
	} else if after, owners, err := h.sessionLeases(ctx, sessions); err != nil {
		problems = append(problems, fmt.Sprintf("session leases: %v", err))
	} else {
		ev.EpochAfter = after
		ev.OwnerAfter = owners
		for _, s := range sessions {
			before, nowEpoch := ev.EpochBefore[s.session], after[s.session]
			owner := owners[s.session]
			if ev.FinalRuntime != "" && owner != ev.FinalRuntime {
				problems = append(problems, fmt.Sprintf("session %s still owned by %q (want %q)", shortID(s.session), owner, ev.FinalRuntime))
			}
			if nowEpoch <= before {
				problems = append(problems, fmt.Sprintf("session %s owner_epoch %d -> %d did not advance", shortID(s.session), before, nowEpoch))
			}
		}
	}

	// 4. The adoption trail is informational: this runtime claims through
	// kernel.Claim, not the coordinator's Adopt path, so per-session adoption
	// rows normally do not exist here. When they do (an external handoff
	// deployment), they must name the new runtime as the taker.
	if ev.FinalRuntime != "" {
		adoptions, err := h.adoptionCount(ctx, ev.FinalRuntime, started)
		if err != nil {
			problems = append(problems, fmt.Sprintf("adoption rows: %v", err))
		} else {
			ev.Adoptions = adoptions
		}
	}
	return problems
}

// awaitSuccession polls checkSuccession until the proof is complete or the
// bounded wait expires, so a slow-but-real handoff is not reported as failed.
func (h *harness) awaitSuccession(ctx context.Context, ev *successionEvidence, sessions []*sessionState, started time.Time) {
	deadline := time.Now().Add(successionWait)
	for {
		problems := h.checkSuccession(ctx, ev, sessions, started)
		if len(problems) == 0 {
			ev.Proven = true
			ev.Problems = nil
			return
		}
		ev.Problems = problems
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			ev.Problems = append(problems, "context cancelled while waiting for succession evidence: "+ctx.Err().Error())
			return
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func auditTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
