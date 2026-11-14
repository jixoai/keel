package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentteam"
	"github.com/jixoai/keel/internal/modelprovider"
)

// The DTO contract tests live here: JSON roundtrip, stable field names, strict
// decoding (unknown fields, tenant fields, plaintext credentials) and the
// field-level validation of every request body. Routes are deliberately not
// exercised: this file pins the shapes the handlers will hand to the store.

const testTenantID = "11111111-1111-1111-1111-111111111111"

func testTenant() uuid.UUID { return uuid.MustParse(testTenantID) }

// mustJSON marshals a value or fails the test.
func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// roundTripStrict marshals original, decodes it with the strict decoder and
// requires the value and the encoding to survive unchanged.
func roundTripStrict[T any](t *testing.T, original T) T {
	t.Helper()
	raw := mustJSON(t, original)
	var decoded T
	if err := decodeStrictJSON(bytes.NewReader(raw), &decoded); err != nil {
		t.Fatalf("strict decode: %v", err)
	}
	if !reflect.DeepEqual(original, decoded) {
		t.Fatalf("roundtrip changed the value:\n got: %#v\nwant: %#v", decoded, original)
	}
	if again := mustJSON(t, decoded); !bytes.Equal(raw, again) {
		t.Fatalf("encoding is not stable:\n got: %s\nwant: %s", again, raw)
	}
	return decoded
}

// requireFieldError asserts err is a validation failure carrying exactly the
// field/code pair, and returns all of its field errors.
func requireFieldError(t *testing.T, err error, field, code string) []agentteam.FieldError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a validation error with %s/%s, got nil", field, code)
	}
	fields := agentteam.FieldErrors(err)
	if fields == nil {
		t.Fatalf("error is not a field-level validation error: %v", err)
	}
	for _, problem := range fields {
		if problem.Field == field && problem.Code == code {
			return fields
		}
	}
	t.Fatalf("no field error %s/%s in %v", field, code, fields)
	return nil
}

// requireNoFieldError asserts Validate accepted the value.
func requireNoFieldError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func sampleAgent() agentteam.Agent {
	return agentteam.Agent{
		ID:          "agent_design_lead",
		Name:        "设计主理人",
		Description: "负责视觉方向、组件规范与评审",
		Avatar:      agentteam.Avatar{Kind: agentteam.AvatarIdenticon, Seed: "agent_design_lead"},
		DefaultModel: agentteam.ModelBinding{
			ProviderID: "prov_ds",
			ModelID:    "deepseek-v4.1",
			Tuning:     map[string]any{"reasoning": "high"},
		},
		Tools: []agentteam.ToolGrant{
			{ToolID: "bash", Enabled: true},
			{ToolID: "read", Enabled: true, Config: map[string]any{"depth": "full"}},
		},
		SystemPrompt: agentteam.SystemPrompt{Kind: agentteam.SystemPromptFile, Ref: "AGENTS.md"},
		TeamCLI:      agentteam.TeamCLI{Enabled: true, Commands: []string{agentteam.TeamCLIMemberAdd}},
		Skills:       []string{"design-review"},
		Archived:     true,
	}
}

func sampleAgentRequest() AgentWriteRequest {
	agent := sampleAgent()
	return AgentWriteRequest{
		ID:           agent.ID,
		Name:         agent.Name,
		Description:  agent.Description,
		Avatar:       agent.Avatar,
		DefaultModel: agent.DefaultModel,
		Tools:        agent.Tools,
		SystemPrompt: agent.SystemPrompt,
		TeamCLI:      agent.TeamCLI,
		Skills:       agent.Skills,
	}
}

func sampleTeamRequest() AgentTeamWriteRequest {
	return AgentTeamWriteRequest{
		ID:          "team_product",
		Name:        "产品小队",
		Description: "产品、设计、工程三角协作",
		Captain:     agentteam.TeamMember{AgentID: "agent_product_lead"},
		Members: []agentteam.TeamMember{
			{AgentID: "agent_design_lead", Label: "设计", Description: "视觉与组件"},
			{
				AgentID: "agent_engineer",
				Label:   "工程",
				Inline: &agentteam.Agent{
					ID:   "agent_engineer",
					Name: "工程师",
					Avatar: agentteam.Avatar{
						Kind: agentteam.AvatarIdenticon,
						Seed: "agent_engineer",
					},
				},
			},
		},
		Defaults: agentteam.TeamDefaults{
			Tools:         []string{"bash", "read"},
			ModelOverride: &agentteam.ModelBinding{ProviderID: "prov_ds", ModelID: "deepseek-v4.1"},
		},
		Capabilities: agentteam.TeamCapabilities{
			TeamCLI: agentteam.TeamCLI{
				Enabled:  true,
				Commands: []string{agentteam.TeamCLIMemberAdd, agentteam.TeamCLIMemberRemove},
			},
		},
		FeatureBindings: []agentteam.FeatureBinding{
			{Port: "team.assign", CLI: "keel team assign"},
		},
	}
}

func sampleProviderRequest() ModelProviderWriteRequest {
	return ModelProviderWriteRequest{
		ID:            "prov_ds",
		TypeID:        "llm",
		Name:          "DeepSeek",
		CredentialRef: "vault://ds",
		Endpoint:      "https://api.example.com/v1",
		Config:        map[string]any{"region": "cn"},
		Status:        string(modelprovider.ProviderStatusActive),
	}
}

func sampleProviderView() ModelProviderView {
	return ModelProviderView{
		TenantID:      testTenant(),
		ID:            "prov_ds",
		TypeID:        "llm",
		PluginID:      "keel.model",
		Name:          "DeepSeek",
		CredentialRef: "vault://ds",
		Endpoint:      "https://api.example.com/v1",
		Config:        map[string]any{"region": "cn"},
		Status:        string(modelprovider.ProviderStatusActive),
		Health:        modelprovider.HealthReady,
		Models: []ModelCatalogEntry{{
			ModelID:            "deepseek-v4.1",
			DisplayName:        "DeepSeek V4.1",
			Capabilities:       []string{"reasoning", "tools"},
			ContextWindow:      128000,
			MaxOutput:          8192,
			ParameterSchemaRef: "#/definitions/model_parameters",
		}},
	}
}

// TestAgentWriteRequestRoundTrip pins the agent request shape: a fully
// populated body survives strict decoding unchanged and re-encodes to the same
// bytes.
func TestAgentWriteRequestRoundTrip(t *testing.T) {
	decoded := roundTripStrict(t, sampleAgentRequest())
	if got, want := decoded, sampleAgentRequest(); !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded request = %#v, want %#v", got, want)
	}
	if agent := decoded.Agent(); agent.Avatar.Kind == "" || agent.Avatar.Seed == "" {
		t.Fatalf("converted agent lost its avatar defaults: %#v", agent.Avatar)
	}
}

// TestAgentTeamWriteRequestRoundTrip pins the team request shape, including the
// inline member definition and the team defaults.
func TestAgentTeamWriteRequestRoundTrip(t *testing.T) {
	decoded := roundTripStrict(t, sampleTeamRequest())
	team := decoded.Team()
	if len(team.Members) != 2 || team.Members[1].Inline == nil {
		t.Fatalf("team conversion lost a member or its inline agent: %#v", team.Members)
	}
	if team.Captain.AgentID != "agent_product_lead" {
		t.Fatalf("captain = %q", team.Captain.AgentID)
	}
}

// TestModelProviderWriteRequestRoundTrip pins the provider request shape.
func TestModelProviderWriteRequestRoundTrip(t *testing.T) {
	roundTripStrict(t, sampleProviderRequest())
}

// TestAgentViewRoundTrip pins the agent response shape, including the tenant
// projection and the archived flag.
func TestAgentViewRoundTrip(t *testing.T) {
	view := NewAgentView(testTenant(), sampleAgent())
	if view.TenantID != testTenant() {
		t.Fatalf("tenant projection = %v", view.TenantID)
	}
	roundTripStrict(t, view)
}

// TestAgentTeamViewRoundTrip pins the team response shape.
func TestAgentTeamViewRoundTrip(t *testing.T) {
	team := sampleTeamRequest().Team()
	team.Seal()
	roundTripStrict(t, NewAgentTeamView(testTenant(), *team))
}

// TestModelProviderViewRoundTrip pins the provider response shape.
func TestModelProviderViewRoundTrip(t *testing.T) {
	roundTripStrict(t, sampleProviderView())
}

// TestAgentTeamJSONFieldNamesAreStable pins the exact set of top-level JSON
// names of every request and response DTO. A rename, a removed field or a new
// field (for example a tenant field on a request) fails this test instead of
// silently changing the client contract.
func TestAgentTeamJSONFieldNamesAreStable(t *testing.T) {
	team := sampleTeamRequest().Team()
	team.Archived = true
	team.Seal()
	agent := sampleAgent()
	cases := []struct {
		name  string
		value any
		want  []string
	}{
		{
			name:  "AgentWriteRequest",
			value: sampleAgentRequest(),
			want: []string{
				"avatar", "defaultModel", "description", "id", "name",
				"skills", "systemPrompt", "teamCli", "tools",
			},
		},
		{
			name:  "AgentView",
			value: NewAgentView(testTenant(), agent),
			want: []string{
				"archived", "avatar", "defaultModel", "description", "id", "name",
				"skills", "systemPrompt", "teamCli", "tenantId", "tools",
			},
		},
		{
			name:  "AgentTeamWriteRequest",
			value: sampleTeamRequest(),
			want: []string{
				"capabilities", "captain", "defaults", "description", "featureBindings", "id",
				"members", "name",
			},
		},
		{
			name:  "AgentTeamView",
			value: NewAgentTeamView(testTenant(), *team),
			want: []string{
				"archived", "capabilities", "captain", "defaults", "description",
				"featureBindings", "hash", "id", "members", "name", "revision", "status",
				"tenantId",
			},
		},
		{
			name:  "ModelProviderWriteRequest",
			value: sampleProviderRequest(),
			want: []string{
				"config", "credentialRef", "endpoint", "id", "name", "status", "typeId",
			},
		},
		{
			name:  "ModelProviderView",
			value: sampleProviderView(),
			want: []string{
				"config", "credentialRef", "endpoint", "health", "id", "models", "name",
				"pluginId", "status", "tenantId", "typeId",
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var object map[string]json.RawMessage
			if err := json.Unmarshal(mustJSON(t, testCase.value), &object); err != nil {
				t.Fatalf("unmarshal into object: %v", err)
			}
			got := make([]string, 0, len(object))
			for name := range object {
				got = append(got, name)
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("JSON names = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestAgentTeamViewKeepsDerivedFields guards the reason the view is a flat
// struct: embedding agentteam.AgentTeam would promote its MarshalJSON and drop
// the tenant/status/revision/hash projections.
func TestAgentTeamViewKeepsDerivedFields(t *testing.T) {
	team := sampleTeamRequest().Team()
	team.Seal()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(mustJSON(t, NewAgentTeamView(testTenant(), *team)), &object); err != nil {
		t.Fatalf("unmarshal view: %v", err)
	}
	for _, name := range []string{"tenantId", "status", "revision", "hash"} {
		if _, ok := object[name]; !ok {
			t.Fatalf("view lost the %q projection: %v", name, object)
		}
	}
}

// TestDecodeStrictJSONRejectsUnknownFields covers the unknown-field rejection
// at the top level and inside a nested object, including the fields that are
// server-owned and therefore response-only.
func TestDecodeStrictJSONRejectsUnknownFields(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		target    any
		wantField string
	}{
		{
			name:      "agent archived",
			body:      `{"id":"agent_a","name":"A","archived":true}`,
			target:    &AgentWriteRequest{},
			wantField: "archived",
		},
		{
			name:      "nested avatar field",
			body:      `{"id":"agent_a","name":"A","avatar":{"kind":"identicon","seed":"s","bogus":1}}`,
			target:    &AgentWriteRequest{},
			wantField: "bogus",
		},
		{
			name:      "team revision",
			body:      `{"id":"team_a","name":"T","revision":3}`,
			target:    &AgentTeamWriteRequest{},
			wantField: "revision",
		},
		{
			name:      "team hash",
			body:      `{"id":"team_a","name":"T","hash":"deadbeef"}`,
			target:    &AgentTeamWriteRequest{},
			wantField: "hash",
		},
		{
			name:      "provider health",
			body:      `{"id":"prov_a","typeId":"llm","name":"P","health":"ready"}`,
			target:    &ModelProviderWriteRequest{},
			wantField: "health",
		},
		{
			name:      "provider pluginId",
			body:      `{"id":"prov_a","typeId":"llm","name":"P","pluginId":"keel.model"}`,
			target:    &ModelProviderWriteRequest{},
			wantField: "pluginId",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := decodeStrictJSON(strings.NewReader(testCase.body), testCase.target)
			var rejection *BodyRejection
			if !errors.As(err, &rejection) {
				t.Fatalf("error = %v, want *BodyRejection", err)
			}
			if rejection.Reason != BodyRejectionUnknownField {
				t.Fatalf("reason = %q, want %q (%v)", rejection.Reason, BodyRejectionUnknownField, err)
			}
			if rejection.Field != testCase.wantField {
				t.Fatalf("field = %q, want %q", rejection.Field, testCase.wantField)
			}
		})
	}
}

// TestRequestDTOsRejectTenantField is the tenant-ownership check: no request
// DTO declares a tenant field (reflection) and a body that carries one is
// rejected as an unknown field (decoding), so a client can never choose the
// tenant it writes to.
func TestRequestDTOsRejectTenantField(t *testing.T) {
	requests := []struct {
		name   string
		target any
		field  reflect.Type
	}{
		{name: "AgentWriteRequest", target: &AgentWriteRequest{}, field: reflect.TypeOf(AgentWriteRequest{})},
		{name: "AgentTeamWriteRequest", target: &AgentTeamWriteRequest{}, field: reflect.TypeOf(AgentTeamWriteRequest{})},
		{name: "ModelProviderWriteRequest", target: &ModelProviderWriteRequest{}, field: reflect.TypeOf(ModelProviderWriteRequest{})},
	}
	for _, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			for _, tag := range declaredJSONNames(t, request.field) {
				if tag == "tenantId" || tag == "tenant" || tag == "tenant_id" {
					t.Fatalf("%s declares the tenant field %q; the tenant must come from X-Tenant-ID", request.name, tag)
				}
			}
			body := `{"id":"x","name":"X","tenantId":"` + testTenantID + `"}`
			err := decodeStrictJSON(strings.NewReader(body), request.target)
			var rejection *BodyRejection
			if !errors.As(err, &rejection) || rejection.Reason != BodyRejectionUnknownField || rejection.Field != "tenantId" {
				t.Fatalf("tenantId was not rejected as an unknown field: %v", err)
			}
		})
	}
}

// TestRequestDTOsRejectPlaintextCredentialField is the credential check: no DTO
// declares a credential value field (apiKey/secret/password/...), a body that
// carries one is rejected as an unknown field, and a credentialRef holding a
// literal key fails validation.
func TestRequestDTOsRejectPlaintextCredentialField(t *testing.T) {
	dtos := []struct {
		name   string
		target any
		field  reflect.Type
	}{
		{name: "AgentWriteRequest", target: &AgentWriteRequest{}, field: reflect.TypeOf(AgentWriteRequest{})},
		{name: "AgentView", target: &AgentView{}, field: reflect.TypeOf(AgentView{})},
		{name: "AgentTeamWriteRequest", target: &AgentTeamWriteRequest{}, field: reflect.TypeOf(AgentTeamWriteRequest{})},
		{name: "AgentTeamView", target: &AgentTeamView{}, field: reflect.TypeOf(AgentTeamView{})},
		{name: "ModelProviderWriteRequest", target: &ModelProviderWriteRequest{}, field: reflect.TypeOf(ModelProviderWriteRequest{})},
		{name: "ModelProviderView", target: &ModelProviderView{}, field: reflect.TypeOf(ModelProviderView{})},
	}
	for _, dto := range dtos {
		t.Run(dto.name, func(t *testing.T) {
			for _, tag := range declaredJSONNames(t, dto.field) {
				if modelprovider.CredentialValueKey(tag) {
					t.Fatalf("%s declares the credential value field %q; only credentialRef is allowed", dto.name, tag)
				}
			}
		})
	}

	bodies := []struct {
		name   string
		body   string
		target any
	}{
		{
			name:   "agent apiKey",
			body:   `{"id":"agent_a","name":"A","apiKey":"sk-live-secret"}`,
			target: &AgentWriteRequest{},
		},
		{
			name:   "team apiKey",
			body:   `{"id":"team_a","name":"T","apiKey":"sk-live-secret"}`,
			target: &AgentTeamWriteRequest{},
		},
		{
			name:   "provider apiKey",
			body:   `{"id":"prov_a","typeId":"llm","name":"P","apiKey":"sk-live-secret"}`,
			target: &ModelProviderWriteRequest{},
		},
	}
	for _, body := range bodies {
		t.Run(body.name, func(t *testing.T) {
			err := decodeStrictJSON(strings.NewReader(body.body), body.target)
			var rejection *BodyRejection
			if !errors.As(err, &rejection) || rejection.Reason != BodyRejectionUnknownField || rejection.Field != "apiKey" {
				t.Fatalf("apiKey was not rejected as an unknown field: %v", err)
			}
		})
	}

	literal := sampleProviderRequest()
	literal.CredentialRef = "sk-live-not-a-reference"
	requireFieldError(t, literal.Validate(), "credentialRef", modelprovider.CodeInvalidCredentialRef)
}

// TestDecodeStrictJSONRejectsEmptyTrailingAndMalformed covers the remaining
// decoder rejections: an empty body, a second JSON value and broken JSON.
func TestDecodeStrictJSONRejectsEmptyTrailingAndMalformed(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "empty body", body: "", want: BodyRejectionEmpty},
		{name: "trailing value", body: `{"id":"a","name":"A"}{"id":"b"}`, want: BodyRejectionTrailingData},
		{name: "malformed", body: `{"id":`, want: BodyRejectionMalformed},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := decodeStrictJSON(strings.NewReader(testCase.body), &AgentWriteRequest{})
			var rejection *BodyRejection
			if !errors.As(err, &rejection) {
				t.Fatalf("error = %v, want *BodyRejection", err)
			}
			if rejection.Reason != testCase.want {
				t.Fatalf("reason = %q, want %q (%v)", rejection.Reason, testCase.want, err)
			}
		})
	}

	err := decodeStrictJSON(strings.NewReader(`{"id":"a","name":"A","defaultModel":{"providerId":5}}`), &AgentWriteRequest{})
	var rejection *BodyRejection
	if !errors.As(err, &rejection) || rejection.Reason != BodyRejectionFieldType {
		t.Fatalf("wrong-type field was not reported: %v", err)
	}
	if !strings.Contains(rejection.Field, "providerId") {
		t.Fatalf("field = %q, want it to name providerId", rejection.Field)
	}
}

// TestAgentWriteRequestValidate covers the agent field errors the acceptance
// list names (empty id, empty name, illegal avatar kind) plus the shared
// domain rules the DTO must not bypass (half model, inline credential).
func TestAgentWriteRequestValidate(t *testing.T) {
	valid := sampleAgentRequest()
	requireNoFieldError(t, valid.Validate())

	cases := []struct {
		name   string
		mutate func(*AgentWriteRequest)
		field  string
		code   string
	}{
		{
			name:   "blank name",
			mutate: func(request *AgentWriteRequest) { request.Name = "   " },
			field:  "name",
			code:   agentteam.CodeMissingName,
		},
		{
			name:   "illegal avatar kind",
			mutate: func(request *AgentWriteRequest) { request.Avatar.Kind = "photo" },
			field:  "avatar.kind",
			code:   agentteam.CodeInvalidAvatarKind,
		},
		{
			name:   "half model binding",
			mutate: func(request *AgentWriteRequest) { request.DefaultModel.ModelID = "" },
			field:  "defaultModel",
			code:   agentteam.CodeIncompleteModel,
		},
		{
			name: "inline credential in tuning",
			mutate: func(request *AgentWriteRequest) {
				request.DefaultModel.Tuning = map[string]any{"apiKey": "sk-live-secret"}
			},
			field: "defaultModel.tuning.apiKey",
			code:  agentteam.CodeInlineCredential,
		},
		{
			name:   "id outside the stable grammar",
			mutate: func(request *AgentWriteRequest) { request.ID = "not a stable id" },
			field:  "id",
			code:   agentteam.CodeInvalidID,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := sampleAgentRequest()
			testCase.mutate(&request)
			requireFieldError(t, request.Validate(), testCase.field, testCase.code)
		})
	}

	// Create from a name alone: the id is minted, the avatar is defaulted, and
	// the result is a valid agent. Update is the other way round — it must carry
	// the stored id and can never mint a second identity.
	nameless := sampleAgentRequest()
	nameless.ID = ""
	nameless.Avatar = agentteam.Avatar{}
	requireNoFieldError(t, nameless.Validate())
	created := nameless.NewAgent()
	if !strings.HasPrefix(created.ID, "agent_") {
		t.Fatalf("minted id = %q, want the agent_ prefix", created.ID)
	}
	if created.Avatar.Kind != agentteam.AvatarIdenticon || created.Avatar.Seed != created.ID {
		t.Fatalf("minted agent avatar = %#v, want the identicon seeded with the id", created.Avatar)
	}
	requireNoFieldError(t, created.Validate())
	requireFieldError(t, nameless.ValidateUpdate(), "id", agentteam.CodeMissingID)
}

// TestAgentWriteRequestValidateWithTools is the spec's save-time tool check: an
// unknown toolId fails while the reference is kept, and a nil registry behaves
// like the shape-only validation.
func TestAgentWriteRequestValidateWithTools(t *testing.T) {
	request := sampleAgentRequest()
	request.Tools = []agentteam.ToolGrant{{ToolID: "bash", Enabled: true}}
	requireNoFieldError(t, request.ValidateWithTools(nil))
	requireNoFieldError(t, request.ValidateWithTools(func(toolID string) bool { return toolID == "bash" }))
	requireFieldError(t,
		request.ValidateWithTools(func(toolID string) bool { return toolID == "read" }),
		"tools[0].toolId", agentteam.CodeToolNotFound,
	)
}

// TestAgentTeamWriteRequestValidate covers the team field errors the acceptance
// list names (missing captain, duplicate member), the captain/member overlap
// and the identity rules. A team with only a captain stays valid.
func TestAgentTeamWriteRequestValidate(t *testing.T) {
	onlyCaptain := AgentTeamWriteRequest{ID: "team_solo", Name: "单人队", Captain: agentteam.TeamMember{AgentID: "agent_a"}}
	requireNoFieldError(t, onlyCaptain.Validate())

	cases := []struct {
		name   string
		mutate func(*AgentTeamWriteRequest)
		field  string
		code   string
	}{
		{
			name:   "empty id",
			mutate: func(request *AgentTeamWriteRequest) { request.ID = "" },
			field:  "id",
			code:   agentteam.CodeMissingID,
		},
		{
			name:   "empty name",
			mutate: func(request *AgentTeamWriteRequest) { request.Name = "" },
			field:  "name",
			code:   agentteam.CodeMissingName,
		},
		{
			name:   "missing captain",
			mutate: func(request *AgentTeamWriteRequest) { request.Captain = agentteam.TeamMember{} },
			field:  "captain.agentId",
			code:   agentteam.CodeMissingCaptain,
		},
		{
			name:   "duplicate member",
			mutate: func(request *AgentTeamWriteRequest) { request.Members[1].AgentID = request.Members[0].AgentID },
			field:  "members[1].agentId",
			code:   agentteam.CodeDuplicateMember,
		},
		{
			name: "captain also a member",
			mutate: func(request *AgentTeamWriteRequest) {
				request.Members = append(request.Members, agentteam.TeamMember{AgentID: request.Captain.AgentID})
			},
			field: "members[2].agentId",
			code:  agentteam.CodeCaptainInMembers,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := sampleTeamRequest()
			testCase.mutate(&request)
			requireFieldError(t, request.Validate(), testCase.field, testCase.code)
		})
	}
}

// TestModelProviderWriteRequestValidate covers the provider field errors: empty
// id/name/type, unknown status, a literal credential, a bad endpoint and
// credentials hidden in the opaque config. A plugin-declared type outside the
// first six is accepted, because provider types are an open set.
func TestModelProviderWriteRequestValidate(t *testing.T) {
	valid := sampleProviderRequest()
	requireNoFieldError(t, valid.Validate())

	emptyRef := sampleProviderRequest()
	emptyRef.CredentialRef = ""
	requireNoFieldError(t, emptyRef.Validate())

	pluginType := sampleProviderRequest()
	pluginType.TypeID = "embedding"
	requireNoFieldError(t, pluginType.Validate())

	for _, ref := range []string{"secret:MODEL_KEY", "env:MODEL_KEY", "${MODEL_KEY}"} {
		request := sampleProviderRequest()
		request.CredentialRef = ref
		requireNoFieldError(t, request.Validate())
	}

	allowedConfig := sampleProviderRequest()
	allowedConfig.Config = map[string]any{"maxOutputTokens": 8192}
	requireNoFieldError(t, allowedConfig.Validate())

	cases := []struct {
		name   string
		mutate func(*ModelProviderWriteRequest)
		field  string
		code   string
	}{
		{
			name:   "empty id",
			mutate: func(request *ModelProviderWriteRequest) { request.ID = "" },
			field:  "id",
			code:   agentteam.CodeMissingID,
		},
		{
			name:   "empty name",
			mutate: func(request *ModelProviderWriteRequest) { request.Name = "" },
			field:  "name",
			code:   agentteam.CodeMissingName,
		},
		{
			name:   "missing type",
			mutate: func(request *ModelProviderWriteRequest) { request.TypeID = "" },
			field:  "typeId",
			code:   modelprovider.CodeMissingType,
		},
		{
			name:   "unknown status",
			mutate: func(request *ModelProviderWriteRequest) { request.Status = "retired" },
			field:  "status",
			code:   modelprovider.CodeInvalidProviderStatus,
		},
		{
			name:   "literal credential",
			mutate: func(request *ModelProviderWriteRequest) { request.CredentialRef = "sk-live-secret" },
			field:  "credentialRef",
			code:   modelprovider.CodeInvalidCredentialRef,
		},
		{
			name:   "non-http endpoint",
			mutate: func(request *ModelProviderWriteRequest) { request.Endpoint = "ftp://example.com" },
			field:  "endpoint",
			code:   modelprovider.CodeInvalidEndpoint,
		},
		{
			name: "credentials in the endpoint",
			mutate: func(request *ModelProviderWriteRequest) {
				request.Endpoint = "https://user:not-a-real-password@example.com"
			},
			field: "endpoint",
			code:  agentteam.CodeInlineCredential,
		},
		{
			name:   "credentials in config",
			mutate: func(request *ModelProviderWriteRequest) { request.Config = map[string]any{"apiKey": "sk-live-secret"} },
			field:  "config.apiKey",
			code:   agentteam.CodeInlineCredential,
		},
		{
			name: "credentials in a nested config map",
			mutate: func(request *ModelProviderWriteRequest) {
				request.Config = map[string]any{
					"headers": map[string]any{"Authorization": "Bearer sk-live-secret"},
				}
			},
			field: "config.headers.Authorization",
			code:  agentteam.CodeInlineCredential,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := sampleProviderRequest()
			testCase.mutate(&request)
			requireFieldError(t, request.Validate(), testCase.field, testCase.code)
		})
	}
}

// TestAgentTeamViewProjectsDerivedState checks that status, revision and hash
// are derived from the domain value rather than stored: an unsealed valid team,
// a sealed one, a team without a captain and an archived team all project the
// state the settings page and the session start mode read.
func TestAgentTeamViewProjectsDerivedState(t *testing.T) {
	valid := sampleTeamRequest()
	unsealed := NewAgentTeamView(testTenant(), *valid.Team())
	if unsealed.Status != agentteam.StatusValid {
		t.Fatalf("status = %q, want %q", unsealed.Status, agentteam.StatusValid)
	}
	if unsealed.Revision != 0 {
		t.Fatalf("unsealed revision = %d, want 0", unsealed.Revision)
	}
	if unsealed.Hash != valid.Team().Hash() {
		t.Fatalf("hash = %q, want the content hash %q", unsealed.Hash, valid.Team().Hash())
	}

	sealed := valid.Team()
	sealed.Seal()
	sealedView := NewAgentTeamView(testTenant(), *sealed)
	if sealedView.Status != agentteam.StatusValid {
		t.Fatalf("sealed status = %q, want valid (%v)", sealedView.Status, sealed.Validate())
	}
	if sealedView.Revision != 1 {
		t.Fatalf("sealed revision = %d, want 1", sealedView.Revision)
	}
	if sealedView.Hash != sealed.Hash() {
		t.Fatalf("sealed hash = %q, want %q", sealedView.Hash, sealed.Hash())
	}

	captainless := sampleTeamRequest()
	captainless.Captain = agentteam.TeamMember{}
	if view := NewAgentTeamView(testTenant(), *captainless.Team()); view.Status != agentteam.StatusInvalid {
		t.Fatalf("captainless status = %q, want invalid", view.Status)
	}

	archived := sampleTeamRequest()
	archivedTeam := archived.Team()
	archivedTeam.Archived = true
	if view := NewAgentTeamView(testTenant(), *archivedTeam); view.Status != agentteam.StatusInvalid || !view.Archived {
		t.Fatalf("archived view = %#v, want invalid and archived", view)
	}
}

// TestAgentViewProjectsResolvedAvatar checks that the response always carries a
// resolved avatar (kind and seed), so a client never has to re-derive the
// default identicon.
func TestAgentViewProjectsResolvedAvatar(t *testing.T) {
	agent := agentteam.Agent{ID: "agent_a", Name: "A"}
	view := NewAgentView(testTenant(), agent.Normalized())
	if view.Avatar.Kind != agentteam.AvatarIdenticon || view.Avatar.Seed != "agent_a" {
		t.Fatalf("avatar = %#v, want the identicon seeded with the id", view.Avatar)
	}
}

// declaredJSONNames lists the JSON names declared by a struct's exported fields
// (top level only). Embedded structs are flattened the way encoding/json does
// it, so a promoted tenant field would still be seen.
func declaredJSONNames(t *testing.T, structType reflect.Type) []string {
	t.Helper()
	if structType.Kind() != reflect.Struct {
		t.Fatalf("%s is not a struct", structType)
	}
	names := make([]string, 0, structType.NumField())
	for index := 0; index < structType.NumField(); index++ {
		field := structType.Field(index)
		if field.Anonymous {
			names = append(names, declaredJSONNames(t, field.Type)...)
			continue
		}
		if !field.IsExported() {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestModelProviderStatusAndHealthAreSeparateAxes pins the write/read
// roundtrip the two-axis split enables: the request's status lands in the
// domain's enablement field, the measured health stays its own field, and the
// view projects both without one overwriting the other.
func TestModelProviderStatusAndHealthAreSeparateAxes(t *testing.T) {
	request := sampleProviderRequest()
	request.Status = string(modelprovider.ProviderStatusDisabled)
	instance := request.ProviderInstance(testTenant())
	if instance.Status != modelprovider.ProviderStatusDisabled {
		t.Fatalf("domain status = %q, want disabled", instance.Status)
	}
	instance.Health = modelprovider.HealthStatus{State: modelprovider.HealthNeedsAuth, Message: "credential missing"}

	view := NewModelProviderView(testTenant(), instance)
	if view.Status != string(modelprovider.ProviderStatusDisabled) {
		t.Fatalf("view status = %q, want disabled", view.Status)
	}
	if view.Health != modelprovider.HealthNeedsAuth {
		t.Fatalf("view health = %q, want needs_auth", view.Health)
	}

	// An empty status is the active default, and a view of an unchecked
	// instance carries no health key.
	empty := sampleProviderRequest()
	empty.Status = ""
	unchecked := NewModelProviderView(testTenant(), empty.ProviderInstance(testTenant()))
	if unchecked.Status != string(modelprovider.ProviderStatusActive) {
		t.Fatalf("empty status projected %q, want active", unchecked.Status)
	}
	if unchecked.Health != "" {
		t.Fatalf("unchecked health projected %q, want empty", unchecked.Health)
	}
}

// TestModelProviderWriteRequestAcceptsWebSocketEndpoint proves the HTTP surface
// now accepts what the provider registry accepts: a speech provider's ws/wss
// endpoint is legal instead of being rejected by an http-only DTO check.
func TestModelProviderWriteRequestAcceptsWebSocketEndpoint(t *testing.T) {
	for _, endpoint := range []string{"ws://speech.example.com/v1", "wss://speech.example.com/v1"} {
		request := sampleProviderRequest()
		request.Endpoint = endpoint
		requireNoFieldError(t, request.Validate())
	}
}

// TestModelProviderWriteRequestRejectsCredentialsInArrays covers the shared
// scanner's array walk on the HTTP surface: a credential hidden in an array
// element is rejected exactly as it is in the domain.
func TestModelProviderWriteRequestRejectsCredentialsInArrays(t *testing.T) {
	request := sampleProviderRequest()
	request.Config = map[string]any{
		"headers": []any{map[string]any{"apiKey": "sk-live-secret"}},
	}
	requireFieldError(t, request.Validate(), "config.headers[0].apiKey", agentteam.CodeInlineCredential)
}

// TestModelProviderViewRoundTripsThroughTheDomain pins the view projection: a
// stored instance's models and parameter schema reference appear in the view.
func TestModelProviderViewRoundTripsThroughTheDomain(t *testing.T) {
	instance := sampleProviderRequest().ProviderInstance(testTenant())
	instance.PluginID = "keel.model"
	instance.Models = []modelprovider.ModelDescriptor{{
		ModelID:       "deepseek-v4.1",
		DisplayName:   "DeepSeek V4.1",
		Capabilities:  modelprovider.Capabilities{"reasoning"},
		ContextWindow: 128000,
		MaxOutput:     8192,
		Parameters:    modelprovider.ParameterSchema{Schema: modelprovider.SchemaRef{Ref: "#/definitions/model_parameters"}},
	}}
	view := NewModelProviderView(testTenant(), instance)
	if len(view.Models) != 1 {
		t.Fatalf("models = %v, want one", view.Models)
	}
	model := view.Models[0]
	if model.ModelID != "deepseek-v4.1" || model.ContextWindow != 128000 {
		t.Fatalf("model = %+v", model)
	}
	if model.ParameterSchemaRef != "#/definitions/model_parameters" {
		t.Fatalf("parameter schema ref = %q", model.ParameterSchemaRef)
	}
}
