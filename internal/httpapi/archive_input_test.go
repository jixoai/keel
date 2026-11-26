package httpapi

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

// TestCommitClientInputRefusesArchivedSession pins A1's archived -> running
// transition: an archived session is read-only, so native input is refused
// before any write (no event) and a missing session keeps its own not-found
// path instead of being reported as archived.
func TestCommitClientInputRefusesArchivedSession(t *testing.T) {
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
	if err := st.CreateTenant(ctx, tenant, "archived-"+tenant.String()[:8]); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := st.ArchiveSession(ctx, tenant, session); err != nil {
		t.Fatalf("archive session: %v", err)
	}

	edge := kernel.New(kernel.NewMemStore(), kernel.Options{})
	_, err = CommitClientInput(ctx, st, edge, ClientInput{
		TenantID: tenant, SessionID: session, RequestID: "archived-1", Text: "hello",
	}, ClientInputOptions{})
	if !errors.Is(err, ErrInputArchivedSession) {
		t.Fatalf("archived input = %v, want ErrInputArchivedSession", err)
	}
	events, err := st.LoadEvents(ctx, tenant, session, 0, 10)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("a refused archived input appended %d events", len(events))
	}

	if _, err := CommitClientInput(ctx, st, edge, ClientInput{
		TenantID: tenant, SessionID: uuid.New(), RequestID: "missing-1", Text: "hello",
	}, ClientInputOptions{}); !errors.Is(err, store.ErrNoSession) {
		t.Fatalf("missing session input = %v, want ErrNoSession", err)
	}
}
