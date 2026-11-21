// Built-in permission-group seeding (change keel-bot-identity-access, A4.1).
//
// The seed runs at boot, per tenant, and is idempotent: group ids are
// deterministic (tenant + preset name), the group row is upserted, and its
// grant set is replaced with the code catalog. Re-running after a code change
// converges the tenant to the new preset; a name collision with a
// user-created group fails the seed instead of silently adopting it.
//
// Built-in groups never carry is_super_admin and never create accounts: they
// are presets of (key, scope) grants, nothing more.
package accesswire

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/store"
)

// EnsureBuiltinGroups seeds every preset for one tenant. It returns an error if
// any group cannot be written; the caller (serve boot) treats that as fatal so
// a deployment never runs with a half-seeded authority surface.
func EnsureBuiltinGroups(ctx context.Context, st *store.Store, tenantID uuid.UUID) error {
	if st == nil {
		return errors.New("accesswire: store is required to seed permission groups")
	}
	if tenantID == uuid.Nil {
		return errors.New("accesswire: tenant is required to seed permission groups")
	}
	if err := access.SelfCheckBuiltinGroups(); err != nil {
		return fmt.Errorf("accesswire: builtin catalog is invalid: %w", err)
	}
	for _, preset := range access.BuiltinGroups() {
		groupID := access.BuiltinGroupID(tenantID, preset.Name)
		if _, err := st.UpsertBuiltinPermissionGroup(ctx, tenantID, groupID, preset.Name); err != nil {
			return fmt.Errorf("accesswire: seed permission group %q: %w", preset.Name, err)
		}
		grants := make([]store.PermissionGroupGrantRecord, 0, len(preset.Grants))
		for _, grant := range preset.Grants {
			grants = append(grants, store.PermissionGroupGrantRecord{
				TenantID: tenantID, GroupID: groupID,
				PermissionKey: grant.Key, Scope: string(grant.Scope),
			})
		}
		if err := st.ReplacePermissionGroupGrants(ctx, tenantID, groupID, grants); err != nil {
			return fmt.Errorf("accesswire: seed grants of %q: %w", preset.Name, err)
		}
	}
	return nil
}
