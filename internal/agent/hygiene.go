package agent

// Memory-hygiene logging. The report is the audit of what left the request
// view: every pruned tool result names its durable reference, every offloaded
// image its blob id, and every failure says which message was kept because the
// rewrite could not be guaranteed. This is deliberately log-level fact rather
// than a ledger event: the committed messages are unchanged, and the run summary
// already records prompt/config identity.

import (
	"context"
	"log/slog"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/spill"
)

// applyHygienePrePass runs the compaction hygiene extensions over a rebuilt
// transcript before image resolution. It is the per-attempt half of the two
// placements: the loop prunes after each turn, this pass catches history that
// was already old when the attempt started (notably after a resume, so an
// offloaded image is never resolved into wire bytes again).
func (r *Runner) applyHygienePrePass(ctx context.Context, msgs agentcore.MessageList, lease kernel.Lease) agentcore.MessageList {
	if r == nil || r.Compaction == nil || !r.Compaction.Settings.Enabled || !r.Compaction.Hygiene.Enabled {
		return msgs
	}
	scope := spill.Scope{
		TenantID:  lease.Scope.TenantID.String(),
		SessionID: lease.Scope.SessionID.String(),
	}
	hygienic, report := compaction.ApplyHygiene(ctx, msgs, r.Compaction.Hygiene, r.Compaction.Spill, scope)
	if report.Changed() || len(report.Failures) > 0 {
		logHygiene(r.Logger, lease, report)
	}
	return hygienic
}

// logHygiene records one hygiene pass. Informational when bytes moved,
// warn-level for each failure (a failure means the original stayed, never that
// content was lost).
func logHygiene(logger *slog.Logger, lease kernel.Lease, report compaction.HygieneReport) {
	if logger == nil {
		return
	}
	attrs := []any{
		"session_id", lease.Scope.SessionID,
		"tokens_before", report.TokensBefore,
		"tokens_after", report.TokensAfter,
	}
	for _, pruned := range report.Pruned {
		logger.Info("tool result pruned to a durable reference", append(attrs,
			"tool", pruned.ToolName,
			"tool_call_id", pruned.ToolCallID,
			"ref", pruned.Ref.ID,
			"sha256", pruned.Ref.SHA256,
			"bytes", pruned.Ref.Size,
			"tokens_before", pruned.OriginalTokens,
			"tokens_after", pruned.ReferenceTokens)...)
	}
	for _, image := range report.Offloaded {
		logger.Info("image reference offloaded to a durable reference", append(attrs,
			"blob_id", image.BlobID,
			"mime", image.MIME,
			"bytes", image.Size,
			"tokens_before", image.TokensBefore,
			"tokens_after", image.TokensAfter)...)
	}
	for _, failure := range report.Failures {
		logger.Warn("memory hygiene left the message unchanged", append(attrs,
			"message_index", failure.MessageIndex,
			"kind", failure.Kind,
			"reason", failure.Reason)...)
	}
}
