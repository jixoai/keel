package access

import (
	"errors"
	"sort"
	"strings"
	"testing"
)

func TestRegistrySelfCheck(t *testing.T) {
	if err := SelfCheck(); err != nil {
		t.Fatalf("SelfCheck: %v", err)
	}
}

func TestRegistryEntriesAreWellFormed(t *testing.T) {
	specs := All()
	if len(specs) == 0 {
		t.Fatal("registry is empty")
	}
	seen := make(map[string]bool, len(specs))
	for _, spec := range specs {
		if seen[spec.Key] {
			t.Fatalf("duplicate key %q", spec.Key)
		}
		seen[spec.Key] = true
		if !permissionKeyPattern.MatchString(spec.Key) {
			t.Fatalf("key %q does not match the permission-key grammar", spec.Key)
		}
		if spec.Domain != strings.SplitN(spec.Key, ".", 2)[0] {
			t.Fatalf("key %q has domain %q", spec.Key, spec.Domain)
		}
		if strings.TrimSpace(spec.DisplayName) == "" {
			t.Fatalf("key %q has no display name", spec.Key)
		}
		if !ValidRisk(spec.Risk) {
			t.Fatalf("key %q has unknown risk %q", spec.Key, spec.Risk)
		}
		if spec.Relation != RelationNone && !ValidRelation(spec.Relation) {
			t.Fatalf("key %q has unknown relation %q", spec.Key, spec.Relation)
		}
		if len(spec.AllowedScopes) == 0 {
			t.Fatalf("key %q has no allowed scopes", spec.Key)
		}
		for _, scope := range spec.AllowedScopes {
			if !ValidScope(scope) {
				t.Fatalf("key %q allows unknown scope %q", spec.Key, scope)
			}
		}
		if spec.Relation == RelationNone && (spec.AllowsScope(ScopeOwn) || spec.AllowsScope(ScopeTeam)) {
			t.Fatalf("tenant-level key %q allows own/team scope", spec.Key)
		}
	}
}

// TestRegistryCoversAcceptanceSurface is the completeness check for A3: every
// functional area acceptance.md exercises has a key, and the catalog has no
// unreviewed extra. Add to both lists together when the surface grows.
func TestRegistryCoversAcceptanceSurface(t *testing.T) {
	required := []string{
		KeySessionRead, KeySessionWrite, KeySessionDelete, KeySessionShare,
		KeyRoleRead, KeyRoleManage, KeyProfileRead, KeyProfileManage,
		KeyBotRead, KeyBotCreate, KeyBotManage, KeyBotBind, KeyBotInvoke,
		KeyTeamBotRead, KeyTeamBotManage,
		KeyTriggerRead, KeyTriggerManage,
		KeyPluginRead, KeyPluginInvoke, KeyPluginLifecycleInstall, KeyPluginLifecycleEnable,
		KeyPluginLifecycleDisable, KeyPluginLifecycleUninstall,
		KeyModelRead, KeyModelConfigure, KeyModelInvoke,
		KeySecretReadMetadata, KeySecretReadValue, KeySecretWrite,
		KeyAccountRead, KeyAccountManage, KeyAccountCredentialReset, KeyAccountPermissionGrant,
		KeyPermissionRead, KeyPermissionManage,
		KeyAuditRead,
		KeyBoardRead, KeyBoardWrite, KeyBoardManage,
		KeyAppRead, KeyFileRead, KeyFileWrite, KeyMediaRead, KeyMediaInvoke,
		KeyMCPRead, KeyMCPUse, KeySkillRead, KeySkillManage,
		KeyNotificationRead, KeyNotificationWrite,
		KeyShellExec, KeyTaskRead, KeyTaskWrite, KeyApprovalRead, KeyApprovalWrite, KeyDecisionAsk,
	}
	for _, key := range required {
		if err := ValidateKey(key); err != nil {
			t.Fatalf("acceptance surface key missing: %v", err)
		}
	}
	all := Keys()
	if len(required) != len(all) {
		t.Fatalf("acceptance surface lists %d keys, registry has %d; keep the catalog and the list in sync", len(required), len(all))
	}
	requiredSet := make(map[string]bool, len(required))
	for _, key := range required {
		requiredSet[key] = true
	}
	for _, key := range all {
		if !requiredSet[key] {
			t.Fatalf("registry key %q is not in the acceptance surface list", key)
		}
	}
}

func TestLookupReturnsCopies(t *testing.T) {
	spec, ok := Lookup(KeySessionRead)
	if !ok {
		t.Fatal("Lookup(session.read) failed")
	}
	if spec.Key != KeySessionRead || spec.Domain != "session" {
		t.Fatalf("Lookup(session.read) = %+v", spec)
	}
	spec.AllowedScopes[0] = ScopeAll
	again, _ := Lookup(KeySessionRead)
	if again.AllowedScopes[0] != ScopeOwn {
		t.Fatal("Lookup exposed the registry's scope slice")
	}
	if _, ok := Lookup("session.fly"); ok {
		t.Fatal("Lookup accepted an unknown key")
	}
}

func TestAllIsSortedAndCopied(t *testing.T) {
	specs := All()
	if !sort.SliceIsSorted(specs, func(i, j int) bool {
		if specs[i].Domain != specs[j].Domain {
			return specs[i].Domain < specs[j].Domain
		}
		return specs[i].Key < specs[j].Key
	}) {
		t.Fatal("All() is not sorted by domain then key")
	}
	original := specs[0].DisplayName
	specs[0].DisplayName = "mutated"
	again, _ := Lookup(specs[0].Key)
	if again.DisplayName != original {
		t.Fatal("All() exposed the registry's backing data")
	}
	keys := Keys()
	if len(keys) != len(specs) {
		t.Fatalf("Keys() has %d entries, All() has %d", len(keys), len(specs))
	}
}

func TestValidateKey(t *testing.T) {
	if err := ValidateKey(KeyBotInvoke); err != nil {
		t.Fatalf("ValidateKey(%q): %v", KeyBotInvoke, err)
	}
	for _, key := range []string{"", "session.fly", "SESSION.READ", "session read", "session"} {
		err := ValidateKey(key)
		if err == nil {
			t.Fatalf("ValidateKey(%q) succeeded, want an error", key)
		}
		if !errors.Is(err, ErrUnknownKey) {
			t.Fatalf("ValidateKey(%q) error = %v, want ErrUnknownKey", key, err)
		}
	}
}

func validSpec() KeySpec {
	return KeySpec{
		Key:           "session.read",
		Domain:        "session",
		DisplayName:   "Read sessions",
		Risk:          RiskNormal,
		Relation:      RelationViewer,
		AllowedScopes: []Scope{ScopeOwn, ScopeTeam, ScopeTenant, ScopeAll},
	}
}

func TestSelfCheckRejectsBadCatalogs(t *testing.T) {
	cases := []struct {
		name  string
		specs []KeySpec
	}{
		{name: "empty", specs: nil},
		{name: "empty key", specs: []KeySpec{func() KeySpec { s := validSpec(); s.Key = ""; return s }()}},
		{name: "illegal key", specs: []KeySpec{func() KeySpec { s := validSpec(); s.Key = "Session.Read"; return s }()}},
		{name: "single segment", specs: []KeySpec{func() KeySpec { s := validSpec(); s.Key = "session"; s.Domain = "session"; return s }()}},
		{name: "empty domain", specs: []KeySpec{func() KeySpec { s := validSpec(); s.Domain = ""; return s }()}},
		{name: "domain mismatch", specs: []KeySpec{func() KeySpec { s := validSpec(); s.Domain = "bot"; return s }()}},
		{name: "empty display name", specs: []KeySpec{func() KeySpec { s := validSpec(); s.DisplayName = "  "; return s }()}},
		{name: "unknown risk", specs: []KeySpec{func() KeySpec { s := validSpec(); s.Risk = "medium"; return s }()}},
		{name: "unknown relation", specs: []KeySpec{func() KeySpec { s := validSpec(); s.Relation = "admin"; return s }()}},
		{name: "empty scopes", specs: []KeySpec{func() KeySpec { s := validSpec(); s.AllowedScopes = nil; return s }()}},
		{name: "unknown scope", specs: []KeySpec{func() KeySpec { s := validSpec(); s.AllowedScopes = []Scope{ScopeOwn, "world"}; return s }()}},
		{name: "duplicate scope", specs: []KeySpec{func() KeySpec { s := validSpec(); s.AllowedScopes = []Scope{ScopeOwn, ScopeOwn}; return s }()}},
		{name: "tenant key with own scope", specs: []KeySpec{func() KeySpec {
			s := validSpec()
			s.Key, s.Domain, s.Relation = "account.read", "account", RelationNone
			s.AllowedScopes = []Scope{ScopeOwn, ScopeTenant}
			return s
		}()}},
		{name: "duplicate key", specs: []KeySpec{validSpec(), validSpec()}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := selfCheckCatalog(testCase.specs); err == nil {
				t.Fatal("selfCheckCatalog succeeded, want an error")
			}
		})
	}
}

func TestCatalogStartsWithReviewedRiskLevels(t *testing.T) {
	// A small pin against accidental downgrades of the most sensitive keys.
	critical := []string{
		KeyBotInvoke,
		KeyPluginLifecycleInstall,
		KeyPluginLifecycleEnable,
		KeyPluginLifecycleDisable,
		KeyPluginLifecycleUninstall,
		KeySecretReadValue,
		KeyAccountCredentialReset,
		KeyAccountPermissionGrant,
	}
	for _, key := range critical {
		spec, ok := Lookup(key)
		if !ok {
			t.Fatalf("critical key %q missing", key)
		}
		if spec.Risk != RiskCritical {
			t.Fatalf("key %q risk = %q, want critical", key, spec.Risk)
		}
	}
}
