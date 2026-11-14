package main

// CLI conformance tests for `keel agent` / `keel team`. They run the real
// command functions against an httptest fake of the server surface, so the
// assertions are the wire contract: which method/path/body the CLI sends, that
// the passphrase is exchanged at /v1/auth/login and never printed, that an
// update preserves the fields the command line did not mention, and that the
// captain's actor header is forwarded.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentteam"
	"github.com/jixoai/keel/internal/httpapi"
)

// recordedRequest is one request the CLI sent.
type recordedRequest struct {
	Method string
	Path   string
	Actor  string
	Body   []byte
}

// fakeTeamAPI is a minimal server: cookie login, agent CRUD, team CRUD and
// member add/remove. It keeps the state the CLI reads back.
type fakeTeamAPI struct {
	mu       sync.Mutex
	password string
	agents   map[string]httpapi.AgentView
	teams    map[string]httpapi.AgentTeamView
	requests []recordedRequest
	// refuse, when set, answers every write with this status and body.
	refuseStatus int
	refuseBody   map[string]any
}

func newFakeTeamAPI(password string) *fakeTeamAPI {
	return &fakeTeamAPI{
		password: password,
		agents:   make(map[string]httpapi.AgentView),
		teams:    make(map[string]httpapi.AgentTeamView),
	}
}

func (f *fakeTeamAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		Method: r.Method, Path: r.URL.Path, Actor: r.Header.Get(actorAgentHeader), Body: raw,
	})
	f.mu.Unlock()

	if r.Method == http.MethodPost && r.URL.Path == "/v1/auth/login" {
		var body struct {
			Password string `json:"password"`
		}
		_ = json.Unmarshal(raw, &body)
		if strings.TrimSpace(body.Password) != f.password {
			writeFakeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid passphrase"})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: authCookieName, Value: "fake-session", Path: "/"})
		writeFakeJSON(w, http.StatusOK, map[string]any{"authenticated": true})
		return
	}
	if f.password != "" {
		if cookie, err := r.Cookie(authCookieName); err != nil || cookie.Value != "fake-session" {
			writeFakeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "authentication required: POST /v1/auth/login with the server passphrase",
			})
			return
		}
	}
	if r.Method != http.MethodGet && f.refuseStatus != 0 {
		writeFakeJSON(w, f.refuseStatus, f.refuseBody)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/v1/agents" && r.Method == http.MethodGet:
		list := make([]httpapi.AgentView, 0, len(f.agents))
		for _, view := range f.agents {
			list = append(list, view)
		}
		writeFakeJSON(w, http.StatusOK, list)
	case r.URL.Path == "/v1/agents" && r.Method == http.MethodPost:
		var body httpapi.AgentWriteRequest
		_ = json.Unmarshal(raw, &body)
		id := body.ID
		if id == "" {
			id = agentteam.GenerateAgentID()
		}
		view := httpapi.NewAgentView(fakeTenant, body.Agent())
		view.ID = id
		view.Avatar = view.Avatar.WithDefaults(id)
		f.agents[id] = view
		writeFakeJSON(w, http.StatusCreated, view)
	case strings.HasPrefix(r.URL.Path, "/v1/agents/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/agents/")
		view, found := f.agents[id]
		if !found {
			writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": "agent " + id + " does not exist"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeFakeJSON(w, http.StatusOK, view)
		case http.MethodPut:
			var body httpapi.AgentWriteRequest
			_ = json.Unmarshal(raw, &body)
			next := httpapi.NewAgentView(fakeTenant, body.Agent())
			next.ID = id
			f.agents[id] = next
			writeFakeJSON(w, http.StatusOK, next)
		case http.MethodDelete:
			delete(f.agents, id)
			writeFakeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
		}
	case r.URL.Path == "/v1/teams" && r.Method == http.MethodGet:
		list := make([]httpapi.AgentTeamView, 0, len(f.teams))
		for _, view := range f.teams {
			list = append(list, view)
		}
		writeFakeJSON(w, http.StatusOK, list)
	case r.URL.Path == "/v1/teams" && r.Method == http.MethodPost:
		var body httpapi.AgentTeamWriteRequest
		_ = json.Unmarshal(raw, &body)
		id := body.ID
		if id == "" {
			id = agentteam.GenerateTeamID()
		}
		team := body.Team()
		team.ID = id
		team.Seal()
		view := httpapi.NewAgentTeamView(fakeTenant, *team)
		f.teams[id] = view
		writeFakeJSON(w, http.StatusCreated, view)
	case strings.HasPrefix(r.URL.Path, "/v1/teams/"):
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/teams/"), "/")
		id := parts[0]
		view, found := f.teams[id]
		if !found {
			writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": "team " + id + " does not exist"})
			return
		}
		switch {
		case len(parts) == 1 && r.Method == http.MethodGet:
			writeFakeJSON(w, http.StatusOK, view)
		case len(parts) == 1 && r.Method == http.MethodPut:
			var body httpapi.AgentTeamWriteRequest
			_ = json.Unmarshal(raw, &body)
			team := body.Team()
			team.ID = id
			team.Seal()
			for range view.Revision {
				team.Seal()
			}
			next := httpapi.NewAgentTeamView(fakeTenant, *team)
			f.teams[id] = next
			writeFakeJSON(w, http.StatusOK, next)
		case len(parts) == 1 && r.Method == http.MethodDelete:
			delete(f.teams, id)
			writeFakeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
		case len(parts) == 2 && parts[1] == "members" && r.Method == http.MethodPost:
			var member agentteam.TeamMember
			_ = json.Unmarshal(raw, &member)
			view.Members = append(view.Members, member)
			view.Revision++
			view.Hash = "hash-" + strings.Repeat("x", int(view.Revision))
			f.teams[id] = view
			writeFakeJSON(w, http.StatusOK, view)
		case len(parts) == 3 && parts[1] == "members" && r.Method == http.MethodDelete:
			kept := make([]agentteam.TeamMember, 0, len(view.Members))
			for _, member := range view.Members {
				if member.AgentID != parts[2] {
					kept = append(kept, member)
				}
			}
			view.Members = kept
			view.Revision++
			view.Hash = "hash-" + strings.Repeat("y", int(view.Revision))
			f.teams[id] = view
			writeFakeJSON(w, http.StatusOK, view)
		default:
			writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		}
	default:
		writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

// fakeTenant is the tenant the fake projects into its views.
var fakeTenant = uuid.MustParse("9f6c4c1e-2b7a-4f7a-9a6e-2f0f4b3d5a11")

func writeFakeJSON(w http.ResponseWriter, status int, body any) {
	raw, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func (f *fakeTeamAPI) lastRequest(t *testing.T, method, path string) recordedRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := len(f.requests) - 1; index >= 0; index-- {
		if f.requests[index].Method == method && f.requests[index].Path == path {
			return f.requests[index]
		}
	}
	t.Fatalf("no %s %s request recorded (requests: %+v)", method, path, f.requests)
	return recordedRequest{}
}

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// --- tests ---------------------------------------------------------------

func TestAgentCLILoginAndNoPasswordLeak(t *testing.T) {
	api := newFakeTeamAPI("s3cret-passphrase")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)
	t.Setenv("KEEL_SERVE_PASSWORD", "wrong-passphrase")

	code, stdout, stderr := runCLI(t, "agent", "list", "--server", hs.URL)
	if code != 1 {
		t.Fatalf("wrong passphrase: exit %d, want 1 (stderr %s)", code, stderr)
	}
	if !strings.Contains(stderr, "login") {
		t.Fatalf("stderr = %q, want a login failure", stderr)
	}
	if strings.Contains(stdout+stderr, "wrong-passphrase") {
		t.Fatalf("the passphrase was printed: stdout=%q stderr=%q", stdout, stderr)
	}

	t.Setenv("KEEL_SERVE_PASSWORD", "s3cret-passphrase")
	code, stdout, stderr = runCLI(t, "agent", "list", "--server", hs.URL)
	if code != 0 {
		t.Fatalf("list: exit %d stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "ID") || !strings.Contains(stdout, "NAME") {
		t.Fatalf("list output = %q", stdout)
	}
	if strings.Contains(stdout+stderr, "s3cret-passphrase") {
		t.Fatalf("the passphrase was printed: stdout=%q stderr=%q", stdout, stderr)
	}
	// The login carried the passphrase in its body (not a flag, not a URL).
	login := api.lastRequest(t, http.MethodPost, "/v1/auth/login")
	if !strings.Contains(string(login.Body), "s3cret-passphrase") {
		t.Fatalf("login body = %s", login.Body)
	}
}

func TestAgentCLICreateShowUpdatePreservesFields(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)
	t.Setenv("KEEL_SERVE_PASSWORD", "")

	code, stdout, stderr := runCLI(t, "agent", "create", "--server", hs.URL,
		"--name", "设计主理人", "--provider", "prov_a", "--model", "model_a",
		"--tool", "bash,read", "--prompt-file", "AGENTS.md",
		"--team-cli", "--team-cli-commands", "member.add")
	if code != 0 {
		t.Fatalf("create: exit %d stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "created agent agent_") {
		t.Fatalf("create output = %q", stdout)
	}
	created := api.lastRequest(t, http.MethodPost, "/v1/agents")
	var write httpapi.AgentWriteRequest
	if err := json.Unmarshal(created.Body, &write); err != nil {
		t.Fatal(err)
	}
	if write.Name != "设计主理人" || len(write.Tools) != 2 || write.SystemPrompt.Ref != "AGENTS.md" {
		t.Fatalf("create body = %s", created.Body)
	}
	if !write.TeamCLI.Enabled || len(write.TeamCLI.Commands) != 1 {
		t.Fatalf("teamCli in body = %+v", write.TeamCLI)
	}

	var agentID string
	api.mu.Lock()
	for id := range api.agents {
		agentID = id
	}
	api.mu.Unlock()

	if code, stdout, stderr = runCLI(t, "agent", "show", agentID, "--server", hs.URL); code != 0 {
		t.Fatalf("show: exit %d stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, `"name": "设计主理人"`) {
		t.Fatalf("show output = %q", stdout)
	}

	// A partial update must not clear the fields the command line omitted.
	if code, _, stderr = runCLI(t, "agent", "update", agentID, "--server", hs.URL, "--name", "设计主管"); code != 0 {
		t.Fatalf("update: exit %d stderr %s", code, stderr)
	}
	updated := api.lastRequest(t, http.MethodPut, "/v1/agents/"+agentID)
	if err := json.Unmarshal(updated.Body, &write); err != nil {
		t.Fatal(err)
	}
	if write.Name != "设计主管" {
		t.Fatalf("updated name = %q", write.Name)
	}
	if len(write.Tools) != 2 || write.SystemPrompt.Ref != "AGENTS.md" || !write.TeamCLI.Enabled {
		t.Fatalf("partial update dropped fields: %s", updated.Body)
	}
	if write.DefaultModel.ProviderID != "prov_a" || write.DefaultModel.ModelID != "model_a" {
		t.Fatalf("partial update dropped the model: %+v", write.DefaultModel)
	}

	if code, stdout, stderr = runCLI(t, "agent", "delete", agentID, "--server", hs.URL); code != 0 {
		t.Fatalf("delete: exit %d stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "deleted agent") {
		t.Fatalf("delete output = %q", stdout)
	}
}

func TestAgentCLIRendersFieldValidation(t *testing.T) {
	api := newFakeTeamAPI("")
	api.refuseStatus = http.StatusBadRequest
	api.refuseBody = map[string]any{
		"error": "name: name is required",
		"fields": []map[string]any{
			{"field": "name", "code": "missing_name", "message": "name is required"},
		},
	}
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)

	code, _, stderr := runCLI(t, "agent", "create", "--server", hs.URL, "--name", "x")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "name (missing_name)") {
		t.Fatalf("stderr = %q, want the field path and code", stderr)
	}
}

func TestTeamCLICreateMemberAddRemove(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)
	t.Setenv("KEEL_SERVE_PASSWORD", "")

	if code, _, stderr := runCLI(t, "agent", "create", "--server", hs.URL, "--name", "队长"); code != 0 {
		t.Fatalf("create captain: exit %d stderr %s", code, stderr)
	}
	if code, _, stderr := runCLI(t, "agent", "create", "--server", hs.URL, "--name", "成员"); code != 0 {
		t.Fatalf("create member: exit %d stderr %s", code, stderr)
	}
	var captainID, memberID string
	api.mu.Lock()
	for id, view := range api.agents {
		if view.Name == "队长" {
			captainID = id
		} else {
			memberID = id
		}
	}
	api.mu.Unlock()

	code, stdout, stderr := runCLI(t, "team", "create", "--server", hs.URL,
		"--name", "产品小队", "--captain", captainID, "--team-cli",
		"--team-cli-commands", "member.add,member.remove")
	if code != 0 {
		t.Fatalf("team create: exit %d stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "created team team_") {
		t.Fatalf("team create output = %q", stdout)
	}
	var teamID string
	api.mu.Lock()
	for id := range api.teams {
		teamID = id
	}
	api.mu.Unlock()

	code, stdout, stderr = runCLI(t, "team", "member", "add", teamID, memberID,
		"--server", hs.URL, "--label", "设计")
	if code != 0 {
		t.Fatalf("member add: exit %d stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "added agent "+memberID) || !strings.Contains(stdout, "members=1") {
		t.Fatalf("member add output = %q", stdout)
	}
	added := api.lastRequest(t, http.MethodPost, "/v1/teams/"+teamID+"/members")
	var member agentteam.TeamMember
	if err := json.Unmarshal(added.Body, &member); err != nil {
		t.Fatal(err)
	}
	if member.AgentID != memberID || member.Label != "设计" {
		t.Fatalf("member body = %s", added.Body)
	}
	if added.Actor != "" {
		t.Fatalf("operator call carried an actor header %q", added.Actor)
	}

	code, stdout, stderr = runCLI(t, "team", "list", "--server", hs.URL)
	if code != 0 {
		t.Fatalf("team list: exit %d stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, teamID) || !strings.Contains(stdout, "产品小队") {
		t.Fatalf("team list output = %q", stdout)
	}

	code, stdout, stderr = runCLI(t, "team", "member", "remove", teamID, memberID, "--server", hs.URL)
	if code != 0 {
		t.Fatalf("member remove: exit %d stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "members=0") {
		t.Fatalf("member remove output = %q", stdout)
	}
}

func TestTeamCLIMemberAddErrorsWithoutCaptain(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)

	code, _, stderr := runCLI(t, "team", "create", "--server", hs.URL, "--name", "缺队长")
	if code != 2 {
		t.Fatalf("exit %d, want 2 (usage)", code)
	}
	if !strings.Contains(stderr, "--captain") {
		t.Fatalf("stderr = %q, want the missing flag named", stderr)
	}
	if len(api.requests) != 0 {
		t.Fatalf("a usage error still sent requests: %+v", api.requests)
	}
}

func TestTeamCLIMemberAddAsCaptainForwardsActor(t *testing.T) {
	api := newFakeTeamAPI("pass")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)
	t.Setenv("KEEL_SERVE_PASSWORD", "pass")

	if code, _, stderr := runCLI(t, "team", "create", "--server", hs.URL,
		"--name", "队", "--captain", "agent_cap"); code != 0 {
		t.Fatalf("team create: exit %d stderr %s", code, stderr)
	}
	var teamID string
	api.mu.Lock()
	for id := range api.teams {
		teamID = id
	}
	api.mu.Unlock()

	// A refused captain call is rendered with its typed code.
	api.mu.Lock()
	api.refuseStatus = http.StatusForbidden
	api.refuseBody = map[string]any{
		"error": `team command "member.add" is not available to the captain (disabled)`,
		"code":  "team_cli_disabled",
	}
	api.mu.Unlock()
	code, _, stderr := runCLI(t, "team", "member", "add", teamID, "agent_x", "--server", hs.URL, "--as", "agent_cap")
	if code != 1 {
		t.Fatalf("refused captain add: exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "team_cli_disabled") {
		t.Fatalf("stderr = %q, want the typed refusal code", stderr)
	}
	refused := api.lastRequest(t, http.MethodPost, "/v1/teams/"+teamID+"/members")
	if refused.Actor != "agent_cap" {
		t.Fatalf("actor header = %q, want agent_cap", refused.Actor)
	}

	// With the switch on, the same call succeeds and still declares the actor.
	api.mu.Lock()
	api.refuseStatus = 0
	api.refuseBody = nil
	api.mu.Unlock()
	code, stdout, stderr := runCLI(t, "team", "member", "add", teamID, "agent_x", "--server", hs.URL, "--as", "agent_cap")
	if code != 0 {
		t.Fatalf("captain add: exit %d stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "added agent agent_x") {
		t.Fatalf("output = %q", stdout)
	}
}

func TestAgentCLIUnknownCommandAndHelp(t *testing.T) {
	if code, _, _ := runCLI(t, "agent", "nope"); code != 2 {
		t.Fatalf("unknown agent command: exit %d, want 2", code)
	}
	if code, stdout, _ := runCLI(t, "agent", "help"); code != 0 || !strings.Contains(stdout, "keel agent list") {
		t.Fatalf("agent help: exit %d output %q", code, stdout)
	}
	if code, stdout, _ := runCLI(t, "team", "--help"); code != 0 || !strings.Contains(stdout, "keel team member add") {
		t.Fatalf("team help: exit %d output %q", code, stdout)
	}
}
