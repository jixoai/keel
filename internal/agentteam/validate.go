package agentteam

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/jixoai/keel/internal/modelprovider"
)

// Validation codes. Every FieldError carries one, so a caller (HTTP handler,
// CLI, UI form) can react to the failure without matching prose. The codes are
// part of the contract: do not rename them.
const (
	// CodeInvalidJSON marks a persisted config that is not decodable at all.
	CodeInvalidJSON = "invalid_json"
	// CodeMissingID marks an empty stable identity.
	CodeMissingID = "missing_id"
	// CodeInvalidID marks an identity that is not a legal id.
	CodeInvalidID = "invalid_id"
	// CodeMissingName marks an empty display name.
	CodeMissingName = "missing_name"

	// CodeInvalidAvatarKind marks an unknown avatar kind.
	CodeInvalidAvatarKind = "invalid_avatar_kind"
	// CodeEmptyAvatarSeed marks an identicon without a seed.
	CodeEmptyAvatarSeed = "empty_avatar_seed"
	// CodeMissingAvatarRef marks an upload avatar without a stored reference.
	CodeMissingAvatarRef = "missing_avatar_ref"
	// CodeInvalidAvatarURL marks a url avatar that is not an absolute http(s) URL.
	CodeInvalidAvatarURL = "invalid_avatar_url"

	// CodeIncompleteModel marks a half-filled model binding.
	CodeIncompleteModel = "incomplete_model"
	// CodeInlineCredential marks a credential written into configuration.
	CodeInlineCredential = "inline_credential"
	// CodeUnencodableConfig marks configuration that is not JSON data.
	CodeUnencodableConfig = "unencodable_config"

	// CodeMissingTool marks an empty tool id.
	CodeMissingTool = "missing_tool"
	// CodeDuplicateTool marks a tool id granted twice.
	CodeDuplicateTool = "duplicate_tool"
	// CodeToolNotFound marks a tool id the tenant's tool registry does not know.
	CodeToolNotFound = "tool_not_found"

	// CodeInvalidSystemPromptKind marks an unknown system prompt kind.
	CodeInvalidSystemPromptKind = "invalid_system_prompt_kind"
	// CodeMissingSystemPrompt marks a prompt kind without its payload.
	CodeMissingSystemPrompt = "missing_system_prompt"
	// CodeConflictingSystemPrompt marks a prompt that carries two payloads.
	CodeConflictingSystemPrompt = "conflicting_system_prompt"
	// CodeUnsafeSystemPromptRef marks a prompt ref that escapes the workspace.
	CodeUnsafeSystemPromptRef = "unsafe_system_prompt_ref"

	// CodeMissingSkill marks an empty skill name.
	CodeMissingSkill = "missing_skill"
	// CodeDuplicateSkill marks a skill listed twice.
	CodeDuplicateSkill = "duplicate_skill"

	// CodeInvalidTeamCLICommand marks a subcommand outside the team vocabulary.
	CodeInvalidTeamCLICommand = "invalid_team_cli_command"
	// CodeDuplicateTeamCLICommand marks a subcommand allowlisted twice.
	CodeDuplicateTeamCLICommand = "duplicate_team_cli_command"
	// CodeTeamCLINoCommands marks an enabled team CLI with no allowlist.
	CodeTeamCLINoCommands = "team_cli_no_commands"
	// CodeTeamCLICommandsWhenDisabled marks a disabled team CLI with commands.
	CodeTeamCLICommandsWhenDisabled = "team_cli_commands_when_disabled"

	// CodeMissingCaptain marks a team without its mandatory captain.
	CodeMissingCaptain = "missing_captain"
	// CodeMissingAgent marks a member seat without an agent reference.
	CodeMissingAgent = "missing_agent"
	// CodeAgentMismatch marks an inline definition whose id is not the member id.
	CodeAgentMismatch = "agent_mismatch"
	// CodeDuplicateMember marks the same agent seated twice.
	CodeDuplicateMember = "duplicate_member"
	// CodeCaptainInMembers marks the captain also listed as a member.
	CodeCaptainInMembers = "captain_in_members"
	// CodeAgentNotFound marks a reference to an agent missing from the library.
	CodeAgentNotFound = "agent_not_found"
	// CodeTeamArchived marks an archived (therefore invalid) team.
	CodeTeamArchived = "team_archived"
	// CodeRevisionInconsistent marks a revision/hash pair that cannot coexist.
	CodeRevisionInconsistent = "revision_inconsistent"
	// CodeHashMismatch marks a stored hash that does not match the content.
	CodeHashMismatch = "hash_mismatch"
	// CodeNotSealed marks a team the start path refuses because it has no
	// revision/hash to freeze into the run manifest.
	CodeNotSealed = "not_sealed"

	// CodeInvalidFeaturePort marks a feature binding whose port is malformed.
	CodeInvalidFeaturePort = "invalid_feature_port"
	// CodeDuplicateFeaturePort marks one feature port bound twice.
	CodeDuplicateFeaturePort = "duplicate_feature_port"
	// CodeInvalidFeatureCLI marks a feature binding without a command.
	CodeInvalidFeatureCLI = "invalid_feature_cli"
)

// FieldError is one field-level validation failure.
type FieldError struct {
	// Field is the config path ("name", "avatar.seed", "members[1].agentId").
	Field string `json:"field"`
	// Code is the stable machine-readable reason (missing_captain, ...).
	Code string `json:"code"`
	// Message is the human-readable reason.
	Message string `json:"message"`
}

// String renders the error for logs and CLI output, keeping the code visible.
func (e FieldError) String() string {
	switch {
	case e.Field == "" && e.Code == "":
		return e.Message
	case e.Field == "":
		return e.Code + ": " + e.Message
	case e.Code == "":
		return e.Field + ": " + e.Message
	default:
		return e.Field + " (" + e.Code + "): " + e.Message
	}
}

// ValidationError aggregates field-level failures.
type ValidationError struct {
	// Errors are the failures, in field order.
	Errors []FieldError
}

// Error renders every failure, so one round of feedback can fix them all.
func (e *ValidationError) Error() string {
	switch len(e.Errors) {
	case 0:
		return "configuration is invalid"
	case 1:
		return e.Errors[0].String()
	default:
		parts := make([]string, 0, len(e.Errors))
		for _, field := range e.Errors {
			parts = append(parts, field.String())
		}
		return fmt.Sprintf("%d invalid fields: %s", len(e.Errors), strings.Join(parts, "; "))
	}
}

// HasCode reports whether any failure carries code.
func (e *ValidationError) HasCode(code string) bool {
	for _, field := range e.Errors {
		if field.Code == code {
			return true
		}
	}
	return false
}

// FieldErrors extracts the field-level errors of err, when it carries any.
func FieldErrors(err error) []FieldError {
	var validation *ValidationError
	if !errors.As(err, &validation) {
		return nil
	}
	return validation.Errors
}

// HasCode reports whether err is a validation failure carrying code.
func HasCode(err error, code string) bool {
	var validation *ValidationError
	if !errors.As(err, &validation) {
		return false
	}
	return validation.HasCode(code)
}

// fieldSet collects field failures while validating one value.
type fieldSet struct {
	list []FieldError
}

func (s *fieldSet) add(field, code, format string, args ...any) {
	s.list = append(s.list, FieldError{Field: field, Code: code, Message: fmt.Sprintf(format, args...)})
}

func (s *fieldSet) err() error {
	if len(s.list) == 0 {
		return nil
	}
	return &ValidationError{Errors: s.list}
}

// idPattern is the legal id shape: a stable, URL- and CLI-safe token.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// validateID checks a stable identity.
func validateID(set *fieldSet, field, id string) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		set.add(field, CodeMissingID, "id must not be empty")
		return
	}
	if !idPattern.MatchString(trimmed) {
		set.add(field, CodeInvalidID, "id %q must start with a letter or digit and contain only letters, digits, '.', '_', ':' or '-'", id)
	}
}

// validateName checks a display name.
func validateName(set *fieldSet, field, name string) {
	if strings.TrimSpace(name) == "" {
		set.add(field, CodeMissingName, "name must not be empty")
	}
}

// validateAgent checks a normalized agent under the field prefix prefix ("" for
// a top-level agent, "members[0].inline." for an inline definition).
func validateAgent(set *fieldSet, prefix string, agent Agent) {
	validateID(set, prefix+"id", agent.ID)
	validateName(set, prefix+"name", agent.Name)
	validateAvatar(set, prefix+"avatar", agent.Avatar)
	validateModelBinding(set, prefix+"defaultModel", agent.DefaultModel, true)
	validateToolGrants(set, prefix+"tools", agent.Tools)
	validateSystemPrompt(set, prefix+"systemPrompt", agent.SystemPrompt)
	validateTeamCLI(set, prefix+"teamCli", agent.TeamCLI)
	validateSkills(set, prefix+"skills", agent.Skills)
}

// validateAvatar checks the avatar source. An empty kind means the identicon
// default, but every kind still needs its own payload: a defaulted kind is not
// a licence to drop the seed.
func validateAvatar(set *fieldSet, field string, avatar Avatar) {
	kind := strings.TrimSpace(avatar.Kind)
	if kind == "" {
		kind = AvatarIdenticon
	}
	switch kind {
	case AvatarIdenticon:
		if strings.TrimSpace(avatar.Seed) == "" {
			set.add(field+".seed", CodeEmptyAvatarSeed, "identicon seed must not be empty; default it to the agent id")
		}
	case AvatarUpload:
		if strings.TrimSpace(avatar.Ref) == "" {
			set.add(field+".ref", CodeMissingAvatarRef, "upload avatar requires the stored file reference")
		}
	case AvatarURL:
		validateAvatarURL(set, field+".url", avatar.URL)
	default:
		set.add(field+".kind", CodeInvalidAvatarKind, "unknown avatar kind %q (want %q, %q or %q)", avatar.Kind, AvatarIdenticon, AvatarUpload, AvatarURL)
	}
	// Any URL the config carries must be credential free, whichever kind is
	// active (a leftover from a kind switch is still persisted config).
	if kind != AvatarURL && strings.TrimSpace(avatar.URL) != "" {
		validateAvatarURL(set, field+".url", avatar.URL)
	}
}

// validateAvatarURL checks an avatar URL: absolute http(s), no embedded
// credentials.
func validateAvatarURL(set *fieldSet, field, raw string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		set.add(field, CodeInvalidAvatarURL, "url avatar requires an absolute http(s) URL")
		return
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		set.add(field, CodeInvalidAvatarURL, "%q is not an absolute http(s) URL", raw)
		return
	}
	if parsed.User != nil {
		set.add(field, CodeInlineCredential, "avatar URL must not embed credentials")
	}
}

// validateModelBinding checks a model binding. allowInherit permits the
// both-empty value ("inherit"); a half-filled binding is always invalid, and
// tuning is always credential free.
func validateModelBinding(set *fieldSet, field string, binding ModelBinding, allowInherit bool) {
	switch {
	case binding.Inherits():
		if !allowInherit {
			set.add(field, CodeIncompleteModel, "model override must name both a provider and a model")
		}
	case !binding.Complete():
		set.add(field, CodeIncompleteModel, "providerId and modelId must be set together; an agent never half-binds a model")
	}
	validateCredentialFree(set, field+".tuning", binding.Tuning)
}

// validateToolGrants checks tool references: non-empty, unique, and carrying no
// credentials. Whether the tool exists is the tool registry's check.
func validateToolGrants(set *fieldSet, field string, grants []ToolGrant) {
	seen := make(map[string]int, len(grants))
	for index, grant := range grants {
		path := fmt.Sprintf("%s[%d]", field, index)
		id := strings.TrimSpace(grant.ToolID)
		if id == "" {
			set.add(path+".toolId", CodeMissingTool, "tool grant requires a toolId from the plugin tool registry")
			continue
		}
		if previous, ok := seen[id]; ok {
			set.add(path+".toolId", CodeDuplicateTool, "tool %q is already granted at tools[%d]", id, previous)
			continue
		}
		seen[id] = index
		validateCredentialFree(set, path+".config", grant.Config)
	}
}

// validateSystemPrompt checks the prompt source: exactly one payload, and a
// file ref that stays inside the workspace.
func validateSystemPrompt(set *fieldSet, field string, prompt SystemPrompt) {
	kind := strings.TrimSpace(prompt.Kind)
	text := strings.TrimSpace(prompt.Text)
	ref := strings.TrimSpace(prompt.Ref)
	switch kind {
	case "":
		if text != "" || ref != "" {
			set.add(field+".kind", CodeInvalidSystemPromptKind, "kind must be %q or %q when text or ref is set", SystemPromptInline, SystemPromptFile)
		}
	case SystemPromptInline:
		if text == "" {
			set.add(field+".text", CodeMissingSystemPrompt, "inline system prompt requires text")
		}
		if ref != "" {
			set.add(field+".ref", CodeConflictingSystemPrompt, "ref must be empty for an inline prompt")
		}
	case SystemPromptFile:
		if ref == "" {
			set.add(field+".ref", CodeMissingSystemPrompt, "file system prompt requires a reference such as AGENTS.md")
		} else if !safePromptRef(ref) {
			set.add(field+".ref", CodeUnsafeSystemPromptRef, "prompt ref %q must be a relative path inside the workspace", prompt.Ref)
		}
		if text != "" {
			set.add(field+".text", CodeConflictingSystemPrompt, "text must be empty for a file prompt")
		}
	default:
		set.add(field+".kind", CodeInvalidSystemPromptKind, "unknown system prompt kind %q (want %q or %q)", prompt.Kind, SystemPromptInline, SystemPromptFile)
	}
}

// safePromptRef reports whether ref names a file inside the workspace.
func safePromptRef(ref string) bool {
	normalized := strings.ReplaceAll(ref, "\\", "/")
	if path.IsAbs(normalized) {
		return false
	}
	cleaned := path.Clean(normalized)
	return cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}

// validateSkills checks optional skill names: non-empty and unique.
func validateSkills(set *fieldSet, field string, skills []string) {
	seen := make(map[string]int, len(skills))
	for index, raw := range skills {
		name := strings.TrimSpace(raw)
		path := fmt.Sprintf("%s[%d]", field, index)
		if name == "" {
			set.add(path, CodeMissingSkill, "skill name must not be empty")
			continue
		}
		if previous, ok := seen[name]; ok {
			set.add(path, CodeDuplicateSkill, "skill %q is already listed at skills[%d]", name, previous)
			continue
		}
		seen[name] = index
	}
}

// validateTeamCLI checks the allowlist: known subcommands, no duplicates, an
// explicit list when enabled and no leftovers when disabled.
func validateTeamCLI(set *fieldSet, field string, cli TeamCLI) {
	seen := make(map[string]int, len(cli.Commands))
	for index, raw := range cli.Commands {
		path := fmt.Sprintf("%s.commands[%d]", field, index)
		name := strings.TrimSpace(raw)
		if name == "" {
			set.add(path, CodeInvalidTeamCLICommand, "subcommand name must not be empty")
			continue
		}
		if !knownTeamSubcommand(name) {
			set.add(path, CodeInvalidTeamCLICommand, "unknown team subcommand %q", raw)
			continue
		}
		if previous, ok := seen[name]; ok {
			set.add(path, CodeDuplicateTeamCLICommand, "subcommand %q is already allowed at commands[%d]", name, previous)
			continue
		}
		seen[name] = index
	}
	switch {
	case cli.Enabled && len(cli.Commands) == 0:
		set.add(field+".commands", CodeTeamCLINoCommands, "teamCLI is enabled but allows no subcommand; list the allowed subcommands explicitly")
	case !cli.Enabled && len(cli.Commands) > 0:
		set.add(field+".commands", CodeTeamCLICommandsWhenDisabled, "teamCLI is disabled but lists subcommands; the allowlist must be empty while disabled")
	}
}

// memberField is the config path of a member seat.
func memberField(index int) string {
	return fmt.Sprintf("members[%d]", index)
}

// validateMember checks one seat. missingCode distinguishes the captain's
// mandatory seat from an optional member seat.
func validateMember(set *fieldSet, field string, member TeamMember, missingCode string) {
	id := strings.TrimSpace(member.AgentID)
	if id == "" {
		set.add(field+".agentId", missingCode, "seat must reference an agent")
	}
	if member.Inline == nil {
		return
	}
	inline := member.Inline.Normalized()
	if id != "" && strings.TrimSpace(inline.ID) != id {
		set.add(field+".inline.id", CodeAgentMismatch, "inline agent id %q does not match the member agentId %q", inline.ID, id)
	}
	validateAgent(set, field+".inline.", inline)
}

// validateTeamUniqueness checks seat identity: unique member ids, and the
// captain must not also be seated as a member.
func validateTeamUniqueness(set *fieldSet, team *AgentTeam) {
	seen := make(map[string]int, len(team.Members)+1)
	if captainID := strings.TrimSpace(team.Captain.AgentID); captainID != "" {
		seen[captainID] = -1
	}
	for index, member := range team.Members {
		id := strings.TrimSpace(member.AgentID)
		if id == "" {
			continue
		}
		previous, ok := seen[id]
		if !ok {
			seen[id] = index
			continue
		}
		if previous == -1 {
			set.add(memberField(index)+".agentId", CodeCaptainInMembers, "agent %q is the captain and must not also be a member", id)
			continue
		}
		set.add(memberField(index)+".agentId", CodeDuplicateMember, "agent %q is already seated at members[%d]", id, previous)
	}
}

// validateTeamDefaults checks the team-wide defaults.
func validateTeamDefaults(set *fieldSet, defaults TeamDefaults) {
	seen := make(map[string]int, len(defaults.Tools))
	for index, raw := range defaults.Tools {
		id := strings.TrimSpace(raw)
		field := fmt.Sprintf("defaults.tools[%d]", index)
		if id == "" {
			set.add(field, CodeMissingTool, "team default tool must not be empty")
			continue
		}
		if previous, ok := seen[id]; ok {
			set.add(field, CodeDuplicateTool, "tool %q is already listed at defaults.tools[%d]", id, previous)
			continue
		}
		seen[id] = index
	}
	if defaults.ModelOverride != nil {
		validateModelBinding(set, "defaults.modelOverride", *defaults.ModelOverride, false)
	}
}

// validateFeatureBindings checks the team's feature bindings: the port follows
// the provider feature-port vocabulary, each port is bound at most once, and a
// binding that names a command names a `keel ...` command.
func validateFeatureBindings(set *fieldSet, bindings []FeatureBinding) {
	seen := make(map[string]int, len(bindings))
	for index, binding := range bindings {
		field := fmt.Sprintf("featureBindings[%d]", index)
		port := strings.TrimSpace(binding.Port)
		switch {
		case port == "":
			set.add(field+".port", CodeInvalidFeaturePort, "feature port must not be empty")
			continue
		case !modelprovider.ValidFeaturePort(port):
			set.add(field+".port", CodeInvalidFeaturePort,
				"feature port %q must be lowercase, start with a letter and contain only letters, digits, '.', '_', ':' or '-'", binding.Port)
			continue
		}
		if previous, duplicate := seen[port]; duplicate {
			set.add(field+".port", CodeDuplicateFeaturePort, "feature port %q is already bound at featureBindings[%d]", port, previous)
			continue
		}
		seen[port] = index
		if cli := strings.TrimSpace(binding.CLI); cli != "" && !strings.HasPrefix(cli, "keel ") {
			set.add(field+".cli", CodeInvalidFeatureCLI, "feature binding command %q must be a keel command (\"keel <group> ...\")", binding.CLI)
		}
	}
}

// validateTeamSeal checks the revision/hash pair. A config that was edited
// after sealing fails here instead of being trusted.
func validateTeamSeal(set *fieldSet, team *AgentTeam) {
	switch {
	case (team.revision == 0) != (team.storedHash == ""):
		set.add("revision", CodeRevisionInconsistent, "revision %d and hash %q must be stamped together; call Seal", team.revision, team.storedHash)
	case team.storedHash != "":
		if computed := team.Hash(); computed != team.storedHash {
			set.add("hash", CodeHashMismatch, "recorded hash %s does not match the configuration content %s; the config was edited without sealing", team.storedHash, computed)
		}
	}
}

// validateTeam checks everything a team configuration can prove on its own.
func validateTeam(set *fieldSet, team *AgentTeam) {
	validateID(set, "id", team.ID)
	validateName(set, "name", team.Name)
	if team.Archived {
		set.add("archived", CodeTeamArchived, "archived teams are invalid and must not start")
	}
	validateMember(set, "captain", team.Captain, CodeMissingCaptain)
	for index, member := range team.Members {
		validateMember(set, memberField(index), member, CodeMissingAgent)
	}
	validateTeamUniqueness(set, team)
	validateTeamDefaults(set, team.Defaults)
	validateTeamCLI(set, "capabilities.teamCli", team.Capabilities.TeamCLI)
	validateFeatureBindings(set, team.FeatureBindings)
	validateTeamSeal(set, team)
}

// credentialShapedKey reports whether a configuration key names a credential.
//
// Matching is exact on the key with separators and case removed ("apiKey",
// "api_key" and "API-KEY" all normalize to "apikey"), deliberately not a
// substring match: "maxOutputTokens" must stay legal while "token" must not.
// The vocabulary is modelprovider's one table, shared with the provider
// registry and the HTTP DTO, so the same config cannot be legal here and a
// credential leak there.
func credentialShapedKey(key string) bool { return modelprovider.CredentialValueKey(key) }

// validateCredentialFree rejects credentials written into configuration and
// configuration values that are not JSON data (which would make the content
// hash undefined). The walk is modelprovider's shared scanner: maps AND arrays
// are traversed ({"headers":[{"apiKey":"x"}]} is caught), keys are visited in
// sorted order so reported fields are deterministic, and a second credential
// reference is rejected too.
func validateCredentialFree(set *fieldSet, field string, value map[string]any) {
	if value == nil {
		return
	}
	for _, failure := range modelprovider.ScanConfigForCredentials(value, field) {
		set.add(failure.Field, failure.Code, "%s", failure.Message)
	}
	if _, err := json.Marshal(value); err != nil {
		set.add(field, CodeUnencodableConfig, "configuration must be JSON data: %v", err)
	}
}
