package attachment

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestParseRefsCanonicalizesTheClosedShape(t *testing.T) {
	id := uuid.New()
	raw := json.RawMessage(`[
	  {"fileId":"` + id.String() + `","name":"  report.pdf  ","mime":"application/pdf","size":12,"width":3,"height":4,"url":"data:text/plain,secret","extra":true},
	  {"name":"orphan.txt","mime":"text/plain"}
	]`)
	refs, err := ParseRefs(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("refs = %+v", refs)
	}
	first := refs[0]
	if first.FileID != id.String() || first.Name != "report.pdf" || first.Mime != "application/pdf" ||
		first.Size != 12 || first.Width != 3 || first.Height != 4 {
		t.Fatalf("first ref = %+v", first)
	}
	// The missing-fileId entry survives with only its display metadata: it is a
	// notice at transcript rebuild, never a rejected input.
	if refs[1].FileID != "" || refs[1].Name != "orphan.txt" {
		t.Fatalf("second ref = %+v", refs[1])
	}
	stored, err := MarshalRefs(refs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "url") || strings.Contains(string(stored), "extra") {
		t.Fatalf("unknown fields leaked into the stored shape: %s", stored)
	}
	if strings.Contains(string(stored), `"fileId":""`) {
		t.Fatalf("an absent fileId must be omitted: %s", stored)
	}
}

func TestParseRefsFieldErrors(t *testing.T) {
	id := uuid.NewString()
	cases := []struct {
		name  string
		raw   string
		field string
	}{
		{"not an array", `{"fileId":"` + id + `"}`, "attachments"},
		{"not an object", `["x"]`, "attachments[0]"},
		{"file id not a string", `[{"fileId":3}]`, "attachments[0].fileId"},
		{"file id not a uuid", `[{"fileId":"blob-1"}]`, "attachments[0].fileId"},
		{"name wrong type", `[{"name":1}]`, "attachments[0].name"},
		{"empty name", `[{"name":"   "}]`, "attachments[0].name"},
		{"long name", `[{"name":"` + strings.Repeat("a", NameMaxBytes+1) + `"}]`, "attachments[0].name"},
		{"long mime", `[{"mime":"` + strings.Repeat("a", MimeMaxBytes+1) + `"}]`, "attachments[0].mime"},
		{"negative size", `[{"size":-1}]`, "attachments[0].size"},
		{"fractional size", `[{"size":1.5}]`, "attachments[0].size"},
		{"negative width", `[{"width":-1}]`, "attachments[0].width"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := ParseRefs(json.RawMessage(testCase.raw))
			var fieldErr *FieldError
			if !errors.As(err, &fieldErr) {
				t.Fatalf("err = %v, want a FieldError", err)
			}
			if fieldErr.Field != testCase.field {
				t.Fatalf("field = %q, want %q", fieldErr.Field, testCase.field)
			}
			if fieldErr.HTTPStatus() != 400 {
				t.Fatalf("status = %d, want 400", fieldErr.HTTPStatus())
			}
		})
	}
}

func TestParseRefsCountBound(t *testing.T) {
	items := make([]map[string]any, MaxPerInput+1)
	for index := range items {
		items[index] = map[string]any{"name": "x", "mime": "text/plain"}
	}
	raw, _ := json.Marshal(items)
	_, err := ParseRefs(raw)
	var fieldErr *FieldError
	if !errors.As(err, &fieldErr) || fieldErr.Field != "attachments" || fieldErr.HTTPStatus() != 400 {
		t.Fatalf("err = %v", err)
	}
}

func TestParseRefsEmpty(t *testing.T) {
	for _, raw := range []string{"", "null", "[]"} {
		refs, err := ParseRefs(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if len(refs) != 0 {
			t.Fatalf("%q -> %+v", raw, refs)
		}
	}
}

func TestNormalizeMime(t *testing.T) {
	cases := map[string]string{
		"Application/PDF":             "application/pdf",
		" image/png; charset=binary ": "image/png",
		"text/plain;charset=utf-8":    "text/plain",
		"":                            "",
	}
	for input, want := range cases {
		if got := NormalizeMime(input); got != want {
			t.Fatalf("NormalizeMime(%q) = %q, want %q", input, got, want)
		}
	}
}
