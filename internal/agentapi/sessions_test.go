package agentapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestDeriveSessionTitle pins the derivation rule of the list title: user text
// only, whitespace collapsed (a multi-line prompt stays one line), an
// over-long text cut to the limit with an ellipsis, and nothing for an empty
// or whitespace-only first message.
func TestDeriveSessionTitle(t *testing.T) {
	long := strings.Repeat("字", sessionTitleLimit+40)
	exact := strings.Repeat("a", sessionTitleLimit)
	cases := []struct {
		name string
		text string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", " \n\t\r\n ", ""},
		{"plain", "hello world", "hello world"},
		{"trimmed", "   hello  ", "hello"},
		{"newlines collapse", "line one\nline two\n\nline three", "line one line two line three"},
		{"runs collapse", "spaced\t\tout   text", "spaced out text"},
		{"exact limit is untouched", exact, exact},
		{"one over the limit is cut", exact + "b", strings.Repeat("a", sessionTitleLimit-1) + "…"},
		{"long text is cut", long, string([]rune(long)[:sessionTitleLimit-1]) + "…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveSessionTitle(tc.text)
			if got != tc.want {
				t.Fatalf("deriveSessionTitle(%q) = %q, want %q", tc.text, got, tc.want)
			}
			if runes := []rune(got); len(runes) > sessionTitleLimit {
				t.Fatalf("title %q is %d runes, over the limit %d", got, len(runes), sessionTitleLimit)
			}
		})
	}
	if runes := []rune(deriveSessionTitle(long)); len(runes) != sessionTitleLimit {
		t.Fatalf("cut title is %d runes, want exactly %d", len(runes), sessionTitleLimit)
	}
}

// TestSessionListRowCarriesTitleAndCreatedAt pins the wire shape the client
// contract depends on: every row carries `title` (present even when empty, so
// the client can distinguish "no title yet" from an older server) and
// `createdAt`.
func TestSessionListRowCarriesTitleAndCreatedAt(t *testing.T) {
	tenant := uuid.New()
	created := time.Date(2026, 11, 6, 2, 30, 0, 0, time.UTC)
	lister := &fakeLister{sessions: []SessionSummary{
		{SessionID: uuid.NewString(), CreatedAt: created, UpdatedAt: created, Title: "做一件事"},
		{SessionID: uuid.NewString(), CreatedAt: created, UpdatedAt: created, Title: ""},
	}}
	server := httptest.NewServer(New(Config{Sessions: lister, Logger: testLogger()}))
	defer server.Close()

	var raw struct {
		Sessions []map[string]json.RawMessage `json:"sessions"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions", tenant, nil, &raw); code != 200 {
		t.Fatalf("list sessions: %d", code)
	}
	if len(raw.Sessions) != 2 {
		t.Fatalf("rows = %d, want 2", len(raw.Sessions))
	}
	for i, row := range raw.Sessions {
		title, ok := row["title"]
		if !ok {
			t.Fatalf("row %d has no title key: %v", i, row)
		}
		createdAt, ok := row["createdAt"]
		if !ok {
			t.Fatalf("row %d has no createdAt key: %v", i, row)
		}
		var titleText, createdText string
		if err := json.Unmarshal(title, &titleText); err != nil {
			t.Fatalf("row %d title is not a string: %s", i, title)
		}
		if err := json.Unmarshal(createdAt, &createdText); err != nil || createdText == "" {
			t.Fatalf("row %d createdAt = %s (err %v)", i, createdAt, err)
		}
		if i == 1 && titleText != "" {
			t.Fatalf("row 1 title = %q, want the empty string to be emitted", titleText)
		}
	}
}
