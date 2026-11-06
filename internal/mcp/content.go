// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/protocol/content.ts and the CallToolResult validation in
// packages/mcp/src/client.ts (MIT License); implemented independently in Go.
// Contract kept identical; see docs/UPSTREAM.md.

package mcp

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
)

// ContentBlock is one block of a tools/call result. It carries the fields of
// every block kind and Type selects which apply, mirroring the union in
// protocol/content.ts:7-63. A "resource" block carries Resource; text, image,
// and audio blocks use Text, Data, and MimeType; a "resource_link" uses URI and
// Name.
type ContentBlock struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	Data     string            `json:"data,omitempty"`
	MimeType string            `json:"mimeType,omitempty"`
	URI      string            `json:"uri,omitempty"`
	Name     string            `json:"name,omitempty"`
	Resource *EmbeddedResource `json:"resource,omitempty"`
}

// EmbeddedResource is the resource of a "resource" block: text, or a base64
// blob with a MIME type. Text is a pointer so presence can be detected the way
// pi checks "text" in resource (content.ts:91): an empty string is text, an
// absent or null text is a blob.
type EmbeddedResource struct {
	URI      string  `json:"uri"`
	MimeType string  `json:"mimeType,omitempty"`
	Text     *string `json:"text,omitempty"`
	Blob     string  `json:"blob,omitempty"`
}

// CallToolResult is a tools/call result: the content blocks a server returned
// and, optionally, any JSON value as structured content (MCP 2026-07-28; older
// revisions allowed only objects). A nil StructuredContent means absent; a
// present JSON null is kept as the four-byte literal and converts to "null".
type CallToolResult struct {
	Content           []ContentBlock  `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// ToLLMContent converts a tool result to text and image content for a model, in
// the shape of agentcore's TextContent and ImageContent. Text and images pass
// through, embedded text and image resources are unwrapped, and other blocks
// (audio, resource links, binary resources) become a short text placeholder. A
// result without content blocks but with structuredContent becomes its JSON,
// since servers should, but do not always, mirror structured results as text.
func ToLLMContent(result CallToolResult) []agentcore.Content {
	out := make([]agentcore.Content, 0, len(result.Content)+1)
	for _, block := range result.Content {
		out = append(out, blockToLLMContent(block))
	}
	if len(out) == 0 && len(result.StructuredContent) > 0 {
		out = append(out, agentcore.NewTextContent(indentJSON(result.StructuredContent)))
	}
	return out
}

// blockToLLMContent converts one block, mirroring blockToLlmContent
// (content.ts:79-103). An unknown block kind becomes a text placeholder rather
// than an error, as in pi.
func blockToLLMContent(block ContentBlock) agentcore.Content {
	switch block.Type {
	case "text":
		return agentcore.NewTextContent(block.Text)
	case "image":
		return agentcore.NewImageContent(block.Data, block.MimeType)
	case "audio":
		return agentcore.NewTextContent("[audio " + block.MimeType + " omitted]")
	case "resource_link":
		return agentcore.NewTextContent(block.Name + ": " + block.URI)
	case "resource":
		resource := block.Resource
		if resource == nil {
			// pi dereferences undefined resource here and throws; a wire result
			// always has one, so stay defensive instead of panicking.
			return agentcore.NewTextContent("[binary resource (unknown type) omitted]")
		}
		if resource.Text != nil {
			return agentcore.NewTextContent(*resource.Text)
		}
		if strings.HasPrefix(resource.MimeType, "image/") {
			return agentcore.NewImageContent(resource.Blob, resource.MimeType)
		}
		mimeType := resource.MimeType
		if mimeType == "" {
			// pi distinguishes undefined/null ("unknown type") from "". Go's
			// zero value cannot, and an empty MIME type means the same thing.
			mimeType = "unknown type"
		}
		return agentcore.NewTextContent("[binary resource " + resource.URI + " (" + mimeType + ") omitted]")
	default:
		return agentcore.NewTextContent("[unsupported MCP content " + block.Type + "]")
	}
}

// indentJSON renders any JSON value the way JSON.stringify(value, null, 2)
// does. json.Indent keeps the original scalar lexemes (pi re-serializes parsed
// numbers), which is the closest lossless equivalent.
func indentJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}
