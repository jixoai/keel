package mcp

// The `_meta` size bound (P1-B): app metadata is advisory, so an oversized
// block is ignored as a whole with one diagnostic instead of being copied,
// parsed and published. The document itself stays usable.

import (
	"encoding/json"
	"strings"
	"testing"
)

func appResultWithMeta(meta json.RawMessage) ReadResourceResult {
	return ReadResourceResult{
		Contents: []ResourceContents{{
			URI:      "ui://weather/dashboard",
			MimeType: AppMIMEType,
			Text:     "<html>ok</html>",
			Meta:     meta,
		}},
	}
}

func TestValidateAppResourceIgnoresOversizedMeta(t *testing.T) {
	// The probe from the review: a 16 MiB `_meta` (the transport frame limit)
	// must not reach the parser.
	huge := json.RawMessage(`{"ui":{"padding":"` + strings.Repeat("x", 16<<20) + `"}}`)
	resource, err := ValidateAppResource(appResultWithMeta(huge))
	if err != nil {
		t.Fatalf("the document became unusable: %v", err)
	}
	if len(resource.Meta) != 0 {
		t.Fatalf("an oversized _meta was kept: %d bytes", len(resource.Meta))
	}
	if len(resource.Diagnostics) != 1 || !strings.Contains(resource.Diagnostics[0], "_meta ignored") {
		t.Fatalf("diagnostics = %v", resource.Diagnostics)
	}
	if resource.Text != "<html>ok</html>" {
		t.Fatalf("text = %q", resource.Text)
	}
}

func TestValidateAppResourceMetaLimitIsSummed(t *testing.T) {
	// Two individually-small blocks whose sum crosses the bound are ignored as
	// a whole: the limit is on what the server sent, not on the chosen copy.
	half := strings.Repeat("y", AppResourceMetaMaxBytes/2)
	result := ReadResourceResult{
		Contents: []ResourceContents{{
			URI:      "ui://weather/dashboard",
			MimeType: AppMIMEType,
			Text:     "<html>ok</html>",
			Meta:     json.RawMessage(`{"ui":{"a":"` + half + `"}}`),
		}},
		Meta: json.RawMessage(`{"ui":{"b":"` + half + `"}}`),
	}
	resource, err := ValidateAppResource(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(resource.Meta) != 0 || len(resource.Diagnostics) != 1 {
		t.Fatalf("meta=%d diagnostics=%v", len(resource.Meta), resource.Diagnostics)
	}
}

func TestValidateAppResourceKeepsBoundedMeta(t *testing.T) {
	meta := json.RawMessage(`{"ui":{"prefersBorder":true}}`)
	resource, err := ValidateAppResource(appResultWithMeta(meta))
	if err != nil {
		t.Fatal(err)
	}
	if string(resource.Meta) != string(meta) {
		t.Fatalf("meta = %s", resource.Meta)
	}
	if len(resource.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %v", resource.Diagnostics)
	}
}
