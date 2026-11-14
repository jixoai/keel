// Package modelprovider defines the domain model of the model provider
// registry: the provider types plugins declare, the tenant-scoped provider
// instances created from them, their model catalogs, the parameter-schema
// layers a model inherits and the health state of an instance.
//
// # Position in the stack
//
// This is the registry-level contract, not the executable configuration layer.
// internal/modelcfg resolves one tenant's catalog document into executable
// provider/model views; this package describes what a plugin declares (a
// provider *type*) and what a tenant stores (a provider *instance* with its
// model catalog). A caller picks a (providerId, modelId) pair from an instance
// and hands that identity to the runtime, so the registry can grow without
// reshaping executable configuration.
//
// # Rules this package owns
//
//   - Provider types are declared by plugins. The first-batch families
//     llm/realtime/image/video/decision/tts/stt are the vocabulary the harness
//     ships (BuiltinTypeIDs); the vocabulary stays open, because a plugin may
//     declare "embedding" and must be accepted without a code change. TTS, STT
//     and realtime are their own families: speech synthesis, speech
//     transcription and a live audio channel each have their own configuration,
//     transport and session shape, so none may be folded into llm or image.
//   - A type may declare the session surface it opens (SurfaceDescriptor): the
//     surface kind decides which composer and timeline a client mounts. The
//     kind is derived from the family id when omitted (DefaultSurfaceKind), the
//     realtime surface is exclusive to the realtime family, and a realtime
//     surface must declare how to connect.
//   - A provider instance reaches its credential only through CredentialRef:
//     ${NAME}, env:NAME or secret:NAME. No type in this package carries a
//     credential value, and instance configuration may not smuggle one in under
//     a credential-shaped key.
//   - Type, model and parameter declarations name their JSON Schemas either by
//     reference (SchemaRef.Ref, resolved against the plugin's SchemaSet) or
//     inline (SchemaRef.Inline, compiled); both forms are checked, so a broken
//     plugin declaration fails at assembly time instead of at the first call.
//   - The model catalog is a set: identical duplicate entries collapse and two
//     different declarations of one model id are a conflict rather than an
//     order-dependent winner (NormalizeCatalog).
//   - Tunable parameters merge in exactly one order: type default → provider
//     instance → model (EffectiveParameters).
//
// # What this package is not
//
// It performs no IO and imports no store, HTTP, plugin or runtime package.
// Tenant isolation, deletion protection (a provider referenced by a preset is
// disabled, not deleted), catalog caching and "a failed health check never
// deletes user configuration" are enforcement points of the layers above; this
// package supplies the identity vocabulary, ConfigHash and status vocabulary
// they need.
//
// Spec: openspec/changes/keel-agent-team-v1/specs/provider-registry/spec.md
package modelprovider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Provider type ids of the first batch. A provider *type* is a capability
// family declared by a plugin; these are the families the harness ships a
// vocabulary for, not a closed enumeration — a plugin may declare any other
// family and the registry must accept it.
//
// TTS, STT and realtime are independent families: speech synthesis, speech
// transcription and a live audio channel each have their own configuration,
// credentials, model catalogs, parameter schemas and session shape, so none may
// be modelled as an llm or image subtype.
const (
	TypeLLM      = "llm"
	TypeRealtime = "realtime"
	TypeImage    = "image"
	TypeVideo    = "video"
	TypeDecision = "decision"
	TypeTTS      = "tts"
	TypeSTT      = "stt"
)

// BuiltinTypeIDs returns the first-batch family ids in a stable order. The
// result is a fresh slice: callers may sort or filter it freely.
func BuiltinTypeIDs() []string {
	return []string{TypeLLM, TypeRealtime, TypeImage, TypeVideo, TypeDecision, TypeTTS, TypeSTT}
}

// IsBuiltinTypeID reports whether id is one of the first-batch families. It is
// a grouping convenience for the settings UI; it is deliberately not a
// validity check, because plugin-declared families are legal.
func IsBuiltinTypeID(id string) bool {
	for _, builtin := range BuiltinTypeIDs() {
		if id == builtin {
			return true
		}
	}
	return false
}

// Capability flags the harness and the settings UI branch on. The set is open:
// a plugin may declare flags of its own (for example "image.generate" on an
// image family) and they are carried through untouched. A model's capabilities
// are the facts the runtime reads instead of guessing from the model name.
const (
	// CapabilityReasoning marks a model that accepts a thinking-strength
	// tuning; a model without it must have that parameter disabled in the UI.
	CapabilityReasoning = "reasoning"
	// CapabilityStreaming marks a model that can stream deltas.
	CapabilityStreaming = "streaming"
	// CapabilityToolCall marks a model that accepts tool definitions.
	CapabilityToolCall = "tool-call"
	// CapabilityJSONSchemaOutput marks a model that can be constrained to a
	// JSON Schema response.
	CapabilityJSONSchemaOutput = "json-schema-output"
	// CapabilityVAD marks a realtime channel that runs voice-activity detection
	// (on the provider or on the client), so a turn ends without a manual stop.
	CapabilityVAD = "vad"
	// CapabilityInterruption marks a realtime channel where a new user turn can
	// cancel the assistant's current output.
	CapabilityInterruption = "interruption"
	// CapabilityTranscriptDelta marks a realtime channel that streams partial
	// transcripts. Partials are UI state; they are never ledger events.
	CapabilityTranscriptDelta = "transcript-delta"
	// CapabilityTextFallback marks a realtime channel that stays usable as a
	// text session when the microphone is unavailable or permission is denied.
	CapabilityTextFallback = "text-fallback"
)

// Provider health states. They are the vocabulary of every surface that shows
// whether an instance can be used: the API, the CLI, the settings page and the
// runtime's typed errors.
const (
	// HealthUnchecked is the state of an instance whose health check has not
	// run yet (the zero value). It is not a failure.
	HealthUnchecked = "unchecked"
	// HealthReady means the last check reached the endpoint and authenticated.
	HealthReady = "ready"
	// HealthNeedsAuth means the credential reference did not resolve or the
	// endpoint rejected it; callers must not retry without credentials.
	HealthNeedsAuth = "needs_auth"
	// HealthUnreachable means the endpoint could not be reached.
	HealthUnreachable = "unreachable"
	// HealthInvalidConfig means the stored configuration cannot be used as is.
	HealthInvalidConfig = "invalid_config"
	// HealthDegraded means the instance answered but not fully (some models or
	// capabilities are unavailable); a failed check never deletes the
	// configuration, it only changes this state.
	HealthDegraded = "degraded"
)

// ProviderStatus is the enablement axis of a provider instance: whether the
// instance is offered to new bindings (active) or kept but not offered
// (disabled). It is deliberately separate from HealthStatus: enablement is a
// user decision, health is a measurement, and one JSON name cannot mean both.
// The zero value means ProviderStatusActive.
type ProviderStatus string

const (
	// ProviderStatusActive is the default: the instance may be bound.
	ProviderStatusActive ProviderStatus = "active"
	// ProviderStatusDisabled keeps the configuration but offers nothing.
	ProviderStatusDisabled ProviderStatus = "disabled"
)

// ProviderStatuses returns the closed enablement vocabulary in a stable order.
func ProviderStatuses() []ProviderStatus {
	return []ProviderStatus{ProviderStatusActive, ProviderStatusDisabled}
}

// ParseProviderStatus validates an enablement status string.
func ParseProviderStatus(raw string) (ProviderStatus, bool) {
	for _, status := range ProviderStatuses() {
		if string(status) == raw {
			return status, true
		}
	}
	return "", false
}

// HealthStatus is the last known health of one provider instance. It is the
// `health` axis of an instance; the `status` axis is ProviderStatus.
type HealthStatus struct {
	// State is one of the Health* constants. The zero value means
	// HealthUnchecked; Normalize makes that explicit.
	State string `json:"state,omitempty"`
	// Message is a human-readable reason shown next to the state. It must not
	// carry a credential value: health text ends up in logs and UI state.
	Message string `json:"message,omitempty"`
	// CheckedAt is when the check ran; zero means never.
	CheckedAt time.Time `json:"checkedAt,omitzero"`
}

// Capabilities is an open set of capability flags. It is a set, not a list:
// normalization trims entries and drops duplicates, and membership is what the
// runtime asks about.
type Capabilities []string

// Has reports whether flag is declared.
func (c Capabilities) Has(flag string) bool {
	for _, value := range c {
		if value == flag {
			return true
		}
	}
	return false
}

// SchemaRef names one JSON Schema. Exactly one form is used:
//
//   - Ref names a document the declaring plugin shipped in its SchemaSet. The
//     registry never fetches schemas, so resolution is a map lookup and
//     validation stays pure.
//   - Inline carries the document itself.
//
// Setting both is a contradiction and validation reports it; setting neither
// means "not declared", which is only legal where the slot is optional.
type SchemaRef struct {
	Ref    string          `json:"ref,omitempty" yaml:"ref,omitempty"`
	Inline json.RawMessage `json:"inline,omitempty" yaml:"inline,omitempty"`
}

// IsZero reports whether no schema is declared.
func (r SchemaRef) IsZero() bool {
	return r.Ref == "" && len(r.Inline) == 0
}

// UnmarshalYAML accepts both declared forms plus a bare JSON Schema document,
// which is how a plugin author naturally writes a schema in plugin.yaml:
//
//	config_schema: { ref: family.config }
//	config_schema: { inline: { type: object } }
//	config_schema: { type: object }        # the whole mapping is the document
//
// The bare form is converted to JSON and stored as Inline, so the domain keeps
// one representation (ref or inline document) however the manifest spelled it.
func (r *SchemaRef) UnmarshalYAML(value *yaml.Node) error {
	var fields map[string]yaml.Node
	if err := value.Decode(&fields); err != nil {
		return err
	}
	refNode, hasRef := fields["ref"]
	inlineNode, hasInline := fields["inline"]
	if hasRef || hasInline {
		for key := range fields {
			if key != "ref" && key != "inline" {
				return fmt.Errorf("schema slot carries either ref or inline, not %q", key)
			}
		}
		if hasRef {
			if err := refNode.Decode(&r.Ref); err != nil {
				return err
			}
		}
		if hasInline {
			var document any
			if err := inlineNode.Decode(&document); err != nil {
				return err
			}
			blob, err := json.Marshal(document)
			if err != nil {
				return fmt.Errorf("inline schema document is not JSON-encodable: %w", err)
			}
			r.Inline = blob
		}
		return nil
	}
	// No ref/inline wrapper: the whole mapping is the schema document.
	var document any
	if err := value.Decode(&document); err != nil {
		return err
	}
	blob, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("schema document is not JSON-encodable: %w", err)
	}
	r.Inline = blob
	return nil
}

// SchemaSet is the named JSON Schema documents a plugin ships with its provider
// type declarations. Plugin assembly decodes the plugin's extensibility
// package into this map; the domain never loads files or URLs.
type SchemaSet map[string]json.RawMessage

// ParameterSchema is one layer of the tunable-parameter contract: the schema
// that declares the parameters, plus the default values this layer
// contributes.
//
// Layers merge in the mandated order type default → provider instance → model
// (EffectiveParameters). Inside one merge the later layer wins: its Schema
// replaces an earlier one (an empty Schema inherits), and its Defaults are
// folded with JSON Merge Patch semantics — absent keys inherit, null clears,
// nested objects merge, arrays and scalars replace.
type ParameterSchema struct {
	// Schema declares which parameters exist and their value domains. Optional:
	// a layer may contribute defaults only, or nothing at all.
	Schema SchemaRef `json:"schema,omitempty" yaml:"schema,omitempty"`
	// Defaults are the parameter values this layer contributes.
	Defaults map[string]any `json:"defaults,omitempty" yaml:"defaults,omitempty"`
}

// AuthField is one field of a provider type's credential document.
type AuthField struct {
	// Name is the field name as the adapter expects it ("apiKey", "projectId").
	Name string `json:"name" yaml:"name"`
	// Label is the human-readable form shown in the settings dialog.
	Label string `json:"label,omitempty" yaml:"label,omitempty"`
	// Secret marks a value that must never be echoed back to a client, written
	// to a log or stored outside the tenant secret store. A credential-shaped
	// field name without Secret is a validation error.
	Secret bool `json:"secret,omitempty" yaml:"secret,omitempty"`
	// Required marks a field the instance cannot be built without.
	Required bool `json:"required,omitempty" yaml:"required,omitempty"`
}

// AuthSchema declares the credential fields of one provider type. A type that
// needs no credential declares an empty field list explicitly; a nil
// *AuthSchema means the plugin forgot to declare it and is rejected.
type AuthSchema struct {
	Fields []AuthField `json:"fields,omitempty" yaml:"fields,omitempty"`
}

// Session surface kinds. A surface is what a session of that provider family
// looks like: the composer a client mounts and the timeline it renders. The
// kind is a closed vocabulary — a client must be able to mount something, and
// SurfaceGeneric is what "no dedicated panel yet" means. Provider *families*
// stay open; a plugin-declared family maps to SurfaceGeneric unless its
// declaration says otherwise.
const (
	// SurfaceLLM is a text composer over a conversation history.
	SurfaceLLM = "llm"
	// SurfaceRealtime is a voice composer over a live transcript.
	SurfaceRealtime = "realtime"
	// SurfaceImage is a prompt/reference composer over a generated gallery.
	SurfaceImage = "image"
	// SurfaceVideo is the image surface's moving-picture sibling.
	SurfaceVideo = "video"
	// SurfaceSpeech is the TTS/STT workbench: one surface, two modes.
	SurfaceSpeech = "speech"
	// SurfaceGeneric is the fallback for a family with no dedicated panel.
	SurfaceGeneric = "generic"
)

// SurfaceKinds returns the closed surface-kind vocabulary in a stable order.
func SurfaceKinds() []string {
	return []string{SurfaceLLM, SurfaceRealtime, SurfaceImage, SurfaceVideo, SurfaceSpeech, SurfaceGeneric}
}

// IsSurfaceKind reports whether kind is one of SurfaceKinds.
func IsSurfaceKind(kind string) bool {
	for _, known := range SurfaceKinds() {
		if kind == known {
			return true
		}
	}
	return false
}

// DefaultSurfaceKind maps a provider family id to the session surface it opens
// when its declaration does not say. Built-in families map one to one, TTS and
// STT share the speech workbench, and anything else is generic: an unknown
// family must say "no dedicated panel" instead of pretending to be a chat
// model. TypeDecision deliberately floors to SurfaceGeneric: a decision family
// is a first-class configuration family, but the client surface registry
// (web/src/surfaces/registry.ts) has no decision panel yet, so it must not be
// forced onto the llm surface either.
func DefaultSurfaceKind(typeID string) string {
	switch typeID {
	case TypeLLM:
		return SurfaceLLM
	case TypeRealtime:
		return SurfaceRealtime
	case TypeImage:
		return SurfaceImage
	case TypeVideo:
		return SurfaceVideo
	case TypeTTS, TypeSTT:
		return SurfaceSpeech
	default:
		return SurfaceGeneric
	}
}

// Realtime transports a realtime surface may declare. The vocabulary is closed
// because the client has to know which channel implementation to open.
const (
	// TransportWebSocket is a message-oriented duplex channel (audio frames,
	// transcript deltas and tool calls travel as messages).
	TransportWebSocket = "websocket"
	// TransportWebRTC is a peer-connection channel (media negotiated at connect
	// time, control messages on its data channel).
	TransportWebRTC = "webrtc"
)

// RealtimeTransports returns the transport vocabulary in a stable order.
func RealtimeTransports() []string {
	return []string{TransportWebSocket, TransportWebRTC}
}

// IsRealtimeTransport reports whether transport is one of RealtimeTransports.
func IsRealtimeTransport(transport string) bool {
	for _, known := range RealtimeTransports() {
		if transport == known {
			return true
		}
	}
	return false
}

// Audio encodings a realtime channel may carry. Closed: an encoding the client
// cannot decode cannot be played, and "some codec" is not a contract.
const (
	// AudioEncodingPCM16 is signed 16-bit little-endian PCM.
	AudioEncodingPCM16 = "pcm16"
	// AudioEncodingPCM24 is signed 24-bit little-endian PCM.
	AudioEncodingPCM24 = "pcm24"
	// AudioEncodingPCM32 is signed 32-bit little-endian PCM.
	AudioEncodingPCM32 = "pcm32"
	// AudioEncodingOpus is Opus (framed, usually inside WebRTC).
	AudioEncodingOpus = "opus"
	// AudioEncodingG711ULaw is G.711 mu-law, the telephony default.
	AudioEncodingG711ULaw = "g711_ulaw"
	// AudioEncodingG711ALaw is G.711 A-law.
	AudioEncodingG711ALaw = "g711_alaw"
)

// AudioEncodings returns the realtime audio-encoding vocabulary in a stable
// order.
func AudioEncodings() []string {
	return []string{AudioEncodingPCM16, AudioEncodingPCM24, AudioEncodingPCM32, AudioEncodingOpus, AudioEncodingG711ULaw, AudioEncodingG711ALaw}
}

// IsAudioEncoding reports whether encoding is one of AudioEncodings.
func IsAudioEncoding(encoding string) bool {
	for _, known := range AudioEncodings() {
		if encoding == known {
			return true
		}
	}
	return false
}

// Reconnect modes a realtime surface may declare: how a dropped channel is
// re-established.
const (
	// ReconnectResume continues the session with a cursor: finalized
	// transcripts are not repeated and partials may be replaced.
	ReconnectResume = "resume"
	// ReconnectRestart opens a fresh channel for the same session.
	ReconnectRestart = "restart"
	// ReconnectNone reports the failure and leaves the retry to the user.
	ReconnectNone = "none"
)

// ReconnectModes returns the reconnect-mode vocabulary in a stable order.
func ReconnectModes() []string {
	return []string{ReconnectResume, ReconnectRestart, ReconnectNone}
}

// IsReconnectMode reports whether mode is one of ReconnectModes.
func IsReconnectMode(mode string) bool {
	for _, known := range ReconnectModes() {
		if mode == known {
			return true
		}
	}
	return false
}

// Realtime audio bounds. A declared format must be decodable, so the numbers
// are a small closed domain rather than "any integer".
const (
	minRealtimeSampleRateHz = 8000
	maxRealtimeSampleRateHz = 192000
	maxRealtimeChannels     = 2
)

// AudioFormat declares the wire format of a realtime audio stream: sample rate
// in Hz, channel count and encoding. An all-zero value means "not declared" —
// the provider negotiates the format at connect time. A partially declared
// format is a contradiction (a client cannot decode half a format) and is
// rejected by validation.
type AudioFormat struct {
	// SampleRateHz is the sample rate in Hz.
	SampleRateHz int `json:"sample_rate_hz,omitempty" yaml:"sample_rate_hz,omitempty"`
	// Channels is the channel count (1 mono, 2 stereo).
	Channels int `json:"channels,omitempty" yaml:"channels,omitempty"`
	// Encoding is one of AudioEncodings.
	Encoding string `json:"encoding,omitempty" yaml:"encoding,omitempty"`
}

// IsZero reports whether no audio format is declared.
func (f AudioFormat) IsZero() bool {
	return f.SampleRateHz == 0 && f.Channels == 0 && strings.TrimSpace(f.Encoding) == ""
}

// RealtimeReconnect declares how a dropped realtime channel is re-established.
// A zero value means "the provider's default": the client may still show a
// retry, but no session-resume promise is made.
type RealtimeReconnect struct {
	// Mode is one of ReconnectModes.
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty"`
	// WindowSeconds is how long a dropped session stays resumable; 0 means
	// undeclared.
	WindowSeconds int `json:"window_seconds,omitempty" yaml:"window_seconds,omitempty"`
}

// IsZero reports whether no reconnect policy is declared.
func (r RealtimeReconnect) IsZero() bool {
	return strings.TrimSpace(r.Mode) == "" && r.WindowSeconds == 0
}

// RealtimeSurface declares the live channel of a realtime surface. It is the
// part of a realtime declaration a client needs before it can connect, so it is
// carried by the provider type once rather than duplicated per model.
type RealtimeSurface struct {
	// Transport is the channel kind (TransportWebSocket or TransportWebRTC).
	// Required for a realtime surface.
	Transport string `json:"transport,omitempty" yaml:"transport,omitempty"`
	// Capabilities are the realtime flags the channel implements:
	// CapabilityVAD, CapabilityInterruption, CapabilityTranscriptDelta,
	// CapabilityToolCall, CapabilityTextFallback. The set stays open.
	Capabilities Capabilities `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	// Audio is the wire format; zero means negotiated at connect time.
	Audio AudioFormat `json:"audio,omitzero" yaml:"audio,omitempty"`
	// MaxConcurrentSessions bounds simultaneous channels of one instance; 0
	// means undeclared.
	MaxConcurrentSessions int `json:"max_concurrent_sessions,omitempty" yaml:"max_concurrent_sessions,omitempty"`
	// MaxSessionDurationSeconds bounds one channel's lifetime; 0 means
	// undeclared.
	MaxSessionDurationSeconds int `json:"max_session_duration_seconds,omitempty" yaml:"max_session_duration_seconds,omitempty"`
	// Reconnect is the resume policy after a drop.
	Reconnect RealtimeReconnect `json:"reconnect,omitzero" yaml:"reconnect,omitempty"`
}

// SurfaceDescriptor declares the session surface a provider family opens. It is
// optional on a declaration: an omitted surfaceKind is derived from the family
// id, so a provider that needs nothing special stays untouched.
type SurfaceDescriptor struct {
	// SurfaceKind is one of SurfaceKinds; empty means "derive from the family".
	SurfaceKind string `json:"surface_kind,omitempty" yaml:"surface_kind,omitempty"`
	// Realtime carries the channel declaration of a realtime surface. It is
	// required when the effective surface kind is realtime and forbidden
	// otherwise.
	Realtime *RealtimeSurface `json:"realtime,omitempty" yaml:"realtime,omitempty"`
}

// IsZero reports whether no surface is declared.
func (s SurfaceDescriptor) IsZero() bool {
	return strings.TrimSpace(s.SurfaceKind) == "" && s.Realtime == nil
}

// normalized returns a trimmed, deep-copied declaration: set-like capability
// flags are normalized and the realtime block is never aliased.
func (s SurfaceDescriptor) normalized() SurfaceDescriptor {
	normalized := SurfaceDescriptor{SurfaceKind: strings.TrimSpace(s.SurfaceKind)}
	if s.Realtime == nil {
		return normalized
	}
	realtime := *s.Realtime
	realtime.Transport = strings.TrimSpace(realtime.Transport)
	realtime.Capabilities = normalizeFlags(realtime.Capabilities)
	realtime.Audio.Encoding = strings.TrimSpace(realtime.Audio.Encoding)
	realtime.Reconnect.Mode = strings.TrimSpace(realtime.Reconnect.Mode)
	normalized.Realtime = &realtime
	return normalized
}

// ProviderType is one capability family declared by a plugin. It is the
// declaration the settings UI renders and the Harness validates instances
// against; it never contains a credential value.
// The wire names of a declaration are the plugin.yaml names of
// specs/provider-registry (snake_case): the same struct decodes a manifest
// declaration and encodes an extensibility document, so a manifest written to
// the spec cannot be rejected by a camelCase tag.
type ProviderType struct {
	// TypeID is the stable family id ("llm", "image", "embedding", ...).
	TypeID string `json:"type_id" yaml:"type_id"`
	// DisplayName is the group title in the settings UI.
	DisplayName string `json:"display_name" yaml:"display_name"`
	// Icon is the icon token for that group.
	Icon string `json:"icon,omitempty" yaml:"icon,omitempty"`
	// PluginID is the declaring plugin. It is part of the declaration because
	// a type without a source could not be installed, upgraded or removed. A
	// manifest declaration may omit it: the declaring plugin is the manifest id.
	PluginID string `json:"plugin_id,omitempty" yaml:"plugin_id,omitempty"`
	// Capabilities are the family-level capability flags (open vocabulary).
	Capabilities Capabilities `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	// ConfigSchema declares the instance configuration fields.
	ConfigSchema SchemaRef `json:"config_schema" yaml:"config_schema"`
	// AuthSchema declares the credential fields; secret fields must be marked.
	AuthSchema *AuthSchema `json:"auth_schema" yaml:"auth_schema"`
	// ModelSchema declares the shape of one model catalog entry.
	ModelSchema SchemaRef `json:"model_schema" yaml:"model_schema"`
	// Parameters are the family default parameters (may be empty).
	Parameters ParameterSchema `json:"parameters,omitempty" yaml:"parameters,omitempty"`
	// FeaturePorts are the feature ports an instance of this type may bind to.
	FeaturePorts []string `json:"feature_ports,omitempty" yaml:"feature_ports,omitempty"`
	// Surface declares the session surface this family opens. It is optional:
	// an omitted surfaceKind is derived from TypeID (DefaultSurfaceKind), so a
	// family that needs nothing special stays untouched. A realtime family must
	// declare the realtime block (transport and channel facts).
	Surface SurfaceDescriptor `json:"surface,omitzero" yaml:"surface,omitempty"`
}

// EffectiveSurface resolves the session surface of the family: an omitted
// surfaceKind is derived from the type id, and the declaration is normalized.
// Every consumer resolves the surface through this method, so the derivation
// lives in one place instead of being re-implemented by each caller.
func (t ProviderType) EffectiveSurface() SurfaceDescriptor {
	surface := t.Surface.normalized()
	if surface.SurfaceKind == "" {
		surface.SurfaceKind = DefaultSurfaceKind(strings.TrimSpace(t.TypeID))
	}
	return surface
}

// ModelDescriptor is one entry of a provider's model catalog. It describes
// what a specific model can do; it never carries prices or plan data, because
// usage is metered but pricing belongs to the contract side.
//
// The session surface is a property of the family, not of one catalog entry: a
// model inherits the surface of its provider type (ProviderType.EffectiveSurface),
// so the channel facts a client needs before connecting are declared once and
// cannot drift per model.
type ModelDescriptor struct {
	// ModelID is the identity inside the provider. It is never guessed from a
	// display name and never routed without its provider.
	ModelID string `json:"modelId"`
	// DisplayName defaults to ModelID.
	DisplayName string `json:"displayName,omitempty"`
	// ContextWindow is the input budget in tokens (0 means undeclared).
	ContextWindow int `json:"contextWindow,omitempty"`
	// MaxOutput is the output budget in tokens (0 means undeclared).
	MaxOutput int `json:"maxOutput,omitempty"`
	// Capabilities are this model's capability flags.
	Capabilities Capabilities `json:"capabilities,omitempty"`
	// Parameters override the provider/type parameter layers for this model.
	Parameters ParameterSchema `json:"parameters,omitempty"`
}

// ProviderInstance is one tenant's configured provider: a credential
// reference, an endpoint, configuration, a model catalog and a health state.
//
// The credential itself is never here. CredentialRef names where it lives
// (${NAME}, env:NAME or secret:NAME); the instance, its API responses, its
// events, its logs and its UI state carry the reference only.
type ProviderInstance struct {
	// ID is the stable instance identity inside the tenant.
	ID string `json:"id"`
	// TenantID scopes the instance: a tenant only ever sees its own.
	TenantID string `json:"tenantId"`
	// TypeID names the declaring family.
	TypeID string `json:"typeId"`
	// PluginID records the plugin source. Empty means "inherit the declaring
	// plugin of TypeID" (WithTypeDefaults resolves it before persisting).
	PluginID string `json:"pluginId,omitempty"`
	// Name is the display name the user gave the instance.
	Name string `json:"name"`
	// CredentialRef is a reference, never a credential value.
	CredentialRef string `json:"credentialRef,omitempty"`
	// Endpoint is the absolute base URL (http, https, ws or wss).
	Endpoint string `json:"endpoint,omitempty"`
	// Config is provider-specific instance configuration. It must be JSON data
	// and must not contain credentials or a second credential reference.
	Config map[string]any `json:"config,omitempty"`
	// Parameters are the provider-layer parameter override.
	Parameters ParameterSchema `json:"parameters,omitempty"`
	// Models is the model catalog (may be empty while discovery is pending).
	Models []ModelDescriptor `json:"models,omitempty"`
	// Status is the enablement axis (active|disabled); empty means active.
	// It is the `status` of the API and the CLI (`--status active|disabled`).
	Status ProviderStatus `json:"status,omitempty"`
	// Health is the last health check result. It is the `health` axis: a
	// measurement, never a user-set enablement flag.
	Health HealthStatus `json:"health,omitzero"`
}

// TypeSet is the provider-type declarations currently installed, keyed by type
// id. Plugin assembly builds it; the HTTP API, the CLI and validation read it.
type TypeSet map[string]ProviderType

// WithTypeDefaults returns a copy with the fields an instance inherits from
// its type declaration filled in. Only the plugin source is inherited today: an
// explicit PluginID is never overwritten, so a record that disagrees with its
// type stays visible to validation instead of being silently repaired.
//
// Callers that persist an instance record the resolved value, so a later type
// re-declaration cannot silently re-point an existing instance.
func (i ProviderInstance) WithTypeDefaults(types TypeSet) ProviderInstance {
	if i.PluginID != "" {
		return i
	}
	declaration, ok := types[i.TypeID]
	if !ok || declaration.PluginID == "" {
		return i
	}
	i.PluginID = declaration.PluginID
	return i
}

// ConfigHash digests everything that decides what an instance answers with:
// type, declaring plugin, endpoint, credential reference, configuration and the
// provider-layer parameter override.
//
// A catalog discovered at runtime is cached under this hash, so any
// configuration change invalidates the cache instead of serving models from a
// previous endpoint. The credential reference — not the credential — is part of
// the digest: rotating the secret behind the same reference is invisible here
// by design, because the reference is an indirection; the store invalidates on
// secret writes.
func (i ProviderInstance) ConfigHash() (string, error) {
	payload := struct {
		TypeID        string          `json:"typeId"`
		PluginID      string          `json:"pluginId,omitempty"`
		Endpoint      string          `json:"endpoint,omitempty"`
		CredentialRef string          `json:"credentialRef,omitempty"`
		Config        map[string]any  `json:"config,omitempty"`
		Parameters    ParameterSchema `json:"parameters,omitempty"`
	}{
		TypeID:        i.TypeID,
		PluginID:      i.PluginID,
		Endpoint:      i.Endpoint,
		CredentialRef: i.CredentialRef,
		Config:        i.Config,
		Parameters:    i.Parameters,
	}
	blob, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("modelprovider: provider configuration is not JSON data: %w", err)
	}
	sum := sha256.Sum256(blob)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// EffectiveParameters resolves the tunable parameters of one model by folding
// the three layers in their mandated order: the type's default first, the
// provider instance's override second, the model's override last. A later layer
// wins per key; keys it does not mention are inherited.
func EffectiveParameters(declaration ProviderType, instance ProviderInstance, model ModelDescriptor) ParameterSchema {
	return MergeParameterSchemas(
		MergeParameterSchemas(declaration.Parameters, instance.Parameters),
		model.Parameters,
	)
}

// MergeParameterSchemas folds override over base. The result is a new value:
// neither input is mutated. The schema reference follows "later wins, empty
// inherits"; defaults follow JSON Merge Patch semantics (absent keys inherit,
// null clears, nested objects merge, arrays and scalars replace).
func MergeParameterSchemas(base, override ParameterSchema) ParameterSchema {
	merged := ParameterSchema{Schema: cloneSchemaRef(base.Schema)}
	if !override.Schema.IsZero() {
		merged.Schema = cloneSchemaRef(override.Schema)
	}
	merged.Defaults = mergeParameterDefaults(base.Defaults, override.Defaults)
	return merged
}

// mergeParameterDefaults applies override to base with JSON Merge Patch
// semantics and returns a fresh map. An empty result is nil so the encoding
// cannot tell "no defaults" from an empty object.
func mergeParameterDefaults(base, override map[string]any) map[string]any {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	merged := cloneJSONMap(base)
	if merged == nil {
		merged = make(map[string]any, len(override))
	}
	for key, value := range override {
		if value == nil {
			delete(merged, key)
			continue
		}
		merged[key] = mergeJSONValue(merged[key], value)
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// mergeJSONValue merges one override value over one base value.
func mergeJSONValue(base, override any) any {
	overrideObject, ok := override.(map[string]any)
	if !ok {
		// Scalars, arrays and null replace wholesale.
		return cloneJSONValue(override)
	}
	baseObject, _ := base.(map[string]any)
	return mergeParameterDefaults(baseObject, overrideObject)
}

// cloneSchemaRef returns a copy whose inline bytes are not shared with the
// input.
func cloneSchemaRef(ref SchemaRef) SchemaRef {
	if len(ref.Inline) == 0 {
		return ref
	}
	return SchemaRef{Ref: ref.Ref, Inline: append(json.RawMessage(nil), ref.Inline...)}
}

// cloneParameterSchema returns a deep copy of one parameter layer.
func cloneParameterSchema(schema ParameterSchema) ParameterSchema {
	return ParameterSchema{
		Schema:   cloneSchemaRef(schema.Schema),
		Defaults: cloneJSONMap(schema.Defaults),
	}
}

// cloneJSONValue deep-copies the JSON shapes encoding/json produces. Scalars are
// immutable and returned as-is.
func cloneJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneJSONMap(typed)
	case []any:
		cloned := make([]any, len(typed))
		for index := range typed {
			cloned[index] = cloneJSONValue(typed[index])
		}
		return cloned
	default:
		return value
	}
}

// cloneJSONMap deep-copies a decoded JSON object.
func cloneJSONMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = cloneJSONValue(value)
	}
	return out
}
