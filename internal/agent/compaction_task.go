package agent

// Compaction task recovery (PI CompactionTask parity).
//
// The durable compaction task row is the in-flight half of compaction
// recovery: `checkpoints` holds a projection of a FINISHED compaction, while
// this row records the phase of an unfinished one (select -> summarize ->
// retry) and the range a manual task already pinned. Neither replaces the
// event log.
//
// Recovery rules implemented here:
//   - a manual request whose compaction EVENT was committed but whose
//     checkpoint projection was lost (crash between Commit and SaveCheckpoint)
//     is recovered from the committed event by requestId; it never compacts
//     twice;
//   - a manual request whose summary call was interrupted resumes the pinned
//     range (CompactAt) instead of selecting a new cut;
//   - an automatic compaction entry (CompactionStartEvent) records a select
//     checkpoint, and a committed automatic event is projected on the next
//     attempt even if the process died before saving it. An uncommitted
//     automatic task is discarded with an identifier-only log line: the
//     context is unchanged, so the next threshold crossing re-runs it.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

const autoCompactionRequestPrefix = "keel-auto-compaction:"

// autoCompactionRequestID is deterministic across a replay of the same log:
// the cursor is the last event committed before the loop started summarizing,
// so a retry that sees the same log derives the same task identity.
func autoCompactionRequestID(cursor int64) string {
	return fmt.Sprintf("%s%d", autoCompactionRequestPrefix, cursor)
}

func isAutoCompactionRequestID(requestID string) bool {
	return strings.HasPrefix(requestID, autoCompactionRequestPrefix)
}

// indexOfPositiveSeq returns the transcript index holding a committed seq, or
// -1. Synthetic negative seqs (the inherited fork prefix and a checkpoint
// placeholder) and zero never match.
func indexOfPositiveSeq(seqs []int64, target int64) int {
	if target <= 0 {
		return -1
	}
	for index, seq := range seqs {
		if seq == target {
			return index
		}
	}
	return -1
}

// saveCompactionTask upserts the task checkpoint; a nil seam is a no-op.
func (r *Runner) saveCompactionTask(ctx context.Context, scope kernel.Scope, checkpoint kernel.CompactionTaskCheckpoint) error {
	if r == nil || r.CompactionTasks == nil {
		return nil
	}
	return r.CompactionTasks.SaveCompactionTask(ctx, scope, checkpoint)
}

// saveManualCompactRetry persists the retry state on a detached, bounded
// context. The summary failure is often a cancellation; writing with the
// cancelled parent would always fail and lose the retry/attempt fact.
func (r *Runner) saveManualCompactRetry(parent context.Context, scope kernel.Scope, checkpoint kernel.CompactionTaskCheckpoint) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	return r.saveCompactionTask(ctx, scope, checkpoint)
}

// loadCompactionTask reads one task checkpoint; a nil seam answers not found.
func (r *Runner) loadCompactionTask(ctx context.Context, scope kernel.Scope, requestID string) (kernel.CompactionTaskCheckpoint, bool, error) {
	if r == nil || r.CompactionTasks == nil {
		return kernel.CompactionTaskCheckpoint{}, false, nil
	}
	return r.CompactionTasks.LoadCompactionTask(ctx, scope, requestID)
}

// deleteCompactionTask removes a finished task; failures are logged and never
// roll back the compaction event or the checkpoint.
func (r *Runner) deleteCompactionTask(ctx context.Context, scope kernel.Scope, requestID string) {
	if r == nil || r.CompactionTasks == nil {
		return
	}
	if err := r.CompactionTasks.DeleteCompactionTask(ctx, scope, requestID); err != nil {
		r.Logger.Warn("delete compaction task", "session_id", scope.SessionID, "request_id", requestID, "err", err)
	}
}

// compactionEventPayload is the subset of a committed compaction event the
// recovery path needs. keptFromSeq is written by this feature; an older event
// without it cannot be recovered into a checkpoint projection (the caller
// falls back to the event log rather than guessing a boundary).
type compactionEventPayload struct {
	Summary       string `json:"summary"`
	Reason        string `json:"reason"`
	TokensBefore  int    `json:"tokensBefore"`
	TokensAfter   int    `json:"tokensAfter"`
	FirstKept     int    `json:"firstKept"`
	KeptFromSeq   int64  `json:"keptFromSeq"`
	Summarized    int    `json:"summarized"`
	Kept          int    `json:"kept"`
	RequestID     string `json:"requestId"`
	Error         string `json:"error,omitempty"`
	PolicyVersion string `json:"policyVersion,omitempty"`
}

// findCommittedCompaction returns the newest committed compaction event that
// carries requestID. reason, when non-empty, must match the payload's reason.
func findCommittedCompaction(events []store.Event, requestID, reason string) (store.Event, compactionEventPayload, bool) {
	if strings.TrimSpace(requestID) == "" {
		return store.Event{}, compactionEventPayload{}, false
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Kind != store.EventCompaction {
			continue
		}
		var payload compactionEventPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			continue
		}
		if payload.RequestID != requestID {
			continue
		}
		if reason != "" && payload.Reason != reason {
			continue
		}
		if strings.TrimSpace(payload.Summary) == "" || payload.KeptFromSeq <= 0 {
			continue
		}
		return event, payload, true
	}
	return store.Event{}, compactionEventPayload{}, false
}

// recoverCommittedCompaction projects a committed manual compaction whose
// checkpoint save was lost. It reports whether it recovered anything; an error
// means the projection could not be restored (the caller must not compact
// again silently).
func (r *Runner) recoverCommittedCompaction(ctx context.Context, scope kernel.Scope, requestID string, events []store.Event) (ManualCompactResult, bool, error) {
	event, payload, found := findCommittedCompaction(events, requestID, "manual")
	if !found {
		return ManualCompactResult{}, false, nil
	}
	if r.Checkpoints != nil {
		if err := r.Checkpoints.SaveCheckpoint(ctx, scope, kernel.CheckpointRecord{
			UptoSeq:       event.Seq,
			KeptFromSeq:   payload.KeptFromSeq,
			Summary:       payload.Summary,
			TokensBefore:  payload.TokensBefore,
			TokensAfter:   payload.TokensAfter,
			PolicyVersion: payload.PolicyVersion,
			RequestID:     requestID,
		}); err != nil {
			return ManualCompactResult{}, false, fmt.Errorf("agent: recover compaction checkpoint: %w", err)
		}
	}
	r.deleteCompactionTask(ctx, scope, requestID)
	if r.CheckpointMetrics != nil {
		r.CheckpointMetrics.CheckpointRecovered()
	}
	r.Logger.Info("manual compaction recovered from committed event",
		"tenant_id", scope.TenantID, "session_id", scope.SessionID,
		"seq", event.Seq, "kept_from_seq", payload.KeptFromSeq, "request_id", requestID)
	return ManualCompactResult{
		CheckpointID:  event.Seq,
		Recovered:     true,
		TokensBefore:  payload.TokensBefore,
		TokensAfter:   payload.TokensAfter,
		KeptFromSeq:   payload.KeptFromSeq,
		PolicyVersion: payload.PolicyVersion,
		Reason:        "recovered",
	}, true, nil
}

// recoverAutomaticCompactions closes the crash window of automatic compaction:
// a committed event is projected, an uncommitted entry marker is discarded.
// Manual tasks are left to their caller (CompactNow owns their requestId).
func (r *Runner) recoverAutomaticCompactions(ctx context.Context, scope kernel.Scope, events []store.Event) {
	if r == nil || r.CompactionTasks == nil {
		return
	}
	tasks, err := r.CompactionTasks.ListCompactionTasks(ctx, scope)
	if err != nil {
		r.Logger.Warn("list compaction tasks", "session_id", scope.SessionID, "err", err)
		return
	}
	for _, task := range tasks {
		if !isAutoCompactionRequestID(task.RequestID) {
			continue
		}
		event, payload, found := findCommittedCompaction(events, task.RequestID, "threshold")
		if !found {
			// The loop compacted in memory but the process died before the event
			// was committed. The committed context is unchanged, so the marker is
			// stale: the next threshold crossing re-runs compaction from the log.
			r.deleteCompactionTask(ctx, scope, task.RequestID)
			r.Logger.Warn("discarded uncommitted automatic compaction task",
				"session_id", scope.SessionID, "request_id", task.RequestID)
			continue
		}
		if r.Checkpoints != nil {
			if err := r.Checkpoints.SaveCheckpoint(ctx, scope, kernel.CheckpointRecord{
				UptoSeq:       event.Seq,
				KeptFromSeq:   payload.KeptFromSeq,
				Summary:       payload.Summary,
				TokensBefore:  payload.TokensBefore,
				TokensAfter:   payload.TokensAfter,
				PolicyVersion: task.PolicyVersion,
			}); err != nil {
				r.Logger.Warn("recover automatic compaction checkpoint",
					"session_id", scope.SessionID, "seq", event.Seq, "err", err)
				continue
			}
		}
		r.deleteCompactionTask(ctx, scope, task.RequestID)
		if r.CheckpointMetrics != nil {
			r.CheckpointMetrics.CheckpointRecovered()
		}
		r.Logger.Info("automatic compaction checkpoint recovered",
			"tenant_id", scope.TenantID, "session_id", scope.SessionID,
			"seq", event.Seq, "kept_from_seq", payload.KeptFromSeq)
	}
}
