// Audit-ledger read surface (change keel-bot-identity-access, A4.4c).
//
// GET /v1/audit/events and GET /v1/audit/events/{id} are the read behind the 05
// 记录 page. The ledger itself is auth_events (A2/A4.1): this file is a
// projection, never a second store, and it never writes.
//
// Three rules drive the shape:
//
//   - audit.read (tenant scope) is the only key. The route matrix and this
//     handler both demand it; a deployment without the access seam fails closed.
//   - the wire matches web/src/data/identity.ts's AuditEventView exactly:
//     eventId/occurredAt/kind/action/summary/actor*/object*/result/requestId/
//     source/capabilityKey/objectOwner*/grantableBy/teamId/runId. `detail` is
//     included (redacted) because the client reads structured fallbacks from it.
//   - a row's detail may contain anything a writer put there, so it crosses
//     logsafe.RedactValue and an extra hash/ip scrub before serialization. No
//     password, token, key or hash value is returned; sensitive keys become
//     "[REDACTED]".
//
// Cross-tenant reads are explicit: the request's X-Tenant-ID is the tenant
// boundary, and only a deployment super admin may name another tenant with
// ?tenantId=. An ordinary account that names another tenant gets the same
// structured 403 the access engine writes; a row id from another tenant answers
// 404, never 403-with-existence.
package harnessapi

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/logsafe"
	"github.com/jixoai/keel/internal/store"
)

const (
	defaultAuditPageLimit = 100
	maxAuditPageLimit     = 200
	// maxAuditRange bounds one read's time window. The ledger is append-only and
	// older rows stay queryable in explicit slices; the request path refuses an
	// unbounded export.
	maxAuditRange = 90 * 24 * time.Hour
	// maxAuditQueryLength bounds ?q= (bytes). A longer term is a client bug.
	maxAuditQueryLength = 256
)

// auditKindRawKinds maps the page's display buckets to the raw auth_events kind
// tokens they cover. It is the single vocabulary used by both the response's
// `kind` field and the ?kind= filter, so a bucket never disagrees with itself.
var auditKindRawKinds = map[string][]string{
	"login": {
		store.AuthEventLoginSucceeded, store.AuthEventLoginFailed,
		store.AuthEventLoginRateLimit, store.AuthEventSessionReject,
		store.AuthEventLogout,
	},
	"grant": {
		store.AuthEventPermissionGrant, store.AuthEventPermissionRevoke,
	},
	"deny": {
		store.AuthEventPermissionDenied,
	},
	"super-admin": {
		store.AuthEventSuperAdmin,
	},
	"relation": {
		store.AuthEventRelationChanged,
	},
	"invoke": {
		store.AuthEventBotInvoked,
	},
	"account": {
		store.AuthEventPasswordChange, store.AuthEventPasswordReset,
		store.AuthEventAccountCreated, store.AuthEventAccountDisable,
		store.AuthEventAccountEnable, store.AuthEventAccountBoot,
		store.AuthEventBootstrapToken, store.AuthEventBootstrapUsed,
		store.AuthEventBotKeyIssued, store.AuthEventBotKeyRevoked,
	},
}

// auditResultRawResults maps the page's three result words to the raw values
// auth_events stores. A token outside the map is treated as a raw result.
var auditResultRawResults = map[string][]string{
	"ok":     {store.AuthResultOK},
	"denied": {store.AuthResultDenied},
	"error":  {store.AuthResultFailed, store.AuthResultRefused},
}

// auditEventView is the wire shape web/src/data/identity.ts parses. Optional
// fields are omitted rather than invented; the client keeps an absent field
// absent instead of filling in a guess.
type auditEventView struct {
	EventID    string    `json:"eventId"`
	OccurredAt time.Time `json:"occurredAt"`
	Kind       string    `json:"kind"`
	Action     string    `json:"action"`
	Summary    string    `json:"summary,omitempty"`

	ActorName string `json:"actorName,omitempty"`
	ActorKind string `json:"actorKind,omitempty"`
	ActorID   string `json:"actorId,omitempty"`

	// The row's subject account (account_id). The Web contract names the actor
	// and the object; the subject is additive and lets a client cross-check the
	// account index without re-deriving it from detail.
	AccountID   string `json:"accountId,omitempty"`
	AccountName string `json:"accountName,omitempty"`

	ObjectType string `json:"objectType,omitempty"`
	ObjectName string `json:"objectName,omitempty"`
	ObjectID   string `json:"objectId,omitempty"`

	Result    string `json:"result"`
	RequestID string `json:"requestId,omitempty"`
	Source    string `json:"source,omitempty"`

	CapabilityKey   string   `json:"capabilityKey,omitempty"`
	ObjectOwnerName string   `json:"objectOwnerName,omitempty"`
	ObjectOwnerID   string   `json:"objectOwnerId,omitempty"`
	GrantableBy     []string `json:"grantableBy,omitempty"`

	TeamID string `json:"teamId,omitempty"`
	RunID  string `json:"runId,omitempty"`

	// Detail is the redacted structured row. The client reads capabilityKey,
	// objectOwner*, grantableBy and id fallbacks from it.
	Detail map[string]any `json:"detail"`
}

// auditRoutes mounts the read-only audit surface.
func (s *Server) auditRoutes() {
	s.mux.HandleFunc("GET /v1/audit/events", s.listAuditEvents)
	s.mux.HandleFunc("GET /v1/audit/events/{id}", s.getAuditEvent)
}

// listAuditEvents implements GET /v1/audit/events.
func (s *Server) listAuditEvents(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	principal, tenantID, ok := s.requireKey(w, r, access.KeyAuditRead)
	if !ok {
		return
	}
	target, ok := s.auditTargetTenant(w, r, principal, tenantID)
	if !ok {
		return
	}
	query, err := parseAuditQuery(r)
	if err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	page, err := s.cfg.Identity.QueryAuditEvents(r.Context(), target, store.AuditReadFilter{
		From:      query.from,
		To:        query.to,
		Kinds:     query.kinds,
		Results:   query.results,
		AccountID: query.accountID,
		Query:     query.q,
		After:     query.after,
		Limit:     query.limit,
	})
	if err != nil {
		// ErrInvalid (a malformed cursor) maps to 400 through writeStoreError.
		writeStoreError(w, err)
		return
	}
	events := make([]auditEventView, 0, len(page.Events))
	for _, record := range page.Events {
		events = append(events, auditEventViewOf(record))
	}
	s.attachGrantableHints(r.Context(), target, events)
	writeJSON(w, http.StatusOK, map[string]any{
		"events":     events,
		"nextCursor": page.NextCursor,
		"hasMore":    page.HasMore,
	})
}

// getAuditEvent implements GET /v1/audit/events/{id}.
func (s *Server) getAuditEvent(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	principal, tenantID, ok := s.requireKey(w, r, access.KeyAuditRead)
	if !ok {
		return
	}
	target, ok := s.auditTargetTenant(w, r, principal, tenantID)
	if !ok {
		return
	}
	eventID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil || eventID <= 0 {
		writeIdentityError(w, http.StatusBadRequest, "invalid_event_id", "id must be a positive event id")
		return
	}
	record, err := s.cfg.Identity.GetAuditEventByID(r.Context(), target, eventID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	view := auditEventViewOf(record)
	views := []auditEventView{view}
	s.attachGrantableHints(r.Context(), target, views)
	writeJSON(w, http.StatusOK, map[string]any{"event": views[0]})
}

// auditTargetTenant applies the cross-tenant rule: the header tenant is the
// boundary; naming another tenant requires the deployment super-admin flag.
// The refusal is audited exactly like a key-level denial.
func (s *Server) auditTargetTenant(w http.ResponseWriter, r *http.Request,
	principal access.Principal, headerTenant uuid.UUID) (uuid.UUID, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if raw == "" {
		return headerTenant, true
	}
	target, err := uuid.Parse(raw)
	if err != nil || target == uuid.Nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_tenant_id", "tenantId must be a UUID")
		return uuid.Nil, false
	}
	if target != headerTenant && !principal.IsSuperAdmin {
		decision := access.Decision{
			Allow: false, Reason: access.ReasonMissingGrant, Key: access.KeyAuditRead,
			Detail: "audit.read at tenant scope does not reach another tenant",
		}
		s.cfg.Access.Denied(r.Context(), principal, decision, access.Resource{TenantID: headerTenant})
		writeDenied(w, decision)
		return uuid.Nil, false
	}
	return target, true
}

// auditQuery is one parsed, bounded request. The handler validates every value;
// the store clamps again defensively.
type auditQuery struct {
	from, to  time.Time
	kinds     []string
	results   []string
	accountID uuid.UUID
	q         string
	after     string
	limit     int
}

func parseAuditQuery(r *http.Request) (auditQuery, error) {
	query := r.URL.Query()
	limit, err := queryInt(r, "limit", defaultAuditPageLimit, 1, maxAuditPageLimit)
	if err != nil {
		return auditQuery{}, err
	}
	fromRaw := strings.TrimSpace(query.Get("from"))
	if fromRaw == "" {
		// The Web client's AuditQuery calls the lower bound `since`; accept both
		// spellings with `from` winning when both are sent.
		fromRaw = strings.TrimSpace(query.Get("since"))
	}
	from, err := parseAuditTimestamp(fromRaw)
	if err != nil {
		return auditQuery{}, fmt.Errorf("from must be RFC3339 or unix seconds: %w", err)
	}
	to, err := parseAuditTimestamp(strings.TrimSpace(query.Get("to")))
	if err != nil {
		return auditQuery{}, fmt.Errorf("to must be RFC3339 or unix seconds: %w", err)
	}
	now := time.Now().UTC()
	if to.IsZero() {
		to = now
	}
	if from.IsZero() {
		from = to.Add(-maxAuditRange)
	}
	if from.After(to) {
		return auditQuery{}, fmt.Errorf("from must not be after to")
	}
	if to.Sub(from) > maxAuditRange {
		return auditQuery{}, fmt.Errorf("the time range must not exceed %d days", int(maxAuditRange/(24*time.Hour)))
	}

	accountID := uuid.Nil
	if raw := strings.TrimSpace(query.Get("accountId")); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil || parsed == uuid.Nil {
			return auditQuery{}, fmt.Errorf("accountId must be a UUID")
		}
		accountID = parsed
	}
	after := strings.TrimSpace(query.Get("cursor"))
	if after == "" {
		after = strings.TrimSpace(query.Get("after"))
	}
	q := strings.TrimSpace(query.Get("q"))
	if len(q) > maxAuditQueryLength {
		return auditQuery{}, fmt.Errorf("q must be at most %d bytes", maxAuditQueryLength)
	}
	return auditQuery{
		from: from, to: to,
		kinds:     auditKindFilters(query.Get("kind")),
		results:   auditResultFilters(query.Get("result")),
		accountID: accountID, q: q, after: after, limit: limit,
	}, nil
}

// parseAuditTimestamp accepts RFC3339/RFC3339Nano or a unix timestamp. A
// 13-or-more digit integer is milliseconds (JavaScript's Date.now shape); a
// shorter one is seconds. An empty value is the zero time.
func parseAuditTimestamp(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if value, err := strconv.ParseInt(raw, 10, 64); err == nil {
		digits := strings.TrimPrefix(raw, "-")
		if len(digits) >= 13 {
			return time.UnixMilli(value).UTC(), nil
		}
		return time.Unix(value, 0).UTC(), nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported timestamp %q", raw)
}

// splitAuditTokens splits a comma-separated filter, accepting the full-width
// comma and the Chinese enumeration comma the UI may produce.
func splitAuditTokens(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '，' || r == '、'
	})
}

// auditKindFilters turns display buckets and raw kind tokens into the exact
// raw kinds the store matches. A token outside the bucket vocabulary is a raw
// kind (forward compatibility with a writer this read has not learned yet).
func auditKindFilters(value string) []string {
	out := make([]string, 0, 8)
	for _, token := range splitAuditTokens(value) {
		token = strings.ToLower(strings.TrimSpace(token))
		if token == "" || token == "all" {
			continue
		}
		if raws, ok := auditKindRawKinds[token]; ok {
			out = append(out, raws...)
			continue
		}
		out = append(out, token)
	}
	return uniqueSorted(out)
}

// auditResultFilters maps ok/denied/error to the raw result values and passes
// any other token through as a raw result.
func auditResultFilters(value string) []string {
	out := make([]string, 0, 4)
	for _, token := range splitAuditTokens(value) {
		token = strings.ToLower(strings.TrimSpace(token))
		if token == "" || token == "all" {
			continue
		}
		if raws, ok := auditResultRawResults[token]; ok {
			out = append(out, raws...)
			continue
		}
		out = append(out, token)
	}
	return uniqueSorted(out)
}

// auditKindView maps a raw kind to the page's display bucket. It mirrors
// auditKindRawKinds; a raw kind outside the map is "other" and keeps its action
// token, exactly as the client's parser expects.
func auditKindView(raw string) string {
	switch {
	case raw == store.AuthEventPermissionDenied:
		return "deny"
	case raw == store.AuthEventPermissionGrant || raw == store.AuthEventPermissionRevoke:
		return "grant"
	case raw == store.AuthEventSuperAdmin:
		return "super-admin"
	case raw == store.AuthEventRelationChanged:
		return "relation"
	case raw == store.AuthEventBotInvoked:
		return "invoke"
	case strings.HasPrefix(raw, "login.") ||
		raw == store.AuthEventSessionReject || raw == store.AuthEventLogout:
		return "login"
	case strings.HasPrefix(raw, "account.") ||
		strings.HasPrefix(raw, "bootstrap.") ||
		strings.HasPrefix(raw, "bot_key.") ||
		raw == store.AuthEventPasswordChange || raw == store.AuthEventPasswordReset:
		return "account"
	case strings.HasPrefix(raw, "team.") || strings.HasPrefix(raw, "team_bot.") ||
		strings.HasPrefix(raw, "team_run."):
		return "team"
	default:
		return "other"
	}
}

// auditResultView maps the stored outcome to the client's closed set:
// ok/denied/error. failed and refused both read as error; an unknown value is
// an error, never a silently safer ok.
func auditResultView(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case store.AuthResultOK:
		return "ok"
	case store.AuthResultDenied:
		return "denied"
	default:
		return "error"
	}
}

// auditEventViewOf projects one stored row into the Web contract.
func auditEventViewOf(record store.AuditEventRecord) auditEventView {
	detail := redactAuditDetail(record.Detail)
	actorID, actorName, actorKind := auditActorOf(record)
	accountID, accountName := auditSubjectOf(record)
	objectType, objectID, objectName := auditObjectOf(record)
	ownerID, ownerName := auditOwnerOf(objectType, record)
	capabilityKey := auditDetailText(record.Detail,
		"required_key", "requiredKey", "capability_key", "capabilityKey", "key")
	requestID := auditDetailText(record.Detail, "request_id", "requestId")
	source := auditDetailText(record.Detail, "source")

	view := auditEventView{
		EventID:         strconv.FormatInt(record.EventID, 10),
		OccurredAt:      record.CreatedAt.UTC(),
		Kind:            auditKindView(record.Kind),
		Action:          record.Kind,
		Summary:         auditSummaryOf(record, objectName),
		ActorName:       logsafe.Redact(actorName),
		ActorKind:       actorKind,
		ActorID:         actorID,
		AccountID:       accountID,
		AccountName:     logsafe.Redact(accountName),
		ObjectType:      objectType,
		ObjectName:      logsafe.Redact(objectName),
		ObjectID:        objectID,
		Result:          auditResultView(record.Result),
		RequestID:       logsafe.Redact(requestID),
		Source:          logsafe.Redact(source),
		CapabilityKey:   capabilityKey,
		ObjectOwnerName: logsafe.Redact(ownerName),
		ObjectOwnerID:   ownerID,
		TeamID:          auditTeamIDOf(record),
		RunID:           auditRunIDOf(record),
		Detail:          detail,
	}
	return view
}

// auditActorOf resolves the acting account. actor_account_id wins; a row with
// only account_id (a login) treats the subject as the actor; a failed login
// that names no account keeps the attempted username from detail.
func auditActorOf(record store.AuditEventRecord) (string, string, string) {
	actor := record.Actor
	if actor == nil {
		actor = record.Subject
	}
	if actor != nil {
		return actor.ID.String(), actor.Name(), actor.Kind
	}
	id := ""
	if record.ActorAccountID != nil {
		id = record.ActorAccountID.String()
	} else if record.AccountID != nil {
		id = record.AccountID.String()
	}
	if username := auditDetailText(record.Detail, "username"); username != "" {
		return id, username, "human"
	}
	if label := auditDetailText(record.Detail, "actor"); label != "" {
		return id, label, "service"
	}
	return id, "", ""
}

// auditSubjectOf resolves the row's subject account. A deleted account leaves
// only the id (auth_events deliberately has no FK), which is still useful for
// a link; the name falls back to the username the detail recorded.
func auditSubjectOf(record store.AuditEventRecord) (string, string) {
	if record.Subject != nil {
		return record.Subject.ID.String(), record.Subject.Name()
	}
	if record.AccountID == nil {
		return "", ""
	}
	return record.AccountID.String(), auditDetailText(record.Detail, "username")
}

// auditObjectOf names the object the row acted on. The mapping is explicit per
// kind so a detail key that happens to exist cannot invent a link.
func auditObjectOf(record store.AuditEventRecord) (string, string, string) {
	detail := record.Detail
	resourceType := auditDetailText(detail, "resource_type")
	resourceID := auditDetailText(detail, "resource_id")
	var objectType, objectID, objectName string

	switch record.Kind {
	case store.AuthEventBotInvoked, store.AuthEventBotKeyIssued, store.AuthEventBotKeyRevoked:
		objectType, objectID = "Bot", auditDetailText(detail, "bot_id")
	case store.AuthEventPermissionGrant, store.AuthEventPermissionRevoke:
		// A membership write targets an account; a grant write targets a group
		// (AuditGrant leaves account_id NULL for a group grant).
		if groupID := auditDetailText(detail, "group_id"); groupID != "" {
			objectType, objectID = "权限组", groupID
		} else if record.AccountID != nil {
			objectType, objectID = "账号", record.AccountID.String()
		}
	case store.AuthEventPermissionDenied, store.AuthEventRelationChanged:
		objectType = auditObjectType(resourceType)
		objectID = resourceID
	case store.AuthEventSuperAdmin:
		switch {
		case auditDetailText(detail, "bot_id") != "":
			objectType, objectID = "Bot", auditDetailText(detail, "bot_id")
		case auditDetailText(detail, "team_bot_id") != "":
			objectType, objectID = "团队", auditDetailText(detail, "team_bot_id")
		case auditDetailText(detail, "group_id") != "":
			objectType, objectID = "权限组", auditDetailText(detail, "group_id")
		case auditDetailText(detail, "target_account_id") != "":
			objectType, objectID = "账号", auditDetailText(detail, "target_account_id")
		}
	case store.AuthEventAccountCreated, store.AuthEventAccountDisable,
		store.AuthEventAccountEnable, store.AuthEventAccountBoot,
		store.AuthEventPasswordChange, store.AuthEventPasswordReset,
		store.AuthEventLoginSucceeded, store.AuthEventLoginFailed,
		store.AuthEventLoginRateLimit, store.AuthEventSessionReject,
		store.AuthEventLogout, store.AuthEventBootstrapUsed:
		if record.AccountID != nil {
			objectType, objectID = "账号", record.AccountID.String()
		}
	}
	if objectType == "" && resourceType != "" {
		objectType, objectID = auditObjectType(resourceType), resourceID
	}
	objectName = auditObjectNameOf(objectType, objectID, resourceType, record)
	return objectType, objectID, objectName
}

func auditObjectType(resourceType string) string {
	switch strings.TrimSpace(resourceType) {
	case "":
		return ""
	case "bot":
		return "Bot"
	case "team_bot":
		return "团队"
	case "permission_group":
		return "权限组"
	case "account":
		return "账号"
	case "session":
		return "会话"
	default:
		return resourceType
	}
}

func auditObjectNameOf(objectType, objectID, resourceType string, record store.AuditEventRecord) string {
	if objectID == "" {
		return ""
	}
	switch objectType {
	case "Bot":
		return firstNonEmpty(record.BotName, objectID)
	case "团队":
		return firstNonEmpty(record.TeamBotName, objectID)
	case "权限组":
		return firstNonEmpty(record.GroupName, auditDetailText(record.Detail, "group"), objectID)
	case "账号":
		if record.Subject != nil && record.Subject.ID.String() == objectID {
			return record.Subject.Name()
		}
		if record.Actor != nil && record.Actor.ID.String() == objectID {
			return record.Actor.Name()
		}
		return ""
	case "会话":
		// The ledger names a session, not its derived title; the id is the link.
		return ""
	}
	// An unknown resource type keeps its raw token and id (never a guessed name).
	if resourceType != "" {
		return objectID
	}
	return ""
}

func auditOwnerOf(objectType string, record store.AuditEventRecord) (string, string) {
	if objectType != "Bot" || record.BotOwner == nil {
		return "", ""
	}
	return record.BotOwner.ID.String(), record.BotOwner.Name()
}

func auditTeamIDOf(record store.AuditEventRecord) string {
	if teamID := auditDetailText(record.Detail, "team_bot_id", "teamId", "team_id"); teamID != "" {
		return teamID
	}
	if auditDetailText(record.Detail, "resource_type") == "team_bot" {
		return auditDetailText(record.Detail, "resource_id")
	}
	return ""
}

func auditRunIDOf(record store.AuditEventRecord) string {
	return auditDetailText(record.Detail, "session_id", "run_id", "runId", "sessionId")
}

func auditSubjectNameOf(record store.AuditEventRecord) string {
	if record.Subject == nil {
		return ""
	}
	return record.Subject.Name()
}

// auditSummaryOf renders the human sentence the page prints. It names only
// values already in the row (ids, keys, scopes, usernames, object names); the
// final string is passed through logsafe.Redact so a user-supplied name cannot
// smuggle a credential-shaped string into the page.
func auditSummaryOf(record store.AuditEventRecord, objectName string) string {
	detail := record.Detail
	action := auditDetailText(detail, "action")
	key := auditDetailText(detail, "key", "required_key", "requiredKey")
	scope := auditDetailText(detail, "scope")
	group := firstNonEmpty(record.GroupName, auditDetailText(detail, "group"))
	resourceType := auditDetailText(detail, "resource_type")
	resourceID := auditDetailText(detail, "resource_id")
	username := auditDetailText(detail, "username")
	var summary string

	switch record.Kind {
	case store.AuthEventPermissionGrant, store.AuthEventPermissionRevoke:
		granting := record.Kind == store.AuthEventPermissionGrant
		switch {
		case auditDetailText(detail, "via") == "group_membership":
			if granting {
				summary = "加入权限组 " + firstNonEmpty(group, "—")
			} else {
				summary = "移出权限组 " + firstNonEmpty(group, "—")
			}
		case key != "" && granting && group != "":
			summary = "为权限组 " + group + " 授予 " + key
		case key != "" && group != "":
			summary = "撤销权限组 " + group + " 的 " + key
		case key != "" && granting:
			summary = "授予 " + key
		case key != "":
			summary = "撤销 " + key
		case granting:
			summary = "授权变更"
		default:
			summary = "撤销授权"
		}
		if key != "" && scope != "" && auditDetailText(detail, "via") != "group_membership" {
			summary += "（" + scope + "）"
		}
	case store.AuthEventPermissionDenied:
		summary = "缺少能力 " + firstNonEmpty(key, "—")
	case store.AuthEventRelationChanged:
		summary = "变更关系 " + firstNonEmpty(
			strings.TrimSpace(resourceType+" / "+resourceID), auditDetailText(detail, "action"), "—")
	case store.AuthEventBotInvoked:
		summary = "调用 Bot " + firstNonEmpty(objectName, auditDetailText(detail, "bot_id"))
		if strings.EqualFold(auditDetailText(detail, "result"), "failed") {
			summary += " 失败"
		}
	case store.AuthEventSuperAdmin:
		summary = "超级管理员执行 " + firstNonEmpty(action, "操作")
	case store.AuthEventLoginSucceeded:
		summary = "登录成功"
	case store.AuthEventLoginFailed:
		summary = "登录失败"
	case store.AuthEventLoginRateLimit:
		summary = "登录被限流"
	case store.AuthEventSessionReject:
		summary = "会话被拒绝"
	case store.AuthEventLogout:
		summary = "退出登录"
	case store.AuthEventPasswordChange:
		summary = "修改密码"
	case store.AuthEventPasswordReset:
		summary = "重置密码"
	case store.AuthEventAccountCreated:
		summary = "创建账号 " + firstNonEmpty(username, auditSubjectNameOf(record))
	case store.AuthEventAccountDisable:
		summary = "停用账号 " + firstNonEmpty(username, auditSubjectNameOf(record))
	case store.AuthEventAccountEnable:
		summary = "启用账号 " + firstNonEmpty(username, auditSubjectNameOf(record))
	case store.AuthEventAccountBoot:
		summary = "初始化超级管理员"
	case store.AuthEventBootstrapToken:
		summary = "生成初始化令牌"
	case store.AuthEventBootstrapUsed:
		summary = "使用初始化令牌"
	case store.AuthEventBotKeyIssued:
		summary = "签发 Bot 密钥"
	case store.AuthEventBotKeyRevoked:
		summary = "吊销 Bot 密钥"
	default:
		summary = record.Kind
	}
	return logsafe.Redact(summary)
}

// attachGrantableHints fills the denial detail's 「可授予组/人」. One bounded
// query for the page's distinct missing keys; a failure to compute the hint
// must not fail the ledger read (the row is still true without it).
func (s *Server) attachGrantableHints(ctx context.Context, tenantID uuid.UUID, views []auditEventView) {
	keys := make([]string, 0, len(views))
	for _, view := range views {
		if view.Result == "denied" && view.CapabilityKey != "" {
			keys = append(keys, view.CapabilityKey)
		}
	}
	keys = uniqueSorted(keys)
	if len(keys) == 0 {
		return
	}
	groups, err := s.cfg.Identity.AuditGrantableGroups(ctx, tenantID, keys)
	if err != nil {
		if s.cfg.Logger != nil {
			s.cfg.Logger.Warn("audit grantable hint failed", "err", err)
		}
		groups = nil
	}
	for i := range views {
		view := &views[i]
		if view.Result != "denied" || view.CapabilityKey == "" {
			continue
		}
		hints := groups[view.CapabilityKey]
		if len(hints) == 0 {
			hints = []string{"超级管理员"}
		}
		view.GrantableBy = append([]string(nil), hints...)
	}
}

// redactAuditDetail applies logsafe's rules to a detail map and additionally
// masks hash/ip/user-agent fields. The audit writers store hashes (password,
// token, IP, user agent) and the page must never receive them.
func redactAuditDetail(detail map[string]any) map[string]any {
	if detail == nil {
		return map[string]any{}
	}
	redacted, _ := logsafe.RedactValue(detail).(map[string]any)
	if redacted == nil {
		return map[string]any{}
	}
	out, _ := redactAuditHashes(redacted).(map[string]any)
	if out == nil {
		return map[string]any{}
	}
	return out
}

func redactAuditHashes(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			normalized := strings.NewReplacer("-", "_", " ", "_", ".", "_").Replace(strings.ToLower(key))
			if strings.Contains(normalized, "hash") ||
				normalized == "ip" || normalized == "user_agent" || normalized == "useragent" ||
				normalized == "key_prefix" {
				out[key] = logsafe.Redacted
				continue
			}
			out[key] = redactAuditHashes(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = redactAuditHashes(item)
		}
		return out
	default:
		return value
	}
}

// auditDetailText reads the first non-empty text value at one of the names.
// JSON numbers are rendered as their decimal form; booleans and objects are
// not text and are skipped.
func auditDetailText(detail map[string]any, names ...string) string {
	for _, name := range names {
		raw, ok := detail[name]
		if !ok {
			continue
		}
		switch value := raw.(type) {
		case string:
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		case float64:
			return strconv.FormatFloat(value, 'f', -1, 64)
		case int64:
			return strconv.FormatInt(value, 10)
		case int:
			return strconv.Itoa(value)
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func uniqueSorted(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
