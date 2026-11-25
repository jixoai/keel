package collab_test

import (
	"context"
	"errors"
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

type testEnv struct {
	tenant      uuid.UUID
	registry    *collab.ModeRegistry
	work        *collab.MemoryWorkStore
	sessions    *collab.MemorySessionIndex
	launcher    *fakeLauncher
	permissions *fakePermissions
	audit       *fakeAudit
	publisher   *fakePublisher
	host        *collab.Coordinator
	parent      collab.SessionRef
}

func defaultCeiling() collab.Limits {
	return collab.Limits{
		MaxDepth: 8, MaxChildren: 8, MaxConcurrent: 8,
		MaxTurns: 100, MaxTokens: 100_000, MaxDurationMs: 600_000,
	}
}

func defaultModeLimits() collab.Limits {
	return collab.Limits{
		MaxDepth: 4, MaxChildren: 4, MaxConcurrent: 4,
		MaxTurns: 10, MaxTokens: 1_000, MaxDurationMs: 60_000,
	}
}

func newTestEnv(t *testing.T, ceiling, modeLimits collab.Limits) *testEnv {
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
	now := func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) }
	work := collab.NewMemoryWorkStore(now)
	sessions := collab.NewMemorySessionIndex()
	launcher := &fakeLauncher{}
	permissions := &fakePermissions{}
	audit := &fakeAudit{}
	publisher := &fakePublisher{}
	parent := collab.SessionRef{
		TenantID: tenant, SessionID: uuid.New(), BotID: "captain",
		Depth: 0, State: collab.SessionActive, CreatedAt: now(),
	}
	if err := sessions.Put(ctx, parent); err != nil {
		t.Fatalf("put parent: %v", err)
	}
	host, err := collab.NewHost(collab.HostOptions{
		TenantID:    tenant,
		Modes:       registry,
		Work:        work,
		Sessions:    sessions,
		Permissions: permissions,
		Audit:       audit,
		Launcher:    launcher,
		Attention:   publisher,
		Now:         now,
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	return &testEnv{
		tenant: tenant, registry: registry, work: work, sessions: sessions,
		launcher: launcher, permissions: permissions, audit: audit, publisher: publisher,
		host: host, parent: parent,
	}
}

func (e *testEnv) openWork(t *testing.T, title string) collab.WorkItem {
	t.Helper()
	item, err := e.host.OpenWorkItem(context.Background(), e.tenant, "test.mode", collab.WorkItemRequest{
		Title:  title,
		Source: collab.SourceRef{Kind: "manual"},
	})
	if err != nil {
		t.Fatalf("OpenWorkItem: %v", err)
	}
	return item
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
	assignment, err := env.host.Assign(context.Background(), env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID,
	})
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
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

func TestCreateSessionRejectsDepth(t *testing.T) {
	limits := defaultModeLimits()
	limits.MaxDepth = 2
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
}

func TestCreateSessionRejectsWidth(t *testing.T) {
	limits := defaultModeLimits()
	limits.MaxChildren = 1
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

	// The tenant ceiling wins over a wider mode descriptor, and a request may
	// narrow it further but not exceed it.
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

func TestCreateSessionRejectsMismatchedWorkItemMode(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	other := testMode(defaultModeLimits())
	other.ID = "other.mode"
	if err := env.registry.Register(other); err != nil {
		t.Fatalf("register other mode: %v", err)
	}
	otherWork, err := env.host.OpenWorkItem(context.Background(), env.tenant, "other.mode", collab.WorkItemRequest{Title: "other"})
	if err != nil {
		t.Fatalf("OpenWorkItem(other): %v", err)
	}
	_, err = env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		WorkItemID:      otherWork.ID,
		ParentSessionID: env.parent.SessionID,
		BotID:           "worker",
		Task:            "wrong mode",
	})
	if !errors.Is(err, collab.ErrInvalidCreation) {
		t.Fatalf("mismatched work item = %v, want ErrInvalidCreation", err)
	}
}

func TestCreateSessionRejectsParentFromAnotherWorkItem(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	first := env.openWork(t, "first")
	second := env.openWork(t, "second")
	child := env.createSession(t, collab.SessionRequest{WorkItemID: first.ID, BotID: "worker"})
	_, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		WorkItemID:      second.ID,
		ParentSessionID: child.SessionID,
		BotID:           "worker",
		Task:            "borrowed authority",
	})
	if !errors.Is(err, collab.ErrInvalidCreation) {
		t.Fatalf("cross-work parent = %v, want ErrInvalidCreation", err)
	}
}

func TestDisabledModeKeepsExistingWork(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "existing ticket")
	assignment, err := env.host.Assign(context.Background(), env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID,
	})
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := env.registry.SetEnabled("test.mode", false); err != nil {
		t.Fatalf("disable: %v", err)
	}

	if _, err := env.host.OpenWorkItem(context.Background(), env.tenant, "test.mode", collab.WorkItemRequest{Title: "new"}); !errors.Is(err, collab.ErrModeDisabled) {
		t.Fatalf("OpenWorkItem while disabled = %v, want ErrModeDisabled", err)
	}
	stored, err := env.host.WorkItem(context.Background(), env.tenant, item.ID)
	if err != nil || stored.ID != item.ID {
		t.Fatalf("existing work unreadable after disable: %+v, %v", stored, err)
	}
	items, err := env.host.WorkItems(context.Background(), env.tenant, collab.WorkFilter{ModeID: "test.mode"})
	if err != nil || len(items) != 1 {
		t.Fatalf("WorkItems after disable = %+v, %v", items, err)
	}
	assignments, err := env.host.Assignments(context.Background(), env.tenant, item.ID)
	if err != nil || len(assignments) != 1 || assignments[0].ID != assignment.ID {
		t.Fatalf("Assignments after disable = %+v, %v", assignments, err)
	}
	if _, err := env.host.Assign(context.Background(), env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID,
	}); !errors.Is(err, collab.ErrModeDisabled) {
		t.Fatalf("Assign while disabled = %v, want ErrModeDisabled", err)
	}
	if _, err := env.host.Assign(context.Background(), env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID,
		HumanTakeover:   true,
	}); err != nil {
		t.Fatalf("human takeover assignment: %v", err)
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
	assignment1, err := env.host.Assign(context.Background(), env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID,
	})
	if err != nil {
		t.Fatalf("Assign 1: %v", err)
	}
	session1 := env.createSession(t, collab.SessionRequest{
		WorkItemID:   item.ID,
		BotID:        "worker",
		AssignmentID: assignment1.ID,
	})
	if err := env.sessions.SetState(context.Background(), env.tenant, session1.SessionID, collab.SessionDone); err != nil {
		t.Fatalf("end session 1: %v", err)
	}
	assignment2, err := env.host.Assign(context.Background(), env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID,
	})
	if err != nil {
		t.Fatalf("Assign 2: %v", err)
	}
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
		WorkItemID:   item.ID,
		BotID:        "worker",
		AssignmentID: assignment2.ID,
	})
	if session2.SessionID == session1.SessionID {
		t.Fatal("replacement worker reused the old session")
	}
}

func TestAskStructuredAttention(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	assignment, err := env.host.Assign(context.Background(), env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID,
	})
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
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
	// The same request id returns the original row and is not published twice.
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

func TestCancelPropagatesAndIsIdempotent(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	assignment1, err := env.host.Assign(context.Background(), env.tenant, item.ID, "worker", collab.AssignmentRequest{CallerSessionID: env.parent.SessionID})
	if err != nil {
		t.Fatalf("Assign 1: %v", err)
	}
	session1 := env.createSession(t, collab.SessionRequest{WorkItemID: item.ID, BotID: "worker", AssignmentID: assignment1.ID})
	assignment2, err := env.host.Assign(context.Background(), env.tenant, item.ID, "worker", collab.AssignmentRequest{CallerSessionID: env.parent.SessionID})
	if err != nil {
		t.Fatalf("Assign 2: %v", err)
	}
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
	cancelled := map[uuid.UUID]bool{}
	env.launcher.mu.Lock()
	for _, id := range env.launcher.cancelled {
		cancelled[id] = true
	}
	env.launcher.mu.Unlock()
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

func TestCancelFailureKeepsWorkInCancelling(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	assignment, err := env.host.Assign(context.Background(), env.tenant, item.ID, "worker", collab.AssignmentRequest{CallerSessionID: env.parent.SessionID})
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	env.createSession(t, collab.SessionRequest{WorkItemID: item.ID, BotID: "worker", AssignmentID: assignment.ID})
	env.launcher.cancelErr = errors.New("cancel seam down")
	if err := env.host.Cancel(context.Background(), env.tenant, item.ID, "stop"); !errors.Is(err, collab.ErrCancelFailed) {
		t.Fatalf("Cancel = %v, want ErrCancelFailed", err)
	}
	stored, err := env.host.WorkItem(context.Background(), env.tenant, item.ID)
	if err != nil || stored.Status != collab.WorkItemCancelling {
		t.Fatalf("work item after failed cancel = %+v, %v", stored, err)
	}
}

func TestAuditFailureBlocksSessionCreation(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	env.audit.err = errors.New("audit store down")
	_, err := env.host.CreateSession(context.Background(), env.tenant, "test.mode", collab.SessionRequest{
		ParentSessionID: env.parent.SessionID,
		BotID:           "worker",
		Task:            "must not start",
	})
	if !errors.Is(err, collab.ErrAuditUnavailable) {
		t.Fatalf("CreateSession = %v, want ErrAuditUnavailable", err)
	}
	if env.launcher.launchCount() != 0 {
		t.Fatal("un-auditable creation reached the launcher")
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
	if _, err := env.host.OpenWorkItem(context.Background(), uuid.New(), "test.mode", collab.WorkItemRequest{Title: "x"}); !errors.Is(err, collab.ErrTenantMismatch) {
		t.Fatalf("cross-tenant OpenWorkItem = %v, want ErrTenantMismatch", err)
	}
	if _, err := env.host.OpenWorkItem(context.Background(), env.tenant, "test.mode", collab.WorkItemRequest{}); !errors.Is(err, collab.ErrInvalidRequest) {
		t.Fatalf("empty work item = %v, want ErrInvalidRequest", err)
	}
	if _, err := env.host.OpenWorkItem(context.Background(), env.tenant, "test.mode", collab.WorkItemRequest{
		Title:  "x",
		Limits: collab.Limits{MaxTurns: defaultModeLimits().MaxTurns + 1},
	}); !errors.Is(err, collab.ErrLimitExceeded) {
		t.Fatalf("over-mode work limits = %v, want ErrLimitExceeded", err)
	}
	item, err := env.host.OpenWorkItem(context.Background(), env.tenant, "test.mode", collab.WorkItemRequest{
		Title:          "idempotent",
		Limits:         collab.Limits{MaxTurns: 3},
		IdempotencyKey: "source-1",
	})
	if err != nil {
		t.Fatalf("OpenWorkItem: %v", err)
	}
	if item.Limits.MaxTurns != 3 || item.Limits.MaxDepth != defaultModeLimits().MaxDepth {
		t.Fatalf("work item limits = %+v", item.Limits)
	}
	again, err := env.host.OpenWorkItem(context.Background(), env.tenant, "test.mode", collab.WorkItemRequest{
		Title:          "idempotent retry",
		IdempotencyKey: "source-1",
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
