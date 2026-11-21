package access

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Permission keys. They are the only permission vocabulary in the product:
// an HTTP route, tool, CLI subcommand or UI action names one of these keys and
// nothing else (design §3.1, L7). Keys are coarse `domain.action` names with a
// small high-risk refinement in the secret/plugin/account domains.
const (
	// Sessions.
	KeySessionRead   = "session.read"
	KeySessionWrite  = "session.write"
	KeySessionDelete = "session.delete"
	// KeySessionShare is separate from session.write so an editor cannot
	// re-share a session (widening access); sharing needs the owner relation.
	KeySessionShare = "session.share"

	// Roles and profiles. Role is identity/soul only and never grants a
	// permission; these keys govern editing the documents, not authority.
	KeyRoleRead      = "role.read"
	KeyRoleManage    = "role.manage"
	KeyProfileRead   = "profile.read"
	KeyProfileManage = "profile.manage"

	// Bots.
	KeyBotRead   = "bot.read"
	KeyBotCreate = "bot.create"
	KeyBotManage = "bot.manage"
	KeyBotBind   = "bot.bind"
	// KeyBotInvoke is the capability-authorization boundary (L2/L13): the
	// caller may drive the Bot, which then acts with its own permissions.
	KeyBotInvoke = "bot.invoke"

	// Team bots.
	KeyTeamBotRead   = "team_bot.read"
	KeyTeamBotManage = "team_bot.manage"

	// Triggers (scheduled/webhook/unattended entry points).
	KeyTriggerRead   = "trigger.read"
	KeyTriggerManage = "trigger.manage"

	// Plugins. The lifecycle actions are the high-risk refinement; there is no
	// coarse plugin.install/uninstall key that could bypass them.
	KeyPluginRead               = "plugin.read"
	KeyPluginLifecycleInstall   = "plugin.lifecycle.install"
	KeyPluginLifecycleEnable    = "plugin.lifecycle.enable"
	KeyPluginLifecycleDisable   = "plugin.lifecycle.disable"
	KeyPluginLifecycleUninstall = "plugin.lifecycle.uninstall"

	// Model configuration.
	KeyModelRead      = "model.read"
	KeyModelConfigure = "model.configure"

	// Secrets. Reads are split so metadata can be shown without exposing the
	// value; there is no coarse secret.read that would include both.
	KeySecretReadMetadata = "secret.read.metadata"
	KeySecretReadValue    = "secret.read.value"
	KeySecretWrite        = "secret.write"

	// Accounts. Credential reset and permission grant are separated from
	// ordinary account edits because both are privilege operations.
	KeyAccountRead            = "account.read"
	KeyAccountManage          = "account.manage"
	KeyAccountCredentialReset = "account.credential.reset"
	KeyAccountPermissionGrant = "account.permission.grant"

	// Permission groups.
	KeyPermissionRead   = "permission.read"
	KeyPermissionManage = "permission.manage"

	// Audit trail.
	KeyAuditRead = "audit.read"
)

// Risk classifies how dangerous a key is. The UI uses it to add friction to a
// grant; the engine itself treats every key equally.
type Risk string

// Risk levels.
const (
	RiskNormal   Risk = "normal"
	RiskHigh     Risk = "high"
	RiskCritical Risk = "critical"
)

// ValidRisk reports whether risk is a known risk level.
func ValidRisk(risk Risk) bool {
	switch risk {
	case RiskNormal, RiskHigh, RiskCritical:
		return true
	}
	return false
}

// KeySpec is the metadata of one permission key. It is the single source of
// truth for the key's domain, display name, risk, required resource relation
// and the scopes a grant may use. A4 builds UI matrices and route tables from
// this, never from a second list.
type KeySpec struct {
	// Key is the `domain.action` token.
	Key string
	// Domain is the first key segment.
	Domain string
	// DisplayName is the human-readable label.
	DisplayName string
	// Risk is normal, high or critical.
	Risk Risk
	// Relation is the resource relation an action requires; RelationNone means
	// the key is a tenant-level action with no per-resource relation.
	Relation Relation
	// AllowedScopes is the closed set of scopes a grant for this key may use.
	AllowedScopes []Scope
}

// RequiresRelation reports whether the key is resource-scoped.
func (s KeySpec) RequiresRelation() bool { return s.Relation != RelationNone }

// AllowsScope reports whether a grant for this key may use scope.
func (s KeySpec) AllowsScope(scope Scope) bool {
	for _, allowed := range s.AllowedScopes {
		if allowed == scope {
			return true
		}
	}
	return false
}

func (s KeySpec) clone() KeySpec {
	s.AllowedScopes = append([]Scope(nil), s.AllowedScopes...)
	return s
}

// ErrUnknownKey marks a permission key that is not in the registry.
var ErrUnknownKey = errors.New("access: unknown permission key")

// permissionKeyPattern is the key grammar: lowercase dot-separated segments,
// at least domain.action. It matches internal/botteam's structural check; the
// registry is the authority on which keys exist.
var permissionKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// resourceScopes is the scope set of a key that is checked against a resource
// relation.
var resourceScopes = []Scope{ScopeOwn, ScopeTeam, ScopeTenant, ScopeAll}

// tenantScopes is the scope set of a tenant-level key. Such a key has no
// resource to establish ownership or team reach, so own/team would be
// meaningless; the self-check rejects them.
var tenantScopes = []Scope{ScopeTenant, ScopeAll}

// key builds one catalog entry, deriving the domain from the key's first
// segment and copying the scope slice so no caller can mutate the catalog.
func key(name, displayName string, risk Risk, relation Relation, scopes ...Scope) KeySpec {
	domain := name
	if index := strings.IndexByte(name, '.'); index >= 0 {
		domain = name[:index]
	}
	return KeySpec{
		Key:           name,
		Domain:        domain,
		DisplayName:   displayName,
		Risk:          risk,
		Relation:      relation,
		AllowedScopes: append([]Scope(nil), scopes...),
	}
}

// keyCatalog is the initial permission catalog. It is ordered by domain for
// reviewability; All() sorts defensively.
//
// The catalog covers the acceptance surface: sessions, bots, team bots,
// triggers, plugins, models, secrets, accounts, permission groups and audit.
// It deliberately has no coarse secret.read or plugin.install/uninstall: the
// refined keys are the enforcement point, so a UI cannot grant a weaker key
// and then reach the stronger action.
var keyCatalog = []KeySpec{
	// Sessions.
	key(KeySessionRead, "Read sessions", RiskNormal, RelationForAction(ActionRead), resourceScopes...),
	key(KeySessionWrite, "Write sessions", RiskNormal, RelationForAction(ActionWrite), resourceScopes...),
	key(KeySessionDelete, "Delete sessions", RiskHigh, RelationForAction(ActionDelete), resourceScopes...),
	key(KeySessionShare, "Share sessions", RiskHigh, RelationForAction(ActionManage), resourceScopes...),

	// Roles and profiles.
	key(KeyRoleRead, "Read roles", RiskNormal, RelationNone, tenantScopes...),
	key(KeyRoleManage, "Manage roles", RiskHigh, RelationNone, tenantScopes...),
	key(KeyProfileRead, "Read profiles", RiskNormal, RelationNone, tenantScopes...),
	key(KeyProfileManage, "Manage profiles", RiskHigh, RelationNone, tenantScopes...),

	// Bots.
	key(KeyBotRead, "Read bots", RiskNormal, RelationForAction(ActionRead), resourceScopes...),
	key(KeyBotCreate, "Create bots", RiskNormal, RelationForAction(ActionCreate), tenantScopes...),
	key(KeyBotManage, "Manage bots", RiskHigh, RelationForAction(ActionManage), resourceScopes...),
	key(KeyBotBind, "Bind bot profile and harness", RiskHigh, RelationForAction(ActionManage), resourceScopes...),
	key(KeyBotInvoke, "Invoke bots", RiskCritical, RelationForAction(ActionInvoke), resourceScopes...),

	// Team bots. A TeamBot is a resource, never a permission subject (L12).
	key(KeyTeamBotRead, "Read team bots", RiskNormal, RelationForAction(ActionRead), resourceScopes...),
	key(KeyTeamBotManage, "Manage team bots", RiskHigh, RelationForAction(ActionManage), resourceScopes...),

	// Triggers.
	key(KeyTriggerRead, "Read triggers", RiskNormal, RelationForAction(ActionRead), resourceScopes...),
	key(KeyTriggerManage, "Manage triggers", RiskHigh, RelationForAction(ActionManage), resourceScopes...),

	// Plugins.
	key(KeyPluginRead, "Read plugins", RiskNormal, RelationNone, tenantScopes...),
	key(KeyPluginLifecycleInstall, "Install plugins", RiskCritical, RelationNone, tenantScopes...),
	key(KeyPluginLifecycleEnable, "Enable plugins", RiskCritical, RelationNone, tenantScopes...),
	key(KeyPluginLifecycleDisable, "Disable plugins", RiskCritical, RelationNone, tenantScopes...),
	key(KeyPluginLifecycleUninstall, "Uninstall plugins", RiskCritical, RelationNone, tenantScopes...),

	// Model configuration.
	key(KeyModelRead, "Read model configuration", RiskNormal, RelationNone, tenantScopes...),
	key(KeyModelConfigure, "Configure models", RiskHigh, RelationNone, tenantScopes...),

	// Secrets.
	key(KeySecretReadMetadata, "Read secret metadata", RiskNormal, RelationNone, tenantScopes...),
	key(KeySecretReadValue, "Read secret values", RiskCritical, RelationNone, tenantScopes...),
	key(KeySecretWrite, "Write secrets", RiskHigh, RelationNone, tenantScopes...),

	// Accounts.
	key(KeyAccountRead, "Read accounts", RiskNormal, RelationNone, tenantScopes...),
	key(KeyAccountManage, "Manage accounts", RiskHigh, RelationNone, tenantScopes...),
	key(KeyAccountCredentialReset, "Reset account credentials", RiskCritical, RelationNone, tenantScopes...),
	key(KeyAccountPermissionGrant, "Grant account permissions", RiskCritical, RelationNone, tenantScopes...),

	// Permission groups.
	key(KeyPermissionRead, "Read permission groups", RiskNormal, RelationNone, tenantScopes...),
	key(KeyPermissionManage, "Manage permission groups", RiskHigh, RelationNone, tenantScopes...),

	// Audit.
	key(KeyAuditRead, "Read the audit trail", RiskHigh, RelationNone, tenantScopes...),
}

// registry is the key lookup index. It is built once from keyCatalog.
var registry = indexCatalog(keyCatalog)

func indexCatalog(specs []KeySpec) map[string]KeySpec {
	byKey := make(map[string]KeySpec, len(specs))
	for _, spec := range specs {
		byKey[spec.Key] = spec
	}
	return byKey
}

func init() {
	if err := SelfCheck(); err != nil {
		panic("access: permission registry self-check failed: " + err.Error())
	}
}

// Lookup returns the metadata of one key. The returned spec owns its scope
// slice, so a caller cannot mutate the registry.
func Lookup(key string) (KeySpec, bool) {
	spec, ok := registry[key]
	if !ok {
		return KeySpec{}, false
	}
	return spec.clone(), true
}

// All returns every key's metadata sorted by domain then key. The result is a
// copy.
func All() []KeySpec {
	out := make([]KeySpec, len(keyCatalog))
	for index, spec := range keyCatalog {
		out[index] = spec.clone()
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Domain != out[j].Domain {
			return out[i].Domain < out[j].Domain
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Keys returns every permission key sorted by domain then key.
func Keys() []string {
	specs := All()
	out := make([]string, len(specs))
	for index, spec := range specs {
		out[index] = spec.Key
	}
	return out
}

// ValidateKey reports whether key exists in the registry. An unknown key is an
// error; there is no wildcard and no implicit key.
func ValidateKey(key string) error {
	if _, ok := registry[key]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownKey, key)
	}
	return nil
}

// SelfCheck validates the registry's invariants: a non-empty catalog, unique
// keys, legal names, metadata present, and scopes consistent with whether the
// key is resource-scoped. It runs at package init; callers may also run it at
// startup for an explicit error.
func SelfCheck() error {
	if err := selfCheckCatalog(keyCatalog); err != nil {
		return err
	}
	if len(registry) != len(keyCatalog) {
		return fmt.Errorf("registry has %d keys but the catalog has %d", len(registry), len(keyCatalog))
	}
	return nil
}

// selfCheckCatalog is the testable half of SelfCheck: it accepts a synthetic
// catalog so the failure modes can be exercised without corrupting the real
// registry.
func selfCheckCatalog(specs []KeySpec) error {
	if len(specs) == 0 {
		return errors.New("permission catalog is empty")
	}
	seen := make(map[string]int, len(specs))
	for index, spec := range specs {
		where := fmt.Sprintf("catalog[%d]", index)
		switch {
		case spec.Key == "":
			return fmt.Errorf("%s: key is empty", where)
		case !permissionKeyPattern.MatchString(spec.Key):
			return fmt.Errorf("%s: key %q must be lowercase dot-separated segments (domain.action)", where, spec.Key)
		case spec.Domain == "":
			return fmt.Errorf("%s (%s): domain is empty", where, spec.Key)
		case strings.SplitN(spec.Key, ".", 2)[0] != spec.Domain:
			return fmt.Errorf("%s (%s): domain %q is not the first key segment", where, spec.Key, spec.Domain)
		case strings.TrimSpace(spec.DisplayName) == "":
			return fmt.Errorf("%s (%s): display name is empty", where, spec.Key)
		case !ValidRisk(spec.Risk):
			return fmt.Errorf("%s (%s): unknown risk %q", where, spec.Key, spec.Risk)
		case spec.Relation != RelationNone && !ValidRelation(spec.Relation):
			return fmt.Errorf("%s (%s): unknown relation %q", where, spec.Key, spec.Relation)
		case len(spec.AllowedScopes) == 0:
			return fmt.Errorf("%s (%s): allowed scopes are empty", where, spec.Key)
		}
		if previous, duplicate := seen[spec.Key]; duplicate {
			return fmt.Errorf("%s: duplicate key %q (already at catalog[%d])", where, spec.Key, previous)
		}
		seen[spec.Key] = index

		scopeSeen := make(map[Scope]bool, len(spec.AllowedScopes))
		for _, scope := range spec.AllowedScopes {
			if !ValidScope(scope) {
				return fmt.Errorf("%s (%s): unknown scope %q", where, spec.Key, scope)
			}
			if scopeSeen[scope] {
				return fmt.Errorf("%s (%s): duplicate scope %q", where, spec.Key, scope)
			}
			scopeSeen[scope] = true
		}
		if spec.Relation == RelationNone && (scopeSeen[ScopeOwn] || scopeSeen[ScopeTeam]) {
			return fmt.Errorf("%s (%s): a key without a resource relation cannot allow own/team scope", where, spec.Key)
		}
	}
	return nil
}
