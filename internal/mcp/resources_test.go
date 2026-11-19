package mcp

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestClientListResourcesPaginatesAndReadsCacheHints(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.setHandler("resources/list", func(params json.RawMessage) (any, *ErrorObject) {
		var page struct {
			Cursor string `json:"cursor"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &page); err != nil {
				return nil, &ErrorObject{Code: CodeInvalidParams, Message: err.Error()}
			}
		}
		if page.Cursor == "" {
			return map[string]any{
				"resources": []any{
					map[string]any{"uri": "ui://weather/dashboard", "mimeType": AppMIMEType},
				},
				"nextCursor": "page-2",
				"ttlMs":      1500,
				"cacheScope": "public",
			}, nil
		}
		return map[string]any{
			"resources": []any{
				map[string]any{"uri": "ui://weather/legend", "name": "Legend", "mimeType": AppMIMEType},
			},
			"nextCursor": "",
		}, nil
	})
	client := connectTestClient(t, clientTransport)

	first, err := client.ListResourcesPage(testContext(t), "")
	if err != nil {
		t.Fatalf("ListResourcesPage: %v", err)
	}
	if first.TTLMs == nil || *first.TTLMs != 1500 || first.CacheScope != "public" {
		t.Fatalf("cache hints = %v/%q, want 1500/public", first.TTLMs, first.CacheScope)
	}
	if first.Resources[0].Name != "ui://weather/dashboard" {
		t.Errorf("missing name = %q, want the URI", first.Resources[0].Name)
	}

	resources, err := client.ListResources(testContext(t))
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(resources) != 2 || resources[1].Name != "Legend" {
		t.Fatalf("resources = %+v", resources)
	}
	pages := server.requests("resources/list")
	if len(pages) != 3 {
		t.Fatalf("server saw %d resources/list requests, want 3 (page + two pages)", len(pages))
	}
	if pages[0].Params != nil || pages[1].Params != nil {
		t.Errorf("first-page params = %s / %s, want both omitted", pages[0].Params, pages[1].Params)
	}
	if got := string(pages[2].Params); got != `{"cursor":"page-2"}` {
		t.Errorf("second page params = %s", got)
	}
}

func TestClientListResourceTemplates(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.setHandler("resources/templates/list", func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"resourceTemplates": []any{
			map[string]any{"uriTemplate": "ui://weather/{city}/view", "mimeType": AppMIMEType},
		}}, nil
	})
	client := connectTestClient(t, clientTransport)

	templates, err := client.ListResourceTemplates(testContext(t))
	if err != nil {
		t.Fatalf("ListResourceTemplates: %v", err)
	}
	if len(templates) != 1 {
		t.Fatalf("templates = %+v", templates)
	}
	if templates[0].Name != "ui://weather/{city}/view" {
		t.Errorf("name = %q, want the URI template", templates[0].Name)
	}
	if templates[0].MimeType != AppMIMEType {
		t.Errorf("mimeType = %q", templates[0].MimeType)
	}
}

func TestClientReadResource(t *testing.T) {
	clientTransport, server := newTestServer(t)
	var seenURI string
	server.setHandler("resources/read", func(params json.RawMessage) (any, *ErrorObject) {
		var request struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, &ErrorObject{Code: CodeInvalidParams, Message: err.Error()}
		}
		seenURI = request.URI
		switch request.URI {
		case "ui://weather/dashboard":
			return map[string]any{
				"contents": []any{map[string]any{
					"uri":      request.URI,
					"mimeType": AppMIMEType,
					"text":     "<html>app</html>",
				}},
				"ttlMs":      60000,
				"cacheScope": "private",
			}, nil
		case "ui://weather/blob":
			return map[string]any{"contents": []any{map[string]any{
				"uri":      request.URI,
				"mimeType": AppMIMEType,
				"blob":     base64.StdEncoding.EncodeToString([]byte("<html>blob</html>")),
			}}}, nil
		case "ui://weather/both":
			return map[string]any{"contents": []any{map[string]any{
				"uri": request.URI, "text": "x", "blob": "eA==",
			}}}, nil
		default:
			return map[string]any{"contents": "not a list"}, nil
		}
	})
	client := connectTestClient(t, clientTransport)

	result, err := client.ReadResource(testContext(t), "ui://weather/dashboard")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if seenURI != "ui://weather/dashboard" {
		t.Errorf("server saw uri %q", seenURI)
	}
	if len(result.Contents) != 1 || result.Contents[0].Text != "<html>app</html>" {
		t.Fatalf("contents = %+v", result.Contents)
	}
	if result.TTLMs == nil || *result.TTLMs != 60000 || result.CacheScope != "private" {
		t.Fatalf("cache hints = %v/%q", result.TTLMs, result.CacheScope)
	}

	app, err := client.ReadAppResource(testContext(t), "ui://weather/dashboard")
	if err != nil {
		t.Fatalf("ReadAppResource(text): %v", err)
	}
	if app.Text != "<html>app</html>" || app.MIMEType != AppMIMEType {
		t.Fatalf("app = %+v", app)
	}

	app, err = client.ReadAppResource(testContext(t), "ui://weather/blob")
	if err != nil {
		t.Fatalf("ReadAppResource(blob): %v", err)
	}
	if string(app.Blob) != "<html>blob</html>" {
		t.Fatalf("blob = %q", app.Blob)
	}

	if _, err := client.ReadResource(testContext(t), "ui://weather/both"); err == nil {
		t.Fatal("a result carrying both text and blob was accepted")
	}
	if _, err := client.ReadResource(testContext(t), "ui://weather/invalid"); err == nil {
		t.Fatal("a non-array contents field was accepted")
	}
}

func TestReadAppResourceRejectsNonAppDocuments(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.setHandler("resources/read", func(params json.RawMessage) (any, *ErrorObject) {
		var request struct {
			URI string `json:"uri"`
		}
		_ = json.Unmarshal(params, &request)
		switch request.URI {
		case "ui://weather/json":
			return map[string]any{"contents": []any{map[string]any{
				"uri": request.URI, "mimeType": "application/json", "text": "{}",
			}}}, nil
		case "ui://weather/badbase64":
			return map[string]any{"contents": []any{map[string]any{
				"uri": request.URI, "mimeType": AppMIMEType, "blob": "!!!",
			}}}, nil
		case "ui://weather/two":
			return map[string]any{"contents": []any{
				map[string]any{"uri": request.URI, "mimeType": AppMIMEType, "text": "a"},
				map[string]any{"uri": request.URI, "mimeType": AppMIMEType, "text": "b"},
			}}, nil
		default:
			return map[string]any{"contents": []any{}}, nil
		}
	})
	client := connectTestClient(t, clientTransport)

	for _, uri := range []string{"ui://weather/json", "ui://weather/badbase64", "ui://weather/two", "ui://weather/none"} {
		_, err := client.ReadAppResource(testContext(t), uri)
		if !errors.Is(err, ErrNotAppResource) {
			t.Errorf("ReadAppResource(%s) error = %v, want ErrNotAppResource", uri, err)
		}
	}
}

func TestClientListPromptsPaginates(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.setHandler("prompts/list", func(params json.RawMessage) (any, *ErrorObject) {
		var page struct {
			Cursor string `json:"cursor"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &page); err != nil {
				return nil, &ErrorObject{Code: CodeInvalidParams, Message: err.Error()}
			}
		}
		if page.Cursor == "" {
			return map[string]any{
				"prompts": []any{map[string]any{
					"name":        "greet",
					"description": "Greet someone",
					"arguments": []any{
						map[string]any{"name": "who", "description": "the person", "required": true},
					},
				}},
				"nextCursor": "prompts-2",
				"ttlMs":      30000,
			}, nil
		}
		return map[string]any{"prompts": []any{map[string]any{"name": "summarize"}}}, nil
	})
	client := connectTestClient(t, clientTransport)

	first, err := client.ListPromptsPage(testContext(t), "")
	if err != nil {
		t.Fatalf("ListPromptsPage: %v", err)
	}
	if first.TTLMs == nil || *first.TTLMs != 30000 {
		t.Fatalf("ttl = %v", first.TTLMs)
	}
	if len(first.Prompts) != 1 || len(first.Prompts[0].Arguments) != 1 || !first.Prompts[0].Arguments[0].Required {
		t.Fatalf("prompts = %+v", first.Prompts)
	}

	prompts, err := client.ListPrompts(testContext(t))
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	if len(prompts) != 2 || prompts[1].Name != "summarize" {
		t.Fatalf("prompts = %+v", prompts)
	}
}

func TestClientGetPrompt(t *testing.T) {
	clientTransport, server := newTestServer(t)
	var seenParams string
	server.setHandler("prompts/get", func(params json.RawMessage) (any, *ErrorObject) {
		seenParams = string(params)
		var request struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(params, &request)
		switch request.Name {
		case "greet":
			return map[string]any{
				"description": "Greet someone",
				"messages": []any{map[string]any{
					"role":    "user",
					"content": map[string]any{"type": "text", "text": "Hello Ada"},
				}},
			}, nil
		case "bad-role":
			return map[string]any{"messages": []any{map[string]any{
				"role": "system", "content": map[string]any{"type": "text", "text": "x"},
			}}}, nil
		default:
			return map[string]any{"messages": []any{map[string]any{"role": "user"}}}, nil
		}
	})
	client := connectTestClient(t, clientTransport)

	result, err := client.GetPrompt(testContext(t), "greet", map[string]string{"who": "Ada"})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if result.Description != "Greet someone" || len(result.Messages) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if result.Messages[0].Role != "user" || result.Messages[0].Content.Text != "Hello Ada" {
		t.Fatalf("message = %+v", result.Messages[0])
	}
	if !strings.Contains(seenParams, `"name":"greet"`) || !strings.Contains(seenParams, `"arguments":{"who":"Ada"}`) {
		t.Fatalf("server params = %s", seenParams)
	}

	if _, err := client.GetPrompt(testContext(t), "bad-role", nil); err == nil {
		t.Fatal("a non user/assistant role was accepted")
	}
	if _, err := client.GetPrompt(testContext(t), "missing-content", nil); err == nil {
		t.Fatal("a message without content was accepted")
	}
	if _, err := client.GetPrompt(testContext(t), "", nil); err == nil {
		t.Fatal("an empty prompt name was accepted")
	}
}

func TestClientListResourcesRejectsDuplicateCursor(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.setHandler("resources/list", func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{
			"resources": []any{
				map[string]any{"uri": "ui://weather/dashboard", "mimeType": AppMIMEType},
			},
			"nextCursor": "stuck",
		}, nil
	})
	client := connectTestClient(t, clientTransport)

	_, err := client.ListResources(testContext(t))
	if err == nil || !strings.Contains(err.Error(), "duplicate cursor") {
		t.Fatalf("ListResources error = %v, want a duplicate-cursor failure", err)
	}
	if pages := server.requests("resources/list"); len(pages) != 2 {
		t.Fatalf("server saw %d pages, want 2 (the duplicate is detected before a third)", len(pages))
	}
}

func TestValidateAppResourceSizeLimit(t *testing.T) {
	within := strings.Repeat("x", AppResourceMaxBytes)
	resource, err := ValidateAppResource(ReadResourceResult{Contents: []ResourceContents{{
		URI: "ui://panel/view", MimeType: AppMIMEType, Text: within,
	}}})
	if err != nil {
		t.Fatalf("exact-limit document rejected: %v", err)
	}
	if resource.Text != within {
		t.Fatalf("text length = %d, want %d", len(resource.Text), len(within))
	}

	over := strings.Repeat("x", AppResourceMaxBytes+1)
	_, err = ValidateAppResource(ReadResourceResult{Contents: []ResourceContents{{
		URI: "ui://panel/view", MimeType: AppMIMEType, Text: over,
	}}})
	if !errors.Is(err, ErrNotAppResource) || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("over-limit text error = %v, want ErrNotAppResource naming the limit", err)
	}

	blob := base64.StdEncoding.EncodeToString([]byte(over))
	_, err = ValidateAppResource(ReadResourceResult{Contents: []ResourceContents{{
		URI: "ui://panel/view", MimeType: AppMIMEType, Blob: blob,
	}}})
	if !errors.Is(err, ErrNotAppResource) || !strings.Contains(err.Error(), "blob") {
		t.Fatalf("over-limit blob error = %v, want ErrNotAppResource naming the blob", err)
	}
}

func TestReadAppResourceInheritsSizeLimit(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.setHandler("resources/read", func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"contents": []any{map[string]any{
			"uri":      "ui://panel/view",
			"mimeType": AppMIMEType,
			"text":     strings.Repeat("x", AppResourceMaxBytes+1),
		}}}, nil
	})
	client := connectTestClient(t, clientTransport)

	_, err := client.ReadAppResource(testContext(t), "ui://panel/view")
	if !errors.Is(err, ErrNotAppResource) {
		t.Fatalf("ReadAppResource error = %v, want ErrNotAppResource", err)
	}
}

func TestValidateAppResourceRejectsEmptyDocument(t *testing.T) {
	_, err := ValidateAppResource(ReadResourceResult{Contents: []ResourceContents{{
		URI: "ui://panel/view", MimeType: AppMIMEType,
	}}})
	if !errors.Is(err, ErrNotAppResource) || !strings.Contains(err.Error(), "neither text nor blob") {
		t.Fatalf("empty document error = %v, want ErrNotAppResource naming the missing content", err)
	}
}

func TestValidateAppResourceRejectsTextAndBlobTogether(t *testing.T) {
	_, err := ValidateAppResource(ReadResourceResult{Contents: []ResourceContents{{
		URI: "ui://panel/view", MimeType: AppMIMEType, Text: "<html></html>", Blob: "aGk=",
	}}})
	if !errors.Is(err, ErrNotAppResource) || !strings.Contains(err.Error(), "both text and blob") {
		t.Fatalf("both-content error = %v, want ErrNotAppResource", err)
	}
}
