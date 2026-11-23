package harnessapi

// The panel route's wire contract: the tree's fields, the member/ancestor
// flags, the attachment references, the event stream, the page limits, the
// 503/not-wired answer and the foreign-session 404. A fake projection keeps the
// test honest about what the route adds: no field, no path and no member is
// invented by the handler.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/fileplugin"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/tasklog"
)

type fakeTaskPanel struct {
	panel tasklog.Panel
	err   error
}

func (f fakeTaskPanel) Panel(context.Context, kernel.Scope, int, int) (tasklog.Panel, error) {
	return f.panel, f.err
}

type fakeFilePreview struct {
	results map[string]fileplugin.ReadResult
}

func (f fakeFilePreview) ReadEntry(_ context.Context, _ fileplugin.Scope, path string, offset, limit int) (fileplugin.ReadResult, error) {
	result, ok := f.results[path]
	if !ok {
		return fileplugin.ReadResult{}, fileplugin.Errorf(fileplugin.CodeNotFound, "file %q does not exist in this session", path)
	}
	if offset > 0 {
		result.Offset = offset
	}
	if limit > 0 {
		result.Limit = limit
	}
	return result, nil
}

func panelFixture(tenant, session uuid.UUID) tasklog.Panel {
	childSession := uuid.New()
	parent := uuid.New()
	at := testTime
	created := tasklog.Card{
		TenantID: tenant, SessionID: session, TaskID: parent,
		Title: "本周退款原因分析", Status: tasklog.StatusInProgress, Origin: tasklog.OriginSession,
		CreatedSeq: 3, UpdatedSeq: 9, CreatedAt: at, UpdatedAt: at.Add(2 * time.Minute),
	}
	fork := tasklog.Card{
		TenantID: tenant, SessionID: childSession, TaskID: uuid.New(),
		ParentTaskID: &parent, Title: "退款分支", Status: tasklog.StatusDone,
		Origin: tasklog.OriginFork, OriginSeq: 12, CreatedSeq: 1, UpdatedSeq: 2,
		CreatedAt: at, UpdatedAt: at.Add(time.Minute),
	}
	ancestorCard := tasklog.Card{
		TenantID: tenant, SessionID: uuid.New(), TaskID: uuid.New(),
		Title: "宿主的卡", Status: tasklog.StatusOpen, Origin: tasklog.OriginSession,
		CreatedSeq: 1, UpdatedSeq: 1, CreatedAt: at, UpdatedAt: at,
	}
	return tasklog.Panel{
		TenantID: tenant, SessionID: session,
		Cards:     []tasklog.Card{created},
		Members:   []tasklog.Card{fork},
		Ancestors: []tasklog.Card{ancestorCard},
		Attachments: []tasklog.PanelAttachment{
			{TaskID: parent, Path: "dist/app.tar.gz", Hash: "sha256:" + "a" + "000000000000000000000000000000000000000000000000000000000000000"},
		},
		Events: []tasklog.DescribedEvent{
			{TaskID: parent, Kind: string(kernel.KindTaskCreated), Seq: 3, At: at, Title: "本周退款原因分析", Status: tasklog.StatusOpen, Origin: tasklog.OriginSession},
			{TaskID: fork.TaskID, Kind: string(kernel.KindTaskAttached), Seq: 4, At: at, Path: "dist/app.tar.gz", Hash: "sha256:" + "a"},
			{TaskID: fork.TaskID, Kind: "task.future_kind", Seq: 5, At: at, Unparsed: true},
		},
		CardLimit: 1000, MemberLimit: 200, EventLimit: 500, AttachmentLimit: 1000,
	}
}

func TestTaskPanelRouteWireShape(t *testing.T) {
	tenant := uuid.New()
	session := uuid.New()
	fixture := panelFixture(tenant, session)
	server := settingsServer(t, func(cfg *Config) {
		cfg.TaskPanel = fakeTaskPanel{panel: fixture}
		cfg.SessionExists = func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return true, nil }
	})
	var body TaskPanelView
	code := doJSON(t, server.Client(), http.MethodGet,
		server.URL+"/v1/sessions/"+session.String()+"/task-panel", tenant, nil, &body)
	if code != 200 {
		t.Fatalf("GET task-panel = %d", code)
	}
	if body.SessionID != session.String() {
		t.Fatalf("sessionId = %q", body.SessionID)
	}
	if len(body.Cards) != 1 || body.Cards[0].TaskID != fixture.Cards[0].TaskID.String() {
		t.Fatalf("cards = %+v", body.Cards)
	}
	if body.Cards[0].Member {
		t.Fatalf("own card reported as a member: %+v", body.Cards[0])
	}
	if len(body.Members) != 1 || !body.Members[0].Member {
		t.Fatalf("members = %+v", body.Members)
	}
	if body.Members[0].Origin != tasklog.OriginFork || body.Members[0].OriginSeq != 12 {
		t.Fatalf("fork member = %+v", body.Members[0])
	}
	if body.Members[0].ParentTaskID != fixture.Cards[0].TaskID.String() {
		t.Fatalf("member parent = %q", body.Members[0].ParentTaskID)
	}
	if body.Members[0].SessionID != fixture.Members[0].SessionID.String() {
		t.Fatalf("member session = %q", body.Members[0].SessionID)
	}
	if len(body.Ancestors) != 1 || body.Ancestors[0].TaskID != fixture.Ancestors[0].TaskID.String() {
		t.Fatalf("ancestors = %+v", body.Ancestors)
	}
	if len(body.Attachments) != 1 || body.Attachments[0].Path != "dist/app.tar.gz" {
		t.Fatalf("attachments = %+v", body.Attachments)
	}
	if body.Attachments[0].TaskID != fixture.Cards[0].TaskID.String() {
		t.Fatalf("attachment task = %q", body.Attachments[0].TaskID)
	}
	if len(body.Events) != 3 {
		t.Fatalf("events = %+v", body.Events)
	}
	if body.Events[0].Kind != string(kernel.KindTaskCreated) || body.Events[0].Seq != 3 {
		t.Fatalf("event[0] = %+v", body.Events[0])
	}
	// An unparsed kind is carried, not dropped or guessed.
	if !body.Events[2].Unparsed || body.Events[2].Kind != "task.future_kind" {
		t.Fatalf("event[2] = %+v", body.Events[2])
	}
	if body.Limits.MemberLimit != 200 || body.Limits.EventLimit != 500 {
		t.Fatalf("limits = %+v", body.Limits)
	}
	if body.Limits.CardLimit != 1000 || body.Limits.AttachmentLimit != 1000 {
		t.Fatalf("read bounds = %+v", body.Limits)
	}
	if body.Limits.CardsTruncated || body.Limits.AttachmentsTruncated {
		t.Fatalf("a complete read reported truncation: %+v", body.Limits)
	}
}

func TestTaskPanelRouteRefusals(t *testing.T) {
	tenant := uuid.New()
	session := uuid.New()

	// A deployment without the projection is a not-wired 501, and the
	// capability bit is false.
	unwired := settingsServer(t, func(cfg *Config) {})
	var notWired map[string]any
	if code := doJSON(t, unwired.Client(), http.MethodGet,
		unwired.URL+"/v1/sessions/"+session.String()+"/task-panel", tenant, nil, &notWired); code != http.StatusNotImplemented {
		t.Fatalf("unwired panel = %d %+v", code, notWired)
	}
	var caps Capabilities
	if code := doJSON(t, unwired.Client(), http.MethodGet, unwired.URL+"/v1/capabilities", tenant, nil, &caps); code != 200 {
		t.Fatalf("capabilities = %d", code)
	}
	if caps.Features["task.panel"] {
		t.Fatalf("task.panel reported true without a projection")
	}

	// A foreign session 404s before the projection is read, exactly like the
	// other session reads.
	foreign := settingsServer(t, func(cfg *Config) {
		cfg.TaskPanel = fakeTaskPanel{panel: panelFixture(tenant, uuid.New())}
		cfg.SessionExists = func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return false, nil }
	})
	var refusal map[string]any
	if code := doJSON(t, foreign.Client(), http.MethodGet,
		foreign.URL+"/v1/sessions/"+uuid.New().String()+"/task-panel", tenant, nil, &refusal); code != http.StatusNotFound {
		t.Fatalf("foreign session = %d %+v", code, refusal)
	}

	// A limit outside the route's bounds is a 400, not a clamped answer.
	server := settingsServer(t, func(cfg *Config) {
		cfg.TaskPanel = fakeTaskPanel{panel: panelFixture(tenant, uuid.New())}
		cfg.SessionExists = func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return true, nil }
	})
	var badLimit map[string]any
	if code := doJSON(t, server.Client(), http.MethodGet,
		server.URL+"/v1/sessions/"+session.String()+"/task-panel?memberLimit=0", tenant, nil, &badLimit); code != http.StatusBadRequest {
		t.Fatalf("bad memberLimit = %d %+v", code, badLimit)
	}
}

func TestFilePreviewRouteReadsTheManifest(t *testing.T) {
	tenant := uuid.New()
	session := uuid.New()
	hash := "sha256:" + "b" + "0000000000000000000000000000000000000000000000000000000000000"
	server := settingsServer(t, func(cfg *Config) {
		cfg.FilePreview = fakeFilePreview{results: map[string]fileplugin.ReadResult{
			"dist/app.tar.gz": {
				Path: "dist/app.tar.gz", Hash: hash, Size: 2048, Seq: 12,
				Offset: 0, Limit: fileplugin.DefaultReadLines,
				TotalLines: 3, ReturnedLines: 3, NextOffset: 3,
				Content: "line one\nline two\nline three\n",
			},
		}}
		cfg.SessionExists = func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return true, nil }
	})
	var body FilePreviewView
	if code := doJSON(t, server.Client(), http.MethodGet,
		server.URL+"/v1/sessions/"+session.String()+"/file-preview?path=dist/app.tar.gz&hash="+hash, tenant, nil, &body); code != 200 {
		t.Fatalf("GET file-preview = %d", code)
	}
	if body.Path != "dist/app.tar.gz" || body.Hash != hash {
		t.Fatalf("preview = %+v", body)
	}
	if body.MatchesReference == nil || !*body.MatchesReference {
		t.Fatalf("reference match = %+v", body.MatchesReference)
	}
	if body.TotalLines != 3 || body.ReturnedLines != 3 {
		t.Fatalf("page = %+v", body)
	}

	// A path the manifest does not hold is a 404: the preview never invents a
	// path (an event's reference may name one the session never wrote).
	var missing map[string]any
	if code := doJSON(t, server.Client(), http.MethodGet,
		server.URL+"/v1/sessions/"+session.String()+"/file-preview?path=invented.txt", tenant, nil, &missing); code != http.StatusNotFound {
		t.Fatalf("missing path = %d %+v", code, missing)
	}

	// A changed file answers the current hash and says the reference no longer
	// matches, so the panel can state the fact instead of claiming the old
	// reference was read.
	stale := settingsServer(t, func(cfg *Config) {
		cfg.FilePreview = fakeFilePreview{results: map[string]fileplugin.ReadResult{
			"dist/app.tar.gz": {
				Path: "dist/app.tar.gz", Hash: "sha256:" + "c" + "0000000000000000000000000000000000000000000000000000000000000",
				Size: 1, Seq: 20, Content: "new\n", TotalLines: 1, ReturnedLines: 1, NextOffset: 1,
			},
		}}
		cfg.SessionExists = func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return true, nil }
	})
	var changed FilePreviewView
	if code := doJSON(t, stale.Client(), http.MethodGet,
		stale.URL+"/v1/sessions/"+session.String()+"/file-preview?path=dist/app.tar.gz&hash="+hash, tenant, nil, &changed); code != 200 {
		t.Fatalf("stale preview = %d", code)
	}
	if changed.MatchesReference == nil || *changed.MatchesReference {
		t.Fatalf("changed file claimed a reference match: %+v", changed.MatchesReference)
	}

	// The preview route is not reported as available without its reader.
	var caps Capabilities
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/capabilities", tenant, nil, &caps); code != 200 {
		t.Fatalf("capabilities = %d", code)
	}
	if !caps.Features["session.filePreview"] {
		t.Fatalf("session.filePreview = false with a reader")
	}
	if caps.Features["task.panel"] {
		t.Fatalf("task.panel = true without a projection")
	}
	_ = httptest.NewServer
}
