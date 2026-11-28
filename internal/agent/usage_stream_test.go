package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
)

// recordingUsage is the ledger seam's test double: it keeps every record in
// order so the unaccounted -> settled lifecycle is observable.
type recordingUsage struct {
	mu      sync.Mutex
	records []UsageRecord
}

func (r *recordingUsage) RecordUsage(_ context.Context, _ kernel.Scope, record UsageRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record)
	return nil
}

func (r *recordingUsage) snapshot() []UsageRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]UsageRecord(nil), r.records...)
}

// TestUsageStreamRecordsRequestLifecycle pins the production ledger wiring: the
// runner wraps every provider request, writes the pre-request crash marker,
// settles it with the provider-reported numbers, and the attempt summary reads
// the same values.
func TestUsageStreamRecordsRequestLifecycle(t *testing.T) {
	mem := kernel.NewMemStore()
	kern := kernel.New(mem, kernel.Options{})
	ctx := context.Background()
	tenant, session := uuid.New(), uuid.New()
	mem.CreateSession(tenant, session)
	mem.EnqueueRun(tenant, session)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, kern, scope, "usage-r1", "hello", "", nil)
	lease := claimScope(t, kern, tenant, session)

	recorder := &recordingUsage{}
	runner := NewRunner(kern, kern, usageStream(), "fake", "fake", "", discardLog())
	runner.UsageRecorder = recorder
	res, err := runner.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	records := recorder.snapshot()
	if len(records) != 2 {
		t.Fatalf("usage records = %d (%+v), want marker + settlement", len(records), records)
	}
	marker, settled := records[0], records[1]
	if marker.Settled {
		t.Fatalf("first record is settled: %+v", marker)
	}
	if !settled.Settled {
		t.Fatalf("second record is not settled: %+v", settled)
	}
	if marker.RequestID == "" || marker.RequestID != settled.RequestID {
		t.Fatalf("request ids = %q / %q, want one deterministic id", marker.RequestID, settled.RequestID)
	}
	if settled.InputTokens != 11 || settled.OutputTokens != 4 {
		t.Fatalf("settled tokens = %d/%d, want 11/4", settled.InputTokens, settled.OutputTokens)
	}
	if res.Summary.Tokens.Input != settled.InputTokens || res.Summary.Tokens.Output != settled.OutputTokens {
		t.Fatalf("summary tokens = %+v, ledger = %d/%d (must read the same values)",
			res.Summary.Tokens, settled.InputTokens, settled.OutputTokens)
	}
	if settled.Seq <= 0 {
		t.Fatalf("ledger seq = %d, want the prompting event's seq", settled.Seq)
	}
}

// TestUsageStreamDoesNotSettleWithoutUsage pins the honest crash/exception
// row: a terminal event or cancellation without a provider usage report leaves
// the pre-request marker unaccounted instead of settling a false zero.
func TestUsageStreamDoesNotSettleWithoutUsage(t *testing.T) {
	recorder := &recordingUsage{}
	scope := kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()}
	base := func(context.Context, string, provider.LlmContext, provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		msg := agentcore.AssistantMessage{
			RoleField:  agentcore.RoleAssistant,
			Content:    agentcore.ContentList{agentcore.NewTextContent("ok")},
			StopReason: agentcore.StopReasonEndTurn,
		}
		s := provider.NewAssistantMessageEventStream(2)
		go func() {
			defer s.Close()
			_ = s.Emit(context.Background(), provider.StreamDoneEvent{Message: msg})
		}()
		return s, nil
	}
	stream := NewUsageStream(base, recorder, scope, "u1-a0", 3, "prov", "model")
	out, err := stream(context.Background(), "model", provider.LlmContext{}, provider.StreamConfig{})
	if err != nil {
		t.Fatalf("wrapped stream: %v", err)
	}
	for range out.Events() {
	}
	records := recorder.snapshot()
	if len(records) != 1 {
		t.Fatalf("records = %+v, want only the unaccounted marker", records)
	}
	if records[0].Settled {
		t.Fatalf("no-usage terminal event was settled as zero: %+v", records[0])
	}
}

// TestUsageStreamSettlesExplicitZeroUsage is the other half: a provider that
// explicitly reports an empty usage object DID measure zero tokens, so that
// settles.
func TestUsageStreamSettlesExplicitZeroUsage(t *testing.T) {
	recorder := &recordingUsage{}
	scope := kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()}
	base := func(context.Context, string, provider.LlmContext, provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		msg := agentcore.AssistantMessage{
			RoleField:  agentcore.RoleAssistant,
			Content:    agentcore.ContentList{agentcore.NewTextContent("ok")},
			StopReason: agentcore.StopReasonEndTurn,
			Usage:      &agentcore.Usage{},
		}
		s := provider.NewAssistantMessageEventStream(2)
		go func() {
			defer s.Close()
			_ = s.Emit(context.Background(), provider.StreamDoneEvent{Message: msg})
		}()
		return s, nil
	}
	stream := NewUsageStream(base, recorder, scope, "u1-a0", 3, "prov", "model")
	out, err := stream(context.Background(), "model", provider.LlmContext{}, provider.StreamConfig{})
	if err != nil {
		t.Fatalf("wrapped stream: %v", err)
	}
	for range out.Events() {
	}
	records := recorder.snapshot()
	if len(records) != 2 || records[0].Settled || !records[1].Settled {
		t.Fatalf("records = %+v, want marker then explicit zero settlement", records)
	}
}

// TestUsageRecordOutlivesCancellation pins that a cancelled model stream still
// closes the accounting row: the write context outlives the attempt, or the
// ledger would report a possibly-billed request as never settled.
func TestUsageRecordOutlivesCancellation(t *testing.T) {
	recorder := &ctxStrictUsage{}
	observer := &usageObserver{recorder: recorder}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	observer.settle(ctx, "r1", "model", "prov", &agentcore.Usage{InputTokens: 7, OutputTokens: 2})
	records := recorder.snapshot()
	if len(records) != 1 {
		t.Fatalf("records = %+v, want one settlement", records)
	}
	if !records[0].Settled || records[0].InputTokens != 7 || records[0].OutputTokens != 2 {
		t.Fatalf("settled record = %+v, want 7/2", records[0])
	}
}

// ctxStrictUsage refuses a write whose context is already cancelled, proving
// the observer detaches accounting from the attempt context.
type ctxStrictUsage struct {
	recordingUsage
}

func (c *ctxStrictUsage) RecordUsage(ctx context.Context, scope kernel.Scope, record UsageRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.recordingUsage.RecordUsage(ctx, scope, record)
}
