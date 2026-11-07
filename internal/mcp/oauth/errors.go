// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/oauth/errors.ts (MIT License); implemented independently in
// Go. Contract kept identical; see docs/UPSTREAM.md.

package oauth

import (
	"errors"
	"fmt"
)

// Error is an OAuth error response (`error`, `error_description`).
type Error struct {
	Code         string
	Message      string
	ErrorURI     string
	HTTPStatus   int
	ResponseBody string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

// IssuerMismatchError reports a metadata issuer that does not match discovery,
// or an authorization response `iss` that does not name the authorization
// server (RFC 9207).
type IssuerMismatchError struct {
	Expected *string
	Received *string
}

func (e *IssuerMismatchError) Error() string {
	format := func(issuer *string) string {
		if issuer == nil {
			return "none"
		}
		return fmt.Sprintf("%q", *issuer)
	}
	return fmt.Sprintf("OAuth issuer mismatch: expected %s, received %s", format(e.Expected), format(e.Received))
}

// InsecureEndpointError refuses to send credentials to a non-HTTPS endpoint.
// Loopback HTTP is allowed for local clients.
type InsecureEndpointError struct {
	Endpoint string
}

func (e *InsecureEndpointError) Error() string {
	return fmt.Sprintf("Refusing to send OAuth credentials to non-HTTPS endpoint %s", e.Endpoint)
}

// RegistrationError is a failed dynamic client registration.
type RegistrationError struct {
	Status int
	Body   string
}

func (e *RegistrationError) Error() string {
	return fmt.Sprintf("OAuth dynamic client registration failed with status %d: %s", e.Status, e.Body)
}

// AuthorizationRequiredError means the user has to authorize (again) in a
// browser. It is the typed signal a host turns into a sign-in prompt; the MCP
// connection never starts a browser flow itself.
type AuthorizationRequiredError struct {
	// Reason describes why interaction is needed, when known.
	Reason string
}

func (e *AuthorizationRequiredError) Error() string {
	if e.Reason != "" {
		return "MCP OAuth authorization requires user interaction: " + e.Reason
	}
	return "MCP OAuth authorization requires user interaction"
}

// RefreshLockBusyError means another process holds the refresh lock for the
// whole wait. Sign-in then skips the refresh and goes to the browser step.
type RefreshLockBusyError struct{}

func (e *RefreshLockBusyError) Error() string {
	return "Another process is refreshing the MCP server's tokens"
}

// ErrSignInCancelled is returned when a sign-in is cancelled through its
// context (or the user gives up).
var ErrSignInCancelled = errors.New("sign-in cancelled")

// ErrSignInInProgress is returned when another callback consumed the pending
// step and has not stored tokens yet. The caller may retry; once the winning
// exchange saved tokens the same callback is an idempotent success.
var ErrSignInInProgress = errors.New("another callback is completing this sign-in")

// ErrClientMetadataURL is a malformed Client ID Metadata Document URL.
var ErrClientMetadataURL = errors.New("Invalid OAuth client metadata URL")

// IsAuthorizationRequired reports whether err is an interactive-authorization
// signal. Wrapped errors keep the identity through errors.As.
func IsAuthorizationRequired(err error) bool {
	var target *AuthorizationRequiredError
	return errors.As(err, &target)
}

// IsRefreshLockBusy reports whether the refresh lock was busy for the whole
// wait.
func IsRefreshLockBusy(err error) bool {
	var target *RefreshLockBusyError
	return errors.As(err, &target)
}
