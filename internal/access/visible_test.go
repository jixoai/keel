package access

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// fakeBatchSource is the fakeRelationSource plus the optional batch seam.
type fakeBatchSource struct {
	*fakeRelationSource
	batchCalls int
	batchErr   error
}

func (s *fakeBatchSource) RelationsForResources(ctx context.Context, resource Resource, ids []string) (map[string][]RelationTuple, error) {
	s.batchCalls++
	if s.batchErr != nil {
		return nil, s.batchErr
	}
	out := make(map[string][]RelationTuple, len(ids))
	for _, id := range ids {
		out[id] = append([]RelationTuple(nil), s.tuples[relationResourceKey(Resource{
			TenantID: resource.TenantID, Type: resource.Type, ID: id,
		})]...)
	}
	return out, nil
}

func visibleAlice() Principal {
	return Principal{TenantID: tenantOne, AccountID: accountAlice}
}

func TestVisibleResourcesFiltersByRelation(t *testing.T) {
	grants := &fakeGrantSource{direct: map[uuid.UUID][]Grant{
		accountAlice: {{Key: KeyBotRead, Scope: ScopeTenant}},
	}}
	relations := newFakeRelationSource()
	// Alice owns bot-a and can view bot-b; bot-c has no tuple for her.
	relations.put(resourceOf(tenantOne, "bot", "bot-a"), RelationOwner, SubjectTypeAccount, accountAlice.String())
	relations.put(resourceOf(tenantOne, "bot", "bot-b"), RelationViewer, SubjectTypeAccount, accountAlice.String())
	relations.put(resourceOf(tenantOne, "bot", "bot-c"), RelationViewer, SubjectTypeAccount, accountBob.String())
	authorizer := newTestAuthorizer(t, grants, relations)

	result, err := authorizer.VisibleResources(context.Background(), visibleAlice(), KeyBotRead, "bot",
		[]string{"bot-a", "bot-b", "bot-c"})
	if err != nil {
		t.Fatalf("VisibleResources: %v", err)
	}
	if !result.Allowed() {
		t.Fatalf("key gate denied: %+v", result.Decision)
	}
	if len(result.IDs) != 2 || result.IDs[0] != "bot-a" || result.IDs[1] != "bot-b" {
		t.Fatalf("visible = %v, want [bot-a bot-b]", result.IDs)
	}
	if result.Scope != ScopeTenant {
		t.Fatalf("scope = %q, want tenant", result.Scope)
	}
}

func TestVisibleResourcesTenantScopeDoesNotBypassRelation(t *testing.T) {
	// The spec scenario: tenant-scoped key, no relation -> still invisible. A
	// "scope >= tenant" shortcut would leak bot-c here.
	grants := &fakeGrantSource{direct: map[uuid.UUID][]Grant{
		accountAlice: {{Key: KeyBotRead, Scope: ScopeTenant}},
	}}
	relations := newFakeRelationSource()
	relations.put(resourceOf(tenantOne, "bot", "bot-c"), RelationOwner, SubjectTypeAccount, accountBob.String())
	authorizer := newTestAuthorizer(t, grants, relations)

	result, err := authorizer.VisibleResources(context.Background(), visibleAlice(), KeyBotRead, "bot", []string{"bot-c"})
	if err != nil {
		t.Fatalf("VisibleResources: %v", err)
	}
	if !result.Allowed() {
		t.Fatalf("key gate denied: %+v", result.Decision)
	}
	if len(result.IDs) != 0 {
		t.Fatalf("tenant scope leaked a relation-free resource: %v", result.IDs)
	}
}

func TestVisibleResourcesDeniedWithoutGrant(t *testing.T) {
	relations := newFakeRelationSource()
	authorizer := newTestAuthorizer(t, &fakeGrantSource{}, relations)

	result, err := authorizer.VisibleResources(context.Background(), visibleAlice(), KeyBotRead, "bot", []string{"bot-a"})
	if err != nil {
		t.Fatalf("VisibleResources: %v", err)
	}
	if result.Allowed() {
		t.Fatal("missing grant allowed the list")
	}
	if result.Decision.Reason != ReasonMissingGrant {
		t.Fatalf("reason = %q, want missing_grant", result.Decision.Reason)
	}
	if len(result.IDs) != 0 {
		t.Fatalf("denied list carried ids: %v", result.IDs)
	}
}

func TestVisibleResourcesOwnScopeNeedsDirectRelation(t *testing.T) {
	grants := &fakeGrantSource{direct: map[uuid.UUID][]Grant{
		accountAlice: {{Key: KeySessionRead, Scope: ScopeOwn}},
	}}
	relations := newFakeRelationSource()
	// Direct viewer on s-own, group-derived viewer on s-team.
	relations.put(resourceOf(tenantOne, "session", "s-own"), RelationViewer, SubjectTypeAccount, accountAlice.String())
	relations.put(resourceOf(tenantOne, "session", "s-team"), RelationViewer, SubjectTypeGroup, groupSupport.String())
	relations.groups[groupSupport.String()] = map[string]bool{accountAlice.String(): true}
	authorizer := newTestAuthorizer(t, grants, relations)

	result, err := authorizer.VisibleResources(context.Background(), visibleAlice(), KeySessionRead, "session",
		[]string{"s-own", "s-team"})
	if err != nil {
		t.Fatalf("VisibleResources: %v", err)
	}
	if len(result.IDs) != 1 || result.IDs[0] != "s-own" {
		t.Fatalf("own scope ids = %v, want [s-own]", result.IDs)
	}
}

func TestVisibleResourcesUnresolvedSubjectFailsClosed(t *testing.T) {
	grants := &fakeGrantSource{direct: map[uuid.UUID][]Grant{
		accountAlice: {{Key: KeyBotRead, Scope: ScopeTeam}},
	}}
	relations := newFakeRelationSource()
	relations.put(resourceOf(tenantOne, "bot", "bot-x"), RelationViewer, SubjectTypeGroup, groupSupport.String())
	relations.unresolvedGroups[groupSupport.String()] = true
	authorizer := newTestAuthorizer(t, grants, relations)

	result, err := authorizer.VisibleResources(context.Background(), visibleAlice(), KeyBotRead, "bot", []string{"bot-x"})
	if err != nil {
		t.Fatalf("VisibleResources: %v", err)
	}
	if len(result.IDs) != 0 {
		t.Fatalf("unresolved subject granted visibility: %v", result.IDs)
	}
}

func TestVisibleResourcesCrossTenantTuplesAreInvisible(t *testing.T) {
	grants := &fakeGrantSource{direct: map[uuid.UUID][]Grant{
		accountAlice: {{Key: KeyBotRead, Scope: ScopeAll}},
	}}
	relations := newFakeRelationSource()
	// Alice owns the same handle in ANOTHER tenant; this tenant's bot-a has no
	// relation. Scope all still does not import a foreign tenant's tuple.
	relations.put(resourceOf(tenantTwo, "bot", "bot-a"), RelationOwner, SubjectTypeAccount, accountAlice.String())
	authorizer := newTestAuthorizer(t, grants, relations)

	result, err := authorizer.VisibleResources(context.Background(), visibleAlice(), KeyBotRead, "bot", []string{"bot-a"})
	if err != nil {
		t.Fatalf("VisibleResources: %v", err)
	}
	if len(result.IDs) != 0 {
		t.Fatalf("cross-tenant tuple granted visibility: %v", result.IDs)
	}
}

func TestVisibleResourcesBotHarnessNarrowing(t *testing.T) {
	grants := &fakeGrantSource{direct: map[uuid.UUID][]Grant{
		accountAlice: {{Key: KeyBotRead, Scope: ScopeTenant}},
	}}
	relations := newFakeRelationSource()
	relations.put(resourceOf(tenantOne, "bot", "bot-a"), RelationOwner, SubjectTypeAccount, accountAlice.String())
	authorizer := newTestAuthorizer(t, grants, relations)

	principal := visibleAlice()
	principal.BotID = botX
	principal.HarnessPermissions = []string{KeySessionRead}
	result, err := authorizer.VisibleResources(context.Background(), principal, KeyBotRead, "bot", []string{"bot-a"})
	if err != nil {
		t.Fatalf("VisibleResources: %v", err)
	}
	if result.Allowed() || result.Decision.Reason != ReasonMissingKey {
		t.Fatalf("harness narrowing = %+v, want missing_key", result.Decision)
	}
}

func TestVisibleResourcesSuperAdminSeesAll(t *testing.T) {
	authorizer := newTestAuthorizer(t, &fakeGrantSource{}, newFakeRelationSource())
	principal := Principal{TenantID: tenantOne, AccountID: accountAlice, IsSuperAdmin: true}
	result, err := authorizer.VisibleResources(context.Background(), principal, KeyBotRead, "bot", []string{"a", "b", "a", ""})
	if err != nil {
		t.Fatalf("VisibleResources: %v", err)
	}
	if len(result.IDs) != 2 || result.IDs[0] != "a" || result.IDs[1] != "b" {
		t.Fatalf("ids = %v, want deduplicated [a b]", result.IDs)
	}
	if result.Scope != ScopeAll {
		t.Fatalf("scope = %q, want all", result.Scope)
	}
}

func TestVisibleResourcesRequiresResourceScopedKey(t *testing.T) {
	authorizer := newTestAuthorizer(t, &fakeGrantSource{}, newFakeRelationSource())
	_, err := authorizer.VisibleResources(context.Background(), visibleAlice(), KeyAccountRead, "account", []string{"a"})
	if !errors.Is(err, ErrVisibilityNotResourceScoped) {
		t.Fatalf("err = %v, want ErrVisibilityNotResourceScoped", err)
	}
}

func TestVisibleResourcesUsesBatchSeam(t *testing.T) {
	grants := &fakeGrantSource{direct: map[uuid.UUID][]Grant{
		accountAlice: {{Key: KeyBotRead, Scope: ScopeTenant}},
	}}
	batch := &fakeBatchSource{fakeRelationSource: newFakeRelationSource()}
	batch.put(resourceOf(tenantOne, "bot", "bot-a"), RelationOwner, SubjectTypeAccount, accountAlice.String())
	authorizer, err := NewAuthorizer(grants, batch)
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	result, err := authorizer.VisibleResources(context.Background(), visibleAlice(), KeyBotRead, "bot", []string{"bot-a", "bot-b"})
	if err != nil {
		t.Fatalf("VisibleResources: %v", err)
	}
	if batch.batchCalls != 1 {
		t.Fatalf("batch calls = %d, want 1", batch.batchCalls)
	}
	if len(result.IDs) != 1 || result.IDs[0] != "bot-a" {
		t.Fatalf("ids = %v, want [bot-a]", result.IDs)
	}
}

func TestVisibleResourcesSourceErrorIsAnError(t *testing.T) {
	grants := &fakeGrantSource{direct: map[uuid.UUID][]Grant{
		accountAlice: {{Key: KeyBotRead, Scope: ScopeTenant}},
	}}
	relations := newFakeRelationSource()
	relations.relationsErr = failErr
	authorizer := newTestAuthorizer(t, grants, relations)

	if _, err := authorizer.VisibleResources(context.Background(), visibleAlice(), KeyBotRead, "bot", []string{"bot-a"}); err == nil {
		t.Fatal("source failure was not returned as an error")
	}
}

func TestVisibleResourcesEmptyCandidates(t *testing.T) {
	grants := &fakeGrantSource{direct: map[uuid.UUID][]Grant{
		accountAlice: {{Key: KeyBotRead, Scope: ScopeTenant}},
	}}
	authorizer := newTestAuthorizer(t, grants, newFakeRelationSource())
	result, err := authorizer.VisibleResources(context.Background(), visibleAlice(), KeyBotRead, "bot", nil)
	if err != nil {
		t.Fatalf("VisibleResources: %v", err)
	}
	if !result.Allowed() || len(result.IDs) != 0 {
		t.Fatalf("empty candidates = %+v, want an allowed empty answer", result)
	}
}
