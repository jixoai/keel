package access

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// newTestAuthorizer builds the pipeline with the fake sources.
func newTestAuthorizer(t *testing.T, grants *fakeGrantSource, relations *fakeRelationSource) *Authorizer {
	t.Helper()
	authorizer, err := NewAuthorizer(grants, relations)
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}
	return authorizer
}

func TestNewAuthorizerRequiresSources(t *testing.T) {
	relations := newFakeRelationSource()
	if _, err := NewAuthorizer(nil, relations); err == nil {
		t.Fatal("NewAuthorizer accepted a nil grant source")
	}
	if _, err := NewAuthorizer(&fakeGrantSource{}, nil); err == nil {
		t.Fatal("NewAuthorizer accepted a nil relation source")
	}
}

func TestAuthorizeUnknownKeyDoesNotConsultSources(t *testing.T) {
	grants := &fakeGrantSource{}
	relations := newFakeRelationSource()
	authorizer := newTestAuthorizer(t, grants, relations)

	decision, err := authorizer.Authorize(context.Background(),
		Principal{TenantID: tenantOne, AccountID: accountAlice},
		"session.fly", resourceOf(tenantOne, "session", "s-1"))
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if decision.Allow || decision.Reason != ReasonUnknownKey {
		t.Fatalf("decision = %s, want unknown_key", decision)
	}
	if grants.directCalls != 0 || grants.groupCalls != 0 || relations.relationsCalls != 0 {
		t.Fatal("unknown key consulted a source")
	}
}

func TestAuthorizeInvalidPrincipalFailsClosed(t *testing.T) {
	grants := &fakeGrantSource{}
	relations := newFakeRelationSource()
	authorizer := newTestAuthorizer(t, grants, relations)

	cases := []struct {
		name      string
		principal Principal
	}{
		{name: "missing tenant", principal: Principal{AccountID: accountAlice}},
		{name: "missing account", principal: Principal{TenantID: tenantOne}},
		{name: "disabled", principal: Principal{TenantID: tenantOne, AccountID: accountAlice, Disabled: true}},
		{name: "bot super admin", principal: Principal{TenantID: tenantOne, AccountID: accountAlice, BotID: botX, IsSuperAdmin: true}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			decision, err := authorizer.Authorize(context.Background(), testCase.principal,
				KeySessionRead, resourceOf(tenantOne, "session", "s-1"))
			if err != nil {
				t.Fatalf("Authorize: %v", err)
			}
			if decision.Allow || decision.Reason != ReasonInvalidPrincipal {
				t.Fatalf("decision = %s, want invalid_principal", decision)
			}
		})
	}
	if grants.directCalls != 0 {
		t.Fatal("invalid principal consulted the grant source")
	}
}

func TestAuthorizeSuperAdminShortCircuits(t *testing.T) {
	grants := &fakeGrantSource{}
	relations := newFakeRelationSource()
	authorizer := newTestAuthorizer(t, grants, relations)
	superAdmin := Principal{TenantID: tenantOne, AccountID: accountAlice, IsSuperAdmin: true}

	// A super admin does not need a grant or a relation, and may cross tenants.
	decision, err := authorizer.Authorize(context.Background(), superAdmin,
		KeyBotInvoke, resourceOf(tenantTwo, "bot", botX))
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !decision.Allow || decision.Scope != ScopeAll || decision.Reason != ReasonAllow {
		t.Fatalf("decision = %s, want allow at scope all", decision)
	}
	if grants.directCalls != 0 || relations.relationsCalls != 0 {
		t.Fatal("super admin consulted a source")
	}

	// A super admin still cannot use a key that is not in the registry.
	decision, err = authorizer.Authorize(context.Background(), superAdmin, "session.fly", Resource{})
	if err != nil {
		t.Fatalf("Authorize(unknown key): %v", err)
	}
	if decision.Allow || decision.Reason != ReasonUnknownKey {
		t.Fatalf("unknown-key decision = %s, want unknown_key", decision)
	}
}

func TestAuthorizeTenantLevelKey(t *testing.T) {
	grants := &fakeGrantSource{direct: map[uuid.UUID][]Grant{
		accountAlice: {grantOf(KeyAccountManage, ScopeTenant)},
	}}
	relations := newFakeRelationSource()
	authorizer := newTestAuthorizer(t, grants, relations)

	decision, err := authorizer.Authorize(context.Background(),
		Principal{TenantID: tenantOne, AccountID: accountAlice},
		KeyAccountManage, Resource{})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !decision.Allow || decision.Scope != ScopeTenant {
		t.Fatalf("decision = %s, want allow at tenant scope", decision)
	}
	if relations.relationsCalls != 0 {
		t.Fatal("tenant-level key consulted the relation source")
	}
}

func TestAuthorizeMissingGrant(t *testing.T) {
	authorizer := newTestAuthorizer(t, &fakeGrantSource{}, newFakeRelationSource())
	decision, err := authorizer.Authorize(context.Background(),
		Principal{TenantID: tenantOne, AccountID: accountAlice},
		KeySessionRead, resourceOf(tenantOne, "session", "s-1"))
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if decision.Allow || decision.Reason != ReasonMissingGrant {
		t.Fatalf("decision = %s, want missing_grant", decision)
	}
}

func TestAuthorizeBotHarnessNarrows(t *testing.T) {
	grants := &fakeGrantSource{direct: map[uuid.UUID][]Grant{
		accountAlice: {grantOf(KeySessionRead, ScopeTenant)},
	}}
	relations := newFakeRelationSource()
	resource := resourceOf(tenantOne, "session", "s-1")
	relations.put(resource, RelationViewer, SubjectTypeAccount, accountAlice.String())
	authorizer := newTestAuthorizer(t, grants, relations)

	bot := Principal{
		TenantID: tenantOne, AccountID: accountAlice, BotID: botX,
		HarnessPermissions: []string{KeySessionWrite},
	}
	decision, err := authorizer.Authorize(context.Background(), bot, KeySessionRead, resource)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if decision.Allow || decision.Reason != ReasonMissingKey {
		t.Fatalf("decision = %s, want missing_key", decision)
	}

	bot.HarnessPermissions = []string{KeySessionRead}
	decision, err = authorizer.Authorize(context.Background(), bot, KeySessionRead, resource)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !decision.Allow {
		t.Fatalf("decision = %s, want allow", decision)
	}
}

func TestAuthorizeResourceRelations(t *testing.T) {
	resource := resourceOf(tenantOne, "session", "s-1")
	cases := []struct {
		name        string
		grantScope  Scope
		key         string
		relation    Relation
		subjectType SubjectType
		subjectID   string
		wantAllow   bool
		wantReason  Reason
	}{
		{
			name: "direct viewer with own grant", grantScope: ScopeOwn, key: KeySessionRead,
			relation: RelationViewer, subjectType: SubjectTypeAccount, subjectID: accountAlice.String(),
			wantAllow: true,
		},
		{
			name: "owner satisfies read", grantScope: ScopeOwn, key: KeySessionRead,
			relation: RelationOwner, subjectType: SubjectTypeAccount, subjectID: accountAlice.String(),
			wantAllow: true,
		},
		{
			name: "editor satisfies write", grantScope: ScopeOwn, key: KeySessionWrite,
			relation: RelationEditor, subjectType: SubjectTypeAccount, subjectID: accountAlice.String(),
			wantAllow: true,
		},
		{
			name: "viewer cannot write", grantScope: ScopeTenant, key: KeySessionWrite,
			relation: RelationViewer, subjectType: SubjectTypeAccount, subjectID: accountAlice.String(),
			wantReason: ReasonMissingRelation,
		},
		{
			name: "tenant scope with no relation", grantScope: ScopeTenant, key: KeySessionRead,
			wantReason: ReasonMissingRelation,
		},
		{
			name: "group viewer needs team scope", grantScope: ScopeOwn, key: KeySessionRead,
			relation: RelationViewer, subjectType: SubjectTypeGroup, subjectID: groupSupport.String(),
			wantReason: ReasonMissingRelation,
		},
		{
			name: "group viewer with team scope", grantScope: ScopeTeam, key: KeySessionRead,
			relation: RelationViewer, subjectType: SubjectTypeGroup, subjectID: groupSupport.String(),
			wantAllow: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			grants := &fakeGrantSource{direct: map[uuid.UUID][]Grant{
				accountAlice: {grantOf(testCase.key, testCase.grantScope)},
			}}
			relations := newFakeRelationSource()
			if testCase.relation != RelationNone {
				relations.put(resource, testCase.relation, testCase.subjectType, testCase.subjectID)
			}
			if testCase.subjectType == SubjectTypeGroup {
				relations.groups[testCase.subjectID] = map[string]bool{accountAlice.String(): true}
			}
			authorizer := newTestAuthorizer(t, grants, relations)
			decision, err := authorizer.Authorize(context.Background(),
				Principal{TenantID: tenantOne, AccountID: accountAlice}, testCase.key, resource)
			if err != nil {
				t.Fatalf("Authorize: %v", err)
			}
			if decision.Allow != testCase.wantAllow {
				t.Fatalf("decision = %s, want allow=%v", decision, testCase.wantAllow)
			}
			if !testCase.wantAllow && decision.Reason != testCase.wantReason {
				t.Fatalf("reason = %q, want %q", decision.Reason, testCase.wantReason)
			}
		})
	}
}

func TestAuthorizeUsesGroupGrants(t *testing.T) {
	resource := resourceOf(tenantOne, "session", "s-1")
	grants := &fakeGrantSource{group: map[uuid.UUID][]Grant{
		accountAlice: {grantOf(KeySessionRead, ScopeTeam)},
	}}
	relations := newFakeRelationSource()
	relations.put(resource, RelationViewer, SubjectTypeGroup, groupSupport.String())
	relations.groups[groupSupport.String()] = map[string]bool{accountAlice.String(): true}
	authorizer := newTestAuthorizer(t, grants, relations)

	decision, err := authorizer.Authorize(context.Background(),
		Principal{TenantID: tenantOne, AccountID: accountAlice}, KeySessionRead, resource)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !decision.Allow || decision.Scope != ScopeTeam {
		t.Fatalf("decision = %s, want allow at team scope", decision)
	}
	if grants.groupCalls != 1 {
		t.Fatalf("group grant source calls = %d, want 1", grants.groupCalls)
	}
}

func TestAuthorizeUnresolvedSubjectFailsClosed(t *testing.T) {
	resource := resourceOf(tenantOne, "session", "s-1")
	grants := &fakeGrantSource{direct: map[uuid.UUID][]Grant{
		accountAlice: {grantOf(KeySessionRead, ScopeTenant)},
	}}
	relations := newFakeRelationSource()
	relations.put(resource, RelationViewer, SubjectTypeGroup, groupSupport.String())
	relations.unresolvedGroups[groupSupport.String()] = true
	authorizer := newTestAuthorizer(t, grants, relations)

	decision, err := authorizer.Authorize(context.Background(),
		Principal{TenantID: tenantOne, AccountID: accountAlice}, KeySessionRead, resource)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if decision.Allow || decision.Reason != ReasonUnresolvedSubject {
		t.Fatalf("decision = %s, want unresolved_subject", decision)
	}

	// A valid path still works even when another tuple is unresolved.
	relations.put(resource, RelationViewer, SubjectTypeAccount, accountAlice.String())
	decision, err = authorizer.Authorize(context.Background(),
		Principal{TenantID: tenantOne, AccountID: accountAlice}, KeySessionRead, resource)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !decision.Allow {
		t.Fatalf("decision = %s, want allow through the direct tuple", decision)
	}
}

func TestAuthorizeCrossTenant(t *testing.T) {
	otherResource := resourceOf(tenantTwo, "session", "s-2")
	relations := newFakeRelationSource()
	relations.put(otherResource, RelationViewer, SubjectTypeAccount, accountAlice.String())

	tenantGrants := &fakeGrantSource{direct: map[uuid.UUID][]Grant{
		accountAlice: {grantOf(KeySessionRead, ScopeTenant)},
	}}
	authorizer := newTestAuthorizer(t, tenantGrants, relations)
	decision, err := authorizer.Authorize(context.Background(),
		Principal{TenantID: tenantOne, AccountID: accountAlice}, KeySessionRead, otherResource)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if decision.Allow || decision.Reason != ReasonMissingGrant {
		t.Fatalf("tenant-scoped decision = %s, want missing_grant", decision)
	}
	if relations.relationsCalls != 0 {
		t.Fatal("cross-tenant tenant-scope check queried the relation source")
	}

	allGrants := &fakeGrantSource{direct: map[uuid.UUID][]Grant{
		accountAlice: {grantOf(KeySessionRead, ScopeAll)},
	}}
	authorizer = newTestAuthorizer(t, allGrants, relations)
	decision, err = authorizer.Authorize(context.Background(),
		Principal{TenantID: tenantOne, AccountID: accountAlice}, KeySessionRead, otherResource)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !decision.Allow {
		t.Fatalf("all-scoped decision = %s, want allow with a relation", decision)
	}
}

func TestAuthorizeIncompleteResource(t *testing.T) {
	grants := &fakeGrantSource{direct: map[uuid.UUID][]Grant{
		accountAlice: {grantOf(KeySessionRead, ScopeTenant)},
	}}
	authorizer := newTestAuthorizer(t, grants, newFakeRelationSource())
	decision, err := authorizer.Authorize(context.Background(),
		Principal{TenantID: tenantOne, AccountID: accountAlice}, KeySessionRead,
		Resource{TenantID: tenantOne, Type: "session"})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if decision.Allow || decision.Reason != ReasonMissingRelation {
		t.Fatalf("decision = %s, want missing_relation", decision)
	}
}

func TestAuthorizeSourceErrorsAreErrors(t *testing.T) {
	authorizer := newTestAuthorizer(t,
		&fakeGrantSource{directErr: failErr}, newFakeRelationSource())
	if _, err := authorizer.Authorize(context.Background(),
		Principal{TenantID: tenantOne, AccountID: accountAlice},
		KeySessionRead, resourceOf(tenantOne, "session", "s-1")); !errors.Is(err, failErr) {
		t.Fatalf("grant source error = %v, want %v", err, failErr)
	}

	grants := &fakeGrantSource{direct: map[uuid.UUID][]Grant{
		accountAlice: {grantOf(KeySessionRead, ScopeTenant)},
	}}
	relations := newFakeRelationSource()
	relations.relationsErr = failErr
	authorizer = newTestAuthorizer(t, grants, relations)
	if _, err := authorizer.Authorize(context.Background(),
		Principal{TenantID: tenantOne, AccountID: accountAlice},
		KeySessionRead, resourceOf(tenantOne, "session", "s-1")); !errors.Is(err, failErr) {
		t.Fatalf("relation source error = %v, want %v", err, failErr)
	}
}

func TestAuthorizeNilAuthorizer(t *testing.T) {
	var authorizer *Authorizer
	if _, err := authorizer.Authorize(context.Background(),
		Principal{TenantID: tenantOne, AccountID: accountAlice},
		KeySessionRead, Resource{}); err == nil {
		t.Fatal("nil authorizer succeeded")
	}
}

// TestAuthorizeCapabilityDoesNotMergeCallerPermissions is the L2/L13 contract:
// bot.invoke authorizes the caller to drive the Bot; the Bot then acts with its
// own Account permissions ∩ Harness, and neither side inherits the other's
// grants.
func TestAuthorizeCapabilityDoesNotMergeCallerPermissions(t *testing.T) {
	botResource := resourceOf(tenantOne, "bot", botX)
	secretResource := resourceOf(tenantOne, "secret", "secret-1")
	otherBotResource := resourceOf(tenantOne, "bot", botY)

	relations := newFakeRelationSource()
	// Alice may view/use bot-x.
	relations.put(botResource, RelationViewer, SubjectTypeAccount, accountAlice.String())
	// Bot-x's owning account owns the secret.
	relations.put(secretResource, RelationOwner, SubjectTypeAccount, accountBob.String())

	grants := &fakeGrantSource{direct: map[uuid.UUID][]Grant{
		// The caller only holds bot.invoke.
		accountAlice: {grantOf(KeyBotInvoke, ScopeTenant)},
		// The callee's account can read secrets.
		accountBob: {grantOf(KeySecretReadValue, ScopeTenant)},
	}}
	authorizer := newTestAuthorizer(t, grants, relations)
	caller := Principal{TenantID: tenantOne, AccountID: accountAlice}
	callee := Principal{
		TenantID: tenantOne, AccountID: accountBob, BotID: botX,
		HarnessPermissions: []string{KeySecretReadValue},
	}

	// 1. The caller may invoke bot-x.
	decision, err := authorizer.Authorize(context.Background(), caller, KeyBotInvoke, botResource)
	if err != nil {
		t.Fatalf("Authorize(caller invoke): %v", err)
	}
	if !decision.Allow {
		t.Fatalf("caller invoke decision = %s, want allow", decision)
	}

	// 2. The caller does not gain the callee's secret.read.
	decision, err = authorizer.Authorize(context.Background(), caller, KeySecretReadValue, secretResource)
	if err != nil {
		t.Fatalf("Authorize(caller secret): %v", err)
	}
	if decision.Allow || decision.Reason != ReasonMissingGrant {
		t.Fatalf("caller secret decision = %s, want missing_grant", decision)
	}

	// 3. The callee acts with its own permission.
	decision, err = authorizer.Authorize(context.Background(), callee, KeySecretReadValue, secretResource)
	if err != nil {
		t.Fatalf("Authorize(callee secret): %v", err)
	}
	if !decision.Allow {
		t.Fatalf("callee secret decision = %s, want allow", decision)
	}

	// 4. The callee does not gain the caller's bot.invoke.
	decision, err = authorizer.Authorize(context.Background(), callee, KeyBotInvoke, otherBotResource)
	if err != nil {
		t.Fatalf("Authorize(callee invoke): %v", err)
	}
	if decision.Allow || decision.Reason != ReasonMissingGrant {
		t.Fatalf("callee invoke decision = %s, want missing_grant", decision)
	}
}

func TestDecisionStringAndDenied(t *testing.T) {
	allow := allowDecision(KeySessionRead, ScopeOwn)
	if allow.Denied() || allow.String() == "" {
		t.Fatalf("allow decision = %s", allow)
	}
	deny := denyDecision(KeySessionRead, ScopeOwn, ReasonMissingRelation, "no viewer")
	if !deny.Denied() || deny.String() != "deny session.read: missing_relation: no viewer" {
		t.Fatalf("deny decision = %s", deny)
	}
}
