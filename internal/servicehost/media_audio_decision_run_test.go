package servicehost

// Run-level speech and decision coverage: the scripted chat model asks for a
// speech or decision tool, the tenant-configured provider answers over the real
// HTTP wire, generated audio lands in layerfs, and the committed tool.result
// carries references/transcript/answers only. Decision failures prove the
// enforce/shadow split without synthesizing an answer.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/layerfs"
	"github.com/jixoai/keel/internal/loadout"
	"github.com/jixoai/keel/internal/mediaadapter"
)

func bootScriptedRun(t *testing.T, document string, lookup map[string]string, artifacts *layerfs.Store) (*Runtime, *kernel.Kernel, *kernel.MemStore, uuid.UUID, uuid.UUID) {
	t.Helper()
	mem := kernel.NewMemStore()
	k := kernel.New(mem, kernel.Options{})
	tenant, session := uuid.New(), uuid.New()
	mem.CreateSession(tenant, session)
	rt, err := Boot(context.Background(), Config{
		Seams:      Seams{Committer: k, Reader: k, Leases: k, External: k, Runs: fakeMirror{}, Logger: discardLogger()},
		Tenant:     host.TenantConfig{Models: json.RawMessage(document)},
		LLM:        testLLM(lookup, ""),
		Options:    loadout.Options{Worker: true, RunConcurrency: 1},
		RuntimeID:  "media-speech-decision-run",
		ToolAccess: allowAllToolAccess(),
		Product:    mediaProduct(t, lookup, artifacts),
	})
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })
	return rt, k, mem, tenant, session
}

func runScripted(t *testing.T, mem *kernel.MemStore, k *kernel.Kernel, tenant, session uuid.UUID, requestID string) []kernel.Event {
	t.Helper()
	if _, err := k.Commit(context.Background(), kernel.CommitRequest{
		Scope:     kernel.Scope{TenantID: tenant, SessionID: session},
		Kind:      kernel.KindUserMessage,
		RequestID: requestID,
		Payload:   json.RawMessage(`{"text":"run the scripted media tool"}`),
	}); err != nil {
		t.Fatalf("commit input: %v", err)
	}
	mem.EnqueueRun(tenant, session)
	pin := waitForRunSettled(t, mem, tenant, session, 15*time.Second)
	if pin.Status != kernel.RunStatusDone {
		t.Fatalf("run status = %q, want done (%+v)", pin.Status, pin)
	}
	events, err := mem.LoadEvents(context.Background(), tenant, session, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

type committedToolResult struct {
	ToolName string                  `json:"toolName"`
	IsError  bool                    `json:"isError"`
	Content  []struct{ Text string } `json:"content"`
	Details  map[string]any          `json:"details"`
}

func lastCommittedToolResult(t *testing.T, events []kernel.Event) committedToolResult {
	t.Helper()
	var found *kernel.Event
	for index := range events {
		if events[index].Kind == kernel.KindToolResult {
			found = &events[index]
		}
	}
	if found == nil {
		t.Fatalf("the run committed no tool.result: %+v", events)
	}
	var payload committedToolResult
	if err := json.Unmarshal(found.Payload, &payload); err != nil {
		t.Fatalf("decode tool.result: %v", err)
	}
	return payload
}

func TestWorkerRunCallsSpeechTTSAndCommitsAnAudioReference(t *testing.T) {
	fixture := newRunFixture(t, "speech_tts", "{\\\"text\\\":\\\"speak this\\\",\\\"voice\\\":\\\"alloy\\\",\\\"language\\\":\\\"en\\\"}")
	artifacts, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()
	lookup := map[string]string{"CHAT_KEY": "chat-key", "SPEECH_KEY": "speech-key"}
	_, k, mem, tenant, session := bootScriptedRun(t,
		runModelsDocument(fixture.server.URL, "tts", "tts-1", "SPEECH_KEY"), lookup, artifacts)
	events := runScripted(t, mem, k, tenant, session, "speech-tts-run-1")

	payload := lastCommittedToolResult(t, events)
	if payload.ToolName != "speech_tts" || payload.IsError {
		t.Fatalf("tool.result = %+v", payload)
	}
	if len(payload.Content) != 1 {
		t.Fatalf("content blocks = %d", len(payload.Content))
	}
	var output mediaadapter.SpeechTTSToolOutput
	if err := json.Unmarshal([]byte(payload.Content[0].Text), &output); err != nil {
		t.Fatalf("decode output: %v (%s)", err, payload.Content[0].Text)
	}
	if !output.Success || output.Audio.BlobID == "" || output.Audio.Mime != "audio/wav" {
		t.Fatalf("output = %+v", output)
	}
	if output.Usage.InputCharacters != 10 || output.Usage.AudioBytes != int64(len("run-wav")) || output.Usage.AudioMillis != 1200 {
		t.Fatalf("usage = %+v", output.Usage)
	}
	hash, err := layerfs.ParseHash(output.Audio.BlobID)
	if err != nil {
		t.Fatal(err)
	}
	object, err := artifacts.Open(hash)
	if err != nil {
		t.Fatalf("audio object missing: %v", err)
	}
	data, _ := io.ReadAll(object)
	object.Close()
	if string(data) != "run-wav" {
		t.Fatalf("layerfs audio = %q", data)
	}

	rawDetails, _ := json.Marshal(payload.Details)
	for _, want := range []string{`"media"`, `"family":"tts"`, `"blobId":"` + output.Audio.BlobID + `"`} {
		if !strings.Contains(string(rawDetails), want) {
			t.Fatalf("tool.result details missing %s: %s", want, rawDetails)
		}
	}
	for _, forbidden := range []string{"cost", "price", "run-wav"} {
		if strings.Contains(string(rawDetails), forbidden) || strings.Contains(payload.Content[0].Text, forbidden) {
			t.Fatalf("speech result leaked %q (details=%s content=%s)", forbidden, rawDetails, payload.Content[0].Text)
		}
	}

	var auth string
	for _, request := range fixture.paths() {
		if request.Path == "/audio/speech" {
			auth = request.Auth
			for _, want := range []string{`"model":"tts-1"`, `"input":"speak this"`, `"voice":"alloy"`} {
				if !strings.Contains(request.Body, want) {
					t.Fatalf("speech wire body missing %s: %s", want, request.Body)
				}
			}
		}
	}
	if auth != "Bearer speech-key" {
		t.Fatalf("speech auth = %q", auth)
	}
}

func TestWorkerRunCallsSpeechSTTWithALayerFSInput(t *testing.T) {
	artifacts, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()
	hash, _, err := artifacts.PutBytes([]byte("run-wav"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := newRunFixture(t, "speech_stt",
		"{\\\"blob_id\\\":\\\""+hash.Prefixed()+"\\\",\\\"mime\\\":\\\"audio/wav\\\",\\\"language\\\":\\\"en\\\"}")
	lookup := map[string]string{"CHAT_KEY": "chat-key", "SPEECH_KEY": "speech-key"}
	_, k, mem, tenant, session := bootScriptedRun(t,
		runModelsDocument(fixture.server.URL, "stt", "whisper-1", "SPEECH_KEY"), lookup, artifacts)
	events := runScripted(t, mem, k, tenant, session, "speech-stt-run-1")

	payload := lastCommittedToolResult(t, events)
	if payload.ToolName != "speech_stt" || payload.IsError {
		t.Fatalf("tool.result = %+v", payload)
	}
	var output mediaadapter.SpeechSTTToolOutput
	if err := json.Unmarshal([]byte(payload.Content[0].Text), &output); err != nil {
		t.Fatalf("decode output: %v (%s)", err, payload.Content[0].Text)
	}
	if !output.Success || output.Transcript.Text != "transcribed run" || len(output.Transcript.Segments) != 2 {
		t.Fatalf("output = %+v", output)
	}
	if output.Transcript.Segments[1].StartMillis != 600 || output.Transcript.Segments[1].EndMillis != 1200 {
		t.Fatalf("segments = %+v", output.Transcript.Segments)
	}
	if output.Audio == nil || output.Audio.BlobID != hash.Prefixed() {
		t.Fatalf("input audio reference = %+v", output.Audio)
	}
	if output.Transcript.Usage.TranscriptCharacters != 15 || output.Transcript.Usage.AudioBytes != int64(len("run-wav")) {
		t.Fatalf("usage = %+v", output.Transcript.Usage)
	}
	rawDetails, _ := json.Marshal(payload.Details)
	if !strings.Contains(string(rawDetails), `"family":"stt"`) || !strings.Contains(string(rawDetails), `"blobId":"`+hash.Prefixed()+`"`) {
		t.Fatalf("tool.result details = %s", rawDetails)
	}
	for _, forbidden := range []string{"cost", "price", "run-wav"} {
		if strings.Contains(string(rawDetails), forbidden) || strings.Contains(payload.Content[0].Text, forbidden) {
			t.Fatalf("transcript result leaked %q (details=%s content=%s)", forbidden, rawDetails, payload.Content[0].Text)
		}
	}

	var found bool
	for _, request := range fixture.paths() {
		if request.Path != "/audio/transcriptions" {
			continue
		}
		found = true
		if request.Auth != "Bearer speech-key" {
			t.Fatalf("stt auth = %q", request.Auth)
		}
		if !strings.Contains(request.Body, "run-wav") || !strings.Contains(request.Body, `name="file"`) {
			t.Fatalf("stt multipart body = %s", request.Body)
		}
	}
	if !found {
		t.Fatal("no transcription request reached the wire")
	}
}

func runDecisionDocument(serverURL, mode string) string {
	document := strings.ReplaceAll(`{
	  "providers": [
	    {"id": "chat", "api": {"type": "openai-chat-completions", "baseUrl": "SERVER"},
	     "access": {"type": "api-key", "env": "CHAT_KEY"}, "modelIds": ["chat-1"]},
	    {"id": "judge", "type": "decision",
	     "api": {"type": "openai-chat-completions", "baseUrl": "SERVER"},
	     "access": {"type": "api-key", "env": "DECISION_KEY"}, "modelIds": ["judge-1"],
	     "decision": {"adapter": "typesafe", "binding": {
	       "state": {"ticket": "customer is angry"},
	       "questions": {"urgent": {"type": "noul", "instructions": "is this urgent?"}},
	       "mode": "MODE"}}}
	  ],
	  "selection": {"providerId": "chat", "modelId": "chat-1"}
	}`, "SERVER", serverURL)
	return strings.ReplaceAll(document, "MODE", mode)
}

func TestWorkerRunCallsDecisionAskAndCommitsTypedAnswers(t *testing.T) {
	fixture := newRunFixture(t, "decision_ask", "{}")
	artifacts, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()
	lookup := map[string]string{"CHAT_KEY": "chat-key", "DECISION_KEY": "decision-key"}
	_, k, mem, tenant, session := bootScriptedRun(t, runDecisionDocument(fixture.server.URL, "enforce"), lookup, artifacts)
	events := runScripted(t, mem, k, tenant, session, "decision-run-1")

	payload := lastCommittedToolResult(t, events)
	if payload.ToolName != "decision_ask" || payload.IsError {
		t.Fatalf("tool.result = %+v", payload)
	}
	var output mediaadapter.DecisionToolOutput
	if err := json.Unmarshal([]byte(payload.Content[0].Text), &output); err != nil {
		t.Fatalf("decode output: %v (%s)", err, payload.Content[0].Text)
	}
	if !output.Success || !output.Answered || output.Mode != "enforce" || output.Shadow {
		t.Fatalf("output = %+v", output)
	}
	if len(output.Answers) != 1 || !strings.Contains(string(output.Answers["urgent"]), `"noul":0.93`) {
		t.Fatalf("answers = %+v", output.Answers)
	}
	if strings.Contains(string(output.Answers["urgent"]), "confidence") {
		t.Fatalf("noul answer gained confidence: %s", output.Answers["urgent"])
	}
	if output.Usage.InputTokens != 7 || output.Usage.OutputTokens != 1 || output.Usage.Questions != 1 {
		t.Fatalf("usage = %+v", output.Usage)
	}
	raw, _ := json.Marshal(output)
	for _, forbidden := range []string{"cost", "2.5", "decision-key"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("decision output leaked %q: %s", forbidden, raw)
		}
	}

	var auth string
	for _, request := range fixture.paths() {
		if request.Path == "/v1/systemone" {
			auth = request.Auth
			for _, want := range []string{`"state":{"ticket":"customer is angry"}`, `"questions":{"urgent"`, `"model":"judge-1"`} {
				if !strings.Contains(request.Body, want) {
					t.Fatalf("decision wire body missing %s: %s", want, request.Body)
				}
			}
		}
	}
	if auth != "Bearer decision-key" {
		t.Fatalf("decision auth = %q", auth)
	}
}

func TestWorkerRunDecisionEnforceFailureIsFailClosed(t *testing.T) {
	fixture := newRunFixture(t, "decision_ask", "{}")
	fixture.failDecision(http.StatusBadGateway)
	artifacts, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()
	lookup := map[string]string{"CHAT_KEY": "chat-key", "DECISION_KEY": "decision-key"}
	_, k, mem, tenant, session := bootScriptedRun(t, runDecisionDocument(fixture.server.URL, "enforce"), lookup, artifacts)
	events := runScripted(t, mem, k, tenant, session, "decision-run-fail-1")

	payload := lastCommittedToolResult(t, events)
	if payload.ToolName != "decision_ask" || !payload.IsError {
		t.Fatalf("enforce failure did not block: %+v", payload)
	}
	text := payload.Content[0].Text
	if !strings.Contains(text, "decision unavailable (fail-closed)") || !strings.Contains(text, string(mediaadapter.KindProviderUnavailable)) {
		t.Fatalf("failure = %s", text)
	}
	for _, forbidden := range []string{`"noul":0`, `"choice":"unknown"`, `"confidence":0`, `"answers"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("enforce failure fabricated an answer (%s): %s", forbidden, text)
		}
	}
}

func TestWorkerRunDecisionShadowFailureIsObservableAndNonBlocking(t *testing.T) {
	fixture := newRunFixture(t, "decision_ask", "{}")
	fixture.failDecision(http.StatusBadGateway)
	artifacts, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()
	lookup := map[string]string{"CHAT_KEY": "chat-key", "DECISION_KEY": "decision-key"}
	_, k, mem, tenant, session := bootScriptedRun(t, runDecisionDocument(fixture.server.URL, "shadow"), lookup, artifacts)
	events := runScripted(t, mem, k, tenant, session, "decision-run-shadow-1")

	payload := lastCommittedToolResult(t, events)
	if payload.ToolName != "decision_ask" || payload.IsError {
		t.Fatalf("shadow failure became a tool error: %+v", payload)
	}
	var output mediaadapter.DecisionToolOutput
	if err := json.Unmarshal([]byte(payload.Content[0].Text), &output); err != nil {
		t.Fatalf("decode output: %v (%s)", err, payload.Content[0].Text)
	}
	if !output.Success || output.Answered || !output.Shadow || output.Failure == "" || len(output.Answers) != 0 {
		t.Fatalf("shadow output = %+v", output)
	}
	if output.Usage.Questions != 1 {
		t.Fatalf("shadow usage = %+v", output.Usage)
	}
	if strings.Contains(payload.Content[0].Text, `"noul"`) {
		t.Fatalf("shadow failure fabricated an answer: %s", payload.Content[0].Text)
	}
}
