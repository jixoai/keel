// TeamBots HTTP surface (change keel-bot-identity-access, A4.1).
//
// A TeamBot is Mission + Roster. It is a resource, never a permission subject:
// it has no Account/Profile and grants no permission (L12). Whether a Bot may be
// seated or started is decided by bot.invoke on that Bot (L20); this surface
// enforces that at roster creation, roster edit and start.
package agentapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/botteam"
	"github.com/jixoai/keel/internal/store"
)

type teamBotView struct {
	TeamBotID string                `json:"teamBotId"`
	Name      string                `json:"name"`
	Mission   string                `json:"mission"`
	Roster    []botteam.RosterEntry `json:"roster"`
	Revision  int64                 `json:"revision"`
	Hash      string                `json:"hash"`
	Archived  bool                  `json:"archived"`
	CreatedAt time.Time             `json:"createdAt"`
	UpdatedAt time.Time             `json:"updatedAt"`
}

func teamBotViewOf(record store.TeamBotRecord) teamBotView {
	var roster []botteam.RosterEntry
	_ = json.Unmarshal(record.Roster, &roster)
	if roster == nil {
		roster = []botteam.RosterEntry{}
	}
	return teamBotView{
		TeamBotID: record.ID, Name: record.Name, Mission: record.Mission,
		Roster: roster, Revision: record.Revision, Hash: record.ContentHash,
		Archived: record.Archived, CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC(),
	}
}

// teamBotDomain decodes a stored row into the domain value, preserving the
// revision/hash claims so Validate can reject a hand-edited row.
func teamBotDomain(record store.TeamBotRecord) (botteam.TeamBot, error) {
	raw, err := json.Marshal(map[string]any{
		"teamBotId": record.ID, "name": record.Name, "mission": record.Mission,
		"roster": json.RawMessage(record.Roster), "archived": record.Archived,
		"revision": record.Revision, "hash": record.ContentHash,
	})
	if err != nil {
		return botteam.TeamBot{}, err
	}
	var team botteam.TeamBot
	if err := json.Unmarshal(raw, &team); err != nil {
		return botteam.TeamBot{}, err
	}
	if err := team.Validate(); err != nil {
		return botteam.TeamBot{}, err
	}
	return team, nil
}

func teamBotRecord(tenantID uuid.UUID, team botteam.TeamBot) (store.TeamBotRecord, error) {
	roster, err := json.Marshal(team.Roster)
	if err != nil {
		return store.TeamBotRecord{}, err
	}
	return store.TeamBotRecord{
		TenantID: tenantID, ID: team.ID, Name: team.Name, Mission: team.Mission,
		Roster: roster, Revision: int64(team.Revision()), ContentHash: team.Hash(),
		Archived: team.Archived,
	}, nil
}

func (s *Server) listTeamBots(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	principal, err := s.cfg.Access.Principal(r.Context(), tenantID)
	if err != nil {
		writeIdentityError(w, http.StatusUnauthorized, "invalid_session", "authentication required")
		return
	}
	limit, err := identityLimit(r)
	if err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	records, err := s.cfg.Identity.ListTeamBots(r.Context(), tenantID, identityFlag(r, "includeArchived"), limit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	candidates := make([]string, 0, len(records))
	for _, record := range records {
		candidates = append(candidates, record.ID)
	}
	visible, err := s.cfg.Access.Visible(r.Context(), principal, access.KeyTeamBotRead, "team_bot", candidates)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !visible.Allowed() {
		writeDenied(w, visible.Decision)
		return
	}
	allowed := make(map[string]bool, len(visible.IDs))
	for _, id := range visible.IDs {
		allowed[id] = true
	}
	views := make([]teamBotView, 0, len(visible.IDs))
	for _, record := range records {
		if allowed[record.ID] {
			views = append(views, teamBotViewOf(record))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"teamBots": views})
}

type teamBotWriteRequest struct {
	TeamBotID        string                 `json:"teamBotId"`
	Name             string                 `json:"name"`
	Mission          string                 `json:"mission"`
	Roster           *[]botteam.RosterEntry `json:"roster"`
	Archived         *bool                  `json:"archived"`
	ExpectedRevision *int64                 `json:"expectedRevision"`
}

func (s *Server) createTeamBot(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	principal, tenantID, ok := s.requireHeldKey(w, r, access.KeyTeamBotManage)
	if !ok {
		return
	}
	var body teamBotWriteRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	teamID := strings.TrimSpace(body.TeamBotID)
	if teamID == "" {
		teamID = botteam.GenerateTeamBotID()
	}
	team := botteam.NewTeamBot(teamID, strings.TrimSpace(body.Name), strings.TrimSpace(body.Mission))
	if body.Roster != nil {
		team.Roster = *body.Roster
	}
	if body.Archived != nil {
		team.Archived = *body.Archived
	}
	team.Seal()
	if err := team.Validate(); err != nil {
		writeValidationError(w, err)
		return
	}
	if !s.requireRosterInvoke(w, r, principal, tenantID, team.Roster) {
		return
	}
	record, err := teamBotRecord(tenantID, *team)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	created, err := s.cfg.Identity.CreateTeamBot(r.Context(), record)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if _, err := s.cfg.Identity.PutResourceRelation(r.Context(), store.ResourceRelation{
		TenantID: tenantID, ResourceType: "team_bot", ResourceID: created.ID,
		Relation: store.RelationOwner, SubjectType: store.RelationSubjectAccount,
		SubjectID: principal.AccountID.String(), CreatedBy: principal.AccountID,
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.cfg.Access.AuditRelation(r.Context(), tenantID, principal.AccountID, "create", "team_bot", created.ID,
		store.RelationOwner, store.RelationSubjectAccount, principal.AccountID.String())
	if principal.IsSuperAdmin {
		s.cfg.Access.AuditSuperAdmin(r.Context(), principal, "team_bot.create", map[string]any{
			"team_bot_id": created.ID,
		})
	}
	writeJSON(w, http.StatusCreated, map[string]any{"teamBot": teamBotViewOf(created)})
}

func (s *Server) getTeamBot(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	teamID := strings.TrimSpace(r.PathValue("id"))
	if teamID == "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_team_bot_id", "team bot id is required")
		return
	}
	if _, _, ok := s.requireResource(w, r, access.KeyTeamBotRead, "team_bot", teamID); !ok {
		return
	}
	tenantID, _ := tenantFrom(w, r)
	record, err := s.cfg.Identity.GetTeamBot(r.Context(), tenantID, teamID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"teamBot": teamBotViewOf(record)})
}

func (s *Server) updateTeamBot(w http.ResponseWriter, r *http.Request) {
	s.updateTeamBotContent(w, r, false)
}

func (s *Server) updateTeamBotRoster(w http.ResponseWriter, r *http.Request) {
	s.updateTeamBotContent(w, r, true)
}

// updateTeamBotContent applies a CAS edit. rosterOnly restricts the change to
// the roster (the dedicated roster endpoint).
func (s *Server) updateTeamBotContent(w http.ResponseWriter, r *http.Request, rosterOnly bool) {
	if !s.identityReady(w) {
		return
	}
	teamID := strings.TrimSpace(r.PathValue("id"))
	if teamID == "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_team_bot_id", "team bot id is required")
		return
	}
	principal, tenantID, ok := s.requireResource(w, r, access.KeyTeamBotManage, "team_bot", teamID)
	if !ok {
		return
	}
	var body teamBotWriteRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if body.ExpectedRevision == nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", "expectedRevision is required")
		return
	}
	record, err := s.cfg.Identity.GetTeamBot(r.Context(), tenantID, teamID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	team, err := teamBotDomain(record)
	if err != nil {
		writeIdentityError(w, http.StatusConflict, "invalid_team_bot", err.Error())
		return
	}
	if !rosterOnly {
		if name := strings.TrimSpace(body.Name); name != "" {
			team.Name = name
		}
		if mission := strings.TrimSpace(body.Mission); mission != "" {
			team.Mission = mission
		}
	}
	if body.Roster != nil {
		team.Roster = *body.Roster
	}
	team.Seal()
	if err := team.Validate(); err != nil {
		writeValidationError(w, err)
		return
	}
	if !s.requireRosterInvoke(w, r, principal, tenantID, team.Roster) {
		return
	}
	if body.Archived != nil && !*body.Archived && record.Archived {
		writeIdentityError(w, http.StatusConflict, "team_bot_archived", "an archived team bot cannot be unarchived by update")
		return
	}
	updated, err := teamBotRecord(tenantID, team)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	stored, err := s.cfg.Identity.UpdateTeamBot(r.Context(), updated, *body.ExpectedRevision)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if principal.IsSuperAdmin {
		s.cfg.Access.AuditSuperAdmin(r.Context(), principal, "team_bot.update", map[string]any{
			"team_bot_id": teamID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"teamBot": teamBotViewOf(stored)})
}

func (s *Server) archiveTeamBot(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	teamID := strings.TrimSpace(r.PathValue("id"))
	if teamID == "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_team_bot_id", "team bot id is required")
		return
	}
	principal, tenantID, ok := s.requireResource(w, r, access.KeyTeamBotManage, "team_bot", teamID)
	if !ok {
		return
	}
	if err := s.cfg.Identity.ArchiveTeamBot(r.Context(), tenantID, teamID); err != nil {
		writeStoreError(w, err)
		return
	}
	if principal.IsSuperAdmin {
		s.cfg.Access.AuditSuperAdmin(r.Context(), principal, "team_bot.archive", map[string]any{
			"team_bot_id": teamID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"teamBotId": teamID, "archived": true})
}

type teamBotStartRequest struct {
	Text      string `json:"text"`
	SessionID string `json:"sessionId"`
	RequestID string `json:"requestId"`
}

func (s *Server) startTeamBot(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	teamID := strings.TrimSpace(r.PathValue("id"))
	if teamID == "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_team_bot_id", "team bot id is required")
		return
	}
	principal, tenantID, ok := s.requireResource(w, r, access.KeyTeamBotManage, "team_bot", teamID)
	if !ok {
		return
	}
	if s.cfg.StartBotSession == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("team bot start is not wired in this deployment"))
		return
	}
	var body teamBotStartRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	record, err := s.cfg.Identity.GetTeamBot(r.Context(), tenantID, teamID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if record.Archived {
		writeIdentityError(w, http.StatusConflict, "team_bot_archived", "an archived team bot cannot start")
		return
	}
	team, err := teamBotDomain(record)
	if err != nil {
		writeIdentityError(w, http.StatusConflict, "invalid_team_bot", err.Error())
		return
	}
	captain := ""
	for _, seat := range team.Roster {
		if seat.Role == botteam.RosterRoleCaptain {
			captain = seat.BotID
			break
		}
	}
	if captain == "" {
		writeIdentityError(w, http.StatusConflict, "invalid_team_bot", "the roster has no captain")
		return
	}
	// Starting a team drives the captain, which is a bot.invoke action on that
	// Bot (L20). Members are invoked by the captain later and are checked at
	// roster write time.
	if _, _, ok := s.requireResource(w, r, access.KeyBotInvoke, "bot", captain); !ok {
		return
	}
	botRecord, err := s.cfg.Identity.GetBot(r.Context(), tenantID, captain)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if botRecord.Archived {
		writeIdentityError(w, http.StatusConflict, "bot_archived", "the captain bot is archived")
		return
	}
	profile, err := s.cfg.Identity.GetProfile(r.Context(), tenantID, botRecord.ProfileID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		text = "Start."
	}
	sessionID := uuid.Nil
	if raw := strings.TrimSpace(body.SessionID); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			writeIdentityError(w, http.StatusBadRequest, "invalid_session_id", "sessionId must be a UUID")
			return
		}
		sessionID = parsed
	}
	result, err := s.cfg.StartBotSession(r.Context(), BotSessionRequest{
		TenantID: tenantID, SessionID: sessionID, BotID: captain,
		AccountID: profile.AccountID, ActorAccountID: principal.AccountID,
		TeamBotID: teamID, Mission: team.Mission, Text: text,
		RequestID: strings.TrimSpace(body.RequestID),
	})
	if err != nil {
		s.cfg.Access.AuditInvoke(r.Context(), tenantID, principal, captain, sessionID, "failed")
		writeStoreError(w, err)
		return
	}
	s.cfg.Access.AuditInvoke(r.Context(), tenantID, principal, captain, result.SessionID, "ok")
	writeJSON(w, http.StatusCreated, map[string]any{
		"teamBotId": teamID, "captainBotId": captain,
		"sessionId": result.SessionID.String(), "seq": result.Seq,
		"requestId": result.RequestID, "deduped": result.Deduped,
	})
}

// requireRosterInvoke checks every seat: the Bot must exist and the caller must
// hold bot.invoke on it. The denial is the engine's structured 403; it never
// names another account's Bot.
func (s *Server) requireRosterInvoke(w http.ResponseWriter, r *http.Request, principal access.Principal, tenantID uuid.UUID, roster []botteam.RosterEntry) bool {
	for _, seat := range roster {
		if _, err := s.cfg.Identity.GetBot(r.Context(), tenantID, seat.BotID); err != nil {
			writeStoreError(w, err)
			return false
		}
		decision, err := s.cfg.Access.Authorize(r.Context(), principal, access.KeyBotInvoke, access.Resource{
			TenantID: tenantID, Type: "bot", ID: seat.BotID,
		})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return false
		}
		if decision.Denied() {
			writeDenied(w, decision)
			return false
		}
	}
	return true
}
