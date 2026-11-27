package main

// The superagent bridge E2E: the same wrapper chain `keel serve` mounts
// (auth principal -> Surface.Gate -> superagentboard.Bridge -> Surface -> real
// board log) over real PostgreSQL and HTTP, with a controllable supervisor.
//
// It exists because the other sync scenarios exercise sync entries only; the
// bridge's task actions take a different path (supervisor first, then mirror)
// and its disabled/timeout/failure behavior is a Phase 1 acceptance gate.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/superagent"
	"github.com/jixoai/keel/internal/superagentboard"
)

type e2eSupervisor struct {
	mu       sync.Mutex
	tenant   uuid.UUID
	store    *superagent.MemoryStore
	ticketID uuid.UUID
	calls    int
	delay    time.Duration
	err      error
}

func (s *e2eSupervisor) configure(delay time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = delay
	s.err = err
}

func (s *e2eSupervisor) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// begin waits for the configured delay and applies the configured failure.
// The bridge cancels the context at its command timeout, so a slow supervisor
// returns ctx.Err (the 504 path).
func (s *e2eSupervisor) begin(ctx context.Context) error {
	s.mu.Lock()
	delay, err := s.delay, s.err
	s.calls++
	s.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (s *e2eSupervisor) ResumeTicket(ctx context.Context, tenantID, ticketID uuid.UUID, actor, reason string) (superagent.Ticket, error) {
	if err := s.begin(ctx); err != nil {
		return superagent.Ticket{}, err
	}
	// The supervisor accepted: dispatch the ticket and open the assignment the
	// bridge will mirror as a claim.
	item, err := s.store.GetWorkItem(ctx, tenantID, ticketID)
	if err != nil {
		return superagent.Ticket{}, err
	}
	item.Status = collab.WorkItemActive
	item.UpdatedAt = time.Now().UTC()
	updated, err := s.store.UpdateWorkItem(ctx, item)
	if err != nil {
		return superagent.Ticket{}, err
	}
	if _, err := s.store.CreateAssignment(ctx, collab.Assignment{
		ID: uuid.New(), TenantID: tenantID, WorkItemID: ticketID, ModeID: superagent.DefaultModeID,
		BotID: "researcher", SessionID: uuid.New(), Status: collab.AssignmentActive,
		Title: item.Title, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		return superagent.Ticket{}, err
	}
	return superagent.TicketFromWorkItem(updated), nil
}

func (s *e2eSupervisor) Report(ctx context.Context, request superagent.ReportRequest) error {
	if err := s.begin(ctx); err != nil {
		return err
	}
	// Apply the same durable facts the orchestrator would: the assignment and
	// ticket reach a terminal state, and an applied worker report is what lets
	// the mirror move the board task to completed/failed.
	var assignmentStatus collab.AssignmentStatus
	var itemStatus collab.WorkItemStatus
	switch request.State {
	case superagent.ReportReady:
		assignmentStatus, itemStatus = collab.AssignmentDone, collab.WorkItemDone
	case superagent.ReportFailed:
		assignmentStatus, itemStatus = collab.AssignmentFailed, collab.WorkItemFailed
	default:
		return nil
	}
	assignment, err := s.store.GetAssignment(ctx, s.tenant, request.AssignmentID)
	if err != nil {
		return err
	}
	assignment.Status = assignmentStatus
	assignment.Result = request.Result
	assignment.UpdatedAt = time.Now().UTC()
	if _, err := s.store.UpdateAssignment(ctx, assignment); err != nil {
		return err
	}
	now := time.Now().UTC()
	report, created, err := s.store.ClaimWorkerReport(ctx, superagent.WorkerReport{
		ID: uuid.New(), TenantID: s.tenant, WorkItemID: request.WorkItemID,
		AssignmentID: request.AssignmentID, SessionID: assignment.SessionID,
		RequestID: "e2e:" + uuid.NewString(), State: request.State,
		Summary: request.Summary, Result: request.Result, CreatedAt: now,
	})
	if err != nil {
		return err
	}
	if created {
		if _, err := s.store.MarkWorkerReportAudited(ctx, s.tenant, report.ID, now); err != nil {
			return err
		}
		if _, err := s.store.MarkWorkerReportApplied(ctx, s.tenant, report.ID, now); err != nil {
			return err
		}
	}
	item, err := s.store.GetWorkItem(ctx, s.tenant, request.WorkItemID)
	if err != nil {
		return err
	}
	item.Status = itemStatus
	item.UpdatedAt = now
	_, err = s.store.UpdateWorkItem(ctx, item)
	return err
}

func (s *e2eSupervisor) Ask(ctx context.Context, ticketID, assignmentID uuid.UUID, request collab.AttentionRequest) (collab.Attention, error) {
	if err := s.begin(ctx); err != nil {
		return collab.Attention{}, err
	}
	// A successful block opens the Attention the board mirror will project.
	now := time.Now().UTC()
	attention, err := s.store.CreateAttention(ctx, collab.Attention{
		ID: uuid.New(), TenantID: s.tenant, WorkItemID: ticketID, AssignmentID: assignmentID,
		ModeID: superagent.DefaultModeID, RequestID: request.RequestID, Kind: request.Kind,
		Prompt: request.Prompt, AllowFreeText: request.AllowFreeText, Route: request.Route,
		Status: collab.AttentionOpen, CreatedAt: now, Deadline: now.Add(time.Hour),
	})
	if err != nil {
		return collab.Attention{}, err
	}
	item, err := s.store.GetWorkItem(ctx, s.tenant, ticketID)
	if err == nil {
		item.Status = collab.WorkItemBlocked
		item.UpdatedAt = now
		_, err = s.store.UpdateWorkItem(ctx, item)
	}
	return attention, err
}

func (s *e2eSupervisor) AnswerAttention(ctx context.Context, tenantID, attentionID uuid.UUID, optionID, text, actor string) (collab.Attention, error) {
	if err := s.begin(ctx); err != nil {
		return collab.Attention{}, err
	}
	return collab.Attention{}, nil
}

func (s *e2eSupervisor) StopTicket(ctx context.Context, tenantID, ticketID uuid.UUID, actor, reason string) error {
	return s.begin(ctx)
}

type allowTaskWrite struct{}

func (allowTaskWrite) AuthorizeTaskWrite(context.Context, uuid.UUID) error { return nil }

// e2eOperatorACL is the production operator-relation seam: the builtin
// tenant-admin and bot-operator groups become owner of the mirrored channel.
type e2eOperatorACL struct{ store *store.Store }

func (a e2eOperatorACL) EnsureOperator(ctx context.Context, tenantID uuid.UUID, channel string) error {
	if a.store == nil || tenantID == uuid.Nil || strings.TrimSpace(channel) == "" {
		return nil
	}
	for _, name := range []string{"tenant-admin", "bot-operator"} {
		if _, err := a.store.PutResourceRelation(ctx, store.ResourceRelation{
			TenantID: tenantID, ResourceType: "board_channel", ResourceID: channel,
			Relation: store.RelationOwner, SubjectType: store.RelationSubjectGroup,
			SubjectID: access.BuiltinGroupID(tenantID, name).String(), CreatedBy: tenantID,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (e *env) scenarioSuperagentBridge(scenario *Scenario) error {
	ctx := e.ctx
	config := json.RawMessage(`{"board":{"enabled":true,"sync":{"enabled":true},"superagent":{"mirror":true,"bridge":true,"channel":"superagent"}}}`)
	setConfig := func(document json.RawMessage) error {
		_, err := e.store.SetTenantConfig(ctx, e.tenant, document)
		return err
	}
	if err := setConfig(config); err != nil {
		return fmt.Errorf("tenant config: %w", err)
	}
	// The operator's authority is real group membership; it has no individual
	// board grant.
	if _, err := e.store.AddPermissionGroupMember(ctx, e.tenant,
		access.BuiltinGroupID(e.tenant, "tenant-admin"), e.operator.ID, e.operator.ID); err != nil {
		return fmt.Errorf("operator group membership: %w", err)
	}

	// Two supervisor tickets in a memory store: one completes, one blocks. The
	// bridge/mirror only read and write the board; the fake supervisor applies
	// the same assignment/ticket/report facts the orchestrator would.
	memory := superagent.NewMemoryStore(func() time.Time { return time.Now().UTC() })
	seedTicket := func(eventID, title string) superagent.Ticket {
		event := superagent.Event{
			SourceID: "manual", EventID: eventID, Kind: superagent.EventIssueOpened,
			Actor:  superagent.Actor{ID: "octocat", Login: "octocat", Display: "Octo Cat"},
			Thread: superagent.ThreadRef{Source: "manual", Kind: "issue", ID: eventID},
			Title:  title, Body: "Verify the board command bridge.",
		}
		ticket := superagent.NewTicket(e.tenant, superagent.DefaultModeID, event,
			superagent.Verdict{Action: superagent.ActionAccept, Strength: superagent.MandateWrite}, "researcher", time.Now().UTC())
		item, err := memory.CreateWorkItem(ctx, ticket.Record())
		if err != nil {
			return superagent.Ticket{}
		}
		return superagent.TicketFromWorkItem(item)
	}
	completeTicket := seedTicket("bridge-e2e-complete", "Bridge complete E2E")
	blockTicket := seedTicket("bridge-e2e-block", "Bridge block E2E")
	if completeTicket.ID == uuid.Nil || blockTicket.ID == uuid.Nil {
		return fmt.Errorf("create tickets")
	}

	tokens, err := superagentboard.NewTokenSource("bridge-e2e-secret")
	if err != nil {
		return err
	}
	boardPort := &superagentboard.LogBoard{Board: e.surface.Board()}
	mirror, err := superagentboard.NewMirror(&superagentboard.Mirror{
		TenantID: e.tenant, Store: memory, Board: boardPort, Links: e.store,
		Config: superagentboard.StoreConfigReader{Store: e.store}, Secret: tokens,
		ACL: e2eOperatorACL{store: e.store}, Logger: e.logger,
		Now: func() time.Time { return time.Now().UTC() }, Interval: -1,
	})
	if err != nil {
		return fmt.Errorf("mirror: %v", err)
	}
	if _, err := mirror.SyncOnce(ctx); err != nil {
		return fmt.Errorf("seed mirror: %v", err)
	}
	taskFor := func(ticketID uuid.UUID) (uint64, error) {
		link, found, err := e.store.GetBoardLink(ctx, e.tenant, superagentboard.BoardSource, ticketID.String(), store.BoardLinkTicket)
		if err != nil || !found {
			return 0, fmt.Errorf("mirrored ticket link = %+v/%v/%v", link, found, err)
		}
		return link.BoardTaskID, nil
	}
	completeTaskID, err := taskFor(completeTicket.ID)
	if err != nil {
		return err
	}
	blockTaskID, err := taskFor(blockTicket.ID)
	if err != nil {
		return err
	}

	supervisor := &e2eSupervisor{tenant: e.tenant, store: memory}
	commands, err := superagentboard.NewSupervisorCommands(e.tenant, memory, supervisor)
	if err != nil {
		return err
	}
	bridge := &superagentboard.Bridge{
		Next: e.surface, Identity: e.surface.Identity(), Authorizer: e.surface.Authorizer(),
		Operator: allowTaskWrite{}, Logger: e.logger, Timeout: 300 * time.Millisecond,
	}
	bridge.Attach(e.store, superagentboard.StoreConfigReader{Store: e.store}, mirror, commands, boardPort)
	// The serve mount composition: the same Surface gate wraps the bridge.
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principal, ok := e.principals[r.Header.Get("X-E2E-Who")]; ok {
			r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
		}
		e.surface.Gate(bridge).ServeHTTP(w, r)
	}))
	defer gateway.Close()
	postAs := func(who string, taskID uint64, action string, body map[string]any) (int, string) {
		raw, _ := json.Marshal(body)
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			gateway.URL+"/v1/board/tasks/"+formatTaskID(taskID)+"/"+action, strings.NewReader(string(raw)))
		request.Header.Set("content-type", "application/json")
		request.Header.Set("X-E2E-Who", who)
		response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
		if err != nil {
			return 0, err.Error()
		}
		defer response.Body.Close()
		payload, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(payload)
	}
	if !scenario.Step("a board.write user with no relation on the mirrored channel is refused", func() (string, error) {
		beforeCalls := supervisor.callCount()
		beforeHead, err := e.surface.Board().Head(e.tenant.String())
		if err != nil {
			return "", err
		}
		for _, action := range []string{"claim", "complete", "fail", "block", "unblock", "assign", "cancel"} {
			status, body := postAs("bob", blockTaskID, action, map[string]any{"reason": "not mine", "result": "x"})
			if status != http.StatusNotFound {
				return "", fmt.Errorf("%s = %d %s", action, status, body)
			}
		}
		afterHead, err := e.surface.Board().Head(e.tenant.String())
		if err != nil {
			return "", err
		}
		if supervisor.callCount() != beforeCalls || afterHead != beforeHead {
			return "", fmt.Errorf("a non-member reached the supervisor: calls %d->%d head %d->%d",
				beforeCalls, supervisor.callCount(), beforeHead, afterHead)
		}
		return "404 for all seven actions; supervisor calls and board head unchanged", nil
	}) {
		return errors.New("channel relation E2E failed")
	}

	if !scenario.Step("the operator group claims through auth -> gate -> bridge -> supervisor -> board", func() (string, error) {
		status, body := postAs("operator", completeTaskID, "claim", map[string]any{})
		if status != http.StatusOK {
			return "", fmt.Errorf("claim = %d %s", status, body)
		}
		task, err := boardPort.Task(ctx, e.tenant, completeTaskID)
		if err != nil {
			return "", err
		}
		if task.State != superagentboard.StateClaimed {
			return "", fmt.Errorf("task state = %q", task.State)
		}
		return "HTTP 200; board task claimed by a tenant-admin member", nil
	}) {
		return errors.New("superagent claim E2E failed")
	}

	if !scenario.Step("the operator group completes the ticket and the board task reaches completed", func() (string, error) {
		status, body := postAs("operator", completeTaskID, "complete", map[string]any{"result": "done from board"})
		if status != http.StatusOK {
			return "", fmt.Errorf("complete = %d %s", status, body)
		}
		task, err := boardPort.Task(ctx, e.tenant, completeTaskID)
		if err != nil {
			return "", err
		}
		if task.State != superagentboard.StateCompleted {
			return "", fmt.Errorf("task state = %q, want completed", task.State)
		}
		return "supervisor ReportReady applied; board task completed", nil
	}) {
		return errors.New("operator complete E2E failed")
	}

	if !scenario.Step("the operator group blocks a second ticket and the board task is blocked", func() (string, error) {
		if status, body := postAs("operator", blockTaskID, "claim", map[string]any{}); status != http.StatusOK {
			return "", fmt.Errorf("claim = %d %s", status, body)
		}
		status, body := postAs("operator", blockTaskID, "block", map[string]any{"reason": "operator question"})
		if status != http.StatusOK {
			return "", fmt.Errorf("block = %d %s", status, body)
		}
		task, err := boardPort.Task(ctx, e.tenant, blockTaskID)
		if err != nil {
			return "", err
		}
		if task.State != superagentboard.StateBlocked {
			return "", fmt.Errorf("task state = %q, want blocked", task.State)
		}
		return "supervisor Ask applied; board task blocked", nil
	}) {
		return errors.New("operator block E2E failed")
	}

	if !scenario.Step("board.enabled=false answers 404 and never reaches the supervisor", func() (string, error) {
		if err := setConfig(json.RawMessage(`{"board":{"enabled":false,"superagent":{"mirror":true,"bridge":true,"channel":"superagent"}}}`)); err != nil {
			return "", err
		}
		beforeCalls := supervisor.callCount()
		beforeHead, err := e.surface.Board().Head(e.tenant.String())
		if err != nil {
			return "", err
		}
		status, body := postAs("operator", blockTaskID, "block", map[string]any{"reason": "disabled"})
		if status != http.StatusNotFound || !strings.Contains(body, "board_disabled") {
			return "", fmt.Errorf("block while disabled = %d %s", status, body)
		}
		afterHead, err := e.surface.Board().Head(e.tenant.String())
		if err != nil {
			return "", err
		}
		if supervisor.callCount() != beforeCalls || afterHead != beforeHead {
			return "", fmt.Errorf("disabled request reached the supervisor or wrote: calls %d->%d head %d->%d",
				beforeCalls, supervisor.callCount(), beforeHead, afterHead)
		}
		if err := setConfig(config); err != nil {
			return "", err
		}
		return "404 board_disabled; supervisor calls and board head unchanged", nil
	}) {
		return errors.New("board_disabled gate E2E failed")
	}

	if !scenario.Step("a supervisor timeout is 504 and leaves the board unchanged", func() (string, error) {
		supervisor.configure(time.Second, nil)
		beforeHead, err := e.surface.Board().Head(e.tenant.String())
		if err != nil {
			return "", err
		}
		status, body := postAs("operator", blockTaskID, "block", map[string]any{"reason": "timeout"})
		if status != http.StatusGatewayTimeout || !strings.Contains(body, "supervisor_timeout") {
			return "", fmt.Errorf("timeout = %d %s", status, body)
		}
		afterHead, err := e.surface.Board().Head(e.tenant.String())
		if err != nil {
			return "", err
		}
		if afterHead != beforeHead {
			return "", fmt.Errorf("a timed-out action wrote the board: %d -> %d", beforeHead, afterHead)
		}
		return "504 supervisor_timeout; no board event", nil
	}) {
		return errors.New("supervisor timeout E2E failed")
	}

	if !scenario.Step("a supervisor 5xx is 502 and the action can be retried", func() (string, error) {
		supervisor.configure(0, errors.New("supervisor unavailable"))
		beforeHead, err := e.surface.Board().Head(e.tenant.String())
		if err != nil {
			return "", err
		}
		status, body := postAs("operator", blockTaskID, "block", map[string]any{"reason": "server error"})
		if status != http.StatusBadGateway || !strings.Contains(body, "supervisor_rejected") {
			return "", fmt.Errorf("5xx = %d %s", status, body)
		}
		afterHead, err := e.surface.Board().Head(e.tenant.String())
		if err != nil {
			return "", err
		}
		if afterHead != beforeHead {
			return "", fmt.Errorf("a rejected action wrote the board: %d -> %d", beforeHead, afterHead)
		}
		supervisor.configure(0, nil)
		status, body = postAs("operator", blockTaskID, "block", map[string]any{"reason": "server error"})
		if status != http.StatusOK {
			return "", fmt.Errorf("retry = %d %s", status, body)
		}
		return "502 then 200 on retry; no board event from the failure", nil
	}) {
		return errors.New("supervisor 5xx retry E2E failed")
	}
	_ = supervisor.ticketID
	return nil
}

func formatTaskID(id uint64) string {
	return fmt.Sprintf("%d", id)
}
