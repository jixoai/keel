package tasklog

// The panel's task-tree read: one session's cards, the member cards linked to
// them, the parents those cards point at outside the session, the attachment
// references their events record, and the session's task event stream. It is a
// read-only fold over the projection tables — it creates no truth and opens no
// write path (spec: 任务树是投影，跨会话可拼接; 任务树与面板 MUST 只读该投影).
//
// Ordering is stable so two reads of the same state render identically:
//   - cards: ledger creation sequence, then task id (a deterministic id when
//     two cards were created in the same commit);
//   - members: newest activity first, then task id (the projection's own keyset
//     order);
//   - events: ledger sequence.
//
// The panel draws the tree from `parentTaskId`; a card whose parent lives in
// another session is reported in `Ancestors` so the tree can still name it
// without copying that session's ledger.

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
)

// Panel bounds. A panel read is a page, not an export: every list it carries
// has a bound and a truncation fact, and the projector never decodes a row the
// page did not include.
const (
	DefaultPanelCardLimit       = 1000
	MaxPanelCardLimit           = 5000
	DefaultPanelMemberLimit     = 200
	MaxPanelMemberLimit         = 500
	DefaultPanelEventLimit      = 500
	MaxPanelEventLimit          = 2000
	DefaultPanelAttachmentLimit = 1000
	MaxPanelAttachmentLimit     = 5000
	DefaultPanelAncestorLimit   = 50
	MaxPanelAncestorLimit       = 200
)

// PanelAttachment is one live (path, hash) reference a card carries.
type PanelAttachment struct {
	TaskID uuid.UUID `json:"taskId"`
	Path   string    `json:"path"`
	Hash   string    `json:"hash"`
}

// Panel is one session's task memory as a read-only surface sees it.
type Panel struct {
	TenantID  uuid.UUID `json:"-"`
	SessionID uuid.UUID `json:"sessionId"`
	Cards     []Card    `json:"cards"`
	Members   []Card    `json:"members"`
	// Ancestors holds cards this session's cards point at by parentTaskId while
	// the ancestors live elsewhere (a fork origin, a sub-agent's spawning card).
	Ancestors []Card `json:"ancestors"`
	// Attachments folds each own card's live task.attached/task.detached events.
	Attachments []PanelAttachment `json:"attachments"`
	// Events is the newest page of the session's stream, in ledger order.
	Events []DescribedEvent `json:"events"`
	// Bounds and truncation, so a surface says "more exist" instead of
	// dropping rows silently.
	CardLimit      int  `json:"cardLimit"`
	CardsTruncated bool `json:"cardsTruncated"`
	MemberLimit    int  `json:"memberLimit"`
	Truncated      bool `json:"truncated"`
	// TruncatedMemberCount is how many linked cards the page left out: exact
	// when the over-fetch read every linked row, a floor of at least 1 when the
	// read itself stopped at its bound (it cannot count rows it never read).
	TruncatedMemberCount int  `json:"truncatedMemberCount,omitempty"`
	EventLimit           int  `json:"eventLimit"`
	EventsTruncated      bool `json:"eventsTruncated"`
	// AttachmentLimit bounds the attach/detach history the live set is folded
	// from; AttachmentsTruncated says an older reference may not be listed.
	AttachmentLimit      int  `json:"attachmentLimit"`
	AttachmentsTruncated bool `json:"attachmentsTruncated"`
	AncestorsTruncated   bool `json:"ancestorsTruncated"`
}

// Panel reads one session's task memory. memberLimit and eventLimit <= 0 take
// the defaults; a value above the maximum is clamped to it.
func (s *Service) Panel(ctx context.Context, scope kernel.Scope, memberLimit, eventLimit int) (Panel, error) {
	// The panel is a projection read, so only the projection has to be wired:
	// a service built without a ledger still answers its read, and this method
	// cannot reach the writer even by accident (the narrow guard is the
	// structural half of "面板只读").
	if s.projection == nil {
		return Panel{}, ErrNoBackend
	}
	if scope.IsZero() {
		return Panel{}, fmt.Errorf("%w: tenant and session are required", ErrTaskRequired)
	}
	panel := Panel{
		TenantID:    scope.TenantID,
		SessionID:   scope.SessionID,
		Cards:       []Card{},
		Members:     []Card{},
		Ancestors:   []Card{},
		Attachments: []PanelAttachment{},
		Events:      []DescribedEvent{},
	}
	panel.CardLimit = DefaultPanelCardLimit
	panel.MemberLimit = memberLimit
	if panel.MemberLimit <= 0 {
		panel.MemberLimit = DefaultPanelMemberLimit
	}
	if panel.MemberLimit > MaxPanelMemberLimit {
		panel.MemberLimit = MaxPanelMemberLimit
	}
	panel.EventLimit = eventLimit
	if panel.EventLimit <= 0 {
		panel.EventLimit = DefaultPanelEventLimit
	}
	if panel.EventLimit > MaxPanelEventLimit {
		panel.EventLimit = MaxPanelEventLimit
	}
	panel.AttachmentLimit = DefaultPanelAttachmentLimit

	// The session's own cards, bounded in creation order: the root card is the
	// session's first, so a pathological card count truncates the newest tail
	// (and says so) instead of dropping the tree's top.
	cards, err := s.projection.ListPanelCards(ctx, scope.TenantID, scope.SessionID, panel.CardLimit+1)
	if err != nil {
		return Panel{}, err
	}
	if len(cards) > panel.CardLimit {
		panel.CardsTruncated = true
		cards = cards[:panel.CardLimit]
	}
	sortCardsForTree(cards)
	panel.Cards = cards

	// The newest page of the session's task event stream. The read stops at
	// eventLimit+1 (one sentinel row proving more history exists); only the
	// page itself is decoded, so a corrupt older row outside the page cannot
	// fail the panel.
	events, err := s.projection.ListSessionEvents(ctx, scope.TenantID, scope.SessionID, panel.EventLimit+1)
	if err != nil {
		return Panel{}, err
	}
	sortRawEventsBySeq(events)
	if len(events) > panel.EventLimit {
		// Keep the newest page: a panel reader wants the latest fact, and the
		// gap sentence says the older ones were not read.
		panel.EventsTruncated = true
		events = events[len(events)-panel.EventLimit:]
	}
	described := make([]DescribedEvent, 0, len(events))
	for _, event := range events {
		view, err := DescribeEvent(event)
		if err != nil {
			return Panel{}, fmt.Errorf("tasklog: describe %s at seq %d: %w", event.Kind, event.Seq, err)
		}
		described = append(described, view)
	}
	panel.Events = described

	// Live attachment set per own card, folded from a separate bounded page of
	// attach/detach events. A detached path leaves the set; its history stays
	// readable in the event stream above. The page is the newest history, so a
	// reference whose newest event fell outside it is omitted (never invented),
	// and AttachmentsTruncated says so.
	attachmentEvents, err := s.projection.ListSessionAttachmentEvents(ctx, scope.TenantID, scope.SessionID, panel.AttachmentLimit+1)
	if err != nil {
		return Panel{}, err
	}
	sortRawEventsBySeq(attachmentEvents)
	if len(attachmentEvents) > panel.AttachmentLimit {
		panel.AttachmentsTruncated = true
		attachmentEvents = attachmentEvents[len(attachmentEvents)-panel.AttachmentLimit:]
	}
	eventsByTask := map[uuid.UUID][]Event{}
	for _, event := range attachmentEvents {
		eventsByTask[event.TaskID] = append(eventsByTask[event.TaskID], event)
	}
	for _, card := range panel.Cards {
		attachments, foldErr := attachmentsFromEvents(eventsByTask[card.TaskID])
		if foldErr != nil {
			// A row inside the attachment page that violates its kind's contract
			// is corruption, exactly like a corrupt event in the stream page:
			// fail the read instead of presenting a silently short live set.
			return Panel{}, foldErr
		}
		for _, attachment := range attachments {
			panel.Attachments = append(panel.Attachments, PanelAttachment{
				TaskID: card.TaskID,
				Path:   attachment.Path,
				Hash:   attachment.Hash,
			})
		}
	}
	sort.Slice(panel.Attachments, func(i, j int) bool {
		left, right := panel.Attachments[i], panel.Attachments[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.TaskID != right.TaskID {
			return left.TaskID.String() < right.TaskID.String()
		}
		return left.Hash < right.Hash
	})

	// Member cards linked to any of this session's cards. Their rows live in
	// their own sessions; only the card row travels, never the transcript.
	//
	// The read over-fetches by the number of own cards: the projection's join
	// returns every card whose parent is a card of this session, which includes
	// this session's own child cards. Those are own cards, not members, and
	// they must not consume a member slot — the page is filled after that
	// filter. The over-fetch is bounded and exact: at most one own card can
	// appear per own card, so `limit+own+1` either fills the page or proves the
	// linked set ended.
	known := map[uuid.UUID]bool{}
	for _, card := range panel.Cards {
		known[card.TaskID] = true
	}
	read := panel.MemberLimit + len(panel.Cards) + 1
	if read > MaxPanelMemberLimit {
		read = MaxPanelMemberLimit
	}
	childPage, err := s.projection.ListChildCards(ctx, scope.TenantID, scope.SessionID, read, nil, nil)
	if err != nil {
		return Panel{}, err
	}
	sortMembersForTree(childPage)
	overflow := 0
	for _, card := range childPage {
		if known[card.TaskID] {
			continue
		}
		if len(panel.Members) >= panel.MemberLimit {
			overflow++
			continue
		}
		panel.Members = append(panel.Members, card)
	}
	// A read that stopped exactly at its bound cannot say how many rows follow;
	// one is the floor the page certifies. A row past the budget proves at least
	// that many.
	if len(childPage) == read || overflow > 0 {
		panel.Truncated = true
		panel.TruncatedMemberCount = max(overflow, 1)
	}

	// Parents that live outside this session: resolve their card rows so the
	// tree can name the fork origin or the spawning card. Bounded; a chain of
	// external parents is not walked (the card row carries its own parent id).
	ancestorBudget := DefaultPanelAncestorLimit
	for _, card := range append(append([]Card{}, panel.Cards...), panel.Members...) {
		if card.ParentTaskID == nil || known[*card.ParentTaskID] {
			continue
		}
		if ancestorBudget <= 0 {
			panel.AncestorsTruncated = true
			break
		}
		ancestorBudget--
		parent, err := s.projection.GetCardByID(ctx, scope.TenantID, *card.ParentTaskID)
		if err != nil {
			// A dangling parent (a card removed with its session) leaves the
			// link visible as an id; the read never invents a card for it.
			continue
		}
		known[parent.TaskID] = true
		panel.Ancestors = append(panel.Ancestors, parent)
	}
	sortCardsForTree(panel.Ancestors)
	return panel, nil
}

// sortCardsForTree gives the panel a stable order: ledger creation sequence,
// then task id (a deterministic tiebreaker when two cards share a commit).
func sortCardsForTree(cards []Card) {
	sort.Slice(cards, func(i, j int) bool {
		left, right := cards[i], cards[j]
		if left.CreatedSeq != right.CreatedSeq {
			return left.CreatedSeq < right.CreatedSeq
		}
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.Before(right.CreatedAt)
		}
		return left.TaskID.String() < right.TaskID.String()
	})
}

// sortMembersForTree keeps member cards in the projection's own order: newest
// activity first, then task id.
func sortMembersForTree(cards []Card) {
	sort.Slice(cards, func(i, j int) bool {
		left, right := cards[i], cards[j]
		if !left.UpdatedAt.Equal(right.UpdatedAt) {
			return left.UpdatedAt.After(right.UpdatedAt)
		}
		return left.TaskID.String() > right.TaskID.String()
	})
}

func sortRawEventsBySeq(events []Event) {
	sort.Slice(events, func(i, j int) bool {
		if events[i].Seq != events[j].Seq {
			return events[i].Seq < events[j].Seq
		}
		return events[i].TaskID.String() < events[j].TaskID.String()
	})
}

func sortEventsBySeq(events []DescribedEvent) {
	sort.Slice(events, func(i, j int) bool {
		if events[i].Seq != events[j].Seq {
			return events[i].Seq < events[j].Seq
		}
		return events[i].TaskID.String() < events[j].TaskID.String()
	})
}
