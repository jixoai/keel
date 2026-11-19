package approval

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"
)

// Postgres provenance round trip and tenant boundary. Skipped without
// KEEL_TEST_DSN, like the other store integration tests.
func TestPostgresProvenanceRoundTrip(t *testing.T) {
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
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "approval-provenance-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM approval_provenance WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM tenants WHERE tenant_id = $1`, tenant)
	})

	session := uuid.New()
	decision := uuid.New()
	service := NewService(Options{Store: NewPostgresStore(st)})
	opened, created, err := service.Open(ctx, Record{
		TenantID:   tenant.String(),
		SessionID:  session.String(),
		DecisionID: decision.String(),
		Kind:       KindConfirm,
		Mode:       ModeConfirm,
		Actor:      "agent",
		Channel:    ChannelTool,
	})
	if err != nil || !created {
		t.Fatalf("open: created=%v err=%v", created, err)
	}
	if opened.Status != StatusOpen {
		t.Fatalf("opened = %+v", opened)
	}

	answer := Record{
		TenantID:   tenant.String(),
		SessionID:  session.String(),
		DecisionID: decision.String(),
		Kind:       KindConfirm,
		Mode:       ModeConfirm,
		Validated:  true,
		OptionID:   "yes",
		Actor:      "owner@example.test",
		Channel:    ChannelHTTP,
		Context:    map[string]string{"remoteAddr": "127.0.0.1:1"},
	}
	stored, duplicate, err := service.RecordAnswer(ctx, answer)
	if err != nil || duplicate {
		t.Fatalf("answer: duplicate=%v err=%v", duplicate, err)
	}
	if stored.Status != StatusAnswered || stored.Text != "" || stored.OptionID != "yes" {
		t.Fatalf("stored = %+v", stored)
	}
	if stored.Context["remoteAddr"] != "127.0.0.1:1" {
		t.Fatalf("context lost: %+v", stored.Context)
	}
	again, duplicate, err := service.RecordAnswer(ctx, answer)
	if err != nil || !duplicate {
		t.Fatalf("duplicate answer: duplicate=%v err=%v", duplicate, err)
	}
	if again.Actor != "owner@example.test" {
		t.Fatalf("duplicate lost actor: %+v", again)
	}

	if _, found, err := service.Get(ctx, uuid.New(), session, decision); err != nil || found {
		t.Fatalf("foreign tenant read = found=%v err=%v", found, err)
	}
	if _, found, err := service.Get(ctx, tenant, session, decision); err != nil || !found {
		t.Fatalf("own read = found=%v err=%v", found, err)
	}

	conflict := answer
	conflict.OptionID = "no"
	if _, _, err := service.RecordAnswer(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict = %v, want ErrConflict", err)
	}
}
