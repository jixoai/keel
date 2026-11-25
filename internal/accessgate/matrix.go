// Package accessgate is the single HTTP route-permission matrix and the
// default-deny gate in front of it (change keel-bot-identity-access, A4.2a).
//
// Every /v1 route must appear here with one explicit mode:
//
//   - public:          no session is required by product policy (probes, the A2
//     auth surface, token-addressed notification links, compiled-in demo
//     assets, the MCP OAuth callback whose state token is the authority);
//   - operator:        the handler checks the deployment operator credential
//     (KEEL_ADMIN_KEY / runtime admin API);
//   - key:             a tenant-level permission key (no resource relation);
//   - held key:        a resource-scoped key at a collection/create edge where
//     no resource exists yet; any held scope passes and the created
//     resource's owner relation governs later single-resource calls;
//   - tenant key:      a resource-scoped key whose resource is tenant-global
//     (triggers); the grant must reach tenant or all scope;
//   - resource key:    a resource-scoped key plus a relation on one resource
//     identified by the declared path/query parameter.
//
// A registration that is not in this table is a boot failure (cmd/keel runs
// CheckRoutes) and a request for an unlisted /v1 path is a fail-closed 404.
// There is deliberately no "authenticated but no key" mode: every non-public
// route names a key.
package accessgate

import (
	"net/http"
	"sort"

	"github.com/jixoai/keel/internal/access"
)

// Mode is how the gate decides one route.
type Mode string

const (
	// ModePublic needs no session and no key. Use only for the exceptions the
	// change names (probes, /v1/auth/*, token-addressed links, static assets).
	ModePublic Mode = "public"
	// ModeOperator delegates to the handler's operator-credential check.
	ModeOperator Mode = "operator"
	// ModeKey requires a tenant-level permission key.
	ModeKey Mode = "key"
	// ModeHeldKey requires a resource-scoped key at any held scope; there is no
	// resource to attach a relation to yet (collection/create edge).
	ModeHeldKey Mode = "held_key"
	// ModeTenantKey requires a resource-scoped key at tenant or all scope. It is
	// for tenant-global resources that carry no relation tuples (triggers).
	ModeTenantKey Mode = "tenant_key"
	// ModeResourceKey requires a permission key plus the required relation on
	// one resource named by ResourceParam.
	ModeResourceKey Mode = "resource_key"
	// ModeDelegated marks the product subtree mount. It is validated for
	// coverage but never matched by the gate: the concrete inner route must own
	// the decision.
	ModeDelegated Mode = "delegated"
)

// ValidMode reports whether mode is one of the enumerated modes.
func ValidMode(mode Mode) bool {
	switch mode {
	case ModePublic, ModeOperator, ModeKey, ModeHeldKey, ModeTenantKey, ModeResourceKey, ModeDelegated:
		return true
	}
	return false
}

// Rule is one route declaration.
type Rule struct {
	// Method is the HTTP method, or "" for a method-less mux pattern.
	Method string
	// Pattern is the net/http ServeMux pattern path ("/v1/sessions/{id}/events").
	Pattern string
	// Mode decides the check.
	Mode Mode
	// Key is the required permission key for key/hold/tenant/resource modes.
	Key string
	// ResourceType is the access.Resource type (session, bot, team_bot, ...).
	ResourceType string
	// ResourceParam names where the resource id lives: "path:id" or
	// "query:session_id".
	ResourceParam string
	// Note explains a non-obvious classification to the next reviewer.
	Note string
}

// String renders the rule for coverage logs.
func (r Rule) String() string {
	method := r.Method
	if method == "" {
		method = "*"
	}
	return method + " " + r.Pattern
}

// route is a fluent rule constructor used by the matrix below.
func route(method, pattern string, mode Mode, key string) Rule {
	return Rule{Method: method, Pattern: pattern, Mode: mode, Key: key}
}

func resource(method, pattern, key, resourceType, param string) Rule {
	return Rule{
		Method: method, Pattern: pattern, Mode: ModeResourceKey, Key: key,
		ResourceType: resourceType, ResourceParam: param,
	}
}

// rules is the single matrix. It is ordered by route family so review can see
// a family whole; AllRules sorts defensively.
var rules = []Rule{
	// --- probes, A2 auth surface and explicit token/public exceptions -------
	route(http.MethodGet, "/v1/health", ModePublic, ""),
	route(http.MethodGet, "/v1/ready", ModePublic, ""),
	// The Prometheus scrape endpoint is not a tenant route and the A2 auth edge
	// deliberately exempts it (operator network placement is the boundary). It is
	// public by contract, not a fake operator route.
	{Method: http.MethodGet, Pattern: "/metrics", Mode: ModePublic,
		Note: "operator/Prometheus scrape contract; the auth edge exempts /metrics"},
	{Method: http.MethodGet, Pattern: "/v1/metrics", Mode: ModeOperator,
		Note: "handler enforces the KEEL_ADMIN_KEY Bearer credential (requireAdmin)"},
	route(http.MethodPost, "/v1/auth/login", ModePublic, ""),
	route(http.MethodPost, "/v1/auth/logout", ModePublic, ""),
	route(http.MethodGet, "/v1/auth/session", ModePublic, ""),
	route(http.MethodPost, "/v1/auth/password", ModePublic, ""),
	route(http.MethodPost, "/v1/auth/bootstrap", ModePublic, ""),
	// whoami is identity introspection, not a resource read: A2's own surface
	// answers it for every authenticated credential (including bot keys).
	route(http.MethodGet, "/v1/auth/whoami", ModePublic, ""),
	// The notification open/pixel links carry a single-use capability token and
	// deliberately do not require X-Tenant-ID. The unguessable token is the
	// authority: the auth edge must let these two paths through even in
	// auth=required, the handler answers 401 for a missing/invalid token, and the
	// row supplies its tenant.
	route(http.MethodGet, "/v1/notifications/{id}/open", ModePublic, ""),
	route(http.MethodGet, "/v1/notifications/{id}/pixel", ModePublic, ""),
	// Compiled-in demo tiles: fixed bytes, no tenant data.
	route(http.MethodGet, "/v1/demo/assets/{name}", ModePublic, ""),
	// MCP OAuth callback: the provider redirects the browser here; the signed
	// state is the authority, so no permission key can be required.
	route("", "/v1/mcp/callback", ModePublic, ""),
	// Product subtree mount; the concrete harnessapi route owns the decision.
	{Pattern: "/v1/", Mode: ModeDelegated, Note: "product subtree mount"},

	// --- operator surface (deployment credential checked in the handler) ----
	route(http.MethodPost, "/v1/tenants", ModeOperator, ""),
	route(http.MethodPut, "/v1/tenants/{id}/config", ModeOperator, ""),
	route(http.MethodGet, "/v1/tenants/{id}/config", ModeOperator, ""),
	route(http.MethodGet, "/v1/tenants/{id}/usage", ModeOperator, ""),
	route(http.MethodGet, "/v1/runtime/status", ModeOperator, ""),
	route(http.MethodPost, "/v1/runtime/drain", ModeOperator, ""),
	route(http.MethodPost, "/v1/mcp/servers/{name}/signin", ModeOperator, ""),
	// The legacy tenant provisioning API accepts KEEL_ADMIN_KEY or a
	// super-admin principal; the handler enforces it (A4.2b deletes the route).
	route(http.MethodPost, "/v1/admin/tenants", ModeOperator, ""),

	// --- sessions ----------------------------------------------------------
	route(http.MethodGet, "/v1/sessions", ModeHeldKey, access.KeySessionRead),
	route(http.MethodPost, "/v1/sessions", ModeHeldKey, access.KeySessionWrite),
	resource(http.MethodGet, "/v1/sessions/{id}/events", access.KeySessionRead, "session", "path:id"),
	resource(http.MethodPost, "/v1/sessions/{id}/events", access.KeySessionWrite, "session", "path:id"),
	resource(http.MethodGet, "/v1/sessions/{id}/events/stream", access.KeySessionRead, "session", "path:id"),
	resource(http.MethodPost, "/v1/sessions/{id}/input", access.KeySessionWrite, "session", "path:id"),
	resource(http.MethodPost, "/v1/sessions/{id}/retry", access.KeySessionWrite, "session", "path:id"),
	resource(http.MethodPost, "/v1/sessions/{id}/run", access.KeySessionWrite, "session", "path:id"),
	resource(http.MethodGet, "/v1/sessions/{id}/run", access.KeySessionRead, "session", "path:id"),
	resource(http.MethodGet, "/v1/sessions/{id}/run/loadout", access.KeySessionRead, "session", "path:id"),
	resource(http.MethodDelete, "/v1/sessions/{id}", access.KeySessionDelete, "session", "path:id"),
	resource(http.MethodPost, "/v1/sessions/{id}/cancel", access.KeySessionWrite, "session", "path:id"),
	// Manual checkpoint compaction (change keel-checkpoint): a write on the
	// session's context, idempotent by requestId.
	resource(http.MethodPost, "/v1/sessions/{id}/compact", access.KeySessionWrite, "session", "path:id"),
	resource(http.MethodGet, "/v1/sessions/{id}/decisions", access.KeySessionRead, "session", "path:id"),
	resource(http.MethodPost, "/v1/sessions/{id}/decisions", access.KeySessionWrite, "session", "path:id"),
	resource(http.MethodGet, "/v1/sessions/{id}/decisions/{decisionId}", access.KeySessionRead, "session", "path:id"),
	resource(http.MethodPost, "/v1/sessions/{id}/decisions/{decisionId}", access.KeySessionWrite, "session", "path:id"),
	resource(http.MethodGet, "/v1/sessions/{id}/approvals", access.KeySessionRead, "session", "path:id"),
	resource(http.MethodGet, "/v1/sessions/{id}/approvals/{decisionId}", access.KeySessionRead, "session", "path:id"),
	resource(http.MethodGet, "/v1/sessions/{id}/team-tasks", access.KeySessionRead, "session", "path:id"),
	resource(http.MethodGet, "/v1/sessions/{id}/files", access.KeySessionRead, "session", "path:id"),
	resource(http.MethodGet, "/v1/sessions/{id}/file-preview", access.KeySessionRead, "session", "path:id"),
	// The W12 product reads. Tenant-wide search is a collection read (the
	// handler filters candidates through Visible); export and deliverables are
	// one session's resource reads.
	route(http.MethodGet, "/v1/sessions/search", ModeHeldKey, access.KeySessionRead),
	resource(http.MethodGet, "/v1/sessions/{id}/export", access.KeySessionRead, "session", "path:id"),
	resource(http.MethodGet, "/v1/sessions/{id}/deliverables", access.KeySessionRead, "session", "path:id"),
	resource(http.MethodGet, "/v1/sessions/{id}/tasks", access.KeySessionRead, "session", "path:id"),
	resource(http.MethodGet, "/v1/sessions/{id}/task-panel", access.KeySessionRead, "session", "path:id"),
	// The external-run projection (change keel-oar-renderer) is one session's
	// read of its external backend's records: it carries exactly the transcript's
	// visibility, so it takes the same session.read relation as the neighboring
	// session reads (task-panel/export/file-preview) and there is no new key.
	{
		Method: http.MethodGet, Pattern: "/v1/sessions/{id}/external/projection",
		Mode: ModeResourceKey, Key: access.KeySessionRead,
		ResourceType: "session", ResourceParam: "path:id",
		Note: "read-only OAR renderer projection; same session.read relation as task-panel",
	},
	resource(http.MethodPost, "/v1/sessions/{id}/subagents", access.KeySessionWrite, "session", "path:id"),
	resource(http.MethodPost, "/v1/sessions/{id}/fork", access.KeySessionWrite, "session", "path:id"),
	resource(http.MethodPost, "/v1/sessions/{id}/resume", access.KeySessionWrite, "session", "path:id"),
	// Sharing writes/reads relation tuples. session.share requires the owner
	// relation, so an editor can never re-share (widening access). Revoke is a
	// POST with a body: the matrix stays method+pattern simple.
	resource(http.MethodGet, "/v1/sessions/{id}/shares", access.KeySessionShare, "session", "path:id"),
	resource(http.MethodPost, "/v1/sessions/{id}/shares", access.KeySessionShare, "session", "path:id"),
	resource(http.MethodPost, "/v1/sessions/{id}/shares/revoke", access.KeySessionShare, "session", "path:id"),
	route(http.MethodGet, "/v1/subagents", ModeHeldKey, access.KeySessionRead),
	resource(http.MethodGet, "/v1/subagents/{id}", access.KeySessionRead, "session", "path:id"),
	resource(http.MethodPost, "/v1/subagents/{id}/cancel", access.KeySessionWrite, "session", "query:parentSessionId"),

	// --- super-agent board (read-only projections) --------------------------
	// The queue is tenant-scoped; task.read is relation-scoped on individual
	// task cards, so the held-key mode is the same shape as the other
	// collection read edges. Writes stay in the Source/Channel and operator
	// paths, not these routes.
	route(http.MethodGet, "/v1/superagent/tickets", ModeHeldKey, access.KeyTaskRead),
	route(http.MethodGet, "/v1/superagent/attention", ModeHeldKey, access.KeyTaskRead),

	// --- realtime voice bridge ---------------------------------------------
	resource(http.MethodGet, "/v1/realtime", access.KeySessionRead, "session", "query:session_id"),
	route(http.MethodGet, "/v1/realtime/status", ModeKey, access.KeyAppRead),

	// --- external gateway sessions -----------------------------------------
	route(http.MethodPost, "/v1/external/sessions", ModeHeldKey, access.KeySessionWrite),
	route(http.MethodGet, "/v1/external/sessions", ModeHeldKey, access.KeySessionRead),
	resource(http.MethodGet, "/v1/external/sessions/{id}", access.KeySessionRead, "session", "path:id"),
	resource(http.MethodPost, "/v1/external/sessions/{id}/messages", access.KeySessionWrite, "session", "path:id"),
	resource(http.MethodPost, "/v1/external/sessions/{id}/cancel", access.KeySessionWrite, "session", "path:id"),
	resource(http.MethodPost, "/v1/external/sessions/{id}/close", access.KeySessionWrite, "session", "path:id"),
	route(http.MethodGet, "/v1/external/backends", ModeHeldKey, access.KeySessionRead),

	// --- decisions and approvals (session subresources) --------------------
	route(http.MethodPost, "/v1/decisions/ask", ModeKey, access.KeyModelInvoke),
	// Plugin approvals are plugin actions; listing them is plugin.read.
	route(http.MethodGet, "/v1/plugin-approvals/{decisionId}", ModeKey, access.KeyPluginInvoke),
	route(http.MethodPost, "/v1/plugin-approvals/{decisionId}", ModeKey, access.KeyPluginInvoke),

	// --- identity: accounts -------------------------------------------------
	route(http.MethodGet, "/v1/accounts", ModeKey, access.KeyAccountRead),
	route(http.MethodPost, "/v1/accounts", ModeKey, access.KeyAccountManage),
	route(http.MethodGet, "/v1/accounts/{id}", ModeKey, access.KeyAccountRead),
	route(http.MethodPost, "/v1/accounts/{id}/disable", ModeKey, access.KeyAccountManage),
	route(http.MethodPost, "/v1/accounts/{id}/password-reset", ModeKey, access.KeyAccountCredentialReset),
	route(http.MethodGet, "/v1/accounts/{id}/groups", ModeKey, access.KeyAccountRead),
	route(http.MethodPost, "/v1/accounts/{id}/groups", ModeKey, access.KeyAccountPermissionGrant),
	route(http.MethodDelete, "/v1/accounts/{id}/groups/{groupId}", ModeKey, access.KeyAccountPermissionGrant),

	// --- identity: roles and profiles --------------------------------------
	// Both are tenant-level documents. Role never grants authority; Profile is
	// the Account+Role binding a Bot runs under. A profile rebind moves Bot
	// owner relations in the same store transaction.
	route(http.MethodGet, "/v1/roles", ModeKey, access.KeyRoleRead),
	route(http.MethodPost, "/v1/roles", ModeKey, access.KeyRoleManage),
	route(http.MethodGet, "/v1/roles/{id}", ModeKey, access.KeyRoleRead),
	route(http.MethodPatch, "/v1/roles/{id}", ModeKey, access.KeyRoleManage),
	route(http.MethodDelete, "/v1/roles/{id}", ModeKey, access.KeyRoleManage),
	route(http.MethodGet, "/v1/profiles", ModeKey, access.KeyProfileRead),
	route(http.MethodPost, "/v1/profiles", ModeKey, access.KeyProfileManage),
	route(http.MethodGet, "/v1/profiles/{id}", ModeKey, access.KeyProfileRead),
	route(http.MethodPatch, "/v1/profiles/{id}", ModeKey, access.KeyProfileManage),
	route(http.MethodDelete, "/v1/profiles/{id}", ModeKey, access.KeyProfileManage),

	// --- identity: permission groups ---------------------------------------
	route(http.MethodGet, "/v1/permission-groups", ModeKey, access.KeyPermissionRead),
	route(http.MethodPost, "/v1/permission-groups", ModeKey, access.KeyPermissionManage),
	route(http.MethodGet, "/v1/permission-groups/{id}", ModeKey, access.KeyPermissionRead),
	route(http.MethodPatch, "/v1/permission-groups/{id}", ModeKey, access.KeyPermissionManage),
	route(http.MethodDelete, "/v1/permission-groups/{id}", ModeKey, access.KeyPermissionManage),
	route(http.MethodPost, "/v1/permission-groups/{id}/members", ModeKey, access.KeyAccountPermissionGrant),
	route(http.MethodDelete, "/v1/permission-groups/{id}/members/{accountId}", ModeKey, access.KeyAccountPermissionGrant),
	route(http.MethodPost, "/v1/permission-groups/{id}/grants", ModeKey, access.KeyPermissionManage),
	route(http.MethodDelete, "/v1/permission-groups/{id}/grants/{key}", ModeKey, access.KeyPermissionManage),

	// --- identity: bots -----------------------------------------------------
	route(http.MethodGet, "/v1/bots", ModeHeldKey, access.KeyBotRead),
	route(http.MethodPost, "/v1/bots", ModeKey, access.KeyBotCreate),
	resource(http.MethodGet, "/v1/bots/{id}", access.KeyBotRead, "bot", "path:id"),
	resource(http.MethodPatch, "/v1/bots/{id}", access.KeyBotManage, "bot", "path:id"),
	resource(http.MethodPost, "/v1/bots/{id}/archive", access.KeyBotManage, "bot", "path:id"),
	resource(http.MethodPost, "/v1/bots/{id}/invoke", access.KeyBotInvoke, "bot", "path:id"),
	resource(http.MethodGet, "/v1/bots/{id}/keys", access.KeyBotManage, "bot", "path:id"),
	resource(http.MethodPost, "/v1/bots/{id}/keys", access.KeyBotManage, "bot", "path:id"),
	resource(http.MethodDelete, "/v1/bots/{id}/keys/{keyId}", access.KeyBotManage, "bot", "path:id"),
	resource(http.MethodGet, "/v1/bots/{id}/shares", access.KeyBotManage, "bot", "path:id"),
	resource(http.MethodPost, "/v1/bots/{id}/shares", access.KeyBotManage, "bot", "path:id"),
	resource(http.MethodPost, "/v1/bots/{id}/shares/revoke", access.KeyBotManage, "bot", "path:id"),

	// --- identity: team bots -------------------------------------------------
	route(http.MethodGet, "/v1/team-bots", ModeHeldKey, access.KeyTeamBotRead),
	route(http.MethodPost, "/v1/team-bots", ModeHeldKey, access.KeyTeamBotManage),
	resource(http.MethodGet, "/v1/team-bots/{id}", access.KeyTeamBotRead, "team_bot", "path:id"),
	resource(http.MethodPatch, "/v1/team-bots/{id}", access.KeyTeamBotManage, "team_bot", "path:id"),
	resource(http.MethodPost, "/v1/team-bots/{id}/archive", access.KeyTeamBotManage, "team_bot", "path:id"),
	resource(http.MethodPost, "/v1/team-bots/{id}/roster", access.KeyTeamBotManage, "team_bot", "path:id"),
	resource(http.MethodPost, "/v1/team-bots/{id}/start", access.KeyTeamBotManage, "team_bot", "path:id"),
	resource(http.MethodGet, "/v1/team-bots/{id}/shares", access.KeyTeamBotManage, "team_bot", "path:id"),
	resource(http.MethodPost, "/v1/team-bots/{id}/shares", access.KeyTeamBotManage, "team_bot", "path:id"),
	resource(http.MethodPost, "/v1/team-bots/{id}/shares/revoke", access.KeyTeamBotManage, "team_bot", "path:id"),

	// --- audit trail ---------------------------------------------------------
	// The ledger is tenant-global (no relation tuples). The handler additionally
	// checks ?tenantId=: another tenant requires the deployment super admin.
	route(http.MethodGet, "/v1/audit/events", ModeKey, access.KeyAuditRead),
	route(http.MethodGet, "/v1/audit/events/{id}", ModeKey, access.KeyAuditRead),

	// --- triggers ------------------------------------------------------------
	// Triggers are tenant-global runtime registrations with no relation tuples.
	// The key must reach tenant scope and the handler filters by tenant.
	route(http.MethodGet, "/v1/triggers", ModeTenantKey, access.KeyTriggerRead),
	route(http.MethodPost, "/v1/triggers/{id}/fire", ModeTenantKey, access.KeyTriggerManage),

	// --- settings: models, providers and secrets ---------------------------
	route(http.MethodGet, "/v1/providers", ModeKey, access.KeyModelRead),
	route(http.MethodPost, "/v1/providers/{id}/test", ModeKey, access.KeyModelConfigure),
	route(http.MethodGet, "/v1/models", ModeKey, access.KeyModelRead),
	route(http.MethodGet, "/v1/settings/models", ModeKey, access.KeyModelRead),
	route(http.MethodPut, "/v1/settings/models", ModeKey, access.KeyModelConfigure),
	route(http.MethodDelete, "/v1/settings/models", ModeKey, access.KeyModelConfigure),
	route(http.MethodGet, "/v1/settings/secrets", ModeKey, access.KeySecretReadMetadata),
	route(http.MethodPut, "/v1/settings/secrets/{name}", ModeKey, access.KeySecretWrite),
	route(http.MethodDelete, "/v1/settings/secrets/{name}", ModeKey, access.KeySecretWrite),
	// The provider editor's direct-entry path stores the value in the same tenant
	// secret store as PUT /v1/settings/secrets/{name} (the models document is
	// saved separately), so it carries the secret-write authority rather than the
	// model-configure one.
	route(http.MethodPut, "/v1/settings/providers/{id}/credential", ModeKey, access.KeySecretWrite),

	// --- settings: skills ----------------------------------------------------
	route(http.MethodGet, "/v1/settings/skills", ModeKey, access.KeySkillRead),
	route(http.MethodPost, "/v1/settings/skills", ModeKey, access.KeySkillManage),
	route(http.MethodGet, "/v1/settings/skills/sources", ModeKey, access.KeySkillRead),
	route(http.MethodPost, "/v1/settings/skills/sources", ModeKey, access.KeySkillManage),
	route(http.MethodDelete, "/v1/settings/skills/sources/{id}", ModeKey, access.KeySkillManage),
	route(http.MethodGet, "/v1/settings/skills/{name}", ModeKey, access.KeySkillRead),
	route(http.MethodDelete, "/v1/settings/skills/{name}", ModeKey, access.KeySkillManage),
	route(http.MethodPost, "/v1/settings/skills/{name}/update", ModeKey, access.KeySkillManage),
	route(http.MethodPost, "/v1/settings/skills/{name}/pin", ModeKey, access.KeySkillManage),
	route(http.MethodPost, "/v1/settings/skills/{name}/unpin", ModeKey, access.KeySkillManage),

	// --- plugins -------------------------------------------------------------
	route(http.MethodGet, "/v1/plugins", ModeKey, access.KeyPluginRead),
	route(http.MethodGet, "/v1/plugins/audit", ModeKey, access.KeyAuditRead),
	route(http.MethodGet, "/v1/settings/plugins", ModeKey, access.KeyPluginRead),
	route(http.MethodPost, "/v1/plugins/install", ModeKey, access.KeyPluginLifecycleInstall),
	route(http.MethodPost, "/v1/plugins/{id}/enable", ModeKey, access.KeyPluginLifecycleEnable),
	route(http.MethodPost, "/v1/plugins/{id}/disable", ModeKey, access.KeyPluginLifecycleDisable),
	route(http.MethodPost, "/v1/plugins/{id}/uninstall", ModeKey, access.KeyPluginLifecycleUninstall),
	// An update replaces the installed code; the install authority is the closest
	// registry key (there is no coarse plugin.write).
	route(http.MethodPost, "/v1/plugins/{id}/update", ModeKey, access.KeyPluginLifecycleInstall),
	route(http.MethodGet, "/v1/plugins/{id}/bridge/state", ModeKey, access.KeyPluginRead),
	route(http.MethodGet, "/v1/plugins/{id}/bridge/subscribe", ModeKey, access.KeyPluginRead),
	route(http.MethodGet, "/v1/plugins/{id}/bridge/approvals/{decisionId}", ModeKey, access.KeyPluginInvoke),
	route(http.MethodPost, "/v1/plugins/{id}/bridge/call", ModeKey, access.KeyPluginInvoke),
	route(http.MethodPost, "/v1/plugins/{id}/bridge/approve", ModeKey, access.KeyPluginInvoke),
	route(http.MethodPost, "/v1/plugins/{id}/bridge/approvals/{decisionId}", ModeKey, access.KeyPluginInvoke),

	// --- MCP servers and apps ------------------------------------------------
	route(http.MethodGet, "/v1/mcp/servers", ModeKey, access.KeyMCPRead),
	route(http.MethodGet, "/v1/mcp/apps", ModeKey, access.KeyMCPRead),
	route(http.MethodGet, "/v1/mcp/apps/audit", ModeKey, access.KeyAuditRead),
	route(http.MethodPost, "/v1/mcp/apps/audit", ModeKey, access.KeyMCPUse),
	route(http.MethodGet, "/v1/mcp/apps/{appId}/view", ModeKey, access.KeyMCPRead),
	route(http.MethodPost, "/v1/mcp/apps/{appId}/tools/call", ModeKey, access.KeyMCPUse),
	route(http.MethodPost, "/v1/mcp/apps/{appId}/resources/read", ModeKey, access.KeyMCPUse),
	route(http.MethodPost, "/v1/mcp/apps/{appId}/ui/message", ModeKey, access.KeyMCPUse),
	route(http.MethodPost, "/v1/mcp/apps/{appId}/ui/open-link", ModeKey, access.KeyMCPUse),
	route(http.MethodPost, "/v1/mcp/apps/{appId}/context", ModeKey, access.KeyMCPUse),

	// --- notifications -------------------------------------------------------
	// The key is tenant-level; the handler narrows the list to sessions the
	// caller may read (and ack to sessions the caller may write).
	route(http.MethodGet, "/v1/notifications", ModeKey, access.KeyNotificationRead),
	route(http.MethodPost, "/v1/notifications/{id}/ack", ModeKey, access.KeyNotificationWrite),

	// --- files ---------------------------------------------------------------
	route(http.MethodPost, "/v1/files", ModeKey, access.KeyFileWrite),
	route(http.MethodGet, "/v1/files/{id}/meta", ModeKey, access.KeyFileRead),
	route(http.MethodGet, "/v1/files/{id}/thumbnail", ModeKey, access.KeyFileRead),
	route(http.MethodGet, "/v1/files/{id}", ModeKey, access.KeyFileRead),

	// --- media gateway -------------------------------------------------------
	route(http.MethodGet, "/v1/media/capabilities", ModeKey, access.KeyMediaRead),
	route(http.MethodPost, "/v1/media/images/generate", ModeKey, access.KeyMediaInvoke),
	route(http.MethodPost, "/v1/media/images/edit", ModeKey, access.KeyMediaInvoke),
	route(http.MethodPost, "/v1/media/decision", ModeKey, access.KeyMediaInvoke),
	route(http.MethodPost, "/v1/media/llm/chat", ModeKey, access.KeyMediaInvoke),
	route(http.MethodPost, "/v1/media/audio/speech", ModeKey, access.KeyMediaInvoke),
	route(http.MethodPost, "/v1/media/audio/transcriptions", ModeKey, access.KeyMediaInvoke),

	// --- application discovery and quota ------------------------------------
	route(http.MethodGet, "/v1/capabilities", ModeKey, access.KeyAppRead),
	route(http.MethodGet, "/v1/quota", ModeKey, access.KeyAppRead),
	// The identity invalidation stream: the same tenant-level discovery key as
	// capabilities. Facts are projection names and times, scoped by the hub to
	// the caller's tenant.
	route(http.MethodGet, "/v1/identity/events", ModeKey, access.KeyAppRead),
}

// AuthRoutes returns the A2 auth patterns the host mounts outside the runtime
// server. They are public by design and included in the startup coverage check.
func AuthRoutes() []string {
	return []string{
		"POST /v1/auth/login",
		"POST /v1/auth/logout",
		"GET /v1/auth/session",
		"POST /v1/auth/password",
		"POST /v1/auth/bootstrap",
	}
}

// AllRules returns a copy of the matrix sorted for review.
func AllRules() []Rule {
	out := append([]Rule(nil), rules...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pattern != out[j].Pattern {
			return out[i].Pattern < out[j].Pattern
		}
		return out[i].Method < out[j].Method
	})
	return out
}
