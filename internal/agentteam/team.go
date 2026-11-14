package agentteam

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// TeamStatus is the derived state of a team configuration. It is never stored:
// it is computed from the configuration, so an archived team or a team that
// lost its captain cannot be reported as valid by a stale row.
type TeamStatus string

// Team statuses.
const (
	// StatusValid is a team the session start path may use.
	StatusValid TeamStatus = "valid"
	// StatusInvalid is a team that must not start.
	StatusInvalid TeamStatus = "invalid"
)

// Team hash domain separation and version. The version tag is part of the hash
// input, so a future canonical form is a new, non-comparable identity instead
// of a silent reinterpretation of old hashes.
const teamHashDomain = "keel.agentteam.team.v1\x00"

// TeamMember is one seat in a team: a reference to an Agent (or an inline Agent
// definition) plus the team-local, editable label and description.
//
// The label and description are suggestions with no immutable default: whatever
// the user writes is kept, and an empty label falls back to a caller-supplied
// display name. A member is a full agent, never a subagent parameter.
type TeamMember struct {
	// AgentID references the Agent library entry (or the id of Inline).
	AgentID string `json:"agentId"`
	// Label is the editable team-local display name.
	Label string `json:"label,omitempty"`
	// Description is the editable team-local role text.
	Description string `json:"description,omitempty"`
	// Inline carries an agent definition authored inside the team config
	// instead of the Agent library. When set, its ID must equal AgentID.
	Inline *Agent `json:"inline,omitempty"`
}

// DisplayLabel returns the editable label, falling back to fallback when the
// label is blank. It never invents a permanent name: clearing the label just
// reveals the fallback.
func (m TeamMember) DisplayLabel(fallback string) string {
	if label := strings.TrimSpace(m.Label); label != "" {
		return label
	}
	return fallback
}

// MemberFromAgent builds a member referencing agent, with its name and
// description as the starting suggestion for the editable member text.
func MemberFromAgent(agent Agent) TeamMember {
	return TeamMember{AgentID: agent.ID, Label: agent.Name, Description: agent.Description}
}

// normalized returns a copy with the editable text trimmed and the inline agent
// normalized (avatar defaults included), so the team hash and the persisted
// config agree on one spelling of the same seat.
func (m TeamMember) normalized() TeamMember {
	m.AgentID = strings.TrimSpace(m.AgentID)
	m.Label = strings.TrimSpace(m.Label)
	m.Description = strings.TrimSpace(m.Description)
	if m.Inline != nil {
		inline := m.Inline.Normalized()
		m.Inline = &inline
	}
	return m
}

// canonical returns a copy with the inline agent canonicalized, so hashing a
// team does not depend on the order inside an inline agent's set-like slices.
func (m TeamMember) canonical() TeamMember {
	if m.Inline != nil {
		inline := m.Inline.canonical()
		m.Inline = &inline
	}
	return m
}

// FeatureBinding binds one feature port to the command that serves it
// (design.md §5: featureBindings). The team preset records the composition, so a
// team can offer a capability without changing the shared provider config.
type FeatureBinding struct {
	// Port is the feature port ("team.assign", "image.generate").
	Port string `json:"port"`
	// CLI is the command that serves the port ("keel team assign").
	CLI string `json:"cli,omitempty"`
}

// TeamDefaults are the team-wide defaults every member starts from. They are
// defaults, not locks: a member or a session may override them.
type TeamDefaults struct {
	// Tools are tool ids granted to every member of the team.
	Tools []string `json:"tools,omitempty"`
	// ModelOverride replaces the member's default model binding when set.
	ModelOverride *ModelBinding `json:"modelOverride,omitempty"`
}

// TeamCapabilities are the team-level capability switches.
type TeamCapabilities struct {
	// TeamCLI is the captain's self-assembly allowlist.
	TeamCLI TeamCLI `json:"teamCli"`
}

// AgentTeam is a named team configuration: exactly one captain, zero or more
// members, team defaults, capability switches and a stamped revision.
//
// A team is a configuration space, not a second execution topology: members
// start through the ordinary agent/run mechanism. The team only records who is
// on it and which defaults apply.
//
// Revision and hash are deliberately unexported. The content hash is derived
// from the content by Hash; the revision only moves forward through Seal. A
// caller can neither pin an arbitrary hash nor jump the revision, and a stored
// hash that no longer matches the content is a validation error rather than a
// silent pass.
type AgentTeam struct {
	// ID is the stable team identity.
	ID string `json:"id"`
	// Name is the editable display name.
	Name string `json:"name"`
	// Description is the editable free-form description.
	Description string `json:"description,omitempty"`
	// Captain is the single, mandatory team lead.
	Captain TeamMember `json:"captain"`
	// Members are the additional seats; 0..N, never including the captain.
	Members []TeamMember `json:"members,omitempty"`
	// Defaults are the team-wide defaults.
	Defaults TeamDefaults `json:"defaults,omitempty"`
	// Capabilities are the team-level switches.
	Capabilities TeamCapabilities `json:"capabilities,omitempty"`
	// FeatureBindings bind feature ports to the commands that serve them. They
	// are part of the team preset (design.md §5), not of the shared provider
	// configuration.
	FeatureBindings []FeatureBinding `json:"featureBindings,omitempty"`
	// Archived marks a retired team. Archived teams are invalid and must not
	// start; history keeps its snapshot.
	Archived bool `json:"archived,omitempty"`

	// revision is the monotonic configuration revision, advanced only by Seal.
	revision uint64
	// storedHash is the content hash stamped by Seal (or claimed by a decoded
	// config file). Validate rejects it when it does not match the content.
	storedHash string
}

// NewTeam returns an unsealed team with captain and no members. A team with
// only a captain is legal: the captain may assemble members later when the
// team CLI allowlist permits it.
func NewTeam(id, name string, captain TeamMember) *AgentTeam {
	return &AgentTeam{ID: id, Name: name, Captain: captain}
}

// GenerateTeamID mints a stable team id for a create request that carries only a
// name and a captain: "team_" plus a dashless UUIDv4. It is the team counterpart
// of GenerateAgentID and satisfies the same id grammar, so `keel team create
// --captain ... --name ...` needs no store round-trip to learn its identity.
func GenerateTeamID() string {
	return "team_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

// Revision is the monotonic configuration revision. It is 0 until the team is
// sealed for the first time.
func (t *AgentTeam) Revision() uint64 { return t.revision }

// Sealed reports whether a revision and hash have been stamped.
func (t *AgentTeam) Sealed() bool { return t.revision != 0 && t.storedHash != "" }

// Seal stamps the current content: it normalizes the configuration, advances the
// revision and records the content hash. It is the only way the revision moves,
// so a caller cannot pick a revision; and because the hash is derived, a caller
// cannot pick a hash either. Call it after every accepted edit, before
// persisting.
//
// Normalizing here is the single normalization point the content hash has: the
// same semantic configuration seals to the same hash whether or not the caller
// called Normalized first.
func (t *AgentTeam) Seal() {
	normalized := t.Normalized()
	*t = normalized
	t.revision++
	t.storedHash = t.Hash()
}

// Normalized returns a canonical copy of the configuration content: identity
// text trimmed, member seats and inline agents normalized (avatar defaults
// included), set-like lists trimmed, and the seal preserved. Hash hashes this
// form, Seal stores it, and Validate judges it, so the persisted config, the
// content hash and the run manifest cannot disagree about what the team is.
func (t *AgentTeam) Normalized() AgentTeam {
	normalized := *t
	normalized.ID = strings.TrimSpace(t.ID)
	normalized.Name = strings.TrimSpace(t.Name)
	normalized.Description = strings.TrimSpace(t.Description)
	normalized.Captain = t.Captain.normalized()
	if t.Members != nil {
		normalized.Members = make([]TeamMember, len(t.Members))
		for index, member := range t.Members {
			normalized.Members[index] = member.normalized()
		}
	}
	normalized.Defaults.Tools = trimmedStrings(t.Defaults.Tools)
	if override := t.Defaults.ModelOverride; override != nil {
		copied := *override
		copied.ProviderID = strings.TrimSpace(override.ProviderID)
		copied.ModelID = strings.TrimSpace(override.ModelID)
		normalized.Defaults.ModelOverride = &copied
	}
	normalized.Capabilities.TeamCLI.Commands = trimmedStrings(t.Capabilities.TeamCLI.Commands)
	if t.FeatureBindings != nil {
		normalized.FeatureBindings = make([]FeatureBinding, len(t.FeatureBindings))
		for index, binding := range t.FeatureBindings {
			normalized.FeatureBindings[index] = FeatureBinding{
				Port: strings.TrimSpace(binding.Port),
				CLI:  strings.TrimSpace(binding.CLI),
			}
		}
	}
	return normalized
}

// trimmedStrings returns a copy with each entry trimmed; empty entries are kept
// so validation can report them instead of silently shrinking the config.
func trimmedStrings(values []string) []string {
	if values == nil {
		return nil
	}
	trimmed := make([]string, len(values))
	for index, value := range values {
		trimmed[index] = strings.TrimSpace(value)
	}
	return trimmed
}

// Status is the derived team state: valid only when Validate reports no
// problem, which includes "not archived" and "stored hash matches content".
func (t *AgentTeam) Status() TeamStatus {
	if t.Validate() == nil {
		return StatusValid
	}
	return StatusInvalid
}

// Hash is the lowercase hex SHA-256 of the canonical team configuration.
//
// The input is the canonical encoding of the content only: revision and the
// stored hash are excluded (so re-sealing unchanged content keeps the hash),
// set-like slices are sorted, and empty and nil are the same. It is therefore
// stable across processes and machines, and it changes when any identity field
// changes — a member's label included.
//
// Hash returns "" only when the configuration cannot be JSON-encoded (a tuning
// or tool config value that is not JSON data). Validate reports that case as a
// field error, so an unencodable team can never be considered valid.
func (t *AgentTeam) Hash() string {
	blob, err := json.Marshal(t.canonical())
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(append([]byte(teamHashDomain), blob...))
	return hex.EncodeToString(sum[:])
}

// Validate checks the team and returns a *ValidationError carrying one
// FieldError per problem, or nil. It validates what the configuration alone can
// prove; use ValidateWithAgents to also check that referenced agents exist.
func (t *AgentTeam) Validate() error {
	set := &fieldSet{}
	normalized := t.Normalized()
	validateTeam(set, &normalized)
	return set.err()
}

// ValidateWithAgents validates the team and additionally checks that every
// referenced agent exists in the Agent library. Members carrying an inline
// definition are self-contained and are not looked up. A nil exists function
// behaves like Validate.
func (t *AgentTeam) ValidateWithAgents(exists func(agentID string) bool) error {
	set := &fieldSet{}
	normalized := t.Normalized()
	validateTeam(set, &normalized)
	validateTeamAgentRefs(set, &normalized, exists)
	return set.err()
}

// ValidateForStart checks the conditions a session start requires: the team is
// a valid configuration, it is sealed (revision and hash stamped, so the run
// manifest can freeze the team version/hash), and — when exists is non-nil —
// every referenced agent exists. It is the gate the start path uses; Validate
// stays the draft-edit gate, because an unsealed team is a legal draft.
func (t *AgentTeam) ValidateForStart(exists func(agentID string) bool) error {
	set := &fieldSet{}
	normalized := t.Normalized()
	validateTeam(set, &normalized)
	if !normalized.Sealed() {
		set.add(
			"revision",
			CodeNotSealed,
			"team %q is not sealed; call Seal before starting a run so the manifest can freeze its revision and hash",
			normalized.ID,
		)
	}
	validateTeamAgentRefs(set, &normalized, exists)
	return set.err()
}

// validateTeamAgentRefs checks that referenced (non-inline) agents exist.
func validateTeamAgentRefs(set *fieldSet, team *AgentTeam, exists func(agentID string) bool) {
	if exists == nil {
		return
	}
	check := func(field string, member TeamMember) {
		if member.Inline != nil {
			return
		}
		id := strings.TrimSpace(member.AgentID)
		if id == "" {
			return
		}
		if !exists(id) {
			set.add(field+".agentId", CodeAgentNotFound, "agent %q is not in the agent library", id)
		}
	}
	check("captain", team.Captain)
	for index, member := range team.Members {
		check(memberField(index), member)
	}
}

// canonical returns a deep copy with the seal cleared, the content normalized
// and set-like slices sorted; it is the exact value Hash encodes.
func (t *AgentTeam) canonical() AgentTeam {
	canonical := t.Normalized()
	canonical.revision = 0
	canonical.storedHash = ""
	canonical.Captain = canonical.Captain.canonical()
	if canonical.Members != nil {
		for index, member := range canonical.Members {
			canonical.Members[index] = member.canonical()
		}
	}
	canonical.Defaults.Tools = sortedStrings(canonical.Defaults.Tools)
	canonical.Capabilities.TeamCLI.Commands = sortedStrings(canonical.Capabilities.TeamCLI.Commands)
	canonical.FeatureBindings = sortedFeatureBindings(canonical.FeatureBindings)
	return canonical
}

// sortedFeatureBindings returns a copy ordered by port, so a team that lists the
// same bindings in another order hashes the same. A port bound twice is a
// validation error, so the order carries no meaning.
func sortedFeatureBindings(bindings []FeatureBinding) []FeatureBinding {
	if len(bindings) == 0 {
		return nil
	}
	sorted := append([]FeatureBinding(nil), bindings...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Port < sorted[j].Port })
	return sorted
}

// teamWire is the persisted shape of a team configuration. revision and hash
// travel with the file so a reader can detect an edit that bypassed Seal.
type teamWire struct {
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	Description     string           `json:"description,omitempty"`
	Captain         TeamMember       `json:"captain"`
	Members         []TeamMember     `json:"members,omitempty"`
	Defaults        TeamDefaults     `json:"defaults,omitempty"`
	Capabilities    TeamCapabilities `json:"capabilities,omitempty"`
	FeatureBindings []FeatureBinding `json:"featureBindings,omitempty"`
	Archived        bool             `json:"archived,omitempty"`
	Revision        uint64           `json:"revision,omitempty"`
	Hash            string           `json:"hash,omitempty"`
}

// MarshalJSON encodes the team with its stamped revision and hash.
func (t AgentTeam) MarshalJSON() ([]byte, error) {
	return json.Marshal(teamWire{
		ID:              t.ID,
		Name:            t.Name,
		Description:     t.Description,
		Captain:         t.Captain,
		Members:         t.Members,
		Defaults:        t.Defaults,
		Capabilities:    t.Capabilities,
		FeatureBindings: t.FeatureBindings,
		Archived:        t.Archived,
		Revision:        t.revision,
		Hash:            t.storedHash,
	})
}

// UnmarshalJSON decodes a persisted team configuration. The decoded revision
// and hash are treated as claims: Validate rejects them when they are
// inconsistent or do not match the content, so a hand-edited file cannot pass
// as sealed.
func (t *AgentTeam) UnmarshalJSON(data []byte) error {
	var wire teamWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return &ValidationError{Errors: []FieldError{{
			Field:   "",
			Code:    CodeInvalidJSON,
			Message: "team config: " + err.Error(),
		}}}
	}
	*t = AgentTeam{
		ID:              wire.ID,
		Name:            wire.Name,
		Description:     wire.Description,
		Captain:         wire.Captain,
		Members:         wire.Members,
		Defaults:        wire.Defaults,
		Capabilities:    wire.Capabilities,
		FeatureBindings: wire.FeatureBindings,
		Archived:        wire.Archived,
		revision:        wire.Revision,
		storedHash:      wire.Hash,
	}
	return nil
}
