package superagentboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"

	boarddomain "github.com/jixoai/keel-board/board"
	boardlog "github.com/jixoai/keel-board/log"
	"github.com/jixoai/keel-board/protocol"
)

// Board task states, as the board log's fold spells them.
const (
	StateWaiting   = "waiting"
	StateReady     = "ready"
	StateClaimed   = "claimed"
	StateBlocked   = "blocked"
	StateCompleted = "completed"
	StateFailed    = "failed"
	StateCanceled  = "canceled"
)

// Hold kinds used for a mirrored Attention. `needs_input` is a clarify
// question; `awaiting_approval` is an approval or a decision.
const (
	HoldNeedsInput       = "needs_input"
	HoldAwaitingApproval = "awaiting_approval"
)

// ArchivedLabel marks a board task imported from an already terminal Ticket.
// The mirror writes it through the board log (never through a client label
// change), so the board's reserved board.* namespace still belongs to the
// board. The mount layer maps it to TaskView.archived.
const ArchivedLabel = "board.archived"

// Mirror lease defaults. Design §15.2: claim lease 900s; renew from the
// assignment heartbeat. The adapter renews only when the remaining lease is
// below renewBefore, so an idle sync does not append a renewal every tick.
const (
	DefaultLease        = 900 * time.Second
	DefaultRenewBefore  = 300 * time.Second
	DefaultCommandLimit = 5 * time.Second
)

// Mirror errors. They are stable so the bridge can map them to a status
// without inspecting text.
var (
	ErrTaskNotFound       = errors.New("superagentboard: the mirrored board task does not exist")
	ErrClaimConflict      = errors.New("superagentboard: the board task is claimed by another actor")
	ErrNotClaimable       = errors.New("superagentboard: the board task cannot be claimed in its current state")
	ErrTaskClosed         = errors.New("superagentboard: the board task is finished")
	ErrNoActiveAssignment = errors.New("superagentboard: the ticket has no active assignment")
	ErrNoOpenAttention    = errors.New("superagentboard: the ticket has no open attention")
)

// ActorInfo is the board identity a mirrored write carries. It is display
// provenance; authorization is the board authorizer's or the supervisor's.
type ActorInfo struct {
	ID        string
	Principal string
	Profile   string
	Kind      string
	Handle    string
	Name      string
}

// Author projects the actor as a board write's provenance.
func (a ActorInfo) Author() boardlog.Author {
	author := boardlog.Author{Actor: a.ID, Principal: a.Principal}
	if strings.TrimSpace(a.Profile) != "" {
		profile := a.Profile
		author.Profile = &profile
	}
	return author
}

// Seen projects the actor into an actor.seen event.
func (a ActorInfo) Seen() *boardlog.ActorSeenPayload {
	kind := strings.TrimSpace(a.Kind)
	if kind == "" {
		kind = string(protocol.ActorWorkflow)
	}
	handle := strings.TrimSpace(a.Handle)
	if handle == "" {
		handle = a.ID
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		name = handle
	}
	return &boardlog.ActorSeenPayload{
		Actor: a.ID, Kind: kind, Handle: handle, Name: name,
	}
}

// ServiceActor is the stable board principal of the mirror itself. A ticket's
// requester may be a platform login with no keel Account; the mirror is the
// board-side author of the root, and the original requester is recorded in the
// thread text and labels.
func ServiceActor(tenantID uuid.UUID) ActorInfo {
	id := "superagent:" + tenantID.String()
	return ActorInfo{
		ID: id, Principal: id, Kind: string(protocol.ActorWorkflow),
		Handle: "superagent", Name: "Super Agent",
	}
}

// WorkerActor is the board identity of a supervisor worker Bot. It is stable
// per Bot and independent of a display name or a session.
func WorkerActor(botID string) ActorInfo {
	botID = strings.TrimSpace(botID)
	if botID == "" {
		return ActorInfo{}
	}
	id := "bot:" + botID
	return ActorInfo{
		ID: id, Principal: id, Kind: string(protocol.ActorAgent),
		Handle: botID, Name: botID,
	}
}

// TaskInfo is the adapter's read of one board task.
type TaskInfo struct {
	ID      uint64
	Channel string
	Owner   string
	State   string
	Epoch   uint64
	// Version is the root message's version: the seq of the last event that
	// changed the thread root. It advances on claim/block/unblock/assign, so it
	// distinguishes a new block intent from a retry that changed nothing.
	Version       uint64
	ClaimActor    string
	ClaimUntil    time.Time
	BlockedKind   string
	BlockedReason string
	For           string
	Labels        map[string]string
	Text          string
	Replies       int
}

// TicketThread is the root write of one Ticket.
type TicketThread struct {
	TicketID string
	Channel  string
	Title    string
	Request  string
	Labels   map[string]string
	Actor    ActorInfo
}

// ClaimRequest mirrors one active Assignment as a board claim.
type ClaimRequest struct {
	TaskID uint64
	Actor  ActorInfo
	Token  string
	Lease  time.Duration
}

// RenewRequest extends a mirrored claim.
type RenewRequest struct {
	TaskID      uint64
	Actor       ActorInfo
	Epoch       uint64
	Lease       time.Duration
	RenewBefore time.Duration
}

// ReleaseRequest hands a mirrored claim back. It is used when the supervisor's
// assignment ended but the Ticket stays open for a new assignment.
type ReleaseRequest struct {
	TaskID uint64
	Actor  ActorInfo
	Epoch  uint64
}

// ReopenRequest revives a failed mirrored task after the supervisor reopened
// its Ticket. The board domain only reopens failed tasks; a completed task
// stays completed.
type ReopenRequest struct {
	TaskID uint64
	Actor  ActorInfo
}

// BlockRequest holds a mirrored task for a person.
type BlockRequest struct {
	TaskID uint64
	Kind   string
	Reason string
	Actor  ActorInfo
}

// UnblockRequest lifts a hold.
type UnblockRequest struct {
	TaskID uint64
	Actor  ActorInfo
}

// SettleRequest completes, fails or cancels a mirrored task.
type SettleRequest struct {
	TaskID uint64
	Epoch  uint64
	Result string
	Reason string
	Actor  ActorInfo
}

// AssignRequest keeps a mirrored task for one profile.
type AssignRequest struct {
	TaskID uint64
	For    string
	Actor  ActorInfo
}

// ReplyRequest appends one thread reply. RequestKey/Digest make it idempotent.
type ReplyRequest struct {
	TaskID     uint64
	Text       string
	Labels     map[string]string
	Actor      ActorInfo
	RequestKey string
	Digest     string
}

// BoardPort is the board write/read seam the adapter needs. Its production
// implementation is LogBoard, which appends through log.Board; a fake is a
// plain map. Keeping this interface small is what lets the bridge and mirror
// be tested without PostgreSQL.
type BoardPort interface {
	EnsureTicketThread(ctx context.Context, tenantID uuid.UUID, request TicketThread) (TaskInfo, error)
	Claim(ctx context.Context, tenantID uuid.UUID, request ClaimRequest) (TaskInfo, bool, error)
	Renew(ctx context.Context, tenantID uuid.UUID, request RenewRequest) (bool, error)
	Release(ctx context.Context, tenantID uuid.UUID, request ReleaseRequest) (bool, error)
	Reopen(ctx context.Context, tenantID uuid.UUID, request ReopenRequest) (bool, error)
	Complete(ctx context.Context, tenantID uuid.UUID, request SettleRequest) error
	Fail(ctx context.Context, tenantID uuid.UUID, request SettleRequest) error
	Cancel(ctx context.Context, tenantID uuid.UUID, request SettleRequest) error
	Block(ctx context.Context, tenantID uuid.UUID, request BlockRequest) error
	Unblock(ctx context.Context, tenantID uuid.UUID, request UnblockRequest) error
	Assign(ctx context.Context, tenantID uuid.UUID, request AssignRequest) error
	Reply(ctx context.Context, tenantID uuid.UUID, request ReplyRequest) (uint64, error)
	Task(ctx context.Context, tenantID uuid.UUID, taskID uint64) (TaskInfo, error)
	MessageRoot(ctx context.Context, tenantID uuid.UUID, messageID uint64) (uint64, string, bool, error)
	TaskView(ctx context.Context, tenantID uuid.UUID, taskID uint64) (protocol.MessageView, error)
}

// LogBoard implements BoardPort over the board's own log API. Every mutation is
// one conditional append; nothing here writes board_events with SQL, so the
// board's projection, SSE and idempotency rules apply unchanged.
type LogBoard struct {
	Board *boardlog.Board
	Now   func() time.Time
}

var _ BoardPort = (*LogBoard)(nil)

func (b *LogBoard) now() time.Time {
	if b != nil && b.Now != nil {
		return b.Now().UTC()
	}
	return time.Now().UTC()
}

func (b *LogBoard) tenantName(tenantID uuid.UUID) (string, error) {
	if tenantID == uuid.Nil {
		return "", errors.New("superagentboard: a tenant is required for a board write")
	}
	return tenantID.String(), nil
}

// EnsureTicketThread creates the root task thread exactly once. The idempotency
// key is the Ticket id, so a repeated sync or a restart answers the first task
// instead of appending a second root.
func (b *LogBoard) EnsureTicketThread(ctx context.Context, tenantID uuid.UUID, request TicketThread) (TaskInfo, error) {
	if b == nil || b.Board == nil {
		return TaskInfo{}, errors.New("superagentboard: the board log is not wired")
	}
	tenant, err := b.tenantName(tenantID)
	if err != nil {
		return TaskInfo{}, err
	}
	if strings.TrimSpace(request.TicketID) == "" {
		return TaskInfo{}, errors.New("superagentboard: a ticket id is required")
	}
	if err := protocol.ValidateChannel(request.Channel); err != nil {
		return TaskInfo{}, fmt.Errorf("superagentboard: channel: %w", err)
	}
	text := BoardText(request.Title, request.Request)
	labels := cleanLabels(request.Labels)
	actor := request.Actor
	if !validActorID(actor.ID) {
		actor = ServiceActor(tenantID)
	}
	if actor.Principal == "" {
		actor.Principal = actor.ID
	}
	digest := digestOf(struct {
		Channel string
		Text    string
		Labels  map[string]string
		Actor   string
	}{request.Channel, text, labels, actor.ID})

	var taskID uint64
	err = b.Board.Update(ctx, tenant, boardlog.UpdateOptions{Idempotency: &boardlog.Idempotency{
		Key: "superagent:ticket:" + request.TicketID, Digest: digest,
	}}, func(tx *boardlog.Tx) error {
		if replay, ok := tx.Replay(); ok {
			if len(replay.MessageIDs) == 0 {
				return fmt.Errorf("superagentboard: ticket %s replayed without a message id", request.TicketID)
			}
			taskID = replay.MessageIDs[0]
			return nil
		}
		taskID = tx.NextID(0)
		tx.Emit(boardlog.EventMessagePosted, &boardlog.MessagePostedPayload{
			Channel: request.Channel, Text: text, Labels: labels,
			By: actor.Author(), NoSync: true,
		})
		tx.Emit(boardlog.EventTaskCreated, &boardlog.TaskCreatedPayload{
			ID: taskID, Thread: boardlog.ThreadKey(taskID), By: actor.Author(),
		})
		tx.Emit(boardlog.EventActorSeen, actor.Seen())
		tx.RecordResult(taskID)
		return nil
	})
	if err != nil {
		return TaskInfo{}, err
	}
	return b.Task(ctx, tenantID, taskID)
}

// Claim mirrors one active Assignment. It never overrides a live claim by
// another actor: that is a claim conflict the caller logs and leaves alone.
func (b *LogBoard) Claim(ctx context.Context, tenantID uuid.UUID, request ClaimRequest) (TaskInfo, bool, error) {
	tenant, err := b.tenantName(tenantID)
	if err != nil {
		return TaskInfo{}, false, err
	}
	if request.TaskID == 0 || !validActorID(request.Actor.ID) {
		return TaskInfo{}, false, errors.New("superagentboard: claim needs a task and an actor")
	}
	if protocol.ValidateClaimToken(request.Token) != nil {
		return TaskInfo{}, false, errors.New("superagentboard: claim token is not 32 base64url bytes")
	}
	lease := request.Lease
	if lease == 0 {
		lease = DefaultLease
	}
	tokenHash := hashToken(request.Token)
	claimed := false
	err = b.Board.Update(ctx, tenant, boardlog.UpdateOptions{}, func(tx *boardlog.Tx) error {
		root := tx.Message(request.TaskID)
		if root == nil || root.Task == nil {
			return ErrTaskNotFound
		}
		now := tx.Now()
		task := root.Task
		if task.State == StateClaimed && task.Claim != nil {
			if now.Before(task.Claim.Until) {
				if task.Claim.Actor == request.Actor.ID {
					return nil // already the mirrored claim
				}
				return fmt.Errorf("%w: task %d is held by %s until %s",
					ErrClaimConflict, request.TaskID, task.Claim.Actor, protocol.FormatTime(task.Claim.Until))
			}
			// The lease ran out. Expire it first, exactly as the board's claim
			// path does, so the fold accepts the next epoch.
			tx.Emit(boardlog.EventTaskExpired, &boardlog.TaskExpiredPayload{
				ID: request.TaskID, Thread: root.Thread, Epoch: task.Epoch,
			})
		} else if task.State != StateReady {
			return fmt.Errorf("%w: task %d is %s", ErrNotClaimable, request.TaskID, task.State)
		}
		epoch := task.Epoch + 1
		tx.Emit(boardlog.EventTaskClaimed, &boardlog.TaskClaimedPayload{
			ID: request.TaskID, Thread: root.Thread, Epoch: epoch,
			Until: protocol.FormatTime(now.Add(lease)), Token: tokenHash,
			By: request.Actor.Author(),
		})
		tx.Emit(boardlog.EventActorSeen, request.Actor.Seen())
		claimed = true
		return nil
	})
	if err != nil {
		return TaskInfo{}, false, err
	}
	info, err := b.Task(ctx, tenantID, request.TaskID)
	return info, claimed, err
}

// Renew extends a mirrored claim only when it still belongs to the same
// assignment epoch and the remaining lease is below renewBefore. A claim taken
// over by another actor returns ErrClaimConflict and is not touched.
func (b *LogBoard) Renew(ctx context.Context, tenantID uuid.UUID, request RenewRequest) (bool, error) {
	tenant, err := b.tenantName(tenantID)
	if err != nil {
		return false, err
	}
	lease := request.Lease
	if lease == 0 {
		lease = DefaultLease
	}
	renewBefore := request.RenewBefore
	if renewBefore <= 0 {
		renewBefore = DefaultRenewBefore
	}
	renewed := false
	err = b.Board.Update(ctx, tenant, boardlog.UpdateOptions{}, func(tx *boardlog.Tx) error {
		root := tx.Message(request.TaskID)
		if root == nil || root.Task == nil {
			return ErrTaskNotFound
		}
		task := root.Task
		if task.State != StateClaimed || task.Claim == nil {
			return nil
		}
		if task.Claim.Actor != request.Actor.ID || task.Epoch != request.Epoch {
			return fmt.Errorf("%w: task %d is held by %s at epoch %d",
				ErrClaimConflict, request.TaskID, task.Claim.Actor, task.Epoch)
		}
		now := tx.Now()
		if !now.Before(task.Claim.Until) {
			return nil
		}
		if task.Claim.Until.Sub(now) > renewBefore {
			return nil
		}
		tx.Emit(boardlog.EventTaskRenewed, &boardlog.TaskRenewedPayload{
			ID: request.TaskID, Thread: root.Thread, Epoch: request.Epoch,
			Until: protocol.FormatTime(now.Add(lease)), By: request.Actor.Author(),
		})
		renewed = true
		return nil
	})
	return renewed, err
}

// Release hands a mirrored claim back to the pool. It only acts on the exact
// same actor+epoch the adapter mirrored; anything else is a conflict.
func (b *LogBoard) Release(ctx context.Context, tenantID uuid.UUID, request ReleaseRequest) (bool, error) {
	tenant, err := b.tenantName(tenantID)
	if err != nil {
		return false, err
	}
	released := false
	err = b.Board.Update(ctx, tenant, boardlog.UpdateOptions{}, func(tx *boardlog.Tx) error {
		root := tx.Message(request.TaskID)
		if root == nil || root.Task == nil {
			return ErrTaskNotFound
		}
		task := root.Task
		if task.State != StateClaimed || task.Claim == nil {
			return nil
		}
		if task.Claim.Actor != request.Actor.ID || task.Epoch != request.Epoch {
			return fmt.Errorf("%w: task %d is held by %s at epoch %d",
				ErrClaimConflict, request.TaskID, task.Claim.Actor, task.Epoch)
		}
		tx.Emit(boardlog.EventTaskReleased, &boardlog.TaskReleasedPayload{
			ID: request.TaskID, Thread: root.Thread, Epoch: task.Epoch,
			By: request.Actor.Author(),
		})
		released = true
		return nil
	})
	return released, err
}

// Reopen revives a failed mirrored task after the supervisor reopened the
// Ticket. A task that is not failed is left as it is.
func (b *LogBoard) Reopen(ctx context.Context, tenantID uuid.UUID, request ReopenRequest) (bool, error) {
	tenant, err := b.tenantName(tenantID)
	if err != nil {
		return false, err
	}
	reopened := false
	err = b.Board.Update(ctx, tenant, boardlog.UpdateOptions{}, func(tx *boardlog.Tx) error {
		root := tx.Message(request.TaskID)
		if root == nil || root.Task == nil {
			return ErrTaskNotFound
		}
		if root.Task.State != StateFailed {
			return nil
		}
		actor := request.Actor
		if !validActorID(actor.ID) {
			actor = ServiceActor(tenantID)
		}
		tx.Emit(boardlog.EventTaskReopened, &boardlog.TaskReopenedPayload{
			ID: request.TaskID, Thread: root.Thread, By: actor.Author(),
		})
		tx.Emit(boardlog.EventActorSeen, actor.Seen())
		reopened = true
		return nil
	})
	return reopened, err
}

// Complete settles a mirrored task as completed. A claim taken over by another
// actor is left alone (ErrClaimConflict); the supervisor remains the truth and
// its recovery owns that assignment.
func (b *LogBoard) Complete(ctx context.Context, tenantID uuid.UUID, request SettleRequest) error {
	return b.settle(ctx, tenantID, request, boardlog.EventTaskCompleted)
}

// Fail settles a mirrored task as failed.
func (b *LogBoard) Fail(ctx context.Context, tenantID uuid.UUID, request SettleRequest) error {
	return b.settle(ctx, tenantID, request, boardlog.EventTaskFailed)
}

// Cancel calls a mirrored task off. It ignores the epoch: cancellation is the
// supervisor's human stop, not a claim's own write.
func (b *LogBoard) Cancel(ctx context.Context, tenantID uuid.UUID, request SettleRequest) error {
	tenant, err := b.tenantName(tenantID)
	if err != nil {
		return err
	}
	reason := BoardNote(request.Reason, "canceled")
	return b.Board.Update(ctx, tenant, boardlog.UpdateOptions{}, func(tx *boardlog.Tx) error {
		root := tx.Message(request.TaskID)
		if root == nil || root.Task == nil {
			return ErrTaskNotFound
		}
		switch root.Task.State {
		case StateCanceled, StateCompleted:
			return nil
		}
		actor := request.Actor
		if !validActorID(actor.ID) {
			actor = ServiceActor(tenantID)
		}
		tx.Emit(boardlog.EventTaskCanceled, &boardlog.TaskCanceledPayload{
			ID: request.TaskID, Thread: root.Thread, Reason: reason, By: actor.Author(),
		})
		tx.Emit(boardlog.EventActorSeen, actor.Seen())
		return nil
	})
}

func (b *LogBoard) settle(ctx context.Context, tenantID uuid.UUID, request SettleRequest, kind boardlog.EventType) error {
	tenant, err := b.tenantName(tenantID)
	if err != nil {
		return err
	}
	return b.Board.Update(ctx, tenant, boardlog.UpdateOptions{}, func(tx *boardlog.Tx) error {
		root := tx.Message(request.TaskID)
		if root == nil || root.Task == nil {
			return ErrTaskNotFound
		}
		task := root.Task
		if task.State == StateCompleted || task.State == StateFailed || task.State == StateCanceled {
			return nil
		}
		if task.State != StateClaimed || task.Claim == nil {
			return fmt.Errorf("%w: task %d is %s", ErrNotClaimable, request.TaskID, task.State)
		}
		if request.Epoch != 0 && task.Epoch != request.Epoch {
			return fmt.Errorf("%w: task %d is at epoch %d", ErrClaimConflict, request.TaskID, task.Epoch)
		}
		if request.Actor.ID != "" && task.Claim.Actor != request.Actor.ID {
			return fmt.Errorf("%w: task %d is held by %s", ErrClaimConflict, request.TaskID, task.Claim.Actor)
		}
		actor := request.Actor
		if !validActorID(actor.ID) {
			actor = ServiceActor(tenantID)
		}
		if kind == boardlog.EventTaskCompleted {
			result := BoardNote(request.Result, "completed")
			tx.Emit(kind, &boardlog.TaskCompletedPayload{
				ID: request.TaskID, Thread: root.Thread, Epoch: task.Epoch,
				Result: &result, By: actor.Author(),
			})
		} else {
			reason := BoardNote(request.Reason, "failed")
			tx.Emit(kind, &boardlog.TaskFailedPayload{
				ID: request.TaskID, Thread: root.Thread, Epoch: task.Epoch,
				Reason: reason, By: actor.Author(),
			})
		}
		tx.Emit(boardlog.EventActorSeen, actor.Seen())
		return nil
	})
}

// Block holds a mirrored task for a person. An existing hold with another
// reason is replaced by unblock+block so the newest open Attention is the
// reason the board shows; a claim taken over by another actor is left alone.
func (b *LogBoard) Block(ctx context.Context, tenantID uuid.UUID, request BlockRequest) error {
	tenant, err := b.tenantName(tenantID)
	if err != nil {
		return err
	}
	kind := strings.TrimSpace(request.Kind)
	if kind == "" {
		kind = HoldNeedsInput
	}
	reason := BoardNote(request.Reason, "waiting for a person")
	return b.Board.Update(ctx, tenant, boardlog.UpdateOptions{}, func(tx *boardlog.Tx) error {
		root := tx.Message(request.TaskID)
		if root == nil || root.Task == nil {
			return ErrTaskNotFound
		}
		task := root.Task
		switch task.State {
		case StateCompleted, StateFailed, StateCanceled:
			return fmt.Errorf("%w: task %d is %s", ErrTaskClosed, request.TaskID, task.State)
		case StateBlocked:
			if task.Blocked != nil && task.Blocked.Reason == reason &&
				task.Blocked.Kind != nil && *task.Blocked.Kind == kind {
				return nil
			}
			tx.Emit(boardlog.EventTaskUnblocked, &boardlog.TaskUnblockedPayload{
				ID: request.TaskID, Thread: root.Thread, By: request.Actor.Author(),
			})
		case StateClaimed:
			if task.Claim != nil && request.Actor.ID != "" && task.Claim.Actor != request.Actor.ID {
				return fmt.Errorf("%w: task %d is held by %s", ErrClaimConflict, request.TaskID, task.Claim.Actor)
			}
		}
		holdKind := kind
		tx.Emit(boardlog.EventTaskBlocked, &boardlog.TaskBlockedPayload{
			ID: request.TaskID, Thread: root.Thread, Kind: &holdKind,
			Reason: reason, By: request.Actor.Author(),
		})
		tx.Emit(boardlog.EventActorSeen, request.Actor.Seen())
		return nil
	})
}

// Unblock lifts a mirrored hold. A finished task stays finished.
func (b *LogBoard) Unblock(ctx context.Context, tenantID uuid.UUID, request UnblockRequest) error {
	tenant, err := b.tenantName(tenantID)
	if err != nil {
		return err
	}
	return b.Board.Update(ctx, tenant, boardlog.UpdateOptions{}, func(tx *boardlog.Tx) error {
		root := tx.Message(request.TaskID)
		if root == nil || root.Task == nil {
			return ErrTaskNotFound
		}
		switch root.Task.State {
		case StateBlocked:
		case StateCompleted, StateFailed, StateCanceled:
			return nil
		default:
			return nil
		}
		actor := request.Actor
		if !validActorID(actor.ID) {
			actor = ServiceActor(tenantID)
		}
		tx.Emit(boardlog.EventTaskUnblocked, &boardlog.TaskUnblockedPayload{
			ID: request.TaskID, Thread: root.Thread, By: actor.Author(),
		})
		tx.Emit(boardlog.EventActorSeen, actor.Seen())
		return nil
	})
}

// Assign keeps a mirrored task for one of its owner's profiles. An empty For
// returns it to the pool.
func (b *LogBoard) Assign(ctx context.Context, tenantID uuid.UUID, request AssignRequest) error {
	tenant, err := b.tenantName(tenantID)
	if err != nil {
		return err
	}
	forValue := strings.TrimSpace(request.For)
	return b.Board.Update(ctx, tenant, boardlog.UpdateOptions{}, func(tx *boardlog.Tx) error {
		root := tx.Message(request.TaskID)
		if root == nil || root.Task == nil {
			return ErrTaskNotFound
		}
		task := root.Task
		if task.State == StateCompleted || task.State == StateFailed || task.State == StateCanceled {
			return fmt.Errorf("%w: task %d is %s", ErrTaskClosed, request.TaskID, task.State)
		}
		current := ""
		if task.For != nil {
			current = *task.For
		}
		if current == forValue {
			return nil
		}
		var assigned *string
		if forValue != "" {
			if !strings.HasPrefix(forValue, task.Owner+"+") {
				return fmt.Errorf("superagentboard: %q is not one of %s's profiles", forValue, task.Owner)
			}
			assigned = &forValue
		}
		tx.Emit(boardlog.EventTaskAssigned, &boardlog.TaskAssignedPayload{
			ID: request.TaskID, Thread: root.Thread, For: assigned, By: request.Actor.Author(),
		})
		tx.Emit(boardlog.EventActorSeen, request.Actor.Seen())
		return nil
	})
}

// Reply appends one thread reply. Request key + digest make a repeated report
// or attention reply answer the first message instead of appending a second.
func (b *LogBoard) Reply(ctx context.Context, tenantID uuid.UUID, request ReplyRequest) (uint64, error) {
	tenant, err := b.tenantName(tenantID)
	if err != nil {
		return 0, err
	}
	text := sanitizeText(request.Text)
	if text == "" {
		text = "(no summary)"
	}
	if utf8.RuneCountInString(text) > protocol.MessageTextMax {
		text = truncateRunes(text, protocol.MessageTextMax)
	}
	labels := cleanLabels(request.Labels)
	actor := request.Actor
	if !validActorID(actor.ID) {
		actor = ServiceActor(tenantID)
	}
	digest := request.Digest
	if digest == "" {
		digest = digestOf(struct {
			Text   string
			Labels map[string]string
			Actor  string
		}{text, labels, actor.ID})
	}
	var messageID uint64
	err = b.Board.Update(ctx, tenant, boardlog.UpdateOptions{Idempotency: &boardlog.Idempotency{
		Key: request.RequestKey, Digest: digest,
	}}, func(tx *boardlog.Tx) error {
		if replay, ok := tx.Replay(); ok {
			if len(replay.MessageIDs) == 0 {
				return errors.New("superagentboard: reply replayed without a message id")
			}
			messageID = replay.MessageIDs[0]
			return nil
		}
		root := tx.Message(request.TaskID)
		if root == nil || root.Task == nil {
			return ErrTaskNotFound
		}
		re := request.TaskID
		messageID = tx.NextID(0)
		tx.Emit(boardlog.EventMessagePosted, &boardlog.MessagePostedPayload{
			Channel: root.Channel, Text: text, Re: &re, Thread: root.Thread,
			Labels: labels, By: actor.Author(), NoSync: true,
		})
		tx.Emit(boardlog.EventActorSeen, actor.Seen())
		tx.RecordResult(messageID)
		return nil
	})
	return messageID, err
}

// Task reads one board task's current state.
func (b *LogBoard) Task(ctx context.Context, tenantID uuid.UUID, taskID uint64) (TaskInfo, error) {
	tenant, err := b.tenantName(tenantID)
	if err != nil {
		return TaskInfo{}, err
	}
	var info TaskInfo
	found := false
	err = b.Board.ReadContext(ctx, tenant, func(snapshot *boardlog.Snapshot) error {
		message := snapshot.Message(taskID)
		if message == nil || message.Task == nil {
			return nil
		}
		found = true
		info = taskInfoOf(message, snapshot)
		return nil
	})
	if err != nil {
		return TaskInfo{}, err
	}
	if !found {
		return TaskInfo{}, ErrTaskNotFound
	}
	return info, nil
}

// MessageRoot resolves the thread root of a message, so the bridge can refuse
// edits/deletes on a mirrored thread even when the message is a reply.
func (b *LogBoard) MessageRoot(ctx context.Context, tenantID uuid.UUID, messageID uint64) (uint64, string, bool, error) {
	tenant, err := b.tenantName(tenantID)
	if err != nil {
		return 0, "", false, err
	}
	var rootID uint64
	var channel string
	found := false
	err = b.Board.ReadContext(ctx, tenant, func(snapshot *boardlog.Snapshot) error {
		message := snapshot.Message(messageID)
		if message == nil {
			return nil
		}
		root := snapshot.RootOf(messageID)
		if root == nil {
			return nil
		}
		found = true
		rootID = root.ID
		channel = root.Channel
		return nil
	})
	return rootID, channel, found, err
}

// TaskView returns the board's own message view for a mirrored task. It is the
// write response shape, so a Phase 2 client reads the same fields as from the
// board's HTTP handlers.
func (b *LogBoard) TaskView(ctx context.Context, tenantID uuid.UUID, taskID uint64) (protocol.MessageView, error) {
	tenant, err := b.tenantName(tenantID)
	if err != nil {
		return protocol.MessageView{}, err
	}
	var view protocol.MessageView
	found := false
	err = b.Board.ReadContext(ctx, tenant, func(snapshot *boardlog.Snapshot) error {
		message := snapshot.Message(taskID)
		if message == nil || message.Task == nil {
			return nil
		}
		found = true
		view = messageViewOf(snapshot, message)
		return nil
	})
	if err != nil {
		return protocol.MessageView{}, err
	}
	if !found {
		return protocol.MessageView{}, ErrTaskNotFound
	}
	return view, nil
}

func taskInfoOf(message *boardlog.Message, snapshot *boardlog.Snapshot) TaskInfo {
	info := TaskInfo{
		ID: message.ID, Channel: message.Channel, Owner: message.Task.Owner,
		State: message.Task.State,
		Epoch: message.Task.Epoch, Version: message.Version, Text: message.Text,
		Labels:  effectiveLabels(message),
		Replies: len(snapshot.Replies(message.ID)),
	}
	if message.Task.Claim != nil {
		info.ClaimActor = message.Task.Claim.Actor
		info.ClaimUntil = message.Task.Claim.Until
	}
	if message.Task.Blocked != nil {
		info.BlockedReason = message.Task.Blocked.Reason
		if message.Task.Blocked.Kind != nil {
			info.BlockedKind = *message.Task.Blocked.Kind
		}
	}
	if message.Task.For != nil {
		info.For = *message.Task.For
	}
	return info
}

func messageViewOf(snapshot *boardlog.Snapshot, message *boardlog.Message) protocol.MessageView {
	view := protocol.MessageView{
		ID:        strconv.FormatUint(message.ID, 10),
		Channel:   message.Channel,
		Text:      message.Text,
		Thread:    message.Thread,
		Labels:    effectiveLabels(message),
		Reactions: map[string][]protocol.Ref{},
		NoSync:    message.NoSync,
		CreatedAt: protocol.FormatTime(message.CreatedAt),
		Deleted:   message.Deleted,
		Version:   message.Version,
		Revision:  message.Revision,
		Blocks:    []protocol.BlockView{},
		Links:     []protocol.LinkView{},
	}
	if message.Re != nil {
		re := strconv.FormatUint(*message.Re, 10)
		view.Re = &re
	}
	if message.ReplyTo != nil {
		replyTo := strconv.FormatUint(*message.ReplyTo, 10)
		view.ReplyTo = &replyTo
	}
	if message.From != nil {
		from := *message.From
		view.From = &from
	}
	if message.EditedAt != nil {
		edited := protocol.FormatTime(*message.EditedAt)
		view.EditedAt = &edited
	}
	for emoji, actors := range message.Reactions {
		refs := make([]protocol.Ref, 0, len(actors))
		for _, actor := range actors {
			refs = append(refs, actor)
		}
		view.Reactions[emoji] = refs
	}
	if message.Task != nil {
		view.Task = taskViewOf(message.Task)
	}
	actorView := actorViewOf(snapshot, message.By.Actor)
	view.By = actorView
	view.Author = actorView
	return view
}

func taskViewOf(task *boardlog.Task) *protocol.TaskView {
	if task == nil {
		return nil
	}
	view := &protocol.TaskView{
		State: task.State, Owner: task.Owner, By: task.By,
	}
	if task.For != nil {
		forValue := *task.For
		view.For = &forValue
	}
	for _, need := range task.Needs {
		view.Needs = append(view.Needs, strconv.FormatUint(need, 10))
	}
	if task.Claim != nil {
		view.Claim = &protocol.ClaimView{
			Actor: task.Claim.Actor,
			Until: protocol.FormatTime(task.Claim.Until),
		}
	}
	if task.Blocked != nil {
		hold := &protocol.HoldView{Reason: task.Blocked.Reason}
		if task.Blocked.Kind != nil {
			kind := *task.Blocked.Kind
			hold.Kind = &kind
		}
		view.Blocked = hold
	}
	if task.Result != nil {
		result := *task.Result
		view.Result = &result
	}
	if task.Reason != nil {
		reason := *task.Reason
		view.Reason = &reason
	}
	return view
}

func actorViewOf(snapshot *boardlog.Snapshot, id string) *protocol.ActorView {
	record := snapshot.Actor(id)
	if record == nil {
		return &protocol.ActorView{Kind: protocol.ActorWorkflow, Handle: id, Name: id}
	}
	view := &protocol.ActorView{
		Kind: protocol.ActorKind(record.Kind), Handle: record.Handle, Name: record.Name,
		Platform: record.Platform, Verified: record.Verified,
	}
	if record.Avatar != nil {
		avatar := *record.Avatar
		view.Avatar = &avatar
	}
	if record.Via != nil {
		via := &protocol.Via{Name: record.Via.Name}
		if record.Via.Environment != nil {
			environment := *record.Via.Environment
			via.Environment = &environment
		}
		view.Via = via
	}
	return view
}

func effectiveLabels(message *boardlog.Message) map[string]string {
	labels := make(map[string]string, len(message.Labels)+1)
	for key, value := range message.Labels {
		labels[key] = value
	}
	if message.Task != nil {
		labels[protocol.TaskLabel] = message.Task.State
	}
	return labels
}

// BoardText is the mirrored root text: Title then Request, bounded to the
// board's 10k message limit. The full content stays in superagent.
func BoardText(title, request string) string {
	title = sanitizeText(strings.TrimSpace(title))
	request = sanitizeText(strings.TrimSpace(request))
	switch {
	case title == "" && request == "":
		return "(superagent ticket)"
	case request == "":
		return truncateRunes(title, protocol.MessageTextMax)
	case title == "":
		return truncateRunes(request, protocol.MessageTextMax)
	default:
		return truncateRunes(title+"\n\n"+request, protocol.MessageTextMax)
	}
}

// BoardNote is a result/reason bounded to the board's 1000-character note.
func BoardNote(value, fallback string) string {
	value = sanitizeText(strings.TrimSpace(value))
	if value == "" {
		value = fallback
	}
	return truncateRunes(value, 1000)
}

// sanitizeText removes NUL and invalid UTF-8 and collapses control characters
// JSONB cannot store. Newlines and tabs stay: a ticket request is text.
func sanitizeText(value string) string {
	if value == "" {
		return ""
	}
	value = strings.ToValidUTF8(value, "\uFFFD")
	var builder strings.Builder
	builder.Grow(len(value))
	for _, r := range value {
		switch {
		case r == 0:
			continue
		case r == '\n' || r == '\t' || r == '\r':
			builder.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			builder.WriteRune(' ')
		default:
			builder.WriteRune(r)
		}
	}
	return strings.TrimSpace(builder.String())
}

func truncateRunes(value string, max int) string {
	if utf8.RuneCountInString(value) <= max {
		return value
	}
	count := 0
	for index := range value {
		if count == max {
			return strings.TrimSpace(value[:index])
		}
		count++
	}
	return value
}

func cleanLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	for key, value := range labels {
		if err := protocol.ValidateLabelKey(key); err != nil {
			continue
		}
		if err := protocol.ValidateLabelValue(value); err != nil {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func digestOf(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		encoded = []byte(fmt.Sprintf("%v", value))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func hashToken(token string) string { return boarddomain.HashToken(token) }

func validActorID(id string) bool { return boarddomain.ValidActorID(id) }

// TicketLabels is the label set of one mirrored Ticket root. board.* keys are
// the board's own and are never written by a caller; source/ticket/mandate/
// policy are ordinary labels.
func TicketLabels(ticket superagent.Ticket) map[string]string {
	labels := map[string]string{
		"source": BoardSource,
		"ticket": ticket.ID.String(),
	}
	if strength := strings.TrimSpace(string(ticket.Mandate.Strength)); strength != "" {
		labels["mandate"] = strength
	}
	if policy := strings.TrimSpace(ticket.Mandate.PolicyVersion); policy != "" {
		labels["policy"] = policy
	}
	return labels
}

// ReplyLabels labels a mirrored report/attention reply.
func ReplyLabels(ticketID uuid.UUID) map[string]string {
	return map[string]string{"source": BoardSource, "ticket": ticketID.String()}
}

// attentionHoldKind maps a superagent Attention kind to the board hold kind.
func attentionHoldKind(kind string) string {
	switch strings.TrimSpace(kind) {
	case collab.AttentionApproval, collab.AttentionDecision:
		return HoldAwaitingApproval
	default:
		return HoldNeedsInput
	}
}
