package superagentboard

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	keelstore "github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/superagent"
)

func mirrorFixture(t *testing.T) (*Mirror, *superagent.MemoryStore, *LogBoard, *memoryLinks, *testClock, uuid.UUID) {
	t.Helper()
	tenantID := uuid.New()
	clock := newTestClock()
	board := newTestBoard(clock)
	links := newMemoryLinks()
	store := superagent.NewMemoryStore(clock.Now)
	mirror := newMirrorForTest(t, tenantID, store, board, links,
		SuperagentConfig{Mirror: true, Bridge: true, Channel: ChannelForTest()}, clock)
	return mirror, store, board, links, clock, tenantID
}

func ChannelForTest() string { return "superagent" }

func TestMirrorTicketIsIdempotentAcrossPassesAndRestart(t *testing.T) {
	mirror, store, board, links, clock, tenantID := mirrorFixture(t)
	ticket := seedTicket(t, store, tenantID, "Fix the build", "The CI job fails.", "github", "42", "evt-1")

	first, err := mirror.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if first.TasksCreated != 1 {
		t.Fatalf("first report = %+v", first)
	}
	link := mustLink(t, links, tenantID, ticket.ID.String(), storeTicketKind())
	task := mustTask(t, board, tenantID, link.BoardTaskID)
	if task.State != StateReady {
		t.Fatalf("state = %q", task.State)
	}
	head := boardHead(t, board, tenantID)

	second, err := mirror.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if second.TasksCreated != 0 {
		t.Fatalf("second pass created a task: %+v", second)
	}
	if head != boardHead(t, board, tenantID) {
		t.Fatal("a repeated sync appended board events")
	}

	// A fresh process (new Mirror and a new LogBoard over the same stores)
	// finds the link instead of creating a second thread.
	restarted := newMirrorForTest(t, tenantID, store, newTestBoard(clock), links,
		SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"}, clock)
	if _, err := restarted.SyncOnce(context.Background()); err != nil {
		t.Fatalf("restart sync: %v", err)
	}
	again := mustLink(t, links, tenantID, ticket.ID.String(), storeTicketKind())
	if again.BoardTaskID != link.BoardTaskID {
		t.Fatalf("restart mapped task %d, want %d", again.BoardTaskID, link.BoardTaskID)
	}
}

func storeTicketKind() string { return keelstore.BoardLinkTicket }

func TestMirrorClaimRenewAndLeaseExpiry(t *testing.T) {
	mirror, store, board, links, clock, tenantID := mirrorFixture(t)
	ticket := seedTicket(t, store, tenantID, "Fix the build", "Body", "manual", "t-1", "evt-1")
	assignment := addAssignment(t, store, ticket, collab.AssignmentActive, "researcher", uuid.New())
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemActive)

	report, err := mirror.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if report.Claims != 1 {
		t.Fatalf("report = %+v", report)
	}
	link := mustLink(t, links, tenantID, ticket.ID.String(), keelstore.BoardLinkTicket)
	task := mustTask(t, board, tenantID, link.BoardTaskID)
	if task.State != StateClaimed || task.Epoch != 1 {
		t.Fatalf("task = %+v", task)
	}
	if task.ClaimActor != WorkerActor(assignment.BotID).ID {
		t.Fatalf("claim actor = %q", task.ClaimActor)
	}
	if link.ClaimEpoch != 1 {
		t.Fatalf("link claim epoch = %d", link.ClaimEpoch)
	}
	head := boardHead(t, board, tenantID)

	// A repeated pass is a no-op: no claim, no renewal yet.
	if _, err := mirror.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if head != boardHead(t, board, tenantID) {
		t.Fatal("a repeated sync renewed or re-claimed")
	}

	// Inside the renewal window the lease is extended.
	clock.Advance(700 * time.Second)
	report, err = mirror.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Renewed != 1 {
		t.Fatalf("renew report = %+v", report)
	}
	renewed := mustTask(t, board, tenantID, link.BoardTaskID)
	if renewed.Epoch != 1 || !renewed.ClaimUntil.After(clock.Now().Add(DefaultRenewBefore)) {
		t.Fatalf("renewed = %+v now=%s", renewed, clock.Now())
	}

	// Past the lease, the mirror expires the old claim and starts a new epoch.
	clock.Advance(DefaultLease)
	report, err = mirror.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Claims != 1 {
		t.Fatalf("after expiry report = %+v", report)
	}
	reclaimed := mustTask(t, board, tenantID, link.BoardTaskID)
	if reclaimed.Epoch != 2 || reclaimed.State != StateClaimed {
		t.Fatalf("reclaimed = %+v", reclaimed)
	}
}

func TestMirrorDoesNotOverrideAnotherActorsLiveClaim(t *testing.T) {
	mirror, store, board, links, clock, tenantID := mirrorFixture(t)
	ticket := seedTicket(t, store, tenantID, "Fix the build", "Body", "manual", "t-2", "evt-2")
	if _, err := mirror.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	link := mustLink(t, links, tenantID, ticket.ID.String(), keelstore.BoardLinkTicket)
	other := WorkerActor("someone-else")
	if _, _, err := board.Claim(context.Background(), tenantID, ClaimRequest{
		TaskID: link.BoardTaskID, Actor: other, Token: validToken(), Lease: DefaultLease,
	}); err != nil {
		t.Fatalf("other claim: %v", err)
	}
	before := mustTask(t, board, tenantID, link.BoardTaskID)
	head := boardHead(t, board, tenantID)

	addAssignment(t, store, ticket, collab.AssignmentActive, "researcher", uuid.New())
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemActive)
	report, err := mirror.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Conflicts == 0 {
		t.Fatalf("report = %+v, want a conflict", report)
	}
	after := mustTask(t, board, tenantID, link.BoardTaskID)
	if after.ClaimActor != other.ID || after.Epoch != before.Epoch {
		t.Fatalf("mirror overrode the claim: before=%+v after=%+v", before, after)
	}
	if head != boardHead(t, board, tenantID) {
		t.Fatal("a conflicting mirror pass still appended events")
	}
	_ = clock
}

func TestMirrorAttentionBlockUnblockAndReply(t *testing.T) {
	mirror, store, board, links, clock, tenantID := mirrorFixture(t)
	ticket := seedTicket(t, store, tenantID, "Fix the build", "Body", "manual", "t-3", "evt-3")
	assignment := addAssignment(t, store, ticket, collab.AssignmentActive, "researcher", uuid.New())
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemActive)
	if _, err := mirror.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	link := mustLink(t, links, tenantID, ticket.ID.String(), keelstore.BoardLinkTicket)

	attention := addAttention(t, store, ticket, assignment, collab.AttentionApproval, collab.AttentionOpen)
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemBlocked)
	report, err := mirror.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Blocked != 1 {
		t.Fatalf("block report = %+v", report)
	}
	blocked := mustTask(t, board, tenantID, link.BoardTaskID)
	if blocked.State != StateBlocked {
		t.Fatalf("state = %q", blocked.State)
	}
	view, _ := board.TaskView(context.Background(), tenantID, link.BoardTaskID)
	if view.Task.Blocked == nil || view.Task.Blocked.Kind == nil || *view.Task.Blocked.Kind != HoldAwaitingApproval {
		t.Fatalf("blocked = %#v", view.Task.Blocked)
	}
	if view.Task.Blocked.Reason != "Approve the write?" {
		t.Fatalf("reason = %q", view.Task.Blocked.Reason)
	}

	attention = answerAttention(t, store, attention, "yes")
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemActive)
	report, err = mirror.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Unblocked != 1 || report.Replies != 1 {
		t.Fatalf("unblock report = %+v", report)
	}
	unblocked := mustTask(t, board, tenantID, link.BoardTaskID)
	if unblocked.State != StateClaimed {
		t.Fatalf("state after unblock = %q, want claimed", unblocked.State)
	}
	if unblocked.Replies != 1 {
		t.Fatalf("replies = %d", unblocked.Replies)
	}
	head := boardHead(t, board, tenantID)
	if _, err := mirror.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if head != boardHead(t, board, tenantID) {
		t.Fatal("a repeated attention sync appended a duplicate reply")
	}

	// timed_out maps to the timeout reply and still unblocks.
	second := addAttention(t, store, ticket, assignment, collab.AttentionClarify, collab.AttentionOpen)
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemBlocked)
	if _, err := mirror.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mustTask(t, board, tenantID, link.BoardTaskID).State != StateBlocked {
		t.Fatal("second attention did not block")
	}
	second.Status = collab.AttentionTimedOut
	if _, err := store.UpdateAttention(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemActive)
	if _, err := mirror.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	final := mustTask(t, board, tenantID, link.BoardTaskID)
	if final.State != StateClaimed {
		t.Fatalf("state after timeout = %q", final.State)
	}
	if final.Replies != 2 {
		t.Fatalf("replies after timeout = %d, want 2", final.Replies)
	}
	_ = clock
}

func TestMirrorReportReplyAndTerminalState(t *testing.T) {
	mirror, store, board, links, _, tenantID := mirrorFixture(t)
	ticket := seedTicket(t, store, tenantID, "Fix the build", "Body", "manual", "t-4", "evt-4")
	assignment := addAssignment(t, store, ticket, collab.AssignmentActive, "researcher", uuid.New())
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemActive)
	first, err := mirror.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Claims != 1 || first.TasksCreated != 1 {
		t.Fatalf("first report = %+v", first)
	}
	link := mustLink(t, links, tenantID, ticket.ID.String(), keelstore.BoardLinkTicket)
	if mustTask(t, board, tenantID, link.BoardTaskID).State != StateClaimed {
		t.Fatal("the first pass did not mirror the claim")
	}

	report, err := mirror.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Claims != 0 {
		t.Fatalf("second pass re-claimed: %+v", report)
	}

	addAppliedReport(t, store, ticket, assignment, superagent.ReportReady, "all tests pass")
	report, err = mirror.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Replies != 1 {
		t.Fatalf("report = %+v", report)
	}
	task := mustTask(t, board, tenantID, link.BoardTaskID)
	if task.Replies != 1 {
		t.Fatalf("replies = %d", task.Replies)
	}
	head := boardHead(t, board, tenantID)
	if _, err := mirror.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if head != boardHead(t, board, tenantID) {
		t.Fatal("a repeated report appended a duplicate reply")
	}

	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemDone)
	report, err = mirror.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Completed != 1 {
		t.Fatalf("complete report = %+v", report)
	}
	completed := mustTask(t, board, tenantID, link.BoardTaskID)
	if completed.State != StateCompleted {
		t.Fatalf("state = %q", completed.State)
	}
	view, _ := board.TaskView(context.Background(), tenantID, link.BoardTaskID)
	if view.Task.Result == nil || *view.Task.Result != "all tests pass" {
		t.Fatalf("result = %#v", view.Task.Result)
	}
}

func TestMirrorImportsArchivedTicketAsReadOnlyHistory(t *testing.T) {
	cases := []struct {
		name   string
		status collab.WorkItemStatus
		state  string
		count  func(SyncReport) int
	}{
		{"done", collab.WorkItemDone, StateCompleted, func(report SyncReport) int { return report.Completed }},
		{"failed", collab.WorkItemFailed, StateFailed, func(report SyncReport) int { return report.Failed }},
		{"cancelled", collab.WorkItemCancelled, StateCanceled, func(report SyncReport) int { return report.Canceled }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			mirror, store, board, links, _, tenantID := mirrorFixture(t)
			ticket := seedTicket(t, store, tenantID, "Archived", "Body", "manual", "t-5", "evt-5")
			updateTicketStatus(t, store, tenantID, ticket.ID, test.status)
			before := ticketSnapshot(t, store, tenantID)
			report, err := mirror.SyncOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if report.Archived != 1 || report.TasksCreated != 1 || report.Skipped != 0 {
				t.Fatalf("report = %+v", report)
			}
			if got := test.count(report); got != 1 {
				t.Fatalf("final state count = %d: %+v", got, report)
			}
			if links.count() != 1 {
				t.Fatalf("links = %d, want 1", links.count())
			}
			link := mustLink(t, links, tenantID, ticket.ID.String(), keelstore.BoardLinkTicket)
			task := mustTask(t, board, tenantID, link.BoardTaskID)
			if task.State != test.state {
				t.Fatalf("state = %q, want %q", task.State, test.state)
			}
			view, err := board.TaskView(context.Background(), tenantID, link.BoardTaskID)
			if err != nil {
				t.Fatal(err)
			}
			if view.Labels[ArchivedLabel] != "true" {
				t.Fatalf("labels = %#v, want %s=true", view.Labels, ArchivedLabel)
			}
			if !view.NoSync {
				t.Fatal("an archived import is not marked noSync")
			}
			if after := ticketSnapshot(t, store, tenantID); after != before {
				t.Fatal("the mirror changed a collab record")
			}
			head := boardHead(t, board, tenantID)
			if _, err := mirror.SyncOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if head != boardHead(t, board, tenantID) {
				t.Fatal("an archived import was rewritten on the next pass")
			}
		})
	}
}

func TestMirrorSettlesTerminalTicketWithoutAssignment(t *testing.T) {
	mirror, store, board, links, _, tenantID := mirrorFixture(t)
	ticket := seedTicket(t, store, tenantID, "Handled outside", "Body", "manual", "t-7", "evt-7")
	if _, err := mirror.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	link := mustLink(t, links, tenantID, ticket.ID.String(), keelstore.BoardLinkTicket)
	if task := mustTask(t, board, tenantID, link.BoardTaskID); task.State != StateReady {
		t.Fatalf("initial state = %q", task.State)
	}
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemDone)
	report, err := mirror.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Completed != 1 || report.Archived != 0 {
		t.Fatalf("report = %+v", report)
	}
	task := mustTask(t, board, tenantID, link.BoardTaskID)
	if task.State != StateCompleted {
		t.Fatalf("state = %q, want completed", task.State)
	}
	view, err := board.TaskView(context.Background(), tenantID, link.BoardTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Labels[ArchivedLabel] == "true" {
		t.Fatal("a ticket mirrored while live was labelled archived")
	}
}

func TestMirrorGitHubEventsAreLinkedAndNeverSynced(t *testing.T) {
	mirror, store, board, links, _, tenantID := mirrorFixture(t)
	ticket := seedTicket(t, store, tenantID, "PR review", "Body", "github", "77", "issue-77#comment-1")
	if _, err := mirror.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	link := mustLink(t, links, tenantID, ticket.ID.String(), keelstore.BoardLinkTicket)
	eventLink, found, err := links.GetBoardLink(context.Background(), tenantID, "github", ticket.IdempotencyKey, keelstore.BoardLinkGitHub)
	if err != nil || !found {
		t.Fatalf("github event link = %+v found=%v err=%v", eventLink, found, err)
	}
	if eventLink.BoardTaskID != link.BoardTaskID {
		t.Fatalf("event link task = %d, want %d", eventLink.BoardTaskID, link.BoardTaskID)
	}
	view, err := board.TaskView(context.Background(), tenantID, link.BoardTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !view.NoSync {
		t.Fatal("a GitHub-sourced mirror task must be noSync so unified sync owns the GitHub exchange")
	}
	head := boardHead(t, board, tenantID)
	if _, err := mirror.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if head != boardHead(t, board, tenantID) {
		t.Fatal("a repeated GitHub ticket sync appended events")
	}
}

func TestMirrorOffIsNoopAndMissingSecretFailsClosed(t *testing.T) {
	tenantID := uuid.New()
	clock := newTestClock()
	board := newTestBoard(clock)
	links := newMemoryLinks()
	store := superagent.NewMemoryStore(clock.Now)
	seedTicket(t, store, tenantID, "Ticket", "Body", "manual", "t-7", "evt-7")

	off := newMirrorForTest(t, tenantID, store, board, links,
		SuperagentConfig{Channel: "superagent"}, clock)
	report, err := off.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("off sync: %v", err)
	}
	if report.TasksCreated != 0 || boardHead(t, board, tenantID) != 0 {
		t.Fatalf("a disabled mirror wrote: %+v head=%d", report, boardHead(t, board, tenantID))
	}

	noSecret, err := NewMirror(&Mirror{
		TenantID: tenantID, Store: store, Board: board, Links: links,
		Config: StaticConfigReader{Config: SuperagentConfig{Mirror: true, Channel: "superagent"}},
		Logger: discardLogger(), Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := noSecret.SyncOnce(context.Background()); !errors.Is(err, ErrSecretMissing) {
		t.Fatalf("missing secret sync = %v, want ErrSecretMissing", err)
	}
	if boardHead(t, board, tenantID) != 0 {
		t.Fatal("a missing secret still wrote to the board")
	}
}

func ticketSnapshot(t *testing.T, store *superagent.MemoryStore, tenantID uuid.UUID) string {
	t.Helper()
	items, err := store.ListWorkItems(context.Background(), tenantID, collab.WorkFilter{ModeID: superagent.DefaultModeID})
	if err != nil {
		t.Fatalf("list tickets: %v", err)
	}
	payload := make([]string, 0, len(items))
	for _, item := range items {
		payload = append(payload, item.ID.String()+":"+string(item.Status)+":"+item.UpdatedAt.String()+":"+string(item.Input))
	}
	return joinSorted(payload)
}

func joinSorted(values []string) string {
	for i := 0; i < len(values); i++ {
		for j := i + 1; j < len(values); j++ {
			if values[j] < values[i] {
				values[i], values[j] = values[j], values[i]
			}
		}
	}
	out := ""
	for _, value := range values {
		out += value + "\n"
	}
	return out
}

// TestSwitchMatrix is the 11.1 matrix: the mirror writes only when
// board.superagent.mirror is true; every other combination leaves the board
// unchanged.
func TestSwitchMatrix(t *testing.T) {
	cases := []struct {
		name   string
		mirror bool
		bridge bool
	}{
		{name: "off/off"},
		{name: "mirror only", mirror: true},
		{name: "bridge only", bridge: true},
		{name: "both", mirror: true, bridge: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			tenantID := uuid.New()
			clock := newTestClock()
			board := newTestBoard(clock)
			links := newMemoryLinks()
			store := superagent.NewMemoryStore(clock.Now)
			seedTicket(t, store, tenantID, "Switch", "Body", "manual", "switch", "evt-switch")
			mirror := newMirrorForTest(t, tenantID, store, board, links,
				SuperagentConfig{Mirror: testCase.mirror, Bridge: testCase.bridge, Channel: "superagent"}, clock)
			report, err := mirror.SyncOnce(context.Background())
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			wantTasks := 0
			if testCase.mirror {
				wantTasks = 1
			}
			if report.TasksCreated != wantTasks || links.count() != wantTasks {
				t.Fatalf("report=%+v links=%d, want %d", report, links.count(), wantTasks)
			}
			if wantTasks == 0 && boardHead(t, board, tenantID) != 0 {
				t.Fatal("a switched-off mirror wrote board events")
			}
		})
	}
}

func TestMirrorLogsContainNoSecretOrTicketBody(t *testing.T) {
	tenantID := uuid.New()
	clock := newTestClock()
	board := newTestBoard(clock)
	links := newMemoryLinks()
	store := superagent.NewMemoryStore(clock.Now)
	const bodyMarker = "BODY_SECRET_MARKER_7f3a"
	ticket := seedTicket(t, store, tenantID, "Title", bodyMarker, "manual", "log-1", "evt-log")
	if _, err := newMirrorForTest(t, tenantID, store, board, links,
		SuperagentConfig{Mirror: true, Channel: "superagent"}, clock).SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	link := mustLink(t, links, tenantID, ticket.ID.String(), storeTicketKind())
	if _, _, err := board.Claim(context.Background(), tenantID, ClaimRequest{
		TaskID: link.BoardTaskID, Actor: WorkerActor("someone-else"), Token: validToken(),
	}); err != nil {
		t.Fatal(err)
	}
	assignment := addAssignment(t, store, ticket, collab.AssignmentActive, "researcher", uuid.New())
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemActive)

	var buffer bytes.Buffer
	tokens, _ := NewTokenSource("log-secret")
	token, err := tokens.ClaimToken(tenantID, assignment.ID, assignment.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := NewMirror(&Mirror{
		TenantID: tenantID, Store: store, Board: board, Links: links,
		Config: StaticConfigReader{Config: SuperagentConfig{Mirror: true, Channel: "superagent"}},
		Secret: tokens, Logger: slog.New(slog.NewTextHandler(&buffer, nil)), Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mirror.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	logged := buffer.String()
	if logged == "" {
		t.Fatal("expected a claim-conflict warning")
	}
	for _, forbidden := range []string{bodyMarker, "log-secret", token} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("log leaked %q: %s", forbidden, logged)
		}
	}
}

type recordingACL struct {
	mu       sync.Mutex
	channels []string
	err      error
}

func (a *recordingACL) EnsureOperator(_ context.Context, _ uuid.UUID, channel string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.channels = append(a.channels, channel)
	return nil
}

func (a *recordingACL) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.channels)
}

func TestMirrorGrantsChannelOperatorOnceAndRetriesFailure(t *testing.T) {
	tenantID := uuid.New()
	clock := newTestClock()
	board := newTestBoard(clock)
	links := newMemoryLinks()
	store := superagent.NewMemoryStore(clock.Now)
	seedTicket(t, store, tenantID, "ACL", "Body", "manual", "acl-1", "evt-acl")
	acl := &recordingACL{err: errors.New("relations unavailable")}
	tokens, _ := NewTokenSource("test-secret")
	mirror, err := NewMirror(&Mirror{
		TenantID: tenantID, Store: store, Board: board, Links: links,
		Config: StaticConfigReader{Config: SuperagentConfig{Mirror: true, Channel: "superagent"}},
		Secret: tokens, ACL: acl, Logger: discardLogger(), Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mirror.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A failed relation write is retried on the next pass.
	acl.mu.Lock()
	acl.err = nil
	acl.mu.Unlock()
	if _, err := mirror.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := mirror.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if acl.count() != 1 {
		t.Fatalf("operator writes = %d, want 1", acl.count())
	}
}

func TestMirrorReopensAFailedTaskAfterSupervisorResume(t *testing.T) {
	mirror, store, board, links, _, tenantID := mirrorFixture(t)
	ctx := context.Background()
	ticket := seedTicket(t, store, tenantID, "Resume", "Body", "manual", "resume-1", "evt-resume")
	first := addAssignment(t, store, ticket, collab.AssignmentActive, "researcher", uuid.New())
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemActive)
	if _, err := mirror.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	link := mustLink(t, links, tenantID, ticket.ID.String(), storeTicketKind())

	first.Status = collab.AssignmentFailed
	if _, err := store.UpdateAssignment(ctx, first); err != nil {
		t.Fatal(err)
	}
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemFailed)
	if _, err := mirror.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if mustTask(t, board, tenantID, link.BoardTaskID).State != StateFailed {
		t.Fatal("board task did not fail")
	}

	// ResumeTicket: a new active assignment and an open ticket.
	addAssignment(t, store, ticket, collab.AssignmentActive, "researcher", uuid.New())
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemOpen)
	report, err := mirror.SyncOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Reopened != 1 || report.Claims != 1 {
		t.Fatalf("resume report = %+v", report)
	}
	resumed := mustTask(t, board, tenantID, link.BoardTaskID)
	if resumed.State != StateClaimed || resumed.Epoch != 2 {
		t.Fatalf("resumed = %+v", resumed)
	}
}

func TestMirrorTimedOutAttentionUnblocksEvenWhileTicketStaysBlocked(t *testing.T) {
	mirror, store, board, links, _, tenantID := mirrorFixture(t)
	ctx := context.Background()
	ticket := seedTicket(t, store, tenantID, "Timeout", "Body", "manual", "timeout-1", "evt-timeout")
	assignment := addAssignment(t, store, ticket, collab.AssignmentActive, "researcher", uuid.New())
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemActive)
	if _, err := mirror.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	link := mustLink(t, links, tenantID, ticket.ID.String(), keelstore.BoardLinkTicket)
	attention := addAttention(t, store, ticket, assignment, collab.AttentionClarify, collab.AttentionOpen)
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemBlocked)
	if _, err := mirror.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if mustTask(t, board, tenantID, link.BoardTaskID).State != StateBlocked {
		t.Fatal("attention did not block")
	}
	attention.Status = collab.AttentionTimedOut
	if _, err := store.UpdateAttention(ctx, attention); err != nil {
		t.Fatal(err)
	}
	// The supervisor leaves the Ticket blocked; the expired question is still
	// the board's unblock signal.
	report, err := mirror.SyncOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task := mustTask(t, board, tenantID, link.BoardTaskID)
	if task.State == StateBlocked {
		t.Fatalf("board stayed blocked after timeout: %+v", report)
	}
	if report.Unblocked != 1 || report.Replies != 1 {
		t.Fatalf("report = %+v", report)
	}
}

func TestMirrorCompletedTaskReopenIsExplicitAndAppendOnly(t *testing.T) {
	mirror, store, board, links, _, tenantID := mirrorFixture(t)
	ctx := context.Background()
	ticket := seedTicket(t, store, tenantID, "Done", "Body", "manual", "done-1", "evt-done")
	addAssignment(t, store, ticket, collab.AssignmentActive, "researcher", uuid.New())
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemActive)
	if _, err := mirror.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	link := mustLink(t, links, tenantID, ticket.ID.String(), keelstore.BoardLinkTicket)
	if mustTask(t, board, tenantID, link.BoardTaskID).State != StateClaimed {
		t.Fatal("setup did not claim")
	}
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemDone)
	if _, err := mirror.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if mustTask(t, board, tenantID, link.BoardTaskID).State != StateCompleted {
		t.Fatal("board did not complete")
	}
	// The supervisor reopens the Ticket; the board has no completed -> ready
	// edge, so the mirror must warn and leave history alone.
	addAssignment(t, store, ticket, collab.AssignmentActive, "researcher", uuid.New())
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemOpen)
	var buffer bytes.Buffer
	mirror.Logger = slog.New(slog.NewTextHandler(&buffer, nil))
	report, err := mirror.SyncOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Conflicts != 1 {
		t.Fatalf("report = %+v", report)
	}
	if mustTask(t, board, tenantID, link.BoardTaskID).State != StateCompleted {
		t.Fatal("a completed board task was rewritten")
	}
	if !strings.Contains(buffer.String(), "board history is append-only") {
		t.Fatalf("no explicit warning: %s", buffer.String())
	}
}

func TestMirrorCountsOnlyRealBlockAndRelease(t *testing.T) {
	mirror, store, _, links, _, tenantID := mirrorFixture(t)
	ctx := context.Background()
	ticket := seedTicket(t, store, tenantID, "Counts", "Body", "manual", "counts-1", "evt-counts")
	assignment := addAssignment(t, store, ticket, collab.AssignmentActive, "researcher", uuid.New())
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemActive)
	if _, err := mirror.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	_ = mustLink(t, links, tenantID, ticket.ID.String(), keelstore.BoardLinkTicket)
	attention := addAttention(t, store, ticket, assignment, collab.AttentionClarify, collab.AttentionOpen)
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemBlocked)
	first, err := mirror.SyncOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Blocked != 1 {
		t.Fatalf("first block = %+v", first)
	}
	second, err := mirror.SyncOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Blocked != 0 || second.Replies != 0 {
		t.Fatalf("an unchanged blocked task was counted again: %+v", second)
	}
	// Answer and drop the active assignment: the board unblocks, then releases
	// the mirrored claim exactly once.
	answerAttention(t, store, attention, "yes")
	assignments, _ := store.ListAssignments(ctx, tenantID, ticket.ID)
	for _, assignment := range assignments {
		assignment.Status = collab.AssignmentDone
		if _, err := store.UpdateAssignment(ctx, assignment); err != nil {
			t.Fatal(err)
		}
	}
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemOpen)
	third, err := mirror.SyncOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if third.Unblocked != 1 || third.Released != 0 || third.Replies != 1 {
		t.Fatalf("unblock/release = %+v", third)
	}
	fourth, err := mirror.SyncOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fourth.Unblocked != 0 || fourth.Released != 0 || fourth.Replies != 0 {
		t.Fatalf("a settled task was counted again: %+v", fourth)
	}
}

func TestMirrorCountsReleaseOnce(t *testing.T) {
	mirror, store, board, links, _, tenantID := mirrorFixture(t)
	ctx := context.Background()
	ticket := seedTicket(t, store, tenantID, "Release", "Body", "manual", "release-1", "evt-release")
	assignment := addAssignment(t, store, ticket, collab.AssignmentActive, "researcher", uuid.New())
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemActive)
	if _, err := mirror.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	link := mustLink(t, links, tenantID, ticket.ID.String(), keelstore.BoardLinkTicket)
	assignment.Status = collab.AssignmentDone
	if _, err := store.UpdateAssignment(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	first, err := mirror.SyncOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Released != 1 {
		t.Fatalf("release = %+v", first)
	}
	if mustTask(t, board, tenantID, link.BoardTaskID).State != StateReady {
		t.Fatal("board did not return to ready")
	}
	second, err := mirror.SyncOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Released != 0 {
		t.Fatalf("release counted again: %+v", second)
	}
}

// failingMirrorStore injects a read failure into one list call. The rest of
// the supervisor store behaves normally.
type failingMirrorStore struct {
	*superagent.MemoryStore
	failAssignments bool
	failAttentions  bool
}

func (s *failingMirrorStore) ListAssignments(ctx context.Context, tenantID, workItemID uuid.UUID) ([]collab.Assignment, error) {
	if s.failAssignments {
		return nil, errFake
	}
	return s.MemoryStore.ListAssignments(ctx, tenantID, workItemID)
}

func (s *failingMirrorStore) ListAttentions(ctx context.Context, tenantID, workItemID uuid.UUID) ([]collab.Attention, error) {
	if s.failAttentions {
		return nil, errFake
	}
	return s.MemoryStore.ListAttentions(ctx, tenantID, workItemID)
}

// TestMirrorReadFailureSkipsTheTicketInsteadOfReleasing is the P2 hardening
// regression: a failed assignment read is not "no assignments". The old code
// continued with an empty set and released the mirrored claim.
func TestMirrorReadFailureSkipsTheTicketInsteadOfReleasing(t *testing.T) {
	tenantID := uuid.New()
	clock := newTestClock()
	board := newTestBoard(clock)
	links := newMemoryLinks()
	memory := superagent.NewMemoryStore(clock.Now)
	store := &failingMirrorStore{MemoryStore: memory}
	mirror := newMirrorForTest(t, tenantID, store, board, links,
		SuperagentConfig{Mirror: true, Bridge: true, Channel: ChannelForTest()}, clock)
	ticket := seedTicket(t, memory, tenantID, "Fix the build", "Body", "manual", "t-read", "evt-read")
	assignment := addAssignment(t, memory, ticket, collab.AssignmentActive, "researcher", uuid.New())
	updateTicketStatus(t, memory, tenantID, ticket.ID, collab.WorkItemActive)
	if _, err := mirror.SyncOnce(context.Background()); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	link := mustLink(t, links, tenantID, ticket.ID.String(), keelstore.BoardLinkTicket)
	if state := mustTask(t, board, tenantID, link.BoardTaskID).State; state != StateClaimed {
		t.Fatalf("seed state = %q", state)
	}

	store.failAssignments = true
	report, err := mirror.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("sync with a failed read: %v", err)
	}
	if report.Skipped == 0 {
		t.Fatalf("report = %+v, want the ticket skipped", report)
	}
	if state := mustTask(t, board, tenantID, link.BoardTaskID).State; state != StateClaimed {
		t.Fatalf("a failed assignment read released the claim; state = %q", state)
	}
	_ = assignment
}

// TestMirrorAttentionReadFailureDoesNotUnblock mirrors the same rule for a
// hold: without the attention list the mirror cannot know the hold is over.
func TestMirrorAttentionReadFailureDoesNotUnblock(t *testing.T) {
	tenantID := uuid.New()
	clock := newTestClock()
	board := newTestBoard(clock)
	links := newMemoryLinks()
	memory := superagent.NewMemoryStore(clock.Now)
	store := &failingMirrorStore{MemoryStore: memory}
	mirror := newMirrorForTest(t, tenantID, store, board, links,
		SuperagentConfig{Mirror: true, Bridge: true, Channel: ChannelForTest()}, clock)
	ticket := seedTicket(t, memory, tenantID, "Fix the build", "Body", "manual", "t-attn", "evt-attn")
	assignment := addAssignment(t, memory, ticket, collab.AssignmentActive, "researcher", uuid.New())
	attention := addAttention(t, memory, ticket, assignment, collab.AttentionClarify, collab.AttentionOpen)
	updateTicketStatus(t, memory, tenantID, ticket.ID, collab.WorkItemActive)
	if _, err := mirror.SyncOnce(context.Background()); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	link := mustLink(t, links, tenantID, ticket.ID.String(), keelstore.BoardLinkTicket)
	if state := mustTask(t, board, tenantID, link.BoardTaskID).State; state != StateBlocked {
		t.Fatalf("seed state = %q, want blocked", state)
	}

	store.failAttentions = true
	report, err := mirror.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("sync with a failed read: %v", err)
	}
	if report.Skipped == 0 {
		t.Fatalf("report = %+v, want the ticket skipped", report)
	}
	if state := mustTask(t, board, tenantID, link.BoardTaskID).State; state != StateBlocked {
		t.Fatalf("a failed attention read unblocked the task; state = %q", state)
	}
	_ = attention
}

// TestMirrorBoardGateStopsEveryWrite covers P1-1 for the background writer: a
// closed board.enabled switch appends no board event and syncs no ticket; the
// same mirror resumes once the switch is back.
func TestMirrorBoardGateStopsEveryWrite(t *testing.T) {
	mirror, store, board, links, clock, tenantID := mirrorFixture(t)
	enabled := false
	mirror.Enabled = func(context.Context) bool { return enabled }
	ticket := seedTicket(t, store, tenantID, "Gate the mirror", "Body", "manual", "t-gate", "evt-gate")

	if report, err := mirror.SyncOnce(context.Background()); err != nil || report.TasksCreated != 0 {
		t.Fatalf("disabled SyncOnce = %+v/%v", report, err)
	}
	if report, err := mirror.SyncTicket(context.Background(), ticket.ID); err != nil || report.TasksCreated != 0 {
		t.Fatalf("disabled SyncTicket = %+v/%v", report, err)
	}
	if head := boardHead(t, board, tenantID); head != 0 {
		t.Fatalf("a disabled mirror wrote head %d", head)
	}
	if links.count() != 0 {
		t.Fatalf("a disabled mirror wrote %d links", links.count())
	}

	enabled = true
	if _, err := mirror.SyncOnce(context.Background()); err != nil {
		t.Fatalf("enabled sync: %v", err)
	}
	link := mustLink(t, links, tenantID, ticket.ID.String(), keelstore.BoardLinkTicket)
	head := boardHead(t, board, tenantID)
	if head == 0 {
		t.Fatal("the re-enabled mirror wrote nothing")
	}

	enabled = false
	next := seedTicket(t, store, tenantID, "Must not import", "Body", "manual", "t-gate-2", "evt-gate-2")
	clock.Advance(10 * time.Second)
	if _, err := mirror.SyncOnce(context.Background()); err != nil {
		t.Fatalf("second disabled sync: %v", err)
	}
	if head != boardHead(t, board, tenantID) {
		t.Fatal("the mirror wrote while disabled")
	}
	if _, found, _ := links.GetBoardLink(context.Background(), tenantID, BoardSource, next.ID.String(), keelstore.BoardLinkTicket); found {
		t.Fatal("a ticket imported while the board was disabled")
	}
	if state := mustTask(t, board, tenantID, link.BoardTaskID).State; state == "" {
		t.Fatal("the existing task vanished")
	}
}
