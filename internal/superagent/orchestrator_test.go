package superagent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
)

type fakeLauncher struct {
	mu        sync.Mutex
	launches  []collab.LaunchRequest
	cancelled []uuid.UUID
	launchErr error
}

func (f *fakeLauncher) Launch(_ context.Context, req collab.LaunchRequest) (collab.SessionRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launches = append(f.launches, req)
	if f.launchErr != nil {
		return collab.SessionRef{}, f.launchErr
	}
	return collab.SessionRef{
		TenantID:        req.TenantID,
		SessionID:       uuid.New(),
		ParentSessionID: req.ParentSessionID,
		BotID:           req.BotID,
		ModeID:          req.ModeID,
		WorkItemID:      req.WorkItemID,
		Depth:           req.Depth,
		State:           collab.SessionActive,
	}, nil
}

func (f *fakeLauncher) CancelSession(_ context.Context, _, sessionID uuid.UUID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, sessionID)
	return nil
}

func (f *fakeLauncher) setLaunchErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launchErr = err
}

func (f *fakeLauncher) lastLaunch() collab.LaunchRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.launches) == 0 {
		return collab.LaunchRequest{}
	}
	return f.launches[len(f.launches)-1]
}

func (f *fakeLauncher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.launches)
}

func (f *fakeLauncher) cancelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cancelled)
}

type fakePermissions struct {
	deny bool
}

func (p fakePermissions) CheckBotInvoke(context.Context, uuid.UUID, collab.SessionRef, string) error {
	if p.deny {
		return errors.New("bot.invoke denied by test")
	}
	return nil
}

type fakeAudit struct {
	mu     sync.Mutex
	events []collab.AuditEvent
	err    error
}

func (a *fakeAudit) Record(_ context.Context, event collab.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.events = append(a.events, event)
	return nil
}

func (a *fakeAudit) count(kind string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, event := range a.events {
		if event.Kind == kind {
			n++
		}
	}
	return n
}

type allowOperator struct{}

func (allowOperator) AuthorizeOperator(context.Context, uuid.UUID, uuid.UUID, string, string) error {
	return nil
}

type fixedPolicy struct {
	verdict superagent.Verdict
}

func (p fixedPolicy) Evaluate(context.Context, uuid.UUID, superagent.Event) (superagent.Verdict, error) {
	return p.verdict, nil
}

type fixedInspector struct {
	state superagent.WorkerState
	err   error
}

func (i fixedInspector) WorkerState(context.Context, uuid.UUID, uuid.UUID) (superagent.WorkerState, error) {
	return i.state, i.err
}

type recordingTreeCanceller struct {
	mu       sync.Mutex
	sessions []uuid.UUID
}

func (c *recordingTreeCanceller) CancelTree(_ context.Context, _, sessionID uuid.UUID, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions = append(c.sessions, sessionID)
	return nil
}

func (c *recordingTreeCanceller) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sessions)
}

type recordingSink struct {
	mu        sync.Mutex
	delivered []superagent.InputRequest
}

func (s *recordingSink) Deliver(_ context.Context, request superagent.InputRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered = append(s.delivered, request)
	return nil
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.delivered)
}

func (s *recordingSink) last() superagent.InputRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.delivered) == 0 {
		return superagent.InputRequest{}
	}
	return s.delivered[len(s.delivered)-1]
}

type changeRecorder struct {
	mu      sync.Mutex
	tenants []uuid.UUID
}

func (c *changeRecorder) record(tenantID uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tenants = append(c.tenants, tenantID)
}

func (c *changeRecorder) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.tenants)
}

func (c *changeRecorder) last() uuid.UUID {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.tenants) == 0 {
		return uuid.Nil
	}
	return c.tenants[len(c.tenants)-1]
}

func (c *changeRecorder) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tenants = nil
}

type envOpts struct {
	slots         int
	humanReserved int
	noOperator    bool
	policy        superagent.Policy
	inspector     superagent.WorkerInspector
	tree          superagent.WorkerTreeCanceller
}

type testEnv struct {
	tenant              uuid.UUID
	clock               *time.Time
	store               *superagent.MemoryStore
	registry            *collab.ModeRegistry
	sessions            *collab.MemorySessionIndex
	launcher            *fakeLauncher
	permissions         *fakePermissions
	audit               *fakeAudit
	source              *superagent.FixtureSource
	outbox              *superagent.Outbox
	host                *collab.Coordinator
	sink                *recordingSink
	changes             *changeRecorder
	orchestrator        *superagent.Orchestrator
	orchestratorSession collab.SessionRef
}

func newEnv(t *testing.T, opts envOpts) *testEnv {
	t.Helper()
	ctx := context.Background()
	tenant := uuid.New()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	env := &testEnv{tenant: tenant, clock: &now}
	ceiling := collab.Limits{Depth: collab.LimitedDepth(8), MaxChildren: 32, MaxConcurrent: 8, MaxTurns: 100, MaxTokens: 100_000, MaxDurationMs: 600_000}
	registry, err := collab.NewModeRegistry(tenant, ceiling)
	if err != nil {
		t.Fatalf("NewModeRegistry: %v", err)
	}
	if err := registry.Register(collab.SuperagentMode()); err != nil {
		t.Fatalf("Register superagent: %v", err)
	}
	if err := registry.SetEnabled(collab.ModeSuperagent, true); err != nil {
		t.Fatalf("enable superagent: %v", err)
	}
	env.registry = registry
	env.store = superagent.NewMemoryStore(func() time.Time { return *env.clock })
	env.sessions = collab.NewMemorySessionIndex()
	env.launcher = &fakeLauncher{}
	env.permissions = &fakePermissions{}
	env.audit = &fakeAudit{}
	env.source = superagent.NewFixtureSource(func() time.Time { return *env.clock })
	env.sink = &recordingSink{}
	env.changes = &changeRecorder{}
	env.orchestratorSession = collab.SessionRef{
		TenantID: tenant, SessionID: uuid.New(), BotID: "orchestrator",
		Depth: 0, State: collab.SessionActive, CreatedAt: now,
	}
	if err := env.sessions.Put(ctx, env.orchestratorSession); err != nil {
		t.Fatalf("put orchestrator session: %v", err)
	}
	outbox, err := superagent.NewOutbox(superagent.OutboxOptions{
		Store: env.store, Channel: env.source, Now: func() time.Time { return *env.clock },
		Backoff: func(int) time.Duration { return time.Second }, Audit: env.audit,
	})
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	env.outbox = outbox
	host, err := collab.NewHost(collab.HostOptions{
		TenantID: tenant, Modes: registry, Work: env.store, Sessions: env.sessions,
		Permissions: env.permissions, Audit: env.audit, Launcher: env.launcher,
		Attention: outbox, Now: func() time.Time { return *env.clock },
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	env.host = host
	env.orchestrator = env.newOrchestrator(t, opts)
	return env
}

func (e *testEnv) newOrchestrator(t *testing.T, opts envOpts) *superagent.Orchestrator {
	t.Helper()
	var slots *superagent.Slots
	if opts.slots > 0 {
		slots = superagent.NewSlots(e.store, superagent.SlotPolicy{Max: opts.slots, HumanReserved: opts.humanReserved})
	}
	var operator superagent.OperatorAuthorizer = allowOperator{}
	if opts.noOperator {
		operator = nil
	}
	policy := opts.policy
	if policy == nil {
		policy = &superagent.AllowlistPolicy{
			Actors: map[string]bool{"alice": true}, WorkerBot: "worker", PolicyVersion: "test-v1",
		}
	}
	orch, err := superagent.NewOrchestrator(superagent.Config{
		TenantID: e.tenant, Store: e.store, Host: e.host, Source: e.source, Channel: e.source,
		Policy:              policy,
		Inputs:              e.sink,
		Sessions:            e.sessions,
		Audit:               e.audit,
		Outbox:              e.outbox,
		Slots:               slots,
		Operator:            operator,
		WorkerInspector:     opts.inspector,
		WorkerTreeCanceller: opts.tree,
		OrchestratorSession: e.orchestratorSession.SessionID,
		OrchestratorBot:     "orchestrator",
		WorkerBot:           "worker",
		MaxPoll:             20,
		Now:                 func() time.Time { return *e.clock },
		Changed:             e.changes.record,
	})
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}
	return orch
}

func alice() superagent.Actor {
	return superagent.Actor{ID: "u1", Login: "alice"}
}

func issueEvent(id, threadID string) superagent.Event {
	return superagent.Event{
		SourceID: "fixture", EventID: id, Kind: superagent.EventIssueOpened,
		Actor: alice(), Thread: superagent.ThreadRef{Source: "fixture", Kind: "issue", ID: threadID},
		Title: "Fix " + threadID, Body: "Please fix it.",
	}
}

func TestPollCreatesTicketAssignmentAndAck(t *testing.T) {
	env := newEnv(t, envOpts{})
	if err := env.source.PushEvent(env.tenant, issueEvent("e1", "issue#1")); err != nil {
		t.Fatalf("PushEvent: %v", err)
	}
	if _, err := env.orchestrator.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	board, err := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if len(board.Tickets) != 1 {
		t.Fatalf("tickets = %d, want 1", len(board.Tickets))
	}
	ticket := board.Tickets[0]
	if ticket.Status != collab.WorkItemActive {
		t.Fatalf("ticket status = %s, want active", ticket.Status)
	}
	if ticket.Mandate.Requester.Login != "alice" || ticket.Mandate.PolicyVersion != "test-v1" {
		t.Fatalf("mandate = %+v", ticket.Mandate)
	}
	if env.launcher.count() != 1 {
		t.Fatalf("launches = %d, want 1", env.launcher.count())
	}
	if ticket.ActiveAssignment == nil || ticket.ActiveAssignment.SessionID == uuid.Nil {
		t.Fatalf("active assignment = %+v", ticket.ActiveAssignment)
	}
	if got := len(env.source.Messages()); got != 1 {
		t.Fatalf("channel messages = %d, want 1 ack", got)
	}
	cursor, err := env.store.GetSourceCursor(context.Background(), env.tenant, "fixture")
	if err != nil || cursor == "" {
		t.Fatalf("cursor = %q, err=%v", cursor, err)
	}
}

func TestDuplicateEventIsIdempotent(t *testing.T) {
	env := newEnv(t, envOpts{})
	event := issueEvent("e1", "issue#1")
	raw := collab.SourceEvent{
		SourceID: "fixture", Kind: string(superagent.EventIssueOpened), ExternalID: "e1",
		Payload: mustJSON(t, superagent.EventPayload{
			EventID: "e1", Kind: superagent.EventIssueOpened, Actor: alice(), Thread: event.Thread,
			Title: event.Title, Body: event.Body,
		}),
	}
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), raw); err != nil {
		t.Fatalf("first handle: %v", err)
	}
	result, err := env.orchestrator.HandleSourceEvent(context.Background(), raw)
	if err != nil {
		t.Fatalf("second handle: %v", err)
	}
	if !result.Duplicate {
		t.Fatalf("second handle result = %+v, want duplicate", result)
	}
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if len(board.Tickets) != 1 {
		t.Fatalf("tickets = %d, want 1", len(board.Tickets))
	}
	assignments, err := env.store.ListAssignments(context.Background(), env.tenant, board.Tickets[0].ID)
	if err != nil {
		t.Fatalf("ListAssignments: %v", err)
	}
	if len(assignments) != 1 {
		t.Fatalf("assignments = %d, want 1", len(assignments))
	}
}

func TestCommentFansOutToSubscriptionWithoutNewTicket(t *testing.T) {
	env := newEnv(t, envOpts{})
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, issueEvent("e1", "issue#1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}
	comment := superagent.Event{
		SourceID: "fixture", EventID: "c1", Kind: superagent.EventIssueComment,
		Actor: alice(), Thread: superagent.ThreadRef{Source: "fixture", Kind: "issue", ID: "issue#1"},
		Body: "Also update the tests.",
	}
	result, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, comment))
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	if result.Decision != string(superagent.ActionAttach) {
		t.Fatalf("decision = %q, want attach", result.Decision)
	}
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if len(board.Tickets) != 1 {
		t.Fatalf("tickets = %d, want 1", len(board.Tickets))
	}
	if env.sink.count() != 1 {
		t.Fatalf("deliveries = %d, want 1", env.sink.count())
	}
	if env.sink.last().RequestID != "followup:fixture:c1" {
		t.Fatalf("delivery = %+v", env.sink.last())
	}
}

// TestUnchangedIssueUpdateIsNotAFollowUp pins the live-GitHub echo rule: every
// Channel comment bumps the issue's updated_at, so the Source emits an
// issue_updated event. The durable snapshot is the delivery baseline, not the
// accept-time request: a real edit is delivered exactly once, and the comment
// bumps that follow must not re-deliver that edited body.
func TestUnchangedIssueUpdateIsNotAFollowUp(t *testing.T) {
	env := newEnv(t, envOpts{})
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, issueEvent("e1", "issue#1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}
	base := superagent.Event{
		SourceID: "fixture", EventID: "u0", Kind: superagent.EventIssueUpdated,
		Actor: alice(), Thread: superagent.ThreadRef{Source: "fixture", Kind: "issue", ID: "issue#1"},
		Title: "Fix issue#1", Body: "Please fix it.",
	}
	result, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, base))
	if err != nil {
		t.Fatalf("unchanged update: %v", err)
	}
	if result.Decision != string(superagent.ActionAttach) {
		t.Fatalf("decision = %q, want attach", result.Decision)
	}
	if env.sink.count() != 0 {
		t.Fatalf("deliveries = %d, want 0 for an unchanged body", env.sink.count())
	}

	// A real body edit is delivered once.
	edited := base
	edited.EventID = "u1"
	edited.Body = "Please fix it. Also update the tests."
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, edited)); err != nil {
		t.Fatalf("edited update: %v", err)
	}
	if env.sink.count() != 1 {
		t.Fatalf("deliveries = %d, want 1 after a real edit", env.sink.count())
	}

	// The next comment bumps updated_at; the body is the edited one, so the
	// snapshot (not the accept-time request) must suppress it.
	bump := edited
	bump.EventID = "u2"
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, bump)); err != nil {
		t.Fatalf("post-edit bump: %v", err)
	}
	if env.sink.count() != 1 {
		t.Fatalf("deliveries = %d, want 1: a post-edit comment bump re-delivered the edited body", env.sink.count())
	}

	// A title-only edit is also a real edit, delivered once...
	titled := bump
	titled.EventID = "u3"
	titled.Title = "Fix issue#1 (P1)"
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, titled)); err != nil {
		t.Fatalf("title-only update: %v", err)
	}
	if env.sink.count() != 2 {
		t.Fatalf("deliveries = %d, want 2 after a title-only edit", env.sink.count())
	}

	// ...and its own comment bumps do not re-deliver it either.
	titledAgain := titled
	titledAgain.EventID = "u4"
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, titledAgain)); err != nil {
		t.Fatalf("title bump: %v", err)
	}
	if env.sink.count() != 2 {
		t.Fatalf("deliveries = %d, want 2: a title bump re-delivered the title", env.sink.count())
	}

	// The snapshot is in the durable Ticket document.
	board, err := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if err != nil {
		t.Fatalf("board: %v", err)
	}
	delivered := board.Tickets[0].LastDelivered()
	if delivered.Title != "Fix issue#1 (P1)" || delivered.Body != "Please fix it. Also update the tests." {
		t.Fatalf("durable snapshot = %+v", delivered)
	}
}

func TestUnknownActorCommentStillFansOutToSubscribedTicket(t *testing.T) {
	env := newEnv(t, envOpts{})
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, issueEvent("e1", "issue#1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}
	comment := superagent.Event{
		SourceID: "fixture", EventID: "c-untrusted", Kind: superagent.EventIssueComment,
		Actor:  superagent.Actor{ID: "u2", Login: "mallory"},
		Thread: superagent.ThreadRef{Source: "fixture", Kind: "issue", ID: "issue#1"},
		Body:   "untrusted but relevant context",
	}
	result, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, comment))
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	if result.Decision != string(superagent.ActionAttach) {
		t.Fatalf("decision = %q, want attach", result.Decision)
	}
	if env.sink.count() != 1 {
		t.Fatalf("deliveries = %d, want 1", env.sink.count())
	}
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if len(board.Tickets) != 1 {
		t.Fatalf("tickets = %d, want 1", len(board.Tickets))
	}
}

func TestPolicyRejectsUnknownActor(t *testing.T) {
	env := newEnv(t, envOpts{})
	event := issueEvent("e1", "issue#1")
	event.Actor = superagent.Actor{ID: "u2", Login: "mallory"}
	result, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, event))
	if err != nil {
		t.Fatalf("HandleSourceEvent: %v", err)
	}
	if result.Decision != string(superagent.ActionIgnore) {
		t.Fatalf("decision = %q, want ignore", result.Decision)
	}
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if len(board.Tickets) != 0 {
		t.Fatalf("tickets = %d, want 0", len(board.Tickets))
	}
	seen, err := env.store.SourceEventSeen(context.Background(), env.tenant, "fixture", "e1")
	if err != nil || !seen {
		t.Fatalf("seen = %v, err=%v", seen, err)
	}
}

func TestResumeTicketReopensAndDispatches(t *testing.T) {
	env := newEnv(t, envOpts{})
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, issueEvent("e1", "issue#1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	ticket := board.Tickets[0]
	if err := env.orchestrator.Report(context.Background(), superagent.ReportRequest{
		TenantID: env.tenant, WorkItemID: ticket.ID, AssignmentID: ticket.ActiveAssignment.ID,
		State: superagent.ReportReady, Summary: "done",
	}); err != nil {
		t.Fatalf("Report ready: %v", err)
	}
	if _, err := env.orchestrator.ResumeTicket(context.Background(), env.tenant, ticket.ID, "alice", "please continue"); err != nil {
		t.Fatalf("ResumeTicket: %v", err)
	}
	board, _ = env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	resumed := board.Tickets[0]
	if resumed.Status != collab.WorkItemActive || resumed.ActiveAssignment == nil {
		t.Fatalf("resumed = %+v", resumed)
	}
	if env.launcher.count() != 2 {
		t.Fatalf("launches = %d, want 2 (initial + resume)", env.launcher.count())
	}
}

func TestSubscriptionDoesNotAutoResumeFinishedTicket(t *testing.T) {
	env := newEnv(t, envOpts{})
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, issueEvent("e1", "issue#1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	ticket := board.Tickets[0]
	if err := env.orchestrator.Report(context.Background(), superagent.ReportRequest{
		TenantID: env.tenant, WorkItemID: ticket.ID, AssignmentID: ticket.ActiveAssignment.ID,
		State: superagent.ReportReady, Summary: "done",
	}); err != nil {
		t.Fatalf("Report ready: %v", err)
	}
	comment := superagent.Event{
		SourceID: "fixture", EventID: "c-after-done", Kind: superagent.EventIssueComment,
		Actor: alice(), Thread: superagent.ThreadRef{Source: "fixture", Kind: "issue", ID: "issue#1"},
		Body: "one more thing",
	}
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, comment)); err != nil {
		t.Fatalf("comment: %v", err)
	}
	board, _ = env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if board.Tickets[0].Status != collab.WorkItemDone {
		t.Fatalf("status = %s, want done", board.Tickets[0].Status)
	}
	if env.sink.count() != 0 {
		t.Fatalf("deliveries = %d, want 0", env.sink.count())
	}
}

func TestHumanStopRevokesMandateAndCancelsWorker(t *testing.T) {
	env := newEnv(t, envOpts{})
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, issueEvent("e1", "issue#1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}
	stop := superagent.Event{
		SourceID: "fixture", EventID: "stop-1", Kind: superagent.EventIssueComment,
		Actor: alice(), Thread: superagent.ThreadRef{Source: "fixture", Kind: "issue", ID: "issue#1"},
		Body: "/keel stop",
	}
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, stop)); err != nil {
		t.Fatalf("stop: %v", err)
	}
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if len(board.Tickets) != 1 {
		t.Fatalf("tickets = %d", len(board.Tickets))
	}
	ticket := board.Tickets[0]
	if ticket.Status != collab.WorkItemCancelled {
		t.Fatalf("status = %s, want cancelled", ticket.Status)
	}
	if !ticket.Mandate.Revoked() {
		t.Fatal("mandate should be revoked")
	}
	if env.launcher.cancelCount() != 1 {
		t.Fatalf("cancels = %d, want 1", env.launcher.cancelCount())
	}
	subs, err := env.store.ListThreadSubscriptions(context.Background(), env.tenant, ticket.Thread)
	if err != nil {
		t.Fatalf("ListThreadSubscriptions: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("active subscriptions = %d, want 0", len(subs))
	}
}

func TestAttentionAnswerWakesSameTicket(t *testing.T) {
	env := newEnv(t, envOpts{})
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, issueEvent("e1", "issue#1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	ticket := board.Tickets[0]
	assignment := ticket.ActiveAssignment
	attention, err := env.orchestrator.Ask(context.Background(), ticket.ID, assignment.ID, collab.AttentionRequest{
		Prompt: "Which branch?",
		Options: []collab.AttentionOption{
			{ID: "main", Label: "main"}, {ID: "next", Label: "next"},
		},
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if attention.Route != collab.AttentionRouteChannel || attention.Channel != "issue#1" {
		t.Fatalf("attention route = %+v", attention)
	}
	if err := env.orchestrator.Report(context.Background(), superagent.ReportRequest{
		TenantID: env.tenant, WorkItemID: ticket.ID, AssignmentID: assignment.ID,
		State: superagent.ReportWaiting, Summary: "need a decision", AttentionID: attention.ID,
	}); err != nil {
		t.Fatalf("Report waiting: %v", err)
	}
	blocked, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if blocked.Tickets[0].Status != collab.WorkItemBlocked {
		t.Fatalf("status = %s, want blocked", blocked.Tickets[0].Status)
	}
	answered, err := env.orchestrator.AnswerAttention(context.Background(), env.tenant, attention.ID, "next", "", "alice")
	if err != nil {
		t.Fatalf("AnswerAttention: %v", err)
	}
	if answered.Status != collab.AttentionAnswered || answered.Answer == nil || answered.Answer.OptionID != "next" {
		t.Fatalf("answered = %+v", answered)
	}
	active, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if active.Tickets[0].Status != collab.WorkItemActive {
		t.Fatalf("status = %s, want active", active.Tickets[0].Status)
	}
	if env.sink.count() != 1 {
		t.Fatalf("deliveries = %d, want 1", env.sink.count())
	}
	if env.sink.last().Kind != "attention_answer" || env.sink.last().RequestID != "attention:"+attention.ID.String() {
		t.Fatalf("delivery = %+v", env.sink.last())
	}
}

func TestRecoveryMarksLostAssignmentAndRedispatches(t *testing.T) {
	env := newEnv(t, envOpts{})
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, issueEvent("e1", "issue#1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	ticket := board.Tickets[0]
	firstAssignment := *ticket.ActiveAssignment

	// Simulate a restart: the session index is rebuilt with only the
	// orchestrator root; the durable ticket/assignment rows survive.
	restartSessions := collab.NewMemorySessionIndex()
	if err := restartSessions.Put(context.Background(), env.orchestratorSession); err != nil {
		t.Fatalf("put orchestrator: %v", err)
	}
	host, err := collab.NewHost(collab.HostOptions{
		TenantID: env.tenant, Modes: env.registry, Work: env.store, Sessions: restartSessions,
		Permissions: env.permissions, Audit: env.audit, Launcher: env.launcher,
		Attention: env.outbox, Now: func() time.Time { return *env.clock },
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	orch, err := superagent.NewOrchestrator(superagent.Config{
		TenantID: env.tenant, Store: env.store, Host: host, Source: env.source, Channel: env.source,
		Policy: &superagent.AllowlistPolicy{Actors: map[string]bool{"alice": true}, WorkerBot: "worker"},
		Inputs: env.sink, Sessions: restartSessions, Audit: env.audit, Outbox: env.outbox,
		OrchestratorSession: env.orchestratorSession.SessionID, OrchestratorBot: "orchestrator",
		WorkerBot: "worker", Now: func() time.Time { return *env.clock },
		Changed: env.changes.record,
	})
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}
	if _, err := orch.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if env.changes.count() == 0 {
		t.Fatal("Recover did not publish a board invalidation")
	}
	assignments, err := env.store.ListAssignments(context.Background(), env.tenant, ticket.ID)
	if err != nil {
		t.Fatalf("ListAssignments: %v", err)
	}
	if len(assignments) != 2 {
		t.Fatalf("assignments = %d, want 2 (old interrupted + new)", len(assignments))
	}
	var old, replacement *collab.Assignment
	for i := range assignments {
		if assignments[i].ID == firstAssignment.ID {
			old = &assignments[i]
		} else {
			replacement = &assignments[i]
		}
	}
	if old == nil || replacement == nil {
		t.Fatalf("assignments = %+v, want the original and a replacement", assignments)
	}
	if old.Status != collab.AssignmentFailed || !strings.Contains(old.Error, "recovery") {
		t.Fatalf("old assignment = %+v, want failed with a recovery note", old)
	}
	if replacement.Status != collab.AssignmentActive || replacement.SessionID == uuid.Nil {
		t.Fatalf("replacement assignment = %+v, want active with a session", replacement)
	}
}

func TestConcurrencySlotsQueueAndDrain(t *testing.T) {
	env := newEnv(t, envOpts{slots: 1})
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, issueEvent("e1", "issue#1"))); err != nil {
		t.Fatalf("first accept: %v", err)
	}
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, issueEvent("e2", "issue#2"))); err != nil {
		t.Fatalf("second accept: %v", err)
	}
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if len(board.Tickets) != 2 {
		t.Fatalf("tickets = %d, want 2", len(board.Tickets))
	}
	var active, open int
	for _, ticket := range board.Tickets {
		switch ticket.Status {
		case collab.WorkItemActive:
			active++
		case collab.WorkItemOpen:
			open++
		}
	}
	if active != 1 || open != 1 {
		t.Fatalf("statuses active=%d open=%d, want 1/1", active, open)
	}
	var first superagent.TicketView
	for _, ticket := range board.Tickets {
		if ticket.Status == collab.WorkItemActive {
			first = ticket
			break
		}
	}
	if err := env.orchestrator.Report(context.Background(), superagent.ReportRequest{
		TenantID: env.tenant, WorkItemID: first.ID, AssignmentID: first.ActiveAssignment.ID,
		State: superagent.ReportReady, Summary: "done",
	}); err != nil {
		t.Fatalf("Report ready: %v", err)
	}
	board, _ = env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	active = 0
	for _, ticket := range board.Tickets {
		if ticket.Status == collab.WorkItemActive {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active after drain = %d, want 1", active)
	}
	if env.launcher.count() != 2 {
		t.Fatalf("launches = %d, want 2", env.launcher.count())
	}
}

func TestCrossTenantIsolation(t *testing.T) {
	env := newEnv(t, envOpts{})
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, issueEvent("e1", "issue#1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}
	other := uuid.New()
	if _, err := env.orchestrator.Board(context.Background(), other, superagent.BoardFilter{}); !errors.Is(err, collab.ErrTenantMismatch) {
		t.Fatalf("Board other tenant err = %v, want tenant mismatch", err)
	}
	if _, err := env.orchestrator.Inbox(context.Background(), other); !errors.Is(err, collab.ErrTenantMismatch) {
		t.Fatalf("Inbox other tenant err = %v, want tenant mismatch", err)
	}
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if _, err := env.store.GetWorkItem(context.Background(), other, board.Tickets[0].ID); !errors.Is(err, collab.ErrWorkItemNotFound) {
		t.Fatalf("foreign GetWorkItem err = %v, want not found", err)
	}
}

func TestOutboxRetryAndIdempotency(t *testing.T) {
	env := newEnv(t, envOpts{})
	env.source.FailSends(1)
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, issueEvent("e1", "issue#1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := env.outbox.Flush(context.Background(), env.tenant, 10); err != nil {
		t.Fatalf("first Flush: %v", err)
	}
	messages, err := env.store.ListOutbox(context.Background(), env.tenant, superagent.OutboxPending, 10)
	if err != nil {
		t.Fatalf("ListOutbox: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("pending = %d, want 1", len(messages))
	}
	if messages[0].Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", messages[0].Attempts)
	}
	// Same idempotency key must return the same row, not a second message.
	duplicate, err := env.outbox.Enqueue(context.Background(), superagent.OutboxMessage{
		TenantID: env.tenant, ChannelID: "issue#1", Text: "duplicate",
		IdempotencyKey: messages[0].IdempotencyKey,
	})
	if err != nil {
		t.Fatalf("Enqueue duplicate: %v", err)
	}
	if duplicate.ID != messages[0].ID {
		t.Fatalf("duplicate id = %s, want %s", duplicate.ID, messages[0].ID)
	}
	*env.clock = env.clock.Add(2 * time.Second)
	if _, err := env.outbox.Flush(context.Background(), env.tenant, 10); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := len(env.source.Messages()); got != 1 {
		t.Fatalf("messages = %d, want 1", got)
	}
	sent, err := env.store.ListOutbox(context.Background(), env.tenant, superagent.OutboxSent, 10)
	if err != nil || len(sent) != 1 {
		t.Fatalf("sent = %d, err=%v", len(sent), err)
	}
}

func TestTicketTransitions(t *testing.T) {
	valid := [][2]collab.WorkItemStatus{
		{collab.WorkItemOpen, collab.WorkItemActive},
		{collab.WorkItemOpen, collab.WorkItemDone},
		{collab.WorkItemOpen, collab.WorkItemFailed},
		{collab.WorkItemActive, collab.WorkItemBlocked},
		{collab.WorkItemBlocked, collab.WorkItemActive},
		{collab.WorkItemActive, collab.WorkItemDone},
		{collab.WorkItemActive, collab.WorkItemFailed},
		{collab.WorkItemDone, collab.WorkItemOpen},
		{collab.WorkItemFailed, collab.WorkItemOpen},
		{collab.WorkItemActive, collab.WorkItemCancelled},
	}
	for _, pair := range valid {
		if err := superagent.ValidateTransition(pair[0], pair[1]); err != nil {
			t.Fatalf("transition %s -> %s: %v", pair[0], pair[1], err)
		}
	}
	if err := superagent.ValidateTransition(collab.WorkItemCancelled, collab.WorkItemOpen); err == nil {
		t.Fatal("cancelled -> open must be refused")
	}
}

func TestAcceptedTicketIsFailClosedWithoutAudit(t *testing.T) {
	env := newEnv(t, envOpts{})
	env.audit.err = errors.New("audit is down")
	result, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, issueEvent("e1", "issue#1")))
	if err == nil {
		t.Fatalf("event %+v should fail when audit is unavailable", result)
	}
	if !errors.Is(err, collab.ErrAuditUnavailable) {
		t.Fatalf("err = %v, want audit unavailable", err)
	}
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if len(board.Tickets) != 0 {
		t.Fatalf("tickets = %d, want 0 when audit failed", len(board.Tickets))
	}
}

func TestWorkerBotInvokeDenialDoesNotBypassCore(t *testing.T) {
	env := newEnv(t, envOpts{})
	env.permissions.deny = true
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, issueEvent("e1", "issue#1"))); err != nil {
		t.Fatalf("accept event: %v", err)
	}
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if len(board.Tickets) != 1 || board.Tickets[0].Status != collab.WorkItemOpen {
		t.Fatalf("tickets = %+v, want one open ticket", board.Tickets)
	}
	assignments, err := env.store.ListAssignments(context.Background(), env.tenant, board.Tickets[0].ID)
	if err != nil {
		t.Fatalf("ListAssignments: %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("assignments = %d, want 0 after bot.invoke denial", len(assignments))
	}
	if env.launcher.count() != 0 {
		t.Fatalf("launches = %d, want 0 after bot.invoke denial", env.launcher.count())
	}
}

func TestAttentionExpires(t *testing.T) {
	env := newEnv(t, envOpts{})
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, issueEvent("e1", "issue#1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	ticket := board.Tickets[0]
	attention, err := env.orchestrator.Ask(context.Background(), ticket.ID, ticket.ActiveAssignment.ID, collab.AttentionRequest{
		Prompt: "Which branch?", Options: []collab.AttentionOption{{ID: "main"}}, Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	*env.clock = env.clock.Add(2 * time.Second)
	expired, err := env.orchestrator.ExpireAttentions(context.Background())
	if err != nil || expired != 1 {
		t.Fatalf("ExpireAttentions = %d, err=%v", expired, err)
	}
	stored, err := env.store.GetAttention(context.Background(), env.tenant, attention.ID)
	if err != nil || stored.Status != collab.AttentionTimedOut {
		t.Fatalf("attention = %+v, err=%v", stored, err)
	}
	inbox, err := env.orchestrator.Inbox(context.Background(), env.tenant)
	if err != nil || len(inbox.Items) != 0 {
		t.Fatalf("inbox = %+v, err=%v", inbox, err)
	}
}

func TestReconcileReleasesCancelledWorkerSlot(t *testing.T) {
	env := newEnv(t, envOpts{slots: 1})
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, issueEvent("e1", "issue#1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	ticket := board.Tickets[0]
	if err := env.sessions.SetState(context.Background(), env.tenant, ticket.ActiveAssignment.SessionID, collab.SessionCancelled); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	changed, err := env.orchestrator.Reconcile(context.Background())
	if err != nil || changed != 1 {
		t.Fatalf("Reconcile = %d, err=%v", changed, err)
	}
	if _, err := env.orchestrator.DispatchQueued(context.Background()); err != nil {
		t.Fatalf("DispatchQueued: %v", err)
	}
	assignments, err := env.store.ListAssignments(context.Background(), env.tenant, ticket.ID)
	if err != nil {
		t.Fatalf("ListAssignments: %v", err)
	}
	if len(assignments) != 2 {
		t.Fatalf("assignments = %d, want cancelled old + active new", len(assignments))
	}
	active := 0
	for _, assignment := range assignments {
		if assignment.Status == collab.AssignmentActive {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active assignments = %d, want 1: %+v", active, assignments)
	}
}

func TestOperatorActionsRequireSeam(t *testing.T) {
	env := newEnv(t, envOpts{noOperator: true})
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, issueEvent("e1", "issue#1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	ticket := board.Tickets[0]
	if err := env.orchestrator.StopTicket(context.Background(), env.tenant, ticket.ID, "alice", "stop"); !errors.Is(err, superagent.ErrOperatorUnavailable) {
		t.Fatalf("StopTicket err = %v, want operator unavailable", err)
	}
	if _, err := env.orchestrator.ResumeTicket(context.Background(), env.tenant, ticket.ID, "alice", "resume"); !errors.Is(err, superagent.ErrOperatorUnavailable) {
		t.Fatalf("ResumeTicket err = %v, want operator unavailable", err)
	}
}

func TestDuplicateAnswerDoesNotReopenOrDispatch(t *testing.T) {
	env := newEnv(t, envOpts{})
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	ticket := board.Tickets[0]
	attention, err := env.orchestrator.Ask(context.Background(), ticket.ID, ticket.ActiveAssignment.ID, collab.AttentionRequest{
		Prompt: "Which branch?", Options: []collab.AttentionOption{{ID: "main"}, {ID: "next"}},
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := env.orchestrator.AnswerAttention(context.Background(), env.tenant, attention.ID, "next", "", "alice"); err != nil {
		t.Fatalf("first answer: %v", err)
	}
	// Mark the ticket done and retry the same answer: it must return the
	// original result without reopening or dispatching another worker.
	item, _ := env.store.GetWorkItem(context.Background(), env.tenant, ticket.ID)
	item.Status = collab.WorkItemDone
	if _, err := env.store.UpdateWorkItem(context.Background(), item); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	launches := env.launcher.count()
	if _, err := env.orchestrator.AnswerAttention(context.Background(), env.tenant, attention.ID, "next", "", "alice"); err != nil {
		t.Fatalf("duplicate answer: %v", err)
	}
	stored, _ := env.store.GetWorkItem(context.Background(), env.tenant, ticket.ID)
	if stored.Status != collab.WorkItemDone {
		t.Fatalf("status = %s, want done (duplicate answer must not reopen)", stored.Status)
	}
	if env.launcher.count() != launches {
		t.Fatalf("launches = %d, want %d", env.launcher.count(), launches)
	}
}

func TestUntrustedCommentDoesNotResumeBlockedTicket(t *testing.T) {
	env := newEnv(t, envOpts{})
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	ticket := board.Tickets[0]
	attention, err := env.orchestrator.Ask(context.Background(), ticket.ID, ticket.ActiveAssignment.ID, collab.AttentionRequest{
		Prompt: "Which branch?", Options: []collab.AttentionOption{{ID: "main"}, {ID: "next"}},
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if err := env.orchestrator.Report(context.Background(), superagent.ReportRequest{
		TenantID: env.tenant, WorkItemID: ticket.ID, AssignmentID: ticket.ActiveAssignment.ID,
		State: superagent.ReportWaiting, AttentionID: attention.ID,
	}); err != nil {
		t.Fatalf("Report waiting: %v", err)
	}
	// Simulate no live worker.
	assignment := *ticket.ActiveAssignment
	assignment.Status = collab.AssignmentFailed
	if _, err := env.store.UpdateAssignment(context.Background(), assignment); err != nil {
		t.Fatalf("fail assignment: %v", err)
	}
	untrusted := superagent.Event{
		SourceID: "fixture", EventID: "c-untrusted", Kind: superagent.EventIssueComment,
		Actor:  superagent.Actor{ID: "u2", Login: "mallory"},
		Thread: superagent.ThreadRef{Source: "fixture", Kind: "issue", ID: "issue#1"},
		Body:   "please continue",
	}
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, untrusted)); err != nil {
		t.Fatalf("untrusted comment: %v", err)
	}
	blocked, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if blocked.Tickets[0].Status != collab.WorkItemBlocked {
		t.Fatalf("status = %s, want blocked", blocked.Tickets[0].Status)
	}
	if env.launcher.count() != 1 {
		t.Fatalf("launches = %d, want 1 (untrusted comment must not start a worker)", env.launcher.count())
	}
	trusted := untrusted
	trusted.EventID = "c-trusted"
	trusted.Actor = alice()
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, trusted)); err != nil {
		t.Fatalf("trusted follow-up: %v", err)
	}
	active, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if active.Tickets[0].Status != collab.WorkItemActive {
		t.Fatalf("status = %s, want active after trusted follow-up", active.Tickets[0].Status)
	}
	if env.launcher.count() != 2 {
		t.Fatalf("launches = %d, want 2 after trusted follow-up", env.launcher.count())
	}
}

func TestSourceCommentAnswersOpenAttention(t *testing.T) {
	env := newEnv(t, envOpts{})
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	ticket := board.Tickets[0]
	attention, err := env.orchestrator.Ask(context.Background(), ticket.ID, ticket.ActiveAssignment.ID, collab.AttentionRequest{
		Prompt: "Which branch?", Options: []collab.AttentionOption{{ID: "main", Label: "main"}, {ID: "next", Label: "next"}},
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	comment := superagent.Event{
		SourceID: "fixture", EventID: "answer-1", Kind: superagent.EventIssueComment,
		Actor: alice(), Thread: superagent.ThreadRef{Source: "fixture", Kind: "issue", ID: "issue#1"},
		Body: "next",
	}
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, comment)); err != nil {
		t.Fatalf("answer comment: %v", err)
	}
	stored, err := env.store.GetAttention(context.Background(), env.tenant, attention.ID)
	if err != nil || stored.Status != collab.AttentionAnswered {
		t.Fatalf("attention = %+v, err=%v", stored, err)
	}
	if stored.Answer == nil || stored.Answer.OptionID != "next" || stored.Answer.Actor != "alice" {
		t.Fatalf("answer = %+v", stored.Answer)
	}
	if env.sink.count() != 1 || env.sink.last().Kind != "attention_answer" {
		t.Fatalf("deliveries = %+v", env.sink.delivered)
	}
	assignments, _ := env.store.ListAssignments(context.Background(), env.tenant, ticket.ID)
	if len(assignments) != 1 {
		t.Fatalf("assignments = %d, want the original worker only", len(assignments))
	}
}

func TestWorkerInspectorFailsOverStaleActiveSession(t *testing.T) {
	env := newEnv(t, envOpts{inspector: fixedInspector{state: superagent.WorkerFailed}})
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	ticket := board.Tickets[0]
	if _, err := env.orchestrator.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	assignments, _ := env.store.ListAssignments(context.Background(), env.tenant, ticket.ID)
	var failed, active int
	for _, assignment := range assignments {
		if assignment.Status == collab.AssignmentFailed {
			failed++
		}
		if assignment.Status == collab.AssignmentActive {
			active++
		}
	}
	if failed != 1 || active != 1 {
		t.Fatalf("failed=%d active=%d, want one interrupted + one replacement: %+v", failed, active, assignments)
	}
}

func TestOutboxPerThreadOrderingAndTicketBinding(t *testing.T) {
	env := newEnv(t, envOpts{})
	if _, err := env.outbox.Enqueue(context.Background(), superagent.OutboxMessage{
		TenantID: env.tenant, ChannelID: "thread-1", Text: "first", IdempotencyKey: "m1",
	}); err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	*env.clock = env.clock.Add(time.Millisecond)
	if _, err := env.outbox.Enqueue(context.Background(), superagent.OutboxMessage{
		TenantID: env.tenant, ChannelID: "thread-1", Text: "second", IdempotencyKey: "m2",
	}); err != nil {
		t.Fatalf("enqueue second: %v", err)
	}
	env.source.FailSends(1)
	if _, err := env.outbox.Flush(context.Background(), env.tenant, 10); err != nil {
		t.Fatalf("first flush: %v", err)
	}
	if got := len(env.source.Messages()); got != 0 {
		t.Fatalf("messages after failed first = %d, want 0 (later message must wait)", got)
	}
	*env.clock = env.clock.Add(2 * time.Second)
	if _, err := env.outbox.Flush(context.Background(), env.tenant, 10); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	messages := env.source.Messages()
	if len(messages) != 2 || messages[0].Message.Text != "first" || messages[1].Message.Text != "second" {
		t.Fatalf("messages = %+v, want first then second", messages)
	}

	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if _, err := env.outbox.Enqueue(context.Background(), superagent.OutboxMessage{
		TenantID: env.tenant, WorkItemID: board.Tickets[0].ID, ChannelID: "another-thread",
		Text: "leak", IdempotencyKey: "leak",
	}); err == nil {
		t.Fatal("a ticket-bound outbox message must be refused on a different channel")
	}
}

func TestStopCancelsWorkerSubtree(t *testing.T) {
	tree := &recordingTreeCanceller{}
	env := newEnv(t, envOpts{tree: tree})
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	ticket := board.Tickets[0]
	stop := superagent.Event{
		SourceID: "fixture", EventID: "stop-tree", Kind: superagent.EventIssueComment,
		Actor: alice(), Thread: superagent.ThreadRef{Source: "fixture", Kind: "issue", ID: "issue#1"},
		Body: "/keel stop",
	}
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, stop)); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if tree.count() != 1 {
		t.Fatalf("tree cancels = %d, want 1", tree.count())
	}
	if tree.sessions[0] != ticket.ActiveAssignment.SessionID {
		t.Fatalf("tree cancel session = %s, want %s", tree.sessions[0], ticket.ActiveAssignment.SessionID)
	}
}

func TestObserveMandateDoesNotDispatch(t *testing.T) {
	env := newEnv(t, envOpts{policy: fixedPolicy{verdict: superagent.Verdict{
		Action: superagent.ActionAccept, Strength: superagent.MandateObserve,
	}}})
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if len(board.Tickets) != 1 || board.Tickets[0].Status != collab.WorkItemBlocked {
		t.Fatalf("tickets = %+v, want one observe-blocked ticket", board.Tickets)
	}
	assignments, _ := env.store.ListAssignments(context.Background(), env.tenant, board.Tickets[0].ID)
	if len(assignments) != 0 || env.launcher.count() != 0 {
		t.Fatalf("assignments=%d launches=%d, want observe-only without a worker", len(assignments), env.launcher.count())
	}
}

func TestHousekeepingRunsWhenPollFails(t *testing.T) {
	env := newEnv(t, envOpts{})
	if _, err := env.outbox.Enqueue(context.Background(), superagent.OutboxMessage{
		TenantID: env.tenant, ChannelID: "thread-1", Text: "pending", IdempotencyKey: "pending-1",
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	env.source.FailPolls(1)
	if _, err := env.orchestrator.PollOnce(context.Background()); err == nil {
		t.Fatal("PollOnce should surface the source failure")
	}
	if got := len(env.source.Messages()); got != 1 {
		t.Fatalf("messages = %d, want outbox flushed despite poll failure", got)
	}
}

func TestResumeCarriesFollowUpTextToWorkerTask(t *testing.T) {
	env := newEnv(t, envOpts{})
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	ticket := board.Tickets[0]
	if err := env.orchestrator.Report(context.Background(), superagent.ReportRequest{
		TenantID: env.tenant, WorkItemID: ticket.ID, AssignmentID: ticket.ActiveAssignment.ID,
		State: superagent.ReportReady, Summary: "done",
	}); err != nil {
		t.Fatalf("Report ready: %v", err)
	}
	if _, err := env.orchestrator.ResumeTicket(context.Background(), env.tenant, ticket.ID, "alice", "please add regression tests"); err != nil {
		t.Fatalf("ResumeTicket: %v", err)
	}
	launch := env.launcher.lastLaunch()
	if !strings.Contains(launch.Task, "please add regression tests") {
		t.Fatalf("worker task = %q, want the follow-up text", launch.Task)
	}
}

func TestDispatchFailureClosesAssignmentAndRetries(t *testing.T) {
	env := newEnv(t, envOpts{})
	env.launcher.setLaunchErr(errors.New("launcher unavailable"))
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if len(board.Tickets) != 1 || board.Tickets[0].Status != collab.WorkItemOpen {
		t.Fatalf("tickets = %+v, want one open ticket after launch failure", board.Tickets)
	}
	assignments, _ := env.store.ListAssignments(context.Background(), env.tenant, board.Tickets[0].ID)
	if len(assignments) != 1 || assignments[0].Status != collab.AssignmentFailed {
		t.Fatalf("assignments = %+v, want one failed assignment (no open orphan)", assignments)
	}
	env.launcher.setLaunchErr(nil)
	if _, err := env.orchestrator.DispatchQueued(context.Background()); err != nil {
		t.Fatalf("DispatchQueued retry: %v", err)
	}
	ready, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if ready.Tickets[0].Status != collab.WorkItemActive || ready.Tickets[0].ActiveAssignment == nil {
		t.Fatalf("ticket = %+v, want active after retry", ready.Tickets[0])
	}
}

func TestAnswerWithoutLiveWorkerCarriesAnswerText(t *testing.T) {
	env := newEnv(t, envOpts{})
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	board, _ := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	ticket := board.Tickets[0]
	attention, err := env.orchestrator.Ask(context.Background(), ticket.ID, ticket.ActiveAssignment.ID, collab.AttentionRequest{
		Prompt: "What next?", AllowFreeText: true,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	assignment := *ticket.ActiveAssignment
	assignment.Status = collab.AssignmentFailed
	if _, err := env.store.UpdateAssignment(context.Background(), assignment); err != nil {
		t.Fatalf("fail assignment: %v", err)
	}
	if _, err := env.orchestrator.AnswerAttention(context.Background(), env.tenant, attention.ID, "", "use the staging branch", "alice"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	launch := env.launcher.lastLaunch()
	if !strings.Contains(launch.Task, "use the staging branch") {
		t.Fatalf("worker task = %q, want the answer text", launch.Task)
	}
}

func acceptEvent(t *testing.T, env *testEnv, event superagent.Event) {
	t.Helper()
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, event)); err != nil {
		t.Fatalf("accept event %s: %v", event.EventID, err)
	}
}

func fixtureEvent(t *testing.T, event superagent.Event) collab.SourceEvent {
	t.Helper()
	return collab.SourceEvent{
		SourceID: event.SourceID, Kind: string(event.Kind), ExternalID: event.EventID,
		Payload: mustJSON(t, superagent.EventPayload{
			EventID: event.EventID, Kind: event.Kind, Actor: event.Actor, Thread: event.Thread,
			Title: event.Title, Body: event.Body, URL: event.URL,
		}),
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
