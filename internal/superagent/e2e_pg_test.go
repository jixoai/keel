package superagent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/superagent"
)

// TestSuperagentFixtureEndToEnd is the local fixture E2E: real PostgreSQL,
// fixture Source, fake worker session, HTTP Channel. It exercises
// Source -> Ticket -> Assignment -> Worker Ask -> Attention answer ->
// Channel writeback -> Report ready -> completed Ticket.
//
// It skips without KEEL_TEST_DSN, like the other PG integration tests.
func TestSuperagentFixtureEndToEnd(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "e2e-"+tenant.String()[:8]); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	channel := newHTTPChannel(t)
	trace := &e2eTrace{TenantID: tenant.String()}
	trace.Add("tenant", map[string]any{"tenantId": tenant.String(), "dsn": "local-temporary"})

	owner := collab.SessionRef{
		TenantID: tenant, SessionID: uuid.New(), BotID: "orchestrator",
		State: collab.SessionActive, CreatedAt: time.Now().UTC(),
	}
	if err := st.Put(ctx, owner); err != nil {
		t.Fatalf("Put owner: %v", err)
	}
	registry := e2eRegistry(t, tenant)
	source := superagent.NewFixtureSource(func() time.Time { return time.Now().UTC() })
	inputs := &recordingSink{}
	outbox, err := superagent.NewOutbox(superagent.OutboxOptions{
		Store: st, Channel: channel, Audit: &fakeAudit{},
	})
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	host := e2eHost(t, st, registry, outbox)
	orch, err := superagent.NewOrchestrator(superagent.Config{
		TenantID: tenant, Store: st, Host: host, Source: source, Channel: channel,
		Policy: &superagent.AllowlistPolicy{Actors: map[string]bool{"alice": true}, WorkerBot: "worker", PolicyVersion: "e2e-v1"},
		Inputs: inputs, Sessions: st, Audit: &fakeAudit{}, Outbox: outbox,
		Operator:            allowOperator{},
		OrchestratorSession: owner.SessionID, OrchestratorBot: "orchestrator",
		WorkerBot: "worker", MaxPoll: 20,
	})
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}

	event := issueEvent("e2e-1", "issue#1")
	if err := source.PushEvent(tenant, event); err != nil {
		t.Fatalf("PushEvent: %v", err)
	}
	if _, err := orch.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	board := mustBoard(t, orch, tenant)
	if len(board.Tickets) != 1 || board.Tickets[0].Status != collab.WorkItemActive {
		t.Fatalf("ticket after accept = %+v", board.Tickets)
	}
	ticket := board.Tickets[0]
	if ticket.ActiveAssignment == nil || ticket.ActiveAssignment.SessionID == uuid.Nil {
		t.Fatalf("assignment after accept = %+v", ticket.ActiveAssignment)
	}
	trace.Add("accepted", map[string]any{
		"ticketId": ticket.ID.String(), "assignmentId": ticket.ActiveAssignment.ID.String(),
		"sessionId": ticket.ActiveAssignment.SessionID.String(), "status": ticket.Status,
	})

	attention, err := orch.Ask(ctx, ticket.ID, ticket.ActiveAssignment.ID, collab.AttentionRequest{
		RequestID: "e2e-question",
		Prompt:    "Approve the plan?",
		Options:   []collab.AttentionOption{{ID: "approve", Label: "approve"}, {ID: "reject", Label: "reject"}},
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if err := orch.Report(ctx, superagent.ReportRequest{
		TenantID: tenant, WorkItemID: ticket.ID, AssignmentID: ticket.ActiveAssignment.ID,
		State: superagent.ReportWaiting, Summary: "waiting for approval", AttentionID: attention.ID,
	}); err != nil {
		t.Fatalf("Report waiting: %v", err)
	}
	blocked := mustBoard(t, orch, tenant)
	if blocked.Tickets[0].Status != collab.WorkItemBlocked {
		t.Fatalf("status after waiting report = %s, want blocked", blocked.Tickets[0].Status)
	}
	trace.Add("attention_opened", map[string]any{"attentionId": attention.ID.String(), "status": blocked.Tickets[0].Status})

	answered, err := orch.AnswerAttention(ctx, tenant, attention.ID, "approve", "", "alice")
	if err != nil {
		t.Fatalf("AnswerAttention: %v", err)
	}
	if answered.Status != collab.AttentionAnswered || answered.Answer == nil || answered.Answer.Actor != "alice" {
		t.Fatalf("answered = %+v", answered)
	}
	if inputs.count() != 1 || inputs.last().Kind != "attention_answer" {
		t.Fatalf("worker deliveries = %+v", inputs.delivered)
	}
	active := mustBoard(t, orch, tenant)
	if active.Tickets[0].Status != collab.WorkItemActive {
		t.Fatalf("status after answer = %s, want active", active.Tickets[0].Status)
	}
	trace.Add("attention_answered", map[string]any{"attentionId": attention.ID.String(), "status": active.Tickets[0].Status})

	if err := orch.Report(ctx, superagent.ReportRequest{
		TenantID: tenant, WorkItemID: ticket.ID, AssignmentID: ticket.ActiveAssignment.ID,
		State: superagent.ReportReady, Summary: "plan applied and verified",
	}); err != nil {
		t.Fatalf("Report ready: %v", err)
	}
	if err := orch.Housekeeping(ctx); err != nil {
		t.Fatalf("Housekeeping: %v", err)
	}
	done := mustBoard(t, orch, tenant)
	if done.Tickets[0].Status != collab.WorkItemDone {
		t.Fatalf("status after ready = %s, want done", done.Tickets[0].Status)
	}
	trace.Add("completed", map[string]any{"ticketId": ticket.ID.String(), "status": done.Tickets[0].Status})

	// Durable facts survive a fresh read.
	stored, err := st.GetWorkItem(ctx, tenant, ticket.ID)
	if err != nil || stored.Status != collab.WorkItemDone {
		t.Fatalf("durable ticket = %+v, err=%v", stored, err)
	}
	assignments, err := st.ListAssignments(ctx, tenant, ticket.ID)
	if err != nil || len(assignments) != 1 || assignments[0].Status != collab.AssignmentDone {
		t.Fatalf("durable assignments = %+v, err=%v", assignments, err)
	}
	storedAttention, err := st.GetAttention(ctx, tenant, attention.ID)
	if err != nil || storedAttention.Status != collab.AttentionAnswered {
		t.Fatalf("durable attention = %+v, err=%v", storedAttention, err)
	}

	messages := channel.Messages()
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		texts = append(texts, message.Text)
	}
	for _, want := range []string{"accepted", "Approve the plan?", "Human selected: approve", "completed"} {
		if !containsText(texts, want) {
			t.Fatalf("channel messages %q do not contain %q", texts, want)
		}
	}
	trace.Add("channel", map[string]any{"messages": texts})
	if err := trace.Write(os.Getenv("KEEL_SUPERAGENT_E2E_TRACE")); err != nil {
		t.Fatalf("write trace: %v", err)
	}
	t.Logf("E2E trace: ticket=%s assignment=%s attention=%s messages=%d", ticket.ID, ticket.ActiveAssignment.ID, attention.ID, len(messages))
}

func e2eRegistry(t *testing.T, tenant uuid.UUID) *collab.ModeRegistry {
	t.Helper()
	registry, err := collab.NewModeRegistry(tenant, collab.Limits{
		Depth: collab.LimitedDepth(8), MaxChildren: 32, MaxConcurrent: 16,
		MaxTurns: 100, MaxTokens: 100_000, MaxDurationMs: 3_600_000,
	})
	if err != nil {
		t.Fatalf("NewModeRegistry: %v", err)
	}
	if err := registry.Register(collab.SuperagentMode()); err != nil {
		t.Fatalf("Register superagent: %v", err)
	}
	if err := registry.SetEnabled(collab.ModeSuperagent, true); err != nil {
		t.Fatalf("enable superagent: %v", err)
	}
	return registry
}

func e2eHost(t *testing.T, st *store.Store, registry *collab.ModeRegistry, outbox *superagent.Outbox) *collab.Coordinator {
	t.Helper()
	host, err := collab.NewHost(collab.HostOptions{
		TenantID: registry.TenantID(), Modes: registry, Work: st, Sessions: st,
		Permissions: fakePermissions{}, Audit: &fakeAudit{}, Launcher: &fakeLauncher{},
		Attention: outbox,
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	return host
}

func mustBoard(t *testing.T, orch *superagent.Orchestrator, tenant uuid.UUID) superagent.BoardView {
	t.Helper()
	board, err := orch.Board(context.Background(), tenant, superagent.BoardFilter{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	return board
}

func containsText(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}

type httpChannelMessage struct {
	ChannelID string `json:"channelId"`
	Text      string `json:"text"`
}

type httpChannel struct {
	mu       sync.Mutex
	messages []httpChannelMessage
	server   *httptest.Server
}

func newHTTPChannel(t *testing.T) *httpChannel {
	t.Helper()
	c := &httpChannel{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var message httpChannelMessage
		if err := json.Unmarshal(raw, &message); err != nil {
			http.Error(w, "json", http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.messages = append(c.messages, message)
		c.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)
	c.server = server
	return c
}

func (c *httpChannel) Send(ctx context.Context, _ uuid.UUID, channelID string, message collab.ChannelMessage) error {
	payload, err := json.Marshal(httpChannelMessage{ChannelID: channelID, Text: message.Text})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.server.URL+"/comments", strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("channel status %d", resp.StatusCode)
	}
	return nil
}

func (c *httpChannel) Messages() []httpChannelMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]httpChannelMessage(nil), c.messages...)
}

type e2eTrace struct {
	TenantID string           `json:"tenantId"`
	Steps    []map[string]any `json:"steps"`
}

func (t *e2eTrace) Add(step string, data map[string]any) {
	entry := map[string]any{"step": step, "at": time.Now().UTC().Format(time.RFC3339Nano)}
	for key, value := range data {
		entry[key] = value
	}
	t.Steps = append(t.Steps, entry)
}

func (t *e2eTrace) Write(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}
