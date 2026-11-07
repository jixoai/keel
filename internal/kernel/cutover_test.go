package kernel

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"
)

// TestLegacyWriterCutoverDiscipline documents the cross-version cutover window
// (adversarial review, high-risk scenario 3): an OLD binary (store-based
// writers, no fencing) sharing a database with a NEW binary (kernel writers).
//
// Two facts are pinned here, and they are the reason the upgrade discipline is
// "drain the old process BEFORE starting the new binary; never run two writers
// side by side":
//
//  1. a legacy append is not fenced — it lands in the log even while a kernel
//     owner holds the session, because the old code path never consults the
//     epoch (the kernel can only reject writers that present a token);
//  2. the legacy finish leaves the lease behind (sessions.lease_until is not
//     cleared by store.FinishRun), so a new kernel process answers
//     ErrLeaseHeld for up to one lease TTL after an old-style finish.
//
// The handoff protocol itself is the mitigation: the old process drains (stops
// claiming, releases its lease) before the new one takes over, and the kernel's
// idempotency keys keep a replayed fact from duplicating.
func TestLegacyWriterCutoverDiscipline(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	kpg, err := NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	k := New(kpg, Options{})

	tenant, session := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "cutover"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	scope := Scope{TenantID: tenant, SessionID: session}
	if _, err := k.Commit(ctx, CommitRequest{Scope: scope, Kind: KindUserMessage,
		RequestID: "c1", Payload: userPayload("one")}); err != nil {
		t.Fatal(err)
	}

	// The new binary claims the session; the OLD binary then finishes it with
	// its own store-based settle (the cutover window: both binaries alive).
	lease, err := k.ClaimScope(ctx, scope, "runtime-new", 5*time.Second)
	if err != nil {
		t.Fatalf("kernel claim: %v", err)
	}
	run, err := st.GetRun(ctx, tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.FinishRun(ctx, tenant, session, 1, nil, 3, run.Attempts); err != nil {
		t.Fatalf("legacy finish: %v", err)
	}
	// Fact 2: the lease residue. store.FinishRun settles the run row but never
	// clears the session authority, so the kernel still sees a live lease.
	state, err := k.LeaseState(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Found || !state.Live {
		t.Fatalf("legacy finish did not leave the lease residue: %+v", state)
	}
	if _, err := k.ClaimScope(ctx, scope, "runtime-other", time.Minute); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("kernel claim during the residue window = %v, want lease-held", err)
	}

	// Fact 1: a legacy append is not fenced — the old code path never consults
	// the epoch, so it appends while the new owner's claim is live. That is why
	// the two binaries must never write side by side.
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventAssistantMessage,
		map[string]any{"content": "legacy write"}); err != nil {
		t.Fatalf("legacy append: %v", err)
	}
	events, err := k.LoadEvents(ctx, scope, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 {
		t.Fatalf("legacy append did not reach the log: %d events", len(events))
	}
	_ = lease

	// The mitigation, made concrete: once the residue lapses, the kernel takes
	// the session over cleanly — a new input re-queues the run and the claim
	// returns a fresh epoch that fences everything after it.
	residueDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(residueDeadline) {
		state, err := k.LeaseState(ctx, scope)
		if err != nil {
			t.Fatal(err)
		}
		if !state.Live {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := k.Commit(ctx, CommitRequest{Scope: scope, Kind: KindUserMessage,
		RequestID: "c2", Payload: userPayload("two")}); err != nil {
		t.Fatalf("edge commit after the residue: %v", err)
	}
	newLease, err := k.ClaimScope(ctx, scope, "runtime-new", time.Minute)
	if err != nil {
		t.Fatalf("kernel never took the session over: %v", err)
	}
	if newLease.Token.Epoch <= lease.Token.Epoch {
		t.Fatalf("takeover epoch %d did not advance past %d", newLease.Token.Epoch, lease.Token.Epoch)
	}
	if _, err := k.Commit(ctx, CommitRequest{Scope: scope, Kind: KindAssistantMessage,
		Token: newLease.Token, GenerationID: "cutover-1", Payload: assistantPayload("new")}); err != nil {
		t.Fatalf("kernel commit after takeover: %v", err)
	}
	// And the old owner's token is fenced from here on.
	if _, err := k.Commit(ctx, CommitRequest{Scope: scope, Kind: KindAssistantMessage,
		Token: lease.Token, GenerationID: "cutover-2", Payload: assistantPayload("stale")}); !errors.Is(err, ErrStaleEpoch) && !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("old-owner commit after takeover = %v, want a fencing rejection", err)
	}
}
