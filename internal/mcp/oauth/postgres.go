// Keel runtime replacement for PI's per-server JSON state and proper-lockfile:
// the same OAuth state document encrypted in PostgreSQL, plus the pending
// browser step and the cross-process refresh lock.

package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"
)

// PostgresStore persists one tenant/server's OAuth state in PostgreSQL.
type PostgresStore struct {
	DB       *store.Store
	Box      *store.SecretBox
	TenantID uuid.UUID
	Server   string
	// LockWait bounds waiting for another process's refresh. Zero uses 30s.
	LockWait time.Duration
}

// NewPostgresStore builds the store of one MCP server.
func NewPostgresStore(db *store.Store, box *store.SecretBox, tenantID uuid.UUID, server string) *PostgresStore {
	return &PostgresStore{DB: db, Box: box, TenantID: tenantID, Server: server, LockWait: 30 * time.Second}
}

// Load reads and decrypts the state document.
func (p *PostgresStore) Load(ctx context.Context) (*OAuthState, error) {
	if p == nil || p.DB == nil {
		return nil, nil
	}
	credentials, err := p.DB.LoadMCPOAuthCredentials(ctx, p.TenantID, p.Server, p.Box)
	if errors.Is(err, store.ErrMCPOAuthNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state OAuthState
	if err := json.Unmarshal(credentials.Payload, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// Save encrypts and writes the state document.
func (p *PostgresStore) Save(ctx context.Context, state *OAuthState) error {
	if p == nil || p.DB == nil {
		return nil
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	credentials := store.MCPOAuthCredentials{
		TenantID: p.TenantID,
		Server:   p.Server,
		Payload:  payload,
	}
	if state != nil && state.Tokens != nil {
		credentials.Scopes = state.Tokens.Scope
		if state.TokensExpireAt > 0 {
			expires := time.UnixMilli(state.TokensExpireAt)
			credentials.ExpiresAt = &expires
		}
	}
	return p.DB.SaveMCPOAuthCredentials(ctx, credentials, p.Box)
}

// WithRefreshLock runs fn under the cross-process refresh lock.
func (p *PostgresStore) WithRefreshLock(ctx context.Context, fn func(ctx context.Context) error) error {
	if p == nil || p.DB == nil {
		return fn(ctx)
	}
	err := p.DB.WithMCPOAuthRefreshLock(ctx, p.TenantID, p.Server, p.LockWait, fn)
	if errors.Is(err, store.ErrMCPOAuthRefreshLockBusy) {
		return &RefreshLockBusyError{}
	}
	return err
}

// StartPending supersedes older browser steps and records this one.
func (p *PostgresStore) StartPending(ctx context.Context, pending PendingSignIn) error {
	return p.DB.StartMCPOAuthPending(ctx, store.MCPOAuthPending{
		TenantID:         p.TenantID,
		Server:           p.Server,
		State:            pending.State,
		CodeVerifier:     pending.CodeVerifier,
		RedirectURL:      pending.RedirectURL,
		AuthorizationURL: pending.AuthorizationURL,
		Scopes:           pending.Scope,
		ExpiresAt:        pending.ExpiresAt,
	})
}

// Pending returns an unconsumed browser step, or nil.
func (p *PostgresStore) Pending(ctx context.Context, state string) (*PendingSignIn, error) {
	row, err := p.DB.GetMCPOAuthPending(ctx, p.TenantID, p.Server, state)
	if errors.Is(err, store.ErrMCPOAuthNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return pendingFromStore(row, false), nil
}

// FindPendingByState finds a browser step when the browser callback has no
// tenant header. State is the unguessable capability.
func (p *PostgresStore) FindPendingByState(ctx context.Context, state string) (*PendingSignIn, error) {
	row, err := p.DB.FindMCPOAuthPendingByState(ctx, state)
	if errors.Is(err, store.ErrMCPOAuthNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return pendingFromStore(row, false), nil
}

// ConsumePending atomically marks the browser step used and returns it. It
// returns nil when another caller already consumed the state (or it expired);
// CompleteSignIn then treats a callback whose tokens already exist as an
// idempotent success.
func (p *PostgresStore) ConsumePending(ctx context.Context, state string) (*PendingSignIn, error) {
	row, err := p.DB.ConsumeMCPOAuthPending(ctx, p.TenantID, p.Server, state)
	if errors.Is(err, store.ErrMCPOAuthNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return pendingFromStore(row, true), nil
}

// Delete drops the credentials (sign-out).
func (p *PostgresStore) Delete(ctx context.Context) error {
	return p.DB.DeleteMCPOAuthCredentials(ctx, p.TenantID, p.Server)
}

func pendingFromStore(row store.MCPOAuthPending, consumed bool) *PendingSignIn {
	if !consumed && row.ConsumedAt != nil {
		return nil
	}
	return &PendingSignIn{
		State:            row.State,
		CodeVerifier:     row.CodeVerifier,
		RedirectURL:      row.RedirectURL,
		AuthorizationURL: row.AuthorizationURL,
		Scope:            row.Scopes,
		ExpiresAt:        row.ExpiresAt,
	}
}
