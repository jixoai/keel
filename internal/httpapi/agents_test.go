package httpapi

// Conformance tests for the agent registry and agent-team surface
// (change keel-agent-team-v1, P2/P3). They run without PostgreSQL against a
// map-backed registry fake: the handlers' contract is routing, tenant
// isolation, validation mapping and reference/permission refusals, all of which
// are decided above the store. The SQL side has its own tests in
// internal/store (skipped without KEEL_TEST_DSN).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentteam"
	"github.com/jixoai/keel/internal/store"
)

// --- registry fake -------------------------------------------------------

// fakeSessionBinding is the map-backed mirror of one sessions row's binding
// projection plus the lifecycle facts the reference query reads.
type fakeSessionBinding struct {
	AgentID   string
	TeamID    string
	State     string
	Archived  bool
	RunStatus string
}

// memoryRegistry is the AgentTeamRegistry seam over maps. It mirrors the SQL
// behaviour the handlers rely on: per-tenant keys, create-never-overwrites,
// revision-guarded team updates and reference lookup by parsing the documents
// (the same information the store's jsonb query reads).
type memoryRegistry struct {
	mu       sync.Mutex
	agents   map[string]map[string]store.AgentRecord
	teams    map[string]map[string]store.TeamRecord
	sessions map[string]map[string]fakeSessionBinding
}

func newMemoryRegistry() *memoryRegistry {
	return &memoryRegistry{
		agents:   make(map[string]map[string]store.AgentRecord),
		teams:    make(map[string]map[string]store.TeamRecord),
		sessions: make(map[string]map[string]fakeSessionBinding),
	}
}

// bindSession records one session binding in the fake (the store's SetSessionBinding
// equivalent for handler tests).
func (m *memoryRegistry) bindSession(tenantID uuid.UUID, sessionID, agentID, teamID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	scope := m.sessions[tenantID.String()]
	if scope == nil {
		scope = make(map[string]fakeSessionBinding)
		m.sessions[tenantID.String()] = scope
	}
	scope[sessionID] = fakeSessionBinding{AgentID: agentID, TeamID: teamID, State: "active"}
}

// archiveSession marks one fake session archived; a live run still counts.
func (m *memoryRegistry) archiveSession(tenantID uuid.UUID, sessionID, runStatus string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	scope := m.sessions[tenantID.String()]
	if scope == nil {
		return
	}
	binding := scope[sessionID]
	binding.Archived = true
	binding.RunStatus = runStatus
	scope[sessionID] = binding
}

func (m *memoryRegistry) sessionsReferencing(tenantID uuid.UUID, match func(fakeSessionBinding) bool) ([]store.SessionReference, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.SessionReference, 0)
	for id, binding := range m.sessions[tenantID.String()] {
		if !match(binding) {
			continue
		}
		if binding.Archived && binding.RunStatus != "pending" && binding.RunStatus != "running" {
			continue
		}
		state := binding.State
		if state == "" {
			state = "active"
		}
		out = append(out, store.SessionReference{
			ID: id, State: state, Archived: binding.Archived, RunStatus: binding.RunStatus,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	total := len(out)
	if len(out) > 20 {
		out = out[:20]
	}
	return out, total
}

func (m *memoryRegistry) SessionsReferencingAgent(_ context.Context, tenantID uuid.UUID, agentID string) ([]store.SessionReference, int, error) {
	refs, total := m.sessionsReferencing(tenantID, func(binding fakeSessionBinding) bool {
		return binding.AgentID == agentID
	})
	return refs, total, nil
}

func (m *memoryRegistry) SessionsReferencingTeam(_ context.Context, tenantID uuid.UUID, teamID string) ([]store.SessionReference, int, error) {
	refs, total := m.sessionsReferencing(tenantID, func(binding fakeSessionBinding) bool {
		return binding.TeamID == teamID
	})
	return refs, total, nil
}

func (m *memoryRegistry) CreateAgent(_ context.Context, record store.AgentRecord) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	scope := m.agents[record.TenantID.String()]
	if scope == nil {
		scope = make(map[string]store.AgentRecord)
		m.agents[record.TenantID.String()] = scope
	}
	if _, exists := scope[record.ID]; exists {
		return false, nil
	}
	record.CreatedAt, record.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	scope[record.ID] = record
	return true, nil
}

func (m *memoryRegistry) UpdateAgent(_ context.Context, record store.AgentRecord, expectedArchived bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	scope := m.agents[record.TenantID.String()]
	existing, exists := scope[record.ID]
	if !exists || existing.Archived != expectedArchived {
		return false, nil
	}
	record.CreatedAt = existing.CreatedAt
	record.UpdatedAt = time.Now().UTC()
	scope[record.ID] = record
	return true, nil
}

func (m *memoryRegistry) GetAgent(_ context.Context, tenantID uuid.UUID, agentID string) (store.AgentRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, found := m.agents[tenantID.String()][agentID]
	return record, found, nil
}

func (m *memoryRegistry) ListAgents(_ context.Context, tenantID uuid.UUID) ([]store.AgentRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.AgentRecord, 0, len(m.agents[tenantID.String()]))
	for _, record := range m.agents[tenantID.String()] {
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memoryRegistry) DeleteAgent(_ context.Context, tenantID uuid.UUID, agentID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	scope := m.agents[tenantID.String()]
	if _, exists := scope[agentID]; !exists {
		return false, nil
	}
	delete(scope, agentID)
	return true, nil
}

func (m *memoryRegistry) TeamsReferencingAgent(_ context.Context, tenantID uuid.UUID, agentID string) ([]store.TeamReference, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.TeamReference, 0)
	for _, record := range m.teams[tenantID.String()] {
		var team agentteam.AgentTeam
		if err := json.Unmarshal(record.Document, &team); err != nil {
			continue
		}
		if team.Captain.Inline == nil && team.Captain.AgentID == agentID {
			out = append(out, store.TeamReference{ID: team.ID, Name: team.Name})
			continue
		}
		for _, member := range team.Members {
			if member.Inline == nil && member.AgentID == agentID {
				out = append(out, store.TeamReference{ID: team.ID, Name: team.Name})
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memoryRegistry) CreateTeam(_ context.Context, record store.TeamRecord) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	scope := m.teams[record.TenantID.String()]
	if scope == nil {
		scope = make(map[string]store.TeamRecord)
		m.teams[record.TenantID.String()] = scope
	}
	if _, exists := scope[record.ID]; exists {
		return false, nil
	}
	record.CreatedAt, record.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	scope[record.ID] = record
	return true, nil
}

func (m *memoryRegistry) UpdateTeam(_ context.Context, record store.TeamRecord, expectedRevision int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	scope := m.teams[record.TenantID.String()]
	existing, exists := scope[record.ID]
	if !exists || existing.Revision != expectedRevision {
		return false, nil
	}
	record.CreatedAt = existing.CreatedAt
	record.UpdatedAt = time.Now().UTC()
	scope[record.ID] = record
	return true, nil
}

func (m *memoryRegistry) GetTeam(_ context.Context, tenantID uuid.UUID, teamID string) (store.TeamRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, found := m.teams[tenantID.String()][teamID]
	return record, found, nil
}

func (m *memoryRegistry) ListTeams(_ context.Context, tenantID uuid.UUID) ([]store.TeamRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.TeamRecord, 0, len(m.teams[tenantID.String()]))
	for _, record := range m.teams[tenantID.String()] {
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memoryRegistry) DeleteTeam(_ context.Context, tenantID uuid.UUID, teamID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	scope := m.teams[tenantID.String()]
	if _, exists := scope[teamID]; !exists {
		return false, nil
	}
	delete(scope, teamID)
	return true, nil
}

// --- harness -------------------------------------------------------------

func newRegistryServer(t *testing.T, opts ...func(*Config)) (*httptest.Server, *memoryRegistry) {
	t.Helper()
	registry := newMemoryRegistry()
	cfg := Config{
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		AgentTeams: registry,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	hs := httptest.NewServer(New(cfg))
	t.Cleanup(hs.Close)
	return hs, registry
}

// call performs one request and decodes the answer into out (when non-nil).
func call(t *testing.T, hc *http.Client, method, url, tenant string, headers map[string]string, body, out any) int {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if tenant != "" {
		request.Header.Set("X-Tenant-ID", tenant)
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := hc.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: decode %s: %v", method, url, raw, err)
		}
	}
	return response.StatusCode
}

// callRaw sends an exact byte body, for the JSON-layer rejections that a
// re-marshalled value cannot express (trailing data, an empty body).
func callRaw(t *testing.T, hc *http.Client, method, url, tenant string, body []byte, out any) int {
	t.Helper()
	request, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Tenant-ID", tenant)
	request.Header.Set("Content-Type", "application/json")
	response, err := hc.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: decode %s: %v", method, url, raw, err)
		}
	}
	return response.StatusCode
}

// createAgentFixture creates an agent and returns its view.
func createAgentFixture(t *testing.T, base, tenant, name string, extra map[string]any) AgentView {
	t.Helper()
	body := map[string]any{"name": name}
	for key, value := range extra {
		body[key] = value
	}
	var view AgentView
	if status := call(t, http.DefaultClient, http.MethodPost, base+"/v1/agents", tenant, nil, body, &view); status != http.StatusCreated {
		t.Fatalf("create agent %q: status %d", name, status)
	}
	return view
}

// --- agent tests ---------------------------------------------------------

func TestAgentRegistryCRUDAndMintedIdentity(t *testing.T) {
	hs, _ := newRegistryServer(t)
	tenant := uuid.NewString()

	created := createAgentFixture(t, hs.URL, tenant, "设计主理人", nil)
	if created.ID == "" || created.ID[:6] != "agent_" {
		t.Fatalf("minted id = %q, want agent_<uuid>", created.ID)
	}
	if created.Avatar.Kind != agentteam.AvatarIdenticon || created.Avatar.Seed != created.ID {
		t.Fatalf("avatar = %+v, want identicon seeded with the id", created.Avatar)
	}
	if created.TenantID.String() != tenant {
		t.Fatalf("tenantId = %s, want %s", created.TenantID, tenant)
	}

	var fetched AgentView
	if status := call(t, http.DefaultClient, http.MethodGet, hs.URL+"/v1/agents/"+created.ID, tenant, nil, nil, &fetched); status != http.StatusOK {
		t.Fatalf("get: status %d", status)
	}
	if fetched.Name != "设计主理人" {
		t.Fatalf("name = %q", fetched.Name)
	}

	var list []AgentView
	if status := call(t, http.DefaultClient, http.MethodGet, hs.URL+"/v1/agents", tenant, nil, nil, &list); status != http.StatusOK || len(list) != 1 {
		t.Fatalf("list: status %d len %d", status, len(list))
	}

	update := AgentWriteRequest{Name: "设计主管", Description: "视觉方向"}
	var updated AgentView
	if status := call(t, http.DefaultClient, http.MethodPut, hs.URL+"/v1/agents/"+created.ID, tenant, nil, update, &updated); status != http.StatusOK {
		t.Fatalf("update: status %d", status)
	}
	if updated.Name != "设计主管" || updated.Description != "视觉方向" {
		t.Fatalf("updated view = %+v", updated)
	}
	// The update carried no id in the body; the path's stored id must survive.
	if updated.ID != created.ID {
		t.Fatalf("update re-keyed the agent: %s -> %s", created.ID, updated.ID)
	}

	if status := call(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/agents/"+created.ID, tenant, nil, nil, nil); status != http.StatusOK {
		t.Fatalf("delete: status %d", status)
	}
	if status := call(t, http.DefaultClient, http.MethodGet, hs.URL+"/v1/agents/"+created.ID, tenant, nil, nil, nil); status != http.StatusNotFound {
		t.Fatalf("get after delete: status %d, want 404", status)
	}
	if status := call(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/agents/"+created.ID, tenant, nil, nil, nil); status != http.StatusNotFound {
		t.Fatalf("second delete: status %d, want 404", status)
	}
}

func TestAgentRegistryTenantIsolation(t *testing.T) {
	hs, _ := newRegistryServer(t)
	owner, other := uuid.NewString(), uuid.NewString()
	created := createAgentFixture(t, hs.URL, owner, "私有 Agent", nil)

	if status := call(t, http.DefaultClient, http.MethodGet, hs.URL+"/v1/agents/"+created.ID, other, nil, nil, nil); status != http.StatusNotFound {
		t.Fatalf("foreign get: status %d, want 404", status)
	}
	var list []AgentView
	if status := call(t, http.DefaultClient, http.MethodGet, hs.URL+"/v1/agents", other, nil, nil, &list); status != http.StatusOK || len(list) != 0 {
		t.Fatalf("foreign list: status %d len %d, want 200 and empty", status, len(list))
	}
	if status := call(t, http.DefaultClient, http.MethodPut, hs.URL+"/v1/agents/"+created.ID, other, nil, AgentWriteRequest{Name: "hijack"}, nil); status != http.StatusNotFound {
		t.Fatalf("foreign update: status %d, want 404", status)
	}
	if status := call(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/agents/"+created.ID, other, nil, nil, nil); status != http.StatusNotFound {
		t.Fatalf("foreign delete: status %d, want 404", status)
	}
	// The owner still has the agent, unchanged.
	var kept AgentView
	if status := call(t, http.DefaultClient, http.MethodGet, hs.URL+"/v1/agents/"+created.ID, owner, nil, nil, &kept); status != http.StatusOK || kept.Name != "私有 Agent" {
		t.Fatalf("owner get: status %d name %q", status, kept.Name)
	}

	// A missing tenant header is 401, never an implicit tenant.
	if status := call(t, http.DefaultClient, http.MethodGet, hs.URL+"/v1/agents", "", nil, nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("no tenant: status %d, want 401", status)
	}
}

func TestAgentValidationIsFieldLevel400(t *testing.T) {
	hs, _ := newRegistryServer(t)
	tenant := uuid.NewString()

	// Missing name: the domain's missing_name at the JSON field "name".
	var fields struct {
		Error  string                 `json:"error"`
		Fields []agentteam.FieldError `json:"fields"`
	}
	status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/agents", tenant, nil, map[string]any{}, &fields)
	if status != http.StatusBadRequest {
		t.Fatalf("empty name: status %d, want 400", status)
	}
	if !hasField(fields.Fields, "name", agentteam.CodeMissingName) {
		t.Fatalf("fields = %+v, want name/missing_name", fields.Fields)
	}

	// A tenant in the body is an unknown field (never a second tenant).
	var refusal struct {
		Code   string                 `json:"code"`
		Field  string                 `json:"field"`
		Fields []agentteam.FieldError `json:"fields"`
	}
	status = call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/agents", tenant, nil,
		map[string]any{"name": "x", "tenantId": uuid.NewString()}, &refusal)
	if status != http.StatusBadRequest || refusal.Code != BodyRejectionUnknownField || refusal.Field != "tenantId" {
		t.Fatalf("tenant in body: status %d body %+v, want 400 unknown_field tenantId", status, refusal)
	}

	// An invalid avatar is the domain's field path.
	status = call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/agents", tenant, nil,
		map[string]any{"name": "x", "avatar": map[string]any{"kind": "emoji"}}, &fields)
	if status != http.StatusBadRequest || !hasField(fields.Fields, "avatar.kind", agentteam.CodeInvalidAvatarKind) {
		t.Fatalf("bad avatar: status %d fields %+v", status, fields.Fields)
	}

	// Tool grants are checked against the wired registry.
	hs2, _ := newRegistryServer(t, func(cfg *Config) {
		cfg.ToolExists = func(toolID string) bool { return toolID == "bash" }
	})
	status = call(t, http.DefaultClient, http.MethodPost, hs2.URL+"/v1/agents", tenant, nil,
		map[string]any{"name": "x", "tools": []map[string]any{{"toolId": "ghost", "enabled": true}}}, &fields)
	if status != http.StatusBadRequest || !hasField(fields.Fields, "tools[0].toolId", agentteam.CodeToolNotFound) {
		t.Fatalf("unknown tool: status %d fields %+v", status, fields.Fields)
	}
	if status := call(t, http.DefaultClient, http.MethodPost, hs2.URL+"/v1/agents", tenant, nil,
		map[string]any{"name": "ok", "tools": []map[string]any{{"toolId": "bash", "enabled": true}}}, nil); status != http.StatusCreated {
		t.Fatalf("known tool: status %d, want 201", status)
	}
}

func hasField(fields []agentteam.FieldError, field, code string) bool {
	for _, failure := range fields {
		if failure.Field == field && failure.Code == code {
			return true
		}
	}
	return false
}

func TestAgentCreateNeverOverwrites(t *testing.T) {
	hs, _ := newRegistryServer(t)
	tenant := uuid.NewString()
	created := createAgentFixture(t, hs.URL, tenant, "first", nil)

	var refusal struct {
		Code string `json:"code"`
	}
	status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/agents", tenant, nil,
		map[string]any{"id": created.ID, "name": "second"}, &refusal)
	if status != http.StatusConflict || refusal.Code != "agent_exists" {
		t.Fatalf("duplicate create: status %d code %q, want 409 agent_exists", status, refusal.Code)
	}
	var kept AgentView
	call(t, http.DefaultClient, http.MethodGet, hs.URL+"/v1/agents/"+created.ID, tenant, nil, nil, &kept)
	if kept.Name != "first" {
		t.Fatalf("duplicate create overwrote the stored agent: name %q", kept.Name)
	}
}

// --- team tests ----------------------------------------------------------

func TestTeamRegistryCRUDWithCaptain(t *testing.T) {
	hs, _ := newRegistryServer(t)
	tenant := uuid.NewString()
	captain := createAgentFixture(t, hs.URL, tenant, "队长", nil)
	member := createAgentFixture(t, hs.URL, tenant, "成员", nil)

	create := AgentTeamWriteRequest{
		Name:    "产品小队",
		Captain: agentteam.TeamMember{AgentID: captain.ID},
		Members: []agentteam.TeamMember{{AgentID: member.ID, Label: "设计"}},
	}
	var created AgentTeamView
	if status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams", tenant, nil, create, &created); status != http.StatusCreated {
		t.Fatalf("create team: status %d", status)
	}
	if created.ID == "" || created.ID[:5] != "team_" {
		t.Fatalf("minted team id = %q", created.ID)
	}
	if created.Status != agentteam.StatusValid || created.Revision != 1 || created.Hash == "" {
		t.Fatalf("created team = %+v, want valid revision 1 with a hash", created)
	}

	// A team with only a captain is legal.
	var solo AgentTeamView
	if status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams", tenant, nil,
		AgentTeamWriteRequest{Name: "单人队", Captain: agentteam.TeamMember{AgentID: captain.ID}}, &solo); status != http.StatusCreated {
		t.Fatalf("solo team: status %d", status)
	}
	if solo.Status != agentteam.StatusValid {
		t.Fatalf("solo team status = %q", solo.Status)
	}

	// Update bumps the revision and the content hash.
	update := AgentTeamWriteRequest{
		Name:            "产品小队 v2",
		Captain:         create.Captain,
		Members:         create.Members,
		Capabilities:    create.Capabilities,
		FeatureBindings: create.FeatureBindings,
	}
	var updated AgentTeamView
	if status := call(t, http.DefaultClient, http.MethodPut, hs.URL+"/v1/teams/"+created.ID, tenant, nil, update, &updated); status != http.StatusOK {
		t.Fatalf("update team: status %d", status)
	}
	if updated.Revision != 2 || updated.Hash == created.Hash || updated.Name != "产品小队 v2" {
		t.Fatalf("updated team = rev %d hash %s name %q", updated.Revision, updated.Hash, updated.Name)
	}

	var list []AgentTeamView
	if status := call(t, http.DefaultClient, http.MethodGet, hs.URL+"/v1/teams", tenant, nil, nil, &list); status != http.StatusOK || len(list) != 2 {
		t.Fatalf("list teams: status %d len %d", status, len(list))
	}

	if status := call(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/teams/"+created.ID, tenant, nil, nil, nil); status != http.StatusOK {
		t.Fatalf("delete team: status %d", status)
	}
	if status := call(t, http.DefaultClient, http.MethodGet, hs.URL+"/v1/teams/"+created.ID, tenant, nil, nil, nil); status != http.StatusNotFound {
		t.Fatalf("get after delete: status %d", status)
	}
}

func TestTeamCaptainIsMandatoryAndMustExist(t *testing.T) {
	hs, _ := newRegistryServer(t)
	tenant := uuid.NewString()
	captain := createAgentFixture(t, hs.URL, tenant, "队长", nil)

	var fields struct {
		Fields []agentteam.FieldError `json:"fields"`
	}
	status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams", tenant, nil,
		map[string]any{"name": "缺队长"}, &fields)
	if status != http.StatusBadRequest || !hasField(fields.Fields, "captain.agentId", agentteam.CodeMissingCaptain) {
		t.Fatalf("missing captain: status %d fields %+v, want 400 missing_captain", status, fields.Fields)
	}

	status = call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams", tenant, nil,
		map[string]any{"name": "幽灵队长", "captain": map[string]any{"agentId": "agent_ghost"}}, &fields)
	if status != http.StatusBadRequest || !hasField(fields.Fields, "captain.agentId", agentteam.CodeAgentNotFound) {
		t.Fatalf("missing captain agent: status %d fields %+v, want 400 agent_not_found", status, fields.Fields)
	}

	// The captain may not also be a member, and a seat may not repeat.
	status = call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams", tenant, nil,
		map[string]any{"name": "重复", "captain": map[string]any{"agentId": captain.ID},
			"members": []map[string]any{{"agentId": captain.ID}}}, &fields)
	if status != http.StatusBadRequest || !hasField(fields.Fields, "members[0].agentId", agentteam.CodeCaptainInMembers) {
		t.Fatalf("captain in members: status %d fields %+v", status, fields.Fields)
	}

	// A foreign tenant's agent is not a valid seat.
	other := uuid.NewString()
	if status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams", other, nil,
		map[string]any{"name": "跨租户", "captain": map[string]any{"agentId": captain.ID}}, &fields); status != http.StatusBadRequest {
		t.Fatalf("foreign captain: status %d, want 400", status)
	}
}

func TestAgentDeletionIsRefusedWhileTeamed(t *testing.T) {
	hs, _ := newRegistryServer(t)
	tenant := uuid.NewString()
	captain := createAgentFixture(t, hs.URL, tenant, "队长", nil)
	member := createAgentFixture(t, hs.URL, tenant, "成员", nil)

	var team AgentTeamView
	if status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams", tenant, nil,
		AgentTeamWriteRequest{Name: "有成员的队", Captain: agentteam.TeamMember{AgentID: captain.ID},
			Members: []agentteam.TeamMember{{AgentID: member.ID}}}, &team); status != http.StatusCreated {
		t.Fatalf("create team: status %d", status)
	}

	var refusal struct {
		Code       string           `json:"code"`
		References []map[string]any `json:"references"`
	}
	for _, agentID := range []string{captain.ID, member.ID} {
		status := call(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/agents/"+agentID, tenant, nil, nil, &refusal)
		if status != http.StatusConflict || refusal.Code != "agent_in_use" {
			t.Fatalf("delete %s: status %d code %q, want 409 agent_in_use", agentID, status, refusal.Code)
		}
		if len(refusal.References) != 1 || refusal.References[0]["id"] != team.ID {
			t.Fatalf("references = %+v, want the team %s", refusal.References, team.ID)
		}
	}

	// After the member leaves the team the agent is deletable.
	if status := call(t, http.DefaultClient, http.MethodDelete,
		hs.URL+"/v1/teams/"+team.ID+"/members/"+member.ID, tenant, nil, nil, nil); status != http.StatusOK {
		t.Fatalf("remove member: status %d", status)
	}
	if status := call(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/agents/"+member.ID, tenant, nil, nil, nil); status != http.StatusOK {
		t.Fatalf("delete unseated agent: status %d", status)
	}
}

// sessionRefusal is the wire shape of a registry refusal carrying references.
type sessionRefusal struct {
	Code       string           `json:"code"`
	Message    string           `json:"error"`
	References []map[string]any `json:"references"`
	Total      int              `json:"total"`
	Truncated  bool             `json:"truncated"`
}

// TestTeamDeletionAndArchiveRefuseSessionBindings pins the team half of the
// session reference guard: an active binding blocks both delete and archive with
// 409 team_in_use, archive the session then both succeed, and an archived
// session that still owns a live run keeps blocking.
func TestTeamDeletionAndArchiveRefuseSessionBindings(t *testing.T) {
	hs, registry := newRegistryServer(t)
	tenant := uuid.NewString()
	tenantID := uuid.MustParse(tenant)
	captain := createAgentFixture(t, hs.URL, tenant, "队长", nil)

	var team AgentTeamView
	if status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams", tenant, nil,
		AgentTeamWriteRequest{Name: "被会话绑定的队", Captain: agentteam.TeamMember{AgentID: captain.ID}}, &team); status != http.StatusCreated {
		t.Fatalf("create team: status %d", status)
	}
	session := uuid.NewString()
	registry.bindSession(tenantID, session, "", team.ID)

	var refusal sessionRefusal
	for _, request := range []struct {
		name   string
		method string
		path   string
	}{
		{"delete", http.MethodDelete, "/v1/teams/" + team.ID},
		{"archive", http.MethodPost, "/v1/teams/" + team.ID + "/archive"},
	} {
		status := call(t, http.DefaultClient, request.method, hs.URL+request.path, tenant, nil, nil, &refusal)
		if status != http.StatusConflict || refusal.Code != "team_in_use" {
			t.Fatalf("%s: status %d code %q, want 409 team_in_use", request.name, status, refusal.Code)
		}
		if len(refusal.References) != 1 {
			t.Fatalf("%s references = %+v, want one session", request.name, refusal.References)
		}
		reference := refusal.References[0]
		if reference["kind"] != "session" || reference["id"] != session || reference["state"] != "active" {
			t.Fatalf("%s reference = %+v, want kind=session id=%s state=active", request.name, reference, session)
		}
	}

	// An archived session with a live run still blocks: archive does not cancel.
	registry.archiveSession(tenantID, session, "running")
	status := call(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/teams/"+team.ID, tenant, nil, nil, &refusal)
	if status != http.StatusConflict || refusal.Code != "team_in_use" {
		t.Fatalf("archived-live delete: status %d code %q, want 409", status, refusal.Code)
	}
	if len(refusal.References) != 1 || refusal.References[0]["archived"] != true ||
		refusal.References[0]["runStatus"] != "running" {
		t.Fatalf("archived-live reference = %+v", refusal.References)
	}

	// Settle the run: the archived session stops blocking and archive succeeds,
	// advancing the revision and marking the view archived.
	registry.archiveSession(tenantID, session, "done")
	var archived AgentTeamView
	if status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams/"+team.ID+"/archive", tenant, nil, nil, &archived); status != http.StatusOK {
		t.Fatalf("archive retired team: status %d", status)
	}
	if !archived.Archived || archived.Revision != team.Revision+1 {
		t.Fatalf("archived view = %+v, want archived revision %d", archived, team.Revision+1)
	}
	// Idempotent: the second archive answers the same view.
	var again AgentTeamView
	if status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams/"+team.ID+"/archive", tenant, nil, nil, &again); status != http.StatusOK || !again.Archived {
		t.Fatalf("repeat archive: status %d view %+v", status, again)
	}
	// An archived team with no live binding is deletable.
	if status := call(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/teams/"+team.ID, tenant, nil, nil, nil); status != http.StatusOK {
		t.Fatalf("delete archived team: status %d", status)
	}
}

// TestAgentDeletionAndArchiveRefuseSessionBindings pins the agent half: delete
// lists both the team seat and the session binding; archive is the remedy for a
// team seat, so only the session binding blocks it; once the session is archived
// and the team seat removed, the agent is deletable.
func TestAgentDeletionAndArchiveRefuseSessionBindings(t *testing.T) {
	hs, registry := newRegistryServer(t)
	tenant := uuid.NewString()
	tenantID := uuid.MustParse(tenant)
	captain := createAgentFixture(t, hs.URL, tenant, "队长", nil)
	member := createAgentFixture(t, hs.URL, tenant, "成员", nil)

	var team AgentTeamView
	if status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams", tenant, nil,
		AgentTeamWriteRequest{Name: "座位队", Captain: agentteam.TeamMember{AgentID: captain.ID},
			Members: []agentteam.TeamMember{{AgentID: member.ID}}}, &team); status != http.StatusCreated {
		t.Fatalf("create team: status %d", status)
	}
	session := uuid.NewString()
	registry.bindSession(tenantID, session, member.ID, "")

	var refusal sessionRefusal
	status := call(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/agents/"+member.ID, tenant, nil, nil, &refusal)
	if status != http.StatusConflict || refusal.Code != "agent_in_use" {
		t.Fatalf("delete member: status %d code %q, want 409 agent_in_use", status, refusal.Code)
	}
	if len(refusal.References) != 2 {
		t.Fatalf("delete references = %+v, want the team and the session", refusal.References)
	}
	var sawTeam, sawSession bool
	for _, reference := range refusal.References {
		if reference["kind"] == "session" {
			sawSession = reference["id"] == session
			continue
		}
		sawTeam = reference["id"] == team.ID
	}
	if !sawTeam || !sawSession {
		t.Fatalf("delete references = %+v, want team %s and session %s", refusal.References, team.ID, session)
	}

	// Archive: the team seat does not block, the live session does.
	status = call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/agents/"+member.ID+"/archive", tenant, nil, nil, &refusal)
	if status != http.StatusConflict || refusal.Code != "agent_in_use" {
		t.Fatalf("archive member: status %d code %q, want 409 agent_in_use", status, refusal.Code)
	}
	if len(refusal.References) != 1 || refusal.References[0]["kind"] != "session" {
		t.Fatalf("archive references = %+v, want only the session", refusal.References)
	}

	// Archive the session and the agent archives (its team seat is ignored).
	registry.archiveSession(tenantID, session, "")
	var archived AgentView
	if status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/agents/"+member.ID+"/archive", tenant, nil, nil, &archived); status != http.StatusOK {
		t.Fatalf("archive agent: status %d", status)
	}
	if !archived.Archived {
		t.Fatalf("archived view = %+v, want archived", archived)
	}
	// Delete still refuses while the team seats the agent (the archive did not
	// silently remove the seat).
	status = call(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/agents/"+member.ID, tenant, nil, nil, &refusal)
	if status != http.StatusConflict || refusal.Code != "agent_in_use" || len(refusal.References) != 1 {
		t.Fatalf("post-archive delete: status %d code %q refs %+v, want team-only 409", status, refusal.Code, refusal.References)
	}
	if status := call(t, http.DefaultClient, http.MethodDelete,
		hs.URL+"/v1/teams/"+team.ID+"/members/"+member.ID, tenant, nil, nil, nil); status != http.StatusOK {
		t.Fatalf("remove archived member seat: status %d", status)
	}
	if status := call(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/agents/"+member.ID, tenant, nil, nil, nil); status != http.StatusOK {
		t.Fatalf("delete unseated archived agent: status %d", status)
	}
}

// TestSessionReferencesAreTenantScopedAndDoNotBlockUnrelatedDeletes pins the two
// negative cases: another tenant's binding is invisible, and a team/agent with
// no binding is never blocked by an unrelated one.
func TestSessionReferencesAreTenantScopedAndDoNotBlockUnrelatedDeletes(t *testing.T) {
	hs, registry := newRegistryServer(t)
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	captain := createAgentFixture(t, hs.URL, tenantA, "队长", nil)
	var teamA AgentTeamView
	if status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams", tenantA, nil,
		AgentTeamWriteRequest{Name: "A 队", Captain: agentteam.TeamMember{AgentID: captain.ID}}, &teamA); status != http.StatusCreated {
		t.Fatalf("create team A: status %d", status)
	}
	// A session in tenant B naming the same team id must not block tenant A.
	registry.bindSession(uuid.MustParse(tenantB), uuid.NewString(), "", teamA.ID)
	if status := call(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/teams/"+teamA.ID, tenantA, nil, nil, nil); status != http.StatusOK {
		t.Fatalf("foreign binding blocked tenant A delete: status %d", status)
	}

	// An unrelated team with no binding is always deletable.
	var teamQuiet AgentTeamView
	if status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams", tenantA, nil,
		AgentTeamWriteRequest{Name: "安静队", Captain: agentteam.TeamMember{AgentID: captain.ID}}, &teamQuiet); status != http.StatusCreated {
		t.Fatalf("create quiet team: status %d", status)
	}
	if status := call(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/teams/"+teamQuiet.ID, tenantA, nil, nil, nil); status != http.StatusOK {
		t.Fatalf("unrelated team delete: status %d", status)
	}
}

func TestTeamMemberAddRemove(t *testing.T) {
	hs, _ := newRegistryServer(t)
	tenant := uuid.NewString()
	captain := createAgentFixture(t, hs.URL, tenant, "队长", nil)
	member := createAgentFixture(t, hs.URL, tenant, "成员", nil)

	var team AgentTeamView
	if status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams", tenant, nil,
		AgentTeamWriteRequest{Name: "空队", Captain: agentteam.TeamMember{AgentID: captain.ID}}, &team); status != http.StatusCreated {
		t.Fatalf("create team: status %d", status)
	}

	var added AgentTeamView
	status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams/"+team.ID+"/members", tenant, nil,
		agentteam.TeamMember{AgentID: member.ID, Label: "设计"}, &added)
	if status != http.StatusCreated {
		t.Fatalf("add member: status %d, want 201", status)
	}
	if len(added.Members) != 1 || added.Members[0].AgentID != member.ID || added.Members[0].Label != "设计" {
		t.Fatalf("members = %+v", added.Members)
	}
	if added.Revision != team.Revision+1 {
		t.Fatalf("revision = %d, want %d", added.Revision, team.Revision+1)
	}

	// Adding the same agent twice is the domain's duplicate_member.
	var fields struct {
		Fields []agentteam.FieldError `json:"fields"`
	}
	status = call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams/"+added.ID+"/members", tenant, nil,
		agentteam.TeamMember{AgentID: member.ID}, &fields)
	if status != http.StatusBadRequest || !hasField(fields.Fields, "members[1].agentId", agentteam.CodeDuplicateMember) {
		t.Fatalf("duplicate member: status %d fields %+v", status, fields.Fields)
	}

	// A member that names no library agent is agent_not_found.
	status = call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams/"+added.ID+"/members", tenant, nil,
		agentteam.TeamMember{AgentID: "agent_ghost"}, &fields)
	if status != http.StatusBadRequest || !hasField(fields.Fields, "agentId", agentteam.CodeAgentNotFound) {
		t.Fatalf("ghost member: status %d fields %+v", status, fields.Fields)
	}

	// Removing the captain is refused: a team keeps exactly one captain.
	status = call(t, http.DefaultClient, http.MethodDelete,
		hs.URL+"/v1/teams/"+added.ID+"/members/"+captain.ID, tenant, nil, nil, &fields)
	if status != http.StatusBadRequest || !hasField(fields.Fields, "captain.agentId", agentteam.CodeMissingCaptain) {
		t.Fatalf("remove captain: status %d fields %+v", status, fields.Fields)
	}

	// Removing an absent member is a 404, not a silent success.
	if status := call(t, http.DefaultClient, http.MethodDelete,
		hs.URL+"/v1/teams/"+added.ID+"/members/agent_ghost", tenant, nil, nil, nil); status != http.StatusNotFound {
		t.Fatalf("remove absent member: status %d, want 404", status)
	}

	var removed AgentTeamView
	if status := call(t, http.DefaultClient, http.MethodDelete,
		hs.URL+"/v1/teams/"+added.ID+"/members/"+member.ID, tenant, nil, nil, &removed); status != http.StatusOK {
		t.Fatalf("remove member: status %d", status)
	}
	if len(removed.Members) != 0 || removed.Revision != added.Revision+1 {
		t.Fatalf("after remove: members %+v revision %d", removed.Members, removed.Revision)
	}
}

// --- captain team CLI permission ----------------------------------------

// captainTeamFixtures creates a captain agent and a team whose captain seat is
// it, then returns the team id. agentCLI is the captain's own teamCli; teamCLI
// is the team-level switch.
func captainTeamFixtures(t *testing.T, base, tenant string, agentCLI, teamCLI agentteam.TeamCLI) (string, string) {
	t.Helper()
	captain := createAgentFixture(t, base, tenant, "队长", map[string]any{
		"teamCli": map[string]any{"enabled": agentCLI.Enabled, "commands": agentCLI.Commands},
	})
	var team AgentTeamView
	if status := call(t, http.DefaultClient, http.MethodPost, base+"/v1/teams", tenant, nil, AgentTeamWriteRequest{
		Name:         "队",
		Captain:      agentteam.TeamMember{AgentID: captain.ID},
		Capabilities: agentteam.TeamCapabilities{TeamCLI: teamCLI},
	}, &team); status != http.StatusCreated {
		t.Fatalf("create team: status %d", status)
	}
	return team.ID, captain.ID
}

func TestCaptainTeamCLIPermissionAllowsAndDenies(t *testing.T) {
	hs, _ := newRegistryServer(t)
	tenant := uuid.NewString()
	member := createAgentFixture(t, hs.URL, tenant, "成员", nil)

	// Both sides enable member.add: the captain's call succeeds.
	teamID, captainID := captainTeamFixtures(t, hs.URL, tenant,
		agentteam.TeamCLI{Enabled: true, Commands: []string{"member.add", "member.remove"}},
		agentteam.TeamCLI{Enabled: true, Commands: []string{"member.add", "member.remove"}})
	actor := map[string]string{actorAgentHeader: captainID}

	var added AgentTeamView
	status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams/"+teamID+"/members", tenant, actor,
		agentteam.TeamMember{AgentID: member.ID}, &added)
	if status != http.StatusCreated || len(added.Members) != 1 {
		t.Fatalf("captain add with permission: status %d members %+v, want 201", status, added.Members)
	}

	// The intersection is the effective allowlist: both sides allow member.add
	// but not member.remove here, so the captain's remove is a typed refusal.
	narrowTeam, narrowCaptain := captainTeamFixtures(t, hs.URL, tenant,
		agentteam.TeamCLI{Enabled: true, Commands: []string{"member.add"}},
		agentteam.TeamCLI{Enabled: true, Commands: []string{"member.add"}})
	if status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams/"+narrowTeam+"/members", tenant,
		map[string]string{actorAgentHeader: narrowCaptain}, agentteam.TeamMember{AgentID: member.ID}, nil); status != http.StatusCreated {
		t.Fatalf("captain add on narrow team: status %d, want 201", status)
	}
	var refusal struct {
		Code string `json:"code"`
	}
	if status := call(t, http.DefaultClient, http.MethodDelete,
		hs.URL+"/v1/teams/"+narrowTeam+"/members/"+member.ID, tenant,
		map[string]string{actorAgentHeader: narrowCaptain}, nil, &refusal); status != http.StatusForbidden || refusal.Code != "team_cli_not_allowed" {
		t.Fatalf("captain remove not allowed: status %d code %q, want 403 team_cli_not_allowed", status, refusal.Code)
	}

	// A disabled team-level switch refuses everything, whatever the agent allows.
	disabledTeam, disabledCaptain := captainTeamFixtures(t, hs.URL, tenant,
		agentteam.TeamCLI{Enabled: true, Commands: []string{"member.add"}},
		agentteam.TeamCLI{})
	if status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams/"+disabledTeam+"/members", tenant,
		map[string]string{actorAgentHeader: disabledCaptain}, agentteam.TeamMember{AgentID: member.ID}, &refusal); status != http.StatusForbidden || refusal.Code != "team_cli_disabled" {
		t.Fatalf("disabled team cli: status %d code %q, want 403 team_cli_disabled", status, refusal.Code)
	}

	// A disabled agent-level switch refuses too (either side off is off).
	disabledAgent, disabledAgentCaptain := captainTeamFixtures(t, hs.URL, tenant,
		agentteam.TeamCLI{},
		agentteam.TeamCLI{Enabled: true, Commands: []string{"member.add"}})
	if status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams/"+disabledAgent+"/members", tenant,
		map[string]string{actorAgentHeader: disabledAgentCaptain}, agentteam.TeamMember{AgentID: member.ID}, &refusal); status != http.StatusForbidden || refusal.Code != "team_cli_disabled" {
		t.Fatalf("disabled agent cli: status %d code %q, want 403 team_cli_disabled", status, refusal.Code)
	}

	// A non-captain agent may not act as the team's captain.
	if status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams/"+teamID+"/members", tenant,
		map[string]string{actorAgentHeader: member.ID}, agentteam.TeamMember{AgentID: member.ID}, &refusal); status != http.StatusForbidden || refusal.Code != "not_captain" {
		t.Fatalf("non-captain actor: status %d code %q, want 403 not_captain", status, refusal.Code)
	}

	// The operator face (no actor header) is unaffected by the captain's switch:
	// the settings UI and the local CLI manage teams regardless.
	if status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams/"+disabledTeam+"/members", tenant, nil,
		agentteam.TeamMember{AgentID: member.ID}, nil); status != http.StatusCreated {
		t.Fatalf("operator add: status %d, want 201", status)
	}
}

// --- wiring and body guards ---------------------------------------------

func TestAgentTeamSurfacesAnswer503WithoutARegistry(t *testing.T) {
	hs := httptest.NewServer(New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	t.Cleanup(hs.Close)
	tenant := uuid.NewString()
	for _, path := range []string{"/v1/agents", "/v1/teams"} {
		if status := call(t, http.DefaultClient, http.MethodGet, hs.URL+path, tenant, nil, nil, nil); status != http.StatusServiceUnavailable {
			t.Fatalf("GET %s without registry: status %d, want 503", path, status)
		}
	}
}

func TestAgentTeamBodiesAreStrict(t *testing.T) {
	hs, _ := newRegistryServer(t)
	tenant := uuid.NewString()

	// Trailing JSON data.
	status := callRaw(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/agents", tenant,
		[]byte(`{"name":"a"}{"name":"b"}`), nil)
	if status != http.StatusBadRequest {
		t.Fatalf("trailing data: status %d, want 400", status)
	}
	// An unknown field inside the team body.
	var refusal struct {
		Code  string `json:"code"`
		Field string `json:"field"`
	}
	status = call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams", tenant, nil,
		map[string]any{"name": "x", "revision": 7, "captain": map[string]any{"agentId": "a"}}, &refusal)
	if status != http.StatusBadRequest || refusal.Code != BodyRejectionUnknownField || refusal.Field != "revision" {
		t.Fatalf("server-owned revision: status %d body %+v, want 400 unknown_field revision", status, refusal)
	}
	// An empty body.
	status = call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/agents", tenant, nil, nil, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("empty body: status %d, want 400", status)
	}
}

// TestAgentTeamRouteMatrix keeps the route list from drifting silently: every
// path the change promises must answer something other than 405.
func TestAgentTeamRouteMatrix(t *testing.T) {
	hs, _ := newRegistryServer(t)
	tenant := uuid.NewString()
	cases := []struct{ method, path string }{
		{http.MethodGet, "/v1/agents"},
		{http.MethodPost, "/v1/agents"},
		{http.MethodGet, "/v1/agents/a"},
		{http.MethodPut, "/v1/agents/a"},
		{http.MethodDelete, "/v1/agents/a"},
		{http.MethodGet, "/v1/teams"},
		{http.MethodPost, "/v1/teams"},
		{http.MethodGet, "/v1/teams/t"},
		{http.MethodPut, "/v1/teams/t"},
		{http.MethodDelete, "/v1/teams/t"},
		{http.MethodPost, "/v1/teams/t/members"},
		{http.MethodDelete, "/v1/teams/t/members/a"},
	}
	for _, tc := range cases {
		status := call(t, http.DefaultClient, tc.method, hs.URL+tc.path, tenant, nil, map[string]any{}, nil)
		// 404 is the expected answer for the id-bound routes (the fixtures were
		// never created); 405 would mean the route is not registered at all.
		if status == http.StatusMethodNotAllowed {
			t.Fatalf("%s %s: status 405, route not registered", tc.method, tc.path)
		}
		if status >= 500 {
			t.Fatalf("%s %s: status %d", tc.method, tc.path, status)
		}
	}
}

// TestSessionReferenceRefusalIsBounded pins the 409 envelope's size bound: the
// server returns at most 20 session references plus the real total/truncated
// flag, so a tenant with many bound sessions cannot turn the error body into an
// unbounded id dump.
func TestSessionReferenceRefusalIsBounded(t *testing.T) {
	hs, registry := newRegistryServer(t)
	tenant := uuid.NewString()
	tenantID := uuid.MustParse(tenant)
	captain := createAgentFixture(t, hs.URL, tenant, "队长", nil)
	var team AgentTeamView
	if status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams", tenant, nil,
		AgentTeamWriteRequest{Name: "多会话队", Captain: agentteam.TeamMember{AgentID: captain.ID}}, &team); status != http.StatusCreated {
		t.Fatalf("create team: status %d", status)
	}
	const bound = 25
	for i := 0; i < bound; i++ {
		registry.bindSession(tenantID, uuid.NewString(), "", team.ID)
	}
	var refusal sessionRefusal
	status := call(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/teams/"+team.ID, tenant, nil, nil, &refusal)
	if status != http.StatusConflict || refusal.Code != "team_in_use" {
		t.Fatalf("delete team: status %d code %q, want 409 team_in_use", status, refusal.Code)
	}
	if refusal.Total != bound || !refusal.Truncated {
		t.Fatalf("refusal total=%d truncated=%v, want %d/true", refusal.Total, refusal.Truncated, bound)
	}
	if len(refusal.References) != 20 {
		t.Fatalf("references = %d, want the 20-session bound", len(refusal.References))
	}
	if !strings.Contains(refusal.Message, "(+5 more)") {
		t.Fatalf("message = %q, want the (+5 more) marker", refusal.Message)
	}
}

// TestArchivedAgentCannotBeResurrectedByUpdate pins the lifecycle CAS on the
// update path: a PUT after archive keeps the archived flag (the handler carries
// the stored lifecycle value; the store refuses a stale write), so a retired
// agent cannot be silently brought back by an edit prepared before the archive.
func TestArchivedAgentCannotBeResurrectedByUpdate(t *testing.T) {
	hs, _ := newRegistryServer(t)
	tenant := uuid.NewString()
	agent := createAgentFixture(t, hs.URL, tenant, "待归档", nil)

	var archived AgentView
	if status := call(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/agents/"+agent.ID+"/archive", tenant, nil, nil, &archived); status != http.StatusOK || !archived.Archived {
		t.Fatalf("archive: status %d view %+v", status, archived)
	}

	var updated AgentView
	status := call(t, http.DefaultClient, http.MethodPut, hs.URL+"/v1/agents/"+agent.ID, tenant, nil,
		AgentWriteRequest{ID: agent.ID, Name: "改名后仍归档"}, &updated)
	if status != http.StatusOK {
		t.Fatalf("update archived agent: status %d", status)
	}
	if !updated.Archived {
		t.Fatalf("update resurrected the archived agent: %+v", updated)
	}
	var stored AgentView
	if status := call(t, http.DefaultClient, http.MethodGet, hs.URL+"/v1/agents/"+agent.ID, tenant, nil, nil, &stored); status != http.StatusOK || !stored.Archived {
		t.Fatalf("stored agent = %+v status %d, want archived", stored, status)
	}
}
