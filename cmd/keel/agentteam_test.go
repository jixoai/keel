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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
	Query  string
	Actor  string
	Body   []byte
}

// fakeEvent is one committed session event.
type fakeEvent struct {
	Seq     int64
	Kind    string
	Payload map[string]any
}

// fakeSession is the session/run state the runtime commands read back.
type fakeSession struct {
	ID        string
	RunStatus string
	Selection map[string]string
	Text      string
	RequestID string
	Cancelled bool
	Events    []fakeEvent
}

// fakeTeamAPI is a minimal server: cookie login, agent CRUD, team CRUD and
// member add/remove, plus the session/run surface `agent start|stop` and
// `team start` use (create session, commit input, list sessions/events,
// cancel).
type fakeTeamAPI struct {
	mu       sync.Mutex
	password string
	agents   map[string]httpapi.AgentView
	teams    map[string]httpapi.AgentTeamView
	sessions map[string]*fakeSession
	requests []recordedRequest
	// refuse, when set, answers every write with this status and body.
	refuseStatus int
	refuseBody   map[string]any
	// inputRefuse, when set, answers POST /input with this status and body
	// after the session exists (the start's cleanup path).
	inputRefuseStatus int
	inputRefuseBody   map[string]any
	// cancelAnswer, when set, is answered verbatim by POST /cancel and the
	// session is left untouched, so a settlement race can be modelled.
	cancelAnswer map[string]any
	// sessionsHasMore/sessionsNextCursor model a tenant whose session list
	// outlives the stop scan's bounded window.
	sessionsHasMore    bool
	sessionsNextCursor string
}

func newFakeTeamAPI(password string) *fakeTeamAPI {
	return &fakeTeamAPI{
		password: password,
		agents:   make(map[string]httpapi.AgentView),
		teams:    make(map[string]httpapi.AgentTeamView),
		sessions: make(map[string]*fakeSession),
	}
}

func (f *fakeTeamAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		Actor: r.Header.Get(actorAgentHeader), Body: raw,
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
	case r.URL.Path == "/v1/sessions" && r.Method == http.MethodGet:
		page := make([]map[string]any, 0, len(f.sessions))
		for _, session := range f.sessions {
			page = append(page, map[string]any{
				"sessionId": session.ID,
				"runStatus": session.RunStatus,
				"updatedAt": "2026-11-20T00:00:00Z",
			})
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"sessions": page, "nextCursor": f.sessionsNextCursor, "hasMore": f.sessionsHasMore,
		})
	case r.URL.Path == "/v1/sessions" && r.Method == http.MethodPost:
		var body struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(raw, &body)
		id := body.SessionID
		if id == "" {
			id = uuid.NewString()
		}
		if f.sessions[id] == nil {
			f.sessions[id] = &fakeSession{ID: id}
		}
		writeFakeJSON(w, http.StatusCreated, map[string]any{"sessionId": id})
	case strings.HasPrefix(r.URL.Path, "/v1/sessions/"):
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/sessions/"), "/")
		session := f.sessions[parts[0]]
		if session == nil {
			writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
			return
		}
		switch {
		case len(parts) == 2 && parts[1] == "input" && r.Method == http.MethodPost:
			if f.inputRefuseStatus != 0 {
				writeFakeJSON(w, f.inputRefuseStatus, f.inputRefuseBody)
				return
			}
			var body struct {
				Text      string            `json:"text"`
				RequestID string            `json:"requestId"`
				Selection map[string]string `json:"selection"`
			}
			_ = json.Unmarshal(raw, &body)
			session.Text = body.Text
			session.RequestID = body.RequestID
			session.Selection = body.Selection
			session.RunStatus = "pending"
			session.Events = append(session.Events, fakeEvent{
				Seq:  int64(len(session.Events) + 1),
				Kind: "user.message",
				Payload: map[string]any{
					"text": body.Text, "requestId": body.RequestID, "selection": body.Selection,
				},
			})
			writeFakeJSON(w, http.StatusCreated, map[string]any{
				"submissionId": uuid.NewString(),
				"requestId":    body.RequestID,
				"status":       "pending",
				"seq":          len(session.Events),
				"created":      true,
			})
		case len(parts) == 2 && parts[1] == "events" && r.Method == http.MethodGet:
			after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			if limit <= 0 {
				limit = 1000
			}
			events := make([]map[string]any, 0, len(session.Events))
			for _, event := range session.Events {
				if event.Seq <= after {
					continue
				}
				if len(events) >= limit {
					break
				}
				events = append(events, map[string]any{
					"seq": event.Seq, "kind": event.Kind, "payload": event.Payload,
				})
			}
			writeFakeJSON(w, http.StatusOK, map[string]any{"events": events})
		case len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost:
			if f.cancelAnswer != nil {
				writeFakeJSON(w, http.StatusOK, f.cancelAnswer)
				return
			}
			session.Cancelled = true
			session.RunStatus = "cancelled"
			writeFakeJSON(w, http.StatusOK, map[string]any{
				"sessionId": session.ID, "status": "cancelled", "runStatus": "cancelled",
				"cancelled": true, "detail": "the in-process attempt was cancelled",
			})
		case len(parts) == 1 && r.Method == http.MethodDelete:
			delete(f.sessions, session.ID)
			writeFakeJSON(w, http.StatusOK, map[string]any{"sessionId": session.ID, "archived": true})
		case len(parts) == 2 && parts[1] == "run" && r.Method == http.MethodGet:
			writeFakeJSON(w, http.StatusOK, map[string]any{
				"status": session.RunStatus, "attempts": 1, "cursorSeq": len(session.Events),
			})
		default:
			writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		}
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

// --- runtime faces: agent start|stop, team save|start ---------------------

// createCLIAgent creates an agent through the CLI and returns its id.
func createCLIAgent(t *testing.T, api *fakeTeamAPI, server, name string) string {
	t.Helper()
	if code, _, stderr := runCLI(t, "agent", "create", "--server", server, "--name", name); code != 0 {
		t.Fatalf("create agent %q: exit %d stderr %s", name, code, stderr)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for id, view := range api.agents {
		if view.Name == name {
			return id
		}
	}
	t.Fatalf("agent %q was not stored", name)
	return ""
}

func TestAgentStartCreatesSessionAndQueuesRun(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)
	t.Setenv("KEEL_SERVE_PASSWORD", "")

	agentID := createCLIAgent(t, api, hs.URL, "运行主理人")
	code, stdout, stderr := runCLI(t, "agent", "start", agentID, "--server", hs.URL, "--text", "请开始工作")
	if code != 0 {
		t.Fatalf("agent start: exit %d stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "started agent "+agentID) {
		t.Fatalf("start output = %q", stdout)
	}
	if !strings.Contains(stdout, "sessionId:") || !strings.Contains(stdout, "runId:") {
		t.Fatalf("start output lacks sessionId/runId: %q", stdout)
	}

	created := api.lastRequest(t, http.MethodPost, "/v1/sessions")
	if len(created.Body) != 0 && string(created.Body) != "{}" {
		t.Fatalf("session create body = %s", created.Body)
	}
	var sessions int
	api.mu.Lock()
	sessions = len(api.sessions)
	api.mu.Unlock()
	if sessions != 1 {
		t.Fatalf("sessions = %d, want 1", sessions)
	}
	var sessionID string
	api.mu.Lock()
	for id := range api.sessions {
		sessionID = id
	}
	api.mu.Unlock()

	input := api.lastRequest(t, http.MethodPost, "/v1/sessions/"+sessionID+"/input")
	var body struct {
		Text      string            `json:"text"`
		RequestID string            `json:"requestId"`
		Selection map[string]string `json:"selection"`
	}
	if err := json.Unmarshal(input.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Text != "请开始工作" {
		t.Fatalf("text = %q", body.Text)
	}
	if body.Selection["agentId"] != agentID || len(body.Selection) != 1 {
		t.Fatalf("selection = %+v, want agentId only (the server merges the default model)", body.Selection)
	}
	if !strings.HasPrefix(body.RequestID, "keel-start-") {
		t.Fatalf("requestId = %q", body.RequestID)
	}
}

func TestAgentStopCancelsTheRunningSession(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)
	t.Setenv("KEEL_SERVE_PASSWORD", "")

	agentID := createCLIAgent(t, api, hs.URL, "被停止者")
	if code, _, stderr := runCLI(t, "agent", "start", agentID, "--server", hs.URL, "--text", "开始"); code != 0 {
		t.Fatalf("agent start: exit %d stderr %s", code, stderr)
	}

	api.mu.Lock()
	var sessionID string
	for id, session := range api.sessions {
		sessionID = id
		session.RunStatus = "running"
	}
	api.mu.Unlock()
	if sessionID == "" {
		t.Fatal("no session was created")
	}

	code, stdout, stderr := runCLI(t, "agent", "stop", agentID, "--server", hs.URL)
	if code != 0 {
		t.Fatalf("agent stop: exit %d stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "stopped session "+sessionID) {
		t.Fatalf("stop output = %q", stdout)
	}
	cancelled := api.lastRequest(t, http.MethodPost, "/v1/sessions/"+sessionID+"/cancel")
	if cancelled.Method != http.MethodPost {
		t.Fatalf("cancel request = %+v", cancelled)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if !api.sessions[sessionID].Cancelled || api.sessions[sessionID].RunStatus != "cancelled" {
		t.Fatalf("session after stop = %+v", api.sessions[sessionID])
	}
}

func TestAgentStopWithoutActiveRunIsARefusal(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)

	agentID := createCLIAgent(t, api, hs.URL, "空闲者")
	code, stdout, stderr := runCLI(t, "agent", "stop", agentID, "--server", hs.URL)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "no active run for agent "+agentID) {
		t.Fatalf("stderr = %q, want the explicit no-active message", stderr)
	}
	if strings.Contains(stdout, "stopped") {
		t.Fatalf("stdout pretends a cancellation: %q", stdout)
	}
}

func TestTeamSaveUpsertsYAMLAndIgnoresServerOwnedFields(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)

	captainID := createCLIAgent(t, api, hs.URL, "队长")
	memberID := createCLIAgent(t, api, hs.URL, "成员")
	document := `id: team_file
name: 文件小队
description: 第一次保存
tenantId: 9f6c4c1e-2b7a-4f7a-9a6e-2f0f4b3d5a11
status: valid
revision: 7
hash: stale-hash
archived: false
captain:
  agentId: ` + captainID + `
members:
  - agentId: ` + memberID + `
    label: 设计
defaults:
  tools: [bash, read]
capabilities:
  teamCli:
    enabled: true
    commands: [member.add]
`
	path := filepath.Join(t.TempDir(), "team.yaml")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI(t, "team", "save", path, "--server", hs.URL)
	if code != 0 {
		t.Fatalf("first save: exit %d stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "saved team team_file") || !strings.Contains(stdout, "created") {
		t.Fatalf("first save output = %q", stdout)
	}
	created := api.lastRequest(t, http.MethodPost, "/v1/teams")
	var write httpapi.AgentTeamWriteRequest
	if err := json.Unmarshal(created.Body, &write); err != nil {
		t.Fatal(err)
	}
	if write.ID != "team_file" || write.Name != "文件小队" || write.Captain.AgentID != captainID {
		t.Fatalf("team write body = %s", created.Body)
	}
	if len(write.Members) != 1 || write.Members[0].Label != "设计" {
		t.Fatalf("members = %+v", write.Members)
	}
	if len(write.Defaults.Tools) != 2 || !write.Capabilities.TeamCLI.Enabled {
		t.Fatalf("defaults/capabilities = %+v %+v", write.Defaults, write.Capabilities)
	}
	// The server-owned fields are ignored, never forwarded: a client cannot pin
	// a revision or a hash.
	if strings.Contains(string(created.Body), "revision") || strings.Contains(string(created.Body), "tenantId") {
		t.Fatalf("server-owned fields reached the wire: %s", created.Body)
	}

	code, stdout, stderr = runCLI(t, "team", "save", path, "--server", hs.URL)
	if code != 0 {
		t.Fatalf("second save: exit %d stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "updated") || !strings.Contains(stdout, "revision=2") {
		t.Fatalf("second save output = %q", stdout)
	}
	updated := api.lastRequest(t, http.MethodPut, "/v1/teams/team_file")
	if !strings.Contains(string(updated.Body), `"id":"team_file"`) {
		t.Fatalf("update body = %s", updated.Body)
	}
}

func TestTeamSaveRejectsUnknownFields(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)

	path := filepath.Join(t.TempDir(), "team.yaml")
	if err := os.WriteFile(path, []byte("id: team_bad\nname: 坏队\nbogus: true\ncaptain:\n  agentId: agent_x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCLI(t, "team", "save", path, "--server", hs.URL)
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr, "bogus") {
		t.Fatalf("stderr = %q, want the unknown field named", stderr)
	}
	if len(api.requests) != 0 {
		t.Fatalf("an invalid file still sent requests: %+v", api.requests)
	}
}

func TestTeamStartUsesTheTeamBinding(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)
	t.Setenv("KEEL_SERVE_PASSWORD", "")

	captainID := createCLIAgent(t, api, hs.URL, "团队队长")
	if code, _, stderr := runCLI(t, "team", "create", "--server", hs.URL,
		"--id", "team_run", "--name", "启动小队", "--captain", captainID); code != 0 {
		t.Fatalf("team create: exit %d stderr %s", code, stderr)
	}

	code, stdout, stderr := runCLI(t, "team", "start", "team_run", "--server", hs.URL, "--text", "团队开始")
	if code != 0 {
		t.Fatalf("team start: exit %d stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "started team team_run") || !strings.Contains(stdout, "runId:") {
		t.Fatalf("team start output = %q", stdout)
	}
	var sessionID string
	api.mu.Lock()
	for id := range api.sessions {
		sessionID = id
	}
	api.mu.Unlock()
	input := api.lastRequest(t, http.MethodPost, "/v1/sessions/"+sessionID+"/input")
	var body struct {
		Selection map[string]string `json:"selection"`
	}
	if err := json.Unmarshal(input.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Selection["teamId"] != "team_run" || len(body.Selection) != 1 {
		t.Fatalf("selection = %+v, want teamId only", body.Selection)
	}
}

func TestAgentDeleteRendersReferenceRefusal(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)

	api.mu.Lock()
	api.refuseStatus = http.StatusConflict
	api.refuseBody = map[string]any{
		"error": `agent "agent_x" is a captain or member of team_1; remove it from those teams before deleting it`,
		"code":  "agent_in_use",
		"references": []map[string]any{
			{"id": "team_1", "name": "产品小队"},
		},
	}
	api.mu.Unlock()

	code, _, stderr := runCLI(t, "agent", "delete", "agent_x", "--server", hs.URL)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	for _, want := range []string{"agent_in_use", "referencing team: team_1 产品小队"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want %q", stderr, want)
		}
	}
}

// seedSession stores one live session whose newest user.message carries a
// selection, which is what the stop scan reads.
func seedSession(api *fakeTeamAPI, sessionID, runStatus string, selection map[string]string) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.sessions[sessionID] = &fakeSession{
		ID:        sessionID,
		RunStatus: runStatus,
		Selection: selection,
		Events: []fakeEvent{{
			Seq:     1,
			Kind:    "user.message",
			Payload: map[string]any{"text": "seed", "selection": selection},
		}},
	}
}

// hasRequest reports whether the CLI sent method+path (the stop scan must not
// cancel a session another agent owns).
func (f *fakeTeamAPI) hasRequest(method, path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, request := range f.requests {
		if request.Method == method && request.Path == path {
			return true
		}
	}
	return false
}

func TestAgentStartRefusesAnArchivedAgent(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)

	agentID := createCLIAgent(t, api, hs.URL, "已归档")
	api.mu.Lock()
	view := api.agents[agentID]
	view.Archived = true
	api.agents[agentID] = view
	api.mu.Unlock()

	code, stdout, stderr := runCLI(t, "agent", "start", agentID, "--server", hs.URL)
	if code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %s)", code, stderr)
	}
	if !strings.Contains(stderr, "is archived") {
		t.Fatalf("stderr = %q, want the archived refusal", stderr)
	}
	if strings.Contains(stdout, "started") {
		t.Fatalf("stdout pretends a start: %q", stdout)
	}
	api.mu.Lock()
	sessions := len(api.sessions)
	api.mu.Unlock()
	if sessions != 0 {
		t.Fatalf("an archived agent still created %d session(s)", sessions)
	}
}

func TestTeamStartRefusesAnInvalidTeam(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)

	api.mu.Lock()
	api.teams["team_bad"] = httpapi.AgentTeamView{
		ID: "team_bad", Name: "无队长", Status: agentteam.StatusInvalid,
	}
	api.mu.Unlock()

	code, stdout, stderr := runCLI(t, "team", "start", "team_bad", "--server", hs.URL)
	if code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %s)", code, stderr)
	}
	if !strings.Contains(stderr, "not startable") {
		t.Fatalf("stderr = %q, want the invalid-team refusal", stderr)
	}
	if strings.Contains(stdout, "started") {
		t.Fatalf("stdout pretends a start: %q", stdout)
	}
	api.mu.Lock()
	sessions := len(api.sessions)
	api.mu.Unlock()
	if sessions != 0 {
		t.Fatalf("an invalid team still created %d session(s)", sessions)
	}
}

func TestAgentStopMatchesTheCaptainOfATeamSession(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)

	agentID := createCLIAgent(t, api, hs.URL, "团队队长")
	api.mu.Lock()
	api.teams["team_cap"] = httpapi.NewAgentTeamView(fakeTenant, agentteam.AgentTeam{
		ID: "team_cap", Name: "队长队",
		Captain: agentteam.TeamMember{AgentID: agentID},
	})
	api.mu.Unlock()
	const sessionID = "00000000-0000-0000-0000-0000000000c1"
	seedSession(api, sessionID, "running", map[string]string{"teamId": "team_cap"})

	code, stdout, stderr := runCLI(t, "agent", "stop", agentID, "--server", hs.URL)
	if code != 0 {
		t.Fatalf("exit %d, want 0 (stderr %s)", code, stderr)
	}
	if !strings.Contains(stdout, "stopped session "+sessionID) {
		t.Fatalf("stdout = %q, want the captain's session stopped", stdout)
	}
	if !api.hasRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/cancel") {
		t.Fatal("the captain's team session was not cancelled")
	}
}

func TestAgentStopGivesTeamBindingPriorityOverAgentBinding(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)

	targetID := createCLIAgent(t, api, hs.URL, "被误认的成员")
	otherCaptainID := createCLIAgent(t, api, hs.URL, "真正的队长")
	api.mu.Lock()
	api.teams["team_other"] = httpapi.NewAgentTeamView(fakeTenant, agentteam.AgentTeam{
		ID: "team_other", Name: "别人的队",
		Captain: agentteam.TeamMember{AgentID: otherCaptainID},
	})
	api.mu.Unlock()
	const sessionID = "00000000-0000-0000-0000-0000000000c2"
	// The selection contract says teamId wins: this is the other captain's run,
	// even though the payload also names the target agent.
	seedSession(api, sessionID, "running", map[string]string{
		"teamId": "team_other", "agentId": targetID,
	})

	code, _, stderr := runCLI(t, "agent", "stop", targetID, "--server", hs.URL)
	if code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %s)", code, stderr)
	}
	if !strings.Contains(stderr, "no active run") {
		t.Fatalf("stderr = %q, want the no-active refusal", stderr)
	}
	if api.hasRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/cancel") {
		t.Fatal("a team session was cancelled for a non-captain agentId")
	}
}

func TestAgentStopSettlementRaceIsNotReportedAsSuccess(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)

	agentID := createCLIAgent(t, api, hs.URL, "刚好跑完")
	const sessionID = "00000000-0000-0000-0000-0000000000c3"
	seedSession(api, sessionID, "running", map[string]string{"agentId": agentID})
	api.mu.Lock()
	api.cancelAnswer = map[string]any{
		"sessionId": sessionID, "status": "settled", "runStatus": "done",
		"cancelled": false, "detail": "the run already reached a terminal state",
	}
	api.mu.Unlock()

	code, stdout, stderr := runCLI(t, "agent", "stop", agentID, "--server", hs.URL)
	if code != 1 {
		t.Fatalf("exit %d, want 1: a settled race must not be a fake success (stderr %s)", code, stderr)
	}
	if !strings.Contains(stdout, "already settled") {
		t.Fatalf("stdout = %q, want the settled state reported", stdout)
	}
	if strings.Contains(stdout, "stopped session") {
		t.Fatalf("stdout claims a cancellation: %q", stdout)
	}
}

func TestAgentStopJSONIsTheOutcomeArray(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)

	agentID := createCLIAgent(t, api, hs.URL, "JSON 停止")
	const sessionID = "00000000-0000-0000-0000-0000000000c4"
	seedSession(api, sessionID, "running", map[string]string{"agentId": agentID})

	code, stdout, stderr := runCLI(t, "agent", "stop", agentID, "--server", hs.URL, "--json")
	if code != 0 {
		t.Fatalf("exit %d, want 0 (stderr %s)", code, stderr)
	}
	var outcomes []stopOutcome
	if err := json.Unmarshal([]byte(stdout), &outcomes); err != nil {
		t.Fatalf("--json output is not the outcome array: %v (%q)", err, stdout)
	}
	if len(outcomes) != 1 || outcomes[0].SessionID != sessionID || !outcomes[0].Cancelled {
		t.Fatalf("outcomes = %+v", outcomes)
	}
}

func TestAgentStartDiscardsTheSessionWhenInputIsRefused(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)

	agentID := createCLIAgent(t, api, hs.URL, "拒绝输入")
	api.mu.Lock()
	api.inputRefuseStatus = http.StatusBadRequest
	api.inputRefuseBody = map[string]any{
		"error": `selection binding is not available: agent "` + agentID + `" has no usable default model`,
		"code":  "selection_unavailable",
	}
	api.mu.Unlock()

	code, stdout, stderr := runCLI(t, "agent", "start", agentID, "--server", hs.URL)
	if code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %s)", code, stderr)
	}
	if !strings.Contains(stderr, "selection_unavailable") {
		t.Fatalf("stderr = %q, want the typed refusal", stderr)
	}
	if strings.Contains(stdout, "started") {
		t.Fatalf("stdout pretends a start: %q", stdout)
	}
	api.mu.Lock()
	var deleted string
	for _, request := range api.requests {
		if request.Method == http.MethodDelete && strings.HasPrefix(request.Path, "/v1/sessions/") {
			deleted = strings.TrimPrefix(request.Path, "/v1/sessions/")
		}
	}
	sessions := len(api.sessions)
	api.mu.Unlock()
	if deleted == "" {
		t.Fatal("a refused input left the empty session behind (no DELETE was sent)")
	}
	if sessions != 0 {
		t.Fatalf("the empty session still exists after the cleanup (%d sessions)", sessions)
	}
}

func TestTeamSaveRequiresAStableID(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)

	path := filepath.Join(t.TempDir(), "team.yaml")
	if err := os.WriteFile(path, []byte("name: 无 id 队\ncaptain:\n  agentId: agent_x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCLI(t, "team", "save", path, "--server", hs.URL)
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr, "must carry id") {
		t.Fatalf("stderr = %q, want the id requirement", stderr)
	}
	if len(api.requests) != 0 {
		t.Fatalf("an id-less document still sent requests: %+v", api.requests)
	}
}

func TestAgentStopDoesNotResurrectAnOlderBindingAfterAModelOnlySelection(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)

	agentID := createCLIAgent(t, api, hs.URL, "旧绑定")
	const sessionID = "00000000-0000-0000-0000-0000000000d1"
	api.mu.Lock()
	api.sessions[sessionID] = &fakeSession{
		ID:        sessionID,
		RunStatus: "running",
		Events: []fakeEvent{
			{Seq: 1, Kind: "user.message", Payload: map[string]any{
				"selection": map[string]any{"agentId": agentID},
			}},
			// The newest selection names only a model: the runner's
			// runSelection makes this attempt unbound, so the earlier agent
			// binding is superseded and must not be stopped.
			{Seq: 2, Kind: "user.message", Payload: map[string]any{
				"selection": map[string]any{"modelId": "model_only"},
			}},
		},
	}
	api.mu.Unlock()

	code, stdout, stderr := runCLI(t, "agent", "stop", agentID, "--server", hs.URL)
	if code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %s)", code, stderr)
	}
	if !strings.Contains(stderr, "no active run") {
		t.Fatalf("stderr = %q, want the no-active refusal", stderr)
	}
	if strings.Contains(stdout, "stopped session") {
		t.Fatalf("stdout claims a cancellation: %q", stdout)
	}
	if api.hasRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/cancel") {
		t.Fatal("a model-only selection was attributed to an older agent binding")
	}
}

func TestAgentStopWarnsWhenTheSessionWindowIsTruncated(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)

	agentID := createCLIAgent(t, api, hs.URL, "窗口外")
	api.mu.Lock()
	api.sessionsHasMore = true
	api.sessionsNextCursor = "next-page"
	api.mu.Unlock()

	code, _, stderr := runCLI(t, "agent", "stop", agentID, "--server", hs.URL)
	if code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %s)", code, stderr)
	}
	if !strings.Contains(stderr, "scanned at most the 1000 most recently active sessions") {
		t.Fatalf("stderr = %q, want the bounded-window warning", stderr)
	}
	api.mu.Lock()
	pages := 0
	for _, request := range api.requests {
		if request.Method == http.MethodGet && request.Path == "/v1/sessions" {
			pages++
		}
	}
	api.mu.Unlock()
	if pages != maxSessionPages {
		t.Fatalf("session-list requests = %d, want the bounded %d", pages, maxSessionPages)
	}
}

func TestAgentStopNewestSelectionPagesUntilAShortPage(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)
	t.Setenv("KEEL_SERVE_PASSWORD", "")

	client, err := newCLIClient(hs.URL)
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "00000000-0000-0000-0000-0000000000d2"
	api.mu.Lock()
	api.sessions[sessionID] = &fakeSession{
		ID:        sessionID,
		RunStatus: "running",
		Events: []fakeEvent{
			{Seq: 1, Kind: "user.message", Payload: map[string]any{
				"selection": map[string]any{"agentId": "agent_old"},
			}},
			{Seq: 2, Kind: "user.message", Payload: map[string]any{
				"selection": map[string]any{"modelId": "model_only"},
			}},
			{Seq: 3, Kind: "user.message", Payload: map[string]any{
				"selection": map[string]any{"agentId": "agent_new"},
			}},
		},
	}
	api.mu.Unlock()

	// A two-event page forces a second request; the newest fact is on page 2.
	selection, err := newestSelectionPage(client, sessionID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if selection == nil || selection.AgentID != "agent_new" {
		t.Fatalf("selection = %+v, want the newest page's agent_new", selection)
	}

	api.mu.Lock()
	var eventPages []recordedRequest
	for _, request := range api.requests {
		if request.Method == http.MethodGet && strings.HasSuffix(request.Path, "/events") {
			eventPages = append(eventPages, request)
		}
	}
	api.mu.Unlock()
	if len(eventPages) != 2 {
		t.Fatalf("event requests = %d, want 2 (a full page must continue)", len(eventPages))
	}
	if !strings.Contains(eventPages[0].Query, "limit=2") {
		t.Fatalf("first event request query = %q, want limit=2", eventPages[0].Query)
	}
	if !strings.Contains(eventPages[1].Query, "after=2") {
		t.Fatalf("second event request query = %q, want after=2", eventPages[1].Query)
	}
}

func TestAgentStopNewestSelectionFailsClosedAtThePageBound(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)
	t.Setenv("KEEL_SERVE_PASSWORD", "")

	client, err := newCLIClient(hs.URL)
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "00000000-0000-0000-0000-0000000000d3"
	events := make([]fakeEvent, 0, maxEventPages*2+1)
	for seq := 1; seq <= maxEventPages*2+1; seq++ {
		events = append(events, fakeEvent{
			Seq: int64(seq), Kind: "user.message",
			Payload: map[string]any{"selection": map[string]any{"agentId": "agent_x"}},
		})
	}
	api.mu.Lock()
	api.sessions[sessionID] = &fakeSession{ID: sessionID, RunStatus: "running", Events: events}
	api.mu.Unlock()

	// Two events per page: after maxEventPages full pages the scan cannot know
	// whether a newer selection exists, so it must refuse to attribute.
	_, err = newestSelectionPage(client, sessionID, 2)
	if !errors.Is(err, errSelectionScanTruncated) {
		t.Fatalf("err = %v, want errSelectionScanTruncated", err)
	}
}

func TestTeamSaveRoundTripsTheShowExport(t *testing.T) {
	api := newFakeTeamAPI("")
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)

	captainID := createCLIAgent(t, api, hs.URL, "往返队长")
	if code, _, stderr := runCLI(t, "team", "create", "--server", hs.URL,
		"--id", "team_roundtrip", "--name", "往返小队", "--captain", captainID); code != 0 {
		t.Fatalf("team create: exit %d stderr %s", code, stderr)
	}
	code, exported, stderr := runCLI(t, "team", "show", "team_roundtrip", "--server", hs.URL)
	if code != 0 {
		t.Fatalf("team show: exit %d stderr %s", code, stderr)
	}
	for _, serverOwned := range []string{`"revision"`, `"hash"`, `"status"`, `"tenantId"`} {
		if !strings.Contains(exported, serverOwned) {
			t.Fatalf("show export lacks %s: %s", serverOwned, exported)
		}
	}

	path := filepath.Join(t.TempDir(), "team.json")
	if err := os.WriteFile(path, []byte(exported), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "team", "save", path, "--server", hs.URL)
	if code != 0 {
		t.Fatalf("save of the show export: exit %d stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "updated") || !strings.Contains(stdout, "revision=2") {
		t.Fatalf("save output = %q", stdout)
	}
	updated := api.lastRequest(t, http.MethodPut, "/v1/teams/team_roundtrip")
	for _, serverOwned := range []string{"revision", "hash", "status", "tenantId", "archived"} {
		if strings.Contains(string(updated.Body), `"`+serverOwned+`"`) {
			t.Fatalf("server-owned field %q reached the wire: %s", serverOwned, updated.Body)
		}
	}
}
