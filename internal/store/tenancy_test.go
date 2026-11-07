package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
)

// Tenant config and usage ledger integration tests. Skipped when KEEL_TEST_DSN
// is unset, like the other store integration tests.

func tenancyTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	st, err := Open(context.Background(), Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func decodeRawJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

func TestTenantConfigRevisionLifecycle(t *testing.T) {
	st := tenancyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "tenant-config-test"); err != nil {
		t.Fatal(err)
	}

	initial, err := st.GetTenantConfig(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Revision != 1 {
		t.Fatalf("initial revision = %d, want 1", initial.Revision)
	}
	if got := decodeRawJSON(t, initial.Config); len(got) != 0 {
		t.Fatalf("initial config = %v, want empty object", got)
	}

	firstWrite := json.RawMessage(`{"agent":{"model":"gpt-5","provider":"openai"}}`)
	revision, err := st.SetTenantConfig(ctx, tenant, firstWrite)
	if err != nil {
		t.Fatal(err)
	}
	if revision != 2 {
		t.Fatalf("first write revision = %d, want 2", revision)
	}
	got, err := st.GetTenantConfig(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 2 {
		t.Fatalf("get revision = %d, want 2", got.Revision)
	}
	agent := decodeRawJSON(t, got.Config)["agent"].(map[string]any)
	if agent["model"] != "gpt-5" || agent["provider"] != "openai" {
		t.Fatalf("stored config = %v", agent)
	}

	revision, err = st.SetTenantConfig(ctx, tenant, json.RawMessage(`{"telemetry":{"adapter":"memory"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if revision != 3 {
		t.Fatalf("second write revision = %d, want 3", revision)
	}
	revision, err = st.GetTenantRevision(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if revision != 3 {
		t.Fatalf("revision = %d, want 3", revision)
	}
	if _, err := st.SetTenantConfig(ctx, tenant, json.RawMessage(`not json`)); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
}

func TestTenantConfigMissingTenant(t *testing.T) {
	st := tenancyTestStore(t)
	ctx := context.Background()
	missing := uuid.New()
	if _, err := st.GetTenantConfig(ctx, missing); !errors.Is(err, ErrNoTenant) {
		t.Fatalf("GetTenantConfig err = %v, want ErrNoTenant", err)
	}
	if _, err := st.SetTenantConfig(ctx, missing, json.RawMessage(`{}`)); !errors.Is(err, ErrNoTenant) {
		t.Fatalf("SetTenantConfig err = %v, want ErrNoTenant", err)
	}
	if _, err := st.GetTenantRevision(ctx, missing); !errors.Is(err, ErrNoTenant) {
		t.Fatalf("GetTenantRevision err = %v, want ErrNoTenant", err)
	}
}

func TestUsageLedgerAppendIsIdempotentAndSums(t *testing.T) {
	st := tenancyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	session := uuid.New()
	otherTenant := uuid.New()
	otherSession := uuid.New()
	for _, id := range []uuid.UUID{tenant, otherTenant} {
		if err := st.CreateTenant(ctx, id, "usage-ledger-test"); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range [][2]uuid.UUID{{tenant, session}, {otherTenant, otherSession}} {
		if err := st.CreateSession(ctx, pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}

	base := UsageRecord{
		TenantID: tenant, SessionID: session, RequestID: "req-1", Seq: 1,
		Provider: "openai", Model: "gpt-5",
		InputTokens: 100, OutputTokens: 40,
		CacheReadTokens: 30, CacheWriteTokens: 20, ReasoningTokens: 10,
	}
	recorded, err := st.AppendUsage(ctx, base)
	if err != nil || !recorded {
		t.Fatalf("first append recorded=%v err=%v", recorded, err)
	}
	// Replaying the same request id must not bill twice, whatever the values in
	// the replay are.
	replay := base
	replay.InputTokens = 999
	recorded, err = st.AppendUsage(ctx, replay)
	if err != nil {
		t.Fatal(err)
	}
	if recorded {
		t.Fatal("duplicate request id was recorded")
	}

	// A second provider request can share the same event seq (for example a
	// compaction summary alongside the main turn); the request id distinguishes
	// them, so both must be recorded.
	second := base
	second.RequestID = "req-2"
	second.InputTokens = 50
	second.OutputTokens = 5
	second.CacheReadTokens = 0
	second.CacheWriteTokens = 0
	second.ReasoningTokens = 0
	if recorded, err := st.AppendUsage(ctx, second); err != nil || !recorded {
		t.Fatalf("same-seq second request recorded=%v err=%v", recorded, err)
	}
	third := base
	third.RequestID = "req-3"
	third.Seq = 3
	third.Model = "gpt-5-mini"
	third.InputTokens = 7
	third.OutputTokens = 1
	third.CacheReadTokens = 0
	third.CacheWriteTokens = 0
	third.ReasoningTokens = 0
	if recorded, err := st.AppendUsage(ctx, third); err != nil || !recorded {
		t.Fatalf("third append recorded=%v err=%v", recorded, err)
	}
	// An unaccounted request (crash after the provider call, before the ledger
	// append) is recorded with zero tokens so reconciliation can see it.
	unaccounted := base
	unaccounted.RequestID = "req-4"
	unaccounted.Seq = 4
	unaccounted.InputTokens = 0
	unaccounted.OutputTokens = 0
	unaccounted.CacheReadTokens = 0
	unaccounted.CacheWriteTokens = 0
	unaccounted.ReasoningTokens = 0
	unaccounted.Status = UsageUnaccounted
	if recorded, err := st.AppendUsage(ctx, unaccounted); err != nil || !recorded {
		t.Fatalf("unaccounted append recorded=%v err=%v", recorded, err)
	}
	// A different tenant may reuse the same request id without colliding.
	foreign := base
	foreign.TenantID = otherTenant
	foreign.SessionID = otherSession
	foreign.InputTokens = 5000
	if recorded, err := st.AppendUsage(ctx, foreign); err != nil || !recorded {
		t.Fatalf("foreign append recorded=%v err=%v", recorded, err)
	}

	totals, err := st.SumUsage(ctx, UsageQuery{TenantID: tenant})
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 2 {
		t.Fatalf("groups = %d, want 2: %+v", len(totals), totals)
	}
	major := totals[0]
	if major.Model != "gpt-5" || major.Entries != 3 || major.UnaccountedEntries != 1 {
		t.Fatalf("major group = %+v", major)
	}
	if major.InputTokens != 150 || major.OutputTokens != 45 ||
		major.CacheReadTokens != 30 || major.CacheWriteTokens != 20 ||
		major.ReasoningTokens != 10 {
		t.Fatalf("major totals = %+v", major)
	}
	minor := totals[1]
	if minor.Model != "gpt-5-mini" || minor.Entries != 1 || minor.InputTokens != 7 {
		t.Fatalf("minor group = %+v", minor)
	}

	// Recovering the lost usage upgrades the unaccounted row in place: entries
	// stay at 3 and the tokens appear exactly once.
	settled := unaccounted
	settled.InputTokens = 33
	settled.OutputTokens = 3
	settled.Status = UsageSettled
	if recorded, err := st.AppendUsage(ctx, settled); err != nil || !recorded {
		t.Fatalf("upgrade recorded=%v err=%v", recorded, err)
	}
	if recorded, err := st.AppendUsage(ctx, settled); err != nil || recorded {
		t.Fatalf("settled replay recorded=%v err=%v", recorded, err)
	}
	totals, err = st.SumUsage(ctx, UsageQuery{TenantID: tenant, Model: "gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 1 {
		t.Fatalf("gpt-5 groups = %+v", totals)
	}
	if totals[0].Entries != 3 || totals[0].UnaccountedEntries != 0 ||
		totals[0].InputTokens != 183 || totals[0].OutputTokens != 48 {
		t.Fatalf("upgraded totals = %+v", totals[0])
	}

	filtered, err := st.SumUsage(ctx, UsageQuery{TenantID: tenant, Model: "gpt-5-mini"})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Model != "gpt-5-mini" {
		t.Fatalf("model filter = %+v", filtered)
	}
	filtered, err = st.SumUsage(ctx, UsageQuery{TenantID: tenant, SessionID: &session, Provider: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 2 {
		t.Fatalf("session filter groups = %d, want 2", len(filtered))
	}
	unknown := uuid.New()
	empty, err := st.SumUsage(ctx, UsageQuery{TenantID: tenant, SessionID: &unknown})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("unknown session totals = %+v", empty)
	}
}

func TestUsageLedgerValidatesIdentity(t *testing.T) {
	st := tenancyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	session := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "usage-validation"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	record := UsageRecord{TenantID: tenant, SessionID: session, Seq: 1}
	if _, err := st.AppendUsage(ctx, record); err == nil {
		t.Fatal("empty request id was accepted")
	}
	record.RequestID = "req-1"
	record.Status = "bogus"
	if _, err := st.AppendUsage(ctx, record); err == nil {
		t.Fatal("unknown status was accepted")
	}
	record.Status = UsageSettled
	record.InputTokens = -1
	if _, err := st.AppendUsage(ctx, record); err == nil {
		t.Fatal("negative token count was accepted")
	}
}
