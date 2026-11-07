// Manager wiring for MCP OAuth: an HTTP entry with an oauth block gets the
// stored-token provider, and a missing interactive grant surfaces as the typed
// AuthorizationRequiredError instead of a generic 401.

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/mcp/oauth"
)

func TestManagerOAuthAuthorizationRequired(t *testing.T) {
	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("www-authenticate", `Bearer error="invalid_token"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Unauthorized"))
	}))
	defer unauthorized.Close()

	serverURL := unauthorized.URL + "/mcp"
	memory := oauth.NewMemoryServerStore()
	provider := oauth.NewProvider(oauth.ProviderOptions{
		ServerURL:      serverURL,
		RedirectURL:    "http://127.0.0.1/callback",
		ClientMetadata: oauth.ClientMetadata{ClientName: "keel-test"},
		Store:          memory,
	})
	if err := provider.SaveTokens(context.Background(), oauth.Tokens{
		AccessToken: "stored-token", TokenType: "Bearer",
	}); err != nil {
		t.Fatal(err)
	}

	tenant := uuid.New()
	var challengeServer string
	var challenge oauth.Challenge
	manager := Connect(context.Background(), []ServerEntry{{
		Name: "remote",
		Config: map[string]any{
			"url":   serverURL,
			"oauth": map[string]any{"scope": "read"},
		},
	}}, ConnectOptions{
		TenantID: tenant,
		OAuth: &OAuthOptions{
			Storage: func(id uuid.UUID, _ string) oauth.ServerStore {
				if id != tenant {
					t.Errorf("storage tenant = %s", id)
				}
				return memory
			},
			// The test server is loopback; production uses the guarded client.
			HTTPClient: &http.Client{},
			OnChallenge: func(server string, got oauth.Challenge) {
				challengeServer = server
				challenge = got
			},
		},
	})
	defer manager.Close()
	servers := manager.Servers()
	if len(servers) != 1 {
		t.Fatalf("servers = %#v", servers)
	}
	// The manager surfaces the challenge so a host can log it / drive sign-in.
	if challengeServer != "remote" || challenge.Error != "invalid_token" {
		t.Errorf("OnChallenge = server %q challenge %+v, want remote/invalid_token", challengeServer, challenge)
	}
	var required *oauth.AuthorizationRequiredError
	if !errors.As(servers[0].Err, &required) {
		t.Fatalf("connect error = %v, want AuthorizationRequiredError", servers[0].Err)
	}
	if !oauth.IsAuthorizationRequired(servers[0].Err) {
		t.Error("IsAuthorizationRequired = false")
	}
}

func TestManagerWithoutOAuthKeepsPlain401(t *testing.T) {
	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Unauthorized"))
	}))
	defer unauthorized.Close()
	manager := Connect(context.Background(), []ServerEntry{{
		Name:   "plain",
		Config: map[string]any{"url": unauthorized.URL + "/mcp"},
	}}, ConnectOptions{})
	defer manager.Close()
	var required *AuthRequiredError
	if !errors.As(manager.Servers()[0].Err, &required) {
		t.Fatalf("connect error = %v, want AuthRequiredError", manager.Servers()[0].Err)
	}
}

func TestManagerReconnectAfterGrantStored(t *testing.T) {
	var authorized atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authorized.Load() {
			var message map[string]any
			_ = json.NewDecoder(r.Body).Decode(&message)
			if _, ok := message["id"]; !ok {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			var result any
			if message["method"] == "initialize" {
				result = map[string]any{
					"protocolVersion": "2025-06-18",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "reconnect", "version": "1.0.0"},
				}
			} else {
				result = map[string]any{"tools": []any{map[string]any{
					"name": "ping", "inputSchema": map[string]any{"type": "object"},
				}}}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": message["id"], "result": result})
			return
		}
		w.Header().Set("www-authenticate", "Bearer")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	tenant := uuid.New()
	memory := oauth.NewMemoryServerStore()
	manager := Connect(context.Background(), []ServerEntry{{
		Name:   "remote",
		Config: map[string]any{"url": server.URL + "/mcp", "oauth": map[string]any{}},
	}}, ConnectOptions{
		TenantID: tenant,
		OAuth: &OAuthOptions{
			Storage:    func(uuid.UUID, string) oauth.ServerStore { return memory },
			HTTPClient: &http.Client{},
		},
	})
	defer manager.Close()
	var required *oauth.AuthorizationRequiredError
	if !errors.As(manager.Servers()[0].Err, &required) {
		t.Fatalf("boot error = %v", manager.Servers()[0].Err)
	}

	provider := oauth.NewProvider(oauth.ProviderOptions{ServerURL: server.URL + "/mcp", Store: memory})
	if err := provider.SaveTokens(context.Background(), oauth.Tokens{AccessToken: "good", TokenType: "Bearer"}); err != nil {
		t.Fatal(err)
	}
	authorized.Store(true)
	if err := manager.Reconnect(context.Background(), "remote"); err != nil {
		t.Fatal(err)
	}
	servers := manager.Servers()
	if len(servers) != 1 || servers[0].Err != nil || len(servers[0].Tools) != 1 {
		t.Fatalf("reconnected = %#v", servers)
	}
	if err := manager.Reconnect(context.Background(), "missing"); err == nil {
		t.Error("reconnecting an unknown server succeeded")
	}
}
