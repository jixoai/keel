package superagentplugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/boardplugin"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
	"github.com/jixoai/keel/internal/superagentstore"

	"github.com/jixoai/keel-board/protocol"
)

// LinkStore is the durable identity map the mirror and bridge share. The
// production implementation is keel's *store.Store; tests use a map.
type LinkStore interface {
	PutBoardLink(ctx context.Context, link superagentstore.BoardLink) (superagentstore.BoardLink, error)
	GetBoardLink(ctx context.Context, tenantID uuid.UUID, source, sourceID, kind string) (superagentstore.BoardLink, bool, error)
	GetBoardLinkByTask(ctx context.Context, tenantID uuid.UUID, source, kind string, boardTaskID uint64) (superagentstore.BoardLink, bool, error)
	SetBoardLinkClaimEpoch(ctx context.Context, tenantID uuid.UUID, source, sourceID, kind string, epoch uint64) error
}

// SyncReport is one mirror pass's accounting. It never carries text, a token
// or a secret: counters and ids only.
type SyncReport struct {
	Tickets      int `json:"tickets"`
	TasksCreated int `json:"tasksCreated"`
	Archived     int `json:"archived"`
	Claims       int `json:"claims"`
	Renewed      int `json:"renewed"`
	Released     int `json:"released"`
	Reopened     int `json:"reopened"`
	Blocked      int `json:"blocked"`
	Unblocked    int `json:"unblocked"`
	Replies      int `json:"replies"`
	Completed    int `json:"completed"`
	Failed       int `json:"failed"`
	Canceled     int `json:"canceled"`
	Skipped      int `json:"skipped"`
	Conflicts    int `json:"conflicts"`
}

// ChannelACL grants the relation a mirrored channel's operators need. The
// mirror writes through the board log (no request identity), so it must record
// who may operate the queue: a board channel with no relation is invisible even
// to a tenant's board.read holders. The production implementation grants the
// operator groups owner, which includes editor (bridge actions) and viewer
// (reads). A nil ACL leaves the channel relation-free (a deployment that wires
// its own access policy).
type ChannelACL interface {
	EnsureOperator(ctx context.Context, tenantID uuid.UUID, channel string) error
}

// Writer projects one tenant's supervisor records into the board. It is a
// read of collab_* plus append-only board writes; it never updates a
// supervisor record, which is why a board write failure can be repaired by the
// next pass without losing truth.
type Writer struct {
	TenantID uuid.UUID
	Store    superagent.Store
	Board    boardplugin.Service
	Links    LinkStore
	Config   ConfigReader
	Secret   *TokenSource
	ACL      ChannelACL
	// Enabled is the tenant board switch. A nil value means "no gate" (an
	// embedded test); production wires Surface.BoardEnabled. When it returns
	// false, a pass appends no board event and does not read/sync a ticket.
	Enabled func(ctx context.Context) bool
	// Import pulls new board tasks into tickets before the write-back pass.
	// Nil keeps the adapter ticket -> board only.
	Import func(ctx context.Context) error
	Logger *slog.Logger
	Now    func() time.Time

	Lease       time.Duration
	RenewBefore time.Duration
	// Interval is Run's tick. Zero uses 30s; a negative value runs no ticker
	// (a caller drives SyncOnce).
	Interval time.Duration
	// TicketLimit bounds one pass's head, newest-updated first. Zero uses 500.
	TicketLimit int

	aclMu       sync.Mutex
	aclChannels map[string]bool
}

// NewWriter validates the mandatory seams. A missing secret is not a
// construction error: Run and SyncOnce fail closed with ErrSecretMissing, so a
// deployment can keep serving the board while an operator fixes the secret.
func NewWriter(opts *Writer) (*Writer, error) {
	if opts == nil {
		return nil, errors.New("superagentplugin: mirror options are required")
	}
	if opts.TenantID == uuid.Nil {
		return nil, errors.New("superagentplugin: mirror tenant is required")
	}
	if opts.Store == nil {
		return nil, errors.New("superagentplugin: mirror needs the supervisor store")
	}
	if opts.Board == nil {
		return nil, errors.New("superagentplugin: mirror needs a board port")
	}
	if opts.Links == nil {
		return nil, errors.New("superagentplugin: mirror needs a link store")
	}
	if opts.Config == nil {
		return nil, errors.New("superagentplugin: mirror needs a config reader")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Lease == 0 {
		opts.Lease = DefaultLease
	}
	if opts.RenewBefore == 0 {
		opts.RenewBefore = DefaultRenewBefore
	}
	if opts.Interval == 0 {
		opts.Interval = 30 * time.Second
	}
	if opts.TicketLimit == 0 {
		opts.TicketLimit = 500
	}
	return opts, nil
}

// Run syncs on a ticker until ctx ends. Every tick re-reads the tenant
// configuration, so switching mirror off stops new board writes without a
// restart and switching it on starts them.
func (m *Writer) Run(ctx context.Context) {
	if m.Interval < 0 {
		return
	}
	ticker := time.NewTicker(m.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := m.SyncOnce(ctx); err != nil && !errors.Is(err, ErrSecretMissing) && ctx.Err() == nil {
				m.Logger.Warn("superagent board mirror pass failed", "err", err)
			}
		}
	}
}

// boardEnabled applies the configuration gate. A nil gate is permissive (the
// test/embedding seam); the production gate fails closed on a flag read error.
func (m *Writer) boardEnabled(ctx context.Context) bool {
	return m.Enabled == nil || m.Enabled(ctx)
}

// SyncOnce mirrors every ticket. A disabled mirror is a no-op, not an error.
func (m *Writer) SyncOnce(ctx context.Context) (SyncReport, error) {
	var report SyncReport
	if !m.boardEnabled(ctx) {
		return report, nil
	}
	config, err := m.Config.SuperagentConfig(ctx, m.TenantID)
	if err != nil {
		return report, err
	}
	if !config.Mirror {
		return report, nil
	}
	if m.Secret == nil {
		return report, ErrSecretMissing
	}
	if m.Import != nil {
		if err := m.Import(ctx); err != nil {
			return report, err
		}
	}
	items, err := m.Store.ListWorkItems(ctx, m.TenantID, collab.WorkFilter{
		ModeID: superagent.DefaultModeID, Order: collab.WorkOrderUpdatedDesc,
	})
	if err != nil {
		return report, fmt.Errorf("superagentplugin: list tickets: %w", err)
	}
	limit := m.TicketLimit
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	report.Tickets = len(items)
	for _, item := range items {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		ticketReport, err := m.syncTicket(ctx, config, superagent.TicketFromWorkItem(item))
		accumulate(&report, ticketReport)
		if err != nil {
			// One ticket's failure must not stop the rest: each ticket is
			// independent and the next pass retries it.
			m.Logger.Warn("superagent board mirror ticket failed",
				"ticket", item.ID, "err", err)
			continue
		}
	}
	return report, nil
}

// SyncTicket mirrors one ticket. The command bridge calls it after the
// supervisor accepted an action, so the board reflects the new truth only
// after the supervisor wrote it.
func (m *Writer) SyncTicket(ctx context.Context, ticketID uuid.UUID) (SyncReport, error) {
	var report SyncReport
	if !m.boardEnabled(ctx) {
		return report, nil
	}
	config, err := m.Config.SuperagentConfig(ctx, m.TenantID)
	if err != nil {
		return report, err
	}
	if !config.Mirror {
		return report, nil
	}
	if m.Secret == nil {
		return report, ErrSecretMissing
	}
	item, err := m.Store.GetWorkItem(ctx, m.TenantID, ticketID)
	if err != nil {
		return report, err
	}
	if item.ModeID != superagent.DefaultModeID {
		return report, fmt.Errorf("superagentplugin: work item %s is not a super-agent ticket", ticketID)
	}
	return m.syncTicket(ctx, config, superagent.TicketFromWorkItem(item))
}

func (m *Writer) syncTicket(ctx context.Context, config SuperagentConfig, ticket superagent.Ticket) (SyncReport, error) {
	var report SyncReport
	if ticket.ID == uuid.Nil {
		return report, errors.New("superagentplugin: ticket has no id")
	}
	terminal := isTerminalTicket(ticket)
	archivedImport := terminal
	link, linked, err := m.Links.GetBoardLink(ctx, m.TenantID, BoardSource, ticket.ID.String(), superagentstore.BoardLinkTicket)
	if err != nil {
		return report, err
	}
	var task boardplugin.TaskInfo
	if linked {
		archivedImport = false
		task, err = m.Board.Task(ctx, m.TenantID, link.BoardTaskID)
		if err != nil {
			return report, err
		}
	} else {
		labels := TicketLabels(ticket)
		if archivedImport {
			// An archived Ticket is board history, not a second task face:
			// import it once, read-only, and keep its old audit in superagent.
			// The label is written through the board log, so the reserved
			// board.* namespace stays the mirror's own.
			labels[ArchivedLabel] = "true"
		}
		task, err = m.Board.EnsureTicketThread(ctx, m.TenantID, TicketThread{
			TicketID: ticket.ID.String(),
			Channel:  config.Channel,
			Title:    ticket.Title,
			Request:  ticket.Request,
			Labels:   labels,
			Actor:    ServiceActor(m.TenantID),
		})
		if err != nil {
			return report, err
		}
		if _, err := m.Links.PutBoardLink(ctx, superagentstore.BoardLink{
			TenantID: m.TenantID, Source: BoardSource, SourceID: ticket.ID.String(),
			Kind: superagentstore.BoardLinkTicket, BoardTaskID: task.ID,
		}); err != nil {
			return report, err
		}
		report.TasksCreated++
		if archivedImport {
			report.Archived++
		}
	}
	m.ensureACL(ctx, config.Channel)
	stateReport, err := m.syncTicketState(ctx, ticket, task)
	if err != nil {
		// P2 (hardening): a failed supervisor read is not an empty set. Without
		// the assignments/attentions/reports we cannot tell "no hold" from
		// "hold unknown", and advancing could release or unblock a task.
		report.Skipped++
		return report, err
	}
	accumulate(&report, stateReport)
	m.recordGitHubEvent(ctx, ticket, task.ID)
	return report, nil
}

// ensureACL writes the channel's read relation once per process; a failure is
// retried by the next pass instead of being forgotten with the created flag.
func (m *Writer) ensureACL(ctx context.Context, channel string) {
	if m == nil || m.ACL == nil || strings.TrimSpace(channel) == "" {
		return
	}
	m.aclMu.Lock()
	if m.aclChannels == nil {
		m.aclChannels = map[string]bool{}
	}
	if m.aclChannels[channel] {
		m.aclMu.Unlock()
		return
	}
	m.aclMu.Unlock()
	if err := m.ACL.EnsureOperator(ctx, m.TenantID, channel); err != nil {
		m.Logger.Warn("superagent board mirror channel operator relation failed", "channel", channel, "err", err)
		return
	}
	m.aclMu.Lock()
	m.aclChannels[channel] = true
	m.aclMu.Unlock()
}

func (m *Writer) syncTicketState(ctx context.Context, ticket superagent.Ticket, task boardplugin.TaskInfo) (SyncReport, error) {
	var report SyncReport
	if task.ID == 0 {
		return report, nil
	}
	assignments, err := m.Store.ListAssignments(ctx, m.TenantID, ticket.ID)
	if err != nil {
		return report, fmt.Errorf("superagentplugin: read assignments: %w", err)
	}
	attentions, err := m.Store.ListAttentions(ctx, m.TenantID, ticket.ID)
	if err != nil {
		return report, fmt.Errorf("superagentplugin: read attentions: %w", err)
	}
	reports, err := m.Store.ListWorkerReports(ctx, m.TenantID, ticket.ID)
	if err != nil {
		return report, fmt.Errorf("superagentplugin: read reports: %w", err)
	}

	activeAssignment := activeAssignmentOf(assignments)
	openAttention := oldestOpenAttention(attentions)
	hasClosedAttention := false
	for _, attention := range attentions {
		if attention.Status != collab.AttentionOpen {
			hasClosedAttention = true
			break
		}
	}
	ticketTerminal := isTerminalTicket(ticket)
	ticketAcceptsWork := ticket.Status == collab.WorkItemOpen || ticket.Status == collab.WorkItemActive

	// 0. A supervisor reopen (ResumeTicket) revives a failed task in place, so
	// the same pass can claim or block it. A completed board task cannot be
	// reopened by the board domain; that remains the supervisor's record.
	if !ticketTerminal && task.State == StateFailed {
		if reopened, err := m.Board.Reopen(ctx, m.TenantID, ReopenRequest{
			TaskID: task.ID, Actor: ServiceActor(m.TenantID),
		}); err != nil {
			m.Logger.Warn("superagent board mirror reopen failed", "ticket", ticket.ID, "err", err)
		} else if reopened {
			report.Reopened++
			task, _ = m.Board.Task(ctx, m.TenantID, task.ID)
		}
	}

	// A completed board task cannot be reopened by the board domain. Say so
	// explicitly instead of retrying a claim that the fold must refuse forever.
	if !ticketTerminal && task.State == StateCompleted {
		report.Conflicts++
		m.Logger.Warn("superagent board task is completed but the ticket is not; board history is append-only",
			"ticket", ticket.ID, "task", task.ID, "ticketStatus", ticket.Status)
	}

	// 1. A hold wins over a claim. When the newest Attention closed, unblock
	// first so the same pass can claim the replacement assignment.
	if openAttention != nil && !ticketTerminal {
		actor := ServiceActor(m.TenantID)
		if task.State == StateClaimed && strings.TrimSpace(task.ClaimActor) != "" {
			// The hold comes from the claim that saw the question: the board's
			// own rule is that a live claim's actor places the hold, and the
			// fold then ends that claim.
			actor = claimActorOf(task)
		}
		holdKind := attentionHoldKind(openAttention.Kind)
		holdReason := BoardNote(openAttention.Prompt, "waiting for a person")
		alreadyHeld := task.State == StateBlocked && task.BlockedKind == holdKind && task.BlockedReason == holdReason
		if err := m.Board.Block(ctx, m.TenantID, BlockRequest{
			TaskID: task.ID, Kind: holdKind,
			Reason: openAttention.Prompt, Actor: actor,
		}); err != nil {
			if errors.Is(err, boardplugin.ErrClaimConflict) {
				report.Conflicts++
				m.logClaimConflict(ticket, task, err)
			} else {
				m.Logger.Warn("superagent board mirror block failed", "ticket", ticket.ID, "err", err)
			}
		} else if !alreadyHeld {
			report.Blocked++
		}
		task, _ = m.Board.Task(ctx, m.TenantID, task.ID)
	} else if task.State == StateBlocked && (ticketTerminal || (openAttention == nil &&
		(ticket.Status != collab.WorkItemBlocked || hasClosedAttention))) {
		if err := m.Board.Unblock(ctx, m.TenantID, UnblockRequest{
			TaskID: task.ID, Actor: ServiceActor(m.TenantID),
		}); err != nil {
			m.Logger.Warn("superagent board mirror unblock failed", "ticket", ticket.ID, "err", err)
		} else {
			report.Unblocked++
		}
		task, _ = m.Board.Task(ctx, m.TenantID, task.ID)
	}

	// 2. Mirrored claim / lease. A blocked or finished ticket holds no claim.
	// A completed board task is left alone (warned above): the board domain has
	// no completed -> ready edge.
	if ticketAcceptsWork && openAttention == nil && activeAssignment != nil &&
		task.State != StateBlocked && task.State != StateCompleted {
		if m.mirrorClaim(ctx, ticket, *activeAssignment, task, &report) {
			task, _ = m.Board.Task(ctx, m.TenantID, task.ID)
		}
	}
	if !ticketTerminal && openAttention == nil && activeAssignment == nil && task.State == StateClaimed {
		released, err := m.Board.Release(ctx, m.TenantID, ReleaseRequest{
			TaskID: task.ID, Actor: claimActorOf(task), Epoch: task.Epoch,
		})
		if err != nil {
			if errors.Is(err, boardplugin.ErrClaimConflict) {
				report.Conflicts++
				m.logClaimConflict(ticket, task, err)
			} else {
				m.Logger.Warn("superagent board mirror release failed", "ticket", ticket.ID, "err", err)
			}
		} else if released {
			report.Released++
		}
		task, _ = m.Board.Task(ctx, m.TenantID, task.ID)
	}

	// 3. Closed Attentions become replies; a free-text answer stays in
	// superagent. The link row is the fast idempotency check, the board request
	// key is the durable one.
	for _, attention := range attentions {
		if attention.Status == collab.AttentionOpen {
			continue
		}
		if _, found, err := m.Links.GetBoardLink(ctx, m.TenantID, BoardSource, attention.ID.String(), superagentstore.BoardLinkAttention); err == nil && found {
			continue
		}
		if err := m.mirrorAttentionReply(ctx, ticket, task.ID, attention, &report); err != nil {
			m.Logger.Warn("superagent board mirror attention reply failed",
				"ticket", ticket.ID, "attention", attention.ID, "err", err)
		}
	}

	// 4. Reports become replies. A report whose state transition has not landed
	// is skipped; the next pass sees the applied row.
	for _, row := range reports {
		if row.AppliedAt == nil {
			continue
		}
		if _, found, err := m.Links.GetBoardLink(ctx, m.TenantID, BoardSource, row.ID.String(), superagentstore.BoardLinkReport); err == nil && found {
			continue
		}
		if _, err := m.Board.Reply(ctx, m.TenantID, ReplyRequest{
			TaskID:     task.ID,
			Text:       ReportText(row),
			Labels:     ReplyLabels(ticket.ID),
			Actor:      ServiceActor(m.TenantID),
			RequestKey: "superagent:report:" + row.ID.String(),
			Digest:     digestOf(struct{ State, Summary string }{string(row.State), strings.TrimSpace(row.Summary)}),
		}); err != nil {
			m.Logger.Warn("superagent board mirror report reply failed",
				"ticket", ticket.ID, "report", row.ID, "err", err)
			continue
		}
		if _, err := m.Links.PutBoardLink(ctx, superagentstore.BoardLink{
			TenantID: m.TenantID, Source: BoardSource, SourceID: row.ID.String(),
			Kind: superagentstore.BoardLinkReport, BoardTaskID: task.ID,
		}); err != nil {
			m.Logger.Warn("superagent board mirror report link failed",
				"ticket", ticket.ID, "report", row.ID, "err", err)
			continue
		}
		report.Replies++
	}
	task, _ = m.Board.Task(ctx, m.TenantID, task.ID)

	// 5. Final ticket state. Terminal board states are append-only: a
	// completed/canceled task is never reopened by a later mirror pass. A
	// terminal Ticket with no claim (an archived import, or a ticket closed
	// without a worker) takes a short service claim first so the board fold
	// can accept the final event.
	switch ticket.Status {
	case collab.WorkItemDone:
		if task.State != StateCompleted {
			settled, claimed := m.ensureSettlementClaim(ctx, ticket, task, &report)
			task = settled
			if claimed {
				err := m.Board.Complete(ctx, m.TenantID, SettleRequest{
					TaskID: task.ID, Epoch: task.Epoch, Result: finalSummary(ticket, reports),
					Actor: claimActorOf(task),
				})
				m.accountSettle(err, "complete", ticket, task, &report, func() { report.Completed++ })
			}
		}
	case collab.WorkItemFailed:
		if task.State != StateFailed {
			settled, claimed := m.ensureSettlementClaim(ctx, ticket, task, &report)
			task = settled
			if claimed {
				err := m.Board.Fail(ctx, m.TenantID, SettleRequest{
					TaskID: task.ID, Epoch: task.Epoch, Reason: failureReason(ticket, reports, assignments),
					Actor: claimActorOf(task),
				})
				m.accountSettle(err, "fail", ticket, task, &report, func() { report.Failed++ })
			}
		}
	case collab.WorkItemCancelling, collab.WorkItemCancelled:
		if task.State != StateCanceled {
			reason := strings.TrimSpace(ticket.Mandate.RevocationCause)
			if err := m.Board.Cancel(ctx, m.TenantID, SettleRequest{
				TaskID: task.ID, Reason: reason, Actor: ServiceActor(m.TenantID),
			}); err != nil {
				m.Logger.Warn("superagent board mirror cancel failed", "ticket", ticket.ID, "err", err)
			} else {
				report.Canceled++
			}
		}
	case collab.WorkItemBlocked:
		// A timed-out/cancelled Attention's unblock is the supervisor's final
		// answer even when it leaves the Ticket blocked; do not re-block it.
		if openAttention == nil && !hasClosedAttention && task.State != StateBlocked && !boardFinished(task.State) {
			if err := m.Board.Block(ctx, m.TenantID, BlockRequest{
				TaskID: task.ID, Kind: HoldNeedsInput, Reason: "the ticket is blocked",
				Actor: ServiceActor(m.TenantID),
			}); err != nil {
				m.Logger.Warn("superagent board mirror ticket block failed", "ticket", ticket.ID, "err", err)
			} else {
				report.Blocked++
			}
		}
	}
	return report, nil
}

func (m *Writer) accountSettle(err error, action string, ticket superagent.Ticket, task boardplugin.TaskInfo, report *SyncReport, ok func()) {
	if err != nil {
		if errors.Is(err, boardplugin.ErrClaimConflict) || errors.Is(err, boardplugin.ErrNotClaimable) {
			report.Conflicts++
			m.logClaimConflict(ticket, task, err)
			return
		}
		m.Logger.Warn("superagent board mirror "+action+" failed", "ticket", ticket.ID, "err", err)
		return
	}
	ok()
}

// ensureSettlementClaim takes a short service claim when a terminal Ticket has
// no live claim, so complete/fail can append the final board state. It returns
// the task as the board now has it; claimed=false means this pass cannot settle
// (blocked, claimed by a conflicting actor, secret missing) and the next pass
// retries. The claim is service bookkeeping, not a supervisor assignment, so it
// is not counted as report.Claims.
func (m *Writer) ensureSettlementClaim(ctx context.Context, ticket superagent.Ticket, task boardplugin.TaskInfo, report *SyncReport) (boardplugin.TaskInfo, bool) {
	if task.State == StateClaimed {
		return task, true
	}
	if task.State != StateReady || m.Secret == nil {
		return task, false
	}
	token, err := m.Secret.SettlementToken(m.TenantID, ticket.ID)
	if err != nil {
		m.Logger.Warn("superagent board settlement token unavailable", "ticket", ticket.ID, "err", err)
		return task, false
	}
	info, claimed, err := m.Board.Claim(ctx, m.TenantID, ClaimRequest{
		TaskID: task.ID, Actor: ServiceActor(m.TenantID), Token: token, Lease: m.Lease,
	})
	if err != nil {
		if errors.Is(err, boardplugin.ErrClaimConflict) || errors.Is(err, boardplugin.ErrNotClaimable) {
			report.Conflicts++
			m.logClaimConflict(ticket, task, err)
		} else {
			m.Logger.Warn("superagent board settlement claim failed", "ticket", ticket.ID, "err", err)
		}
		return task, false
	}
	if !claimed || info.State != StateClaimed {
		return info, false
	}
	return info, true
}

// mirrorClaim returns true when the board task changed.
func (m *Writer) mirrorClaim(ctx context.Context, ticket superagent.Ticket, assignment collab.Assignment, task boardplugin.TaskInfo, report *SyncReport) bool {
	if assignment.SessionID == uuid.Nil {
		return false
	}
	actor := WorkerActor(assignment.BotID)
	if !validActorID(actor.ID) {
		actor = ServiceActor(m.TenantID)
	}
	token, err := m.Secret.ClaimToken(m.TenantID, assignment.ID, assignment.SessionID)
	if err != nil {
		m.Logger.Warn("superagent board mirror claim token unavailable", "ticket", ticket.ID, "err", err)
		return false
	}
	info, claimed, err := m.Board.Claim(ctx, m.TenantID, ClaimRequest{
		TaskID: task.ID, Actor: actor, Token: token, Lease: m.Lease,
	})
	if err != nil {
		if errors.Is(err, boardplugin.ErrClaimConflict) || errors.Is(err, boardplugin.ErrNotClaimable) {
			report.Conflicts++
			m.logClaimConflict(ticket, task, err)
			return false
		}
		m.Logger.Warn("superagent board mirror claim failed", "ticket", ticket.ID, "err", err)
		return false
	}
	if claimed {
		report.Claims++
	}
	epoch := info.Epoch
	if epoch == 0 {
		epoch = task.Epoch
	}
	if epoch != 0 {
		if err := m.Links.SetBoardLinkClaimEpoch(ctx, m.TenantID, BoardSource, ticket.ID.String(), superagentstore.BoardLinkTicket, epoch); err != nil && !errors.Is(err, superagentstore.ErrNotFound) {
			m.Logger.Warn("superagent board mirror claim epoch link failed", "ticket", ticket.ID, "err", err)
		}
	}
	if info.State == StateClaimed && info.ClaimActor == actor.ID {
		renewed, err := m.Board.Renew(ctx, m.TenantID, RenewRequest{
			TaskID: info.ID, Actor: actor, Epoch: info.Epoch,
			Lease: m.Lease, RenewBefore: m.RenewBefore,
		})
		if err != nil {
			if errors.Is(err, boardplugin.ErrClaimConflict) {
				report.Conflicts++
				m.logClaimConflict(ticket, info, err)
			}
		} else if renewed {
			report.Renewed++
		}
	}
	return claimed
}

func (m *Writer) mirrorAttentionReply(ctx context.Context, ticket superagent.Ticket, taskID uint64, attention collab.Attention, report *SyncReport) error {
	if attention.Status == collab.AttentionOpen {
		return nil
	}
	_, err := m.Board.Reply(ctx, m.TenantID, ReplyRequest{
		TaskID:     taskID,
		Text:       AttentionReplyText(attention),
		Labels:     ReplyLabels(ticket.ID),
		Actor:      ServiceActor(m.TenantID),
		RequestKey: "superagent:attention:" + attention.ID.String() + ":reply",
		Digest:     digestOf(struct{ Status, Option string }{string(attention.Status), answeredOption(attention)}),
	})
	if err != nil {
		return err
	}
	if _, err := m.Links.PutBoardLink(ctx, superagentstore.BoardLink{
		TenantID: m.TenantID, Source: BoardSource, SourceID: attention.ID.String(),
		Kind: superagentstore.BoardLinkAttention, BoardTaskID: taskID,
	}); err != nil {
		return err
	}
	report.Replies++
	return nil
}

func (m *Writer) recordGitHubEvent(ctx context.Context, ticket superagent.Ticket, taskID uint64) {
	source := strings.TrimSpace(ticket.Mandate.Source)
	threadSource := strings.TrimSpace(ticket.Thread.Source)
	if source != superagent.SourceGitHub && threadSource != superagent.SourceGitHub {
		return
	}
	eventID := strings.TrimSpace(ticket.IdempotencyKey)
	if eventID == "" {
		return
	}
	if _, err := m.Links.PutBoardLink(ctx, superagentstore.BoardLink{
		TenantID: m.TenantID, Source: superagent.SourceGitHub, SourceID: eventID,
		Kind: superagentstore.BoardLinkGitHub, BoardTaskID: taskID,
	}); err != nil {
		m.Logger.Warn("superagent board mirror github event link failed",
			"ticket", ticket.ID, "err", err)
	}
}

func (m *Writer) logClaimConflict(ticket superagent.Ticket, task boardplugin.TaskInfo, err error) {
	// No prompt/token/body: the ids and the holder are enough to investigate,
	// and the supervisor's own records remain the truth.
	m.Logger.Warn("superagent board mirror claim conflict; board claim left in place",
		"ticket", ticket.ID, "task", task.ID, "reason", err)
}

func accumulate(into *SyncReport, add SyncReport) {
	into.TasksCreated += add.TasksCreated
	into.Archived += add.Archived
	into.Claims += add.Claims
	into.Renewed += add.Renewed
	into.Released += add.Released
	into.Reopened += add.Reopened
	into.Blocked += add.Blocked
	into.Unblocked += add.Unblocked
	into.Replies += add.Replies
	into.Completed += add.Completed
	into.Failed += add.Failed
	into.Canceled += add.Canceled
	into.Skipped += add.Skipped
	into.Conflicts += add.Conflicts
}

func isTerminalTicket(ticket superagent.Ticket) bool {
	switch ticket.Status {
	case collab.WorkItemDone, collab.WorkItemFailed, collab.WorkItemCancelling, collab.WorkItemCancelled:
		return true
	default:
		return false
	}
}

func boardFinished(state string) bool {
	switch state {
	case StateCompleted, StateFailed, StateCanceled:
		return true
	default:
		return false
	}
}

func activeAssignmentOf(assignments []collab.Assignment) *collab.Assignment {
	// The newest active assignment is the worker the Ticket currently shows.
	// Open assignments without a session are dispatch bookkeeping, not a claim.
	var active *collab.Assignment
	sorted := append([]collab.Assignment(nil), assignments...)
	sort.Slice(sorted, func(i, j int) bool {
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
		}
		return sorted[i].ID.String() < sorted[j].ID.String()
	})
	for index := range sorted {
		assignment := sorted[index]
		if assignment.Status == collab.AssignmentActive && assignment.SessionID != uuid.Nil {
			active = &assignment
		}
	}
	return active
}

func oldestOpenAttention(attentions []collab.Attention) *collab.Attention {
	sorted := append([]collab.Attention(nil), attentions...)
	sort.Slice(sorted, func(i, j int) bool {
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
		}
		return sorted[i].ID.String() < sorted[j].ID.String()
	})
	for index := range sorted {
		if sorted[index].Status == collab.AttentionOpen {
			return &sorted[index]
		}
	}
	return nil
}

func claimActorOf(task boardplugin.TaskInfo) ActorInfo {
	if strings.TrimSpace(task.ClaimActor) == "" {
		return ActorInfo{}
	}
	return ActorInfo{
		ID: task.ClaimActor, Principal: task.ClaimActor,
		Kind: string(protocol.ActorAgent), Handle: task.ClaimActor, Name: task.ClaimActor,
	}
}

// ReportText is the board reply of one worker report: the state and the
// summary, bounded and sanitized. The full report stays in superagent.
func ReportText(report superagent.WorkerReport) string {
	state := strings.TrimSpace(string(report.State))
	summary := sanitizeText(strings.TrimSpace(report.Summary))
	if summary == "" {
		return state
	}
	if state == "" {
		return summary
	}
	return state + ": " + summary
}

// AttentionReplyText is the board reply of a closed Attention. It names the
// outcome and the chosen option only: a free-text answer can contain sensitive
// content, so it stays in superagent.
func AttentionReplyText(attention collab.Attention) string {
	switch attention.Status {
	case collab.AttentionAnswered:
		if option := answeredOption(attention); option != "" {
			return "Attention answered: " + option
		}
		return "Attention answered"
	case collab.AttentionTimedOut:
		return "Attention timed out"
	case collab.AttentionCancelled:
		return "Attention cancelled"
	default:
		return "Attention " + string(attention.Status)
	}
}

func answeredOption(attention collab.Attention) string {
	if attention.Answer == nil {
		return ""
	}
	return strings.TrimSpace(attention.Answer.OptionID)
}

func finalSummary(ticket superagent.Ticket, reports []superagent.WorkerReport) string {
	for index := len(reports) - 1; index >= 0; index-- {
		if reports[index].AppliedAt == nil {
			continue
		}
		if summary := strings.TrimSpace(reports[index].Summary); summary != "" {
			return summary
		}
	}
	return "completed"
}

func failureReason(ticket superagent.Ticket, reports []superagent.WorkerReport, assignments []collab.Assignment) string {
	for index := len(assignments) - 1; index >= 0; index-- {
		if reason := strings.TrimSpace(assignments[index].Error); reason != "" {
			return reason
		}
	}
	return finalSummary(ticket, reports)
}
