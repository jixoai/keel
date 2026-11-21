// Package sessionbinding projects the execution binding of a committed
// user.message onto the session's ownership columns (sessions.bot_id /
// team_bot_id; change keel-bot-identity-access, A4.2b3a).
//
// A Session belongs to exactly one Bot: ownership is written at creation and a
// selection fact can never move it. The projection therefore verifies the
// selection against the ownership (a child session may name a member Bot for
// its prompt/tools, which is a surface-only binding) instead of overwriting it.
// The event ledger stays the truth; the columns are the durable fact every
// route and the runtime principal read.
//
// The projection is a single choke point on purpose. Every product path that
// commits a user.message — POST /input, /retry, the edge event door, the MCP
// app bridge, triggers, sub-agent spawn and manual fire — calls Record with the
// payload it just committed and the receipt seq.
package sessionbinding

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/modelcfg"
	"github.com/jixoai/keel/internal/store"
)

// Recorder writes the derived binding projection. A nil Recorder (or a nil
// Store) is a no-op: tests and deployments without the store keep their
// pre-projection behavior.
type Recorder struct {
	Store *store.Store
}

// New returns a recorder over st.
func New(st *store.Store) *Recorder { return &Recorder{Store: st} }

// Record projects the selection fact of one committed user.message payload.
// seq is the committed event seq (diagnostic: the projection is not a
// newest-wins index anymore); deduped marks a replay whose payload must be read
// back from the ledger instead of trusted from the caller.
func (r *Recorder) Record(ctx context.Context, tenantID, sessionID uuid.UUID,
	payload json.RawMessage, seq int64, deduped bool) error {
	if r == nil || r.Store == nil {
		return nil
	}
	// A deduped request did not write this event: the ledger's payload is the
	// truth, and the request body may differ (a retry after a crash normally
	// carries the same selection, but nothing enforces it). Reading the committed
	// event keeps the projection a function of the ledger, not of the request.
	if deduped {
		events, err := r.Store.LoadEvents(ctx, tenantID, sessionID, seq-1, 1)
		if err != nil {
			return fmt.Errorf("project session binding: read committed event: %w", err)
		}
		if len(events) == 0 {
			// The edge that deduped this request keeps its ledger somewhere this
			// store cannot read (a conformance seam with an in-memory kernel over a
			// real submissions store). The first commit already projected the fact;
			// guessing from the request body would be worse than leaving it alone.
			return nil
		}
		if events[0].Seq != seq {
			return fmt.Errorf("project session binding: committed event seq %d is not the deduped seq %d", events[0].Seq, seq)
		}
		payload = events[0].Payload
	}
	selection, present := Selection(payload)
	if !present {
		return nil
	}
	parsed, err := modelcfg.ParseSelection(selection)
	if err != nil {
		return fmt.Errorf("project session binding: %w", err)
	}
	botID, teamBotID := strings.TrimSpace(parsed.BotID), strings.TrimSpace(parsed.TeamBotID)
	// The resolver's ruling mirrored: a fact naming both is a team fact.
	if teamBotID != "" {
		botID = ""
	}
	if _, err := r.Store.SetSessionBinding(ctx, tenantID, sessionID, botID, teamBotID); err != nil {
		return fmt.Errorf("project session binding: %w", err)
	}
	return nil
}

// Selection extracts the selection field of a committed user.message payload.
// present=false covers an absent field, the literal null and a payload that is
// not an object; the kernel owns payload shape, so a malformed payload is not
// this helper's refusal.
func Selection(payload json.RawMessage) (json.RawMessage, bool) {
	var probe struct {
		Selection json.RawMessage `json:"selection"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return nil, false
	}
	trimmed := strings.TrimSpace(string(probe.Selection))
	if trimmed == "" || trimmed == "null" {
		return nil, false
	}
	return probe.Selection, true
}
