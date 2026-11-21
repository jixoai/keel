package access

import (
	"context"
	"errors"
	"testing"
)

func TestRelationVocabulary(t *testing.T) {
	for _, relation := range []Relation{RelationOwner, RelationTeamMember, RelationViewer, RelationEditor} {
		if !ValidRelation(relation) {
			t.Fatalf("ValidRelation(%q) = false, want true", relation)
		}
	}
	for _, relation := range []Relation{RelationNone, "admin", "OWNER"} {
		if ValidRelation(relation) {
			t.Fatalf("ValidRelation(%q) = true, want false", relation)
		}
	}
	for _, subjectType := range []SubjectType{SubjectTypeAccount, SubjectTypeGroup, SubjectTypeTeamBot} {
		if !ValidSubjectType(subjectType) {
			t.Fatalf("ValidSubjectType(%q) = false, want true", subjectType)
		}
	}
	for _, subjectType := range []SubjectType{"", "bot", "team"} {
		if ValidSubjectType(subjectType) {
			t.Fatalf("ValidSubjectType(%q) = true, want false", subjectType)
		}
	}
}

func TestRelationForAction(t *testing.T) {
	cases := []struct {
		action Action
		want   Relation
	}{
		{ActionRead, RelationViewer},
		{ActionInvoke, RelationViewer},
		{ActionWrite, RelationEditor},
		{ActionManage, RelationOwner},
		{ActionDelete, RelationOwner},
		{ActionCreate, RelationNone},
		{Action("archive"), RelationNone},
	}
	for _, testCase := range cases {
		if got := RelationForAction(testCase.action); got != testCase.want {
			t.Fatalf("RelationForAction(%q) = %q, want %q", testCase.action, got, testCase.want)
		}
	}
}

func TestRelationSatisfaction(t *testing.T) {
	cases := []struct {
		held     Relation
		required Relation
		want     bool
	}{
		{RelationOwner, RelationOwner, true},
		{RelationOwner, RelationEditor, true},
		{RelationOwner, RelationViewer, true},
		{RelationEditor, RelationEditor, true},
		{RelationEditor, RelationViewer, true},
		{RelationEditor, RelationOwner, false},
		{RelationViewer, RelationViewer, true},
		{RelationViewer, RelationEditor, false},
		{RelationViewer, RelationOwner, false},
		{RelationTeamMember, RelationTeamMember, true},
		{RelationTeamMember, RelationViewer, true},
		{RelationTeamMember, RelationEditor, false},
		{RelationTeamMember, RelationOwner, false},
	}
	for _, testCase := range cases {
		held := map[Relation]bool{testCase.held: true}
		if got := relationSatisfied(held, testCase.required); got != testCase.want {
			t.Fatalf("relationSatisfied(%q, %q) = %v, want %v", testCase.held, testCase.required, got, testCase.want)
		}
	}
	if !relationSatisfied(nil, RelationNone) {
		t.Fatal("RelationNone should always be satisfied")
	}
	if relationSatisfied(nil, RelationViewer) {
		t.Fatal("no held relation should satisfy viewer")
	}
}

func TestEvaluateRelationsMatchesAccountDirectly(t *testing.T) {
	source := newFakeRelationSource()
	resource := resourceOf(tenantOne, "session", "s-1")
	source.put(resource, RelationViewer, SubjectTypeAccount, accountAlice.String())
	source.put(resource, RelationOwner, SubjectTypeAccount, accountBob.String())

	evaluation, err := EvaluateRelations(context.Background(), source,
		Principal{TenantID: tenantOne, AccountID: accountAlice}, resource)
	if err != nil {
		t.Fatalf("EvaluateRelations: %v", err)
	}
	if !evaluation.Held[RelationViewer] {
		t.Fatal("Alice should hold viewer")
	}
	if !evaluation.Direct[RelationViewer] {
		t.Fatal("viewer should be a direct relation for Alice")
	}
	if evaluation.Held[RelationOwner] {
		t.Fatal("Bob's owner relation leaked to Alice")
	}
	if evaluation.Unresolved {
		t.Fatalf("evaluation unexpectedly unresolved: %s", evaluation.Detail)
	}
	if source.resolveCalls != 0 {
		t.Fatalf("account subjects should be matched locally, source resolve calls = %d", source.resolveCalls)
	}
}

func TestEvaluateRelationsResolvesGroupAndTeamBot(t *testing.T) {
	resource := resourceOf(tenantOne, "session", "s-1")
	source := newFakeRelationSource()
	source.put(resource, RelationViewer, SubjectTypeGroup, groupSupport.String())
	source.put(resource, RelationEditor, SubjectTypeTeamBot, teamSupport)
	source.groups[groupSupport.String()] = map[string]bool{accountAlice.String(): true}
	source.teams[teamSupport] = map[string]bool{botX: true}

	alice, err := EvaluateRelations(context.Background(), source,
		Principal{TenantID: tenantOne, AccountID: accountAlice}, resource)
	if err != nil {
		t.Fatalf("EvaluateRelations(Alice): %v", err)
	}
	if !alice.Held[RelationViewer] {
		t.Fatal("Alice should hold viewer through the group")
	}
	if alice.Direct[RelationViewer] {
		t.Fatal("a group relation is not direct")
	}

	bot, err := EvaluateRelations(context.Background(), source,
		Principal{TenantID: tenantOne, AccountID: accountAlice, BotID: botX}, resource)
	if err != nil {
		t.Fatalf("EvaluateRelations(bot): %v", err)
	}
	if !bot.Held[RelationEditor] {
		t.Fatal("bot-x should hold editor through the TeamBot roster")
	}

	otherBot, err := EvaluateRelations(context.Background(), source,
		Principal{TenantID: tenantOne, AccountID: accountAlice, BotID: botY}, resource)
	if err != nil {
		t.Fatalf("EvaluateRelations(other bot): %v", err)
	}
	if otherBot.Held[RelationEditor] {
		t.Fatal("bot-y is not in the roster and must not hold editor")
	}
}

func TestEvaluateRelationsHumanIsNotATeamBotMember(t *testing.T) {
	resource := resourceOf(tenantOne, "session", "s-1")
	source := newFakeRelationSource()
	source.put(resource, RelationViewer, SubjectTypeTeamBot, teamSupport)
	source.teams[teamSupport] = map[string]bool{botX: true}

	evaluation, err := EvaluateRelations(context.Background(), source,
		Principal{TenantID: tenantOne, AccountID: accountAlice}, resource)
	if err != nil {
		t.Fatalf("EvaluateRelations: %v", err)
	}
	if evaluation.Held[RelationViewer] {
		t.Fatal("a human principal must not resolve as a TeamBot member")
	}
	if evaluation.Unresolved {
		t.Fatal("a human principal is a resolvable non-member, not an unresolved subject")
	}
	if source.resolveCalls != 0 {
		t.Fatalf("human/team_bot tuple should not call ResolveSubject, calls = %d", source.resolveCalls)
	}
}

func TestEvaluateRelationsUnresolvedSubjectFailsClosed(t *testing.T) {
	resource := resourceOf(tenantOne, "session", "s-1")
	source := newFakeRelationSource()
	source.put(resource, RelationViewer, SubjectTypeGroup, groupSupport.String())
	source.unresolvedGroups[groupSupport.String()] = true

	evaluation, err := EvaluateRelations(context.Background(), source,
		Principal{TenantID: tenantOne, AccountID: accountAlice}, resource)
	if err != nil {
		t.Fatalf("EvaluateRelations: %v", err)
	}
	if !evaluation.Unresolved {
		t.Fatal("a missing group must mark the evaluation unresolved")
	}
	if evaluation.Held[RelationViewer] {
		t.Fatal("an unresolved group must not grant viewer")
	}
	if evaluation.Detail == "" {
		t.Fatal("unresolved evaluation should carry a detail")
	}
}

func TestEvaluateRelationsMalformedTupleFailsClosed(t *testing.T) {
	resource := resourceOf(tenantOne, "session", "s-1")
	source := newFakeRelationSource()
	source.put(resource, "admin", SubjectTypeAccount, accountAlice.String())
	source.put(resource, RelationViewer, SubjectTypeAccount, "not-a-uuid")

	evaluation, err := EvaluateRelations(context.Background(), source,
		Principal{TenantID: tenantOne, AccountID: accountAlice}, resource)
	if err != nil {
		t.Fatalf("EvaluateRelations: %v", err)
	}
	if !evaluation.Unresolved {
		t.Fatal("malformed tuples must mark the evaluation unresolved")
	}
	if len(evaluation.Held) != 0 {
		t.Fatalf("malformed tuples granted relations: %v", evaluation.Held)
	}
}

func TestEvaluateRelationsSourceError(t *testing.T) {
	source := newFakeRelationSource()
	source.relationsErr = failErr
	_, err := EvaluateRelations(context.Background(), source,
		Principal{TenantID: tenantOne, AccountID: accountAlice},
		resourceOf(tenantOne, "session", "s-1"))
	if !errors.Is(err, failErr) {
		t.Fatalf("EvaluateRelations error = %v, want %v", err, failErr)
	}
}

func TestEvaluateRelationsRejectsIncompleteResource(t *testing.T) {
	source := newFakeRelationSource()
	_, err := EvaluateRelations(context.Background(), source,
		Principal{TenantID: tenantOne, AccountID: accountAlice},
		Resource{TenantID: tenantOne, Type: "session"})
	if err == nil {
		t.Fatal("incomplete resource should be an error")
	}
}

func TestScopeReachSatisfied(t *testing.T) {
	evaluation := RelationEvaluation{
		Held: map[Relation]bool{
			RelationViewer: true,
			RelationEditor: true,
		},
		Direct: map[Relation]bool{
			RelationViewer: true,
		},
	}
	cases := []struct {
		name     string
		scope    Scope
		required Relation
		want     bool
	}{
		{name: "own direct viewer", scope: ScopeOwn, required: RelationViewer, want: true},
		{name: "own editor held only indirectly", scope: ScopeOwn, required: RelationEditor, want: false},
		{name: "team indirect editor", scope: ScopeTeam, required: RelationEditor, want: true},
		{name: "tenant indirect editor", scope: ScopeTenant, required: RelationEditor, want: true},
		{name: "all indirect editor", scope: ScopeAll, required: RelationEditor, want: true},
		{name: "own owner not held", scope: ScopeOwn, required: RelationOwner, want: false},
		{name: "unknown scope", scope: Scope("world"), required: RelationViewer, want: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := scopeReachSatisfied(testCase.scope, evaluation, testCase.required); got != testCase.want {
				t.Fatalf("scopeReachSatisfied(%q, %q) = %v, want %v", testCase.scope, testCase.required, got, testCase.want)
			}
		})
	}
	if scopeReachSatisfied(ScopeTenant, RelationEvaluation{}, RelationViewer) {
		t.Fatal("no relations must fail closed even for tenant scope")
	}
}
