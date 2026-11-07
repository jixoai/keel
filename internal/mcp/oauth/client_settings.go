// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/oauth/client-settings.ts (MIT License); implemented
// independently in Go. Contract kept identical; see docs/UPSTREAM.md.
//
// The OAuth client of one MCP server: its settings, how it identifies itself
// (a pre-registered client, a Client ID Metadata Document, or dynamic client
// registration), and the redirect URI of the loopback helper.

package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// DefaultRedirectURL is the redirect URI when settings name none and no client
// is stored.
const DefaultRedirectURL = "http://127.0.0.1/callback"

// Settings are the OAuth client settings of one MCP server, normally from the
// server's configuration.
type Settings struct {
	// ClientID is a pre-registered client id. Without it, sign-in uses a Client
	// ID Metadata Document or registers a client.
	ClientID string
	// ClientSecret is already resolved.
	ClientSecret string
	// CallbackPort is the port of the loopback callback server for clients
	// registered with a fixed redirect URI. Without CallbackURL the redirect URI
	// is http://127.0.0.1:<port>/callback.
	CallbackPort *int
	// CallbackURL is the loopback redirect URI registered for ClientID, for
	// example http://localhost:8080/oauth/callback. Without a port the callback
	// server listens on CallbackPort or a free port.
	CallbackURL string
	// Scope requests scopes separated by spaces. Default: the scopes the server
	// advertises.
	Scope string
	// ClientName is client_name for dynamic client registration.
	ClientName string
	// ClientRegistration is "auto" (default), "dcr", or "cimd".
	ClientRegistration string
	// AuthServerMetadataURL is an authorization server metadata document used
	// instead of discovery, for servers that advertise a wrong authorization
	// server or none. It is trusted as configured.
	AuthServerMetadataURL string
	// MetadataBaseURL is the public base URL under which the application
	// publishes Client ID Metadata Documents: <base>/client.json, and
	// <base>/<callback ID>/client.json for server-specific documents. Empty
	// disables CIMD, so "auto" and "cimd" fail or fall back to DCR.
	MetadataBaseURL string
	// MetadataRedirectURLs lists the redirect URIs the published documents
	// contain when they cannot be derived (for example a fixed https callback
	// route). Empty means loopback http redirect URIs and the configured
	// CallbackURL.
	MetadataRedirectURLs []string
}

// ClientMetadataDocuments are the published Client ID Metadata Documents.
type ClientMetadataDocuments struct {
	// BaseURL is the public base URL, for example https://pi.dev/oauth.
	BaseURL string
	// ListsRedirectURL reports whether the documents list redirectURL.
	// Loopback redirect URIs match on any port (RFC 8252).
	ListsRedirectURL func(redirectURL string) bool
}

// DocumentsFromSettings derives the CIMD documents from settings. It returns
// nil when no public metadata base URL is configured, which makes the flow use
// dynamic client registration.
func DocumentsFromSettings(settings Settings) *ClientMetadataDocuments {
	base := strings.TrimSuffix(strings.TrimSpace(settings.MetadataBaseURL), "/")
	if base == "" {
		return nil
	}
	listed := make(map[string]bool, len(settings.MetadataRedirectURLs)+1)
	for _, value := range settings.MetadataRedirectURLs {
		listed[value] = true
	}
	if settings.CallbackURL != "" {
		listed[settings.CallbackURL] = true
	}
	return &ClientMetadataDocuments{
		BaseURL: base,
		ListsRedirectURL: func(redirectURL string) bool {
			if listed[redirectURL] {
				return true
			}
			parsed, err := url.Parse(redirectURL)
			if err != nil || parsed.Scheme != "http" {
				return false
			}
			return isLoopbackHost(parsed.Hostname())
		},
	}
}

// LoopbackSettings is where the loopback callback server listens and the
// redirect URI it serves.
type LoopbackSettings struct {
	// URL is the redirect URI, without the port unless one is configured.
	URL string
	// Host is the address to listen on.
	Host string
	// RedirectHost is the host name in the redirect URI.
	RedirectHost string
	Port         *int
	Path         string
	// FixedRedirectURL is the exact redirect URI when the port is fixed.
	FixedRedirectURL string
}

// LoopbackSettingsOf derives the loopback callback settings from Settings.
func LoopbackSettingsOf(settings Settings) LoopbackSettings {
	configured := settings.CallbackURL
	if configured == "" {
		configured = DefaultRedirectURL
	}
	parsed, err := url.Parse(configured)
	if err != nil {
		parsed = &url.URL{Scheme: "http", Host: "127.0.0.1", Path: "/callback"}
	}
	address := strings.Trim(parsed.Hostname(), "[]")
	var port *int
	if parsed.Port() != "" {
		value := 0
		fmt.Sscanf(parsed.Port(), "%d", &value)
		port = &value
	} else if settings.CallbackPort != nil {
		value := *settings.CallbackPort
		port = &value
	}
	fixedRedirectURL := ""
	if parsed.Port() != "" {
		fixedRedirectURL = settings.CallbackURL
	} else if port != nil {
		copy := *parsed
		copy.Host = netJoinHostPort(address, *port)
		fixedRedirectURL = copy.String()
	}
	host := address
	if address == "localhost" {
		host = "127.0.0.1"
	}
	return LoopbackSettings{
		URL:              configured,
		Host:             host,
		RedirectHost:     address,
		Port:             port,
		Path:             parsed.Path,
		FixedRedirectURL: fixedRedirectURL,
	}
}

func netJoinHostPort(host string, port int) string {
	if strings.Contains(host, ":") {
		return fmt.Sprintf("[%s]:%d", host, port)
	}
	return fmt.Sprintf("%s:%d", host, port)
}

func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// OffersClientMetadataDocument reports whether the client may identify with a
// Client ID Metadata Document instead of registering. "auto" registers when the
// user asked for a client name or the documents do not list the redirect URI.
func OffersClientMetadataDocument(settings Settings, documents *ClientMetadataDocuments, redirectURL string) bool {
	registration := settings.ClientRegistration
	if registration == "" {
		registration = "auto"
	}
	if registration != "auto" {
		return registration == "cimd"
	}
	return settings.ClientID == "" && settings.ClientName == "" &&
		documents != nil && documents.ListsRedirectURL != nil && documents.ListsRedirectURL(redirectURL)
}

// ServerSpecificPath appends the callback ID to a redirect path.
func ServerSpecificPath(path, serverURL string) string {
	id := CallbackID(serverURL)
	return strings.TrimSuffix(path, "/") + "/" + id
}

// CallbackID is the 12-character identifier of an MCP server URL in callback
// paths, computed like PI (and Codex) do. The URL is normalized like
// JavaScript's URL.href: an empty path becomes "/".
func CallbackID(serverURL string) string {
	parsed, err := url.Parse(serverURL)
	normalized := serverURL
	if err == nil {
		parsed.Fragment = ""
		if parsed.Path == "" {
			parsed.Path = "/"
		}
		normalized = parsed.String()
	}
	digest := sha256.Sum256([]byte(normalized))
	return base64.RawURLEncoding.EncodeToString(digest[:9])
}

// RegisteredRedirectURLs returns the redirect URIs a stored client was
// registered with.
func RegisteredRedirectURLs(client *ClientInformation) []string {
	if client == nil {
		return nil
	}
	return client.RedirectURIs
}

// ApplicationType derives the OpenID Connect application_type from redirect
// URIs (MCP SEP-837): loopback hosts and custom schemes are native.
func ApplicationType(redirectURIs []string) string {
	for _, value := range redirectURIs {
		parsed, err := url.Parse(value)
		if err != nil {
			continue
		}
		if (parsed.Scheme != "http" && parsed.Scheme != "https") || isLoopbackHost(parsed.Hostname()) {
			return "native"
		}
	}
	return "web"
}

// clientMetadataDocument chooses the document to identify as, or nil when the
// authorization server does not support documents for public clients.
func clientMetadataDocument(serverURL, redirectURL string, metadata *AuthorizationServerMetadata, baseURL string) *ClientMetadataDocument {
	if metadata == nil || !metadata.ClientIDMetadataDocumentSupported || !contains(metadata.TokenEndpointAuthMethodsSupported, "none") {
		return nil
	}
	if metadata.AuthorizationResponseIssParameter {
		return &ClientMetadataDocument{URL: baseURL + "/client.json", RedirectURL: redirectURL}
	}
	parsed, err := url.Parse(redirectURL)
	if err != nil {
		return nil
	}
	parsed.Path = ServerSpecificPath(parsed.Path, serverURL)
	return &ClientMetadataDocument{URL: baseURL + "/" + CallbackID(serverURL) + "/client.json", RedirectURL: parsed.String()}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// ClientMetadataDocument is an https URL used as client_id, and a redirect URI
// it lists.
type ClientMetadataDocument struct {
	URL         string
	RedirectURL string
}

// marshalClientMetadata encodes the metadata for a registration request,
// adding application_type and scope like PI.
func marshalClientMetadata(metadata ClientMetadata, scope string) ([]byte, error) {
	body := metadata
	if body.ApplicationType == "" {
		body.ApplicationType = ApplicationType(body.RedirectURIs)
	}
	if scope != "" {
		body.Scope = scope
	}
	return json.Marshal(body)
}
