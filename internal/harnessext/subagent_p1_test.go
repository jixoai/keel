package harnessext_test

// P1 review guards for keel-collab-subagent: the Host seam is one-shot, the
// exported launcher cannot widen depth, refusals are audited, a subtree cap is
// inherited, a detached spawn is budget-supervised, cancels check the result and
// persist state, and a ghost link cannot poison an idempotent retry.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/kernel"
)

// memLinkStore is an in-memory SubagentLinkStore + state writer + cleaner +
// active counter. It mirrors the production adapter's semantics.
type memLinkStore struct {
	mu      sync.Mutex
	links   map[uuid.UUID]harnessext.SubagentLink
	states  map[uuid.UUID]string
	deleted []uuid.UUID
	active  map[uuid.UUID]int
}

func newMemLinkStore() *memLinkStore {
	return &memLinkStore{
		links:  make(map[uuid.UUID]harnessext.SubagentLink),
		states: make(map[uuid.UUID]string),
		active: make(map[uuid.UUID]int),
	}
}

func (m *memLinkStore) RecordSubagentLink(_ context.Context, link harnessext.SubagentLink) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.links[link.SessionID] = link
	return nil
}

func (m *memLinkStore) SubagentLink(_ context.Context, _, childID uuid.UUID) (harnessext.SubagentLink, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	link, ok := m.links[childID]
	return link, ok, nil
}

func (m *memLinkStore) SubagentChildren(_ context.Context, _, parentID uuid.UUID) ([]harnessext.SubagentLink, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]harnessext.SubagentLink, 0)
	for _, link := range m.links {
		if link.ParentSessionID == parentID {
			out = append(out, link)
		}
	}
	return out, nil
}

func (m *memLinkStore) SetSubagentState(_ context.Context, _, childID uuid.UUID, state string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[childID] = state
	return nil
}

func (m *memLinkStore) DeleteSubagentLink(_ context.Context, _, childID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.links, childID)
	m.deleted = append(m.deleted, childID)
	return nil
}

func (m *memLinkStore) ActiveSubagentChildren(_ context.Context, _, parentID uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active[parentID], nil
}

func (m *memLinkStore) stateOf(childID uuid.UUID) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.states[childID]
}

// noRunReader is the "the input never committed" run projection.
type noRunReader struct{}

func (noRunReader) RunState(context.Context, uuid.UUID, uuid.UUID) (harnessext.RunState, error) {
	return harnessext.RunState{}, harnessext.ErrNoRun
}

// runningRuns keeps a run live for the budget supervisor.
type runningRuns struct{}

func (runningRuns) RunState(context.Context, uuid.UUID, uuid.UUID) (harnessext.RunState, error) {
	return harnessext.RunState{Status: "running"}, nil
}

func TestSubagentSetHostIsOneShot(t *testing.T) {
	h, host, _ := newHostBackedHarness(t, allowAllPermissions{}, 8)
	if err := h.runner.SetHost(nil); !errors.Is(err, harnessext.ErrSpawnHostAlreadyBound) {
		t.Fatalf("SetHost(nil) = %v, want ErrSpawnHostAlreadyBound", err)
	}
	if err := h.runner.SetHost(host); !errors.Is(err, harnessext.ErrSpawnHostAlreadyBound) {
		t.Fatalf("SetHost(replace) = %v, want ErrSpawnHostAlreadyBound", err)
	}
	// The original gate is still the one in force.
	record := h.spawn(t, h.root, harnessext.SpawnRequest{})
	if record.Depth != 1 {
		t.Fatalf("spawn after refused SetHost = depth %d, want 1", record.Depth)
	}
}

// TestSubagentLauncherCannotWidenDepth calls the exported launcher directly
// with a forged depth/cap and proves the unguarded path still refuses.
func TestSubagentLauncherCannotWidenDepth(t *testing.T) {
	h := newDepthHarness(t, intPtr(3))
	child := h.spawn(t, h.root, harnessext.SpawnRequest{})
	childID := uuid.MustParse(child.SessionID)
	launcher := harnessext.CollabSubagentLauncher{
		Runner: func() *harnessext.SubagentRunner { return h.runner },
	}
	before := h.gate.count()
	_, err := launcher.Launch(context.Background(), collab.LaunchRequest{
		TenantID: h.tenant, ParentSessionID: childID, Task: "forged depth", Depth: 99,
	})
	if !errors.Is(err, harnessext.ErrSubagentDepthExceeded) {
		t.Fatalf("forged depth = %v, want ErrSubagentDepthExceeded", err)
	}
	if h.gate.count() != before {
		t.Fatalf("a forged launch created %d sessions", h.gate.count()-before)
	}
	// A forged effective cap wider than the durable parent allows is refused.
	_, err = launcher.Launch(context.Background(), collab.LaunchRequest{
		TenantID: h.tenant, ParentSessionID: childID, Task: "forged cap", Depth: 2,
		Inputs: json.RawMessage(`{"effectiveMaxDepth":9}`),
	})
	if !errors.Is(err, harnessext.ErrSubagentMaxDepthWidened) {
		t.Fatalf("forged cap = %v, want ErrSubagentMaxDepthWidened", err)
	}
	if h.gate.count() != before {
		t.Fatalf("a forged cap launch created %d sessions", h.gate.count()-before)
	}
}

func TestSubagentLauncherDefensiveBotInvoke(t *testing.T) {
	h := newDepthHarness(t, intPtr(3), withBotInvoker(func(context.Context, uuid.UUID, uuid.UUID, string) error {
		return errors.New("no bot.invoke")
	}))
	child := h.spawn(t, h.root, harnessext.SpawnRequest{})
	launcher := harnessext.CollabSubagentLauncher{
		Runner: func() *harnessext.SubagentRunner { return h.runner },
	}
	before := h.gate.count()
	_, err := launcher.Launch(context.Background(), collab.LaunchRequest{
		TenantID: h.tenant, ParentSessionID: uuid.MustParse(child.SessionID), Task: "denied",
		BotID: "other-bot", Depth: 2,
	})
	if !errors.Is(err, collab.ErrPermissionDenied) {
		t.Fatalf("direct launch bot.invoke = %v, want permission denied", err)
	}
	if h.gate.count() != before {
		t.Fatalf("a denied direct launch created %d sessions", h.gate.count()-before)
	}
}

// TestSubagentRefusalAudit covers every pre-Host refusal: 0/3/-1, widening,
// parent missing and an old link. The row names parent/depth/maxDepth/mode and
// never the task text.
func TestSubagentRefusalAudit(t *testing.T) {
	t.Run("forbidden", func(t *testing.T) {
		audit := &recordingAudit{}
		h := newDepthHarness(t, intPtr(0), withAudit(audit))
		_, err := h.runner.SpawnAsync(context.Background(), harnessext.SpawnRequest{
			TenantID: h.tenant, ParentSessionID: h.root, Task: "secret task body",
		})
		if !errors.Is(err, harnessext.ErrSubagentDepthExceeded) {
			t.Fatalf("forbidden spawn = %v", err)
		}
		event := audit.last(t)
		if event.Kind != harnessext.AuditSubagentSpawnRefused || event.Result != "denied" ||
			event.Reason != harnessext.CodeSubagentDepthExceeded ||
			event.ParentSessionID != h.root || event.ModeID != collab.ModeSubagent ||
			event.Actual != 1 || event.Limit != 0 {
			t.Fatalf("audit event = %+v", event)
		}
		if strings.Contains(event.Detail, "secret") {
			t.Fatalf("audit detail carries task text: %q", event.Detail)
		}
	})

	t.Run("default three", func(t *testing.T) {
		audit := &recordingAudit{}
		h := newDepthHarness(t, nil, withAudit(audit))
		parent := h.root
		for depth := 1; depth <= 3; depth++ {
			record := h.spawn(t, parent, harnessext.SpawnRequest{})
			parent = uuid.MustParse(record.SessionID)
		}
		_, err := h.runner.SpawnAsync(context.Background(), harnessext.SpawnRequest{
			TenantID: h.tenant, ParentSessionID: parent, Task: "too deep",
		})
		if !errors.Is(err, harnessext.ErrSubagentDepthExceeded) {
			t.Fatalf("depth-4 spawn = %v", err)
		}
		event := audit.last(t)
		if event.Actual != 4 || event.Limit != 3 || event.Reason != harnessext.CodeSubagentDepthExceeded {
			t.Fatalf("audit event = %+v", event)
		}
	})

	t.Run("unlimited no refusal", func(t *testing.T) {
		audit := &recordingAudit{}
		h := newDepthHarness(t, intPtr(-1), withAudit(audit))
		parent := h.root
		for depth := 1; depth <= 5; depth++ {
			record := h.spawn(t, parent, harnessext.SpawnRequest{})
			parent = uuid.MustParse(record.SessionID)
		}
		if audit.has(harnessext.AuditSubagentSpawnRefused) {
			t.Fatalf("unlimited spawn audited a refusal: %+v", audit.events)
		}
	})

	t.Run("widening", func(t *testing.T) {
		audit := &recordingAudit{}
		h := newDepthHarness(t, intPtr(3), withAudit(audit))
		_, err := h.runner.SpawnAsync(context.Background(), harnessext.SpawnRequest{
			TenantID: h.tenant, ParentSessionID: h.root, Task: "wider", MaxDepth: intPtr(4),
		})
		if !errors.Is(err, harnessext.ErrSubagentMaxDepthWidened) {
			t.Fatalf("widening spawn = %v", err)
		}
		event := audit.last(t)
		if event.Reason != harnessext.CodeSubagentMaxDepthWidened || event.Limit != 3 || event.Actual != 4 {
			t.Fatalf("widening audit = %+v", event)
		}
	})

	t.Run("parent missing", func(t *testing.T) {
		audit := &recordingAudit{}
		h := newDepthHarness(t, intPtr(3), withAudit(audit))
		missing := uuid.New()
		_, err := h.runner.SpawnAsync(context.Background(), harnessext.SpawnRequest{
			TenantID: h.tenant, ParentSessionID: missing, Task: "orphan",
		})
		if !errors.Is(err, harnessext.ErrParentSessionNotFound) {
			t.Fatalf("missing parent = %v", err)
		}
		event := audit.last(t)
		if event.Reason != harnessext.CodeSubagentParentNotFound || event.ParentSessionID != missing {
			t.Fatalf("parent audit = %+v", event)
		}
	})

	t.Run("old link", func(t *testing.T) {
		audit := &recordingAudit{}
		h := newDepthHarness(t, intPtr(3), withAudit(audit))
		old := uuid.New()
		if err := h.gate.CreateSession(context.Background(), h.tenant, old); err != nil {
			t.Fatal(err)
		}
		commitLink(t, h.kern, h.tenant, old, map[string]any{
			"subagent": map[string]any{"parentSessionId": h.root.String(), "origin": "spawn"},
		})
		_, err := h.runner.SpawnAsync(context.Background(), harnessext.SpawnRequest{
			TenantID: h.tenant, ParentSessionID: old, Task: "legacy",
		})
		if !errors.Is(err, harnessext.ErrSubagentLinkInvalid) {
			t.Fatalf("old link = %v", err)
		}
		event := audit.last(t)
		if event.Reason != harnessext.CodeSubagentLinkInvalid {
			t.Fatalf("old-link audit = %+v", event)
		}
	})
}

// TestSubagentSubtreeMaxDepthInherited is P1-4: a parent may narrow the cap for
// its whole subtree, and a descendant that does not repeat maxDepth still
// inherits it (also from a cold runner via the durable link).
func TestSubagentSubtreeMaxDepthInherited(t *testing.T) {
	h := newDepthHarness(t, intPtr(3))
	child := h.spawn(t, h.root, harnessext.SpawnRequest{MaxDepth: intPtr(1)})
	childID := uuid.MustParse(child.SessionID)
	if child.MaxDepth != 1 {
		t.Fatalf("child MaxDepth = %d, want 1", child.MaxDepth)
	}
	link, ok, err := harnessext.DurableSubagentLink(context.Background(), h.kern, h.tenant, childID)
	if err != nil || !ok || link.MaxDepth != 1 {
		t.Fatalf("durable link = %+v ok=%v err=%v", link, ok, err)
	}
	// The grandchild does not pass maxDepth: the parent's cap 1 still refuses
	// depth 2.
	_, err = h.runner.SpawnAsync(context.Background(), harnessext.SpawnRequest{
		TenantID: h.tenant, ParentSessionID: childID, Task: "grandchild",
	})
	if !errors.Is(err, harnessext.ErrSubagentDepthExceeded) {
		t.Fatalf("grandchild without maxDepth = %v, want depth refusal", err)
	}
	// A cold runner reads the same cap from the ledger.
	cold := harnessext.NewSubagentRunner(harnessext.SubagentOptions{
		Gate: h.gate, Edge: h.kern, Events: h.kern, Runs: doneRuns{}, Logger: testLogger(),
		MaxDepth: intPtr(3), ValidateSelection: func(json.RawMessage) error { return nil },
	})
	_, err = cold.SpawnAsync(context.Background(), harnessext.SpawnRequest{
		TenantID: h.tenant, ParentSessionID: childID, Task: "cold grandchild",
	})
	if !errors.Is(err, harnessext.ErrSubagentDepthExceeded) {
		t.Fatalf("cold grandchild = %v, want depth refusal", err)
	}
	// Asking for 3 (wider than the parent cap 1) is a widening refusal.
	_, err = h.runner.SpawnAsync(context.Background(), harnessext.SpawnRequest{
		TenantID: h.tenant, ParentSessionID: childID, Task: "widen", MaxDepth: intPtr(3),
	})
	if !errors.Is(err, harnessext.ErrSubagentMaxDepthWidened) {
		t.Fatalf("widening under parent cap = %v", err)
	}
}

func TestSubagentBudgetRestoredOnColdRetry(t *testing.T) {
	h := newDepthHarness(t, intPtr(3))
	first := h.spawn(t, h.root, harnessext.SpawnRequest{
		IdempotencyKey: "budget-key",
		Budget:         harnessext.Budget{MaxTurns: 7, MaxTokens: 11, TimeoutMs: 9000},
	})
	cold := harnessext.NewSubagentRunner(harnessext.SubagentOptions{
		Gate: h.gate, Edge: h.kern, Events: h.kern, Runs: doneRuns{}, Logger: testLogger(),
		MaxDepth: intPtr(3), ValidateSelection: func(json.RawMessage) error { return nil },
	})
	recovered, err := cold.SpawnAsync(context.Background(), harnessext.SpawnRequest{
		TenantID: h.tenant, ParentSessionID: h.root, Task: "retry", IdempotencyKey: "budget-key",
	})
	if err != nil {
		t.Fatalf("cold retry: %v", err)
	}
	if recovered.SessionID != first.SessionID {
		t.Fatalf("cold retry session = %s, want %s", recovered.SessionID, first.SessionID)
	}
	if recovered.Budget == nil || recovered.Budget.MaxTurns != 7 || recovered.Budget.MaxTokens != 11 || recovered.Budget.TimeoutMs != 9000 {
		t.Fatalf("restored budget = %+v", recovered.Budget)
	}
}

// TestSubagentDetachedBudgetSupervisor proves a SpawnAsync (no Wait) still
// enforces its timeout: the watcher cancels the live local attempt and marks
// the record budget_exhausted.
func TestSubagentDetachedBudgetSupervisor(t *testing.T) {
	control := harnessext.NewRunControl(runningRuns{}, testLogger())
	h := newDepthHarness(t, intPtr(3),
		withControl(control), withRuns(runningRuns{}), withBudgetPoll(10*time.Millisecond))
	record := h.spawn(t, h.root, harnessext.SpawnRequest{
		Budget: harnessext.Budget{TimeoutMs: 60},
	})
	childID := uuid.MustParse(record.SessionID)
	runCtx, finish := control.BeginRun(context.Background(), kernel.Scope{
		TenantID: h.tenant, SessionID: childID,
	}, "budget-run")
	defer finish("cancelled")
	select {
	case <-runCtx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("detached budget supervisor did not cancel the run")
	}
	finish("cancelled")
	waitForCondition(t, 2*time.Second, func() bool {
		latest, ok := h.runner.Get(childID)
		return ok && latest.Status == "budget_exhausted"
	}, "budget_exhausted record status")
}

func TestSubagentGhostLinkDoesNotPoisonRetry(t *testing.T) {
	links := newMemLinkStore()
	h := newDepthHarness(t, intPtr(3), withLinks(links), withRuns(noRunReader{}))
	key := "ghost-key"
	ghostID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("keel.subagent.v1|"+
		h.tenant.String()+"|"+h.root.String()+"|"+key))
	if err := links.RecordSubagentLink(context.Background(), harnessext.SubagentLink{
		TenantID: h.tenant, SessionID: ghostID, ParentSessionID: h.root,
		ModeID: collab.ModeSubagent, Depth: 1, MaxDepth: 3, MaxChildren: 8,
	}); err != nil {
		t.Fatal(err)
	}
	before := h.gate.count()
	record, err := h.runner.SpawnAsync(context.Background(), harnessext.SpawnRequest{
		TenantID: h.tenant, ParentSessionID: h.root, Task: "retry", IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("ghost retry: %v", err)
	}
	if record.SessionID != ghostID.String() {
		t.Fatalf("retry session = %s, want the deterministic %s", record.SessionID, ghostID)
	}
	if h.gate.count() != before+1 {
		t.Fatalf("ghost retry created %d sessions", h.gate.count()-before)
	}
	links.mu.Lock()
	deleted := len(links.deleted)
	links.mu.Unlock()
	if deleted != 1 {
		t.Fatalf("ghost link deletes = %d, want 1", deleted)
	}
}

func TestSubagentCancelPersistsStateAndAudits(t *testing.T) {
	links := newMemLinkStore()
	audit := &recordingAudit{}
	control := harnessext.NewRunControlWithCanceller(pendingRuns{}, &recordingCanceller{}, testLogger())
	h := newDepthHarness(t, intPtr(3),
		withLinks(links), withAudit(audit), withControl(control), withRuns(pendingRuns{}))
	child := h.spawn(t, h.root, harnessext.SpawnRequest{})
	childID := uuid.MustParse(child.SessionID)
	result, err := h.runner.CancelSubtree(context.Background(), h.tenant, h.root, "operator stop")
	if err != nil {
		t.Fatalf("cancel subtree: %v", err)
	}
	if !result.Cancelled {
		t.Fatalf("cancel result = %+v", result)
	}
	if got := links.stateOf(childID); got != "cancelled" {
		t.Fatalf("child state = %q, want cancelled", got)
	}
	if got := links.stateOf(h.root); got != "cancelled" {
		t.Fatalf("root state = %q, want cancelled", got)
	}
	cancelledAudit := false
	for _, event := range audit.events {
		if event.Kind == harnessext.AuditSubagentCancelled && event.SessionID == childID {
			cancelledAudit = true
		}
	}
	if !cancelledAudit {
		t.Fatalf("child cancel audit missing: %+v", audit.events)
	}
}

// TestSubagentPluginSurfaceIsNarrow pins the compile-time guarantee: the plugin
// env holds SubagentAPI, whose method set has no SetHost/spawnChild/fork seam.
func TestSubagentPluginSurfaceIsNarrow(t *testing.T) {
	var api harnessext.SubagentAPI = harnessext.NewSubagentRunner(harnessext.SubagentOptions{})
	env := &harnessext.PluginEnv{Subagents: api}
	if env.Subagents == nil {
		t.Fatal("narrow plugin surface is nil")
	}
}

func (a *recordingAudit) last(t *testing.T) collab.AuditEvent {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.events) == 0 {
		t.Fatal("no audit events recorded")
	}
	return a.events[len(a.events)-1]
}

// failingCommitter fails the child input commit after the link was written.
type failingCommitter struct{}

func (failingCommitter) Commit(context.Context, kernel.CommitRequest) (kernel.Receipt, error) {
	return kernel.Receipt{}, errors.New("commit failed")
}
func (failingCommitter) Lookup(context.Context, kernel.Scope, kernel.KeyRef) (kernel.Receipt, bool, error) {
	return kernel.Receipt{}, false, nil
}

func TestSubagentInputFailureClearsGhostLink(t *testing.T) {
	links := newMemLinkStore()
	h := newDepthHarness(t, intPtr(3), withLinks(links))
	runner := harnessext.NewSubagentRunner(harnessext.SubagentOptions{
		Gate: h.gate, Edge: failingCommitter{}, Events: h.kern, Runs: doneRuns{},
		Logger: testLogger(), MaxDepth: intPtr(3), Links: links,
		ValidateSelection: func(json.RawMessage) error { return nil },
	})
	_, err := runner.SpawnAsync(context.Background(), harnessext.SpawnRequest{
		TenantID: h.tenant, ParentSessionID: h.root, Task: "fail", IdempotencyKey: "fail-key",
	})
	if err == nil {
		t.Fatal("spawn with a failing input must fail")
	}
	links.mu.Lock()
	deleted := append([]uuid.UUID(nil), links.deleted...)
	links.mu.Unlock()
	if len(deleted) != 1 {
		t.Fatalf("ghost link cleanup = %v, want one delete", deleted)
	}
}

func TestSubagentVerifyChild(t *testing.T) {
	h := newDepthHarness(t, intPtr(3), withLinks(newMemLinkStore()))
	child := h.spawn(t, h.root, harnessext.SpawnRequest{})
	childID := uuid.MustParse(child.SessionID)
	if err := h.runner.VerifyChild(context.Background(), h.tenant, h.root, childID); err != nil {
		t.Fatalf("VerifyChild: %v", err)
	}
	if err := h.runner.VerifyChild(context.Background(), h.tenant, uuid.New(), childID); !errors.Is(err, harnessext.ErrSubagentNotFound) {
		t.Fatalf("VerifyChild foreign parent = %v, want ErrSubagentNotFound", err)
	}
}
