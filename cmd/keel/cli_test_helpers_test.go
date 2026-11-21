package main

// Shared fakes for the CLI client tests (`keel bot`, `keel team-bot`,
// `keel model`, `keel skills`). They speak the account login + CSRF protocol and
// the Bot/TeamBot HTTP surface, so a command test exercises the same credential
// path a real deployment does.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// recordedRequest is one request the fake server received.
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Auth   string
	CSRF   string
	Body   []byte
}

// writeFakeJSON answers one JSON document.
func writeFakeJSON(w http.ResponseWriter, status int, body any) {
	raw, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// runCLI invokes the command tree in-process.
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// setAccountEnv is the one credential a test gives the CLI: a human account.
// Every other auth variable is cleared so a developer's shell cannot leak in.
func setAccountEnv(t *testing.T, username, password string) {
	t.Helper()
	t.Setenv("KEEL_ACCOUNT_USERNAME", username)
	t.Setenv("KEEL_ACCOUNT_PASSWORD", password)
	t.Setenv("KEEL_AGENT_TOKEN", "")
	t.Setenv("KEEL_SERVE_PASSWORD", "")
	cliAuthUsername, cliAuthPasswordFile = "", ""
}

// setBotKeyEnv gives the CLI a machine credential: no username, no password.
func setBotKeyEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv("KEEL_ACCOUNT_USERNAME", "")
	t.Setenv("KEEL_ACCOUNT_PASSWORD", "")
	t.Setenv("KEEL_AGENT_TOKEN", key)
	t.Setenv("KEEL_SERVE_PASSWORD", "")
	cliAuthUsername, cliAuthPasswordFile = "", ""
}

// fakeProductAPI is the account-login + Bot/TeamBot surface the client commands
// talk to. It records every request and answers from the maps a test seeds.
type fakeProductAPI struct {
	mu       sync.Mutex
	username string
	password string
	csrf     string
	requests []recordedRequest
	server   *httptest.Server

	bots     map[string]map[string]any
	teamBots map[string]map[string]any
	lastKey  map[string]any
}

func newFakeProductAPI(t *testing.T) *fakeProductAPI {
	t.Helper()
	f := &fakeProductAPI{
		username: "owner",
		password: "correct horse",
		csrf:     "csrf-test-token",
		bots:     map[string]map[string]any{},
		teamBots: map[string]map[string]any{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/auth/session", func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, 200, map[string]any{"csrfToken": f.csrf, "authenticated": false})
	})
	mux.HandleFunc("POST /v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "login")
		if !f.writeAllowed(w, r) {
			return
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Username != f.username || body.Password != f.password {
			writeFakeJSON(w, 401, map[string]any{"error": "invalid credentials", "code": "invalid_credentials"})
			return
		}
		writeFakeJSON(w, 200, map[string]any{"csrfToken": f.csrf, "authenticated": true})
	})
	mux.HandleFunc("GET /v1/bots", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "bots.list")
		bots := make([]map[string]any, 0, len(f.bots))
		for _, bot := range f.bots {
			bots = append(bots, bot)
		}
		writeFakeJSON(w, 200, map[string]any{"bots": bots})
	})
	mux.HandleFunc("POST /v1/bots", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "bots.create")
		if !f.writeAllowed(w, r) {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		botID, _ := body["botId"].(string)
		if botID == "" {
			botID = "bot_minted"
		}
		bot := map[string]any{"botId": botID, "name": body["name"], "profileId": body["profileId"],
			"modelKind": body["modelKind"], "defaultModel": body["defaultModel"], "harness": body["harness"]}
		f.bots[botID] = bot
		writeFakeJSON(w, 201, map[string]any{"bot": bot})
	})
	mux.HandleFunc("GET /v1/bots/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "bots.get")
		bot, ok := f.bots[r.PathValue("id")]
		if !ok {
			writeFakeJSON(w, 404, map[string]any{"error": "not found", "code": "not_found"})
			return
		}
		writeFakeJSON(w, 200, map[string]any{"bot": bot})
	})
	mux.HandleFunc("PATCH /v1/bots/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "bots.update")
		if !f.writeAllowed(w, r) {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bot, ok := f.bots[r.PathValue("id")]
		if !ok {
			writeFakeJSON(w, 404, map[string]any{"error": "not found", "code": "not_found"})
			return
		}
		for key, value := range body {
			if key == "expectedArchived" {
				continue
			}
			bot[key] = value
		}
		writeFakeJSON(w, 200, map[string]any{"bot": bot})
	})
	mux.HandleFunc("POST /v1/bots/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "bots.archive")
		if !f.writeAllowed(w, r) {
			return
		}
		bot, ok := f.bots[r.PathValue("id")]
		if !ok {
			writeFakeJSON(w, 404, map[string]any{"error": "not found", "code": "not_found"})
			return
		}
		bot["archived"] = true
		writeFakeJSON(w, 200, map[string]any{"botId": r.PathValue("id"), "archived": true})
	})
	mux.HandleFunc("POST /v1/bots/{id}/invoke", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "bots.invoke")
		if !f.writeAllowed(w, r) {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeFakeJSON(w, 201, map[string]any{"botId": r.PathValue("id"), "sessionId": "11111111-1111-1111-1111-111111111111",
			"seq": 1, "requestId": body["requestId"], "deduped": false})
	})
	mux.HandleFunc("GET /v1/bots/{id}/keys", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "bots.keys.list")
		writeFakeJSON(w, 200, map[string]any{"keys": []any{}})
	})
	mux.HandleFunc("POST /v1/bots/{id}/keys", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "bots.keys.create")
		if !f.writeAllowed(w, r) {
			return
		}
		f.lastKey = map[string]any{
			"key":       map[string]any{"keyId": "22222222-2222-2222-2222-222222222222", "prefix": "keel_bot_abcdefgh"},
			"plaintext": "keel_bot_abcdefgh_plaintext", "label": "test",
		}
		writeFakeJSON(w, 201, f.lastKey)
	})
	mux.HandleFunc("DELETE /v1/bots/{id}/keys/{keyId}", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "bots.keys.revoke")
		if !f.writeAllowed(w, r) {
			return
		}
		writeFakeJSON(w, 200, map[string]any{"revoked": true})
	})
	mux.HandleFunc("GET /v1/team-bots", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "teamBots.list")
		teams := make([]map[string]any, 0, len(f.teamBots))
		for _, team := range f.teamBots {
			teams = append(teams, team)
		}
		writeFakeJSON(w, 200, map[string]any{"teamBots": teams})
	})
	mux.HandleFunc("POST /v1/team-bots", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "teamBots.create")
		if !f.writeAllowed(w, r) {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		teamID, _ := body["teamBotId"].(string)
		if teamID == "" {
			teamID = "team_minted"
		}
		team := map[string]any{"teamBotId": teamID, "name": body["name"], "mission": body["mission"],
			"roster": body["roster"], "revision": 1, "hash": "h"}
		f.teamBots[teamID] = team
		writeFakeJSON(w, 201, map[string]any{"teamBot": team})
	})
	mux.HandleFunc("GET /v1/team-bots/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "teamBots.get")
		team, ok := f.teamBots[r.PathValue("id")]
		if !ok {
			writeFakeJSON(w, 404, map[string]any{"error": "not found", "code": "not_found"})
			return
		}
		writeFakeJSON(w, 200, map[string]any{"teamBot": team})
	})
	mux.HandleFunc("PATCH /v1/team-bots/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "teamBots.update")
		if !f.writeAllowed(w, r) {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		team, ok := f.teamBots[r.PathValue("id")]
		if !ok {
			writeFakeJSON(w, 404, map[string]any{"error": "not found", "code": "not_found"})
			return
		}
		for key, value := range body {
			if key == "expectedRevision" {
				continue
			}
			team[key] = value
		}
		team["revision"] = float64(2)
		writeFakeJSON(w, 200, map[string]any{"teamBot": team})
	})
	mux.HandleFunc("POST /v1/team-bots/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "teamBots.archive")
		if !f.writeAllowed(w, r) {
			return
		}
		team, ok := f.teamBots[r.PathValue("id")]
		if !ok {
			writeFakeJSON(w, 404, map[string]any{"error": "not found", "code": "not_found"})
			return
		}
		team["archived"] = true
		writeFakeJSON(w, 200, map[string]any{"teamBotId": r.PathValue("id"), "archived": true})
	})
	mux.HandleFunc("POST /v1/team-bots/{id}/roster", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "teamBots.roster")
		if !f.writeAllowed(w, r) {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		team, ok := f.teamBots[r.PathValue("id")]
		if !ok {
			writeFakeJSON(w, 404, map[string]any{"error": "not found", "code": "not_found"})
			return
		}
		team["roster"] = body["roster"]
		team["revision"] = float64(2)
		writeFakeJSON(w, 200, map[string]any{"teamBot": team})
	})
	mux.HandleFunc("POST /v1/team-bots/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "teamBots.start")
		if !f.writeAllowed(w, r) {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeFakeJSON(w, 201, map[string]any{"teamBotId": r.PathValue("id"), "captainBotId": "bot_captain",
			"sessionId": "11111111-1111-1111-1111-111111111111", "seq": 1,
			"requestId": body["requestId"], "deduped": false})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeProductAPI) record(r *http.Request, label string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw := []byte{}
	if r.Body != nil {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		raw = buf.Bytes()
		r.Body = io.NopCloser(bytes.NewReader(raw))
	}
	f.requests = append(f.requests, recordedRequest{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		Auth: r.Header.Get("Authorization"), CSRF: r.Header.Get("X-Keel-CSRF"), Body: raw,
	})
}

func (f *fakeProductAPI) writeAllowed(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet {
		return true
	}
	if r.Header.Get("X-Keel-CSRF") != f.csrf {
		writeFakeJSON(w, 403, map[string]any{"error": "csrf token required", "code": "csrf_failed"})
		return false
	}
	return true
}

func (f *fakeProductAPI) lastRequest(t *testing.T, method, path string) recordedRequest {
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
