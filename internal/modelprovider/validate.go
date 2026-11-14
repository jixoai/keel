package modelprovider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Validation codes. Every FieldError carries one, so an HTTP handler, the CLI
// and a UI form can react without matching prose. The codes are part of the
// contract: do not rename them.
const (
	// CodeMissingID marks an empty instance identity.
	CodeMissingID = "missing_id"
	// CodeInvalidID marks an identity that is not a legal token.
	CodeInvalidID = "invalid_id"
	// CodeMissingTenant marks an instance with no tenant scope.
	CodeMissingTenant = "missing_tenant"
	// CodeInvalidTenant marks a tenant id that is not a legal token.
	CodeInvalidTenant = "invalid_tenant"
	// CodeMissingName marks an empty display name.
	CodeMissingName = "missing_name"

	// CodeMissingType marks an empty provider type id.
	CodeMissingType = "missing_type"
	// CodeInvalidType marks a type id outside the type-id vocabulary.
	CodeInvalidType = "invalid_type"
	// CodeUnknownType marks an instance whose type no installed plugin declares.
	CodeUnknownType = "unknown_type"
	// CodeMissingPlugin marks a type declaration with no plugin source.
	CodeMissingPlugin = "missing_plugin"
	// CodeInvalidPlugin marks a plugin id that is not a legal token.
	CodeInvalidPlugin = "invalid_plugin"
	// CodePluginMismatch marks an instance and its type declaration naming
	// different plugins.
	CodePluginMismatch = "plugin_mismatch"
	// CodeMissingCapability marks a type declaration with no capability flag.
	CodeMissingCapability = "missing_capability"
	// CodeInvalidFeaturePort marks a feature port that is not a legal token.
	CodeInvalidFeaturePort = "invalid_feature_port"

	// CodeInvalidSurfaceKind marks a session surface kind outside the closed
	// vocabulary (llm|realtime|image|video|speech|generic).
	CodeInvalidSurfaceKind = "invalid_surface_kind"
	// CodeSurfaceMismatch marks a declaration whose surface contradicts its
	// provider family: a realtime surface on an llm/image family, the realtime
	// family opening something else, or a realtime channel block without the
	// realtime surface.
	CodeSurfaceMismatch = "surface_mismatch"
	// CodeMissingSurfaceTransport marks a realtime surface that does not say how
	// to connect.
	CodeMissingSurfaceTransport = "missing_surface_transport"
	// CodeInvalidSurfaceTransport marks an unknown realtime transport.
	CodeInvalidSurfaceTransport = "invalid_surface_transport"
	// CodeInvalidAudioFormat marks a realtime audio format a client cannot
	// decode: a partial declaration, an out-of-range rate or channel count, or
	// an unknown encoding.
	CodeInvalidAudioFormat = "invalid_audio_format"
	// CodeInvalidSurfaceLimit marks a negative concurrency or session bound.
	CodeInvalidSurfaceLimit = "invalid_surface_limit"
	// CodeInvalidReconnectMode marks an unknown reconnect mode.
	CodeInvalidReconnectMode = "invalid_reconnect_mode"

	// CodeMissingSchema marks a required schema slot that declares nothing.
	CodeMissingSchema = "missing_schema"
	// CodeUnknownSchema marks a reference the plugin's schema set does not
	// contain.
	CodeUnknownSchema = "unknown_schema"
	// CodeConflictingSchema marks a reference carrying both a name and an
	// inline document.
	CodeConflictingSchema = "conflicting_schema"
	// CodeInvalidSchema marks a schema document that does not compile.
	CodeInvalidSchema = "invalid_schema"
	// CodeInvalidAuthField marks a malformed credential field declaration.
	CodeInvalidAuthField = "invalid_auth_field"
	// CodeDuplicateAuthField marks one credential field declared twice.
	CodeDuplicateAuthField = "duplicate_auth_field"
	// CodeSecretFieldUnmarked marks a credential-shaped auth field that is not
	// marked secret.
	CodeSecretFieldUnmarked = "unmarked_secret_field"

	// CodeInvalidCredentialRef marks a credential reference that is not a
	// reference (a literal credential is never stored).
	CodeInvalidCredentialRef = "invalid_credential_ref"
	// CodeInlineCredential marks a credential value written into configuration.
	CodeInlineCredential = "inline_credential"
	// CodeUnencodableConfig marks configuration that is not JSON data.
	CodeUnencodableConfig = "unencodable_config"

	// CodeInvalidEndpoint marks an endpoint that is not an absolute URL.
	CodeInvalidEndpoint = "invalid_endpoint"
	// CodeInvalidProviderStatus marks an unknown enablement status
	// (the `status` axis: active|disabled).
	CodeInvalidProviderStatus = "invalid_provider_status"
	// CodeInvalidStatus marks an unknown provider health state
	// (the `health` axis).
	CodeInvalidStatus = "invalid_status"
	// CodeCredentialInMessage marks health text carrying a credential value.
	CodeCredentialInMessage = "credential_in_message"

	// CodeMissingModel marks a catalog entry with no model id.
	CodeMissingModel = "missing_model"
	// CodeInvalidModel marks a model id that is not a legal token.
	CodeInvalidModel = "invalid_model"
	// CodeConflictingModel marks one model id declared twice with different
	// content.
	CodeConflictingModel = "conflicting_model"
	// CodeInvalidContextWindow marks a negative context window.
	CodeInvalidContextWindow = "invalid_context_window"
	// CodeInvalidMaxOutput marks a negative output budget.
	CodeInvalidMaxOutput = "invalid_max_output"
)

// FieldError is one field-level validation failure.
type FieldError struct {
	// Field is the value path ("typeId", "authSchema.fields[0].secret",
	// "models[1].modelId").
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
	Errors []FieldError
}

// Error renders every failure, so one round of feedback can fix them all.
func (e *ValidationError) Error() string {
	switch len(e.Errors) {
	case 0:
		return "provider configuration is invalid"
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

// fieldSet collects failures while validating one value.
type fieldSet struct {
	list []FieldError
}

func (s *fieldSet) add(field, code, format string, args ...any) {
	s.list = append(s.list, FieldError{Field: field, Code: code, Message: fmt.Sprintf(format, args...)})
}

// merge appends the field errors of another validation result.
func (s *fieldSet) merge(err error) {
	for _, field := range FieldErrors(err) {
		s.list = append(s.list, field)
	}
}

func (s *fieldSet) err() error {
	if len(s.list) == 0 {
		return nil
	}
	return &ValidationError{Errors: s.list}
}

// Vocabulary patterns.
var (
	// identityPattern is the legal identity shape: a stable, URL- and CLI-safe
	// token (instance ids, tenant ids, plugin ids).
	identityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	// typeIDPattern keeps provider type ids lowercase, like feature ports: they
	// are URL segments and CLI arguments.
	typeIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	// portPattern is the feature-port vocabulary ("image.generate").
	portPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._:-][a-z0-9]+)*$`)
	// modelIDPattern accepts the ids real providers use, including namespaced
	// forms such as "@cf/cloudflare/clef" and "openai/gpt-4o".
	modelIDPattern = regexp.MustCompile(`^@?[A-Za-z0-9][A-Za-z0-9._:@/-]*$`)
	// authFieldPattern is an environment-style field name.
	authFieldPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// schemaNamePattern is the name vocabulary of a plugin schema set.
	schemaNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._:/-]*$`)
	// credentialRefPattern is the credential reference vocabulary: a credential
	// is always an indirection — an opaque scheme reference (vault://name), the
	// host's secret-reference language (${NAME}, env:NAME, secret:NAME) — and
	// never a literal value. The DTO, the CLI and this package share it, so one
	// reference form cannot be legal on one surface and rejected on another.
	credentialRefPattern = regexp.MustCompile(
		`^(?:[a-z][a-z0-9+.-]*://[A-Za-z0-9._~%:/@-]+|\$\{[A-Za-z_][A-Za-z0-9_]{0,63}\}|(?:env|secret):[A-Za-z_][A-Za-z0-9_]{0,63})$`)
)

// credentialLiteralPatterns are the literal credential shapes a health message
// or configuration value must never carry. Detection is best effort: it is a
// last guard on text the domain stores, not a substitute for keeping secrets
// behind references.
var credentialLiteralPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`),                // OpenAI-style bearer tokens
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`), // "Bearer <token>"
	regexp.MustCompile(`\bAKIA[0-9A-Z]{12,}\b`),                 // AWS access key ids
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{8,}`),         // Slack tokens
}

// Normalized returns a copy with the canonical spelling of every identity and
// defaulted presentation field. Set-like slices (capabilities, feature ports)
// are trimmed and deduplicated.
func (t ProviderType) Normalized() ProviderType {
	normalized := t
	normalized.TypeID = strings.TrimSpace(t.TypeID)
	normalized.DisplayName = strings.TrimSpace(t.DisplayName)
	normalized.Icon = strings.TrimSpace(t.Icon)
	normalized.PluginID = strings.TrimSpace(t.PluginID)
	normalized.Capabilities = normalizeFlags(t.Capabilities)
	normalized.FeaturePorts = normalizeFlags(t.FeaturePorts)
	normalized.Surface = t.Surface.normalized()
	normalized.ConfigSchema = cloneSchemaRef(t.ConfigSchema)
	normalized.ModelSchema = cloneSchemaRef(t.ModelSchema)
	normalized.Parameters = cloneParameterSchema(t.Parameters)
	if t.AuthSchema != nil {
		auth := AuthSchema{Fields: make([]AuthField, len(t.AuthSchema.Fields))}
		for index, field := range t.AuthSchema.Fields {
			auth.Fields[index] = AuthField{
				Name:     strings.TrimSpace(field.Name),
				Label:    strings.TrimSpace(field.Label),
				Secret:   field.Secret,
				Required: field.Required,
			}
		}
		normalized.AuthSchema = &auth
	}
	return normalized
}

// Normalized returns a copy with the canonical model id, the defaulted display
// name (the model id) and normalized capability flags. Parameter schemas are
// deep-copied so later merges cannot mutate the declaration.
func (m ModelDescriptor) Normalized() ModelDescriptor {
	normalized := m
	normalized.ModelID = strings.TrimSpace(m.ModelID)
	normalized.DisplayName = strings.TrimSpace(m.DisplayName)
	if normalized.DisplayName == "" && normalized.ModelID != "" {
		normalized.DisplayName = normalized.ModelID
	}
	normalized.Capabilities = normalizeFlags(m.Capabilities)
	normalized.Parameters = cloneParameterSchema(m.Parameters)
	return normalized
}

// Normalize returns a canonical copy: trimmed identities, the explicit
// unchecked health state and a deduplicated model catalog.
//
// Duplicate catalog entries are resolved by identity (NormalizeCatalog):
// identical entries collapse and two different declarations of one model id are
// an error. That error is why Normalize returns one: a conflicting catalog has
// no canonical form, and silently picking a winner would make what the provider
// serves depend on declaration order.
func (i ProviderInstance) Normalize() (ProviderInstance, error) {
	normalized := i
	normalized.ID = strings.TrimSpace(i.ID)
	normalized.TenantID = strings.TrimSpace(i.TenantID)
	normalized.TypeID = strings.TrimSpace(i.TypeID)
	normalized.PluginID = strings.TrimSpace(i.PluginID)
	normalized.Name = strings.TrimSpace(i.Name)
	normalized.CredentialRef = strings.TrimSpace(i.CredentialRef)
	normalized.Endpoint = strings.TrimSpace(i.Endpoint)
	normalized.Config = cloneJSONMap(i.Config)
	normalized.Parameters = cloneParameterSchema(i.Parameters)
	normalized.Status = ProviderStatus(strings.TrimSpace(string(i.Status)))
	if normalized.Status == "" {
		normalized.Status = ProviderStatusActive
	}
	normalized.Health = HealthStatus{
		State:     strings.TrimSpace(i.Health.State),
		Message:   strings.TrimSpace(i.Health.Message),
		CheckedAt: i.Health.CheckedAt,
	}
	if normalized.Health.State == "" {
		normalized.Health.State = HealthUnchecked
	}
	models, err := NormalizeCatalog(i.Models)
	normalized.Models = models
	return normalized, err
}

// NormalizeCatalog canonicalizes a provider's model catalog. Entries are
// normalized and the result keeps first-seen order.
//
// Duplicates are resolved by normalized content: entries whose descriptors are
// identical collapse to the first one, while two entries that share a model id
// but differ in content are a validation error. Silently preferring one of them
// would make the effective catalog depend on declaration order, which is
// exactly the ambiguity the registry must not have.
func NormalizeCatalog(models []ModelDescriptor) ([]ModelDescriptor, error) {
	if models == nil {
		return nil, nil
	}
	set := &fieldSet{}
	normalized := make([]ModelDescriptor, 0, len(models))
	firstSeen := make(map[string]int, len(models))
	firstDescriptor := make(map[string]ModelDescriptor, len(models))
	for position, model := range models {
		current := model.Normalized()
		id := current.ModelID
		if id == "" {
			// A missing id has no identity to dedupe on; model validation
			// reports it at models[position].modelId.
			normalized = append(normalized, current)
			continue
		}
		first, seen := firstSeen[id]
		if !seen {
			firstSeen[id] = position
			firstDescriptor[id] = current
			normalized = append(normalized, current)
			continue
		}
		if reflect.DeepEqual(firstDescriptor[id], current) {
			continue // identical declaration: collapse
		}
		set.add(
			fmt.Sprintf("models[%d].modelId", position),
			CodeConflictingModel,
			"model %q is declared again at models[%d] with different content; one model id must have one descriptor",
			id, first,
		)
	}
	return normalized, set.err()
}

// Validate checks one provider type declaration against the schema set the
// declaring plugin shipped. A declaration that fails here cannot be used by any
// provider instance.
func (t ProviderType) Validate(schemas SchemaSet) error {
	normalized := t.Normalized()
	set := &fieldSet{}
	validateTypeID(set, "typeId", normalized.TypeID)
	validateDisplayName(set, "displayName", normalized.DisplayName)
	validateIdentity(set, "pluginId", normalized.PluginID, CodeMissingPlugin, CodeInvalidPlugin)
	if len(normalized.Capabilities) == 0 {
		set.add("capabilities", CodeMissingCapability, "a provider type must declare at least one capability flag")
	}
	for index, port := range normalized.FeaturePorts {
		if !portPattern.MatchString(port) {
			set.add(
				fmt.Sprintf("featurePorts[%d]", index),
				CodeInvalidFeaturePort,
				"feature port %q must be lowercase, start with a letter and contain only letters, digits, '.', '_', ':' or '-'",
				port,
			)
		}
	}
	validateSchemaRef(set, "configSchema", normalized.ConfigSchema, schemas, true)
	validateAuthSchema(set, "authSchema", normalized.AuthSchema)
	validateSchemaRef(set, "modelSchema", normalized.ModelSchema, schemas, true)
	validateParameterSchema(set, "parameters", normalized.Parameters, schemas)
	validateSurface(set, "surface", normalized.TypeID, normalized.Surface)
	return set.err()
}

// ValidateDeclaration checks a provider type declared inside a plugin manifest
// (`provides.provider_types`). It applies the identity, capability and
// required-schema rules without the plugin's SchemaSet: pluginId may be empty
// because the declaring plugin is the manifest itself, and schema references are
// resolved later by Validate at assembly time. A declaration that fails here
// cannot be installed; a declaration that passes here can still fail assembly
// if its schema references do not resolve.
func (t ProviderType) ValidateDeclaration() error {
	normalized := t.Normalized()
	set := &fieldSet{}
	validateTypeID(set, "typeId", normalized.TypeID)
	validateDisplayName(set, "displayName", normalized.DisplayName)
	if normalized.PluginID != "" {
		validateIdentity(set, "pluginId", normalized.PluginID, CodeMissingPlugin, CodeInvalidPlugin)
	}
	if len(normalized.Capabilities) == 0 {
		set.add("capabilities", CodeMissingCapability, "a provider type must declare at least one capability flag")
	}
	for index, port := range normalized.FeaturePorts {
		if !ValidFeaturePort(port) {
			set.add(
				fmt.Sprintf("featurePorts[%d]", index),
				CodeInvalidFeaturePort,
				"feature port %q must be lowercase, start with a letter and contain only letters, digits, '.', '_', ':' or '-'",
				port,
			)
		}
	}
	validateSchemaRef(set, "configSchema", normalized.ConfigSchema, nil, true)
	validateAuthSchema(set, "authSchema", normalized.AuthSchema)
	validateSchemaRef(set, "modelSchema", normalized.ModelSchema, nil, true)
	validateParameterSchema(set, "parameters", normalized.Parameters, nil)
	validateSurface(set, "surface", normalized.TypeID, normalized.Surface)
	return set.err()
}

// ValidFeaturePort reports whether port satisfies the feature-port vocabulary
// ("image.generate", "llm.chat"). Plugin manifests and provider types use the
// same rule.
func ValidFeaturePort(port string) bool { return portPattern.MatchString(port) }

// validateSurface checks one session surface declaration against its family.
// The rules exist so a client can mount a panel without guessing: the kind must
// be a vocabulary member, the realtime surface is exclusive to the realtime
// family (a live audio channel is never a mode of llm or image, and the
// realtime family never opens another surface), and a realtime surface must say
// how to connect.
func validateSurface(set *fieldSet, field, typeID string, surface SurfaceDescriptor) {
	kind := strings.TrimSpace(surface.SurfaceKind)
	if kind != "" && !IsSurfaceKind(kind) {
		set.add(
			field+".surfaceKind",
			CodeInvalidSurfaceKind,
			"unknown surface kind %q (want one of %s)",
			surface.SurfaceKind, strings.Join(SurfaceKinds(), ", "),
		)
	}
	switch {
	case kind == SurfaceRealtime && IsBuiltinTypeID(typeID) && typeID != TypeRealtime:
		set.add(
			field+".surfaceKind",
			CodeSurfaceMismatch,
			"the realtime surface belongs to the %q family; a %q provider must not open a live audio channel",
			TypeRealtime, typeID,
		)
	case typeID == TypeRealtime && kind != "" && IsSurfaceKind(kind) && kind != SurfaceRealtime:
		set.add(
			field+".surfaceKind",
			CodeSurfaceMismatch,
			"provider family %q always opens the realtime surface, got %q",
			TypeRealtime, surface.SurfaceKind,
		)
	}
	effective := kind
	if effective == "" {
		effective = DefaultSurfaceKind(typeID)
	}
	if surface.Realtime == nil {
		if effective == SurfaceRealtime {
			set.add(
				field+".realtime.transport",
				CodeMissingSurfaceTransport,
				"a realtime surface must declare its transport (%s)",
				strings.Join(RealtimeTransports(), " or "),
			)
		}
		return
	}
	if effective != SurfaceRealtime {
		set.add(
			field+".realtime",
			CodeSurfaceMismatch,
			"a realtime channel block requires the realtime surface, got %q",
			effective,
		)
		return
	}
	validateRealtimeSurface(set, field+".realtime", *surface.Realtime)
}

// validateRealtimeSurface checks the channel declaration of a realtime surface:
// the transport (required), the audio format (when declared), the session
// bounds and the reconnect policy.
func validateRealtimeSurface(set *fieldSet, field string, realtime RealtimeSurface) {
	transport := strings.TrimSpace(realtime.Transport)
	switch {
	case transport == "":
		set.add(
			field+".transport",
			CodeMissingSurfaceTransport,
			"a realtime surface must declare its transport (%s)",
			strings.Join(RealtimeTransports(), " or "),
		)
	case !IsRealtimeTransport(transport):
		set.add(
			field+".transport",
			CodeInvalidSurfaceTransport,
			"unknown realtime transport %q (want %s)",
			realtime.Transport, strings.Join(RealtimeTransports(), " or "),
		)
	}
	validateAudioFormat(set, field+".audio", realtime.Audio)
	if realtime.MaxConcurrentSessions < 0 {
		set.add(
			field+".maxConcurrentSessions",
			CodeInvalidSurfaceLimit,
			"max concurrent sessions must not be negative, got %d",
			realtime.MaxConcurrentSessions,
		)
	}
	if realtime.MaxSessionDurationSeconds < 0 {
		set.add(
			field+".maxSessionDurationSeconds",
			CodeInvalidSurfaceLimit,
			"max session duration must not be negative, got %d",
			realtime.MaxSessionDurationSeconds,
		)
	}
	if mode := strings.TrimSpace(realtime.Reconnect.Mode); mode != "" && !IsReconnectMode(mode) {
		set.add(
			field+".reconnect.mode",
			CodeInvalidReconnectMode,
			"unknown reconnect mode %q (want %s)",
			realtime.Reconnect.Mode, strings.Join(ReconnectModes(), ", "),
		)
	}
	if realtime.Reconnect.WindowSeconds < 0 {
		set.add(
			field+".reconnect.windowSeconds",
			CodeInvalidSurfaceLimit,
			"reconnect window must not be negative, got %d",
			realtime.Reconnect.WindowSeconds,
		)
	}
}

// validateAudioFormat checks a declared realtime audio format. A zero value
// means "negotiated at connect time" and is legal; once any field is declared
// the whole format must be decodable, so a rate, a channel count and a known
// encoding are all required.
func validateAudioFormat(set *fieldSet, field string, format AudioFormat) {
	if format.IsZero() {
		return
	}
	if format.SampleRateHz < minRealtimeSampleRateHz || format.SampleRateHz > maxRealtimeSampleRateHz {
		set.add(
			field+".sampleRateHz",
			CodeInvalidAudioFormat,
			"sample rate must be between %d and %d Hz for a realtime channel, got %d",
			minRealtimeSampleRateHz, maxRealtimeSampleRateHz, format.SampleRateHz,
		)
	}
	if format.Channels < 1 || format.Channels > maxRealtimeChannels {
		set.add(
			field+".channels",
			CodeInvalidAudioFormat,
			"channel count must be between 1 and %d, got %d",
			maxRealtimeChannels, format.Channels,
		)
	}
	encoding := strings.TrimSpace(format.Encoding)
	switch {
	case encoding == "":
		set.add(
			field+".encoding",
			CodeInvalidAudioFormat,
			"a declared audio format must name its encoding (%s)",
			strings.Join(AudioEncodings(), ", "),
		)
	case !IsAudioEncoding(encoding):
		set.add(
			field+".encoding",
			CodeInvalidAudioFormat,
			"unknown audio encoding %q (want one of %s)",
			format.Encoding, strings.Join(AudioEncodings(), ", "),
		)
	}
}

// Validate checks one model catalog entry on its own. The same rules run inside
// ProviderInstance.Validate with the catalog path prefixed.
func (m ModelDescriptor) Validate(schemas SchemaSet) error {
	set := &fieldSet{}
	validateModel(set, "", m.Normalized(), schemas)
	return set.err()
}

// Validate checks one provider instance against the installed type
// declarations and their schema sets.
//
// types is the installed TypeSet: an instance of an undeclared type is invalid
// even when the type id is well formed, because nothing could execute it.
// Declarations in types are expected to have passed ProviderType.Validate when
// the plugin was assembled, so only the instance side is checked here.
// schemas is the union of the installed plugins' schema sets.
func (i ProviderInstance) Validate(types TypeSet, schemas SchemaSet) error {
	set := &fieldSet{}
	normalized, err := i.WithTypeDefaults(types).Normalize()
	if err != nil {
		set.merge(err)
	}

	validateIdentity(set, "id", normalized.ID, CodeMissingID, CodeInvalidID)
	validateIdentity(set, "tenantId", normalized.TenantID, CodeMissingTenant, CodeInvalidTenant)
	i.validateContent(set, normalized, schemas)
	validateTypeID(set, "typeId", normalized.TypeID)

	if typeIDPattern.MatchString(normalized.TypeID) {
		declaration, declared := types[normalized.TypeID]
		switch {
		case !declared:
			set.add("typeId", CodeUnknownType, "provider type %q is not declared by any installed plugin", normalized.TypeID)
		case declaration.PluginID != "" && normalized.PluginID != declaration.PluginID:
			set.add(
				"pluginId",
				CodePluginMismatch,
				"provider instance names plugin %q but type %q is declared by plugin %q",
				normalized.PluginID, normalized.TypeID, declaration.PluginID,
			)
		}
	}
	return set.err()
}

// ValidateShape checks every instance rule that does not depend on the
// installed registry or on tenant scope: identity, name, type-id grammar,
// credential reference, endpoint, configuration, parameter defaults, model
// catalog, enablement status and health. The HTTP and CLI write paths call it
// on a request body before the handler supplies the tenant and the installed
// type set; Validate adds the tenant requirement, type existence, plugin match
// and schema resolution on top.
//
// Schema references are only checked for shape here: resolving a ref needs the
// declaring plugin's SchemaSet, which only assembly has.
func (i ProviderInstance) ValidateShape() error {
	set := &fieldSet{}
	normalized, err := i.Normalize()
	if err != nil {
		set.merge(err)
	}
	validateIdentity(set, "id", normalized.ID, CodeMissingID, CodeInvalidID)
	validateTypeID(set, "typeId", normalized.TypeID)
	i.validateContent(set, normalized, nil)
	return set.err()
}

// validateContent checks the fields every write path shares. schemas nil means
// "no plugin schema set is available here": schema refs are checked for shape
// only.
func (i ProviderInstance) validateContent(set *fieldSet, normalized ProviderInstance, schemas SchemaSet) {
	validateDisplayName(set, "name", normalized.Name)
	validateCredentialRef(set, "credentialRef", normalized.CredentialRef)
	validateEndpoint(set, "endpoint", normalized.Endpoint)
	validateConfig(set, "config", normalized.Config)
	validateParameterSchema(set, "parameters", normalized.Parameters, schemas)
	for index, model := range normalized.Models {
		validateModel(set, fmt.Sprintf("models[%d]", index), model, schemas)
	}
	validateProviderStatus(set, "status", normalized.Status)
	validateHealth(set, "health", normalized.Health)
}

// validateProviderStatus checks the enablement axis. The empty value is the
// active default Normalize fills in, so only an unknown non-empty value fails.
func validateProviderStatus(set *fieldSet, field string, status ProviderStatus) {
	switch status {
	case "", ProviderStatusActive, ProviderStatusDisabled:
	default:
		set.add(
			field,
			CodeInvalidProviderStatus,
			"unknown provider status %q (want %q or %q)",
			status, ProviderStatusActive, ProviderStatusDisabled,
		)
	}
}

// Validate checks one health state. The empty state means "not checked yet" and
// is legal; any other value must be one of the Health* constants.
func (h HealthStatus) Validate() error {
	set := &fieldSet{}
	validateHealth(set, "", h)
	return set.err()
}

// validateTypeID checks a provider type id against the lowercase family
// vocabulary. It does not require the type to be a built-in one: plugins may
// declare new families.
func validateTypeID(set *fieldSet, field, id string) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		set.add(field, CodeMissingType, "provider type id must not be empty")
		return
	}
	if !typeIDPattern.MatchString(trimmed) {
		set.add(
			field,
			CodeInvalidType,
			"provider type id %q must be lowercase, start with a letter and contain only letters, digits, '.', '_' or '-'",
			id,
		)
	}
}

// validateIdentity checks one identity token.
func validateIdentity(set *fieldSet, field, value, missingCode, invalidCode string) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		set.add(field, missingCode, "%s must not be empty", field)
		return
	}
	if !identityPattern.MatchString(trimmed) {
		set.add(
			field,
			invalidCode,
			"%q must start with a letter or digit and contain only letters, digits, '.', '_', ':' or '-'",
			value,
		)
	}
}

// validateDisplayName checks a display name.
func validateDisplayName(set *fieldSet, field, name string) {
	if strings.TrimSpace(name) == "" {
		set.add(field, CodeMissingName, "%s must not be empty", field)
	}
}

// validateModelID checks a model identity. Model ids are provider-specific, so
// the shape is permissive; what is forbidden is whitespace, path traversal and
// control characters, because the id doubles as a cache key.
func validateModelID(set *fieldSet, field, id string) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		set.add(field, CodeMissingModel, "model id must not be empty")
		return
	}
	if !modelIDPattern.MatchString(trimmed) || strings.Contains(trimmed, "..") {
		set.add(
			field,
			CodeInvalidModel,
			"model id %q must start with a letter, digit or '@' and contain only letters, digits, '.', '_', ':', '@', '/' or '-' (no '..')",
			id,
		)
	}
}

// validateSchemaRef checks one schema slot. required reports whether the slot
// must declare a schema at all; optional slots (parameter schemas) may be
// empty.
func validateSchemaRef(set *fieldSet, field string, ref SchemaRef, schemas SchemaSet, required bool) {
	name := strings.TrimSpace(ref.Ref)
	hasName := name != ""
	hasInline := len(bytes.TrimSpace(ref.Inline)) > 0
	switch {
	case !hasName && !hasInline:
		if required {
			set.add(field, CodeMissingSchema, "a schema is required; declare a plugin schema name or an inline JSON Schema")
		}
	case hasName && hasInline:
		set.add(field, CodeConflictingSchema, "a schema slot carries either a ref or an inline document, never both")
	case hasName:
		if !schemaNamePattern.MatchString(name) {
			set.add(
				field+".ref",
				CodeInvalidSchema,
				"schema name %q must start with a letter and contain only letters, digits, '.', '_', ':', '/' or '-'",
				name,
			)
			return
		}
		// A nil schema set means the caller has no plugin assembly context
		// (a request body before the installed registry is known): the name is
		// checked for shape only, and resolution happens in Validate.
		if schemas == nil {
			return
		}
		if _, declared := schemas[name]; !declared {
			set.add(field+".ref", CodeUnknownSchema, "schema %q is not declared by the plugin's schema set", name)
		}
	default:
		if err := compileInlineSchema(ref.Inline); err != nil {
			set.add(field+".inline", CodeInvalidSchema, "inline schema does not compile as a JSON Schema: %v", err)
		}
	}
}

// Validate compiles every document in the set. Plugin assembly calls it once per
// plugin: a schema that does not compile must fail assembly, not the first
// provider call that references it.
func (s SchemaSet) Validate() error {
	set := &fieldSet{}
	names := make([]string, 0, len(s))
	for name := range s {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !schemaNamePattern.MatchString(name) {
			set.add(
				fmt.Sprintf("schemas[%q]", name),
				CodeInvalidSchema,
				"schema name %q must start with a letter and contain only letters, digits, '.', '_', ':', '/' or '-'",
				name,
			)
			continue
		}
		if err := compileInlineSchema(s[name]); err != nil {
			set.add(fmt.Sprintf("schemas[%q]", name), CodeInvalidSchema, "schema %q does not compile as a JSON Schema: %v", name, err)
		}
	}
	return set.err()
}

// compileInlineSchema compiles a raw JSON Schema document held in memory.
func compileInlineSchema(raw json.RawMessage) error {
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	// A synthetic in-memory location; each document is compiled alone so no two
	// inline schemas can clash.
	const location = "mem:///modelprovider/schema.json"
	if err := compiler.AddResource(location, document); err != nil {
		return err
	}
	_, err = compiler.Compile(location)
	return err
}

// validateAuthSchema checks the credential field declarations. A nil schema
// means the plugin forgot to declare it; an empty field list is the explicit
// declaration of a type that needs no credential.
func validateAuthSchema(set *fieldSet, field string, schema *AuthSchema) {
	if schema == nil {
		set.add(
			field,
			CodeMissingSchema,
			"the provider type must declare an auth schema; a type that needs no credential declares an empty field list",
		)
		return
	}
	seen := make(map[string]int, len(schema.Fields))
	for index, authField := range schema.Fields {
		fieldPath := fmt.Sprintf("%s.fields[%d]", field, index)
		name := strings.TrimSpace(authField.Name)
		if name == "" {
			set.add(fieldPath+".name", CodeInvalidAuthField, "auth field must be named")
			continue
		}
		if !authFieldPattern.MatchString(name) {
			set.add(
				fieldPath+".name",
				CodeInvalidAuthField,
				"auth field %q must start with a letter or '_' and contain only letters, digits or '_'",
				authField.Name,
			)
			continue
		}
		if previous, duplicate := seen[name]; duplicate {
			set.add(
				fieldPath+".name",
				CodeDuplicateAuthField,
				"auth field %q is already declared at authSchema.fields[%d]",
				name, previous,
			)
			continue
		}
		seen[name] = index
		if credentialValueKey(name) && !authField.Secret {
			set.add(
				fieldPath+".secret",
				CodeSecretFieldUnmarked,
				"auth field %q holds a credential and must be marked secret",
				name,
			)
		}
	}
}

// validateParameterSchema checks one parameter layer: its schema reference (when
// declared) must resolve, and its defaults must be JSON data with no credential
// value or second credential reference hidden in them. Defaults are persisted
// configuration and are merged into EffectiveParameters, so a key such as
// {"apiKey": "..."} would otherwise enter the runtime through a side door.
func validateParameterSchema(set *fieldSet, field string, schema ParameterSchema, schemas SchemaSet) {
	validateSchemaRef(set, field+".schema", schema.Schema, schemas, false)
	if schema.Defaults == nil {
		return
	}
	if _, err := json.Marshal(schema.Defaults); err != nil {
		set.add(field+".defaults", CodeUnencodableConfig, "parameter defaults must be JSON data: %v", err)
	}
	for _, failure := range ScanConfigForCredentials(schema.Defaults, field+".defaults") {
		set.list = append(set.list, failure)
	}
}

// validateModel checks one catalog entry under the given path prefix ("" for a
// standalone entry, "models[0]" inside an instance).
func validateModel(set *fieldSet, prefix string, model ModelDescriptor, schemas SchemaSet) {
	validateModelID(set, path(prefix, "modelId"), model.ModelID)
	if model.ContextWindow < 0 {
		set.add(path(prefix, "contextWindow"), CodeInvalidContextWindow, "context window must not be negative, got %d", model.ContextWindow)
	}
	if model.MaxOutput < 0 {
		set.add(path(prefix, "maxOutput"), CodeInvalidMaxOutput, "max output must not be negative, got %d", model.MaxOutput)
	}
	validateParameterSchema(set, path(prefix, "parameters"), model.Parameters, schemas)
}

// validateHealth checks one health state plus its message. field is "status"
// inside an instance and "" when the health status is validated on its own.
func validateHealth(set *fieldSet, field string, health HealthStatus) {
	statePath := "state"
	messagePath := "message"
	if field != "" {
		statePath = field
		messagePath = field + ".message"
	}
	switch strings.TrimSpace(health.State) {
	case "", HealthUnchecked, HealthReady, HealthNeedsAuth, HealthUnreachable, HealthInvalidConfig, HealthDegraded:
	default:
		set.add(
			statePath,
			CodeInvalidStatus,
			"unknown provider status %q (want %q, %q, %q, %q, %q or %q)",
			health.State, HealthUnchecked, HealthReady, HealthNeedsAuth, HealthUnreachable, HealthInvalidConfig, HealthDegraded,
		)
	}
	if containsCredentialLiteral(health.Message) {
		set.add(
			messagePath,
			CodeCredentialInMessage,
			"health message must not carry a credential value; report the failure without the secret",
		)
	}
}

// validateCredentialRef checks the credential reference. Empty means "this
// provider needs no credential" and is legal; anything else must be a
// reference, never a literal credential.
func validateCredentialRef(set *fieldSet, field, ref string) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return
	}
	if !credentialRefPattern.MatchString(trimmed) {
		set.add(
			field,
			CodeInvalidCredentialRef,
			"the value is not a credential reference; use ${NAME}, env:NAME or secret:NAME — a literal credential is never stored",
		)
	}
}

// validateEndpoint checks the base URL. Speech providers legitimately use
// websocket endpoints, so ws and wss are accepted alongside http and https.
func validateEndpoint(set *fieldSet, field, endpoint string) {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		return
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		set.add(field, CodeInvalidEndpoint, "%q is not an absolute URL", endpoint)
		return
	}
	switch parsed.Scheme {
	case "http", "https", "ws", "wss":
	default:
		set.add(field, CodeInvalidEndpoint, "%q must use http, https, ws or wss", endpoint)
		return
	}
	if parsed.User != nil {
		set.add(field, CodeInlineCredential, "endpoint must not embed credentials; reference them through credentialRef")
	}
}

// validateConfig checks instance configuration: JSON data, no credential value
// under a credential-shaped key and no second credential reference (the
// instance's credentialRef is the only one).
func validateConfig(set *fieldSet, field string, config map[string]any) {
	if config == nil {
		return
	}
	if _, err := json.Marshal(config); err != nil {
		set.add(field, CodeUnencodableConfig, "provider configuration must be JSON data: %v", err)
	}
	scanConfig(set, field, config)
}

// ScanConfigForCredentials walks one configuration object and reports every
// credential-shaped key and literal credential value at its field path. It is
// the one scanner the provider registry, the agent/team registry and the HTTP
// DTO share: the same map yields the same verdict on every surface.
func ScanConfigForCredentials(object map[string]any, field string) []FieldError {
	set := &fieldSet{}
	scanConfig(set, field, object)
	return set.list
}

// scanConfig walks configuration values in sorted key order so reported fields
// are deterministic.
func scanConfig(set *fieldSet, field string, object map[string]any) {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		path := field + "." + key
		if credentialValueKey(key) {
			set.add(
				path,
				CodeInlineCredential,
				"credentials must be referenced, not inlined; %q cannot appear in provider configuration",
				key,
			)
			continue
		}
		if credentialRefKey(key) {
			set.add(
				path,
				CodeInlineCredential,
				"the credential reference is a property of the instance (credentialRef), not of its configuration",
			)
			continue
		}
		scanConfigValue(set, path, object[key])
	}
}

// scanConfigValue walks one configuration value.
func scanConfigValue(set *fieldSet, field string, value any) {
	switch typed := value.(type) {
	case map[string]any:
		scanConfig(set, field, typed)
	case []any:
		for index, item := range typed {
			scanConfigValue(set, fmt.Sprintf("%s[%d]", field, index), item)
		}
	case string:
		if containsCredentialLiteral(typed) {
			set.add(field, CodeInlineCredential, "configuration value looks like a literal credential; reference it through credentialRef instead")
		}
	}
}

// CredentialValueKey reports whether a field name is a credential value slot.
// It is the single vocabulary every layer uses (provider configuration,
// parameter defaults, agent tuning, tool config, team defaults), so one
// configuration cannot be legal in one layer and a credential leak in another.
func CredentialValueKey(key string) bool {
	switch normalizeKey(key) {
	case "apikey", "apitoken", "apisecret", "accesstoken", "refreshtoken",
		"bearer", "bearertoken", "authorization", "clientsecret",
		"credential", "credentials", "password", "passwd", "secret", "token",
		"privatekey", "accesskey", "accesskeyid", "secretkey", "secretaccesskey":
		return true
	}
	return false
}

// credentialValueKey is the package-internal spelling of CredentialValueKey.
func credentialValueKey(key string) bool { return CredentialValueKey(key) }

// CredentialRefKey reports whether a field name is a second credential
// reference. Credential references belong to ProviderInstance.CredentialRef;
// one instance has exactly one.
func CredentialRefKey(key string) bool {
	switch normalizeKey(key) {
	case "credentialref", "apikeyref", "secretref":
		return true
	}
	return false
}

// credentialRefKey is the package-internal spelling of CredentialRefKey.
func credentialRefKey(key string) bool { return CredentialRefKey(key) }

// normalizeKey folds a field name for shape comparison: separators are removed
// and the result is lowercased.
func normalizeKey(key string) string {
	return strings.Map(func(character rune) rune {
		switch character {
		case '_', '-', '.', ' ':
			return -1
		}
		return unicode.ToLower(character)
	}, key)
}

// ContainsCredentialLiteral reports whether text carries a literal credential
// shape (best effort; see credentialLiteralPatterns). Every layer that stores
// free text (health messages, config values, tuning) uses this one detector.
func ContainsCredentialLiteral(text string) bool { return containsCredentialLiteral(text) }

// containsCredentialLiteral reports whether text carries a literal credential
// shape (best effort; see credentialLiteralPatterns).
func containsCredentialLiteral(text string) bool {
	for _, pattern := range credentialLiteralPatterns {
		if pattern.MatchString(text) {
			return true
		}
	}
	return false
}

// normalizeFlags trims a set-like slice, drops empty entries and removes
// duplicates while keeping first-seen order.
func normalizeFlags(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

// path joins a validation path prefix and one field name.
func path(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}
