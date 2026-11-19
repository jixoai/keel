// MCP resources and prompts client methods (the resources/* and prompts/*
// families) plus the MCP Apps document validation. The wire types live in
// protocol.go; this file holds the request methods, pagination, the 2026-07-28
// cache hints (ttlMs/cacheScope), and the strict app-resource check the host
// relies on before rendering anything.
//
// Nothing here changes the tool surface: these methods are the client half of
// MCP Apps/resources and are reached through the aggregate `mcp` tool or the
// host registry, never through per-server tool names.

package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNotAppResource is the typed "the resource does not satisfy the MCP Apps
// document contract" failure: wrong media type, wrong content shape, or a
// non-decodable blob.
var ErrNotAppResource = errors.New("mcp: resource is not an MCP app document")

// AppResource is one validated MCP app document returned by ReadAppResource.
type AppResource struct {
	URI        string
	MIMEType   string
	Text       string
	Blob       []byte
	Meta       json.RawMessage
	TTLMs      *int64
	CacheScope string
}

// ListResources returns every resource, following nextCursor through all pages.
func (c *Client) ListResources(ctx context.Context) ([]Resource, error) {
	return paginate(ctx, "resources/list", func(ctx context.Context, cursor string) ([]Resource, string, error) {
		page, err := c.ListResourcesPage(ctx, cursor)
		if err != nil {
			return nil, "", err
		}
		return page.Resources, page.NextCursor, nil
	})
}

// ListResourcesPage requests one page of resources/list. An empty cursor asks
// for the first page and omits the params object.
func (c *Client) ListResourcesPage(ctx context.Context, cursor string) (ListResourcesResult, error) {
	params, err := cursorParams(cursor)
	if err != nil {
		return ListResourcesResult{}, err
	}
	raw, err := c.request(ctx, "resources/list", params, false)
	if err != nil {
		return ListResourcesResult{}, err
	}
	return validateListResourcesResult(raw)
}

// ListResourceTemplates returns every template, following nextCursor.
func (c *Client) ListResourceTemplates(ctx context.Context) ([]ResourceTemplate, error) {
	return paginate(ctx, "resources/templates/list", func(ctx context.Context, cursor string) ([]ResourceTemplate, string, error) {
		page, err := c.ListResourceTemplatesPage(ctx, cursor)
		if err != nil {
			return nil, "", err
		}
		return page.ResourceTemplates, page.NextCursor, nil
	})
}

// ListResourceTemplatesPage requests one page of resources/templates/list.
func (c *Client) ListResourceTemplatesPage(ctx context.Context, cursor string) (ListResourceTemplatesResult, error) {
	params, err := cursorParams(cursor)
	if err != nil {
		return ListResourceTemplatesResult{}, err
	}
	raw, err := c.request(ctx, "resources/templates/list", params, false)
	if err != nil {
		return ListResourceTemplatesResult{}, err
	}
	return validateListResourceTemplatesResult(raw)
}

// ReadResource reads one resource by URI.
func (c *Client) ReadResource(ctx context.Context, uri string) (ReadResourceResult, error) {
	if uri == "" {
		return ReadResourceResult{}, fmt.Errorf("mcp: resources/read: empty uri")
	}
	params, err := json.Marshal(struct {
		URI string `json:"uri"`
	}{URI: uri})
	if err != nil {
		return ReadResourceResult{}, fmt.Errorf("mcp: encode resources/read params: %w", err)
	}
	raw, err := c.request(ctx, "resources/read", params, false)
	if err != nil {
		return ReadResourceResult{}, err
	}
	return validateReadResourceResult(raw)
}

// AppResourceMaxBytes bounds one MCP App document (spec requirement: default
// 2 MiB). The transport's 16 MiB frame limit is not a document limit: a server
// must not be able to push a multi-megabyte document through resources/read
// just because the frame fits. The bound is measured on document bytes (text
// length, or decoded blob length).
const AppResourceMaxBytes = 2 << 20

// ReadAppResource reads a ui:// resource and enforces the MCP Apps document
// contract: the media type MUST be text/html;profile=mcp-app, the content MUST
// be text or a base64 blob (never both, never neither), and the document MUST
// stay within AppResourceMaxBytes. A failure is reported as ErrNotAppResource
// with the concrete reason; the caller records a diagnostic and keeps the text
// fallback.
func (c *Client) ReadAppResource(ctx context.Context, uri string) (AppResource, error) {
	result, err := c.ReadResource(ctx, uri)
	if err != nil {
		return AppResource{}, err
	}
	return ValidateAppResource(result)
}

// ValidateAppResource checks one resources/read result as an app document.
func ValidateAppResource(result ReadResourceResult) (AppResource, error) {
	if len(result.Contents) != 1 {
		return AppResource{}, fmt.Errorf("%w: expected exactly one content item, got %d", ErrNotAppResource, len(result.Contents))
	}
	content := result.Contents[0]
	if content.MimeType != AppMIMEType {
		return AppResource{}, fmt.Errorf("%w: resource %q declares mimeType %q, want %q",
			ErrNotAppResource, content.URI, content.MimeType, AppMIMEType)
	}
	if content.Blob != "" && content.Text != "" {
		return AppResource{}, fmt.Errorf("%w: resource %q carries both text and blob", ErrNotAppResource, content.URI)
	}
	if content.Blob == "" && content.Text == "" {
		return AppResource{}, fmt.Errorf("%w: resource %q carries neither text nor blob", ErrNotAppResource, content.URI)
	}
	resource := AppResource{
		URI:        content.URI,
		MIMEType:   content.MimeType,
		Meta:       appResourceMeta(content.Meta, result.Meta),
		TTLMs:      result.TTLMs,
		CacheScope: result.CacheScope,
	}
	if content.Blob != "" {
		decoded, err := base64.StdEncoding.DecodeString(content.Blob)
		if err != nil {
			return AppResource{}, fmt.Errorf("%w: resource %q has an invalid base64 blob: %v", ErrNotAppResource, content.URI, err)
		}
		if len(decoded) > AppResourceMaxBytes {
			return AppResource{}, fmt.Errorf("%w: resource %q blob is %d bytes, over the %d-byte limit",
				ErrNotAppResource, content.URI, len(decoded), AppResourceMaxBytes)
		}
		resource.Blob = decoded
		return resource, nil
	}
	if len(content.Text) > AppResourceMaxBytes {
		return AppResource{}, fmt.Errorf("%w: resource %q text is %d bytes, over the %d-byte limit",
			ErrNotAppResource, content.URI, len(content.Text), AppResourceMaxBytes)
	}
	resource.Text = content.Text
	return resource, nil
}

// appResourceMeta picks the `_meta` an app document's metadata is read from:
// the content item's own `_meta` when present, otherwise the result-level
// `_meta`. MCP Apps puts `_meta.ui` on the resource item; a server that places
// it on the result object is accepted as well (kept for the P1 fixture shape).
func appResourceMeta(item, result json.RawMessage) json.RawMessage {
	if len(item) > 0 && !isJSONNull(item) {
		return append(json.RawMessage(nil), item...)
	}
	if len(result) > 0 && !isJSONNull(result) {
		return append(json.RawMessage(nil), result...)
	}
	return nil
}

// ListPrompts returns every prompt, following nextCursor through all pages.
func (c *Client) ListPrompts(ctx context.Context) ([]Prompt, error) {
	return paginate(ctx, "prompts/list", func(ctx context.Context, cursor string) ([]Prompt, string, error) {
		page, err := c.ListPromptsPage(ctx, cursor)
		if err != nil {
			return nil, "", err
		}
		return page.Prompts, page.NextCursor, nil
	})
}

// ListPromptsPage requests one page of prompts/list.
func (c *Client) ListPromptsPage(ctx context.Context, cursor string) (ListPromptsResult, error) {
	params, err := cursorParams(cursor)
	if err != nil {
		return ListPromptsResult{}, err
	}
	raw, err := c.request(ctx, "prompts/list", params, false)
	if err != nil {
		return ListPromptsResult{}, err
	}
	return validateListPromptsResult(raw)
}

// GetPrompt renders one prompt. A nil arguments map is sent without an
// arguments field.
func (c *Client) GetPrompt(ctx context.Context, name string, arguments map[string]string) (GetPromptResult, error) {
	if name == "" {
		return GetPromptResult{}, fmt.Errorf("mcp: prompts/get: empty name")
	}
	payload := struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments,omitempty"`
	}{Name: name, Arguments: arguments}
	params, err := json.Marshal(payload)
	if err != nil {
		return GetPromptResult{}, fmt.Errorf("mcp: encode prompts/get params: %w", err)
	}
	raw, err := c.request(ctx, "prompts/get", params, false)
	if err != nil {
		return GetPromptResult{}, err
	}
	return validateGetPromptResult(raw)
}

// paginate follows nextCursor through every page, with the same bound and
// duplicate-cursor rule tools/list uses. Like pi's list helpers, the aggregate
// grows with every page: the bound is maxListPages, not a byte budget. That
// cross-page accumulation is a known PI-aligned semantics (not a P1 change);
// per-page shapes are bounded by the transport frame limit.
func paginate[T any](ctx context.Context, method string, page func(context.Context, string) ([]T, string, error)) ([]T, error) {
	var out []T
	seen := make(map[string]bool)
	cursor := ""
	for index := 0; index < maxListPages; index++ {
		items, next, err := page(ctx, cursor)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if next == "" {
			return out, nil
		}
		if seen[next] {
			return nil, fmt.Errorf("mcp: %s returned duplicate cursor: %s", method, next)
		}
		seen[next] = true
		cursor = next
	}
	return nil, fmt.Errorf("mcp: %s exceeded %d pages", method, maxListPages)
}

// cursorParams encodes the optional cursor parameter: an empty cursor omits the
// params object entirely, like tools/list.
func cursorParams(cursor string) (json.RawMessage, error) {
	if cursor == "" {
		return nil, nil
	}
	encoded, err := json.Marshal(struct {
		Cursor string `json:"cursor"`
	}{Cursor: cursor})
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// parseCacheHints reads the 2026-07-28 ttlMs/cacheScope fields. They are
// advisory: a malformed value is ignored instead of failing a legacy server's
// otherwise valid result.
func parseCacheHints(fields map[string]json.RawMessage) (*int64, string) {
	var ttl *int64
	if raw, ok := fields["ttlMs"]; ok && !isJSONNull(raw) {
		var value int64
		if err := json.Unmarshal(raw, &value); err == nil && value >= 0 {
			ttl = &value
		}
	}
	var scope string
	if raw, ok := fields["cacheScope"]; ok && !isJSONNull(raw) {
		var value string
		if err := json.Unmarshal(raw, &value); err == nil {
			scope = value
		}
	}
	return ttl, scope
}

func validateListResourcesResult(data json.RawMessage) (ListResourcesResult, error) {
	invalid := &McpError{Code: CodeInvalidRequest, Message: "Invalid MCP resources/list result"}
	invalidEntry := &McpError{Code: CodeInvalidRequest, Message: "Invalid entry in MCP resources/list result"}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return ListResourcesResult{}, invalid
	}
	resourcesRaw, ok := fields["resources"]
	if !ok || isJSONNull(resourcesRaw) {
		return ListResourcesResult{}, invalid
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(resourcesRaw, &entries); err != nil || entries == nil {
		return ListResourcesResult{}, invalid
	}
	resources := make([]Resource, 0, len(entries))
	for _, entry := range entries {
		var entryFields map[string]json.RawMessage
		if err := json.Unmarshal(entry, &entryFields); err != nil || entryFields == nil {
			return ListResourcesResult{}, invalidEntry
		}
		var uri string
		if err := json.Unmarshal(entryFields["uri"], &uri); err != nil || uri == "" {
			return ListResourcesResult{}, invalidEntry
		}
		var resource Resource
		if err := json.Unmarshal(entry, &resource); err != nil {
			return ListResourcesResult{}, invalidEntry
		}
		if resource.Name == "" {
			resource.Name = resource.URI
		}
		if raw, ok := entryFields["mimeType"]; ok && !isJSONNull(raw) {
			if err := json.Unmarshal(raw, &resource.MimeType); err != nil {
				return ListResourcesResult{}, invalidEntry
			}
		}
		resources = append(resources, resource)
	}
	result := ListResourcesResult{Resources: resources}
	if err := decodeCursor(fields, &result.NextCursor); err != nil {
		return ListResourcesResult{}, err
	}
	result.TTLMs, result.CacheScope = parseCacheHints(fields)
	return result, nil
}

func validateListResourceTemplatesResult(data json.RawMessage) (ListResourceTemplatesResult, error) {
	invalid := &McpError{Code: CodeInvalidRequest, Message: "Invalid MCP resources/templates/list result"}
	invalidEntry := &McpError{Code: CodeInvalidRequest, Message: "Invalid entry in MCP resources/templates/list result"}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return ListResourceTemplatesResult{}, invalid
	}
	templatesRaw, ok := fields["resourceTemplates"]
	if !ok || isJSONNull(templatesRaw) {
		return ListResourceTemplatesResult{}, invalid
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(templatesRaw, &entries); err != nil || entries == nil {
		return ListResourceTemplatesResult{}, invalid
	}
	templates := make([]ResourceTemplate, 0, len(entries))
	for _, entry := range entries {
		var entryFields map[string]json.RawMessage
		if err := json.Unmarshal(entry, &entryFields); err != nil || entryFields == nil {
			return ListResourceTemplatesResult{}, invalidEntry
		}
		var uriTemplate string
		if err := json.Unmarshal(entryFields["uriTemplate"], &uriTemplate); err != nil || uriTemplate == "" {
			return ListResourceTemplatesResult{}, invalidEntry
		}
		var template ResourceTemplate
		if err := json.Unmarshal(entry, &template); err != nil {
			return ListResourceTemplatesResult{}, invalidEntry
		}
		if template.Name == "" {
			template.Name = template.URITemplate
		}
		if raw, ok := entryFields["mimeType"]; ok && !isJSONNull(raw) {
			if err := json.Unmarshal(raw, &template.MimeType); err != nil {
				return ListResourceTemplatesResult{}, invalidEntry
			}
		}
		templates = append(templates, template)
	}
	result := ListResourceTemplatesResult{ResourceTemplates: templates}
	if err := decodeCursor(fields, &result.NextCursor); err != nil {
		return ListResourceTemplatesResult{}, err
	}
	result.TTLMs, result.CacheScope = parseCacheHints(fields)
	return result, nil
}

func validateReadResourceResult(data json.RawMessage) (ReadResourceResult, error) {
	invalid := &McpError{Code: CodeInvalidRequest, Message: "Invalid MCP resources/read result"}
	invalidEntry := &McpError{Code: CodeInvalidRequest, Message: "Invalid entry in MCP resources/read result"}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return ReadResourceResult{}, invalid
	}
	contentsRaw, ok := fields["contents"]
	if !ok || isJSONNull(contentsRaw) {
		return ReadResourceResult{}, invalid
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(contentsRaw, &entries); err != nil || entries == nil {
		return ReadResourceResult{}, invalid
	}
	contents := make([]ResourceContents, 0, len(entries))
	for _, entry := range entries {
		var entryFields map[string]json.RawMessage
		if err := json.Unmarshal(entry, &entryFields); err != nil || entryFields == nil {
			return ReadResourceResult{}, invalidEntry
		}
		var uri string
		if err := json.Unmarshal(entryFields["uri"], &uri); err != nil || uri == "" {
			return ReadResourceResult{}, invalidEntry
		}
		// The spec allows exactly one of text/blob. Both present is a broken
		// document, not a precedence question.
		hasText := rawPresent(entryFields["text"])
		hasBlob := rawPresent(entryFields["blob"])
		if hasText && hasBlob {
			return ReadResourceResult{}, invalidEntry
		}
		var content ResourceContents
		if err := json.Unmarshal(entry, &content); err != nil {
			return ReadResourceResult{}, invalidEntry
		}
		if raw, ok := entryFields["mimeType"]; ok && !isJSONNull(raw) {
			if err := json.Unmarshal(raw, &content.MimeType); err != nil {
				return ReadResourceResult{}, invalidEntry
			}
		}
		contents = append(contents, content)
	}
	result := ReadResourceResult{Contents: contents}
	if raw, ok := fields["_meta"]; ok && !isJSONNull(raw) {
		result.Meta = append(json.RawMessage(nil), raw...)
	}
	result.TTLMs, result.CacheScope = parseCacheHints(fields)
	return result, nil
}

func validateListPromptsResult(data json.RawMessage) (ListPromptsResult, error) {
	invalid := &McpError{Code: CodeInvalidRequest, Message: "Invalid MCP prompts/list result"}
	invalidEntry := &McpError{Code: CodeInvalidRequest, Message: "Invalid entry in MCP prompts/list result"}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return ListPromptsResult{}, invalid
	}
	promptsRaw, ok := fields["prompts"]
	if !ok || isJSONNull(promptsRaw) {
		return ListPromptsResult{}, invalid
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(promptsRaw, &entries); err != nil || entries == nil {
		return ListPromptsResult{}, invalid
	}
	prompts := make([]Prompt, 0, len(entries))
	for _, entry := range entries {
		var entryFields map[string]json.RawMessage
		if err := json.Unmarshal(entry, &entryFields); err != nil || entryFields == nil {
			return ListPromptsResult{}, invalidEntry
		}
		var name string
		if err := json.Unmarshal(entryFields["name"], &name); err != nil || name == "" {
			return ListPromptsResult{}, invalidEntry
		}
		var prompt Prompt
		if err := json.Unmarshal(entry, &prompt); err != nil {
			return ListPromptsResult{}, invalidEntry
		}
		if raw, ok := entryFields["arguments"]; ok && !isJSONNull(raw) {
			var arguments []json.RawMessage
			if err := json.Unmarshal(raw, &arguments); err != nil || arguments == nil {
				return ListPromptsResult{}, invalidEntry
			}
			for _, argument := range arguments {
				var argumentFields map[string]json.RawMessage
				if err := json.Unmarshal(argument, &argumentFields); err != nil || argumentFields == nil {
					return ListPromptsResult{}, invalidEntry
				}
				var argumentName string
				if err := json.Unmarshal(argumentFields["name"], &argumentName); err != nil || argumentName == "" {
					return ListPromptsResult{}, invalidEntry
				}
			}
		}
		prompts = append(prompts, prompt)
	}
	result := ListPromptsResult{Prompts: prompts}
	if err := decodeCursor(fields, &result.NextCursor); err != nil {
		return ListPromptsResult{}, err
	}
	result.TTLMs, result.CacheScope = parseCacheHints(fields)
	return result, nil
}

func validateGetPromptResult(data json.RawMessage) (GetPromptResult, error) {
	invalid := &McpError{Code: CodeInvalidRequest, Message: "Invalid MCP prompts/get result"}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return GetPromptResult{}, invalid
	}
	messagesRaw, ok := fields["messages"]
	if !ok || isJSONNull(messagesRaw) {
		return GetPromptResult{}, invalid
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(messagesRaw, &entries); err != nil || entries == nil {
		return GetPromptResult{}, invalid
	}
	messages := make([]PromptMessage, 0, len(entries))
	for _, entry := range entries {
		var entryFields map[string]json.RawMessage
		if err := json.Unmarshal(entry, &entryFields); err != nil || entryFields == nil {
			return GetPromptResult{}, invalid
		}
		var role string
		if err := json.Unmarshal(entryFields["role"], &role); err != nil {
			return GetPromptResult{}, invalid
		}
		if role != "user" && role != "assistant" {
			return GetPromptResult{}, invalid
		}
		contentRaw, ok := entryFields["content"]
		if !ok || isJSONNull(contentRaw) {
			return GetPromptResult{}, invalid
		}
		var contentFields map[string]json.RawMessage
		if err := json.Unmarshal(contentRaw, &contentFields); err != nil || contentFields == nil {
			return GetPromptResult{}, invalid
		}
		var contentType string
		if err := json.Unmarshal(contentFields["type"], &contentType); err != nil || contentType == "" {
			return GetPromptResult{}, invalid
		}
		var message PromptMessage
		if err := json.Unmarshal(entry, &message); err != nil {
			return GetPromptResult{}, invalid
		}
		messages = append(messages, message)
	}
	result := GetPromptResult{Messages: messages}
	if raw, ok := fields["description"]; ok && !isJSONNull(raw) {
		if err := json.Unmarshal(raw, &result.Description); err != nil {
			return GetPromptResult{}, invalid
		}
	}
	if raw, ok := fields["_meta"]; ok && !isJSONNull(raw) {
		result.Meta = append(json.RawMessage(nil), raw...)
	}
	return result, nil
}

// decodeCursor reads nextCursor into cursor. Servers that end pagination with
// "" instead of omitting the field are accepted, like tools/list.
func decodeCursor(fields map[string]json.RawMessage, cursor *string) error {
	raw, ok := fields["nextCursor"]
	if !ok || isJSONNull(raw) {
		return nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return &McpError{Code: CodeInvalidRequest, Message: "Invalid MCP nextCursor"}
	}
	*cursor = value
	return nil
}

// rawPresent reports whether a JSON object field is present and not null.
func rawPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && !isJSONNull(raw)
}
