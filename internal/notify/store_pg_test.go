package notify

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"
)

// Postgres notification round trip, due selection and tenant boundary. Skipped
// without KEEL_TEST_DSN, like the other store integration tests.
func TestPostgresNotificationStore(t *testing.T) {
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
	if err := st.CreateTenant(ctx, tenant, "notify-store-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM notifications WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM tenants WHERE tenant_id = $1`, tenant)
	})
	session := uuid.New()
	pg := NewPostgresStore(st)
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	expires := now.Add(time.Hour)
	id := uuid.New()

	row := Notification{
		NotificationID: id.String(),
		TenantID:       tenant.String(),
		SessionID:      session.String(),
		Title:          "Keel",
		Text:           "please look",
		Status:         StatusDelivered,
		CreatedAt:      now,
		Deadline:       now.Add(10 * time.Second),
		UpdatedAt:      now,
		Consumers: []Consumer{{
			ID: "mail", Kind: ConsumerURL, AckCapable: true,
			TokenHash: hashToken("secret-token"), TokenExpiresAt: &expires,
		}},
	}
	stored, created, err := pg.Create(ctx, row)
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	if stored.Consumers[0].TokenHash != hashToken("secret-token") {
		t.Fatalf("token hash lost: %+v", stored.Consumers[0])
	}
	again, created, err := pg.Create(ctx, row)
	if err != nil || created || again.NotificationID != id.String() {
		t.Fatalf("duplicate create = created=%v err=%v", created, err)
	}

	got, err := pg.GetAny(ctx, id)
	if err != nil || got.Text != "please look" {
		t.Fatalf("get any = %+v err=%v", got, err)
	}
	if _, err := pg.Get(ctx, uuid.New(), id); err != ErrNotFound {
		t.Fatalf("foreign get = %v, want ErrNotFound", err)
	}
	list, err := pg.List(ctx, tenant, &session, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d err=%v", len(list), err)
	}
	unrelated := uuid.New()
	otherSession, err := pg.List(ctx, tenant, &unrelated, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(otherSession) != 0 {
		t.Fatalf("unrelated session list = %d", len(otherSession))
	}

	// Before the deadline nothing is due.
	due, err := pg.ListDue(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range due {
		if candidate.TenantID == tenant.String() {
			t.Fatalf("future notification was due: %+v", candidate)
		}
	}
	// After the deadline it is due exactly until claimed.
	due, err = pg.ListDue(ctx, now.Add(11*time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range due {
		if candidate.TenantID == tenant.String() {
			found = true
		}
	}
	if !found {
		t.Fatal("due notification not listed")
	}
	claimed, err := pg.ClaimPush(ctx, tenant, id, now.Add(11*time.Second))
	if err != nil || !claimed {
		t.Fatalf("claim = %v err=%v", claimed, err)
	}
	if claimed, _ := pg.ClaimPush(ctx, tenant, id, now.Add(12*time.Second)); claimed {
		t.Fatal("second claim succeeded")
	}
	// A claimed row without a push record is listed for reconciliation.
	due, err = pg.ListDue(ctx, now.Add(12*time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, candidate := range due {
		if candidate.TenantID == tenant.String() && candidate.Push == nil && candidate.PushAttemptedAt != nil {
			found = true
		}
	}
	if !found {
		t.Fatal("claimed-but-unrecorded notification not listed")
	}
	// Record the push and make sure it leaves the due set.
	claimedAt := now.Add(11 * time.Second)
	record := PushRecord{Status: PushSent, Channel: "native", At: claimedAt}
	got.PushAttemptedAt = &claimedAt
	got.Push = &record
	got.Status = StatusDelivered
	got.UpdatedAt = now.Add(11 * time.Second)
	if err := pg.Save(ctx, got); err != nil {
		t.Fatal(err)
	}
	due, err = pg.ListDue(ctx, now.Add(12*time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range due {
		if candidate.TenantID == tenant.String() {
			t.Fatalf("pushed notification still due: %+v", candidate)
		}
	}
}
