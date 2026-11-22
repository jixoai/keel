// Bots HTTP surface (change keel-bot-identity-access, A4.1).
//
// A Bot is a runnable AI employee: Profile + ModelKind + default model +
// Harness. It belongs to the Profile's Account. This surface creates, reads,
// updates (CAS), archives, invokes and issues bot-bound credentials.
//
// Lists go through the engine's VisibleResources filter: a bot's owner gets it
// from the owner relation, an explicitly shared account from viewer/editor, and
// nothing else. There is no "scope tenant means list every bot" branch.
package harnessapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/botteam"
	"github.com/jixoai/keel/internal/store"
)

type botView struct {
	BotID        string           `json:"botId"`
	Name         string           `json:"name"`
	ProfileID    string           `json:"profileId"`
	AccountID    string           `json:"accountId,omitempty"`
	ModelKind    string           `json:"modelKind"`
	DefaultModel botteam.ModelRef `json:"defaultModel"`
	Harness      botteam.Harness  `json:"harness"`
	Archived     bool             `json:"archived"`
	CreatedAt    time.Time        `json:"createdAt"`
	UpdatedAt    time.Time        `json:"updatedAt"`
	// SharedCount is the number of viewer/editor tuples (accounts, groups and
	// team bots). The list badge reads it; the share dialog is the detail.
	SharedCount int `json:"sharedCount"`
}

// botViewOf projects a stored row. Account resolution is best effort: a dangling
// profile is a data problem the read should not hide behind a 500.
func (s *Server) botViewOf(r *http.Request, tenantID uuid.UUID, record store.BotRecord) botView {
	var model botteam.ModelRef
	_ = json.Unmarshal(record.DefaultModel, &model)
	var harness botteam.Harness
	_ = json.Unmarshal(record.Harness, &harness)
	view := botView{
		BotID: record.ID, Name: record.Name, ProfileID: record.ProfileID.String(),
		ModelKind: record.ModelKind, DefaultModel: model, Harness: harness,
		Archived: record.Archived, CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC(),
	}
	if profile, err := s.cfg.Identity.GetProfile(r.Context(), tenantID, record.ProfileID); err == nil {
		view.AccountID = profile.AccountID.String()
	}
	return view
}

// botViewWithSharedCount is the single-bot detail projection: the same view as
// the list, with the share badge resolved in one bounded query.
func (s *Server) botViewWithSharedCount(r *http.Request, tenantID uuid.UUID, record store.BotRecord) botView {
	view := s.botViewOf(r, tenantID, record)
	if counts, err := s.cfg.Identity.ResourceRelationCounts(r.Context(), tenantID, "bot",
		[]string{record.ID}, []string{store.RelationViewer, store.RelationEditor}); err == nil {
		view.SharedCount = counts[record.ID]
	}
	return view
}

// validateHarnessPermissions rejects a Harness narrowing that names a key the
// registry does not know. Storage would accept the string, but the Bot could
// never authenticate (NarrowToHarness fails closed), so the write is refused
// at the edge with the offending key.
func validateHarnessPermissions(permissions []string) error {
	for _, raw := range permissions {
		key := strings.TrimSpace(raw)
		if err := access.ValidateKey(key); err != nil {
			return fmt.Errorf("harness permission: %w", err)
		}
	}
	return nil
}

// validateHarnessTools refuses a tool id the deployment does not serve. The
// Harness is an effective whitelist (registry ∩ whitelist), so an entry the
// registry cannot honour would silently disappear at run time; the save is the
// honest place to name it. A deployment without the seam keeps shape-only
// validation (the run path still logs the unavailable entry).
func (s *Server) validateHarnessTools(tools []string) error {
	if s.cfg.ToolExists == nil {
		return nil
	}
	fields := make([]botteam.FieldError, 0)
	for index, raw := range tools {
		id := strings.TrimSpace(raw)
		if id == "" || s.cfg.ToolExists(id) {
			continue
		}
		fields = append(fields, botteam.FieldError{
			Field:   fmt.Sprintf("harness.tools[%d]", index),
			Code:    botteam.CodeMissingTool,
			Message: fmt.Sprintf("tool %q is not served by this deployment", id),
		})
	}
	if len(fields) == 0 {
		return nil
	}
	return &botteam.ValidationError{Errors: fields}
}

func (s *Server) listBots(w http.ResponseWriter, r *http.Request) {
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
	// Read the bounded candidate window, filter by relation, then apply the page
	// limit: limiting before the filter could return an empty page while a later
	// candidate in the same window is visible.
	records, err := s.cfg.Identity.ListBots(r.Context(), tenantID, identityFlag(r, "includeArchived"), maxIdentityPageLimit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	candidates := make([]string, 0, len(records))
	for _, record := range records {
		candidates = append(candidates, record.ID)
	}
	visible, err := s.cfg.Access.Visible(r.Context(), principal, access.KeyBotRead, "bot", candidates)
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
	views := make([]botView, 0, len(visible.IDs))
	viewIDs := make([]string, 0, len(visible.IDs))
	for _, record := range records {
		if allowed[record.ID] {
			views = append(views, s.botViewOf(r, tenantID, record))
			viewIDs = append(viewIDs, record.ID)
		}
	}
	s.setBotSharedCounts(r.Context(), tenantID, views, viewIDs)
	if len(views) > limit {
		views = views[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"bots": views})
}

// setBotSharedCounts fills the list badge in one query. A count failure is not
// a list failure: the badge is additive, the rows are the server's truth.
func (s *Server) setBotSharedCounts(ctx context.Context, tenantID uuid.UUID, views []botView, ids []string) {
	counts, err := s.cfg.Identity.ResourceRelationCounts(ctx, tenantID, "bot", ids,
		[]string{store.RelationViewer, store.RelationEditor})
	if err != nil {
		return
	}
	for index := range views {
		views[index].SharedCount = counts[views[index].BotID]
	}
}

type botWriteRequest struct {
	BotID            string            `json:"botId"`
	Name             string            `json:"name"`
	ProfileID        string            `json:"profileId"`
	ModelKind        string            `json:"modelKind"`
	DefaultModel     *botteam.ModelRef `json:"defaultModel"`
	Harness          *botteam.Harness  `json:"harness"`
	Archived         *bool             `json:"archived"`
	ExpectedArchived *bool             `json:"expectedArchived"`
}

// botRecordFrom validates a create/update request into a store record.
func botRecordFrom(tenantID uuid.UUID, request botWriteRequest, existing *store.BotRecord) (store.BotRecord, error) {
	record := store.BotRecord{TenantID: tenantID}
	if existing != nil {
		record = *existing
		// A bot handle is its stable identity; a PATCH may repeat it but never
		// change it (the path is the authority).
		if id := strings.TrimSpace(request.BotID); id != "" && id != existing.ID {
			return store.BotRecord{}, errors.New("botId cannot be changed")
		}
	} else if id := strings.TrimSpace(request.BotID); id != "" {
		record.ID = id
	}
	if record.ID == "" {
		record.ID = botteam.GenerateBotID()
	}
	if name := strings.TrimSpace(request.Name); name != "" {
		record.Name = name
	}
	if raw := strings.TrimSpace(request.ProfileID); raw != "" {
		profileID, err := uuid.Parse(raw)
		if err != nil {
			return store.BotRecord{}, errors.New("profileId must be a UUID")
		}
		record.ProfileID = profileID
	}
	if kind := strings.TrimSpace(request.ModelKind); kind != "" {
		record.ModelKind = kind
	}
	if request.DefaultModel != nil {
		raw, err := json.Marshal(request.DefaultModel)
		if err != nil {
			return store.BotRecord{}, err
		}
		record.DefaultModel = raw
	}
	if request.Harness != nil {
		raw, err := json.Marshal(request.Harness)
		if err != nil {
			return store.BotRecord{}, err
		}
		record.Harness = raw
	}
	if request.Archived != nil {
		if existing != nil && *request.Archived && !existing.Archived {
			// Archiving through the update write is allowed exactly once and never
			// reversed here; the audit still names it.
			record.Archived = true
		}
		if existing != nil && !*request.Archived && existing.Archived {
			return store.BotRecord{}, errors.New("an archived bot cannot be unarchived by update")
		}
	}
	if existing == nil && request.Archived != nil {
		record.Archived = *request.Archived
	}
	return record, nil
}

func (s *Server) createBot(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	principal, tenantID, ok := s.requireKey(w, r, access.KeyBotCreate)
	if !ok {
		return
	}
	var body botWriteRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	record, err := botRecordFrom(tenantID, body, nil)
	if err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if record.Archived {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", "a bot cannot be created archived")
		return
	}
	domain := botteam.Bot{
		ID: record.ID, Name: record.Name, ProfileID: record.ProfileID,
		ModelKind: botteam.ModelKind(record.ModelKind),
	}
	_ = json.Unmarshal(record.DefaultModel, &domain.DefaultModel)
	_ = json.Unmarshal(record.Harness, &domain.Harness)
	if err := domain.Validate(); err != nil {
		writeValidationError(w, err)
		return
	}
	if err := s.validateHarnessTools(domain.Harness.Tools); err != nil {
		writeValidationError(w, err)
		return
	}
	if err := validateHarnessPermissions(domain.Harness.Context.Permissions); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	profile, err := s.cfg.Identity.GetProfile(r.Context(), tenantID, record.ProfileID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "profileId does not exist in this tenant", "code": "invalid_request",
			"fields": []botteam.FieldError{{
				Field: "profileId", Code: botteam.CodeMissingProfile,
				Message: "profile " + record.ProfileID.String() + " does not exist in this tenant",
			}},
		})
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// The Bot and its owner relation are one transaction: the owner is the
	// Profile's Account (the account that will run it), not necessarily the
	// creator. No Bot can exist without the relation the resource gate needs.
	created, err := s.cfg.Identity.CreateBotWithOwner(r.Context(), record, profile.AccountID, principal.AccountID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.cfg.Access.AuditRelation(r.Context(), tenantID, principal.AccountID, "create", "bot", created.ID,
		store.RelationOwner, store.RelationSubjectAccount, profile.AccountID.String()); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if principal.IsSuperAdmin {
		if err := s.cfg.Access.AuditSuperAdmin(r.Context(), principal, "bot.create", map[string]any{
			"bot_id": created.ID, "account_id": profile.AccountID.String(),
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	s.identityChanged(tenantID, IdentityEventBots)
	writeJSON(w, http.StatusCreated, map[string]any{
		"bot":            s.botViewWithSharedCount(r, tenantID, created),
		"ownerAccountId": profile.AccountID.String(),
	})
}

func (s *Server) getBot(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	botID := strings.TrimSpace(r.PathValue("id"))
	if botID == "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_bot_id", "bot id is required")
		return
	}
	if _, _, ok := s.requireResource(w, r, access.KeyBotRead, "bot", botID); !ok {
		return
	}
	tenantID, _ := tenantFrom(w, r)
	record, err := s.cfg.Identity.GetBot(r.Context(), tenantID, botID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bot": s.botViewWithSharedCount(r, tenantID, record)})
}

func (s *Server) updateBot(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	botID := strings.TrimSpace(r.PathValue("id"))
	if botID == "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_bot_id", "bot id is required")
		return
	}
	principal, tenantID, ok := s.requireResource(w, r, access.KeyBotManage, "bot", botID)
	if !ok {
		return
	}
	var body botWriteRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	current, err := s.cfg.Identity.GetBot(r.Context(), tenantID, botID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// Rebinding the Profile changes which Account owns the Bot; that is the
	// bot.bind authority, not ordinary manage.
	profileChanged := strings.TrimSpace(body.ProfileID) != "" &&
		strings.TrimSpace(body.ProfileID) != current.ProfileID.String()
	if profileChanged {
		if _, _, ok := s.requireResource(w, r, access.KeyBotBind, "bot", botID); !ok {
			return
		}
	}
	record, err := botRecordFrom(tenantID, body, &current)
	if err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	domain := botteam.Bot{
		ID: record.ID, Name: record.Name, ProfileID: record.ProfileID,
		ModelKind: botteam.ModelKind(record.ModelKind),
	}
	_ = json.Unmarshal(record.DefaultModel, &domain.DefaultModel)
	_ = json.Unmarshal(record.Harness, &domain.Harness)
	if err := domain.Validate(); err != nil {
		writeValidationError(w, err)
		return
	}
	if err := s.validateHarnessTools(domain.Harness.Tools); err != nil {
		writeValidationError(w, err)
		return
	}
	if err := validateHarnessPermissions(domain.Harness.Context.Permissions); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var newOwner, oldOwner uuid.UUID
	if profileChanged {
		profile, err := s.cfg.Identity.GetProfile(r.Context(), tenantID, record.ProfileID)
		if errors.Is(err, store.ErrNotFound) {
			writeIdentityError(w, http.StatusBadRequest, "invalid_profile", "profileId does not exist in this tenant")
			return
		}
		if err != nil {
			writeStoreError(w, err)
			return
		}
		oldProfile, err := s.cfg.Identity.GetProfile(r.Context(), tenantID, current.ProfileID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		newOwner, oldOwner = profile.AccountID, oldProfile.AccountID
	}
	expectedArchived := current.Archived
	if body.ExpectedArchived != nil {
		expectedArchived = *body.ExpectedArchived
	}
	// A Profile rebind moves ownership; the content CAS and the relation move are
	// one transaction, so a stale update can never hand the Bot to another
	// account or leave two owners.
	var updated store.BotRecord
	if profileChanged {
		updated, err = s.cfg.Identity.UpdateBotWithOwner(r.Context(), record, expectedArchived,
			newOwner, oldOwner, principal.AccountID)
	} else {
		updated, err = s.cfg.Identity.UpdateBot(r.Context(), record, expectedArchived)
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if profileChanged && newOwner != oldOwner {
		if err := s.cfg.Access.AuditRelation(r.Context(), tenantID, principal.AccountID, "update", "bot", botID,
			store.RelationOwner, store.RelationSubjectAccount, newOwner.String()); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	if principal.IsSuperAdmin {
		if err := s.cfg.Access.AuditSuperAdmin(r.Context(), principal, "bot.update", map[string]any{"bot_id": botID}); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	s.identityChanged(tenantID, IdentityEventBots)
	writeJSON(w, http.StatusOK, map[string]any{"bot": s.botViewWithSharedCount(r, tenantID, updated)})
}

func (s *Server) archiveBot(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	botID := strings.TrimSpace(r.PathValue("id"))
	if botID == "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_bot_id", "bot id is required")
		return
	}
	principal, tenantID, ok := s.requireResource(w, r, access.KeyBotManage, "bot", botID)
	if !ok {
		return
	}
	if err := s.cfg.Identity.ArchiveBot(r.Context(), tenantID, botID); err != nil {
		writeStoreError(w, err)
		return
	}
	if principal.IsSuperAdmin {
		if err := s.cfg.Access.AuditSuperAdmin(r.Context(), principal, "bot.archive", map[string]any{"bot_id": botID}); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	s.identityChanged(tenantID, IdentityEventBots)
	writeJSON(w, http.StatusOK, map[string]any{"botId": botID, "archived": true})
}

type botInvokeRequest struct {
	Text      string `json:"text"`
	SessionID string `json:"sessionId"`
	RequestID string `json:"requestId"`
}

func (s *Server) invokeBot(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	botID := strings.TrimSpace(r.PathValue("id"))
	if botID == "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_bot_id", "bot id is required")
		return
	}
	principal, tenantID, ok := s.requireResource(w, r, access.KeyBotInvoke, "bot", botID)
	if !ok {
		return
	}
	if s.cfg.StartBotSession == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("bot invocation is not wired in this deployment"))
		return
	}
	var body botInvokeRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", "text is required")
		return
	}
	record, err := s.cfg.Identity.GetBot(r.Context(), tenantID, botID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if record.Archived {
		writeIdentityError(w, http.StatusConflict, "bot_archived", "an archived bot refuses new sessions")
		return
	}
	profile, err := s.cfg.Identity.GetProfile(r.Context(), tenantID, record.ProfileID)
	if err != nil {
		writeStoreError(w, err)
		return
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
		TenantID: tenantID, SessionID: sessionID, BotID: record.ID,
		AccountID: profile.AccountID, ActorAccountID: principal.AccountID,
		Text: text, RequestID: strings.TrimSpace(body.RequestID),
	})
	if err != nil {
		// The invocation failed; the audit is still required. A failure to write
		// it is reported (500) instead of being hidden behind the original error.
		if auditErr := s.cfg.Access.AuditInvoke(r.Context(), tenantID, principal, record.ID, sessionID, "failed"); auditErr != nil {
			writeErr(w, http.StatusInternalServerError, auditErr)
			return
		}
		writeStoreError(w, err)
		return
	}
	if err := s.cfg.Access.AuditInvoke(r.Context(), tenantID, principal, record.ID, result.SessionID, "ok"); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.identityChanged(tenantID, IdentityEventSessions)
	writeJSON(w, http.StatusCreated, map[string]any{
		"botId": record.ID, "sessionId": result.SessionID.String(), "seq": result.Seq,
		"requestId": result.RequestID, "deduped": result.Deduped,
	})
}

type botKeyView struct {
	KeyID      string     `json:"keyId"`
	Prefix     string     `json:"prefix"`
	AccountID  string     `json:"accountId"`
	BotID      string     `json:"botId"`
	Label      string     `json:"label"`
	CreatedAt  time.Time  `json:"createdAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
}

func botKeyViewOf(key store.BotAPIKey) botKeyView {
	return botKeyView{
		KeyID: key.KeyID.String(), Prefix: key.Prefix, AccountID: key.AccountID.String(),
		BotID: key.BotID, Label: key.Label, CreatedAt: key.CreatedAt.UTC(),
		RevokedAt: key.RevokedAt, LastUsedAt: key.LastUsedAt,
	}
}

func (s *Server) listBotKeys(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	botID := strings.TrimSpace(r.PathValue("id"))
	if botID == "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_bot_id", "bot id is required")
		return
	}
	_, tenantID, ok := s.requireResource(w, r, access.KeyBotManage, "bot", botID)
	if !ok {
		return
	}
	if _, err := s.cfg.Identity.GetBot(r.Context(), tenantID, botID); err != nil {
		writeStoreError(w, err)
		return
	}
	keys, err := s.cfg.Identity.ListBotAPIKeys(r.Context(), tenantID, botID, maxIdentityPageLimit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]botKeyView, 0, len(keys))
	for _, key := range keys {
		views = append(views, botKeyViewOf(key))
	}
	writeJSON(w, http.StatusOK, map[string]any{"botId": botID, "keys": views})
}

type issueBotKeyRequest struct {
	AccountID string `json:"accountId"`
	Label     string `json:"label"`
}

func (s *Server) issueBotKey(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	botID := strings.TrimSpace(r.PathValue("id"))
	if botID == "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_bot_id", "bot id is required")
		return
	}
	principal, tenantID, ok := s.requireResource(w, r, access.KeyBotManage, "bot", botID)
	if !ok {
		return
	}
	var body issueBotKeyRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	owner, archived, err := s.cfg.Access.BotOwnerAccount(r.Context(), tenantID, botID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if archived {
		writeIdentityError(w, http.StatusConflict, "bot_archived", "an archived bot cannot receive a credential")
		return
	}
	accountID := owner
	if raw := strings.TrimSpace(body.AccountID); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			writeIdentityError(w, http.StatusBadRequest, "invalid_account_id", "accountId must be a UUID")
			return
		}
		if parsed != owner {
			s.cfg.Access.Denied(r.Context(), principal, access.Decision{
				Allow: false, Reason: access.ReasonMissingRelation, Key: access.KeyBotManage,
				Detail: "the key's account must be the bot's owner account",
			}, access.Resource{TenantID: tenantID, Type: "bot", ID: botID})
			writeIdentityError(w, http.StatusForbidden, "key_account_mismatch",
				"a bot key must bind to the bot's owner account")
			return
		}
		accountID = parsed
	}
	minted, err := auth.MintBotKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	key, err := s.cfg.Identity.CreateBotAPIKey(r.Context(), tenantID, uuid.New(), minted.Hash,
		minted.Prefix, accountID, botID, strings.TrimSpace(body.Label))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.cfg.Access.AuditBotKey(r.Context(), tenantID, principal.AccountID, accountID, botID,
		"issue", key.KeyID.String(), key.Prefix, key.Label); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.identityChanged(tenantID, IdentityEventBots)
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":       botKeyViewOf(key),
		"plaintext": minted.Plaintext,
		"notice":    "this key is shown once; it cannot be read again",
	})
}

func (s *Server) revokeBotKey(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	botID := strings.TrimSpace(r.PathValue("id"))
	if botID == "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_bot_id", "bot id is required")
		return
	}
	principal, tenantID, ok := s.requireResource(w, r, access.KeyBotManage, "bot", botID)
	if !ok {
		return
	}
	keyID, ok := pathUUID(w, r, "keyId", "invalid_key_id")
	if !ok {
		return
	}
	key, err := s.cfg.Identity.GetBotAPIKey(r.Context(), tenantID, keyID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// A key of another bot answers exactly like a missing one.
	if key.BotID != botID {
		writeIdentityError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if err := s.cfg.Identity.RevokeBotAPIKey(r.Context(), tenantID, keyID); err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.cfg.Access.AuditBotKey(r.Context(), tenantID, principal.AccountID, key.AccountID, botID,
		"revoke", key.KeyID.String(), key.Prefix, key.Label); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.identityChanged(tenantID, IdentityEventBots)
	writeJSON(w, http.StatusOK, map[string]any{"botId": botID, "keyId": keyID.String(), "revoked": true})
}
