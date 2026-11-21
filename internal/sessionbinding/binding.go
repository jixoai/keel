// Package sessionbinding projects the execution binding of a committed
// user.message onto sessions.bound_agent_id / bound_team_id (change
// keel-agent-team-v1, P3 session reference protection).
//
// The event ledger is the only truth: the projection exists so the registry's
// delete/archive guard can answer "is a live session bound to this row?"
// without scanning every session's events. It is never read to run an attempt.
//
// The projection is a single choke point on purpose. Every product path that
// commits a user.message — POST /input, /retry, the edge event door, the MCP
// app bridge, triggers, sub-agent spawn and manual fire — calls Record with the
// payload it just committed and the receipt seq. A path that bypasses this
// package would leave a live child session invisible to the guard, which is
// exactly the defect this package prevents.
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
// seq is the committed event seq; deduped marks a replay whose payload must be
// read back from the ledger instead of trusted from the caller.
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
	agentID, teamID := parsed.AgentID, parsed.TeamID
	// The resolver's ruling mirrored: a fact naming both is a team fact.
	if teamID != "" {
		agentID = ""
	}
	if _, err := r.Store.SetSessionBinding(ctx, tenantID, sessionID, agentID, teamID, seq); err != nil {
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
