package boardintegration

// GET /v1/board/me: the caller's board identity and effective permissions.
//
// board v0.1.1 has no caller route; the Phase 2 UI needs one to know its actor
// (for "my claim" rendering), whether it may read/write/manage the board, and
// the channel `can` map it bootstraps from. The answer is built from the edge
// principal only: no header, path or query can choose another account, bot or
// tenant. A request without an authenticated principal is 401; a principal
// from another tenant sees only its own board.

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/auth"

	boardlog "github.com/jixoai/keel-board/log"
	"github.com/jixoai/keel-board/protocol"
)

// meChannelLimit bounds the channel `can` map. The UI's channel picker pages at
// 100; 500 keeps `/me` useful without turning one boot request into an
// unbounded projection walk.
const meChannelLimit = 500

type meAccount struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
}

type meBot struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type meCan struct {
	Board struct {
		Read   bool `json:"read"`
		Write  bool `json:"write"`
		Manage bool `json:"manage"`
	} `json:"board"`
}

type meResponse struct {
	Account   meAccount                    `json:"account"`
	Bot       *meBot                       `json:"bot"`
	Actor     string                       `json:"actor"`
	ActorName string                       `json:"actorName,omitempty"`
	Tenant    string                       `json:"tenant"`
	Can       meCan                        `json:"can"`
	Channels  map[string]boardMeChannelCan `json:"channels"`
}

func (h *handler) me(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok || principal.TenantID == uuid.Nil || principal.AccountID == uuid.Nil {
		writeMeError(w, http.StatusUnauthorized, protocol.CodeUnauthorized, "authentication required")
		return
	}
	if h.identity == nil || h.authorizer == nil {
		writeMeError(w, http.StatusServiceUnavailable, protocol.CodeUnavailable, "board identity is unavailable")
		return
	}
	actor, err := h.identity.Actor(r)
	if err != nil {
		status, code, message := meErrorOf(err)
		writeMeError(w, status, code, message)
		return
	}
	tenant := principal.TenantID.String()
	abilities, err := h.authorizer.BoardAbilities(r.Context(), tenant, actor)
	if err != nil {
		status, code, message := meErrorOf(err)
		writeMeError(w, status, code, message)
		return
	}

	response := meResponse{
		Account: meAccount{
			ID:          principal.AccountID.String(),
			Username:    principal.Username,
			DisplayName: principal.DisplayName,
		},
		Actor:     actor.ID,
		ActorName: actor.Name,
		Tenant:    tenant,
		Channels:  map[string]boardMeChannelCan{},
	}
	if principal.BotID != "" {
		response.Bot = &meBot{ID: principal.BotID, Name: actor.Name}
	}
	response.Can.Board.Read = abilities.Read
	response.Can.Board.Write = abilities.Write
	response.Can.Board.Manage = abilities.Manage
	response.Channels = h.channelCans(r, tenant, actor)
	writeAdminJSON(w, http.StatusOK, response)
}

// channelCans maps every channel the caller may read to the three actions the
// task surface cares about. The probe records no denial; a channel without a
// readable relation is simply absent, exactly as /channels omits it.
func (h *handler) channelCans(r *http.Request, tenant string, actor protocol.Actor) map[string]boardMeChannelCan {
	out := map[string]boardMeChannelCan{}
	if h.board == nil || h.authorizer == nil {
		return out
	}
	err := h.board.Read(tenant, func(snapshot *boardlog.Snapshot) error {
		for _, channel := range snapshot.Channels() {
			abilities := h.authorizer.ChannelAbilities(r.Context(), tenant, actor, channel.Path)
			if !abilities.Read {
				continue
			}
			out[channel.Path] = boardMeChannelCan{
				Read: true, Write: abilities.Post || abilities.Label, Manage: abilities.Manage,
			}
			if len(out) >= meChannelLimit {
				break
			}
		}
		return nil
	})
	if err != nil && h.logger != nil {
		h.logger.Warn("board me channel map could not be read", "tenant", tenant, "error", err)
	}
	return out
}

func meErrorOf(err error) (int, protocol.Code, string) {
	if boardErr, ok := protocol.AsError(err); ok {
		return boardErr.Status, boardErr.Code, boardErr.Message
	}
	return http.StatusServiceUnavailable, protocol.CodeUnavailable, "board identity is unavailable"
}

func writeMeError(w http.ResponseWriter, status int, code protocol.Code, message string) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(protocol.NewError(code, message).Body())
}
