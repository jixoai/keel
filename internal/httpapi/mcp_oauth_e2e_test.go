// End-to-end: HTTP sign-in stores the grant, the callback reconnects the same
// process's MCP manager, and an agent tool call then reaches the MCP server with
// the new access token. Skipped without KEEL_TEST_DSN.

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/mcp/oauth"
	"github.com/jixoai/keel/internal/store"
)

const e2eAccessToken = "e2e-access-token"

func TestMCPOAuthEndToEndSignInReconnectAndToolCall(t *testing.T) {
	dsn := skipUnlessDSN(t)
	_ = dsn
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "mcp-oauth-e2e"); err != nil {
		t.Fatal(err)
	}
	box, err := store.NewSecretBox(bytes.Repeat([]byte{0x44}, 32))
	if err != nil {
		t.Fatal(err)
	}
	serverName := "e2e-" + uuid.NewString()[:8]

	as := startOAuthTestAuthorizationServer(t)
	as.mu.Lock()
	as.accessToken = e2eAccessToken
	as.mu.Unlock()

	var toolCalls int32
	mcpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-protected-resource/mcp" {
			writeJSON(w, 200, map[string]any{
				"resource":              "http://" + r.Host + "/mcp",
				"authorization_servers": []string{as.origin},
			})
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(200)
			return
		}
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+e2eAccessToken {
			w.Header().Set("www-authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("Unauthorized"))
			return
		}
		var message map[string]any
		_ = json.Unmarshal([]byte(readAll(r)), &message)
		if _, ok := message["id"]; !ok {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch message["method"] {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "e2e", "version": "1.0.0"},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name":        "echo",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
			}}}
		case "tools/call":
			atomic.AddInt32(&toolCalls, 1)
			text := ""
			if params, ok := message["params"].(map[string]any); ok {
				if arguments, ok := params["arguments"].(map[string]any); ok {
					text, _ = arguments["text"].(string)
				}
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "pong:" + text}}}
		default:
			result = map[string]any{}
		}
		writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": message["id"], "result": result})
	}))
	defer mcpServer.Close()
	serverURL := mcpServer.URL + "/mcp"

	storage := func(tenantID uuid.UUID, server string) oauth.ServerStore {
		return oauth.NewPostgresStore(st, box, tenantID, server)
	}
	var challengeMu sync.Mutex
	var challengeServer string
	var challenges []oauth.Challenge
	manager := mcp.Connect(ctx, []mcp.ServerEntry{{
		Name: serverName,
		Config: map[string]any{
			"url":   serverURL,
			"oauth": map[string]any{},
		},
	}}, mcp.ConnectOptions{
		ClientName: "keel-e2e",
		TenantID:   tenant,
		OAuth: &mcp.OAuthOptions{
			Storage:    storage,
			ClientName: "keel-e2e",
			HTTPClient: &http.Client{},
			OnChallenge: func(server string, challenge oauth.Challenge) {
				challengeMu.Lock()
				challengeServer = server
				challenges = append(challenges, challenge)
				challengeMu.Unlock()
			},
		},
	})
	defer manager.Close()
	before := manager.Servers()
	if len(before) != 1 {
		t.Fatalf("boot servers = %#v", before)
	}
	var required *oauth.AuthorizationRequiredError
	if !errors.As(before[0].Err, &required) {
		t.Fatalf("boot error = %v, want AuthorizationRequiredError", before[0].Err)
	}
	challengeMu.Lock()
	challengeCount := len(challenges)
	challengeFrom := challengeServer
	challengeMu.Unlock()
	if challengeCount == 0 || challengeFrom != serverName {
		t.Fatalf("OnChallenge calls = %d from %q, want the boot 401 of %q to be visible",
			challengeCount, challengeFrom, serverName)
	}

	config := &MCPOAuth{
		Servers: func(_ uuid.UUID, name string) (MCPOAuthServer, error) {
			if name != serverName {
				return MCPOAuthServer{}, errors.New("unknown MCP server")
			}
			return MCPOAuthServer{URL: serverURL, Settings: oauth.Settings{}}, nil
		},
		NewStore: func(tenantID uuid.UUID, name string) *oauth.PostgresStore {
			return oauth.NewPostgresStore(st, box, tenantID, name)
		},
		ClientName: "keel-e2e",
		AdminKey:   testAdminKey,
		HTTPClient: &http.Client{},
		Reconnect: func(ctx context.Context, tenantID uuid.UUID, name string) error {
			if tenantID != tenant {
				return errors.New("wrong tenant")
			}
			return manager.Reconnect(ctx, name)
		},
	}
	apiServer := httptest.NewServer(New(Config{
		Store:    st,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		MCPOAuth: config,
	}))
	defer apiServer.Close()

	_, signIn := postJSON(t, apiServer.Client(), apiServer.URL+"/v1/mcp/servers/"+serverName+"/signin", tenant.String(), map[string]any{})
	authorizationURL, _ := signIn["authorizationUrl"].(string)
	if authorizationURL == "" {
		t.Fatalf("sign-in = %#v", signIn)
	}
	response, err := noFollowClient.Get(authorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	callbackResponse, err := apiServer.Client().Get(response.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	callbackBody, _ := io.ReadAll(callbackResponse.Body)
	callbackResponse.Body.Close()
	if callbackResponse.StatusCode != 200 {
		t.Fatalf("callback = %d %s", callbackResponse.StatusCode, callbackBody)
	}

	servers := manager.Servers()
	if len(servers) != 1 || servers[0].Err != nil {
		t.Fatalf("reconnected server = %#v", servers)
	}
	if len(servers[0].Tools) != 1 {
		t.Fatalf("tools = %#v", servers[0].Tools)
	}
	tool := servers[0].Tools[0]
	if !strings.HasPrefix(tool.Name(), "mcp__") || !strings.HasSuffix(tool.Name(), "__echo") {
		t.Errorf("tool name = %q", tool.Name())
	}
	result, err := tool.Execute(ctx, "call-1", json.RawMessage(`{"text":"hi"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result.Content)
	if !strings.Contains(string(encoded), "pong:hi") {
		t.Errorf("tool result = %s", encoded)
	}
	if atomic.LoadInt32(&toolCalls) != 1 {
		t.Errorf("tool calls = %d", toolCalls)
	}
}

// skipUnlessDSN returns the test DSN or skips.
func skipUnlessDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	return dsn
}
