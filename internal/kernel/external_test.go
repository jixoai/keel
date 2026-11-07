package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"
)

// externalMarks is a valid mark set for the tests.
func externalMarks(backend, ref string) ExternalMarks {
	return ExternalMarks{
		StateOwner: StateOwnerExternal,
		Backend:    backend,
		Ref:        ref,
		Status:     ExternalStatusActive,
		Metering:   MeteringNone,
	}
}

// bareSession registers a session WITHOUT a run row: the shape an external
// session has. (The harness's newSession enqueues a run, which is the native
// shape and must be refused as external.)
func (h *harness) bareSession() Scope {
	h.t.Helper()
	scope := Scope{TenantID: h.tenant, SessionID: uuid.New()}
	if h.mem != nil {
		h.mem.CreateSession(scope.TenantID, scope.SessionID)
		return scope
	}
	if err := h.st.CreateSession(context.Background(), scope.TenantID, scope.SessionID); err != nil {
		h.t.Fatal(err)
	}
	return scope
}

// bareForeignSession registers a run-less session under another tenant.
func (h *harness) bareForeignSession() Scope {
	h.t.Helper()
	tenant, session := uuid.New(), uuid.New()
	if h.mem != nil {
		h.mem.CreateSession(tenant, session)
		return Scope{TenantID: tenant, SessionID: session}
	}
	ctx := context.Background()
	if err := h.st.CreateTenant(ctx, tenant, "external-foreign"); err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.CreateSession(ctx, tenant, session); err != nil {
		h.t.Fatal(err)
	}
	return Scope{TenantID: tenant, SessionID: session}
}

// TestExternalSessionLifecycle covers the persistence of the external-session
// marks: open, read, status transitions, listing, and the invariant that the
// session never enters the native run/lease flow.
func TestExternalSessionLifecycle(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()
			scope := h.bareSession()

			session, err := h.k.OpenExternalSession(ctx, scope, externalMarks("dsh", "sess-1"))
			if err != nil {
				t.Fatalf("open external session: %v", err)
			}
			if session.StateOwner != StateOwnerExternal || session.Backend != "dsh" ||
				session.Ref != "sess-1" || session.Status != ExternalStatusActive || session.Metering != MeteringNone {
				t.Fatalf("marks = %+v", session.ExternalMarks)
			}
			if !session.CompatMode() {
				t.Fatal("external session does not report compatibility mode")
			}

			// The session must not be claimable: no run row exists.
			if _, err := h.k.ClaimScope(ctx, scope, "runtime-x", h.ttl()); !iso(err, ErrNoRunnable) {
				t.Fatalf("claim of an external session = %v, want no-runnable", err)
			}
			// And the global claim never returns it either.
			for i := 0; i < 5; i++ {
				lease, err := h.k.Claim(ctx, "runtime-y", h.ttl())
				if err != nil {
					if !iso(err, ErrNoRunnable) && !iso(err, ErrLeaseHeld) {
						t.Fatalf("global claim: %v", err)
					}
					break
				}
				if lease.Scope == scope {
					t.Fatal("global claim returned an external session")
				}
				if err := h.k.Release(ctx, lease); err != nil {
					t.Fatal(err)
				}
			}
			// The run pin answers "no run row" for an external session.
			if _, err := h.k.RunPin(ctx, scope); !iso(err, ErrNoRunnable) {
				t.Fatalf("run pin of an external session = %v, want no-runnable", err)
			}

			// Status transitions are validated and queryable.
			if err := h.k.SetExternalStatus(ctx, scope, "not-a-status"); !iso(err, ErrInvalidRequest) {
				t.Fatalf("invalid status = %v, want invalid-request", err)
			}
			if err := h.k.SetExternalStatus(ctx, scope, ExternalStatusClosed); err != nil {
				t.Fatalf("close: %v", err)
			}
			read, err := h.k.ExternalSession(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			if read.Status != ExternalStatusClosed {
				t.Fatalf("status = %s, want closed", read.Status)
			}
			// A closed session is terminal.
			if err := h.k.SetExternalStatus(ctx, scope, ExternalStatusActive); !iso(err, ErrExternalConflict) {
				t.Fatalf("reopen of a closed session = %v, want external-conflict", err)
			}

			// Listing answers the tenant boundary.
			second := h.bareSession()
			if _, err := h.k.OpenExternalSession(ctx, second, externalMarks("dsh", "sess-2")); err != nil {
				t.Fatal(err)
			}
			list, err := h.k.ListExternalSessions(ctx, h.tenant)
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 2 {
				t.Fatalf("listed %d external sessions, want 2", len(list))
			}
			foreign := h.bareForeignSession()
			if _, err := h.k.OpenExternalSession(ctx, foreign, externalMarks("dsh", "sess-3")); err != nil {
				t.Fatal(err)
			}
			list, err = h.k.ListExternalSessions(ctx, h.tenant)
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 2 {
				t.Fatalf("tenant listing leaked a foreign session: %d rows", len(list))
			}
			// A foreign tenant's session answers exactly like a missing one.
			if _, err := h.k.ExternalSession(ctx, Scope{TenantID: h.tenant, SessionID: foreign.SessionID}); !iso(err, ErrNotFound) {
				t.Fatalf("foreign session read = %v, want not-found", err)
			}

			// A native session is reported as such, with empty external marks
			// (PG parity: the external_* columns are NULL and read as "").
			native := h.bareSession()
			read, err = h.k.ExternalSession(ctx, native)
			if err != nil {
				t.Fatal(err)
			}
			if read.StateOwner != StateOwnerKeel || read.CompatMode() {
				t.Fatalf("native session marks = %+v", read.ExternalMarks)
			}
			if read.Status != "" || read.Metering != "" || read.Backend != "" || read.Ref != "" {
				t.Fatalf("native session marks = %+v, want empty external fields", read.ExternalMarks)
			}
			if _, err := h.k.ExternalSession(ctx, Scope{TenantID: h.tenant, SessionID: uuid.New()}); !iso(err, ErrNotFound) {
				t.Fatalf("missing session = %v, want not-found", err)
			}

			// No event was written for any of this.
			if n := h.eventCount(scope); n != 0 {
				t.Fatalf("external session lifecycle wrote %d events", n)
			}
		})
	}
}

// TestExternalSessionRefusesNativeHijack: only a pristine native session may
// be promoted; one with history (events or a run row) is refused.
func TestExternalSessionRefusesNativeHijack(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()

			// Pristine native session (no run row, no events): promotion is allowed.
			pristine := h.bareSession()
			if _, err := h.k.OpenExternalSession(ctx, pristine, externalMarks("dsh", "p1")); err != nil {
				t.Fatalf("promote pristine session: %v", err)
			}

			// Native session with a committed event: refused.
			withEvents := h.bareSession()
			h.mustCommit(CommitRequest{Scope: withEvents, Kind: KindUserMessage,
				RequestID: "r1", Payload: userPayload("hi")})
			if _, err := h.k.OpenExternalSession(ctx, withEvents, externalMarks("dsh", "p2")); !iso(err, ErrExternalConflict) {
				t.Fatalf("promote session with events = %v, want external-conflict", err)
			}

			// Native session with a run row (the harness session shape): refused.
			withRun := h.newSession()
			if _, err := h.k.OpenExternalSession(ctx, withRun, externalMarks("dsh", "p3")); !iso(err, ErrExternalConflict) {
				t.Fatalf("promote session with a run row = %v, want external-conflict", err)
			}

			// A claimed (running) session: refused.
			running := h.newSession()
			_ = h.claimScope(running, "runtime-a")
			if _, err := h.k.OpenExternalSession(ctx, running, externalMarks("dsh", "p4")); !iso(err, ErrExternalConflict) {
				t.Fatalf("promote claimed session = %v, want external-conflict", err)
			}

			// The state owner must be external.
			if _, err := h.k.OpenExternalSession(ctx, h.bareSession(), ExternalMarks{
				StateOwner: StateOwnerKeel, Backend: "dsh"}); !iso(err, ErrInvalidRequest) {
				t.Fatalf("keel state owner = %v, want invalid-request", err)
			}
			if _, err := h.k.OpenExternalSession(ctx, h.bareSession(), ExternalMarks{Backend: ""}); !iso(err, ErrInvalidRequest) {
				t.Fatalf("missing backend = %v, want invalid-request", err)
			}
			if _, err := h.k.OpenExternalSession(ctx, h.bareSession(), ExternalMarks{
				Backend: "dsh", Metering: "guessed"}); !iso(err, ErrInvalidRequest) {
				t.Fatalf("unknown metering = %v, want invalid-request", err)
			}
		})
	}
}

// TestExternalUpsertRefreshesInPlaceOrConflicts pins the refresh rules of an
// existing external session: the marks may only be refreshed idempotently by
// the same backend handle. A different backend or reference is a conflict — the
// previous backend session would otherwise be stranded (never closed, still
// projecting into this log under its old ref) while every later query answers
// with the wrong owner — and closed is terminal, the same rule
// SetExternalStatus enforces.
func TestExternalUpsertRefreshesInPlaceOrConflicts(t *testing.T) {
	for _, im := range implementations(t) {
		im := im
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()
			scope := h.bareSession()

			if _, err := h.k.OpenExternalSession(ctx, scope, externalMarks("dsh", "ref-a")); err != nil {
				t.Fatalf("open: %v", err)
			}
			// Same backend + same ref: an idempotent refresh (the status moves on).
			refreshed, err := h.k.OpenExternalSession(ctx, scope, ExternalMarks{
				Backend: "dsh", Ref: "ref-a", Status: ExternalStatusOpening, Metering: MeteringExternal})
			if err != nil {
				t.Fatalf("refresh with the same ref: %v", err)
			}
			if refreshed.Ref != "ref-a" || refreshed.Status != ExternalStatusOpening || refreshed.Metering != MeteringExternal {
				t.Fatalf("refresh = %+v", refreshed.ExternalMarks)
			}
			// An unspecified ref never erases the known one.
			kept, err := h.k.OpenExternalSession(ctx, scope, ExternalMarks{
				Backend: "dsh", Status: ExternalStatusActive, Metering: MeteringNone})
			if err != nil {
				t.Fatalf("refresh with an unspecified ref: %v", err)
			}
			if kept.Ref != "ref-a" || kept.Status != ExternalStatusActive {
				t.Fatalf("refresh kept = %+v", kept.ExternalMarks)
			}
			// A different ref is a second backend handle: refused.
			if _, err := h.k.OpenExternalSession(ctx, scope, externalMarks("dsh", "ref-b")); !iso(err, ErrExternalConflict) {
				t.Fatalf("second ref = %v, want external-conflict", err)
			}
			// A different backend must not hijack the marks either.
			if _, err := h.k.OpenExternalSession(ctx, scope, externalMarks("other", "ref-a")); !iso(err, ErrExternalConflict) {
				t.Fatalf("second backend = %v, want external-conflict", err)
			}
			// Neither refusal may have touched the marks.
			read, err := h.k.ExternalSession(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			if read.Backend != "dsh" || read.Ref != "ref-a" || read.Status != ExternalStatusActive {
				t.Fatalf("marks after refusals = %+v", read.ExternalMarks)
			}
			// Closed is terminal: nothing reopens it.
			if err := h.k.SetExternalStatus(ctx, scope, ExternalStatusClosed); err != nil {
				t.Fatalf("close: %v", err)
			}
			if _, err := h.k.OpenExternalSession(ctx, scope, externalMarks("dsh", "ref-a")); !iso(err, ErrExternalConflict) {
				t.Fatalf("reopen closed = %v, want external-conflict", err)
			}
			final, err := h.k.ExternalSession(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			if final.Status != ExternalStatusClosed {
				t.Fatalf("closed session status = %s, want closed", final.Status)
			}
		})
	}
}

// TestExternalBackendUnavailable covers the crash scenario: the backend's open
// sessions become a queryable terminal state, closed ones stay closed, other
// backends are untouched, and nothing is fabricated (no event, no run, no
// native recovery).
func TestExternalBackendUnavailable(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()

			one := h.bareSession()
			two := h.bareSession()
			closed := h.bareSession()
			other := h.bareSession()
			for _, scope := range []Scope{one, two} {
				if _, err := h.k.OpenExternalSession(ctx, scope, externalMarks("dsh", "s-"+scope.SessionID.String())); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.k.OpenExternalSession(ctx, closed, externalMarks("dsh", "closed-1")); err != nil {
				t.Fatal(err)
			}
			if err := h.k.SetExternalStatus(ctx, closed, ExternalStatusClosed); err != nil {
				t.Fatal(err)
			}
			if _, err := h.k.OpenExternalSession(ctx, other, externalMarks("other-backend", "o-1")); err != nil {
				t.Fatal(err)
			}

			n, err := h.k.MarkBackendUnavailable(ctx, h.tenant, "dsh")
			if err != nil {
				t.Fatal(err)
			}
			if n != 2 {
				t.Fatalf("marked %d sessions, want 2", n)
			}
			for _, scope := range []Scope{one, two} {
				session, err := h.k.ExternalSession(ctx, scope)
				if err != nil {
					t.Fatal(err)
				}
				if session.Status != ExternalStatusUnavailable {
					t.Fatalf("session status = %s, want %s", session.Status, ExternalStatusUnavailable)
				}
			}
			stillClosed, err := h.k.ExternalSession(ctx, closed)
			if err != nil {
				t.Fatal(err)
			}
			if stillClosed.Status != ExternalStatusClosed {
				t.Fatalf("closed session became %s", stillClosed.Status)
			}
			untouched, err := h.k.ExternalSession(ctx, other)
			if err != nil {
				t.Fatal(err)
			}
			if untouched.Status != ExternalStatusActive {
				t.Fatalf("other backend session = %s, want active", untouched.Status)
			}
			// Idempotent: nothing left to mark.
			n, err = h.k.MarkBackendUnavailable(ctx, h.tenant, "dsh")
			if err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("second mark changed %d rows, want 0", n)
			}

			// Nothing was fabricated: no events, no run rows.
			for _, scope := range []Scope{one, two, closed, other} {
				if got := h.eventCount(scope); got != 0 {
					t.Fatalf("backend crash wrote %d events for %s", got, scope)
				}
			}
			if _, err := h.k.RunPin(ctx, one); !iso(err, ErrNoRunnable) {
				t.Fatalf("crash created a run row: %v", err)
			}
		})
	}
}

// TestExternalMeteringNeverZero pins the metering boundary: the mark is
// explicit (external|none) and a missing usage report is never recorded.
func TestExternalMeteringNeverZero(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()

			// No usage capability declared: none, never zero.
			scope := h.bareSession()
			session, err := h.k.OpenExternalSession(ctx, scope, ExternalMarks{Backend: "dsh"})
			if err != nil {
				t.Fatal(err)
			}
			if session.Metering != MeteringNone {
				t.Fatalf("default metering = %q, want none", session.Metering)
			}
			// A backend that accounts for its own usage says so.
			accounted := h.bareSession()
			session, err = h.k.OpenExternalSession(ctx, accounted, ExternalMarks{
				Backend: "dsh", Metering: MeteringExternal})
			if err != nil {
				t.Fatal(err)
			}
			if session.Metering != MeteringExternal {
				t.Fatalf("metering = %q, want external", session.Metering)
			}
			// The kernel writes no usage record either way, so a report shows
			// "not measurable", not zero.
			if n := h.eventCount(scope); n != 0 {
				t.Fatalf("metering mark wrote %d events", n)
			}
		})
	}
}

// TestRunLoadoutPin covers the pin write: fenced, readable, and retained
// across a settle so the audit can answer "which revision did this run finish
// on".
func TestRunLoadoutPin(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()
			lease := h.claimShort(h.scope, "runtime-a")

			if err := h.k.PinRunLoadout(ctx, lease, "keel-default", 7); err != nil {
				t.Fatalf("pin: %v", err)
			}
			pin, err := h.k.RunPin(ctx, h.scope)
			if err != nil {
				t.Fatal(err)
			}
			if pin.Owner != "runtime-a" || pin.Epoch != lease.Token.Epoch ||
				pin.LoadoutID != "keel-default" || pin.LoadoutRevision != 7 || pin.Status != RunStatusRunning {
				t.Fatalf("pin = %s", pin)
			}

			// A missing token is refused.
			if err := h.k.PinRunLoadout(ctx, Lease{Scope: h.scope}, "x", 1); !iso(err, ErrMissingToken) {
				t.Fatalf("pin without token = %v, want missing-token", err)
			}

			// A stale owner cannot re-pin: the new claim clears the pin first.
			h.expire(lease)
			successor, err := h.k.ClaimScope(ctx, h.scope, "runtime-b", h.ttl())
			if err != nil {
				t.Fatalf("successor claim: %v", err)
			}
			pin, err = h.k.RunPin(ctx, h.scope)
			if err != nil {
				t.Fatal(err)
			}
			if pin.Owner != "runtime-b" || pin.LoadoutRevision != 0 || pin.LoadoutID != "" {
				t.Fatalf("pin after takeover = %s, want cleared for the new owner", pin)
			}
			if err := h.k.PinRunLoadout(ctx, lease, "keel-default", 7); !iso(err, ErrStaleEpoch) {
				t.Fatalf("stale pin = %v, want stale-epoch", err)
			}
			if err := h.k.PinRunLoadout(ctx, successor, "keel-default", 8); err != nil {
				t.Fatalf("successor pin: %v", err)
			}

			// The pin survives the settle.
			if err := h.k.Finish(ctx, FinishRequest{Lease: successor, Cursor: 3}); err != nil {
				t.Fatalf("finish: %v", err)
			}
			pin, err = h.k.RunPin(ctx, h.scope)
			if err != nil {
				t.Fatal(err)
			}
			if pin.Status != RunStatusDone || pin.LoadoutRevision != 8 {
				t.Fatalf("pin after settle = %s, want revision 8 retained with status done", pin)
			}
			// Owner/epoch mirror the runs row, which is cleared on settle (PG
			// parity); the session authority keeps the last token separately.
			if pin.Owner != "" || pin.Epoch != 0 {
				t.Fatalf("pin after settle = %s, want cleared owner/epoch", pin)
			}
		})
	}
}

// TestStoreAndKernelExternalAgree pins the store-layer external surface to the
// kernel seam: both write the same columns, enforce the same rules and share
// the same vocabulary.
func TestStoreAndKernelExternalAgree(t *testing.T) {
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
	if err := st.CreateTenant(ctx, tenant, "external-agree"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	// The store writes the marks...
	stored, err := st.CreateExternalSession(ctx, tenant, session, store.ExternalMarks{
		Backend: "dsh", Ref: "ref-1", Status: store.ExternalStatusActive, Metering: store.MeteringNone})
	if err != nil {
		t.Fatal(err)
	}
	// ... and the kernel reads them, mark for mark.
	viaKernel, err := k.ExternalSession(ctx, Scope{TenantID: tenant, SessionID: session})
	if err != nil {
		t.Fatal(err)
	}
	if viaKernel.StateOwner != stored.StateOwner || viaKernel.Backend != stored.Backend ||
		viaKernel.Ref != stored.Ref || viaKernel.Status != stored.Status || viaKernel.Metering != stored.Metering {
		t.Fatalf("kernel read %+v, store wrote %+v", viaKernel.ExternalMarks, stored.ExternalMarks)
	}
	// The vocabulary is pinned.
	if StateOwnerExternal != store.StateOwnerExternal || StateOwnerKeel != store.StateOwnerKeel {
		t.Fatal("state owner vocabulary drifted between store and kernel")
	}
	if ExternalStatusUnavailable != store.ExternalStatusUnavailable || ExternalStatusClosed != store.ExternalStatusClosed {
		t.Fatal("external status vocabulary drifted between store and kernel")
	}
	if MeteringNone != store.MeteringNone || MeteringExternal != store.MeteringExternal {
		t.Fatal("metering vocabulary drifted between store and kernel")
	}

	// The store enqueues nothing for an external session.
	if err := st.EnqueueRun(ctx, tenant, session); !errors.Is(err, store.ErrExternalSession) {
		t.Fatalf("store enqueue for external session = %v, want ErrExternalSession", err)
	}
	// The kernel marks it unavailable; the store reads it back.
	n, err := k.MarkBackendUnavailable(ctx, tenant, "dsh")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("marked %d sessions, want 1", n)
	}
	read, found, err := st.GetExternalSession(ctx, tenant, session)
	if err != nil || !found {
		t.Fatalf("store read: found=%t err=%v", found, err)
	}
	if read.Status != store.ExternalStatusUnavailable {
		t.Fatalf("store status = %s, want backend-unavailable", read.Status)
	}
	if list, err := st.ListExternalSessions(ctx, tenant); err != nil || len(list) != 1 {
		t.Fatalf("store list = %d rows, err=%v", len(list), err)
	}
	// The store refuses to hijack the same session twice with a native history:
	// a committed event turns it non-pristine, so a second create is refused.
	if _, _, err := st.AppendEventDedup(ctx, tenant, session, store.EventUserMessage,
		map[string]any{"text": "external session note"}, ""); err != nil {
		t.Fatal(err)
	}
	// Refreshing the same handle stays allowed (it only refreshes the marks,
	// the history belongs to the backend); a second ref is refused — the marks
	// are the single fact of who owns the session.
	if _, err := st.CreateExternalSession(ctx, tenant, session, store.ExternalMarks{
		Backend: "dsh", Ref: "ref-1", Status: store.ExternalStatusActive}); err != nil {
		t.Fatalf("refresh external session: %v", err)
	}
	if _, err := st.CreateExternalSession(ctx, tenant, session, store.ExternalMarks{
		Backend: "dsh", Ref: "ref-2", Status: store.ExternalStatusActive}); !errors.Is(err, store.ErrExternalConflict) {
		t.Fatalf("refresh with a second ref = %v, want external-conflict", err)
	}
}

// TestStoreEnqueueRunStillEnqueuesNativeSessions guards the external-session
// guard against breaking the native queue.
func TestStoreEnqueueRunStillEnqueuesNativeSessions(t *testing.T) {
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
	tenant, session := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "native-enqueue"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueRun(ctx, tenant, session); err != nil {
		t.Fatalf("native enqueue: %v", err)
	}
	run, err := st.GetRun(ctx, tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunPending {
		t.Fatalf("native run status = %s, want pending", run.Status)
	}
	// Re-enqueue is idempotent for a pending run.
	if err := st.EnqueueRun(ctx, tenant, session); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	run, err = st.GetRun(ctx, tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunPending {
		t.Fatalf("run status after re-enqueue = %s, want pending", run.Status)
	}
	if n, err := st.CountPendingRuns(ctx); err != nil || n < 1 {
		t.Fatalf("pending runs = %d (err %v), want at least 1", n, err)
	}
}

// TestExternalPromotionRefusesSubmissionHistory pins the PG-only part of the
// pristine check: a submission row implies native bookkeeping even when the log
// is empty (legacy/partial data). MemStore cannot represent submissions; its
// events check covers the normal flow, and the difference is documented there.
func TestExternalPromotionRefusesSubmissionHistory(t *testing.T) {
	if os.Getenv("KEEL_TEST_DSN") == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	h := newPGHarness(t)
	ctx := context.Background()
	scope := h.bareSession()
	if _, err := h.st.Pool().Exec(ctx,
		`INSERT INTO submissions (tenant_id, request_id, session_id, submission_id)
		 VALUES ($1, 'legacy-1', $2, $3)`,
		scope.TenantID, scope.SessionID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.k.OpenExternalSession(ctx, scope, externalMarks("dsh", "legacy-sub")); !iso(err, ErrExternalConflict) {
		t.Fatalf("promotion with a submission-only history = %v, want external-conflict", err)
	}
}

// TestExternalSessionNeverClaimedEvenWithDirtyRun is the M5 fail-closed layer:
// even if a pre-fix database already holds a run row for an external session,
// neither the scoped nor the global claim may return it.
func TestExternalSessionNeverClaimedEvenWithDirtyRun(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()
			scope := h.bareSession()
			if _, err := h.k.OpenExternalSession(ctx, scope, externalMarks("dsh", "dirty-run")); err != nil {
				t.Fatal(err)
			}
			if h.mem != nil {
				h.mem.mu.Lock()
				h.mem.sessions[scope].run = &memRun{status: RunStatusPending}
				h.mem.mu.Unlock()
			} else {
				if _, err := h.st.Pool().Exec(ctx,
					`INSERT INTO runs (tenant_id, session_id, status) VALUES ($1, $2, 'pending')`,
					scope.TenantID, scope.SessionID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.k.ClaimScope(ctx, scope, "runtime-x", h.ttl()); !iso(err, ErrNoRunnable) {
				t.Fatalf("scoped claim of a dirty external run = %v, want no-runnable", err)
			}
			for i := 0; i < 5; i++ {
				lease, err := h.k.Claim(ctx, "runtime-y", h.ttl())
				if err != nil {
					if !iso(err, ErrNoRunnable) && !iso(err, ErrLeaseHeld) {
						t.Fatalf("global claim: %v", err)
					}
					break
				}
				if lease.Scope == scope {
					t.Fatal("global claim returned an external session with a dirty run row")
				}
				if err := h.k.Release(ctx, lease); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// TestExternalEventCommitLane covers the external-scoped commit class: the
// compatibility log is appendable without a lease, only into an open external
// session, idempotently by the gateway's per-event key, and through the same
// SSE-visible event log as every native fact.
func TestExternalEventCommitLane(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()
			scope := h.bareSession()
			if _, err := h.k.OpenExternalSession(ctx, scope, externalMarks("dsh", "sess-ext-1")); err != nil {
				t.Fatal(err)
			}
			payload := json.RawMessage(`{"backend":"dsh","externalRef":"sess-ext-1","stateOwner":"external","seq":1,"type":"assistant.message","data":{"text":"hello"}}`)
			receipt, err := h.k.Commit(ctx, CommitRequest{
				Scope: scope, Kind: KindExternalEvent, RequestID: "dsh:sess-ext-1:1", Payload: payload,
			})
			if err != nil {
				t.Fatalf("commit external.event: %v", err)
			}
			if receipt.Seq != 1 || receipt.Kind != KindExternalEvent {
				t.Fatalf("receipt = %+v", receipt)
			}
			events, err := h.k.LoadEvents(ctx, scope, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || events[0].Kind != KindExternalEvent {
				t.Fatalf("log = %+v, want one external.event", events)
			}
			// The gateway retries a replayed cursor under the same per-event
			// key: the fact is reused, not appended twice.
			again, err := h.k.Commit(ctx, CommitRequest{
				Scope: scope, Kind: KindExternalEvent, RequestID: "dsh:sess-ext-1:1", Payload: payload,
			})
			if err != nil || !again.Deduped || again.Seq != receipt.Seq {
				t.Fatalf("replay = %+v, %v; want deduped at seq %d", again, err, receipt.Seq)
			}
			// Same key, different content is a bug, not a retry.
			changed := json.RawMessage(`{"backend":"dsh","externalRef":"sess-ext-1","seq":1,"type":"assistant.message","data":{"text":"other"}}`)
			if _, err := h.k.Commit(ctx, CommitRequest{
				Scope: scope, Kind: KindExternalEvent, RequestID: "dsh:sess-ext-1:1", Payload: changed,
			}); !iso(err, ErrKeyConflict) {
				t.Fatalf("same key/different payload = %v, want key-conflict", err)
			}
			// The external lane never accepts an owner token.
			if _, err := h.k.Commit(ctx, CommitRequest{
				Scope: scope, Kind: KindExternalEvent, RequestID: "dsh:sess-ext-1:2",
				Token: OwnerToken{Owner: "runtime", Epoch: 1}, Payload: payload,
			}); !iso(err, ErrInvalidRequest) {
				t.Fatalf("external.event with token = %v, want invalid-request", err)
			}
			// The provenance marks are mandatory: without backend/externalRef
			// the record could pass for a native message.
			if _, err := h.k.Commit(ctx, CommitRequest{
				Scope: scope, Kind: KindExternalEvent, RequestID: "dsh:sess-ext-1:3",
				Payload: json.RawMessage(`{"seq":2}`),
			}); !iso(err, ErrInvalidRequest) {
				t.Fatalf("payload without marks = %v, want invalid-request", err)
			}
			// A native session refuses the external lane.
			native := h.bareSession()
			if _, err := h.k.Commit(ctx, CommitRequest{
				Scope: native, Kind: KindExternalEvent, RequestID: "dsh:native:1", Payload: payload,
			}); !iso(err, ErrNotExternalSession) {
				t.Fatalf("external.event on a native session = %v, want not-external-session", err)
			}
			// A closed external session refuses it too: the session is terminal.
			if err := h.k.SetExternalStatus(ctx, scope, ExternalStatusClosed); err != nil {
				t.Fatal(err)
			}
			if _, err := h.k.Commit(ctx, CommitRequest{
				Scope: scope, Kind: KindExternalEvent, RequestID: "dsh:sess-ext-1:9", Payload: payload,
			}); !iso(err, ErrNotExternalSession) {
				t.Fatalf("external.event on a closed session = %v, want not-external-session", err)
			}
		})
	}
}
