// Copied from github.com/smallnest/pigo@891d1f3 (MIT License).
// Upstream: internal/agentcore/content.go
// Modified for keel: module import paths rewritten; see docs/UPSTREAM.md.

// Package agentcore defines the core "leaf" data types and control flow for the
// pigo agent harness, a Go reimplementation of the pi agent loop. It is the
// foundation package that every other agent sub-package depends on and imports
// nothing from them.
//
// This file defines the Content model: a sealed interface implemented by the
// four content block kinds (text, thinking, toolCall, image). Because Go's
// encoding/json cannot dispatch to an interface based on a discriminant field,
// containers holding []Content implement custom UnmarshalJSON that peeks at the
// "type" field and decodes into the concrete struct. Mirrors pi's discriminated
// union (packages/ai/src/types.ts) as interface + type switch.
package agentcore

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Content is a sealed interface implemented by every content block kind.
// Consumers dispatch with a type switch. The interface is sealed via the
// unexported isContent marker so no type outside this package can satisfy it.
type Content interface {
	isContent()
}

// Content type discriminants, matching pi's wire format.
//
// ContentTypeImageRef is Keel's addition (keel-ga §3 provider-files): a
// persisted image is a *reference* to a blob, not bytes. The distinction is the
// whole point of the durable contract — the event log must never carry base64
// (pi issue #9613: a session file of 2.6GB / 6739 inline images) — so an image
// the transcript stores is always {"type":"imageRef","blobId":…}. The byte
// carrying ImageContent remains the *wire* form: it is produced at the provider
// boundary by the resolver and never committed.
const (
	ContentTypeText     = "text"
	ContentTypeThinking = "thinking"
	ContentTypeToolCall = "toolCall"
	ContentTypeImage    = "image"
	ContentTypeImageRef = "imageRef"
)

// TextContent is a plain text block.
type TextContent struct {
	Type          string `json:"type"`
	Text          string `json:"text"`
	TextSignature string `json:"textSignature,omitempty"`
}

// ThinkingContent is a reasoning/thinking block. Never folded into text.
type ThinkingContent struct {
	Type              string `json:"type"`
	Thinking          string `json:"thinking"`
	ThinkingSignature string `json:"thinkingSignature,omitempty"`
	Redacted          bool   `json:"redacted,omitempty"`
}

// ToolCallContent is a request from the model to invoke a tool. Arguments are
// kept as raw JSON so validation (JSON Schema) and shaping happen downstream.
type ToolCallContent struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	Arguments        json.RawMessage `json:"arguments"`
	ThoughtSignature string          `json:"thoughtSignature,omitempty"`
}

// ImageContent is an image block in its *transport* form: exactly one of the
// three sources is populated, and the provider adapter reads them in the order
// file_id > url > data (api-key style gateways differ per vendor; the resolver
// picks the form the target declared, see internal/provider/image_resolve.go).
//
// It is never what the event log stores (see ImageRefContent): Data holds base64
// without the data: prefix; FileID is a provider-side file handle; URL is an
// absolute http(s) URL a provider may fetch.
type ImageContent struct {
	Type     string `json:"type"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	FileID   string `json:"fileId,omitempty"`
	URL      string `json:"url,omitempty"`
}

// WireForm reports which source this block carries ("file_id", "url", "base64",
// or "" when none is set). Providers use it for diagnostics; encoders switch on
// the fields directly.
func (c ImageContent) WireForm() string {
	switch {
	case c.FileID != "":
		return "file_id"
	case c.URL != "":
		return "url"
	case c.Data != "":
		return "base64"
	}
	return ""
}

// ImageRefContent is a durable reference to one stored image: the blob id plus
// the metadata a client needs to render and a provider needs to size-check it.
//
// The event log stores this shape and only this shape (keel-ga §3, "事件只存
// blobId"): the same image referenced from many sessions/turns is one blob row
// and one copy of the bytes. Resolution to a provider-usable form happens in the
// worker before the outbound request, against the target provider's declared
// capabilities.
//
// BlobID is the platform file id (uuid string). Size is the byte length of the
// original bytes (0 = unknown); Width/Height are informational (0 = unknown).
type ImageRefContent struct {
	Type     string `json:"type"`
	BlobID   string `json:"blobId"`
	MimeType string `json:"mimeType,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	// Name is a client-supplied display name (never used for routing).
	Name string `json:"name,omitempty"`
}

func (TextContent) isContent()     {}
func (ThinkingContent) isContent() {}
func (ToolCallContent) isContent() {}
func (ImageContent) isContent()    {}
func (ImageRefContent) isContent() {}

// MarshalJSON encodes a ToolCallContent, tolerating malformed Arguments. A
// model can stream syntactically invalid tool-call JSON (a truncated or
// duplicated key, e.g. `{"todos": []{}...`); such bytes are kept verbatim in
// Arguments so schema validation can report "not valid JSON" to the model, but
// json.RawMessage.MarshalJSON rejects them, which would otherwise abort every
// downstream serialization (session persistence, provider re-serialization) and
// take the whole turn down. To keep those paths crash-free we emit invalid
// arguments as a JSON string of the raw bytes: valid JSON that round-trips the
// original text. Well-formed arguments are emitted unchanged.
func (t ToolCallContent) MarshalJSON() ([]byte, error) {
	args := t.Arguments
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage("{}")
	} else if !json.Valid(args) {
		s, err := json.Marshal(string(args))
		if err != nil {
			return nil, fmt.Errorf("content: encode invalid tool arguments: %w", err)
		}
		args = s
	}
	// A named alias avoids recursing into this MarshalJSON.
	type wire struct {
		Type             string          `json:"type"`
		ID               string          `json:"id"`
		Name             string          `json:"name"`
		Arguments        json.RawMessage `json:"arguments"`
		ThoughtSignature string          `json:"thoughtSignature,omitempty"`
	}
	return json.Marshal(wire{
		Type:             t.Type,
		ID:               t.ID,
		Name:             t.Name,
		Arguments:        args,
		ThoughtSignature: t.ThoughtSignature,
	})
}

// Constructors set the Type discriminant so callers never desync it.

// NewTextContent returns a TextContent with the type discriminant set.
func NewTextContent(text string) TextContent {
	return TextContent{Type: ContentTypeText, Text: text}
}

// NewThinkingContent returns a ThinkingContent with the type discriminant set.
func NewThinkingContent(thinking string) ThinkingContent {
	return ThinkingContent{Type: ContentTypeThinking, Thinking: thinking}
}

// NewToolCallContent returns a ToolCallContent with the type discriminant set.
func NewToolCallContent(id, name string, arguments json.RawMessage) ToolCallContent {
	return ToolCallContent{Type: ContentTypeToolCall, ID: id, Name: name, Arguments: arguments}
}

// NewImageContent returns an ImageContent with the type discriminant set.
func NewImageContent(data, mimeType string) ImageContent {
	return ImageContent{Type: ContentTypeImage, Data: data, MimeType: mimeType}
}

// NewImageRefContent returns an ImageRefContent with the type discriminant set.
// An empty blobID is refused by the constructor's caller (the blob is the whole
// reference, so a ref without one is not a reference); validation lives one
// layer up where the error can name the offending input.
func NewImageRefContent(blobID, mimeType string) ImageRefContent {
	return ImageRefContent{Type: ContentTypeImageRef, BlobID: blobID, MimeType: mimeType}
}

// decodeContent peeks at the "type" field of a JSON object and decodes it into
// the matching concrete Content struct. This is the single dispatch point used
// by every container that holds Content (messages, tool results, session
// entries, provider parsing).
func decodeContent(raw json.RawMessage) (Content, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("content: peek type: %w", err)
	}
	switch probe.Type {
	case ContentTypeText:
		var c TextContent
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		return c, nil
	case ContentTypeThinking:
		var c ThinkingContent
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		return c, nil
	case ContentTypeToolCall:
		var c ToolCallContent
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		return c, nil
	case ContentTypeImage:
		var c ImageContent
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		return c, nil
	case ContentTypeImageRef:
		var c ImageRefContent
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		return c, nil
	case "":
		return nil, fmt.Errorf("content: missing type discriminant")
	default:
		return nil, fmt.Errorf("content: unknown type %q", probe.Type)
	}
}

// ContentList is a slice of Content with discriminated JSON (un)marshalling.
// Fields typed []Content in messages use this so decoding dispatches on "type".
type ContentList []Content

// UnmarshalJSON decodes a JSON array of content blocks, dispatching each element
// on its "type" discriminant.
func (cl *ContentList) UnmarshalJSON(data []byte) error {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return err
	}
	out := make(ContentList, 0, len(raws))
	for i, raw := range raws {
		c, err := decodeContent(raw)
		if err != nil {
			return fmt.Errorf("content[%d]: %w", i, err)
		}
		out = append(out, c)
	}
	*cl = out
	return nil
}
