// MCP OAuth challenge visibility. A server's 401 challenge is the only signal
// that a sign-in is required; without it an operator sees just a disconnected
// MCP server. The runtime logs the OAuth error, scope, and resource metadata
// URL (never tokens or the WWW-Authenticate header verbatim).

package main

import (
	"log/slog"

	"github.com/jixoai/keel/internal/mcp/oauth"
)

// logMCPChallenge adapts the MCP manager's OnChallenge hook to the runtime log.
func logMCPChallenge(log *slog.Logger) func(server string, challenge oauth.Challenge) {
	return func(server string, challenge oauth.Challenge) {
		log.Warn("mcp oauth challenge",
			"server", server,
			"error", challenge.Error,
			"scope", challenge.Scope,
			"resource_metadata_url", challenge.ResourceMetadataURL)
	}
}
