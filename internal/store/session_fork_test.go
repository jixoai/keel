package store

import (
	"context"
	"errors"
	"testing"

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
