// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/oauth/discovery.ts (MIT License); implemented independently
// in Go. Contract kept identical; see docs/UPSTREAM.md.
//
// RFC 9728 protected resource metadata, RFC 8414 / OpenID Connect authorization
// server metadata, WWW-Authenticate parsing, and the resource binding.

package oauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// discoveryProtocolVersion is sent as MCP-Protocol-Version with metadata
// requests. It mirrors mcp.LatestProtocolVersion (the oauth package stays
// independent of the transport package).
const discoveryProtocolVersion = "2025-11-25"

const metadataBodyLimit = 256 << 10

var wwwAuthenticateField = regexp.MustCompile(`(?i)(?:^|[,\s])([A-Za-z_][A-Za-z0-9_.-]*)=(?:"([^"]*)"|([^\s,]+))`)

// ParseWWWAuthenticate parses a Bearer (or DPoP) challenge. Empty values count
// as absent.
func ParseWWWAuthenticate(header string) Challenge {
	challenge := Challenge{}
	if strings.TrimSpace(header) == "" {
		return challenge
	}
	scheme := strings.ToLower(strings.Fields(strings.TrimLeft(header, " \t"))[0])
	if scheme != "bearer" && scheme != "dpop" {
		return challenge
	}
	fields := map[string]string{}
	for _, match := range wwwAuthenticateField.FindAllStringSubmatch(header, -1) {
		value := match[2]
		if value == "" {
			value = match[3]
		}
		if value != "" {
			fields[strings.ToLower(match[1])] = value
		}
	}
	challenge.ResourceMetadataURL = fields["resource_metadata"]
	challenge.Scope = fields["scope"]
	challenge.Error = fields["error"]
	challenge.ErrorDescription = fields["error_description"]
	return challenge
}

// isDiscoveryMiss means "not here", so the next candidate URL is tried.
func isDiscoveryMiss(status int) bool {
	return (status >= 400 && status < 500) || status == 502
}

// pathSuffix is the path part of `/.well-known/<kind><path>`; empty at root.
func pathSuffix(pathname string) string {
	return strings.TrimSuffix(pathname, "/")
}

func (options DiscoveryOptions) client() *http.Client {
	return clientOr(options.HTTPClient)
}

// DiscoveryOptions configures metadata discovery.
type DiscoveryOptions struct {
	// ResourceMetadataURL overrides the well-known URL.
	ResourceMetadataURL string
	// AuthorizationServerMetadataURL uses a configured document instead of
	// discovery; it is trusted as configured, so its issuer is not checked.
	AuthorizationServerMetadataURL string
	// ProtocolVersion overrides the MCP-Protocol-Version header.
	ProtocolVersion string
	// SkipIssuerValidation accepts a metadata issuer that differs from the
	// discovered authorization server URL.
	SkipIssuerValidation bool
	// HTTPClient is used for every request. Nil uses the SSRF-guarded client.
	HTTPClient *http.Client
}

func fetchMetadata(ctx context.Context, client *http.Client, target string, protocolVersion string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("MCP-Protocol-Version", protocolVersion)
	return client.Do(request)
}

func readJSONBody(response *http.Response) ([]byte, error) {
	defer response.Body.Close()
	return io.ReadAll(io.LimitReader(response.Body, metadataBodyLimit))
}

func versionOr(options DiscoveryOptions) string {
	if options.ProtocolVersion != "" {
		return options.ProtocolVersion
	}
	return discoveryProtocolVersion
}

// DiscoverProtectedResourceMetadata loads RFC 9728 metadata for the MCP
// server. Without an explicit URL it tries the path-suffixed well-known URL
// and falls back to the root document.
func DiscoverProtectedResourceMetadata(ctx context.Context, serverURL string, options DiscoveryOptions) (ProtectedResourceMetadata, error) {
	server, err := url.Parse(serverURL)
	if err != nil || server.Host == "" {
		return ProtectedResourceMetadata{}, fmt.Errorf("Invalid MCP server URL %s", serverURL)
	}
	client := options.client()
	version := versionOr(options)
	target := server.ResolveReference(&url.URL{Path: "/.well-known/oauth-protected-resource" + pathSuffix(server.Path)})
	if options.ResourceMetadataURL != "" {
		target, err = url.Parse(options.ResourceMetadataURL)
		if err != nil || target.Host == "" || !IsHTTPURL(target) {
			return ProtectedResourceMetadata{}, fmt.Errorf("Invalid OAuth protected resource metadata URL %s", options.ResourceMetadataURL)
		}
	}
	response, err := fetchMetadata(ctx, client, target.String(), version)
	if err != nil {
		return ProtectedResourceMetadata{}, err
	}
	if options.ResourceMetadataURL == "" && server.Path != "" && server.Path != "/" && isDiscoveryMiss(response.StatusCode) {
		drain(response)
		target = server.ResolveReference(&url.URL{Path: "/.well-known/oauth-protected-resource"})
		response, err = fetchMetadata(ctx, client, target.String(), version)
		if err != nil {
			return ProtectedResourceMetadata{}, err
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		status := response.StatusCode
		drain(response)
		return ProtectedResourceMetadata{}, fmt.Errorf("HTTP %d loading OAuth protected resource metadata", status)
	}
	body, err := readJSONBody(response)
	if err != nil {
		return ProtectedResourceMetadata{}, err
	}
	return ParseProtectedResourceMetadata(body)
}

// AuthorizationServerDiscoveryURL is one metadata candidate URL and its kind.
type AuthorizationServerDiscoveryURL struct {
	URL  string
	Type string // "oauth" or "oidc"
}

// BuildAuthorizationServerDiscoveryURLs returns the RFC 8414 and OIDC
// well-known URLs for an issuer, in the order they are tried.
func BuildAuthorizationServerDiscoveryURLs(authorizationServerURL string) ([]AuthorizationServerDiscoveryURL, error) {
	issuer, err := url.Parse(authorizationServerURL)
	if err != nil || issuer.Host == "" {
		return nil, fmt.Errorf("Invalid authorization server URL %s", authorizationServerURL)
	}
	path := pathSuffix(issuer.Path)
	urls := []AuthorizationServerDiscoveryURL{
		{URL: issuer.ResolveReference(&url.URL{Path: "/.well-known/oauth-authorization-server" + path}).String(), Type: "oauth"},
		{URL: issuer.ResolveReference(&url.URL{Path: "/.well-known/openid-configuration" + path}).String(), Type: "oidc"},
	}
	if path != "" {
		urls = append(urls, AuthorizationServerDiscoveryURL{
			URL:  issuer.ResolveReference(&url.URL{Path: path + "/.well-known/openid-configuration"}).String(),
			Type: "oidc",
		})
	}
	return urls, nil
}

// DiscoverAuthorizationServerMetadata loads authorization server metadata,
// returning nil when no candidate URL answers.
func DiscoverAuthorizationServerMetadata(ctx context.Context, authorizationServerURL string, options DiscoveryOptions) (*AuthorizationServerMetadata, error) {
	candidates, err := BuildAuthorizationServerDiscoveryURLs(authorizationServerURL)
	if err != nil {
		return nil, err
	}
	client := options.client()
	version := versionOr(options)
	for _, candidate := range candidates {
		response, err := fetchMetadata(ctx, client, candidate.URL, version)
		if err != nil {
			return nil, err
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			status := response.StatusCode
			drain(response)
			if isDiscoveryMiss(status) {
				continue
			}
			return nil, fmt.Errorf("HTTP %d loading authorization server metadata from %s", status, candidate.URL)
		}
		body, err := readJSONBody(response)
		if err != nil {
			return nil, err
		}
		metadata, err := ParseAuthorizationServerMetadata(body)
		if err != nil {
			return nil, err
		}
		if !options.SkipIssuerValidation {
			trim := func(value string) string { return strings.TrimSuffix(value, "/") }
			if trim(metadata.Issuer) != trim(authorizationServerURL) {
				expected, received := authorizationServerURL, metadata.Issuer
				return nil, &IssuerMismatchError{Expected: &expected, Received: &received}
			}
		}
		return &metadata, nil
	}
	return nil, nil
}

// DiscoverOAuthServerInfo discovers the resource metadata and its authorization
// server. A resource metadata failure is ignored unless the network failed;
// the server origin is then used as the authorization server.
func DiscoverOAuthServerInfo(ctx context.Context, serverURL string, options DiscoveryOptions) (ServerInfo, error) {
	resource, err := DiscoverProtectedResourceMetadata(ctx, serverURL, options)
	if err != nil && isNetworkError(err) {
		return ServerInfo{}, err
	}
	var resourceMetadata *ProtectedResourceMetadata
	if err == nil {
		resourceMetadata = &resource
	}
	if options.AuthorizationServerMetadataURL != "" {
		client := options.client()
		response, err := fetchMetadata(ctx, client, options.AuthorizationServerMetadataURL, versionOr(options))
		if err != nil {
			return ServerInfo{}, err
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			status := response.StatusCode
			drain(response)
			return ServerInfo{}, fmt.Errorf("HTTP %d loading authorization server metadata from %s", status, options.AuthorizationServerMetadataURL)
		}
		body, err := readJSONBody(response)
		if err != nil {
			return ServerInfo{}, err
		}
		metadata, err := ParseAuthorizationServerMetadata(body)
		if err != nil {
			return ServerInfo{}, err
		}
		return ServerInfo{
			AuthorizationServerURL:      metadata.Issuer,
			AuthorizationServerMetadata: &metadata,
			ResourceMetadata:            resourceMetadata,
		}, nil
	}
	authorizationServerURL := ""
	if resourceMetadata != nil && len(resourceMetadata.AuthorizationServers) > 0 {
		authorizationServerURL = resourceMetadata.AuthorizationServers[0]
	} else {
		server, err := url.Parse(serverURL)
		if err != nil {
			return ServerInfo{}, err
		}
		authorizationServerURL = server.ResolveReference(&url.URL{Path: "/"}).String()
	}
	metadata, err := DiscoverAuthorizationServerMetadata(ctx, authorizationServerURL, options)
	if err != nil {
		return ServerInfo{}, err
	}
	return ServerInfo{
		AuthorizationServerURL:      authorizationServerURL,
		AuthorizationServerMetadata: metadata,
		ResourceMetadata:            resourceMetadata,
	}, nil
}

func isNetworkError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

func drain(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
	_ = response.Body.Close()
}

// ResourceURLFromServerURL returns the MCP server URL used as `resource`, with
// the fragment removed.
func ResourceURLFromServerURL(serverURL string) (*url.URL, error) {
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return nil, err
	}
	parsed.Fragment = ""
	return parsed, nil
}

// SelectResource returns the resource metadata's `resource` when it covers the
// MCP server URL, and fails otherwise. It is the RFC 8707 resource indicator
// sent with authorization and token requests.
func SelectResource(serverURL string, metadata *ProtectedResourceMetadata) (string, error) {
	if metadata == nil {
		return "", nil
	}
	requested, err := ResourceURLFromServerURL(serverURL)
	if err != nil {
		return "", err
	}
	configured, err := url.Parse(metadata.Resource)
	if err != nil {
		return "", fmt.Errorf("Protected resource %s does not match MCP server %s", metadata.Resource, requested)
	}
	if requested.Scheme != configured.Scheme || requested.Host != configured.Host {
		return "", fmt.Errorf("Protected resource %s does not match MCP server %s", metadata.Resource, requested)
	}
	requestedPath := requested.Path
	if !strings.HasSuffix(requestedPath, "/") {
		requestedPath += "/"
	}
	configuredPath := configured.Path
	if !strings.HasSuffix(configuredPath, "/") {
		configuredPath += "/"
	}
	if !strings.HasPrefix(requestedPath, configuredPath) {
		return "", fmt.Errorf("Protected resource %s does not match MCP server %s", metadata.Resource, requested)
	}
	return metadata.Resource, nil
}
