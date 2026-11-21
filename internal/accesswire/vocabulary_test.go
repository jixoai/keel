package accesswire

import (
	"testing"

	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/store"
)

// TestStoreAndAccessVocabulariesAgree closes the "two hand-written
// vocabularies" risk: the engine and the schema must accept exactly the same
// scope, relation and subject-type words. A drift would mean a grant the store
// accepts but the engine rejects (or worse, the reverse), so the test fails on
// any set difference in either direction.
func TestStoreAndAccessVocabulariesAgree(t *testing.T) {
	accessScopes := []access.Scope{access.ScopeOwn, access.ScopeTeam, access.ScopeTenant, access.ScopeAll}
	for _, scope := range accessScopes {
		if !store.ValidPermissionScope(string(scope)) {
			t.Fatalf("access scope %q is not a store scope", scope)
		}
		if parsed, err := access.ParseScope(string(scope)); err != nil || parsed != scope {
			t.Fatalf("store/access scope %q round-trip: %v", scope, err)
		}
	}
	storeScopes := []string{
		store.PermissionScopeOwn, store.PermissionScopeTeam,
		store.PermissionScopeTenant, store.PermissionScopeAll,
	}
	if len(storeScopes) != len(accessScopes) {
		t.Fatalf("store has %d scopes, access has %d", len(storeScopes), len(accessScopes))
	}
	for _, scope := range storeScopes {
		if _, err := access.ParseScope(scope); err != nil {
			t.Fatalf("store scope %q is not an access scope: %v", scope, err)
		}
	}

	accessRelations := []access.Relation{
		access.RelationOwner, access.RelationTeamMember,
		access.RelationViewer, access.RelationEditor,
	}
	for _, relation := range accessRelations {
		if !store.ValidRelation(string(relation)) {
			t.Fatalf("access relation %q is not a store relation", relation)
		}
	}
	storeRelations := []string{
		store.RelationOwner, store.RelationTeamMember,
		store.RelationViewer, store.RelationEditor,
	}
	if len(storeRelations) != len(accessRelations) {
		t.Fatalf("store has %d relations, access has %d", len(storeRelations), len(accessRelations))
	}
	for _, relation := range storeRelations {
		if !access.ValidRelation(access.Relation(relation)) {
			t.Fatalf("store relation %q is not an access relation", relation)
		}
	}

	accessSubjects := []access.SubjectType{
		access.SubjectTypeAccount, access.SubjectTypeGroup, access.SubjectTypeTeamBot,
	}
	for _, subject := range accessSubjects {
		if !store.ValidRelationSubject(string(subject)) {
			t.Fatalf("access subject %q is not a store relation subject", subject)
		}
	}
	storeSubjects := []string{
		store.RelationSubjectAccount, store.RelationSubjectGroup, store.RelationSubjectTeamBot,
	}
	if len(storeSubjects) != len(accessSubjects) {
		t.Fatalf("store has %d subject types, access has %d", len(storeSubjects), len(accessSubjects))
	}
	for _, subject := range storeSubjects {
		if !access.ValidSubjectType(access.SubjectType(subject)) {
			t.Fatalf("store subject %q is not an access subject", subject)
		}
	}

	if access.GrantSubjectAccount != store.PermissionSubjectAccount {
		t.Fatalf("direct-grant subject: access %q != store %q",
			access.GrantSubjectAccount, store.PermissionSubjectAccount)
	}
	if access.GrantSubjectGroup != store.RelationSubjectGroup {
		t.Fatalf("group subject word: access %q != store %q",
			access.GrantSubjectGroup, store.RelationSubjectGroup)
	}
	if err := access.ValidateDirectGrantSubject(access.GrantSubjectGroup); err == nil {
		t.Fatal("access accepted a group direct-grant subject")
	}
	if err := access.ValidateDirectGrantSubject(store.PermissionSubjectAccount); err != nil {
		t.Fatalf("access rejected the store's account subject: %v", err)
	}
}
