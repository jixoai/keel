package modelprovider

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// requireNoError fails the test when err is not nil.
func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// requireCode fails the test unless err is a validation failure carrying code
// at field.
func requireCode(t *testing.T, err error, field, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s at %s, got nil", code, field)
	}
	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	for _, failure := range validation.Errors {
		if failure.Field == field && failure.Code == code {
			return
		}
	}
	t.Fatalf("expected %s at %s, got: %v", code, field, err)
}

// testSchemas is the schema set a plugin ships: the family defaults plus one
// model-level parameter document.
func testSchemas() SchemaSet {
	return SchemaSet{
		"family.config":        json.RawMessage(`{"type":"object","additionalProperties":true}`),
		"family.model":         json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		"family.parameters":    json.RawMessage(`{"type":"object","properties":{"reasoning":{"type":"string"},"maxOutputTokens":{"type":"integer"}}}`),
		"llm.model.parameters": json.RawMessage(`{"type":"object","properties":{"reasoning":{"type":"string"}}}`),
	}
}

// testProviderType is a complete llm declaration.
func testProviderType() ProviderType {
	return ProviderType{
		TypeID:       TypeLLM,
		DisplayName:  "Language Model",
		Icon:         "brain",
		PluginID:     "plugin-llm",
		Capabilities: Capabilities{CapabilityStreaming, CapabilityToolCall, CapabilityReasoning},
		ConfigSchema: SchemaRef{Ref: "family.config"},
		AuthSchema: &AuthSchema{Fields: []AuthField{
			{Name: "apiKey", Label: "API key", Secret: true, Required: true},
		}},
		ModelSchema:  SchemaRef{Ref: "family.model"},
		Parameters:   ParameterSchema{Schema: SchemaRef{Ref: "family.parameters"}, Defaults: map[string]any{"reasoning": "medium"}},
		FeaturePorts: []string{"llm.chat"},
	}
}

// testTypes is the installed type set the valid instance validates against.
func testTypes() TypeSet {
	return TypeSet{TypeLLM: testProviderType()}
}

// testInstance is a complete, valid llm provider instance.
func testInstance() ProviderInstance {
	return ProviderInstance{
		ID:            "prov_openai",
		TenantID:      "tenant_acme",
		TypeID:        TypeLLM,
		PluginID:      "plugin-llm",
		Name:          "OpenAI",
		CredentialRef: "secret:openai_key",
		Endpoint:      "https://api.openai.com/v1",
		Config:        map[string]any{"organization": "org-123"},
		Parameters:    ParameterSchema{Defaults: map[string]any{"reasoning": "high"}},
		Models: []ModelDescriptor{
			{
				ModelID:       "gpt-4o",
				ContextWindow: 128000,
				MaxOutput:     16384,
				Capabilities:  Capabilities{CapabilityStreaming, CapabilityToolCall},
			},
		},
		Status: ProviderStatusActive,
		Health: HealthStatus{State: HealthReady},
	}
}

// testRealtimeSurface is the channel declaration a realtime family must carry:
// a transport, the realtime capability flags, a decodable audio format, session
// bounds and a resume policy.
func testRealtimeSurface() SurfaceDescriptor {
	return SurfaceDescriptor{
		Realtime: &RealtimeSurface{
			Transport: TransportWebSocket,
			Capabilities: Capabilities{
				CapabilityVAD,
				CapabilityInterruption,
				CapabilityTranscriptDelta,
				CapabilityToolCall,
				CapabilityTextFallback,
			},
			Audio:                     AudioFormat{SampleRateHz: 24000, Channels: 1, Encoding: AudioEncodingPCM16},
			MaxConcurrentSessions:     4,
			MaxSessionDurationSeconds: 3600,
			Reconnect:                 RealtimeReconnect{Mode: ReconnectResume, WindowSeconds: 30},
		},
	}
}

// surfaceForFamily is the minimal surface a built-in family declaration needs:
// realtime must declare its channel, every other family relies on the kind
// derived from its id and leaves the surface block empty.
func surfaceForFamily(typeID string) SurfaceDescriptor {
	if typeID == TypeRealtime {
		return testRealtimeSurface()
	}
	return SurfaceDescriptor{}
}

// TestBuiltinTypeIDsCoverFirstBatchFamilies pins the families the harness ships
// a vocabulary for and proves they are distinct ids, with TTS, STT and realtime
// as their own families rather than aliases of llm or image.
func TestBuiltinTypeIDsCoverFirstBatchFamilies(t *testing.T) {
	want := []string{TypeLLM, TypeRealtime, TypeImage, TypeVideo, TypeDecision, TypeTTS, TypeSTT}
	got := BuiltinTypeIDs()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuiltinTypeIDs() = %v, want %v", got, want)
	}
	seen := make(map[string]bool, len(got))
	for _, id := range got {
		if seen[id] {
			t.Errorf("builtin type id %q is listed twice", id)
		}
		seen[id] = true
		if !IsBuiltinTypeID(id) {
			t.Errorf("IsBuiltinTypeID(%q) = false, want true", id)
		}
	}
	if TypeTTS == TypeLLM || TypeTTS == TypeImage || TypeSTT == TypeLLM || TypeSTT == TypeImage {
		t.Errorf("tts/stt must be independent families, got ids tts=%q stt=%q", TypeTTS, TypeSTT)
	}
	if TypeRealtime == TypeLLM || TypeRealtime == TypeImage || TypeRealtime == TypeTTS || TypeRealtime == TypeSTT {
		t.Errorf("realtime must be an independent family, got id %q", TypeRealtime)
	}
	if IsBuiltinTypeID("embedding") {
		t.Error("a plugin-declared family must not be reported as built in")
	}
}

// TestEveryBuiltinFamilyDeclaresAndHostsInstances proves each shipped family can
// be declared by a plugin and can host a provider instance. The family is the
// only thing that changes between the subtests: declaration and instance rules
// are family-agnostic, which is what keeps the type vocabulary open.
func TestEveryBuiltinFamilyDeclaresAndHostsInstances(t *testing.T) {
	schemas := testSchemas()
	for _, typeID := range BuiltinTypeIDs() {
		typeID := typeID
		t.Run(typeID, func(t *testing.T) {
			declaration := testProviderType()
			declaration.TypeID = typeID
			declaration.DisplayName = "Family " + typeID
			declaration.FeaturePorts = []string{typeID + ".call"}
			declaration.Surface = surfaceForFamily(typeID)
			requireNoError(t, declaration.Validate(schemas))

			instance := testInstance()
			instance.TypeID = typeID
			instance.PluginID = ""      // inherit the declaring plugin
			instance.CredentialRef = "" // a keyless family is legal
			requireNoError(t, instance.Validate(TypeSet{typeID: declaration}, schemas))
		})
	}
}

// TestValidateProviderTypeAcceptsPluginDeclaredFamily proves the type
// vocabulary is open: a family outside the first batch is valid as long as its
// schemas are complete.
func TestValidateProviderTypeAcceptsPluginDeclaredFamily(t *testing.T) {
	declaration := testProviderType()
	declaration.TypeID = "embedding"
	declaration.DisplayName = "Embedding"
	declaration.FeaturePorts = []string{"embedding.create"}
	requireNoError(t, declaration.Validate(testSchemas()))
}

// TestRealtimeIsAnIndependentProviderFamily proves realtime is its own family,
// not a mode of llm or image: the family id is distinct, it is listed as
// built in, it derives the realtime surface, and neither llm nor image may
// claim that surface while realtime may open nothing else.
func TestRealtimeIsAnIndependentProviderFamily(t *testing.T) {
	if TypeRealtime != "realtime" {
		t.Fatalf("TypeRealtime = %q, want %q", TypeRealtime, "realtime")
	}
	for _, other := range []string{TypeLLM, TypeImage, TypeVideo, TypeDecision, TypeTTS, TypeSTT} {
		if TypeRealtime == other {
			t.Fatalf("realtime collides with family %q", other)
		}
	}
	if !IsBuiltinTypeID(TypeRealtime) {
		t.Fatalf("%q is not listed by BuiltinTypeIDs", TypeRealtime)
	}
	if got := DefaultSurfaceKind(TypeRealtime); got != SurfaceRealtime {
		t.Fatalf("DefaultSurfaceKind(%q) = %q, want %q", TypeRealtime, got, SurfaceRealtime)
	}

	schemas := testSchemas()
	declaration := testProviderType()
	declaration.TypeID = TypeRealtime
	declaration.DisplayName = "Realtime Voice"
	declaration.FeaturePorts = []string{"realtime.session"}
	declaration.Surface = testRealtimeSurface()
	requireNoError(t, declaration.Validate(schemas))

	for _, family := range []string{TypeLLM, TypeImage} {
		family := family
		t.Run("family="+family, func(t *testing.T) {
			// A realtime channel block on a non-realtime built-in family is the
			// "realtime as a button on the chat surface" shape the spec forbids.
			blockOnly := testProviderType()
			blockOnly.TypeID = family
			blockOnly.Surface = testRealtimeSurface()
			requireCode(t, blockOnly.Validate(schemas), "surface.realtime", CodeSurfaceMismatch)

			declared := testProviderType()
			declared.TypeID = family
			declared.Surface.SurfaceKind = SurfaceRealtime
			requireCode(t, declared.Validate(schemas), "surface.surfaceKind", CodeSurfaceMismatch)
		})
	}
}

// TestSurfaceKindDefaultsFromProviderFamily proves an omitted surfaceKind is
// derived from the family id for every shipped family and for a plugin-declared
// one, and that a plugin-declared family may choose its surface explicitly.
func TestSurfaceKindDefaultsFromProviderFamily(t *testing.T) {
	cases := []struct {
		typeID  string
		surface string
	}{
		{TypeLLM, SurfaceLLM},
		{TypeRealtime, SurfaceRealtime},
		{TypeImage, SurfaceImage},
		{TypeVideo, SurfaceVideo},
		{TypeTTS, SurfaceSpeech},
		{TypeSTT, SurfaceSpeech},
		{TypeDecision, SurfaceGeneric},
		{"embedding", SurfaceGeneric},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.typeID, func(t *testing.T) {
			if got := DefaultSurfaceKind(testCase.typeID); got != testCase.surface {
				t.Fatalf("DefaultSurfaceKind(%q) = %q, want %q", testCase.typeID, got, testCase.surface)
			}
			declaration := testProviderType()
			declaration.TypeID = testCase.typeID
			declaration.Surface = surfaceForFamily(testCase.typeID)
			requireNoError(t, declaration.Validate(testSchemas()))
			if got := declaration.EffectiveSurface().SurfaceKind; got != testCase.surface {
				t.Fatalf("EffectiveSurface().SurfaceKind = %q, want %q", got, testCase.surface)
			}
		})
	}

	// A plugin-declared family is free to name a surface explicitly; the
	// derivation is a default, not a constraint on open families.
	explicit := testProviderType()
	explicit.TypeID = "embedding"
	explicit.Surface = SurfaceDescriptor{SurfaceKind: SurfaceLLM}
	requireNoError(t, explicit.Validate(testSchemas()))
	if got := explicit.EffectiveSurface().SurfaceKind; got != SurfaceLLM {
		t.Fatalf("explicit surface kind = %q, want %q", got, SurfaceLLM)
	}

	// Naming the derived kind explicitly is legal for a shipped family too.
	image := testProviderType()
	image.TypeID = TypeImage
	image.Surface = SurfaceDescriptor{SurfaceKind: SurfaceImage}
	requireNoError(t, image.Validate(testSchemas()))
}

// TestValidateProviderTypeAcceptsRealtimeSurface is the positive case: a
// realtime declaration carrying the transport, the realtime capability flags,
// a decodable audio format, session bounds and a resume policy is valid, and
// the effective surface keeps every fact a client needs to connect.
func TestValidateProviderTypeAcceptsRealtimeSurface(t *testing.T) {
	declaration := testProviderType()
	declaration.TypeID = TypeRealtime
	declaration.DisplayName = "Realtime Voice"
	declaration.FeaturePorts = []string{"realtime.session"}
	declaration.Surface = testRealtimeSurface()
	requireNoError(t, declaration.Validate(testSchemas()))

	surface := declaration.EffectiveSurface()
	if surface.SurfaceKind != SurfaceRealtime {
		t.Fatalf("effective surface kind = %q, want %q", surface.SurfaceKind, SurfaceRealtime)
	}
	if surface.Realtime == nil {
		t.Fatal("effective surface lost the realtime block")
	}
	if surface.Realtime.Transport != TransportWebSocket {
		t.Errorf("transport = %q, want %q", surface.Realtime.Transport, TransportWebSocket)
	}
	for _, capability := range []string{
		CapabilityVAD,
		CapabilityInterruption,
		CapabilityTranscriptDelta,
		CapabilityToolCall,
		CapabilityTextFallback,
	} {
		if !surface.Realtime.Capabilities.Has(capability) {
			t.Errorf("effective surface lost capability %q", capability)
		}
	}
	if got, want := surface.Realtime.Audio, (AudioFormat{SampleRateHz: 24000, Channels: 1, Encoding: AudioEncodingPCM16}); got != want {
		t.Errorf("audio format = %+v, want %+v", got, want)
	}
	if surface.Realtime.MaxConcurrentSessions != 4 {
		t.Errorf("max concurrent sessions = %d, want 4", surface.Realtime.MaxConcurrentSessions)
	}
	if surface.Realtime.MaxSessionDurationSeconds != 3600 {
		t.Errorf("max session duration = %d, want 3600", surface.Realtime.MaxSessionDurationSeconds)
	}
	if got, want := surface.Realtime.Reconnect, (RealtimeReconnect{Mode: ReconnectResume, WindowSeconds: 30}); got != want {
		t.Errorf("reconnect = %+v, want %+v", got, want)
	}

	// The smallest legal realtime declaration: a transport is the only required
	// channel fact, and every transport in the vocabulary must be accepted.
	for _, transport := range RealtimeTransports() {
		transport := transport
		t.Run("transport="+transport, func(t *testing.T) {
			minimal := testProviderType()
			minimal.TypeID = TypeRealtime
			minimal.Surface = SurfaceDescriptor{Realtime: &RealtimeSurface{Transport: transport}}
			requireNoError(t, minimal.Validate(testSchemas()))
			if got := minimal.EffectiveSurface().SurfaceKind; got != SurfaceRealtime {
				t.Fatalf("effective surface kind = %q, want %q", got, SurfaceRealtime)
			}
		})
	}
}

// TestValidateProviderTypeRejectsBadSurface covers each way a surface
// declaration can be unusable: an unknown kind, a contradiction between the
// family and the surface, a realtime surface with no (or an unknown) transport,
// an audio format a client cannot decode, out-of-range session bounds and an
// unknown reconnect mode. Every failure names the field.
func TestValidateProviderTypeRejectsBadSurface(t *testing.T) {
	realtimeType := func() ProviderType {
		declaration := testProviderType()
		declaration.TypeID = TypeRealtime
		declaration.DisplayName = "Realtime Voice"
		declaration.Surface = testRealtimeSurface()
		return declaration
	}
	cases := []struct {
		name   string
		mutate func(*ProviderType)
		field  string
		code   string
	}{
		{
			name:   "unknown surface kind",
			mutate: func(d *ProviderType) { d.Surface = SurfaceDescriptor{SurfaceKind: "voice"} },
			field:  "surface.surfaceKind",
			code:   CodeInvalidSurfaceKind,
		},
		{
			name:   "realtime family opens llm surface",
			mutate: func(d *ProviderType) { d.Surface.SurfaceKind = SurfaceLLM },
			field:  "surface.surfaceKind",
			code:   CodeSurfaceMismatch,
		},
		{
			name:   "realtime family without a realtime block",
			mutate: func(d *ProviderType) { d.Surface = SurfaceDescriptor{} },
			field:  "surface.realtime.transport",
			code:   CodeMissingSurfaceTransport,
		},
		{
			name:   "realtime kind without a realtime block",
			mutate: func(d *ProviderType) { d.Surface = SurfaceDescriptor{SurfaceKind: SurfaceRealtime} },
			field:  "surface.realtime.transport",
			code:   CodeMissingSurfaceTransport,
		},
		{
			name:   "missing transport",
			mutate: func(d *ProviderType) { d.Surface.Realtime.Transport = "" },
			field:  "surface.realtime.transport",
			code:   CodeMissingSurfaceTransport,
		},
		{
			name:   "unknown transport",
			mutate: func(d *ProviderType) { d.Surface.Realtime.Transport = "http" },
			field:  "surface.realtime.transport",
			code:   CodeInvalidSurfaceTransport,
		},
		{
			name:   "partial audio format",
			mutate: func(d *ProviderType) { d.Surface.Realtime.Audio = AudioFormat{SampleRateHz: 24000} },
			field:  "surface.realtime.audio.channels",
			code:   CodeInvalidAudioFormat,
		},
		{
			name:   "sample rate out of range",
			mutate: func(d *ProviderType) { d.Surface.Realtime.Audio.SampleRateHz = 4000 },
			field:  "surface.realtime.audio.sampleRateHz",
			code:   CodeInvalidAudioFormat,
		},
		{
			name:   "channel count out of range",
			mutate: func(d *ProviderType) { d.Surface.Realtime.Audio.Channels = 3 },
			field:  "surface.realtime.audio.channels",
			code:   CodeInvalidAudioFormat,
		},
		{
			name:   "missing audio encoding",
			mutate: func(d *ProviderType) { d.Surface.Realtime.Audio.Encoding = "" },
			field:  "surface.realtime.audio.encoding",
			code:   CodeInvalidAudioFormat,
		},
		{
			name:   "unknown audio encoding",
			mutate: func(d *ProviderType) { d.Surface.Realtime.Audio.Encoding = "mp3" },
			field:  "surface.realtime.audio.encoding",
			code:   CodeInvalidAudioFormat,
		},
		{
			name:   "negative concurrency",
			mutate: func(d *ProviderType) { d.Surface.Realtime.MaxConcurrentSessions = -1 },
			field:  "surface.realtime.maxConcurrentSessions",
			code:   CodeInvalidSurfaceLimit,
		},
		{
			name:   "negative max session duration",
			mutate: func(d *ProviderType) { d.Surface.Realtime.MaxSessionDurationSeconds = -1 },
			field:  "surface.realtime.maxSessionDurationSeconds",
			code:   CodeInvalidSurfaceLimit,
		},
		{
			name:   "unknown reconnect mode",
			mutate: func(d *ProviderType) { d.Surface.Realtime.Reconnect.Mode = "forever" },
			field:  "surface.realtime.reconnect.mode",
			code:   CodeInvalidReconnectMode,
		},
		{
			name:   "negative reconnect window",
			mutate: func(d *ProviderType) { d.Surface.Realtime.Reconnect.WindowSeconds = -1 },
			field:  "surface.realtime.reconnect.windowSeconds",
			code:   CodeInvalidSurfaceLimit,
		},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			declaration := realtimeType()
			testCase.mutate(&declaration)
			requireCode(t, declaration.Validate(testSchemas()), testCase.field, testCase.code)
		})
	}

	// The same failures must be reported by manifest-time validation, which has
	// no schema set: a broken surface cannot wait for assembly to be noticed.
	declaration := realtimeType()
	declaration.Surface.Realtime.Transport = "http"
	requireCode(t, declaration.ValidateDeclaration(), "surface.realtime.transport", CodeInvalidSurfaceTransport)
}

// TestExistingProviderTypesStayUnchanged proves adding the surface descriptor
// did not move the existing families: their ids keep their values, a
// declaration written before the field existed still validates, and its wire
// form carries no surface key.
func TestExistingProviderTypesStayUnchanged(t *testing.T) {
	if TypeLLM != "llm" || TypeImage != "image" || TypeVideo != "video" ||
		TypeDecision != "decision" || TypeTTS != "tts" || TypeSTT != "stt" {
		t.Fatalf(
			"existing family ids changed: llm=%q image=%q video=%q decision=%q tts=%q stt=%q",
			TypeLLM, TypeImage, TypeVideo, TypeDecision, TypeTTS, TypeSTT,
		)
	}
	for _, typeID := range []string{TypeLLM, TypeImage, TypeVideo, TypeDecision, TypeTTS, TypeSTT} {
		declaration := testProviderType()
		declaration.TypeID = typeID
		requireNoError(t, declaration.Validate(testSchemas()))
	}

	blob, err := json.Marshal(testProviderType())
	requireNoError(t, err)
	if strings.Contains(string(blob), "surface") {
		t.Fatalf("a surface-less declaration must not grow a wire key: %s", blob)
	}

	// A non-realtime family's declaration has no surface requirement; the
	// derived kind is the only thing a client needs.
	if got := testProviderType().EffectiveSurface(); got.SurfaceKind != SurfaceLLM || got.Realtime != nil {
		t.Fatalf("effective surface of an llm declaration = %+v", got)
	}
}

// TestProviderTypeSurfaceDecodesFromManifestYAML proves the declaration
// vocabulary a plugin writes in plugin.yaml (snake_case keys, nested blocks) is
// the one the domain decodes, and that the decoded declaration validates.
func TestProviderTypeSurfaceDecodesFromManifestYAML(t *testing.T) {
	const document = `
type_id: realtime
display_name: Realtime Voice
plugin_id: plugin-realtime
capabilities: [streaming, tool-call]
config_schema: {ref: family.config}
auth_schema:
  fields:
    - {name: apiKey, secret: true, required: true}
model_schema: {ref: family.model}
feature_ports: [realtime.session]
surface:
  surface_kind: realtime
  realtime:
    transport: webrtc
    capabilities: [vad, interruption, transcript-delta, tool-call, text-fallback]
    audio: {sample_rate_hz: 48000, channels: 2, encoding: opus}
    max_concurrent_sessions: 2
    max_session_duration_seconds: 900
    reconnect: {mode: resume, window_seconds: 60}
`
	var declaration ProviderType
	requireNoError(t, yaml.Unmarshal([]byte(document), &declaration))
	requireNoError(t, declaration.Validate(testSchemas()))

	surface := declaration.EffectiveSurface()
	if surface.SurfaceKind != SurfaceRealtime || surface.Realtime == nil {
		t.Fatalf("effective surface = %+v", surface)
	}
	if surface.Realtime.Transport != TransportWebRTC {
		t.Errorf("transport = %q, want %q", surface.Realtime.Transport, TransportWebRTC)
	}
	if got, want := surface.Realtime.Audio, (AudioFormat{SampleRateHz: 48000, Channels: 2, Encoding: AudioEncodingOpus}); got != want {
		t.Errorf("audio format = %+v, want %+v", got, want)
	}
	if surface.Realtime.Reconnect.Mode != ReconnectResume || surface.Realtime.Reconnect.WindowSeconds != 60 {
		t.Errorf("reconnect = %+v", surface.Realtime.Reconnect)
	}
	for _, capability := range []string{CapabilityVAD, CapabilityInterruption, CapabilityTranscriptDelta, CapabilityTextFallback} {
		if !surface.Realtime.Capabilities.Has(capability) {
			t.Errorf("decoded surface lost capability %q", capability)
		}
	}
}

// TestValidateProviderTypeAcceptsCompleteDeclaration is the baseline: the
// fixture passes, so every rejection test below isolates one rule.
func TestValidateProviderTypeAcceptsCompleteDeclaration(t *testing.T) {
	requireNoError(t, testProviderType().Validate(testSchemas()))
}

// TestValidateProviderTypeRejectsMissingSchemas covers the assembly failure the
// spec requires: a type without config, auth or model schema cannot be used.
func TestValidateProviderTypeRejectsMissingSchemas(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ProviderType)
		field  string
	}{
		{name: "config schema", mutate: func(d *ProviderType) { d.ConfigSchema = SchemaRef{} }, field: "configSchema"},
		{name: "auth schema", mutate: func(d *ProviderType) { d.AuthSchema = nil }, field: "authSchema"},
		{name: "model schema", mutate: func(d *ProviderType) { d.ModelSchema = SchemaRef{} }, field: "modelSchema"},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			declaration := testProviderType()
			testCase.mutate(&declaration)
			requireCode(t, declaration.Validate(testSchemas()), testCase.field, CodeMissingSchema)
		})
	}
}

// TestValidateProviderTypeRejectsUnresolvableSchemaRef proves a schema name is
// verified against the plugin's schema set instead of being trusted.
func TestValidateProviderTypeRejectsUnresolvableSchemaRef(t *testing.T) {
	declaration := testProviderType()
	declaration.ModelSchema = SchemaRef{Ref: "family.unknown"}
	requireCode(t, declaration.Validate(testSchemas()), "modelSchema.ref", CodeUnknownSchema)
}

// TestValidateProviderTypeRejectsMalformedSchemaRefs covers the two ways a
// schema slot can be unusable: a contradictory ref+inline pair and an inline
// document that does not compile.
func TestValidateProviderTypeRejectsMalformedSchemaRefs(t *testing.T) {
	t.Run("conflicting forms", func(t *testing.T) {
		declaration := testProviderType()
		declaration.ConfigSchema = SchemaRef{Ref: "family.config", Inline: json.RawMessage(`{"type":"object"}`)}
		requireCode(t, declaration.Validate(testSchemas()), "configSchema", CodeConflictingSchema)
	})
	t.Run("uncompilable inline document", func(t *testing.T) {
		declaration := testProviderType()
		declaration.ConfigSchema = SchemaRef{Inline: json.RawMessage(`{"type":42}`)}
		requireCode(t, declaration.Validate(testSchemas()), "configSchema.inline", CodeInvalidSchema)
	})
}

// TestValidateProviderTypeRequiresMarkedSecretFields proves an auth field that
// holds a credential cannot be declared as a plain, echoable value.
func TestValidateProviderTypeRequiresMarkedSecretFields(t *testing.T) {
	declaration := testProviderType()
	declaration.AuthSchema.Fields[0] = AuthField{Name: "apiKey", Required: true} // Secret deliberately false
	requireCode(t, declaration.Validate(testSchemas()), "authSchema.fields[0].secret", CodeSecretFieldUnmarked)

	declaration.AuthSchema.Fields[0].Secret = true
	requireNoError(t, declaration.Validate(testSchemas()))
}

// TestValidateAuthSchemaRejectsMalformedFields covers field-level declaration
// hygiene: a name is required, must be env-shaped and must be unique.
func TestValidateAuthSchemaRejectsMalformedFields(t *testing.T) {
	t.Run("missing name", func(t *testing.T) {
		declaration := testProviderType()
		declaration.AuthSchema.Fields = append(declaration.AuthSchema.Fields, AuthField{Secret: true})
		requireCode(t, declaration.Validate(testSchemas()), "authSchema.fields[1].name", CodeInvalidAuthField)
	})
	t.Run("malformed name", func(t *testing.T) {
		declaration := testProviderType()
		declaration.AuthSchema.Fields = append(declaration.AuthSchema.Fields, AuthField{Name: "api key", Secret: true})
		requireCode(t, declaration.Validate(testSchemas()), "authSchema.fields[1].name", CodeInvalidAuthField)
	})
	t.Run("duplicate name", func(t *testing.T) {
		declaration := testProviderType()
		declaration.AuthSchema.Fields = append(declaration.AuthSchema.Fields, AuthField{Name: "apiKey", Secret: true})
		requireCode(t, declaration.Validate(testSchemas()), "authSchema.fields[1].name", CodeDuplicateAuthField)
	})
}

// TestValidateProviderTypeRejectsBadIdentity covers the declaration identity
// rules.
func TestValidateProviderTypeRejectsBadIdentity(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ProviderType)
		field  string
		code   string
	}{
		{name: "missing type id", mutate: func(d *ProviderType) { d.TypeID = " " }, field: "typeId", code: CodeMissingType},
		{name: "uppercase type id", mutate: func(d *ProviderType) { d.TypeID = "LLM" }, field: "typeId", code: CodeInvalidType},
		{name: "missing plugin", mutate: func(d *ProviderType) { d.PluginID = "" }, field: "pluginId", code: CodeMissingPlugin},
		{name: "missing display name", mutate: func(d *ProviderType) { d.DisplayName = " " }, field: "displayName", code: CodeMissingName},
		{name: "no capabilities", mutate: func(d *ProviderType) { d.Capabilities = nil }, field: "capabilities", code: CodeMissingCapability},
		{name: "bad feature port", mutate: func(d *ProviderType) { d.FeaturePorts = []string{"llm chat"} }, field: "featurePorts[0]", code: CodeInvalidFeaturePort},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			declaration := testProviderType()
			testCase.mutate(&declaration)
			requireCode(t, declaration.Validate(testSchemas()), testCase.field, testCase.code)
		})
	}
}

// TestSchemaSetValidateCompilesEveryDocument proves plugin assembly catches a
// broken schema once, before any instance references it.
func TestSchemaSetValidateCompilesEveryDocument(t *testing.T) {
	requireNoError(t, testSchemas().Validate())

	cases := map[string]json.RawMessage{
		"uncompilable document": json.RawMessage(`{"type":42}`),
		"non-json document":     json.RawMessage(`{`),
	}
	for name, document := range cases {
		name, document := name, document
		t.Run(name, func(t *testing.T) {
			err := SchemaSet{"family.config": document}.Validate()
			if err == nil {
				t.Fatalf("schema set accepted %s", name)
			}
			if !HasCode(err, CodeInvalidSchema) {
				t.Fatalf("expected %s, got %v", CodeInvalidSchema, err)
			}
		})
	}
}

// TestValidateProviderInstanceAcceptsCompleteInstance is the baseline for the
// instance rules.
func TestValidateProviderInstanceAcceptsCompleteInstance(t *testing.T) {
	requireNoError(t, testInstance().Validate(testTypes(), testSchemas()))
}

// TestValidateProviderInstanceRejectsBadIdentity covers the instance identity
// and tenant-scope rules.
func TestValidateProviderInstanceRejectsBadIdentity(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ProviderInstance)
		field  string
		code   string
	}{
		{name: "missing id", mutate: func(i *ProviderInstance) { i.ID = "" }, field: "id", code: CodeMissingID},
		{name: "malformed id", mutate: func(i *ProviderInstance) { i.ID = "bad id" }, field: "id", code: CodeInvalidID},
		{name: "missing tenant", mutate: func(i *ProviderInstance) { i.TenantID = "" }, field: "tenantId", code: CodeMissingTenant},
		{name: "missing name", mutate: func(i *ProviderInstance) { i.Name = "" }, field: "name", code: CodeMissingName},
		{name: "missing type", mutate: func(i *ProviderInstance) { i.TypeID = "" }, field: "typeId", code: CodeMissingType},
		{name: "uppercase type", mutate: func(i *ProviderInstance) { i.TypeID = "LLM" }, field: "typeId", code: CodeInvalidType},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			instance := testInstance()
			testCase.mutate(&instance)
			requireCode(t, instance.Validate(testTypes(), testSchemas()), testCase.field, testCase.code)
		})
	}
}

// TestValidateProviderInstanceRejectsUnknownType proves a well-formed type id no
// installed plugin declares is rejected: nothing could execute the instance.
func TestValidateProviderInstanceRejectsUnknownType(t *testing.T) {
	instance := testInstance()
	instance.TypeID = TypeVideo // valid id, not in the installed type set
	requireCode(t, instance.Validate(testTypes(), testSchemas()), "typeId", CodeUnknownType)
}

// TestValidateProviderInstanceInheritsPluginSource proves the instance may omit
// the plugin source (it is inherited from the type declaration) but may not
// contradict it.
func TestValidateProviderInstanceInheritsPluginSource(t *testing.T) {
	inherited := testInstance()
	inherited.PluginID = ""
	requireNoError(t, inherited.Validate(testTypes(), testSchemas()))
	if resolved := inherited.WithTypeDefaults(testTypes()); resolved.PluginID != "plugin-llm" {
		t.Fatalf("WithTypeDefaults() plugin = %q, want %q", resolved.PluginID, "plugin-llm")
	}

	mismatched := testInstance()
	mismatched.PluginID = "plugin-other"
	requireCode(t, mismatched.Validate(testTypes(), testSchemas()), "pluginId", CodePluginMismatch)
}

// TestValidateProviderInstanceAcceptsCredentialReferencesOnly proves the
// credential field accepts references and rejects literals. An empty reference
// is legal: a keyless provider needs no credential.
func TestValidateProviderInstanceAcceptsCredentialReferencesOnly(t *testing.T) {
	accepted := []string{"", "${OPENAI_API_KEY}", "env:OPENAI_API_KEY", "secret:openai_key", "vault://img"}
	for _, ref := range accepted {
		instance := testInstance()
		instance.CredentialRef = ref
		if err := instance.Validate(testTypes(), testSchemas()); err != nil {
			t.Errorf("credentialRef %q: unexpected error: %v", ref, err)
		}
	}
	rejected := []string{"sk-abcdefghijklmnop", "Bearer abcdefghij", "plain-key", "secret:", "secret:has space", "${}"}
	for _, ref := range rejected {
		instance := testInstance()
		instance.CredentialRef = ref
		requireCode(t, instance.Validate(testTypes(), testSchemas()), "credentialRef", CodeInvalidCredentialRef)
	}
}

// TestValidateProviderInstanceRejectsInlineCredentials covers every way a
// credential value can leak into the persisted instance: a credential-shaped
// config key, a nested one, a second credential reference in config, a
// credential in the endpoint and a literal credential in a config value.
func TestValidateProviderInstanceRejectsInlineCredentials(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ProviderInstance)
		field  string
	}{
		{
			name:   "config api key",
			mutate: func(i *ProviderInstance) { i.Config = map[string]any{"apiKey": "abc"} },
			field:  "config.apiKey",
		},
		{
			name:   "nested token",
			mutate: func(i *ProviderInstance) { i.Config = map[string]any{"auth": map[string]any{"token": "abc"}} },
			field:  "config.auth.token",
		},
		{
			name:   "second credential reference",
			mutate: func(i *ProviderInstance) { i.Config = map[string]any{"credentialRef": "secret:other"} },
			field:  "config.credentialRef",
		},
		{
			name:   "credential in endpoint",
			mutate: func(i *ProviderInstance) { i.Endpoint = "https://user:pass@api.example.com/v1" },
			field:  "endpoint",
		},
		{
			name:   "literal credential in a value",
			mutate: func(i *ProviderInstance) { i.Config = map[string]any{"note": "Authorization: Bearer sk-abcdefghijkl"} },
			field:  "config.note",
		},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			instance := testInstance()
			testCase.mutate(&instance)
			requireCode(t, instance.Validate(testTypes(), testSchemas()), testCase.field, CodeInlineCredential)
		})
	}
}

// TestDomainTypesCarryNoCredentialValueField proves structurally that no domain
// type has a field a credential value could be stored in. The only credential
// field is ProviderInstance.CredentialRef, which is a reference by name and is
// checked separately.
func TestDomainTypesCarryNoCredentialValueField(t *testing.T) {
	values := []any{
		ProviderType{},
		AuthSchema{},
		AuthField{},
		ModelDescriptor{},
		ProviderInstance{},
		HealthStatus{},
		ParameterSchema{},
		SchemaRef{},
	}
	for _, value := range values {
		typ := reflect.TypeOf(value)
		walkCredentialFields(t, typ, typ.Name())
	}
}

// walkCredentialFields walks exported and nested struct fields.
func walkCredentialFields(t *testing.T, typ reflect.Type, prefix string) {
	t.Helper()
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		fieldPath := prefix + "." + field.Name
		// A boolean is a flag about a credential ("secret": true), not a value a
		// credential could be stored in, so flags are exempt from the name check.
		canHoldValue := field.Type.Kind() != reflect.Bool
		if canHoldValue && (credentialValueKey(field.Name) || (name != "" && credentialValueKey(name))) {
			t.Errorf("%s is credential-shaped; credentials must be referenced, never stored", fieldPath)
		}
		walkCredentialFields(t, field.Type, fieldPath)
	}
}

// TestProviderInstanceJSONUsesCredentialReferenceOnly proves behaviorally that a
// fully populated instance serializes with a reference and without a single
// credential value key anywhere in the document.
func TestProviderInstanceJSONUsesCredentialReferenceOnly(t *testing.T) {
	instance := testInstance()
	instance.Config = map[string]any{
		"organization": "org-1",
		"headers":      map[string]any{"X-Title": "keel"},
	}
	instance.Models = append(instance.Models, ModelDescriptor{ModelID: "text-embedding-3-small"})

	blob, err := json.Marshal(instance)
	requireNoError(t, err)
	var decoded any
	requireNoError(t, json.Unmarshal(blob, &decoded))

	if offenders := credentialValuePaths(decoded, ""); len(offenders) != 0 {
		t.Fatalf("serialized instance carries credential-shaped keys: %v", offenders)
	}
	reference, ok := decoded.(map[string]any)["credentialRef"].(string)
	if !ok || !credentialRefPattern.MatchString(reference) {
		t.Fatalf("credentialRef %v is not a reference", decoded.(map[string]any)["credentialRef"])
	}
}

// credentialValuePaths returns the paths of credential-shaped keys in a decoded
// JSON document.
func credentialValuePaths(value any, prefix string) []string {
	var offenders []string
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			fieldPath := prefix + key
			if prefix != "" {
				fieldPath = prefix + "." + key
			}
			if credentialValueKey(key) {
				offenders = append(offenders, fieldPath)
				continue
			}
			offenders = append(offenders, credentialValuePaths(nested, fieldPath)...)
		}
	case []any:
		for index, nested := range typed {
			offenders = append(offenders, credentialValuePaths(nested, fmt.Sprintf("%s[%d]", prefix, index))...)
		}
	}
	return offenders
}

// TestNormalizeCatalogCollapsesIdenticalDuplicates proves a catalog that repeats
// one entry converges instead of failing, and that normalization happens before
// comparison.
func TestNormalizeCatalogCollapsesIdenticalDuplicates(t *testing.T) {
	models := []ModelDescriptor{
		{ModelID: "gpt-4o", ContextWindow: 128000},
		{ModelID: " gpt-4o ", ContextWindow: 128000}, // same entry after normalization
		{ModelID: "o3", ContextWindow: 200000},
	}
	normalized, err := NormalizeCatalog(models)
	requireNoError(t, err)
	if len(normalized) != 2 {
		t.Fatalf("catalog length = %d, want 2: %v", len(normalized), normalized)
	}
	if normalized[0].ModelID != "gpt-4o" || normalized[0].DisplayName != "gpt-4o" {
		t.Errorf("first entry = %+v, want id and display name gpt-4o", normalized[0])
	}
	if normalized[1].ModelID != "o3" {
		t.Errorf("second entry = %q, want o3", normalized[1].ModelID)
	}
}

// TestNormalizeCatalogRejectsConflictingDuplicates proves one model id may not
// have two different descriptors: order-dependent catalogs are a conflict.
func TestNormalizeCatalogRejectsConflictingDuplicates(t *testing.T) {
	models := []ModelDescriptor{
		{ModelID: "gpt-4o", ContextWindow: 128000},
		{ModelID: "gpt-4o", ContextWindow: 64000},
	}
	_, err := NormalizeCatalog(models)
	requireCode(t, err, "models[1].modelId", CodeConflictingModel)

	instance := testInstance()
	instance.Models = models
	requireCode(t, instance.Validate(testTypes(), testSchemas()), "models[1].modelId", CodeConflictingModel)
}

// TestProviderInstanceNormalizeDedupesCatalog proves Normalize returns the
// collapsed catalog and makes the unchecked state explicit.
func TestProviderInstanceNormalizeDedupesCatalog(t *testing.T) {
	instance := testInstance()
	instance.Health = HealthStatus{}
	instance.Models = []ModelDescriptor{
		{ModelID: "gpt-4o"},
		{ModelID: "gpt-4o"},
		{ModelID: "o3"},
	}
	normalized, err := instance.Normalize()
	requireNoError(t, err)
	if len(normalized.Models) != 2 {
		t.Fatalf("normalized catalog = %v, want 2 entries", normalized.Models)
	}
	if normalized.Health.State != HealthUnchecked {
		t.Errorf("health = %q, want %q", normalized.Health.State, HealthUnchecked)
	}
	if normalized.Models[0].DisplayName != "gpt-4o" {
		t.Errorf("display name = %q, want the model id", normalized.Models[0].DisplayName)
	}
}

// TestMergeParameterSchemasAppliesLayerOrder pins the merge semantics of one
// fold: the later layer wins for the schema reference, inherits keys it does
// not mention and merges nested objects.
func TestMergeParameterSchemasAppliesLayerOrder(t *testing.T) {
	typeLayer := ParameterSchema{
		Schema: SchemaRef{Ref: "family.parameters"},
		Defaults: map[string]any{
			"reasoning":       "medium",
			"maxOutputTokens": 2048,
			"temperature":     0.7,
			"nested":          map[string]any{"a": 1},
		},
	}
	providerLayer := ParameterSchema{Defaults: map[string]any{"reasoning": "high"}}
	modelLayer := ParameterSchema{
		Schema:   SchemaRef{Ref: "llm.model.parameters"},
		Defaults: map[string]any{"maxOutputTokens": 4096, "nested": map[string]any{"b": 2}},
	}

	merged := MergeParameterSchemas(MergeParameterSchemas(typeLayer, providerLayer), modelLayer)

	if merged.Schema.Ref != "llm.model.parameters" {
		t.Errorf("schema = %q, want the model override to win", merged.Schema.Ref)
	}
	if merged.Defaults["reasoning"] != "high" {
		t.Errorf("reasoning = %v, want the provider override high", merged.Defaults["reasoning"])
	}
	if merged.Defaults["maxOutputTokens"] != 4096 {
		t.Errorf("maxOutputTokens = %v, want the model override 4096", merged.Defaults["maxOutputTokens"])
	}
	if merged.Defaults["temperature"] != 0.7 {
		t.Errorf("temperature = %v, want the inherited type default 0.7", merged.Defaults["temperature"])
	}
	nested, ok := merged.Defaults["nested"].(map[string]any)
	if !ok || nested["a"] != 1 || nested["b"] != 2 {
		t.Errorf("nested = %v, want {a:1 b:2}", merged.Defaults["nested"])
	}
}

// TestEffectiveParametersMergesTypeThenProviderThenModel is the three-layer
// order test: type default first, provider instance second, model last.
func TestEffectiveParametersMergesTypeThenProviderThenModel(t *testing.T) {
	declaration := testProviderType()
	declaration.Parameters = ParameterSchema{
		Schema: SchemaRef{Ref: "family.parameters"},
		Defaults: map[string]any{
			"reasoning":       "medium",
			"maxOutputTokens": 2048,
			"temperature":     0.7,
		},
	}
	instance := testInstance()
	instance.Parameters = ParameterSchema{Defaults: map[string]any{"reasoning": "high"}}
	model := ModelDescriptor{
		ModelID:    "gpt-4o",
		Parameters: ParameterSchema{Defaults: map[string]any{"maxOutputTokens": 4096}},
	}

	merged := EffectiveParameters(declaration, instance, model)

	if merged.Defaults["reasoning"] != "high" {
		t.Errorf("reasoning = %v, want the provider layer to override the type default", merged.Defaults["reasoning"])
	}
	if merged.Defaults["maxOutputTokens"] != 4096 {
		t.Errorf("maxOutputTokens = %v, want the model layer to override the provider and type layers", merged.Defaults["maxOutputTokens"])
	}
	if merged.Defaults["temperature"] != 0.7 {
		t.Errorf("temperature = %v, want the type-only default to survive", merged.Defaults["temperature"])
	}
	if merged.Schema.Ref != "family.parameters" {
		t.Errorf("schema = %q, want the type schema inherited by the later layers", merged.Schema.Ref)
	}
}

// TestMergeParameterSchemasDoesNotMutateInputs proves merging is pure: neither
// the base nor the override is changed, including nested objects.
func TestMergeParameterSchemasDoesNotMutateInputs(t *testing.T) {
	baseDefaults := map[string]any{"nested": map[string]any{"a": 1}}
	overrideDefaults := map[string]any{"nested": map[string]any{"b": 2}}
	base := ParameterSchema{Defaults: baseDefaults}
	override := ParameterSchema{Defaults: overrideDefaults}

	merged := MergeParameterSchemas(base, override)
	nested, ok := merged.Defaults["nested"].(map[string]any)
	if !ok || nested["a"] != 1 || nested["b"] != 2 {
		t.Fatalf("merged nested = %v, want {a:1 b:2}", merged.Defaults["nested"])
	}
	baseNested := baseDefaults["nested"].(map[string]any)
	if len(baseNested) != 1 || baseNested["a"] != 1 {
		t.Errorf("base was mutated: %v", baseNested)
	}
	overrideNested := overrideDefaults["nested"].(map[string]any)
	if len(overrideNested) != 1 || overrideNested["b"] != 2 {
		t.Errorf("override was mutated: %v", overrideNested)
	}
}

// TestProviderConfigHashTracksConfiguration proves the cache key is stable for
// identical configuration and changes with anything that decides what the
// provider answers with.
func TestProviderConfigHashTracksConfiguration(t *testing.T) {
	base := testInstance()
	baseHash, err := base.ConfigHash()
	requireNoError(t, err)
	if !strings.HasPrefix(baseHash, "sha256:") || len(baseHash) != len("sha256:")+64 {
		t.Fatalf("config hash %q is not a sha256 digest", baseHash)
	}

	same := testInstance()
	sameHash, err := same.ConfigHash()
	requireNoError(t, err)
	if sameHash != baseHash {
		t.Errorf("identical configuration hashed differently: %q != %q", sameHash, baseHash)
	}

	changedEndpoint := testInstance()
	changedEndpoint.Endpoint = "https://api.openai.com/v2"
	changedEndpointHash, err := changedEndpoint.ConfigHash()
	requireNoError(t, err)
	if changedEndpointHash == baseHash {
		t.Error("changing the endpoint did not change the config hash")
	}

	changedConfig := testInstance()
	changedConfig.Config = map[string]any{"organization": "org-999"}
	changedConfigHash, err := changedConfig.ConfigHash()
	requireNoError(t, err)
	if changedConfigHash == baseHash {
		t.Error("changing the configuration did not change the config hash")
	}

	rotated := testInstance()
	rotated.CredentialRef = "secret:other_key"
	rotatedHash, err := rotated.ConfigHash()
	requireNoError(t, err)
	if rotatedHash == baseHash {
		t.Error("changing the credential reference did not change the config hash")
	}
}

// TestHealthStatusValidatesStateVocabulary covers the five states the spec
// requires plus the explicit unchecked default.
func TestHealthStatusValidatesStateVocabulary(t *testing.T) {
	legal := []string{"", HealthUnchecked, HealthReady, HealthNeedsAuth, HealthUnreachable, HealthInvalidConfig, HealthDegraded}
	for _, state := range legal {
		requireNoError(t, HealthStatus{State: state}.Validate())
	}
	for _, state := range []string{"healthy", "OK", "READY", "needs-auth"} {
		requireCode(t, HealthStatus{State: state}.Validate(), "state", CodeInvalidStatus)
	}

	instance := testInstance()
	instance.Health.State = "healthy"
	requireCode(t, instance.Validate(testTypes(), testSchemas()), "health", CodeInvalidStatus)
}

// TestHealthStatusRejectsCredentialInMessage proves health text, which reaches
// logs and UI state, cannot carry a literal credential.
func TestHealthStatusRejectsCredentialInMessage(t *testing.T) {
	health := HealthStatus{
		State:   HealthNeedsAuth,
		Message: "401 from https://api.example.com: Authorization: Bearer sk-abcdefghijkl",
	}
	requireCode(t, health.Validate(), "message", CodeCredentialInMessage)

	instance := testInstance()
	instance.Health = health
	requireCode(t, instance.Validate(testTypes(), testSchemas()), "health.message", CodeCredentialInMessage)
}

// TestValidateModelDescriptorRejectsBadCatalogEntry covers the model entry
// rules: identity, non-negative budgets and a resolvable parameter schema.
func TestValidateModelDescriptorRejectsBadCatalogEntry(t *testing.T) {
	cases := []struct {
		name  string
		model ModelDescriptor
		field string
		code  string
	}{
		{name: "missing id", model: ModelDescriptor{}, field: "modelId", code: CodeMissingModel},
		{name: "malformed id", model: ModelDescriptor{ModelID: "bad id"}, field: "modelId", code: CodeInvalidModel},
		{name: "path traversal", model: ModelDescriptor{ModelID: "a/../b"}, field: "modelId", code: CodeInvalidModel},
		{name: "negative window", model: ModelDescriptor{ModelID: "gpt-4o", ContextWindow: -1}, field: "contextWindow", code: CodeInvalidContextWindow},
		{name: "negative output", model: ModelDescriptor{ModelID: "gpt-4o", MaxOutput: -1}, field: "maxOutput", code: CodeInvalidMaxOutput},
		{
			name:  "unknown parameter schema",
			model: ModelDescriptor{ModelID: "gpt-4o", Parameters: ParameterSchema{Schema: SchemaRef{Ref: "missing.parameters"}}},
			field: "parameters.schema.ref",
			code:  CodeUnknownSchema,
		},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			requireCode(t, testCase.model.Validate(testSchemas()), testCase.field, testCase.code)
		})
	}

	requireNoError(t, ModelDescriptor{ModelID: "@cf/cloudflare/clef", DisplayName: "Clef"}.Validate(testSchemas()))
}

// TestModelDescriptorCarriesNoPricingField guards the spec rule that the model
// catalog never carries prices or plan data: usage is metered, pricing belongs
// to the contract side. The check is a regression guard for future fields.
func TestModelDescriptorCarriesNoPricingField(t *testing.T) {
	blob, err := json.Marshal(ModelDescriptor{ModelID: "gpt-4o", DisplayName: "GPT-4o"})
	requireNoError(t, err)
	var decoded map[string]any
	requireNoError(t, json.Unmarshal(blob, &decoded))
	for key := range decoded {
		switch strings.ToLower(key) {
		case "price", "prices", "pricing", "cost", "costs", "currency", "plan", "plans":
			t.Errorf("model descriptor serializes %q; the catalog must not carry pricing data", key)
		}
	}
}

// TestValidateProviderInstanceResolvesParameterSchemaRefs covers the two
// parameter layers an instance owns: the provider-layer reference and each
// model's reference.
func TestValidateProviderInstanceResolvesParameterSchemaRefs(t *testing.T) {
	instance := testInstance()
	instance.Parameters = ParameterSchema{Schema: SchemaRef{Ref: "missing.parameters"}}
	requireCode(t, instance.Validate(testTypes(), testSchemas()), "parameters.schema.ref", CodeUnknownSchema)

	instance = testInstance()
	instance.Models[0].Parameters = ParameterSchema{Schema: SchemaRef{Ref: "missing.parameters"}}
	requireCode(t, instance.Validate(testTypes(), testSchemas()), "models[0].parameters.schema.ref", CodeUnknownSchema)
}

// TestProviderStatusAxisIsSeparateFromHealth pins the two-axis rule: status is
// the user's enablement (active|disabled), health is the measurement. They live
// in different fields with different JSON names, so a disabled instance reads
// back disabled with its last health intact instead of one overwriting the other.
func TestProviderStatusAxisIsSeparateFromHealth(t *testing.T) {
	instance := testInstance()
	instance.Status = ProviderStatusDisabled
	instance.Health = HealthStatus{State: HealthNeedsAuth, Message: "credential missing"}
	requireNoError(t, instance.Validate(testTypes(), testSchemas()))
	requireNoError(t, instance.ValidateShape())

	blob, err := json.Marshal(instance)
	requireNoError(t, err)
	var decoded map[string]any
	requireNoError(t, json.Unmarshal(blob, &decoded))
	if decoded["status"] != string(ProviderStatusDisabled) {
		t.Fatalf("status = %v, want %q", decoded["status"], ProviderStatusDisabled)
	}
	health, ok := decoded["health"].(map[string]any)
	if !ok || health["state"] != HealthNeedsAuth {
		t.Fatalf("health = %v, want the measured needs_auth state", decoded["health"])
	}

	// The empty enablement value is the active default, and Normalize makes it
	// explicit; an unknown value is rejected on its own field.
	normalized, err := testInstance().Normalize()
	requireNoError(t, err)
	if normalized.Status != ProviderStatusActive {
		t.Fatalf("normalized status = %q, want %q", normalized.Status, ProviderStatusActive)
	}
	unknown := testInstance()
	unknown.Status = "retired"
	requireCode(t, unknown.ValidateShape(), "status", CodeInvalidProviderStatus)

	if _, ok := ParseProviderStatus(string(ProviderStatusDisabled)); !ok {
		t.Fatal("disabled must parse")
	}
	if _, ok := ParseProviderStatus("retired"); ok {
		t.Fatal("retired must not parse")
	}
}

// TestValidateProviderInstanceAcceptsWebSocketEndpoint pins the speech-provider
// endpoint rule: ws/wss are legal on the provider registry, so a TTS/STT
// provider cannot be rejected by an http-only check on another surface.
func TestValidateProviderInstanceAcceptsWebSocketEndpoint(t *testing.T) {
	for _, endpoint := range []string{"ws://speech.example.com/v1", "wss://speech.example.com/v1"} {
		instance := testInstance()
		instance.Endpoint = endpoint
		requireNoError(t, instance.Validate(testTypes(), testSchemas()))
		requireNoError(t, instance.ValidateShape())
	}
}

// TestValidateProviderInstanceRejectsCredentialsInParameterDefaults covers the
// side door the shared scanner closes: parameter defaults are persisted
// configuration and merge into EffectiveParameters, so a credential-shaped key or
// a literal token there is rejected at every layer (type, instance, model).
func TestValidateProviderInstanceRejectsCredentialsInParameterDefaults(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ProviderInstance)
		field  string
	}{
		{
			name: "provider layer defaults",
			mutate: func(i *ProviderInstance) {
				i.Parameters = ParameterSchema{Defaults: map[string]any{"apiKey": "sk-live-secret"}}
			},
			field: "parameters.defaults.apiKey",
		},
		{
			name: "nested defaults",
			mutate: func(i *ProviderInstance) {
				i.Parameters = ParameterSchema{Defaults: map[string]any{"auth": map[string]any{"token": "abc"}}}
			},
			field: "parameters.defaults.auth.token",
		},
		{
			name: "model layer defaults",
			mutate: func(i *ProviderInstance) {
				i.Models[0].Parameters = ParameterSchema{Defaults: map[string]any{"secret": "abc"}}
			},
			field: "models[0].parameters.defaults.secret",
		},
		{
			name: "literal credential in a default value",
			mutate: func(i *ProviderInstance) {
				i.Parameters = ParameterSchema{Defaults: map[string]any{"note": "Authorization: Bearer sk-abcdefghijkl"}}
			},
			field: "parameters.defaults.note",
		},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			instance := testInstance()
			testCase.mutate(&instance)
			requireCode(t, instance.Validate(testTypes(), testSchemas()), testCase.field, CodeInlineCredential)
		})
	}

	// The type declaration's defaults are scanned too: a plugin cannot ship a
	// credential in the family default parameters.
	declaration := testProviderType()
	declaration.Parameters = ParameterSchema{Defaults: map[string]any{"apiKey": "sk-live-secret"}}
	requireCode(t, declaration.Validate(testSchemas()), "parameters.defaults.apiKey", CodeInlineCredential)
}

// TestValidateProviderInstanceRejectsCredentialsInArrays covers the array half
// of the scanner: {"headers": [{"apiKey": "x"}]} is the shape a naive
// map-only walk misses.
func TestValidateProviderInstanceRejectsCredentialsInArrays(t *testing.T) {
	instance := testInstance()
	instance.Config = map[string]any{
		"headers": []any{map[string]any{"apiKey": "sk-live-secret"}},
	}
	requireCode(t, instance.Validate(testTypes(), testSchemas()), "config.headers[0].apiKey", CodeInlineCredential)
	requireCode(t, instance.ValidateShape(), "config.headers[0].apiKey", CodeInlineCredential)
}

// TestInvalidCredentialRefDoesNotEchoValue pins the leak-audit fix (R1): the
// rejection message must name the field and the accepted reference forms, never
// the user's literal. A mistyped literal credential would otherwise come back
// in the same response that rejected it.
func TestInvalidCredentialRefDoesNotEchoValue(t *testing.T) {
	const literal = "sk-live-0123456789abcdef"
	instance := testInstance()
	instance.CredentialRef = literal
	err := instance.Validate(testTypes(), testSchemas())
	if err == nil {
		t.Fatal("a literal credential was accepted")
	}
	if msg := err.Error(); strings.Contains(msg, literal) {
		t.Fatalf("validation error echoed the literal credential: %s", msg)
	}
}
