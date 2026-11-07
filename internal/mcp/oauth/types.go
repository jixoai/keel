// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/oauth/types.ts (MIT License); implemented independently in
// Go. Contract kept identical; see docs/UPSTREAM.md.
//
// Structural validation of the OAuth documents discovery, dynamic client
// registration, and token requests exchange. Like PI there is no schema
// library: every document is checked field by field, and optional fields that
// arrive as null or "" are absent.

package oauth

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// ProtectedResourceMetadata is RFC 9728 protected resource metadata.
type ProtectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers,omitempty"`
	ScopesSupported      []string `json:"scopes_supported,omitempty"`
}

// AuthorizationServerMetadata is RFC 8414 / OpenID Connect authorization
// server metadata.
type AuthorizationServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint,omitempty"`
	ScopesSupported                   []string `json:"scopes_supported,omitempty"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported,omitempty"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported,omitempty"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported,omitempty"`
	ClientIDMetadataDocumentSupported bool     `json:"client_id_metadata_document_supported,omitempty"`
	AuthorizationResponseIssParameter bool     `json:"authorization_response_iss_parameter_supported,omitempty"`
}

// Tokens is an OAuth token response.
type Tokens struct {
	AccessToken  string   `json:"access_token"`
	TokenType    string   `json:"token_type"`
	ExpiresIn    *float64 `json:"expires_in,omitempty"`
	Scope        string   `json:"scope,omitempty"`
	RefreshToken string   `json:"refresh_token,omitempty"`
	IDToken      string   `json:"id_token,omitempty"`
}

// ClientMetadata is the RFC 7591 dynamic client registration request body.
type ClientMetadata struct {
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
	ApplicationType         string   `json:"application_type,omitempty"`
	ClientName              string   `json:"client_name,omitempty"`
	ClientURI               string   `json:"client_uri,omitempty"`
	LogoURI                 string   `json:"logo_uri,omitempty"`
	Scope                   string   `json:"scope,omitempty"`
	Contacts                []string `json:"contacts,omitempty"`
	TOSURI                  string   `json:"tos_uri,omitempty"`
	PolicyURI               string   `json:"policy_uri,omitempty"`
	JWKSURI                 string   `json:"jwks_uri,omitempty"`
	JWKS                    any      `json:"jwks,omitempty"`
	SoftwareID              string   `json:"software_id,omitempty"`
	SoftwareVersion         string   `json:"software_version,omitempty"`
	SoftwareStatement       string   `json:"software_statement,omitempty"`
}

// ClientInformation is a registered client, or a pre-registered one from
// configuration.
type ClientInformation struct {
	ClientID                string   `json:"client_id"`
	ClientSecret            string   `json:"client_secret,omitempty"`
	ClientIDIssuedAt        *int64   `json:"client_id_issued_at,omitempty"`
	ClientSecretExpiresAt   *int64   `json:"client_secret_expires_at,omitempty"`
	RedirectURIs            []string `json:"redirect_uris,omitempty"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	ApplicationType         string   `json:"application_type,omitempty"`
	ClientName              string   `json:"client_name,omitempty"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
	Scope                   string   `json:"scope,omitempty"`
}

// DiscoveryState is the cached result of one discovery, persisted next to the
// credentials so later refreshes skip it.
type DiscoveryState struct {
	AuthorizationServerURL      string                       `json:"authorizationServerUrl"`
	AuthorizationServerMetadata *AuthorizationServerMetadata `json:"authorizationServerMetadata,omitempty"`
	ResourceMetadata            *ProtectedResourceMetadata   `json:"resourceMetadata,omitempty"`
	ResourceMetadataURL         string                       `json:"resourceMetadataUrl,omitempty"`
}

// ServerInfo is what discovery found for one MCP server.
type ServerInfo struct {
	AuthorizationServerURL      string
	AuthorizationServerMetadata *AuthorizationServerMetadata
	ResourceMetadata            *ProtectedResourceMetadata
}

// Challenge is a parsed WWW-Authenticate bearer challenge.
type Challenge struct {
	ResourceMetadataURL string
	Scope               string
	Error               string
	ErrorDescription    string
}

// IsHTTPURL reports whether the URL uses http or https.
func IsHTTPURL(value *url.URL) bool {
	return value.Scheme == "http" || value.Scheme == "https"
}

func objectOf(raw []byte, name string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("Invalid %s", name)
	}
	return object, nil
}

func requiredString(object map[string]json.RawMessage, key, name string) (string, error) {
	raw, ok := object[key]
	if !ok {
		return "", fmt.Errorf("Invalid %s", name)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", fmt.Errorf("Invalid %s", name)
	}
	return value, nil
}

func isAbsent(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	text := string(raw)
	return text == "null" || text == `""`
}

func optionalString(object map[string]json.RawMessage, key, name string) (string, error) {
	raw, ok := object[key]
	if !ok || isAbsent(raw) {
		return "", nil
	}
	return requiredString(object, key, name)
}

func optionalStrings(object map[string]json.RawMessage, key, name string) ([]string, error) {
	raw, ok := object[key]
	if !ok || string(raw) == "null" {
		return nil, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("Invalid %s", name)
	}
	return values, nil
}

// safeURL accepts only http and https. The authorization endpoint is opened
// with the platform URL handler, so a file: or custom scheme would launch a
// local program.
func safeURL(object map[string]json.RawMessage, key, name string) (string, error) {
	text, err := requiredString(object, key, name)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(text)
	if err != nil || !IsHTTPURL(parsed) {
		return "", fmt.Errorf("Invalid %s", name)
	}
	return text, nil
}

func optionalURL(object map[string]json.RawMessage, key, name string) (string, error) {
	raw, ok := object[key]
	if !ok || isAbsent(raw) {
		return "", nil
	}
	return safeURL(object, key, name)
}

func optionalBool(object map[string]json.RawMessage, key string) bool {
	raw, ok := object[key]
	if !ok {
		return false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	return value
}

// number reads a JSON number or a numeric string like JavaScript's Number(),
// and reports whether the value was present.
func number(object map[string]json.RawMessage, key, name string) (*float64, error) {
	raw, ok := object[key]
	if !ok || isAbsent(raw) {
		return nil, nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		value, err := number.Float64()
		if err != nil {
			return nil, fmt.Errorf("Invalid %s", name)
		}
		return &value, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		value, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, fmt.Errorf("Invalid %s", name)
		}
		return &value, nil
	}
	return nil, fmt.Errorf("Invalid %s", name)
}

func optionalInt64(object map[string]json.RawMessage, key string) *int64 {
	raw, ok := object[key]
	if !ok {
		return nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return &value
}

// ParseProtectedResourceMetadata validates an RFC 9728 document.
func ParseProtectedResourceMetadata(raw []byte) (ProtectedResourceMetadata, error) {
	object, err := objectOf(raw, "OAuth protected resource metadata")
	if err != nil {
		return ProtectedResourceMetadata{}, err
	}
	resource, err := safeURL(object, "resource", "OAuth protected resource metadata resource")
	if err != nil {
		return ProtectedResourceMetadata{}, err
	}
	servers, err := optionalStrings(object, "authorization_servers", "authorization_servers")
	if err != nil {
		return ProtectedResourceMetadata{}, err
	}
	for _, server := range servers {
		parsed, err := url.Parse(server)
		if err != nil || !IsHTTPURL(parsed) {
			return ProtectedResourceMetadata{}, fmt.Errorf("Invalid authorization server URL")
		}
	}
	scopes, err := optionalStrings(object, "scopes_supported", "scopes_supported")
	if err != nil {
		return ProtectedResourceMetadata{}, err
	}
	return ProtectedResourceMetadata{Resource: resource, AuthorizationServers: servers, ScopesSupported: scopes}, nil
}

// ParseAuthorizationServerMetadata validates RFC 8414 / OIDC metadata.
func ParseAuthorizationServerMetadata(raw []byte) (AuthorizationServerMetadata, error) {
	object, err := objectOf(raw, "authorization server metadata")
	if err != nil {
		return AuthorizationServerMetadata{}, err
	}
	responseTypes, err := optionalStrings(object, "response_types_supported", "response_types_supported")
	if err != nil {
		return AuthorizationServerMetadata{}, err
	}
	if responseTypes == nil {
		return AuthorizationServerMetadata{}, fmt.Errorf("Invalid response_types_supported")
	}
	issuer, err := safeURL(object, "issuer", "authorization server issuer")
	if err != nil {
		return AuthorizationServerMetadata{}, err
	}
	authorizationEndpoint, err := safeURL(object, "authorization_endpoint", "authorization endpoint")
	if err != nil {
		return AuthorizationServerMetadata{}, err
	}
	tokenEndpoint, err := safeURL(object, "token_endpoint", "token endpoint")
	if err != nil {
		return AuthorizationServerMetadata{}, err
	}
	registrationEndpoint, err := optionalURL(object, "registration_endpoint", "registration endpoint")
	if err != nil {
		return AuthorizationServerMetadata{}, err
	}
	scopes, err := optionalStrings(object, "scopes_supported", "scopes_supported")
	if err != nil {
		return AuthorizationServerMetadata{}, err
	}
	grantTypes, err := optionalStrings(object, "grant_types_supported", "grant_types_supported")
	if err != nil {
		return AuthorizationServerMetadata{}, err
	}
	authMethods, err := optionalStrings(object, "token_endpoint_auth_methods_supported", "token_endpoint_auth_methods_supported")
	if err != nil {
		return AuthorizationServerMetadata{}, err
	}
	codeMethods, err := optionalStrings(object, "code_challenge_methods_supported", "code_challenge_methods_supported")
	if err != nil {
		return AuthorizationServerMetadata{}, err
	}
	return AuthorizationServerMetadata{
		Issuer:                            issuer,
		AuthorizationEndpoint:             authorizationEndpoint,
		TokenEndpoint:                     tokenEndpoint,
		RegistrationEndpoint:              registrationEndpoint,
		ScopesSupported:                   scopes,
		ResponseTypesSupported:            responseTypes,
		GrantTypesSupported:               grantTypes,
		TokenEndpointAuthMethodsSupported: authMethods,
		CodeChallengeMethodsSupported:     codeMethods,
		ClientIDMetadataDocumentSupported: optionalBool(object, "client_id_metadata_document_supported"),
		AuthorizationResponseIssParameter: optionalBool(object, "authorization_response_iss_parameter_supported"),
	}, nil
}

// ParseTokens validates a token response.
func ParseTokens(raw []byte) (Tokens, error) {
	object, err := objectOf(raw, "OAuth token response")
	if err != nil {
		return Tokens{}, err
	}
	accessToken, err := requiredString(object, "access_token", "access_token")
	if err != nil {
		return Tokens{}, err
	}
	tokenType, err := requiredString(object, "token_type", "token_type")
	if err != nil {
		return Tokens{}, err
	}
	expires, err := number(object, "expires_in", "expires_in")
	if err != nil {
		return Tokens{}, err
	}
	scope, err := optionalString(object, "scope", "scope")
	if err != nil {
		return Tokens{}, err
	}
	refreshToken, err := optionalString(object, "refresh_token", "refresh_token")
	if err != nil {
		return Tokens{}, err
	}
	idToken, err := optionalString(object, "id_token", "id_token")
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{
		AccessToken:  accessToken,
		TokenType:    tokenType,
		ExpiresIn:    expires,
		Scope:        scope,
		RefreshToken: refreshToken,
		IDToken:      idToken,
	}, nil
}

// ParseClientInformation validates a dynamic client registration response.
func ParseClientInformation(raw []byte) (ClientInformation, error) {
	object, err := objectOf(raw, "OAuth client registration response")
	if err != nil {
		return ClientInformation{}, err
	}
	clientID, err := requiredString(object, "client_id", "client_id")
	if err != nil {
		return ClientInformation{}, err
	}
	clientSecret, err := optionalString(object, "client_secret", "client_secret")
	if err != nil {
		return ClientInformation{}, err
	}
	redirects, err := optionalStrings(object, "redirect_uris", "redirect_uris")
	if err != nil {
		return ClientInformation{}, err
	}
	authMethod, err := optionalString(object, "token_endpoint_auth_method", "token_endpoint_auth_method")
	if err != nil {
		return ClientInformation{}, err
	}
	clientName, err := optionalString(object, "client_name", "client_name")
	if err != nil {
		return ClientInformation{}, err
	}
	applicationType, err := optionalString(object, "application_type", "application_type")
	if err != nil {
		return ClientInformation{}, err
	}
	return ClientInformation{
		ClientID:                clientID,
		ClientSecret:            clientSecret,
		ClientIDIssuedAt:        optionalInt64(object, "client_id_issued_at"),
		ClientSecretExpiresAt:   optionalInt64(object, "client_secret_expires_at"),
		RedirectURIs:            redirects,
		TokenEndpointAuthMethod: authMethod,
		ClientName:              clientName,
		ApplicationType:         applicationType,
	}, nil
}
