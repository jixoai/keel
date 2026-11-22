// MRTR executor tests (P5.2/P5.3): elicitation through the decision primitive,
// sampling bounds, roots/unknown refusals, and the legacy server-request path.

package mcphost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/mcp"
)

// fakeDecisions is the decision bridge the executor opens elicitations on. It
// records the opener document and lets the test answer it.
type fakeDecisions struct {
	mu      sync.Mutex
	opened  []harnessext.OpenDecisionRequest
	answer  *harnessext.DecisionAnswer
	status  harnessext.DecisionStatus
	decided chan struct{}
	once    sync.Once
}

func newFakeDecisions() *fakeDecisions {
	return &fakeDecisions{decided: make(chan struct{}), status: harnessext.DecisionAnswered}
}

func (f *fakeDecisions) Open(_ context.Context, request harnessext.OpenDecisionRequest) (harnessext.Decision, bool, error) {
	f.mu.Lock()
	f.opened = append(f.opened, request)
	f.mu.Unlock()
	id := harnessext.DecisionIDFor(request.TenantID, request.SessionID, request.RequestID)
	return harnessext.Decision{
		DecisionID:    id.String(),
		TenantID:      request.TenantID.String(),
		SessionID:     request.SessionID.String(),
		RequestID:     request.RequestID,
		Kind:          request.Kind,
		Prompt:        request.Prompt,
		Options:       request.Options,
		AllowFreeText: request.AllowFreeText,
		Status:        harnessext.DecisionOpen,
	}, true, nil
}

func (f *fakeDecisions) Wait(ctx context.Context, _, _, _ uuid.UUID) (harnessext.Decision, error) {
	select {
	case <-f.decided:
	case <-ctx.Done():
		return harnessext.Decision{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return harnessext.Decision{Status: f.status, Answer: f.answer}, nil
}

func (f *fakeDecisions) answerWith(optionID, text string) {
	f.mu.Lock()
	f.answer = &harnessext.DecisionAnswer{OptionID: optionID, Text: text, At: time.Now()}
	f.mu.Unlock()
	f.once.Do(func() { close(f.decided) })
}

func (f *fakeDecisions) openedRequests() []harnessext.OpenDecisionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]harnessext.OpenDecisionRequest(nil), f.opened...)
}

// recordingAuditor collects the executor's audit rows.
type recordingAuditor struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (a *recordingAuditor) RecordMCPApp(_ context.Context, event AuditEvent) {
	a.mu.Lock()
	a.events = append(a.events, event)
	a.mu.Unlock()
}

func (a *recordingAuditor) all() []AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AuditEvent(nil), a.events...)
}

// fakeSampler records sampling requests and answers them deterministically.
type fakeSampler struct {
	mu       sync.Mutex
	requests []SamplingRequest
	err      error
}

func (s *fakeSampler) Sample(_ context.Context, request SamplingRequest) (SamplingResult, error) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	err := s.err
	s.mu.Unlock()
	if err != nil {
		return SamplingResult{}, err
	}
	return SamplingResult{Role: "assistant", Text: "sampled answer", Model: "fixture-model", StopReason: "endTurn"}, nil
}

func (s *fakeSampler) seen() []SamplingRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SamplingRequest(nil), s.requests...)
}

func mcpTestScopeContext(t *testing.T) (context.Context, uuid.UUID, uuid.UUID) {
	t.Helper()
	tenant, session := uuid.New(), uuid.New()
	ctx := agentcore.WithToolRunScope(context.Background(), agentcore.ToolRunScope{
		TenantID: tenant.String(), SessionID: session.String(),
	})
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	t.Cleanup(cancel)
	return ctx, tenant, session
}

func TestMRTRElicitationProjectsSchemaOntoDecision(t *testing.T) {
	decisions := newFakeDecisions()
	executor := NewMRTR(MRTROptions{SamplingMode: SamplingOff})
	executor.SetDecisions(decisions)
	ctx, _, _ := mcpTestScopeContext(t)

	round := mcp.InputRequiredResult{
		InputRequests: map[string]mcp.InputRequest{
			"login": {
				Method: "elicitation/create",
				Params: json.RawMessage(`{"message":"Pick an account","requestedSchema":{
					"type":"object","properties":{"user":{"type":"string","title":"Account","enum":["alice","bob"]}}}}`),
			},
		},
		RequestState: json.RawMessage(`"opaque-state"`),
	}
	type outcome struct {
		responses map[string]json.RawMessage
		err       error
	}
	done := make(chan outcome, 1)
	go func() {
		responses, err := executor.HandleInputRequests(ctx, "fixture", round, &mcp.InputRequestState{})
		done <- outcome{responses: responses, err: err}
	}()
	if !waitForCondition(t, 2*time.Second, func() bool { return len(decisions.openedRequests()) == 1 }) {
		t.Fatal("the executor never opened an elicitation decision")
	}
	opened := decisions.openedRequests()[0]
	if opened.Kind != "mcp.elicitation" || !strings.Contains(opened.Prompt, "Pick an account") {
		t.Fatalf("decision = %+v", opened)
	}
	options := map[string]harnessext.DecisionOption{}
	for _, option := range opened.Options {
		options[option.ID] = option
	}
	for _, want := range []string{"v0_0", "v0_1", "decline", "cancel"} {
		if _, ok := options[want]; !ok {
			t.Fatalf("options = %+v, missing %q", opened.Options, want)
		}
	}
	if options["v0_0"].Label != "alice" || options["v0_1"].Label != "bob" {
		t.Fatalf("enum labels = %+v", opened.Options)
	}
	if opened.AllowFreeText {
		t.Fatal("an enum-only schema must not allow free text")
	}
	decisions.answerWith("v0_1", "")
	result := <-done
	if result.err != nil {
		t.Fatalf("handle: %v", result.err)
	}
	if got := string(result.responses["login"]); got != `{"action":"accept","content":{"user":"bob"}}` {
		t.Fatalf("elicit result = %s", got)
	}
	// The request id is deterministic: a retried round finds the same decision.
	if !strings.HasPrefix(opened.RequestID, "mcp-mrtr:") {
		t.Fatalf("request id = %q", opened.RequestID)
	}
}

func TestMRTRElicitationURLModeNeverOpensTheLink(t *testing.T) {
	decisions := newFakeDecisions()
	executor := NewMRTR(MRTROptions{SamplingMode: SamplingOff})
	executor.SetDecisions(decisions)
	ctx, _, _ := mcpTestScopeContext(t)
	round := mcp.InputRequiredResult{InputRequests: map[string]mcp.InputRequest{
		"auth": {
			Method: "elicitation/create",
			Params: json.RawMessage(`{"mode":"url","message":"Sign in","url":"https://example.com/oauth?state=1"}`),
		},
	}}
	done := make(chan map[string]json.RawMessage, 1)
	go func() {
		responses, _ := executor.HandleInputRequests(ctx, "fixture", round, &mcp.InputRequestState{})
		done <- responses
	}()
	if !waitForCondition(t, 2*time.Second, func() bool { return len(decisions.openedRequests()) == 1 }) {
		t.Fatal("no decision opened")
	}
	opened := decisions.openedRequests()[0]
	var foundURL bool
	for _, option := range opened.Options {
		if strings.Contains(option.Description, "https://example.com/oauth") {
			foundURL = true
		}
		if option.ID == "open" {
			t.Fatal("URL mode must not offer an automatic open action")
		}
	}
	if !foundURL {
		t.Fatalf("the URL is not shown to the user: %+v", opened.Options)
	}
	decisions.answerWith("accept", "")
	responses := <-done
	if got := string(responses["auth"]); got != `{"action":"accept"}` {
		t.Fatalf("URL elicitation result = %s, want consent without content", got)
	}
}

func TestMRTRElicitationWithoutScopeIsRefused(t *testing.T) {
	executor := NewMRTR(MRTROptions{SamplingMode: SamplingOff})
	executor.SetDecisions(newFakeDecisions())
	round := mcp.InputRequiredResult{InputRequests: map[string]mcp.InputRequest{
		"login": {Method: "elicitation/create", Params: json.RawMessage(`{"message":"hi"}`)},
	}}
	_, err := executor.HandleInputRequests(context.Background(), "fixture", round, &mcp.InputRequestState{})
	var rpcErr *mcp.McpError
	if !errors.As(err, &rpcErr) || rpcErr.Code != mcp.CodeInternalError {
		t.Fatalf("err = %v, want an internal-error refusal", err)
	}
}

// TestMRTRElicitationDisabledIsMethodNotFound pins the frozen spec: a request
// type this deployment does not implement is -32601, not an internal failure.
// The no-scope case above stays -32603 (the method exists; the context is bad).
func TestMRTRElicitationDisabledIsMethodNotFound(t *testing.T) {
	ctx, _, _ := mcpTestScopeContext(t)
	round := mcp.InputRequiredResult{InputRequests: map[string]mcp.InputRequest{
		"login": {Method: "elicitation/create", Params: json.RawMessage(`{"message":"hi"}`)},
	}}
	for name, executor := range map[string]*MRTR{
		"disabled":           NewMRTR(MRTROptions{SamplingMode: SamplingOff, DisableElicitation: true}),
		"no decision bridge": NewMRTR(MRTROptions{SamplingMode: SamplingOff}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := executor.HandleInputRequests(ctx, "fixture", round, &mcp.InputRequestState{})
			var rpcErr *mcp.McpError
			if !errors.As(err, &rpcErr) || rpcErr.Code != mcp.CodeMethodNotFound {
				t.Fatalf("err = %v, want -32601", err)
			}
		})
	}
}

// elicitationSchemaParams renders one requestedSchema fixture.
func elicitationSchemaParams(t *testing.T, properties map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"message":         "pick one",
		"requestedSchema": map[string]any{"type": "object", "properties": properties},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestMRTRElicitationSchemaBounds pins the P2 bound: an oversized schema is
// invalid params and never opens a decision.
func TestMRTRElicitationSchemaBounds(t *testing.T) {
	manyProperties := map[string]any{}
	for index := 0; index < maxElicitationProperties+1; index++ {
		manyProperties[fmt.Sprintf("field_%02d", index)] = map[string]any{"type": "string"}
	}
	tooManyOptions := map[string]any{"choice": map[string]any{
		"type": "string",
		"enum": func() []string {
			values := make([]string, 0, maxElicitationEnumOptions+1)
			for index := 0; index < maxElicitationEnumOptions+1; index++ {
				values = append(values, fmt.Sprintf("v%d", index))
			}
			return values
		}(),
	}}
	oversizedLiteral := map[string]any{"choice": map[string]any{
		"type": "string",
		"enum": []string{strings.Repeat("x", maxElicitationEnumLiteralBytes+1)},
	}}

	cases := []struct {
		name       string
		properties map[string]any
	}{
		{"too many properties", manyProperties},
		{"too many enum options", tooManyOptions},
		{"enum literal too long", oversizedLiteral},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			decisions := newFakeDecisions()
			executor := NewMRTR(MRTROptions{SamplingMode: SamplingOff})
			executor.SetDecisions(decisions)
			ctx, _, _ := mcpTestScopeContext(t)
			round := mcp.InputRequiredResult{InputRequests: map[string]mcp.InputRequest{
				"login": {Method: "elicitation/create", Params: elicitationSchemaParams(t, test.properties)},
			}}
			_, err := executor.HandleInputRequests(ctx, "fixture", round, &mcp.InputRequestState{})
			var rpcErr *mcp.McpError
			if !errors.As(err, &rpcErr) || rpcErr.Code != mcp.CodeInvalidParams {
				t.Fatalf("err = %v, want -32602", err)
			}
			if opened := decisions.openedRequests(); len(opened) != 0 {
				t.Fatalf("an oversized schema opened %d decisions", len(opened))
			}
		})
	}

	// The exact bounds are accepted: 32 properties and 64 enum options together.
	atBounds := map[string]any{}
	enumValues := make([]string, 0, maxElicitationEnumOptions)
	for index := 0; index < maxElicitationEnumOptions; index++ {
		enumValues = append(enumValues, fmt.Sprintf("v%d", index))
	}
	atBounds["choice"] = map[string]any{"type": "string", "enum": enumValues}
	for index := 1; index < maxElicitationProperties; index++ {
		atBounds[fmt.Sprintf("field_%02d", index)] = map[string]any{"type": "string"}
	}
	decisions := newFakeDecisions()
	executor := NewMRTR(MRTROptions{SamplingMode: SamplingOff})
	executor.SetDecisions(decisions)
	ctx, _, _ := mcpTestScopeContext(t)
	round := mcp.InputRequiredResult{InputRequests: map[string]mcp.InputRequest{
		"login": {Method: "elicitation/create", Params: elicitationSchemaParams(t, atBounds)},
	}}
	done := make(chan error, 1)
	go func() {
		_, err := executor.HandleInputRequests(ctx, "fixture", round, &mcp.InputRequestState{})
		done <- err
	}()
	if !waitForCondition(t, 2*time.Second, func() bool { return len(decisions.openedRequests()) == 1 }) {
		t.Fatal("the at-bounds schema did not open a decision")
	}
	decisions.answerWith("cancel", "")
	if err := <-done; err != nil {
		t.Fatalf("at-bounds schema: %v", err)
	}
}

// TestElicitationAnswerTerminalOptionBeatsText pins the cancel+text fix: a
// terminal option is the answer even when the UI left typed text behind.
func TestElicitationAnswerTerminalOptionBeatsText(t *testing.T) {
	plan := elicitationPlan{
		freeTextField: "note",
		values: map[string]elicitationValue{
			"v0_0": {field: "choice", value: "alice"},
		},
	}
	cases := []struct {
		name       string
		optionID   string
		text       string
		wantAction string
		want       map[string]any
	}{
		{"decline ignores text", "decline", "stale text", "decline", nil},
		{"cancel ignores text", "cancel", "stale text", "cancel", nil},
		{"projected option ignores text", "v0_0", "stale text", "accept", map[string]any{"choice": "alice"}},
		{"empty option with text is free text", "", "typed", "accept", map[string]any{"note": "typed"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			decision := harnessext.Decision{
				Status: harnessext.DecisionAnswered,
				Answer: &harnessext.DecisionAnswer{OptionID: test.optionID, Text: test.text},
			}
			action, content := plan.answer(decision)
			if action != test.wantAction {
				t.Fatalf("action = %q, want %q", action, test.wantAction)
			}
			if !reflect.DeepEqual(content, test.want) {
				t.Fatalf("content = %#v, want %#v", content, test.want)
			}
		})
	}
}

func TestMRTRSamplingBudgetMaxTokensAndSessionGate(t *testing.T) {
	sampler := &fakeSampler{}
	executor := NewMRTR(MRTROptions{SamplingMode: SamplingSession, SamplingMaxTokens: 4096, SamplingPerCall: 3})
	executor.SetSampler(sampler)
	ctx, _, _ := mcpTestScopeContext(t)
	state := &mcp.InputRequestState{}
	round := func(maxTokens int) mcp.InputRequiredResult {
		return mcp.InputRequiredResult{InputRequests: map[string]mcp.InputRequest{
			"cap": {
				Method: "sampling/createMessage",
				Params: json.RawMessage(`{"messages":[{"role":"user","content":{"type":"text","text":"hi"}}],
					"maxTokens":` + jsonNumber(maxTokens) + `}`),
			},
		}}
	}
	for i := 0; i < 3; i++ {
		responses, err := executor.HandleInputRequests(ctx, "fixture", round(999999), state)
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		var answer struct {
			Role    string         `json:"role"`
			Content map[string]any `json:"content"`
			Model   string         `json:"model"`
		}
		if err := json.Unmarshal(responses["cap"], &answer); err != nil {
			t.Fatalf("round %d result: %v", i, err)
		}
		if answer.Role != "assistant" || answer.Content["text"] != "sampled answer" || answer.Model != "fixture-model" {
			t.Fatalf("round %d result = %+v", i, answer)
		}
	}
	seen := sampler.seen()
	if len(seen) != 3 {
		t.Fatalf("sampler calls = %d, want 3", len(seen))
	}
	for _, request := range seen {
		if request.MaxTokens != 4096 {
			t.Fatalf("maxTokens = %d, want the 4096 cap", request.MaxTokens)
		}
		if len(request.Messages) != 1 || request.Messages[0].Text != "hi" {
			t.Fatalf("messages = %+v", request.Messages)
		}
	}
	// The fourth request is over the per-call budget: refused, no model call.
	_, err := executor.HandleInputRequests(ctx, "fixture", round(100), state)
	var rpcErr *mcp.McpError
	if !errors.As(err, &rpcErr) || rpcErr.Code != mcp.CodeInternalError {
		t.Fatalf("over-budget err = %v", err)
	}
	if len(sampler.seen()) != 3 {
		t.Fatalf("over-budget call reached the sampler: %d", len(sampler.seen()))
	}

	// Session mode requires the call scope.
	executor2 := NewMRTR(MRTROptions{SamplingMode: SamplingSession})
	executor2.SetSampler(sampler)
	_, err = executor2.HandleInputRequests(context.Background(), "fixture", round(100), &mcp.InputRequestState{})
	if !errors.As(err, &rpcErr) || rpcErr.Code != mcp.CodeMethodNotFound {
		t.Fatalf("sampling outside a session err = %v", err)
	}

	// Off refuses even inside a session.
	executor3 := NewMRTR(MRTROptions{SamplingMode: SamplingOff})
	executor3.SetSampler(sampler)
	_, err = executor3.HandleInputRequests(ctx, "fixture", round(100), &mcp.InputRequestState{})
	if !errors.As(err, &rpcErr) || rpcErr.Code != mcp.CodeMethodNotFound {
		t.Fatalf("disabled sampling err = %v", err)
	}
}

func TestMRTRRootsAndUnknownMethodsAreRefusedAndAudited(t *testing.T) {
	auditor := &recordingAuditor{}
	executor := NewMRTR(MRTROptions{SamplingMode: SamplingOff, Auditor: auditor})
	ctx, _, _ := mcpTestScopeContext(t)
	for _, method := range []string{"roots/list", "custom/unknown"} {
		round := mcp.InputRequiredResult{InputRequests: map[string]mcp.InputRequest{
			"x": {Method: method, Params: json.RawMessage(`{}`)},
		}}
		_, err := executor.HandleInputRequests(ctx, "fixture", round, &mcp.InputRequestState{})
		var rpcErr *mcp.McpError
		if !errors.As(err, &rpcErr) || rpcErr.Code != mcp.CodeMethodNotFound {
			t.Fatalf("%s err = %v, want -32601", method, err)
		}
	}
	events := auditor.all()
	if len(events) != 2 {
		t.Fatalf("audit rows = %+v, want 2", events)
	}
	for _, event := range events {
		if event.Result != "refused" || !strings.HasPrefix(event.Action, "mrtr.") {
			t.Fatalf("audit event = %+v", event)
		}
	}
}

func TestMRTRSamplingFailureIsReportedNotFabricated(t *testing.T) {
	sampler := &fakeSampler{err: errors.New("upstream unavailable")}
	executor := NewMRTR(MRTROptions{SamplingMode: SamplingSession})
	executor.SetSampler(sampler)
	ctx, _, _ := mcpTestScopeContext(t)
	round := mcp.InputRequiredResult{InputRequests: map[string]mcp.InputRequest{
		"s": {Method: "sampling/createMessage", Params: json.RawMessage(`{"messages":[{"role":"user","content":{"type":"text","text":"hi"}}]}`)},
	}}
	_, err := executor.HandleInputRequests(ctx, "fixture", round, &mcp.InputRequestState{})
	var rpcErr *mcp.McpError
	if !errors.As(err, &rpcErr) || rpcErr.Code != mcp.CodeInternalError || !strings.Contains(rpcErr.Message, "sampling failed") {
		t.Fatalf("err = %v, want a sampling failure", err)
	}
}

func TestMRTRLegacyServerRequestUsesTheSameExecutor(t *testing.T) {
	sampler := &fakeSampler{}
	executor := NewMRTR(MRTROptions{SamplingMode: SamplingSession})
	executor.SetSampler(sampler)
	ctx, _, _ := mcpTestScopeContext(t)
	request := &mcp.Request{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      mcp.NumberID(7),
		Method:  "sampling/createMessage",
		Params:  json.RawMessage(`{"messages":[{"role":"user","content":{"type":"text","text":"legacy hi"}}],"maxTokens":64}`),
	}
	message, err := executor.HandleLegacyServerRequest(ctx, "fixture", request)
	if err != nil {
		t.Fatalf("legacy request: %v", err)
	}
	response, ok := message.(*mcp.SuccessResponse)
	if !ok || response.ID != mcp.NumberID(7) {
		t.Fatalf("response = %#v", message)
	}
	if !strings.Contains(string(response.Result), `"text":"sampled answer"`) {
		t.Fatalf("result = %s", response.Result)
	}

	// A refused legacy method is an error response, never silence.
	refusal := &mcp.Request{JSONRPC: mcp.JSONRPCVersion, ID: mcp.NumberID(8), Method: "roots/list"}
	if _, err := executor.HandleLegacyServerRequest(ctx, "fixture", refusal); err == nil {
		t.Fatal("roots/list was not refused")
	}
}

func TestMRTRClientCapabilitiesMatchTheConfiguration(t *testing.T) {
	off := NewMRTR(MRTROptions{SamplingMode: SamplingOff})
	offCaps := off.ClientCapabilities()
	if offCaps.Sampling != nil {
		t.Fatalf("sampling declared while off")
	}
	if offCaps.Elicitation == nil {
		t.Fatalf("elicitation not declared while enabled")
	}
	if offCaps.Roots != nil {
		t.Fatalf("roots declared (it is always refused)")
	}
	session := NewMRTR(MRTROptions{SamplingMode: SamplingSession})
	if session.ClientCapabilities().Sampling == nil {
		t.Fatalf("sampling not declared in session mode")
	}
	disabled := NewMRTR(MRTROptions{SamplingMode: SamplingSession, DisableElicitation: true})
	if disabled.ClientCapabilities().Elicitation != nil {
		t.Fatalf("elicitation declared while disabled")
	}
}

func TestSamplingModeFromEnvironment(t *testing.T) {
	env := func(value string) func(string) string {
		return func(key string) string {
			if key == "KEEL_MCP_SAMPLING" {
				return value
			}
			return ""
		}
	}
	if got := SamplingModeFromEnvironment(env("")); got != SamplingSession {
		t.Fatalf("missing = %q", got)
	}
	if got := SamplingModeFromEnvironment(env("off")); got != SamplingOff {
		t.Fatalf("off = %q", got)
	}
	if got := SamplingModeFromEnvironment(env("ON")); got != SamplingOn {
		t.Fatalf("on = %q", got)
	}
	if got := SamplingModeFromEnvironment(env("bogus")); got != SamplingOff {
		t.Fatalf("invalid = %q, want off (fail closed)", got)
	}
}

// jsonNumber renders an integer for an inline JSON fixture.
func jsonNumber(value int) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
