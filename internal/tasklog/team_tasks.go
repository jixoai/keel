package tasklog

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Team-task page bounds. They mirror the session list's shape: a small default,
// a bounded page, and an opaque keyset cursor so a page never drifts while
// member cards are being written.
const (
	DefaultTeamTaskLimit = 50
	MaxTeamTaskLimit     = 200
)

// CardPage is one keyset page of child task cards. NextCursor is empty on the
// last page.
type CardPage struct {
	Cards      []Card `json:"cards"`
	NextCursor string `json:"nextCursor,omitempty"`
	HasMore    bool   `json:"hasMore"`
}

// ListTeamTasks reads the member cards linked to a captain session. A sub-agent
// links its own card (living in the child session's ledger) to the parent's
// card through parentTaskId; this projection answers "which member cards belong
// to this captain", without copying a card or reading a child transcript.
//
// The session id is the captain's session: the query joins through the cards
// owned by that session. It is a read-only projection — the ledger remains the
// only truth.
func (s *Service) ListTeamTasks(ctx context.Context, tenantID, sessionID uuid.UUID, limit int, after string) (CardPage, error) {
	if err := s.ready(); err != nil {
		return CardPage{}, err
	}
	if tenantID == uuid.Nil || sessionID == uuid.Nil {
		return CardPage{}, fmt.Errorf("%w: tenant and session are required", ErrTaskRequired)
	}
	if limit <= 0 || limit > MaxTeamTaskLimit {
		limit = DefaultTeamTaskLimit
	}
	var afterUpdatedAt *time.Time
	var afterTaskID *uuid.UUID
	if strings.TrimSpace(after) != "" {
		cursor, err := decodeTeamTaskCursor(after)
		if err != nil {
			return CardPage{}, err
		}
		afterUpdatedAt = &cursor.updatedAt
		afterTaskID = &cursor.taskID
	}
	cards, err := s.projection.ListChildCards(ctx, tenantID, sessionID, limit+1, afterUpdatedAt, afterTaskID)
	if err != nil {
		return CardPage{}, err
	}
	page := CardPage{HasMore: len(cards) > limit}
	if page.HasMore {
		cards = cards[:limit]
	}
	page.Cards = cards
	if page.HasMore && len(cards) > 0 {
		last := cards[len(cards)-1]
		page.NextCursor = encodeTeamTaskCursor(last.UpdatedAt, last.TaskID)
	}
	return page, nil
}

type teamTaskCursor struct {
	updatedAt time.Time
	taskID    uuid.UUID
}

func encodeTeamTaskCursor(updatedAt time.Time, taskID uuid.UUID) string {
	raw := strconv.FormatInt(updatedAt.UTC().UnixMicro(), 10) + ":" + taskID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeTeamTaskCursor(value string) (teamTaskCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return teamTaskCursor{}, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 2 {
		return teamTaskCursor{}, fmt.Errorf("%w: expected <micros>:<taskId>", ErrInvalidCursor)
	}
	micros, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return teamTaskCursor{}, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	taskID, err := uuid.Parse(parts[1])
	if err != nil {
		return teamTaskCursor{}, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	return teamTaskCursor{updatedAt: time.UnixMicro(micros).UTC(), taskID: taskID}, nil
}
