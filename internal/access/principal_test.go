package access

import "testing"

func TestPrincipalValidate(t *testing.T) {
	base := Principal{TenantID: tenantOne, AccountID: accountAlice}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid account principal rejected: %v", err)
	}
	bot := Principal{TenantID: tenantOne, AccountID: accountAlice, BotID: botX, HarnessPermissions: []string{KeySessionRead}}
	if err := bot.Validate(); err != nil {
		t.Fatalf("valid bot principal rejected: %v", err)
	}

	cases := []struct {
		name      string
		principal Principal
	}{
		{name: "missing tenant", principal: Principal{AccountID: accountAlice}},
		{name: "missing account", principal: Principal{TenantID: tenantOne}},
		{name: "disabled", principal: Principal{TenantID: tenantOne, AccountID: accountAlice, Disabled: true}},
		{name: "bot id with space", principal: Principal{TenantID: tenantOne, AccountID: accountAlice, BotID: "bot x"}},
		{name: "bot super admin", principal: Principal{TenantID: tenantOne, AccountID: accountAlice, BotID: botX, IsSuperAdmin: true}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := testCase.principal.Validate(); err == nil {
				t.Fatalf("Validate(%+v) succeeded, want an error", testCase.principal)
			}
		})
	}
}

func TestHarnessAllows(t *testing.T) {
	harness := []string{KeySessionRead, " " + KeyBotInvoke + " "}
	if !harnessAllows(harness, KeySessionRead) {
		t.Fatal("harness should include session.read")
	}
	if !harnessAllows(harness, KeyBotInvoke) {
		t.Fatal("harness should include bot.invoke around surrounding whitespace")
	}
	if harnessAllows(harness, KeySecretReadValue) {
		t.Fatal("harness should not include secret.read.value")
	}
	if harnessAllows(nil, KeySessionRead) {
		t.Fatal("an empty harness must allow nothing")
	}
}
