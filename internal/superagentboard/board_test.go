package superagentboard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	boardlog "github.com/jixoai/keel-board/log"
	"github.com/jixoai/keel-board/protocol"
)

func newLogBoard(t *testing.T) (*LogBoard, *testClock, uuid.UUID) {
	t.Helper()
	clock := newTestClock()
	return newTestBoard(clock), clock, uuid.New()
}

func createTask(t *testing.T, board *LogBoard, tenantID uuid.UUID) TaskInfo {
	t.Helper()
	task, err := board.EnsureTicketThread(context.Background(), tenantID, TicketThread{
		TicketID: uuid.NewString(), Channel: "superagent", Title: "Fix the build",
		Request: "The CI job fails.", Labels: map[string]string{"source": BoardSource},
		Actor: ServiceActor(tenantID),
	})
	if err != nil {
		t.Fatalf("ensure ticket thread: %v", err)
	}
	return task
}

func validToken() string { return strings.Repeat("a", 43) }

func TestEnsureTicketThreadIsIdempotentAndNoSync(t *testing.T) {
	board, _, tenantID := newLogBoard(t)
	ticketID := uuid.NewString()
	request := TicketThread{
		TicketID: ticketID, Channel: "superagent", Title: "Fix the build",
		Request: "The CI job fails.", Labels: map[string]string{"source": BoardSource, "ticket": ticketID},
		Actor: ServiceActor(tenantID),
	}
	first, err := board.EnsureTicketThread(context.Background(), tenantID, request)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	headAfterFirst := boardHead(t, board, tenantID)
	second, err := board.EnsureTicketThread(context.Background(), tenantID, request)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("replayed task = %d, want %d", second.ID, first.ID)
	}
	if head := boardHead(t, board, tenantID); head != headAfterFirst {
		t.Fatalf("repeat append moved head from %d to %d", headAfterFirst, head)
	}
	if second.State != StateReady {
		t.Fatalf("state = %q, want ready", second.State)
	}
	if second.Labels["source"] != BoardSource || second.Labels["ticket"] != ticketID {
		t.Fatalf("labels = %v", second.Labels)
	}
	if second.Labels[protocol.TaskLabel] != "ready" {
		t.Fatalf("board.task = %q", second.Labels[protocol.TaskLabel])
	}
	if !strings.Contains(second.Text, "Fix the build") || !strings.Contains(second.Text, "CI job fails") {
		t.Fatalf("text = %q", second.Text)
	}
	view, err := board.TaskView(context.Background(), tenantID, first.ID)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if !view.NoSync {
		t.Fatal("mirrored root must be noSync")
	}
	if view.Task == nil || view.Task.State != StateReady {
		t.Fatalf("view task = %#v", view.Task)
	}
}

func TestBoardTextTruncatesAndSanitizes(t *testing.T) {
	text := BoardText("title\x00", strings.Repeat("x", 20_000))
	if strings.ContainsRune(text, 0) {
		t.Fatal("NUL survived")
	}
	if protocol.ValidateMessageText(text, false) != nil {
		t.Fatalf("text is not a valid message: %q", text)
	}
	if BoardText("", "") == "" {
		t.Fatal("empty ticket must still say something")
	}
}

func TestClaimRenewAndConflict(t *testing.T) {
	board, clock, tenantID := newLogBoard(t)
	task := createTask(t, board, tenantID)
	actor := WorkerActor("researcher")
	ctx := context.Background()

	claimed, made, err := board.Claim(ctx, tenantID, ClaimRequest{
		TaskID: task.ID, Actor: actor, Token: validToken(), Lease: DefaultLease,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !made || claimed.State != StateClaimed || claimed.Epoch != 1 || claimed.ClaimActor != actor.ID {
		t.Fatalf("claim = %+v made=%v", claimed, made)
	}
	again, madeAgain, err := board.Claim(ctx, tenantID, ClaimRequest{
		TaskID: task.ID, Actor: actor, Token: validToken(), Lease: DefaultLease,
	})
	if err != nil || madeAgain {
		t.Fatalf("repeat claim = %v made=%v", err, madeAgain)
	}
	if again.Epoch != 1 {
		t.Fatalf("repeat claim epoch = %d", again.Epoch)
	}
	other := WorkerActor("other")
	if _, _, err := board.Claim(ctx, tenantID, ClaimRequest{
		TaskID: task.ID, Actor: other, Token: strings.Repeat("b", 43), Lease: DefaultLease,
	}); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("other claim = %v, want ErrClaimConflict", err)
	}

	renewed, err := board.Renew(ctx, tenantID, RenewRequest{
		TaskID: task.ID, Actor: actor, Epoch: 1, Lease: DefaultLease, RenewBefore: DefaultRenewBefore,
	})
	if err != nil || renewed {
		t.Fatalf("early renew = %v/%v, want no-op", renewed, err)
	}
	clock.Advance(700 * time.Second)
	renewed, err = board.Renew(ctx, tenantID, RenewRequest{
		TaskID: task.ID, Actor: actor, Epoch: 1, Lease: DefaultLease, RenewBefore: DefaultRenewBefore,
	})
	if err != nil || !renewed {
		t.Fatalf("late renew = %v/%v, want renewed", renewed, err)
	}
	if _, err := board.Renew(ctx, tenantID, RenewRequest{
		TaskID: task.ID, Actor: other, Epoch: 1, Lease: DefaultLease,
	}); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("foreign renew = %v, want ErrClaimConflict", err)
	}

	// Let the lease end. The next claim expires the old one and starts epoch 2.
	clock.Advance(DefaultLease)
	reclaimed, made, err := board.Claim(ctx, tenantID, ClaimRequest{
		TaskID: task.ID, Actor: actor, Token: strings.Repeat("c", 43), Lease: DefaultLease,
	})
	if err != nil || !made {
		t.Fatalf("reclaim = %v made=%v", err, made)
	}
	if reclaimed.Epoch != 2 {
		t.Fatalf("reclaim epoch = %d, want 2", reclaimed.Epoch)
	}
}

func TestClaimRejectsWaitingTask(t *testing.T) {
	board, _, tenantID := newLogBoard(t)
	// A blocked task is not claimable.
	task := createTask(t, board, tenantID)
	if err := board.Block(context.Background(), tenantID, BlockRequest{
		TaskID: task.ID, Kind: HoldNeedsInput, Reason: "need input", Actor: ServiceActor(tenantID),
	}); err != nil {
		t.Fatalf("block: %v", err)
	}
	if _, _, err := board.Claim(context.Background(), tenantID, ClaimRequest{
		TaskID: task.ID, Actor: WorkerActor("researcher"), Token: validToken(),
	}); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("claim blocked = %v, want ErrNotClaimable", err)
	}
}

func TestSettleBlockAssignCancel(t *testing.T) {
	board, _, tenantID := newLogBoard(t)
	ctx := context.Background()
	actor := WorkerActor("researcher")

	t.Run("complete", func(t *testing.T) {
		task := createTask(t, board, tenantID)
		if _, _, err := board.Claim(ctx, tenantID, ClaimRequest{TaskID: task.ID, Actor: actor, Token: validToken()}); err != nil {
			t.Fatal(err)
		}
		info := mustTask(t, board, tenantID, task.ID)
		if err := board.Complete(ctx, tenantID, SettleRequest{TaskID: task.ID, Epoch: info.Epoch, Result: "fixed", Actor: actor}); err != nil {
			t.Fatal(err)
		}
		done := mustTask(t, board, tenantID, task.ID)
		if done.State != StateCompleted {
			t.Fatalf("state = %q", done.State)
		}
		view, _ := board.TaskView(ctx, tenantID, task.ID)
		if view.Task.Result == nil || *view.Task.Result != "fixed" {
			t.Fatalf("result = %#v", view.Task.Result)
		}
	})

	t.Run("fail", func(t *testing.T) {
		task := createTask(t, board, tenantID)
		if _, _, err := board.Claim(ctx, tenantID, ClaimRequest{TaskID: task.ID, Actor: actor, Token: validToken()}); err != nil {
			t.Fatal(err)
		}
		info := mustTask(t, board, tenantID, task.ID)
		if err := board.Fail(ctx, tenantID, SettleRequest{TaskID: task.ID, Epoch: info.Epoch, Reason: "broken", Actor: actor}); err != nil {
			t.Fatal(err)
		}
		failed := mustTask(t, board, tenantID, task.ID)
		if failed.State != StateFailed {
			t.Fatalf("state = %q", failed.State)
		}
	})

	t.Run("block and unblock", func(t *testing.T) {
		task := createTask(t, board, tenantID)
		if _, _, err := board.Claim(ctx, tenantID, ClaimRequest{TaskID: task.ID, Actor: actor, Token: validToken()}); err != nil {
			t.Fatal(err)
		}
		if err := board.Block(ctx, tenantID, BlockRequest{
			TaskID: task.ID, Kind: HoldAwaitingApproval, Reason: "Approve?", Actor: actor,
		}); err != nil {
			t.Fatal(err)
		}
		blocked := mustTask(t, board, tenantID, task.ID)
		if blocked.State != StateBlocked {
			t.Fatalf("state = %q, want blocked", blocked.State)
		}
		if err := board.Unblock(ctx, tenantID, UnblockRequest{TaskID: task.ID, Actor: ServiceActor(tenantID)}); err != nil {
			t.Fatal(err)
		}
		ready := mustTask(t, board, tenantID, task.ID)
		if ready.State != StateReady {
			t.Fatalf("state = %q, want ready", ready.State)
		}
	})

	t.Run("assign", func(t *testing.T) {
		task := createTask(t, board, tenantID)
		if err := board.Assign(ctx, tenantID, AssignRequest{
			TaskID: task.ID, For: task.Owner + "+researcher", Actor: ServiceActor(tenantID),
		}); err != nil {
			t.Fatal(err)
		}
		assigned := mustTask(t, board, tenantID, task.ID)
		if assigned.For != task.Owner+"+researcher" {
			t.Fatalf("for = %q", assigned.For)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		task := createTask(t, board, tenantID)
		if err := board.Cancel(ctx, tenantID, SettleRequest{TaskID: task.ID, Reason: "stopped", Actor: ServiceActor(tenantID)}); err != nil {
			t.Fatal(err)
		}
		canceled := mustTask(t, board, tenantID, task.ID)
		if canceled.State != StateCanceled {
			t.Fatalf("state = %q", canceled.State)
		}
		// Terminal states are append-only.
		if err := board.Complete(ctx, tenantID, SettleRequest{TaskID: task.ID, Actor: actor}); err != nil {
			t.Fatalf("complete canceled task should be a no-op: %v", err)
		}
	})
}

func TestReplyIdempotentAndNoSync(t *testing.T) {
	board, _, tenantID := newLogBoard(t)
	ctx := context.Background()
	task := createTask(t, board, tenantID)
	actor := ServiceActor(tenantID)
	first, err := board.Reply(ctx, tenantID, ReplyRequest{
		TaskID: task.ID, Text: "ready: done", Labels: ReplyLabels(uuid.New()),
		Actor: actor, RequestKey: "superagent:report:test", Digest: "digest",
	})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	head := boardHead(t, board, tenantID)
	second, err := board.Reply(ctx, tenantID, ReplyRequest{
		TaskID: task.ID, Text: "ready: done", Labels: ReplyLabels(uuid.New()),
		Actor: actor, RequestKey: "superagent:report:test", Digest: "digest",
	})
	if err != nil {
		t.Fatalf("repeat reply: %v", err)
	}
	if first != second {
		t.Fatalf("reply id = %d vs %d", first, second)
	}
	if head != boardHead(t, board, tenantID) {
		t.Fatal("repeat reply appended an event")
	}
	root, channel, found, err := board.MessageRoot(ctx, tenantID, first)
	if err != nil || !found {
		t.Fatalf("message root = %d/%q/%v/%v", root, channel, found, err)
	}
	if root != task.ID || channel != task.Channel {
		t.Fatalf("root = %d channel = %q", root, channel)
	}
	view, err := board.TaskView(ctx, tenantID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Re != nil {
		t.Fatalf("a task root has no re ref, got %q", *view.Re)
	}
	info := mustTask(t, board, tenantID, task.ID)
	if info.Replies != 1 {
		t.Fatalf("task replies = %d, want 1", info.Replies)
	}
	replyView, err := board.TaskView(ctx, tenantID, first)
	if err == nil {
		t.Fatalf("a reply is not a task, got view %#v", replyView)
	}
	// The reply itself must be NoSync so unified board sync cannot echo it.
	var raw protocol.MessageView
	{
		var readErr error
		raw, readErr = board.replyView(ctx, tenantID, first)
		if readErr != nil {
			t.Fatalf("reply view: %v", readErr)
		}
	}
	if !raw.NoSync {
		t.Fatal("reply must be noSync")
	}
}

// replyView is a test-only read of a non-task message.
func (b *LogBoard) replyView(ctx context.Context, tenantID uuid.UUID, messageID uint64) (protocol.MessageView, error) {
	var view protocol.MessageView
	err := b.Board.ReadContext(ctx, tenantID.String(), func(snapshot *boardlog.Snapshot) error {
		message := snapshot.Message(messageID)
		if message == nil {
			return ErrTaskNotFound
		}
		view = messageViewOf(snapshot, message)
		return nil
	})
	return view, err
}

func TestTaskNotFound(t *testing.T) {
	board, _, tenantID := newLogBoard(t)
	if _, err := board.Task(context.Background(), tenantID, 999); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("Task = %v, want ErrTaskNotFound", err)
	}
	if _, err := board.TaskView(context.Background(), tenantID, 999); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("TaskView = %v, want ErrTaskNotFound", err)
	}
}
