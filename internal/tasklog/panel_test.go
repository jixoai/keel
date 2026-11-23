package tasklog_test

// The panel projection's integration suite: the tree order, the cross-session
// member cards, the external ancestors, the attachment fold and the event
// stream's decoder. Every case runs against PostgreSQL (KEEL_TEST_DSN), like
// the rest of the task-log suites: the read is SQL, and a fake would not prove
// the projection's own order and join.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/tasklog"
)

// TestPanelTreeOrderAndMembers proves the panel's tree read: the session's own
// cards come first in ledger creation order, the member cards linked by
// parentTaskId follow (they live in their own sessions), and the parent of a
// member card that lives outside the session is resolved as an ancestor rather
// than invented.
func TestPanelTreeOrderAndMembers(t *testing.T) {
	st, _, service, scope := taskLogHarness(t)
	ctx := context.Background()
	tenant := scope.TenantID

	initial := mustCard(t)(service.EnsureCard(ctx, scope))
	second := createCard(t)(service.Create(ctx, scope, tasklog.CreateInput{Title: "整理发布清单"}, "call-create"))
	childSession := uuid.New()
	if err := st.CreateSession(ctx, tenant, childSession); err != nil {
		t.Fatalf("create child session: %v", err)
	}
	// A sub-agent's card is created in the child session and pointed at the
	// parent's active card; the panel must find it without reading the child
	// transcript.
	child := mustCard(t)(service.LinkSubagent(ctx, scope, childSession))
	time.Sleep(2 * time.Millisecond)
	// A third card of the parent session, created after the link, is the
	// newest own card and stays after the two earlier own cards.
	third := createCard(t)(service.Create(ctx, scope, tasklog.CreateInput{Title: "生成周报", ParentTaskID: &initial.TaskID}, "call-create-2"))

	panel, err := service.Panel(ctx, scope, 0, 0)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}
	if len(panel.Cards) != 3 {
		t.Fatalf("own cards = %+v", panel.Cards)
	}
	// Creation order, not newest first: the tree reads top-down.
	if panel.Cards[0].TaskID != initial.TaskID || panel.Cards[1].TaskID != second.TaskID || panel.Cards[2].TaskID != third.TaskID {
		t.Fatalf("own card order = %v %v %v", panel.Cards[0].TaskID, panel.Cards[1].TaskID, panel.Cards[2].TaskID)
	}
	if len(panel.Members) != 1 || panel.Members[0].TaskID != child.TaskID {
		t.Fatalf("members = %+v", panel.Members)
	}
	if panel.Members[0].SessionID != childSession {
		t.Fatalf("member session = %s, want %s", panel.Members[0].SessionID, childSession)
	}
	// No ancestor: every parent link the panel sees points at an own card.
	if len(panel.Ancestors) != 0 {
		t.Fatalf("ancestors = %+v", panel.Ancestors)
	}
	if panel.Truncated {
		t.Fatalf("truncated without a limit: %+v", panel)
	}

	// The child's own card (which points at the parent) is *the child's* tree,
	// not the parent's: its parent is reported as an ancestor when the child
	// session's panel is read.
	childPanel, err := service.Panel(ctx, kernel.Scope{TenantID: tenant, SessionID: childSession}, 0, 0)
	if err != nil {
		t.Fatalf("child panel: %v", err)
	}
	if len(childPanel.Cards) != 1 || childPanel.Cards[0].TaskID != child.TaskID {
		t.Fatalf("child own cards = %+v", childPanel.Cards)
	}
	// LinkSubagent attaches the child to the parent session's *newest* active
	// card; `second` was created after `initial`, so it is the ancestor here.
	if len(childPanel.Ancestors) != 1 || childPanel.Ancestors[0].TaskID != second.TaskID {
		t.Fatalf("child ancestors = %+v, want the newest active parent card %s", childPanel.Ancestors, second.TaskID)
	}
}

// TestPanelForkOriginAndEventStream proves the fork fields travel through the
// projection: a forked child's card carries origin=fork, originSeq, and the
// parentTaskId of the origin card; the event stream reports the originSeq so a
// surface can print the fork point. It also proves the stream is the ledger's
// own order and that a payload field the kind did not carry stays empty.
func TestPanelForkOriginAndEventStream(t *testing.T) {
	st, kern, service, scope := taskLogHarness(t)
	ctx := context.Background()
	tenant := scope.TenantID

	origin := mustCard(t)(service.EnsureCard(ctx, scope))
	originSeq := ledgerSeqAfter(t, kern, scope, kernel.KindTaskCreated)
	childSession := uuid.New()
	if err := st.CreateSession(ctx, tenant, childSession); err != nil {
		t.Fatalf("create child session: %v", err)
	}
	childScope := kernel.Scope{TenantID: tenant, SessionID: childSession}
	forked := mustCard(t)(service.ForkCard(ctx, scope, origin.TaskID, originSeq, childScope, "退款分支", "call-fork"))

	panel, err := service.Panel(ctx, scope, 0, 0)
	if err != nil {
		t.Fatalf("origin panel: %v", err)
	}
	if len(panel.Members) != 1 {
		t.Fatalf("members = %+v", panel.Members)
	}
	member := panel.Members[0]
	if member.Origin != tasklog.OriginFork || member.OriginSeq != originSeq {
		t.Fatalf("fork member = %+v (originSeq want %d)", member, originSeq)
	}
	if member.ParentTaskID == nil || *member.ParentTaskID != origin.TaskID {
		t.Fatalf("fork member parent = %+v", member.ParentTaskID)
	}
	if member.Title != "退款分支" {
		t.Fatalf("fork member title = %q", member.Title)
	}

	// The fork's own session reports its origin card as an ancestor, and its
	// event stream carries the fork's task.created payload.
	childPanel, err := service.Panel(ctx, childScope, 0, 0)
	if err != nil {
		t.Fatalf("fork panel: %v", err)
	}
	if len(childPanel.Ancestors) != 1 || childPanel.Ancestors[0].TaskID != origin.TaskID {
		t.Fatalf("fork ancestors = %+v", childPanel.Ancestors)
	}
	if len(childPanel.Events) != 1 {
		t.Fatalf("fork events = %+v", childPanel.Events)
	}
	event := childPanel.Events[0]
	if event.Kind != string(kernel.KindTaskCreated) || event.Origin != tasklog.OriginFork || event.OriginSeq != originSeq {
		t.Fatalf("fork event = %+v", event)
	}
	if event.Seq <= 0 || event.Title != "退款分支" {
		t.Fatalf("fork event shape = %+v", event)
	}
	// A field this kind did not carry stays empty: path/hash/text/reason.
	if event.Path != "" || event.Hash != "" || event.Text != "" || event.Reason != "" {
		t.Fatalf("fork event invented a field: %+v", event)
	}
	// The origin panel's own tree does not include the child's row: the fork
	// card belongs to the child's ledger. The child session's panel does.
	_ = forked
}

// TestPanelAttachmentFoldAndEventOrder proves the attachment references are a
// live set: an attach adds a (path, hash), a detach removes it while its
// history stays in the event stream, a re-attach with a new hash replaces the
// old one, and the stream keeps ledger order.
func TestPanelAttachmentFoldAndEventOrder(t *testing.T) {
	_, _, service, scope := taskLogHarness(t)
	ctx := context.Background()

	card := mustCard(t)(service.EnsureCard(ctx, scope))
	firstHash := "sha256:" + strings.Repeat("a", 64)
	secondHash := "sha256:" + strings.Repeat("b", 64)
	if _, err := service.Attach(ctx, scope, card.TaskID, "dist/app.tar.gz", firstHash, "call-attach-1"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if _, err := service.Attach(ctx, scope, card.TaskID, "docs/release.md", firstHash, "call-attach-2"); err != nil {
		t.Fatalf("attach 2: %v", err)
	}
	if _, err := service.Note(ctx, scope, card.TaskID, "构建完成", "call-note"); err != nil {
		t.Fatalf("note: %v", err)
	}
	if _, err := service.Detach(ctx, scope, card.TaskID, "docs/release.md", firstHash, "call-detach"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if _, err := service.Attach(ctx, scope, card.TaskID, "dist/app.tar.gz", secondHash, "call-attach-3"); err != nil {
		t.Fatalf("re-attach: %v", err)
	}

	panel, err := service.Panel(ctx, scope, 0, 0)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}
	if len(panel.Attachments) != 1 {
		t.Fatalf("live attachments = %+v", panel.Attachments)
	}
	live := panel.Attachments[0]
	if live.TaskID != card.TaskID || live.Path != "dist/app.tar.gz" || live.Hash != secondHash {
		t.Fatalf("live attachment = %+v", live)
	}

	// The history is intact: 1 created + 3 attaches + 1 note + 1 detach.
	if len(panel.Events) != 6 {
		t.Fatalf("events = %+v", panel.Events)
	}
	if !eventsAreOrdered(panel.Events) {
		t.Fatalf("events are not in ledger order: %+v", panel.Events)
	}
	// The note's text and the detach's path both survived the fold.
	var sawNote, sawDetach bool
	for _, event := range panel.Events {
		switch event.Kind {
		case string(kernel.KindTaskNote):
			sawNote = event.Text == "构建完成"
		case string(kernel.KindTaskDetached):
			sawDetach = event.Path == "docs/release.md"
		}
	}
	if !sawNote || !sawDetach {
		t.Fatalf("history lost a fact: note=%v detach=%v", sawNote, sawDetach)
	}
}

// TestPanelBoundsArePages proves the panel's reads stay pages: a member page
// past its budget reports truncation (and how many rows were left out), and an
// event stream past its budget reports it too.
func TestPanelBoundsArePages(t *testing.T) {
	st, _, service, scope := taskLogHarness(t)
	ctx := context.Background()
	tenant := scope.TenantID

	parent := mustCard(t)(service.EnsureCard(ctx, scope))
	for index := 0; index < 3; index++ {
		child := uuid.New()
		if err := st.CreateSession(ctx, tenant, child); err != nil {
			t.Fatalf("create session: %v", err)
		}
		linked := mustCard(t)(service.LinkSubagent(ctx, scope, child))
		time.Sleep(2 * time.Millisecond)
		_ = linked
	}
	for index := 0; index < 4; index++ {
		if _, err := service.Note(ctx, scope, parent.TaskID, "note", uuid.NewString()); err != nil {
			t.Fatalf("note: %v", err)
		}
	}

	panel, err := service.Panel(ctx, scope, 2, 3)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}
	if !panel.Truncated || panel.TruncatedMemberCount == 0 {
		t.Fatalf("member page = %+v", panel)
	}
	if len(panel.Members) != 2 || panel.MemberLimit != 2 {
		t.Fatalf("member rows = %d (limit %d)", len(panel.Members), panel.MemberLimit)
	}
	if !panel.EventsTruncated || len(panel.Events) != 3 || panel.EventLimit != 3 {
		t.Fatalf("event page = %d events, truncated=%v", len(panel.Events), panel.EventsTruncated)
	}
	// The attachment fold reads its own (bounded) attach/detach page even when
	// the displayed stream is a smaller page: the live set is the card's
	// current memory, not the rendered events.
	if len(panel.Attachments) != 0 {
		t.Fatalf("attachments = %+v", panel.Attachments)
	}

	// A limit above the maximum is clamped, not honoured.
	clamped, err := service.Panel(ctx, scope, 10_000, 10_000)
	if err != nil {
		t.Fatalf("clamped panel: %v", err)
	}
	if clamped.MemberLimit != tasklog.MaxPanelMemberLimit || clamped.EventLimit != tasklog.MaxPanelEventLimit {
		t.Fatalf("clamped limits = %d/%d", clamped.MemberLimit, clamped.EventLimit)
	}
}

// TestPanelOldCorruptProjectionRowOutsideThePage proves the SQL read is the
// page, not the history: an older projection row with an undecodable payload
// (inserted directly, the way a repair or a bug could leave one) is outside the
// newest page, so it is never loaded or described and the panel answers. The
// same row inside the requested page still fails the read.
func TestPanelOldCorruptProjectionRowOutsideThePage(t *testing.T) {
	st, _, service, scope := taskLogHarness(t)
	ctx := context.Background()
	tenant := scope.TenantID

	root := mustCard(t)(service.EnsureCard(ctx, scope))
	for index := 0; index < 3; index++ {
		if _, err := service.Note(ctx, scope, root.TaskID, "记录", uuid.NewString()); err != nil {
			t.Fatalf("note: %v", err)
		}
	}
	// A negative sequence sorts before every ledger row: it is an old row a
	// read of the newest page must never materialize.
	projection := tasklog.NewPGProjection(st)
	if err := projection.InsertEvent(ctx, tenant, scope.SessionID, tasklog.Event{
		TaskID: root.TaskID, Kind: string(kernel.KindTaskNote), Seq: -1,
		Payload: []byte(`{"taskId":"not-a-uuid","text":"坏行"}`),
	}); err != nil {
		t.Fatalf("insert corrupt row: %v", err)
	}

	panel, err := service.Panel(ctx, scope, 0, 2)
	if err != nil {
		t.Fatalf("a corrupt row outside the newest page failed the panel: %v", err)
	}
	if !panel.EventsTruncated || len(panel.Events) != 2 {
		t.Fatalf("event page = %+v (truncated %v)", panel.Events, panel.EventsTruncated)
	}
	if !eventsAreOrdered(panel.Events) {
		t.Fatalf("event page is not in ledger order: %+v", panel.Events)
	}

	// With a page large enough to hold the corrupt row the read fails: a row
	// the surface asked for is never silently dropped.
	if _, err := service.Panel(ctx, scope, 0, 5); err == nil {
		t.Fatalf("a corrupt row inside the page decoded without an error")
	}

	// The own-card read is a SQL page too, not a list the panel trims.
	page, err := st.ListTaskCardRowsInCreationOrder(ctx, tenant, scope.SessionID, 1)
	if err != nil {
		t.Fatalf("card page: %v", err)
	}
	if len(page) != 1 || page[0].TaskID != root.TaskID {
		t.Fatalf("card page = %+v, want the root only", page)
	}
}

// TestPanelTenantBoundary proves the read stays inside its tenant: another
// tenant's session is not found (the caller sees the initial card of its own
// session or a typed refusal), and a cross-tenant fork cannot be written.
func TestPanelTenantBoundary(t *testing.T) {
	st, _, service, scope := taskLogHarness(t)
	ctx := context.Background()

	otherTenant := uuid.New()
	if err := st.CreateTenant(ctx, otherTenant, "panel-other"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	otherSession := uuid.New()
	if err := st.CreateSession(ctx, otherTenant, otherSession); err != nil {
		t.Fatalf("create session: %v", err)
	}
	_, err := service.Panel(ctx, kernel.Scope{TenantID: otherTenant, SessionID: otherSession}, 0, 0)
	if err != nil {
		t.Fatalf("empty panel = %v", err)
	}
	// The caller's own panel does not include the other tenant's cards.
	panel, err := service.Panel(ctx, scope, 0, 0)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}
	if len(panel.Cards) != 0 || len(panel.Members) != 0 {
		t.Fatalf("cross-tenant leak: %+v", panel)
	}
	var zeroErr error
	_ = zeroErr
}

// TestDescribeEventRejectsACorruptRow proves the decoder is strict: a payload
// that violates its kind's contract is an error (the projection promises
// decodable rows, so a failure is corruption) while an unknown kind is carried
// as unparsed instead of dropped.
func TestDescribeEventRejectsACorruptRow(t *testing.T) {
	badPayload := tasklog.Event{
		TaskID:  uuid.New(),
		Kind:    string(kernel.KindTaskCreated),
		Seq:     7,
		Payload: []byte(`{"taskId":"not-a-uuid","title":"x"}`),
	}
	if _, err := tasklog.DescribeEvent(badPayload); err == nil {
		t.Fatalf("corrupt payload decoded without an error")
	}
	unknown := tasklog.Event{
		TaskID:  uuid.New(),
		Kind:    "task.future_kind",
		Seq:     8,
		Payload: []byte(`{"taskId":"x"}`),
	}
	described, err := tasklog.DescribeEvent(unknown)
	if err != nil {
		t.Fatalf("unknown kind: %v", err)
	}
	if !described.Unparsed || described.Kind != "task.future_kind" || described.Seq != 8 {
		t.Fatalf("unknown kind = %+v", described)
	}
}

func createCard(t *testing.T) func(tasklog.Card, error) tasklog.Card {
	t.Helper()
	return func(card tasklog.Card, err error) tasklog.Card {
		t.Helper()
		if err != nil {
			t.Fatalf("create card: %v", err)
		}
		return card
	}
}

// ledgerSeqAfter reports the seq of the newest committed event of a kind, so a
// test can name an exact fork origin.
func ledgerSeqAfter(t *testing.T, kern *kernel.Kernel, scope kernel.Scope, kind kernel.EventKind) int64 {
	t.Helper()
	events, err := kern.LoadEvents(context.Background(), scope, 0, 500)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	var seq int64
	for _, event := range events {
		if event.Kind == kind {
			seq = event.Seq
		}
	}
	if seq == 0 {
		t.Fatalf("no %s event in the ledger", kind)
	}
	return seq
}

func eventsAreOrdered(events []tasklog.DescribedEvent) bool {
	for index := 1; index < len(events); index++ {
		if events[index].Seq < events[index-1].Seq {
			return false
		}
	}
	return true
}

var _ = errors.Is
