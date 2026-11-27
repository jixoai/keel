package access

import (
	"strings"
	"testing"
)

func TestBuiltinGroupsSelfCheck(t *testing.T) {
	if err := SelfCheckBuiltinGroups(); err != nil {
		t.Fatalf("built-in groups failed self-check: %v", err)
	}
	groups := BuiltinGroups()
	if len(groups) != 5 {
		t.Fatalf("built-in group count = %d, want 5", len(groups))
	}
	names := strings.Join(BuiltinGroupNames(), ",")
	for _, want := range []string{"auditor", "bot-operator", "super-admin", "tenant-admin", "viewer"} {
		if !strings.Contains(names, want) {
			t.Fatalf("missing preset %q in %s", want, names)
		}
	}
	for _, group := range groups {
		if len(group.Grants) == 0 {
			t.Fatalf("preset %q has no grants", group.Name)
		}
	}
}

func TestBuiltinSuperAdminHasEveryKeyAndNeverTheFlag(t *testing.T) {
	preset, ok := LookupBuiltinGroup("super-admin")
	if !ok {
		t.Fatal("super-admin preset missing")
	}
	if len(preset.Grants) != len(Keys()) {
		t.Fatalf("super-admin has %d grants, want all %d keys", len(preset.Grants), len(Keys()))
	}
	held := make(map[string]Scope, len(preset.Grants))
	for _, grant := range preset.Grants {
		held[grant.Key] = grant.Scope
	}
	for _, key := range Keys() {
		if held[key] != ScopeAll {
			t.Fatalf("super-admin key %q scope = %q, want all", key, held[key])
		}
	}
	// A group is a grant set only. Its name must not be mistaken for the
	// deployment super-admin flag: there is no field that could carry it.
	if _, isGrant := held["is_super_admin"]; isGrant {
		t.Fatal("a permission key named is_super_admin exists")
	}
}

// TestBotOperatorTriggerGrantsReachTheRoute pins the preset against the route
// matrix: GET /v1/triggers and POST /v1/triggers/{id}/fire use tenant-scoped
// keys (triggers are tenant-global and have no relation tuples). A team-scoped
// preset grant would pass the key check and then be refused by the route.
func TestBotOperatorTriggerGrantsReachTheRoute(t *testing.T) {
	preset, ok := LookupBuiltinGroup("bot-operator")
	if !ok {
		t.Fatal("bot-operator preset missing")
	}
	held := make(map[string]Scope, len(preset.Grants))
	for _, grant := range preset.Grants {
		held[grant.Key] = grant.Scope
	}
	for _, key := range []string{KeyTriggerRead, KeyTriggerManage} {
		scope, ok := held[key]
		if !ok {
			t.Fatalf("bot-operator does not grant %q", key)
		}
		if !scope.Covers(ScopeTenant) {
			t.Fatalf("bot-operator %q scope = %q, want tenant or wider", key, scope)
		}
	}
}

// TestBotOperatorCarriesBoardKeys pins the superagent operator preset: the
// mirror grants this group a relation on the queue channel, and that relation
// can only authorize an action when the group also holds board.read/write.
func TestBotOperatorCarriesBoardKeys(t *testing.T) {
	preset, ok := LookupBuiltinGroup("bot-operator")
	if !ok {
		t.Fatal("bot-operator preset missing")
	}
	held := make(map[string]Scope, len(preset.Grants))
	for _, grant := range preset.Grants {
		held[grant.Key] = grant.Scope
	}
	for _, key := range []string{KeyBoardRead, KeyBoardWrite} {
		scope, ok := held[key]
		if !ok {
			t.Fatalf("bot-operator does not grant %q", key)
		}
		if !scope.Covers(ScopeTeam) {
			t.Fatalf("bot-operator %q scope = %q, want team or wider", key, scope)
		}
	}
	if _, held := held[KeyBoardManage]; held {
		t.Fatal("bot-operator must not administer board settings or sync")
	}
}

func TestBuiltinTenantAdminIsTenantScoped(t *testing.T) {
	preset, ok := LookupBuiltinGroup("tenant-admin")
	if !ok {
		t.Fatal("tenant-admin preset missing")
	}
	for _, grant := range preset.Grants {
		if grant.Scope != ScopeTenant {
			t.Fatalf("tenant-admin grant %q scope = %q, want tenant", grant.Key, grant.Scope)
		}
	}
	if len(preset.Grants) != len(Keys()) {
		t.Fatalf("tenant-admin covers %d keys, want %d", len(preset.Grants), len(Keys()))
	}
}

func TestBuiltinGroupIDsAreDeterministicPerTenant(t *testing.T) {
	first := BuiltinGroupID(tenantOne, "viewer")
	second := BuiltinGroupID(tenantOne, "viewer")
	if first != second {
		t.Fatalf("ids differ: %s vs %s", first, second)
	}
	if other := BuiltinGroupID(tenantTwo, "viewer"); other == first {
		t.Fatal("different tenants share a built-in group id")
	}
	if other := BuiltinGroupID(tenantOne, "auditor"); other == first {
		t.Fatal("different names share a built-in group id in one tenant")
	}
}

func TestBuiltinGroupsReturnCopies(t *testing.T) {
	groups := BuiltinGroups()
	groups[0].Grants[0].Key = "session.fly"
	groups[0].Name = "mutated"
	fresh := BuiltinGroups()
	if fresh[0].Grants[0].Key == "session.fly" || fresh[0].Name == "mutated" {
		t.Fatal("BuiltinGroups returned a shared mutable catalog")
	}
}

func TestSelfCheckRejectsBadBuiltinCatalog(t *testing.T) {
	cases := map[string][]BuiltinGroup{
		"empty":       nil,
		"empty name":  {{Name: " ", Grants: []Grant{{Key: KeySessionRead, Scope: ScopeOwn}}}},
		"unknown key": {{Name: "x", Grants: []Grant{{Key: "session.fly", Scope: ScopeOwn}}}},
		"bad scope":   {{Name: "x", Grants: []Grant{{Key: KeySessionRead, Scope: "galaxy"}}}},
		"scope":       {{Name: "x", Grants: []Grant{{Key: KeyAccountRead, Scope: ScopeOwn}}}},
		"duplicate": {{Name: "x", Grants: []Grant{
			{Key: KeySessionRead, Scope: ScopeOwn},
			{Key: KeySessionRead, Scope: ScopeOwn},
		}}},
		"duplicate name": {
			{Name: "x", Grants: []Grant{{Key: KeySessionRead, Scope: ScopeOwn}}},
			{Name: "X", Grants: []Grant{{Key: KeySessionRead, Scope: ScopeOwn}}},
		},
	}
	for name, groups := range cases {
		if err := selfCheckBuiltinGroups(groups); err == nil {
			t.Fatalf("%s: bad catalog passed", name)
		}
	}
}

func TestCheckCoverageFloorRejectsEmptyCatalog(t *testing.T) {
	report := CheckCoverageFloor(nil, 10)
	if report.OK() {
		t.Fatal("empty catalog passed the floor gate")
	}
	if len(report.Issues) == 0 || report.Issues[0].Reason != CoverageTooFewEndpoints {
		t.Fatalf("issues = %+v, want too_few_endpoints first", report.Issues)
	}
}

func TestCheckCoverageFloorAcceptsLargeCatalog(t *testing.T) {
	endpoints := make([]Endpoint, 0, MinHTTPEndpoints)
	for index := 0; index < MinHTTPEndpoints; index++ {
		endpoints = append(endpoints, Endpoint{Kind: EndpointHTTP, Name: "GET /v1/x", RequiredKey: KeySessionRead})
	}
	if report := CheckCoverageFloor(endpoints, 0); !report.OK() {
		t.Fatalf("catalog at the floor failed: %v", report.Err())
	}
	if report := CheckCoverageFloor(endpoints[:MinHTTPEndpoints-1], 0); report.OK() {
		t.Fatal("catalog below the floor passed")
	}
}
