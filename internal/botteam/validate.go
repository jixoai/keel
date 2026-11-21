package botteam

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Validation codes. Every FieldError carries one, so an HTTP handler, CLI or UI
// form can react without matching prose. The codes are part of the contract:
// do not rename them.
const (
	// CodeInvalidJSON marks a persisted value that is not decodable at all.
	CodeInvalidJSON = "invalid_json"
	// CodeMissingID marks an empty stable identity.
	CodeMissingID = "missing_id"
	// CodeInvalidID marks an identity that is not a legal handle or uuid.
	CodeInvalidID = "invalid_id"
	// CodeMissingName marks an empty display name.
	CodeMissingName = "missing_name"

	// CodeMissingUsername marks an empty login name.
	CodeMissingUsername = "missing_username"
	// CodeInvalidUsername marks a login name with whitespace, control characters
	// or excessive length.
	CodeInvalidUsername = "invalid_username"
	// CodeMissingDisplayName marks an empty account display name.
	CodeMissingDisplayName = "missing_display_name"
	// CodeInvalidAccountKind marks an account kind outside human|bot|service.
	CodeInvalidAccountKind = "invalid_account_kind"

	// CodeMissingAccount marks a profile without an account reference.
	CodeMissingAccount = "missing_account"
	// CodeMissingRole marks a profile without a role reference.
	CodeMissingRole = "missing_role"
	// CodeMissingProfile marks a bot without a profile reference.
	CodeMissingProfile = "missing_profile"

	// CodeMissingModelKind marks a bot without a model kind.
	CodeMissingModelKind = "missing_model_kind"
	// CodeInvalidModelKind marks a model kind outside the known vocabulary.
	CodeInvalidModelKind = "invalid_model_kind"
	// CodeIncompleteModel marks a default model that is missing provider or
	// model.
	CodeIncompleteModel = "incomplete_model"

	// CodeMissingTool marks an empty tool id in the Harness whitelist.
	CodeMissingTool = "missing_tool"
	// CodeDuplicateTool marks a tool id listed twice.
	CodeDuplicateTool = "duplicate_tool"
	// CodeInvalidPermissionKey marks a permission string that is not
	// dot-separated lowercase segments.
	CodeInvalidPermissionKey = "invalid_permission_key"
	// CodeDuplicatePermission marks a permission listed twice.
	CodeDuplicatePermission = "duplicate_permission"

	// CodeMissingMission marks a TeamBot without a mission.
	CodeMissingMission = "missing_mission"
	// CodeEmptyRoster marks a TeamBot roster with no Bot.
	CodeEmptyRoster = "empty_roster"
	// CodeInvalidRosterRole marks a roster seat whose role is not
	// captain|member.
	CodeInvalidRosterRole = "invalid_roster_role"
	// CodeDuplicateRosterBot marks the same Bot seated twice.
	CodeDuplicateRosterBot = "duplicate_roster_bot"
	// CodeMissingCaptain marks a roster with no captain.
	CodeMissingCaptain = "missing_captain"
	// CodeBotNotFound marks a roster reference the caller's lookup cannot
	// resolve.
	CodeBotNotFound = "bot_not_found"

	// CodeRevisionInconsistent marks a revision/hash pair that cannot coexist.
	CodeRevisionInconsistent = "revision_inconsistent"
	// CodeHashMismatch marks a stored hash that does not match the content.
	CodeHashMismatch = "hash_mismatch"
)

// FieldError is one field-level validation failure.
type FieldError struct {
	// Field is the config path ("username", "roster[1].botId").
	Field string `json:"field"`
	// Code is the stable machine-readable reason.
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
	Errors []FieldError `json:"errors"`
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

// Validate checks the account and returns a *ValidationError carrying one
// FieldError per problem, or nil.
func (a Account) Validate() error {
	set := &fieldSet{}
	normalized := a.Normalized()
	if normalized.ID == uuid.Nil {
		set.add("accountId", CodeMissingID, "account id must not be empty")
	}
	switch {
	case normalized.Username == "":
		set.add("username", CodeMissingUsername, "username must not be empty")
	case len(normalized.Username) > 128:
		set.add("username", CodeInvalidUsername, "username must be at most 128 characters")
	case hasControlOrSpace(normalized.Username):
		set.add("username", CodeInvalidUsername, "username must not contain whitespace or control characters")
	}
	if normalized.DisplayName == "" {
		set.add("displayName", CodeMissingDisplayName, "display name must not be empty")
	}
	if !ValidAccountKind(normalized.Kind) {
		set.add("kind", CodeInvalidAccountKind, "unknown account kind %q (want human, bot or service)", a.Kind)
	}
	return set.err()
}

// Validate checks the role and returns a *ValidationError carrying one
// FieldError per problem, or nil. The instruction may be empty (a role can be
// named before its soul is written); it is never a permission source.
func (r Role) Validate() error {
	set := &fieldSet{}
	normalized := r.Normalized()
	if normalized.ID == uuid.Nil {
		set.add("roleId", CodeMissingID, "role id must not be empty")
	}
	if normalized.Name == "" {
		set.add("name", CodeMissingName, "role name must not be empty")
	}
	return set.err()
}

// Validate checks the profile's two references.
func (p Profile) Validate() error {
	set := &fieldSet{}
	if p.ID == uuid.Nil {
		set.add("profileId", CodeMissingID, "profile id must not be empty")
	}
	if p.AccountID == uuid.Nil {
		set.add("accountId", CodeMissingAccount, "profile requires an account")
	}
	if p.RoleID == uuid.Nil {
		set.add("roleId", CodeMissingRole, "profile requires a role")
	}
	return set.err()
}

// Validate checks the bot's own fields: identity, profile reference, model kind,
// default model completeness and Harness shape. Whether the Profile, the tools
// or the permission keys exist is the caller's registry check.
func (b Bot) Validate() error {
	set := &fieldSet{}
	validateBot(set, "", b.Normalized())
	return set.err()
}

// ValidateWithProfile additionally requires the referenced Profile to exist.
// A nil exists function behaves like Validate. Existence is injected because
// this package cannot reach the store.
func (b Bot) ValidateWithProfile(exists func(profileID uuid.UUID) bool) error {
	set := &fieldSet{}
	normalized := b.Normalized()
	validateBot(set, "", normalized)
	if exists != nil && normalized.ProfileID != uuid.Nil && !exists(normalized.ProfileID) {
		set.add("profileId", CodeMissingProfile, "profile %s does not exist in this tenant", normalized.ProfileID)
	}
	return set.err()
}

// validateBot checks a normalized bot under the field prefix prefix.
func validateBot(set *fieldSet, prefix string, bot Bot) {
	validateHandle(set, prefix+"botId", bot.ID)
	if bot.Name == "" {
		set.add(prefix+"name", CodeMissingName, "bot name must not be empty")
	}
	if bot.ProfileID == uuid.Nil {
		set.add(prefix+"profileId", CodeMissingProfile, "bot requires a profile")
	}
	switch {
	case bot.ModelKind == "":
		set.add(prefix+"modelKind", CodeMissingModelKind, "model kind is required (text, image, decision or realtime)")
	case !ValidModelKind(bot.ModelKind):
		set.add(prefix+"modelKind", CodeInvalidModelKind,
			"unknown model kind %q (want text, image, decision or realtime)", bot.ModelKind)
	}
	if !bot.DefaultModel.Complete() {
		set.add(prefix+"defaultModel", CodeIncompleteModel,
			"providerId and modelId must be set together; a bot always has a default model")
	}
	validateTools(set, prefix+"harness.tools", bot.Harness.Tools)
	validatePermissions(set, prefix+"harness.context.permissions", bot.Harness.Context.Permissions)
}

// validateHandle checks a text identity against the shared handle grammar.
func validateHandle(set *fieldSet, field, id string) {
	if id == "" {
		set.add(field, CodeMissingID, "id must not be empty")
		return
	}
	if !idPattern.MatchString(id) {
		set.add(field, CodeInvalidID,
			"id %q must start with a letter or digit and contain only letters, digits, '.', '_', ':' or '-'", id)
	}
}

// validateTools checks the Harness whitelist: non-empty and unique. Whether a
// tool exists is the tool registry's check.
func validateTools(set *fieldSet, field string, tools []string) {
	seen := make(map[string]int, len(tools))
	for index, raw := range tools {
		path := fmt.Sprintf("%s[%d]", field, index)
		id := strings.TrimSpace(raw)
		if id == "" {
			set.add(path, CodeMissingTool, "tool id must not be empty")
			continue
		}
		if previous, duplicate := seen[id]; duplicate {
			set.add(path, CodeDuplicateTool, "tool %q is already listed at tools[%d]", id, previous)
			continue
		}
		seen[id] = index
	}
}

// validatePermissions checks the Bot's permission narrowing: every entry is a
// structurally valid permission key and no key is listed twice. The authoritative
// known-key catalog is the A3 registry.
func validatePermissions(set *fieldSet, field string, permissions []string) {
	seen := make(map[string]int, len(permissions))
	for index, raw := range permissions {
		path := fmt.Sprintf("%s[%d]", field, index)
		key := strings.TrimSpace(raw)
		if key == "" {
			set.add(path, CodeInvalidPermissionKey, "permission key must not be empty")
			continue
		}
		if !permissionKeyPattern.MatchString(key) {
			set.add(path, CodeInvalidPermissionKey,
				"permission key %q must be lowercase dot-separated segments (domain.action)", raw)
			continue
		}
		if previous, duplicate := seen[key]; duplicate {
			set.add(path, CodeDuplicatePermission, "permission %q is already listed at permissions[%d]", key, previous)
			continue
		}
		seen[key] = index
	}
}

// decodeInvalidJSON wraps a JSON decoding failure as a ValidationError so the
// caller sees the same shape as every other validation failure.
func decodeInvalidJSON(err error) error {
	return &ValidationError{Errors: []FieldError{{
		Field:   "",
		Code:    CodeInvalidJSON,
		Message: "not a valid JSON document: " + err.Error(),
	}}}
}
