package access

import (
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// BuiltinGroup is one deployment-seeded permission-group preset. The
// definitions are code, like the key catalog, so a UI can show them without a
// second hand-written list and a seed can never drift from the engine.
//
// A built-in group is read-only authority: an ordinary request cannot rename or
// delete it, and cannot edit its grants (the preset is the product's promise).
// Membership is still editable — joining or leaving a preset is how an account
// receives it. "Save as a copy" is the customization path (A4.4/L5).
//
// A group grants (key, scope) pairs only. It never grants the is_super_admin
// flag: that stays a deployment bootstrap fact and the only super-admin
// mechanism (L6/L17).
type BuiltinGroup struct {
	// Name is the tenant-unique display name ("tenant-admin").
	Name string
	// Grants is the preset's (key, scope) set.
	Grants []Grant
}

// builtinGroupNamespace is the fixed namespace of the deterministic group id:
// the same tenant + name always maps to the same uuid, so seeding is idempotent
// and several replicas converge on one row.
var builtinGroupNamespace = uuid.MustParse("6f3a0f0a-2f0e-5c1a-9d4e-6b1f0c2a7d10")

// BuiltinGroupID returns the deterministic id of a built-in group in a tenant.
func BuiltinGroupID(tenantID uuid.UUID, name string) uuid.UUID {
	return uuid.NewSHA1(builtinGroupNamespace, []byte(tenantID.String()+"/"+strings.TrimSpace(name)))
}

// builtinGroups is the seeded preset catalog. It is ordered for review.
//
//   - super-admin: every key at scope all. It does not make its members
//     deployment super admins; the flag is separate, and cross-tenant reach is
//     still governed by the request's tenant boundary.
//   - tenant-admin: every key at tenant scope — the tenant-wide administrator.
//   - bot-operator: runs and operates bots and team bots (sessions, invocation,
//     triggers, read-only model/role/profile/plugin/secret-metadata access).
//   - viewer: read-only visibility of its own sessions/bots/team bots plus the
//     tenant's roles, profiles, models and plugins.
//   - auditor: the audit trail and the account/permission directories, without
//     mutation.
var builtinGroups = []BuiltinGroup{
	{Name: "super-admin", Grants: allKeysAt(ScopeAll)},
	{Name: "tenant-admin", Grants: allKeysAt(ScopeTenant)},
	{Name: "bot-operator", Grants: []Grant{
		{Key: KeySessionRead, Scope: ScopeTeam},
		{Key: KeySessionWrite, Scope: ScopeTeam},
		{Key: KeySessionDelete, Scope: ScopeTeam},
		{Key: KeySessionShare, Scope: ScopeTeam},
		{Key: KeyBotRead, Scope: ScopeTeam},
		{Key: KeyBotCreate, Scope: ScopeTenant},
		{Key: KeyBotManage, Scope: ScopeTeam},
		{Key: KeyBotBind, Scope: ScopeTeam},
		{Key: KeyBotInvoke, Scope: ScopeTeam},
		{Key: KeyTeamBotRead, Scope: ScopeTeam},
		{Key: KeyTeamBotManage, Scope: ScopeTeam},
		{Key: KeyTriggerRead, Scope: ScopeTeam},
		{Key: KeyTriggerManage, Scope: ScopeTeam},
		{Key: KeyRoleRead, Scope: ScopeTenant},
		{Key: KeyProfileRead, Scope: ScopeTenant},
		{Key: KeyModelRead, Scope: ScopeTenant},
		{Key: KeyPluginRead, Scope: ScopeTenant},
		{Key: KeySecretReadMetadata, Scope: ScopeTenant},
	}},
	{Name: "viewer", Grants: []Grant{
		{Key: KeySessionRead, Scope: ScopeOwn},
		{Key: KeyBotRead, Scope: ScopeOwn},
		{Key: KeyTeamBotRead, Scope: ScopeOwn},
		{Key: KeyTriggerRead, Scope: ScopeOwn},
		{Key: KeyRoleRead, Scope: ScopeTenant},
		{Key: KeyProfileRead, Scope: ScopeTenant},
		{Key: KeyModelRead, Scope: ScopeTenant},
		{Key: KeyPluginRead, Scope: ScopeTenant},
	}},
	{Name: "auditor", Grants: []Grant{
		{Key: KeyAuditRead, Scope: ScopeTenant},
		{Key: KeyAccountRead, Scope: ScopeTenant},
		{Key: KeyPermissionRead, Scope: ScopeTenant},
	}},
}

// allKeysAt returns every registry key at one scope. It is only called for
// scopes every key allows: all scopes allow ScopeAll, and resource-scoped keys
// allow ScopeTenant.
func allKeysAt(scope Scope) []Grant {
	specs := All()
	grants := make([]Grant, 0, len(specs))
	for _, spec := range specs {
		if spec.AllowsScope(scope) {
			grants = append(grants, Grant{Key: spec.Key, Scope: scope})
		}
	}
	return grants
}

// BuiltinGroups returns a deep copy of the built-in preset catalog.
func BuiltinGroups() []BuiltinGroup {
	out := make([]BuiltinGroup, len(builtinGroups))
	for index, group := range builtinGroups {
		out[index] = BuiltinGroup{
			Name:   group.Name,
			Grants: append([]Grant(nil), group.Grants...),
		}
	}
	return out
}

// BuiltinGroupNames returns the preset names, sorted.
func BuiltinGroupNames() []string {
	names := make([]string, len(builtinGroups))
	for index, group := range builtinGroups {
		names[index] = group.Name
	}
	sort.Strings(names)
	return names
}

// LookupBuiltinGroup returns one preset by its exact name.
func LookupBuiltinGroup(name string) (BuiltinGroup, bool) {
	name = strings.TrimSpace(name)
	for _, group := range builtinGroups {
		if group.Name == name {
			return BuiltinGroup{Name: group.Name, Grants: append([]Grant(nil), group.Grants...)}, true
		}
	}
	return BuiltinGroup{}, false
}

// SelfCheckBuiltinGroups validates the preset catalog: non-empty, unique
// names, non-empty grant lists, and every grant legal for its key and scope.
// It runs from SelfCheck at package init, so a bad preset fails at boot rather
// than at the first seed.
func SelfCheckBuiltinGroups() error {
	return selfCheckBuiltinGroups(builtinGroups)
}

func selfCheckBuiltinGroups(groups []BuiltinGroup) error {
	if len(groups) == 0 {
		return fmt.Errorf("built-in group catalog is empty")
	}
	seen := make(map[string]int, len(groups))
	for index, group := range groups {
		name := strings.TrimSpace(group.Name)
		where := fmt.Sprintf("builtin[%d]", index)
		if name == "" {
			return fmt.Errorf("%s: name is empty", where)
		}
		if name != group.Name {
			return fmt.Errorf("%s: name %q must not have surrounding whitespace", where, group.Name)
		}
		if previous, duplicate := seen[strings.ToLower(name)]; duplicate {
			return fmt.Errorf("%s: duplicate group name %q (already at builtin[%d])", where, name, previous)
		}
		seen[strings.ToLower(name)] = index
		if len(group.Grants) == 0 {
			return fmt.Errorf("%s (%s): grants are empty", where, name)
		}
		grantSeen := make(map[string]bool, len(group.Grants))
		for grantIndex, grant := range group.Grants {
			if err := ValidateGrant(grant); err != nil {
				return fmt.Errorf("%s (%s) grant[%d]: %w", where, name, grantIndex, err)
			}
			tuple := grant.Key + "\x00" + string(grant.Scope)
			if grantSeen[tuple] {
				return fmt.Errorf("%s (%s) grant[%d]: duplicate %q at scope %q", where, name, grantIndex, grant.Key, grant.Scope)
			}
			grantSeen[tuple] = true
		}
	}
	return nil
}
