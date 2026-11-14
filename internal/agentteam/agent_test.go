package agentteam

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// requireNoError fails the test when err is not nil.
func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// requireCode fails the test unless err carries a field error with field and code.
func requireCode(t *testing.T, err error, field, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected field error %s (%s), got nil", field, code)
	}
	for _, failure := range FieldErrors(err) {
		if failure.Field == field && failure.Code == code {
			return
		}
	}
	t.Fatalf("expected field error %s (%s), got %v", field, code, FieldErrors(err))
}

// validAgent is the smallest agent the specs allow: a name and a model.
func validAgent() Agent {
	return Agent{ID: "agent_design_lead", Name: "设计主理人"}
}

func TestAgentMinimalIsValid(t *testing.T) {
	agent := validAgent()
	requireNoError(t, agent.Validate())

	normalized := agent.Normalized()
	if normalized.Avatar.Kind != AvatarIdenticon {
		t.Fatalf("avatar kind = %q, want %q", normalized.Avatar.Kind, AvatarIdenticon)
	}
	if normalized.Avatar.Seed != agent.ID {
		t.Fatalf("avatar seed = %q, want the agent id %q", normalized.Avatar.Seed, agent.ID)
	}
}

func TestAgentIdentityErrors(t *testing.T) {
	blank := Agent{Name: "无 id"}
	requireCode(t, blank.Validate(), "id", CodeMissingID)

	bad := Agent{ID: "-leading-dash", Name: "x"}
	requireCode(t, bad.Validate(), "id", CodeInvalidID)

	nameless := Agent{ID: "agent_x", Name: "   "}
	requireCode(t, nameless.Validate(), "name", CodeMissingName)
}

// TestAgentTypesHaveNoCredentialSlot proves structurally that an agent config
// cannot carry a credential: no exported field, in Go or in JSON, is
// credential-shaped. Credentials belong to the provider layer by id reference.
func TestAgentTypesHaveNoCredentialSlot(t *testing.T) {
	samples := []any{Agent{}, Avatar{}, ModelBinding{}, ToolGrant{}, SystemPrompt{}, TeamCLI{}, TeamMember{}, AgentTeam{}, AgentTeam{}.Captain}
	for _, sample := range samples {
		typ := reflect.TypeOf(sample)
		for index := 0; index < typ.NumField(); index++ {
			field := typ.Field(index)
			jsonName := strings.Split(field.Tag.Get("json"), ",")[0]
			for _, name := range []string{field.Name, jsonName} {
				if name == "" || name == "-" {
					continue
				}
				if credentialShapedKey(name) {
					t.Errorf("%s.%s (json %q) is credential-shaped; credentials must be referenced, never stored", typ.Name(), field.Name, name)
				}
			}
		}
	}
}

// TestAgentJSONCarriesNoInlineCredential proves behaviorally that a fully
// populated agent serializes without a single credential-shaped key.
func TestAgentJSONCarriesNoInlineCredential(t *testing.T) {
	agent := validAgent()
	agent.DefaultModel = ModelBinding{
		ProviderID: "prov_ds",
		ModelID:    "deepseek-v4.1",
		Tuning:     map[string]any{"reasoning": "high", "maxOutputTokens": 4096},
	}
	agent.Tools = []ToolGrant{{ToolID: "bash", Enabled: true, Config: map[string]any{"timeoutMs": 60000}}}
	agent.SystemPrompt = SystemPrompt{Kind: SystemPromptFile, Ref: "AGENTS.md"}
	agent.TeamCLI = TeamCLI{Enabled: true, Commands: []string{TeamCLIMemberAdd}}
	agent.Skills = []string{"design-review"}
	requireNoError(t, agent.Validate())

	blob, err := json.Marshal(agent)
	if err != nil {
		t.Fatalf("marshal agent: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatalf("unmarshal agent: %v", err)
	}
	found := credentialKeys(decoded, "")
	sort.Strings(found)
	if len(found) != 0 {
		t.Fatalf("agent JSON carries credential-shaped keys: %v", found)
	}
}

// credentialKeys walks decoded JSON and returns the paths of credential-shaped keys.
func credentialKeys(value any, prefix string) []string {
	var found []string
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if credentialShapedKey(key) {
				found = append(found, prefix+key)
				continue
			}
			found = append(found, credentialKeys(typed[key], prefix+key+".")...)
		}
	case []any:
		for index, item := range typed {
			found = append(found, credentialKeys(item, fmt.Sprintf("%s[%d].", prefix, index))...)
		}
	}
	return found
}

func TestAgentRejectsInlineCredentials(t *testing.T) {
	agent := validAgent()
	agent.DefaultModel = ModelBinding{
		ProviderID: "prov_ds",
		ModelID:    "deepseek-v4.1",
		Tuning:     map[string]any{"apiKey": "sk-live-not-a-real-key"},
	}
	requireCode(t, agent.Validate(), "defaultModel.tuning.apiKey", CodeInlineCredential)

	agent.DefaultModel.Tuning = map[string]any{"headers": map[string]any{"Authorization": "Bearer sk-live"}}
	requireCode(t, agent.Validate(), "defaultModel.tuning.headers.Authorization", CodeInlineCredential)

	agent.DefaultModel.Tuning = map[string]any{"reasoning": "high", "maxOutputTokens": 4096}
	requireNoError(t, agent.Validate())
}

func TestAgentRejectsUnencodableTuning(t *testing.T) {
	agent := validAgent()
	agent.DefaultModel = ModelBinding{
		ProviderID: "prov_ds",
		ModelID:    "deepseek-v4.1",
		Tuning:     map[string]any{"hook": func() {}},
	}
	requireCode(t, agent.Validate(), "defaultModel.tuning", CodeUnencodableConfig)
}

func TestAgentModelBindingHalves(t *testing.T) {
	agent := validAgent()
	requireNoError(t, agent.Validate()) // both empty = inherit

	agent.DefaultModel = ModelBinding{ProviderID: "prov_ds"}
	requireCode(t, agent.Validate(), "defaultModel", CodeIncompleteModel)

	agent.DefaultModel = ModelBinding{ModelID: "deepseek-v4.1"}
	requireCode(t, agent.Validate(), "defaultModel", CodeIncompleteModel)

	agent.DefaultModel = ModelBinding{ProviderID: "prov_ds", ModelID: "deepseek-v4.1"}
	requireNoError(t, agent.Validate())
}

func TestAgentToolGrants(t *testing.T) {
	agent := validAgent()
	agent.Tools = []ToolGrant{{ToolID: "bash", Enabled: true}, {ToolID: "read", Enabled: true}}
	requireNoError(t, agent.Validate())

	agent.Tools = append(agent.Tools, ToolGrant{ToolID: "bash"})
	requireCode(t, agent.Validate(), "tools[2].toolId", CodeDuplicateTool)

	agent.Tools = []ToolGrant{{ToolID: "  "}}
	requireCode(t, agent.Validate(), "tools[0].toolId", CodeMissingTool)

	agent.Tools = []ToolGrant{{ToolID: "bash", Config: map[string]any{"token": "x"}}}
	requireCode(t, agent.Validate(), "tools[0].config.token", CodeInlineCredential)
}

func TestAgentSystemPrompt(t *testing.T) {
	agent := validAgent()

	agent.SystemPrompt = SystemPrompt{Kind: SystemPromptInline, Text: "You are the design lead."}
	requireNoError(t, agent.Validate())

	agent.SystemPrompt = SystemPrompt{Kind: SystemPromptInline}
	requireCode(t, agent.Validate(), "systemPrompt.text", CodeMissingSystemPrompt)

	agent.SystemPrompt = SystemPrompt{Kind: SystemPromptFile, Ref: "AGENTS.md"}
	requireNoError(t, agent.Validate())

	agent.SystemPrompt = SystemPrompt{Kind: SystemPromptFile, Ref: "../secrets/AGENTS.md"}
	requireCode(t, agent.Validate(), "systemPrompt.ref", CodeUnsafeSystemPromptRef)

	agent.SystemPrompt = SystemPrompt{Kind: SystemPromptFile, Ref: "/etc/passwd"}
	requireCode(t, agent.Validate(), "systemPrompt.ref", CodeUnsafeSystemPromptRef)

	agent.SystemPrompt = SystemPrompt{Kind: SystemPromptFile, Ref: "AGENTS.md", Text: "duplicated"}
	requireCode(t, agent.Validate(), "systemPrompt.text", CodeConflictingSystemPrompt)

	agent.SystemPrompt = SystemPrompt{Text: "text without a kind"}
	requireCode(t, agent.Validate(), "systemPrompt.kind", CodeInvalidSystemPromptKind)

	agent.SystemPrompt = SystemPrompt{Kind: "remote"}
	requireCode(t, agent.Validate(), "systemPrompt.kind", CodeInvalidSystemPromptKind)
}

func TestAgentTeamCLIAllowlist(t *testing.T) {
	agent := validAgent()
	agent.TeamCLI = TeamCLI{Enabled: true, Commands: []string{TeamCLIMemberAdd, TeamCLIMemberRemove}}
	requireNoError(t, agent.Validate())
	if !agent.TeamCLI.Allows(TeamCLIMemberAdd) {
		t.Fatal("member.add should be allowed")
	}
	if agent.TeamCLI.Allows(TeamCLIList) {
		t.Fatal("list was not allowlisted")
	}

	agent.TeamCLI = TeamCLI{Enabled: false, Commands: []string{TeamCLIMemberAdd}}
	requireCode(t, agent.Validate(), "teamCli.commands", CodeTeamCLICommandsWhenDisabled)
	if agent.TeamCLI.Allows(TeamCLIMemberAdd) {
		t.Fatal("a disabled team CLI must allow nothing, whatever the stale allowlist says")
	}

	agent.TeamCLI = TeamCLI{Enabled: true}
	requireCode(t, agent.Validate(), "teamCli.commands", CodeTeamCLINoCommands)

	agent.TeamCLI = TeamCLI{Enabled: true, Commands: []string{"member.promote"}}
	requireCode(t, agent.Validate(), "teamCli.commands[0]", CodeInvalidTeamCLICommand)

	agent.TeamCLI = TeamCLI{Enabled: true, Commands: []string{TeamCLIMemberAdd, TeamCLIMemberAdd}}
	requireCode(t, agent.Validate(), "teamCli.commands[1]", CodeDuplicateTeamCLICommand)
}

func TestAvatarValidation(t *testing.T) {
	cases := []struct {
		name   string
		avatar Avatar
		field  string
		code   string
	}{
		{name: "valid identicon", avatar: Avatar{Kind: AvatarIdenticon, Seed: "agent_x"}},
		{name: "empty kind is the identicon default", avatar: Avatar{Seed: "agent_x"}},
		{name: "invalid kind", avatar: Avatar{Kind: "gravatar", Seed: "agent_x"}, field: "avatar.kind", code: CodeInvalidAvatarKind},
		{name: "empty seed", avatar: Avatar{Kind: AvatarIdenticon}, field: "avatar.seed", code: CodeEmptyAvatarSeed},
		{name: "blank seed", avatar: Avatar{Kind: AvatarIdenticon, Seed: "   "}, field: "avatar.seed", code: CodeEmptyAvatarSeed},
		{name: "valid upload", avatar: Avatar{Kind: AvatarUpload, Ref: "blob:sha256:4f9c"}},
		{name: "upload without ref", avatar: Avatar{Kind: AvatarUpload}, field: "avatar.ref", code: CodeMissingAvatarRef},
		{name: "valid url", avatar: Avatar{Kind: AvatarURL, URL: "https://cdn.example.com/avatar.png"}},
		{name: "url without value", avatar: Avatar{Kind: AvatarURL}, field: "avatar.url", code: CodeInvalidAvatarURL},
		{name: "url with wrong scheme", avatar: Avatar{Kind: AvatarURL, URL: "ftp://example.com/a.png"}, field: "avatar.url", code: CodeInvalidAvatarURL},
		{name: "url without scheme", avatar: Avatar{Kind: AvatarURL, URL: "example.com/a.png"}, field: "avatar.url", code: CodeInvalidAvatarURL},
		{name: "url with credentials", avatar: Avatar{Kind: AvatarURL, URL: "https://user:pass@example.com/a.png"}, field: "avatar.url", code: CodeInlineCredential},
		{name: "identicon with a credential url leftover", avatar: Avatar{Kind: AvatarIdenticon, Seed: "s", URL: "https://user:pass@example.com/a.png"}, field: "avatar.url", code: CodeInlineCredential},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.avatar.Validate()
			if testCase.code == "" {
				requireNoError(t, err)
				return
			}
			requireCode(t, err, testCase.field, testCase.code)
		})
	}
}

// TestAgentRejectsBlankSeedEvenWithDefaults pins that the identicon default
// fills an empty seed but never launders an explicitly blank one.
func TestAgentRejectsBlankSeedEvenWithDefaults(t *testing.T) {
	agent := validAgent()
	agent.Avatar = Avatar{Kind: AvatarIdenticon, Seed: "   "}
	requireCode(t, agent.Validate(), "avatar.seed", CodeEmptyAvatarSeed)
}

func TestIdenticonIsDeterministic(t *testing.T) {
	first := IdenticonGrid("agent_design_lead")
	if again := IdenticonGrid("agent_design_lead"); again != first {
		t.Fatal("the same seed produced different grids")
	}
	if padded := IdenticonGrid("  agent_design_lead  "); padded != first {
		t.Fatal("seed trimming must not change the grid")
	}
	if other := IdenticonGrid("agent_engineer"); other == first {
		t.Fatal("different seeds produced the same grid")
	}
	for row := 0; row < IdenticonGridSize; row++ {
		for column := 0; column < IdenticonGridSize/2; column++ {
			if first[row][column] != first[row][IdenticonGridSize-1-column] {
				t.Fatalf("row %d is not mirrored", row)
			}
		}
	}
	rows := IdenticonRows("agent_design_lead")
	if len(rows) != IdenticonGridSize {
		t.Fatalf("got %d rows, want %d", len(rows), IdenticonGridSize)
	}
	for row, line := range rows {
		if len(line) != IdenticonGridSize {
			t.Fatalf("row %d = %q, want %d cells", row, line, IdenticonGridSize)
		}
		for column := 0; column < IdenticonGridSize; column++ {
			want := byte('.')
			if first[row][column] {
				want = '#'
			}
			if line[column] != want {
				t.Fatalf("row %d column %d = %q, want %q", row, column, line[column], want)
			}
		}
	}
}

// TestIdenticonDependsOnlyOnSeed pins that renaming an agent never changes its
// avatar: the picture follows the seed, and the seed is explicit.
func TestIdenticonDependsOnlyOnSeed(t *testing.T) {
	one := Agent{ID: "agent_one", Name: "第一个名字", Avatar: Avatar{Kind: AvatarIdenticon, Seed: "shared-seed"}}.Normalized()
	two := Agent{ID: "agent_two", Name: "另一个名字", Avatar: Avatar{Kind: AvatarIdenticon, Seed: "shared-seed"}}.Normalized()
	if IdenticonGrid(one.Avatar.SeedOr(one.ID)) != IdenticonGrid(two.Avatar.SeedOr(two.ID)) {
		t.Fatal("the same shared seed produced different identicons")
	}

	renamed := one
	renamed.Name = "改过的名字"
	if renamed.Normalized().Avatar.Seed != "shared-seed" {
		t.Fatalf("renaming changed the avatar seed to %q", renamed.Normalized().Avatar.Seed)
	}

	regenerated := Avatar{Kind: AvatarIdenticon, Seed: "new-seed"}
	if IdenticonGrid(regenerated.SeedOr(one.ID)) == IdenticonGrid(one.Avatar.SeedOr(one.ID)) {
		t.Fatal("a regenerated seed should produce a different identicon")
	}
}

func TestAgentReportsEveryFieldFailure(t *testing.T) {
	agent := Agent{
		ID:      "",
		Name:    "",
		Avatar:  Avatar{Kind: "gravatar"},
		Tools:   []ToolGrant{{ToolID: ""}, {ToolID: "bash"}, {ToolID: "bash"}},
		TeamCLI: TeamCLI{Enabled: true},
	}
	err := agent.Validate()
	if err == nil {
		t.Fatal("expected validation failures")
	}
	if failures := FieldErrors(err); len(failures) < 5 {
		t.Fatalf("expected at least 5 field failures, got %d: %v", len(failures), failures)
	}
	if !HasCode(err, CodeMissingID) || !HasCode(err, CodeDuplicateTool) || !HasCode(err, CodeTeamCLINoCommands) {
		t.Fatalf("aggregated error lost codes: %v", FieldErrors(err))
	}
	if !strings.Contains(err.Error(), "invalid fields") {
		t.Fatalf("aggregated error should name the count, got %q", err.Error())
	}
}

// TestGenerateAgentID pins the create path the spec promises: an agent can be
// created from a name alone, and the system mints a stable id that satisfies the
// same grammar a client-supplied id does.
func TestGenerateAgentID(t *testing.T) {
	first := GenerateAgentID()
	second := GenerateAgentID()
	if first == second {
		t.Fatalf("two generated ids are equal: %q", first)
	}
	for _, id := range []string{first, second} {
		if !strings.HasPrefix(id, "agent_") {
			t.Errorf("id %q does not use the agent_ prefix", id)
		}
		agent := Agent{ID: id, Name: "生成的名字"}
		requireNoError(t, agent.Validate())
	}

	// WithGeneratedID fills an empty id and leaves an explicit one alone.
	minted := Agent{Name: "只有名字"}.WithGeneratedID()
	if minted.ID == "" || minted.Avatar.Kind != AvatarIdenticon || minted.Avatar.Seed != minted.ID {
		t.Fatalf("WithGeneratedID = %+v, want a minted id and a seeded identicon", minted)
	}
	explicit := Agent{ID: "agent_explicit", Name: "显式 id"}.WithGeneratedID()
	if explicit.ID != "agent_explicit" {
		t.Fatalf("WithGeneratedID rewrote an explicit id: %q", explicit.ID)
	}
}

// TestAgentValidateWithTools pins the save-time tool existence hook: an unknown
// toolId fails with tool_not_found while the reference is kept, and a nil lookup
// behaves like shape-only validation.
func TestAgentValidateWithTools(t *testing.T) {
	agent := validAgent()
	agent.Tools = []ToolGrant{{ToolID: "bash", Enabled: true}, {ToolID: "keel.tasks", Enabled: true}}
	requireNoError(t, agent.ValidateWithTools(nil))

	known := map[string]bool{"bash": true}
	requireFieldError := func(err error, field, code string) {
		t.Helper()
		if err == nil {
			t.Fatalf("expected %s at %s, got nil", code, field)
		}
		for _, failure := range FieldErrors(err) {
			if failure.Field == field && failure.Code == code {
				return
			}
		}
		t.Fatalf("expected %s at %s, got %v", code, field, FieldErrors(err))
	}
	requireFieldError(agent.ValidateWithTools(func(toolID string) bool { return known[toolID] }), "tools[1].toolId", CodeToolNotFound)

	// The reference is kept: an uninstalled tool must not be silently dropped.
	if len(agent.Tools) != 2 {
		t.Fatalf("tools = %v, want the references kept", agent.Tools)
	}
}

// TestAgentRejectsCredentialsInArrays covers the array half of the shared
// scanner on the agent surface: tuning and tool config arrays are walked.
func TestAgentRejectsCredentialsInArrays(t *testing.T) {
	agent := validAgent()
	agent.DefaultModel = ModelBinding{
		ProviderID: "prov_ds",
		ModelID:    "deepseek-v4.1",
		Tuning:     map[string]any{"headers": []any{map[string]any{"apiKey": "sk-live-secret"}}},
	}
	requireCode(t, agent.Validate(), "defaultModel.tuning.headers[0].apiKey", CodeInlineCredential)

	agent = validAgent()
	agent.Tools = []ToolGrant{{
		ToolID: "bash",
		Config: map[string]any{"env": []any{map[string]any{"token": "abc"}}},
	}}
	requireCode(t, agent.Validate(), "tools[0].config.env[0].token", CodeInlineCredential)
}
