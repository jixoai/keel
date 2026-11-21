// Tenancy wiring for `keel serve`: the operator provisioning seam the product
// API exposes and the edge quota enforcer.
//
// Credential authentication lives in cmd/keel/auth.go (account sessions and bot
// keys). The legacy tenant api_keys are minted by `keel admin` for pre-A2
// tooling but are not accepted by the serving path.
package main

import (
	"context"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/agentapi"
	"github.com/jixoai/keel/internal/quota"
	"github.com/jixoai/keel/internal/store"
)

// apiKeyStore adapts *store.Store to agentapi.Provisioner (create/mint/list/
// revoke).
type apiKeyStore struct {
	store *store.Store
}

// CreateTenant implements agentapi.Provisioner. A new tenant is born with the
// built-in permission-group presets; the seed is idempotent, so a repeated
// create converges instead of duplicating.
func (a apiKeyStore) CreateTenant(ctx context.Context, tenantID uuid.UUID, name string) (bool, error) {
	created, err := a.store.CreateTenantReport(ctx, tenantID, name)
	if err != nil {
		return false, err
	}
	if err := accesswire.EnsureBuiltinGroups(ctx, a.store, tenantID); err != nil {
		return false, err
	}
	return created, nil
}

// MintAPIKey implements agentapi.Provisioner.
func (a apiKeyStore) MintAPIKey(ctx context.Context, tenantID, keyID uuid.UUID, hash []byte, prefix, role, label string) error {
	_, err := a.store.CreateAPIKey(ctx, tenantID, keyID, hash, prefix, role, label)
	return err
}

// TenantExists implements agentapi.Provisioner.
func (a apiKeyStore) TenantExists(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	return a.store.TenantExists(ctx, tenantID)
}

// ListAPIKeys implements agentapi.Provisioner.
func (a apiKeyStore) ListAPIKeys(ctx context.Context, tenantID uuid.UUID) ([]agentapi.APIKeyView, error) {
	keys, err := a.store.ListAPIKeys(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]agentapi.APIKeyView, 0, len(keys))
	for _, key := range keys {
		out = append(out, agentapi.APIKeyView{
			KeyID:     key.KeyID.String(),
			Prefix:    key.Prefix,
			Role:      key.Role,
			Label:     key.Label,
			CreatedAt: key.CreatedAt.UTC(),
			RevokedAt: key.RevokedAt,
		})
	}
	return out, nil
}

// RevokeAPIKey implements agentapi.Provisioner.
func (a apiKeyStore) RevokeAPIKey(ctx context.Context, tenantID, keyID uuid.UUID) (bool, error) {
	return a.store.RevokeAPIKey(ctx, tenantID, keyID)
}

// quotaReporter adapts the store and the edge enforcer to agentapi.QuotaReporter.
type quotaReporter struct {
	store *store.Store
	edge  *quota.Edge
}

// TenantQuota implements agentapi.QuotaReporter.
func (q quotaReporter) TenantQuota(ctx context.Context, tenantID uuid.UUID) (quota.TenantQuota, error) {
	row, err := q.store.GetTenantQuota(ctx, tenantID)
	if err != nil {
		return quota.TenantQuota{}, err
	}
	return quota.TenantQuota{
		Streams:      row.Streams,
		BytesPerSec:  row.BytesPerSec,
		StorageBytes: row.StorageBytes,
	}, nil
}

// StoredBytes implements agentapi.QuotaReporter.
func (q quotaReporter) StoredBytes(ctx context.Context, tenantID uuid.UUID) (int64, error) {
	return q.store.TenantStoredBytes(ctx, tenantID)
}

// ActiveStreams implements agentapi.QuotaReporter.
func (q quotaReporter) ActiveStreams(tenantID uuid.UUID) int {
	if q.edge == nil {
		return 0
	}
	return q.edge.ActiveStreams(tenantID)
}

// newEdgeQuota builds the per-tenant edge enforcer over the store. A nil store
// yields a nil edge (every dimension admitted), which is what a deployment
// without a database in the test harness wants.
func newEdgeQuota(st *store.Store) *quota.Edge {
	if st == nil {
		return nil
	}
	return quota.NewEdge(quota.QuotaSourceFunc(func(ctx context.Context, tenantID uuid.UUID) (quota.TenantQuota, error) {
		row, err := st.GetTenantQuota(ctx, tenantID)
		if err != nil {
			return quota.TenantQuota{}, err
		}
		return quota.TenantQuota{
			Streams:      row.Streams,
			BytesPerSec:  row.BytesPerSec,
			StorageBytes: row.StorageBytes,
		}, nil
	}), st.TenantStoredBytes)
}
