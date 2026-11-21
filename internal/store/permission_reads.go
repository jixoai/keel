// Permission-group read projections for the access adapter
// (change keel-bot-identity-access, A4.1).
//
// The A1 store writes groups, members and grants one row at a time. The access
// adapter needs the two joined reads it cannot express with those methods: the
// grants an account receives through every group it belongs to, and the groups
// of one account. Keeping the joins here (instead of in internal/accesswire)
// preserves the rule that internal/access and its adapters never hand-write SQL
// against the tenant schema.
package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// maxAccountGroupGrants bounds the group-grant join for one account. The bound
// is far above any real preset combination; crossing it is an error, never a
// silent truncation, because a dropped grant would make authorization depend on
// row order.
const maxAccountGroupGrants = 2000

// ListPermissionGroupGrantsForAccount returns every (key, scope) granted to the
// account through its group memberships. Direct grants are NOT included: they
// have their own read path (ListPermissionGrants), and the access GrantSource
// contract keeps the two sources distinct (A1 decision 5).
func (s *Store) ListPermissionGroupGrantsForAccount(ctx context.Context, tenantID, accountID uuid.UUID, limit int) ([]PermissionGroupGrantRecord, error) {
	if tenantID == uuid.Nil || accountID == uuid.Nil {
		return nil, invalidIdentity("list account group grants", "tenant and account ids are required")
	}
	if limit <= 0 || limit > maxAccountGroupGrants {
		limit = maxAccountGroupGrants
	}
	rows, err := s.pool.Query(ctx,
		`SELECT g.tenant_id, g.group_id, g.permission_key, g.scope
		   FROM permission_group_members m
		   JOIN permission_group_grants g
		     ON g.tenant_id = m.tenant_id AND g.group_id = m.group_id
		  WHERE m.tenant_id = $1 AND m.account_id = $2
		  ORDER BY g.permission_key, g.scope, g.group_id
		  LIMIT $3`, tenantID, accountID, limit+1)
	if err != nil {
		return nil, identityWriteError("list account group grants", err)
	}
	defer rows.Close()
	out := make([]PermissionGroupGrantRecord, 0, 8)
	for rows.Next() {
		var record PermissionGroupGrantRecord
		if err := rows.Scan(&record.TenantID, &record.GroupID,
			&record.PermissionKey, &record.Scope); err != nil {
			return nil, identityWriteError("scan account group grant", err)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("list account group grants", err)
	}
	if len(out) > limit {
		return nil, fmt.Errorf("list account group grants: more than %d grants (refusing to truncate authority)", limit)
	}
	return out, nil
}

// ListPermissionGroupsForAccount returns the groups an account belongs to.
func (s *Store) ListPermissionGroupsForAccount(ctx context.Context, tenantID, accountID uuid.UUID, limit int) ([]PermissionGroupRecord, error) {
	if tenantID == uuid.Nil || accountID == uuid.Nil {
		return nil, invalidIdentity("list account groups", "tenant and account ids are required")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT g.tenant_id, g.group_id, g.name, g.builtin, g.created_at, g.updated_at
		   FROM permission_group_members m
		   JOIN permission_groups g
		     ON g.tenant_id = m.tenant_id AND g.group_id = m.group_id
		  WHERE m.tenant_id = $1 AND m.account_id = $2
		  ORDER BY lower(g.name), g.group_id
		  LIMIT $3`, tenantID, accountID, identityListLimit(limit))
	if err != nil {
		return nil, identityWriteError("list account groups", err)
	}
	defer rows.Close()
	out := make([]PermissionGroupRecord, 0, 8)
	for rows.Next() {
		record, err := scanPermissionGroupRow(rows)
		if err != nil {
			return nil, identityWriteError("scan account group", err)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("list account groups", err)
	}
	return out, nil
}

// PermissionGroupMembership answers the relation-source question "is this
// account a member of this group?" while distinguishing an absent group
// (ErrNotFound, which the access adapter turns into a fail-closed unresolved
// subject) from a group the account simply is not in (false, nil).
func (s *Store) PermissionGroupMembership(ctx context.Context, tenantID, groupID, accountID uuid.UUID) (bool, error) {
	if tenantID == uuid.Nil || groupID == uuid.Nil || accountID == uuid.Nil {
		return false, invalidIdentity("permission group membership", "tenant, group and account ids are required")
	}
	var member bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM permission_group_members
		    WHERE tenant_id = $1 AND group_id = $2 AND account_id = $3)`,
		tenantID, groupID, accountID).Scan(&member); err != nil {
		return false, identityWriteError("permission group membership", err)
	}
	if member {
		return true, nil
	}
	// The group must exist for "not a member" to be a resolvable answer.
	if _, err := s.GetPermissionGroup(ctx, tenantID, groupID); err != nil {
		return false, err
	}
	return false, nil
}

// UpsertBuiltinPermissionGroup creates or refreshes a built-in group row. Only
// the deployment seed calls it: builtin is forced true, and a name conflict
// with a group of another id is ErrConflict (the seed then refuses to start
// instead of silently hijacking a user-created group).
func (s *Store) UpsertBuiltinPermissionGroup(ctx context.Context, tenantID, groupID uuid.UUID, name string) (PermissionGroupRecord, error) {
	name = strings.TrimSpace(name)
	if tenantID == uuid.Nil || groupID == uuid.Nil || name == "" {
		return PermissionGroupRecord{}, invalidIdentity("upsert builtin group", "tenant, group and name are required")
	}
	record, err := scanPermissionGroupRow(s.pool.QueryRow(ctx,
		`INSERT INTO permission_groups (tenant_id, group_id, name, builtin)
		 VALUES ($1, $2, $3, true)
		 ON CONFLICT (tenant_id, group_id)
		 DO UPDATE SET name = EXCLUDED.name, builtin = true, updated_at = now()
		 RETURNING tenant_id, group_id, name, builtin, created_at, updated_at`,
		tenantID, groupID, name))
	if err != nil {
		return PermissionGroupRecord{}, identityWriteError("upsert builtin group", err)
	}
	return record, nil
}

// ReplacePermissionGroupGrants makes the group's grant set exactly grants. It
// runs in one transaction so a concurrent effective-permission read never sees
// a half-replaced preset. Only the built-in seed uses it; the API writes one
// pair at a time.
func (s *Store) ReplacePermissionGroupGrants(ctx context.Context, tenantID, groupID uuid.UUID, grants []PermissionGroupGrantRecord) error {
	if tenantID == uuid.Nil || groupID == uuid.Nil {
		return invalidIdentity("replace group grants", "tenant and group ids are required")
	}
	for _, grant := range grants {
		if err := validatePermissionGrantFields(strings.TrimSpace(grant.PermissionKey), grant.Scope); err != nil {
			return err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return identityWriteError("replace group grants", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`DELETE FROM permission_group_grants WHERE tenant_id = $1 AND group_id = $2`,
		tenantID, groupID); err != nil {
		return identityWriteError("replace group grants", err)
	}
	for _, grant := range grants {
		if _, err := tx.Exec(ctx,
			`INSERT INTO permission_group_grants (tenant_id, group_id, permission_key, scope)
			 VALUES ($1, $2, $3, $4)`,
			tenantID, groupID, strings.TrimSpace(grant.PermissionKey), grant.Scope); err != nil {
			return identityWriteError("replace group grants", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return identityWriteError("replace group grants", err)
	}
	return nil
}

// Relation fan-in bound for one batch visibility read. It is deliberately
// larger than the listing page bound and smaller than "unbounded"; crossing it
// is an error, not a silent truncation (a truncated tuple could hide a grant).
const maxBatchRelationRows = 20000

// ListResourceRelationsForResources returns the tuples of many resources of one
// type, keyed by resource id. A missing id simply has no tuples. The read is
// tenant-scoped and bounded; exceeding the bound fails closed.
func (s *Store) ListResourceRelationsForResources(ctx context.Context, tenantID uuid.UUID, resourceType string, ids []string, limit int) (map[string][]ResourceRelation, error) {
	resourceType = strings.TrimSpace(resourceType)
	if tenantID == uuid.Nil || resourceType == "" {
		return nil, invalidIdentity("list batch relations", "tenant and resource type are required")
	}
	out := make(map[string][]ResourceRelation, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	if limit <= 0 || limit > maxBatchRelationRows {
		limit = maxBatchRelationRows
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, resource_type, resource_id, relation, subject_type,
		        subject_id, created_by, created_at
		   FROM resource_relations
		  WHERE tenant_id = $1 AND resource_type = $2 AND resource_id = ANY($3)
		  ORDER BY resource_id, relation, subject_type, subject_id
		  LIMIT $4`, tenantID, resourceType, ids, limit+1)
	if err != nil {
		return nil, identityWriteError("list batch relations", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		relation, err := scanResourceRelationRow(rows)
		if err != nil {
			return nil, identityWriteError("scan batch relation", err)
		}
		count++
		if count > limit {
			// Fail closed instead of returning a truncated tuple set: a dropped
			// tuple could hide a grant or a share.
			return nil, fmt.Errorf("list batch relations: relation fan-in exceeds %d", limit)
		}
		out[relation.ResourceID] = append(out[relation.ResourceID], relation)
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("list batch relations", err)
	}
	return out, nil
}
