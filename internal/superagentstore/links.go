// superagent_board_links persistence (change keel-board-v1, phase 1 11.1-11.3).
//
// The table maps one superagent record to the board artifact its adapter
// created. It is keel's row, not the board's: the board log remains the board
// projection and the collab_* tables remain the supervisor truth. The unique
// key (tenant, source, source_id, kind) is the mirror's idempotency, so a
// repeated sync or a restart finds the existing task/reply instead of
// appending a second one.
package superagentstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Board link kinds. They stay open strings so a new mirror surface needs no
// schema change; these names are the ones the adapter writes today.
const (
	BoardLinkTicket     = "ticket"
	BoardLinkAssignment = "assignment"
	BoardLinkAttention  = "attention"
	BoardLinkReport     = "report"
	BoardLinkGitHub     = "github_event"
)

// BoardLink is one durable mapping row.
type BoardLink struct {
	TenantID    uuid.UUID `json:"tenantId"`
	Source      string    `json:"source"`
	SourceID    string    `json:"sourceId"`
	Kind        string    `json:"kind"`
	BoardTaskID uint64    `json:"boardTaskId"`
	// ClaimEpoch is the board claim epoch the adapter last mirrored for an
	// assignment. Zero means no claim has been mirrored.
	ClaimEpoch uint64    `json:"claimEpoch"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Validate keeps a malformed link from becoming a lookup key nobody can
// reconstruct.
func (l BoardLink) Validate() error {
	switch {
	case l.TenantID == uuid.Nil:
		return fmt.Errorf("superagentstore: board link tenant is required")
	case strings.TrimSpace(l.Source) == "":
		return fmt.Errorf("superagentstore: board link source is required")
	case strings.TrimSpace(l.SourceID) == "":
		return fmt.Errorf("superagentstore: board link source id is required")
	case strings.TrimSpace(l.Kind) == "":
		return fmt.Errorf("superagentstore: board link kind is required")
	case l.BoardTaskID == 0:
		return fmt.Errorf("superagentstore: board link board task id is required")
	}
	return nil
}

// PutBoardLink inserts one mapping or returns the existing row for the same
// key. A key that already maps to a different board task is a conflict: the
// caller is about to create a second artifact for one source record, which is
// exactly what this table exists to prevent.
func (s *Store) PutBoardLink(ctx context.Context, link BoardLink) (BoardLink, error) {
	link.Source = strings.TrimSpace(link.Source)
	link.SourceID = strings.TrimSpace(link.SourceID)
	link.Kind = strings.TrimSpace(link.Kind)
	if err := link.Validate(); err != nil {
		return BoardLink{}, err
	}
	row, err := scanBoardLink(s.pool.QueryRow(ctx, `
		INSERT INTO superagent_board_links
		    (tenant_id, source, source_id, kind, board_task_id, claim_epoch)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, source, source_id, kind) DO NOTHING
		RETURNING tenant_id, source, source_id, kind, board_task_id, claim_epoch, created_at, updated_at`,
		link.TenantID, link.Source, link.SourceID, link.Kind, link.BoardTaskID, link.ClaimEpoch))
	if errors.Is(err, pgx.ErrNoRows) {
		existing, found, err := s.GetBoardLink(ctx, link.TenantID, link.Source, link.SourceID, link.Kind)
		if err != nil {
			return BoardLink{}, err
		}
		if !found {
			return BoardLink{}, fmt.Errorf("superagentstore: board link %s/%s/%s disappeared during insert",
				link.Source, link.SourceID, link.Kind)
		}
		if existing.BoardTaskID != link.BoardTaskID {
			return BoardLink{}, fmt.Errorf("superagentstore: board link %s/%s/%s already maps task %d, not %d",
				link.Source, link.SourceID, link.Kind, existing.BoardTaskID, link.BoardTaskID)
		}
		return existing, nil
	}
	if err != nil {
		return BoardLink{}, fmt.Errorf("superagentstore: put board link: %w", err)
	}
	return row, nil
}

// GetBoardLink reads one mapping. A missing row and another tenant's row are
// the same not-found answer.
func (s *Store) GetBoardLink(ctx context.Context, tenantID uuid.UUID, source, sourceID, kind string) (BoardLink, bool, error) {
	if tenantID == uuid.Nil {
		return BoardLink{}, false, fmt.Errorf("superagentstore: board link tenant is required")
	}
	row, err := scanBoardLink(s.pool.QueryRow(ctx, `
		SELECT tenant_id, source, source_id, kind, board_task_id, claim_epoch, created_at, updated_at
		  FROM superagent_board_links
		 WHERE tenant_id = $1 AND source = $2 AND source_id = $3 AND kind = $4`,
		tenantID, strings.TrimSpace(source), strings.TrimSpace(sourceID), strings.TrimSpace(kind)))
	if errors.Is(err, pgx.ErrNoRows) {
		return BoardLink{}, false, nil
	}
	if err != nil {
		return BoardLink{}, false, fmt.Errorf("superagentstore: get board link: %w", err)
	}
	return row, true, nil
}

// GetBoardLinkByTask finds the mapping for a board task ID. The bridge uses it
// on the request path: an action on a mirrored task must be routed to the
// supervisor before the board changes.
func (s *Store) GetBoardLinkByTask(ctx context.Context, tenantID uuid.UUID, source, kind string, boardTaskID uint64) (BoardLink, bool, error) {
	if tenantID == uuid.Nil {
		return BoardLink{}, false, fmt.Errorf("superagentstore: board link tenant is required")
	}
	row, err := scanBoardLink(s.pool.QueryRow(ctx, `
		SELECT tenant_id, source, source_id, kind, board_task_id, claim_epoch, created_at, updated_at
		  FROM superagent_board_links
		 WHERE tenant_id = $1 AND source = $2 AND kind = $3 AND board_task_id = $4`,
		tenantID, strings.TrimSpace(source), strings.TrimSpace(kind), boardTaskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return BoardLink{}, false, nil
	}
	if err != nil {
		return BoardLink{}, false, fmt.Errorf("superagentstore: get board link by task: %w", err)
	}
	return row, true, nil
}

// SetBoardLinkClaimEpoch records the last claim epoch the adapter mirrored.
// It never moves the mapping to another task.
func (s *Store) SetBoardLinkClaimEpoch(ctx context.Context, tenantID uuid.UUID, source, sourceID, kind string, epoch uint64) error {
	if tenantID == uuid.Nil {
		return fmt.Errorf("superagentstore: board link tenant is required")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE superagent_board_links
		   SET claim_epoch = $5, updated_at = now()
		 WHERE tenant_id = $1 AND source = $2 AND source_id = $3 AND kind = $4`,
		tenantID, strings.TrimSpace(source), strings.TrimSpace(sourceID), strings.TrimSpace(kind), epoch)
	if err != nil {
		return fmt.Errorf("superagentstore: set board link claim epoch: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

type boardLinkScanner interface {
	Scan(dest ...any) error
}

func scanBoardLink(row boardLinkScanner) (BoardLink, error) {
	var link BoardLink
	var taskID, epoch int64
	if err := row.Scan(&link.TenantID, &link.Source, &link.SourceID, &link.Kind,
		&taskID, &epoch, &link.CreatedAt, &link.UpdatedAt); err != nil {
		return BoardLink{}, err
	}
	if taskID < 0 {
		return BoardLink{}, fmt.Errorf("superagentstore: board link task id is negative")
	}
	link.BoardTaskID = uint64(taskID)
	if epoch < 0 {
		return BoardLink{}, fmt.Errorf("superagentstore: board link claim epoch is negative")
	}
	link.ClaimEpoch = uint64(epoch)
	return link, nil
}
