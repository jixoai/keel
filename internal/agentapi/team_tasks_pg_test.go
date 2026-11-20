package agentapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/tasklog"
)

// TestTeamTasksProjectionAndBoundaries proves the member-task endpoint end to
// end against PostgreSQL: the captain's card and the member card are linked by
// parentTaskId while the member card stays in the child session's ledger, the
// page is keyset-paginated, and a foreign or missing session is a 404 (never an
// empty 200 that leaks the difference).
func TestTeamTasksProjectionAndBoundaries(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tenantA := uuid.New()
	tenantB := uuid.New()
	if err := st.CreateTenant(ctx, tenantA, "team-tasks-a"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, tenantB, "team-tasks-b"); err != nil {
		t.Fatal(err)
	}

	parentA := createTeamTaskSession(t, st, ctx, tenantA)
	emptyA := createTeamTaskSession(t, st, ctx, tenantA)
	childA1 := createTeamTaskSession(t, st, ctx, tenantA)
	childA2 := createTeamTaskSession(t, st, ctx, tenantA)
	parentB := createTeamTaskSession(t, st, ctx, tenantB)
	childB := createTeamTaskSession(t, st, ctx, tenantB)

	kernelLog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatalf("kernel: %v", err)
	}
	kern := kernel.New(kernelLog, kernel.Options{})
	service := tasklog.New(tasklog.Options{
		Committer:  kern,
		Reader:     kern,
		Projection: tasklog.NewPGProjection(st),
		Now:        func() time.Time { return time.Now().UTC() },
	})

	parentScope := kernel.Scope{TenantID: tenantA, SessionID: parentA}
	captain := mustTeamTaskCard(t)(service.EnsureCard(ctx, parentScope))
	if _, err := st.AppendEvent(ctx, tenantA, childA1, store.EventUserMessage, map[string]any{
		"text": "检索竞品资料", "requestId": "child-1",
	}); err != nil {
		t.Fatal(err)
	}
	first := mustTeamTaskCard(t)(service.LinkSubagent(ctx, parentScope, childA1))
	time.Sleep(5 * time.Millisecond)
	if _, err := st.AppendEvent(ctx, tenantA, childA2, store.EventUserMessage, map[string]any{
		"text": "整理发布清单", "requestId": "child-2",
	}); err != nil {
		t.Fatal(err)
	}
	second := mustTeamTaskCard(t)(service.LinkSubagent(ctx, parentScope, childA2))
	time.Sleep(5 * time.Millisecond)
	childAScope := kernel.Scope{TenantID: tenantA, SessionID: childA2}
	if _, err := service.Update(ctx, childAScope, second.TaskID, tasklog.UpdateInput{Status: teamTaskStatus("in_progress")}, "progress-2"); err != nil {
		t.Fatal(err)
	}

	// The rows really live in the child sessions, not in the captain's ledger.
	for _, child := range []uuid.UUID{childA1, childA2} {
		card, err := st.GetTaskCardRow(ctx, tenantA, child, tasklog.InitialTaskID(tenantA, child))
		if err != nil {
			t.Fatalf("child card %s: %v", child, err)
		}
		if card.SessionID != child {
			t.Fatalf("card session = %s, want child %s", card.SessionID, child)
		}
	}

	// A second tenant gets its own parent/child pair; the endpoint must never
	// mix them, and its answer for tenant A's session is a 404.
	parentBScope := kernel.Scope{TenantID: tenantB, SessionID: parentB}
	mustTeamTaskCard(t)(service.EnsureCard(ctx, parentBScope))
	mustTeamTaskCard(t)(service.LinkSubagent(ctx, parentBScope, childB))

	server := httptest.NewServer(New(Config{
		TeamTasks: service,
		SessionExists: func(ctx context.Context, tenantID, sessionID uuid.UUID) (bool, error) {
			_, exists, err := st.SessionOwner(ctx, tenantID, sessionID)
			return exists, err
		},
		Logger: testLogger(),
	}))
	defer server.Close()

	type teamTasksPage struct {
		Tasks      []TeamTaskView `json:"tasks"`
		NextCursor string         `json:"nextCursor"`
		HasMore    bool           `json:"hasMore"`
	}
	var page teamTasksPage
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions/"+parentA.String()+"/team-tasks", tenantA, nil, &page); code != 200 {
		t.Fatalf("team tasks: %d", code)
	}
	if len(page.Tasks) != 2 {
		t.Fatalf("tasks = %+v, want 2", page.Tasks)
	}
	byID := map[string]TeamTaskView{}
	for _, task := range page.Tasks {
		byID[task.TaskID] = task
	}
	firstView, ok := byID[first.TaskID.String()]
	if !ok {
		t.Fatalf("child card %s missing: %+v", first.TaskID, page.Tasks)
	}
	if firstView.ParentTaskID != captain.TaskID.String() {
		t.Fatalf("parentTaskId = %q, want %q", firstView.ParentTaskID, captain.TaskID)
	}
	if firstView.SessionID != childA1.String() {
		t.Fatalf("sessionId = %q, want %q", firstView.SessionID, childA1)
	}
	if firstView.Title != "检索竞品资料" || firstView.Status != tasklog.StatusOpen {
		t.Fatalf("first card = %+v", firstView)
	}
	if secondView := byID[second.TaskID.String()]; secondView.Status != tasklog.StatusInProgress {
		t.Fatalf("second card status = %q, want in_progress", secondView.Status)
	}

	// The captain session has no cards of its own in the answer (only children),
	// and a session without any card is the honest empty page.
	for _, task := range page.Tasks {
		if task.TaskID == captain.TaskID.String() {
			t.Fatalf("the captain card leaked into the member page: %+v", task)
		}
	}
	var empty teamTasksPage
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions/"+emptyA.String()+"/team-tasks", tenantA, nil, &empty); code != 200 {
		t.Fatalf("empty session: %d", code)
	}
	if len(empty.Tasks) != 0 || empty.HasMore {
		t.Fatalf("empty session page = %+v", empty)
	}

	// Keyset pagination: one card per page, no duplicate, and the cursor is
	// opaque on the wire.
	var firstPage teamTasksPage
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions/"+parentA.String()+"/team-tasks?limit=1", tenantA, nil, &firstPage); code != 200 {
		t.Fatalf("first page: %d", code)
	}
	if len(firstPage.Tasks) != 1 || !firstPage.HasMore || firstPage.NextCursor == "" {
		t.Fatalf("first page = %+v", firstPage)
	}
	var secondPage teamTasksPage
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions/"+parentA.String()+"/team-tasks?limit=1&after="+firstPage.NextCursor, tenantA, nil, &secondPage); code != 200 {
		t.Fatalf("second page: %d", code)
	}
	if len(secondPage.Tasks) != 1 || secondPage.HasMore {
		t.Fatalf("second page = %+v", secondPage)
	}
	if secondPage.Tasks[0].TaskID == firstPage.Tasks[0].TaskID {
		t.Fatalf("pagination repeated %s", firstPage.Tasks[0].TaskID)
	}
	if secondPage.Tasks[0].TaskID != first.TaskID.String() || firstPage.Tasks[0].TaskID != second.TaskID.String() {
		t.Fatalf("pages are not newest-first: %+v then %+v", firstPage.Tasks, secondPage.Tasks)
	}

	// The projection itself is tenant-scoped: even with the session id in hand,
	// a foreign tenant's read returns no cards (the tenant predicate plus RLS).
	if foreign, err := service.ListTeamTasks(ctx, tenantB, parentA, 50, ""); err != nil || len(foreign.Cards) != 0 {
		t.Fatalf("foreign-tenant projection = %+v, err = %v", foreign, err)
	}
	if foreign, err := service.ListTeamTasks(ctx, tenantA, parentB, 50, ""); err != nil || len(foreign.Cards) != 0 {
		t.Fatalf("cross-session projection = %+v, err = %v", foreign, err)
	}

	// Tenant and session boundaries.
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions/"+parentA.String()+"/team-tasks", tenantB, nil, nil); code != 404 {
		t.Fatalf("foreign tenant read = %d, want 404", code)
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions/"+parentB.String()+"/team-tasks", tenantA, nil, nil); code != 404 {
		t.Fatalf("other tenant's session read = %d, want 404", code)
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions/"+uuid.NewString()+"/team-tasks", tenantA, nil, nil); code != 404 {
		t.Fatalf("missing session read = %d, want 404", code)
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions/"+parentA.String()+"/team-tasks", uuid.Nil, nil, nil); code != 401 {
		t.Fatalf("missing tenant = %d, want 401", code)
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions/"+parentA.String()+"/team-tasks?after=garbage", tenantA, nil, nil); code != 400 {
		t.Fatalf("malformed cursor = %d, want 400", code)
	}
}

func createTeamTaskSession(t *testing.T, st *store.Store, ctx context.Context, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	sessionID := uuid.New()
	if err := st.CreateSession(ctx, tenantID, sessionID); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return sessionID
}

func mustTeamTaskCard(t *testing.T) func(tasklog.Card, error) tasklog.Card {
	t.Helper()
	return func(card tasklog.Card, err error) tasklog.Card {
		t.Helper()
		if err != nil {
			t.Fatalf("task log: %v", err)
		}
		return card
	}
}

func teamTaskStatus(value string) *string { return &value }
