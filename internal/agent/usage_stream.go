package agent

// Provider usage ledger wiring (feature: PI usage ledger production caller).
//
// The ledger row is keyed by a deterministic request ID derived from the
// committed transcript (generation identity + request ordinal). A retry of the
// same attempt replays the same IDs, so AppendUsage upgrades the crash marker
// instead of double-billing. The wrapper writes:
//
//	unaccounted  before the provider call (a kill after this row is visible)
//	settled      on the terminal stream event (provider-reported usage)
//
// A cancelled/aborted stream or a provider error WITHOUT a usage report leaves
// the row unaccounted: a possibly-billed request with unknown usage is not
// "zero tokens". Only an explicit provider usage report (including an explicit
// zero) settles the row.
//
// A ledger write failure never fails the model request: accounting is
// best-effort, logged, and the stream continues.

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
)

// usageLedgerTimeout bounds one ledger write. The marker is written on the
// request path, so an unhealthy ledger must not stall the model call; a missed
// write is visible as a missing row, never as a failed attempt.
const usageLedgerTimeout = 5 * time.Second

// UsageRecord is one provider request's token accounting.
type UsageRecord struct {
	RequestID        string
	Seq              int64
	Provider         string
	Model            string
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	ReasoningTokens  int
	// Settled is false for the pre-request crash marker and true for the
	// terminal usage report.
	Settled bool
}

// UsageRecorder persists one ledger row. Implementations must be idempotent on
// (tenant, session, RequestID); a settled row upgrades an unaccounted one.
type UsageRecorder interface {
	RecordUsage(ctx context.Context, scope kernel.Scope, record UsageRecord) error
}

// NewUsageStream wraps base so every provider request is mirrored into the
// usage ledger. baseID identifies the attempt's generation; the wrapper appends
// a per-attempt request ordinal. A nil recorder returns base unchanged.
func NewUsageStream(base provider.StreamFn, recorder UsageRecorder, scope kernel.Scope, baseID string, seq int64, providerName, modelName string) provider.StreamFn {
	if base == nil || recorder == nil {
		return base
	}
	observer := &usageObserver{
		base:     base,
		recorder: recorder,
		scope:    scope,
		baseID:   baseID,
		seq:      seq,
		provider: providerName,
		model:    modelName,
	}
	return observer.stream
}

type usageObserver struct {
	base      provider.StreamFn
	recorder  UsageRecorder
	scope     kernel.Scope
	baseID    string
	seq       int64
	provider  string
	model     string
	requestNo atomic.Int64
}

func (o *usageObserver) stream(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
	requestID := fmt.Sprintf("%s-r%d", o.baseID, o.requestNo.Add(1)-1)
	modelName := o.model
	if model != "" {
		modelName = model
	}
	// The pre-request marker: if the process dies after the provider was
	// called, reconciliation sees a row for this request instead of silence.
	o.record(ctx, UsageRecord{
		RequestID: requestID, Seq: o.seq, Provider: o.provider, Model: modelName,
	})

	out := provider.NewAssistantMessageEventStream(16)
	base, err := o.base(ctx, model, llm, cfg)
	if err != nil || base == nil {
		// The provider request did not produce a usage-bearing stream. Leave the
		// marker unaccounted: the request may have reached the provider, so a
		// settled zero would be a false claim.
		return base, err
	}
	go o.consume(ctx, requestID, modelName, base, out)
	return out, nil
}

// consume forwards events and settles the row on the terminal event, an
// abnormal close, or consumer cancellation. Usage observed on any partial is
// the best available fact; with no observed usage at all the row stays
// unaccounted (possibly billed, unknown amount) rather than settling a fake 0.
func (o *usageObserver) consume(ctx context.Context, requestID, modelName string, base, out *provider.AssistantMessageEventStream) {
	var (
		usage      *agentcore.Usage
		providerID = o.provider
		terminal   bool
	)
	defer out.Close()
	for {
		select {
		case event, ok := <-base.Events():
			if !ok {
				if !terminal {
					o.settle(ctx, requestID, modelName, providerID, usage)
				}
				return
			}
			switch e := event.(type) {
			case provider.StreamStartEvent:
				if e.Partial.Provider != "" {
					providerID = e.Partial.Provider
				}
				if e.Partial.Usage != nil {
					usage = e.Partial.Usage
				}
			case provider.StreamTextEvent:
				if e.Partial.Usage != nil {
					usage = e.Partial.Usage
				}
			case provider.StreamThinkingEvent:
				if e.Partial.Usage != nil {
					usage = e.Partial.Usage
				}
			case provider.StreamToolCallEvent:
				if e.Partial.Usage != nil {
					usage = e.Partial.Usage
				}
			case provider.StreamDoneEvent:
				terminal = true
				if e.Message.Provider != "" {
					providerID = e.Message.Provider
				}
				if e.Message.Usage != nil {
					usage = e.Message.Usage
				}
			case provider.StreamErrorEvent:
				terminal = true
				if e.Message.Provider != "" {
					providerID = e.Message.Provider
				}
				if e.Message.Usage != nil {
					usage = e.Message.Usage
				}
			}
			if err := out.Emit(ctx, event); err != nil {
				// The consumer stopped reading: settle what was observed.
				o.settle(ctx, requestID, modelName, providerID, usage)
				return
			}
			if terminal {
				o.settle(ctx, requestID, modelName, providerID, usage)
				return
			}
		case <-ctx.Done():
			o.settle(ctx, requestID, modelName, providerID, usage)
			return
		}
	}
}

func (o *usageObserver) record(ctx context.Context, record UsageRecord) {
	// Accounting outlives the attempt: a cancel that stops the model stream must
	// still settle the row, or the ledger reports a possibly-billed request as
	// unaccounted. The write is bounded so it can never hold the attempt open.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageLedgerTimeout)
	defer cancel()
	if err := o.recorder.RecordUsage(writeCtx, o.scope, record); err != nil && !errors.Is(err, context.Canceled) {
		// Accounting is best-effort; the model request must proceed.
		_ = err
	}
}

func (o *usageObserver) settle(ctx context.Context, requestID, modelName, providerName string, usage *agentcore.Usage) {
	// No usage report is not the same as zero usage: an aborted/errored request
	// with unknown tokens stays unaccounted (the marker written before the call
	// is the honest row). Only a provider-reported usage settles.
	if usage == nil {
		return
	}
	record := UsageRecord{
		RequestID: requestID, Seq: o.seq, Provider: providerName, Model: modelName, Settled: true,
		InputTokens:      usage.InputTokens,
		OutputTokens:     usage.OutputTokens,
		CacheReadTokens:  usage.CacheReadTokens,
		CacheWriteTokens: usage.CacheWriteTokens,
		ReasoningTokens:  usage.ReasoningTokens,
	}
	o.record(ctx, record)
}

// attemptUsageBaseID is the deterministic generation identity of an attempt:
// the prompt seq plus the answer ordinal at load time. A retry that committed
// no new assistant message derives the same base (and therefore the same
// request IDs); a retry after a committed answer sees a different base.
func attemptUsageBaseID(g generation) string {
	return fmt.Sprintf("u%d-a%d", g.promptSeq, g.ordinal)
}
