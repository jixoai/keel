package botteam

import (
	"testing"

	"github.com/google/uuid"
)

func TestAccountValidateAndNormalize(t *testing.T) {
	valid := Account{
		ID:          uuid.New(),
		Username:    "  alice  ",
		DisplayName: "  Alice  ",
		Kind:        AccountKindHuman,
	}
	normalized := valid.Normalized()
	if normalized.Username != "alice" || normalized.DisplayName != "Alice" {
		t.Fatalf("normalized account = %+v", normalized)
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid account rejected: %v", err)
	}
	for _, kind := range KnownAccountKinds() {
		account := valid
		account.Kind = kind
		if err := account.Validate(); err != nil {
			t.Fatalf("kind %q rejected: %v", kind, err)
		}
	}

	invalid := []struct {
		name  string
		edit  func(*Account)
		code  string
		field string
	}{
		{"missing id", func(a *Account) { a.ID = uuid.Nil }, CodeMissingID, "accountId"},
		{"missing username", func(a *Account) { a.Username = "   " }, CodeMissingUsername, "username"},
		{"username with space", func(a *Account) { a.Username = "al ice" }, CodeInvalidUsername, "username"},
		{"missing display name", func(a *Account) { a.DisplayName = "" }, CodeMissingDisplayName, "displayName"},
		{"unknown kind", func(a *Account) { a.Kind = "robot" }, CodeInvalidAccountKind, "kind"},
	}
	for _, test := range invalid {
		account := valid
		test.edit(&account)
		err := account.Validate()
		if !HasCode(err, test.code) {
			t.Fatalf("%s: err = %v, want code %s", test.name, err, test.code)
		}
		if errors := FieldErrors(err); len(errors) == 0 || errors[0].Field != test.field {
			t.Fatalf("%s: fields = %+v, want first %s", test.name, errors, test.field)
		}
	}
}

func TestRoleAndProfileValidate(t *testing.T) {
	role := Role{ID: uuid.New(), Name: "  Support  ", Instruction: "  be kind  "}
	normalized := role.Normalized()
	if normalized.Name != "Support" || normalized.Instruction != "be kind" {
		t.Fatalf("normalized role = %+v", normalized)
	}
	if err := role.Validate(); err != nil {
		t.Fatalf("valid role rejected: %v", err)
	}
	if err := (Role{Name: "x"}).Validate(); !HasCode(err, CodeMissingID) {
		t.Fatalf("role without id: %v", err)
	}
	if err := (Role{ID: uuid.New()}).Validate(); !HasCode(err, CodeMissingName) {
		t.Fatalf("role without name: %v", err)
	}

	profile := Profile{ID: uuid.New(), AccountID: uuid.New(), RoleID: uuid.New()}
	if err := profile.Validate(); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
	if err := (Profile{RoleID: uuid.New()}).Validate(); !HasCode(err, CodeMissingID) {
		t.Fatalf("profile without id: %v", err)
	}
	if err := (Profile{ID: uuid.New(), RoleID: uuid.New()}).Validate(); !HasCode(err, CodeMissingAccount) {
		t.Fatalf("profile without account: %v", err)
	}
	if err := (Profile{ID: uuid.New(), AccountID: uuid.New()}).Validate(); !HasCode(err, CodeMissingRole) {
		t.Fatalf("profile without role: %v", err)
	}
}

func validBot() Bot {
	return Bot{
		ID:           "support-1",
		Name:         "Support One",
		ProfileID:    uuid.New(),
		ModelKind:    ModelKindText,
		DefaultModel: ModelRef{ProviderID: "magpie", ModelID: "qwen"},
		Harness: Harness{
			Tools: []string{"bash", "mcp", "tasks"},
			Context: HarnessContext{
				Instruction: "Help customers.",
				Permissions: []string{"session.read", "session.write"},
			},
		},
	}
}

func TestBotValidate(t *testing.T) {
	bot := validBot()
	if err := bot.Validate(); err != nil {
		t.Fatalf("valid bot rejected: %v", err)
	}
	normalized := bot.Normalized()
	if normalized.ModelKind != ModelKindText {
		t.Fatalf("model kind = %q", normalized.ModelKind)
	}
	caseFolded := bot
	caseFolded.ModelKind = "  TEXT  "
	if err := caseFolded.Validate(); err != nil {
		t.Fatalf("case-folded model kind rejected: %v", err)
	}
	for _, kind := range KnownModelKinds() {
		candidate := bot
		candidate.ModelKind = kind
		if err := candidate.Validate(); err != nil {
			t.Fatalf("model kind %q rejected: %v", kind, err)
		}
	}

	invalid := []struct {
		name string
		edit func(*Bot)
		code string
	}{
		{"missing id", func(b *Bot) { b.ID = "" }, CodeMissingID},
		{"invalid id", func(b *Bot) { b.ID = "has space" }, CodeInvalidID},
		{"missing name", func(b *Bot) { b.Name = " " }, CodeMissingName},
		{"missing profile", func(b *Bot) { b.ProfileID = uuid.Nil }, CodeMissingProfile},
		{"missing model kind", func(b *Bot) { b.ModelKind = "" }, CodeMissingModelKind},
		{"unknown model kind", func(b *Bot) { b.ModelKind = "video" }, CodeInvalidModelKind},
		{"missing provider", func(b *Bot) { b.DefaultModel.ProviderID = "" }, CodeIncompleteModel},
		{"missing model", func(b *Bot) { b.DefaultModel.ModelID = " " }, CodeIncompleteModel},
		{"empty tool", func(b *Bot) { b.Harness.Tools = []string{"bash", " "} }, CodeMissingTool},
		{"duplicate tool", func(b *Bot) { b.Harness.Tools = []string{"bash", "bash"} }, CodeDuplicateTool},
		{"empty permission", func(b *Bot) { b.Harness.Context.Permissions = []string{""} }, CodeInvalidPermissionKey},
		{"bad permission", func(b *Bot) { b.Harness.Context.Permissions = []string{"Session.Read"} }, CodeInvalidPermissionKey},
		{"bare permission", func(b *Bot) { b.Harness.Context.Permissions = []string{"session"} }, CodeInvalidPermissionKey},
		{"duplicate permission", func(b *Bot) {
			b.Harness.Context.Permissions = []string{"session.read", "session.read"}
		}, CodeDuplicatePermission},
	}
	for _, test := range invalid {
		candidate := bot
		test.edit(&candidate)
		if err := candidate.Validate(); !HasCode(err, test.code) {
			t.Fatalf("%s: err = %v, want code %s", test.name, err, test.code)
		}
	}
}

func TestBotValidateWithProfile(t *testing.T) {
	bot := validBot()
	if err := bot.ValidateWithProfile(func(id uuid.UUID) bool { return id == bot.ProfileID }); err != nil {
		t.Fatalf("existing profile rejected: %v", err)
	}
	if err := bot.ValidateWithProfile(func(uuid.UUID) bool { return false }); !HasCode(err, CodeMissingProfile) {
		t.Fatalf("missing profile: %v", err)
	}
	// A nil lookup is shape validation only.
	if err := bot.ValidateWithProfile(nil); err != nil {
		t.Fatalf("nil lookup rejected: %v", err)
	}
}

func TestGenerateIDsUseTheHandleGrammar(t *testing.T) {
	for _, generated := range []string{GenerateBotID(), GenerateTeamBotID()} {
		if !idPattern.MatchString(generated) {
			t.Fatalf("generated id %q does not satisfy the handle grammar", generated)
		}
	}
	if got := GenerateBotID(); len(got) <= len("bot_") || got[:4] != "bot_" {
		t.Fatalf("GenerateBotID = %q", got)
	}
	if got := GenerateTeamBotID(); len(got) <= len("team_") || got[:5] != "team_" {
		t.Fatalf("GenerateTeamBotID = %q", got)
	}
}
