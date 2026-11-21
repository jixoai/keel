package botteam

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// teamBotHashDomain and its version tag separate the TeamBot content hash from
// every other hash this package derives. A future canonical form is a new,
// non-comparable identity instead of a silent reinterpretation of old hashes.
const teamBotHashDomain = "keel.botteam.team_bot.v1\x00"

// RosterRole is a seat's part in a TeamBot.
type RosterRole string

// Roster roles. A TeamBot has at least one captain; members are the remaining
// seats. The role is a team-local label, not a permission grant (L12).
const (
	// RosterRoleCaptain is a team lead seat.
	RosterRoleCaptain RosterRole = "captain"
	// RosterRoleMember is an ordinary member seat.
	RosterRoleMember RosterRole = "member"
)

// ValidRosterRole reports whether role is a known roster role.
func ValidRosterRole(role RosterRole) bool {
	return role == RosterRoleCaptain || role == RosterRoleMember
}

// RosterEntry is one Bot reference in a TeamBot roster. The Bot is referenced,
// never copied or owned: the same Bot may sit in several teams, and team
// membership grants it nothing (L12, L20).
type RosterEntry struct {
	// BotID references an existing Bot in the same tenant.
	BotID string `json:"botId"`
	// Role is captain or member.
	Role RosterRole `json:"role"`
}

// TeamBot is a team definition: a mission plus a roster of Bot references. It
// is not a permission subject, has no Account/Profile/ModelKind and owns no
// Session. Whether a Bot may be put in the roster or invoked is decided by
// bot.invoke, not by this entity (L12, L20).
//
// Revision and the content hash are deliberately unexported. The hash is
// derived from the content by Hash; the revision advances only through Seal.
// A decoded revision/hash pair is a claim that Validate checks against the
// content.
type TeamBot struct {
	// ID is the stable team handle ("support-team").
	ID string `json:"teamBotId"`
	// Name is the editable display name.
	Name string `json:"name"`
	// Mission is the team objective injected into member/captain Sessions.
	Mission string `json:"mission"`
	// Roster is the Bot reference list; at least one captain.
	Roster []RosterEntry `json:"roster"`
	// Archived marks a retired team. Archived teams stay readable and must not
	// start new runs.
	Archived bool `json:"archived,omitempty"`

	// revision is the monotonic content revision, advanced only by Seal.
	revision uint64
	// storedHash is the content hash stamped by Seal (or claimed by a decoded
	// value). Validate rejects it when it does not match the content.
	storedHash string
}

// NewTeamBot returns an unsealed TeamBot with the given identity and mission.
func NewTeamBot(id, name, mission string) *TeamBot {
	return &TeamBot{ID: id, Name: name, Mission: mission}
}

// Revision is the monotonic content revision. It is 0 until the TeamBot is
// sealed for the first time.
func (t *TeamBot) Revision() uint64 { return t.revision }

// Sealed reports whether a revision and hash have been stamped.
func (t *TeamBot) Sealed() bool { return t.revision != 0 && t.storedHash != "" }

// Seal stamps the current content: it normalizes the value, advances the
// revision and records the content hash. It is the only way the revision moves,
// so a caller can neither pick a revision nor pin a hash.
func (t *TeamBot) Seal() {
	normalized := t.Normalized()
	*t = normalized
	t.revision++
	t.storedHash = t.Hash()
}

// Normalized returns a copy with identity text trimmed and every roster seat's
// bot id and role trimmed. The roster order is preserved here (it is a
// presentation choice); Hash canonicalizes it because order carries no meaning.
func (t *TeamBot) Normalized() TeamBot {
	normalized := *t
	normalized.ID = strings.TrimSpace(t.ID)
	normalized.Name = strings.TrimSpace(t.Name)
	normalized.Mission = strings.TrimSpace(t.Mission)
	if t.Roster != nil {
		roster := make([]RosterEntry, len(t.Roster))
		for index, entry := range t.Roster {
			roster[index] = RosterEntry{
				BotID: strings.TrimSpace(entry.BotID),
				Role:  RosterRole(strings.TrimSpace(string(entry.Role))),
			}
		}
		normalized.Roster = roster
	}
	return normalized
}

// Hash is the lowercase hex SHA-256 of the canonical TeamBot content.
//
// The input is the content only: revision and the stored hash are excluded (so
// re-sealing unchanged content keeps the hash), the archived lifecycle bit is
// excluded (archiving is not a content edit), and the roster is sorted by bot
// id (a roster is a set: duplicate bot ids are invalid, so order carries no
// meaning). The result is stable across processes and machines.
//
// Hash returns "" only when the content cannot be JSON-encoded, which cannot
// happen for this shape.
func (t *TeamBot) Hash() string {
	blob, err := json.Marshal(t.canonical())
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(append([]byte(teamBotHashDomain), blob...))
	return hex.EncodeToString(sum[:])
}

// canonical returns a copy with the seal cleared and the roster sorted.
func (t *TeamBot) canonical() TeamBot {
	canonical := t.Normalized()
	canonical.revision = 0
	canonical.storedHash = ""
	canonical.Archived = false
	if len(canonical.Roster) == 0 {
		canonical.Roster = nil
		return canonical
	}
	sorted := append([]RosterEntry(nil), canonical.Roster...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].BotID != sorted[j].BotID {
			return sorted[i].BotID < sorted[j].BotID
		}
		return sorted[i].Role < sorted[j].Role
	})
	canonical.Roster = sorted
	return canonical
}

// Validate checks the TeamBot and returns a *ValidationError carrying one
// FieldError per problem, or nil. It validates what the value can prove on its
// own; ValidateWithBots adds the injected existence check.
func (t *TeamBot) Validate() error {
	set := &fieldSet{}
	validateTeamBot(set, t.Normalized())
	return set.err()
}

// ValidateWithBots validates the TeamBot and additionally checks that every
// roster reference exists. A nil exists function behaves like Validate.
func (t *TeamBot) ValidateWithBots(exists func(botID string) bool) error {
	set := &fieldSet{}
	normalized := t.Normalized()
	validateTeamBot(set, normalized)
	if exists != nil {
		for index, entry := range normalized.Roster {
			if entry.BotID == "" || !idPattern.MatchString(entry.BotID) {
				continue // already reported by validateTeamBot
			}
			if !exists(entry.BotID) {
				set.add(rosterField(index)+".botId", CodeBotNotFound,
					"bot %q does not exist in this tenant", entry.BotID)
			}
		}
	}
	return set.err()
}

// validateTeamBot checks identity, mission, roster shape and the seal.
func validateTeamBot(set *fieldSet, team TeamBot) {
	validateHandle(set, "teamBotId", team.ID)
	if team.Name == "" {
		set.add("name", CodeMissingName, "team bot name must not be empty")
	}
	if team.Mission == "" {
		set.add("mission", CodeMissingMission, "mission must not be empty")
	}
	validateRoster(set, team.Roster)
	validateTeamBotSeal(set, team)
}

// validateRoster checks the roster: seats are non-empty, roles are known, bot
// ids are unique and at least one seat is a captain.
func validateRoster(set *fieldSet, roster []RosterEntry) {
	if len(roster) == 0 {
		set.add("roster", CodeEmptyRoster, "roster must contain at least one bot")
		return
	}
	seen := make(map[string]int, len(roster))
	captains := 0
	for index, entry := range roster {
		field := rosterField(index)
		validateHandle(set, field+".botId", entry.BotID)
		switch {
		case entry.Role == "":
			set.add(field+".role", CodeInvalidRosterRole, "roster role is required (captain or member)")
		case !ValidRosterRole(entry.Role):
			set.add(field+".role", CodeInvalidRosterRole,
				"unknown roster role %q (want captain or member)", entry.Role)
		case entry.Role == RosterRoleCaptain:
			captains++
		}
		if entry.BotID == "" || !idPattern.MatchString(entry.BotID) {
			continue
		}
		if previous, duplicate := seen[entry.BotID]; duplicate {
			set.add(field+".botId", CodeDuplicateRosterBot,
				"bot %q is already seated at roster[%d]", entry.BotID, previous)
			continue
		}
		seen[entry.BotID] = index
	}
	if captains == 0 {
		set.add("roster", CodeMissingCaptain, "roster must contain at least one captain")
	}
}

// validateTeamBotSeal checks the revision/hash pair. A value edited after
// sealing fails here instead of being trusted.
func validateTeamBotSeal(set *fieldSet, team TeamBot) {
	switch {
	case (team.revision == 0) != (team.storedHash == ""):
		set.add("revision", CodeRevisionInconsistent,
			"revision %d and hash %q must be stamped together; call Seal", team.revision, team.storedHash)
	case team.storedHash != "":
		if computed := team.Hash(); computed != team.storedHash {
			set.add("hash", CodeHashMismatch,
				"recorded hash %s does not match the content %s; the value was edited without sealing",
				team.storedHash, computed)
		}
	}
}

// rosterField is the config path of one roster seat.
func rosterField(index int) string {
	return "roster[" + strconv.Itoa(index) + "]"
}

// teamBotWire is the persisted/API shape of a TeamBot. revision and hash travel
// with it so a reader can detect an edit that bypassed Seal.
type teamBotWire struct {
	ID       string        `json:"teamBotId"`
	Name     string        `json:"name"`
	Mission  string        `json:"mission"`
	Roster   []RosterEntry `json:"roster"`
	Archived bool          `json:"archived,omitempty"`
	Revision uint64        `json:"revision,omitempty"`
	Hash     string        `json:"hash,omitempty"`
}

// MarshalJSON encodes the TeamBot with its stamped revision and hash.
func (t TeamBot) MarshalJSON() ([]byte, error) {
	return json.Marshal(teamBotWire{
		ID:       t.ID,
		Name:     t.Name,
		Mission:  t.Mission,
		Roster:   t.Roster,
		Archived: t.Archived,
		Revision: t.revision,
		Hash:     t.storedHash,
	})
}

// UnmarshalJSON decodes a persisted TeamBot. The decoded revision and hash are
// treated as claims: Validate rejects them when inconsistent or when they do
// not match the content, so a hand-edited value cannot pass as sealed.
func (t *TeamBot) UnmarshalJSON(data []byte) error {
	var wire teamBotWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return decodeInvalidJSON(err)
	}
	*t = TeamBot{
		ID:         wire.ID,
		Name:       wire.Name,
		Mission:    wire.Mission,
		Roster:     wire.Roster,
		Archived:   wire.Archived,
		revision:   wire.Revision,
		storedHash: wire.Hash,
	}
	return nil
}
