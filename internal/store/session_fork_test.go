package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/sessionfork"
)

func TestSessionForkRoundTripAndRefusals(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant, parent := runTestSession(t, st)
	child := uuid.New()
	if err := st.CreateSession(ctx, tenant, child); err != nil {
		t.Fatal(err)
	}
	fact := sessionfork.Fact{
		ParentSessionID:  parent,
		OriginSeq:        42,
		TreeHash:         "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ConfigHash:       "sha256:config",
		ToolManifestHash: "sha256:manifest",
		AnchorSeq:        40,
	}
	if err := st.RecordSessionFork(ctx, tenant, child, fact); err != nil {
		t.Fatalf("record fork: %v", err)
	}
	// Idempotent for the same fact.
	if err := st.RecordSessionFork(ctx, tenant, child, fact); err != nil {
		t.Fatalf("record same fork again: %v", err)
	}
	got, found, err := st.SessionFork(ctx, tenant, child)
	if err != nil || !found {
		t.Fatalf("read fork: found=%v err=%v", found, err)
	}
	if got != fact {
		t.Fatalf("fork fact = %+v, want %+v", got, fact)
	}
	// A second origin cannot re-point a deterministic child id.
	other := sessionfork.Fact{ParentSessionID: uuid.New(), OriginSeq: 7}
	if err := st.RecordSessionFork(ctx, tenant, child, other); err == nil {
		t.Fatal("re-pointing a forked session at another parent must fail")
	}
	// A missing session is a typed refusal.
	if err := st.RecordSessionFork(ctx, tenant, uuid.New(), fact); !errors.Is(err, ErrNoSession) {
		t.Fatalf("record missing session = %v, want ErrNoSession", err)
	}
	// A normal session is not a fork.
	plain := uuid.New()
	if err := st.CreateSession(ctx, tenant, plain); err != nil {
		t.Fatal(err)
	}
	if _, found, err := st.SessionFork(ctx, tenant, plain); err != nil || found {
		t.Fatalf("plain session fork = found=%v err=%v", found, err)
	}
	// Another tenant cannot read the fact.
	if _, _, err := st.SessionFork(ctx, uuid.New(), child); !errors.Is(err, ErrNoSession) {
		t.Fatalf("cross-tenant fork read = %v, want ErrNoSession", err)
	}
}

// TestSessionForkCleanupHelpers pins the rollback primitives: a fork fact can be
// cleared, an archived retry child can be revived, and only a session with no
// durable reference is deleted.
func TestSessionForkCleanupHelpers(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant, parent := runTestSession(t, st)
	child := uuid.New()
	if err := st.CreateSession(ctx, tenant, child); err != nil {
		t.Fatal(err)
	}
	fact := sessionfork.Fact{ParentSessionID: parent, OriginSeq: 9, TreeHash: "sha256:tree"}
	if err := st.RecordSessionFork(ctx, tenant, child, fact); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearSessionFork(ctx, tenant, child); err != nil {
		t.Fatal(err)
	}
	if _, found, err := st.SessionFork(ctx, tenant, child); err != nil || found {
		t.Fatalf("cleared fork = found=%v err=%v", found, err)
	}

	// Archive then revive (the retry path).
	if archived, err := st.ArchiveSession(ctx, tenant, child); err != nil || !archived {
		t.Fatalf("archive = %v err=%v", archived, err)
	}
	if err := st.EnsureSessionActive(ctx, tenant, child); err != nil {
		t.Fatal(err)
	}
	var archivedAt *time.Time
	if err := st.Pool().QueryRow(ctx,
		`SELECT archived_at FROM sessions WHERE tenant_id = $1 AND session_id = $2`,
		tenant, child).Scan(&archivedAt); err != nil {
		t.Fatal(err)
	}
	if archivedAt != nil {
		t.Fatalf("session still archived at %v", archivedAt)
	}

	// An empty session is deleted; one with an event is not.
	if deleted, err := st.DeleteSessionIfEmpty(ctx, tenant, child); err != nil || !deleted {
		t.Fatalf("delete empty = %v err=%v", deleted, err)
	}
	withEvent := uuid.New()
	if err := st.CreateSession(ctx, tenant, withEvent); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO events (tenant_id, session_id, seq, kind, payload)
		 VALUES ($1, $2, 1, 'user.message', '{"text":"x"}'::jsonb)`,
		tenant, withEvent); err != nil {
		t.Fatal(err)
	}
	if deleted, err := st.DeleteSessionIfEmpty(ctx, tenant, withEvent); err != nil || deleted {
		t.Fatalf("delete session with events = %v err=%v", deleted, err)
	}
}
