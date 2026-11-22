// Bounded audit-ledger reads (change keel-bot-identity-access, A4.4c).
//
// auth_events is the single append-only ledger A2/A4.1 write. This file is its
// read projection for the 记录 page: a tenant-scoped, keyset-paginated query
// with the filters the page exposes (time window, kind, result, account) and
// the account names the UI needs to render a row without a second round trip.
//
// Two properties are deliberate:
//
//   - the read is always bounded: a caller must supply a time window (the HTTP
//     layer enforces 90 days) and the store clamps the page to 200 rows. There
//     is no unbounded audit export in the request path;
//   - names are joined in the same query (actor, subject, bot/team/group and a
//     Bot's owner), so listing N rows costs one query, not N+1.
//
// Details stay raw here: redaction is a wire concern (harnessapi applies
// logsafe). The store never returns a credential column because auth_events
// has none; the JSON detail is the only place a caller could have written one,
// and the HTTP layer masks it before serialization.
package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Audit read bounds. The HTTP layer validates the same numbers; the store
// clamps defensively so a direct caller cannot ask for an unbounded page.
const (
	defaultAuditReadLimit = 100
	maxAuditReadLimit     = 200
)

// ErrAuditCursor is a malformed keyset cursor. It wraps ErrInvalid so an HTTP
// layer maps it to 400 without parsing prose.
var ErrAuditCursor = fmt.Errorf("%w: malformed audit cursor", ErrInvalid)

// AuditReadFilter is one bounded read of auth_events. A zero field is "any".
// From is inclusive and To is exclusive, both in UTC.
type AuditReadFilter struct {
	From      time.Time
	To        time.Time
	Kinds     []string
	Results   []string
	AccountID uuid.UUID
	// Query is a case-insensitive substring search over the raw kind, the
	// identifier-shaped detail fields the projection exposes, and the joined
	// actor/subject/object display names.
	Query string
	// After is the opaque keyset cursor returned as nextCursor.
	After string
	// Limit is the page size; <=0 uses the default, >max is clamped.
	Limit int
}

// AuditAccountRef is one account named by an audit row.
type AuditAccountRef struct {
	ID          uuid.UUID
	Username    string
	DisplayName string
	Kind        string
}

// Name returns the display name, falling back to the username.
func (r *AuditAccountRef) Name() string {
	if r == nil {
		return ""
	}
	if strings.TrimSpace(r.DisplayName) != "" {
		return r.DisplayName
	}
	return r.Username
}

// AuditEventRecord is one audit row plus the joined names the projection needs.
// Actor is the acting account (actor_account_id, or account_id when the row has
// no separate actor); Subject is account_id, the row's subject.
type AuditEventRecord struct {
	TenantID       uuid.UUID
	EventID        int64
	AccountID      *uuid.UUID
	ActorAccountID *uuid.UUID
	Kind           string
	Result         string
	Detail         map[string]any
	CreatedAt      time.Time

	Actor   *AuditAccountRef
	Subject *AuditAccountRef

	// BotName, TeamBotName and GroupName are the display names of the objects
	// the row's detail points at, when the row and the object still exist.
	BotName     string
	TeamBotName string
	GroupName   string
	// BotOwner is the account a Bot belongs to (through its Profile).
	BotOwner *AuditAccountRef
}

// AuditEventPage is one keyset page. NextCursor is empty on the last page.
type AuditEventPage struct {
	Events     []AuditEventRecord
	NextCursor string
	HasMore    bool
}

// auditEffectiveResultSQL is the row's product outcome. auth_events.result
// records whether the audit write itself succeeded; a bot.invoked row carries
// the call's outcome in detail.result (the write is always "ok"). The read
// projection reports the call's outcome so the 记录 page's 成功/失败 filter is
// truthful, without rewriting history.
const auditEffectiveResultSQL = `CASE
    WHEN e.kind = 'bot.invoked'
     AND e.detail->>'result' IN ('ok', 'failed', 'denied', 'refused')
    THEN e.detail->>'result'
    ELSE e.result
  END`

// auditReadSelect is the one projection shared by the list and detail reads.
// The joins are bounded lookups on primary keys; CASE expressions only decide
// which object id a detail map names, never whether a row is visible.
var auditReadSelect = `
SELECT e.tenant_id, e.event_id, e.account_id, e.actor_account_id,
       e.kind, ` + auditEffectiveResultSQL + ` AS result, e.detail, e.created_at,
       COALESCE(actor.account_id::text, ''), COALESCE(actor.username, ''),
       COALESCE(actor.display_name, ''), COALESCE(actor.kind, ''),
       COALESCE(subject.account_id::text, ''), COALESCE(subject.username, ''),
       COALESCE(subject.display_name, ''), COALESCE(subject.kind, ''),
       COALESCE(bot.name, ''), COALESCE(tb.name, ''), COALESCE(pg.name, ''),
       COALESCE(owner.account_id::text, ''), COALESCE(owner.username, ''),
       COALESCE(owner.display_name, ''), COALESCE(owner.kind, '')
  FROM auth_events e
  LEFT JOIN accounts actor
         ON actor.tenant_id = e.tenant_id AND actor.account_id = e.actor_account_id
  LEFT JOIN accounts subject
         ON subject.tenant_id = e.tenant_id AND subject.account_id = e.account_id
  LEFT JOIN bots bot
         ON bot.tenant_id = e.tenant_id
        AND bot.bot_id = COALESCE(
              e.detail->>'bot_id',
              CASE WHEN e.detail->>'resource_type' = 'bot'
                   THEN e.detail->>'resource_id' END)
  LEFT JOIN team_bots tb
         ON tb.tenant_id = e.tenant_id
        AND tb.team_bot_id = COALESCE(
              e.detail->>'team_bot_id',
              CASE WHEN e.detail->>'resource_type' = 'team_bot'
                   THEN e.detail->>'resource_id' END)
  LEFT JOIN permission_groups pg
         ON pg.tenant_id = e.tenant_id
        AND pg.group_id::text = e.detail->>'group_id'
  LEFT JOIN profiles profile
         ON profile.tenant_id = bot.tenant_id AND profile.profile_id = bot.profile_id
  LEFT JOIN accounts owner
         ON owner.tenant_id = profile.tenant_id AND owner.account_id = profile.account_id`

// QueryAuditEvents returns one keyset page of audit rows, newest first. The
// ordering key is (created_at, event_id); the composite index
// auth_events_by_created_event backs it. The cursor is exclusive: the next
// page contains rows strictly older than the last row of this page.
func (s *Store) QueryAuditEvents(ctx context.Context, tenantID uuid.UUID, filter AuditReadFilter) (AuditEventPage, error) {
	if tenantID == uuid.Nil {
		return AuditEventPage{}, invalidIdentity("query audit events", "tenant id is required")
	}
	limit := filter.Limit
	switch {
	case limit <= 0:
		limit = defaultAuditReadLimit
	case limit > maxAuditReadLimit:
		limit = maxAuditReadLimit
	}
	afterTime, afterID, err := decodeAuditCursor(filter.After)
	if err != nil {
		return AuditEventPage{}, err
	}

	args := []any{tenantID}
	where := []string{"e.tenant_id = $1"}
	addArg := func(value any) int {
		args = append(args, value)
		return len(args)
	}
	if !filter.From.IsZero() {
		where = append(where, fmt.Sprintf("e.created_at >= $%d", addArg(filter.From.UTC())))
	}
	if !filter.To.IsZero() {
		where = append(where, fmt.Sprintf("e.created_at < $%d", addArg(filter.To.UTC())))
	}
	if len(filter.Kinds) > 0 {
		where = append(where, fmt.Sprintf("e.kind = ANY($%d::text[])", addArg(filter.Kinds)))
	}
	if len(filter.Results) > 0 {
		where = append(where, fmt.Sprintf("(%s) = ANY($%d::text[])",
			auditEffectiveResultSQL, addArg(filter.Results)))
	}
	if filter.AccountID != uuid.Nil {
		placeholder := addArg(filter.AccountID)
		where = append(where, fmt.Sprintf(
			"(e.account_id = $%d OR e.actor_account_id = $%d)", placeholder, placeholder))
	}
	if q := strings.TrimSpace(filter.Query); q != "" {
		placeholder := addArg(strings.ToLower(q))
		// Search the identifiers and display names the projection exposes. The
		// detail JSON is not scanned whole: a search oracle over a password
		// mistakenly stored under an unexpected key would confirm a secret even
		// though the API redacts it on the way out.
		where = append(where, fmt.Sprintf(`(
		    strpos(lower(e.kind), $%[1]d) > 0
		 OR strpos(lower(COALESCE(e.detail->>'username', '')), $%[1]d) > 0
		 OR strpos(lower(COALESCE(e.detail->>'key', '')), $%[1]d) > 0
		 OR strpos(lower(COALESCE(e.detail->>'required_key', '')), $%[1]d) > 0
		 OR strpos(lower(COALESCE(e.detail->>'bot_id', '')), $%[1]d) > 0
		 OR strpos(lower(COALESCE(e.detail->>'team_bot_id', '')), $%[1]d) > 0
		 OR strpos(lower(COALESCE(e.detail->>'group_id', '')), $%[1]d) > 0
		 OR strpos(lower(COALESCE(e.detail->>'group', '')), $%[1]d) > 0
		 OR strpos(lower(COALESCE(e.detail->>'resource_type', '')), $%[1]d) > 0
		 OR strpos(lower(COALESCE(e.detail->>'resource_id', '')), $%[1]d) > 0
		 OR strpos(lower(COALESCE(e.detail->>'action', '')), $%[1]d) > 0
		 OR strpos(lower(COALESCE(e.detail->>'session_id', '')), $%[1]d) > 0
		 OR strpos(lower(COALESCE(e.detail->>'request_id', '')), $%[1]d) > 0
		 OR strpos(lower(COALESCE(e.detail->>'reason', '')), $%[1]d) > 0
		 OR strpos(lower(COALESCE(actor.username, '')), $%[1]d) > 0
		 OR strpos(lower(COALESCE(actor.display_name, '')), $%[1]d) > 0
		 OR strpos(lower(COALESCE(subject.username, '')), $%[1]d) > 0
		 OR strpos(lower(COALESCE(subject.display_name, '')), $%[1]d) > 0
		 OR strpos(lower(COALESCE(bot.name, '')), $%[1]d) > 0
		 OR strpos(lower(COALESCE(tb.name, '')), $%[1]d) > 0
		 OR strpos(lower(COALESCE(pg.name, '')), $%[1]d) > 0
		)`, placeholder))
	}
	if !afterTime.IsZero() {
		timePlaceholder := addArg(afterTime.UTC())
		idPlaceholder := addArg(afterID)
		where = append(where, fmt.Sprintf(
			"(e.created_at, e.event_id) < ($%d, $%d)", timePlaceholder, idPlaceholder))
	}
	limitPlaceholder := addArg(limit + 1)

	query := auditReadSelect +
		"\n WHERE " + strings.Join(where, "\n   AND ") +
		"\n ORDER BY e.created_at DESC, e.event_id DESC" +
		"\n LIMIT $" + strconv.Itoa(limitPlaceholder)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return AuditEventPage{}, identityWriteError("query audit events", err)
	}
	defer rows.Close()

	records := make([]AuditEventRecord, 0, limit+1)
	for rows.Next() {
		record, err := scanAuditEventRecord(rows)
		if err != nil {
			return AuditEventPage{}, identityWriteError("scan audit event", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return AuditEventPage{}, identityWriteError("query audit events", err)
	}

	page := AuditEventPage{HasMore: len(records) > limit}
	if page.HasMore {
		records = records[:limit]
	}
	page.Events = records
	if page.HasMore && len(records) > 0 {
		last := records[len(records)-1]
		page.NextCursor = encodeAuditCursor(last.CreatedAt, last.EventID)
	}
	return page, nil
}

// GetAuditEventByID returns one tenant-scoped row. A row of another tenant (or
// a missing id) answers ErrNotFound: the read never distinguishes existence
// across a tenant boundary.
func (s *Store) GetAuditEventByID(ctx context.Context, tenantID uuid.UUID, eventID int64) (AuditEventRecord, error) {
	if tenantID == uuid.Nil || eventID <= 0 {
		return AuditEventRecord{}, invalidIdentity("get audit event", "tenant id and event id are required")
	}
	row := s.pool.QueryRow(ctx, auditReadSelect+
		"\n WHERE e.tenant_id = $1 AND e.event_id = $2", tenantID, eventID)
	record, err := scanAuditEventRecord(row)
	if err != nil {
		return AuditEventRecord{}, identityWriteError("get audit event", err)
	}
	return record, nil
}

// AuditGrantableGroups returns, for each permission key, the permission groups
// that carry it. It answers the 记录 detail's 「可授予组/人」 hint without an
// N+1 query: one bounded read for the page's distinct missing keys.
func (s *Store) AuditGrantableGroups(ctx context.Context, tenantID uuid.UUID, keys []string) (map[string][]string, error) {
	if tenantID == uuid.Nil {
		return nil, invalidIdentity("audit grantable groups", "tenant id is required")
	}
	keys = uniqueTrimmed(keys)
	if len(keys) == 0 {
		return map[string][]string{}, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT g.permission_key, pg.name
		   FROM permission_group_grants g
		   JOIN permission_groups pg
		     ON pg.tenant_id = g.tenant_id AND pg.group_id = g.group_id
		  WHERE g.tenant_id = $1 AND g.permission_key = ANY($2::text[])
		  ORDER BY g.permission_key, pg.name`, tenantID, keys)
	if err != nil {
		return nil, identityWriteError("audit grantable groups", err)
	}
	defer rows.Close()
	out := make(map[string][]string, len(keys))
	seen := make(map[string]map[string]bool, len(keys))
	for rows.Next() {
		var key, name string
		if err := rows.Scan(&key, &name); err != nil {
			return nil, identityWriteError("scan audit grantable groups", err)
		}
		if seen[key] == nil {
			seen[key] = map[string]bool{}
		}
		if seen[key][name] {
			continue
		}
		seen[key][name] = true
		out[key] = append(out[key], name)
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("audit grantable groups", err)
	}
	return out, nil
}

func scanAuditEventRecord(row rowScanner) (AuditEventRecord, error) {
	var (
		record  AuditEventRecord
		raw     []byte
		actorID string
		// The remaining joined names are scanned into strings; an empty id means
		// the join found no row (a deleted account or object).
		actorUsername, actorDisplay, actorKind          string
		subjectID, subjectUsername, subjectDisplay      string
		subjectKind, botName, teamBotName, groupName    string
		ownerID, ownerUsername, ownerDisplay, ownerKind string
	)
	if err := row.Scan(
		&record.TenantID, &record.EventID, &record.AccountID, &record.ActorAccountID,
		&record.Kind, &record.Result, &raw, &record.CreatedAt,
		&actorID, &actorUsername, &actorDisplay, &actorKind,
		&subjectID, &subjectUsername, &subjectDisplay, &subjectKind,
		&botName, &teamBotName, &groupName,
		&ownerID, &ownerUsername, &ownerDisplay, &ownerKind,
	); err != nil {
		return AuditEventRecord{}, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &record.Detail); err != nil {
			return AuditEventRecord{}, err
		}
	}
	if record.Detail == nil {
		record.Detail = map[string]any{}
	}
	record.Actor = auditAccountRef(actorID, actorUsername, actorDisplay, actorKind)
	record.Subject = auditAccountRef(subjectID, subjectUsername, subjectDisplay, subjectKind)
	record.BotName = botName
	record.TeamBotName = teamBotName
	record.GroupName = groupName
	record.BotOwner = auditAccountRef(ownerID, ownerUsername, ownerDisplay, ownerKind)
	return record, nil
}

func auditAccountRef(id, username, displayName, kind string) *AuditAccountRef {
	parsed, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil || parsed == uuid.Nil {
		return nil
	}
	return &AuditAccountRef{
		ID: parsed, Username: username, DisplayName: displayName, Kind: kind,
	}
}

// encodeAuditCursor keys a page by the last row's (created_at, event_id). The
// microsecond precision matches PostgreSQL timestamptz.
func encodeAuditCursor(createdAt time.Time, eventID int64) string {
	raw := strconv.FormatInt(createdAt.UTC().UnixMicro(), 10) + ":" + strconv.FormatInt(eventID, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeAuditCursor(value string) (time.Time, int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("%w: %v", ErrAuditCursor, err)
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 2 {
		return time.Time{}, 0, fmt.Errorf("%w: expected <micros>:<eventId>", ErrAuditCursor)
	}
	micros, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || micros <= 0 {
		return time.Time{}, 0, fmt.Errorf("%w: cursor time must be positive", ErrAuditCursor)
	}
	eventID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || eventID <= 0 {
		return time.Time{}, 0, fmt.Errorf("%w: event id must be positive", ErrAuditCursor)
	}
	return time.UnixMicro(micros).UTC(), eventID, nil
}

func uniqueTrimmed(values []string) []string {
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
	return out
}
