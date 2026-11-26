package superagent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
)

// TestGitHubChannelLiveMarkerDedupe is the only test that writes to a real
// GitHub repository. It is skipped unless the owner supplies a throwaway repo:
//
//	KEEL_LIVE_GITHUB_REPO=owner/name KEEL_LIVE_GITHUB_ISSUE=12 \
//	KEEL_LIVE_GITHUB_TOKEN=<token> go test ./internal/superagent -run LiveMarker
//
// It proves the Channel's idempotency rule against the live API: the second
// Send of the same durable message id finds its own hidden marker and returns
// without a second comment. The marker is what makes an ambiguous Outbox retry
// safe on a transport without idempotency keys.
func TestGitHubChannelLiveMarkerDedupe(t *testing.T) {
	repo := strings.TrimSpace(os.Getenv("KEEL_LIVE_GITHUB_REPO"))
	number := strings.TrimSpace(os.Getenv("KEEL_LIVE_GITHUB_ISSUE"))
	token := strings.TrimSpace(os.Getenv("KEEL_LIVE_GITHUB_TOKEN"))
	if repo == "" || number == "" || token == "" {
		t.Skip("KEEL_LIVE_GITHUB_REPO / KEEL_LIVE_GITHUB_ISSUE / KEEL_LIVE_GITHUB_TOKEN not set")
	}
	if _, err := strconv.Atoi(number); err != nil {
		t.Fatalf("KEEL_LIVE_GITHUB_ISSUE is not a number: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tenant := uuid.New()
	source, err := superagent.NewGitHubSource(superagent.GitHubConfig{
		TenantID: tenant, Repo: repo, Token: token,
	})
	if err != nil {
		t.Fatalf("NewGitHubSource: %v", err)
	}
	messageID := "live-marker-" + uuid.NewString()
	data := json.RawMessage(fmt.Sprintf(`{"messageId":%q}`, messageID))
	message := collab.ChannelMessage{
		Text: "Keel live E2E marker dedupe probe. This comment must appear exactly once.",
		Data: data,
	}
	channelID := repo + "#" + number

	if err := source.Send(ctx, tenant, channelID, message); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	first := countMarkerComments(t, ctx, repo, number, token, messageID)
	if first != 1 {
		t.Fatalf("comments with marker after first Send = %d, want 1", first)
	}
	if err := source.Send(ctx, tenant, channelID, message); err != nil {
		t.Fatalf("second Send: %v", err)
	}
	second := countMarkerComments(t, ctx, repo, number, token, messageID)
	if second != 1 {
		t.Fatalf("comments with marker after retry = %d, want 1 (marker dedupe failed)", second)
	}
	t.Logf("live marker dedupe: issue=%s/%s#%s messageId=%s comments=%d", "https://github.com", repo, number, messageID, second)
}

func countMarkerComments(t *testing.T, ctx context.Context, repo, number, token, messageID string) int {
	t.Helper()
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/issues/%s/comments?per_page=100&sort=created&direction=desc", repo, number)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("list comments: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		t.Fatalf("list comments: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(snippet)))
	}
	var comments []struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&comments); err != nil {
		t.Fatalf("decode comments: %v", err)
	}
	marker := "keel-message:" + messageID
	count := 0
	for _, comment := range comments {
		if strings.Contains(comment.Body, marker) {
			count++
		}
	}
	return count
}
