// Command superagent-acceptance serves a local, real-PostgreSQL super-agent
// board for the web/TUI acceptance pass.
//
// It is not a product deployment: the Source and Channel are the in-repo
// FixtureSource, the worker launcher is a fake, and the access seam grants
// task.read/task.write to the acceptance tenants only so the HTTP surface can
// be exercised without a real login. The four tickets it seeds come from the
// same orchestrator path the E2E test uses (Source event -> Ticket ->
// Assignment -> Worker report -> Attention), so the board and inbox payloads
// are real projections of the durable collab_* rows.
//
// It skips (fails loudly) without KEEL_TEST_DSN and never contacts GitHub.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/harnessapi"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/superagent"
)

//go:embed page.html
var pageHTML string

const (
	orchestratorBot = "orchestrator"
	workerBot       = "worker"
)

type seedInfo struct {
	SeededTenant uuid.UUID  `json:"seededTenant"`
	EmptyTenant  uuid.UUID  `json:"emptyTenant"`
	DeniedTenant uuid.UUID  `json:"deniedTenant"`
	AttentionID  uuid.UUID  `json:"attentionId"`
	Tickets      []seedCard `json:"tickets"`
}

type seedCard struct {
	ID       uuid.UUID `json:"id"`
	Thread   string    `json:"thread"`
	Title    string    `json:"title"`
	Status   string    `json:"status"`
	Assignee string    `json:"assignee,omitempty"`
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	dsn := strings.TrimSpace(os.Getenv("KEEL_TEST_DSN"))
	if dsn == "" {
		log.Fatal("KEEL_TEST_DSN is required: this harness seeds a local temporary PostgreSQL database and never contacts a live Source")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, orch, info, err := seed(ctx, dsn)
	if err != nil {
		log.Fatalf("seed: %v", err)
	}
	defer st.Close()

	service := routedService{primaryTenant: info.SeededTenant, primary: orch}
	api := harnessapi.New(harnessapi.Config{
		Superagent: service,
		Access:     acceptanceAccess{deniedTenant: info.DeniedTenant, accountID: uuid.New()},
	})
	server := &acceptanceServer{api: api, page: pageHTML, info: info}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	baseURL := "http://" + listener.Addr().String()
	state := map[string]any{
		"baseUrl":      baseURL,
		"seededTenant": info.SeededTenant.String(),
		"emptyTenant":  info.EmptyTenant.String(),
		"deniedTenant": info.DeniedTenant.String(),
		"attentionId":  info.AttentionID.String(),
		"tickets":      info.Tickets,
	}
	if path := strings.TrimSpace(os.Getenv("KEEL_SUPERAGENT_ACCEPTANCE_STATE")); path != "" {
		raw, _ := json.MarshalIndent(state, "", "  ")
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			log.Fatalf("write state file: %v", err)
		}
	}
	log.Printf("super-agent acceptance board: %s", baseURL)
	for _, card := range info.Tickets {
		log.Printf("ticket %s %-10s %s", card.ID.String()[:8], card.Status, card.Title)
	}
	log.Printf("open attention: %s", info.AttentionID)

	httpServer := &http.Server{Handler: server, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("serve: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	log.Printf("super-agent acceptance board stopped")
}

func seed(ctx context.Context, dsn string) (*store.Store, *superagent.Orchestrator, seedInfo, error) {
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		return nil, nil, seedInfo{}, fmt.Errorf("open store: %w", err)
	}
	info := seedInfo{
		SeededTenant: uuid.New(),
		EmptyTenant:  uuid.New(),
		DeniedTenant: uuid.New(),
	}
	for _, tenant := range []uuid.UUID{info.SeededTenant, info.EmptyTenant, info.DeniedTenant} {
		if err := st.CreateTenant(ctx, tenant, "acceptance-"+tenant.String()[:8]); err != nil {
			st.Close()
			return nil, nil, seedInfo{}, fmt.Errorf("create tenant: %w", err)
		}
	}
	owner := collab.SessionRef{
		TenantID: info.SeededTenant, SessionID: uuid.New(), BotID: orchestratorBot,
		State: collab.SessionActive, CreatedAt: time.Now().UTC(),
	}
	if err := st.Put(ctx, owner); err != nil {
		st.Close()
		return nil, nil, seedInfo{}, fmt.Errorf("owner session: %w", err)
	}
	registry, err := collab.NewModeRegistry(info.SeededTenant, collab.Limits{
		Depth: collab.LimitedDepth(8), MaxChildren: 32, MaxConcurrent: 16,
		MaxTurns: 100, MaxTokens: 100_000, MaxDurationMs: 3_600_000,
	})
	if err != nil {
		st.Close()
		return nil, nil, seedInfo{}, fmt.Errorf("registry: %w", err)
	}
	if err := registry.Register(collab.SuperagentMode()); err != nil {
		st.Close()
		return nil, nil, seedInfo{}, fmt.Errorf("register mode: %w", err)
	}
	if err := registry.SetEnabled(collab.ModeSuperagent, true); err != nil {
		st.Close()
		return nil, nil, seedInfo{}, fmt.Errorf("enable mode: %w", err)
	}
	source := superagent.NewFixtureSource(func() time.Time { return time.Now().UTC() })
	outbox, err := superagent.NewOutbox(superagent.OutboxOptions{Store: st, Channel: source, Audit: silentAudit{}})
	if err != nil {
		st.Close()
		return nil, nil, seedInfo{}, fmt.Errorf("outbox: %w", err)
	}
	host, err := collab.NewHost(collab.HostOptions{
		TenantID: info.SeededTenant, Modes: registry, Work: st, Sessions: st,
		Permissions: permissivePermissions{}, Audit: silentAudit{}, Launcher: &fixtureLauncher{},
		Attention: outbox,
	})
	if err != nil {
		st.Close()
		return nil, nil, seedInfo{}, fmt.Errorf("host: %w", err)
	}
	orch, err := superagent.NewOrchestrator(superagent.Config{
		TenantID: info.SeededTenant, Store: st, Host: host, Source: source, Channel: source,
		Policy: &superagent.AllowlistPolicy{
			Actors: map[string]bool{"alice": true}, WorkerBot: workerBot, PolicyVersion: "acceptance-v1",
		},
		Inputs: noopInputs{}, Sessions: st, Audit: silentAudit{}, Outbox: outbox,
		Operator:            permissiveOperator{},
		OrchestratorSession: owner.SessionID, OrchestratorBot: orchestratorBot,
		WorkerBot: workerBot, MaxPoll: 20,
	})
	if err != nil {
		st.Close()
		return nil, nil, seedInfo{}, fmt.Errorf("orchestrator: %w", err)
	}

	fixtures := []struct {
		eventID string
		thread  string
		title   string
		body    string
	}{
		{"acc-101", "issue#101", "支付回调偶发重复入账", "财务反馈部分订单在回调重试时被记录两次。"},
		{"acc-102", "issue#102", "导出报表缺少门店维度", "运营需要在日报里按门店拆分营收。"},
		{"acc-103", "issue#103", "修复登录页文案", "把「登录」统一为「进入工作台」。"},
		{"acc-104", "issue#104", "迁移旧版 Webhook", "旧 Webhook 协议要在本周下线。"},
	}
	for _, fixture := range fixtures {
		event := superagent.Event{
			SourceID: superagent.SourceFixture, EventID: fixture.eventID,
			Kind:   superagent.EventIssueOpened,
			Actor:  superagent.Actor{ID: "u1", Login: "alice", Display: "Alice"},
			Thread: superagent.ThreadRef{Source: superagent.SourceFixture, Kind: "issue", ID: fixture.thread, URL: "fixture://" + fixture.thread},
			Title:  fixture.title, Body: fixture.body,
		}
		if err := source.PushEvent(info.SeededTenant, event); err != nil {
			st.Close()
			return nil, nil, seedInfo{}, fmt.Errorf("push %s: %w", fixture.eventID, err)
		}
	}
	if _, err := orch.PollOnce(ctx); err != nil {
		st.Close()
		return nil, nil, seedInfo{}, fmt.Errorf("poll: %w", err)
	}
	board, err := orch.Board(ctx, info.SeededTenant, superagent.BoardFilter{})
	if err != nil {
		st.Close()
		return nil, nil, seedInfo{}, fmt.Errorf("board after poll: %w", err)
	}
	if len(board.Tickets) != len(fixtures) {
		st.Close()
		return nil, nil, seedInfo{}, fmt.Errorf("seeded %d tickets, want %d", len(board.Tickets), len(fixtures))
	}
	byThread := make(map[string]superagent.TicketView, len(board.Tickets))
	for _, ticket := range board.Tickets {
		byThread[ticket.Thread.ID] = ticket
	}
	blocked := byThread["issue#102"]
	done := byThread["issue#103"]
	failed := byThread["issue#104"]
	for name, ticket := range map[string]superagent.TicketView{"blocked": blocked, "done": done, "failed": failed} {
		if ticket.ActiveAssignment == nil || ticket.ActiveAssignment.ID == uuid.Nil {
			st.Close()
			return nil, nil, seedInfo{}, fmt.Errorf("%s ticket has no active assignment: %+v", name, ticket)
		}
	}
	attention, err := orch.Ask(ctx, blocked.ID, blocked.ActiveAssignment.ID, collab.AttentionRequest{
		RequestID: "acceptance-q1", Kind: collab.AttentionApproval,
		Prompt: "是否允许在周五晚高峰灰度发布？",
		Options: []collab.AttentionOption{
			{ID: "approve", Label: "批准", Description: "按计划灰度"},
			{ID: "reject", Label: "拒绝", Description: "等下周再发"},
		},
		AllowFreeText: true,
	})
	if err != nil {
		st.Close()
		return nil, nil, seedInfo{}, fmt.Errorf("ask: %w", err)
	}
	info.AttentionID = attention.ID
	if err := orch.Report(ctx, superagent.ReportRequest{
		TenantID: info.SeededTenant, WorkItemID: blocked.ID, AssignmentID: blocked.ActiveAssignment.ID,
		State: superagent.ReportWaiting, Summary: "等待人工确认灰度窗口", AttentionID: attention.ID,
	}); err != nil {
		st.Close()
		return nil, nil, seedInfo{}, fmt.Errorf("report waiting: %w", err)
	}
	if err := orch.Report(ctx, superagent.ReportRequest{
		TenantID: info.SeededTenant, WorkItemID: done.ID, AssignmentID: done.ActiveAssignment.ID,
		State: superagent.ReportReady, Summary: "文案已更新并通过验收",
	}); err != nil {
		st.Close()
		return nil, nil, seedInfo{}, fmt.Errorf("report ready: %w", err)
	}
	if err := orch.Report(ctx, superagent.ReportRequest{
		TenantID: info.SeededTenant, WorkItemID: failed.ID, AssignmentID: failed.ActiveAssignment.ID,
		State: superagent.ReportFailed, Summary: "旧 Webhook 依赖已下线，迁移脚本回滚",
	}); err != nil {
		st.Close()
		return nil, nil, seedInfo{}, fmt.Errorf("report failed: %w", err)
	}
	if err := orch.Housekeeping(ctx); err != nil {
		st.Close()
		return nil, nil, seedInfo{}, fmt.Errorf("housekeeping: %w", err)
	}

	board, err = orch.Board(ctx, info.SeededTenant, superagent.BoardFilter{})
	if err != nil {
		st.Close()
		return nil, nil, seedInfo{}, fmt.Errorf("final board: %w", err)
	}
	want := map[string]int{"active": 1, "blocked": 1, "done": 1, "failed": 1}
	if err := compareCounts(board.Counts, want); err != nil {
		st.Close()
		return nil, nil, seedInfo{}, err
	}
	for _, ticket := range board.Tickets {
		card := seedCard{ID: ticket.ID, Thread: ticket.Thread.ID, Title: ticket.Title, Status: string(ticket.Status)}
		if ticket.ActiveAssignment != nil {
			card.Assignee = ticket.ActiveAssignment.BotID + "/" + shortID(ticket.ActiveAssignment.SessionID)
		}
		info.Tickets = append(info.Tickets, card)
	}
	inbox, err := orch.Inbox(ctx, info.SeededTenant)
	if err != nil {
		st.Close()
		return nil, nil, seedInfo{}, fmt.Errorf("inbox: %w", err)
	}
	if len(inbox.Items) != 1 || inbox.Items[0].Status != collab.AttentionOpen {
		st.Close()
		return nil, nil, seedInfo{}, fmt.Errorf("open attention count = %d, want 1", len(inbox.Items))
	}
	return st, orch, info, nil
}

func compareCounts(got, want map[string]int) error {
	for status, count := range want {
		if got[status] != count {
			return fmt.Errorf("board counts %v, want %v", got, want)
		}
	}
	return nil
}

func shortID(id uuid.UUID) string {
	text := id.String()
	if len(text) > 8 {
		return text[:8]
	}
	return text
}

// routedService serves the seeded tenant from the orchestrator and an honest
// empty board for the acceptance-only empty tenant. The denied tenant never
// reaches this seam: the access check answers 403 first.
type routedService struct {
	primaryTenant uuid.UUID
	primary       *superagent.Orchestrator
}

func (s routedService) Board(ctx context.Context, tenantID uuid.UUID, filter superagent.BoardFilter) (superagent.BoardView, error) {
	if tenantID == s.primaryTenant {
		return s.primary.Board(ctx, tenantID, filter)
	}
	return superagent.BoardView{TenantID: tenantID, ModeID: collab.ModeSuperagent, Tickets: []superagent.TicketView{}, Counts: map[string]int{}}, nil
}

func (s routedService) Inbox(ctx context.Context, tenantID uuid.UUID) (superagent.AttentionInbox, error) {
	if tenantID == s.primaryTenant {
		return s.primary.Inbox(ctx, tenantID)
	}
	return superagent.AttentionInbox{TenantID: tenantID, Items: []superagent.AttentionView{}}, nil
}

func (s routedService) Report(ctx context.Context, request superagent.ReportRequest) error {
	if request.TenantID == s.primaryTenant {
		return s.primary.Report(ctx, request)
	}
	return fmt.Errorf("acceptance: report is only wired for the seeded tenant")
}

func (s routedService) Ask(ctx context.Context, ticketID, assignmentID uuid.UUID, request collab.AttentionRequest) (collab.Attention, error) {
	return s.primary.Ask(ctx, ticketID, assignmentID, request)
}

func (s routedService) AnswerAttention(ctx context.Context, tenantID, attentionID uuid.UUID, optionID, text, actor string) (collab.Attention, error) {
	if tenantID != s.primaryTenant {
		return collab.Attention{}, fmt.Errorf("acceptance: answers are only wired for the seeded tenant")
	}
	return s.primary.AnswerAttention(ctx, tenantID, attentionID, optionID, text, actor)
}

func (s routedService) ResumeTicket(ctx context.Context, tenantID, ticketID uuid.UUID, actor, reason string) (superagent.Ticket, error) {
	if tenantID != s.primaryTenant {
		return superagent.Ticket{}, fmt.Errorf("acceptance: resume is only wired for the seeded tenant")
	}
	return s.primary.ResumeTicket(ctx, tenantID, ticketID, actor, reason)
}

func (s routedService) StopTicket(ctx context.Context, tenantID, ticketID uuid.UUID, actor, reason string) error {
	if tenantID != s.primaryTenant {
		return fmt.Errorf("acceptance: stop is only wired for the seeded tenant")
	}
	return s.primary.StopTicket(ctx, tenantID, ticketID, actor, reason)
}

func (s routedService) RetryPending(ctx context.Context, tenantID, ticketID uuid.UUID, pendingID, actor string) (superagent.Ticket, error) {
	if tenantID != s.primaryTenant {
		return superagent.Ticket{}, fmt.Errorf("acceptance: pending input is only wired for the seeded tenant")
	}
	return s.primary.RetryPending(ctx, tenantID, ticketID, pendingID, actor)
}

func (s routedService) DismissPending(ctx context.Context, tenantID, ticketID uuid.UUID, pendingID, actor string) (superagent.Ticket, error) {
	if tenantID != s.primaryTenant {
		return superagent.Ticket{}, fmt.Errorf("acceptance: pending input is only wired for the seeded tenant")
	}
	return s.primary.DismissPending(ctx, tenantID, ticketID, pendingID, actor)
}

// acceptanceServer serves the harness page, its config and the real /v1
// super-agent routes. fail/delay are harness-only query switches used to
// capture the error and loading states; the product API never sees them in a
// deployment.
type acceptanceServer struct {
	api  http.Handler
	page string
	info seedInfo
}

func (s *acceptanceServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/" || r.URL.Path == "/index.html":
		s.servePage(w)
		return
	case r.URL.Path == "/acceptance/config":
		writeJSON(w, map[string]any{
			"seededTenant": s.info.SeededTenant.String(),
			"emptyTenant":  s.info.EmptyTenant.String(),
			"deniedTenant": s.info.DeniedTenant.String(),
			"attentionId":  s.info.AttentionID.String(),
			"mode":         collab.ModeSuperagent,
		})
		return
	case strings.HasPrefix(r.URL.Path, "/v1/"):
		if r.URL.Query().Get("fail") == "1" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"acceptance: induced board read failure","code":"acceptance_failure"}`))
			return
		}
		if raw := r.URL.Query().Get("delay"); raw != "" {
			delay, err := time.ParseDuration(raw + "ms")
			if err == nil && delay > 0 && delay <= 10*time.Second {
				select {
				case <-time.After(delay):
				case <-r.Context().Done():
				}
			}
		}
		s.api.ServeHTTP(w, r)
		return
	default:
		http.NotFound(w, r)
	}
}

func (s *acceptanceServer) servePage(w http.ResponseWriter) {
	config, _ := json.Marshal(map[string]any{
		"seededTenant": s.info.SeededTenant.String(),
		"emptyTenant":  s.info.EmptyTenant.String(),
		"deniedTenant": s.info.DeniedTenant.String(),
		"attentionId":  s.info.AttentionID.String(),
		"mode":         collab.ModeSuperagent,
	})
	page := strings.Replace(s.page, "__ACCEPTANCE_CONFIG__", string(config), 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page))
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(body)
}

// --- access seam ---------------------------------------------------------

// acceptanceAccess is a harness-only authorization seam. It grants both
// super-agent keys at tenant scope to the seeded and empty tenants, and no
// grants to the denied tenant. It never runs in a deployment.
type acceptanceAccess struct {
	deniedTenant uuid.UUID
	accountID    uuid.UUID
}

func (a acceptanceAccess) Principal(_ context.Context, tenantID uuid.UUID) (access.Principal, error) {
	return access.Principal{TenantID: tenantID, AccountID: a.accountID}, nil
}

func (a acceptanceAccess) Authorize(_ context.Context, _ access.Principal, key string, _ access.Resource) (access.Decision, error) {
	return access.Decision{Allow: true, Key: key, Reason: access.ReasonAllow, Scope: access.ScopeTenant}, nil
}

func (a acceptanceAccess) Visible(_ context.Context, _ access.Principal, key, _ string, candidateIDs []string) (access.VisibleResult, error) {
	return access.VisibleResult{
		IDs: candidateIDs, Scope: access.ScopeTenant,
		Decision: access.Decision{Allow: true, Key: key, Reason: access.ReasonAllow, Scope: access.ScopeTenant},
	}, nil
}

func (a acceptanceAccess) Effective(_ context.Context, principal access.Principal) (access.Grants, error) {
	if principal.TenantID == a.deniedTenant {
		return access.Grants{}, nil
	}
	return access.Grants{
		access.KeyTaskRead:  access.ScopeTenant,
		access.KeyTaskWrite: access.ScopeTenant,
	}, nil
}

func (acceptanceAccess) CanGrant(context.Context, access.Principal, []access.Grant) error { return nil }

func (acceptanceAccess) BotOwnerAccount(context.Context, uuid.UUID, string) (uuid.UUID, bool, error) {
	return uuid.Nil, false, nil
}

func (acceptanceAccess) AuditGrant(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string, string, string, string, map[string]any) error {
	return nil
}

func (acceptanceAccess) AuditRelation(context.Context, uuid.UUID, uuid.UUID, string, string, string, string, string, string) error {
	return nil
}

func (acceptanceAccess) AuditSuperAdmin(context.Context, access.Principal, string, map[string]any) error {
	return nil
}

func (acceptanceAccess) AuditInvoke(context.Context, uuid.UUID, access.Principal, string, uuid.UUID, string) error {
	return nil
}

func (acceptanceAccess) AuditBotKey(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string, string, string, string) error {
	return nil
}

func (acceptanceAccess) Denied(context.Context, access.Principal, access.Decision, access.Resource) {}

// --- local worker seams --------------------------------------------------

type fixtureLauncher struct {
	mu       sync.Mutex
	launches int
}

func (l *fixtureLauncher) Launch(_ context.Context, req collab.LaunchRequest) (collab.SessionRef, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.launches++
	return collab.SessionRef{
		TenantID: req.TenantID, SessionID: uuid.New(), ParentSessionID: req.ParentSessionID,
		BotID: req.BotID, ModeID: req.ModeID, WorkItemID: req.WorkItemID,
		Depth: req.Depth, State: collab.SessionActive,
	}, nil
}

func (l *fixtureLauncher) CancelSession(context.Context, uuid.UUID, uuid.UUID, string) error {
	return nil
}

type permissivePermissions struct{}

func (permissivePermissions) CheckBotInvoke(context.Context, uuid.UUID, collab.SessionRef, string) error {
	return nil
}

type silentAudit struct{}

func (silentAudit) Record(context.Context, collab.AuditEvent) error { return nil }

type permissiveOperator struct{}

func (permissiveOperator) AuthorizeOperator(context.Context, uuid.UUID, uuid.UUID, string, string) error {
	return nil
}

type noopInputs struct{}

func (noopInputs) Deliver(context.Context, superagent.InputRequest) error { return nil }
