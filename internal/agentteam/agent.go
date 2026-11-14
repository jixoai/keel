// Package agentteam defines the domain model of the Agent registry and the
// AgentTeam configuration space.
//
// The package is pure data and rules: types, normalization, deterministic
// derivation (identicon pixels, team content hash) and field-level validation.
// It performs no IO and imports no store, HTTP, plugin or runtime package, so
// the HTTP API, the CLI, the settings UI and the session start path can all
// apply the same rules to the same value.
//
// Specs:
//   - openspec/changes/keel-agent-team-v1/specs/agent-registry/spec.md
//   - openspec/changes/keel-agent-team-v1/specs/agent-team/spec.md
package agentteam

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// Avatar is where an agent's picture comes from. The zero value means "use the
// default": a deterministic identicon seeded with the agent id.
//
// The three kinds are the only supported sources: a locally derived identicon,
// an uploaded file (Ref points at the stored blob) and an external URL. No kind
// depends on a third-party avatar service.
type Avatar struct {
	// Kind is one of AvatarIdenticon, AvatarUpload or AvatarURL. Empty means
	// AvatarIdenticon (the default for a newly created agent).
	Kind string `json:"kind,omitempty"`
	// Seed feeds the identicon derivation. Empty means "agent id", resolved by
	// WithDefaults.
	Seed string `json:"seed,omitempty"`
	// Ref is the stored-blob reference of an AvatarUpload.
	Ref string `json:"ref,omitempty"`
	// URL is the absolute http(s) source of an AvatarURL.
	URL string `json:"url,omitempty"`
}

// Avatar kinds.
const (
	// AvatarIdenticon is the default: a local, deterministic 5x5 grid derived
	// from Seed.
	AvatarIdenticon = "identicon"
	// AvatarUpload is a file the user uploaded; Ref names the stored blob.
	AvatarUpload = "upload"
	// AvatarURL is an external http(s) image.
	AvatarURL = "url"
)

// IdenticonGridSize is the side length of the identicon grid. The grid is
// GitHub style: only the first three columns are independent and the last two
// mirror them, so the picture is symmetric.
const IdenticonGridSize = 5

// identiconDomain separates the identicon bit stream from any other hash this
// package derives, so a seed cannot collide across derivations by construction.
const identiconDomain = "keel.agentteam.identicon.v1\x00"

// WithDefaults returns the avatar with the default kind and seed filled in:
// an empty kind becomes AvatarIdenticon and an empty seed becomes agentID.
//
// Only an exactly empty seed is defaulted. A whitespace-only seed is kept so
// validation rejects it instead of silently replacing what the caller wrote.
func (a Avatar) WithDefaults(agentID string) Avatar {
	if strings.TrimSpace(a.Kind) == "" {
		a.Kind = AvatarIdenticon
	}
	if a.Seed == "" {
		a.Seed = strings.TrimSpace(agentID)
	}
	return a
}

// SeedOr returns the trimmed seed, falling back to agentID when it is unset.
func (a Avatar) SeedOr(agentID string) string {
	if seed := strings.TrimSpace(a.Seed); seed != "" {
		return seed
	}
	return strings.TrimSpace(agentID)
}

// Validate checks the avatar on its own, using the field prefix "avatar".
//
// Unlike Agent.Validate it applies no defaults, so an identicon with a blank
// seed is rejected here even though a new agent's seed is defaulted to its id.
func (a Avatar) Validate() error {
	set := &fieldSet{}
	validateAvatar(set, "avatar", a)
	return set.err()
}

// IdenticonGrid returns the deterministic identicon grid of seed.
//
// The derivation is a pure SHA-256 of the trimmed seed: same seed, same grid,
// in this process and in every other one, with no network and no stored state.
// Seed trimming means " a b " and "a b" are the same avatar.
func IdenticonGrid(seed string) [IdenticonGridSize][IdenticonGridSize]bool {
	var grid [IdenticonGridSize][IdenticonGridSize]bool
	sum := sha256.Sum256([]byte(identiconDomain + strings.TrimSpace(seed)))
	independent := (IdenticonGridSize + 1) / 2
	for row := 0; row < IdenticonGridSize; row++ {
		for column := 0; column < independent; column++ {
			bit := row*independent + column
			on := sum[bit/8]&(1<<(bit%8)) != 0
			grid[row][column] = on
			grid[row][IdenticonGridSize-1-column] = on
		}
	}
	return grid
}

// IdenticonRows renders IdenticonGrid as one string per row ("#" on, "." off).
// It is the stable textual form UIs and tests can diff.
func IdenticonRows(seed string) []string {
	grid := IdenticonGrid(seed)
	rows := make([]string, IdenticonGridSize)
	for row := 0; row < IdenticonGridSize; row++ {
		var builder strings.Builder
		for column := 0; column < IdenticonGridSize; column++ {
			if grid[row][column] {
				builder.WriteByte('#')
			} else {
				builder.WriteByte('.')
			}
		}
		rows[row] = builder.String()
	}
	return rows
}

// ModelBinding is an agent's default model: a provider plus a model id plus
// provider tuning. It is only a default — a session, a task or the team can
// override it — and it carries no credential: the provider owns credentials,
// the binding owns identity.
//
// The zero value means "inherit from team/session"; a half-filled binding is
// invalid, because a provider without a model (or the reverse) cannot resolve.
type ModelBinding struct {
	// ProviderID is the provider instance id (never a base URL or a key).
	ProviderID string `json:"providerId"`
	// ModelID is the wire model id.
	ModelID string `json:"modelId"`
	// Tuning is the provider-specific request overlay (reasoning effort,
	// context budget, ...). It must never carry credentials.
	Tuning map[string]any `json:"tuning,omitempty"`
}

// Inherits reports whether the binding is absent (both halves empty).
func (b ModelBinding) Inherits() bool {
	return strings.TrimSpace(b.ProviderID) == "" && strings.TrimSpace(b.ModelID) == ""
}

// Complete reports whether the binding names both a provider and a model.
func (b ModelBinding) Complete() bool {
	return strings.TrimSpace(b.ProviderID) != "" && strings.TrimSpace(b.ModelID) != ""
}

// ToolGrant is one tool the agent may call. It stores the registry tool id plus
// the per-agent switch and configuration; the tool's schema, permissions and
// implementation stay in the plugin tool registry, never in the agent config.
type ToolGrant struct {
	// ToolID is the id published by the plugin tool registry.
	ToolID string `json:"toolId"`
	// Enabled is the per-agent switch.
	Enabled bool `json:"enabled"`
	// Config is the tool's per-agent configuration (never credentials).
	Config map[string]any `json:"config,omitempty"`
}

// SystemPrompt is the agent's system prompt: either inline text or a reference
// to a prompt file such as AGENTS.md.
type SystemPrompt struct {
	// Kind is SystemPromptInline, SystemPromptFile or empty for "no prompt".
	Kind string `json:"kind,omitempty"`
	// Text is the inline prompt body.
	Text string `json:"text,omitempty"`
	// Ref is the prompt file reference, relative to the workspace.
	Ref string `json:"ref,omitempty"`
}

// System prompt kinds.
const (
	// SystemPromptInline carries the prompt text in Text.
	SystemPromptInline = "inline"
	// SystemPromptFile references a prompt file in Ref.
	SystemPromptFile = "file"
)

// TeamCLI is the captain's self-assembly allowlist over the team CLI. When it
// is disabled the commands must not exist for the captain at all; when it is
// enabled the allowed subcommands are listed explicitly (fail closed).
type TeamCLI struct {
	// Enabled switches the whole team CLI surface for this agent/team.
	Enabled bool `json:"enabled"`
	// Commands is the explicit allowlist of team subcommands.
	Commands []string `json:"commands,omitempty"`
}

// Team CLI subcommand names. The set is the union of the CLI surface declared
// by the agent-team spec (list|show|create|update|delete|member add|remove|save|start)
// and the design's finer-grained member commands.
const (
	TeamCLIList         = "list"
	TeamCLIShow         = "show"
	TeamCLICreate       = "create"
	TeamCLIUpdate       = "update"
	TeamCLIDelete       = "delete"
	TeamCLIMemberAdd    = "member.add"
	TeamCLIMemberRemove = "member.remove"
	TeamCLIMemberList   = "member.list"
	TeamCLIMemberSave   = "member.save"
	TeamCLISave         = "save"
	TeamCLIStart        = "start"
)

// teamCLISubcommands is the closed vocabulary of team subcommands.
var teamCLISubcommands = []string{
	TeamCLIList,
	TeamCLIShow,
	TeamCLICreate,
	TeamCLIUpdate,
	TeamCLIDelete,
	TeamCLIMemberAdd,
	TeamCLIMemberRemove,
	TeamCLIMemberList,
	TeamCLIMemberSave,
	TeamCLISave,
	TeamCLIStart,
}

// KnownTeamSubcommands returns a copy of the closed team subcommand
// vocabulary, sorted for a stable capability tree.
func KnownTeamSubcommands() []string {
	known := append([]string(nil), teamCLISubcommands...)
	sort.Strings(known)
	return known
}

// knownTeamSubcommand reports whether name is in the closed vocabulary.
func knownTeamSubcommand(name string) bool {
	for _, candidate := range teamCLISubcommands {
		if candidate == name {
			return true
		}
	}
	return false
}

// Allows reports whether subcommand is permitted: the surface must be enabled
// and the subcommand must be on the allowlist. A disabled surface allows
// nothing, whatever Commands happens to contain.
func (c TeamCLI) Allows(subcommand string) bool {
	if !c.Enabled {
		return false
	}
	name := strings.TrimSpace(subcommand)
	for _, allowed := range c.Commands {
		if strings.TrimSpace(allowed) == name {
			return true
		}
	}
	return false
}

// EffectiveTeamCLI resolves the captain's team CLI switch from the two places it
// can be declared: the captain Agent (Agent.TeamCLI) and the team
// (AgentTeam.Capabilities.TeamCLI). The spec leaves the precedence open; this is
// the single ruling both faces obey:
//
//   - the surface exists only when BOTH switches are enabled, so switching it
//     off in either place is enough to take the commands away;
//   - the effective allowlist is the intersection, so an agent-level list can
//     never widen the team-level list.
//
// An empty intersection stays enabled with no commands, which Allows reads as
// "nothing" (fail closed).
func EffectiveTeamCLI(team, agent TeamCLI) TeamCLI {
	if !team.Enabled || !agent.Enabled {
		return TeamCLI{}
	}
	allowed := make([]string, 0, len(team.Commands))
	for _, command := range team.Commands {
		name := strings.TrimSpace(command)
		if name == "" || !agent.Allows(name) {
			continue
		}
		allowed = append(allowed, name)
	}
	return TeamCLI{Enabled: true, Commands: allowed}
}

// PermissionError is the typed refusal a team CLI call receives when the
// captain's effective surface does not include the command. It is the "typed
// permission error" the spec requires instead of a prompt-level warning: the
// command is absent from the face AND a direct call fails with this type.
type PermissionError struct {
	// Subcommand is the team subcommand ("member.add").
	Subcommand string
	// Reason is PermissionDisabled, PermissionNotAllowed or
	// PermissionNotDeclared.
	Reason string
}

// Permission refusal reasons.
const (
	// PermissionDisabled means teamCli.enabled is false on the team or the
	// captain agent.
	PermissionDisabled = "disabled"
	// PermissionNotAllowed means the surface is enabled but the subcommand is
	// not on the effective allowlist.
	PermissionNotAllowed = "not_allowed"
	// PermissionNotDeclared means the capability tree does not declare the
	// command at all (or does not declare it callable by a team captain).
	PermissionNotDeclared = "not_declared"
)

// Error renders the refusal with its subcommand and reason.
func (e *PermissionError) Error() string {
	return fmt.Sprintf("team command %q is not available to the captain (%s)", e.Subcommand, e.Reason)
}

// Code is the stable machine-readable reason ("team_cli_disabled",
// "team_cli_not_allowed", "team_cli_not_declared").
func (e *PermissionError) Code() string { return "team_cli_" + e.Reason }

// GenerateAgentID mints a stable agent id for a create request that carries only
// a name: "agent_" plus a dashless UUIDv4. The result satisfies the id grammar
// and needs no store round-trip, so "只填名字 → 系统生成稳定 id" is one pure
// function shared by the HTTP and CLI create paths.
func GenerateAgentID() string {
	return "agent_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

// WithGeneratedID returns the agent with a minted id when it has none, then
// applies the normalized defaults (avatar kind and seed). It is the create-path
// conversion; an update path must keep the stored id.
func (a Agent) WithGeneratedID() Agent {
	if strings.TrimSpace(a.ID) == "" {
		a.ID = GenerateAgentID()
	}
	return a.Normalized()
}

// Agent is a complete agent configuration — a full employee, not a subagent
// parameter and not one tool call.
//
// It carries identity (ID, Name, Description), appearance (Avatar), a default
// model binding, the tool grants it may call, its system prompt and its team
// CLI allowlist. It carries no credentials: provider credentials belong to the
// provider layer and are only ever referenced by id.
type Agent struct {
	// ID is the stable identity. It never changes when the name changes.
	ID string `json:"id"`
	// Name is the editable display name.
	Name string `json:"name"`
	// Description is the editable free-form description.
	Description string `json:"description,omitempty"`
	// Avatar is the picture source; the zero value is a deterministic
	// identicon seeded with ID.
	Avatar Avatar `json:"avatar"`
	// DefaultModel is the default model binding. It is only a default: a
	// session, a task or the team can override it, and the agent config is not
	// rewritten when they do.
	DefaultModel ModelBinding `json:"defaultModel"`
	// Tools are the grants this agent holds. Tool definitions live in the
	// plugin tool registry; this list only references tool ids.
	Tools []ToolGrant `json:"tools,omitempty"`
	// SystemPrompt is the agent's system prompt (inline or file reference).
	SystemPrompt SystemPrompt `json:"systemPrompt,omitempty"`
	// TeamCLI is the captain self-assembly allowlist.
	TeamCLI TeamCLI `json:"teamCli,omitempty"`
	// Skills are optional skill names the agent loads.
	Skills []string `json:"skills,omitempty"`
	// Archived marks a retired agent. Archived agents stay readable for
	// history, and a team that references one is invalid at resolution time.
	Archived bool `json:"archived,omitempty"`
}

// Normalized returns a copy with the canonical spelling of the identity text
// (id, name, description) and the derivable avatar defaults (kind and seed). It
// is what a new agent needs before it can be validated or persisted with only a
// name, a model and an id, and it is the one normalization point team hashing
// uses, so an inline agent hashes the same whether or not the caller normalized
// it first.
func (a Agent) Normalized() Agent {
	a.ID = strings.TrimSpace(a.ID)
	a.Name = strings.TrimSpace(a.Name)
	a.Description = strings.TrimSpace(a.Description)
	a.Avatar = a.Avatar.WithDefaults(a.ID)
	return a
}

// Validate checks the agent and returns a *ValidationError carrying one
// FieldError per problem, or nil.
//
// Validation runs on the normalized view, so an agent created with only an id,
// a name and a model is valid; the avatar defaults to an identicon seeded with
// the id. Tool ids are checked for shape and uniqueness here; whether a tool id
// exists in the tool registry is the caller's check (it needs the registry).
func (a Agent) Validate() error {
	set := &fieldSet{}
	validateAgent(set, "", a.Normalized())
	return set.err()
}

// ValidateWithTools validates the agent and additionally checks that every
// granted tool id exists in the tenant's tool registry. A nil exists function
// behaves like Validate (shape only), which is what a caller without a registry
// can check.
//
// The spec makes the existence check part of saving an Agent ("保存 Agent 时
// MUST 校验 toolId 存在且当前租户可用"); the domain cannot reach a registry, so
// this is the hook the HTTP/CLI save path passes its lookup to. A missing tool is
// tool_not_found at tools[i].toolId, and the reference is kept (a plugin
// uninstall must not silently delete user configuration).
func (a Agent) ValidateWithTools(exists func(toolID string) bool) error {
	set := &fieldSet{}
	normalized := a.Normalized()
	validateAgent(set, "", normalized)
	if exists != nil {
		for index, grant := range normalized.Tools {
			id := strings.TrimSpace(grant.ToolID)
			if id == "" {
				continue // already reported as missing_tool
			}
			if !exists(id) {
				set.add(
					fmt.Sprintf("tools[%d].toolId", index),
					CodeToolNotFound,
					"tool %q is not in the tool registry or not available to this tenant",
					id,
				)
			}
		}
	}
	return set.err()
}

// canonical returns a copy whose set-like slices are sorted, so the team
// content hash never depends on the order a caller happened to list tools,
// skills or allowlisted commands in. Order that carries meaning (a team's
// member list) is left untouched by the caller.
func (a Agent) canonical() Agent {
	canonical := a
	if a.Tools != nil {
		canonical.Tools = append([]ToolGrant(nil), a.Tools...)
		sort.SliceStable(canonical.Tools, func(i, j int) bool {
			return canonical.Tools[i].ToolID < canonical.Tools[j].ToolID
		})
	}
	canonical.Skills = sortedStrings(a.Skills)
	canonical.TeamCLI.Commands = sortedStrings(a.TeamCLI.Commands)
	return canonical
}

// sortedStrings returns a sorted copy, with an empty input normalized to nil so
// the encoding (and therefore the hash) cannot tell nil from empty.
func sortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	copied := append([]string(nil), values...)
	sort.Strings(copied)
	return copied
}
