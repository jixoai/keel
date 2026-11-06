package durable

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jixoai/keel/internal/agentcore"
)

// SubmissionDraft is a host submission: user input, a passive entry write, or a
// continue (a run without a new user message).
type SubmissionDraft struct {
	RequestID string
	Type      string
	// Input
	Content  json.RawMessage
	Entry    *InputEntry
	WhenBusy string // steer | followUp | reject
	WhenIdle string // reject
	// Write
	Write *EntryDraft
}

// resetDraft builds the pi.reset write.
func resetDraft(handoff string, ts int64) *EntryDraft {
	d := &EntryDraft{Kind: "pi.reset", Head: "self"}
	if handoff != "" {
		d.Model = []agentcore.Message{agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   agentcore.ContentList{agentcore.NewTextContent(handoff)},
			Timestamp: ts,
		}}
	}
	return d
}

func checkInputEntry(entry *InputEntry) error {
	if entry == nil {
		return nil
	}
	if entry.Kind == "" || (len(entry.Kind) >= 3 && entry.Kind[:3] == "pi.") {
		return fmt.Errorf("An input entry's kind must be a non-empty string not starting with \"pi.\", got %q", entry.Kind)
	}
	return nil
}

// admit runs the submission state machine in one critical section.
func (h *Harness) admit(ctx context.Context, conv ConversationID, draft SubmissionDraft) (SubmissionRecord, error) {
	if draft.Type == "" {
		draft.Type = SubmissionInput
	}
	if draft.Type == SubmissionInput {
		if err := checkInputEntry(draft.Entry); err != nil {
			return SubmissionRecord{}, err
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.checkAliveLocked(); err != nil {
		return SubmissionRecord{}, err
	}
	if draft.RequestID != "" {
		if existing, ok := h.store.FindSubmissionByRequest(conv, draft.RequestID); ok {
			if existing.Type != draft.Type {
				return SubmissionRecord{}, fmt.Errorf("Request %s already identifies a submission of type %s", draft.RequestID, existing.Type)
			}
			return existing, nil
		}
	}
	fields := SubmissionRecord{
		ConversationID: conv,
		Type:           draft.Type,
		RequestID:      draft.RequestID,
	}

	// A display-only write is appended at once, busy or not, unless a queued
	// reset must land before it.
	inbox := h.loadInbox(conv)
	if draft.Type == SubmissionWrite && draft.Write != nil && draft.Write.DisplayOnly() && !hasQueuedReset(inbox) {
		entry := h.appendEntryLocked(conv, *draft.Write)
		fields.Status = SubmissionDone
		fields.EntryID = entry.ID
		rec := h.store.CreateSubmission(fields)
		h.notifyLocked()
		return rec, nil
	}

	live := h.loadLive(conv)
	busy := live.Run != nil

	if draft.Type == SubmissionInput && busy && draft.WhenBusy == "reject" {
		return SubmissionRecord{}, &ConversationBusy{ConversationID: conv}
	}
	if draft.Type == SubmissionInput && !busy && draft.WhenIdle == "reject" {
		return SubmissionRecord{}, &ConversationIdle{ConversationID: conv}
	}

	if draft.Type == SubmissionContinue {
		if busy {
			return SubmissionRecord{}, &ConversationBusy{ConversationID: conv}
		}
		res := h.applyBoundaryLocked(conv, inbox, "final")
		fields.Status = SubmissionPlaced
		rec := h.store.CreateSubmission(fields)
		inputs := append(append([]SubmissionID(nil), res.Users...), rec.ID)
		first := res.First
		if live.RunFailure != nil && live.RunFailure.First != 0 {
			first = live.RunFailure.First
		}
		h.startRunLocked(conv, emptyJSON(), inputs, first, false)
		h.notifyLocked()
		return rec, nil
	}

	// Busy, or an idle conversation with items already queued: queue this item,
	// then place a final boundary when idle.
	if busy || len(inbox) > 0 {
		fields.Status = SubmissionQueued
		if draft.Type == SubmissionWrite {
			fields.Write = draft.Write
		} else {
			fields.Content = draft.Content
			fields.Entry = draft.Entry
		}
		rec := h.store.CreateSubmission(fields)
		item := InboxItem{ID: rec.ID}
		if draft.Type == SubmissionWrite {
			item.Mode = "write"
			item.Write = draft.Write
		} else {
			item.Mode = "followUp"
			if draft.WhenBusy == "steer" {
				item.Mode = "steer"
			}
			item.Content = draft.Content
			item.Entry = draft.Entry
		}
		inbox = append(inbox, item)
		if !busy {
			res := h.applyBoundaryLocked(conv, inbox, "final")
			if len(res.Users) > 0 {
				h.startRunLocked(conv, emptyJSON(), res.Users, res.First, false)
			}
			// Re-read the record: the boundary may have placed it.
			if placed, ok := h.store.GetSubmission(rec.ID); ok {
				h.notifyLocked()
				return placed, nil
			}
		} else {
			h.saveInbox(conv, inbox)
		}
		h.notifyLocked()
		return rec, nil
	}

	if draft.Type == SubmissionWrite {
		entry := h.appendEntryLocked(conv, *draft.Write)
		fields.Status = SubmissionDone
		fields.EntryID = entry.ID
		rec := h.store.CreateSubmission(fields)
		h.notifyLocked()
		return rec, nil
	}

	// Idle input: place its entry and start a run.
	entry := h.appendInputLocked(conv, draft.Content, draft.Entry)
	fields.Status = SubmissionPlaced
	fields.EntryID = entry.ID
	fields.Content = draft.Content
	fields.Entry = draft.Entry
	rec := h.store.CreateSubmission(fields)
	h.startRunLocked(conv, emptyJSON(), []SubmissionID{rec.ID}, entry.ID, false)
	h.notifyLocked()
	return rec, nil
}

func emptyJSON() json.RawMessage { return json.RawMessage(`{}`) }

func hasQueuedReset(items []InboxItem) bool {
	for _, item := range items {
		if item.Mode == "write" && item.Write != nil && item.Write.IsReset() {
			return true
		}
	}
	return false
}

func (h *Harness) appendEntryLocked(conv ConversationID, draft EntryDraft) EntryRecord {
	rec := EntryRecord{
		Kind:  draft.Kind,
		Model: cloneMessages(draft.Model),
		Data:  cloneRaw(draft.Data),
		Head:  draft.Head == "self",
		Edits: append([]ContextEdit(nil), draft.Edits...),
	}
	return h.store.AppendEntry(conv, rec)
}

func (h *Harness) appendInputLocked(conv ConversationID, content json.RawMessage, entry *InputEntry) EntryRecord {
	draft := EntryDraft{Model: contentMessages(content, h.now())}
	if entry != nil {
		draft.Kind = entry.Kind
		draft.Data = cloneRaw(entry.Data)
	} else {
		draft.Kind = "pi.user"
	}
	return h.appendEntryLocked(conv, draft)
}

type boundaryResult struct {
	Users []SubmissionID
	First EntryID
	Reset bool
}

// applyBoundaryLocked places the queued items the boundary selects, writes
// first and user items after them, each in ID order.
func (h *Harness) applyBoundaryLocked(conv ConversationID, items []InboxItem, at string) boundaryResult {
	reset := hasQueuedReset(items)
	final := at == "final" || reset
	pick := func(mode, queueMode string, indexes []int) []int {
		var matching []int
		for _, index := range indexes {
			if items[index].Mode == mode {
				matching = append(matching, index)
			}
		}
		if queueMode == QueueAll {
			return matching
		}
		if len(matching) > 0 {
			return matching[:1]
		}
		return nil
	}
	var indexes []int
	for i := range items {
		indexes = append(indexes, i)
	}
	var writes []int
	for i := range items {
		if items[i].Mode == "write" {
			writes = append(writes, i)
		}
	}
	users := pick("steer", h.settings.SteeringMode, indexes)
	if final {
		users = append(users, pick("followUp", h.settings.FollowUpMode, indexes)...)
	}
	// Keep call/queue order (writes are placed first, but user items are
	// selected in their inbox order among themselves).
	sortInts(users)

	var result boundaryResult
	remove := map[int]bool{}
	for _, index := range writes {
		item := items[index]
		entry := h.appendEntryLocked(conv, *item.Write)
		h.placeSubmissionLocked(item.ID, entry.ID)
		if result.First == 0 {
			result.First = entry.ID
		}
		remove[index] = true
	}
	for _, index := range users {
		item := items[index]
		entry := h.appendInputLocked(conv, item.Content, item.Entry)
		h.placeSubmissionLocked(item.ID, entry.ID)
		result.Users = append(result.Users, item.ID)
		if result.First == 0 {
			result.First = entry.ID
		}
		remove[index] = true
	}
	kept := make([]InboxItem, 0, len(items))
	for i, item := range items {
		if !remove[i] {
			kept = append(kept, item)
		}
	}
	h.saveInbox(conv, kept)
	result.Reset = reset
	return result
}

func sortInts(v []int) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

func (h *Harness) placeSubmissionLocked(id SubmissionID, entry EntryID) {
	_, _ = h.store.UpdateSubmission(id, func(rec *SubmissionRecord) {
		// A placed input starts a run; a placed write is settled by its entry.
		if rec.Type == SubmissionWrite {
			rec.Status = SubmissionDone
		} else {
			rec.Status = SubmissionPlaced
		}
		rec.EntryID = entry
	})
}

func (h *Harness) settleSubmissionLocked(id SubmissionID, status, reason, detail string, entry, answer EntryID) {
	_, _ = h.store.UpdateSubmission(id, func(rec *SubmissionRecord) {
		// First settlement wins: a later one leaves the stored outcome.
		if rec.Settled() {
			return
		}
		rec.Status = status
		if reason != "" {
			rec.Reason = reason
		}
		if detail != "" {
			rec.Detail = detail
		}
		if entry != 0 {
			rec.EntryID = entry
		}
		if answer != 0 {
			rec.AnswerID = answer
		}
	})
	h.notifyLocked()
}

// abortSubmission withdraws a queued submission.
func (h *Harness) abortSubmission(ctx context.Context, id SubmissionID, conv ConversationID) (AbortResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.checkAliveLocked(); err != nil {
		return "", err
	}
	rec, ok := h.store.GetSubmission(id)
	if !ok || (conv != "" && rec.ConversationID != conv) {
		return AbortNotFound, nil
	}
	switch rec.Status {
	case SubmissionQueued:
		h.settleSubmissionLocked(id, SubmissionUnanswered, ReasonAborted, "", 0, 0)
		items := h.loadInbox(rec.ConversationID)
		kept := items[:0]
		for _, item := range items {
			if item.ID != id {
				kept = append(kept, item)
			}
		}
		h.saveInbox(rec.ConversationID, kept)
		return AbortAborted, nil
	case SubmissionPlaced:
		return AbortAlreadyPlaced, nil
	default:
		return AbortSettled, nil
	}
}

// abortConversation withdraws queued inputs, aborts the live run and waits for
// the conversation to become idle.
func (h *Harness) abortConversation(ctx context.Context, conv ConversationID) error {
	h.mu.Lock()
	if err := h.checkAliveLocked(); err != nil {
		h.mu.Unlock()
		return err
	}
	items := h.loadInbox(conv)
	kept := items[:0]
	for _, item := range items {
		if item.Mode == "write" {
			kept = append(kept, item)
			continue
		}
		h.settleSubmissionLocked(item.ID, SubmissionUnanswered, ReasonAborted, "", 0, 0)
	}
	h.saveInbox(conv, kept)
	live := h.loadLive(conv)
	if live.Run != nil {
		if cancel, ok := h.running[live.Run.TaskID]; ok {
			cancel()
		}
	}
	h.notifyLocked()
	h.mu.Unlock()

	if live.Run == nil {
		return nil
	}
	for {
		ch := h.notifyCh.current()
		if h.Closed() {
			return ErrHarnessClosed{}
		}
		cur := h.loadLive(conv)
		if cur.Run == nil {
			return nil
		}
		if err := waitChan(ctx, ch); err != nil {
			return err
		}
	}
}

// withdrawQueuedInputsLocked settles every queued input and removes it; queued
// writes stay for later placement.
func (h *Harness) withdrawQueuedInputsLocked(conv ConversationID, reason string) {
	items := h.loadInbox(conv)
	kept := items[:0]
	for _, item := range items {
		if item.Mode == "write" {
			kept = append(kept, item)
			continue
		}
		h.settleSubmissionLocked(item.ID, SubmissionUnanswered, reason, "", 0, 0)
	}
	h.saveInbox(conv, kept)
}

// waitTask blocks until a task is terminal.
func (h *Harness) waitTask(ctx context.Context, id TaskID) (TaskRecord, error) {
	for {
		ch := h.notifyCh.current()
		rec, ok := h.store.GetTask(id)
		if !ok {
			return TaskRecord{}, fmt.Errorf("durable: task %s does not exist", id)
		}
		if rec.Terminal() {
			return rec, nil
		}
		if h.Closed() && !h.isDead() {
			return rec, ErrHarnessClosed{}
		}
		if err := waitChan(ctx, ch); err != nil {
			return rec, err
		}
	}
}
