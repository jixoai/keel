package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestSessionCheckpointPolicyRoundTrip pins the durable override: a missing row
// reports found=false, a write creates revision 1, a second write bumps it, and
// a missing session is ErrNoSession rather than an orphan row.
func TestSessionCheckpointPolicyRoundTrip(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tenant, session := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "checkpoint-policy-test"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}

	if _, found, err := st.GetSessionCheckpointPolicy(ctx, tenant, session); err != nil || found {
		t.Fatalf("empty override = (found=%v, err=%v), want none", found, err)
	}

	doc := json.RawMessage(`{"maxCheckpoints":2,"ttlHours":12}`)
	revision, err := st.SetSessionCheckpointPolicy(ctx, tenant, session, doc)
	if err != nil {
		t.Fatalf("set override: %v", err)
	}
	if revision != 1 {
		t.Fatalf("revision = %d, want 1", revision)
	}
	got, found, err := st.GetSessionCheckpointPolicy(ctx, tenant, session)
	if err != nil || !found || got.Revision != 1 {
		t.Fatalf("get override = (%+v, found=%v, err=%v)", got, found, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got.Policy, &decoded); err != nil {
		t.Fatalf("stored policy is not JSON: %v", err)
	}
	if decoded["maxCheckpoints"] != float64(2) || decoded["ttlHours"] != float64(12) {
		t.Fatalf("stored policy = %s", got.Policy)
	}

	revision, err = st.SetSessionCheckpointPolicy(ctx, tenant, session, json.RawMessage(`{"manualCompact":false}`))
	if err != nil {
		t.Fatalf("update override: %v", err)
	}
	if revision != 2 {
		t.Fatalf("revision after update = %d, want 2", revision)
	}
	got, found, err = st.GetSessionCheckpointPolicy(ctx, tenant, session)
	if err != nil || !found || got.Revision != 2 {
		t.Fatalf("updated override = (%+v, found=%v, err=%v)", got, found, err)
	}

	// The tenant boundary is the same as every other row: another tenant's
	// session is not found and never receives the first tenant's override.
	foreignTenant, foreignSession := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, foreignTenant, "checkpoint-policy-foreign"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, foreignTenant, foreignSession); err != nil {
		t.Fatal(err)
	}
	if _, found, err := st.GetSessionCheckpointPolicy(ctx, foreignTenant, foreignSession); err != nil || found {
		t.Fatalf("foreign override = (found=%v, err=%v), want none", found, err)
	}
	if _, found, err := st.GetSessionCheckpointPolicy(ctx, tenant, foreignSession); found || !errors.Is(err, ErrNoSession) {
		t.Fatalf("cross-tenant override = (found=%v, err=%v), want ErrNoSession", found, err)
	}

	// A missing session refuses both reads and writes.
	missing := uuid.New()
	if _, _, err := st.GetSessionCheckpointPolicy(ctx, tenant, missing); !errors.Is(err, ErrNoSession) {
		t.Fatalf("missing get err = %v, want ErrNoSession", err)
	}
	if _, err := st.SetSessionCheckpointPolicy(ctx, tenant, missing, json.RawMessage(`{}`)); !errors.Is(err, ErrNoSession) {
		t.Fatalf("missing set err = %v, want ErrNoSession", err)
	}
}

// TestSessionCheckpointPolicyRejectsInvalidJSON pins the store's JSON guard.
func TestSessionCheckpointPolicyRejectsInvalidJSON(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tenant, session := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "checkpoint-policy-invalid"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetSessionCheckpointPolicy(ctx, tenant, session, json.RawMessage(`{`)); err == nil {
		t.Fatal("invalid JSON was stored")
	}
}
