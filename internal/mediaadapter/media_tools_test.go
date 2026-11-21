package mediaadapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/layerfs"
)

type fakeDecisionExecutor struct {
	mu       sync.Mutex
	requests []DecisionRequest
	outcome  *DecisionOutcome
	err      error
	panic    any
}

func (e *fakeDecisionExecutor) Decide(_ context.Context, req DecisionRequest) (*DecisionOutcome, error) {
	e.mu.Lock()
	e.requests = append(e.requests, req)
	e.mu.Unlock()
	if e.panic != nil {
		panic(e.panic)
	}
	return e.outcome, e.err
}

func (e *fakeDecisionExecutor) last() DecisionRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.requests[len(e.requests)-1]
}

func TestSpeechAndDecisionToolsRegisterInTheToolRegistry(t *testing.T) {
	registry := agenttool.NewToolRegistry()
	for _, tool := range []agentcore.AgentTool{
		NewImageTool(nil), NewVideoTool(nil), NewSpeechTTSTool(nil),
		NewSpeechSTTTool(nil, nil, 0), NewDecisionTool(nil, DecisionPolicy{}, nil, nil),
	} {
		if err := registry.Register(tool); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	names := registry.Names()
	for _, want := range []string{"image_generate", "video_generate", "speech_tts", "speech_stt", "decision_ask"} {
		if !containsString(names, want) {
			t.Fatalf("registry names = %v, missing %s", names, want)
		}
	}
	valid := map[string]string{
		"speech_tts":   `{"text":"hello","voice":"alloy","language":"en","format":"wav"}`,
		"speech_stt":   `{"blob_id":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","mime":"audio/wav"}`,
		"decision_ask": `{"state":{"ticket":"x"},"questions":{"urgent":{"type":"noul","instructions":"is it urgent?"}}}`,
	}
	for name, args := range valid {
		if fields := registry.Validate(name, json.RawMessage(args)); len(fields) != 0 {
			t.Fatalf("%s rejected valid args: %+v", name, fields)
		}
	}
	for name, args := range map[string]string{
		"speech_tts":   `{"text":""}`,
		"speech_stt":   `{}`,
		"decision_ask": `{"mode":"shadow"}`,
	} {
		if fields := registry.Validate(name, json.RawMessage(args)); len(fields) == 0 {
			t.Fatalf("%s accepted invalid args: %s", name, args)
		}
	}
}

func TestSpeechToolProviderPanicBecomesTypedFailureAndAnotherToolStillWorks(t *testing.T) {
	runCtx := agentcore.WithToolRunScope(context.Background(), agentcore.ToolRunScope{
		TenantID: "tenant-1", SessionID: "session-panic",
	})
	broken := NewSpeechTTSTool(&SpeechAdapter{
		Provider: &fakeSpeechProvider{panicValue: "speech backend exploded"},
		Store:    &recordingStore{},
	})
	result, err := broken.Execute(runCtx, "call-1", json.RawMessage(`{"text":"hello"}`), nil)
	if err != nil {
		t.Fatalf("panic escaped as a Go error: %v", err)
	}
	if result.IsError == nil || !*result.IsError {
		t.Fatalf("panic result is not an error: %+v", result)
	}
	text := contentText(t, result.Content)
	if !strings.Contains(text, string(KindProviderUnavailable)) || !strings.Contains(text, "speech backend exploded") {
		t.Fatalf("panic result = %s", text)
	}
	if strings.Contains(text, "speech-key") {
		t.Fatalf("panic result leaked a credential: %s", text)
	}

	healthy := NewSpeechTTSTool(&SpeechAdapter{
		Provider: &fakeSpeechProvider{audio: SpeechAudio{Data: []byte("wav"), Mime: "audio/wav"}},
		Store:    &recordingStore{},
	})
	ok, err := healthy.Execute(runCtx, "call-2", json.RawMessage(`{"text":"hello"}`), nil)
	if err != nil || (ok.IsError != nil && *ok.IsError) {
		t.Fatalf("healthy speech tool after panic: result=%+v err=%v", ok, err)
	}
}

func TestDecisionToolPanicBecomesTypedFailure(t *testing.T) {
	runCtx := agentcore.WithToolRunScope(context.Background(), agentcore.ToolRunScope{
		TenantID: "tenant-1", SessionID: "session-panic",
	})
	tool := NewDecisionTool(&fakeDecisionExecutor{panic: "decision backend exploded"}, DefaultDecisionPolicy(), nil, nil)
	result, err := tool.Execute(runCtx, "call-1", json.RawMessage(`{"state":{"ticket":"x"},"questions":{"a":{"type":"noul","instructions":"is it?"}}}`), nil)
	if err != nil {
		t.Fatalf("panic escaped as a Go error: %v", err)
	}
	if result.IsError == nil || !*result.IsError {
		t.Fatalf("panic result is not an error: %+v", result)
	}
	text := contentText(t, result.Content)
	if !strings.Contains(text, string(KindProviderUnavailable)) || !strings.Contains(text, "decision backend exploded") {
		t.Fatalf("panic result = %s", text)
	}
}

func TestSpeechToolDetailsCarryOnlyReferencesAndObjectiveUsage(t *testing.T) {
	runCtx := agentcore.WithToolRunScope(context.Background(), agentcore.ToolRunScope{
		TenantID: "tenant-1", SessionID: "session-details",
	})
	tool := NewSpeechTTSTool(&SpeechAdapter{
		Provider: &fakeSpeechProvider{audio: SpeechAudio{Data: []byte("wav-bytes"), Mime: "audio/wav", DurationMillis: 900}},
		Store:    &recordingStore{},
	})
	result, err := tool.Execute(runCtx, "call-1", json.RawMessage(`{"text":"hello"}`), nil)
	if err != nil || (result.IsError != nil && *result.IsError) {
		t.Fatalf("speech tool: result=%+v err=%v", result, err)
	}
	raw, err := json.Marshal(result.Details)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"media"`, `"family":"tts"`, `"blobId":"sha256:`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("details missing %s: %s", want, raw)
		}
	}
	for _, forbidden := range []string{"cost", "price", "currency", "wav-bytes"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("details leaked %q: %s", forbidden, raw)
		}
	}
}

func TestDecisionToolUsesSavedBindingButNeverModelPolicy(t *testing.T) {
	runCtx := agentcore.WithToolRunScope(context.Background(), agentcore.ToolRunScope{
		TenantID: "tenant-1", SessionID: "session-decision",
	})
	executor := &fakeDecisionExecutor{outcome: &DecisionOutcome{
		Mode: DecisionModeShadow, Shadow: true, Failure: "upstream down", Retryable: true,
		Usage: Usage{Questions: 1},
	}}
	savedQuestions := map[string]DecisionQuestion{
		"urgent": {Type: QuestionNoul, Instructions: json.RawMessage(`"is it urgent?"`)},
	}
	tool := NewDecisionTool(executor, DecisionPolicy{Mode: DecisionModeShadow}, json.RawMessage(`{"ticket":"saved"}`), savedQuestions)

	result, err := tool.Execute(runCtx, "call-1", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError != nil && *result.IsError {
		t.Fatalf("shadow failure must not be a tool error: %+v", result)
	}
	var output DecisionToolOutput
	if err := json.Unmarshal([]byte(contentText(t, result.Content)), &output); err != nil {
		t.Fatal(err)
	}
	if !output.Success || output.Answered || !output.Shadow || output.Failure == "" || len(output.Answers) != 0 {
		t.Fatalf("shadow output = %+v", output)
	}
	request := executor.last()
	if request.Policy.Mode != DecisionModeShadow || request.SessionID != "session-decision" || request.User != "tenant-1" {
		t.Fatalf("request = %+v", request)
	}
	if string(request.State) != `{"ticket":"saved"}` || len(request.Questions) != 1 {
		t.Fatalf("saved binding was not used: %+v", request)
	}
	details, _ := json.Marshal(result.Details)
	if strings.Contains(string(details), "saved") || strings.Contains(string(details), "urgent") {
		t.Fatalf("decision details leaked state/questions: %s", details)
	}
}

func TestDecisionToolEnforceFailureIsTypedAndFabricatesNothing(t *testing.T) {
	runCtx := agentcore.WithToolRunScope(context.Background(), agentcore.ToolRunScope{
		TenantID: "tenant-1", SessionID: "session-decision",
	})
	executor := &fakeDecisionExecutor{err: &Error{
		Family: FamilyDecision, Op: "decide", Kind: KindProviderUnavailable,
		Message: "decision unavailable (fail-closed): upstream down", Retryable: true,
	}}
	tool := NewDecisionTool(executor, DefaultDecisionPolicy(), nil, nil)
	result, err := tool.Execute(runCtx, "call-1",
		json.RawMessage(`{"state":{"ticket":"x"},"questions":{"urgent":{"type":"noul","instructions":"is it urgent?"}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError == nil || !*result.IsError {
		t.Fatalf("enforce failure is not a tool error: %+v", result)
	}
	text := contentText(t, result.Content)
	if !strings.Contains(text, "decision unavailable (fail-closed)") || !strings.Contains(text, string(KindProviderUnavailable)) {
		t.Fatalf("failure = %s", text)
	}
	for _, forbidden := range []string{`"noul":0`, `"choice":"unknown"`, `"confidence":0`, "answers"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("failure fabricated an answer (%s): %s", forbidden, text)
		}
	}
}

type panickingStore struct{}

func (panickingStore) Put(context.Context, ArtifactInput) (Artifact, error) {
	panic("artifact store exploded")
}

func TestSpeechToolRecoversASeamPanicOutsideTheProviderGuard(t *testing.T) {
	runCtx := agentcore.WithToolRunScope(context.Background(), agentcore.ToolRunScope{
		TenantID: "tenant-1", SessionID: "session-panic",
	})
	tool := NewSpeechTTSTool(&SpeechAdapter{
		Provider: &fakeSpeechProvider{audio: SpeechAudio{Data: []byte("wav"), Mime: "audio/wav"}},
		Store:    panickingStore{},
	})
	result, err := tool.Execute(runCtx, "call-1", json.RawMessage(`{"text":"hello"}`), nil)
	if err != nil {
		t.Fatalf("seam panic escaped as a Go error: %v", err)
	}
	if result.IsError == nil || !*result.IsError {
		t.Fatalf("seam panic result is not an error: %+v", result)
	}
	text := contentText(t, result.Content)
	if !strings.Contains(text, string(KindProviderUnavailable)) || !strings.Contains(text, "artifact store exploded") {
		t.Fatalf("seam panic result = %s", text)
	}
}

func TestDecisionToolRejectsAnEmptyOutcome(t *testing.T) {
	runCtx := agentcore.WithToolRunScope(context.Background(), agentcore.ToolRunScope{
		TenantID: "tenant-1", SessionID: "session-decision",
	})
	tool := NewDecisionTool(&fakeDecisionExecutor{outcome: &DecisionOutcome{Mode: DecisionModeEnforce}},
		DefaultDecisionPolicy(), nil, nil)
	result, err := tool.Execute(runCtx, "call-1",
		json.RawMessage(`{"state":{"ticket":"x"},"questions":{"urgent":{"type":"noul","instructions":"is it urgent?"}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError == nil || !*result.IsError {
		t.Fatalf("empty outcome was reported as success: %+v", result)
	}
	text := contentText(t, result.Content)
	if !strings.Contains(text, "no answers and no failure") || strings.Contains(text, "answers\":") {
		t.Fatalf("empty outcome failure = %s", text)
	}
}

func TestSpeechSTTToolRejectsHeaderInjectionBeforeTheWire(t *testing.T) {
	store, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hash, _, err := store.PutBytes([]byte("clip"))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"should not be reached"}`))
	}))
	defer server.Close()
	provider, err := NewHTTPSpeechProvider(HTTPSpeechConfig{BaseURL: server.URL, APIKey: "k", Model: "stt-1"})
	if err != nil {
		t.Fatal(err)
	}
	tool := NewSpeechSTTTool(&SpeechAdapter{
		Provider: provider,
		Store:    &LayerFSStore{Store: store},
	}, store, 0)
	runCtx := agentcore.WithToolRunScope(context.Background(), agentcore.ToolRunScope{
		TenantID: "tenant-1", SessionID: "session-stt",
	})
	result, err := tool.Execute(runCtx, "call-1", json.RawMessage(`{
	  "blob_id": "`+hash.Prefixed()+`",
	  "filename": "memo.wav\r\nX-Evil: 1",
	  "mime": "audio/wav"
	}`), nil)
	if err != nil {
		t.Fatalf("tool returned a Go error: %v", err)
	}
	if result.IsError == nil || !*result.IsError {
		t.Fatalf("injection was accepted: %+v", result)
	}
	text := contentText(t, result.Content)
	if !strings.Contains(text, "invalid_request") || !strings.Contains(text, "filename") {
		t.Fatalf("failure = %s", text)
	}
	if strings.ContainsAny(text, "\r\n\x00\t") {
		t.Fatalf("tool result carried a control character: %q", text)
	}
	if calls != 0 {
		t.Fatalf("an injected filename reached the wire %d times", calls)
	}
}
