// Package botteam defines the pure domain model of accounts, roles, profiles,
// bots and team bots.
//
// The package is data and rules only: types, normalization, deterministic
// content hashes and field-level validation. It performs no IO and imports no
// store, HTTP, runtime or plugin package, so the HTTP API, the CLI, the
// settings UI and the session start path can all apply the same rules to the
// same value.
//
// Vocabulary (openspec/changes/keel-bot-identity-access):
//
//   - Account is the login and permission subject; human, bot and service
//     accounts share one model (L1, L14).
//   - Role is identity and soul only. It never grants a permission (L11).
//   - Profile binds one Account to one Role.
//   - Bot = Profile + model kind + default model + Harness (tools and
//     context). Its effective permissions are Account permissions ∩ Harness
//     permissions (L2, L9, L11).
//   - TeamBot = mission + roster. It is a resource, not a permission subject
//     and owns no session (L12, L20).
package botteam

import (
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// AccountKind is the account's nature. Bot and service accounts never log in
// through the browser; they use a bot-bound API key (L18).
type AccountKind string

// Account kinds.
const (
	// AccountKindHuman is a person account.
	AccountKindHuman AccountKind = "human"
	// AccountKindBot is an AI employee account (a Bot's owner identity).
	AccountKindBot AccountKind = "bot"
	// AccountKindService is a machine/integration account.
	AccountKindService AccountKind = "service"
)

// knownAccountKinds is the closed account-kind vocabulary.
var knownAccountKinds = []AccountKind{
	AccountKindHuman,
	AccountKindBot,
	AccountKindService,
}

// ValidAccountKind reports whether kind is a known account kind.
func ValidAccountKind(kind AccountKind) bool {
	for _, candidate := range knownAccountKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// KnownAccountKinds returns a copy of the closed account-kind vocabulary.
func KnownAccountKinds() []AccountKind {
	return append([]AccountKind(nil), knownAccountKinds...)
}

// Account is the login and permission subject. A super admin is a deployment
// bootstrap fact (isSuperAdmin); ordinary profile and permission writes never
// create or promote one (L6, L17).
type Account struct {
	// ID is the stable account identity.
	ID uuid.UUID `json:"accountId"`
	// Username is the login name, unique per tenant under case folding.
	Username string `json:"username"`
	// DisplayName is the editable human-readable name.
	DisplayName string `json:"displayName"`
	// Kind is human, bot or service.
	Kind AccountKind `json:"kind"`
	// PasswordHash is an Argon2id hash. It is opaque to this package and is
	// never serialized: no API response may carry it.
	PasswordHash string `json:"-"`
	// IsSuperAdmin marks the deployment-level super admin. Only the bootstrap
	// path may set it; the domain validates shape, not the authority to set it.
	IsSuperAdmin bool `json:"isSuperAdmin,omitempty"`
	// DisabledAt is the soft-delete/disable mark. A disabled account cannot
	// authenticate; its rows, bots and audit history stay.
	DisabledAt *time.Time `json:"disabledAt,omitempty"`
}

// Normalized returns a copy with the identity text trimmed. PasswordHash is
// deliberately left byte-for-byte untouched: it is an encoded credential, not
// display text.
func (a Account) Normalized() Account {
	a.Username = strings.TrimSpace(a.Username)
	a.DisplayName = strings.TrimSpace(a.DisplayName)
	a.Kind = AccountKind(strings.TrimSpace(string(a.Kind)))
	return a
}

// Disabled reports whether the account is disabled.
func (a Account) Disabled() bool { return a.DisabledAt != nil }

// Role is identity and soul: name, instruction, voice and default behavior.
// It carries no permission and grants nothing (L11).
type Role struct {
	// ID is the stable role identity.
	ID uuid.UUID `json:"roleId"`
	// Name is the editable display name.
	Name string `json:"name"`
	// Instruction is the role's soul/behavior text.
	Instruction string `json:"instruction,omitempty"`
}

// Normalized returns a copy with the identity text trimmed.
func (r Role) Normalized() Role {
	r.Name = strings.TrimSpace(r.Name)
	r.Instruction = strings.TrimSpace(r.Instruction)
	return r
}

// Profile binds one Account to one Role. A Bot references a Profile, so the
// same Account or Role can be reused without copying either (L4, L9).
type Profile struct {
	// ID is the stable profile identity.
	ID uuid.UUID `json:"profileId"`
	// AccountID is the owning account.
	AccountID uuid.UUID `json:"accountId"`
	// RoleID is the identity/role the profile provides.
	RoleID uuid.UUID `json:"roleId"`
}

// Normalized returns the profile unchanged: every field is an opaque uuid.
func (p Profile) Normalized() Profile { return p }

// ModelKind is the model's working mode. The vocabulary is intentionally small
// and extensible: a new kind is a new constant plus a new resolver, never a
// silent reinterpretation of an old value (L15).
type ModelKind string

// Model kinds.
const (
	// ModelKindText is a chat/completion model.
	ModelKindText ModelKind = "text"
	// ModelKindImage is an image generation/edit model.
	ModelKindImage ModelKind = "image"
	// ModelKindDecision is a classifier/decision model.
	ModelKindDecision ModelKind = "decision"
	// ModelKindRealtime is a streaming/realtime model.
	ModelKindRealtime ModelKind = "realtime"
)

// knownModelKinds is the closed model-kind vocabulary of this version.
var knownModelKinds = []ModelKind{
	ModelKindText,
	ModelKindImage,
	ModelKindDecision,
	ModelKindRealtime,
}

// ValidModelKind reports whether kind is a known model kind.
func ValidModelKind(kind ModelKind) bool {
	for _, candidate := range knownModelKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// KnownModelKinds returns a copy of the model-kind vocabulary.
func KnownModelKinds() []ModelKind {
	return append([]ModelKind(nil), knownModelKinds...)
}

// ModelRef is a Bot's default model: a provider plus a wire model id. It is
// only the default; a run may override it and the Session/Run snapshot records
// the model actually used (L15). It carries no credential.
type ModelRef struct {
	// ProviderID is the provider instance id (never a base URL or a key).
	ProviderID string `json:"providerId"`
	// ModelID is the wire model id.
	ModelID string `json:"modelId"`
}

// Complete reports whether the reference names both a provider and a model.
func (m ModelRef) Complete() bool {
	return strings.TrimSpace(m.ProviderID) != "" && strings.TrimSpace(m.ModelID) != ""
}

// HarnessContext is what the model is told: the instruction and the Bot's own
// permission narrowing.
type HarnessContext struct {
	// Instruction is the Bot's operating prompt.
	Instruction string `json:"instruction,omitempty"`
	// Permissions is the Bot's own permission narrowing. The effective set is
	// Account permissions ∩ this set (L11); an empty set therefore narrows to
	// nothing, not to "inherit everything".
	Permissions []string `json:"permissions,omitempty"`
}

// Harness is the environment a Bot's model runs in: tool whitelist plus
// context. The actual tool surface is the deployment registry ∩ Tools.
type Harness struct {
	// Tools is the tool-id whitelist. Whether a tool exists is the registry's
	// check, not this package's.
	Tools []string `json:"tools,omitempty"`
	// Context is the instruction plus permission narrowing.
	Context HarnessContext `json:"context"`
}

// Bot is a runnable AI employee: a Profile, a model kind, a default model and
// a Harness. A Bot belongs to exactly one Account, through its Profile, and
// owns its Sessions (L9, L10).
type Bot struct {
	// ID is the stable bot handle ("support-1"). It never changes when the
	// display name changes.
	ID string `json:"botId"`
	// Name is the editable display name.
	Name string `json:"name"`
	// ProfileID is the Profile that supplies Account and Role.
	ProfileID uuid.UUID `json:"profileId"`
	// ModelKind is the model's working mode; required.
	ModelKind ModelKind `json:"modelKind"`
	// DefaultModel is the provider/model used when a run names none.
	DefaultModel ModelRef `json:"defaultModel"`
	// Harness is the tool and context environment.
	Harness Harness `json:"harness"`
	// Archived marks a retired Bot. Archived bots stay readable for history and
	// refuse new Sessions; existing Sessions are kept.
	Archived bool `json:"archived,omitempty"`
}

// Normalized returns a copy with identity text trimmed, the model kind folded
// to lowercase and the Harness text trimmed. It does not sort tools or
// permissions: they are sets for authorization, but their order is a
// presentation choice the caller keeps.
func (b Bot) Normalized() Bot {
	b.ID = strings.TrimSpace(b.ID)
	b.Name = strings.TrimSpace(b.Name)
	b.ModelKind = ModelKind(strings.ToLower(strings.TrimSpace(string(b.ModelKind))))
	b.DefaultModel.ProviderID = strings.TrimSpace(b.DefaultModel.ProviderID)
	b.DefaultModel.ModelID = strings.TrimSpace(b.DefaultModel.ModelID)
	if b.Harness.Tools != nil {
		tools := make([]string, len(b.Harness.Tools))
		for index, tool := range b.Harness.Tools {
			tools[index] = strings.TrimSpace(tool)
		}
		b.Harness.Tools = tools
	}
	b.Harness.Context.Instruction = strings.TrimSpace(b.Harness.Context.Instruction)
	if b.Harness.Context.Permissions != nil {
		permissions := make([]string, len(b.Harness.Context.Permissions))
		for index, permission := range b.Harness.Context.Permissions {
			permissions[index] = strings.TrimSpace(permission)
		}
		b.Harness.Context.Permissions = permissions
	}
	return b
}

// idPattern is the legal handle shape for text identities (bot/team_bot ids):
// a stable, URL- and CLI-safe token, the same grammar the retired registry used.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// permissionKeyPattern is the permission-key shape: lowercase
// dot-separated segments, at least domain.action. The authoritative key catalog
// is the A3 registry; this package only rejects structurally impossible keys.
var permissionKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// GenerateBotID mints a stable bot id for a create request that carries only a
// name: "bot_" plus a dashless UUIDv4.
func GenerateBotID() string {
	return "bot_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

// GenerateTeamBotID mints a stable team-bot id: "team_" plus a dashless
// UUIDv4. It mirrors the old team-id shape without reusing the agent namespace.
func GenerateTeamBotID() string {
	return "team_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

// hasControlOrSpace reports whether value contains a control character or
// whitespace. Login names are single tokens; internal whitespace is rejected so
// a username cannot be a display-name lookalike.
func hasControlOrSpace(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return true
		}
	}
	return false
}
