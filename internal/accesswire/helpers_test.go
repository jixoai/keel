package accesswire

import (
	"context"

	"github.com/jixoai/keel/internal/auth"
)

// edgePrincipal is the serving-edge identity of the fixture owner.
func edgePrincipal(f fixture) auth.Principal {
	return auth.Principal{
		Kind: auth.PrincipalHuman, TenantID: f.tenant, AccountID: f.owner.ID,
		Username: f.owner.Username, DisplayName: f.owner.DisplayName,
	}
}

// WithEdgePrincipal seeds the request context the way cmd/keel's middleware
// does. It is shared by the accesswire and agentapi tests.
func WithEdgePrincipal(ctx context.Context, principal auth.Principal) context.Context {
	return auth.WithPrincipal(ctx, principal)
}
