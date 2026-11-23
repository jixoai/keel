package harnessapi

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"
)

// TestPGSessionQuerySearchScopeAndPagination pins the W12 search read: tenant
// scope, archived exclusion, first-message-derived titles, kind/session filters
// and keyset pagination without duplicates.
func TestPGSessionQuerySearchScopeAndPagination(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tenantID, otherTenant := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenantID, "query"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, otherTenant, "other"); err != nil {
		t.Fatal(err)
	}
	query := NewPGSessionQuery(st)

	sessionID := uuid.New()
	if err := st.CreateSession(ctx, tenantID, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, tenantID, sessionID, store.EventUserMessage, map[string]any{
		"text": "请分析退款原因", "requestId": "q1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, tenantID, sessionID, store.EventAssistantMessage, map[string]any{
		"role": "assistant", "text": "渠道 B 的退款率翻倍",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, tenantID, sessionID, store.EventToolResult, map[string]any{
		"text": "退款工具的原始输出不能出现在消息检索里",
	}); err != nil {
		t.Fatal(err)
	}

	unrelated := uuid.New()
	if err := st.CreateSession(ctx, tenantID, unrelated); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, tenantID, unrelated, store.EventUserMessage, map[string]any{
		"text": "整理发布清单", "requestId": "q2",
	}); err != nil {
		t.Fatal(err)
	}

	foreign := uuid.New()
	if err := st.CreateSession(ctx, otherTenant, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, otherTenant, foreign, store.EventUserMessage, map[string]any{
		"text": "请分析退款原因（另一个租户）", "requestId": "q3",
	}); err != nil {
		t.Fatal(err)
	}

	archived := uuid.New()
	if err := st.CreateSession(ctx, tenantID, archived); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, tenantID, archived, store.EventUserMessage, map[string]any{
		"text": "请分析退款原因（已归档）", "requestId": "q4",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ArchiveSession(ctx, tenantID, archived); err != nil {
		t.Fatal(err)
	}

	// Two hits in the live session; the tool result is not a message hit.
	page, err := query.SearchMessages(ctx, tenantID, MessageSearchFilter{Text: "退款", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Hits) != 2 || page.HasMore {
		t.Fatalf("hits = %+v", page)
	}
	for _, hit := range page.Hits {
		if hit.SessionID != sessionID.String() {
			t.Fatalf("foreign/archived hit leaked: %+v", hit)
		}
		if hit.SessionTitle != "请分析退款原因" {
			t.Fatalf("title = %q", hit.SessionTitle)
		}
		if hit.Snippet == "" {
			t.Fatalf("snippet is empty: %+v", hit)
		}
	}

	// The session filter narrows the same read.
	filtered, err := query.SearchMessages(ctx, tenantID, MessageSearchFilter{
		Text: "退款", SessionID: unrelated, HasSession: true, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Hits) != 0 {
		t.Fatalf("session filter ignored: %+v", filtered)
	}
	assistantOnly, err := query.SearchMessages(ctx, tenantID, MessageSearchFilter{
		Text: "退款", Kind: "assistant.message", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(assistantOnly.Hits) != 1 || assistantOnly.Hits[0].Role != "assistant" {
		t.Fatalf("kind filter = %+v", assistantOnly)
	}

	// One hit per page, keyset cursor: no duplicates, no omissions.
	seen := map[int64]bool{}
	cursor := ""
	pages := 0
	for {
		page, err := query.SearchMessages(ctx, tenantID, MessageSearchFilter{Text: "退款", Limit: 1, After: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, hit := range page.Hits {
			if seen[hit.Seq] {
				t.Fatalf("duplicate seq %d", hit.Seq)
			}
			seen[hit.Seq] = true
		}
		if !page.HasMore {
			break
		}
		if page.NextCursor == "" {
			t.Fatal("hasMore without cursor")
		}
		cursor = page.NextCursor
		if pages > 5 {
			t.Fatal("pagination did not terminate")
		}
	}
	if pages != 2 || len(seen) != 2 {
		t.Fatalf("pages=%d hits=%d", pages, len(seen))
	}

	// Session search matches the derived title and excludes archived/foreign.
	sessions, err := query.SearchSessions(ctx, tenantID, SessionSearchFilter{Text: "退款", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions.Hits) != 1 || sessions.Hits[0].SessionID != sessionID.String() {
		t.Fatalf("session hits = %+v", sessions.Hits)
	}
	if sessions.Hits[0].Title != "请分析退款原因" || sessions.Hits[0].Snippet == "" {
		t.Fatalf("session hit = %+v", sessions.Hits[0])
	}

	if _, err := query.SearchMessages(ctx, tenantID, MessageSearchFilter{Text: "不存在", Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := query.SearchMessages(ctx, tenantID, MessageSearchFilter{Text: "退款", Limit: 10, After: "garbage"}); err == nil {
		t.Fatal("a malformed cursor must fail")
	}
}

// TestPGSessionQueryExportIsBoundedAndTenantScoped pins the export read: text
// messages only, keyset by seq, a page never exceeds the caller's limit, and a
// foreign tenant sees nothing.
func TestPGSessionQueryExportIsBoundedAndTenantScoped(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tenantID, otherTenant := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenantID, "export"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, otherTenant, "other"); err != nil {
		t.Fatal(err)
	}
	query := NewPGSessionQuery(st)
	sessionID := uuid.New()
	if err := st.CreateSession(ctx, tenantID, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, tenantID, sessionID, store.EventUserMessage, map[string]any{
		"text": "第一行", "requestId": "e1",
	}); err != nil {
		t.Fatal(err)
	}
	// The runner persists an assistant message as {role, content:[{type,text}],
	// ...} — no top-level text. This is the shape the export must read.
	if _, err := st.AppendEvent(ctx, tenantID, sessionID, store.EventAssistantMessage, map[string]any{
		"role":    "assistant",
		"content": []map[string]any{{"type": "text", "text": "第二行"}},
	}); err != nil {
		t.Fatal(err)
	}
	// A tool-only assistant message carries no transcript text.
	if _, err := st.AppendEvent(ctx, tenantID, sessionID, store.EventAssistantMessage, map[string]any{
		"role":    "assistant",
		"content": []map[string]any{{"type": "toolCall", "name": "tasks", "arguments": map[string]any{"action": "create"}}},
	}); err != nil {
		t.Fatal(err)
	}
	// A legacy event with the old top-level text still exports.
	if _, err := st.AppendEvent(ctx, tenantID, sessionID, store.EventAssistantMessage, map[string]any{
		"role": "assistant", "text": "第三行",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, tenantID, sessionID, store.EventToolResult, map[string]any{
		"text": "tool payload must not be exported",
	}); err != nil {
		t.Fatal(err)
	}

	first, err := query.ExportMessages(ctx, tenantID, sessionID, ExportFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) != 1 || !first.HasMore || first.NextAfterSeq == 0 {
		t.Fatalf("first page = %+v", first)
	}
	if first.Title != "第一行" || first.Messages[0].Text != "第一行" {
		t.Fatalf("first page title/message = %+v", first)
	}
	second, err := query.ExportMessages(ctx, tenantID, sessionID, ExportFilter{AfterSeq: first.NextAfterSeq, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Messages) != 1 || !second.HasMore || second.NextAfterSeq <= first.NextAfterSeq {
		t.Fatalf("second page = %+v", second)
	}
	if second.Messages[0].Text != "第二行" || second.Messages[0].Kind != store.EventAssistantMessage {
		t.Fatalf("second message = %+v", second.Messages[0])
	}
	third, err := query.ExportMessages(ctx, tenantID, sessionID, ExportFilter{AfterSeq: second.NextAfterSeq, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Messages) != 1 || third.HasMore || third.Messages[0].Text != "第三行" {
		t.Fatalf("third page = %+v", third)
	}

	foreign, err := query.ExportMessages(ctx, otherTenant, sessionID, ExportFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(foreign.Messages) != 0 || foreign.Title != "" {
		t.Fatalf("foreign export leaked: %+v", foreign)
	}
}
