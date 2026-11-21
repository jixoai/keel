package tasklog_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/tasklog"
)

// The task log's integration suites need a real PostgreSQL (KEEL_TEST_DSN),
// like the other store/kernel integration tests: the projection is SQL, and a
// fake would not prove that rebuilds converge. They skip otherwise, so
// `go test` on a laptop stays green.

func taskLogHarness(t *testing.T) (*store.Store, *kernel.Kernel, *tasklog.Service, kernel.Scope) {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	tenant := uuid.New()
	session := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "tasklog-"+tenant.String()[:8]); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatalf("create session: %v", err)
	}
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
	return st, kern, service, kernel.Scope{TenantID: tenant, SessionID: session}
}

func mustCard(t *testing.T) func(tasklog.Card, error) tasklog.Card {
	t.Helper()
	return func(card tasklog.Card, err error) tasklog.Card {
		t.Helper()
		if err != nil {
			t.Fatalf("task log: %v", err)
		}
		return card
	}
}

func stringPtr(value string) *string { return &value }

func TestInitialCardIsIdempotentAndAutoAttributed(t *testing.T) {
	_, kern, service, scope := taskLogHarness(t)
	ctx := context.Background()

	first := mustCard(t)(service.EnsureCard(ctx, scope))
	second := mustCard(t)(service.EnsureCard(ctx, scope))
	if first.TaskID != second.TaskID {
		t.Fatalf("initial card id is not deterministic: %s vs %s", first.TaskID, second.TaskID)
	}
	if want := tasklog.InitialTaskID(scope.TenantID, scope.SessionID); first.TaskID != want {
		t.Fatalf("initial card id = %s, want %s", first.TaskID, want)
	}
	if first.Origin != tasklog.OriginSession || first.Status != tasklog.StatusOpen {
		t.Fatalf("initial card = %+v", first)
	}

	// An absent taskId resolves to the same card; a malformed or foreign one is
	// a typed refusal instead of silent attribution.
	resolved, err := service.Resolve(ctx, scope, "")
	if err != nil || resolved.TaskID != first.TaskID {
		t.Fatalf("resolve empty = %+v, %v", resolved, err)
	}
	if _, err := service.Resolve(ctx, scope, "not-a-uuid"); !errors.Is(err, tasklog.ErrInvalidTaskID) {
		t.Fatalf("resolve malformed = %v, want ErrInvalidTaskID", err)
	}
	if _, err := service.Resolve(ctx, scope, uuid.NewString()); !errors.Is(err, tasklog.ErrTaskNotFound) {
		t.Fatalf("resolve missing = %v, want ErrTaskNotFound", err)
	}

	// Exactly one task.created reached the ledger (the second EnsureCard read the
	// projection instead of appending).
	events := ledgerTaskEvents(t, kern, scope)
	if len(events) != 1 || events[0].Kind != kernel.KindTaskCreated {
		t.Fatalf("ledger task events = %+v", events)
	}
}

func TestProjectionRebuildIsIdempotentAndRepairsStrayRows(t *testing.T) {
	st, kern, service, scope := taskLogHarness(t)
	ctx := context.Background()

	initial := mustCard(t)(service.EnsureCard(ctx, scope))
	child := mustCard(t)(service.Create(ctx, scope, tasklog.CreateInput{Title: "build dist"}, ""))
	if _, err := service.Update(ctx, scope, child.TaskID, tasklog.UpdateInput{
		Title:  stringPtr("build dist and docs"),
		Status: stringPtr(tasklog.StatusInProgress),
	}, "call-update"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := service.Attach(ctx, scope, child.TaskID, "dist/app.js", "sha256:aaa", "call-attach"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if _, err := service.Attach(ctx, scope, child.TaskID, "dist/app.css", "sha256:bbb", "call-attach-2"); err != nil {
		t.Fatalf("attach 2: %v", err)
	}
	if _, err := service.Detach(ctx, scope, child.TaskID, "dist/app.css", "", "call-detach"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if _, err := service.Note(ctx, scope, child.TaskID, "docs generated", "call-note"); err != nil {
		t.Fatalf("note: %v", err)
	}
	mustCard(t)(service.Close(ctx, scope, child.TaskID, "done", "call-close"))
	if len(ledgerTaskEvents(t, kern, scope)) != 8 {
		t.Fatalf("ledger task events before rebuild = %d, want 8", len(ledgerTaskEvents(t, kern, scope)))
	}

	first, err := service.RebuildSession(ctx, scope)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if first.Events != 8 || first.Cards != 2 {
		t.Fatalf("first rebuild stats = %+v", first)
	}
	second, err := service.RebuildSession(ctx, scope)
	if err != nil {
		t.Fatalf("second rebuild: %v", err)
	}
	if second != first {
		t.Fatalf("rebuild is not idempotent: %+v then %+v", first, second)
	}
	if count, err := st.CountTaskEvents(ctx, scope.TenantID, scope.SessionID); err != nil || count != 8 {
		t.Fatalf("projected events = %d, err = %v, want 8", count, err)
	}

	// Projection payloads equal the ledger's, event by event.
	view, err := service.Show(ctx, scope, child.TaskID)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	ledger := ledgerTaskEvents(t, kern, scope)
	var childLedger []kernel.Event
	for _, event := range ledger {
		var payload struct {
			TaskID string `json:"taskId"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.TaskID == child.TaskID.String() {
			childLedger = append(childLedger, event)
		}
	}
	if len(view.Events) != len(childLedger) {
		t.Fatalf("show events = %d, ledger = %d", len(view.Events), len(childLedger))
	}
	bySeq := map[int64]kernel.Event{}
	for _, event := range childLedger {
		bySeq[event.Seq] = event
	}
	for _, event := range view.Events {
		committed, ok := bySeq[event.Seq]
		if !ok || committed.Kind != kernel.EventKind(event.Kind) {
			t.Fatalf("projected event %d has no ledger fact: %+v", event.Seq, event)
		}
		if !sameJSON(t, committed.Payload, event.Payload) {
			t.Fatalf("event %d payload drifted:\n ledger=%s\n projection=%s", event.Seq, committed.Payload, event.Payload)
		}
	}

	// A row written around the ledger is erased by the next rebuild; that is the
	// enforceable form of "task_events must not be written directly".
	if err := st.InsertTaskEventRow(ctx, store.TaskEventRow{
		TenantID: scope.TenantID, SessionID: scope.SessionID, Seq: 1_000_000,
		TaskID: child.TaskID, Kind: string(kernel.KindTaskNote),
		Payload: json.RawMessage(`{"taskId":"` + child.TaskID.String() + `","text":"stray"}`),
	}); err != nil {
		t.Fatalf("stray write: %v", err)
	}
	if count, _ := st.CountTaskEvents(ctx, scope.TenantID, scope.SessionID); count != 9 {
		t.Fatalf("stray row count = %d, want 9", count)
	}
	if _, err := service.RebuildSession(ctx, scope); err != nil {
		t.Fatalf("repair rebuild: %v", err)
	}
	if count, _ := st.CountTaskEvents(ctx, scope.TenantID, scope.SessionID); count != 8 {
		t.Fatalf("stray row survived the rebuild: %d events", count)
	}

	// The current state is the projection: title from task.updated, closed last.
	card, err := service.Resolve(ctx, scope, child.TaskID.String())
	if err != nil {
		t.Fatalf("resolve child: %v", err)
	}
	if card.Title != "build dist and docs" || card.Status != tasklog.StatusClosed {
		t.Fatalf("projected card = %+v", card)
	}
	if card.ParentTaskID != nil {
		t.Fatalf("parentless card gained a parent: %+v", card.ParentTaskID)
	}
	if initial.TaskID == child.TaskID {
		t.Fatal("created card reused the initial id")
	}
}

func TestAttachDetachNoteAndCloseAreEvents(t *testing.T) {
	_, kern, service, scope := taskLogHarness(t)
	ctx := context.Background()
	card := mustCard(t)(service.Create(ctx, scope, tasklog.CreateInput{Title: "package"}, ""))

	if _, err := service.Attach(ctx, scope, card.TaskID, "dist/app.js", "sha256:aaa", "attach-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Attach(ctx, scope, card.TaskID, "dist/app.js", "sha256:aaa2", "attach-2"); err != nil {
		t.Fatal(err)
	}
	view, err := service.Show(ctx, scope, card.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Attachments) != 1 || view.Attachments[0].Hash != "sha256:aaa2" {
		t.Fatalf("replaced attachment = %+v", view.Attachments)
	}
	if _, err := service.Detach(ctx, scope, card.TaskID, "dist/app.js", "sha256:aaa2", "detach-1"); err != nil {
		t.Fatal(err)
	}
	view, err = service.Show(ctx, scope, card.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Attachments) != 0 {
		t.Fatalf("detach left attachments: %+v", view.Attachments)
	}
	if _, err := service.Note(ctx, scope, card.TaskID, "packaging finished", "note-1"); err != nil {
		t.Fatal(err)
	}
	closed := mustCard(t)(service.Close(ctx, scope, card.TaskID, "shipped", "close-1"))
	if closed.Status != tasklog.StatusClosed || closed.ClosedSeq == 0 {
		t.Fatalf("closed card = %+v", closed)
	}
	// Closing again is a no-op, not a second event.
	again := mustCard(t)(service.Close(ctx, scope, card.TaskID, "shipped", "close-2"))
	if again.ClosedSeq != closed.ClosedSeq {
		t.Fatalf("second close appended an event: %d -> %d", closed.ClosedSeq, again.ClosedSeq)
	}
	kinds := map[string]int{}
	for _, event := range ledgerTaskEvents(t, kern, scope) {
		kinds[string(event.Kind)]++
	}
	for _, want := range []string{
		string(kernel.KindTaskCreated), string(kernel.KindTaskAttached),
		string(kernel.KindTaskDetached), string(kernel.KindTaskNote), string(kernel.KindTaskClosed),
	} {
		if kinds[want] == 0 {
			t.Fatalf("ledger has no %s event: %+v", want, kinds)
		}
	}
	if kinds[string(kernel.KindTaskClosed)] != 1 {
		t.Fatalf("repeated close produced %d task.closed events", kinds[string(kernel.KindTaskClosed)])
	}
}

func TestForkCreatesANewCardPointingAtTheOrigin(t *testing.T) {
	st, _, service, scope := taskLogHarness(t)
	ctx := context.Background()
	origin := mustCard(t)(service.EnsureCard(ctx, scope))

	forkSession := uuid.New()
	if err := st.CreateSession(ctx, scope.TenantID, forkSession); err != nil {
		t.Fatalf("create fork session: %v", err)
	}
	forkScope := kernel.Scope{TenantID: scope.TenantID, SessionID: forkSession}
	forked := mustCard(t)(service.ForkCard(ctx, scope, origin.TaskID, 100, forkScope, "resource analysis (A/B)", "fork-1"))
	if forked.ParentTaskID == nil || *forked.ParentTaskID != origin.TaskID {
		t.Fatalf("fork parent = %+v, want %s", forked.ParentTaskID, origin.TaskID)
	}
	if forked.Origin != tasklog.OriginFork || forked.OriginSeq != 100 || forked.SessionID != forkSession {
		t.Fatalf("forked card = %+v", forked)
	}
	// The parent's children projection sees the fork across sessions.
	parentView, err := service.Show(ctx, scope, origin.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(parentView.Children) != 1 || parentView.Children[0].TaskID != forked.TaskID {
		t.Fatalf("parent children = %+v", parentView.Children)
	}
	// A rebuild of either session preserves the relation (it lives in the rows,
	// not in process memory).
	if _, err := service.RebuildSession(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RebuildSession(ctx, forkScope); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := service.Show(ctx, forkScope, forked.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Card.ParentTaskID == nil || *rebuilt.Card.ParentTaskID != origin.TaskID {
		t.Fatalf("fork parent lost after rebuild: %+v", rebuilt.Card)
	}
	// A fork never crosses tenants.
	otherTenant := uuid.New()
	if err := st.CreateTenant(ctx, otherTenant, "tasklog-other-"+otherTenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ForkCard(ctx, scope, origin.TaskID, 1,
		kernel.Scope{TenantID: otherTenant, SessionID: uuid.New()}, "", ""); !errors.Is(err, tasklog.ErrTaskNotFound) {
		t.Fatalf("cross-tenant fork = %v, want ErrTaskNotFound", err)
	}
}

func TestSubagentCardLinksToParentActiveCard(t *testing.T) {
	st, _, service, scope := taskLogHarness(t)
	ctx := context.Background()
	parent := mustCard(t)(service.EnsureCard(ctx, scope))

	childSession := uuid.New()
	if err := st.CreateSession(ctx, scope.TenantID, childSession); err != nil {
		t.Fatal(err)
	}
	// The child's first tool call may create its card before the hook runs.
	childScope := kernel.Scope{TenantID: scope.TenantID, SessionID: childSession}
	childCard := mustCard(t)(service.EnsureCard(ctx, childScope))
	if childCard.ParentTaskID != nil {
		t.Fatalf("a plain session card has a parent: %+v", childCard.ParentTaskID)
	}
	linked := mustCard(t)(service.LinkSubagent(ctx, scope, childSession))
	if linked.ParentTaskID == nil || *linked.ParentTaskID != parent.TaskID {
		t.Fatalf("linked child = %+v, want parent %s", linked.ParentTaskID, parent.TaskID)
	}
	// Idempotent: the link is not appended twice.
	again := mustCard(t)(service.LinkSubagent(ctx, scope, childSession))
	if again.ParentTaskID == nil || *again.ParentTaskID != parent.TaskID {
		t.Fatalf("second link = %+v", again)
	}
	view, err := service.Show(ctx, childScope, linked.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	updates := 0
	for _, event := range view.Events {
		if event.Kind == string(kernel.KindTaskUpdated) {
			updates++
		}
	}
	if updates != 1 {
		t.Fatalf("sub-agent link appended %d task.updated events, want 1", updates)
	}
}

func TestCrossTenantTaskAccessIsRefused(t *testing.T) {
	st, _, service, scope := taskLogHarness(t)
	ctx := context.Background()
	card := mustCard(t)(service.EnsureCard(ctx, scope))

	otherTenant := uuid.New()
	otherSession := uuid.New()
	if err := st.CreateTenant(ctx, otherTenant, "tasklog-foreign-"+otherTenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, otherTenant, otherSession); err != nil {
		t.Fatal(err)
	}
	foreign := kernel.Scope{TenantID: otherTenant, SessionID: otherSession}

	if _, err := service.Resolve(ctx, foreign, card.TaskID.String()); !errors.Is(err, tasklog.ErrTaskNotFound) {
		t.Fatalf("foreign resolve = %v, want ErrTaskNotFound", err)
	}
	if _, err := service.Show(ctx, foreign, card.TaskID); !errors.Is(err, tasklog.ErrTaskNotFound) {
		t.Fatalf("foreign show = %v, want ErrTaskNotFound", err)
	}
	if _, err := service.Close(ctx, foreign, card.TaskID, "", ""); !errors.Is(err, tasklog.ErrTaskNotFound) {
		t.Fatalf("foreign close = %v, want ErrTaskNotFound", err)
	}
	cards, err := service.List(ctx, foreign, tasklog.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 0 {
		t.Fatalf("foreign list = %+v", cards)
	}
	// The foreign session still has its own initial card, and it is a different
	// card from the other tenant's (same-session-id collisions are tenant-scoped).
	own := mustCard(t)(service.EnsureCard(ctx, foreign))
	if own.TaskID == card.TaskID {
		t.Fatal("two tenants share an initial card id")
	}
}

func TestParentCycleIsRefused(t *testing.T) {
	_, _, service, scope := taskLogHarness(t)
	ctx := context.Background()
	root := mustCard(t)(service.Create(ctx, scope, tasklog.CreateInput{Title: "root"}, ""))
	child := mustCard(t)(service.Create(ctx, scope, tasklog.CreateInput{Title: "child", ParentTaskID: &root.TaskID}, ""))
	if _, err := service.Update(ctx, scope, root.TaskID, tasklog.UpdateInput{ParentTaskID: &child.TaskID}, ""); !errors.Is(err, tasklog.ErrTaskCycle) {
		t.Fatalf("cycle update = %v, want ErrTaskCycle", err)
	}
	missing := uuid.New()
	if _, err := service.Create(ctx, scope, tasklog.CreateInput{Title: "orphan", ParentTaskID: &missing}, ""); !errors.Is(err, tasklog.ErrTaskNotFound) {
		t.Fatalf("missing parent = %v, want ErrTaskNotFound", err)
	}
}

func ledgerTaskEvents(t *testing.T, kern *kernel.Kernel, scope kernel.Scope) []kernel.Event {
	t.Helper()
	ctx := context.Background()
	var out []kernel.Event
	var after int64
	for {
		page, err := kern.LoadEvents(ctx, scope, after, 500)
		if err != nil {
			t.Fatalf("load events: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, event := range page {
			if tasklog.IsTaskKind(event.Kind) {
				out = append(out, event)
			}
		}
		after = page[len(page)-1].Seq
		if len(page) < 500 {
			break
		}
	}
	return out
}

func sameJSON(t *testing.T, left, right []byte) bool {
	t.Helper()
	var l, r any
	if err := json.Unmarshal(left, &l); err != nil {
		t.Fatalf("left payload: %v", err)
	}
	if err := json.Unmarshal(right, &r); err != nil {
		t.Fatalf("right payload: %v", err)
	}
	leftCanonical, _ := json.Marshal(l)
	rightCanonical, _ := json.Marshal(r)
	return string(leftCanonical) == string(rightCanonical)
}

// TestOriginCardResolvesTheLedgerPosition pins the fork origin card rule: the
// card of the newest task event at or before the origin, with the initial card
// as the empty-log fallback.
func TestOriginCardResolvesTheLedgerPosition(t *testing.T) {
	_, _, service, scope := taskLogHarness(t)
	ctx := context.Background()
	initial := mustCard(t)(service.EnsureCard(ctx, scope))
	second := mustCard(t)(service.Create(ctx, scope, tasklog.CreateInput{Title: "branch"}, ""))
	if _, err := service.Note(ctx, scope, second.TaskID, "at the origin", "call-note"); err != nil {
		t.Fatal(err)
	}

	origin, err := service.OriginCard(ctx, scope, 0)
	if err != nil || origin.TaskID != initial.TaskID {
		t.Fatalf("origin at 0 = %+v err=%v, want the initial card", origin, err)
	}
	origin, err = service.OriginCard(ctx, scope, second.CreatedSeq)
	if err != nil || origin.TaskID != second.TaskID {
		t.Fatalf("origin at create = %+v err=%v, want the created card", origin, err)
	}
	origin, err = service.OriginCard(ctx, scope, second.UpdatedSeq+10)
	if err != nil || origin.TaskID != second.TaskID {
		t.Fatalf("origin at tip = %+v err=%v, want the newest card", origin, err)
	}
}

// TestNoteSubagentFinishedLandsOnTheParentCard pins the return note: it lands
// on the card the child card points at (not a guessed active card), and a
// repeated finish does not duplicate it.
func TestNoteSubagentFinishedLandsOnTheParentCard(t *testing.T) {
	st, _, service, parent := taskLogHarness(t)
	ctx := context.Background()
	parentCard := mustCard(t)(service.EnsureCard(ctx, parent))
	child := uuid.New()
	if err := st.CreateSession(ctx, parent.TenantID, child); err != nil {
		t.Fatal(err)
	}
	childCard := mustCard(t)(service.LinkSubagent(ctx, parent, child))
	if childCard.ParentTaskID == nil || *childCard.ParentTaskID != parentCard.TaskID {
		t.Fatalf("child card parent = %+v", childCard.ParentTaskID)
	}
	if err := service.NoteSubagentFinished(ctx, parent, child, "done", "the child answer"); err != nil {
		t.Fatal(err)
	}
	if err := service.NoteSubagentFinished(ctx, parent, child, "done", "the child answer"); err != nil {
		t.Fatalf("repeated note: %v", err)
	}
	view, err := service.Show(ctx, parent, parentCard.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	notes := 0
	for _, event := range view.Events {
		if event.Kind != string(kernel.KindTaskNote) {
			continue
		}
		notes++
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(payload.Text, child.String()) || !strings.Contains(payload.Text, "the child answer") {
			t.Fatalf("note text = %q", payload.Text)
		}
	}
	if notes != 1 {
		t.Fatalf("parent notes = %d, want one idempotent note", notes)
	}
}

// TestForkCardIsIdempotentAndReopens pins the W8 fix: the forked card id is the
// child's deterministic initial card, the create key derives from the caller's
// stable callID, and a card closed by a rolled-back attempt is reopened on the
// retry instead of appending a second card.
func TestForkCardIsIdempotentAndReopens(t *testing.T) {
	st, _, service, scope := taskLogHarness(t)
	ctx := context.Background()
	origin := mustCard(t)(service.EnsureCard(ctx, scope))
	child := uuid.New()
	if err := st.CreateSession(ctx, scope.TenantID, child); err != nil {
		t.Fatal(err)
	}
	childScope := kernel.Scope{TenantID: scope.TenantID, SessionID: child}
	first := mustCard(t)(service.ForkCard(ctx, scope, origin.TaskID, 7, childScope, "branch", "fork-1"))
	wantID := tasklog.InitialTaskID(scope.TenantID, child)
	if first.TaskID != wantID {
		t.Fatalf("forked card id = %s, want the deterministic child card %s", first.TaskID, wantID)
	}
	second := mustCard(t)(service.ForkCard(ctx, scope, origin.TaskID, 7, childScope, "branch", "fork-1"))
	if second.TaskID != first.TaskID {
		t.Fatalf("retry card = %s, want %s", second.TaskID, first.TaskID)
	}
	// Even a different callID converges on the same child card: the child id,
	// not the call, is the fork's identity.
	third := mustCard(t)(service.ForkCard(ctx, scope, origin.TaskID, 7, childScope, "branch", "fork-2"))
	if third.TaskID != first.TaskID {
		t.Fatalf("different-call card = %s, want %s", third.TaskID, first.TaskID)
	}
	created := 0
	for _, event := range mustView(t)(service.Show(ctx, childScope, first.TaskID)).Events {
		if event.Kind == string(kernel.KindTaskCreated) {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("task.created events = %d, want one card", created)
	}

	// A rollback closes the card; the next fork retry reopens the same card.
	if _, err := service.Close(ctx, childScope, first.TaskID, "fork did not start", "close-1"); err != nil {
		t.Fatal(err)
	}
	reopened := mustCard(t)(service.ForkCard(ctx, scope, origin.TaskID, 7, childScope, "branch", "fork-1"))
	if reopened.TaskID != first.TaskID || reopened.Status != tasklog.StatusOpen {
		t.Fatalf("reopened card = %+v", reopened)
	}
	view := mustView(t)(service.Show(ctx, childScope, first.TaskID))
	created, closed, updates := 0, 0, 0
	for _, event := range view.Events {
		switch event.Kind {
		case string(kernel.KindTaskCreated):
			created++
		case string(kernel.KindTaskClosed):
			closed++
		case string(kernel.KindTaskUpdated):
			updates++
		}
	}
	if created != 1 || closed != 1 || updates != 1 {
		t.Fatalf("card events = created %d, closed %d, updated %d; want 1/1/1", created, closed, updates)
	}
	children := mustView(t)(service.Show(ctx, scope, origin.TaskID)).Children
	if len(children) != 1 || children[0].TaskID != first.TaskID {
		t.Fatalf("origin children = %+v", children)
	}
}

func mustView(t *testing.T) func(tasklog.View, error) tasklog.View {
	t.Helper()
	return func(view tasklog.View, err error) tasklog.View {
		t.Helper()
		if err != nil {
			t.Fatalf("task view: %v", err)
		}
		return view
	}
}
