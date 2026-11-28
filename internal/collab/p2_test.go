package collab_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/collab"
)

// hideWorkItemIndex models a stale session projection: the durable assignment
// exists but the index never recorded which WorkItem the session belongs to.
type hideWorkItemIndex struct {
	*collab.MemorySessionIndex
}

func (h hideWorkItemIndex) Session(ctx context.Context, tenantID, sessionID uuid.UUID) (collab.SessionRef, error) {
	ref, err := h.MemorySessionIndex.Session(ctx, tenantID, sessionID)
	ref.WorkItemID = uuid.Nil
	return ref, err
}

// addWorker registers an active worker-Bot session without a Host launch.
func (e *testEnv) addWorker(t *testing.T) collab.SessionRef {
	t.Helper()
	ref := collab.SessionRef{
		TenantID: e.tenant, SessionID: uuid.New(), ParentSessionID: e.parent.SessionID,
		BotID: "worker", Depth: 1, State: collab.SessionActive, CreatedAt: time.Now().UTC(),
	}
	if err := e.sessions.Put(context.Background(), ref); err != nil {
		t.Fatalf("put worker session: %v", err)
	}
	return ref
}

func TestAssignRejectsBoundSessionAcrossWorkItems(t *testing.T) {
	env := newTestEnvWith(t, defaultCeiling(), defaultModeLimits(), func(m *collab.MemorySessionIndex) collab.SessionIndex {
		return hideWorkItemIndex{MemorySessionIndex: m}
	})
	first := env.openWork(t, "first")
	second := env.openWork(t, "second")
	worker := env.addWorker(t)

	ctx := context.Background()
	if _, err := env.host.Assign(ctx, env.tenant, first.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID,
		SessionID:       worker.SessionID,
	}); err != nil {
		t.Fatalf("Assign first: %v", err)
	}
	_, err := env.host.Assign(ctx, env.tenant, second.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID,
		SessionID:       worker.SessionID,
	})
	if !errors.Is(err, collab.ErrSessionAssigned) || !collab.IsCode(err, collab.CodeSessionAssigned) {
		t.Fatalf("cross-work rebind = %v, want ErrSessionAssigned/session_already_assigned", err)
	}
	assignments, err := env.host.Assignments(ctx, env.tenant, second.ID)
	if err != nil || len(assignments) != 0 {
		t.Fatalf("second work assignments = %+v, %v, want none", assignments, err)
	}
	original, err := env.host.Assignments(ctx, env.tenant, first.ID)
	if err != nil || len(original) != 1 || original[0].SessionID != worker.SessionID {
		t.Fatalf("original assignment changed = %+v, %v, want the first binding intact", original, err)
	}
}

func TestAssignSameSessionTwiceInWorkItemRejected(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	worker := env.addWorker(t)
	ctx := context.Background()
	if _, err := env.host.Assign(ctx, env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID, SessionID: worker.SessionID,
	}); err != nil {
		t.Fatalf("first Assign: %v", err)
	}
	_, err := env.host.Assign(ctx, env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID, SessionID: worker.SessionID,
	})
	if !errors.Is(err, collab.ErrAssignmentBound) || !collab.IsCode(err, collab.CodeAssignmentBound) {
		t.Fatalf("duplicate session binding = %v, want ErrAssignmentBound/assignment_already_bound", err)
	}
}

func TestAssignIdempotentParameterConflicts(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	worker1 := env.addWorker(t)
	worker2 := env.addWorker(t)
	ctx := context.Background()

	first, err := env.host.Assign(ctx, env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID, SessionID: worker1.SessionID, IdempotencyKey: "assign-1",
	})
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	replay, err := env.host.Assign(ctx, env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID, SessionID: worker1.SessionID, IdempotencyKey: "assign-1",
	})
	if err != nil || replay.ID != first.ID || replay.SessionID != worker1.SessionID {
		t.Fatalf("idempotent replay = %+v, %v, want %s", replay, err, first.ID)
	}
	if _, err := env.host.Assign(ctx, env.tenant, item.ID, "other", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID, SessionID: worker1.SessionID, IdempotencyKey: "assign-1",
	}); !collab.IsCode(err, collab.CodeAssignmentConflict) {
		t.Fatalf("bot change = %v, want assignment_conflict", err)
	}
	if _, err := env.host.Assign(ctx, env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID, SessionID: worker2.SessionID, IdempotencyKey: "assign-1",
	}); !collab.IsCode(err, collab.CodeAssignmentConflict) {
		t.Fatalf("session change = %v, want assignment_conflict", err)
	}
	// A retry without a worker returns the earlier bound assignment.
	if got, err := env.host.Assign(ctx, env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID, IdempotencyKey: "assign-1",
	}); err != nil || got.ID != first.ID {
		t.Fatalf("retry without worker = %+v, %v", got, err)
	}
	// The first call opened an assignment; a retry that supplies the worker
	// completes the same row instead of creating a second one.
	openAssignment, err := env.host.Assign(ctx, env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID, IdempotencyKey: "assign-2",
	})
	if err != nil || openAssignment.Status != collab.AssignmentOpen {
		t.Fatalf("open assignment = %+v, %v", openAssignment, err)
	}
	completed, err := env.host.Assign(ctx, env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID, SessionID: worker2.SessionID, IdempotencyKey: "assign-2",
	})
	if err != nil {
		t.Fatalf("complete open assignment: %v", err)
	}
	if completed.ID != openAssignment.ID || completed.SessionID != worker2.SessionID || completed.Status != collab.AssignmentActive {
		t.Fatalf("completed assignment = %+v, want the same row bound to worker2", completed)
	}
	assignments, err := env.host.Assignments(ctx, env.tenant, item.ID)
	if err != nil || len(assignments) != 2 {
		t.Fatalf("assignments = %d (%v), want 2", len(assignments), err)
	}
}

// TestAssignConcurrentCrossWorkItemBindsOnce pins the serialisation of the
// binding guard: two concurrent Assign calls for the same session on different
// WorkItems cannot both observe "unbound". Exactly one wins; the other is a
// typed session_already_assigned refusal, and only one assignment row exists.
func TestAssignConcurrentCrossWorkItemBindsOnce(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	first := env.openWork(t, "first")
	second := env.openWork(t, "second")
	worker := env.addWorker(t)
	ctx := context.Background()

	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for index, item := range []collab.WorkItem{first, second} {
		index, item := index, item
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[index] = env.host.Assign(ctx, env.tenant, item.ID, "worker", collab.AssignmentRequest{
				CallerSessionID: env.parent.SessionID, SessionID: worker.SessionID,
			})
		}()
	}
	close(start)
	wg.Wait()

	accepted, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, collab.ErrSessionAssigned):
			refused++
		default:
			t.Fatalf("concurrent Assign error = %v, want nil or ErrSessionAssigned", err)
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatalf("concurrent outcomes = accepted %d / refused %d, want 1/1 (%v)", accepted, refused, errs)
	}
	total := 0
	for _, item := range []collab.WorkItem{first, second} {
		assignments, err := env.host.Assignments(ctx, env.tenant, item.ID)
		if err != nil {
			t.Fatalf("Assignments(%s): %v", item.ID, err)
		}
		total += len(assignments)
	}
	if total != 1 {
		t.Fatalf("assignment rows = %d, want exactly one winner", total)
	}
}

// TestAssignConcurrentSameKeyBindsOnce pins the idempotency-key lock: two
// concurrent retries of one key with different worker sessions cannot both
// complete the same open assignment. Exactly one succeeds; the other is an
// assignment_conflict, and the stored row belongs to the winner.
func TestAssignConcurrentSameKeyBindsOnce(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	workerA := env.addWorker(t)
	workerB := env.addWorker(t)
	ctx := context.Background()

	open, err := env.host.Assign(ctx, env.tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: env.parent.SessionID, IdempotencyKey: "same-key",
	})
	if err != nil || open.SessionID != uuid.Nil {
		t.Fatalf("open assignment = (%+v, %v), want unbound", open, err)
	}

	start := make(chan struct{})
	results := make([]collab.Assignment, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for index, worker := range []collab.SessionRef{workerA, workerB} {
		index, worker := index, worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[index], errs[index] = env.host.Assign(ctx, env.tenant, item.ID, "worker", collab.AssignmentRequest{
				CallerSessionID: env.parent.SessionID, SessionID: worker.SessionID,
				IdempotencyKey: "same-key",
			})
		}()
	}
	close(start)
	wg.Wait()

	accepted, conflicted := 0, 0
	var winner collab.SessionRef
	for index, err := range errs {
		switch {
		case err == nil:
			accepted++
			winner = []collab.SessionRef{workerA, workerB}[index]
		case collab.IsCode(err, collab.CodeAssignmentConflict):
			conflicted++
		default:
			t.Fatalf("concurrent same-key Assign error = %v, want nil or assignment_conflict", err)
		}
	}
	if accepted != 1 || conflicted != 1 {
		t.Fatalf("outcomes = accepted %d / conflicted %d (%v), want 1/1", accepted, conflicted, errs)
	}
	assignments, err := env.host.Assignments(ctx, env.tenant, item.ID)
	if err != nil || len(assignments) != 1 || assignments[0].SessionID != winner.SessionID {
		t.Fatalf("assignments = %+v (%v), want only winner %s", assignments, err, winner.SessionID)
	}
}

func TestCreateSessionRejectsBoundAndTerminalAssignment(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	assignment := env.assign(t, item.ID, "worker")
	ctx := context.Background()
	session := env.createSession(t, collab.SessionRequest{
		WorkItemID: item.ID, BotID: "worker", AssignmentID: assignment.ID,
	})
	if _, err := env.host.CreateSession(ctx, env.tenant, "test.mode", collab.SessionRequest{
		WorkItemID: item.ID, ParentSessionID: env.parent.SessionID, BotID: "worker",
		Task: "second worker", AssignmentID: assignment.ID,
	}); !errors.Is(err, collab.ErrAssignmentBound) || !collab.IsCode(err, collab.CodeAssignmentBound) {
		t.Fatalf("bound assignment = %v, want ErrAssignmentBound", err)
	}
	if err := env.host.CompleteSession(ctx, env.tenant, session.SessionID, collab.SessionOutcomeDone, nil, ""); err != nil {
		t.Fatalf("CompleteSession: %v", err)
	}
	if _, err := env.host.CreateSession(ctx, env.tenant, "test.mode", collab.SessionRequest{
		WorkItemID: item.ID, ParentSessionID: env.parent.SessionID, BotID: "worker",
		Task: "after terminal", AssignmentID: assignment.ID,
	}); !errors.Is(err, collab.ErrAssignmentTerminal) || !collab.IsCode(err, collab.CodeAssignmentTerminal) {
		t.Fatalf("terminal assignment = %v, want ErrAssignmentTerminal", err)
	}
}

func workItemEndpoint(id uuid.UUID) collab.LinkEndpoint {
	return collab.LinkEndpoint{Type: collab.LinkEndpointWorkItem, ID: id.String()}
}

func TestAddLinkIdempotentAcrossWorkItems(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	first := env.openWork(t, "first")
	second := env.openWork(t, "second")
	ctx := context.Background()

	link, err := env.host.AddLink(ctx, env.tenant, collab.Link{
		Kind: collab.LinkReview,
		From: workItemEndpoint(first.ID),
		To:   workItemEndpoint(second.ID),
	})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	again, err := env.host.AddLink(ctx, env.tenant, collab.Link{
		Kind: collab.LinkReview,
		From: workItemEndpoint(first.ID),
		To:   workItemEndpoint(second.ID),
	})
	if err != nil || again.ID != link.ID {
		t.Fatalf("repeated AddLink = %+v, %v, want the same row %s", again, err, link.ID)
	}
	for _, item := range []collab.WorkItem{first, second} {
		links, err := env.host.Links(ctx, env.tenant, item.ID)
		if err != nil || len(links) != 1 || links[0].ID != link.ID {
			t.Fatalf("Links(%s) = %+v, %v, want the cross-work relation", item.ID, links, err)
		}
	}
	// An explicit id for the same natural relation resolves to the existing row.
	explicit := uuid.New()
	stored, err := env.host.AddLink(ctx, env.tenant, collab.Link{
		ID:   explicit,
		Kind: collab.LinkReview,
		From: workItemEndpoint(first.ID),
		To:   workItemEndpoint(second.ID),
	})
	if err != nil || stored.ID != link.ID {
		t.Fatalf("explicit-id dedupe = %+v, %v, want existing %s", stored, err, link.ID)
	}
	// The same explicit id for a different relation is a conflict.
	if _, err := env.host.AddLink(ctx, env.tenant, collab.Link{
		ID:   link.ID,
		Kind: collab.LinkDelegation,
		From: workItemEndpoint(first.ID),
		To:   workItemEndpoint(second.ID),
	}); !collab.IsCode(err, collab.CodeLinkConflict) {
		t.Fatalf("explicit-id conflict = %v, want link_conflict", err)
	}
}

// TestAddLinkCanonicalizesUUIDEndpoints pins that UUID casing/hyphenation is
// not part of the relation identity: the same endpoints spelled differently
// resolve to one row instead of bypassing the natural-key dedupe.
func TestAddLinkCanonicalizesUUIDEndpoints(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	first := env.openWork(t, "first")
	second := env.openWork(t, "second")
	ctx := context.Background()

	link, err := env.host.AddLink(ctx, env.tenant, collab.Link{
		Kind: collab.LinkReview,
		From: collab.LinkEndpoint{Type: collab.LinkEndpointWorkItem, ID: strings.ToUpper(first.ID.String())},
		To:   collab.LinkEndpoint{Type: collab.LinkEndpointWorkItem, ID: strings.ToUpper(second.ID.String())},
	})
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	again, err := env.host.AddLink(ctx, env.tenant, collab.Link{
		Kind: collab.LinkReview,
		From: workItemEndpoint(first.ID),
		To:   workItemEndpoint(second.ID),
	})
	if err != nil || again.ID != link.ID {
		t.Fatalf("lowercase replay = (%+v, %v), want the row %s", again, err, link.ID)
	}
	links, err := env.host.Links(ctx, env.tenant, first.ID)
	if err != nil || len(links) != 1 {
		t.Fatalf("links = %+v (%v), want one canonical row", links, err)
	}
}

func TestAddLinkRejectsUnknownEndpoints(t *testing.T) {
	env := newTestEnv(t, defaultCeiling(), defaultModeLimits())
	item := env.openWork(t, "ticket")
	ctx := context.Background()
	for name, from := range map[string]collab.LinkEndpoint{
		"session":    {Type: collab.LinkEndpointSession, ID: uuid.NewString()},
		"work item":  workItemEndpoint(uuid.New()),
		"assignment": {Type: collab.LinkEndpointAssignment, ID: uuid.NewString()},
	} {
		if _, err := env.host.AddLink(ctx, env.tenant, collab.Link{
			Kind: collab.LinkReview,
			From: from,
			To:   workItemEndpoint(item.ID),
		}); !errors.Is(err, collab.ErrInvalidRequest) {
			t.Fatalf("%s endpoint = %v, want ErrInvalidRequest", name, err)
		}
	}
	// A work item of another tenant is the same not-found answer.
	if _, err := env.host.AddLink(ctx, uuid.New(), collab.Link{
		Kind: collab.LinkReview,
		From: workItemEndpoint(item.ID),
		To:   workItemEndpoint(item.ID),
	}); !errors.Is(err, collab.ErrTenantMismatch) {
		t.Fatalf("foreign tenant = %v, want ErrTenantMismatch", err)
	}
	// bot/channel endpoints have no existence port; a non-empty id is enough.
	if _, err := env.host.AddLink(ctx, env.tenant, collab.Link{
		Kind: collab.LinkSubscription,
		From: collab.LinkEndpoint{Type: collab.LinkEndpointBot, ID: "worker"},
		To:   collab.LinkEndpoint{Type: collab.LinkEndpointChannel, ID: "github:acme/repo#1"},
	}); err != nil {
		t.Fatalf("bot/channel link: %v", err)
	}
}

func tenantModeID(tenantID uuid.UUID) string {
	return fmt.Sprintf("mode.tenant_%s", tenantID.String())
}

func testTenantRegistry(t *testing.T, tenantID uuid.UUID) *collab.ModeRegistry {
	t.Helper()
	registry, err := collab.NewModeRegistry(tenantID, defaultCeiling())
	if err != nil {
		t.Fatalf("NewModeRegistry: %v", err)
	}
	mode := testMode(defaultModeLimits())
	mode.ID = tenantModeID(tenantID)
	if err := registry.Register(mode); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return registry
}

func TestTenantRegistriesBuildOncePerTenant(t *testing.T) {
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	var builds int64
	source, err := collab.NewTenantRegistries(func(_ context.Context, tenantID uuid.UUID) (*collab.ModeRegistry, error) {
		atomic.AddInt64(&builds, 1)
		return testTenantRegistry(t, tenantID), nil
	})
	if err != nil {
		t.Fatalf("NewTenantRegistries: %v", err)
	}
	registryA, err := source.Registry(ctx, tenantA)
	if err != nil || registryA.TenantID() != tenantA {
		t.Fatalf("registry A = %+v, %v", registryA, err)
	}
	again, err := source.Registry(ctx, tenantA)
	if err != nil || again != registryA {
		t.Fatalf("cached registry = %p, %v, want %p", again, err, registryA)
	}
	registryB, err := source.Registry(ctx, tenantB)
	if err != nil || registryB.TenantID() != tenantB || registryB == registryA {
		t.Fatalf("registry B = %+v, %v", registryB, err)
	}
	if got := atomic.LoadInt64(&builds); got != 2 {
		t.Fatalf("factory builds = %d, want 2", got)
	}
	views, err := source.List(ctx, tenantA)
	if err != nil || len(views) != 1 || views[0].ID != tenantModeID(tenantA) {
		t.Fatalf("List(A) = %+v, %v", views, err)
	}
}

func TestTenantRegistriesRejectUnknownAndMismatchedTenants(t *testing.T) {
	ctx := context.Background()
	unknown := uuid.New()
	mismatched := uuid.New()
	source, err := collab.NewTenantRegistries(func(_ context.Context, tenantID uuid.UUID) (*collab.ModeRegistry, error) {
		switch tenantID {
		case unknown:
			return nil, errors.New("tenant is not configured")
		case mismatched:
			return testTenantRegistry(t, uuid.New()), nil
		default:
			return testTenantRegistry(t, tenantID), nil
		}
	})
	if err != nil {
		t.Fatalf("NewTenantRegistries: %v", err)
	}
	if _, err := source.Registry(ctx, unknown); !errors.Is(err, collab.ErrUnknownTenant) || !collab.IsCode(err, collab.CodeTenantUnknown) {
		t.Fatalf("unknown tenant = %v, want ErrUnknownTenant", err)
	}
	if _, err := source.Registry(ctx, mismatched); !errors.Is(err, collab.ErrTenantMismatch) {
		t.Fatalf("mismatched registry = %v, want ErrTenantMismatch", err)
	}
	if _, err := source.Registry(ctx, uuid.Nil); !errors.Is(err, collab.ErrTenantRequired) {
		t.Fatalf("nil tenant = %v, want ErrTenantRequired", err)
	}
}

func TestTenantRegistriesConcurrentFirstUseBuildsOnce(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()
	var builds int64
	source, err := collab.NewTenantRegistries(func(_ context.Context, tenantID uuid.UUID) (*collab.ModeRegistry, error) {
		atomic.AddInt64(&builds, 1)
		time.Sleep(time.Millisecond)
		return testTenantRegistry(t, tenantID), nil
	})
	if err != nil {
		t.Fatalf("NewTenantRegistries: %v", err)
	}
	const workers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]*collab.ModeRegistry, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			results[index], errs[index] = source.Registry(ctx, tenant)
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < workers; i++ {
		if errs[i] != nil || results[i] != results[0] {
			t.Fatalf("worker %d = %p, %v; want %p", i, results[i], errs[i], results[0])
		}
	}
	if got := atomic.LoadInt64(&builds); got != 1 {
		t.Fatalf("factory builds = %d, want 1", got)
	}
}

func TestHostResolvesPerRequestTenantRegistry(t *testing.T) {
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	source, err := collab.NewTenantRegistries(func(_ context.Context, tenantID uuid.UUID) (*collab.ModeRegistry, error) {
		return testTenantRegistry(t, tenantID), nil
	})
	if err != nil {
		t.Fatalf("NewTenantRegistries: %v", err)
	}
	work := collab.NewMemoryWorkStore(nil)
	sessions := collab.NewMemorySessionIndex()
	parentA := collab.SessionRef{
		TenantID: tenantA, SessionID: uuid.New(), BotID: "captain", State: collab.SessionActive,
		CreatedAt: time.Now().UTC(),
	}
	parentB := collab.SessionRef{
		TenantID: tenantB, SessionID: uuid.New(), BotID: "captain", State: collab.SessionActive,
		CreatedAt: time.Now().UTC(),
	}
	if err := sessions.Put(ctx, parentA); err != nil {
		t.Fatalf("put parent A: %v", err)
	}
	if err := sessions.Put(ctx, parentB); err != nil {
		t.Fatalf("put parent B: %v", err)
	}
	audit := &fakeAudit{}
	host, err := collab.NewHost(collab.HostOptions{
		Registries:  source,
		Work:        work,
		Sessions:    sessions,
		Permissions: &fakePermissions{},
		Audit:       audit,
		Launcher:    &fakeLauncher{},
		Now:         func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	itemA, err := host.OpenWorkItem(ctx, tenantA, tenantModeID(tenantA), collab.WorkItemRequest{
		CallerSessionID: parentA.SessionID, Title: "tenant A work",
	})
	if err != nil || itemA.TenantID != tenantA || itemA.ModeID != tenantModeID(tenantA) {
		t.Fatalf("tenant A work = %+v, %v", itemA, err)
	}
	itemB, err := host.OpenWorkItem(ctx, tenantB, tenantModeID(tenantB), collab.WorkItemRequest{
		CallerSessionID: parentB.SessionID, Title: "tenant B work",
	})
	if err != nil || itemB.TenantID != tenantB || itemB.ModeID != tenantModeID(tenantB) {
		t.Fatalf("tenant B work = %+v, %v", itemB, err)
	}
	// Tenant B's registry does not know tenant A's mode.
	if _, err := host.OpenWorkItem(ctx, tenantB, tenantModeID(tenantA), collab.WorkItemRequest{
		CallerSessionID: parentB.SessionID, Title: "wrong catalogue",
	}); !errors.Is(err, collab.ErrUnknownMode) {
		t.Fatalf("cross-tenant mode = %v, want ErrUnknownMode", err)
	}
}

func TestHostRejectsUnknownRequestTenant(t *testing.T) {
	ctx := context.Background()
	known, unknown := uuid.New(), uuid.New()
	source, err := collab.NewTenantRegistries(func(_ context.Context, tenantID uuid.UUID) (*collab.ModeRegistry, error) {
		if tenantID != known {
			return nil, errors.New("tenant is not configured")
		}
		return testTenantRegistry(t, tenantID), nil
	})
	if err != nil {
		t.Fatalf("NewTenantRegistries: %v", err)
	}
	audit := &fakeAudit{}
	host, err := collab.NewHost(collab.HostOptions{
		Registries:  source,
		Work:        collab.NewMemoryWorkStore(nil),
		Sessions:    collab.NewMemorySessionIndex(),
		Permissions: &fakePermissions{},
		Audit:       audit,
		Launcher:    &fakeLauncher{},
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	if _, err := host.OpenWorkItem(ctx, unknown, tenantModeID(unknown), collab.WorkItemRequest{
		CallerSessionID: uuid.New(), Title: "unknown tenant",
	}); !errors.Is(err, collab.ErrUnknownTenant) {
		t.Fatalf("unknown tenant = %v, want ErrUnknownTenant", err)
	}
	event, ok := audit.find(collab.AuditInvalidCreation)
	if !ok || event.TenantID != unknown || event.Result != "denied" {
		t.Fatalf("denial audit = %+v, found=%v, want the request tenant", event, ok)
	}
}
