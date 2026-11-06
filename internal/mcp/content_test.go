// Tests ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi), packages/mcp/test/content.test.ts
// (MIT License). Each subtest keeps the title of the pi test it ports.

package mcp

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

func TestToLLMContent(t *testing.T) {
	t.Run("passes text and images through and replaces other blocks with text", func(t *testing.T) {
		// The wire shape of pi's test blocks, including the annotations and
		// _meta that content conversion drops.
		const raw = `{"content":[
			{"type":"text","text":"hello","annotations":{"priority":1}},
			{"type":"image","data":"aW1n","mimeType":"image/png","_meta":{"x":1}},
			{"type":"audio","data":"YXVk","mimeType":"audio/wav"},
			{"type":"resource_link","uri":"file:///a.txt","name":"a.txt"},
			{"type":"resource","resource":{"uri":"file:///b.txt","text":"inline"}},
			{"type":"resource","resource":{"uri":"file:///c.png","mimeType":"image/png","blob":"Yw=="}},
			{"type":"resource","resource":{"uri":"file:///d.bin","blob":"ZA=="}}
		]}`
		var result CallToolResult
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatalf("decode call tool result: %v", err)
		}
		want := []agentcore.Content{
			agentcore.NewTextContent("hello"),
			agentcore.NewImageContent("aW1n", "image/png"),
			agentcore.NewTextContent("[audio audio/wav omitted]"),
			agentcore.NewTextContent("a.txt: file:///a.txt"),
			agentcore.NewTextContent("inline"),
			agentcore.NewImageContent("Yw==", "image/png"),
			agentcore.NewTextContent("[binary resource file:///d.bin (unknown type) omitted]"),
		}
		if got := ToLLMContent(result); !reflect.DeepEqual(got, want) {
			t.Errorf("ToLLMContent(blocks) = %#v, want %#v", got, want)
		}
	})

	t.Run("falls back to the structured content as JSON when there are no blocks", func(t *testing.T) {
		tests := []struct {
			name          string
			content       []ContentBlock
			structured    json.RawMessage
			wantText      string
			wantNoContent bool
		}{
			{name: "array", structured: json.RawMessage(`[1,2]`), wantText: "[\n  1,\n  2\n]"},
			{name: "null", structured: json.RawMessage(`null`), wantText: "null"},
			{name: "object", structured: json.RawMessage(`{"n":1}`), wantText: "{\n  \"n\": 1\n}"},
			// Blocks win over structured content.
			{
				name:       "blocks present",
				content:    []ContentBlock{{Type: "text", Text: "n=1"}},
				structured: json.RawMessage(`{"n":1}`),
				wantText:   "n=1",
			},
			// Supplementary: nothing to report yields no blocks.
			{name: "neither", wantNoContent: true},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				got := ToLLMContent(CallToolResult{Content: test.content, StructuredContent: test.structured})
				if test.wantNoContent {
					if len(got) != 0 {
						t.Fatalf("ToLLMContent(empty) = %#v, want no content", got)
					}
					return
				}
				want := []agentcore.Content{agentcore.NewTextContent(test.wantText)}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("ToLLMContent() = %#v, want %#v", got, want)
				}
			})
		}
	})
}
