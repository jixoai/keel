// Package usage adapts the agent's usage-recorder seam onto the existing
// usage_ledger table.
//
// It exists so the runner package does not import the store and the store does
// not import the runner: cmd/keel and cmd/runtime wire one Recorder at boot,
// and every provider request is then accounted in the same ledger the HTTP
// usage surface aggregates. There is no second source of truth: the runner
// reports provider usage to the attempt summary and records the same numbers
// here.
package usage

import (
	"context"

	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

// Recorder writes agent usage records into the durable usage ledger.
type Recorder struct {
	Store *store.Store
}

// NewRecorder builds the production recorder. A nil store keeps agent
// accounting off (the runner's nil seam is the only switch).
func NewRecorder(st *store.Store) *Recorder {
	return &Recorder{Store: st}
}

// RecordUsage implements agent.UsageRecorder. An unsettled record is the
// pre-request crash marker (unaccounted); a settled record upgrades it in
// place, so a retried generation cannot double-bill.
func (r *Recorder) RecordUsage(ctx context.Context, scope kernel.Scope, record agent.UsageRecord) error {
	if r == nil || r.Store == nil {
		return nil
	}
	status := store.UsageUnaccounted
	if record.Settled {
		status = store.UsageSettled
	}
	_, err := r.Store.AppendUsage(ctx, store.UsageRecord{
		TenantID:         scope.TenantID,
		SessionID:        scope.SessionID,
		RequestID:        record.RequestID,
		Seq:              record.Seq,
		Provider:         record.Provider,
		Model:            record.Model,
		InputTokens:      int64(record.InputTokens),
		OutputTokens:     int64(record.OutputTokens),
		CacheReadTokens:  int64(record.CacheReadTokens),
		CacheWriteTokens: int64(record.CacheWriteTokens),
		ReasoningTokens:  int64(record.ReasoningTokens),
		Status:           status,
	})
	return err
}

var _ agent.UsageRecorder = (*Recorder)(nil)
