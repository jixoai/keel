package servicehost

// The run-level media gate: a real worker run asks for image_generate, the tool
// call reaches the tenant's image provider wire, the bytes land in layerfs, and
// the committed tool.result carries references + usage only. The chat model
// itself is a recording OpenAI-compatible fixture, so one run exercises the
// whole path: model selection -> provider stream -> tool call -> media adapter
// -> layerfs -> ledger.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/layerfs"
	"github.com/jixoai/keel/internal/loadout"
)

// runFixture is one OpenAI-compatible endpoint that answers the chat stream
// with a scripted image_generate tool call, and the image wire path with one
// inline PNG. It records every request (path, authorization, body).
type runFixture struct {
	mu       sync.Mutex
	requests []mediaWireRequest
	// toolName/toolArgs are the media tool the scripted model asks for.
	toolName string
	toolArgs string
	// decisionStatus, when non-zero, makes the decision endpoint fail with
	// that status. It is set before a run starts, never while one is in flight.
	decisionStatus int
	server         *httptest.Server
}

func newRunFixture(t *testing.T, toolName, toolArgs string) *runFixture {
	t.Helper()
	fixture := &runFixture{toolName: toolName, toolArgs: toolArgs}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fixture.mu.Lock()
		fixture.requests = append(fixture.requests, mediaWireRequest{
			Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Body: string(body),
		})
		decisionStatus := fixture.decisionStatus
		fixture.mu.Unlock()

		switch r.URL.Path {
		case "/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			// First model call: ask for image_generate. After the tool result,
			// answer with a short text so the run settles.
			if strings.Contains(string(body), `"tool_result"`) || strings.Contains(string(body), `"role":"tool"`) {
				_, _ = io.WriteString(w, "data: {\"id\":\"c2\",\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
				_, _ = io.WriteString(w, "data: {\"id\":\"c2\",\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
				return
			}
			_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-media-1\","+
				"\"function\":{\"name\":\""+fixture.toolName+"\",\"arguments\":\""+fixture.toolArgs+"\"}}]}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":1}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case "/images/generations":
			png := base64.StdEncoding.EncodeToString([]byte("run-png"))
			_, _ = io.WriteString(w, `{"model":"img-1","data":[{"b64_json":"`+png+`","mime_type":"image/png","width":4,"height":4}],`+
				`"usage":{"input_tokens":1,"output_tokens":2,"cost":0.25}}`)
		case "/videos/generations":
			// The provider answers with a URL; the adapter (not the model)
			// fetches the bytes and persists them.
			_, _ = io.WriteString(w, `{"model":"vid-1","data":[{"url":"`+fixture.server.URL+`/asset.mp4","mime_type":"video/mp4","duration_ms":1000}],`+
				`"usage":{"input_tokens":1,"cost":0.75}}`)
		case "/asset.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("run-mp4"))
		case "/audio/speech":
			w.Header().Set("Content-Type", "audio/wav")
			w.Header().Set("X-Audio-Duration-Ms", "1200")
			_, _ = w.Write([]byte("run-wav"))
		case "/audio/transcriptions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"text":"transcribed run","language":"en","duration":1.2,`+
				`"segments":[{"start":0,"end":0.6,"text":"transcribed"},{"start":0.6,"end":1.2,"text":"run"}],`+
				`"usage":{"input_tokens":2,"output_tokens":1,"cost":0.3}}`)
		case "/v1/systemone":
			if decisionStatus != 0 {
				w.WriteHeader(decisionStatus)
				_, _ = io.WriteString(w, `{"error":{"message":"decision upstream down"}}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"run-decision-1","model":"judge-1","answers":{"urgent":{"type":"noul","noul":0.93}},`+
				`"usage":{"input_tokens":7,"output_tokens":1,"cost":2.5}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *runFixture) failDecision(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decisionStatus = status
}

func (f *runFixture) paths() []mediaWireRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mediaWireRequest(nil), f.requests...)
}

// runModelsDocument points the llm and one media family at one recording origin
// with distinct credentials, so the test can prove each family authenticates
// with its own provider's key. family/model/credential name the media provider
// the scripted model tool-calls (image_generate or video_generate).
func runModelsDocument(serverURL, family, model, credential string) string {
	media := strings.ReplaceAll(`{"id": "media", "type": "FAMILY",
	     "api": {"type": "openai-chat-completions", "baseUrl": "SERVER"},
	     "access": {"type": "api-key", "env": "CRED"}, "modelIds": ["MODEL"]}`, "FAMILY", family)
	media = strings.ReplaceAll(media, "MODEL", model)
	media = strings.ReplaceAll(media, "CRED", credential)
	media = strings.ReplaceAll(media, "SERVER", serverURL)
	document := strings.ReplaceAll(`{
	  "providers": [
	    {"id": "chat", "api": {"type": "openai-chat-completions", "baseUrl": "SERVER"},
	     "access": {"type": "api-key", "env": "CHAT_KEY"}, "modelIds": ["chat-1"]},
	    MEDIA
	  ],
	  "selection": {"providerId": "chat", "modelId": "chat-1"}
	}`, "SERVER", serverURL)
	return strings.ReplaceAll(document, "MEDIA", media)
}

func TestWorkerRunCallsImageGenerateAndCommitsReferences(t *testing.T) {
	fixture := newRunFixture(t, "image_generate", "{\\\"prompt\\\":\\\"a lighthouse at dusk\\\"}")
	artifacts, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()

	lookup := map[string]string{"CHAT_KEY": "chat-key", "IMG_KEY": "img-key"}
	mem := kernel.NewMemStore()
	k := kernel.New(mem, kernel.Options{})
	tenant, session := uuid.New(), uuid.New()
	mem.CreateSession(tenant, session)
	rt, err := Boot(context.Background(), Config{
		Seams:      Seams{Committer: k, Reader: k, Leases: k, External: k, Runs: fakeMirror{}, Logger: discardLogger()},
		Tenant:     host.TenantConfig{Models: json.RawMessage(runModelsDocument(fixture.server.URL, "image", "img-1", "IMG_KEY"))},
		LLM:        testLLM(lookup, ""),
		Options:    loadout.Options{Worker: true, RunConcurrency: 1},
		RuntimeID:  "media-run-runtime",
		ToolAccess: allowAllToolAccess(),
		Product:    mediaProduct(t, lookup, artifacts),
	})
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })

	if _, err := k.Commit(context.Background(), kernel.CommitRequest{
		Scope:     kernel.Scope{TenantID: tenant, SessionID: session},
		Kind:      kernel.KindUserMessage,
		RequestID: "media-run-1",
		Payload:   json.RawMessage(`{"text":"draw a lighthouse"}`),
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
	var toolResult *kernel.Event
	for index := range events {
		if events[index].Kind == kernel.KindToolResult {
			toolResult = &events[index]
		}
	}
	if toolResult == nil {
		t.Fatalf("the run committed no tool.result: %+v", events)
	}
	var payload struct {
		ToolName string                  `json:"toolName"`
		IsError  bool                    `json:"isError"`
		Content  []struct{ Text string } `json:"content"`
		Details  map[string]any          `json:"details"`
	}
	if err := json.Unmarshal(toolResult.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ToolName != "image_generate" || payload.IsError {
		t.Fatalf("tool.result = %+v", payload)
	}
	if len(payload.Content) != 1 {
		t.Fatalf("tool.result content blocks = %d", len(payload.Content))
	}
	text := payload.Content[0].Text
	if strings.Contains(text, "run-png") || strings.Contains(text, "b64") {
		t.Fatalf("tool.result inlined image bytes: %s", text)
	}
	var output struct {
		Success bool `json:"success"`
		Images  []struct {
			BlobID string `json:"blobId"`
		} `json:"images"`
		Usage struct {
			Images int   `json:"images"`
			Bytes  int64 `json:"bytes"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		t.Fatalf("decode tool result: %v (%s)", err, text)
	}
	if !output.Success || len(output.Images) != 1 || output.Images[0].BlobID == "" {
		t.Fatalf("tool result = %s", text)
	}
	if output.Usage.Images != 1 || output.Usage.Bytes != int64(len("run-png")) {
		t.Fatalf("usage = %+v", output.Usage)
	}
	hash, err := layerfs.ParseHash(output.Images[0].BlobID)
	if err != nil {
		t.Fatal(err)
	}
	object, err := artifacts.Open(hash)
	if err != nil {
		t.Fatalf("layerfs object missing: %v", err)
	}
	data, _ := io.ReadAll(object)
	object.Close()
	if string(data) != "run-png" {
		t.Fatalf("layerfs bytes = %q", data)
	}
	rawDetails, _ := json.Marshal(payload.Details)
	for _, want := range []string{`"media"`, `"family":"image"`, `"blobId":"` + output.Images[0].BlobID + `"`} {
		if !strings.Contains(string(rawDetails), want) {
			t.Fatalf("tool.result details missing %s: %s", want, rawDetails)
		}
	}
	for _, forbidden := range []string{"cost", "price", "run-png"} {
		if strings.Contains(string(rawDetails), forbidden) {
			t.Fatalf("tool.result details leaked %q: %s", forbidden, rawDetails)
		}
	}

	// One credential per family: the chat call carried CHAT_KEY, the image
	// call IMG_KEY, and no request carried a price field.
	var chatAuth, imageAuth string
	for _, request := range fixture.paths() {
		switch request.Path {
		case "/chat/completions":
			chatAuth = request.Auth
		case "/images/generations":
			imageAuth = request.Auth
		}
	}
	if chatAuth != "Bearer chat-key" {
		t.Fatalf("chat auth = %q", chatAuth)
	}
	if imageAuth != "Bearer img-key" {
		t.Fatalf("image auth = %q", imageAuth)
	}
	for _, request := range fixture.paths() {
		if strings.Contains(request.Body, "0.25") || strings.Contains(request.Body, "price") {
			t.Fatalf("request carried pricing: %s", request.Body)
		}
	}

	// The tool result is a layerfs reference, not a platform blob reference.
	if !strings.Contains(text, `"blobId":"`+output.Images[0].BlobID+`"`) {
		t.Fatalf("tool result does not carry the blobId reference: %s", text)
	}
}

// TestWorkerRunFailsClosedWithoutAMediaCredential runs the same scripted model
// with the image provider's credential missing: the tool still exists, the
// call fails with the typed missing_credentials error, no image request leaves
// the process, and the run itself settles.
func TestWorkerRunFailsClosedWithoutAMediaCredential(t *testing.T) {
	fixture := newRunFixture(t, "image_generate", "{\\\"prompt\\\":\\\"a lighthouse at dusk\\\"}")
	artifacts, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()

	lookup := map[string]string{"CHAT_KEY": "chat-key"}
	mem := kernel.NewMemStore()
	k := kernel.New(mem, kernel.Options{})
	tenant, session := uuid.New(), uuid.New()
	mem.CreateSession(tenant, session)
	rt, err := Boot(context.Background(), Config{
		Seams:      Seams{Committer: k, Reader: k, Leases: k, External: k, Runs: fakeMirror{}, Logger: discardLogger()},
		Tenant:     host.TenantConfig{Models: json.RawMessage(runModelsDocument(fixture.server.URL, "image", "img-1", "IMG_KEY"))},
		LLM:        testLLM(lookup, ""),
		Options:    loadout.Options{Worker: true, RunConcurrency: 1},
		RuntimeID:  "media-run-runtime",
		ToolAccess: allowAllToolAccess(),
		Product:    mediaProduct(t, lookup, artifacts),
	})
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })

	if _, err := k.Commit(context.Background(), kernel.CommitRequest{
		Scope:     kernel.Scope{TenantID: tenant, SessionID: session},
		Kind:      kernel.KindUserMessage,
		RequestID: "media-run-2",
		Payload:   json.RawMessage(`{"text":"draw a lighthouse"}`),
	}); err != nil {
		t.Fatalf("commit input: %v", err)
	}
	mem.EnqueueRun(tenant, session)
	pin := waitForRunSettled(t, mem, tenant, session, 15*time.Second)
	if pin.Status != kernel.RunStatusDone {
		t.Fatalf("run status = %q, want done", pin.Status)
	}
	events, err := mem.LoadEvents(context.Background(), tenant, session, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	var resultPayload string
	for _, event := range events {
		if event.Kind == kernel.KindToolResult {
			resultPayload = string(event.Payload)
		}
	}
	if resultPayload == "" {
		t.Fatalf("no tool.result committed: %+v", events)
	}
	if !strings.Contains(resultPayload, "missing_credentials") {
		t.Fatalf("tool.result did not fail closed: %s", resultPayload)
	}
	if strings.Contains(resultPayload, "run-png") {
		t.Fatalf("a failed call carried bytes: %s", resultPayload)
	}
	for _, request := range fixture.paths() {
		if request.Path == "/images/generations" {
			t.Fatalf("an unauthenticated image request left the process: %+v", request)
		}
	}
}

// TestWorkerRunCallsVideoGenerateAndCommitsReferences is the video mirror of the
// image run: the provider answers with a URL, the adapter fetches the bytes
// without forwarding the credential, and the committed result is a layerfs
// reference plus objective usage.
func TestWorkerRunCallsVideoGenerateAndCommitsReferences(t *testing.T) {
	fixture := newRunFixture(t, "video_generate", "{\\\"prompt\\\":\\\"a paper boat\\\",\\\"duration\\\":4}")
	artifacts, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()

	lookup := map[string]string{"CHAT_KEY": "chat-key", "VID_KEY": "vid-key"}
	mem := kernel.NewMemStore()
	k := kernel.New(mem, kernel.Options{})
	tenant, session := uuid.New(), uuid.New()
	mem.CreateSession(tenant, session)
	rt, err := Boot(context.Background(), Config{
		Seams:      Seams{Committer: k, Reader: k, Leases: k, External: k, Runs: fakeMirror{}, Logger: discardLogger()},
		Tenant:     host.TenantConfig{Models: json.RawMessage(runModelsDocument(fixture.server.URL, "video", "vid-1", "VID_KEY"))},
		LLM:        testLLM(lookup, ""),
		Options:    loadout.Options{Worker: true, RunConcurrency: 1},
		RuntimeID:  "media-run-runtime",
		ToolAccess: allowAllToolAccess(),
		Product:    mediaProduct(t, lookup, artifacts),
	})
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })

	if _, err := k.Commit(context.Background(), kernel.CommitRequest{
		Scope:     kernel.Scope{TenantID: tenant, SessionID: session},
		Kind:      kernel.KindUserMessage,
		RequestID: "media-run-3",
		Payload:   json.RawMessage(`{"text":"animate a paper boat"}`),
	}); err != nil {
		t.Fatalf("commit input: %v", err)
	}
	mem.EnqueueRun(tenant, session)
	pin := waitForRunSettled(t, mem, tenant, session, 15*time.Second)
	if pin.Status != kernel.RunStatusDone {
		t.Fatalf("run status = %q, want done", pin.Status)
	}
	events, err := mem.LoadEvents(context.Background(), tenant, session, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	var resultText string
	for _, event := range events {
		if event.Kind != kernel.KindToolResult {
			continue
		}
		var payload struct {
			ToolName string                  `json:"toolName"`
			IsError  bool                    `json:"isError"`
			Content  []struct{ Text string } `json:"content"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.ToolName == "video_generate" && len(payload.Content) == 1 {
			resultText = payload.Content[0].Text
			if payload.IsError {
				t.Fatalf("video tool failed: %s", resultText)
			}
		}
	}
	if resultText == "" {
		t.Fatalf("no video_generate tool.result committed: %+v", events)
	}
	var output struct {
		Success bool `json:"success"`
		Videos  []struct {
			BlobID string `json:"blobId"`
			Mime   string `json:"mime"`
		} `json:"videos"`
		Usage struct {
			Videos int   `json:"videos"`
			Bytes  int64 `json:"bytes"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(resultText), &output); err != nil {
		t.Fatalf("decode video result: %v (%s)", err, resultText)
	}
	if !output.Success || len(output.Videos) != 1 || output.Videos[0].BlobID == "" {
		t.Fatalf("video result = %s", resultText)
	}
	if output.Videos[0].Mime != "video/mp4" || output.Usage.Videos != 1 || output.Usage.Bytes != int64(len("run-mp4")) {
		t.Fatalf("video result facts = %+v", output)
	}
	if strings.Contains(resultText, "run-mp4") {
		t.Fatalf("video result inlined bytes: %s", resultText)
	}
	hash, err := layerfs.ParseHash(output.Videos[0].BlobID)
	if err != nil {
		t.Fatal(err)
	}
	object, err := artifacts.Open(hash)
	if err != nil {
		t.Fatalf("layerfs object missing: %v", err)
	}
	data, _ := io.ReadAll(object)
	object.Close()
	if string(data) != "run-mp4" {
		t.Fatalf("layerfs bytes = %q", data)
	}

	var videoAuth, assetAuth string
	for _, request := range fixture.paths() {
		switch request.Path {
		case "/videos/generations":
			videoAuth = request.Auth
		case "/asset.mp4":
			assetAuth = request.Auth
		}
	}
	if videoAuth != "Bearer vid-key" {
		t.Fatalf("video auth = %q", videoAuth)
	}
	if assetAuth != "" {
		t.Fatalf("the asset fetch forwarded a credential: %q", assetAuth)
	}
}
