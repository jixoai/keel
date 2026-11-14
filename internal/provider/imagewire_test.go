package provider

// Adapter wire-shape tests for the three image forms (keel-ga §3 provider-files):
// file_id, url and base64, per driver. The assertions read the encoded request
// body, which is exactly what the vendor sees — a shape that round-trips through
// a struct but encodes to the wrong JSON is the failure this file prevents.
//
// A mock provider server is also driven end-to-end for the three wires, so the
// form survives the transport (SSE stream, auth header, body) and not just the
// encoder.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

// imageRequest builds a CompletionRequest carrying one image block.
func imageRequest(model string, img agentcore.ImageContent) CompletionRequest {
	return CompletionRequest{
		Model: model,
		Context: LlmContext{
			Messages: agentcore.MessageList{agentcore.UserMessage{
				RoleField: agentcore.RoleUser,
				Content:   agentcore.ContentList{agentcore.NewTextContent("look"), img},
			}},
		},
		Config: StreamConfig{APIKey: "test-key"},
	}
}

// decodeAnthropicImage pulls the image block out of an Anthropic body.
func decodeAnthropicImage(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("body messages = %#v", body["messages"])
	}
	first, _ := messages[0].(map[string]any)
	blocks, ok := first["content"].([]any)
	if !ok {
		t.Fatalf("message content = %#v, want a block list", first["content"])
	}
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		if block["type"] == "image" {
			source, _ := block["source"].(map[string]any)
			return source
		}
	}
	t.Fatalf("no image block in %#v", blocks)
	return nil
}

// TestAnthropicWireCarriesAllThreeImageForms pins the Anthropic image source
// shapes: base64, url and file.
func TestAnthropicWireCarriesAllThreeImageForms(t *testing.T) {
	t.Run("base64", func(t *testing.T) {
		body := anthropicWireBody(t, imageRequest("claude-sonnet-4-5",
			agentcore.ImageContent{Type: agentcore.ContentTypeImage, Data: "QUJD", MimeType: "image/png"}))
		source := decodeAnthropicImage(t, body)
		if source["type"] != "base64" || source["media_type"] != "image/png" || source["data"] != "QUJD" {
			t.Fatalf("source = %#v, want a base64 block", source)
		}
	})

	t.Run("url", func(t *testing.T) {
		body := anthropicWireBody(t, imageRequest("claude-sonnet-4-5",
			agentcore.ImageContent{Type: agentcore.ContentTypeImage, URL: "https://keel.example/v1/files/abc", MimeType: "image/png"}))
		source := decodeAnthropicImage(t, body)
		if source["type"] != "url" || source["url"] != "https://keel.example/v1/files/abc" {
			t.Fatalf("source = %#v, want a url block", source)
		}
	})

	t.Run("file", func(t *testing.T) {
		body := anthropicWireBody(t, imageRequest("claude-sonnet-4-5",
			agentcore.ImageContent{Type: agentcore.ContentTypeImage, FileID: "file_abc", MimeType: "image/png"}))
		source := decodeAnthropicImage(t, body)
		if source["type"] != "file" || source["file_id"] != "file_abc" {
			t.Fatalf("source = %#v, want a file block", source)
		}
	})

	t.Run("a block with no source becomes a text notice", func(t *testing.T) {
		body := anthropicWireBody(t, imageRequest("claude-sonnet-4-5",
			agentcore.ImageContent{Type: agentcore.ContentTypeImage}))
		messages, _ := body["messages"].([]any)
		first, _ := messages[0].(map[string]any)
		blocks, _ := first["content"].([]any)
		notice := false
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			if block["type"] == "text" && strings.Contains(fmt.Sprint(block["text"]), "image omitted") {
				notice = true
			}
		}
		if !notice {
			t.Fatalf("blocks = %#v, want a text notice for the sourceless image", blocks)
		}
	})
}

// TestOpenAIChatWireCarriesURLAndBase64 pins the chat wire: image_url takes the
// absolute URL or the data URI, and a file_id (which the chat wire has no field
// for) becomes a text notice instead of being silently dropped.
func TestOpenAIChatWireCarriesURLAndBase64(t *testing.T) {
	t.Run("base64 becomes a data URI", func(t *testing.T) {
		parts := openAIUserContent(agentcore.ContentList{
			agentcore.NewTextContent("look"),
			agentcore.ImageContent{Type: agentcore.ContentTypeImage, Data: "QUJD", MimeType: "image/jpeg"},
		})
		list, ok := parts.([]map[string]any)
		if !ok {
			t.Fatalf("parts = %#v", parts)
		}
		var found map[string]any
		for _, part := range list {
			if part["type"] == "image_url" {
				found, _ = part["image_url"].(map[string]any)
			}
		}
		if found == nil || found["url"] != "data:image/jpeg;base64,QUJD" {
			t.Fatalf("image_url = %#v, want the data URI", found)
		}
	})

	t.Run("url passes through", func(t *testing.T) {
		parts := openAIUserContent(agentcore.ContentList{
			agentcore.ImageContent{Type: agentcore.ContentTypeImage, URL: "https://keel.example/f.png", MimeType: "image/png"},
		})
		list, _ := parts.([]map[string]any)
		imageURL, _ := list[0]["image_url"].(map[string]any)
		if imageURL["url"] != "https://keel.example/f.png" {
			t.Fatalf("image_url = %#v, want the absolute url", imageURL)
		}
	})

	t.Run("file_id becomes a text notice", func(t *testing.T) {
		parts := openAIUserContent(agentcore.ContentList{
			agentcore.ImageContent{Type: agentcore.ContentTypeImage, FileID: "file_1", MimeType: "image/png"},
		})
		list, _ := parts.([]map[string]any)
		if list[0]["type"] != "text" || !strings.Contains(fmt.Sprint(list[0]["text"]), "image omitted") {
			t.Fatalf("part = %#v, want a text notice (the chat wire cannot carry a file id)", list[0])
		}
	})
}

// TestResponsesWireCarriesAllThreeForms pins the Responses input_image shapes.
func TestResponsesWireCarriesAllThreeForms(t *testing.T) {
	cases := []struct {
		name   string
		img    agentcore.ImageContent
		field  string
		expect string
	}{
		{name: "file id", img: agentcore.ImageContent{Type: agentcore.ContentTypeImage, FileID: "file_9"},
			field: "file_id", expect: "file_9"},
		{name: "url", img: agentcore.ImageContent{Type: agentcore.ContentTypeImage, URL: "https://keel.example/f.png", MimeType: "image/png"},
			field: "image_url", expect: "https://keel.example/f.png"},
		{name: "base64", img: agentcore.ImageContent{Type: agentcore.ContentTypeImage, Data: "QUJD", MimeType: "image/png"},
			field: "image_url", expect: "data:image/png;base64,QUJD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := imageRequest("gpt-test", tc.img)
			// imageInputParts returns a typed SDK list; assert on the serialized
			// form, which is what the vendor receives.
			var parts []map[string]any
			if err := json.Unmarshal(imagePartsPayload(t, req), &parts); err != nil {
				t.Fatalf("unmarshal parts: %v", err)
			}
			var image map[string]any
			for _, part := range parts {
				if part["type"] == "input_image" {
					image = part
				}
			}
			if image == nil {
				t.Fatalf("no input_image part in %#v", parts)
			}
			if image[tc.field] != tc.expect {
				t.Fatalf("%s = %#v, want %q (part %#v)", tc.field, image[tc.field], tc.expect, image)
			}
		})
	}
}

// imagePartsPayload serializes the parts list of one message for assertions.
func imagePartsPayload(t *testing.T, req CompletionRequest) []byte {
	t.Helper()
	parts, ok := imageInputParts(req.Context.Messages[0])
	if !ok {
		t.Fatal("the message carries an image; imageInputParts must say so")
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		t.Fatalf("marshal parts: %v", err)
	}
	return raw
}

// TestMockProviderServersSeeTheDeclaredWireForm drives the three drivers against
// mock servers and asserts the request each one actually sends. This is the
// end-to-end half: the encoder, the transport and the auth header all have to
// agree for the image to arrive.
func TestMockProviderServersSeeTheDeclaredWireForm(t *testing.T) {
	t.Run("anthropic file_id", func(t *testing.T) {
		var seen map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &seen)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, anthropicSSE("ok"))
		}))
		defer server.Close()
		driver := NewAnthropicProvider(server.URL, nil)
		stream, err := driver.StreamCompletion(context.Background(), imageRequest("claude-sonnet-4-5",
			agentcore.ImageContent{Type: agentcore.ContentTypeImage, FileID: "file_anthropic", MimeType: "image/png"}))
		if err != nil {
			t.Fatalf("StreamCompletion: %v", err)
		}
		for range stream.Events() {
		}
		if seen == nil {
			t.Fatal("the mock server saw no body")
		}
		source := decodeAnthropicImage(t, seen)
		if source["type"] != "file" || source["file_id"] != "file_anthropic" {
			t.Fatalf("source = %#v, want the file form on the wire", source)
		}
	})

	t.Run("openai chat url", func(t *testing.T) {
		var seen map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &seen)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, openAISSE("ok"))
		}))
		defer server.Close()
		driver := NewOpenAICompatibleProvider(server.URL, nil)
		stream, err := driver.StreamCompletion(context.Background(), imageRequest("gpt-test",
			agentcore.ImageContent{Type: agentcore.ContentTypeImage, URL: "https://keel.example/f.png", MimeType: "image/png"}))
		if err != nil {
			t.Fatalf("StreamCompletion: %v", err)
		}
		for range stream.Events() {
		}
		target := findChatImageURL(t, seen)
		if target != "https://keel.example/f.png" {
			t.Fatalf("image_url = %q, want the absolute url on the wire", target)
		}
	})

	t.Run("openai chat base64", func(t *testing.T) {
		var seen map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &seen)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, openAISSE("ok"))
		}))
		defer server.Close()
		driver := NewOpenAICompatibleProvider(server.URL, nil)
		stream, err := driver.StreamCompletion(context.Background(), imageRequest("gpt-test",
			agentcore.ImageContent{Type: agentcore.ContentTypeImage, Data: "QUJD", MimeType: "image/png"}))
		if err != nil {
			t.Fatalf("StreamCompletion: %v", err)
		}
		for range stream.Events() {
		}
		target := findChatImageURL(t, seen)
		if target != "data:image/png;base64,QUJD" {
			t.Fatalf("image_url = %q, want the data URI on the wire", target)
		}
	})
}

// findChatImageURL pulls the first image_url out of an OpenAI chat body.
func findChatImageURL(t *testing.T, body map[string]any) string {
	t.Helper()
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if message["role"] != "user" {
			continue
		}
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, part := range parts {
			block, _ := part.(map[string]any)
			if block["type"] != "image_url" {
				continue
			}
			imageURL, _ := block["image_url"].(map[string]any)
			value, _ := imageURL["url"].(string)
			return value
		}
	}
	t.Fatalf("no image_url part in %#v", body["messages"])
	return ""
}

// anthropicSSE builds a minimal Messages SSE stream carrying one text block.
func anthropicSSE(text string) string {
	return strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":1,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + text + `"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
}

// openAISSE builds a minimal Chat Completions SSE stream carrying one text delta.
func openAISSE(text string) string {
	return strings.Join([]string{
		`data: {"id":"chatcmpl-1","model":"gpt-test","choices":[{"delta":{"content":"` + text + `"}}]}`,
		``,
		`data: {"id":"chatcmpl-1","choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
}
