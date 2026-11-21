// Package auth: request-scoped principal carrier.
//
// The serving edge (cmd/keel) resolves a cookie session or a bot key and then
// forwards the request. Handlers above the edge (internal/agentapi, future
// harnessapi) must authorize against that resolved identity, never against a
// client-supplied header. This file carries the principal through the request
// context so the edge and the product surface share one identity object
// (change keel-bot-identity-access, A4.1).
package auth

import "context"

type principalContextKey struct{}

// WithPrincipal returns a child context carrying the authenticated principal.
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

// PrincipalFromContext returns the principal the edge resolved for this
// request. A direct API caller that bypassed the edge has none; authorization
// must then fail closed.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}
