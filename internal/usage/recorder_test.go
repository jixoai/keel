package usage

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

// TestRecorderWritesTheExistingLedger pins the production adapter: the
// pre-request marker is unaccounted, the settled record upgrades it in place,
// and the ledger aggregate reads the provider's numbers. It is the only write
// path, so a missing row here means provider usage is not accounted at all.
func TestRecorderWritesTheExistingLedger(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	tenant, session := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "usage-adapter-test"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	scope := kernel.Scope{TenantID: tenant, SessionID: session}
	recorder := NewRecorder(st)
	requestID := "u4-a0-r0"

	if err := recorder.RecordUsage(ctx, scope, agent.UsageRecord{
		RequestID: requestID, Seq: 4, Provider: "openai", Model: "gpt-5",
	}); err != nil {
		t.Fatalf("marker: %v", err)
	}
	if err := recorder.RecordUsage(ctx, scope, agent.UsageRecord{
		RequestID: requestID, Seq: 4, Provider: "openai", Model: "gpt-5",
		Settled: true, InputTokens: 11, OutputTokens: 4, CacheReadTokens: 3,
	}); err != nil {
		t.Fatalf("settlement: %v", err)
	}
	// A replay of the same generation must not bill twice.
	if err := recorder.RecordUsage(ctx, scope, agent.UsageRecord{
		RequestID: requestID, Seq: 4, Provider: "openai", Model: "gpt-5",
		Settled: true, InputTokens: 11, OutputTokens: 4, CacheReadTokens: 3,
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	totals, err := st.SumUsage(ctx, store.UsageQuery{TenantID: tenant})
	if err != nil {
		t.Fatalf("sum usage: %v", err)
	}
	if len(totals) != 1 {
		t.Fatalf("totals = %+v, want one bucket", totals)
	}
	got := totals[0]
	if got.SessionID != session || got.Provider != "openai" || got.Model != "gpt-5" {
		t.Fatalf("bucket = %+v", got)
	}
	if got.Entries != 1 || got.UnaccountedEntries != 0 {
		t.Fatalf("entries = %d / unaccounted = %d, want 1/0", got.Entries, got.UnaccountedEntries)
	}
	if got.InputTokens != 11 || got.OutputTokens != 4 || got.CacheReadTokens != 3 {
		t.Fatalf("tokens = %d/%d cache %d, want 11/4 cache 3",
			got.InputTokens, got.OutputTokens, got.CacheReadTokens)
	}
}
