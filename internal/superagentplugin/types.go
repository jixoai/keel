package superagentplugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/boardplugin"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"

	boarddomain "github.com/jixoai/keel-board/board"
)

// This file is the superagent side of the board vocabulary. The board plugin
// owns the substrate types and the service; these aliases/helpers project a
// ticket into ordinary board labels so no board store type crosses the
// boundary in the other direction.

// ActorInfo is the board identity a write carries.
type ActorInfo = boardplugin.ActorInfo

// TicketThread is the root write of one ticket.
type TicketThread = boardplugin.TicketThread

// Board service requests the writer builds.
type (
	ClaimRequest   = boardplugin.ClaimRequest
	RenewRequest   = boardplugin.RenewRequest
	ReleaseRequest = boardplugin.ReleaseRequest
	ReopenRequest  = boardplugin.ReopenRequest
	BlockRequest   = boardplugin.BlockRequest
	UnblockRequest = boardplugin.UnblockRequest
	SettleRequest  = boardplugin.SettleRequest
	AssignRequest  = boardplugin.AssignRequest
	ReplyRequest   = boardplugin.ReplyRequest
)

// ArchivedLabel marks a board task imported from an already terminal ticket.
const ArchivedLabel = "board.archived"

// Board task states and hold kinds the writer projects.
const (
	StateWaiting   = boardplugin.StateWaiting
	StateReady     = boardplugin.StateReady
	StateClaimed   = boardplugin.StateClaimed
	StateBlocked   = boardplugin.StateBlocked
	StateCompleted = boardplugin.StateCompleted
	StateFailed    = boardplugin.StateFailed
	StateCanceled  = boardplugin.StateCanceled

	HoldNeedsInput       = boardplugin.HoldNeedsInput
	HoldAwaitingApproval = boardplugin.HoldAwaitingApproval

	DefaultLease        = boardplugin.DefaultLease
	DefaultRenewBefore  = boardplugin.DefaultRenewBefore
	DefaultCommandLimit = boardplugin.DefaultCommandLimit
)

// ServiceActor is the stable board principal of the superagent plugin.
func ServiceActor(tenantID uuid.UUID) ActorInfo { return boardplugin.ServiceActor(tenantID) }

// WorkerActor is the board identity of a supervisor worker Bot.
func WorkerActor(botID string) ActorInfo { return boardplugin.WorkerActor(botID) }

// BoardText bounds a ticket title/request to the board's message limit.
func BoardText(title, request string) string { return boardplugin.BoardText(title, request) }

// BoardNote bounds a result/reason to the board's note limit.
func BoardNote(value, fallback string) string { return boardplugin.BoardNote(value, fallback) }

// TicketLabels is the label set of one projected ticket root.
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

// ReplyLabels labels a projected report/attention reply.
func ReplyLabels(ticketID uuid.UUID) map[string]string {
	return map[string]string{"source": BoardSource, "ticket": ticketID.String()}
}

// attentionHoldKind maps an Attention kind to the board hold kind.
func attentionHoldKind(kind string) string {
	switch strings.TrimSpace(kind) {
	case collab.AttentionApproval, collab.AttentionDecision:
		return HoldAwaitingApproval
	default:
		return HoldNeedsInput
	}
}

func digestOf(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		encoded = []byte(fmt.Sprintf("%v", value))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func validActorID(id string) bool { return boarddomain.ValidActorID(id) }

// sanitizeText removes NUL and invalid UTF-8 and collapses control characters
// the board's JSONB cannot store. Newlines and tabs stay: a ticket request is
// text.
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
