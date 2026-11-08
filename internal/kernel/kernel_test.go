package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jixoai/keel/internal/store"
)

// The contract-kernel test suite runs against both implementations:
// MemStore always, PGStore when KEEL_TEST_DSN is set (the repository's gated
// integration pattern — no DSN means skip, not fail).

type impl struct {
	name  string
	build func(t *testing.T) *harness
}

func implementations(t *testing.T) []impl {
	t.Helper()
	out := []impl{{name: "mem", build: newMemHarness}}
	if os.Getenv("KEEL_TEST_DSN") == "" {
		t.Log("KEEL_TEST_DSN not set: PostgreSQL implementation skipped")
	} else {
		out = append(out, impl{name: "pg", build: newPGHarness})
	}
	return out
}

// testClock is the fake clock the mem harness drives.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock { return &testClock{now: time.Now()} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advanceTo(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.now) {
		c.now = t
	}
}

// testLimits is a mutable admission seam for the quota tests. maxRun (when
// positive) makes it a bounded pool like the runtime's RunConcurrency, so tests
// can assert that failed claims do not consume slots.
type testLimits struct {
	mu          sync.Mutex
	denyRun     bool
	denyEvent   bool
	maxRun      int
	seenRun     int
	seenEvent   int
	releasedRun int
	activeRun   int
}

func (l *testLimits) AdmitRun(ctx context.Context, scope Scope) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seenRun++
	if l.denyRun || (l.maxRun > 0 && l.activeRun >= l.maxRun) {
		return errors.New("test: run quota exceeded")
	}
	l.activeRun++
	return nil
}

func (l *testLimits) ReleaseRun(ctx context.Context, scope Scope) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.releasedRun++
	if l.activeRun > 0 {
		l.activeRun--
	}
}

// Active is the number of run slots currently reserved (bounded by maxRun).
func (l *testLimits) Active() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.activeRun
}

func (l *testLimits) AdmitEvent(ctx context.Context, scope Scope, kind EventKind) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seenEvent++
	if l.denyEvent {
		return errors.New("test: event quota exceeded")
	}
	return nil
}

// harness is one test subject: a session with a run row, plus a kernel wired to
// audit into a slice.
type harness struct {
	t     *testing.T
	k     *Kernel
	log   LogStore
	mem   *MemStore
	st    *store.Store // pg only
	pg    *PGStore     // pg only
	clock *testClock
	lims  *testLimits

	tenant  uuid.UUID
	session uuid.UUID
	scope   Scope

	auditMu sync.Mutex
	audit   []Violation
}

func (h *harness) recordViolation(v Violation) {
	h.auditMu.Lock()
	defer h.auditMu.Unlock()
	h.audit = append(h.audit, v)
}

func (h *harness) violations() []Violation {
	h.auditMu.Lock()
	defer h.auditMu.Unlock()
	return append([]Violation(nil), h.audit...)
}

func (h *harness) countViolations(reason Reason) int {
	n := 0
	for _, v := range h.violations() {
		if v.Reason == reason {
			n++
		}
	}
	return n
}

func newMemHarness(t *testing.T) *harness {
	t.Helper()
	mem := NewMemStore()
	clock := newTestClock()
	mem.SetClock(clock.Now)
	h := &harness{t: t, log: mem, mem: mem, clock: clock}
	h.tenant = uuid.New()
	h.session = uuid.New()
	h.scope = Scope{TenantID: h.tenant, SessionID: h.session}
	mem.CreateSession(h.tenant, h.session)
	mem.EnqueueRun(h.tenant, h.session)
	h.k = New(mem, Options{Audit: h.recordViolation})
	return h
}

func newPGHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: os.Getenv("KEEL_TEST_DSN")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	pg, err := NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatalf("open kernel pg store: %v", err)
	}
	h := &harness{t: t, log: pg, st: st, pg: pg}
	h.tenant = uuid.New()
	h.session = uuid.New()
	h.scope = Scope{TenantID: h.tenant, SessionID: h.session}
	if err := st.CreateTenant(ctx, h.tenant, "kernel-test"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, h.tenant, h.session); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueRun(ctx, h.tenant, h.session); err != nil {
		t.Fatal(err)
	}
	h.k = New(pg, Options{Audit: h.recordViolation})
	return h
}

// newSession creates another session (with a run row) in the same tenant.
func (h *harness) newSession() Scope {
	h.t.Helper()
	scope := Scope{TenantID: h.tenant, SessionID: uuid.New()}
	if h.mem != nil {
		h.mem.CreateSession(scope.TenantID, scope.SessionID)
		h.mem.EnqueueRun(scope.TenantID, scope.SessionID)
		return scope
	}
	ctx := context.Background()
	if err := h.st.CreateSession(ctx, scope.TenantID, scope.SessionID); err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.EnqueueRun(ctx, scope.TenantID, scope.SessionID); err != nil {
		h.t.Fatal(err)
	}
	return scope
}

// foreignSession creates a session under another tenant.
func (h *harness) foreignSession() Scope {
	h.t.Helper()
	scope := Scope{TenantID: uuid.New(), SessionID: uuid.New()}
	if h.mem != nil {
		h.mem.CreateSession(scope.TenantID, scope.SessionID)
		h.mem.EnqueueRun(scope.TenantID, scope.SessionID)
		return scope
	}
	ctx := context.Background()
	if err := h.st.CreateTenant(ctx, scope.TenantID, "kernel-test-foreign"); err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.CreateSession(ctx, scope.TenantID, scope.SessionID); err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.EnqueueRun(ctx, scope.TenantID, scope.SessionID); err != nil {
		h.t.Fatal(err)
	}
	return scope
}

// enqueue marks the session as having work. Enqueueing is the edge's job in
// production (the kernel's claim path only reads the queue); tests do it
// explicitly so a settled run can be picked up again.
func (h *harness) enqueue(scope Scope) {
	h.t.Helper()
	if scope == (Scope{}) {
		scope = h.scope
	}
	if h.mem != nil {
		h.mem.EnqueueRun(scope.TenantID, scope.SessionID)
		return
	}
	if err := h.st.EnqueueRun(context.Background(), scope.TenantID, scope.SessionID); err != nil {
		h.t.Fatalf("enqueue run: %v", err)
	}
}

// ttl is the lease lifetime for tests that only need a held lease. The mem
// clock makes expiry explicit, so a long ttl is fine there; the PG tests run
// on the real clock, so the ttl must outlast a race-instrumented test body —
// a short shared ttl here made tests fail with a genuine lease-expired under
// -race. Tests that want the lease to lapse claim with shortTTL instead.
func (h *harness) ttl() time.Duration {
	return 60 * time.Second
}

// shortTTL is the lifetime for tests that let the lease lapse for real. It
// only needs to cover the ops executed before expire() is called, but under
// -race those are slow, so keep headroom over the raw wait time.
func (h *harness) shortTTL() time.Duration {
	if h.mem != nil {
		return 30 * time.Minute
	}
	return 1200 * time.Millisecond
}

func (h *harness) claimScope(scope Scope, owner string) Lease {
	return h.claimScopeTTL(scope, owner, h.ttl())
}

// claimShort claims a lease that the test will let expire.
func (h *harness) claimShort(scope Scope, owner string) Lease {
	return h.claimScopeTTL(scope, owner, h.shortTTL())
}

func (h *harness) claimScopeTTL(scope Scope, owner string, ttl time.Duration) Lease {
	h.t.Helper()
	lease, err := h.k.ClaimScope(context.Background(), scope, owner, ttl)
	if err != nil {
		h.t.Fatalf("claim scope %s: %v", owner, err)
	}
	return lease
}

// expire forces the lease past its deadline.
func (h *harness) expire(lease Lease) {
	h.t.Helper()
	if h.clock != nil {
		h.clock.advanceTo(lease.Until.Add(time.Millisecond))
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		state, err := h.k.LeaseState(context.Background(), lease.Scope)
		if err != nil {
			h.t.Fatalf("lease state: %v", err)
		}
		if !state.Live {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatal("lease never expired")
}

func (h *harness) mustCommit(req CommitRequest) Receipt {
	h.t.Helper()
	if req.Scope == (Scope{}) {
		req.Scope = h.scope
	}
	receipt, err := h.k.Commit(context.Background(), req)
	if err != nil {
		h.t.Fatalf("commit %s: %v", req.Kind, err)
	}
	return receipt
}

func (h *harness) commitErr(req CommitRequest) error {
	h.t.Helper()
	if req.Scope == (Scope{}) {
		req.Scope = h.scope
	}
	_, err := h.k.Commit(context.Background(), req)
	return err
}

func (h *harness) events(scope Scope) []Event {
	h.t.Helper()
	if scope == (Scope{}) {
		scope = h.scope
	}
	var out []Event
	var after int64
	for {
		page, err := h.k.LoadEvents(context.Background(), scope, after, maxEventPage)
		if err != nil {
			h.t.Fatalf("load events: %v", err)
		}
		out = append(out, page...)
		if len(page) < maxEventPage {
			return out
		}
		after = page[len(page)-1].Seq
	}
}

func (h *harness) eventCount(scope Scope) int { return len(h.events(scope)) }

// runStatus reads the durable run row for the harness session.
func (h *harness) runStatus(scope Scope) (status string, cursor int64, attempts int) {
	h.t.Helper()
	if scope == (Scope{}) {
		scope = h.scope
	}
	if h.mem != nil {
		h.mem.mu.Lock()
		defer h.mem.mu.Unlock()
		s := h.mem.sessions[scope]
		if s == nil || s.run == nil {
			return "", 0, 0
		}
		return s.run.status, s.run.cursor, s.run.attempts
	}
	run, err := h.st.GetRun(context.Background(), scope.TenantID, scope.SessionID)
	if err != nil {
		h.t.Fatalf("get run: %v", err)
	}
	return run.Status, run.CursorSeq, run.Attempts
}

// --- payload builders ----------------------------------------------------

func userPayload(text string) json.RawMessage {
	b, err := json.Marshal(map[string]any{"text": text})
	if err != nil {
		panic(err)
	}
	return b
}

func assistantPayload(text string, calls ...map[string]any) json.RawMessage {
	content := []map[string]any{}
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	content = append(content, calls...)
	b, err := json.Marshal(map[string]any{"role": "assistant", "content": content})
	if err != nil {
		panic(err)
	}
	return b
}

func toolCall(id, name string) map[string]any {
	return map[string]any{"type": "toolCall", "id": id, "name": name, "arguments": map[string]any{}}
}

func toolResultPayload(callID, name string) json.RawMessage {
	b, err := json.Marshal(map[string]any{
		"role":       "toolResult",
		"toolCallId": callID,
		"toolName":   name,
		"content":    []map[string]any{{"type": "text", "text": "ok"}},
		"isError":    false,
	})
	if err != nil {
		panic(err)
	}
	return b
}

func iso(err, target error) bool { return errors.Is(err, target) }

// --- the suite -----------------------------------------------------------

func TestKernelContract(t *testing.T) {
	for _, im := range implementations(t) {
		im := im
		t.Run(im.name, func(t *testing.T) {
			t.Run("CommitUserMessageDedupesRequestID", func(t *testing.T) {
				h := im.build(t)
				first := h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "req-1", Payload: userPayload("hi")})
				if first.Deduped || first.Seq != 1 || first.Kind != KindUserMessage {
					t.Fatalf("first = %+v", first)
				}
				again := h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "req-1", Payload: userPayload("hi")})
				if !again.Deduped || again.Seq != first.Seq {
					t.Fatalf("replay = %+v, want seq %d deduped", again, first.Seq)
				}
				if n := h.eventCount(h.scope); n != 1 {
					t.Fatalf("event count = %d, want 1 (no second user.message)", n)
				}
				// A different request id appends normally and keeps seq monotonic.
				second := h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "req-2", Payload: userPayload("more")})
				if second.Seq != 2 {
					t.Fatalf("second seq = %d, want 2", second.Seq)
				}
				// Lookup answers without writing.
				rec, found, err := h.k.Lookup(context.Background(), h.scope, KeyRef{Kind: KeyRequest, Value: "req-1"})
				if err != nil || !found || rec.Seq != first.Seq || !rec.Deduped {
					t.Fatalf("lookup = %+v found=%v err=%v", rec, found, err)
				}
				if _, found, _ := h.k.Lookup(context.Background(), h.scope, KeyRef{Kind: KeyRequest, Value: "nope"}); found {
					t.Fatal("lookup of an unknown key reported found")
				}
			})

			t.Run("UserMessagePayloadIsValidated", func(t *testing.T) {
				h := im.build(t)
				// The reviewer's bricking repro: a wrong-typed text field was
				// accepted and made every transcript rebuild fail forever.
				for _, bad := range []json.RawMessage{
					json.RawMessage(`{"text":123}`),
					json.RawMessage(`{"text":null}`),
					json.RawMessage(`{"requestId":5}`),
				} {
					err := h.commitErr(CommitRequest{Kind: KindUserMessage, RequestID: "req-bad", Payload: bad})
					if !iso(err, ErrInvalidRequest) {
						t.Fatalf("payload %s = %v, want invalid-request", bad, err)
					}
				}
				if n := h.eventCount(h.scope); n != 0 {
					t.Fatalf("rejected payloads reached the log: %d events", n)
				}
				// The valid shapes still land: text is the payload, metadata is
				// free-form, and an empty object is not a defect.
				h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "req-1",
					Payload: json.RawMessage(`{"text":"hi","requestId":"req-1","extra":{"a":1}}`)})
				h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "req-2",
					Payload: json.RawMessage(`{}`)})
				if n := h.eventCount(h.scope); n != 2 {
					t.Fatalf("event count = %d, want 2", n)
				}
			})

			t.Run("IdempotentReplayWithDifferentContentConflicts", func(t *testing.T) {
				h := im.build(t)
				first := h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "req-1",
					Payload: json.RawMessage(`{"text":"first","requestId":"req-1"}`)})
				// A retry with the same content but different formatting is the
				// same request.
				same := h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "req-1",
					Payload: json.RawMessage(`{ "requestId" : "req-1" , "text" : "first" }`)})
				if !same.Deduped || same.Seq != first.Seq {
					t.Fatalf("formatted replay = %+v, want seq %d deduped", same, first.Seq)
				}
				// The same key with different content must not silently return
				// the old receipt: the new content would be dropped.
				err := h.commitErr(CommitRequest{Kind: KindUserMessage, RequestID: "req-1",
					Payload: json.RawMessage(`{"text":"second","requestId":"req-1"}`)})
				if !iso(err, ErrKeyConflict) {
					t.Fatalf("conflicting replay = %v, want key-conflict", err)
				}
				if n := h.eventCount(h.scope); n != 1 {
					t.Fatalf("conflicting replay wrote (%d events)", n)
				}
			})

			t.Run("CommitAssistantNeedsTokenAndDedupesGeneration", func(t *testing.T) {
				h := im.build(t)
				err := h.commitErr(CommitRequest{Kind: KindAssistantMessage, GenerationID: "g1", Payload: assistantPayload("hello")})
				if !iso(err, ErrMissingToken) {
					t.Fatalf("no-token commit error = %v", err)
				}
				lease := h.claimScope(h.scope, "runtime-a")
				first := h.mustCommit(CommitRequest{Kind: KindAssistantMessage, Token: lease.Token, GenerationID: "g1", Payload: assistantPayload("hello")})
				again := h.mustCommit(CommitRequest{Kind: KindAssistantMessage, Token: lease.Token, GenerationID: "g1", Payload: assistantPayload("hello")})
				if !again.Deduped || again.Seq != first.Seq {
					t.Fatalf("generation replay = %+v, want seq %d", again, first.Seq)
				}
				if n := h.eventCount(h.scope); n != 1 {
					t.Fatalf("event count = %d, want 1 (assistant retry reuses the fact)", n)
				}
				// The worker asks before calling the model.
				rec, found, err := h.k.Lookup(context.Background(), h.scope, KeyRef{Kind: KeyGeneration, Value: "g1"})
				if err != nil || !found || rec.Seq != first.Seq {
					t.Fatalf("generation lookup = %+v found=%v err=%v", rec, found, err)
				}
				// A new generation is a new fact.
				next := h.mustCommit(CommitRequest{Kind: KindAssistantMessage, Token: lease.Token, GenerationID: "g2", Payload: assistantPayload("again")})
				if next.Seq <= first.Seq {
					t.Fatalf("second generation seq = %d, want > %d", next.Seq, first.Seq)
				}
			})

			t.Run("ToolResultsPairExactlyOnce", func(t *testing.T) {
				h := im.build(t)
				lease := h.claimScope(h.scope, "runtime-a")
				h.mustCommit(CommitRequest{Kind: KindAssistantMessage, Token: lease.Token, GenerationID: "g1",
					Payload: assistantPayload("thinking", toolCall("call-1", "clock"), toolCall("call-2", "clock"))})

				unresolved, err := h.k.UnresolvedToolCalls(context.Background(), h.scope)
				if err != nil {
					t.Fatal(err)
				}
				if len(unresolved) != 2 || unresolved[0].ToolCallID != "call-1" || unresolved[0].ToolName != "clock" {
					t.Fatalf("unresolved = %+v", unresolved)
				}

				res1 := h.mustCommit(CommitRequest{Kind: KindToolResult, Token: lease.Token, ToolCallID: "call-1",
					Payload: toolResultPayload("call-1", "clock")})
				dup := h.mustCommit(CommitRequest{Kind: KindToolResult, Token: lease.Token, ToolCallID: "call-1",
					Payload: toolResultPayload("call-1", "clock")})
				if !dup.Deduped || dup.Seq != res1.Seq {
					t.Fatalf("duplicate result = %+v, want seq %d deduped", dup, res1.Seq)
				}
				if n := h.eventCount(h.scope); n != 2 {
					t.Fatalf("event count = %d, want 2 (call + one result)", n)
				}
				unresolved, err = h.k.UnresolvedToolCalls(context.Background(), h.scope)
				if err != nil {
					t.Fatal(err)
				}
				if len(unresolved) != 1 || unresolved[0].ToolCallID != "call-2" {
					t.Fatalf("unresolved after one result = %+v", unresolved)
				}

				// Orphan: no committed call carries this id.
				before := h.eventCount(h.scope)
				err = h.commitErr(CommitRequest{Kind: KindToolResult, Token: lease.Token, ToolCallID: "call-99",
					Payload: toolResultPayload("call-99", "clock")})
				if !iso(err, ErrOrphanToolResult) {
					t.Fatalf("orphan error = %v", err)
				}
				if h.eventCount(h.scope) != before {
					t.Fatal("orphan result changed the log")
				}
				// Wrong call: the id exists but the reported tool name differs.
				err = h.commitErr(CommitRequest{Kind: KindToolResult, Token: lease.Token, ToolCallID: "call-2",
					Payload: toolResultPayload("call-2", "echo")})
				if !iso(err, ErrToolResultMismatch) {
					t.Fatalf("wrong-call error = %v", err)
				}
				// Payload id must agree with the request key.
				err = h.commitErr(CommitRequest{Kind: KindToolResult, Token: lease.Token, ToolCallID: "call-2",
					Payload: toolResultPayload("call-1", "clock")})
				if !iso(err, ErrInvalidRequest) {
					t.Fatalf("payload/key mismatch error = %v", err)
				}
				if h.eventCount(h.scope) != before {
					t.Fatal("rejected results changed the log")
				}

				res2 := h.mustCommit(CommitRequest{Kind: KindToolResult, Token: lease.Token, ToolCallID: "call-2",
					Payload: toolResultPayload("call-2", "clock")})
				if res2.Seq != res1.Seq+1 {
					t.Fatalf("second result seq = %d, want %d (no gap after rejections)", res2.Seq, res1.Seq+1)
				}
				unresolved, err = h.k.UnresolvedToolCalls(context.Background(), h.scope)
				if err != nil {
					t.Fatal(err)
				}
				if len(unresolved) != 0 {
					t.Fatalf("unresolved after all results = %+v", unresolved)
				}
			})

			t.Run("OwnerScopedBookkeepingKinds", func(t *testing.T) {
				h := im.build(t)
				lease := h.claimScope(h.scope, "runtime-a")
				started := h.mustCommit(CommitRequest{Kind: KindRunStarted, Token: lease.Token,
					Payload: json.RawMessage(`{"attempt":1,"timestamp":1}`)})
				if started.Deduped || started.Kind != KindRunStarted {
					t.Fatalf("run.started = %+v", started)
				}
				// Bookkeeping kinds carry no idempotency key: a second run.started is
				// a second fact (nothing to dedupe on).
				second := h.mustCommit(CommitRequest{Kind: KindRunStarted, Token: lease.Token,
					Payload: json.RawMessage(`{"attempt":2,"timestamp":2}`)})
				if second.Seq != started.Seq+1 {
					t.Fatalf("second run.started seq = %d, want %d", second.Seq, started.Seq+1)
				}
				// Compaction is owner-scoped: it needs the fence and lands as a fact.
				comp := h.mustCommit(CommitRequest{Kind: KindCompaction, Token: lease.Token,
					Payload: json.RawMessage(`{"summary":"so far","firstKept":1,"summarized":2,"timestamp":3}`)})
				if comp.Seq != second.Seq+1 {
					t.Fatalf("compaction seq = %d, want %d", comp.Seq, second.Seq+1)
				}
				// Without the fence the same proposal is refused and nothing lands.
				if err := h.commitErr(CommitRequest{Kind: KindCompaction,
					Payload: json.RawMessage(`{"summary":"secret"}`)}); !iso(err, ErrMissingToken) {
					t.Fatalf("unfenced compaction = %v", err)
				}
				if err := h.commitErr(CommitRequest{Kind: KindRunFailed, Token: OwnerToken{Owner: "runtime-a", Epoch: lease.Token.Epoch},
					Payload: json.RawMessage(`{"reason":"aborted"}`)}); err != nil {
					t.Fatalf("run.failed with the current fence: %v", err)
				}
			})

			t.Run("TwoOwnersAndStaleEpoch", func(t *testing.T) {
				h := im.build(t)
				leaseA := h.claimShort(h.scope, "runtime-a")

				// B cannot claim while A's lease is live.
				if _, err := h.k.ClaimScope(context.Background(), h.scope, "runtime-b", h.ttl()); !iso(err, ErrLeaseHeld) {
					t.Fatalf("second claim error = %v, want lease-held", err)
				}

				// After expiry, B takes over with a higher epoch.
				h.expire(leaseA)
				leaseB := h.claimScope(h.scope, "runtime-b")
				if leaseB.Token.Epoch <= leaseA.Token.Epoch {
					t.Fatalf("epoch did not advance: %d -> %d", leaseA.Token.Epoch, leaseB.Token.Epoch)
				}

				// A is now a zombie: every write and lease verb is rejected.
				authErr := h.commitErr(CommitRequest{Kind: KindAssistantMessage, Token: leaseA.Token, GenerationID: "g-a",
					Payload: assistantPayload("late")})
				if !iso(authErr, ErrStaleEpoch) {
					t.Fatalf("stale commit error = %v, want stale-epoch", authErr)
				}
				if _, err := h.k.Renew(context.Background(), leaseA, h.ttl()); !iso(err, ErrStaleEpoch) {
					t.Fatalf("stale renew error = %v", err)
				}
				if err := h.k.Finish(context.Background(), FinishRequest{Lease: leaseA, Cursor: 0}); !iso(err, ErrStaleEpoch) {
					t.Fatalf("stale finish error = %v", err)
				}
				if n := h.countViolations(ReasonStaleEpoch); n < 3 {
					t.Fatalf("stale-epoch violations audited = %d, want >= 3", n)
				}
				// The stale finish did not disturb B's claim.
				state, err := h.k.LeaseState(context.Background(), h.scope)
				if err != nil {
					t.Fatal(err)
				}
				if !state.Found || !state.Live || state.Lease.Token != leaseB.Token {
					t.Fatalf("B's lease damaged by A: %+v", state)
				}
				if n := h.eventCount(h.scope); n != 0 {
					t.Fatalf("stale writes reached the log: %d events", n)
				}
				// B writes normally and settles the run.
				h.mustCommit(CommitRequest{Kind: KindAssistantMessage, Token: leaseB.Token, GenerationID: "g-b",
					Payload: assistantPayload("mine")})
				if err := h.k.Finish(context.Background(), FinishRequest{Lease: leaseB}); err != nil {
					t.Fatalf("finish by the current owner: %v", err)
				}
			})

			t.Run("ExpiredLeaseIsReclaimed", func(t *testing.T) {
				h := im.build(t)
				leaseA := h.claimShort(h.scope, "runtime-a")
				h.expire(leaseA)

				// Expiry alone invalidates the token, even before anyone reclaims.
				if _, err := h.k.Renew(context.Background(), leaseA, h.ttl()); !iso(err, ErrLeaseExpired) {
					t.Fatalf("renew after expiry = %v, want lease-expired", err)
				}
				if err := h.commitErr(CommitRequest{Kind: KindAssistantMessage, Token: leaseA.Token, GenerationID: "g-a",
					Payload: assistantPayload("late")}); !iso(err, ErrLeaseExpired) {
					t.Fatalf("commit after expiry = %v, want lease-expired", err)
				}

				leaseB := h.claimScope(h.scope, "runtime-b")
				if leaseB.Token.Epoch != leaseA.Token.Epoch+1 {
					t.Fatalf("epoch %d, want %d", leaseB.Token.Epoch, leaseA.Token.Epoch+1)
				}
				if _, _, attempts := h.runStatus(h.scope); attempts != 2 {
					t.Fatalf("attempts = %d, want 2 (claim accounting preserved)", attempts)
				}
				h.mustCommit(CommitRequest{Kind: KindAssistantMessage, Token: leaseB.Token, GenerationID: "g-b",
					Payload: assistantPayload("mine")})
			})

			t.Run("CrossTenantAccessLooksMissing", func(t *testing.T) {
				h := im.build(t)
				foreign := h.foreignSession()
				// Tenant A naming tenant B's session id: the pair does not exist,
				// and it answers exactly like a pair that was never created.
				crossed := Scope{TenantID: h.tenant, SessionID: foreign.SessionID}
				missing := Scope{TenantID: uuid.New(), SessionID: uuid.New()}

				// Writes.
				for _, scope := range []Scope{crossed, missing} {
					err := h.commitErr(CommitRequest{Scope: scope, Kind: KindUserMessage, RequestID: "req-x", Payload: userPayload("x")})
					if !iso(err, ErrNotFound) {
						t.Fatalf("commit to %s = %v, want not-found", scope, err)
					}
				}
				// Reads.
				for _, scope := range []Scope{crossed, missing} {
					if _, err := h.k.LoadEvents(context.Background(), scope, 0, 10); !iso(err, ErrNotFound) {
						t.Fatalf("load %s = %v, want not-found", scope, err)
					}
					if _, _, err := h.k.Lookup(context.Background(), scope, KeyRef{Kind: KeyRequest, Value: "req-x"}); !iso(err, ErrNotFound) {
						t.Fatalf("lookup %s = %v, want not-found", scope, err)
					}
					if _, err := h.k.UnresolvedToolCalls(context.Background(), scope); !iso(err, ErrNotFound) {
						t.Fatalf("unresolved %s = %v, want not-found", scope, err)
					}
				}
				if n := h.eventCount(foreign); n != 0 {
					t.Fatalf("foreign tenant session was touched: %d events", n)
				}

				// The same session UUID under two tenants is two sessions, not
				// one: identical idempotency keys do not collide.
				twin := Scope{TenantID: foreign.TenantID, SessionID: h.scope.SessionID}
				if h.mem != nil {
					h.mem.CreateSession(twin.TenantID, twin.SessionID)
					h.mem.EnqueueRun(twin.TenantID, twin.SessionID)
				} else {
					ctx := context.Background()
					if err := h.st.CreateSession(ctx, twin.TenantID, twin.SessionID); err != nil {
						t.Fatal(err)
					}
					if err := h.st.EnqueueRun(ctx, twin.TenantID, twin.SessionID); err != nil {
						t.Fatal(err)
					}
				}
				mine := h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "same-key", Payload: userPayload("mine")})
				theirs := h.mustCommit(CommitRequest{Scope: twin, Kind: KindUserMessage, RequestID: "same-key", Payload: userPayload("theirs")})
				if mine.Seq != 1 || theirs.Seq != 1 {
					t.Fatalf("tenant twins share sequences: %+v %+v", mine, theirs)
				}
				if text := EventText(h.events(h.scope)[0]); text != "mine" {
					t.Fatalf("tenant A sees %q", text)
				}
				if text := EventText(h.events(twin)[0]); text != "theirs" {
					t.Fatalf("tenant B sees %q", text)
				}
			})

			t.Run("KindAndKeyValidation", func(t *testing.T) {
				h := im.build(t)
				lease := h.claimScope(h.scope, "runtime-a")

				// Unknown kind.
				if err := h.commitErr(CommitRequest{Kind: "user.thing", RequestID: "r", Payload: userPayload("x")}); !iso(err, ErrUnknownKind) {
					t.Fatalf("unknown kind = %v", err)
				}
				// Missing / wrong idempotency keys.
				cases := []struct {
					name string
					req  CommitRequest
				}{
					{"user.message without requestId", CommitRequest{Kind: KindUserMessage, Payload: userPayload("x")}},
					{"user.message with generationId", CommitRequest{Kind: KindUserMessage, GenerationID: "g", Payload: userPayload("x")}},
					{"assistant.message without generationId", CommitRequest{Kind: KindAssistantMessage, Token: lease.Token, Payload: assistantPayload("x")}},
					{"assistant.message with requestId", CommitRequest{Kind: KindAssistantMessage, Token: lease.Token, RequestID: "r", Payload: assistantPayload("x")}},
					{"tool.result without toolCallId", CommitRequest{Kind: KindToolResult, Token: lease.Token, Payload: toolResultPayload("c", "t")}},
					{"compaction with a key", CommitRequest{Kind: KindCompaction, Token: lease.Token, RequestID: "r", Payload: json.RawMessage(`{"summary":"s"}`)}},
				}
				for _, c := range cases {
					if err := h.commitErr(c.req); !iso(err, ErrKeyKindMismatch) {
						t.Fatalf("%s = %v, want key-kind-mismatch", c.name, err)
					}
				}
				// A client kind must not carry an owner token.
				if err := h.commitErr(CommitRequest{Kind: KindUserMessage, RequestID: "r", Token: lease.Token, Payload: userPayload("x")}); !iso(err, ErrInvalidRequest) {
					t.Fatalf("user.message with token = %v", err)
				}
				// Malformed assistant payloads.
				if err := h.commitErr(CommitRequest{Kind: KindAssistantMessage, Token: lease.Token, GenerationID: "g-bad",
					Payload: json.RawMessage(`not json`)}); !iso(err, ErrInvalidRequest) {
					t.Fatalf("bad payload = %v", err)
				}
				if err := h.commitErr(CommitRequest{Kind: KindAssistantMessage, Token: lease.Token, GenerationID: "g-dup",
					Payload: assistantPayload("", toolCall("c", "clock"), toolCall("c", "clock"))}); !iso(err, ErrDuplicateToolCall) {
					t.Fatalf("duplicate call ids = %v", err)
				}
				if err := h.commitErr(CommitRequest{Kind: KindAssistantMessage, Token: lease.Token, GenerationID: "g-noid",
					Payload: assistantPayload("", map[string]any{"type": "toolCall", "name": "clock"})}); !iso(err, ErrInvalidRequest) {
					t.Fatalf("call without id = %v", err)
				}
				// Missing scope.
				if err := h.commitErr(CommitRequest{Kind: KindUserMessage, RequestID: "r", Payload: userPayload("x"), Scope: Scope{TenantID: uuid.New()}}); !iso(err, ErrInvalidRequest) {
					t.Fatalf("missing session id = %v", err)
				}
				if n := h.eventCount(h.scope); n != 0 {
					t.Fatalf("rejected commits reached the log: %d events", n)
				}
			})

			t.Run("FinishAppliesOutcomePolicy", func(t *testing.T) {
				h := im.build(t)
				lease := h.claimScope(h.scope, "runtime-a")
				first := h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "r1", Payload: userPayload("hi")})

				// Clean finish with no newer input: done.
				if err := h.k.Finish(context.Background(), FinishRequest{Lease: lease, Cursor: first.Seq}); err != nil {
					t.Fatal(err)
				}
				status, cursor, _ := h.runStatus(h.scope)
				if status != RunStatusDone || cursor != first.Seq {
					t.Fatalf("after clean finish: status=%s cursor=%d", status, cursor)
				}
				// Commits after finish are refused: the lease is gone.
				if err := h.commitErr(CommitRequest{Kind: KindAssistantMessage, Token: lease.Token, GenerationID: "g-late",
					Payload: assistantPayload("late")}); !iso(err, ErrLeaseLost) {
					t.Fatalf("commit after finish = %v", err)
				}

				// New input arrives while the run executes: the run re-queues.
				h.enqueue(h.scope)
				lease2 := h.claimScope(h.scope, "runtime-a")
				second := h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "r2", Payload: userPayload("more")})
				if err := h.k.Finish(context.Background(), FinishRequest{Lease: lease2, Cursor: first.Seq}); err != nil {
					t.Fatal(err)
				}
				status, _, _ = h.runStatus(h.scope)
				if status != RunStatusPending {
					t.Fatalf("status with newer input = %s, want pending", status)
				}

				// Failures: retried until the attempt budget, then failed.
				lease3 := h.claimScope(h.scope, "runtime-a")
				if err := h.k.Finish(context.Background(), FinishRequest{Lease: lease3, Cursor: second.Seq, Err: errors.New("boom"), MaxAttempts: 5}); err != nil {
					t.Fatal(err)
				}
				if status, _, _ = h.runStatus(h.scope); status != RunStatusPending {
					t.Fatalf("status under attempt budget = %s, want pending", status)
				}
				lease4 := h.claimScope(h.scope, "runtime-a")
				if err := h.k.Finish(context.Background(), FinishRequest{Lease: lease4, Cursor: second.Seq, Err: errors.New("boom"), MaxAttempts: 2}); err != nil {
					t.Fatal(err)
				}
				if status, _, _ = h.runStatus(h.scope); status != RunStatusFailed {
					t.Fatalf("status over attempt budget = %s, want failed", status)
				}
				// A finished run is not claimable.
				if _, err := h.k.ClaimScope(context.Background(), h.scope, "runtime-a", h.ttl()); !iso(err, ErrNoRunnable) {
					t.Fatalf("claim of a failed run = %v, want no-runnable", err)
				}
			})

			t.Run("ReleaseReturnsTheClaim", func(t *testing.T) {
				h := im.build(t)
				leaseA := h.claimScope(h.scope, "runtime-a")
				if err := h.k.Release(context.Background(), leaseA); err != nil {
					t.Fatal(err)
				}
				if state, _ := h.k.LeaseState(context.Background(), h.scope); state.Found {
					t.Fatalf("lease survived release: %+v", state)
				}
				if status, _, _ := h.runStatus(h.scope); status != RunStatusPending {
					t.Fatalf("status after release = %s, want pending", status)
				}
				leaseB := h.claimScope(h.scope, "runtime-b")
				if leaseB.Token.Epoch <= leaseA.Token.Epoch {
					t.Fatalf("epoch did not advance: %d -> %d", leaseA.Token.Epoch, leaseB.Token.Epoch)
				}
				if err := h.k.Release(context.Background(), leaseA); !iso(err, ErrStaleEpoch) {
					t.Fatalf("stale release = %v", err)
				}
				if state, _ := h.k.LeaseState(context.Background(), h.scope); !state.Live || state.Lease.Token != leaseB.Token {
					t.Fatalf("stale release damaged the live claim: %+v", state)
				}
			})

			t.Run("ConcurrentClaimsHaveOneWinner", func(t *testing.T) {
				h := im.build(t)
				const workers = 4
				var (
					wg      sync.WaitGroup
					mu      sync.Mutex
					winners int
					others  []error
				)
				for i := 0; i < workers; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						lease, err := h.k.ClaimScope(context.Background(), h.scope, fmt.Sprintf("runtime-%d", i), h.ttl())
						mu.Lock()
						defer mu.Unlock()
						if err == nil {
							winners++
							if lease.Token.Owner == "" || lease.Token.Epoch == 0 {
								t.Errorf("winner has no token: %+v", lease)
							}
							return
						}
						others = append(others, err)
					}(i)
				}
				wg.Wait()
				if winners != 1 {
					t.Fatalf("winners = %d, want exactly 1", winners)
				}
				for _, err := range others {
					if !iso(err, ErrLeaseHeld) {
						t.Fatalf("loser error = %v, want lease-held", err)
					}
				}
			})

			t.Run("DrainWindowAcceptsClientInput", func(t *testing.T) {
				h := im.build(t)
				leaseA := h.claimShort(h.scope, "runtime-a")
				h.mustCommit(CommitRequest{Kind: KindAssistantMessage, Token: leaseA.Token, GenerationID: "g1",
					Payload: assistantPayload("working")})

				// The handoff completes: A is drained, B owns the session.
				h.expire(leaseA)
				leaseB := h.claimScope(h.scope, "runtime-b")

				// Edge-scoped input is accepted without a token even though B now
				// holds the lease, and replays dedupe as usual.
				edge := h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "client-1", Payload: userPayload("still here")})
				if edge.Deduped {
					t.Fatalf("first client input reported as a replay: %+v", edge)
				}
				replay := h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "client-1", Payload: userPayload("still here")})
				if !replay.Deduped || replay.Seq != edge.Seq {
					t.Fatalf("client replay = %+v, want seq %d", replay, edge.Seq)
				}
				// The drained writer still cannot write run-scoped facts.
				if err := h.commitErr(CommitRequest{Kind: KindAssistantMessage, Token: leaseA.Token, GenerationID: "g2",
					Payload: assistantPayload("late")}); !iso(err, ErrStaleEpoch) {
					t.Fatalf("drained writer commit = %v, want stale-epoch", err)
				}
				h.mustCommit(CommitRequest{Kind: KindAssistantMessage, Token: leaseB.Token, GenerationID: "g2",
					Payload: assistantPayload("mine")})
				// Settling B's run re-queues it for the input that arrived mid-drain.
				if err := h.k.Finish(context.Background(), FinishRequest{Lease: leaseB, Cursor: 0}); err != nil {
					t.Fatal(err)
				}
				if status, _, _ := h.runStatus(h.scope); status != RunStatusPending {
					t.Fatalf("run status = %s, want pending (client input must not be dropped)", status)
				}
			})

			t.Run("ClaimAndStaleCommitInterleave", func(t *testing.T) {
				h := im.build(t)
				leaseA := h.claimShort(h.scope, "runtime-a")
				h.expire(leaseA)

				var (
					wg       sync.WaitGroup
					claimErr error
					staleErr error
					leaseB   Lease
				)
				wg.Add(2)
				go func() {
					defer wg.Done()
					leaseB, claimErr = h.k.ClaimScope(context.Background(), h.scope, "runtime-b", h.ttl())
				}()
				go func() {
					defer wg.Done()
					staleErr = h.commitErr(CommitRequest{Kind: KindAssistantMessage, Token: leaseA.Token,
						GenerationID: "g-race", Payload: assistantPayload("stale")})
				}()
				wg.Wait()

				if claimErr != nil {
					t.Fatalf("handoff claim failed: %v", claimErr)
				}
				if staleErr == nil {
					t.Fatal("stale commit was accepted")
				}
				if !iso(staleErr, ErrStaleEpoch) && !iso(staleErr, ErrLeaseExpired) {
					t.Fatalf("stale commit error = %v", staleErr)
				}
				for _, ev := range h.events(h.scope) {
					if ev.Kind == KindAssistantMessage {
						t.Fatalf("stale commit reached the log: %s", ev.Payload)
					}
				}
				h.mustCommit(CommitRequest{Kind: KindAssistantMessage, Token: leaseB.Token,
					GenerationID: "g-final", Payload: assistantPayload("final")})
				replies := 0
				for _, ev := range h.events(h.scope) {
					if ev.Kind == KindAssistantMessage {
						replies++
					}
				}
				if replies != 1 {
					t.Fatalf("final replies = %d, want exactly 1", replies)
				}
			})

			t.Run("GlobalClaimNeverStealsALiveLease", func(t *testing.T) {
				h := im.build(t)
				leaseA := h.claimScope(h.scope, "runtime-a")
				var claimed []Lease
				for i := 0; i < 20; i++ {
					lease, err := h.k.Claim(context.Background(), "runtime-global", h.ttl())
					if err != nil {
						if !iso(err, ErrNoRunnable) {
							t.Fatalf("global claim: %v", err)
						}
						break
					}
					if lease.Scope == h.scope {
						t.Fatal("global claim stole a live lease")
					}
					claimed = append(claimed, lease)
				}
				for _, lease := range claimed {
					if err := h.k.Release(context.Background(), lease); err != nil {
						t.Fatalf("release claimed run: %v", err)
					}
				}
				// Our own session is untouched and still owned by A.
				state, err := h.k.LeaseState(context.Background(), h.scope)
				if err != nil {
					t.Fatal(err)
				}
				if state.Lease.Token != leaseA.Token {
					t.Fatalf("live lease changed: %+v", state)
				}
			})

			t.Run("LookupAndLeaseStateOnMissingSession", func(t *testing.T) {
				h := im.build(t)
				missing := Scope{TenantID: uuid.New(), SessionID: uuid.New()}
				if _, _, err := h.k.Lookup(context.Background(), missing, KeyRef{Kind: KeyRequest, Value: "k"}); !iso(err, ErrNotFound) {
					t.Fatalf("lookup = %v", err)
				}
				if _, err := h.k.LeaseState(context.Background(), missing); !iso(err, ErrNotFound) {
					t.Fatalf("lease state = %v", err)
				}
				if _, err := h.k.ClaimScope(context.Background(), missing, "runtime-a", h.ttl()); !iso(err, ErrNotFound) {
					t.Fatalf("claim = %v", err)
				}
			})

			t.Run("AdmissionSeamGatesClaimAndCommit", func(t *testing.T) {
				h := im.build(t)
				h.lims = &testLimits{denyRun: true}
				h.k = New(h.log, Options{Audit: h.recordViolation, Limits: h.lims})
				if _, err := h.k.Claim(context.Background(), "runtime-a", h.ttl()); !iso(err, ErrQuota) {
					t.Fatalf("claim under run quota = %v, want quota", err)
				}
				// The rejected claim was undone: the run is claimable again.
				status, _, _ := h.runStatus(h.scope)
				if status != RunStatusPending {
					t.Fatalf("status after quota rejection = %s, want pending", status)
				}
				h.lims.mu.Lock()
				h.lims.denyRun = false
				h.lims.mu.Unlock()
				lease := h.claimScope(h.scope, "runtime-a")

				h.lims.mu.Lock()
				h.lims.denyEvent = true
				h.lims.mu.Unlock()
				if err := h.commitErr(CommitRequest{Kind: KindUserMessage, RequestID: "r", Payload: userPayload("x")}); !iso(err, ErrQuota) {
					t.Fatalf("commit under event quota = %v, want quota", err)
				}
				if n := h.eventCount(h.scope); n != 0 {
					t.Fatalf("quota-rejected commit reached the log: %d events", n)
				}
				h.lims.mu.Lock()
				h.lims.denyEvent = false
				h.lims.mu.Unlock()
				h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "r", Payload: userPayload("x")})
				if err := h.k.Finish(context.Background(), FinishRequest{Lease: lease}); err != nil {
					t.Fatal(err)
				}
			})

			if im.name == "mem" {
				t.Run("CommitRetriesTransientFailures", func(t *testing.T) {
					h := im.build(t)
					// Two transient failures (as a deadlocked statement would
					// surface) are replayed inside the retry budget.
					before := h.mem.BeginSessionCount()
					h.mem.FailNextRetryable(2)
					r := h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "r1", Payload: userPayload("x")})
					if r.Seq != 1 || r.Deduped {
						t.Fatalf("receipt = %+v", r)
					}
					if n := h.mem.BeginSessionCount() - before; n != 3 {
						t.Fatalf("attempts = %d, want 3 (two transient + one success)", n)
					}
					if n := h.eventCount(h.scope); n != 1 {
						t.Fatalf("events = %d, want 1", n)
					}
					// A permanent failure is surfaced immediately: one attempt.
					begins := h.mem.BeginSessionCount()
					h.mem.FailNextAppend(errors.New("permanent storage failure"))
					if err := h.commitErr(CommitRequest{Kind: KindUserMessage, RequestID: "r2", Payload: userPayload("y")}); !iso(err, ErrStoreFailure) {
						t.Fatalf("permanent failure = %v", err)
					}
					if n := h.mem.BeginSessionCount() - begins; n != 1 {
						t.Fatalf("attempts = %d, want 1 (no replay for permanent failures)", n)
					}
					// An exhausted budget surfaces one audited store failure and
					// nothing lands.
					begins = h.mem.BeginSessionCount()
					audits := h.countViolations(ReasonStoreFailure)
					h.mem.FailNextRetryable(retryBudget + 2)
					if err := h.commitErr(CommitRequest{Kind: KindUserMessage, RequestID: "r3", Payload: userPayload("z")}); !iso(err, ErrStoreFailure) {
						t.Fatalf("exhausted budget = %v", err)
					}
					if n := h.mem.BeginSessionCount() - begins; n != retryBudget {
						t.Fatalf("attempts = %d, want %d", n, retryBudget)
					}
					if n := h.countViolations(ReasonStoreFailure) - audits; n != 1 {
						t.Fatalf("audited store failures = %d, want 1", n)
					}
					if n := h.eventCount(h.scope); n != 1 {
						t.Fatalf("events = %d, want 1 (transient storms leave nothing behind)", n)
					}
				})

				t.Run("AppendFailureRollsBackWithoutGap", func(t *testing.T) {
					h := im.build(t)
					h.mem.FailNextAppend(errors.New("injected append failure"))
					if err := h.commitErr(CommitRequest{Kind: KindUserMessage, RequestID: "r1", Payload: userPayload("x")}); !iso(err, ErrStoreFailure) {
						t.Fatalf("append failure = %v, want store failure", err)
					}
					if n := h.eventCount(h.scope); n != 0 {
						t.Fatalf("event count after failed append = %d", n)
					}
					// No sequence was consumed: the retry lands on seq 1.
					if r := h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "r1", Payload: userPayload("x")}); r.Seq != 1 {
						t.Fatalf("retry seq = %d, want 1 (no gap)", r.Seq)
					}
					// A failure at commit time rolls the staged append back too.
					h.mem.FailNextCommit(errors.New("injected commit failure"))
					if err := h.commitErr(CommitRequest{Kind: KindUserMessage, RequestID: "r2", Payload: userPayload("y")}); !iso(err, ErrStoreFailure) {
						t.Fatalf("commit failure = %v, want store failure", err)
					}
					if n := h.eventCount(h.scope); n != 1 {
						t.Fatalf("event count after failed commit = %d, want 1", n)
					}
					if r := h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "r2", Payload: userPayload("y")}); r.Seq != 2 {
						t.Fatalf("retry seq = %d, want 2", r.Seq)
					}
				})
			}
		})
	}
}

// TestConcurrentCommitsAndMigrations is the deadlock stress test: commits on one
// session (maximum contention on the session row) run interleaved with
// concurrent migrations. Every commit must land exactly once, sequences stay
// gap-free, and no transient failure may escape (the kernel replays them).
func TestConcurrentCommitsAndMigrations(t *testing.T) {
	for _, im := range implementations(t) {
		im := im
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			const workers, perWorker = 6, 8
			var (
				wg    sync.WaitGroup
				mu    sync.Mutex
				errCh []error
			)
			record := func(err error) {
				if err == nil {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				errCh = append(errCh, err)
			}
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					for j := 0; j < perWorker; j++ {
						_, err := h.k.Commit(context.Background(), CommitRequest{
							Scope: h.scope, Kind: KindUserMessage,
							RequestID: fmt.Sprintf("w%d-%d", i, j), Payload: userPayload("x"),
						})
						if err != nil {
							record(fmt.Errorf("worker %d: %w", i, err))
							return
						}
					}
				}(i)
			}
			if h.pg != nil {
				for i := 0; i < 3; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						record(h.pg.Migrate(context.Background()))
					}(i)
				}
			}
			wg.Wait()
			for _, err := range errCh {
				t.Fatalf("commit storm surfaced: %v", err)
			}
			events := h.events(h.scope)
			if len(events) != workers*perWorker {
				t.Fatalf("events = %d, want %d", len(events), workers*perWorker)
			}
			for i, ev := range events {
				if ev.Seq != int64(i+1) {
					t.Fatalf("sequence gap at index %d: seq %d", i, ev.Seq)
				}
			}
			// Dedupe still holds after the storm.
			if r := h.mustCommit(CommitRequest{Scope: h.scope, Kind: KindUserMessage,
				RequestID: "w0-0", Payload: userPayload("x")}); !r.Deduped {
				t.Fatalf("replay after storm = %+v", r)
			}
			if h.pg != nil {
				t.Logf("postgres replayed %d transient failures during the storm", h.pg.retries.Load())
			}
		})
	}
}

// TestPGFailClassification pins the transient classification the retry loops
// rely on: exactly the codes internal/store retries (deadlock, serialization,
// lock unavailable), and nothing else.
func TestPGFailClassification(t *testing.T) {
	for _, code := range []string{"40P01", "40001", "55P03"} {
		if err := pgFail(&pgconn.PgError{Code: code}); !isRetryableTx(err) {
			t.Errorf("code %s is not marked retryable", code)
		}
	}
	for _, code := range []string{"23505", "42703", "57014"} {
		if err := pgFail(&pgconn.PgError{Code: code}); isRetryableTx(err) {
			t.Errorf("code %s is wrongly marked retryable", code)
		}
	}
	if isRetryableTx(pgFail(nil)) {
		t.Error("nil error marked retryable")
	}
}

// TestPGRetryTxReplaysTransientFailures pins the adapter replay loop: transient
// failures are replayed inside the budget, permanent ones surface at once, and
// an exhausted budget surfaces the last transient failure.
func TestPGRetryTxReplaysTransientFailures(t *testing.T) {
	ctx := context.Background()

	s := &PGStore{}
	calls := 0
	if err := s.retryTx(ctx, func() error {
		calls++
		if calls < 3 {
			return markRetryable(errors.New("deadlock detected"))
		}
		return nil
	}); err != nil || calls != 3 {
		t.Fatalf("transient replay: err=%v calls=%d, want nil/3", err, calls)
	}
	if n := s.retries.Load(); n != 2 {
		t.Fatalf("replay counter = %d, want 2", n)
	}

	permanent := errors.New("column does not exist")
	calls = 0
	if err := s.retryTx(ctx, func() error { calls++; return permanent }); !errors.Is(err, permanent) || calls != 1 {
		t.Fatalf("permanent failure: err=%v calls=%d, want the error and one attempt", err, calls)
	}

	calls = 0
	err := s.retryTx(ctx, func() error { calls++; return markRetryable(errors.New("still deadlocking")) })
	if !isRetryableTx(err) || calls != retryBudget {
		t.Fatalf("exhausted budget: err=%v calls=%d, want a transient error after %d attempts", err, calls, retryBudget)
	}
}

// pgNeedsDSN skips a PostgreSQL-only test when no test database is configured.
func pgNeedsDSN(t *testing.T) {
	t.Helper()
	if os.Getenv("KEEL_TEST_DSN") == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
}

// waitForClaimBlocked waits until a backend is blocked inside the claim's run
// write, so the test can order its settle after the claim has taken its
// candidate snapshot and locked the session row.
func waitForClaimBlocked(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var blocked int
		err := h.st.Pool().QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			 WHERE wait_event_type = 'Lock'
			   AND (query LIKE '%attempts = attempts + 1%'
			        OR query LIKE '%pending_input%read_watermark%FROM runs%')`).Scan(&blocked)
		if err != nil {
			t.Fatalf("pg_stat_activity probe: %v", err)
		}
		if blocked > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("claim never blocked on the contended run row")
}

// TestClaimDoesNotResurrectSettledRun is the regression test for the reviewed
// P0: a claim's candidate snapshot sees a claimable run, another writer settles
// that run before the claim's write lands, and the claim must not bring it back
// to running. The interleaving is forced with a row lock held by the racing
// writer: the claim takes its snapshot and locks the session row, blocks on the
// run row, the settle commits, and the guarded claim write must find nothing.
//
// PostgreSQL only: MemStore serializes everything under one lock, which is
// exactly why the fake cannot express this interleaving.
func TestClaimDoesNotResurrectSettledRun(t *testing.T) {
	pgNeedsDSN(t)
	ctx := context.Background()

	race := func(t *testing.T, claim func(h *harness) (Lease, error), expectNoRunnable bool) {
		t.Helper()
		h := newPGHarness(t)
		old := h.claimShort(h.scope, "runtime-old")
		h.expire(old) // the crashed owner: running, lease expired

		// The racing writer takes the run row lock it is about to settle under.
		tx, err := h.st.Pool().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		var status string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM runs WHERE tenant_id = $1 AND session_id = $2 FOR UPDATE`,
			h.scope.TenantID, h.scope.SessionID).Scan(&status); err != nil {
			t.Fatalf("lock run row: %v", err)
		}

		type outcome struct {
			lease Lease
			err   error
		}
		done := make(chan outcome, 1)
		go func() {
			lease, err := claim(h)
			done <- outcome{lease, err}
		}()
		waitForClaimBlocked(t, h)

		// Settle the run the way an unfenced writer (a drained runtime's legacy
		// finish) would, then let the claim's write proceed.
		if _, err := tx.Exec(ctx,
			`UPDATE runs SET status = 'done', lease_until = NULL WHERE tenant_id = $1 AND session_id = $2`,
			h.scope.TenantID, h.scope.SessionID); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}

		res := <-done
		statusNow, _, _ := h.runStatus(h.scope)
		if statusNow != RunStatusDone {
			t.Fatalf("settled run resurrected: status = %s (claim result %+v)", statusNow, res)
		}
		if res.err == nil && res.lease.Scope == h.scope {
			t.Fatalf("claim handed out a lease for the settled run: %+v", res.lease)
		}
		if expectNoRunnable && !iso(res.err, ErrNoRunnable) {
			t.Fatalf("claim error = %v, want no-runnable", res.err)
		}
		state, err := h.k.LeaseState(ctx, h.scope)
		if err != nil {
			t.Fatal(err)
		}
		if state.Live {
			t.Fatalf("lease live after a refused claim: %+v", state)
		}
		if state.Lease.Token.Epoch != old.Token.Epoch {
			t.Fatalf("failed claim left an epoch bump behind: %d -> %d", old.Token.Epoch, state.Lease.Token.Epoch)
		}
	}

	t.Run("ClaimScope", func(t *testing.T) {
		race(t, func(h *harness) (Lease, error) {
			return h.k.ClaimScope(ctx, h.scope, "runtime-new", h.ttl())
		}, true)
	})
}

// TestClaimRefusesARunWithALiveMirror pins the fail-closed rule of the claim
// scan: a run whose mirror says "held" is never re-claimed, even when the
// session authority looks expired (the divergent state a racing writer leaves
// behind). Both claim paths must leave the run row untouched.
func TestClaimRefusesARunWithALiveMirror(t *testing.T) {
	pgNeedsDSN(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		claim func(h *harness) (Lease, error)
	}{
		{"ClaimScope", func(h *harness) (Lease, error) {
			return h.k.ClaimScope(ctx, h.scope, "runtime-new", h.ttl())
		}},
		{"ClaimLease", func(h *harness) (Lease, error) {
			return h.k.Claim(ctx, "runtime-new", h.ttl())
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			h := newPGHarness(t)
			// A held run whose authority lease already expired: the candidate
			// scan may consider it, the run row itself says otherwise.
			if _, err := h.st.Pool().Exec(ctx,
				`UPDATE sessions SET owner = 'runtime-ghost', owner_epoch = 7, lease_until = now() - interval '1 hour'
				 WHERE tenant_id = $1 AND session_id = $2`,
				h.scope.TenantID, h.scope.SessionID); err != nil {
				t.Fatal(err)
			}
			if _, err := h.st.Pool().Exec(ctx,
				`UPDATE runs SET status = 'running', attempts = 3, owner = 'runtime-ghost', owner_epoch = 7,
				                  lease_until = now() + interval '1 hour'
				 WHERE tenant_id = $1 AND session_id = $2`,
				h.scope.TenantID, h.scope.SessionID); err != nil {
				t.Fatal(err)
			}

			lease, err := tc.claim(h)
			if err == nil && lease.Scope == h.scope {
				t.Fatalf("claim took a run that reports itself held: %+v", lease)
			}
			if err != nil && !iso(err, ErrNoRunnable) && !iso(err, ErrLeaseHeld) {
				t.Fatalf("claim failed for an unexpected reason: %v", err)
			}
			status, cursor, attempts := h.runStatus(h.scope)
			if status != RunStatusRunning || attempts != 3 || cursor != 0 {
				t.Fatalf("run row was modified by a refused claim: status=%s attempts=%d cursor=%d", status, attempts, cursor)
			}
			state, err := h.k.LeaseState(ctx, h.scope)
			if err != nil {
				t.Fatal(err)
			}
			if state.Live || state.Lease.Token.Epoch != 7 || state.Lease.Token.Owner != "runtime-ghost" {
				t.Fatalf("authority changed by a refused claim: %+v", state)
			}
		})
	}
}

// TestEventKindsMatchStore pins the kernel's kind strings to the durable
// constants internal/store publishes.
func TestEventKindsMatchStore(t *testing.T) {
	pairs := map[EventKind]string{
		KindUserMessage:      store.EventUserMessage,
		KindAssistantMessage: store.EventAssistantMessage,
		KindToolResult:       store.EventToolResult,
		KindCompaction:       store.EventCompaction,
		KindRunStarted:       store.EventRunStarted,
		KindRunFailed:        store.EventRunFailed,
		KindRunFinished:      store.EventRunFinished,
		KindExternalEvent:    store.EventExternalEvent,
	}
	for kind, want := range pairs {
		if string(kind) != want {
			t.Errorf("kernel kind %q != store constant %q", kind, want)
		}
	}
	if len(kindRegistry) != len(pairs) {
		t.Errorf("kindRegistry has %d kinds, store has %d", len(kindRegistry), len(pairs))
	}
	if got := Kinds(); len(got) != len(kindRegistry) {
		t.Errorf("Kinds() = %v", got)
	}
}

// TestDedupeKeyNamespace pins the durable namespace format (pre-existing logs
// carry "tool:" keys, so the kernel must keep that prefix).
func TestDedupeKeyNamespace(t *testing.T) {
	cases := map[KeyRef]string{
		{Kind: KeyToolCall, Value: "c1"}:      "tool:c1",
		{Kind: KeyRequest, Value: "r1"}:       "req:r1",
		{Kind: KeyGeneration, Value: "g1"}:    "gen:g1",
		{Kind: KeyKind("custom"), Value: "x"}: "custom:x",
		{}:                                    "",
	}
	for key, want := range cases {
		if got := key.dedupeKey(); got != want {
			t.Errorf("dedupeKey(%v) = %q, want %q", key, got, want)
		}
	}
}

// TestCommitClassesAndEdgeWhitelist pins the three-lane commit model: the edge
// whitelist is exactly the edge-scoped kinds, and the external lane is its own
// class (so an edge endpoint can never accept an external.event).
func TestCommitClassesAndEdgeWhitelist(t *testing.T) {
	edge := EdgeKinds()
	if len(edge) != 1 || edge[0] != KindUserMessage {
		t.Fatalf("EdgeKinds() = %v, want [user.message]", edge)
	}
	for _, kind := range Kinds() {
		class, ok := KindClass(kind)
		if !ok {
			t.Fatalf("kind %s has no class", kind)
		}
		want := ClassOwner
		switch kind {
		case KindUserMessage:
			want = ClassEdge
		case KindExternalEvent:
			want = ClassExternal
		}
		if class != want {
			t.Errorf("kind %s class = %s, want %s", kind, class, want)
		}
	}
	if _, ok := KindClass("assistant.nope"); ok {
		t.Error("unknown kind reported a class")
	}
}

// TestFinishStatusPolicy mirrors store.FinishRun's decisions.
func TestFinishStatusPolicy(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		attempts    int
		maxAttempts int
		moreInput   bool
		cancelled   bool
		want        string
	}{
		{"clean", nil, 1, 3, false, false, RunStatusDone},
		{"newer input", nil, 1, 3, true, false, RunStatusPending},
		{"failure retryable", errors.New("x"), 1, 3, false, false, RunStatusPending},
		{"failure over budget", errors.New("x"), 3, 3, false, false, RunStatusFailed},
		{"failure without budget", errors.New("x"), 9, 0, false, false, RunStatusPending},
		// A cancel is terminal even with an attempt budget left and unread input:
		// re-queueing would make the cancel a no-op.
		{"cancelled", context.Canceled, 1, 3, true, true, RunStatusFailed},
	}
	for _, c := range cases {
		if got := finishStatus(c.err, c.attempts, c.maxAttempts, c.moreInput, c.cancelled); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

// TestTokenGuardClassification pins the fencing decision table.
func TestTokenGuardClassification(t *testing.T) {
	live := LeaseState{Lease: Lease{Token: OwnerToken{Owner: "a", Epoch: 2}}, Found: true, Live: true}
	cases := []struct {
		name  string
		state LeaseState
		token OwnerToken
		want  error
	}{
		{"no lease", LeaseState{}, OwnerToken{Owner: "a", Epoch: 2}, ErrLeaseLost},
		{"expired", LeaseState{Lease: live.Lease, Found: true, Live: false}, OwnerToken{Owner: "a", Epoch: 2}, ErrLeaseExpired},
		{"older epoch", live, OwnerToken{Owner: "a", Epoch: 1}, ErrStaleEpoch},
		{"other owner", live, OwnerToken{Owner: "b", Epoch: 2}, ErrStaleEpoch},
		{"current", live, OwnerToken{Owner: "a", Epoch: 2}, nil},
	}
	for _, c := range cases {
		got := checkToken(c.state, c.token)
		if c.want == nil {
			if got != nil {
				t.Errorf("%s: got %v, want nil", c.name, got)
			}
			continue
		}
		if !errors.Is(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRunLoadoutMetadata: the loadout a run pinned is written with the settle
// transaction and readable afterwards, including the generation and fencing
// epoch; a foreign scope never exposes this tenant's pin.
func TestRunLoadoutMetadata(t *testing.T) {
	for _, im := range implementations(t) {
		im := im
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()
			// The run row exists from the queue insert; until an attempt settles
			// the settle metadata is zero (a claim pin may be written separately).
			if pin, err := h.k.RunPin(ctx, h.scope); err != nil {
				t.Fatal(err)
			} else if pin.LoadoutGeneration != 0 || pin.LoadoutOwnerEpoch != 0 {
				t.Fatalf("before any settle: %+v", pin)
			}
			lease := h.claimScope(h.scope, "owner-a")
			pin := RunLoadout{ID: "tenant-default", Revision: 7, Generation: 3}
			if err := h.k.Finish(ctx, FinishRequest{Lease: lease, MaxAttempts: 3, Loadout: pin}); err != nil {
				t.Fatalf("finish: %v", err)
			}
			got, err := h.k.RunPin(ctx, h.scope)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if got.LoadoutID != pin.ID || got.LoadoutRevision != pin.Revision || got.LoadoutGeneration != pin.Generation {
				t.Fatalf("loadout = %+v, want %+v", got, pin)
			}
			if got.LoadoutOwnerEpoch != lease.Token.Epoch {
				t.Fatalf("owner epoch = %d, want %d", got.LoadoutOwnerEpoch, lease.Token.Epoch)
			}
			// A later attempt records its own revision; the metadata follows the
			// settle, not the first write.
			h.enqueue(h.scope)
			next := h.claimScope(h.scope, "owner-b")
			pin2 := RunLoadout{ID: "tenant-default", Revision: 8, Generation: 4}
			if err := h.k.Finish(ctx, FinishRequest{Lease: next, MaxAttempts: 3, Loadout: pin2}); err != nil {
				t.Fatalf("finish second: %v", err)
			}
			got, err = h.k.RunPin(ctx, h.scope)
			if err != nil {
				t.Fatal(err)
			}
			if got.LoadoutRevision != 8 || got.LoadoutGeneration != 4 || got.LoadoutOwnerEpoch != next.Token.Epoch {
				t.Fatalf("second settle metadata = %+v", got)
			}
			// A foreign tenant's scope never exposes this tenant's pin.
			if foreign, err := h.k.RunPin(ctx, h.foreignSession()); err != nil {
				t.Fatalf("foreign pin: %v", err)
			} else if foreign.LoadoutID == pin.ID || foreign.LoadoutRevision == pin.Revision {
				t.Fatalf("foreign scope leaked the pin: %+v", foreign)
			}
			if _, err := h.k.RunPin(ctx, Scope{TenantID: h.tenant, SessionID: uuid.New()}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("unknown session: want ErrNotFound, got %v", err)
			}
		})
	}
}
