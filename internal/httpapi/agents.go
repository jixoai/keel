// Agent registry and agent-team HTTP surface (change keel-agent-team-v1, P2/P3).
//
// The shapes are the DTOs in agentteam_dto.go; the rules are internal/agentteam's
// validation, normalization and content hash. This file is the wiring between the
// two: it routes, establishes the tenant, decodes strictly, calls the domain,
// maps the domain's typed failures onto status codes, and persists the document
// through the registry seam.
//
// Rules the handlers keep:
//
//   - The tenant comes from X-Tenant-ID only (the auth middleware injects it);
//     no body and no path segment can name another tenant. Every store call
//     carries it, so a foreign id answers 404 exactly like a missing one.
//   - A write seals the team before it is persisted (Seal is the only way the
//     revision moves), so every stored team can be frozen into a run manifest
//     and the stored hash always matches the stored content.
//   - A team is validated against the Agent library (ValidateWithAgents), so a
//     seat that names a missing agent is a 400 agent_not_found instead of a team
//     that silently cannot start.
//   - Deleting an agent a team seats is refused with 409 + the referencing
//     teams; the store's reference query is the only truth for that check.
//   - The captain's team CLI (agentteam.EffectiveTeamCLI) gates member add/
//     remove when the caller declares it acts as the captain (X-Keel-Actor-Agent).
//     Without that header the caller is the operator (the settings UI, the local
//     `keel team` command), which is the face that manages teams; a refused
//     captain call is a typed 403, never a silent success.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentteam"
	"github.com/jixoai/keel/internal/logsafe"
	"github.com/jixoai/keel/internal/store"
)

// AgentTeamRegistry is the persistence seam of the registry surfaces. It is
// satisfied by *store.Store; the interface exists so the HTTP handlers are
// testable without PostgreSQL (the conformance tests run a map-backed fake).
//
// The seam is document-shaped on purpose: internal/store must not import
// internal/agentteam (architecture guard), so the mapping between a stored
// document and a domain value happens here.
type AgentTeamRegistry interface {
	CreateAgent(ctx context.Context, record store.AgentRecord) (bool, error)
	UpdateAgent(ctx context.Context, record store.AgentRecord) (bool, error)
	GetAgent(ctx context.Context, tenantID uuid.UUID, agentID string) (store.AgentRecord, bool, error)
	ListAgents(ctx context.Context, tenantID uuid.UUID) ([]store.AgentRecord, error)
	DeleteAgent(ctx context.Context, tenantID uuid.UUID, agentID string) (bool, error)
	TeamsReferencingAgent(ctx context.Context, tenantID uuid.UUID, agentID string) ([]store.TeamReference, error)

	CreateTeam(ctx context.Context, record store.TeamRecord) (bool, error)
	UpdateTeam(ctx context.Context, record store.TeamRecord, expectedRevision int64) (bool, error)
	GetTeam(ctx context.Context, tenantID uuid.UUID, teamID string) (store.TeamRecord, bool, error)
	ListTeams(ctx context.Context, tenantID uuid.UUID) ([]store.TeamRecord, error)
	DeleteTeam(ctx context.Context, tenantID uuid.UUID, teamID string) (bool, error)
}

// actorAgentHeader names the agent a member mutation acts as. A captain's own
// CLI sets it; an operator (settings UI, local CLI) omits it.
const actorAgentHeader = "X-Keel-Actor-Agent"

func (s *Server) registerAgentTeamRoutes() {
	s.mux.HandleFunc("GET /v1/agents", s.listAgents)
	s.mux.HandleFunc("POST /v1/agents", s.createAgent)
	s.mux.HandleFunc("GET /v1/agents/{id}", s.getAgent)
	s.mux.HandleFunc("PUT /v1/agents/{id}", s.updateAgent)
	s.mux.HandleFunc("DELETE /v1/agents/{id}", s.deleteAgent)

	s.mux.HandleFunc("GET /v1/teams", s.listTeams)
	s.mux.HandleFunc("POST /v1/teams", s.createTeam)
	s.mux.HandleFunc("GET /v1/teams/{id}", s.getTeam)
	s.mux.HandleFunc("PUT /v1/teams/{id}", s.updateTeam)
	s.mux.HandleFunc("DELETE /v1/teams/{id}", s.deleteTeam)
	s.mux.HandleFunc("POST /v1/teams/{id}/members", s.addTeamMember)
	s.mux.HandleFunc("DELETE /v1/teams/{id}/members/{agentId}", s.removeTeamMember)
}

// --- agents --------------------------------------------------------------

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.requireTenant(w, r)
	if !ok || !s.registryReady(w) {
		return
	}
	records, err := s.cfg.AgentTeams.ListAgents(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	views := make([]AgentView, 0, len(records))
	for _, record := range records {
		agent, err := agentFromRecord(record)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		views = append(views, NewAgentView(tenantID, agent))
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.requireTenant(w, r)
	if !ok || !s.registryReady(w) {
		return
	}
	agent, _, found, ok := s.lookupAgent(w, r, tenantID)
	if !ok {
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, fmt.Errorf("agent %q does not exist", r.PathValue("id")))
		return
	}
	writeJSON(w, http.StatusOK, NewAgentView(tenantID, agent))
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.requireTenant(w, r)
	if !ok || !s.registryReady(w) {
		return
	}
	var body AgentWriteRequest
	if !s.decodeAgentBody(w, r, &body) {
		return
	}
	// NewAgent mints the stable id (and the identicon seed) when the client only
	// sent a name; an explicit id must satisfy the same grammar an update does.
	agent := body.NewAgent()
	if err := agent.ValidateWithTools(s.cfg.ToolExists); err != nil {
		writeFieldErrors(w, err)
		return
	}
	record, err := agentRecord(tenantID, agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	created, err := s.cfg.AgentTeams.CreateAgent(r.Context(), record)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !created {
		writeRegistryError(w, http.StatusConflict, "agent_exists",
			fmt.Errorf("agent %q already exists in this tenant; update it instead of creating it again", agent.ID), nil)
		return
	}
	writeJSON(w, http.StatusCreated, NewAgentView(tenantID, agent))
}

func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.requireTenant(w, r)
	if !ok || !s.registryReady(w) {
		return
	}
	existing, record, found, ok := s.lookupAgent(w, r, tenantID)
	if !ok {
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, fmt.Errorf("agent %q does not exist", r.PathValue("id")))
		return
	}
	var body AgentWriteRequest
	if !s.decodeAgentBody(w, r, &body) {
		return
	}
	// The path owns the identity: a body that carries a different id would either
	// be ignored (a second identity silently dropped) or re-key the agent. The
	// stored id wins, and an update can never mint a new agent.
	body.ID = record.ID
	agent := body.Agent()
	// archived is not client-writable: a write must not resurrect a retired
	// agent, so the stored lifecycle value is carried over.
	agent.Archived = existing.Archived
	if err := agent.ValidateWithTools(s.cfg.ToolExists); err != nil {
		writeFieldErrors(w, err)
		return
	}
	updated, err := agentRecord(tenantID, agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	found, err = s.cfg.AgentTeams.UpdateAgent(r.Context(), updated)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, fmt.Errorf("agent %q does not exist", record.ID))
		return
	}
	writeJSON(w, http.StatusOK, NewAgentView(tenantID, agent))
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.requireTenant(w, r)
	if !ok || !s.registryReady(w) {
		return
	}
	_, record, found, ok := s.lookupAgent(w, r, tenantID)
	if !ok {
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, fmt.Errorf("agent %q does not exist", record.ID))
		return
	}
	// Reference protection (spec: Agent 引用与删除保护): an agent seated in a team
	// is not hard-deleted; the refusal lists the teams so the caller can offer
	// "remove it from the team first".
	references, err := s.cfg.AgentTeams.TeamsReferencingAgent(r.Context(), tenantID, record.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if len(references) > 0 {
		teams := make([]map[string]any, 0, len(references))
		names := make([]string, 0, len(references))
		for _, reference := range references {
			teams = append(teams, map[string]any{"id": reference.ID, "name": reference.Name})
			names = append(names, reference.ID)
		}
		writeRegistryError(w, http.StatusConflict, "agent_in_use", fmt.Errorf(
			"agent %q is a captain or member of %s; remove it from those teams before deleting it",
			record.ID, strings.Join(names, ", ")), map[string]any{"references": teams})
		return
	}
	deleted, err := s.cfg.AgentTeams.DeleteAgent(r.Context(), tenantID, record.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !deleted {
		writeErr(w, http.StatusNotFound, fmt.Errorf("agent %q does not exist", record.ID))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": record.ID})
}

// --- teams ---------------------------------------------------------------

func (s *Server) listTeams(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.requireTenant(w, r)
	if !ok || !s.registryReady(w) {
		return
	}
	records, err := s.cfg.AgentTeams.ListTeams(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	views := make([]AgentTeamView, 0, len(records))
	for _, record := range records {
		team, err := teamFromRecord(record)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		views = append(views, NewAgentTeamView(tenantID, *team))
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) getTeam(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.requireTenant(w, r)
	if !ok || !s.registryReady(w) {
		return
	}
	team, _, found, ok := s.lookupTeam(w, r, tenantID)
	if !ok {
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, fmt.Errorf("team %q does not exist", r.PathValue("id")))
		return
	}
	writeJSON(w, http.StatusOK, NewAgentTeamView(tenantID, *team))
}

func (s *Server) createTeam(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.requireTenant(w, r)
	if !ok || !s.registryReady(w) {
		return
	}
	var body AgentTeamWriteRequest
	if !s.decodeTeamBody(w, r, &body) {
		return
	}
	team := body.Team()
	if strings.TrimSpace(team.ID) == "" {
		// `keel team create --captain ... --name ...` must produce a team; the id
		// is minted here exactly like a name-only agent, and never on update.
		team.ID = agentteam.GenerateTeamID()
	}
	if err := team.ValidateWithAgents(s.agentExists(r.Context(), tenantID)); err != nil {
		writeFieldErrors(w, err)
		return
	}
	team.Seal()
	record, err := teamRecord(tenantID, team)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	created, err := s.cfg.AgentTeams.CreateTeam(r.Context(), record)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !created {
		writeRegistryError(w, http.StatusConflict, "team_exists",
			fmt.Errorf("team %q already exists in this tenant; update it instead of creating it again", team.ID), nil)
		return
	}
	writeJSON(w, http.StatusCreated, NewAgentTeamView(tenantID, *team))
}

func (s *Server) updateTeam(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.requireTenant(w, r)
	if !ok || !s.registryReady(w) {
		return
	}
	existing, record, found, ok := s.lookupTeam(w, r, tenantID)
	if !ok {
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, fmt.Errorf("team %q does not exist", r.PathValue("id")))
		return
	}
	var body AgentTeamWriteRequest
	if !s.decodeTeamBody(w, r, &body) {
		return
	}
	next := body.Team()
	next.ID = record.ID
	// archived is not client-writable (DTO rule); an update keeps the stored
	// lifecycle value so a retired team stays retired.
	next.Archived = existing.Archived
	if err := next.ValidateWithAgents(s.agentExists(r.Context(), tenantID)); err != nil {
		writeFieldErrors(w, err)
		return
	}
	if !s.storeTeamRevision(w, r, tenantID, existing, next, record.Revision, http.StatusOK) {
		return
	}
}

func (s *Server) deleteTeam(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.requireTenant(w, r)
	if !ok || !s.registryReady(w) {
		return
	}
	_, record, found, ok := s.lookupTeam(w, r, tenantID)
	if !ok {
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, fmt.Errorf("team %q does not exist", r.PathValue("id")))
		return
	}
	deleted, err := s.cfg.AgentTeams.DeleteTeam(r.Context(), tenantID, record.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !deleted {
		writeErr(w, http.StatusNotFound, fmt.Errorf("team %q does not exist", record.ID))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": record.ID})
}

// addTeamMember seats one agent on an existing team. It edits the team
// configuration (the DB-backed file) and nothing else: the agent's shared
// configuration is not rewritten.
func (s *Server) addTeamMember(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.requireTenant(w, r)
	if !ok || !s.registryReady(w) {
		return
	}
	team, record, found, ok := s.lookupTeam(w, r, tenantID)
	if !ok {
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, fmt.Errorf("team %q does not exist", r.PathValue("id")))
		return
	}
	if !s.authorizeTeamActor(w, r, tenantID, team, agentteam.TeamCLIMemberAdd) {
		return
	}
	var member agentteam.TeamMember
	if !s.decodeJSONBody(w, r, &member) {
		return
	}
	member.AgentID = strings.TrimSpace(member.AgentID)
	if member.Inline == nil {
		if member.AgentID == "" {
			writeFieldErrors(w, &agentteam.ValidationError{Errors: []agentteam.FieldError{{
				Field:   "agentId",
				Code:    agentteam.CodeMissingAgent,
				Message: "a member seat must name an agent (or carry an inline definition)",
			}}})
			return
		}
		if _, exists, err := s.cfg.AgentTeams.GetAgent(r.Context(), tenantID, member.AgentID); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		} else if !exists {
			writeFieldErrors(w, &agentteam.ValidationError{Errors: []agentteam.FieldError{{
				Field:   "agentId",
				Code:    agentteam.CodeAgentNotFound,
				Message: fmt.Sprintf("agent %q is not in the agent library", member.AgentID),
			}}})
			return
		}
	}
	next := *team
	next.Members = append(append([]agentteam.TeamMember(nil), team.Members...), member)
	if !s.storeTeamRevision(w, r, tenantID, team, &next, record.Revision, http.StatusCreated) {
		return
	}
}

// removeTeamMember unsets one agent from a team. Removing the captain is the
// one seat edit that is refused: a team must keep exactly one captain (spec:
// missing_captain), and reassignment is an update of the captain seat.
func (s *Server) removeTeamMember(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.requireTenant(w, r)
	if !ok || !s.registryReady(w) {
		return
	}
	team, record, found, ok := s.lookupTeam(w, r, tenantID)
	if !ok {
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, fmt.Errorf("team %q does not exist", r.PathValue("id")))
		return
	}
	if !s.authorizeTeamActor(w, r, tenantID, team, agentteam.TeamCLIMemberRemove) {
		return
	}
	agentID := strings.TrimSpace(r.PathValue("agentId"))
	if agentID != "" && agentID == team.Captain.AgentID {
		writeFieldErrors(w, &agentteam.ValidationError{Errors: []agentteam.FieldError{{
			Field:   "captain.agentId",
			Code:    agentteam.CodeMissingCaptain,
			Message: "a team must keep exactly one captain; update the captain seat to reassign it",
		}}})
		return
	}
	kept := make([]agentteam.TeamMember, 0, len(team.Members))
	removed := false
	for _, member := range team.Members {
		if !removed && member.AgentID == agentID {
			removed = true
			continue
		}
		kept = append(kept, member)
	}
	if !removed {
		writeErr(w, http.StatusNotFound, fmt.Errorf("agent %q is not a member of team %q", agentID, team.ID))
		return
	}
	next := *team
	next.Members = kept
	if !s.storeTeamRevision(w, r, tenantID, team, &next, record.Revision, http.StatusOK) {
		return
	}
}

// --- shared plumbing -----------------------------------------------------

// requireTenant is tenantFrom plus the 401 the registry surface answers with.
func (s *Server) requireTenant(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("X-Tenant-ID is required"))
	}
	return tenantID, ok
}

// registryReady answers 503 when the persistence seam is not wired, instead of
// serving an empty library that would look like "no agents exist".
func (s *Server) registryReady(w http.ResponseWriter) bool {
	if s.cfg.AgentTeams == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("agent registry is not wired: the deployment has no agent/team store"))
		return false
	}
	return true
}

// lookupAgent reads the agent named by the path. record is returned even when
// the agent is absent so a 404 can name the requested id.
func (s *Server) lookupAgent(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) (agentteam.Agent, store.AgentRecord, bool, bool) {
	agentID := strings.TrimSpace(r.PathValue("id"))
	if agentID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("agent id is required"))
		return agentteam.Agent{}, store.AgentRecord{}, false, false
	}
	record, found, err := s.cfg.AgentTeams.GetAgent(r.Context(), tenantID, agentID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return agentteam.Agent{}, store.AgentRecord{ID: agentID}, false, false
	}
	if !found {
		return agentteam.Agent{}, store.AgentRecord{ID: agentID}, false, true
	}
	agent, err := agentFromRecord(record)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return agentteam.Agent{}, record, false, false
	}
	return agent, record, true, true
}

// lookupTeam reads the team named by the path.
func (s *Server) lookupTeam(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) (*agentteam.AgentTeam, store.TeamRecord, bool, bool) {
	teamID := strings.TrimSpace(r.PathValue("id"))
	if teamID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("team id is required"))
		return nil, store.TeamRecord{}, false, false
	}
	record, found, err := s.cfg.AgentTeams.GetTeam(r.Context(), tenantID, teamID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return nil, store.TeamRecord{ID: teamID}, false, false
	}
	if !found {
		return nil, store.TeamRecord{ID: teamID}, false, true
	}
	team, err := teamFromRecord(record)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return nil, record, false, false
	}
	return team, record, true, true
}

// storeTeamRevision is the one write path of a team edit: it validates the new
// content, seals it (which advances the revision and stamps the hash), persists
// it against the revision the caller read, and answers the fresh view. It
// returns false when it already wrote an error response.
//
// The seal happens on top of the STORED team, not on the request body, so the
// revision the manifest freezes is monotonic across every edit path (create,
// update, member add/remove). status is the success code of the calling edit
// (201 for a new member seat, 200 for an in-place team edit).
func (s *Server) storeTeamRevision(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID,
	stored, next *agentteam.AgentTeam, expectedRevision int64, status int) bool {
	// Validation runs on the draft content, not on the sealed copy: a stored team
	// carries revision N and the hash of its OLD content, so validating the edit
	// against that pair would report hash_mismatch for every legal change.
	draft := contentOnly(next)
	if err := draft.ValidateWithAgents(s.agentExists(r.Context(), tenantID)); err != nil {
		writeFieldErrors(w, err)
		return false
	}
	// Seal on top of the stored team so the revision it advances is the stored
	// one: create -> update -> member add/remove all move it monotonically.
	sealed := *stored
	applyContent(&sealed, draft)
	sealed.Seal()
	record, err := teamRecord(tenantID, &sealed)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return false
	}
	updated, err := s.cfg.AgentTeams.UpdateTeam(r.Context(), record, expectedRevision)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return false
	}
	if !updated {
		// Either another writer moved the revision or the team is gone. Re-read
		// to tell the two apart: a lost update is retryable, a deleted team is not.
		if _, found, readErr := s.cfg.AgentTeams.GetTeam(r.Context(), tenantID, record.ID); readErr == nil && found {
			writeRegistryError(w, http.StatusConflict, "team_revision_conflict", fmt.Errorf(
				"team %q changed while this edit was being prepared (read revision %d); read it again and retry",
				record.ID, expectedRevision), nil)
		} else {
			writeErr(w, http.StatusNotFound, fmt.Errorf("team %q does not exist", record.ID))
		}
		return false
	}
	writeJSON(w, status, NewAgentTeamView(tenantID, sealed))
	return true
}

// authorizeTeamActor enforces the captain's team CLI when the request declares
// an acting agent. No header means the operator face (the settings UI and the
// local `keel team` command managing the team), which is allowed.
func (s *Server) authorizeTeamActor(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID,
	team *agentteam.AgentTeam, subcommand string) bool {
	actor := strings.TrimSpace(r.Header.Get(actorAgentHeader))
	if actor == "" {
		return true
	}
	if actor != team.Captain.AgentID {
		writeRegistryError(w, http.StatusForbidden, "not_captain", fmt.Errorf(
			"agent %q is not the captain of team %q; only the captain's team CLI may %s",
			actor, team.ID, subcommand), nil)
		return false
	}
	captainAgent, err := s.captainAgent(r.Context(), tenantID, team)
	if err != nil {
		writeFieldErrors(w, err)
		return false
	}
	effective := agentteam.EffectiveTeamCLI(team.Capabilities.TeamCLI, captainAgent.TeamCLI)
	if effective.Allows(subcommand) {
		return true
	}
	// The refusal is typed: the whole surface is off on either side
	// (team_cli_disabled), or it is on but this command is not allowlisted
	// (team_cli_not_allowed). Either way the command is absent from the captain's
	// face AND a direct call fails — never a silent success.
	reason := agentteam.PermissionNotAllowed
	if !team.Capabilities.TeamCLI.Enabled || !captainAgent.TeamCLI.Enabled {
		reason = agentteam.PermissionDisabled
	}
	refusal := &agentteam.PermissionError{Subcommand: subcommand, Reason: reason}
	writeRegistryError(w, http.StatusForbidden, refusal.Code(), refusal, nil)
	return false
}

// captainAgent resolves the captain's agent configuration: the team's inline
// definition when it carries one, otherwise the Agent library entry.
func (s *Server) captainAgent(ctx context.Context, tenantID uuid.UUID, team *agentteam.AgentTeam) (agentteam.Agent, error) {
	if team.Captain.Inline != nil {
		return team.Captain.Inline.Normalized(), nil
	}
	record, found, err := s.cfg.AgentTeams.GetAgent(ctx, tenantID, team.Captain.AgentID)
	if err != nil {
		return agentteam.Agent{}, err
	}
	if !found {
		return agentteam.Agent{}, &agentteam.ValidationError{Errors: []agentteam.FieldError{{
			Field:   "captain.agentId",
			Code:    agentteam.CodeAgentNotFound,
			Message: fmt.Sprintf("agent %q is not in the agent library", team.Captain.AgentID),
		}}}
	}
	return agentFromRecord(record)
}

// agentExists builds the team-validation lookup. It is tenant-scoped: another
// tenant's agent is not a valid seat.
func (s *Server) agentExists(ctx context.Context, tenantID uuid.UUID) func(agentID string) bool {
	return func(agentID string) bool {
		_, found, err := s.cfg.AgentTeams.GetAgent(ctx, tenantID, agentID)
		return err == nil && found
	}
}

// decodeAgentBody decodes the strict agent request body (unknown fields,
// trailing data and an empty body are the same 400s the DTO documents).
func (s *Server) decodeAgentBody(w http.ResponseWriter, r *http.Request, dst *AgentWriteRequest) bool {
	return s.decodeJSONBody(w, r, dst)
}

// decodeTeamBody decodes the strict team request body.
func (s *Server) decodeTeamBody(w http.ResponseWriter, r *http.Request, dst *AgentTeamWriteRequest) bool {
	return s.decodeJSONBody(w, r, dst)
}

// decodeJSONBody decodes exactly one JSON object with unknown fields rejected,
// and writes the 400 itself.
func (s *Server) decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBody)
	if err := decodeStrictJSON(r.Body, dst); err != nil {
		var rejection *BodyRejection
		if errors.As(err, &rejection) {
			body := map[string]any{
				"error":  logsafe.Redact(err.Error()),
				"code":   rejection.Reason,
				"fields": []agentteam.FieldError{},
			}
			if rejection.Field != "" {
				body["field"] = rejection.Field
				body["fields"] = []agentteam.FieldError{{
					Field:   rejection.Field,
					Code:    rejection.Reason,
					Message: logsafe.Redact(err.Error()),
				}}
			}
			writeJSON(w, http.StatusBadRequest, body)
			return false
		}
		writeErr(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

// writeFieldErrors writes a 400 carrying the domain's field paths, so a form and
// a CLI both learn which field to fix without parsing prose. The text passes
// through the same redaction as writeErr: a validation message can quote stored
// configuration, and this is the one exit every registry error shares.
func writeFieldErrors(w http.ResponseWriter, err error) {
	fields := agentteam.FieldErrors(err)
	if fields == nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	redacted := make([]agentteam.FieldError, len(fields))
	for index, field := range fields {
		redacted[index] = agentteam.FieldError{
			Field:   field.Field,
			Code:    field.Code,
			Message: logsafe.Redact(field.Message),
		}
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error":  logsafe.Redact(err.Error()),
		"fields": redacted,
	})
}

// writeRegistryError writes a non-validation refusal with a stable machine
// code and any extra structured detail (references, current revision, ...).
func writeRegistryError(w http.ResponseWriter, status int, code string, err error, extra map[string]any) {
	body := map[string]any{"error": logsafe.Redact(err.Error()), "code": code}
	for key, value := range extra {
		body[key] = value
	}
	writeJSON(w, status, body)
}

// --- document mapping ----------------------------------------------------

// contentOnly returns the editable content of a team as an unsealed value. The
// seal (revision + hash) belongs to the stored row: an edit in progress has no
// revision yet, and validating it against the old one is a guaranteed
// hash_mismatch.
func contentOnly(team *agentteam.AgentTeam) *agentteam.AgentTeam {
	return &agentteam.AgentTeam{
		ID:              team.ID,
		Name:            team.Name,
		Description:     team.Description,
		Captain:         team.Captain,
		Members:         team.Members,
		Defaults:        team.Defaults,
		Capabilities:    team.Capabilities,
		FeatureBindings: team.FeatureBindings,
		Archived:        team.Archived,
	}
}

// applyContent copies the editable fields of src onto dst, leaving dst's seal
// (revision and stored hash) untouched so Seal can advance it.
func applyContent(dst, src *agentteam.AgentTeam) {
	dst.ID = src.ID
	dst.Name = src.Name
	dst.Description = src.Description
	dst.Captain = src.Captain
	dst.Members = src.Members
	dst.Defaults = src.Defaults
	dst.Capabilities = src.Capabilities
	dst.FeatureBindings = src.FeatureBindings
	dst.Archived = src.Archived
}

func agentFromRecord(record store.AgentRecord) (agentteam.Agent, error) {
	var agent agentteam.Agent
	if err := json.Unmarshal(record.Document, &agent); err != nil {
		return agentteam.Agent{}, fmt.Errorf("agent %q: stored document is not an agent: %w", record.ID, err)
	}
	// The lifted column is the lifecycle truth the list filters on; the document
	// carries the same value because both are written together.
	agent.Archived = record.Archived
	if agent.ID == "" {
		agent.ID = record.ID
	}
	return agent, nil
}

func agentRecord(tenantID uuid.UUID, agent agentteam.Agent) (store.AgentRecord, error) {
	document, err := json.Marshal(agent)
	if err != nil {
		return store.AgentRecord{}, fmt.Errorf("agent %q: encode document: %w", agent.ID, err)
	}
	return store.AgentRecord{
		TenantID: tenantID,
		ID:       agent.ID,
		Document: json.RawMessage(document),
		Archived: agent.Archived,
	}, nil
}

func teamFromRecord(record store.TeamRecord) (*agentteam.AgentTeam, error) {
	var team agentteam.AgentTeam
	if err := json.Unmarshal(record.Document, &team); err != nil {
		return nil, fmt.Errorf("team %q: stored document is not a team: %w", record.ID, err)
	}
	if team.ID == "" {
		team.ID = record.ID
	}
	team.Archived = record.Archived
	return &team, nil
}

func teamRecord(tenantID uuid.UUID, team *agentteam.AgentTeam) (store.TeamRecord, error) {
	document, err := json.Marshal(team)
	if err != nil {
		return store.TeamRecord{}, fmt.Errorf("team %q: encode document: %w", team.ID, err)
	}
	return store.TeamRecord{
		TenantID:    tenantID,
		ID:          team.ID,
		Revision:    int64(team.Revision()),
		ContentHash: team.Hash(),
		Document:    json.RawMessage(document),
		Archived:    team.Archived,
	}, nil
}
