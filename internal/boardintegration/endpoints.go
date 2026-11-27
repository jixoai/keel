package boardintegration

// Provider endpoint trust boundary (review P2). An entry's settings may name an
// API base URL (needed for tests and private gateways), but the board mount must
// not let a runtime configuration turn a platform token into a request to an
// arbitrary host. A custom endpoint is accepted only for loopback (local
// development/tests) or an HTTPS host the deployment explicitly allowlists.

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/jixoai/keel/internal/store"
)

// validateProviderEndpoints checks the platform settings' custom base URLs.
// Empty URLs keep the adapter's built-in endpoint and are always accepted.
func validateProviderEndpoints(record store.BoardSyncEntryRecord, allowed []string) error {
	settings, err := parsePlatformSettings(record.Platform, record.Settings)
	if err != nil {
		return err
	}
	switch record.Platform {
	case syncPlatformSlack:
		return checkProviderEndpoint(settings.slack.APIBaseURL, "slack.com", allowed)
	case syncPlatformGitHub:
		return checkProviderEndpoint(settings.github.APIBaseURL, "api.github.com", allowed)
	case syncPlatformLinear:
		return checkProviderEndpoint(settings.linear.APIBaseURL, "api.linear.app", allowed)
	case syncPlatformChatSDK:
		// A sidecar is deployment-specific; there is no official host.
		return checkProviderEndpoint(settings.chatsdk.BridgeURL, "", allowed)
	default:
		return fmt.Errorf("settings: unknown platform %q", record.Platform)
	}
}

func checkProviderEndpoint(raw, official string, allowed []string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("settings: provider URL: %v", err)
	}
	if parsed.User != nil {
		return fmt.Errorf("settings: provider URL must not carry credentials")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("settings: provider URL must be http or https")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return fmt.Errorf("settings: provider URL needs a host")
	}
	if isLoopbackHost(host) {
		return nil
	}
	if official != "" && strings.EqualFold(host, official) {
		return nil
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("settings: provider URL %q must use https or a loopback host", host)
	}
	for _, permitted := range allowed {
		if strings.EqualFold(strings.TrimSpace(permitted), host) {
			return nil
		}
	}
	return fmt.Errorf("settings: provider host %q is not in KEEL_BOARD_PROVIDER_HOSTS", host)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
