// P5.5 prompt rendering: the app context block is bounded, text-first, and
// absent when there is no context (the pre-P5 prompt stays byte-identical).

package mcphost

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

func TestRenderAppContextsEmptyIsNil(t *testing.T) {
	if contents := RenderAppContexts(nil); contents != nil {
		t.Fatalf("contents = %#v, want nil", contents)
	}
	if contents := RenderAppContexts([]AppContext{{AppID: "", Content: json.RawMessage(`[{"type":"text","text":"x"}]`)}}); contents != nil {
		t.Fatalf("an app-less context rendered: %#v", contents)
	}
}

func TestRenderAppContextsIncludesTextAndStructured(t *testing.T) {
	contents := RenderAppContexts([]AppContext{
		{AppID: "mcpapp_1111111111111111", Content: json.RawMessage(`[{"type":"text","text":"viewing invoice 42"},{"type":"text","text":"total 10"}]`)},
		{AppID: "mcpapp_2222222222222222", Content: json.RawMessage(`{"view":"dashboard"}`), Structured: json.RawMessage(`{"rows":3}`)},
	})
	if len(contents) != 1 {
		t.Fatalf("contents = %d", len(contents))
	}
	text := agentcore.ContentToText(agentcore.ContentList(contents))
	if !strings.HasPrefix(text, "<mcp_app_context>") || !strings.HasSuffix(text, "</mcp_app_context>") {
		t.Fatalf("block = %q", text)
	}
	for _, want := range []string{"mcpapp_1111111111111111", "viewing invoice 42", "total 10", `{"view":"dashboard"}`, `structuredContent={"rows":3}`} {
		if !strings.Contains(text, want) {
			t.Fatalf("block %q does not contain %q", text, want)
		}
	}
}

func TestRenderAppContextsBoundsThePrompt(t *testing.T) {
	big := strings.Repeat("y", AppContextMaxPromptBytes)
	contents := RenderAppContexts([]AppContext{
		{AppID: "mcpapp_aaaaaaaaaaaaaaaa", Content: json.RawMessage(`[{"type":"text","text":"` + big + `"}]`)},
		{AppID: "mcpapp_bbbbbbbbbbbbbbbb", Content: json.RawMessage(`[{"type":"text","text":"second"}]`)},
	})
	text := agentcore.ContentToText(agentcore.ContentList(contents))
	if len(text) > AppContextMaxPromptBytes+512 {
		t.Fatalf("block = %d bytes, over the prompt bound", len(text))
	}
	if !strings.Contains(text, "prompt budget reached") {
		t.Fatalf("block did not report the omitted context: %q", text[len(text)-200:])
	}
}
