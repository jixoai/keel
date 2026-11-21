package access

// Reason is the machine-readable half of an authorization decision. A 403
// handler maps it to copy; an audit row stores it verbatim. The values are part
// of the contract: do not rename them.
type Reason string

const (
	// ReasonAllow is the only non-denial.
	ReasonAllow Reason = "allow"
	// ReasonUnknownKey means the action key is not in the registry.
	ReasonUnknownKey Reason = "unknown_key"
	// ReasonMissingKey means a Bot's Harness narrowing does not include a key
	// the account itself holds (L11).
	ReasonMissingKey Reason = "missing_key"
	// ReasonMissingGrant means the principal has no grant for the key, or its
	// scope does not reach the resource.
	ReasonMissingGrant Reason = "missing_grant"
	// ReasonMissingRelation means the key grant is present but the required
	// resource relation is absent or not reachable at the granted scope.
	ReasonMissingRelation Reason = "missing_relation"
	// ReasonUnresolvedSubject means a relation tuple named a group or team bot
	// that could not be resolved. The engine fails closed instead of guessing.
	ReasonUnresolvedSubject Reason = "unresolved_subject"
	// ReasonInvalidPrincipal means the principal identity is incomplete, the
	// account is disabled, or super admin authority was attached to a Bot.
	ReasonInvalidPrincipal Reason = "invalid_principal"
	// ReasonInvalidScope means a grant or proposed grant uses an unknown or
	// disallowed scope. It is a write-side validation reason, not a resource
	// denial.
	ReasonInvalidScope Reason = "invalid_scope"
)

// String returns the stable wire value.
func (r Reason) String() string { return string(r) }
