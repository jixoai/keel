// CIMD configuration loading and host wiring: explicit "cimd" must be loadable
// when the deployment publishes Client ID Metadata Documents, and "auto" must
// be able to choose them without a host client name.

package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/mcp/oauth"
)

func TestCIMDConfigLoading(t *testing.T) {
	path := t.TempDir() + "/mcp.json"
	writeConfigFile(t, path, `{"mcpServers":{"remote":{
		"url":"https://mcp.example.com/mcp",
		"oauth":{"clientRegistration":"cimd","callbackUrl":"http://127.0.0.1:8080/oauth/callback"}
	}}}`)

	// Without documents the loader refuses cimd (fail-closed).
	loaded := LoadConfigFiles(path, "")
	if len(loaded.Servers) != 0 || len(loaded.Errors) != 1 || !strings.Contains(loaded.Errors[0], "no Client ID Metadata Documents") {
		t.Fatalf("servers = %#v errors = %#v", loaded.Servers, loaded.Errors)
	}

	loaded = LoadConfigFilesWithOptions(path, "", McpServerConfigOptions{
		CheckMetadataDocumentCallback: func(string) string { return "" },
	})
	if len(loaded.Errors) != 0 || len(loaded.Servers) != 1 {
		t.Fatalf("servers = %#v errors = %#v", loaded.Servers, loaded.Errors)
	}

	loaded = LoadConfigFilesWithOptions(path, "", McpServerConfigOptions{
		CheckMetadataDocumentCallback: func(string) string { return "redirect URI not published" },
	})
	if len(loaded.Servers) != 0 || len(loaded.Errors) != 1 || !strings.Contains(loaded.Errors[0], "not published") {
		t.Fatalf("servers = %#v errors = %#v", loaded.Servers, loaded.Errors)
	}
}

func TestOAuthSettingsFromConfigKeepsHostNameSeparate(t *testing.T) {
	settings := OAuthSettingsFromConfig(map[string]any{
		"oauth": map[string]any{"scope": "read", "callbackUrl": "http://127.0.0.1:8080/oauth/callback"},
	}, "https://app.example/oauth", []string{"https://app.example/mcp/callback"})
	if settings.ClientName != "" {
		t.Errorf("settings.ClientName = %q, want empty (host default is separate)", settings.ClientName)
	}
	if settings.Scope != "read" || settings.MetadataBaseURL != "https://app.example/oauth" ||
		len(settings.MetadataRedirectURLs) != 1 {
		t.Errorf("settings = %#v", settings)
	}
	custom := OAuthSettingsFromConfig(map[string]any{
		"oauth": map[string]any{"clientName": "Custom"},
	}, "", nil)
	if custom.ClientName != "Custom" {
		t.Errorf("configured clientName = %q", custom.ClientName)
	}
}

func TestDocumentsFromSettingsAndAutoCIMD(t *testing.T) {
	if documents := oauth.DocumentsFromSettings(oauth.Settings{}); documents != nil {
		t.Fatalf("documents without a base URL = %#v", documents)
	}
	documents := oauth.DocumentsFromSettings(oauth.Settings{MetadataBaseURL: "https://app.example/oauth"})
	if documents == nil || documents.BaseURL != "https://app.example/oauth" {
		t.Fatalf("documents = %#v", documents)
	}
	if !documents.ListsRedirectURL("http://127.0.0.1:1234/callback") {
		t.Error("loopback redirect not listed")
	}
	withRoute := oauth.DocumentsFromSettings(oauth.Settings{
		MetadataBaseURL:      "https://app.example/oauth",
		MetadataRedirectURLs: []string{"https://app.example/mcp/callback"},
	})
	if !withRoute.ListsRedirectURL("https://app.example/mcp/callback") {
		t.Error("configured https redirect not listed")
	}
	if withRoute.ListsRedirectURL("https://other.example/callback") {
		t.Error("unlisted https redirect accepted")
	}

	// "auto" may choose CIMD when the entry sets no client name; the host's
	// default client name must not force DCR.
	settings := oauth.Settings{}
	if !oauth.OffersClientMetadataDocument(settings, documents, "http://127.0.0.1:1234/callback") {
		t.Error("auto did not offer CIMD without a configured client name")
	}
	settings.ClientName = "custom"
	if oauth.OffersClientMetadataDocument(settings, documents, "http://127.0.0.1:1234/callback") {
		t.Error("auto offered CIMD despite a configured client name")
	}
}

func TestManagerOAuthRejectsImplicitTenantAndInsecureURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	entry := ServerEntry{Name: "remote", Config: map[string]any{
		"url":   server.URL + "/mcp",
		"oauth": map[string]any{},
	}}
	oauthOptions := &OAuthOptions{
		Storage:    func(uuid.UUID, string) oauth.ServerStore { return oauth.NewMemoryServerStore() },
		HTTPClient: &http.Client{},
	}
	manager := Connect(context.Background(), []ServerEntry{entry}, ConnectOptions{OAuth: oauthOptions})
	defer manager.Close()
	if err := manager.Servers()[0].Err; err == nil || !strings.Contains(err.Error(), "explicit tenant") {
		t.Fatalf("nil tenant error = %v", err)
	}

	// http:// to a non-loopback host must not carry OAuth credentials.
	manager = Connect(context.Background(), []ServerEntry{{
		Name:   "insecure",
		Config: map[string]any{"url": "http://mcp.example.com/mcp", "oauth": map[string]any{}},
	}}, ConnectOptions{TenantID: uuid.New(), OAuth: oauthOptions})
	defer manager.Close()
	if err := manager.Servers()[0].Err; err == nil || !strings.Contains(err.Error(), "requires an https URL") {
		t.Fatalf("insecure URL error = %v", err)
	}
}
