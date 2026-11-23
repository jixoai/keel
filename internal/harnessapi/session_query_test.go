package harnessapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/fileplugin"
	"github.com/jixoai/keel/internal/layerfs"
)

type fakeSessionQuery struct {
	messages     MessageHitPage
	sessions     SessionSearchPage
	export       ExportPage
	messageErr   error
	sessionErr   error
	exportErr    error
	lastMessage  MessageSearchFilter
	lastSessions SessionSearchFilter
	lastExport   ExportFilter
}

func (f *fakeSessionQuery) SearchMessages(_ context.Context, _ uuid.UUID, filter MessageSearchFilter) (MessageHitPage, error) {
	f.lastMessage = filter
	return f.messages, f.messageErr
}

func (f *fakeSessionQuery) SearchSessions(_ context.Context, _ uuid.UUID, filter SessionSearchFilter) (SessionSearchPage, error) {
	f.lastSessions = filter
	return f.sessions, f.sessionErr
}

func (f *fakeSessionQuery) ExportMessages(_ context.Context, _ uuid.UUID, _ uuid.UUID, filter ExportFilter) (ExportPage, error) {
	f.lastExport = filter
	return f.export, f.exportErr
}

func searchServer(t *testing.T, configure func(*Config)) *httptest.Server {
	t.Helper()
	cfg := Config{Access: permitAllAccess(), Logger: testLogger()}
	if configure != nil {
		configure(&cfg)
	}
	server := httptest.NewServer(New(cfg))
	t.Cleanup(server.Close)
	return server
}

func TestSessionMessageSearchRoute(t *testing.T) {
	tenant := uuid.New()
	session := uuid.New()
	at := time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)
	query := &fakeSessionQuery{
		messages: MessageHitPage{
			Hits: []MessageHit{{
				SessionID: session.String(), SessionTitle: "退款原因分析", Seq: 7,
				Kind: "assistant.message", Role: "assistant", At: at, Snippet: "渠道 B 的退款率翻倍",
			}},
			HasMore: true, NextCursor: "next-page",
		},
	}
	server := searchServer(t, func(cfg *Config) { cfg.SessionSearch = query })

	var body struct {
		Scope      string       `json:"scope"`
		Query      string       `json:"query"`
		Messages   []MessageHit `json:"messages"`
		Count      int          `json:"count"`
		Limit      int          `json:"limit"`
		NextCursor string       `json:"nextCursor"`
		HasMore    bool         `json:"hasMore"`
	}
	path := server.URL + "/v1/sessions/search?q=" + url.QueryEscape("退款") +
		"&sessionId=" + session.String() + "&kind=assistant&limit=5"
	if code := doJSON(t, server.Client(), http.MethodGet, path, tenant, nil, &body); code != 200 {
		t.Fatalf("GET search = %d", code)
	}
	if body.Scope != "messages" || body.Query != "退款" || body.Count != 1 || body.Limit != 5 {
		t.Fatalf("body = %+v", body)
	}
	if body.NextCursor != "next-page" || !body.HasMore {
		t.Fatalf("pagination = %q %v", body.NextCursor, body.HasMore)
	}
	if body.Messages[0].SessionTitle != "退款原因分析" || body.Messages[0].Snippet != "渠道 B 的退款率翻倍" {
		t.Fatalf("hit = %+v", body.Messages[0])
	}
	if query.lastMessage.Text != "退款" || !query.lastMessage.HasSession ||
		query.lastMessage.SessionID != session || query.lastMessage.Kind != "assistant.message" ||
		query.lastMessage.Limit != 5 {
		t.Fatalf("filter = %+v", query.lastMessage)
	}
}

func TestSessionSearchRouteBoundsAndRefusals(t *testing.T) {
	tenant := uuid.New()
	query := &fakeSessionQuery{}
	server := searchServer(t, func(cfg *Config) { cfg.SessionSearch = query })
	client := server.Client()

	for _, probe := range []string{
		"/v1/sessions/search",
		"/v1/sessions/search?q=" + strings.Repeat("x", searchTextLimit+1),
		"/v1/sessions/search?q=x&scope=tools",
		"/v1/sessions/search?q=x&kind=system",
		"/v1/sessions/search?q=x&sessionId=not-a-uuid",
		"/v1/sessions/search?q=x&limit=0",
		"/v1/sessions/search?q=x&limit=101",
	} {
		if code := doJSON(t, client, http.MethodGet, server.URL+probe, tenant, nil, nil); code != 400 {
			t.Fatalf("%s = %d, want 400", probe, code)
		}
	}

	// A deployment without the seam or without the access relation fails
	// closed instead of searching the tenant.
	unwired := searchServer(t, nil)
	if code := doJSON(t, unwired.Client(), http.MethodGet, unwired.URL+"/v1/sessions/search?q=x", tenant, nil, nil); code != http.StatusNotImplemented {
		t.Fatalf("unwired search = %d, want 501", code)
	}
	noAccess := searchServer(t, func(cfg *Config) {
		cfg.SessionSearch = query
		cfg.Access = nil
	})
	if code := doJSON(t, noAccess.Client(), http.MethodGet, noAccess.URL+"/v1/sessions/search?q=x", tenant, nil, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("search without access = %d, want 503", code)
	}
}

func TestSessionSearchVisibilityFiltersHits(t *testing.T) {
	tenant := uuid.New()
	visible, hidden := uuid.New(), uuid.New()
	query := &fakeSessionQuery{messages: MessageHitPage{Hits: []MessageHit{
		{SessionID: visible.String(), SessionTitle: "可见", Seq: 2},
		{SessionID: hidden.String(), SessionTitle: "不可见", Seq: 1},
	}}}
	accessSeam := &fakeAccess{
		visible: access.VisibleResult{
			IDs:      []string{visible.String()},
			Decision: access.Decision{Allow: true, Key: access.KeySessionRead, Scope: access.ScopeOwn},
		},
	}
	server := searchServer(t, func(cfg *Config) {
		cfg.SessionSearch = query
		cfg.Access = accessSeam
	})
	var body struct {
		Messages []MessageHit `json:"messages"`
		Count    int          `json:"count"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions/search?q=x", tenant, nil, &body); code != 200 {
		t.Fatalf("search = %d", code)
	}
	if body.Count != 1 || body.Messages[0].SessionID != visible.String() {
		t.Fatalf("filtered = %+v", body)
	}

	denied := &fakeAccess{visible: access.VisibleResult{
		Decision: access.Decision{Allow: false, Key: access.KeySessionRead, Reason: access.ReasonMissingGrant},
	}}
	refused := searchServer(t, func(cfg *Config) {
		cfg.SessionSearch = query
		cfg.Access = denied
	})
	if code := doJSON(t, refused.Client(), http.MethodGet, refused.URL+"/v1/sessions/search?q=x", tenant, nil, nil); code != http.StatusForbidden {
		t.Fatalf("denied search = %d, want 403", code)
	}
}

func TestSessionSearchScopeSessions(t *testing.T) {
	tenant := uuid.New()
	session := uuid.New()
	query := &fakeSessionQuery{sessions: SessionSearchPage{Hits: []SessionHit{{
		SessionID: session.String(), Title: "发布准备周报", Snippet: "本周发布准备",
		CreatedAt: testTime, UpdatedAt: testTime,
	}}}}
	server := searchServer(t, func(cfg *Config) { cfg.SessionSearch = query })
	var body struct {
		Scope    string       `json:"scope"`
		Sessions []SessionHit `json:"sessions"`
		Count    int          `json:"count"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions/search?q=发布&scope=sessions", tenant, nil, &body); code != 200 {
		t.Fatalf("GET search sessions = %d", code)
	}
	if body.Scope != "sessions" || body.Count != 1 || body.Sessions[0].Title != "发布准备周报" {
		t.Fatalf("body = %+v", body)
	}
	if query.lastSessions.Text != "发布" || query.lastSessions.Limit != defaultSearchLimit {
		t.Fatalf("filter = %+v", query.lastSessions)
	}
}

func TestSessionExportRouteJSONAndNDJSON(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	at := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	query := &fakeSessionQuery{export: ExportPage{
		SessionID: session.String(),
		Title:     "发布准备周报",
		Messages: []ExportMessage{
			{Seq: 1, Kind: "user.message", Role: "user", At: at, Text: "帮我总结发布准备"},
			{Seq: 2, Kind: "assistant.message", Role: "assistant", At: at.Add(time.Second), Text: "已检查当前工作区"},
		},
		NextAfterSeq: 2,
		HasMore:      true,
	}}
	server := searchServer(t, func(cfg *Config) {
		cfg.SessionExport = query
		cfg.SessionExists = func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return true, nil }
	})
	base := server.URL + "/v1/sessions/" + session.String() + "/export"

	var body SessionExportView
	if code := doJSON(t, server.Client(), http.MethodGet, base+"?afterSeq=0&limit=10", tenant, nil, &body); code != 200 {
		t.Fatalf("GET export = %d", code)
	}
	if body.SessionID != session.String() || body.Title != "发布准备周报" || body.Count != 2 ||
		body.NextAfterSeq != 2 || !body.HasMore || body.Format != "json" {
		t.Fatalf("body = %+v", body)
	}
	if body.Messages[1].Role != "assistant" || body.Messages[1].Text != "已检查当前工作区" {
		t.Fatalf("message = %+v", body.Messages[1])
	}
	if query.lastExport.AfterSeq != 0 || query.lastExport.Limit != 10 {
		t.Fatalf("filter = %+v", query.lastExport)
	}

	response, err := server.Client().Do(exportRequest(t, base+"?format=ndjson&limit=10", tenant))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("ndjson = %d", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/x-ndjson") {
		t.Fatalf("content type = %q", got)
	}
	if got := response.Header.Get("X-Keel-Next-After-Seq"); got != "2" {
		t.Fatalf("next after = %q", got)
	}
	var lines []map[string]any
	decoder := json.NewDecoder(response.Body)
	for decoder.More() {
		var line map[string]any
		if err := decoder.Decode(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if len(lines) != 2 || lines[0]["text"] != "帮我总结发布准备" || lines[1]["role"] != "assistant" {
		t.Fatalf("ndjson lines = %+v", lines)
	}
}

func TestSessionExportRefusalsAndBounds(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	query := &fakeSessionQuery{}
	base := "/v1/sessions/" + session.String() + "/export"

	// A session that exists makes each malformed query a 400 (the existence
	// check owns the 404, so without this fixture every probe would be 404 and
	// the grammar branches would never run).
	server := searchServer(t, func(cfg *Config) {
		cfg.SessionExport = query
		cfg.SessionExists = func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return true, nil }
	})
	client := server.Client()
	for _, probe := range []string{"?format=xml", "?limit=0", "?limit=501", "?afterSeq=-1"} {
		if code := doJSON(t, client, http.MethodGet, server.URL+base+probe, tenant, nil, nil); code != 400 {
			t.Fatalf("%s = %d, want 400", probe, code)
		}
	}

	// A missing/foreign session is 404.
	missing := searchServer(t, func(cfg *Config) {
		cfg.SessionExport = query
		cfg.SessionExists = func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return false, nil }
	})
	if code := doJSON(t, missing.Client(), http.MethodGet, missing.URL+base, tenant, nil, nil); code != http.StatusNotFound {
		t.Fatalf("foreign export = %d, want 404", code)
	}

	unwired := searchServer(t, nil)
	if code := doJSON(t, unwired.Client(), http.MethodGet, unwired.URL+base, tenant, nil, nil); code != http.StatusNotImplemented {
		t.Fatalf("unwired export = %d, want 501", code)
	}
}

func TestBoundedExportPageHonoursByteAndMessageBounds(t *testing.T) {
	session := uuid.New()
	messages := make([]ExportMessage, 0, 20)
	for i := 0; i < 20; i++ {
		messages = append(messages, ExportMessage{
			Seq: int64(i + 1), Text: strings.Repeat("a", exportMessageBytes),
		})
	}
	view := boundedExportPage(session, ExportPage{Messages: messages}, maxExportLimit, 0)
	if view.Count == 0 || view.Count >= len(messages) {
		t.Fatalf("view kept %d of %d messages", view.Count, len(messages))
	}
	if !view.Truncated || !view.HasMore {
		t.Fatalf("byte bound not reported: %+v", view)
	}
	if view.NextAfterSeq != view.Messages[view.Count-1].Seq {
		t.Fatalf("next after = %d, last = %d", view.NextAfterSeq, view.Messages[view.Count-1].Seq)
	}
	for _, message := range view.Messages {
		if len(message.Text) > exportMessageBytes {
			t.Fatalf("message %d is %d bytes", message.Seq, len(message.Text))
		}
	}

	// One over-long message is cut and flagged, never silently shortened.
	overlong := boundedExportPage(session, ExportPage{Messages: []ExportMessage{{
		Seq: 1, Text: strings.Repeat("b", exportMessageBytes+100),
	}}}, maxExportLimit, 0)
	if overlong.Count != 1 || !overlong.Messages[0].TextTruncated ||
		len(overlong.Messages[0].Text) != exportMessageBytes {
		t.Fatalf("overlong = count %d truncated %v bytes %d",
			overlong.Count, overlong.Messages[0].TextTruncated, len(overlong.Messages[0].Text))
	}
}

func exportRequest(t *testing.T, rawURL string, tenant uuid.UUID) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Tenant-ID", tenant.String())
	return request
}

// --- deliverables ---------------------------------------------------------

func TestDeliverablesRouteReadsTheManifest(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	hash := layerfs.Hash("sha256:" + strings.Repeat("ab", 32))
	server := searchServer(t, func(cfg *Config) {
		cfg.SessionFiles = fakeSessionFiles{entries: []fileplugin.Entry{
			{Path: "src/b.ts", Size: 20, Mode: 0o644, Hash: hash, UpdatedSeq: 5, LastTaskID: "task-2"},
			{Path: "docs/a.md", Size: 10, Mode: 0o644, Hash: hash, UpdatedSeq: 4, LastTaskID: "task-1"},
		}}
		cfg.SessionExists = func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return true, nil }
	})
	var body struct {
		SessionID    string            `json:"sessionId"`
		Deliverables []DeliverableView `json:"deliverables"`
		Count        int               `json:"count"`
		HasMore      bool              `json:"hasMore"`
		NextCursor   string            `json:"nextCursor"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions/"+session.String()+"/deliverables", tenant, nil, &body); code != 200 {
		t.Fatalf("GET deliverables = %d", code)
	}
	if body.SessionID != session.String() || body.Count != 2 || body.HasMore || body.NextCursor != "" {
		t.Fatalf("body = %+v", body)
	}
	if body.Deliverables[0].Path != "docs/a.md" || body.Deliverables[1].Path != "src/b.ts" {
		t.Fatalf("order = %+v", body.Deliverables)
	}
	first := body.Deliverables[0]
	if first.Hash != string(hash) || first.Size != 10 || first.Mode != 0o644 || first.UpdatedSeq != 4 || first.LastTaskID != "task-1" {
		t.Fatalf("row = %+v", first)
	}
	if !strings.HasPrefix(first.PreviewURL, "/v1/sessions/"+session.String()+"/file-preview?") ||
		!strings.Contains(first.PreviewURL, "path=docs%2Fa.md") {
		t.Fatalf("preview url = %q", first.PreviewURL)
	}
}

func TestDeliverablesPaginationAndRefusals(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	entries := make([]fileplugin.Entry, 0, 5)
	for _, path := range []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"} {
		entries = append(entries, fileplugin.Entry{Path: path, Size: 1, Hash: layerfs.Hash("sha256:" + strings.Repeat("cd", 32))})
	}
	server := searchServer(t, func(cfg *Config) {
		cfg.SessionFiles = fakeSessionFiles{entries: entries}
		cfg.SessionExists = func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return true, nil }
	})
	base := server.URL + "/v1/sessions/" + session.String() + "/deliverables?limit=2"
	var first struct {
		Deliverables []DeliverableView `json:"deliverables"`
		NextCursor   string            `json:"nextCursor"`
		HasMore      bool              `json:"hasMore"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet, base, tenant, nil, &first); code != 200 {
		t.Fatalf("page 1 = %d", code)
	}
	if len(first.Deliverables) != 2 || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("page 1 = %+v", first)
	}
	var second struct {
		Deliverables []DeliverableView `json:"deliverables"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet, base+"&after="+first.NextCursor, tenant, nil, &second); code != 200 {
		t.Fatalf("page 2 = %d", code)
	}
	if len(second.Deliverables) != 2 || second.Deliverables[0].Path != "c.txt" {
		t.Fatalf("page 2 = %+v", second)
	}
	if code := doJSON(t, server.Client(), http.MethodGet, base+"&after=***", tenant, nil, nil); code != 400 {
		t.Fatalf("bad cursor = %d, want 400", code)
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions/"+session.String()+"/deliverables?limit=501", tenant, nil, nil); code != 400 {
		t.Fatalf("bad limit = %d, want 400", code)
	}

	foreign := searchServer(t, func(cfg *Config) {
		cfg.SessionFiles = fakeSessionFiles{entries: entries}
		cfg.SessionExists = func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return false, nil }
	})
	if code := doJSON(t, foreign.Client(), http.MethodGet, foreign.URL+"/v1/sessions/"+session.String()+"/deliverables", tenant, nil, nil); code != http.StatusNotFound {
		t.Fatalf("foreign deliverables = %d, want 404", code)
	}
	unwired := searchServer(t, nil)
	if code := doJSON(t, unwired.Client(), http.MethodGet, unwired.URL+"/v1/sessions/"+session.String()+"/deliverables", tenant, nil, nil); code != http.StatusNotImplemented {
		t.Fatalf("unwired deliverables = %d, want 501", code)
	}
}
