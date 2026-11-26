package superagent_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
)

func TestGitHubSourcePollsIssuesAndComments(t *testing.T) {
	var mu sync.Mutex
	var issueSince, commentSince string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/repos/acme/repo/issues":
			issueSince = r.URL.Query().Get("since")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[
				{"number":7,"title":"Fix it","body":"body","html_url":"https://github.com/acme/repo/issues/7",
				 "state":"open","created_at":"2026-11-25T01:00:00Z","updated_at":"2026-11-25T02:00:00Z",
				 "user":{"login":"alice","id":11,"type":"User"}}
			]`)
		case "/repos/acme/repo/issues/comments":
			commentSince = r.URL.Query().Get("since")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[
				{"id":99,"body":"a comment","html_url":"https://github.com/acme/repo/issues/7#issuecomment-99",
				 "issue_url":"https://api.github.com/repos/acme/repo/issues/7",
				 "created_at":"2026-11-25T01:30:00Z","updated_at":"2026-11-25T03:00:00Z",
				 "user":{"login":"alice","id":11,"type":"User"}}
			]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tenant := uuid.New()
	source, err := superagent.NewGitHubSource(superagent.GitHubConfig{
		TenantID: tenant, Repo: "acme/repo", BaseURL: server.URL, Token: "secret-token",
	})
	if err != nil {
		t.Fatalf("NewGitHubSource: %v", err)
	}
	page, err := source.Poll(context.Background(), tenant, "2026-11-25T00:00:00Z", 50)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if issueSince != "2026-11-25T00:00:00Z" || commentSince != "2026-11-25T00:00:00Z" {
		t.Fatalf("since values = %q/%q", issueSince, commentSince)
	}
	if len(page.Events) != 2 {
		t.Fatalf("events = %d, want 2: %+v", len(page.Events), page.Events)
	}
	if page.NextCursor != "2026-11-25T03:00:00Z" {
		t.Fatalf("next cursor = %q, want 03:00:00Z", page.NextCursor)
	}
	if page.Events[0].ExternalID != "issue:7:issue_opened:1795572000" && !strings.HasPrefix(page.Events[0].ExternalID, "issue:7:") {
		t.Fatalf("issue event id = %q", page.Events[0].ExternalID)
	}
	var payload superagent.EventPayload
	if err := json.Unmarshal(page.Events[0].Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Thread.ID != "acme/repo#7" || payload.Actor.Login != "alice" {
		t.Fatalf("payload = %+v", payload)
	}
	if page.Events[1].ExternalID != "comment:99" {
		t.Fatalf("comment event id = %q", page.Events[1].ExternalID)
	}
}

// TestGitHubSourceIgnoresOwnChannelComments pins the live-loop fix: GitHub
// attributes the Channel's comments to the token's owner, so without the
// marker filter the deployment reads its own acceptance/question/completion
// comments back as trusted follow-ups. The filtered comment must still advance
// the cursor, or the page would grow until per_page hides newer human input.
func TestGitHubSourceIgnoresOwnChannelComments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/repo/issues":
			_, _ = io.WriteString(w, `[
				{"number":7,"title":"Fix it","body":"body","html_url":"https://github.com/acme/repo/issues/7",
				 "state":"open","created_at":"2026-11-25T01:00:00Z","updated_at":"2026-11-25T04:30:00Z",
				 "user":{"login":"alice","id":11,"type":"User"}}
			]`)
		case "/repos/acme/repo/issues/comments":
			_, _ = io.WriteString(w, `[
				{"id":99,"body":"a human comment","html_url":"https://github.com/acme/repo/issues/7#issuecomment-99",
				 "issue_url":"https://api.github.com/repos/acme/repo/issues/7",
				 "created_at":"2026-11-25T03:00:00Z","updated_at":"2026-11-25T03:00:00Z",
				 "user":{"login":"alice","id":11,"type":"User"}},
				{"id":100,"body":"Ticket 123 accepted\n\n<!-- keel-message:65e8a7ff-0000-0000-0000-000000000000 -->",
				 "html_url":"https://github.com/acme/repo/issues/7#issuecomment-100",
				 "issue_url":"https://api.github.com/repos/acme/repo/issues/7",
				 "created_at":"2026-11-25T04:00:00Z","updated_at":"2026-11-25T04:00:00Z",
				 "user":{"login":"deploy-bot","id":12,"type":"User"}}
			]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tenant := uuid.New()
	source, err := superagent.NewGitHubSource(superagent.GitHubConfig{
		TenantID: tenant, Repo: "acme/repo", BaseURL: server.URL, Token: "secret-token",
	})
	if err != nil {
		t.Fatalf("NewGitHubSource: %v", err)
	}
	page, err := source.Poll(context.Background(), tenant, "2026-11-25T00:00:00Z", 50)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("events = %d, want issue + one human comment: %+v", len(page.Events), page.Events)
	}
	for _, event := range page.Events {
		if event.ExternalID == "comment:100" {
			t.Fatal("the deployment's own marked comment was returned as a source event")
		}
	}
	if page.NextCursor != "2026-11-25T04:30:00Z" {
		t.Fatalf("next cursor = %q, want the ignored comment's newer timestamp to advance it", page.NextCursor)
	}
}

func TestGitHubSourceBaselineCursor(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	tenant := uuid.New()
	source, err := superagent.NewGitHubSource(superagent.GitHubConfig{
		TenantID: tenant, Repo: "acme/repo", BaseURL: "http://example.invalid",
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewGitHubSource: %v", err)
	}
	page, err := source.Poll(context.Background(), tenant, "", 50)
	if err != nil {
		t.Fatalf("Poll baseline: %v", err)
	}
	if len(page.Events) != 0 || page.NextCursor != "2026-11-25T12:00:00Z" {
		t.Fatalf("baseline page = %+v", page)
	}
}

func TestGitHubChannelWritesCommentWithMarker(t *testing.T) {
	var path, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[]`)
			return
		}
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	tenant := uuid.New()
	source, err := superagent.NewGitHubSource(superagent.GitHubConfig{
		TenantID: tenant, Repo: "acme/repo", BaseURL: server.URL, Token: "secret-token",
	})
	if err != nil {
		t.Fatalf("NewGitHubSource: %v", err)
	}
	err = source.Send(context.Background(), tenant, "acme/repo#7", collabMessage("fixing now", "m-1"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if path != "/repos/acme/repo/issues/7/comments" {
		t.Fatalf("path = %q", path)
	}
	if !strings.Contains(body, "fixing now") || !strings.Contains(body, "keel-message:m-1") {
		t.Fatalf("body = %q", body)
	}
}

func TestGitHubChannelSkipsCommentWhenMarkerExists(t *testing.T) {
	var posted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[{"id":1,"body":"old\n\n<!-- keel-message:m-1 -->","issue_url":"https://api.github.com/repos/acme/repo/issues/7"}]`)
			return
		}
		posted = true
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	tenant := uuid.New()
	source, err := superagent.NewGitHubSource(superagent.GitHubConfig{
		TenantID: tenant, Repo: "acme/repo", BaseURL: server.URL, Token: "secret-token",
	})
	if err != nil {
		t.Fatalf("NewGitHubSource: %v", err)
	}
	if err := source.Send(context.Background(), tenant, "acme/repo#7", collabMessage("retry", "m-1")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if posted {
		t.Fatal("a marker-bearing retry must not POST a second comment")
	}
}

func TestGitHubChannelRejectsOtherRepo(t *testing.T) {
	tenant := uuid.New()
	source, err := superagent.NewGitHubSource(superagent.GitHubConfig{
		TenantID: tenant, Repo: "acme/repo", BaseURL: "http://example.invalid",
	})
	if err != nil {
		t.Fatalf("NewGitHubSource: %v", err)
	}
	if err := source.Send(context.Background(), tenant, "other/repo#7", collabMessage("x", "m")); err == nil {
		t.Fatal("Send to another repository must fail")
	}
}

func TestGitHubChannelRejectsOtherTenant(t *testing.T) {
	tenant := uuid.New()
	source, err := superagent.NewGitHubSource(superagent.GitHubConfig{
		TenantID: tenant, Repo: "acme/repo", BaseURL: "http://example.invalid",
	})
	if err != nil {
		t.Fatalf("NewGitHubSource: %v", err)
	}
	if err := source.Send(context.Background(), uuid.New(), "acme/repo#7", collabMessage("x", "m")); err == nil {
		t.Fatal("Send from a foreign tenant must fail")
	}
}

func collabMessage(text, messageID string) collab.ChannelMessage {
	return collab.ChannelMessage{
		Text: text,
		Data: json.RawMessage(`{"messageId":"` + messageID + `"}`),
	}
}
