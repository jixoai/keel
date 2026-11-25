package harnessext_test

// Sub-agent depth semantics (change keel-collab-subagent): the mode request
// layer interprets maxDepth=-1/0/positive, resolves the parent depth from the
// durable ledger link, refuses before any child session exists, and keeps forks
// outside the delegation tree. The Host integration test proves both entries
// (runner spawn and the launcher) share one creation gate.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/kernel"
)

// depthGate is the SessionGate over the in-memory kernel. It records every
// child session creation so a refusal can be proven to create nothing.
type depthGate struct {
	mem *kernel.MemStore
	mu  sync.Mutex
	// known holds every session the gate created/saw.
	known  map[uuid.UUID]bool
	create []uuid.UUID
}

func (g *depthGate) CreateSession(_ context.Context, tenantID, sessionID uuid.UUID) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.known == nil {
		g.known = make(map[uuid.UUID]bool)
	}
	g.known[sessionID] = true
	g.create = append(g.create, sessionID)
	g.mem.CreateSession(tenantID, sessionID)
	return nil
}

func (g *depthGate) SessionOwner(_ context.Context, _ uuid.UUID, sessionID uuid.UUID) (string, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return "owner", g.known[sessionID], nil
}

func (g *depthGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.create)
}

// doneRuns is the run-row projection: an immediate terminal run keeps the
// depth tests from waiting on a worker.
type doneRuns struct{}

func (doneRuns) RunState(context.Context, uuid.UUID, uuid.UUID) (harnessext.RunState, error) {
	return harnessext.RunState{Status: "done"}, nil
}

type depthHarness struct {
	tenant uuid.UUID
	root   uuid.UUID
	gate   *depthGate
	kern   *kernel.Kernel
	mem    *kernel.MemStore
	runner *harnessext.SubagentRunner
}

func newDepthHarness(t *testing.T, maxDepth *int) *depthHarness {
	t.Helper()
	mem := kernel.NewMemStore()
	kern := kernel.New(mem, kernel.Options{})
	gate := &depthGate{mem: mem}
	tenant, root := uuid.New(), uuid.New()
	if err := gate.CreateSession(context.Background(), tenant, root); err != nil {
		t.Fatal(err)
	}
	runner := harnessext.NewSubagentRunner(harnessext.SubagentOptions{
		Gate:     gate,
		Edge:     kern,
		Events:   kern,
		Runs:     doneRuns{},
		Logger:   testLogger(),
		MaxDepth: maxDepth,
	})
	return &depthHarness{tenant: tenant, root: root, gate: gate, kern: kern, mem: mem, runner: runner}
}

func (h *depthHarness) spawn(t *testing.T, parent uuid.UUID, extra harnessext.SpawnRequest) harnessext.SubagentRecord {
	t.Helper()
	extra.TenantID = h.tenant
	extra.ParentSessionID = parent
	if extra.Task == "" {
		extra.Task = "do the work"
	}
	record, err := h.runner.SpawnAsync(context.Background(), extra)
	if err != nil {
		t.Fatalf("spawn under %s: %v", parent, err)
	}
	return record
}

func intPtr(value int) *int { return &value }

// TestSubagentDepthDefaultThreeLevels pins the mode default: main=0, spawns at
// depth 1..3 run, and the depth-3 spawn of a depth-4 child is refused before a
// session exists.
func TestSubagentDepthDefaultThreeLevels(t *testing.T) {
	h := newDepthHarness(t, nil)
	level1 := h.spawn(t, h.root, harnessext.SpawnRequest{})
	if level1.Depth != 1 {
		t.Fatalf("level1 depth = %d, want 1", level1.Depth)
	}
	level2 := h.spawn(t, uuid.MustParse(level1.SessionID), harnessext.SpawnRequest{})
	if level2.Depth != 2 {
		t.Fatalf("level2 depth = %d, want 2", level2.Depth)
	}
	level3 := h.spawn(t, uuid.MustParse(level2.SessionID), harnessext.SpawnRequest{})
	if level3.Depth != 3 {
		t.Fatalf("level3 depth = %d, want 3", level3.Depth)
	}
	before := h.gate.count()
	_, err := h.runner.SpawnAsync(context.Background(), harnessext.SpawnRequest{
		TenantID: h.tenant, ParentSessionID: uuid.MustParse(level3.SessionID), Task: "too deep",
	})
	if !errors.Is(err, harnessext.ErrSubagentDepthExceeded) {
		t.Fatalf("depth-4 spawn = %v, want ErrSubagentDepthExceeded", err)
	}
	var depthErr *harnessext.SubagentDepthError
	if !errors.As(err, &depthErr) || depthErr.Depth != 4 || depthErr.MaxDepth != 3 {
		t.Fatalf("depth refusal = %+v", depthErr)
	}
	if h.gate.count() != before {
		t.Fatalf("a refused spawn created %d sessions", h.gate.count()-before)
	}
}

func TestSubagentDepthUnlimitedAndForbidden(t *testing.T) {
	unlimited := newDepthHarness(t, intPtr(harnessext.SubagentDepthUnlimited))
	parent := unlimited.root
	for depth := 1; depth <= 6; depth++ {
		record := unlimited.spawn(t, parent, harnessext.SpawnRequest{})
		if record.Depth != depth {
			t.Fatalf("unlimited chain depth = %d, want %d", record.Depth, depth)
		}
		parent = uuid.MustParse(record.SessionID)
	}

	forbidden := newDepthHarness(t, intPtr(harnessext.SubagentDepthForbidden))
	before := forbidden.gate.count()
	_, err := forbidden.runner.SpawnAsync(context.Background(), harnessext.SpawnRequest{
		TenantID: forbidden.tenant, ParentSessionID: forbidden.root, Task: "nope",
	})
	if !errors.Is(err, harnessext.ErrSubagentDepthExceeded) {
		t.Fatalf("maxDepth=0 spawn = %v, want ErrSubagentDepthExceeded", err)
	}
	if forbidden.gate.count() != before {
		t.Fatalf("maxDepth=0 created %d sessions", forbidden.gate.count()-before)
	}
}

func TestSubagentDepthRequestNarrowing(t *testing.T) {
	h := newDepthHarness(t, intPtr(3))
	narrowed := h.spawn(t, h.root, harnessext.SpawnRequest{MaxDepth: intPtr(1)})
	if narrowed.Depth != 1 {
		t.Fatalf("narrowed depth = %d, want 1", narrowed.Depth)
	}
	// The child request narrowed the whole subtree to 1; its own child would be
	// depth 2 and must be refused.
	_, err := h.runner.SpawnAsync(context.Background(), harnessext.SpawnRequest{
		TenantID: h.tenant, ParentSessionID: uuid.MustParse(narrowed.SessionID),
		Task: "grandchild", MaxDepth: intPtr(1),
	})
	if !errors.Is(err, harnessext.ErrSubagentDepthExceeded) {
		t.Fatalf("narrowed grandchild = %v, want depth refusal", err)
	}
	// A request may not widen the deployment cap, and -1 is not a narrowing.
	for _, invalid := range []int{4, -1} {
		_, err := h.runner.SpawnAsync(context.Background(), harnessext.SpawnRequest{
			TenantID: h.tenant, ParentSessionID: h.root, Task: "wider", MaxDepth: intPtr(invalid),
		})
		if !errors.Is(err, harnessext.ErrInvalidSpawn) {
			t.Fatalf("maxDepth=%d = %v, want ErrInvalidSpawn", invalid, err)
		}
	}
}

// TestSubagentDepthSurvivesRestart proves the depth is read from the child's
// first ledger event: a fresh runner (cold registry) on the same ledger still
// knows the child is depth 1.
func TestSubagentDepthSurvivesRestart(t *testing.T) {
	h := newDepthHarness(t, intPtr(3))
	child := h.spawn(t, h.root, harnessext.SpawnRequest{})
	childID := uuid.MustParse(child.SessionID)

	cold := harnessext.NewSubagentRunner(harnessext.SubagentOptions{
		Gate: h.gate, Edge: h.kern, Events: h.kern, Runs: doneRuns{}, Logger: testLogger(),
		MaxDepth: intPtr(1),
	})
	_, err := cold.SpawnAsync(context.Background(), harnessext.SpawnRequest{
		TenantID: h.tenant, ParentSessionID: childID, Task: "grandchild",
	})
	if !errors.Is(err, harnessext.ErrSubagentDepthExceeded) {
		t.Fatalf("cold runner depth-2 spawn = %v, want depth refusal", err)
	}
	wider := harnessext.NewSubagentRunner(harnessext.SubagentOptions{
		Gate: h.gate, Edge: h.kern, Events: h.kern, Runs: doneRuns{}, Logger: testLogger(),
		MaxDepth: intPtr(2),
	})
	grandchild, err := wider.SpawnAsync(context.Background(), harnessext.SpawnRequest{
		TenantID: h.tenant, ParentSessionID: childID, Task: "grandchild",
	})
	if err != nil {
		t.Fatalf("cold runner depth-2 spawn: %v", err)
	}
	if grandchild.Depth != 2 {
		t.Fatalf("cold recovery grandchild depth = %d, want 2", grandchild.Depth)
	}
}

// TestSubagentOldLinkFailsClosed: a pre-depth spawn link (parent but no depth)
// is refused instead of being guessed as a root.
func TestSubagentOldLinkFailsClosed(t *testing.T) {
	h := newDepthHarness(t, intPtr(3))
	old := uuid.New()
	if err := h.gate.CreateSession(context.Background(), h.tenant, old); err != nil {
		t.Fatal(err)
	}
	commitLink(t, h.kern, h.tenant, old, map[string]any{
		"subagent": map[string]any{
			"parentSessionId": h.root.String(),
			"origin":          "spawn",
		},
	})
	_, err := h.runner.SpawnAsync(context.Background(), harnessext.SpawnRequest{
		TenantID: h.tenant, ParentSessionID: old, Task: "legacy child",
	})
	if !errors.Is(err, harnessext.ErrSubagentLinkInvalid) {
		t.Fatalf("legacy link spawn = %v, want ErrSubagentLinkInvalid", err)
	}
}

// TestSubagentForkDoesNotConsumeDepth: a fork child is a ledger branch, not a
// delegation, so it stays at depth 0 for the depth guard.
func TestSubagentForkDoesNotConsumeDepth(t *testing.T) {
	h := newDepthHarness(t, intPtr(1))
	forked := uuid.New()
	if err := h.gate.CreateSession(context.Background(), h.tenant, forked); err != nil {
		t.Fatal(err)
	}
	commitLink(t, h.kern, h.tenant, forked, map[string]any{
		"subagent": map[string]any{
			"parentSessionId": h.root.String(),
			"origin":          "fork",
		},
		"fork": map[string]any{
			"parentSessionId": h.root.String(),
			"originSeq":       4,
		},
	})
	if link, ok, err := harnessext.DurableSubagentLink(context.Background(), h.kern, h.tenant, forked); err != nil || ok {
		t.Fatalf("fork durable link = %+v, ok=%v err=%v; want no delegation link", link, ok, err)
	}
	child := h.spawn(t, forked, harnessext.SpawnRequest{})
	if child.Depth != 1 {
		t.Fatalf("spawn under fork depth = %d, want 1 (fork is depth 0)", child.Depth)
	}
}

func commitLink(t *testing.T, kern *kernel.Kernel, tenant, session uuid.UUID, extras map[string]any) {
	t.Helper()
	payload := map[string]any{"text": "link", "requestId": "link-" + uuid.NewString()}
	for key, value := range extras {
		payload[key] = value
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kern.Commit(context.Background(), kernel.CommitRequest{
		Scope:     kernel.Scope{TenantID: tenant, SessionID: session},
		Kind:      kernel.KindUserMessage,
		RequestID: payload["requestId"].(string),
		Payload:   raw,
	}); err != nil {
		t.Fatalf("commit link: %v", err)
	}
}

// TestDurableSubagentLinkRoundTrip reads the spawn link a normal spawn wrote.
func TestDurableSubagentLinkRoundTrip(t *testing.T) {
	h := newDepthHarness(t, intPtr(3))
	child := h.spawn(t, h.root, harnessext.SpawnRequest{Agent: "reviewer"})
	link, ok, err := harnessext.DurableSubagentLink(context.Background(), h.kern, h.tenant, uuid.MustParse(child.SessionID))
	if err != nil || !ok {
		t.Fatalf("durable link ok=%v err=%v", ok, err)
	}
	if link.ParentSessionID != h.root || link.Depth != 1 || link.ModeID != collab.ModeSubagent {
		t.Fatalf("durable link = %+v", link)
	}
}

// --- guarded Host integration ---------------------------------------------

type allowAllPermissions struct{}

func (allowAllPermissions) CheckBotInvoke(context.Context, uuid.UUID, collab.SessionRef, string) error {
	return nil
}

type denyAllPermissions struct{}

func (denyAllPermissions) CheckBotInvoke(context.Context, uuid.UUID, collab.SessionRef, string) error {
	return collab.ErrPermissionDenied
}

type recordingAudit struct {
	mu     sync.Mutex
	events []collab.AuditEvent
}

func (a *recordingAudit) Record(_ context.Context, event collab.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, event)
	return nil
}

func (a *recordingAudit) has(kind string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, event := range a.events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

func newHostBackedHarness(t *testing.T, permissions collab.PermissionChecker, maxChildren int) (*depthHarness, *collab.Coordinator, *recordingAudit) {
	return newHostBackedHarnessWithAudit(t, permissions, maxChildren, nil)
}

func newHostBackedHarnessWithAudit(t *testing.T, permissions collab.PermissionChecker, maxChildren int, auditSink collab.AuditSink) (*depthHarness, *collab.Coordinator, *recordingAudit) {
	t.Helper()
	h := newDepthHarness(t, intPtr(3))
	modes, err := collab.NewModeRegistry(h.tenant, collab.Limits{Depth: collab.UnlimitedDepth(), MaxChildren: 32, MaxConcurrent: 64})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := collab.SubagentMode()
	descriptor.Limits.Depth = collab.UnlimitedDepth() // the runner owns the 3-level default
	descriptor.Limits.MaxChildren = maxChildren
	if err := modes.Register(descriptor); err != nil {
		t.Fatal(err)
	}
	index := collab.NewMemorySessionIndex()
	if err := index.Put(context.Background(), collab.SessionRef{
		TenantID: h.tenant, SessionID: h.root, BotID: "bot-parent",
		ModeID: collab.ModeSubagent, Depth: 0, State: collab.SessionActive,
	}); err != nil {
		t.Fatal(err)
	}
	audit := &recordingAudit{}
	sink := collab.AuditSink(audit)
	if auditSink != nil {
		sink = auditSink
	}
	host, err := collab.NewHost(collab.HostOptions{
		TenantID:    h.tenant,
		Modes:       modes,
		Work:        collab.NewMemoryWorkStore(nil),
		Sessions:    index,
		Permissions: permissions,
		Audit:       sink,
		Launcher: harnessext.CollabSubagentLauncher{
			Runner: func() *harnessext.SubagentRunner { return h.runner },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.runner.SetHost(host)
	return h, host, audit
}

func TestSubagentHostSharedGateSpawnAndAudit(t *testing.T) {
	h, _, audit := newHostBackedHarness(t, allowAllPermissions{}, 8)
	record := h.spawn(t, h.root, harnessext.SpawnRequest{})
	if record.Depth != 1 {
		t.Fatalf("host-created child depth = %d, want 1", record.Depth)
	}
	// The Host resolved the mode ceiling into the record Wait enforces.
	descriptor := collab.SubagentMode()
	if record.Budget == nil || record.Budget.MaxTurns != descriptor.Limits.MaxTurns {
		t.Fatalf("effective budget = %+v, want maxTurns %d", record.Budget, descriptor.Limits.MaxTurns)
	}
	if !audit.has(collab.AuditSessionCreating) || !audit.has(collab.AuditSessionCreated) {
		t.Fatalf("spawn audit events missing: %+v", audit.events)
	}
	for _, event := range audit.events {
		if event.Kind != collab.AuditSessionCreating && event.Kind != collab.AuditSessionCreated {
			continue
		}
		if event.ParentSessionID != h.root || event.Actual != 1 {
			t.Fatalf("spawn audit lost parent/depth: %+v", event)
		}
	}
}

func TestSubagentHostPermissionDenialCreatesNothing(t *testing.T) {
	h, _, _ := newHostBackedHarness(t, denyAllPermissions{}, 8)
	before := h.gate.count()
	_, err := h.runner.SpawnAsync(context.Background(), harnessext.SpawnRequest{
		TenantID: h.tenant, ParentSessionID: h.root, Task: "denied",
	})
	if !errors.Is(err, collab.ErrPermissionDenied) {
		t.Fatalf("host permission denial = %v, want collab.ErrPermissionDenied", err)
	}
	if h.gate.count() != before {
		t.Fatalf("a denied spawn created %d sessions", h.gate.count()-before)
	}
}

func TestSubagentHostWidthRefusalCreatesNothing(t *testing.T) {
	h, _, _ := newHostBackedHarness(t, allowAllPermissions{}, 1)
	first := h.spawn(t, h.root, harnessext.SpawnRequest{})
	if first.Depth != 1 {
		t.Fatalf("first child depth = %d", first.Depth)
	}
	before := h.gate.count()
	_, err := h.runner.SpawnAsync(context.Background(), harnessext.SpawnRequest{
		TenantID: h.tenant, ParentSessionID: h.root, Task: "second child",
	})
	if !errors.Is(err, harnessext.ErrSubagentChildrenExceeded) {
		t.Fatalf("second child = %v, want ErrSubagentChildrenExceeded", err)
	}
	if h.gate.count() != before {
		t.Fatalf("a width-refused spawn created %d sessions", h.gate.count()-before)
	}
}

func TestSubagentHostTenantMismatch(t *testing.T) {
	_, host, _ := newHostBackedHarness(t, allowAllPermissions{}, 8)
	_, err := host.CreateSession(context.Background(), uuid.New(), collab.ModeSubagent, collab.SessionRequest{
		ParentSessionID: uuid.New(), Task: "cross tenant",
	})
	if !collab.IsCode(err, collab.CodeTenantMismatch) {
		t.Fatalf("cross-tenant create = %v, want tenant_mismatch", err)
	}
}

// TestSubagentCancelTreePropagates walks a two-level tree and stops both
// children. The run rows are "pending" (no active local attempt), which the
// cancel semantics answer as an idempotent idle stop.
func TestSubagentCancelTreePropagates(t *testing.T) {
	h := newDepthHarness(t, intPtr(3))
	control := harnessext.NewRunControl(pendingRuns{}, testLogger())
	h.runner = harnessext.NewSubagentRunner(harnessext.SubagentOptions{
		Gate: h.gate, Edge: h.kern, Events: h.kern, Runs: doneRuns{}, Control: control,
		Logger: testLogger(), MaxDepth: intPtr(3),
	})
	child := h.spawn(t, h.root, harnessext.SpawnRequest{})
	grandchild := h.spawn(t, uuid.MustParse(child.SessionID), harnessext.SpawnRequest{})
	result, err := h.runner.CancelTree(context.Background(), h.tenant, h.root, "test")
	if err != nil {
		t.Fatalf("cancel tree: %v", err)
	}
	if len(result.Cancelled) != 2 || len(result.Failed) != 0 {
		t.Fatalf("cancel tree = %+v, want both descendants cancelled", result)
	}
	want := map[string]bool{child.SessionID: true, grandchild.SessionID: true}
	for _, id := range result.Cancelled {
		delete(want, id)
	}
	if len(want) != 0 {
		t.Fatalf("cancel tree missed %v", want)
	}
}

// pendingRuns answers "pending" so CancelTree sees a queued run (nothing to
// interrupt) instead of an unreachable local attempt.
type pendingRuns struct{}

func (pendingRuns) RunState(context.Context, uuid.UUID, uuid.UUID) (harnessext.RunState, error) {
	return harnessext.RunState{Status: "pending"}, nil
}

// failingAudit models an unavailable audit stream: the accepted creation path
// must fail closed, because an unauditable spawn is not allowed.
type failingAudit struct{}

func (failingAudit) Record(context.Context, collab.AuditEvent) error {
	return errors.New("audit stream unavailable")
}

func TestSubagentHostAuditFailureFailsClosed(t *testing.T) {
	h, _, _ := newHostBackedHarnessWithAudit(t, allowAllPermissions{}, 8, failingAudit{})
	before := h.gate.count()
	_, err := h.runner.SpawnAsync(context.Background(), harnessext.SpawnRequest{
		TenantID: h.tenant, ParentSessionID: h.root, Task: "unauditable",
	})
	if !errors.Is(err, collab.ErrAuditUnavailable) {
		t.Fatalf("audit failure = %v, want collab.ErrAuditUnavailable", err)
	}
	if h.gate.count() != before {
		t.Fatalf("an unauditable spawn created %d sessions", h.gate.count()-before)
	}
}

// TestSubagentIdempotentRetryReturnsSameChild is the P1-2 guard: a retried
// spawn key returns the first child, never session_exists/width_exceeded, and
// still converges when the registry is cold (the durable link answers).
func TestSubagentIdempotentRetryReturnsSameChild(t *testing.T) {
	h, _, _ := newHostBackedHarness(t, allowAllPermissions{}, 1)
	before := h.gate.count()
	first := h.spawn(t, h.root, harnessext.SpawnRequest{IdempotencyKey: "same-key"})
	second := h.spawn(t, h.root, harnessext.SpawnRequest{IdempotencyKey: "same-key"})
	if first.SessionID != second.SessionID {
		t.Fatalf("retry sessions = %s / %s, want one child", first.SessionID, second.SessionID)
	}
	if h.gate.count() != before+1 {
		t.Fatalf("retry created %d sessions", h.gate.count()-(before+1))
	}
	// Cold registry: the same key under a fresh runner still resolves the child
	// from the durable link and creates nothing.
	cold := harnessext.NewSubagentRunner(harnessext.SubagentOptions{
		Gate: h.gate, Edge: h.kern, Events: h.kern, Runs: doneRuns{}, Logger: testLogger(),
	})
	recovered, err := cold.SpawnAsync(context.Background(), harnessext.SpawnRequest{
		TenantID: h.tenant, ParentSessionID: h.root, Task: "retry", IdempotencyKey: "same-key",
	})
	if err != nil {
		t.Fatalf("cold retry: %v", err)
	}
	if recovered.SessionID != first.SessionID {
		t.Fatalf("cold retry session = %s, want %s", recovered.SessionID, first.SessionID)
	}
	if h.gate.count() != before+1 {
		t.Fatalf("cold retry created a second session")
	}
}

// TestSubagentHostConcurrentFanoutRespectsMaxChildren is the P1-7 guard: the
// check-then-act width guard is serialized by the runner's spawn gate, so a
// burst cannot all pass the same pre-check.
func TestSubagentHostConcurrentFanoutRespectsMaxChildren(t *testing.T) {
	h, _, _ := newHostBackedHarness(t, allowAllPermissions{}, 2)
	const attempts = 6
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
		refused int
		other   []error
	)
	for index := 0; index < attempts; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, err := h.runner.SpawnAsync(context.Background(), harnessext.SpawnRequest{
				TenantID: h.tenant, ParentSessionID: h.root, Task: "burst child",
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case errors.Is(err, harnessext.ErrSubagentChildrenExceeded):
				refused++
			default:
				other = append(other, err)
			}
		}(index)
	}
	wg.Wait()
	if len(other) > 0 {
		t.Fatalf("unexpected spawn errors: %v", other)
	}
	if created != 2 || refused != attempts-2 {
		t.Fatalf("concurrent fanout created=%d refused=%d, want 2/%d", created, refused, attempts-2)
	}
	if h.gate.count() != 3 {
		t.Fatalf("gate sessions = %d, want root+2", h.gate.count())
	}
}

// TestSubagentToolEntrySharesDepthGuard proves the `agent` tool entry (the
// model-facing parser) reaches the same guard as SpawnAsync: zero
// recordings and zero children on a maxDepth=0 deployment.
func TestSubagentToolEntrySharesDepthGuard(t *testing.T) {
	h := newDepthHarness(t, intPtr(harnessext.SubagentDepthForbidden))
	before := h.gate.count()
	ctx := harnessext.WithRunScope(context.Background(), harnessext.RunScope{
		TenantID: h.tenant, SessionID: h.root, RunID: "run-tool",
	})
	_, err := harnessext.SpawnFromTool(ctx, h.runner, json.RawMessage(`{"task":"tool spawn"}`))
	if !errors.Is(err, harnessext.ErrSubagentDepthExceeded) {
		t.Fatalf("tool spawn = %v, want ErrSubagentDepthExceeded", err)
	}
	if h.gate.count() != before {
		t.Fatalf("tool refusal created %d sessions", h.gate.count()-before)
	}
}
