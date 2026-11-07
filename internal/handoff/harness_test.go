package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

// The handoff protocol suite runs against both persistence implementations:
// MemStore + MemRegistry always, PGStore + PGRegistry when KEEL_TEST_DSN is
// set (the repository's gated integration pattern: no DSN skips, never fails).
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

// testClock is the fake clock the mem harness drives (PG uses now()).
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

// harness is one test subject: a session with a run row, a kernel wired to an
// audit sink, and a handoff registry.
type harness struct {
	t     *testing.T
	k     *kernel.Kernel
	mem   *kernel.MemStore
	st    *store.Store // pg only
	reg   Registry
	pg    *kernel.PGStore // pg only
	clock *testClock      // mem only

	tenant  uuid.UUID
	session uuid.UUID
	scope   kernel.Scope

	auditMu sync.Mutex
	audit   []kernel.Violation
}

func (h *harness) recordViolation(v kernel.Violation) {
	h.auditMu.Lock()
	defer h.auditMu.Unlock()
	h.audit = append(h.audit, v)
}

func (h *harness) violations() []kernel.Violation {
	h.auditMu.Lock()
	defer h.auditMu.Unlock()
	return append([]kernel.Violation(nil), h.audit...)
}

func (h *harness) staleViolations() []kernel.Violation {
	var out []kernel.Violation
	for _, v := range h.violations() {
		if v.Reason == kernel.ReasonStaleEpoch || v.Reason == kernel.ReasonLeaseExpired || v.Reason == kernel.ReasonLeaseLost {
			out = append(out, v)
		}
	}
	return out
}

func newMemHarness(t *testing.T) *harness {
	t.Helper()
	mem := kernel.NewMemStore()
	clock := newTestClock()
	mem.SetClock(clock.Now)
	h := &harness{t: t, mem: mem, clock: clock, reg: NewMemRegistry()}
	h.tenant = uuid.New()
	h.session = uuid.New()
	h.scope = kernel.Scope{TenantID: h.tenant, SessionID: h.session}
	mem.CreateSession(h.tenant, h.session)
	mem.EnqueueRun(h.tenant, h.session)
	h.k = kernel.New(mem, kernel.Options{Audit: h.recordViolation})
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
	kpg, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatalf("open kernel pg store: %v", err)
	}
	reg, err := NewPGRegistry(ctx, st.Pool())
	if err != nil {
		t.Fatalf("open pg registry: %v", err)
	}
	h := &harness{t: t, st: st, pg: kpg, reg: reg}
	h.tenant = uuid.New()
	h.session = uuid.New()
	h.scope = kernel.Scope{TenantID: h.tenant, SessionID: h.session}
	if err := st.CreateTenant(ctx, h.tenant, "handoff-test"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, h.tenant, h.session); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueRun(ctx, h.tenant, h.session); err != nil {
		t.Fatal(err)
	}
	h.k = kernel.New(kpg, kernel.Options{Audit: h.recordViolation})
	return h
}

// newRuntime builds a coordinator over this harness.
func (h *harness) newRuntime(t *testing.T, runtimeID string, revision int64) *Coordinator {
	t.Helper()
	coord, err := New(Config{
		Identity: Identity{ID: runtimeID, LoadoutID: "keel-default", Revision: revision},
		Registry: h.reg,
		Kernel:   h.k,
		LeaseTTL: h.ttl(),
		Logger:   discardLogger(),
		Clock:    h.now,
	})
	if err != nil {
		t.Fatalf("new coordinator: %v", err)
	}
	return coord
}

// readyRuntime registers and readies a coordinator.
func (h *harness) readyRuntime(t *testing.T, runtimeID string, revision int64) *Coordinator {
	t.Helper()
	coord := h.newRuntime(t, runtimeID, revision)
	ctx := context.Background()
	if err := coord.Register(ctx); err != nil {
		t.Fatal(err)
	}
	if err := coord.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	return coord
}

func (h *harness) now() time.Time {
	if h.clock != nil {
		return h.clock.Now()
	}
	return time.Now()
}

// ttl is the lease ttl the tests use: long enough that a lease never lapses
// under -race (the kernel suite's convention), so a test that lets it lapse
// must claim with shortTTL.
func (h *harness) ttl() time.Duration { return 60 * time.Second }

// shortTTL is the lifetime for tests that let the lease expire for real.
func (h *harness) shortTTL() time.Duration {
	if h.mem != nil {
		return 30 * time.Minute
	}
	return 1200 * time.Millisecond
}

// expire waits (mem: advances the fake clock) until the lease is not live.
func (h *harness) expire(lease kernel.Lease) {
	h.t.Helper()
	if h.clock != nil {
		h.clock.advanceTo(lease.Until.Add(time.Millisecond))
		return
	}
	deadline := time.Now().Add(5 * time.Second)
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

// claim claims the harness session for one runtime and pins its loadout.
func (h *harness) claim(coord *Coordinator) kernel.Lease {
	h.t.Helper()
	return h.claimTTL(coord, h.ttl())
}

// claimShort claims a lease the test will let expire (PG: a bounded wall-clock
// TTL; mem: the fake clock is advanced by expire).
func (h *harness) claimShort(coord *Coordinator) kernel.Lease {
	h.t.Helper()
	return h.claimTTL(coord, h.shortTTL())
}

func (h *harness) claimTTL(coord *Coordinator, ttl time.Duration) kernel.Lease {
	h.t.Helper()
	lease, err := h.k.ClaimScope(context.Background(), h.scope, coord.RuntimeID(), ttl)
	if err != nil {
		h.t.Fatalf("claim %s: %v", coord.RuntimeID(), err)
	}
	if err := h.k.PinRunLoadout(context.Background(), lease, coord.LoadoutID(), coord.LoadoutRevision()); err != nil {
		h.t.Fatalf("pin loadout: %v", err)
	}
	return lease
}

// commit commits a request and fails the test on error.
func (h *harness) commit(req kernel.CommitRequest) kernel.Receipt {
	h.t.Helper()
	receipt, err := h.k.Commit(context.Background(), req)
	if err != nil {
		h.t.Fatalf("commit %s: %v", req.Kind, err)
	}
	return receipt
}

// commitErr commits and returns the error.
func (h *harness) commitErr(req kernel.CommitRequest) error {
	h.t.Helper()
	_, err := h.k.Commit(context.Background(), req)
	return err
}

// runStatus reads the run row's status.
func (h *harness) runStatus() string {
	h.t.Helper()
	pin, err := h.k.RunPin(context.Background(), h.scope)
	if err != nil {
		h.t.Fatalf("run pin: %v", err)
	}
	return pin.Status
}

// events reads the whole log.
func (h *harness) events() []kernel.Event {
	h.t.Helper()
	var all []kernel.Event
	var after int64
	for {
		page, err := h.k.LoadEvents(context.Background(), h.scope, after, 500)
		if err != nil {
			h.t.Fatalf("load events: %v", err)
		}
		if len(page) == 0 {
			break
		}
		all = append(all, page...)
		after = page[len(page)-1].Seq
		if len(page) < 500 {
			break
		}
	}
	return all
}

// countKind counts committed events of one kind.
func (h *harness) countKind(kind kernel.EventKind) int {
	n := 0
	for _, ev := range h.events() {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

// assertContiguous fails when the log has a hole (a lost event) or a gap in
// the sequence allocation.
func (h *harness) assertContiguous() {
	h.t.Helper()
	events := h.events()
	for i, ev := range events {
		want := int64(i + 1)
		if ev.Seq != want {
			h.t.Fatalf("event log is not contiguous: position %d has seq %d, want %d (%d events)", i, ev.Seq, want, len(events))
		}
	}
}

// assertCompleteMessages fails when any assistant payload is not a complete,
// parseable message document (the "0 half messages" assertion).
func (h *harness) assertCompleteMessages() {
	h.t.Helper()
	for _, ev := range h.events() {
		if ev.Kind != kernel.KindAssistantMessage {
			continue
		}
		var msg struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
				ID   string `json:"id"`
			} `json:"content"`
		}
		if err := json.Unmarshal(ev.Payload, &msg); err != nil {
			h.t.Fatalf("assistant event %d is not a complete message: %v", ev.Seq, err)
		}
		if len(msg.Content) == 0 {
			h.t.Fatalf("assistant event %d has no content: %s", ev.Seq, ev.Payload)
		}
		for _, block := range msg.Content {
			if block.Type == "text" && block.Text == "" {
				h.t.Fatalf("assistant event %d carries an empty text block: %s", ev.Seq, ev.Payload)
			}
			if block.Type == "toolCall" && block.ID == "" {
				h.t.Fatalf("assistant event %d carries an unidentified tool call: %s", ev.Seq, ev.Payload)
			}
		}
	}
}

// assertRunConverges asserts the run's final state is one of the legal
// outcomes, with a reason the report can explain.
func (h *harness) assertRunConverges(status string) {
	h.t.Helper()
	switch status {
	case kernel.RunStatusDone, kernel.RunStatusPending, kernel.RunStatusFailed:
		return
	}
	h.t.Fatalf("run did not converge: status = %q", status)
}

// payload helpers, matching the kernel's pairing contract.
func userPayload(text string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"text": text})
	return raw
}

func assistantText(text string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"role":    "assistant",
		"content": []map[string]any{{"type": "text", "text": text}},
	})
	return raw
}

func assistantToolCall(id, name string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"role":    "assistant",
		"content": []map[string]any{{"type": "toolCall", "id": id, "name": name, "arguments": map[string]any{}}},
	})
	return raw
}

func toolResultPayload(id, name, text string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"toolCallId": id,
		"toolName":   name,
		"content":    []map[string]any{{"type": "text", "text": text}},
	})
	return raw
}

// adopt runs the successor side and fails the test on error.
func (h *harness) adopt(coord *Coordinator, wait time.Duration) Adoption {
	h.t.Helper()
	adoption, err := coord.Adopt(context.Background(), h.scope, wait)
	if err != nil {
		h.t.Fatalf("adopt by %s: %v", coord.RuntimeID(), err)
	}
	return adoption
}

// finish settles the claimed run at the given cursor, with the truthful
// read-through watermark equal to the cursor (a run that read everything it
// produced).
func (h *harness) finish(lease kernel.Lease, cursor int64, runErr error) {
	h.t.Helper()
	h.finishWith(lease, cursor, cursor, runErr)
}

// finishWith settles a claimed run with an explicit read-through watermark.
func (h *harness) finishWith(lease kernel.Lease, cursor, readThrough int64, runErr error) {
	h.t.Helper()
	if err := h.k.Finish(context.Background(), kernel.FinishRequest{
		Lease: lease, Cursor: cursor, ReadThrough: readThrough, Err: runErr, MaxAttempts: 3,
	}); err != nil {
		h.t.Fatalf("finish: %v", err)
	}
}

// recordRead simulates a real attempt recording its load point right after
// loading the log (the runner does this before the first model call).
func (h *harness) recordRead(lease kernel.Lease, watermark int64) {
	h.t.Helper()
	if err := h.k.RecordRead(context.Background(), lease, watermark); err != nil {
		h.t.Fatalf("record read: %v", err)
	}
}

// unread reports whether the run row still carries unread edge input.
func (h *harness) unread() bool {
	h.t.Helper()
	pin, err := h.k.RunPin(context.Background(), h.scope)
	if err != nil {
		h.t.Fatalf("run pin: %v", err)
	}
	return pin.PendingInput
}

// lastSeq is the current tail of the log.
func (h *harness) lastSeq() int64 {
	h.t.Helper()
	events := h.events()
	if len(events) == 0 {
		return 0
	}
	return events[len(events)-1].Seq
}

// drainWindowTime is the deadline tests hand to BeginDrain.
func drainWindowTime() time.Duration { return 5 * time.Second }

// mustIso fails when err does not wrap the sentinel.
func (h *harness) mustIso(err error, sentinel error, what string) {
	h.t.Helper()
	if !errors.Is(err, sentinel) {
		h.t.Fatalf("%s = %v, want %v", what, err, sentinel)
	}
}

// handoffRow fetches one audit row.
func (h *harness) handoffRow(id string) HandoffRecord {
	h.t.Helper()
	rec, err := h.reg.Handoff(context.Background(), id)
	if err != nil {
		h.t.Fatalf("handoff row %s: %v", id, err)
	}
	return rec
}

// sessionHandoffs lists the adoption rows of the harness session.
func (h *harness) sessionHandoffs() []HandoffRecord {
	h.t.Helper()
	rows, err := h.reg.ListHandoffs(context.Background(), HandoffQuery{TenantID: h.tenant, SessionID: h.session})
	if err != nil {
		h.t.Fatalf("list handoffs: %v", err)
	}
	return rows
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
