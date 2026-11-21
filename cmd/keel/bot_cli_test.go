package main

// CLI contract tests for the Bot/TeamBot product surface (A4.2b3a): the
// commands speak /v1/bots and /v1/team-bots, carry the account login + CSRF
// (or a bot key), and the retired `keel agent` / `keel team` names are unknown
// commands, not aliases.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBotCLIListsAndShowsOverBotEndpoints(t *testing.T) {
	fake := newFakeProductAPI(t)
	fake.bots["support-1"] = map[string]any{
		"botId": "support-1", "name": "Support One", "profileId": "p-1",
		"modelKind": "text", "defaultModel": map[string]any{"providerId": "magpie", "modelId": "qwen"},
		"harness": map[string]any{"tools": []any{"bash"}, "context": map[string]any{"permissions": []any{"session.read"}}},
	}
	setAccountEnv(t, "owner", "correct horse")
	code, stdout, stderr := runCLI(t, "bot", "list", "--server", fake.server.URL)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "support-1") || !strings.Contains(stdout, "magpie/qwen") {
		t.Fatalf("list output:\n%s", stdout)
	}
	fake.lastRequest(t, "GET", "/v1/bots")

	code, stdout, stderr = runCLI(t, "bot", "show", "support-1", "--server", fake.server.URL)
	if code != 0 || !strings.Contains(stdout, "permissions: session.read") {
		t.Fatalf("show exit=%d stderr=%s stdout=%s", code, stderr, stdout)
	}
}

func TestBotCLICreateCarriesHarnessAndDefaultModel(t *testing.T) {
	fake := newFakeProductAPI(t)
	setAccountEnv(t, "owner", "correct horse")
	code, _, stderr := runCLI(t, "bot", "create",
		"--server", fake.server.URL,
		"--name", "Support One",
		"--profile", "11111111-1111-1111-1111-111111111111",
		"--model-kind", "text",
		"--provider", "magpie",
		"--model", "qwen",
		"--tools", "bash,read",
		"--permissions", "session.read,session.write",
		"--instruction", "Be kind.",
	)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	request := fake.lastRequest(t, "POST", "/v1/bots")
	if request.CSRF == "" {
		t.Fatal("the write carried no CSRF token")
	}
	var body struct {
		Name         string            `json:"name"`
		ProfileID    string            `json:"profileId"`
		ModelKind    string            `json:"modelKind"`
		DefaultModel map[string]string `json:"defaultModel"`
		Harness      struct {
			Tools   []string `json:"tools"`
			Context struct {
				Instruction string   `json:"instruction"`
				Permissions []string `json:"permissions"`
			} `json:"context"`
		} `json:"harness"`
	}
	if err := json.Unmarshal(request.Body, &body); err != nil {
		t.Fatalf("body %s: %v", request.Body, err)
	}
	if body.Name != "Support One" || body.ProfileID != "11111111-1111-1111-1111-111111111111" ||
		body.ModelKind != "text" {
		t.Fatalf("identity body = %+v", body)
	}
	if body.DefaultModel["providerId"] != "magpie" || body.DefaultModel["modelId"] != "qwen" {
		t.Fatalf("model = %+v", body.DefaultModel)
	}
	if len(body.Harness.Tools) != 2 || body.Harness.Tools[0] != "bash" || body.Harness.Tools[1] != "read" {
		t.Fatalf("tools = %+v", body.Harness.Tools)
	}
	if body.Harness.Context.Instruction != "Be kind." || len(body.Harness.Context.Permissions) != 2 {
		t.Fatalf("context = %+v", body.Harness.Context)
	}
}

func TestBotCLIStartInvokesTheBot(t *testing.T) {
	fake := newFakeProductAPI(t)
	fake.bots["support-1"] = map[string]any{"botId": "support-1", "name": "Support One"}
	setAccountEnv(t, "owner", "correct horse")
	code, stdout, stderr := runCLI(t, "bot", "start", "support-1", "--server", fake.server.URL, "--text", "hello")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	request := fake.lastRequest(t, "POST", "/v1/bots/support-1/invoke")
	var body map[string]any
	if err := json.Unmarshal(request.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["text"] != "hello" {
		t.Fatalf("invoke body = %+v", body)
	}
	if !strings.Contains(stdout, "11111111-1111-1111-1111-111111111111") {
		t.Fatalf("start output:\n%s", stdout)
	}
}

func TestBotCLIBotKeyNeedsNoLogin(t *testing.T) {
	fake := newFakeProductAPI(t)
	setBotKeyEnv(t, "keel_bot_abcdefgh_secret")
	code, _, stderr := runCLI(t, "bot", "list", "--server", fake.server.URL)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	fake.mu.Lock()
	for _, request := range fake.requests {
		if request.Path == "/v1/auth/login" {
			fake.mu.Unlock()
			t.Fatal("a bot key performed an account login")
		}
	}
	fake.mu.Unlock()
	request := fake.lastRequest(t, "GET", "/v1/bots")
	if request.Auth != "Bearer keel_bot_abcdefgh_secret" {
		t.Fatalf("Authorization = %q", request.Auth)
	}
}

func TestBotKeyCreatePrintsThePlaintextOnce(t *testing.T) {
	fake := newFakeProductAPI(t)
	setAccountEnv(t, "owner", "correct horse")
	code, stdout, stderr := runCLI(t, "bot", "key-create", "support-1", "--server", fake.server.URL, "--label", "ci")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if strings.Count(stdout, "keel_bot_abcdefgh_plaintext") != 1 {
		t.Fatalf("the key plaintext must appear exactly once:\n%s", stdout)
	}
}

func TestTeamBotCLICreateRosterStart(t *testing.T) {
	fake := newFakeProductAPI(t)
	fake.teamBots["support-team"] = map[string]any{
		"teamBotId": "support-team", "name": "Support", "mission": "Resolve",
		"roster": []any{map[string]any{"botId": "captain", "role": "captain"}}, "revision": float64(1),
	}
	setAccountEnv(t, "owner", "correct horse")

	code, _, stderr := runCLI(t, "team-bot", "create", "--server", fake.server.URL,
		"--name", "Support", "--mission", "Resolve tickets",
		"--roster", "captain:captain,member:member")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, stderr)
	}
	request := fake.lastRequest(t, "POST", "/v1/team-bots")
	if !strings.Contains(string(request.Body), `"roster"`) {
		t.Fatalf("create body = %s", request.Body)
	}

	code, _, stderr = runCLI(t, "team-bot", "roster", "support-team", "--bot", "member",
		"--role", "member", "--server", fake.server.URL)
	if code != 0 {
		t.Fatalf("roster exit %d: %s", code, stderr)
	}
	request = fake.lastRequest(t, "POST", "/v1/team-bots/support-team/roster")
	if !strings.Contains(string(request.Body), `"expectedRevision":1`) {
		t.Fatalf("roster body = %s", request.Body)
	}

	code, stdout, stderr := runCLI(t, "team-bot", "start", "support-team",
		"--server", fake.server.URL, "--text", "go")
	if code != 0 {
		t.Fatalf("start exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "bot_captain") {
		t.Fatalf("start output:\n%s", stdout)
	}
	fake.lastRequest(t, "POST", "/v1/team-bots/support-team/start")
}

func TestRetiredAgentAndTeamCommandsAreUnknown(t *testing.T) {
	for _, retired := range [][]string{
		{"agent", "list"},
		{"team", "list"},
	} {
		code, stdout, stderr := runCLI(t, retired...)
		if code != 2 {
			t.Fatalf("%v exit = %d, want 2 (stdout=%q)", retired, code, stdout)
		}
		if !strings.Contains(stderr, "unknown command") {
			t.Fatalf("%v stderr = %q, want an unknown-command refusal", retired, stderr)
		}
		if strings.Contains(stderr, "keel bot") && strings.Contains(stderr, "Manage the Agent") {
			t.Fatalf("%v still advertises the old surface: %s", retired, stderr)
		}
	}
}

func TestBotCLIUpdatePreservesUnchangedHarnessAndModel(t *testing.T) {
	fake := newFakeProductAPI(t)
	fake.bots["support-1"] = map[string]any{
		"botId": "support-1", "name": "Support One", "profileId": "p-1",
		"modelKind": "text", "defaultModel": map[string]any{"providerId": "magpie", "modelId": "qwen"},
		"harness": map[string]any{
			"tools":   []any{"bash", "read"},
			"context": map[string]any{"instruction": "old", "permissions": []any{"session.read"}},
		},
	}
	setAccountEnv(t, "owner", "correct horse")
	code, _, stderr := runCLI(t, "bot", "update", "support-1", "--instruction", "new",
		"--server", fake.server.URL)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	request := fake.lastRequest(t, "PATCH", "/v1/bots/support-1")
	var body struct {
		Harness struct {
			Tools   []string `json:"tools"`
			Context struct {
				Instruction string   `json:"instruction"`
				Permissions []string `json:"permissions"`
			} `json:"context"`
		} `json:"harness"`
		DefaultModel map[string]string `json:"defaultModel"`
	}
	if err := json.Unmarshal(request.Body, &body); err != nil {
		t.Fatalf("body %s: %v", request.Body, err)
	}
	if len(body.Harness.Tools) != 2 || body.Harness.Context.Instruction != "new" ||
		len(body.Harness.Context.Permissions) != 1 {
		t.Fatalf("merged harness = %+v", body.Harness)
	}
	if _, present := body.DefaultModel["providerId"]; present {
		t.Fatalf("an untouched default model was rewritten: %+v", body.DefaultModel)
	}
}
