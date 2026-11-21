// Access audit vocabulary (change keel-bot-identity-access, A4.1).
//
// The A2 auth store owns auth_events and the login/bootstrap/bot-key kinds.
// A4.1 adds the permission and relation facts to the same append-only table so
// "who granted what, to whom, and who was denied" is one queryable stream. The
// kind strings are contract values: a report/UI may match on them.
//
// Detail maps never carry a credential: no password, hash, token, plaintext key
// or secret value. They carry ids, permission keys, scopes and reasons only.
package store

// Permission and relation audit kinds. They are recorded through
// RecordAuthEvent like every other auth fact.
const (
	// AuthEventPermissionGrant records a grant write: a group grant, a direct
	// account grant, or an account joining a permission group.
	AuthEventPermissionGrant = "permission.grant"
	// AuthEventPermissionRevoke records the inverse of a grant write.
	AuthEventPermissionRevoke = "permission.revoke"
	// AuthEventPermissionDenied records an authorization denial with its
	// structured reason (missing_grant, missing_relation, ...).
	AuthEventPermissionDenied = "permission.denied"
	// AuthEventRelationChanged records a resource relation tuple write/delete.
	AuthEventRelationChanged = "relation.changed"
)
