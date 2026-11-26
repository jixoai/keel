// Package attachment defines the client-facing attachment reference of a
// user.message and its field-level validation (change keel-plugin-attachment).
//
// The attachment lifecycle is not a second file system: bytes live in
// internal/blob, the event log stores only a fileId (blob id) plus display
// metadata, and the model dispatch in internal/agent decides native vs text
// fallback. This package is the one shape shared by POST /v1/sessions/{id}/input,
// the MCP Apps ui/message bridge and retry, so the three paths cannot drift.
package attachment

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Bounds of one input's attachment list. MaxPerInput matches the MCP Apps
// contract (`mcpAppMaxAttachments`) and the spec's "≤16/输入".
const (
	MaxPerInput  = 16
	NameMaxBytes = 256
	MimeMaxBytes = 128
)

// Ref is the canonical attachment reference a client sends and the event log
// persists. The shape is deliberately closed: only these fields are stored;
// unknown client fields (for example a display-only `url`) are ignored.
type Ref struct {
	// FileID is the blob id returned by POST /v1/files. Empty is the one
	// tolerated degradation: the model receives an explicit notice.
	FileID string `json:"fileId,omitempty"`
	// Name is display metadata only; it never carries path semantics.
	Name string `json:"name,omitempty"`
	// Mime is the declared MIME. The stored blob record is the authority; the
	// commit path canonicalizes this field to it.
	Mime string `json:"mime,omitempty"`
	// Size is the stored byte count, filled from the blob record.
	Size int64 `json:"size,omitempty"`
	// Width and Height are optional display facts supplied by the client.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
}

// FieldError is a field-level refusal. Field is a path a client can map back
// to its payload (`attachments[2].fileId`); Status is the HTTP status the
// transport should use (400 for a malformed reference, 413 for a size/quota
// refusal).
type FieldError struct {
	Field   string
	Message string
	Status  int
}

// Error implements error; the message starts with the field path.
func (e *FieldError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

// HTTPStatus is the wire status of the refusal.
func (e *FieldError) HTTPStatus() int {
	if e == nil || e.Status == 0 {
		return 400
	}
	return e.Status
}

// InputField is the field path, for transports that report one.
func (e *FieldError) InputField() string {
	if e == nil {
		return ""
	}
	return e.Field
}

func fieldError(status int, format string, args ...any) *FieldError {
	return &FieldError{Status: status, Message: fmt.Sprintf(format, args...)}
}

// ParseRefs decodes and validates the attachments array. It returns (nil, nil)
// for an absent/null value and an empty slice for `[]`. Every failure is a
// *FieldError; unknown object fields are ignored (the stored shape stays the
// closed Ref).
func ParseRefs(raw json.RawMessage) ([]Ref, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	if trimmed[0] != '[' {
		err := fieldError(400, "attachments must be an array")
		err.Field = "attachments"
		return nil, err
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		err := fieldError(400, "attachments must be an array")
		err.Field = "attachments"
		return nil, err
	}
	if len(items) > MaxPerInput {
		err := fieldError(400, "at most %d attachments are accepted per input", MaxPerInput)
		err.Field = "attachments"
		return nil, err
	}
	refs := make([]Ref, 0, len(items))
	for index, item := range items {
		ref, err := parseRef(index, item)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	if refs == nil {
		refs = []Ref{}
	}
	return refs, nil
}

func parseRef(index int, item json.RawMessage) (Ref, error) {
	path := func(name string) string { return fmt.Sprintf("attachments[%d].%s", index, name) }
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(item, &fields); err != nil || fields == nil {
		err := fieldError(400, "each attachment must be an object")
		err.Field = fmt.Sprintf("attachments[%d]", index)
		return Ref{}, err
	}
	ref := Ref{}
	if raw, ok := fields["fileId"]; ok {
		value, err := optionalString(raw)
		if err != nil {
			return Ref{}, &FieldError{Field: path("fileId"), Message: "fileId must be a string"}
		}
		value = strings.TrimSpace(value)
		if value != "" {
			id, parseErr := uuid.Parse(value)
			if parseErr != nil || id == uuid.Nil {
				// The same field error for malformed, unknown, foreign and
				// incomplete ids: the input path must not tell a caller whether
				// an id exists in another tenant.
				return Ref{}, &FieldError{Field: path("fileId"), Message: "fileId must name a stored file"}
			}
			ref.FileID = id.String()
		}
	}
	if raw, ok := fields["name"]; ok {
		value, err := optionalString(raw)
		if err != nil {
			return Ref{}, &FieldError{Field: path("name"), Message: "name must be a string"}
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return Ref{}, &FieldError{Field: path("name"), Message: "name must be 1..256 bytes and is display-only"}
		}
		if len(value) > NameMaxBytes {
			return Ref{}, &FieldError{Field: path("name"), Message: fmt.Sprintf("name must be at most %d bytes", NameMaxBytes)}
		}
		ref.Name = value
	}
	if raw, ok := fields["mime"]; ok {
		value, err := optionalString(raw)
		if err != nil {
			return Ref{}, &FieldError{Field: path("mime"), Message: "mime must be a string"}
		}
		value = strings.TrimSpace(value)
		if len(value) > MimeMaxBytes {
			return Ref{}, &FieldError{Field: path("mime"), Message: fmt.Sprintf("mime must be at most %d bytes", MimeMaxBytes)}
		}
		ref.Mime = value
	}
	if raw, ok := fields["size"]; ok {
		var size int64
		if err := json.Unmarshal(raw, &size); err != nil || size < 0 {
			return Ref{}, &FieldError{Field: path("size"), Message: "size must be a non-negative integer"}
		}
		ref.Size = size
	}
	if raw, ok := fields["width"]; ok {
		var width int
		if err := json.Unmarshal(raw, &width); err != nil || width < 0 {
			return Ref{}, &FieldError{Field: path("width"), Message: "width must be a non-negative integer"}
		}
		ref.Width = width
	}
	if raw, ok := fields["height"]; ok {
		var height int
		if err := json.Unmarshal(raw, &height); err != nil || height < 0 {
			return Ref{}, &FieldError{Field: path("height"), Message: "height must be a non-negative integer"}
		}
		ref.Height = height
	}
	return ref, nil
}

// optionalString decodes a JSON string, treating null as absent.
func optionalString(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", err
	}
	return value, nil
}

// MarshalRefs encodes the canonical stored shape. An empty list is omitted by
// the caller rather than persisted as `[]`.
func MarshalRefs(refs []Ref) (json.RawMessage, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	return json.Marshal(refs)
}

// NormalizeMime lowercases a MIME type and strips its parameters, matching the
// provider dispatch path's normalization. It is the comparison used between a
// client's declared MIME and the blob record's authoritative one.
func NormalizeMime(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if index := strings.IndexByte(value, ';'); index >= 0 {
		value = strings.TrimSpace(value[:index])
	}
	return value
}
