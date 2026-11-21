package botteam

import (
	"encoding/json"
	"testing"
)

func validTeamBot() *TeamBot {
	return &TeamBot{
		ID:      "support-team",
		Name:    "Support Team",
		Mission: "Resolve customer issues within 24 hours.",
		Roster: []RosterEntry{
			{BotID: "support-1", Role: RosterRoleCaptain},
			{BotID: "image-1", Role: RosterRoleMember},
		},
	}
}

func TestTeamBotValidate(t *testing.T) {
	team := validTeamBot()
	if err := team.Validate(); err != nil {
		t.Fatalf("valid team bot rejected: %v", err)
	}
	normalized := team.Normalized()
	if normalized.ID != "support-team" || normalized.Name != "Support Team" ||
		normalized.Mission != "Resolve customer issues within 24 hours." {
		t.Fatalf("normalized team bot = %+v", normalized)
	}

	invalid := []struct {
		name string
		edit func(*TeamBot)
		code string
	}{
		{"missing id", func(tb *TeamBot) { tb.ID = "" }, CodeMissingID},
		{"invalid id", func(tb *TeamBot) { tb.ID = "has space" }, CodeInvalidID},
		{"missing name", func(tb *TeamBot) { tb.Name = " " }, CodeMissingName},
		{"missing mission", func(tb *TeamBot) { tb.Mission = " " }, CodeMissingMission},
		{"empty roster", func(tb *TeamBot) { tb.Roster = nil }, CodeEmptyRoster},
		{"missing captain", func(tb *TeamBot) {
			tb.Roster = []RosterEntry{{BotID: "image-1", Role: RosterRoleMember}}
		}, CodeMissingCaptain},
		{"missing seat bot", func(tb *TeamBot) {
			tb.Roster = append(tb.Roster, RosterEntry{Role: RosterRoleMember})
		}, CodeMissingID},
		{"invalid seat role", func(tb *TeamBot) {
			tb.Roster = append(tb.Roster, RosterEntry{BotID: "ops-1", Role: "lead"})
		}, CodeInvalidRosterRole},
		{"empty seat role", func(tb *TeamBot) {
			tb.Roster = append(tb.Roster, RosterEntry{BotID: "ops-1"})
		}, CodeInvalidRosterRole},
		{"duplicate seat", func(tb *TeamBot) {
			tb.Roster = append(tb.Roster, RosterEntry{BotID: "support-1", Role: RosterRoleMember})
		}, CodeDuplicateRosterBot},
	}
	for _, test := range invalid {
		candidate := validTeamBot()
		test.edit(candidate)
		if err := candidate.Validate(); !HasCode(err, test.code) {
			t.Fatalf("%s: err = %v, want code %s", test.name, err, test.code)
		}
	}
}

func TestTeamBotValidateWithBots(t *testing.T) {
	team := validTeamBot()
	known := map[string]bool{"support-1": true, "image-1": true}
	if err := team.ValidateWithBots(func(id string) bool { return known[id] }); err != nil {
		t.Fatalf("known roster rejected: %v", err)
	}
	if err := team.ValidateWithBots(func(id string) bool { return id != "image-1" }); !HasCode(err, CodeBotNotFound) {
		t.Fatalf("unknown roster bot: %v", err)
	}
	if err := team.ValidateWithBots(nil); err != nil {
		t.Fatalf("nil lookup rejected: %v", err)
	}
}

func TestTeamBotHashIsDeterministic(t *testing.T) {
	first := validTeamBot()
	second := validTeamBot()
	if first.Hash() != second.Hash() {
		t.Fatalf("same content hashed differently: %s vs %s", first.Hash(), second.Hash())
	}

	reversed := validTeamBot()
	reversed.Roster = []RosterEntry{reversed.Roster[1], reversed.Roster[0]}
	if first.Hash() != reversed.Hash() {
		t.Fatalf("roster order changed the hash: %s vs %s", first.Hash(), reversed.Hash())
	}

	changed := validTeamBot()
	changed.Mission = "A different mission."
	if first.Hash() == changed.Hash() {
		t.Fatal("changing the mission did not change the hash")
	}

	archived := validTeamBot()
	archived.Archived = true
	if first.Hash() != archived.Hash() {
		t.Fatal("archiving changed the content hash")
	}

	// Pin the canonical vector: a cross-process or cross-machine change to the
	// domain separation or canonical form must fail loudly.
	const want = "3bfdc5e464f570af686391b295de7d6fd5994588f62b756452fc11af3e1b7951"
	if got := first.Hash(); got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
}

func TestTeamBotSeal(t *testing.T) {
	team := validTeamBot()
	if team.Sealed() {
		t.Fatal("new team bot is sealed")
	}
	before := team.Hash()
	team.Seal()
	if !team.Sealed() {
		t.Fatal("Seal did not stamp the team bot")
	}
	if team.Revision() != 1 {
		t.Fatalf("revision = %d, want 1", team.Revision())
	}
	if got := team.Hash(); got != before {
		t.Fatalf("sealing changed the content hash: %s vs %s", before, got)
	}
	if err := team.Validate(); err != nil {
		t.Fatalf("sealed team bot rejected: %v", err)
	}

	// Re-sealing unchanged content advances the revision but not the hash.
	team.Seal()
	if team.Revision() != 2 {
		t.Fatalf("revision after second seal = %d, want 2", team.Revision())
	}
	if got := team.Hash(); got != before {
		t.Fatalf("re-sealing changed the content hash: %s vs %s", before, got)
	}

	// Editing after sealing is detectable: the stored hash no longer matches.
	team.Mission = "Changed without sealing."
	if err := team.Validate(); !HasCode(err, CodeHashMismatch) {
		t.Fatalf("edited sealed team bot: %v", err)
	}
}

func TestTeamBotJSONRoundTrip(t *testing.T) {
	team := validTeamBot()
	team.Seal()
	data, err := json.Marshal(team)
	if err != nil {
		t.Fatal(err)
	}
	var decoded TeamBot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Revision() != team.Revision() {
		t.Fatalf("decoded revision = %d, want %d", decoded.Revision(), team.Revision())
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded sealed team bot rejected: %v", err)
	}

	decoded.Name = "Tampered"
	if err := decoded.Validate(); !HasCode(err, CodeHashMismatch) {
		t.Fatalf("tampered team bot: %v", err)
	}

	if err := decoded.UnmarshalJSON([]byte("not json")); !HasCode(err, CodeInvalidJSON) {
		t.Fatalf("invalid json: %v", err)
	}
}

func TestTeamBotRevisionMustPairWithHash(t *testing.T) {
	team := validTeamBot()
	team.Seal()
	team.storedHash = ""
	if err := team.Validate(); !HasCode(err, CodeRevisionInconsistent) {
		t.Fatalf("revision without hash: %v", err)
	}
	team.storedHash = team.Hash()
	team.revision = 0
	if err := team.Validate(); !HasCode(err, CodeRevisionInconsistent) {
		t.Fatalf("hash without revision: %v", err)
	}
}
