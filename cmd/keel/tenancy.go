// Tenancy wiring for `keel serve`: the API-key middleware, the provisioning
// seam the product API exposes, and the edge quota enforcer.
//
// The credential path is deliberately layered so nothing that existed before
// this change changes behaviour:
//
//	auth.Authorizer   machine credential (Bearer keel_* / X-API-Key) → tenant
//	authHandler       browser passphrase + signed cookie → tenant
//
// The authorizer only acts on a credential that is unambiguously a key; every
// other request falls through to the cookie handler, which is why a deployment
// with no keys (and no KEEL_ADMIN_KEY) behaves exactly as it did.
package main

import (
	"context"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentapi"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/quota"
	"github.com/jixoai/keel/internal/store"
)

// apiKeyStore adapts *store.Store to the two seams the tenancy surface needs:
// auth.KeyStore (hash → credential) and agentapi.Provisioner (create/mint/list/
// revoke). Both live here because the store's row type carries more than either
// seam should see.
type apiKeyStore struct {
	store *store.Store
}

// LookupAPIKey implements auth.KeyStore.
//
// The lookup runs through the tenant pool with app.bypass_rls='on': the key's
// tenant is what the lookup is trying to discover, so there is no tenant scope
// to set yet. It is the one place the bypass marker is used on a request path,
// and the query is a single indexed probe that returns only the key's own row.
func (a apiKeyStore) LookupAPIKey(ctx context.Context, hash []byte) (auth.KeyRecord, error) {
	if a.store == nil {
		return auth.KeyRecord{}, auth.ErrInvalidCredential
	}
	record, err := a.store.LookupAPIKey(ctx, hash)
	if err != nil {
		return auth.KeyRecord{}, err
	}
	return auth.KeyRecord{
		TenantID: record.TenantID,
		KeyID:    record.KeyID,
		Prefix:   record.Prefix,
		Role:     record.Role,
	}, nil
}

// CreateTenant implements agentapi.Provisioner.
func (a apiKeyStore) CreateTenant(ctx context.Context, tenantID uuid.UUID, name string) (bool, error) {
	return a.store.CreateTenantReport(ctx, tenantID, name)
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
