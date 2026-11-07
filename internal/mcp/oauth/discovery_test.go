// Discovery, challenge parsing, and the SSRF guard (Keel extensions where PI
// relies on the platform fetch).

package oauth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpoauth "github.com/jixoai/keel/internal/mcp/oauth"
)

func TestParseWWWAuthenticate(t *testing.T) {
	challenge := mcpoauth.ParseWWWAuthenticate(`Bearer resource_metadata="https://auth.example/meta", scope="read write", error="insufficient_scope", error_description="needs more"`)
	if challenge.ResourceMetadataURL != "https://auth.example/meta" {
		t.Errorf("resource metadata = %q", challenge.ResourceMetadataURL)
	}
	if challenge.Scope != "read write" || challenge.Error != "insufficient_scope" || challenge.ErrorDescription != "needs more" {
		t.Errorf("challenge = %#v", challenge)
	}
	// An empty value carries no information.
	if challenge := mcpoauth.ParseWWWAuthenticate(`Bearer scope=""`); challenge.Scope != "" {
		t.Errorf("empty scope = %q", challenge.Scope)
	}
	// DPoP challenges are accepted; other schemes are ignored.
	if challenge := mcpoauth.ParseWWWAuthenticate(`DPoP error="invalid_token"`); challenge.Error != "invalid_token" {
		t.Errorf("dpop challenge = %#v", challenge)
	}
	if challenge := mcpoauth.ParseWWWAuthenticate(`Basic realm="x"`); challenge.Error != "" || challenge.Scope != "" {
		t.Errorf("basic challenge = %#v", challenge)
	}
	if challenge := mcpoauth.ParseWWWAuthenticate(""); challenge != (mcpoauth.Challenge{}) {
		t.Errorf("empty header = %#v", challenge)
	}
}

func TestHTTPClientBlocksLoopbackAndPrivateAddresses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer server.Close()

	guarded := mcpoauth.NewHTTPClient(mcpoauth.HTTPClientOptions{})
	_, err := guarded.Get(server.URL)
	if err == nil || !errors.Is(err, mcpoauth.ErrBlockedAddress) {
		t.Fatalf("guarded loopback error = %v", err)
	}
	for _, address := range []string{
		"http://10.0.0.1/", "http://192.168.1.1/", "http://169.254.1.1/", "http://100.64.0.1/",
		"http://0.0.0.1/", "http://240.0.0.1/", "http://255.255.255.255/", "http://198.18.0.1/",
		"http://[64:ff9b::a00:1]/",
	} {
		if _, err := guarded.Get(address); err == nil || !errors.Is(err, mcpoauth.ErrBlockedAddress) {
			t.Errorf("%s error = %v", address, err)
		}
	}

	local := mcpoauth.NewHTTPClient(mcpoauth.HTTPClientOptions{AllowLoopback: true})
	response, err := local.Get(server.URL)
	if err != nil {
		t.Fatalf("local helper error = %v", err)
	}
	response.Body.Close()
}

func TestDiscoveryFallsBackToRootWellKnown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp":
			w.WriteHeader(404)
		case "/.well-known/oauth-protected-resource":
			writeJSON(w, 200, map[string]any{
				"resource":              originOf(r) + "/mcp",
				"authorization_servers": []string{originOf(r)},
				"scopes_supported":      []string{"a", "b"},
			})
		case "/.well-known/oauth-authorization-server":
			writeJSON(w, 200, map[string]any{
				"issuer":                   originOf(r) + "/",
				"authorization_endpoint":   originOf(r) + "/authorize",
				"token_endpoint":           originOf(r) + "/token",
				"response_types_supported": []string{"code"},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	metadata, err := mcpoauth.DiscoverProtectedResourceMetadata(context.Background(), server.URL+"/mcp", mcpoauth.DiscoveryOptions{HTTPClient: testHTTPClient})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Resource != server.URL+"/mcp" || len(metadata.ScopesSupported) != 2 {
		t.Errorf("metadata = %#v", metadata)
	}
	// The trailing slash URL parsing adds to a bare issuer is not a mismatch.
	serverMetadata, err := mcpoauth.DiscoverAuthorizationServerMetadata(context.Background(), server.URL, mcpoauth.DiscoveryOptions{HTTPClient: testHTTPClient})
	if err != nil || serverMetadata == nil {
		t.Fatalf("metadata = %#v, %v", serverMetadata, err)
	}
	resource, err := mcpoauth.SelectResource(server.URL+"/mcp", &metadata)
	if err != nil || resource != metadata.Resource {
		t.Errorf("select = %q, %v", resource, err)
	}
	if _, err := mcpoauth.SelectResource("https://other.example/mcp", &metadata); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("mismatch error = %v", err)
	}
}
