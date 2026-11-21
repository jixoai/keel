package agentapi

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/fileplugin"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/skilllib"
	"github.com/jixoai/keel/internal/skills"
	"github.com/jixoai/keel/internal/tasklog"
)

func newSkillServer(t *testing.T) (*skilllib.Library, func(*Config)) {
	t.Helper()
	library := skilllib.New(skilllib.Options{Root: t.TempDir()})
	return library, func(cfg *Config) { cfg.Skills = library }
}

func writeSkillSource(t *testing.T, dir, name, description, body string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	document, err := skills.ComposeSkillDocument(name, description, body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, skills.SkillFileName), document, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSkillSettingsInstallShowPinAndRemove(t *testing.T) {
	_, mutate := newSkillServer(t)
	server := settingsServer(t, mutate)
	client := server.Client()
	tenant := uuid.New()

	var empty skillsResponse
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/skills", tenant, nil, &empty); code != 200 {
		t.Fatalf("GET empty = %d", code)
	}
	if empty.Skills == nil || empty.Sources == nil || !empty.Checked {
		t.Fatalf("empty read = %+v", empty)
	}
	if empty.Policy.ScriptExecution != "bash" {
		t.Fatalf("script policy = %q, want bash", empty.Policy.ScriptExecution)
	}

	var installed skilllib.Record
	code := doSettingsJSON(t, client, http.MethodPost, server.URL+"/v1/settings/skills", tenant, map[string]any{
		"name": "deploy-flow", "description": "Ship a release", "body": "1. Build.\n",
	}, &installed)
	if code != 201 {
		t.Fatalf("POST install = %d", code)
	}
	if installed.Name != "deploy-flow" || installed.Trust != skilllib.TrustLocal || installed.CurrentHash == "" {
		t.Fatalf("installed = %+v", installed)
	}

	var listed skillsResponse
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/skills", tenant, nil, &listed); code != 200 {
		t.Fatalf("GET list = %d", code)
	}
	if len(listed.Skills) != 1 || listed.Skills[0].Name != "deploy-flow" {
		t.Fatalf("list = %+v", listed.Skills)
	}
	if listed.Revision == 0 {
		t.Fatal("revision did not advance on install")
	}

	var detail skilllib.Detail
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/skills/deploy-flow", tenant, nil, &detail); code != 200 {
		t.Fatalf("GET show = %d", code)
	}
	if !strings.Contains(detail.Body, "1. Build.") || len(detail.Files) != 1 {
		t.Fatalf("detail = %+v", detail)
	}

	var pinned skilllib.Record
	if code := doSettingsJSON(t, client, http.MethodPost, server.URL+"/v1/settings/skills/deploy-flow/pin", tenant, map[string]any{}, &pinned); code != 200 {
		t.Fatalf("POST pin = %d", code)
	}
	if pinned.PinnedHash != installed.CurrentHash {
		t.Fatalf("pinned = %+v", pinned)
	}
	var unpinned skilllib.Record
	if code := doSettingsJSON(t, client, http.MethodPost, server.URL+"/v1/settings/skills/deploy-flow/unpin", tenant, nil, &unpinned); code != 200 {
		t.Fatalf("DELETE pin = %d", code)
	}
	if unpinned.PinnedHash != "" {
		t.Fatalf("unpin = %+v", unpinned)
	}

	var removed map[string]any
	if code := doSettingsJSON(t, client, http.MethodDelete, server.URL+"/v1/settings/skills/deploy-flow", tenant, nil, &removed); code != 200 {
		t.Fatalf("DELETE = %d", code)
	}
	if removed["removed"] != true {
		t.Fatalf("remove = %+v", removed)
	}
}

func TestSkillSettingsTrustPinAndUpdateConflicts(t *testing.T) {
	_, mutate := newSkillServer(t)
	server := settingsServer(t, mutate)
	client := server.Client()
	tenant := uuid.New()
	sourceDir := t.TempDir()
	writeSkillSource(t, sourceDir, "alpha", "alpha v1", "body v1")

	// Subscribe an untrusted source; installing without the acknowledgement is
	// a 403 with the library's code.
	var source skilllib.Source
	if code := doSettingsJSON(t, client, http.MethodPost, server.URL+"/v1/settings/skills/sources", tenant, map[string]any{
		"id": "team", "kind": "local", "url": sourceDir, "trust": "untrusted",
	}, &source); code != 201 {
		t.Fatalf("POST source = %d", code)
	}
	var refusal map[string]any
	code := doSettingsJSON(t, client, http.MethodPost, server.URL+"/v1/settings/skills", tenant, map[string]any{
		"name": "alpha", "sourceId": "team",
	}, &refusal)
	if code != http.StatusForbidden || refusal["code"] != skilllib.CodeUntrusted {
		t.Fatalf("untrusted install = %d %+v", code, refusal)
	}
	var installed skilllib.Record
	if code := doSettingsJSON(t, client, http.MethodPost, server.URL+"/v1/settings/skills", tenant, map[string]any{
		"name": "alpha", "sourceId": "team", "allowUntrusted": true,
	}, &installed); code != 201 {
		t.Fatalf("acknowledged install = %d", code)
	}
	if installed.Trust != skilllib.TrustUntrusted {
		t.Fatalf("trust = %q", installed.Trust)
	}

	// The source moves: the list shows the update; update needs the
	// acknowledgement again because the source is still untrusted.
	writeSkillSource(t, sourceDir, "alpha", "alpha v2", "body v2")
	var listed skillsResponse
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/skills", tenant, nil, &listed); code != 200 {
		t.Fatalf("GET after source move = %d", code)
	}
	if !listed.Skills[0].UpdateAvailable || listed.Skills[0].AvailableHash == "" {
		t.Fatalf("update not visible: %+v", listed.Skills[0])
	}
	code = doSettingsJSON(t, client, http.MethodPost, server.URL+"/v1/settings/skills/alpha/update", tenant, map[string]any{}, &refusal)
	if code != http.StatusForbidden {
		t.Fatalf("unacknowledged update = %d %+v", code, refusal)
	}
	var updated skilllib.UpdateResult
	if code := doSettingsJSON(t, client, http.MethodPost, server.URL+"/v1/settings/skills/alpha/update", tenant, map[string]any{
		"allowUntrusted": true,
	}, &updated); code != 200 {
		t.Fatalf("update = %d", code)
	}
	if updated.Status != "updated" || updated.Record.Description != "alpha v2" {
		t.Fatalf("update = %+v", updated)
	}

	// A pin holds the next move back: the read names the conflict and the write
	// is a 409 with the pinned code.
	var pinned skilllib.Record
	if code := doSettingsJSON(t, client, http.MethodPost, server.URL+"/v1/settings/skills/alpha/pin", tenant, map[string]any{}, &pinned); code != 200 {
		t.Fatalf("pin = %d", code)
	}
	writeSkillSource(t, sourceDir, "alpha", "alpha v3", "body v3")
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/skills", tenant, nil, &listed); code != 200 {
		t.Fatalf("GET pinned = %d", code)
	}
	if listed.Skills[0].Conflict != skilllib.ConflictPinned {
		t.Fatalf("conflict = %q", listed.Skills[0].Conflict)
	}
	code = doSettingsJSON(t, client, http.MethodPost, server.URL+"/v1/settings/skills/alpha/update", tenant, map[string]any{
		"allowUntrusted": true,
	}, &refusal)
	if code != http.StatusConflict || refusal["code"] != skilllib.CodePinned {
		t.Fatalf("pinned update = %d %+v", code, refusal)
	}

	// The source is in use: unsubscribing is refused until the skill is gone.
	code = doSettingsJSON(t, client, http.MethodDelete, server.URL+"/v1/settings/skills/sources/team", tenant, nil, &refusal)
	if code != http.StatusConflict || refusal["code"] != skilllib.CodeSourceInUse {
		t.Fatalf("unsubscribe in use = %d %+v", code, refusal)
	}
}

func TestSkillSettingsValidationAndTenantScope(t *testing.T) {
	_, mutate := newSkillServer(t)
	server := settingsServer(t, mutate)
	client := server.Client()
	tenant := uuid.New()

	// No tenant: the middleware refuses before the handler.
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/skills", uuid.Nil, nil, nil); code != 401 {
		t.Fatalf("GET without tenant = %d", code)
	}
	// A body that names a tenant is refused, not stored.
	var refusal map[string]any
	code := doSettingsJSON(t, client, http.MethodPost, server.URL+"/v1/settings/skills", tenant, map[string]any{
		"name": "x", "description": "d", "tenantId": uuid.New().String(),
	}, &refusal)
	if code != 400 {
		t.Fatalf("tenantId body = %d %+v", code, refusal)
	}
	// Unknown fields are refused (a typo must not silently install something).
	code = doSettingsJSON(t, client, http.MethodPost, server.URL+"/v1/settings/skills", tenant, map[string]any{
		"name": "x", "description": "d", "unexpected": true,
	}, &refusal)
	if code != 400 {
		t.Fatalf("unknown field = %d %+v", code, refusal)
	}
	// /sources is a literal route, not a skill named "sources".
	var sources skillSourcesResponse
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/skills/sources", tenant, nil, &sources); code != 200 {
		t.Fatalf("GET sources = %d", code)
	}
	if sources.Sources == nil {
		t.Fatal("sources must render as []")
	}
	// A missing skill is a typed 404.
	code = doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/skills/missing", tenant, nil, &refusal)
	if code != 404 || refusal["code"] != skilllib.CodeNotFound {
		t.Fatalf("missing skill = %d %+v", code, refusal)
	}
}

func TestSkillSettingsNotWired(t *testing.T) {
	server := settingsServer(t, nil)
	var body map[string]any
	code := doSettingsJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/settings/skills", uuid.New(), nil, &body)
	if code != http.StatusNotImplemented || body["status"] != "not_wired" {
		t.Fatalf("not wired = %d %+v", code, body)
	}
}

// --- completion sources --------------------------------------------------

type fakeSessionFiles struct{ entries []fileplugin.Entry }

func (f fakeSessionFiles) List(context.Context, fileplugin.Scope) ([]fileplugin.Entry, error) {
	return f.entries, nil
}

type fakeSessionTasks struct{ cards []tasklog.Card }

func (f fakeSessionTasks) List(context.Context, kernel.Scope, tasklog.ListFilter) ([]tasklog.Card, error) {
	return f.cards, nil
}

func TestCompletionRoutes(t *testing.T) {
	tenant := uuid.New()
	session := uuid.New()
	parent := uuid.New()
	server := settingsServer(t, func(cfg *Config) {
		cfg.SessionFiles = fakeSessionFiles{entries: []fileplugin.Entry{
			{Path: "src/b.ts", Size: 2, UpdatedSeq: 4},
			{Path: "src/a.ts", Size: 1, UpdatedSeq: 3, LastTaskID: "t1"},
		}}
		cfg.SessionTasks = fakeSessionTasks{cards: []tasklog.Card{
			{TaskID: uuid.New(), Title: "checkout", Status: tasklog.StatusOpen, UpdatedAt: testTime.Add(-time.Minute)},
			{TaskID: uuid.New(), ParentTaskID: &parent, Title: "subtask", Status: tasklog.StatusDone, UpdatedAt: testTime},
		}}
		cfg.SessionExists = func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return true, nil }
	})
	client := server.Client()

	var files struct {
		Files []SessionFileView `json:"files"`
	}
	if code := doJSON(t, client, http.MethodGet, server.URL+"/v1/sessions/"+session.String()+"/files", tenant, nil, &files); code != 200 {
		t.Fatalf("GET files = %d", code)
	}
	if len(files.Files) != 2 || files.Files[0].Path != "src/a.ts" {
		t.Fatalf("files = %+v", files.Files)
	}

	var tasks struct {
		Tasks []SessionTaskView `json:"tasks"`
	}
	if code := doJSON(t, client, http.MethodGet, server.URL+"/v1/sessions/"+session.String()+"/tasks", tenant, nil, &tasks); code != 200 {
		t.Fatalf("GET tasks = %d", code)
	}
	if len(tasks.Tasks) != 2 || tasks.Tasks[0].ParentTaskID != parent.String() {
		t.Fatalf("tasks order/parent = %+v", tasks.Tasks)
	}

	// A session the tenant does not own is a 404, not an empty page.
	server2 := settingsServer(t, func(cfg *Config) {
		cfg.SessionFiles = fakeSessionFiles{}
		cfg.SessionExists = func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return false, nil }
	})
	var refusal map[string]any
	if code := doJSON(t, server2.Client(), http.MethodGet, server2.URL+"/v1/sessions/"+uuid.New().String()+"/files", tenant, nil, &refusal); code != 404 {
		t.Fatalf("foreign session = %d %+v", code, refusal)
	}
}

var testTime = mustParseTime("2026-11-21T10:00:00Z")

func mustParseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed
}
