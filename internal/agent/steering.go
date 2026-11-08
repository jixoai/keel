package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

// guideFeed supplies the loop's steering messages from the durable log: user
// inputs committed with payload.delivery="guide" after this attempt loaded its
// transcript. The loop polls it at PI's three drain points, so a guide input
// reaches the model inside the running turn — after every sibling tool result
// of the turn has been committed — instead of waiting for the next run.
//
// The feed is deliberately stateless about who else might drain it: the log is
// the queue, and consumption is recorded where all other consumption is
// recorded — in the assistant message's inputSeqs. An input this feed hands out
// is marked locally so the same attempt cannot inject it twice; if the attempt
// dies before committing an answer, the input stays unanswered in the log and
// the next attempt presents it as a normal prompt (at-least-once delivery, the
// same rule at-least-once tools follow).
type guideFeed struct {
	runner *Runner
	lease  kernel.Lease
	// after is the load watermark of this attempt: everything at or below it is
	// already in the transcript, so steering must not inject it again (an input
	// committed while no attempt was live is consumed at the run boundary —
	// that is the queue behaviour, by construction).
	after  int64
	handed map[int64]bool
	// seqs is handed in order, so inputSeqs can record exactly what the model
	// saw without re-scanning the log.
	seqs []int64
}

func newGuideFeed(r *Runner, lease kernel.Lease, after int64) *guideFeed {
	return &guideFeed{runner: r, lease: lease, after: after, handed: make(map[int64]bool)}
}

// poll returns at most one guide message: PI's per-turn semantics inject one
// steering message per drain point, and one-at-a-time keeps the model's view of
// the turn stable. A poll error is not fatal — the run continues on the
// transcript it has (the input will be answered by the next attempt; the
// watermark forces the re-queue).
func (f *guideFeed) poll(ctx context.Context) []agentcore.AgentMessage {
	if ctx.Err() != nil {
		// A stop/cancel request is in flight: steering must not extend the run.
		return nil
	}
	events, err := f.load(ctx)
	if err != nil {
		f.runner.Logger.Warn("steering poll", "session_id", f.lease.Scope.SessionID, "err", err)
		return nil
	}
	for _, ev := range events {
		if ev.Kind != store.EventUserMessage || f.handed[ev.Seq] {
			continue
		}
		var payload struct {
			Text        string            `json:"text"`
			Delivery    string            `json:"delivery"`
			Attachments []json.RawMessage `json:"attachments"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			continue
		}
		if payload.Delivery != kernel.DeliveryGuide {
			continue
		}
		if len(payload.Attachments) > 0 {
			// Downgrade: an input with attachments belongs to the queue path
			// (the attachment pipeline runs at a run boundary, not mid-turn).
			// Leaving it untouched means the next attempt presents it.
			continue
		}
		f.handed[ev.Seq] = true
		f.seqs = append(f.seqs, ev.Seq)
		return []agentcore.AgentMessage{agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   agentcore.ContentList{agentcore.NewTextContent(payload.Text)},
		}}
	}
	return nil
}

// consumed returns the seqs this attempt has handed to the loop, in order. They
// join the inputSeqs fact of every assistant message committed after the hand
// out, so "unanswered input" stays exact for guide inputs too.
func (f *guideFeed) consumed() []int64 {
	return f.seqs
}

// load reads the committed events above the attempt's load watermark.
func (f *guideFeed) load(ctx context.Context) ([]store.Event, error) {
	var (
		all   []store.Event
		after = f.after
	)
	for {
		page, err := f.runner.Events.LoadEvents(ctx, f.lease.Scope, after, 200)
		if err != nil {
			return nil, fmt.Errorf("steering: load events: %w", err)
		}
		if len(page) == 0 {
			return all, nil
		}
		for _, ev := range page {
			all = append(all, store.Event{Seq: ev.Seq, Kind: string(ev.Kind), Payload: ev.Payload})
		}
		after = page[len(page)-1].Seq
		if len(page) < 200 {
			return all, nil
		}
	}
}

// mergeInputSeqs unions the attempt's transcript inputs with the guide seqs it
// consumed, ascending and deduplicated, so the assistant message records one
// consumption fact rather than two.
func mergeInputSeqs(base, extra []int64) []int64 {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[int64]bool, len(base)+len(extra))
	out := make([]int64, 0, len(base)+len(extra))
	for _, seq := range append(append([]int64{}, base...), extra...) {
		if seen[seq] {
			continue
		}
		seen[seq] = true
		out = append(out, seq)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
