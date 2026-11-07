// NativeEdge and admission regression tests for the review rounds.

package kernel

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestNativeEdgeRejectedOnExternalSession is N3: a native edge commit is
// refused inside the session transaction, before the append, while the kernel
// direct lane (the external gateway's "record but never wake" contract) still
// records the message.
func TestNativeEdgeRejectedOnExternalSession(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()
			scope := h.bareSession()
			if _, err := h.k.OpenExternalSession(ctx, scope, externalMarks("dsh", "native-edge")); err != nil {
				t.Fatal(err)
			}
			_, err := h.k.Commit(ctx, CommitRequest{
				Scope: scope, Kind: KindUserMessage, RequestID: "native-1",
				Payload: userPayload("hi"), NativeEdge: true,
			})
			if !iso(err, ErrExternalSession) {
				t.Fatalf("native edge commit on external session = %v, want external-session", err)
			}
			if n := h.eventCount(scope); n != 0 {
				t.Fatalf("refused native edge commit landed %d events", n)
			}

			// Kernel-direct (external gateway) commits do not set NativeEdge: the
			// message is recorded, but no run is woken.
			receipt, err := h.k.Commit(ctx, CommitRequest{
				Scope: scope, Kind: KindUserMessage, RequestID: "gateway-1", Payload: userPayload("mirrored"),
			})
			if err != nil || receipt.Seq != 1 || receipt.Deduped {
				t.Fatalf("gateway direct commit = %+v, %v", receipt, err)
			}
			if n := h.eventCount(scope); n != 1 {
				t.Fatalf("gateway direct commit events = %d, want 1", n)
			}
			if _, err := h.k.RunPin(ctx, scope); !iso(err, ErrNoRunnable) {
				t.Fatalf("external session got a run row: %v", err)
			}

			// Positive control: a native session accepts the same NativeEdge
			// write, so the refusal above is about the session's owner, not the
			// marker.
			native := h.bareSession()
			if _, err := h.k.Commit(ctx, CommitRequest{
				Scope: native, Kind: KindUserMessage, RequestID: "native-ok-1",
				Payload: userPayload("hi"), NativeEdge: true,
			}); err != nil {
				t.Fatalf("native edge write to a native session: %v", err)
			}
			// The marker is meaningless outside the edge class.
			if _, err := h.k.Commit(ctx, CommitRequest{
				Scope: native, Kind: KindExternalEvent, RequestID: "bad-flag",
				Payload: json.RawMessage(`{"backend":"dsh","externalRef":"r"}`), NativeEdge: true,
			}); !iso(err, ErrInvalidRequest) {
				t.Fatalf("NativeEdge on external.event = %v, want invalid-request", err)
			}
		})
	}
}

// TestNativeEdgeMissingSessionIsNotFound pins the error precedence: a missing
// session answers ErrNotFound even with NativeEdge set; the class check must
// not mask it (and must not consume admission).
func TestNativeEdgeMissingSessionIsNotFound(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			missing := Scope{TenantID: h.tenant, SessionID: uuid.New()}
			_, err := h.k.Commit(context.Background(), CommitRequest{
				Scope: missing, Kind: KindUserMessage, RequestID: "missing-1",
				Payload: userPayload("hi"), NativeEdge: true,
			})
			if !iso(err, ErrNotFound) {
				t.Fatalf("native edge commit on missing session = %v, want not-found", err)
			}
		})
	}
}

// TestNativeEdgeCommitSerializesWithPromotion is the deterministic N3
// interleaving: the handler's fast pre-check reads "keel", then a promotion
// commits, then the commit starts. It must block on the session row and be
// refused after the promotion commits, leaving zero events.
func TestNativeEdgeCommitSerializesWithPromotion(t *testing.T) {
	if os.Getenv("KEEL_TEST_DSN") == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	h := newPGHarness(t)
	ctx := context.Background()
	scope := h.bareSession()

	// The fast pre-check in the handler sees the session as native.
	owner, exists, err := h.st.SessionOwner(ctx, scope.TenantID, scope.SessionID)
	if err != nil || !exists || owner != StateOwnerKeel {
		t.Fatalf("pre-check = owner %q exists %v err %v, want keel", owner, exists, err)
	}

	// The promotion wins the race and holds the session row lock until commit.
	tx, err := h.st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`UPDATE sessions SET state_owner = 'external', external_backend = 'dsh',
		                     external_status = 'active', metering = 'none',
		                     external_updated_at = now(), updated_at = now()
		 WHERE tenant_id = $1 AND session_id = $2`,
		scope.TenantID, scope.SessionID); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := h.k.Commit(ctx, CommitRequest{
			Scope: scope, Kind: KindUserMessage, RequestID: "racing-native-1",
			Payload: userPayload("hi"), NativeEdge: true,
		})
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("commit returned before the promotion committed (not atomic): %v", err)
	default:
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !iso(err, ErrExternalSession) {
			t.Fatalf("commit after promotion = %v, want external-session", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("commit did not resume after the promotion committed")
	}
	if n := h.eventCount(scope); n != 0 {
		t.Fatalf("racing native edge commit landed %d events, want 0", n)
	}
}

// TestClaimScopeFailureDoesNotConsumeAdmission is N4: failed claims (held
// lease, missing session) must not touch the tenant's run budget, and a
// rejection must not mask ErrNotFound.
func TestClaimScopeFailureDoesNotConsumeAdmission(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			limits := &testLimits{maxRun: 4}
			h.lims = limits
			h.k = New(h.log, Options{Audit: h.recordViolation, Limits: limits})
			ctx := context.Background()

			first := h.claimScope(h.scope, "runtime-a")
			if got := limits.Active(); got != 1 {
				t.Fatalf("active after one claim = %d, want 1", got)
			}
			for i := 0; i < 3; i++ {
				if _, err := h.k.ClaimScope(ctx, h.scope, "runtime-b", h.ttl()); !iso(err, ErrLeaseHeld) {
					t.Fatalf("contended claim = %v, want lease-held", err)
				}
			}
			if got := limits.Active(); got != 1 {
				t.Fatalf("failed claims leaked admission slots: active = %d, want 1", got)
			}

			// A free session still fits the budget (a leak would have exhausted it).
			free := h.newSession()
			if _, err := h.k.ClaimScope(ctx, free, "runtime-c", h.ttl()); err != nil {
				t.Fatalf("free session claim = %v, want success", err)
			}
			if got := limits.Active(); got != 2 {
				t.Fatalf("active after second claim = %d, want 2", got)
			}

			// A missing session is NotFound, never Quota, and consumes nothing.
			missing := Scope{TenantID: h.tenant, SessionID: uuid.New()}
			if _, err := h.k.ClaimScope(ctx, missing, "runtime-d", h.ttl()); !iso(err, ErrNotFound) {
				t.Fatalf("missing session = %v, want not-found", err)
			}
			if got := limits.Active(); got != 2 {
				t.Fatalf("missing-session claim changed active = %d, want 2", got)
			}

			// Release gives the slot back.
			if err := h.k.Release(ctx, first); err != nil {
				t.Fatal(err)
			}
			if got := limits.Active(); got != 1 {
				t.Fatalf("active after release = %d, want 1", got)
			}
		})
	}
}
