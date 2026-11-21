package mediaadapter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
)

func TestMediaToolsCompileInTheToolRegistry(t *testing.T) {
	registry := agenttool.NewToolRegistry()
	if err := registry.Register(NewImageTool(nil)); err != nil {
		t.Fatalf("image tool schema: %v", err)
	}
	if err := registry.Register(NewVideoTool(nil)); err != nil {
		t.Fatalf("video tool schema: %v", err)
	}
	names := registry.Names()
	if len(names) != 2 || names[0] != "image_generate" || names[1] != "video_generate" {
		t.Fatalf("names = %v", names)
	}

	valid := map[string]string{
		"image_generate": `{"prompt":"a lighthouse","negative_prompt":"text","size":"1024x1024","n":2,"reference_image_urls":["data:image/png;base64,AAAA"]}`,
		"video_generate": `{"prompt":"a paper boat","duration":5,"aspect_ratio":"16:9","resolution":"1080p","seed":1,"audio":true}`,
	}
	for name, args := range valid {
		if fields := registry.Validate(name, json.RawMessage(args)); len(fields) != 0 {
			t.Fatalf("%s rejected valid args: %+v", name, fields)
		}
	}

	fields := registry.Validate("image_generate", json.RawMessage(`{"negative_prompt":"x"}`))
	found := false
	for _, field := range fields {
		if strings.Contains(field.Message, "prompt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing prompt was not reported: %+v", fields)
	}
	fields = registry.Validate("video_generate", json.RawMessage(`{"prompt":"x","duration":301}`))
	if len(fields) == 0 {
		t.Fatal("out-of-range duration passed schema validation")
	}
}

// TestMediaToolsTakeTheSessionFromTheRunScope pins the serving-runtime path: a
// run stamps agentcore.ToolRunScope on the tool context, and the adapter uses
// that session as the layerfs retention owner without a second wiring step. An
// explicit WithArtifactSession still wins for callers outside a run.
func TestMediaToolsTakeTheSessionFromTheRunScope(t *testing.T) {
	imageStore := &recordingStore{}
	imageTool := NewImageTool(&ImageAdapter{
		Provider: &fakeImageProvider{batch: ImageBatch{
			Assets: []ProviderAsset{{Data: []byte("png"), Mime: "image/png"}},
		}},
		Store: imageStore,
	})
	runCtx := agentcore.WithToolRunScope(context.Background(), agentcore.ToolRunScope{
		TenantID: "tenant-1", SessionID: "session-from-run",
	})
	result, err := imageTool.Execute(runCtx, "call-1", json.RawMessage(`{"prompt":"a kite"}`), nil)
	if err != nil || (result.IsError != nil && *result.IsError) {
		t.Fatalf("image tool under a run scope: result=%+v err=%v", result, err)
	}
	if got := imageStore.call(0).Session; got != "session-from-run" {
		t.Fatalf("image artifact session = %q, want the run scope's", got)
	}

	videoStore := &recordingStore{}
	videoTool := NewVideoTool(&VideoAdapter{
		Provider: &fakeVideoProvider{batch: VideoBatch{
			Assets: []ProviderAsset{{Data: []byte("mp4"), Mime: "video/mp4"}},
		}},
		Store: videoStore,
	})
	result, err = videoTool.Execute(runCtx, "call-2", json.RawMessage(`{"prompt":"a kite"}`), nil)
	if err != nil || (result.IsError != nil && *result.IsError) {
		t.Fatalf("video tool under a run scope: result=%+v err=%v", result, err)
	}
	if got := videoStore.call(0).Session; got != "session-from-run" {
		t.Fatalf("video artifact session = %q, want the run scope's", got)
	}

	overrideStore := &recordingStore{}
	overrideTool := NewImageTool(&ImageAdapter{
		Provider: &fakeImageProvider{batch: ImageBatch{
			Assets: []ProviderAsset{{Data: []byte("png"), Mime: "image/png"}},
		}},
		Store: overrideStore,
	})
	explicit := WithArtifactSession(runCtx, "explicit-session")
	if _, err := overrideTool.Execute(explicit, "call-3", json.RawMessage(`{"prompt":"a kite"}`), nil); err != nil {
		t.Fatal(err)
	}
	if got := overrideStore.call(0).Session; got != "explicit-session" {
		t.Fatalf("explicit session must win, got %q", got)
	}
}
