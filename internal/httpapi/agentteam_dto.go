// HTTP DTOs for the agent registry, agent teams and model providers.
//
// The shapes here are the wire contract of the /v1/agents, /v1/agent-teams and
// /v1/model/providers surfaces (design.md §9). Routes and handlers are not part
// of this file: it holds the request bodies, the response projections, the
// strict decoder and the field-level validation those handlers will call.
//
// Specs:
//   - openspec/changes/keel-agent-team-v1/specs/agent-registry/spec.md
//   - openspec/changes/keel-agent-team-v1/specs/agent-team/spec.md
//   - openspec/changes/keel-agent-team-v1/specs/provider-registry/spec.md
//   - openspec/changes/keel-agent-team-v1/design.md (§4, §5, §9)
//
// Three rules the shapes encode:
//
//   - Tenant ownership never comes from a body. No request DTO declares a
//     tenant field, and the strict decoder rejects the field if a client sends
//     it; the tenant is established by X-Tenant-ID (see tenantFrom) or by an
//     operator path segment. A response carries it as a read-only projection
//     ("tenantId"), so a collection is self-describing instead of echoing
//     request state.
//   - Credentials are references, never values. No DTO has an apiKey, token or
//     password field; a model provider carries credentialRef, and the accepted
//     values are indirections only (vault://name, secret:NAME, env:NAME,
//     ${NAME}) — the same rule internal/host/config.go applies to apiKeyRef.
//     Credential-shaped keys inside an opaque tuning/config object are rejected
//     as well, because configuration is still persisted data.
//   - Server-owned state is not client-writable. A team's revision/hash (moved
//     only by Seal), a team's derived status, a provider's measured health and
//     an agent's/team's archived flag are response-only: archiving is its own
//     operation. A client sending one of them is rejected as an unknown field
//     instead of having the value silently ignored.
//
// Validation is the domain's, not a second dialect: the request DTOs convert
// into the same agentteam and modelprovider values the store and the run path
// use, and Validate returns *agentteam.ValidationError whose FieldError paths
// are the JSON names declared here ("id", "avatar.kind", "captain.agentId",
// "members[1].agentId"). The provider body is validated by
// modelprovider.ProviderInstance.ValidateShape and its field errors are
// translated into the same envelope; this file owns no provider rules of its own.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentteam"
	"github.com/jixoai/keel/internal/modelprovider"
)

// BodyRejection reasons. Each one answers a different client fix: send a body,
// fix the syntax, remove a field, or send a single object.
const (
	// BodyRejectionEmpty marks a request with no JSON value at all.
	BodyRejectionEmpty = "empty_body"
	// BodyRejectionMalformed marks a body that is not valid JSON.
	BodyRejectionMalformed = "malformed_json"
	// BodyRejectionUnknownField marks a field the target DTO does not declare.
	BodyRejectionUnknownField = "unknown_field"
	// BodyRejectionFieldType marks a declared field carrying the wrong JSON type.
	BodyRejectionFieldType = "invalid_field_type"
	// BodyRejectionTrailingData marks a second JSON value after the object.
	BodyRejectionTrailingData = "trailing_data"
)

// BodyRejection is a body that cannot be accepted at the JSON layer, before any
// field validation runs. Errors.As recovers it, and Field names the offending
// field when the rejection has one, so the handler can answer 400 with the same
// field-level shape as a validation failure.
type BodyRejection struct {
	// Reason is one of the BodyRejection* constants.
	Reason string
	// Field is the rejected field ("tenantId", "avatar.bogus"), when the
	// rejection names one.
	Field string
	// Err is the decoder error, kept for logging.
	Err error
}

// Error renders the rejection as one line that names the field when there is
// one.
func (e *BodyRejection) Error() string {
	switch {
	case e.Field != "":
		return fmt.Sprintf("request body %s: %s (%v)", e.Reason, e.Field, e.Err)
	case e.Err != nil:
		return fmt.Sprintf("request body %s: %v", e.Reason, e.Err)
	default:
		return "request body " + e.Reason
	}
}

// Unwrap exposes the decoder error.
func (e *BodyRejection) Unwrap() error { return e.Err }

// decodeStrictJSON decodes exactly one JSON object from reader into dst,
// rejecting unknown fields at every nesting level, trailing values and an empty
// body.
//
// Unknown fields must be rejected rather than ignored here because two of the
// contract's promises depend on it: a body cannot smuggle a tenant ("tenantId")
// or a plaintext credential ("apiKey"), and it cannot set server-owned state
// (revision, hash, status, archived) that the handler would otherwise drop
// silently — a client that is told "accepted" must not have written nothing.
//
// The size bound stays with the caller: pass http.MaxBytesReader-wrapped bodies
// the way the existing routes do.
func decodeStrictJSON(reader io.Reader, dst any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return classifyDecodeError(err)
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return &BodyRejection{Reason: BodyRejectionTrailingData, Err: err}
	}
	return nil
}

// classifyDecodeError turns a decoder error into a BodyRejection, extracting
// the field name when encoding/json names one.
func classifyDecodeError(err error) error {
	if errors.Is(err, io.EOF) {
		return &BodyRejection{Reason: BodyRejectionEmpty, Err: err}
	}
	if field, ok := unknownFieldName(err); ok {
		return &BodyRejection{Reason: BodyRejectionUnknownField, Field: field, Err: err}
	}
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &typeError) && typeError.Field != "" {
		return &BodyRejection{Reason: BodyRejectionFieldType, Field: typeError.Field, Err: err}
	}
	return &BodyRejection{Reason: BodyRejectionMalformed, Err: err}
}

// unknownFieldName extracts the field name from encoding/json's unknown-field
// error, which is the only unknown-field signal the package exposes.
func unknownFieldName(err error) (string, bool) {
	const prefix = "json: unknown field "
	message := err.Error()
	if !strings.HasPrefix(message, prefix) {
		return "", false
	}
	name, unquoteErr := strconv.Unquote(strings.TrimPrefix(message, prefix))
	if unquoteErr != nil {
		return "", false
	}
	return name, true
}

// providerFieldErrors translates a modelprovider validation failure into the
// API's error envelope (agentteam.ValidationError), so a client parses one
// shape whether the agent/team domain or the provider registry rejected the
// body. The provider field paths are already the DTO's JSON names ("id",
// "typeId", "config.apiKey").
func providerFieldErrors(err error) error {
	fields := modelprovider.FieldErrors(err)
	if fields == nil {
		return err
	}
	converted := make([]agentteam.FieldError, len(fields))
	for index, failure := range fields {
		converted[index] = agentteam.FieldError{
			Field:   failure.Field,
			Code:    failure.Code,
			Message: failure.Message,
		}
	}
	return &agentteam.ValidationError{Errors: converted}
}

// AgentWriteRequest is the body of creating or updating an agent.
//
// It carries the editable configuration only. id is optional on create (the
// agent-registry scenario creates an agent from a name alone and the system
// mints the stable id) and required on update; the two paths are explicit
// (NewAgent/Validate vs ValidateUpdate) so an update can never silently mint a
// second identity. archived is absent on purpose: archiving is its own
// operation, and a retired agent must not be resurrected by a write that
// happens to carry false.
type AgentWriteRequest struct {
	// ID is the stable identity ("agent_design_lead"). Optional on create (a
	// generated id is returned), required on update.
	ID string `json:"id"`
	// Name is the editable display name. Required.
	Name string `json:"name"`
	// Description is the editable free-form description.
	Description string `json:"description,omitempty"`
	// Avatar is the picture source. Omitting it means the deterministic
	// identicon seeded with the agent id.
	Avatar agentteam.Avatar `json:"avatar"`
	// DefaultModel is the default model binding (provider + model + tuning).
	// It is only a default: a session, a task or the team may override it.
	DefaultModel agentteam.ModelBinding `json:"defaultModel"`
	// Tools are the tool grants this agent holds, referencing registry tool ids.
	Tools []agentteam.ToolGrant `json:"tools,omitempty"`
	// SystemPrompt is the inline prompt or the prompt-file reference.
	SystemPrompt agentteam.SystemPrompt `json:"systemPrompt,omitempty"`
	// TeamCLI is the captain self-assembly allowlist over the team CLI.
	TeamCLI agentteam.TeamCLI `json:"teamCli,omitempty"`
	// Skills are the optional skill names the agent loads.
	Skills []string `json:"skills,omitempty"`
}

// Agent converts the request into the domain value the store and the run path
// share, with the avatar defaults (kind, seed) filled in.
func (r AgentWriteRequest) Agent() agentteam.Agent {
	return agentteam.Agent{
		ID:           r.ID,
		Name:         r.Name,
		Description:  r.Description,
		Avatar:       r.Avatar,
		DefaultModel: r.DefaultModel,
		Tools:        r.Tools,
		SystemPrompt: r.SystemPrompt,
		TeamCLI:      r.TeamCLI,
		Skills:       r.Skills,
	}.Normalized()
}

// NewAgent converts a create request: an empty id is minted here (the stable id
// and the deterministic identicon the spec promises for "只填名字"), and the
// avatar defaults are filled in. The returned agent is what the store persists.
func (r AgentWriteRequest) NewAgent() agentteam.Agent {
	return r.Agent().WithGeneratedID()
}

// Validate applies the create rules: an empty id is legal because NewAgent
// mints it; a supplied id must satisfy the same grammar an update does.
func (r AgentWriteRequest) Validate() error {
	return r.NewAgent().Validate()
}

// ValidateUpdate applies the update rules: the stored id is required, so an
// update can never mint a new agent.
func (r AgentWriteRequest) ValidateUpdate() error {
	return r.Agent().Validate()
}

// ValidateWithTools applies the create rules plus the spec's save-time tool
// check: every granted tool id must exist in the tenant's tool registry. The
// lookup is the caller's (the HTTP handler passes the installed tool source); a
// nil lookup behaves like Validate.
func (r AgentWriteRequest) ValidateWithTools(exists func(toolID string) bool) error {
	return r.NewAgent().ValidateWithTools(exists)
}

// AgentView is one agent as the API returns it: the stored configuration plus
// the tenant projection. An agent carries no credential, so nothing here needs
// redacting.
type AgentView struct {
	// TenantID is the owning tenant. It is a projection; request bodies never
	// declare it.
	TenantID uuid.UUID `json:"tenantId"`
	// ID is the stable identity.
	ID string `json:"id"`
	// Name is the display name.
	Name string `json:"name"`
	// Description is the free-form description.
	Description string `json:"description,omitempty"`
	// Avatar is the resolved avatar source (kind and seed always populated).
	Avatar agentteam.Avatar `json:"avatar"`
	// DefaultModel is the default model binding.
	DefaultModel agentteam.ModelBinding `json:"defaultModel"`
	// Tools are the tool grants, referencing registry tool ids.
	Tools []agentteam.ToolGrant `json:"tools,omitempty"`
	// SystemPrompt is the inline prompt or prompt-file reference.
	SystemPrompt agentteam.SystemPrompt `json:"systemPrompt,omitempty"`
	// TeamCLI is the captain self-assembly allowlist.
	TeamCLI agentteam.TeamCLI `json:"teamCli,omitempty"`
	// Skills are the optional skill names.
	Skills []string `json:"skills,omitempty"`
	// Archived marks a retired agent; a team referencing one is invalid.
	Archived bool `json:"archived,omitempty"`
}

// NewAgentView projects a stored agent for one tenant.
func NewAgentView(tenantID uuid.UUID, agent agentteam.Agent) AgentView {
	return AgentView{
		TenantID:     tenantID,
		ID:           agent.ID,
		Name:         agent.Name,
		Description:  agent.Description,
		Avatar:       agent.Avatar,
		DefaultModel: agent.DefaultModel,
		Tools:        agent.Tools,
		SystemPrompt: agent.SystemPrompt,
		TeamCLI:      agent.TeamCLI,
		Skills:       agent.Skills,
		Archived:     agent.Archived,
	}
}

// AgentTeamWriteRequest is the body of creating or updating a team.
//
// captain is a value rather than a pointer so a missing captain decodes as the
// zero seat and the failure is the field error missing_captain at
// captain.agentId — the same code the spec names and the UI shows on the
// captain slot — instead of a JSON type error. revision and hash are absent on
// purpose: only Seal may move them, and a stored hash that no longer matches
// the content is a validation error rather than something a client supplies.
type AgentTeamWriteRequest struct {
	// ID is the stable team identity. Required.
	ID string `json:"id"`
	// Name is the editable display name. Required.
	Name string `json:"name"`
	// Description is the editable free-form description.
	Description string `json:"description,omitempty"`
	// Captain is the single mandatory team lead.
	Captain agentteam.TeamMember `json:"captain"`
	// Members are the additional seats; 0..N and never the captain. A team with
	// only a captain is legal.
	Members []agentteam.TeamMember `json:"members,omitempty"`
	// Defaults are the team-wide defaults (tool list, model override).
	Defaults agentteam.TeamDefaults `json:"defaults,omitempty"`
	// Capabilities are the team-level switches (the captain's team CLI).
	Capabilities agentteam.TeamCapabilities `json:"capabilities,omitempty"`
	// FeatureBindings bind feature ports to the commands that serve them
	// (design.md §5).
	FeatureBindings []agentteam.FeatureBinding `json:"featureBindings,omitempty"`
}

// Team converts the request into an unsealed domain team. Seal it after an
// accepted write so the revision advances.
func (r AgentTeamWriteRequest) Team() *agentteam.AgentTeam {
	return &agentteam.AgentTeam{
		ID:              r.ID,
		Name:            r.Name,
		Description:     r.Description,
		Captain:         r.Captain,
		Members:         r.Members,
		Defaults:        r.Defaults,
		Capabilities:    r.Capabilities,
		FeatureBindings: r.FeatureBindings,
	}
}

// Validate applies the agent-team rules and returns an
// *agentteam.ValidationError with one FieldError per problem, or nil
// (missing_captain, duplicate_member, captain_in_members, ...).
func (r AgentTeamWriteRequest) Validate() error {
	return r.Team().Validate()
}

// AgentTeamView is one team as the API returns it: the editable configuration,
// the tenant projection and the derived state the settings page and the
// session start mode need (status, revision, hash).
//
// It is a flat struct, not an embedding of agentteam.AgentTeam, because
// AgentTeam carries a MarshalJSON that would shadow the outer type's fields and
// silently drop tenantId, status, revision and hash.
type AgentTeamView struct {
	// TenantID is the owning tenant. It is a projection; request bodies never
	// declare it.
	TenantID uuid.UUID `json:"tenantId"`
	// ID is the stable team identity.
	ID string `json:"id"`
	// Name is the display name.
	Name string `json:"name"`
	// Description is the free-form description.
	Description string `json:"description,omitempty"`
	// Captain is the single captain seat.
	Captain agentteam.TeamMember `json:"captain"`
	// Members are the additional seats.
	Members []agentteam.TeamMember `json:"members,omitempty"`
	// Defaults are the team-wide defaults.
	Defaults agentteam.TeamDefaults `json:"defaults,omitempty"`
	// Capabilities are the team-level switches.
	Capabilities agentteam.TeamCapabilities `json:"capabilities,omitempty"`
	// FeatureBindings bind feature ports to the commands that serve them.
	FeatureBindings []agentteam.FeatureBinding `json:"featureBindings,omitempty"`
	// Archived marks a retired team; archived teams are invalid and must not
	// start.
	Archived bool `json:"archived,omitempty"`
	// Status is the derived team state (valid|invalid); the session start mode
	// only offers valid teams.
	Status agentteam.TeamStatus `json:"status"`
	// Revision is the monotonic configuration revision; 0 until sealed.
	Revision uint64 `json:"revision,omitempty"`
	// Hash is the content hash the team snapshots and the UI compare.
	Hash string `json:"hash,omitempty"`
}

// NewAgentTeamView projects a stored team for one tenant.
func NewAgentTeamView(tenantID uuid.UUID, team agentteam.AgentTeam) AgentTeamView {
	return AgentTeamView{
		TenantID:        tenantID,
		ID:              team.ID,
		Name:            team.Name,
		Description:     team.Description,
		Captain:         team.Captain,
		Members:         team.Members,
		Defaults:        team.Defaults,
		Capabilities:    team.Capabilities,
		FeatureBindings: team.FeatureBindings,
		Archived:        team.Archived,
		Status:          team.Status(),
		Revision:        team.Revision(),
		Hash:            team.Hash(),
	}
}

// ModelCatalogEntry is one model in a provider's catalog. The catalog comes
// from the plugin's static declaration or from runtime discovery; it is never
// written by a client, so it appears in the view only.
type ModelCatalogEntry struct {
	// ModelID is the wire model id.
	ModelID string `json:"modelId"`
	// DisplayName is the optional human-readable name.
	DisplayName string `json:"displayName,omitempty"`
	// Capabilities are the model's capability flags (image input, reasoning,
	// ...), as declared or discovered.
	Capabilities []string `json:"capabilities,omitempty"`
	// ContextWindow is the context budget in tokens (the design's `context`
	// column).
	ContextWindow int `json:"contextWindow,omitempty"`
	// MaxOutput is the maximum output budget in tokens.
	MaxOutput int `json:"maxOutput,omitempty"`
	// ParameterSchemaRef names the parameter schema the tuning editor renders;
	// parameters the model does not support are absent, never guessed here.
	ParameterSchemaRef string `json:"parameterSchemaRef,omitempty"`
}

// ModelProviderWriteRequest is the body of creating or updating a model
// provider instance.
//
// The type vocabulary is not enumerated here: provider types are declared by
// plugins (specs/provider-registry), so a new type must be usable without an
// HTTP change. typeId is therefore required but only checked for shape.
// pluginId is absent because it follows from the type's declaration, and health
// is absent because it is measured.
//
// status and health are two axes, not one: status is the user's enablement
// (active|disabled), health is the measured result (ready|needs_auth|...). The
// domain ProviderInstance stores them in separate fields, so a disabled provider
// reads back disabled with its last health intact.
type ModelProviderWriteRequest struct {
	// ID is the stable provider identity. Required, like agent and team ids.
	ID string `json:"id"`
	// TypeID is the plugin-declared provider type ("llm", "image", ...).
	// Required; the set is open.
	TypeID string `json:"typeId"`
	// Name is the editable display name. Required.
	Name string `json:"name"`
	// CredentialRef is the credential indirection. Empty means the provider has
	// no usable credential yet (its health projects needs_auth); a literal
	// secret is rejected.
	CredentialRef string `json:"credentialRef"`
	// Endpoint is the upstream base URL; empty uses the plugin default. Speech
	// providers may use a ws/wss endpoint.
	Endpoint string `json:"endpoint,omitempty"`
	// Config is the provider-instance configuration the type's config_schema
	// describes. It must be credential free.
	Config map[string]any `json:"config,omitempty"`
	// Status is the enablement status (active|disabled); empty means active.
	Status string `json:"status,omitempty"`
}

// ProviderInstance converts the request into the domain value the store and the
// run path share. tenantID is the tenant the handler established (X-Tenant-ID);
// the body never carries it, so a shape-only validation passes uuid.Nil.
func (r ModelProviderWriteRequest) ProviderInstance(tenantID uuid.UUID) modelprovider.ProviderInstance {
	tenant := ""
	if tenantID != uuid.Nil {
		tenant = tenantID.String()
	}
	return modelprovider.ProviderInstance{
		ID:            r.ID,
		TenantID:      tenant,
		TypeID:        r.TypeID,
		Name:          r.Name,
		CredentialRef: r.CredentialRef,
		Endpoint:      r.Endpoint,
		Config:        r.Config,
		Status:        modelprovider.ProviderStatus(strings.TrimSpace(r.Status)),
	}
}

// Validate applies the provider-registry rules that do not need the plugin
// registry, and returns an *agentteam.ValidationError with one FieldError per
// problem, or nil. The rules are modelprovider's ValidateShape: this file does
// not keep a second dialect of endpoint, credential or config validation.
func (r ModelProviderWriteRequest) Validate() error {
	return providerFieldErrors(r.ProviderInstance(uuid.Nil).ValidateShape())
}

// ModelProviderView is one provider instance as the API returns it: the stored
// configuration, the owning tenant, the model catalog projection and the two
// state axes the settings page renders (enablement status, measured health).
//
// CredentialRef is a reference by construction: the registry stores no secret,
// and nothing in this view can render one.
type ModelProviderView struct {
	// TenantID is the owning tenant. It is a projection; request bodies never
	// declare it.
	TenantID uuid.UUID `json:"tenantId"`
	// ID is the stable provider identity.
	ID string `json:"id"`
	// TypeID is the plugin-declared provider type.
	TypeID string `json:"typeId"`
	// PluginID is the plugin the type and instance configuration came from.
	PluginID string `json:"pluginId,omitempty"`
	// Name is the display name.
	Name string `json:"name"`
	// CredentialRef is the credential indirection; the value is a reference,
	// never a secret.
	CredentialRef string `json:"credentialRef,omitempty"`
	// Endpoint is the upstream base URL, when the instance overrides the
	// plugin default.
	Endpoint string `json:"endpoint,omitempty"`
	// Config is the provider-instance configuration (credential free).
	Config map[string]any `json:"config,omitempty"`
	// Status is the enablement status (active|disabled).
	Status string `json:"status"`
	// Health is the measured health projection (ready|needs_auth|unreachable|
	// invalid_config|degraded); empty until a health check has run.
	Health string `json:"health,omitempty"`
	// Models is the provider's model catalog.
	Models []ModelCatalogEntry `json:"models,omitempty"`
}

// NewModelProviderView projects a stored instance for one tenant. Status and
// health are read from their own domain fields, so the two axes never overwrite
// each other.
func NewModelProviderView(tenantID uuid.UUID, instance modelprovider.ProviderInstance) ModelProviderView {
	view := ModelProviderView{
		TenantID:      tenantID,
		ID:            instance.ID,
		TypeID:        instance.TypeID,
		PluginID:      instance.PluginID,
		Name:          instance.Name,
		CredentialRef: instance.CredentialRef,
		Endpoint:      instance.Endpoint,
		Config:        instance.Config,
		Status:        string(instance.Status),
		Health:        instance.Health.State,
	}
	if view.Status == "" {
		view.Status = string(modelprovider.ProviderStatusActive)
	}
	for _, model := range instance.Models {
		view.Models = append(view.Models, ModelCatalogEntry{
			ModelID:            model.ModelID,
			DisplayName:        model.DisplayName,
			Capabilities:       append([]string(nil), model.Capabilities...),
			ContextWindow:      model.ContextWindow,
			MaxOutput:          model.MaxOutput,
			ParameterSchemaRef: model.Parameters.Schema.Ref,
		})
	}
	return view
}
