package agentteam

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// testTeam is the smallest legal team: one captain, no members.
func testTeam() *AgentTeam {
	return NewTeam("team_product", "产品小队", TeamMember{AgentID: "agent_product_lead", Label: "队长", Description: "统筹与交付"})
}

func TestTeamWithOnlyCaptainIsValid(t *testing.T) {
	team := testTeam()
	requireNoError(t, team.Validate())
	if team.Status() != StatusValid {
		t.Fatalf("status = %q, want %q", team.Status(), StatusValid)
	}
	if team.Members != nil {
		t.Fatalf("a captain-only team must have 0 members, got %v", team.Members)
	}
	if team.Revision() != 0 || team.Sealed() {
		t.Fatalf("a new team must be unsealed, got revision %d sealed=%v", team.Revision(), team.Sealed())
	}
}

func TestTeamRequiresExactlyOneCaptain(t *testing.T) {
	team := NewTeam("team_product", "产品小队", TeamMember{})
	requireCode(t, team.Validate(), "captain.agentId", CodeMissingCaptain)
	if team.Status() != StatusInvalid {
		t.Fatalf("status = %q, want %q", team.Status(), StatusInvalid)
	}
}

func TestTeamSeatingRules(t *testing.T) {
	team := testTeam()
	team.Members = []TeamMember{{AgentID: "agent_design_lead"}, {AgentID: "agent_engineer"}}
	requireNoError(t, team.Validate())
	if team.Status() != StatusValid {
		t.Fatalf("status = %q, want valid for two distinct members", team.Status())
	}

	duplicate := testTeam()
	duplicate.Members = []TeamMember{{AgentID: "agent_design_lead"}, {AgentID: "agent_design_lead"}}
	requireCode(t, duplicate.Validate(), "members[1].agentId", CodeDuplicateMember)

	captainSeated := testTeam()
	captainSeated.Members = []TeamMember{{AgentID: captainSeated.Captain.AgentID}}
	requireCode(t, captainSeated.Validate(), "members[0].agentId", CodeCaptainInMembers)
	if captainSeated.Status() != StatusInvalid {
		t.Fatalf("status = %q, want invalid", captainSeated.Status())
	}

	blankSeat := testTeam()
	blankSeat.Members = []TeamMember{{Label: "空缺"}}
	requireCode(t, blankSeat.Validate(), "members[0].agentId", CodeMissingAgent)
}

func TestTeamMemberLabelIsEditable(t *testing.T) {
	suggested := MemberFromAgent(Agent{ID: "agent_design_lead", Name: "设计主理人", Description: "视觉方向"})
	if suggested.Label != "设计主理人" || suggested.Description != "视觉方向" {
		t.Fatalf("suggestion = %+v, want the agent name and description", suggested)
	}

	suggested.Label = "视觉负责人"
	suggested.Description = "组件规范与评审"
	team := testTeam()
	team.Members = []TeamMember{suggested}
	requireNoError(t, team.Validate())

	blob, err := json.Marshal(team)
	if err != nil {
		t.Fatalf("marshal team: %v", err)
	}
	var decoded AgentTeam
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatalf("unmarshal team: %v", err)
	}
	if got := decoded.Members[0].Label; got != "视觉负责人" {
		t.Fatalf("label = %q, want the edited value", got)
	}
	if got := decoded.Members[0].Description; got != "组件规范与评审" {
		t.Fatalf("description = %q, want the edited value", got)
	}
	if got := suggested.DisplayLabel("兜底"); got != "视觉负责人" {
		t.Fatalf("DisplayLabel = %q, want the edited label", got)
	}

	cleared := TeamMember{AgentID: "agent_x"}
	if got := cleared.DisplayLabel("agent_x"); got != "agent_x" {
		t.Fatalf("DisplayLabel fallback = %q, want agent_x", got)
	}
	team.Members = []TeamMember{cleared}
	requireNoError(t, team.Validate())
}

func TestTeamDefaults(t *testing.T) {
	team := testTeam()
	team.Defaults.Tools = []string{"bash", "read", "write"}
	requireNoError(t, team.Validate())

	team.Defaults.Tools = []string{"bash", "bash"}
	requireCode(t, team.Validate(), "defaults.tools[1]", CodeDuplicateTool)

	team.Defaults.Tools = []string{" "}
	requireCode(t, team.Validate(), "defaults.tools[0]", CodeMissingTool)

	team.Defaults.Tools = nil
	team.Defaults.ModelOverride = &ModelBinding{ProviderID: "prov_ds"}
	requireCode(t, team.Validate(), "defaults.modelOverride", CodeIncompleteModel)

	team.Defaults.ModelOverride = &ModelBinding{}
	requireCode(t, team.Validate(), "defaults.modelOverride", CodeIncompleteModel)

	team.Defaults.ModelOverride = &ModelBinding{ProviderID: "prov_ds", ModelID: "deepseek-v4.1"}
	requireNoError(t, team.Validate())
}

func TestTeamCapabilitiesTeamCLI(t *testing.T) {
	team := testTeam()
	team.Capabilities.TeamCLI = TeamCLI{
		Enabled:  true,
		Commands: []string{TeamCLIMemberAdd, TeamCLIMemberRemove, TeamCLISave, TeamCLIStart},
	}
	requireNoError(t, team.Validate())
	if !team.Capabilities.TeamCLI.Allows(TeamCLIMemberAdd) {
		t.Fatal("member.add should be allowed")
	}
	if team.Capabilities.TeamCLI.Allows(TeamCLIDelete) {
		t.Fatal("delete was not allowlisted")
	}

	team.Capabilities.TeamCLI = TeamCLI{Enabled: true}
	requireCode(t, team.Validate(), "capabilities.teamCli.commands", CodeTeamCLINoCommands)

	team.Capabilities.TeamCLI = TeamCLI{Enabled: false, Commands: []string{TeamCLISave}}
	requireCode(t, team.Validate(), "capabilities.teamCli.commands", CodeTeamCLICommandsWhenDisabled)
}

func TestTeamArchivedIsInvalid(t *testing.T) {
	team := testTeam()
	team.Archived = true
	requireCode(t, team.Validate(), "archived", CodeTeamArchived)
	if team.Status() != StatusInvalid {
		t.Fatalf("status = %q, want %q", team.Status(), StatusInvalid)
	}
}

func TestTeamValidateWithAgents(t *testing.T) {
	known := map[string]bool{"agent_product_lead": true, "agent_design_lead": true}
	exists := func(agentID string) bool { return known[agentID] }

	team := testTeam()
	team.Members = []TeamMember{{AgentID: "agent_design_lead"}}
	requireNoError(t, team.ValidateWithAgents(exists))

	team.Members = append(team.Members, TeamMember{AgentID: "agent_ghost"})
	requireCode(t, team.ValidateWithAgents(exists), "members[1].agentId", CodeAgentNotFound)

	ghostCaptain := testTeam()
	ghostCaptain.Captain = TeamMember{AgentID: "agent_ghost"}
	requireCode(t, ghostCaptain.ValidateWithAgents(exists), "captain.agentId", CodeAgentNotFound)
	requireNoError(t, ghostCaptain.Validate()) // shape-only validation cannot see the library

	// An inline definition is self-contained and is not looked up.
	inline := testTeam()
	inline.Members = []TeamMember{{AgentID: "agent_inline", Inline: &Agent{ID: "agent_inline", Name: "内联工程师"}}}
	requireNoError(t, inline.ValidateWithAgents(exists))
}

func TestTeamInlineMember(t *testing.T) {
	team := testTeam()
	team.Members = []TeamMember{{AgentID: "agent_inline", Inline: &Agent{ID: "agent_inline", Name: "内联工程师"}}}
	requireNoError(t, team.Validate())

	team.Members[0].Inline = &Agent{ID: "agent_other", Name: "内联工程师"}
	requireCode(t, team.Validate(), "members[0].inline.id", CodeAgentMismatch)

	team.Members[0].Inline = &Agent{ID: "agent_inline"}
	requireCode(t, team.Validate(), "members[0].inline.name", CodeMissingName)
}

func TestTeamHashIsStable(t *testing.T) {
	one := testTeam()
	one.Members = []TeamMember{{AgentID: "agent_design_lead"}, {AgentID: "agent_engineer"}}
	one.Defaults.Tools = []string{"write", "bash", "read"}
	one.Capabilities.TeamCLI = TeamCLI{Enabled: true, Commands: []string{TeamCLISave, TeamCLIMemberAdd}}

	two := testTeam()
	two.Members = []TeamMember{{AgentID: "agent_design_lead"}, {AgentID: "agent_engineer"}}
	two.Defaults.Tools = []string{"bash", "read", "write"} // same set, other order
	two.Capabilities.TeamCLI = TeamCLI{Enabled: true, Commands: []string{TeamCLIMemberAdd, TeamCLISave}}

	if one.Hash() == "" || two.Hash() == "" {
		t.Fatal("a valid team must have a content hash")
	}
	if one.Hash() != two.Hash() {
		t.Fatalf("set-like ordering changed the hash: %s != %s", one.Hash(), two.Hash())
	}

	edited := *one
	edited.Members = []TeamMember{{AgentID: "agent_design_lead", Label: "设计"}, {AgentID: "agent_engineer"}}
	if edited.Hash() == one.Hash() {
		t.Fatal("editing a member label must change the content hash")
	}

	// The hash covers content only: sealing unchanged content keeps it.
	before := one.Hash()
	one.Seal()
	one.Seal()
	if one.Hash() != before {
		t.Fatalf("re-sealing changed the content hash: %s != %s", one.Hash(), before)
	}
	if one.Revision() != 2 {
		t.Fatalf("revision = %d, want 2 after two seals", one.Revision())
	}
	if !one.Sealed() {
		t.Fatal("a sealed team must report Sealed()")
	}
	if len(one.Hash()) != 64 {
		t.Fatalf("hash = %q, want a 64 character sha256 hex digest", one.Hash())
	}
}

func TestTeamSealDetectsEditsAndForgeries(t *testing.T) {
	team := testTeam()
	team.Seal()
	requireNoError(t, team.Validate())

	// An edit that bypasses Seal leaves a stale hash: rejected, not trusted.
	team.Name = "改名后的团队"
	requireCode(t, team.Validate(), "hash", CodeHashMismatch)
	if team.Status() != StatusInvalid {
		t.Fatalf("status = %q, want invalid for a stale seal", team.Status())
	}

	// Sealing again restores validity and moves the revision forward.
	team.Seal()
	requireNoError(t, team.Validate())
	if team.Revision() != 2 {
		t.Fatalf("revision = %d, want 2", team.Revision())
	}
}

func TestTeamJSONRoundTripAndTamper(t *testing.T) {
	team := testTeam()
	team.Members = []TeamMember{MemberFromAgent(Agent{ID: "agent_design_lead", Name: "设计主理人"})}
	team.Capabilities.TeamCLI = TeamCLI{Enabled: true, Commands: []string{TeamCLIMemberAdd, TeamCLISave}}
	team.Seal()

	blob, err := json.Marshal(team)
	if err != nil {
		t.Fatalf("marshal team: %v", err)
	}
	var decoded AgentTeam
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatalf("unmarshal team: %v", err)
	}
	if decoded.Revision() != team.Revision() {
		t.Fatalf("revision = %d, want %d", decoded.Revision(), team.Revision())
	}
	if decoded.Hash() != team.Hash() {
		t.Fatalf("hash = %s, want %s", decoded.Hash(), team.Hash())
	}
	requireNoError(t, decoded.Validate())

	// A hand-edited hash cannot pass as sealed.
	var fields map[string]any
	if err := json.Unmarshal(blob, &fields); err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	fields["hash"] = strings.Repeat("0", 64)
	forged := decodeTampered(t, fields)
	requireCode(t, forged.Validate(), "hash", CodeHashMismatch)

	// A revision without a hash is inconsistent, so a caller cannot just bump it.
	delete(fields, "hash")
	fields["revision"] = float64(7)
	revisionForged := decodeTampered(t, fields)
	requireCode(t, revisionForged.Validate(), "revision", CodeRevisionInconsistent)
}

// decodeTampered re-encodes a mutated wire object and decodes it as a team.
func decodeTampered(t *testing.T, fields map[string]any) AgentTeam {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal tampered fields: %v", err)
	}
	var team AgentTeam
	if err := json.Unmarshal(raw, &team); err != nil {
		t.Fatalf("unmarshal tampered team: %v", err)
	}
	return team
}

// TestTeamSealIsNotCallerWritable pins the seal's structure: neither the
// revision nor the hash (Go or JSON name) is an exported field, so a caller
// cannot pin them with a struct literal.
func TestTeamSealIsNotCallerWritable(t *testing.T) {
	typ := reflect.TypeOf(AgentTeam{})
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		switch strings.ToLower(field.Name) {
		case "hash", "revision", "storedhash":
			if field.IsExported() {
				t.Fatalf("%s is exported; the seal must not be caller writable", field.Name)
			}
		}
	}
}

func TestTeamUnencodableConfigHasNoHash(t *testing.T) {
	team := testTeam()
	team.Defaults.ModelOverride = &ModelBinding{
		ProviderID: "prov_ds",
		ModelID:    "deepseek-v4.1",
		Tuning:     map[string]any{"hook": func() {}},
	}
	requireCode(t, team.Validate(), "defaults.modelOverride.tuning", CodeUnencodableConfig)
	if team.Hash() != "" {
		t.Fatalf("hash = %q, want empty for unencodable content", team.Hash())
	}
}

func TestTeamReportsFieldLevelErrors(t *testing.T) {
	team := NewTeam("", "", TeamMember{})
	team.Members = []TeamMember{{Label: "没有 agent"}, {AgentID: "agent_dup"}, {AgentID: "agent_dup"}}
	team.Capabilities.TeamCLI = TeamCLI{Enabled: true, Commands: []string{"member.promote"}}
	team.Archived = true

	err := team.Validate()
	if err == nil {
		t.Fatal("expected validation failures")
	}
	failures := FieldErrors(err)
	if len(failures) < 6 {
		t.Fatalf("expected at least 6 field failures, got %d: %v", len(failures), failures)
	}
	expected := [][2]string{
		{"id", CodeMissingID},
		{"name", CodeMissingName},
		{"captain.agentId", CodeMissingCaptain},
		{"members[0].agentId", CodeMissingAgent},
		{"members[2].agentId", CodeDuplicateMember},
		{"capabilities.teamCli.commands[0]", CodeInvalidTeamCLICommand},
		{"archived", CodeTeamArchived},
	}
	for _, want := range expected {
		if !HasCode(err, want[1]) {
			t.Errorf("missing code %s in %v", want[1], failures)
		}
		found := false
		for _, failure := range failures {
			if failure.Field == want[0] && failure.Code == want[1] {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing field error %s (%s) in %v", want[0], want[1], failures)
		}
	}
}

// TestTeamFeatureBindings pins the design field the implementation was missing:
// featureBindings is a real, validated, hashed part of the team preset.
func TestTeamFeatureBindings(t *testing.T) {
	team := testTeam()
	team.FeatureBindings = []FeatureBinding{{Port: "team.assign", CLI: "keel team assign"}}
	requireNoError(t, team.Validate())

	// Ports follow the provider feature-port vocabulary and bind once.
	team.FeatureBindings = []FeatureBinding{{Port: "Team Assign"}}
	requireCode(t, team.Validate(), "featureBindings[0].port", CodeInvalidFeaturePort)
	team.FeatureBindings = []FeatureBinding{{Port: "team.assign"}, {Port: "team.assign"}}
	requireCode(t, team.Validate(), "featureBindings[1].port", CodeDuplicateFeaturePort)
	team.FeatureBindings = []FeatureBinding{{Port: "team.assign", CLI: "team assign"}}
	requireCode(t, team.Validate(), "featureBindings[0].cli", CodeInvalidFeatureCLI)

	// A binding is content: adding one changes the hash.
	plain := testTeam()
	bound := testTeam()
	bound.FeatureBindings = []FeatureBinding{{Port: "team.assign", CLI: "keel team assign"}}
	if plain.Hash() == bound.Hash() {
		t.Fatal("adding a feature binding did not change the content hash")
	}

	// The bindings travel through the persisted wire shape.
	bound.Seal()
	blob, err := json.Marshal(bound)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded AgentTeam
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.FeatureBindings) != 1 || decoded.FeatureBindings[0].Port != "team.assign" {
		t.Fatalf("round trip lost the bindings: %+v", decoded.FeatureBindings)
	}
	requireNoError(t, decoded.Validate())
}

// TestTeamNormalizationIsTheSingleHashInput pins the one normalization point:
// the same semantic team hashes the same whether or not the caller normalized
// it first, and Seal stores the normalized form.
func TestTeamNormalizationIsTheSingleHashInput(t *testing.T) {
	raw := testTeam()
	raw.Name = "  产品小队  "
	raw.Captain.Label = "  队长  "
	raw.Members = []TeamMember{{
		AgentID: "  agent_inline  ",
		Inline:  &Agent{ID: "agent_inline", Name: "内联工程师"},
	}}
	normalized := raw.Normalized()
	if normalized.Name != "产品小队" || normalized.Captain.Label != "队长" || normalized.Members[0].AgentID != "agent_inline" {
		t.Fatalf("Normalized() = %+v", normalized)
	}
	if normalized.Members[0].Inline.Avatar.Kind != AvatarIdenticon || normalized.Members[0].Inline.Avatar.Seed != "agent_inline" {
		t.Fatalf("inline avatar was not defaulted: %+v", normalized.Members[0].Inline.Avatar)
	}
	if raw.Hash() != normalized.Hash() {
		t.Fatalf("normalization changed the hash: %s != %s", raw.Hash(), normalized.Hash())
	}

	// Seal persists the normalized form, so a later read validates and hashes
	// identically.
	raw.Seal()
	requireNoError(t, raw.Validate())
	if raw.Name != "产品小队" || raw.Captain.Label != "队长" {
		t.Fatalf("Seal did not normalize in place: %+v", raw)
	}
}

// TestTeamValidateForStart pins the start gate: a draft validates, but a run
// needs a sealed team so the manifest can freeze a revision and hash.
func TestTeamValidateForStart(t *testing.T) {
	team := testTeam()
	requireNoError(t, team.Validate())
	requireCode(t, team.ValidateForStart(nil), "revision", CodeNotSealed)

	team.Seal()
	requireNoError(t, team.ValidateForStart(nil))

	// A stale seal is still a validation failure at start time.
	team.Name = "改名后未封存"
	requireCode(t, team.ValidateForStart(nil), "hash", CodeHashMismatch)

	team.Seal()
	requireCode(t, team.ValidateForStart(func(agentID string) bool { return false }), "captain.agentId", CodeAgentNotFound)
}
