package superagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
)

// GitHubSource is the first real Source/Channel adapter. It is an ordinary
// collab.Source/collab.Channel implementation with no privileged access: a
// deployment that does not configure a repository leaves the super-agent mode
// disabled.
//
// The MVP reads issues (including pull requests) and issue/PR comments. CI
// status is deliberately not polled in this slice: the EventCIStatus kind is
// reserved, and a later adapter can emit it without changing the orchestrator.
// Real GitHub E2E is not part of the automated suite; the adapter is tested
// against an httptest server.
type GitHubConfig struct {
	// TenantID is the deployment's tenant. Poll/Send refuse another tenant.
	TenantID uuid.UUID
	// Repo is "owner/name".
	Repo string
	// Token is a GitHub token. Empty works for public reads but is required for
	// writes. It must be a USER token (classic PAT or `gh auth token`): the
	// Source resolves GET /user to recognise the deployment's own comments, and
	// a GitHub App installation token has no user login, so it is refused
	// rather than guessed. It is never logged or included in an error.
	Token string
	// BaseURL defaults to https://api.github.com.
	BaseURL string
	// HTTPClient defaults to a client with a 20s timeout.
	HTTPClient *http.Client
	// PerPage bounds one list request (1..100, default 50). The adapter follows
	// the API's Link rel="next" pages (bounded), so a window larger than one
	// page is not truncated.
	PerPage int
	// InitialCursor seeds the read position when no durable cursor exists. Empty
	// uses the process clock: the first poll establishes a baseline instead of
	// replaying the repository's whole history.
	InitialCursor string
	// Now overrides the clock (tests).
	Now func() time.Time
}

// GitHubSource implements both halves of the GitHub surface.
type GitHubSource struct {
	cfg  GitHubConfig
	base *url.URL

	// selfMu guards the cached token-owner login. The Source must know whose
	// comments are its own writes: GitHub attributes them to the token owner,
	// and the policy otherwise trusts that account like a human.
	selfMu     sync.Mutex
	selfLogin  string
	selfAt     time.Time
	selfLookup bool
}

var _ collab.Source = (*GitHubSource)(nil)
var _ collab.Channel = (*GitHubSource)(nil)

// NewGitHubSource validates the configuration and builds the adapter.
func NewGitHubSource(cfg GitHubConfig) (*GitHubSource, error) {
	if cfg.TenantID == uuid.Nil {
		return nil, fmt.Errorf("superagent: github tenant is required")
	}
	parts := strings.Split(strings.TrimSpace(cfg.Repo), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("superagent: github repo must be owner/name")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.github.com"
	}
	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("superagent: github base url: %w", err)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 20 * time.Second}
	}
	if cfg.PerPage <= 0 || cfg.PerPage > 100 {
		cfg.PerPage = 50
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	return &GitHubSource{cfg: cfg, base: base}, nil
}

// Poll reads issues and comments updated at or after the cursor. The cursor is
// an RFC3339 timestamp; an empty cursor establishes a baseline and returns no
// events so a fresh deployment does not turn the repository history into
// tickets.
func (g *GitHubSource) Poll(ctx context.Context, tenantID uuid.UUID, cursor string, limit int) (collab.SourcePage, error) {
	if tenantID != g.cfg.TenantID {
		return collab.SourcePage{}, fmt.Errorf("%w: source belongs to another tenant", collab.ErrTenantMismatch)
	}
	if limit <= 0 {
		limit = g.cfg.PerPage
	}
	perPage := g.cfg.PerPage
	if limit < perPage {
		perPage = limit
	}
	since := strings.TrimSpace(cursor)
	if since == "" {
		since = strings.TrimSpace(g.cfg.InitialCursor)
	}
	if since == "" {
		baseline := g.cfg.Now().UTC().Format(time.RFC3339)
		return collab.SourcePage{NextCursor: baseline}, nil
	}
	sinceTime, err := time.Parse(time.RFC3339, since)
	if err != nil {
		return collab.SourcePage{}, fmt.Errorf("superagent: github cursor is not RFC3339: %w", err)
	}

	// Resolve the deployment's own identity before reading anything: without
	// it we cannot tell our own comments from a human's, and ingesting them is
	// worse than failing this poll (the next tick retries).
	if _, err := g.selfLoginOf(ctx); err != nil {
		return collab.SourcePage{}, err
	}
	issues, issueLatest, err := g.listIssues(ctx, sinceTime, perPage)
	if err != nil {
		return collab.SourcePage{}, err
	}
	comments, commentLatest, err := g.listComments(ctx, sinceTime, perPage)
	if err != nil {
		return collab.SourcePage{}, err
	}
	events := append(issues, comments...)
	next := sinceTime
	// The cursor advances past everything the page returned, including the
	// Channel's own comments the Source deliberately ignores. A filtered
	// comment was still consumed: leaving the cursor behind re-reads an
	// ever-growing page, and the per_page cap would eventually hide newer
	// human comments.
	for _, latest := range []time.Time{issueLatest, commentLatest} {
		if latest.After(next) {
			next = latest
		}
	}
	if len(events) == 0 && issueLatest.IsZero() && commentLatest.IsZero() {
		// An empty page does not move the cursor: a late-arriving event with an
		// older timestamp is still read on the next poll.
		next = sinceTime
	}
	return collab.SourcePage{Events: events, NextCursor: next.UTC().Format(time.RFC3339)}, nil
}

type githubIssue struct {
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	HTMLURL     string     `json:"html_url"`
	State       string     `json:"state"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	User        githubUser `json:"user"`
	PullRequest *struct{}  `json:"pull_request,omitempty"`
}

type githubComment struct {
	ID        int64      `json:"id"`
	Body      string     `json:"body"`
	HTMLURL   string     `json:"html_url"`
	IssueURL  string     `json:"issue_url"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	User      githubUser `json:"user"`
}

type githubUser struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}

func (g *GitHubSource) listIssues(ctx context.Context, since time.Time, perPage int) ([]collab.SourceEvent, time.Time, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/issues", g.base.String(), g.cfg.Repo)
	values := url.Values{}
	values.Set("state", "all")
	values.Set("sort", "updated")
	values.Set("direction", "asc")
	values.Set("since", since.UTC().Format(time.RFC3339))
	values.Set("per_page", strconv.Itoa(perPage))
	var issues []githubIssue
	if err := g.listPaged(ctx, endpoint+"?"+values.Encode(), func(batch []byte) error {
		var page []githubIssue
		if err := json.Unmarshal(batch, &page); err != nil {
			return err
		}
		issues = append(issues, page...)
		return nil
	}); err != nil {
		return nil, time.Time{}, err
	}
	events := make([]collab.SourceEvent, 0, len(issues))
	latest := time.Time{}
	for _, issue := range issues {
		if issue.UpdatedAt.After(latest) {
			latest = issue.UpdatedAt
		}
		threadKind := "issue"
		kind := EventIssueOpened
		if issue.PullRequest != nil {
			threadKind = "pull_request"
			kind = EventPullRequestOpen
		}
		if !issue.CreatedAt.After(since) {
			kind = EventIssueUpdated
		}
		eventID := fmt.Sprintf("issue:%d:%s:%s", issue.Number, kind, issue.UpdatedAt.UTC().Format(time.RFC3339Nano))
		thread := ThreadRef{
			Source: SourceGitHub,
			Kind:   threadKind,
			ID:     g.cfg.Repo + "#" + strconv.Itoa(issue.Number),
			URL:    issue.HTMLURL,
		}
		payload := EventPayload{
			EventID: eventID,
			Kind:    kind,
			Actor: Actor{
				ID:    strconv.FormatInt(issue.User.ID, 10),
				Login: issue.User.Login,
				IsBot: strings.EqualFold(issue.User.Type, "Bot"),
			},
			Thread: thread,
			Title:  issue.Title,
			Body:   issue.Body,
			URL:    issue.HTMLURL,
		}
		events = append(events, collab.SourceEvent{
			SourceID:   SourceGitHub,
			Kind:       string(kind),
			ExternalID: eventID,
			Payload:    mustMarshal(payload),
			ReceivedAt: issue.UpdatedAt,
		})
	}
	return events, latest, nil
}

// channelMarkerPrefix starts the hidden marker GitHubChannel appends to every
// comment it writes. The Source ignores its own comments: GitHub attributes
// them to the token's owner account, and the policy otherwise trusts that
// account like a human.
const channelMarkerPrefix = "<!-- keel-message:"

// trailingMarkerLine matches the marker only as the final line, exactly as
// Send writes it. A trusted human who quotes the marker mid-comment is not
// filtered; only the deployment's own trailing marker is.
var trailingMarkerLine = regexp.MustCompile(`^<!-- keel-message:[A-Za-z0-9._:-]{1,200} -->$`)

func trailingChannelMarker(body string) bool {
	trimmed := strings.TrimRight(body, " \t\r\n")
	if trimmed == "" {
		return false
	}
	if index := strings.LastIndex(trimmed, "\n"); index >= 0 {
		trimmed = trimmed[index+1:]
	}
	return trailingMarkerLine.MatchString(strings.TrimSpace(trimmed))
}

// selfLoginOf resolves and caches the token owner's login. An empty token
// cannot write, so there is nothing to filter. A lookup failure is returned as
// an error: ingesting our own comments as trusted follow-ups is worse than
// pausing this poll, and the next tick retries. The cache avoids a /user call
// on every poll.
func (g *GitHubSource) selfLoginOf(ctx context.Context) (string, error) {
	if strings.TrimSpace(g.cfg.Token) == "" {
		return "", nil
	}
	g.selfMu.Lock()
	defer g.selfMu.Unlock()
	if g.selfLookup && g.cfg.Now().Sub(g.selfAt) < 30*time.Minute {
		return g.selfLogin, nil
	}
	var user githubUser
	if err := g.getJSON(ctx, g.base.String()+"/user", &user); err != nil {
		return "", fmt.Errorf("superagent: github token owner lookup: %w", err)
	}
	login := strings.TrimSpace(user.Login)
	if login == "" {
		return "", fmt.Errorf("superagent: github /user returned no login")
	}
	g.selfLogin = login
	g.selfAt = g.cfg.Now()
	g.selfLookup = true
	return login, nil
}

func (g *GitHubSource) listComments(ctx context.Context, since time.Time, perPage int) ([]collab.SourceEvent, time.Time, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/issues/comments", g.base.String(), g.cfg.Repo)
	values := url.Values{}
	values.Set("sort", "updated")
	values.Set("direction", "asc")
	values.Set("since", since.UTC().Format(time.RFC3339))
	values.Set("per_page", strconv.Itoa(perPage))
	self, err := g.selfLoginOf(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	var comments []githubComment
	if err := g.listPaged(ctx, endpoint+"?"+values.Encode(), func(batch []byte) error {
		var page []githubComment
		if err := json.Unmarshal(batch, &page); err != nil {
			return err
		}
		comments = append(comments, page...)
		return nil
	}); err != nil {
		return nil, time.Time{}, err
	}
	events := make([]collab.SourceEvent, 0, len(comments))
	latest := time.Time{}
	for _, comment := range comments {
		// The cursor must still move past a filtered comment (see Poll), so the
		// timestamp is read before the skip.
		if comment.UpdatedAt.After(latest) {
			latest = comment.UpdatedAt
		}
		if self != "" && strings.EqualFold(comment.User.Login, self) && trailingChannelMarker(comment.Body) {
			continue
		}
		number := lastPathSegment(comment.IssueURL)
		if number == "" {
			continue
		}
		eventID := fmt.Sprintf("comment:%d", comment.ID)
		thread := ThreadRef{
			Source: SourceGitHub,
			Kind:   "conversation",
			ID:     g.cfg.Repo + "#" + number,
			URL:    comment.HTMLURL,
		}
		payload := EventPayload{
			EventID: eventID,
			Kind:    EventIssueComment,
			Actor: Actor{
				ID:    strconv.FormatInt(comment.User.ID, 10),
				Login: comment.User.Login,
				IsBot: strings.EqualFold(comment.User.Type, "Bot"),
			},
			Thread: thread,
			Body:   comment.Body,
			URL:    comment.HTMLURL,
		}
		events = append(events, collab.SourceEvent{
			SourceID:   SourceGitHub,
			Kind:       string(EventIssueComment),
			ExternalID: eventID,
			Payload:    mustMarshal(payload),
			ReceivedAt: comment.UpdatedAt,
		})
	}
	return events, latest, nil
}

func (g *GitHubSource) getJSON(ctx context.Context, endpoint string, target any) error {
	_, err := g.getJSONPage(ctx, endpoint, target)
	return err
}

// maxListPages bounds one endpoint's pagination in one poll. With the default
// or maximum per_page this covers 1000-2000 items; a window larger than that is
// pathological and the next poll starts from the returned cursor anyway.
const maxListPages = 20

// listPaged follows the API's Link rel="next" pages and hands each raw page to
// decode. GitHub caps a single response at per_page items, so without this a
// busy window (>50 issues or comments) would silently skip the tail. A page URL
// that leaves the configured API host fails closed instead of being followed.
func (g *GitHubSource) listPaged(ctx context.Context, endpoint string, decode func([]byte) error) error {
	next := endpoint
	for page := 0; page < maxListPages && next != ""; page++ {
		body, follow, err := g.getPage(ctx, next)
		if err != nil {
			return err
		}
		if err := decode(body); err != nil {
			return fmt.Errorf("superagent: decode github response: %w", err)
		}
		next = ""
		if follow == "" {
			continue
		}
		parsed, err := url.Parse(follow)
		if err != nil || parsed.Scheme != g.base.Scheme || parsed.Host != g.base.Host {
			return fmt.Errorf("superagent: github pagination left the API host: %q", follow)
		}
		next = follow
	}
	return nil
}

func (g *GitHubSource) getPage(ctx context.Context, endpoint string) ([]byte, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", fmt.Errorf("superagent: github request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.cfg.Token != "" {
		request.Header.Set("Authorization", "Bearer "+g.cfg.Token)
	}
	response, err := g.cfg.HTTPClient.Do(request)
	if err != nil {
		return nil, "", fmt.Errorf("superagent: github request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", githubStatusError("read", response)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, "", fmt.Errorf("superagent: read github response: %w", err)
	}
	return body, nextLink(response.Header.Get("Link")), nil
}

func (g *GitHubSource) getJSONPage(ctx context.Context, endpoint string, target any) (string, error) {
	body, follow, err := g.getPage(ctx, endpoint)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(body, target); err != nil {
		return "", fmt.Errorf("superagent: decode github response: %w", err)
	}
	return follow, nil
}

// nextLink returns the rel="next" target of a GitHub Link header.
func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		segments := strings.Split(part, ";")
		if len(segments) < 2 {
			continue
		}
		target := strings.TrimSpace(segments[0])
		rel := ""
		for _, attribute := range segments[1:] {
			attribute = strings.TrimSpace(attribute)
			if strings.HasPrefix(strings.ToLower(attribute), "rel=") {
				rel = strings.Trim(strings.TrimPrefix(attribute, "rel="), `"`)
			}
		}
		if rel != "next" {
			continue
		}
		if strings.HasPrefix(target, "<") && strings.HasSuffix(target, ">") {
			return strings.TrimSuffix(strings.TrimPrefix(target, "<"), ">")
		}
	}
	return ""
}

// Send writes one comment back to the issue/PR thread. channelID is
// "owner/repo#number" or a bare issue number for the configured repository.
func (g *GitHubSource) Send(ctx context.Context, tenantID uuid.UUID, channelID string, message collab.ChannelMessage) error {
	if tenantID != g.cfg.TenantID {
		return fmt.Errorf("%w: channel belongs to another tenant", collab.ErrTenantMismatch)
	}
	repo, number, err := g.parseChannel(channelID)
	if err != nil {
		return err
	}
	body := message.Text
	messageID := messageIDFromData(message.Data)
	if messageID != "" {
		// GitHub has no idempotency key. Before an ambiguous retry, look for our
		// own hidden marker; a found marker means the previous attempt actually
		// landed. A failed read refuses the write rather than risking a duplicate.
		exists, err := g.commentWithMarkerExists(ctx, repo, number, messageID)
		if err != nil {
			return err
		}
		if exists {
			return nil
		}
		body += "\n\n" + channelMarkerPrefix + messageID + " -->"
	}
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return fmt.Errorf("superagent: encode github comment: %w", err)
	}
	endpoint := fmt.Sprintf("%s/repos/%s/issues/%s/comments", g.base.String(), repo, number)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("superagent: github request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.cfg.Token != "" {
		request.Header.Set("Authorization", "Bearer "+g.cfg.Token)
	}
	response, err := g.cfg.HTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("superagent: github comment failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return githubStatusError("write", response)
	}
	return nil
}

func (g *GitHubSource) parseChannel(channelID string) (string, string, error) {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return "", "", fmt.Errorf("superagent: github channel id is required")
	}
	repo := g.cfg.Repo
	number := channelID
	if strings.Contains(channelID, "#") {
		parts := strings.SplitN(channelID, "#", 2)
		if strings.TrimSpace(parts[0]) != "" {
			repo = strings.TrimSpace(parts[0])
		}
		number = strings.TrimSpace(parts[1])
	}
	if repo != g.cfg.Repo {
		// The adapter is configured for one repository. A comment must never be
		// written to a thread from another repo, even if a caller names it.
		return "", "", fmt.Errorf("superagent: github channel %q is outside the configured repository", channelID)
	}
	if _, err := strconv.Atoi(number); err != nil {
		return "", "", fmt.Errorf("superagent: github channel %q has no issue number", channelID)
	}
	return repo, number, nil
}

func (g *GitHubSource) commentWithMarkerExists(ctx context.Context, repo, number, messageID string) (bool, error) {
	self, err := g.selfLoginOf(ctx)
	if err != nil {
		return false, err
	}
	if self == "" {
		// Without a token the write itself cannot succeed; there is no prior
		// comment this deployment could have left.
		return false, nil
	}
	endpoint := fmt.Sprintf("%s/repos/%s/issues/%s/comments", g.base.String(), repo, number)
	values := url.Values{}
	values.Set("sort", "created")
	values.Set("direction", "desc")
	values.Set("per_page", "100")
	var comments []githubComment
	if err := g.getJSON(ctx, endpoint+"?"+values.Encode(), &comments); err != nil {
		return false, fmt.Errorf("superagent: check github comment marker: %w", err)
	}
	marker := "keel-message:" + messageID
	for _, comment := range comments {
		// Only our own trailing marker is evidence of a landed write; a human
		// quoting the same id mid-comment must not suppress the retry.
		if !strings.EqualFold(comment.User.Login, self) || !trailingChannelMarker(comment.Body) {
			continue
		}
		if strings.Contains(comment.Body, marker) {
			return true, nil
		}
	}
	return false, nil
}

func githubStatusError(operation string, response *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	detail := strings.TrimSpace(string(snippet))
	if len(detail) > 300 {
		detail = detail[:300]
	}
	if detail == "" {
		return fmt.Errorf("superagent: github %s returned %d", operation, response.StatusCode)
	}
	return fmt.Errorf("superagent: github %s returned %d: %s", operation, response.StatusCode, detail)
}

func lastPathSegment(raw string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return ""
	}
	index := strings.LastIndex(trimmed, "/")
	if index < 0 {
		return ""
	}
	return trimmed[index+1:]
}

func messageIDFromData(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var data struct {
		MessageID string `json:"messageId"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return ""
	}
	return data.MessageID
}

func mustMarshal(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return raw
}
