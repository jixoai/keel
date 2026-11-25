package collab_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
)

type fakeLauncher struct {
	mu        sync.Mutex
	launches  []collab.LaunchRequest
	cancelled []uuid.UUID
	launchErr error
	cancelErr error
}

func (f *fakeLauncher) Launch(_ context.Context, req collab.LaunchRequest) (collab.SessionRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launches = append(f.launches, req)
	if f.launchErr != nil {
		return collab.SessionRef{}, f.launchErr
	}
	return collab.SessionRef{
		TenantID:        req.TenantID,
		SessionID:       uuid.New(),
		ParentSessionID: req.ParentSessionID,
		BotID:           req.BotID,
		ModeID:          req.ModeID,
		WorkItemID:      req.WorkItemID,
		Depth:           req.Depth,
		State:           collab.SessionActive,
		CreatedAt:       time.Now().UTC(),
	}, nil
}

func (f *fakeLauncher) CancelSession(_ context.Context, _, sessionID uuid.UUID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, sessionID)
	return f.cancelErr
}

func (f *fakeLauncher) launchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.launches)
}

func (f *fakeLauncher) lastLaunch() collab.LaunchRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.launches) == 0 {
		return collab.LaunchRequest{}
	}
	return f.launches[len(f.launches)-1]
}

func (f *fakeLauncher) cancelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cancelled)
}

func (f *fakeLauncher) cancelledSet() map[uuid.UUID]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[uuid.UUID]bool, len(f.cancelled))
	for _, id := range f.cancelled {
		out[id] = true
	}
	return out
}

type fakePermissions struct {
	mu         sync.Mutex
	deny       bool
	err        error
	calls      int
	lastTarget string
	lastCaller uuid.UUID
}

func (p *fakePermissions) CheckBotInvoke(_ context.Context, _ uuid.UUID, caller collab.SessionRef, targetBotID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.lastTarget = targetBotID
	p.lastCaller = caller.SessionID
	if p.err != nil {
		return p.err
	}
	if p.deny {
		return errors.New("faked denial")
	}
	return nil
}

type fakeOperators struct {
	mu    sync.Mutex
	allow bool
	err   error
	calls int
}

func (o *fakeOperators) CheckOperator(_ context.Context, _ uuid.UUID, _ collab.SessionRef) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	if o.err != nil {
		return o.err
	}
	if !o.allow {
		return errors.New("faked operator denial")
	}
	return nil
}

type fakeAudit struct {
	mu     sync.Mutex
	events []collab.AuditEvent
	err    error
}

func (a *fakeAudit) Record(_ context.Context, event collab.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.events = append(a.events, event)
	return nil
}

func (a *fakeAudit) find(kind string) (collab.AuditEvent, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.events) - 1; i >= 0; i-- {
		if a.events[i].Kind == kind {
			return a.events[i], true
		}
	}
	return collab.AuditEvent{}, false
}

func (a *fakeAudit) count(kind string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, event := range a.events {
		if event.Kind == kind {
			n++
		}
	}
	return n
}

type fakePublisher struct {
	mu        sync.Mutex
	published []uuid.UUID
	err       error
}

func (p *fakePublisher) Publish(_ context.Context, attention collab.Attention) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.published = append(p.published, attention.ID)
	return nil
}

func (p *fakePublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.published)
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// flakySessionIndex fails the next Put(s) to exercise a partial creation
// failure: the launcher succeeded but the session index did not.
type flakySessionIndex struct {
	collab.SessionIndex
	mu       sync.Mutex
	failPuts int
}

func (f *flakySessionIndex) Put(ctx context.Context, ref collab.SessionRef) error {
	f.mu.Lock()
	if f.failPuts > 0 {
		f.failPuts--
		f.mu.Unlock()
		return errors.New("session index unavailable")
	}
	f.mu.Unlock()
	return f.SessionIndex.Put(ctx, ref)
}

type testEnv struct {
	tenant      uuid.UUID
	registry    *collab.ModeRegistry
	work        *collab.MemoryWorkStore
	sessions    collab.SessionIndex
	launcher    *fakeLauncher
	permissions *fakePermissions
	operators   *fakeOperators
	audit       *fakeAudit
	publisher   *fakePublisher
	host        *collab.Coordinator
	parent      collab.SessionRef
	clock       *testClock
}

func defaultCeiling() collab.Limits {
	return collab.Limits{
		Depth: collab.LimitedDepth(8), MaxChildren: 8, MaxConcurrent: 8,
		MaxTurns: 100, MaxTokens: 100_000, MaxDurationMs: 600_000,
	}
}

func defaultModeLimits() collab.Limits {
	return collab.Limits{
		Depth: collab.LimitedDepth(4), MaxChildren: 4, MaxConcurrent: 4,
		MaxTurns: 10, MaxTokens: 1_000, MaxDurationMs: 60_000,
	}
}

func newTestEnv(t *testing.T, ceiling, modeLimits collab.Limits) *testEnv {
	t.Helper()
	return newTestEnvWith(t, ceiling, modeLimits, nil)
}

func newTestEnvWith(t *testing.T, ceiling, modeLimits collab.Limits, wrap func(*collab.MemorySessionIndex) collab.SessionIndex) *testEnv {
	t.Helper()
	ctx := context.Background()
	tenant := uuid.New()
	registry, err := collab.NewModeRegistry(tenant, ceiling)
	if err != nil {
		t.Fatalf("NewModeRegistry: %v", err)
	}
	if err := registry.Register(testMode(modeLimits)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	clock := newTestClock()
	work := collab.NewMemoryWorkStore(clock.Now)
	memory := collab.NewMemorySessionIndex()
	launcher := &fakeLauncher{}
	permissions := &fakePermissions{}
	operators := &fakeOperators{}
	audit := &fakeAudit{}
	publisher := &fakePublisher{}
	parent := collab.SessionRef{
		TenantID: tenant, SessionID: uuid.New(), BotID: "captain",
		Depth: 0, State: collab.SessionActive, CreatedAt: clock.Now(),
	}
	if err := memory.Put(ctx, parent); err != nil {
		t.Fatalf("put parent: %v", err)
	}
	var sessions collab.SessionIndex = memory
	if wrap != nil {
		sessions = wrap(memory)
	}
	host, err := collab.NewHost(collab.HostOptions{
		TenantID:    tenant,
		Modes:       registry,
		Work:        work,
		Sessions:    sessions,
		Permissions: permissions,
		Operators:   operators,
		Audit:       audit,
		Launcher:    launcher,
		Attention:   publisher,
		Now:         clock.Now,
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	return &testEnv{
		tenant: tenant, registry: registry, work: work, sessions: sessions,
		launcher: launcher, permissions: permissions, operators: operators,
		audit: audit, publisher: publisher, host: host, parent: parent, clock: clock,
	}
}

func (e *testEnv) openWork(t *testing.T, title string) collab.WorkItem {
	t.Helper()
	return e.openWorkWithScope(t, title, collab.Scope{})
}

func (e *testEnv) openWorkWithScope(t *testing.T, title string, scope collab.Scope) collab.WorkItem {
	t.Helper()
	item, err := e.host.OpenWorkItem(context.Background(), e.tenant, "test.mode", collab.WorkItemRequest{
		CallerSessionID: e.parent.SessionID,
		Title:           title,
		Source:          collab.SourceRef{Kind: "manual"},
		Scope:           scope,
	})
	if err != nil {
		t.Fatalf("OpenWorkItem: %v", err)
	}
	return item
}

func (e *testEnv) assign(t *testing.T, workItemID uuid.UUID, botID string) collab.Assignment {
	t.Helper()
	assignment, err := e.host.Assign(context.Background(), e.tenant, workItemID, botID, collab.AssignmentRequest{
		CallerSessionID: e.parent.SessionID,
	})
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	return assignment
}

func (e *testEnv) addSession(t *testing.T, depth int, parentID uuid.UUID, state collab.SessionState) collab.SessionRef {
	t.Helper()
	ref := collab.SessionRef{
		TenantID: e.tenant, SessionID: uuid.New(), ParentSessionID: parentID,
		BotID: "captain", Depth: depth, State: state, CreatedAt: time.Now().UTC(),
	}
	if err := e.sessions.Put(context.Background(), ref); err != nil {
		t.Fatalf("put session: %v", err)
	}
	return ref
}

func (e *testEnv) createSession(t *testing.T, req collab.SessionRequest) collab.SessionRef {
	t.Helper()
	if req.ParentSessionID == uuid.Nil {
		req.ParentSessionID = e.parent.SessionID
	}
	if req.Task == "" {
		req.Task = "do the work"
	}
	ref, err := e.host.CreateSession(context.Background(), e.tenant, "test.mode", req)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return ref
}

func TestCreateSessionHappyPath(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket 1")
	assignment := env.assign(t, item.ID, "worker")
	ref := env.createSession(t, collab.SessionRequest{
		WorkItemID:     item.ID,
		BotID:          "worker",
		Task:           "fix it",
		AssignmentID:   assignment.ID,
		IdempotencyKey: "spawn-1",
		Budget:         collab.Budget{MaxTurns: 3},
	})
	if ref.Depth != 1 {
		t.Fatalf("depth = %d, want 1", ref.Depth)
	}
	if ref.ParentSessionID != env.parent.SessionID || ref.BotID != "worker" ||
		ref.ModeID != "test.mode" || ref.WorkItemID != item.ID || ref.State != collab.SessionActive {
		t.Fatalf("session ref = %+v", ref)
	}
	launch := env.launcher.lastLaunch()
	if launch.ParentSessionID != env.parent.SessionID || launch.BotID != "worker" ||
		launch.IdempotencyKey != "spawn-1" || launch.Depth != 1 {
		t.Fatalf("launch request = %+v", launch)
	}
	if launch.Budget.MaxTurns != 3 {
		t.Fatalf("launch budget MaxTurns = %d, want 3", launch.Budget.MaxTurns)
	}
	if launch.Budget.TimeoutMs != 60_000 {
		t.Fatalf("launch budget TimeoutMs = %d, want inherited 60000", launch.Budget.TimeoutMs)
	}
	if env.permissions.lastTarget != "worker" || env.permissions.lastCaller != env.parent.SessionID {
		t.Fatalf("permission check = %q/%s", env.permissions.lastTarget, env.permissions.lastCaller)
	}
	stored, err := env.work.GetAssignment(context.Background(), env.tenant, assignment.ID)
	if err != nil {
		t.Fatalf("GetAssignment: %v", err)
	}
	if stored.Status != collab.AssignmentActive || stored.SessionID != ref.SessionID {
		t.Fatalf("bound assignment = %+v", stored)
	}
	workItem, err := env.host.WorkItem(context.Background(), env.tenant, item.ID)
	if err != nil {
		t.Fatalf("WorkItem: %v", err)
	}
	if workItem.Status != collab.WorkItemActive {
		t.Fatalf("work item status = %s, want active", workItem.Status)
	}
	if env.audit.count(collab.AuditSessionCreating) != 1 || env.audit.count(collab.AuditSessionCreated) != 1 {
		t.Fatalf("creation audits: creating=%d created=%d", env.audit.count(collab.AuditSessionCreating), env.audit.count(collab.AuditSessionCreated))
	}
}

func TestCreateSessionRejectsCrossTenant(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	other := uuid.New()
	_, err := env.host.CreateSession(context.Background(), other, "test.mode", collab.SessionRequest{
		ParentSessionID: env.parent.SessionID,
		Task:            "steal",
	})
	if !errors.Is(err, collab.ErrTenantMismatch) {
		t.Fatalf("CreateSession = %v, want ErrTenantMismatch", err)
	}
	if env.launcher.launchCount() != 0 {
		t.Fatalf("cross-tenant request launched %d sessions", env.launcher.launchCount())
	}
	event, ok := env.audit.find(collab.AuditInvalidCreation)
	if !ok || event.Result != "denied" || event.Reason != string(collab.CodeTenantMismatch) {
		t.Fatalf("cross-tenant audit = %+v, ok=%v", event, ok)
	}
}

func TestCreateSessionRejectsMissingPermission(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	env.permissions.deny = true
	_, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		ParentSessionID: env.parent.SessionID,
		BotID:           "worker",
		Task:            "fix it",
	})
	if !errors.Is(err, collab.ErrPermissionDenied) {
		t.Fatalf("CreateSession = %v, want ErrPermissionDenied", err)
	}
	if env.launcher.launchCount() != 0 {
		t.Fatalf("denied request launched a session")
	}
	event, ok := env.audit.find(collab.AuditPermissionDenied)
	if !ok || event.Result != "denied" || event.Reason != string(collab.CodePermissionDenied) {
		t.Fatalf("permission audit = %+v, ok=%v", event, ok)
	}
	if event.BotID != "worker" {
		t.Fatalf("permission audit bot = %q", event.BotID)
	}
}

func TestDepthPolicyApplies(t *testing.T) {
	t.Run("limited", func(t *testing.T) {
		limits := defaultModeLimits()
		limits.Depth = collab.LimitedDepth(2)
		env := newTestEnv(t, defaultCeiling(), limits)
		deep := env.addSession(t, 2, uuid.Nil, collab.SessionActive)
		_, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
			ParentSessionID: deep.SessionID,
			BotID:           "worker",
			Task:            "too deep",
		})
		if !errors.Is(err, collab.ErrDepthExceeded) {
			t.Fatalf("CreateSession = %v, want ErrDepthExceeded", err)
		}
		shallow := env.addSession(t, 1, uuid.Nil, collab.SessionActive)
		if _, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
			ParentSessionID: shallow.SessionID,
			BotID:           "worker",
			Task:            "fits",
		}); err != nil {
			t.Fatalf("depth-2 child refused: %v", err)
		}
	})
	t.Run("forbidden", func(t *testing.T) {
		limits := defaultModeLimits()
		limits.Depth = collab.ForbiddenDepth()
		env := newTestEnv(t, defaultCeiling(), limits)
		_, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
			ParentSessionID: env.parent.SessionID,
			BotID:           "worker",
			Task:            "no children",
		})
		if !errors.Is(err, collab.ErrDepthExceeded) {
			t.Fatalf("CreateSession = %v, want ErrDepthExceeded", err)
		}
	})
	t.Run("unlimited", func(t *testing.T) {
		limits := defaultModeLimits()
		limits.Depth = collab.UnlimitedDepth()
		ceiling := defaultCeiling()
		ceiling.Depth = collab.UnlimitedDepth()
		env := newTestEnv(t, ceiling, limits)
		deep := env.addSession(t, 100, uuid.Nil, collab.SessionActive)
		ref, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
			ParentSessionID: deep.SessionID,
			BotID:           "worker",
			Task:            "deep but allowed",
		})
		if err != nil {
			t.Fatalf("CreateSession unlimited: %v", err)
		}
		if ref.Depth != 101 {
			t.Fatalf("depth = %d, want 101", ref.Depth)
		}
	})
}

func TestCreateSessionRejectsWidth(t *testing.T) {
	limits := defaultModeLimits()
	limits.MaxChildren = 1
	limits.MaxConcurrent = 8
	env := newTestEnv(t, defaultCeiling(), limits)
	env.createSession(t, collab.SessionRequest{BotID: "worker"})
	_, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		ParentSessionID: env.parent.SessionID,
		BotID:           "worker",
		Task:            "one too many",
	})
	if !errors.Is(err, collab.ErrWidthExceeded) {
		t.Fatalf("CreateSession = %v, want ErrWidthExceeded", err)
	}
}

func TestCreateSessionRejectsConcurrency(t *testing.T) {
	limits := defaultModeLimits()
	limits.MaxConcurrent = 1
	limits.MaxChildren = 8
	env := newTestEnv(t, defaultCeiling(), limits)
	first := env.createSession(t, collab.SessionRequest{BotID: "worker"})
	otherParent := env.addSession(t, 0, uuid.Nil, collab.SessionActive)
	_, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		ParentSessionID: otherParent.SessionID,
		BotID:           "worker",
		Task:            "blocked",
	})
	if !errors.Is(err, collab.ErrConcurrencyExceeded) {
		t.Fatalf("CreateSession = %v, want ErrConcurrencyExceeded", err)
	}
	if err := env.sessions.SetState(context.Background(), env.tenant, first.SessionID, collab.SessionDone); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if _, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		ParentSessionID: otherParent.SessionID,
		BotID:           "worker",
		Task:            "slot released",
	}); err != nil {
		t.Fatalf("CreateSession after release: %v", err)
	}
}

func TestCreateSessionIdempotentRetry(t *testing.T) {
	limits := defaultModeLimits()
	limits.MaxChildren = 1
	limits.MaxConcurrent = 1
	env := newTestEnv(t, defaultCeiling(), limits)
	item := env.openWork(t, "ticket")
	assignment := env.assign(t, item.ID, "worker")
	req := collab.SessionRequest{
		WorkItemID:      item.ID,
		ParentSessionID: env.parent.SessionID,
		BotID:           "worker",
		Task:            "retry me",
		AssignmentID:    assignment.ID,
		IdempotencyKey:  "same-key",
	}
	first := env.createSession(t, req)
	retry, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", req)
	if err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if retry.SessionID != first.SessionID {
		t.Fatalf("retry session = %s, want %s", retry.SessionID, first.SessionID)
	}
	if env.launcher.launchCount() != 1 {
		t.Fatalf("retry launched %d sessions, want 1", env.launcher.launchCount())
	}
	// A different key is new work and hits the (now full) width limit.
	assignment2 := env.assign(t, item.ID, "worker")
	other := req
	other.AssignmentID = assignment2.ID
	other.IdempotencyKey = "another-key"
	if _, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", other); !errors.Is(err, collab.ErrWidthExceeded) {
		t.Fatalf("new key = %v, want ErrWidthExceeded", err)
	}
}

func TestCreateSessionIdempotentPartialFailure(t *testing.T) {
	env := newTestEnvWith(t, defaultCeiling(), defaultModeLimits(), func(memory *collab.MemorySessionIndex) collab.SessionIndex {
		return &flakySessionIndex{SessionIndex: memory, failPuts: 1}
	})
	item := env.openWork(t, "ticket")
	assignment := env.assign(t, item.ID, "worker")
	req := collab.SessionRequest{
		WorkItemID:      item.ID,
		ParentSessionID: env.parent.SessionID,
		BotID:           "worker",
		Task:            "resume after index failure",
		AssignmentID:    assignment.ID,
		IdempotencyKey:  "partial-key",
	}
	if _, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", req); err == nil {
		t.Fatal("first CreateSession unexpectedly succeeded")
	}
	if env.launcher.launchCount() != 1 {
		t.Fatalf("first attempt launched %d sessions", env.launcher.launchCount())
	}
	retry, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", req)
	if err != nil {
		t.Fatalf("retry after partial failure: %v", err)
	}
	if env.launcher.launchCount() != 1 {
		t.Fatalf("retry launched again: %d", env.launcher.launchCount())
	}
	if _, err := env.sessions.Session(context.Background(), env.tenant, retry.SessionID); err != nil {
		t.Fatalf("retried session is not indexed: %v", err)
	}
	stored, err := env.work.GetAssignment(context.Background(), env.tenant, assignment.ID)
	if err != nil {
		t.Fatalf("GetAssignment: %v", err)
	}
	if stored.SessionID != retry.SessionID || stored.Status != collab.AssignmentActive {
		t.Fatalf("assignment after retry = %+v", stored)
	}
}

func TestConcurrentCreateRespectsWidth(t *testing.T) {
	limits := defaultModeLimits()
	limits.MaxChildren = 2
	limits.MaxConcurrent = 100
	env := newTestEnv(t, defaultCeiling(), limits)
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes, widthDenials := 0, 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
				ParentSessionID: env.parent.SessionID,
				BotID:           "worker",
				Task:            fmt.Sprintf("parallel %d", i),
				IdempotencyKey:  fmt.Sprintf("parallel-%d", i),
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, collab.ErrWidthExceeded):
				widthDenials++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if successes != 2 || widthDenials != 10 {
		t.Fatalf("successes=%d widthDenials=%d, want 2/10", successes, widthDenials)
	}
}

func TestConcurrentCreateRespectsConcurrency(t *testing.T) {
	limits := defaultModeLimits()
	limits.MaxChildren = 100
	limits.MaxConcurrent = 2
	env := newTestEnv(t, defaultCeiling(), limits)
	parents := []uuid.UUID{env.parent.SessionID, env.addSession(t, 0, uuid.Nil, collab.SessionActive).SessionID}
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes, concurrencyDenials := 0, 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
				ParentSessionID: parents[i%len(parents)],
				BotID:           "worker",
				Task:            fmt.Sprintf("parallel %d", i),
				IdempotencyKey:  fmt.Sprintf("parallel-%d", i),
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, collab.ErrConcurrencyExceeded):
				concurrencyDenials++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if successes != 2 || concurrencyDenials != 10 {
		t.Fatalf("successes=%d concurrencyDenials=%d, want 2/10", successes, concurrencyDenials)
	}
}

func TestCreateSessionRejectsOverBudgetAndNarrowsTenantCeiling(t *testing.T) {
	limits := defaultModeLimits()
	limits.MaxTurns = 5
	env := newTestEnv(t, defaultCeiling(), limits)
	_, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		ParentSessionID: env.parent.SessionID,
		BotID:           "worker",
		Task:            "too big",
		Budget:          collab.Budget{MaxTurns: 6},
	})
	if !errors.Is(err, collab.ErrBudgetExceeded) {
		t.Fatalf("CreateSession = %v, want ErrBudgetExceeded", err)
	}
	if collab.CodeOf(err) != collab.CodeBudgetExceeded {
		t.Fatalf("code = %s", collab.CodeOf(err))
	}
	if env.launcher.launchCount() != 0 {
		t.Fatal("over-budget request launched")
	}

	ceiling := defaultCeiling()
	ceiling.MaxTurns = 3
	modeLimits := defaultModeLimits()
	modeLimits.MaxTurns = 10
	narrow := newTestEnv(t, ceiling, modeLimits)
	if _, err := narrow.host.CreateSession(context.Background(), narrow.tenant, "test.mode", collab.SessionRequest{
		ParentSessionID: narrow.parent.SessionID,
		BotID:           "worker",
		Task:            "tenant cap",
		Budget:          collab.Budget{MaxTurns: 4},
	}); !errors.Is(err, collab.ErrBudgetExceeded) {
		t.Fatalf("tenant-capped request = %v, want ErrBudgetExceeded", err)
	}
	ref, err := narrow.host.CreateSession(context.Background(), narrow.tenant, "test.mode", collab.SessionRequest{
		ParentSessionID: narrow.parent.SessionID,
		BotID:           "worker",
		Task:            "tenant cap",
		Budget:          collab.Budget{MaxTurns: 3},
	})
	if err != nil {
		t.Fatalf("tenant-capped allowed request: %v", err)
	}
	if ref.SessionID == uuid.Nil {
		t.Fatal("missing session id")
	}
	if launch := narrow.launcher.lastLaunch(); launch.Budget.MaxTurns != 3 {
		t.Fatalf("launch budget = %d, want 3", launch.Budget.MaxTurns)
	}
}

func TestCreateSessionRejectsBadParentAndTask(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	if _, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		ParentSessionID: uuid.New(),
		BotID:           "worker",
		Task:            "x",
	}); !errors.Is(err, collab.ErrParentSessionNotFound) {
		t.Fatalf("missing parent = %v, want ErrParentSessionNotFound", err)
	}
	inactive := env.addSession(t, 0, uuid.Nil, collab.SessionDone)
	if _, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		ParentSessionID: inactive.SessionID,
		BotID:           "worker",
		Task:            "x",
	}); !errors.Is(err, collab.ErrParentSessionInactive) {
		t.Fatalf("inactive parent = %v, want ErrParentSessionInactive", err)
	}
	if _, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		ParentSessionID: env.parent.SessionID,
		BotID:           "worker",
	}); !errors.Is(err, collab.ErrInvalidCreation) {
		t.Fatalf("empty task = %v, want ErrInvalidCreation", err)
	}
	if _, err := env.host.CreateSession(context.Background(), env.tenant, "missing", collab.SessionRequest{
		ParentSessionID: env.parent.SessionID,
		Task:            "x",
	}); !errors.Is(err, collab.ErrUnknownMode) {
		t.Fatalf("unknown mode = %v, want ErrUnknownMode", err)
	}
}

func TestWorkItemSessionRequiresAssignment(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	_, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		WorkItemID:      item.ID,
		ParentSessionID: env.parent.SessionID,
		BotID:           "worker",
		Task:            "unbound",
	})
	if !errors.Is(err, collab.ErrInvalidCreation) {
		t.Fatalf("unbound work session = %v, want ErrInvalidCreation", err)
	}
}

func TestCreateSessionRejectsMismatchedWorkItemMode(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	other := testMode(defaultModeLimits())
	other.ID = "other.mode"
	if err := env.registry.Register(other); err != nil {
		t.Fatalf("register other mode: %v", err)
	}
	otherWork, err := env.host.OpenWorkItem(context.Background(), env.tenant, "other.mode", collab.WorkItemRequest{
		CallerSessionID: env.parent.SessionID, Title: "other",
	})
	if err != nil {
		t.Fatalf("OpenWorkItem(other): %v", err)
	}
	otherAssignment, err := env.host.Assign(context.Background(), env.tenant, otherWork.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID,
	})
	if err != nil {
		t.Fatalf("Assign(other): %v", err)
	}
	_, err = env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		WorkItemID:      otherWork.ID,
		ParentSessionID: env.parent.SessionID,
		BotID:           "worker",
		Task:            "wrong mode",
		AssignmentID:    otherAssignment.ID,
	})
	if !errors.Is(err, collab.ErrInvalidCreation) {
		t.Fatalf("mismatched work item = %v, want ErrInvalidCreation", err)
	}
}

func TestWorkItemMembership(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	first := env.openWork(t, "first")
	second := env.openWork(t, "second")
	bare := env.createSession(t, collab.SessionRequest{BotID: "worker"})
	if _, err := env.host.Assign(context.Background(), env.tenant, first.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: bare.SessionID,
	}); !errors.Is(err, collab.ErrPermissionDenied) {
		t.Fatalf("bare-child Assign = %v, want ErrPermissionDenied", err)
	}
	if _, err := env.host.Ask(context.Background(), env.tenant, first.ID, collab.AttentionRequest{
		CallerSessionID: bare.SessionID,
		Prompt:          "sneak in",
		AllowFreeText:   true,
	}); !errors.Is(err, collab.ErrPermissionDenied) {
		t.Fatalf("bare-child Ask = %v, want ErrPermissionDenied", err)
	}
	firstAssignment := env.assign(t, first.ID, "worker")
	worker := env.createSession(t, collab.SessionRequest{
		WorkItemID: first.ID, BotID: "worker", AssignmentID: firstAssignment.ID,
	})
	if _, err := env.host.Assign(context.Background(), env.tenant, second.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: worker.SessionID,
	}); !errors.Is(err, collab.ErrPermissionDenied) {
		t.Fatalf("cross-work Assign = %v, want ErrPermissionDenied", err)
	}
	if _, err := env.host.Ask(context.Background(), env.tenant, first.ID, collab.AttentionRequest{
		CallerSessionID: worker.SessionID,
		AssignmentID:    firstAssignment.ID,
		Prompt:          "worker asks",
		AllowFreeText:   true,
	}); err != nil {
		t.Fatalf("worker Ask on its own work item: %v", err)
	}
	if _, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		WorkItemID: first.ID, ParentSessionID: bare.SessionID, BotID: "worker",
		Task: "borrowed membership", AssignmentID: firstAssignment.ID,
	}); !errors.Is(err, collab.ErrPermissionDenied) {
		t.Fatalf("bare-parent work session = %v, want ErrPermissionDenied", err)
	}
}

func TestScopeBotIDs(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWorkWithScope(t, "scoped", collab.Scope{BotIDs: []string{"worker"}})
	if _, err := env.host.Assign(context.Background(), env.tenant, item.ID, "other", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID,
	}); !errors.Is(err, collab.ErrPermissionDenied) {
		t.Fatalf("out-of-scope Assign = %v, want ErrPermissionDenied", err)
	}
	assignment := env.assign(t, item.ID, "worker")
	if _, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		WorkItemID: item.ID, ParentSessionID: env.parent.SessionID, BotID: "other",
		Task: "out of scope", AssignmentID: assignment.ID,
	}); !errors.Is(err, collab.ErrPermissionDenied) {
		t.Fatalf("out-of-scope CreateSession = %v, want ErrPermissionDenied", err)
	}
	if _, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		WorkItemID: item.ID, ParentSessionID: env.parent.SessionID, BotID: "worker",
		Task: "in scope", AssignmentID: assignment.ID,
	}); err != nil {
		t.Fatalf("in-scope CreateSession: %v", err)
	}
}

func TestDisabledModeKeepsExistingWorkAndNeedsOperator(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "existing ticket")
	assignment := env.assign(t, item.ID, "worker")
	if err := env.registry.SetEnabled("test.mode", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := env.host.OpenWorkItem(context.Background(), env.tenant, "test.mode", collab.WorkItemRequest{
		CallerSessionID: env.parent.SessionID, Title: "new",
	}); !errors.Is(err, collab.ErrModeDisabled) {
		t.Fatalf("OpenWorkItem while disabled = %v, want ErrModeDisabled", err)
	}
	stored, err := env.host.WorkItem(context.Background(), env.tenant, item.ID)
	if err != nil || stored.ID != item.ID {
		t.Fatalf("existing work unreadable after disable: %+v, %v", stored, err)
	}
	assignments, err := env.host.Assignments(context.Background(), env.tenant, item.ID)
	if err != nil || len(assignments) != 1 || assignments[0].ID != assignment.ID {
		t.Fatalf("Assignments after disable = %+v, %v", assignments, err)
	}
	if _, err := env.host.Assign(context.Background(), env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID,
	}); !errors.Is(err, collab.ErrPermissionDenied) {
		t.Fatalf("Assign while disabled without operator = %v, want ErrPermissionDenied", err)
	}
	env.operators.allow = true
	if _, err := env.host.Assign(context.Background(), env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID,
	}); err != nil {
		t.Fatalf("operator takeover assignment: %v", err)
	}
	// An operator session that is not yet a work-item member may take over.
	operatorSession := env.addSession(t, 0, uuid.Nil, collab.SessionActive)
	if _, err := env.host.Assign(context.Background(), env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: operatorSession.SessionID,
	}); err != nil {
		t.Fatalf("non-member operator takeover: %v", err)
	}
	if _, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		ParentSessionID: env.parent.SessionID,
		BotID:           "worker",
		Task:            "new work while disabled",
	}); !errors.Is(err, collab.ErrModeDisabled) {
		t.Fatalf("CreateSession while disabled = %v, want ErrModeDisabled", err)
	}
	if err := env.host.Cancel(context.Background(), env.tenant, item.ID, "operator"); err != nil {
		t.Fatalf("Cancel disabled work: %v", err)
	}
}

func TestAssignmentLifecycleIndependentOfSession(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	assignment1 := env.assign(t, item.ID, "worker")
	session1 := env.createSession(t, collab.SessionRequest{
		WorkItemID: item.ID, BotID: "worker", AssignmentID: assignment1.ID,
	})
	if err := env.sessions.SetState(context.Background(), env.tenant, session1.SessionID, collab.SessionDone); err != nil {
		t.Fatalf("end session 1: %v", err)
	}
	assignment2 := env.assign(t, item.ID, "worker")
	assignments, err := env.host.Assignments(context.Background(), env.tenant, item.ID)
	if err != nil || len(assignments) != 2 {
		t.Fatalf("Assignments = %+v, %v", assignments, err)
	}
	byID := map[uuid.UUID]collab.Assignment{}
	for _, assignment := range assignments {
		byID[assignment.ID] = assignment
	}
	first := byID[assignment1.ID]
	if first.SessionID != session1.SessionID || first.Status != collab.AssignmentActive {
		t.Fatalf("assignment 1 lost its binding: %+v", first)
	}
	if byID[assignment2.ID].SessionID != uuid.Nil || byID[assignment2.ID].Status != collab.AssignmentOpen {
		t.Fatalf("assignment 2 = %+v", byID[assignment2.ID])
	}
	session2 := env.createSession(t, collab.SessionRequest{
		WorkItemID: item.ID, BotID: "worker", AssignmentID: assignment2.ID,
	})
	if session2.SessionID == session1.SessionID {
		t.Fatal("replacement worker reused the old session")
	}
}

func TestCreateSessionRejectsParentFromAnotherWorkItem(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	first := env.openWork(t, "first")
	second := env.openWork(t, "second")
	firstAssignment := env.assign(t, first.ID, "worker")
	child := env.createSession(t, collab.SessionRequest{
		WorkItemID: first.ID, BotID: "worker", AssignmentID: firstAssignment.ID,
	})
	secondAssignment := env.assign(t, second.ID, "worker")
	_, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		WorkItemID:      second.ID,
		ParentSessionID: child.SessionID,
		BotID:           "worker",
		Task:            "borrowed authority",
		AssignmentID:    secondAssignment.ID,
	})
	if !errors.Is(err, collab.ErrPermissionDenied) {
		t.Fatalf("cross-work parent = %v, want ErrPermissionDenied", err)
	}
}

func TestAskStructuredAttention(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	assignment := env.assign(t, item.ID, "worker")
	attention, err := env.host.Ask(context.Background(), env.tenant, item.ID, collab.AttentionRequest{
		CallerSessionID: env.parent.SessionID,
		AssignmentID:    assignment.ID,
		RequestID:       "ask-1",
		Prompt:          "Which option?",
		Options:         []collab.AttentionOption{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}},
		Route:           collab.AttentionRouteHuman,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if attention.Status != collab.AttentionOpen || attention.Route != collab.AttentionRouteHuman {
		t.Fatalf("attention = %+v", attention)
	}
	if attention.WorkItemID != item.ID || attention.AssignmentID != assignment.ID ||
		attention.SessionID != env.parent.SessionID || attention.ModeID != "test.mode" {
		t.Fatalf("attention identity = %+v", attention)
	}
	if deadline := attention.Deadline.Sub(attention.CreatedAt); deadline != collab.DefaultAttentionTimeout {
		t.Fatalf("deadline = %v, want %v", deadline, collab.DefaultAttentionTimeout)
	}
	if env.publisher.count() != 1 {
		t.Fatalf("published %d attentions, want 1", env.publisher.count())
	}
	again, err := env.host.Ask(context.Background(), env.tenant, item.ID, collab.AttentionRequest{
		CallerSessionID: env.parent.SessionID,
		AssignmentID:    assignment.ID,
		RequestID:       "ask-1",
		Prompt:          "Which option?",
		Options:         []collab.AttentionOption{{ID: "a", Label: "A"}},
	})
	if err != nil {
		t.Fatalf("Ask retry: %v", err)
	}
	if again.ID != attention.ID || env.publisher.count() != 1 {
		t.Fatalf("idempotent ask duplicated the attention: %+v %d", again, env.publisher.count())
	}
	attentions, err := env.host.Attentions(context.Background(), env.tenant, item.ID)
	if err != nil || len(attentions) != 1 {
		t.Fatalf("Attentions = %+v, %v", attentions, err)
	}
	clamped, err := env.host.Ask(context.Background(), env.tenant, item.ID, collab.AttentionRequest{
		CallerSessionID: env.parent.SessionID,
		Prompt:          "free text",
		AllowFreeText:   true,
		Route:           collab.AttentionRouteHuman,
		Timeout:         48 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Ask clamped: %v", err)
	}
	if deadline := clamped.Deadline.Sub(clamped.CreatedAt); deadline != collab.MaxAttentionTimeout {
		t.Fatalf("clamped deadline = %v, want %v", deadline, collab.MaxAttentionTimeout)
	}
}

func TestAskRejectsMalformedAttention(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	cases := map[string]collab.AttentionRequest{
		"missing prompt": {
			CallerSessionID: env.parent.SessionID,
			Options:         []collab.AttentionOption{{ID: "a"}},
		},
		"no answer surface": {
			CallerSessionID: env.parent.SessionID,
			Prompt:          "pick",
		},
		"duplicate option": {
			CallerSessionID: env.parent.SessionID,
			Prompt:          "pick",
			Options:         []collab.AttentionOption{{ID: "a"}, {ID: "a"}},
		},
		"channel without channel id": {
			CallerSessionID: env.parent.SessionID,
			Prompt:          "pick",
			AllowFreeText:   true,
			Route:           collab.AttentionRouteChannel,
		},
		"negative timeout": {
			CallerSessionID: env.parent.SessionID,
			Prompt:          "pick",
			AllowFreeText:   true,
			Timeout:         -time.Second,
		},
		"unknown caller": {
			CallerSessionID: uuid.New(),
			Prompt:          "pick",
			AllowFreeText:   true,
		},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := env.host.Ask(context.Background(), env.tenant, item.ID, req); !errors.Is(err, collab.ErrInvalidRequest) && !errors.Is(err, collab.ErrSessionNotFound) {
				t.Fatalf("Ask = %v, want a typed refusal", err)
			}
		})
	}
	attention, err := env.host.Ask(context.Background(), env.tenant, item.ID, collab.AttentionRequest{
		CallerSessionID: env.parent.SessionID,
		Prompt:          "free text",
		AllowFreeText:   true,
	})
	if err != nil {
		t.Fatalf("Ask default route: %v", err)
	}
	if attention.Route != collab.AttentionRouteHuman {
		t.Fatalf("default route = %s", attention.Route)
	}
}

func TestAskPublishFailureIsVisible(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	env.publisher.err = errors.New("broker down")
	item := env.openWork(t, "ticket")
	_, err := env.host.Ask(context.Background(), env.tenant, item.ID, collab.AttentionRequest{
		CallerSessionID: env.parent.SessionID,
		Prompt:          "pick",
		AllowFreeText:   true,
	})
	if !errors.Is(err, collab.ErrAttentionPublish) {
		t.Fatalf("Ask = %v, want ErrAttentionPublish", err)
	}
	if _, ok := env.audit.find(collab.AuditAttentionFailed); !ok {
		t.Fatal("publish failure was not audited")
	}
	attentions, err := env.host.Attentions(context.Background(), env.tenant, item.ID)
	if err != nil || len(attentions) != 1 {
		t.Fatalf("the stored attention should remain readable: %+v, %v", attentions, err)
	}
}

func TestAnswerAndExpireAttention(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	attention, err := env.host.Ask(context.Background(), env.tenant, item.ID, collab.AttentionRequest{
		CallerSessionID: env.parent.SessionID,
		RequestID:       "answer-me",
		Prompt:          "pick",
		Options:         []collab.AttentionOption{{ID: "a"}, {ID: "b"}},
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	answered, err := env.host.AnswerAttention(context.Background(), env.tenant, item.ID, attention.ID, collab.AttentionAnswerRequest{OptionID: "a"})
	if err != nil {
		t.Fatalf("AnswerAttention: %v", err)
	}
	if answered.Status != collab.AttentionAnswered || answered.Answer == nil || answered.Answer.OptionID != "a" {
		t.Fatalf("answered attention = %+v", answered)
	}
	same, err := env.host.AnswerAttention(context.Background(), env.tenant, item.ID, attention.ID, collab.AttentionAnswerRequest{OptionID: "a"})
	if err != nil || same.Status != collab.AttentionAnswered {
		t.Fatalf("idempotent answer = %+v, %v", same, err)
	}
	if _, err := env.host.AnswerAttention(context.Background(), env.tenant, item.ID, attention.ID, collab.AttentionAnswerRequest{OptionID: "b"}); !errors.Is(err, collab.ErrAttentionConflict) {
		t.Fatalf("conflicting answer = %v, want ErrAttentionConflict", err)
	}
	if _, err := env.host.AnswerAttention(context.Background(), env.tenant, item.ID, attention.ID, collab.AttentionAnswerRequest{Text: "free"}); !errors.Is(err, collab.ErrAttentionConflict) {
		t.Fatalf("conflicting free-text answer = %v, want ErrAttentionConflict", err)
	}

	expiring, err := env.host.Ask(context.Background(), env.tenant, item.ID, collab.AttentionRequest{
		CallerSessionID: env.parent.SessionID,
		Prompt:          "expire",
		AllowFreeText:   true,
		Timeout:         time.Minute,
	})
	if err != nil {
		t.Fatalf("Ask expiring: %v", err)
	}
	env.clock.Advance(2 * time.Minute)
	if _, err := env.host.AnswerAttention(context.Background(), env.tenant, item.ID, expiring.ID, collab.AttentionAnswerRequest{Text: "late"}); !errors.Is(err, collab.ErrAttentionExpired) {
		t.Fatalf("late answer = %v, want ErrAttentionExpired", err)
	}
	stored, err := env.work.GetAttention(context.Background(), env.tenant, expiring.ID)
	if err != nil || stored.Status != collab.AttentionTimedOut {
		t.Fatalf("expired attention = %+v, %v", stored, err)
	}
}

func TestExpireAttentions(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	if _, err := env.host.Ask(context.Background(), env.tenant, item.ID, collab.AttentionRequest{
		CallerSessionID: env.parent.SessionID,
		Prompt:          "expire",
		AllowFreeText:   true,
		Timeout:         time.Minute,
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	env.clock.Advance(2 * time.Minute)
	count, err := env.host.ExpireAttentions(context.Background(), env.tenant, item.ID)
	if err != nil || count != 1 {
		t.Fatalf("ExpireAttentions = %d, %v", count, err)
	}
	count, err = env.host.ExpireAttentions(context.Background(), env.tenant, item.ID)
	if err != nil || count != 0 {
		t.Fatalf("second ExpireAttentions = %d, %v", count, err)
	}
}

func TestCancelPropagatesAndIsIdempotent(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	assignment1 := env.assign(t, item.ID, "worker")
	session1 := env.createSession(t, collab.SessionRequest{WorkItemID: item.ID, BotID: "worker", AssignmentID: assignment1.ID})
	assignment2 := env.assign(t, item.ID, "worker")
	session2 := env.createSession(t, collab.SessionRequest{WorkItemID: item.ID, BotID: "worker", AssignmentID: assignment2.ID})
	if _, err := env.host.Ask(context.Background(), env.tenant, item.ID, collab.AttentionRequest{
		CallerSessionID: env.parent.SessionID,
		AssignmentID:    assignment1.ID,
		Prompt:          "pick",
		AllowFreeText:   true,
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if err := env.host.Cancel(context.Background(), env.tenant, item.ID, "operator stop"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if env.launcher.cancelCount() != 2 {
		t.Fatalf("cancel propagation = %d, want 2", env.launcher.cancelCount())
	}
	cancelled := env.launcher.cancelledSet()
	if !cancelled[session1.SessionID] || !cancelled[session2.SessionID] {
		t.Fatalf("cancelled sessions = %+v", cancelled)
	}
	for _, sessionID := range []uuid.UUID{session1.SessionID, session2.SessionID} {
		ref, err := env.sessions.Session(context.Background(), env.tenant, sessionID)
		if err != nil || ref.State != collab.SessionCancelled {
			t.Fatalf("session %s state = %+v, %v", sessionID, ref, err)
		}
	}
	stored, err := env.host.WorkItem(context.Background(), env.tenant, item.ID)
	if err != nil || stored.Status != collab.WorkItemCancelled {
		t.Fatalf("work item = %+v, %v", stored, err)
	}
	assignments, _ := env.host.Assignments(context.Background(), env.tenant, item.ID)
	for _, assignment := range assignments {
		if assignment.Status != collab.AssignmentCancelled {
			t.Fatalf("assignment %s status = %s", assignment.ID, assignment.Status)
		}
	}
	attentions, _ := env.host.Attentions(context.Background(), env.tenant, item.ID)
	if len(attentions) != 1 || attentions[0].Status != collab.AttentionCancelled {
		t.Fatalf("attention after cancel = %+v", attentions)
	}
	if err := env.host.Cancel(context.Background(), env.tenant, item.ID, "again"); err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
	if env.launcher.cancelCount() != 2 {
		t.Fatalf("second Cancel re-cancelled sessions: %d", env.launcher.cancelCount())
	}
	if _, err := env.host.Ask(context.Background(), env.tenant, item.ID, collab.AttentionRequest{
		CallerSessionID: env.parent.SessionID,
		Prompt:          "after cancel",
		AllowFreeText:   true,
	}); !errors.Is(err, collab.ErrWorkItemClosed) {
		t.Fatalf("Ask on cancelled work = %v, want ErrWorkItemClosed", err)
	}
}

func TestCancelBareChildSubtree(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	assignment := env.assign(t, item.ID, "worker")
	worker := env.createSession(t, collab.SessionRequest{WorkItemID: item.ID, BotID: "worker", AssignmentID: assignment.ID})
	bare := env.createSession(t, collab.SessionRequest{ParentSessionID: worker.SessionID, BotID: "worker"})
	grandchild := env.createSession(t, collab.SessionRequest{ParentSessionID: bare.SessionID, BotID: "worker"})
	outside := env.createSession(t, collab.SessionRequest{BotID: "worker"})
	if err := env.host.Cancel(context.Background(), env.tenant, item.ID, "stop"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	cancelled := env.launcher.cancelledSet()
	for _, ref := range []collab.SessionRef{worker, bare, grandchild} {
		if !cancelled[ref.SessionID] {
			t.Fatalf("session %s was not cancelled", ref.SessionID)
		}
		stored, err := env.sessions.Session(context.Background(), env.tenant, ref.SessionID)
		if err != nil || stored.State != collab.SessionCancelled {
			t.Fatalf("session %s state = %+v, %v", ref.SessionID, stored, err)
		}
	}
	if cancelled[outside.SessionID] {
		t.Fatalf("unrelated bare child %s was cancelled", outside.SessionID)
	}
	stored, err := env.sessions.Session(context.Background(), env.tenant, outside.SessionID)
	if err != nil || stored.State != collab.SessionActive {
		t.Fatalf("outside session = %+v, %v", stored, err)
	}
}

func TestCancelFailureKeepsWorkInCancelling(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	assignment := env.assign(t, item.ID, "worker")
	env.createSession(t, collab.SessionRequest{WorkItemID: item.ID, BotID: "worker", AssignmentID: assignment.ID})
	env.launcher.cancelErr = errors.New("cancel seam down")
	if err := env.host.Cancel(context.Background(), env.tenant, item.ID, "stop"); !errors.Is(err, collab.ErrCancelFailed) {
		t.Fatalf("Cancel = %v, want ErrCancelFailed", err)
	}
	stored, err := env.host.WorkItem(context.Background(), env.tenant, item.ID)
	if err != nil || stored.Status != collab.WorkItemCancelling {
		t.Fatalf("work item after failed cancel = %+v, %v", stored, err)
	}
	assignments, err := env.host.Assignments(context.Background(), env.tenant, item.ID)
	if err != nil || len(assignments) != 1 || assignments[0].Status != collab.AssignmentActive {
		t.Fatalf("failed cancel should keep the assignment active: %+v, %v", assignments, err)
	}
}

func TestCompleteSessionReleasesCapacity(t *testing.T) {
	limits := defaultModeLimits()
	limits.MaxConcurrent = 1
	limits.MaxChildren = 8
	env := newTestEnv(t, defaultCeiling(), limits)
	first := env.createSession(t, collab.SessionRequest{BotID: "worker"})
	if _, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		ParentSessionID: env.parent.SessionID, BotID: "worker", Task: "blocked",
	}); !errors.Is(err, collab.ErrConcurrencyExceeded) {
		t.Fatalf("second CreateSession = %v, want ErrConcurrencyExceeded", err)
	}
	if err := env.host.CompleteSession(context.Background(), env.tenant, first.SessionID, collab.SessionOutcomeDone, nil, ""); err != nil {
		t.Fatalf("CompleteSession: %v", err)
	}
	stored, err := env.sessions.Session(context.Background(), env.tenant, first.SessionID)
	if err != nil || stored.State != collab.SessionDone {
		t.Fatalf("completed session = %+v, %v", stored, err)
	}
	if _, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		ParentSessionID: env.parent.SessionID, BotID: "worker", Task: "now fits",
	}); err != nil {
		t.Fatalf("CreateSession after CompleteSession: %v", err)
	}
}

func TestCompleteSessionProjectsAssignment(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	assignment := env.assign(t, item.ID, "worker")
	session := env.createSession(t, collab.SessionRequest{WorkItemID: item.ID, BotID: "worker", AssignmentID: assignment.ID})
	if err := env.host.CompleteSession(context.Background(), env.tenant, session.SessionID, collab.SessionOutcomeFailed, nil, "provider error"); err != nil {
		t.Fatalf("CompleteSession: %v", err)
	}
	stored, err := env.work.GetAssignment(context.Background(), env.tenant, assignment.ID)
	if err != nil || stored.Status != collab.AssignmentFailed || stored.Error != "provider error" {
		t.Fatalf("assignment after failure = %+v, %v", stored, err)
	}
}

func TestAuditFailClosedForAcceptedWrites(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	assignment := env.assign(t, item.ID, "worker")
	env.audit.err = errors.New("audit store down")

	if _, err := env.host.OpenWorkItem(context.Background(), env.tenant, "test.mode", collab.WorkItemRequest{
		CallerSessionID: env.parent.SessionID, Title: "un-audited",
	}); !errors.Is(err, collab.ErrAuditUnavailable) {
		t.Fatalf("OpenWorkItem = %v, want ErrAuditUnavailable", err)
	}
	items, err := env.host.WorkItems(context.Background(), env.tenant, collab.WorkFilter{})
	if err != nil || len(items) != 1 {
		t.Fatalf("un-audited work item was stored: %+v, %v", items, err)
	}
	if _, err := env.host.Assign(context.Background(), env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID,
	}); !errors.Is(err, collab.ErrAuditUnavailable) {
		t.Fatalf("Assign = %v, want ErrAuditUnavailable", err)
	}
	assignments, _ := env.host.Assignments(context.Background(), env.tenant, item.ID)
	if len(assignments) != 1 {
		t.Fatalf("un-audited assignment was stored: %+v", assignments)
	}
	if _, err := env.host.Ask(context.Background(), env.tenant, item.ID, collab.AttentionRequest{
		CallerSessionID: env.parent.SessionID,
		AssignmentID:    assignment.ID,
		Prompt:          "un-audited",
		AllowFreeText:   true,
	}); !errors.Is(err, collab.ErrAuditUnavailable) {
		t.Fatalf("Ask = %v, want ErrAuditUnavailable", err)
	}
	attentions, _ := env.host.Attentions(context.Background(), env.tenant, item.ID)
	if len(attentions) != 0 || env.publisher.count() != 0 {
		t.Fatalf("un-audited attention was stored/published: %+v/%d", attentions, env.publisher.count())
	}
	if _, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		ParentSessionID: env.parent.SessionID, BotID: "worker", Task: "un-audited",
	}); !errors.Is(err, collab.ErrAuditUnavailable) {
		t.Fatalf("CreateSession = %v, want ErrAuditUnavailable", err)
	}
	if env.launcher.launchCount() != 0 {
		t.Fatalf("un-auditable creation reached the launcher: %d", env.launcher.launchCount())
	}
}

func TestLauncherFailureIsReturnedAndAudited(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	env.launcher.launchErr = errors.New("provider exploded")
	if _, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		ParentSessionID: env.parent.SessionID,
		BotID:           "worker",
		Task:            "x",
	}); err == nil || !errors.Is(err, env.launcher.launchErr) {
		t.Fatalf("CreateSession = %v, want the launcher error", err)
	}
	if _, ok := env.audit.find(collab.AuditSessionFailed); !ok {
		t.Fatal("launcher failure was not audited")
	}
}

func TestOpenWorkItemTenantAndLimits(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	if _, err := env.host.OpenWorkItem(context.Background(), uuid.New(), "test.mode", collab.WorkItemRequest{
		CallerSessionID: env.parent.SessionID, Title: "x",
	}); !errors.Is(err, collab.ErrTenantMismatch) {
		t.Fatalf("cross-tenant OpenWorkItem = %v, want ErrTenantMismatch", err)
	}
	if _, err := env.host.OpenWorkItem(context.Background(), env.tenant, "test.mode", collab.WorkItemRequest{}); !errors.Is(err, collab.ErrInvalidRequest) {
		t.Fatalf("empty work item = %v, want ErrInvalidRequest", err)
	}
	if _, err := env.host.OpenWorkItem(context.Background(), env.tenant, "test.mode", collab.WorkItemRequest{
		CallerSessionID: env.parent.SessionID,
		Title:           "x",
		Limits:          collab.Limits{MaxTurns: defaultModeLimits().MaxTurns + 1},
	}); !errors.Is(err, collab.ErrLimitExceeded) {
		t.Fatalf("over-mode work limits = %v, want ErrLimitExceeded", err)
	}
	item, err := env.host.OpenWorkItem(context.Background(), env.tenant, "test.mode", collab.WorkItemRequest{
		CallerSessionID: env.parent.SessionID,
		Title:           "idempotent",
		Limits:          collab.Limits{MaxTurns: 3},
		IdempotencyKey:  "source-1",
	})
	if err != nil {
		t.Fatalf("OpenWorkItem: %v", err)
	}
	if item.Limits.MaxTurns != 3 || item.Limits.Depth != defaultModeLimits().Depth {
		t.Fatalf("work item limits = %+v", item.Limits)
	}
	again, err := env.host.OpenWorkItem(context.Background(), env.tenant, "test.mode", collab.WorkItemRequest{
		CallerSessionID: env.parent.SessionID,
		Title:           "idempotent retry",
		IdempotencyKey:  "source-1",
	})
	if err != nil || again.ID != item.ID {
		t.Fatalf("idempotent OpenWorkItem = %+v, %v", again, err)
	}
	if _, err := env.host.WorkItem(context.Background(), uuid.New(), item.ID); !errors.Is(err, collab.ErrTenantMismatch) {
		t.Fatalf("cross-tenant read = %v, want ErrTenantMismatch", err)
	}
}

func TestAddLinkValidatesVocabulary(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	link, err := env.host.AddLink(context.Background(), env.tenant, collab.Link{
		WorkItemID: item.ID,
		Kind:       collab.LinkReview,
		From:       collab.LinkEndpoint{Type: collab.LinkEndpointSession, ID: uuid.NewString()},
		To:         collab.LinkEndpoint{Type: collab.LinkEndpointWorkItem, ID: item.ID.String()},
	})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	if link.ID == uuid.Nil || link.TenantID != env.tenant {
		t.Fatalf("link = %+v", link)
	}
	links, err := env.host.Links(context.Background(), env.tenant, item.ID)
	if err != nil || len(links) != 1 {
		t.Fatalf("Links = %+v, %v", links, err)
	}
	if _, err := env.host.AddLink(context.Background(), env.tenant, collab.Link{
		Kind: "galaxy",
		From: collab.LinkEndpoint{Type: collab.LinkEndpointSession, ID: "a"},
		To:   collab.LinkEndpoint{Type: collab.LinkEndpointSession, ID: "b"},
	}); !errors.Is(err, collab.ErrInvalidRequest) {
		t.Fatalf("bad link kind = %v, want ErrInvalidRequest", err)
	}
	if _, err := env.host.AddLink(context.Background(), env.tenant, collab.Link{
		Kind: collab.LinkReview,
		From: collab.LinkEndpoint{Type: collab.LinkEndpointSession, ID: "not-a-uuid"},
		To:   collab.LinkEndpoint{Type: collab.LinkEndpointWorkItem, ID: item.ID.String()},
	}); !errors.Is(err, collab.ErrInvalidRequest) {
		t.Fatalf("bad link endpoint = %v, want ErrInvalidRequest", err)
	}
}

func TestNewHostRequiresSeams(t *testing.T) {
	registry := newRegistry(t, collab.Limits{})
	_, err := collab.NewHost(collab.HostOptions{})
	if !errors.Is(err, collab.ErrTenantRequired) {
		t.Fatalf("empty HostOptions = %v, want ErrTenantRequired", err)
	}
	_, err = collab.NewHost(collab.HostOptions{TenantID: uuid.New(), Modes: registry})
	if !errors.Is(err, collab.ErrInvalidRequest) {
		t.Fatalf("missing seams = %v, want ErrInvalidRequest", err)
	}
}

func TestNewHostRejectsRegistryTenantMismatch(t *testing.T) {
	registry := newRegistry(t, collab.Limits{})
	hostTenant := uuid.New()
	_, err := collab.NewHost(collab.HostOptions{
		TenantID:    hostTenant,
		Modes:       registry,
		Work:        collab.NewMemoryWorkStore(nil),
		Sessions:    collab.NewMemorySessionIndex(),
		Permissions: &fakePermissions{},
		Audit:       &fakeAudit{},
		Launcher:    &fakeLauncher{},
	})
	if !errors.Is(err, collab.ErrTenantMismatch) {
		t.Fatalf("NewHost with foreign registry = %v, want ErrTenantMismatch", err)
	}
}
