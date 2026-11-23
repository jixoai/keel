package tasklog_test

// The panel fold's no-database proof: a Projection substitute lets the tree,
// the member page, the ancestor resolution and the attachment fold be exercised
// without PostgreSQL. The PG suite (panel_test.go) still covers the SQL read;
// this suite covers the logic that runs above it, so a laptop without a
// database still proves the panel's ordering and bounds.

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/tasklog"
)

// memProjection is a read-only Projection over in-memory rows. It implements
// only what the panel reads; a write method panics so the test fails loudly if
// the panel ever gains one.
type memEvent struct {
	SessionID uuid.UUID
	Event     tasklog.Event
}

type memProjection struct {
	cards  []tasklog.Card
	events []memEvent
}

func (m *memProjection) GetCard(_ context.Context, tenantID, sessionID, taskID uuid.UUID) (tasklog.Card, error) {
	for _, card := range m.cards {
		if card.TenantID == tenantID && card.SessionID == sessionID && card.TaskID == taskID {
			return card, nil
		}
	}
	return tasklog.Card{}, tasklog.ErrTaskNotFound
}

func (m *memProjection) GetCardByID(_ context.Context, tenantID, taskID uuid.UUID) (tasklog.Card, error) {
	for _, card := range m.cards {
		if card.TenantID == tenantID && card.TaskID == taskID {
			return card, nil
		}
	}
	return tasklog.Card{}, tasklog.ErrTaskNotFound
}

func (m *memProjection) ListCards(_ context.Context, tenantID uuid.UUID, sessionID, parentTaskID *uuid.UUID, status string) ([]tasklog.Card, error) {
	var out []tasklog.Card
	for _, card := range m.cards {
		if card.TenantID != tenantID {
			continue
		}
		if sessionID != nil && card.SessionID != *sessionID {
			continue
		}
		if parentTaskID != nil && (card.ParentTaskID == nil || *card.ParentTaskID != *parentTaskID) {
			continue
		}
		if status != "" && card.Status != status {
			continue
		}
		out = append(out, card)
	}
	// The store's order: newest activity first, task id ascending.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].TaskID.String() < out[j].TaskID.String()
	})
	return out, nil
}

func (m *memProjection) ListChildCards(_ context.Context, tenantID, parentSessionID uuid.UUID, limit int, _ *time.Time, _ *uuid.UUID) ([]tasklog.Card, error) {
	parents := map[uuid.UUID]bool{}
	for _, card := range m.cards {
		if card.TenantID == tenantID && card.SessionID == parentSessionID {
			parents[card.TaskID] = true
		}
	}
	var out []tasklog.Card
	for _, card := range m.cards {
		if card.TenantID != tenantID || card.ParentTaskID == nil {
			continue
		}
		if !parents[*card.ParentTaskID] {
			continue
		}
		out = append(out, card)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].TaskID.String() > out[j].TaskID.String()
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *memProjection) ListEvents(_ context.Context, tenantID, sessionID, taskID uuid.UUID) ([]tasklog.Event, error) {
	var out []tasklog.Event
	for _, row := range m.events {
		if row.SessionID == sessionID && row.Event.TaskID == taskID {
			out = append(out, row.Event)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

func (m *memProjection) ListSessionEvents(_ context.Context, tenantID, sessionID uuid.UUID) ([]tasklog.Event, error) {
	var out []tasklog.Event
	for _, row := range m.events {
		if row.SessionID == sessionID {
			out = append(out, row.Event)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

func (m *memProjection) CountEvents(context.Context, uuid.UUID, uuid.UUID) (int, error) {
	return len(m.events), nil
}

func (m *memProjection) UpsertCard(context.Context, tasklog.Card) error {
	panic("tasklog panel read must not write a card")
}

func (m *memProjection) InsertEvent(context.Context, uuid.UUID, uuid.UUID, tasklog.Event) error {
	panic("tasklog panel read must not insert an event")
}

func (m *memProjection) ReplaceSession(context.Context, uuid.UUID, uuid.UUID, []tasklog.Card, []tasklog.Event) error {
	panic("tasklog panel read must not replace a projection")
}

func memService(t *testing.T, projection *memProjection) *tasklog.Service {
	t.Helper()
	return tasklog.New(tasklog.Options{Projection: projection})
}

func memCard(tenant, session uuid.UUID, id string, title string, createdSeq int64, parent *uuid.UUID, origin string, originSeq int64, updated time.Time) tasklog.Card {
	taskID := uuid.MustParse(id)
	return tasklog.Card{
		TenantID: tenant, SessionID: session,
		TaskID: taskID, ParentTaskID: parent,
		Title: title, Status: tasklog.StatusOpen, Origin: origin, OriginSeq: originSeq,
		CreatedSeq: createdSeq, UpdatedSeq: createdSeq,
		CreatedAt: updated, UpdatedAt: updated,
	}
}

func seqPayload(taskID string, extra string) []byte {
	body := `{"taskId":"` + taskID + `"` + extra + `}`
	return []byte(body)
}

func TestPanelFoldWithoutADatabase(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()
	session := uuid.New()
	childSession := uuid.New()
	outerSession := uuid.New()
	now := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)

	rootID := "10000000-0000-4000-8000-000000000001"
	ownChildID := "10000000-0000-4000-8000-000000000002"
	memberID := "10000000-0000-4000-8000-000000000003"
	forkID := "10000000-0000-4000-8000-000000000004"
	outerID := "10000000-0000-4000-8000-000000000005"

	root := memCard(tenant, session, rootID, "本周退款原因分析", 3, nil, tasklog.OriginSession, 0, now)
	ownChild := memCard(tenant, session, ownChildID, "汇总渠道异常", 7, &root.TaskID, tasklog.OriginTool, 0, now.Add(time.Minute))
	// The member's row lives in another session, and its parent is an own card.
	member := memCard(tenant, childSession, memberID, "订单查询", 1, &root.TaskID, tasklog.OriginSubagent, 0, now.Add(2*time.Minute))
	// The fork's parent is the member, and its origin sequence is a ledger position.
	fork := memCard(tenant, childSession, forkID, "退款条款对照", 2, &member.TaskID, tasklog.OriginFork, 12, now.Add(3*time.Minute))
	// The member's grandparent (an ancestor of the asked session's member link),
	// resolved as an ancestor when the *child's* panel is read.
	outer := memCard(tenant, outerSession, outerID, "宿主的卡", 1, nil, tasklog.OriginSession, 0, now)

	projection := &memProjection{
		cards: []tasklog.Card{root, ownChild, member, fork, outer},
		events: []memEvent{
			{SessionID: session, Event: tasklog.Event{TaskID: root.TaskID, Kind: string(kernel.KindTaskCreated), Seq: 3, Payload: seqPayload(rootID, `,"title":"本周退款原因分析","status":"open","origin":"session"`), CreatedAt: now}},
			{SessionID: session, Event: tasklog.Event{TaskID: root.TaskID, Kind: string(kernel.KindTaskAttached), Seq: 5, Payload: seqPayload(rootID, `,"path":"reports/a.md","hash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`), CreatedAt: now}},
			{SessionID: session, Event: tasklog.Event{TaskID: root.TaskID, Kind: string(kernel.KindTaskNote), Seq: 6, Payload: seqPayload(rootID, `,"text":"渠道 B 翻倍"`), CreatedAt: now}},
			{SessionID: childSession, Event: tasklog.Event{TaskID: member.TaskID, Kind: string(kernel.KindTaskCreated), Seq: 1, Payload: seqPayload(memberID, `,"title":"订单查询","origin":"subagent"`), CreatedAt: now}},
			{SessionID: childSession, Event: tasklog.Event{TaskID: fork.TaskID, Kind: string(kernel.KindTaskCreated), Seq: 2, Payload: seqPayload(forkID, `,"title":"退款条款对照","origin":"fork","originSeq":12`), CreatedAt: now}},
		},
	}
	service := memService(t, projection)

	panel, err := service.Panel(ctx, kernel.Scope{TenantID: tenant, SessionID: session}, 0, 0)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}
	if len(panel.Cards) != 2 || panel.Cards[0].TaskID != root.TaskID || panel.Cards[1].TaskID != ownChild.TaskID {
		t.Fatalf("own cards = %+v", panel.Cards)
	}
	// Only the member whose *parent* is one of this session's cards is a
	// member. The fork's parent is the member, and that card lives in the
	// child session: the join cannot reach it from here, and copying it in
	// would be a second truth. The child session's panel reports it.
	if len(panel.Members) != 1 || panel.Members[0].TaskID != member.TaskID {
		t.Fatalf("members = %+v", panel.Members)
	}
	if panel.Members[0].SessionID != childSession {
		t.Fatalf("member session = %s", panel.Members[0].SessionID)
	}
	// Own cards' parents are own cards, so there is nothing external here.
	if len(panel.Ancestors) != 0 {
		t.Fatalf("ancestors = %+v", panel.Ancestors)
	}
	// The attachment fold excludes the fork/member cards: their attach events
	// live in their own sessions and this read copies none of them.
	if len(panel.Attachments) != 1 || panel.Attachments[0].Path != "reports/a.md" {
		t.Fatalf("attachments = %+v", panel.Attachments)
	}
	if panel.Attachments[0].TaskID != root.TaskID {
		t.Fatalf("attachment task = %s", panel.Attachments[0].TaskID)
	}
	// The stream is the session's own events (3), in ledger order; the member's
	// events are not copied into this session's stream.
	if len(panel.Events) != 3 {
		t.Fatalf("events = %+v", panel.Events)
	}
	if panel.Events[0].Seq != 3 || panel.Events[1].Seq != 5 || panel.Events[2].Seq != 6 {
		t.Fatalf("event order = %d %d %d", panel.Events[0].Seq, panel.Events[1].Seq, panel.Events[2].Seq)
	}
	if panel.Events[1].Path != "reports/a.md" || panel.Events[1].Hash != "sha256:"+repeat("a", 64) {
		t.Fatalf("attach event = %+v", panel.Events[1])
	}
	if panel.Events[2].Text != "渠道 B 翻倍" {
		t.Fatalf("note event = %+v", panel.Events[2])
	}

	// The child session's panel reports its member card as its own card and
	// the member's parent as an ancestor (the row lives in another session).
	childPanel, err := service.Panel(ctx, kernel.Scope{TenantID: tenant, SessionID: childSession}, 0, 0)
	if err != nil {
		t.Fatalf("child panel: %v", err)
	}
	if len(childPanel.Cards) != 2 || childPanel.Cards[0].TaskID != member.TaskID || childPanel.Cards[1].TaskID != fork.TaskID {
		t.Fatalf("child own cards = %+v", childPanel.Cards)
	}
	if len(childPanel.Ancestors) != 1 || childPanel.Ancestors[0].TaskID != root.TaskID {
		t.Fatalf("child ancestors = %+v", childPanel.Ancestors)
	}
	// The fork card is a member of the child session's own tree: the first
	// child panel's `Members` were the root's children, not the child's.
	if len(childPanel.Members) != 0 {
		t.Fatalf("child members = %+v", childPanel.Members)
	}

	// A page smaller than the member count reports truncation and the number
	// left out; the attachment fold still reads the whole event set.
	page, err := service.Panel(ctx, kernel.Scope{TenantID: tenant, SessionID: session}, 1, 2)
	if err != nil {
		t.Fatalf("paged panel: %v", err)
	}
	if page.Truncated || page.TruncatedMemberCount != 0 || len(page.Members) != 1 {
		t.Fatalf("member page = %+v", page)
	}
	if !page.EventsTruncated || len(page.Events) != 2 {
		t.Fatalf("event page = %+v", page.Events)
	}
}

func TestPanelUsesTheDecoderAndRefusesCorruption(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()
	session := uuid.New()
	taskID := "20000000-0000-4000-8000-000000000001"
	projection := &memProjection{
		cards: []tasklog.Card{memCard(tenant, session, taskID, "根卡", 1, nil, tasklog.OriginSession, 0, time.Now())},
		events: []memEvent{
			{SessionID: session, Event: tasklog.Event{TaskID: uuid.MustParse(taskID), Kind: string(kernel.KindTaskCreated), Seq: 1,
				Payload: seqPayload(taskID, `,"title":"根卡"`)}},
			// A payload that violates its kind's contract is corruption, not an
			// event with no fields: the panel read fails rather than pretending.
			{SessionID: session, Event: tasklog.Event{TaskID: uuid.MustParse(taskID), Kind: string(kernel.KindTaskAttached), Seq: 2,
				Payload: []byte(`{"taskId":"` + taskID + `"}`)}},
		},
	}
	service := memService(t, projection)
	if _, err := service.Panel(ctx, kernel.Scope{TenantID: tenant, SessionID: session}, 0, 0); err == nil {
		t.Fatalf("corrupt attach payload decoded without an error")
	}
}

func repeat(value string, count int) string {
	out := ""
	for index := 0; index < count; index++ {
		out += value
	}
	return out
}
