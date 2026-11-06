// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/config.ts (MIT License); implemented independently in Go.
// Contract kept identical; see docs/UPSTREAM.md.
//
// The validator accepts the shared `mcpServers` configuration shape so configs
// can be copied between MCP clients: stdio entries (command/args/env/cwd) and
// Streamable HTTP entries (url/headers/oauth), with enabled, description and
// timeout for both. Unknown fields are kept in Config, unchecked, for the
// caller to validate afterwards.

package mcp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Server name grammar: tool names and namespaces are built from it.
var serverNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// McpServerConfigResult is one validated entry: Kind is "stdio", "http", or
// "invalid" (with Error). Config is the entry as given.
type McpServerConfigResult struct {
	Kind   string
	Config map[string]any
	Error  string
}

// McpServerConfigOptions adds application checks to the shared validation.
type McpServerConfigOptions struct {
	// CheckMetadataDocumentCallback validates the oauth.callbackUrl of a CIMD
	// entry. It returns an error message. Without it, "cimd" is refused.
	CheckMetadataDocumentCallback func(callbackURL string) string
}

// ValidateMcpServerConfig validates one entry of mcpServers.
func ValidateMcpServerConfig(name string, raw json.RawMessage, options McpServerConfigOptions) McpServerConfigResult {
	invalid := func(format string, args ...any) McpServerConfigResult {
		return McpServerConfigResult{Kind: "invalid", Error: fmt.Sprintf(format, args...)}
	}
	if !serverNamePattern.MatchString(name) {
		return invalid(`invalid server name %q (use letters, digits, "_" and "-")`, name)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil || config == nil {
		return invalid(`server %q must be an object`, name)
	}
	if fieldError := validateBaseFields(config); fieldError != "" {
		return invalid(`server %q: %s`, name, fieldError)
	}
	typeValue, hasType := config["type"]
	if hasType && typeValue == "sse" {
		return invalid(`server %q: legacy SSE transport is not supported; use the streamable HTTP URL`, name)
	}
	urlValue, hasURL := config["url"]
	commandValue, hasCommand := config["command"]
	if hasURL {
		if _, ok := urlValue.(string); ok && transportTypeAllowed(typeValue, hasType, "http", "streamable-http") {
			if fieldError := validateHTTPFields(config, urlValue.(string), options); fieldError != "" {
				return invalid(`server %q: %s`, name, fieldError)
			}
			return McpServerConfigResult{Kind: "http", Config: config}
		}
	}
	if hasCommand {
		if _, ok := commandValue.(string); ok && transportTypeAllowed(typeValue, hasType, "stdio") {
			if fieldError := validateStdioFields(config); fieldError != "" {
				return invalid(`server %q: %s`, name, fieldError)
			}
			return McpServerConfigResult{Kind: "stdio", Config: config}
		}
	}
	return invalid(`server %q needs either "command" (stdio) or "url" (streamable HTTP)`, name)
}

func transportTypeAllowed(value any, hasType bool, allowed ...string) bool {
	if !hasType {
		return true
	}
	name, ok := value.(string)
	if !ok {
		return false
	}
	for _, candidate := range allowed {
		if name == candidate {
			return true
		}
	}
	return false
}

func validateBaseFields(entry map[string]any) string {
	if enabled, ok := entry["enabled"]; ok {
		if _, ok := enabled.(bool); !ok {
			return "enabled must be a boolean"
		}
	}
	if description, ok := entry["description"]; ok {
		if _, ok := description.(string); !ok {
			return "description must be a string"
		}
	}
	if timeout, ok := entry["timeout"]; ok {
		number, ok := timeout.(float64)
		if !ok || !(number > 0) {
			return "timeout must be a positive number of seconds"
		}
	}
	return ""
}

func validateHTTPFields(entry map[string]any, rawURL string, options McpServerConfigOptions) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "url must be an http or https URL"
	}
	if headers, ok := entry["headers"]; ok {
		if !isStringRecord(headers) {
			return "headers must map names to strings"
		}
	}
	if oauth, ok := entry["oauth"]; ok {
		return validateOAuth(oauth, options)
	}
	return ""
}

func validateStdioFields(entry map[string]any) string {
	if args, ok := entry["args"]; ok {
		list, ok := args.([]any)
		if !ok {
			return "args must be an array of strings"
		}
		for _, arg := range list {
			if _, ok := arg.(string); !ok {
				return "args must be an array of strings"
			}
		}
	}
	if env, ok := entry["env"]; ok && !isStringRecord(env) {
		return "env must map names to strings"
	}
	if cwd, ok := entry["cwd"]; ok {
		if _, ok := cwd.(string); !ok {
			return "cwd must be a string"
		}
	}
	return ""
}

func isStringRecord(value any) bool {
	record, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for _, entry := range record {
		if _, ok := entry.(string); !ok {
			return false
		}
	}
	return true
}

func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}

func validateOAuth(value any, options McpServerConfigOptions) string {
	oauth, ok := value.(map[string]any)
	if !ok {
		return "oauth must be an object"
	}
	if clientID, ok := oauth["clientId"]; ok {
		if _, ok := clientID.(string); !ok {
			return "oauth.clientId must be a string"
		}
	}
	if secret, ok := oauth["clientSecret"]; ok {
		if _, ok := secret.(string); !ok {
			return "oauth.clientSecret must be a string"
		}
	}
	var callbackPort int
	hasCallbackPort := false
	if port, ok := oauth["callbackPort"]; ok {
		number, ok := port.(float64)
		if !ok || number != float64(int(number)) || number < 1 || number > 65535 {
			return "oauth.callbackPort must be a port number"
		}
		callbackPort, hasCallbackPort = int(number), true
	}
	if callbackURL, ok := oauth["callbackUrl"]; ok {
		text, ok := callbackURL.(string)
		if !ok || !isLoopbackRedirectURI(text) {
			return "oauth.callbackUrl must be an http URI on localhost, 127.0.0.1, or [::1] without query or fragment"
		}
		parsed, _ := url.Parse(text)
		if parsed.Port() != "" && hasCallbackPort {
			var portNumber int
			fmt.Sscanf(parsed.Port(), "%d", &portNumber)
			if portNumber != callbackPort {
				return "oauth.callbackUrl and oauth.callbackPort name different ports"
			}
		}
	}
	if scope, ok := oauth["scope"]; ok {
		if _, ok := scope.(string); !ok {
			return "oauth.scope must be a string"
		}
	}
	if clientName, ok := oauth["clientName"]; ok {
		text, ok := clientName.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return "oauth.clientName must be a non-empty string"
		}
	}
	if registration, ok := oauth["clientRegistration"]; ok {
		text, ok := registration.(string)
		if !ok {
			return `oauth.clientRegistration must be "auto", "dcr", or "cimd"`
		}
		switch text {
		case "auto", "dcr":
		case "cimd":
			if _, ok := oauth["clientId"]; ok {
				return `oauth.clientRegistration "cimd" cannot be combined with oauth.clientId or oauth.clientName`
			}
			if _, ok := oauth["clientName"]; ok {
				return `oauth.clientRegistration "cimd" cannot be combined with oauth.clientId or oauth.clientName`
			}
			if options.CheckMetadataDocumentCallback == nil {
				return `oauth.clientRegistration "cimd" is not supported: there are no Client ID Metadata Documents`
			}
			callback := ""
			if text, ok := oauth["callbackUrl"].(string); ok {
				callback = text
			}
			if message := options.CheckMetadataDocumentCallback(callback); message != "" {
				return message
			}
		default:
			return `oauth.clientRegistration must be "auto", "dcr", or "cimd"`
		}
	}
	if metadata, ok := oauth["authServerMetadataUrl"]; ok {
		text, ok := metadata.(string)
		if !ok {
			return "oauth.authServerMetadataUrl must be an https URL, or http on localhost, 127.0.0.1, or [::1]"
		}
		parsed, err := url.Parse(text)
		if err != nil || parsed.Host == "" ||
			!(parsed.Scheme == "https" || (parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname()))) {
			return "oauth.authServerMetadataUrl must be an https URL, or http on localhost, 127.0.0.1, or [::1]"
		}
	}
	return ""
}

func isLoopbackRedirectURI(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || !isLoopbackHost(parsed.Hostname()) {
		return false
	}
	return parsed.RawQuery == "" && parsed.Fragment == ""
}

// IsMcpServerOverride reports whether an entry overrides a server defined
// elsewhere instead of defining one: no command, url, or type.
func IsMcpServerOverride(entry map[string]any) bool {
	_, hasCommand := entry["command"]
	_, hasURL := entry["url"]
	_, hasType := entry["type"]
	return !hasCommand && !hasURL && !hasType
}
