package superagent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
)

// OutboxStatus is the durable lifecycle of one outbound Channel message.
type OutboxStatus string

const (
	OutboxPending OutboxStatus = "pending"
	OutboxSending OutboxStatus = "sending"
	OutboxSent    OutboxStatus = "sent"
	OutboxFailed  OutboxStatus = "failed"
)

// Terminal reports whether the message will not be sent again.
func (s OutboxStatus) Terminal() bool { return s == OutboxSent || s == OutboxFailed }

// OutboxMessage is one durable Channel write. A message is persisted before it
// is sent, ordered by Ticket/thread, and retried with backoff. IdempotencyKey
// makes enqueueing the same logical message return the same row, so a retried
// acceptance/completion/attention cannot produce a duplicate notification.
type OutboxMessage struct {
	ID             uuid.UUID       `json:"id"`
	TenantID       uuid.UUID       `json:"tenantId"`
	WorkItemID     uuid.UUID       `json:"workItemId,omitempty"`
	AttentionID    uuid.UUID       `json:"attentionId,omitempty"`
	ChannelID      string          `json:"channelId"`
	Thread         ThreadRef       `json:"thread,omitempty"`
	Text           string          `json:"text"`
	Data           json.RawMessage `json:"data,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	Status         OutboxStatus    `json:"status"`
	Attempts       int             `json:"attempts"`
	NextAttemptAt  time.Time       `json:"nextAttemptAt"`
	LastError      string          `json:"lastError,omitempty"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
}

// OutboxOptions configures the retry policy and Channel fan-out.
type OutboxOptions struct {
	Store       Store
	Channel     collab.Channel
	Now         func() time.Time
	MaxAttempts int
	// Backoff returns the delay before attempt N+1. Nil uses an exponential
	// 250ms * 2^attempt capped at 30s.
	Backoff func(attempt int) time.Duration
	// Audit optionally records sent/failed messages.
	Audit collab.AuditSink
}

// Outbox is the durable Channel writer. It is also the AttentionPublisher the
// guarded Host uses, so a structured question reaches the same Ticket thread.
type Outbox struct {
	store       Store
	channel     collab.Channel
	now         func() time.Time
	maxAttempts int
	backoff     func(attempt int) time.Duration
	audit       collab.AuditSink
}

var _ collab.AttentionPublisher = (*Outbox)(nil)

// NewOutbox builds the writer. A nil channel keeps messages durable but makes
// Flush a no-op, which is the honest state of a deployment without a Channel.
func NewOutbox(opts OutboxOptions) (*Outbox, error) {
	if opts.Store == nil {
		return nil, fmt.Errorf("superagent: outbox store is required")
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 5
	}
	if opts.Backoff == nil {
		opts.Backoff = func(attempt int) time.Duration {
			if attempt < 1 {
				attempt = 1
			}
			delay := 250 * time.Millisecond
			for i := 1; i < attempt; i++ {
				delay *= 2
				if delay >= 30*time.Second {
					return 30 * time.Second
				}
			}
			return delay
		}
	}
	return &Outbox{
		store:       opts.Store,
		channel:     opts.Channel,
		now:         opts.Now,
		maxAttempts: opts.MaxAttempts,
		backoff:     opts.Backoff,
		audit:       opts.Audit,
	}, nil
}

// Enqueue persists one message. A repeated IdempotencyKey returns the existing
// row (including a terminal one): an already-sent or already-failed message is
// never re-enqueued.
func (o *Outbox) Enqueue(ctx context.Context, message OutboxMessage) (OutboxMessage, error) {
	if message.TenantID == uuid.Nil {
		return OutboxMessage{}, fmt.Errorf("%w: tenant is required", collab.ErrTenantRequired)
	}
	if message.ChannelID == "" {
		return OutboxMessage{}, fmt.Errorf("superagent: channel id is required")
	}
	if message.Text == "" {
		return OutboxMessage{}, fmt.Errorf("superagent: message text is required")
	}
	if message.Status == "" {
		message.Status = OutboxPending
	}
	if message.CreatedAt.IsZero() {
		message.CreatedAt = o.now()
	}
	if message.NextAttemptAt.IsZero() {
		message.NextAttemptAt = o.now()
	}
	return o.store.EnqueueOutbox(ctx, message)
}

// Publish implements collab.AttentionPublisher: a question is routed back to
// the thread that produced the Ticket, not to an unrelated Channel. Attention
// requests without a channel are left readable in the store but not enqueued
// (the Host's human/session routes are delivered by their own seam).
func (o *Outbox) Publish(ctx context.Context, attention collab.Attention) error {
	if attention.Route != collab.AttentionRouteChannel || attention.Channel == "" {
		return nil
	}
	_, err := o.Enqueue(ctx, OutboxMessage{
		TenantID:       attention.TenantID,
		WorkItemID:     attention.WorkItemID,
		AttentionID:    attention.ID,
		ChannelID:      attention.Channel,
		Thread:         ThreadRef{ID: attention.Channel},
		Text:           attention.Prompt,
		Data:           marshalData(map[string]any{"attentionId": attention.ID.String(), "options": attention.Options}),
		IdempotencyKey: "attention:" + attention.ID.String(),
	})
	if err != nil {
		return err
	}
	return nil
}

// Flush sends due pending messages in creation order. It is bounded by limit
// and returns the number of messages sent. A Channel failure leaves the row
// pending with an incremented attempt and a backoff; an exhausted row becomes
// failed and stays visible (never silently dropped).
func (o *Outbox) Flush(ctx context.Context, tenantID uuid.UUID, limit int) (int, error) {
	if o.channel == nil {
		return 0, nil
	}
	if limit <= 0 {
		limit = 50
	}
	messages, err := o.store.ListOutbox(ctx, tenantID, OutboxPending, limit)
	if err != nil {
		return 0, err
	}
	now := o.now()
	sent := 0
	for _, message := range messages {
		if message.NextAttemptAt.After(now) {
			continue
		}
		// Mark sending first so a crash leaves a visible attempt. A retry
		// treats a stale "sending" row as pending through the store's ordering
		// (ListOutbox pending only), so the row is re-claimed by RecoverOutbox.
		message.Status = OutboxSending
		message.Attempts++
		message.UpdatedAt = now
		if _, err := o.store.UpdateOutbox(ctx, message); err != nil {
			return sent, err
		}
		data := message.Data
		if len(data) == 0 {
			data = marshalData(map[string]any{"messageId": message.ID.String()})
		} else {
			var object map[string]any
			if err := json.Unmarshal(data, &object); err == nil && object != nil {
				if _, ok := object["messageId"]; !ok {
					object["messageId"] = message.ID.String()
					data = marshalData(object)
				}
			}
		}
		err := o.channel.Send(ctx, tenantID, message.ChannelID, collab.ChannelMessage{
			WorkItemID:  message.WorkItemID,
			AttentionID: message.AttentionID,
			Text:        message.Text,
			Data:        data,
			ThreadID:    message.Thread.ID,
		})
		if err != nil {
			message.Status = OutboxPending
			message.LastError = err.Error()
			message.NextAttemptAt = now.Add(o.backoff(message.Attempts))
			if message.Attempts >= o.maxAttempts {
				message.Status = OutboxFailed
			}
			message.UpdatedAt = now
			if _, updateErr := o.store.UpdateOutbox(ctx, message); updateErr != nil {
				return sent, updateErr
			}
			o.record(ctx, message, "failed", err.Error())
			continue
		}
		message.Status = OutboxSent
		message.LastError = ""
		message.UpdatedAt = now
		if _, err := o.store.UpdateOutbox(ctx, message); err != nil {
			return sent, err
		}
		o.record(ctx, message, "accepted", "")
		sent++
	}
	return sent, nil
}

// RecoverOutbox re-queues rows left in "sending" by a crash. A sending row has
// an unknown outcome; it is returned to pending with an attempt already
// counted, so the maximum-attempt bound still applies and no terminal row is
// duplicated.
func (o *Outbox) RecoverOutbox(ctx context.Context, tenantID uuid.UUID, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	messages, err := o.store.ListOutbox(ctx, tenantID, OutboxSending, limit)
	if err != nil {
		return 0, err
	}
	for _, message := range messages {
		if message.Attempts >= o.maxAttempts {
			message.Status = OutboxFailed
			message.LastError = "delivery outcome unknown after restart"
		} else {
			message.Status = OutboxPending
			message.NextAttemptAt = o.now()
		}
		message.UpdatedAt = o.now()
		if _, err := o.store.UpdateOutbox(ctx, message); err != nil {
			return 0, err
		}
	}
	return len(messages), nil
}

func (o *Outbox) record(ctx context.Context, message OutboxMessage, result, detail string) {
	if o.audit == nil {
		return
	}
	kind := AuditOutboxSent
	if result == "failed" {
		kind = AuditOutboxFailed
	}
	_ = o.audit.Record(ctx, collab.AuditEvent{
		Kind:       kind,
		Result:     result,
		TenantID:   message.TenantID,
		WorkItemID: message.WorkItemID,
		ModeID:     DefaultModeID,
		Detail:     detail,
		At:         o.now(),
	})
}

func marshalData(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return raw
}
