package superagentplugin

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/boardplugin"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
	"github.com/jixoai/keel/internal/superagentstore"
)

// BoardTaskSource is the source kind of a ticket imported from a board task.
const BoardTaskSource = "board"

// ImportOptions wires the board -> ticket direction. It is the explicit
// replacement for the deleted mirror's import path: a board task is turned into
// a ticket, and the link row makes a repeat a no-op. Nothing here reads a board
// table; the task arrives from the board plugin's Service.
//
// The importer never writes an assignment row. A sessionless OPEN assignment
// used to block the supervisor's dispatch forever (dispatch skips an
// assignment that already exists), so the imported ticket is handed to
// Dispatch, which runs the ordinary queue pump and lets the Host create the
// worker session with the configured WorkerBot. A deployment without Dispatch
// (a pure import test) still gets the ticket, just without a worker.
type ImportOptions struct {
	Store     superagent.Store
	Links     LinkStore
	TenantID  uuid.UUID
	Requester superagent.Actor
	WorkerBot string
	// OwnerSessionID is the supervisor's orchestrator session. The guarded Host
	// only lets a work-item member dispatch an assignment, and a source-event
	// ticket records this owner at creation; an imported ticket must do the
	// same or the normal dispatch would be permission-denied.
	OwnerSessionID uuid.UUID
	// Dispatch runs the supervisor's normal dispatch for a newly imported (or
	// previously imported but still undelivered) ticket. Nil leaves the ticket
	// queued for the orchestrator's own housekeeping pump.
	Dispatch func(ctx context.Context, ticketID uuid.UUID) error
	Now      func() time.Time
}

// ImportBoardTask creates (or finds) the ticket of one board task. created=false
// means the task is already linked, is a projection of an existing ticket, or
// is not an executable board state (completed/canceled/archived history is not
// a new open ticket).
func ImportBoardTask(ctx context.Context, opts ImportOptions, task boardplugin.TaskInfo) (superagent.Ticket, bool, error) {
	if opts.Store == nil || opts.Links == nil {
		return superagent.Ticket{}, false, fmt.Errorf("superagentplugin: import needs the ticket store and link store")
	}
	if opts.TenantID == uuid.Nil || task.ID == 0 {
		return superagent.Ticket{}, false, fmt.Errorf("superagentplugin: import needs a tenant and a task id")
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	// The board task's own link is the first identity check. A task that the
	// superagent writer projected from a ticket already has a ticket link with
	// this board_task_id; treating it as a new source task would create a
	// second ticket and then collide with the one-ticket-per-task index.
	if existing, found, err := opts.Links.GetBoardLinkByTask(ctx, opts.TenantID, BoardSource, superagentstore.BoardLinkTicket, task.ID); err != nil {
		return superagent.Ticket{}, false, err
	} else if found {
		ticketID, parseErr := uuid.Parse(existing.SourceID)
		if parseErr != nil {
			return superagent.Ticket{}, false, fmt.Errorf("superagentplugin: board link for task %d has a malformed ticket id %q: %w", task.ID, existing.SourceID, parseErr)
		}
		item, err := opts.Store.GetWorkItem(ctx, opts.TenantID, ticketID)
		if err != nil {
			return superagent.Ticket{}, false, fmt.Errorf("superagentplugin: board link for task %d points to a missing ticket: %w", task.ID, err)
		}
		ticket := superagent.TicketFromWorkItem(item)
		// A link without a live assignment is a repairable state (a crash
		// between import and dispatch). Re-running the normal pump is
		// idempotent: an active assignment makes it a no-op.
		if err := opts.dispatch(ctx, ticket.ID); err != nil {
			return superagent.Ticket{}, false, err
		}
		return ticket, false, nil
	}
	if projectedBoardTask(task) {
		// No link yet, but the labels say this task is a projection. Skipping is
		// safer than importing a mirrored task as a second truth; a missing link
		// for a projection is repaired on the writer's next pass.
		return superagent.Ticket{}, false, nil
	}
	if !importableBoardState(task) {
		// completed/canceled/failed/archived board tasks are board history, not
		// a new open ticket. The supervisor's own history stays the truth.
		return superagent.Ticket{}, false, nil
	}
	now := opts.Now()
	requester := opts.Requester
	if requester.ID == "" {
		requester = superagent.Actor{ID: "board:" + opts.TenantID.String(), Display: "Board"}
	}
	taskRef := strconv.FormatUint(task.ID, 10)
	ticketID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("board.ticket|"+opts.TenantID.String()+"|"+taskRef))
	thread := superagent.ThreadRef{Source: BoardTaskSource, Kind: "task", ID: taskRef}
	title := task.Text
	if title == "" {
		title = taskRef
	}
	ticket := superagent.Ticket{
		WorkItem: collab.WorkItem{
			ID: ticketID, TenantID: opts.TenantID, ModeID: superagent.DefaultModeID,
			Title:          title,
			OwnerSessionID: opts.OwnerSessionID,
			Source: collab.SourceRef{
				Kind: BoardTaskSource, ID: taskRef, URI: "/v1/board/tasks/" + taskRef,
			},
			Status:         collab.WorkItemOpen,
			IdempotencyKey: "board:" + taskRef,
			CreatedAt:      now,
		},
		Mandate: superagent.Mandate{
			Requester: requester, Source: BoardTaskSource, Thread: thread,
			Strength: superagent.MandateWrite, AcceptedAt: now, AcceptedBy: "board",
		},
		Thread:    thread,
		Request:   task.Text,
		WorkerBot: opts.WorkerBot,
	}
	stored, err := opts.Store.CreateWorkItem(ctx, ticket.Record())
	if err != nil {
		return superagent.Ticket{}, false, err
	}
	linkSourceID := ticketID.String()
	if _, found, err := opts.Links.GetBoardLink(ctx, opts.TenantID, BoardSource, linkSourceID, superagentstore.BoardLinkTicket); err != nil {
		return superagent.Ticket{}, false, err
	} else if found {
		if err := opts.dispatch(ctx, ticketID); err != nil {
			return superagent.Ticket{}, false, err
		}
		return superagent.TicketFromWorkItem(stored), false, nil
	}
	if _, err := opts.Links.PutBoardLink(ctx, superagentstore.BoardLink{
		TenantID: opts.TenantID, Source: BoardSource, SourceID: linkSourceID,
		Kind: superagentstore.BoardLinkTicket, BoardTaskID: task.ID,
	}); err != nil {
		return superagent.Ticket{}, false, err
	}
	if err := opts.dispatch(ctx, stored.ID); err != nil {
		return superagent.Ticket{}, false, err
	}
	return superagent.TicketFromWorkItem(stored), true, nil
}

func (opts ImportOptions) dispatch(ctx context.Context, ticketID uuid.UUID) error {
	if opts.Dispatch == nil || ticketID == uuid.Nil {
		return nil
	}
	if err := opts.Dispatch(ctx, ticketID); err != nil {
		if errors.Is(err, collab.ErrConcurrencyExceeded) {
			// The queue is at its concurrency ceiling; the orchestrator's
			// housekeeping pump retries this ticket. Import itself succeeded.
			return nil
		}
		return fmt.Errorf("superagentplugin: dispatch imported ticket %s: %w", ticketID, err)
	}
	return nil
}

// projectedBoardTask reports whether the task already belongs to a ticket. The
// writer labels every root it creates with source=superagent and ticket=<id>;
// reading the label is the second check that survives a lost link row.
func projectedBoardTask(task boardplugin.TaskInfo) bool {
	if strings.EqualFold(strings.TrimSpace(task.Labels["source"]), BoardSource) {
		return true
	}
	return strings.TrimSpace(task.Labels["ticket"]) != ""
}

// importableBoardState keeps board history out of the work queue. Only a task
// a worker can still act on becomes a ticket; completed/canceled/failed tasks
// are append-only board records.
func importableBoardState(task boardplugin.TaskInfo) bool {
	if strings.EqualFold(strings.TrimSpace(task.Labels[ArchivedLabel]), "true") {
		return false
	}
	switch task.State {
	case StateWaiting, StateReady, StateClaimed, StateBlocked:
		return true
	default:
		return false
	}
}

// ImportChannel imports every importable task of one board channel that has no
// ticket link yet. It reads through the board Service (never a board table) and
// returns how many tickets it created.
func ImportChannel(ctx context.Context, board boardplugin.Service, opts ImportOptions, channel string, limit int) (int, error) {
	if board == nil {
		return 0, fmt.Errorf("superagentplugin: a board service is required")
	}
	tasks, err := board.Tasks(ctx, opts.TenantID, boardplugin.TaskQuery{Channel: channel, Limit: limit})
	if err != nil {
		return 0, err
	}
	created := 0
	for _, task := range tasks {
		if ctx.Err() != nil {
			return created, ctx.Err()
		}
		if _, wasCreated, err := ImportBoardTask(ctx, opts, task); err != nil {
			return created, err
		} else if wasCreated {
			created++
		}
	}
	return created, nil
}
