package servicehost

// The product media seam: a configured image/video provider becomes a real
// host tool on every generation, the tool call reaches the provider wire and
// writes a layerfs reference, and the settings write path runs the decision
// Preflight before a document is stored.

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

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/layerfs"
	"github.com/jixoai/keel/internal/mediaadapter"
	"github.com/jixoai/keel/internal/mediawiring"
	"github.com/jixoai/keel/internal/modelcfg"
)

type mediaWire struct {
	mu       sync.Mutex
	requests []mediaWireRequest
	server   *httptest.Server
}

type mediaWireRequest struct {
	Path string
	Auth string
	Body string
}

func newMediaWire(t *testing.T) *mediaWire {
	t.Helper()
	wire := &mediaWire{}
	wire.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		wire.mu.Lock()
		wire.requests = append(wire.requests, mediaWireRequest{
			Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Body: string(body),
		})
		wire.mu.Unlock()
		if r.URL.Path != "/images/generations" {
			http.NotFound(w, r)
			return
		}
		png := base64.StdEncoding.EncodeToString([]byte("host-png"))
		_, _ = io.WriteString(w, `{"model":"img-1","data":[{"b64_json":"`+png+`","mime_type":"image/png"}],`+
			`"usage":{"input_tokens":2,"output_tokens":3,"cost":12.5}}`)
	}))
	t.Cleanup(wire.server.Close)
	return wire
}

func (w *mediaWire) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.requests)
}

// mediaModelsDocument is a llm selection (the runtime's default must resolve)
// plus one image provider and one decision provider.
func mediaModelsDocument(serverURL string) string {
	return strings.ReplaceAll(`{
	  "providers": [
	    {"id": "tenant-gw", "name": "tenant-gw",
	     "api": {"type": "openai-chat-completions", "baseUrl": "https://gw.example.invalid/v1"},
	     "access": {"type": "api-key", "env": "TENANT_GW_KEY"},
	     "modelIds": ["deepseek-v4.1-flash"]},
	    {"id": "img", "type": "image",
	     "api": {"type": "openai-chat-completions", "baseUrl": "SERVER"},
	     "access": {"type": "api-key", "env": "IMG_KEY"},
	     "modelIds": ["img-1"]},
	    {"id": "judge", "type": "decision",
	     "api": {"type": "openai-chat-completions", "baseUrl": "https://decision.example/v1"},
	     "access": {"type": "api-key", "env": "DECISION_KEY"},
	     "modelIds": ["judge-1"],
	     "decision": {"adapter": "typesafe"}}
	  ],
	  "selection": {"providerId": "tenant-gw", "modelId": "deepseek-v4.1-flash", "reasoningLevel": "high"}
	}`, "SERVER", serverURL)
}

func mediaProduct(t *testing.T, lookup map[string]string, artifacts *layerfs.Store) ProductOptions {
	t.Helper()
	credential := func(access modelcfg.Access) (string, string) {
		return ResolveAccessCredential(access, "", func(name string) (string, bool) {
			value, ok := lookup[name]
			return value, ok
		}, nil)
	}
	legacy := func(tenant host.TenantConfig) modelcfg.LegacyAgent {
		return modelcfg.LegacyAgent{
			Provider:  tenant.Agent.Provider,
			Model:     tenant.Agent.Model,
			BaseURL:   tenant.Agent.BaseURL,
			Protocol:  tenant.Agent.Protocol,
			APIKeyRef: tenant.Agent.APIKeyRef,
		}
	}
	return ProductOptions{
		MediaTools: func(tenant host.TenantConfig) ([]agentcore.AgentTool, error) {
			result := mediawiring.Build(mediawiring.Options{
				Models:     tenant.Models,
				Legacy:     legacy(tenant),
				Credential: credential,
				Artifacts:  artifacts,
			})
			return result.Tools, nil
		},
		ValidateMedia: func(tenant host.TenantConfig) []modelcfg.Issue {
			return mediawiring.ValidateModels(mediawiring.Options{
				Models:     tenant.Models,
				Legacy:     legacy(tenant),
				Credential: credential,
			})
		},
	}
}

func bootMediaRuntime(t *testing.T, document string, lookup map[string]string, artifacts *layerfs.Store) *Runtime {
	t.Helper()
	st := kernel.NewMemStore()
	k := kernel.New(st, kernel.Options{})
	rt, err := Boot(context.Background(), Config{
		Seams:     Seams{Committer: k, Reader: k, Leases: k, External: k, Runs: fakeMirror{}, Logger: discardLogger()},
		Tenant:    host.TenantConfig{Models: json.RawMessage(document)},
		LLM:       testLLM(lookup, ""),
		RuntimeID: "test-runtime",
		Product:   mediaProduct(t, lookup, artifacts),
	})
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })
	return rt
}

func runHostTool(t *testing.T, rt *Runtime, name, args, session string) agentcore.AgentToolCallOutcome {
	t.Helper()
	tool, ok := rt.Assembly().Tools().Get(name)
	if !ok {
		t.Fatalf("tool %q is not in the assembled host registry", name)
	}
	ctx := agentcore.WithToolRunScope(context.Background(), agentcore.ToolRunScope{
		TenantID: "tenant-1", SessionID: session,
	})
	return agenttool.RunToolCall(ctx, agentcore.AgentToolCall{
		ID: "call-" + name, Name: name, Arguments: json.RawMessage(args),
	}, agenttool.RunToolCallOptions{Tools: []agentcore.AgentTool{tool}})
}

func TestProductMediaToolServesARealHostCall(t *testing.T) {
	wire := newMediaWire(t)
	artifacts, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()
	document := mediaModelsDocument(wire.server.URL)
	rt := bootMediaRuntime(t, document, map[string]string{
		"TENANT_GW_KEY": "gw-key", "IMG_KEY": "img-key",
	}, artifacts)

	outcome := runHostTool(t, rt, "image_generate", `{"prompt":"a lighthouse"}`, "session-1")
	if outcome.IsError {
		t.Fatalf("image_generate failed: %+v", outcome.Result)
	}
	text := ""
	if len(outcome.Result.Content) == 1 {
		if block, ok := outcome.Result.Content[0].(agentcore.TextContent); ok {
			text = block.Text
		}
	}
	var output mediaadapter.ImageToolOutput
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		t.Fatalf("decode tool output: %v (%s)", err, text)
	}
	if !output.Success || len(output.Images) != 1 || output.Images[0].BlobID == "" {
		t.Fatalf("tool output = %+v", output)
	}
	if output.Usage.Images != 1 || output.Usage.Bytes != int64(len("host-png")) {
		t.Fatalf("usage = %+v", output.Usage)
	}
	if strings.Contains(text, "host-png") || strings.Contains(text, base64.StdEncoding.EncodeToString([]byte("host-png"))) {
		t.Fatalf("tool output inlined image bytes: %s", text)
	}
	hash, err := layerfs.ParseHash(output.Images[0].BlobID)
	if err != nil {
		t.Fatal(err)
	}
	object, err := artifacts.Open(hash)
	if err != nil {
		t.Fatalf("artifact missing from layerfs: %v", err)
	}
	data, _ := io.ReadAll(object)
	object.Close()
	if string(data) != "host-png" {
		t.Fatalf("artifact bytes = %q", data)
	}

	wire.mu.Lock()
	requests := append([]mediaWireRequest(nil), wire.requests...)
	wire.mu.Unlock()
	if len(requests) != 1 || requests[0].Auth != "Bearer img-key" {
		t.Fatalf("wire requests = %+v", requests)
	}
	if strings.Contains(requests[0].Body, "cost") || strings.Contains(requests[0].Body, "price") {
		t.Fatalf("request carried pricing: %s", requests[0].Body)
	}
}

func TestMediaToolsFollowTheGenerationOnSettingsWrite(t *testing.T) {
	first := newMediaWire(t)
	second := newMediaWire(t)
	artifacts, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()
	lookup := map[string]string{"TENANT_GW_KEY": "gw-key", "IMG_KEY": "img-key"}
	rt := bootMediaRuntime(t, mediaModelsDocument(first.server.URL), lookup, artifacts)

	if outcome := runHostTool(t, rt, "image_generate", `{"prompt":"before"}`, "session-1"); outcome.IsError {
		t.Fatalf("first generation tool failed: %+v", outcome.Result)
	}
	next := mediaModelsDocument(second.server.URL)
	if _, err := rt.ApplyModels(context.Background(), json.RawMessage(next)); err != nil {
		t.Fatalf("apply models: %v", err)
	}
	if outcome := runHostTool(t, rt, "image_generate", `{"prompt":"after"}`, "session-2"); outcome.IsError {
		t.Fatalf("next generation tool failed: %+v", outcome.Result)
	}
	if first.count() != 1 || second.count() != 1 {
		t.Fatalf("wire calls before/after = %d/%d, want 1/1", first.count(), second.count())
	}
}

func TestSettingsWriteRunsDecisionPreflight(t *testing.T) {
	wire := newMediaWire(t)
	artifacts, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()
	rt := bootMediaRuntime(t, mediaModelsDocument(wire.server.URL), map[string]string{
		"TENANT_GW_KEY": "gw-key", "IMG_KEY": "img-key",
	}, artifacts)

	withBinding := func(adapter, binding string) json.RawMessage {
		document := mediaModelsDocument(wire.server.URL)
		document = strings.Replace(document, `"decision": {"adapter": "typesafe"}`,
			`"decision": {"adapter": "`+adapter+`"`+binding+`}`, 1)
		return json.RawMessage(document)
	}
	valid := `,"binding":{"state":{"text":"a customer is angry"},"questions":{"urgent":{"type":"noul","instructions":"is this urgent?"}},"mode":"enforce"}`
	if _, issues, err := rt.ValidateModelsDocument(withBinding("typesafe", valid)); err != nil || len(issues) != 0 {
		t.Fatalf("valid decision binding: issues=%+v err=%v", issues, err)
	}

	// Cloudflare does not support box; the save path must return the field
	// error and must not apply the document.
	invalid := `,"binding":{"state":{"image_url":"data:image/png;base64,AAAA"},"questions":{"find":{"type":"box","instructions":"locate it"}}}`
	bad := withBinding("cloudflare", invalid)
	before := rt.Snapshot().Revision
	if _, issues, err := rt.ValidateModelsDocument(bad); err != nil {
		t.Fatalf("validate: %v", err)
	} else if !hasServiceIssue(issues, "providers[judge].decision.binding.questions.find.type") {
		t.Fatalf("cloudflare box issue missing: %+v", issues)
	}
	if rt.Snapshot().Revision != before {
		t.Fatal("a rejected document moved the serving revision")
	}
}

func hasServiceIssue(issues []modelcfg.Issue, field string) bool {
	for _, issue := range issues {
		if issue.Field == field {
			return true
		}
	}
	return false
}
