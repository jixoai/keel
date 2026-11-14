package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Agent and agent-team persistence (change keel-agent-team-v1, P2/P3).
//
// This layer is deliberately document-blind: internal/agentteam owns the agent
// and team vocabulary (validation, normalization, the content hash), and the
// architecture guard (internal/architecture/agentteam_boundary_test.go) keeps
// the store from reaching up into it. So a row here is "tenant + id + one JSON
// document + the columns the list/guard queries need", and mapping a document to
// a domain value belongs to the caller (internal/httpapi).
//
// Two columns are lifted out of the documents because they are queried rather
// than read:
//
//   - agents.archived — the list filters on it, and a JSON scan would defeat the
//     tenant index;
//   - agent_teams.revision/content_hash — the change identity clients compare
//     and the optimistic-concurrency guard for writes. Both are derived from the
//     document by agentteam.Seal; the store never computes or invents them.

// ErrInvalidAgentDocument marks a document that is not JSON at all.
var ErrInvalidAgentDocument = errors.New("store: agent document is not valid JSON")

// AgentRecord is one stored agent row.
type AgentRecord struct {
	// TenantID is the owning tenant. Every query carries it.
	TenantID uuid.UUID
	// ID is the stable agent identity (agents.agent_id).
	ID string
	// Document is the agent JSON document as the registry wrote it.
	Document json.RawMessage
	// Archived mirrors the document's archived flag so the list can filter
	// without decoding.
	Archived bool
	// CreatedAt and UpdatedAt are UTC.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TeamRecord is one stored agent-team row: the team configuration document plus
// its change identity.
type TeamRecord struct {
	// TenantID is the owning tenant.
	TenantID uuid.UUID
	// ID is the stable team identity (agent_teams.team_id).
	ID string
	// Revision is the monotonic configuration revision carried by the document.
	Revision int64
	// ContentHash is the document's content hash.
	ContentHash string
	// Document is the team JSON document (captain, members, capabilities, and
	// the stamped revision/hash).
	Document json.RawMessage
	// Archived mirrors the document's archived flag.
	Archived bool
	// CreatedAt and UpdatedAt are UTC.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TeamReference names a team that seats an agent. It is what a refused agent
// deletion reports so the caller can offer "remove it from this team first".
type TeamReference struct {
	// ID is the team identity.
	ID string
	// Name is the team's display name ("" when the document carries none).
	Name string
}

// ProviderReference names one registry document that references a model
// provider instance, and the field inside it that carries the reference. It is
// what a refused provider removal reports so the caller can offer "repoint that
// agent/team first, or disable the provider instead".
type ProviderReference struct {
	// Kind is the referencing document's domain: "agent" or "team".
	Kind string
	// ID is the referencing document's identity.
	ID string
	// Name is the referencing document's display name ("" when it carries none).
	Name string
	// Field is the document field carrying the reference:
	// "defaultModel" (an agent), "defaults.modelOverride" (a team's members'
	// default binding) or "members[].inline.defaultModel" (an agent definition
	// authored inside a team).
	Field string
}

// ProvidersReferencingProvider lists the tenant's agents and teams that name
// providerID in a model binding: an agent's defaultModel, a team's
// defaults.modelOverride, or an inline member agent's defaultModel. It is the
// read behind the provider registry's deletion protection, and like
// TeamsReferencingAgent it answers a document query — the caller decides what a
// reference means (an archived row still counts here: it is restorable).
func (s *Store) ProvidersReferencingProvider(ctx context.Context, tenantID uuid.UUID, providerID string) ([]ProviderReference, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT 'agent', agent_id, COALESCE(document->>'name', ''), 'defaultModel'
		   FROM agents
		  WHERE tenant_id = $1 AND (document->'defaultModel'->>'providerId') = $2
		 UNION ALL
		 SELECT 'team', team_id, COALESCE(document->>'name', ''), 'defaults.modelOverride'
		   FROM agent_teams
		  WHERE tenant_id = $1 AND (document->'defaults'->'modelOverride'->>'providerId') = $2
		 UNION ALL
		 SELECT 'team', team_id, COALESCE(document->>'name', ''), 'members[].inline.defaultModel'
		   FROM agent_teams team_row
		  WHERE team_row.tenant_id = $1
		    AND EXISTS (
		      SELECT 1
		      FROM jsonb_array_elements(
		        CASE WHEN jsonb_typeof(team_row.document->'members') = 'array'
		             THEN team_row.document->'members' ELSE '[]'::jsonb END) AS member
		      WHERE (member->'inline'->'defaultModel'->>'providerId') = $2
		    )
		 ORDER BY 1, 2`, tenantID, providerID)
	if err != nil {
		return nil, fmt.Errorf("store: providers referencing provider: %w", err)
	}
	defer rows.Close()
	out := make([]ProviderReference, 0)
	for rows.Next() {
		var reference ProviderReference
		if err := rows.Scan(&reference.Kind, &reference.ID, &reference.Name, &reference.Field); err != nil {
			return nil, fmt.Errorf("store: scan provider reference: %w", err)
		}
		out = append(out, reference)
	}
	return out, rows.Err()
}

// CreateAgent inserts a new agent row. created=false means the id already
// exists for this tenant and nothing was written: a create never silently
// overwrites a stored agent.
func (s *Store) CreateAgent(ctx context.Context, record AgentRecord) (bool, error) {
	if err := validateAgentRecord(record); err != nil {
		return false, err
	}
	tag, err := s.execRetry(ctx,
		`INSERT INTO agents (tenant_id, agent_id, document, archived)
		 VALUES ($1, $2, $3::jsonb, $4)
		 ON CONFLICT (tenant_id, agent_id) DO NOTHING`,
		record.TenantID, record.ID, string(record.Document), record.Archived)
	if err != nil {
		return false, fmt.Errorf("store: create agent: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// UpdateAgent replaces an existing agent document. found=false means the agent
// does not exist for this tenant (the caller answers 404, never an implicit
// create from a PUT).
func (s *Store) UpdateAgent(ctx context.Context, record AgentRecord) (bool, error) {
	if err := validateAgentRecord(record); err != nil {
		return false, err
	}
	tag, err := s.execRetry(ctx,
		`UPDATE agents SET document = $3::jsonb, archived = $4, updated_at = now()
		 WHERE tenant_id = $1 AND agent_id = $2`,
		record.TenantID, record.ID, string(record.Document), record.Archived)
	if err != nil {
		return false, fmt.Errorf("store: update agent: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// GetAgent reads one agent. found=false means the tenant has no such agent
// (another tenant's agent answers the same way: the row is tenant-scoped).
func (s *Store) GetAgent(ctx context.Context, tenantID uuid.UUID, agentID string) (AgentRecord, bool, error) {
	record, err := scanAgentRow(s.pool.QueryRow(ctx,
		`SELECT tenant_id, agent_id, document, archived, created_at, updated_at
		 FROM agents WHERE tenant_id = $1 AND agent_id = $2`, tenantID, agentID))
	if errors.Is(err, errNoRows) {
		return AgentRecord{}, false, nil
	}
	if err != nil {
		return AgentRecord{}, false, fmt.Errorf("store: get agent: %w", err)
	}
	return record, true, nil
}

// ListAgents returns the tenant's agents ordered by id, which is the stable
// order a CLI listing and a diff depend on.
func (s *Store) ListAgents(ctx context.Context, tenantID uuid.UUID) ([]AgentRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, agent_id, document, archived, created_at, updated_at
		 FROM agents WHERE tenant_id = $1 ORDER BY agent_id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list agents: %w", err)
	}
	defer rows.Close()
	out := make([]AgentRecord, 0)
	for rows.Next() {
		record, err := scanAgentRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan agent: %w", err)
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// DeleteAgent removes one agent. found=false means it was already gone.
// Reference protection (409 when a team seats the agent) is the caller's rule:
// the store cannot know what a reference means, TeamsReferencingAgent answers it.
func (s *Store) DeleteAgent(ctx context.Context, tenantID uuid.UUID, agentID string) (bool, error) {
	tag, err := s.execRetry(ctx,
		`DELETE FROM agents WHERE tenant_id = $1 AND agent_id = $2`, tenantID, agentID)
	if err != nil {
		return false, fmt.Errorf("store: delete agent: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// TeamsReferencingAgent lists the tenant's teams that seat agentID as captain or
// member. A member carrying an inline agent definition is self-contained and is
// not a reference to the library entry, so it does not count (deleting the
// library agent leaves that team intact).
func (s *Store) TeamsReferencingAgent(ctx context.Context, tenantID uuid.UUID, agentID string) ([]TeamReference, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT team_id, COALESCE(document->>'name', '')
		 FROM agent_teams
		 WHERE tenant_id = $1
		   AND (
		     ((document->'captain'->>'agentId') = $2 AND (document->'captain'->'inline') IS NULL)
		     OR EXISTS (
		       SELECT 1
		       FROM jsonb_array_elements(
		         CASE WHEN jsonb_typeof(document->'members') = 'array'
		              THEN document->'members' ELSE '[]'::jsonb END) AS member
		       WHERE (member->>'agentId') = $2 AND (member->'inline') IS NULL
		     )
		   )
		 ORDER BY team_id`, tenantID, agentID)
	if err != nil {
		return nil, fmt.Errorf("store: teams referencing agent: %w", err)
	}
	defer rows.Close()
	out := make([]TeamReference, 0)
	for rows.Next() {
		var reference TeamReference
		if err := rows.Scan(&reference.ID, &reference.Name); err != nil {
			return nil, fmt.Errorf("store: scan team reference: %w", err)
		}
		out = append(out, reference)
	}
	return out, rows.Err()
}

// CreateTeam inserts a new team row. created=false means the id already exists.
func (s *Store) CreateTeam(ctx context.Context, record TeamRecord) (bool, error) {
	if err := validateTeamRecord(record); err != nil {
		return false, err
	}
	tag, err := s.execRetry(ctx,
		`INSERT INTO agent_teams (tenant_id, team_id, revision, content_hash, document, archived)
		 VALUES ($1, $2, $3, $4, $5::jsonb, $6)
		 ON CONFLICT (tenant_id, team_id) DO NOTHING`,
		record.TenantID, record.ID, record.Revision, record.ContentHash, string(record.Document), record.Archived)
	if err != nil {
		return false, fmt.Errorf("store: create team: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// UpdateTeam replaces an existing team document, but only when the stored
// revision is still expectedRevision. ok=false means either the team does not
// exist (or belongs to another tenant) or another write moved the revision
// first; the caller distinguishes them by reading the row again.
func (s *Store) UpdateTeam(ctx context.Context, record TeamRecord, expectedRevision int64) (bool, error) {
	if err := validateTeamRecord(record); err != nil {
		return false, err
	}
	tag, err := s.execRetry(ctx,
		`UPDATE agent_teams
		    SET revision = $4, content_hash = $5, document = $6::jsonb, archived = $7, updated_at = now()
		  WHERE tenant_id = $1 AND team_id = $2 AND revision = $3`,
		record.TenantID, record.ID, expectedRevision, record.Revision, record.ContentHash,
		string(record.Document), record.Archived)
	if err != nil {
		return false, fmt.Errorf("store: update team: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// GetTeam reads one team.
func (s *Store) GetTeam(ctx context.Context, tenantID uuid.UUID, teamID string) (TeamRecord, bool, error) {
	record, err := scanTeamRow(s.pool.QueryRow(ctx,
		`SELECT tenant_id, team_id, revision, content_hash, document, archived, created_at, updated_at
		 FROM agent_teams WHERE tenant_id = $1 AND team_id = $2`, tenantID, teamID))
	if errors.Is(err, errNoRows) {
		return TeamRecord{}, false, nil
	}
	if err != nil {
		return TeamRecord{}, false, fmt.Errorf("store: get team: %w", err)
	}
	return record, true, nil
}

// ListTeams returns the tenant's teams ordered by id.
func (s *Store) ListTeams(ctx context.Context, tenantID uuid.UUID) ([]TeamRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, team_id, revision, content_hash, document, archived, created_at, updated_at
		 FROM agent_teams WHERE tenant_id = $1 ORDER BY team_id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list teams: %w", err)
	}
	defer rows.Close()
	out := make([]TeamRecord, 0)
	for rows.Next() {
		record, err := scanTeamRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan team: %w", err)
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// DeleteTeam removes one team. found=false means it was already gone.
func (s *Store) DeleteTeam(ctx context.Context, tenantID uuid.UUID, teamID string) (bool, error) {
	tag, err := s.execRetry(ctx,
		`DELETE FROM agent_teams WHERE tenant_id = $1 AND team_id = $2`, tenantID, teamID)
	if err != nil {
		return false, fmt.Errorf("store: delete team: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// errNoRows is pgx.ErrNoRows, named locally so the scan helpers' callers read
// "the row is absent" instead of a driver detail.
var errNoRows = pgx.ErrNoRows

func validateAgentRecord(record AgentRecord) error {
	if record.TenantID == uuid.Nil {
		return errors.New("store: agent tenant is required")
	}
	if strings.TrimSpace(record.ID) == "" {
		return errors.New("store: agent id is required")
	}
	if len(record.Document) == 0 || !json.Valid(record.Document) {
		return ErrInvalidAgentDocument
	}
	return nil
}

func validateTeamRecord(record TeamRecord) error {
	if record.TenantID == uuid.Nil {
		return errors.New("store: team tenant is required")
	}
	if strings.TrimSpace(record.ID) == "" {
		return errors.New("store: team id is required")
	}
	if len(record.Document) == 0 || !json.Valid(record.Document) {
		return errors.New("store: team document is not valid JSON")
	}
	return nil
}

func scanAgentRow(row rowScanner) (AgentRecord, error) {
	var (
		record   AgentRecord
		document []byte
	)
	if err := row.Scan(&record.TenantID, &record.ID, &document, &record.Archived,
		&record.CreatedAt, &record.UpdatedAt); err != nil {
		return AgentRecord{}, err
	}
	record.Document = json.RawMessage(document)
	return record, nil
}

func scanTeamRow(row rowScanner) (TeamRecord, error) {
	var (
		record   TeamRecord
		document []byte
	)
	if err := row.Scan(&record.TenantID, &record.ID, &record.Revision, &record.ContentHash,
		&document, &record.Archived, &record.CreatedAt, &record.UpdatedAt); err != nil {
		return TeamRecord{}, err
	}
	record.Document = json.RawMessage(document)
	return record, nil
}
