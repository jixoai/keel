package superagentplugin

import (
	"context"
	"fmt"
	"strconv"
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
// a ticket + its first open assignment, and the link row makes a repeat a
// no-op. Nothing here reads a board table; the task arrives from the board
// plugin's Service.
type ImportOptions struct {
	Store     superagent.Store
	Links     LinkStore
	TenantID  uuid.UUID
	Requester superagent.Actor
	WorkerBot string
	Now       func() time.Time
}

// ImportBoardTask creates (or finds) the ticket of one board task and its first
// open assignment. created=false means the link already existed or the
// idempotent create returned the original row.
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
			Title: title,
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
		return superagent.TicketFromWorkItem(stored), false, nil
	}
	if _, err := opts.Links.PutBoardLink(ctx, superagentstore.BoardLink{
		TenantID: opts.TenantID, Source: BoardSource, SourceID: linkSourceID,
		Kind: superagentstore.BoardLinkTicket, BoardTaskID: task.ID,
	}); err != nil {
		return superagent.Ticket{}, false, err
	}
	assignment := collab.Assignment{
		ID:       uuid.NewSHA1(uuid.NameSpaceOID, []byte("board.assignment|"+opts.TenantID.String()+"|"+taskRef)),
		TenantID: opts.TenantID, WorkItemID: stored.ID, ModeID: superagent.DefaultModeID,
		BotID: opts.WorkerBot, Status: collab.AssignmentOpen, Title: title,
		IdempotencyKey: "board-open:" + taskRef, CreatedAt: now,
	}
	if _, err := opts.Store.CreateAssignment(ctx, assignment); err != nil {
		return superagent.Ticket{}, false, err
	}
	return superagent.TicketFromWorkItem(stored), true, nil
}

// ImportChannel imports every task of one board channel that has no ticket
// link yet. It reads through the board Service (never a board table) and
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
