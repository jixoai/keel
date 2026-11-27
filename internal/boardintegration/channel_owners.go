package boardintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/store"

	boardlog "github.com/jixoai/keel-board/log"
	"github.com/jixoai/keel-board/protocol"
)

// handler is the board surface's outer HTTP handler. It adds the two keel-side
// behaviours the board module deliberately leaves to its host: recording the
// creator as channel owner after a successful create, and the SSE keepalive /
// lifetime policy.
type handler struct {
	next       http.Handler
	board      *boardlog.Board
	owners     *ChannelOwners
	sse        *sseHandler
	syncAdmin  *syncAdmin
	identity   *Identity
	authorizer *Authorizer
	logger     *slog.Logger
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if len(path) > 1 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}
	// GET /v1/board/me is a keel-owned contract addition (Phase 2 UI DESIGN
	// §15): board v0.1.1 has no caller identity route. It is handled here, after
	// the surface's tenant-switch gate, so a disabled board still answers 404
	// instead of leaking who would have been authenticated.
	if r.Method == http.MethodGet && path == "/v1/board/me" {
		h.me(w, r)
		return
	}
	// keel-owned sync entry management (POST/PATCH) is not part of board
	// v0.1.0; the mount layer implements it. It is checked before the board's
	// own route table so board's GET routes stay untouched.
	if h.syncAdmin != nil && h.syncAdmin.handles(r) {
		h.syncAdmin.ServeHTTP(w, r)
		return
	}
	if r.Method == http.MethodGet && path == boardserverEventsPath {
		h.sse.ServeHTTP(w, r)
		return
	}
	if r.Method == http.MethodPost && path == "/v1/board/channels" {
		h.createChannel(w, r)
		return
	}
	if r.Method == http.MethodPost && (path == "/v1/board/messages" || path == "/v1/board/tasks") {
		h.createThreadRoot(w, r)
		return
	}
	h.next.ServeHTTP(w, r)
}

// createChannel forwards the create and, when the board reports a newly
// created channel (201), writes the creator's owner relation. The board and
// keel cannot share one transaction across the module boundary; the write is
// immediate and the reconciler repairs a failure or a crash window. A create
// that lost the race (200, created=false) must not grant ownership.
func (h *handler) createChannel(w http.ResponseWriter, r *http.Request) {
	body, readErr := io.ReadAll(io.LimitReader(r.Body, channelCreateBodyLimit+1))
	r.Body = io.NopCloser(bytes.NewReader(body))
	if readErr != nil || len(body) > channelCreateBodyLimit {
		h.next.ServeHTTP(w, r)
		return
	}
	var request struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(body, &request)

	recorder := &statusWriter{ResponseWriter: w}
	h.next.ServeHTTP(recorder, r.WithContext(withChannelCreate(r.Context())))
	if request.Path == "" {
		return
	}
	principal, ok := EdgePrincipal(r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	switch recorder.status {
	case http.StatusCreated:
		if err := h.owners.GrantOwner(ctx, principal.TenantID.String(), request.Path, principal.AccountID, principal.AccountID); err != nil {
			h.logger.Warn("board channel owner relation was not recorded",
				"channel", request.Path, "err", err)
		}
	case http.StatusOK:
		// The channel already existed (created=false). A tenant board.manage
		// holder may adopt a channel that has no relation tuple at all; this is
		// the explicit repair path for a channel created before the ACL existed.
		// Any existing relation leaves the channel untouched.
		if h.authorizer == nil || !h.authorizer.tenantManager(ctx, principal) {
			return
		}
		claimed, err := h.owners.ClaimOrphan(ctx, principal.TenantID.String(), request.Path, principal.AccountID)
		if err != nil {
			h.logger.Warn("board orphan channel adoption failed", "channel", request.Path, "err", err)
			return
		}
		if claimed {
			h.logger.Info("board orphan channel adopted by a tenant manager", "channel", request.Path)
		}
	}
}

// createThreadRoot forwards a new thread (POST /messages or POST /tasks) and
// records the channel's creator as owner when the target channel did not exist
// before the request. Board v0.1.x does not emit channel.created for a channel
// that first appears in message.posted; the board projection creates the
// channel implicitly under a parent whose editor may post, and the reconciler
// repairs the same fact from the first root message. A reply never creates a
// channel.
func (h *handler) createThreadRoot(w http.ResponseWriter, r *http.Request) {
	body, readErr := io.ReadAll(io.LimitReader(r.Body, channelCreateBodyLimit+1))
	r.Body = io.NopCloser(bytes.NewReader(body))
	if readErr != nil || len(body) > channelCreateBodyLimit {
		h.next.ServeHTTP(w, r)
		return
	}
	var request struct {
		Channel string  `json:"channel"`
		Re      *string `json:"re"`
	}
	_ = json.Unmarshal(body, &request)
	channel := strings.Trim(strings.TrimSpace(request.Channel), "/")
	if request.Re != nil || channel == "" {
		h.next.ServeHTTP(w, r)
		return
	}
	principal, ok := EdgePrincipal(r)
	if !ok {
		h.next.ServeHTTP(w, r)
		return
	}
	existed := h.channelExists(r.Context(), principal.TenantID.String(), channel)

	recorder := &statusWriter{ResponseWriter: w}
	h.next.ServeHTTP(recorder, r)
	if recorder.status != http.StatusCreated || existed {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.owners.GrantOwner(ctx, principal.TenantID.String(), channel, principal.AccountID, principal.AccountID); err != nil {
		h.logger.Warn("implicit board channel owner relation was not recorded",
			"channel", channel, "err", err)
	}
}

func (h *handler) channelExists(ctx context.Context, tenant, channel string) bool {
	if h.board == nil {
		return true
	}
	// A second replica may have created this channel inside the freshness window;
	// refresh before deciding whether this root is the implicit creator.
	_ = h.board.Refresh(ctx, tenant)
	exists := false
	if err := h.board.Read(tenant, func(snapshot *boardlog.Snapshot) error {
		exists = snapshot.Channel(channel) != nil
		return nil
	}); err != nil {
		// Do not guess "new" on a read failure: the reconciler will repair the
		// relation from the committed event.
		return true
	}
	return exists
}

// statusWriter observes the status without buffering the body, so a normal
// JSON response still streams to the client.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ChannelOwners records and repairs the owner relation a board channel needs.
// board itself has no keel identity; a channel.created event or the first root
// message.posted in a channel names the actor, and the actor's principal is the
// Account that owns the channel.
//
// The write is idempotent: the same (tenant, channel, owner) tuple inserted
// twice changes nothing, so the request path and the reconciler can race
// safely. The relation write cannot share board's transaction across the module
// boundary; the request path writes immediately after a 201 and this reconciler
// repairs a crash window.
type ChannelOwners struct {
	Store  *store.Store
	Audit  *accesswire.Bridge
	DSN    string
	Schema string
	Logger *slog.Logger
}

// GrantOwner writes the owner relation for one channel. It validates the
// channel path first so a malformed create body cannot write an arbitrary
// resource id.
func (c *ChannelOwners) GrantOwner(ctx context.Context, tenant string, channel string, accountID uuid.UUID, actorID uuid.UUID) error {
	if c == nil || c.Store == nil {
		return errors.New("boardintegration: channel owners are not wired")
	}
	tenantID, err := uuid.Parse(strings.TrimSpace(tenant))
	if err != nil || tenantID == uuid.Nil {
		return errors.New("boardintegration: invalid tenant for channel owner")
	}
	channel = strings.Trim(strings.TrimSpace(channel), "/")
	if err := protocol.ValidateChannel(channel); err != nil {
		return err
	}
	if accountID == uuid.Nil {
		return errors.New("boardintegration: channel owner account is required")
	}
	if actorID == uuid.Nil {
		actorID = accountID
	}
	created, err := c.Store.PutResourceRelation(ctx, store.ResourceRelation{
		TenantID:     tenantID,
		ResourceType: BoardChannelResourceType,
		ResourceID:   channel,
		Relation:     store.RelationOwner,
		SubjectType:  store.RelationSubjectAccount,
		SubjectID:    accountID.String(),
		CreatedBy:    actorID,
	})
	if err != nil {
		return err
	}
	if !created {
		// The tuple already existed: a retry, a repair scan, or another replica.
		// Do not append a duplicate relation audit row for it.
		return nil
	}
	if c.Audit != nil {
		if auditErr := c.Audit.AuditRelation(ctx, tenantID, actorID, "grant", BoardChannelResourceType, channel, store.RelationOwner, store.RelationSubjectAccount, accountID.String()); auditErr != nil {
			c.Logger.Warn("board channel owner relation is unaudited", "channel", channel, "err", auditErr)
		}
	}
	return nil
}

// ClaimOrphan records accountID as owner of a channel that has no relation
// tuple at all. It is the explicit adoption path for a channel created before
// the ACL existed: it never changes a channel that has any relation, so it
// cannot be used to take over an owned channel. It reports whether this call
// wrote the relation.
func (c *ChannelOwners) ClaimOrphan(ctx context.Context, tenant string, channel string, accountID uuid.UUID) (bool, error) {
	if c == nil || c.Store == nil {
		return false, errors.New("boardintegration: channel owners are not wired")
	}
	tenantID, err := uuid.Parse(strings.TrimSpace(tenant))
	if err != nil || tenantID == uuid.Nil {
		return false, errors.New("boardintegration: invalid tenant for channel owner")
	}
	channel = strings.Trim(strings.TrimSpace(channel), "/")
	if err := protocol.ValidateChannel(channel); err != nil {
		return false, err
	}
	if accountID == uuid.Nil {
		return false, errors.New("boardintegration: channel owner account is required")
	}
	created, err := c.Store.PutResourceRelationIfOrphan(ctx, store.ResourceRelation{
		TenantID:     tenantID,
		ResourceType: BoardChannelResourceType,
		ResourceID:   channel,
		Relation:     store.RelationOwner,
		SubjectType:  store.RelationSubjectAccount,
		SubjectID:    accountID.String(),
		CreatedBy:    accountID,
	}, channelAncestors(channel))
	if err != nil || !created {
		return created, err
	}
	if c.Audit != nil {
		if auditErr := c.Audit.AuditRelation(ctx, tenantID, accountID, "grant", BoardChannelResourceType, channel, store.RelationOwner, store.RelationSubjectAccount, accountID.String()); auditErr != nil {
			c.Logger.Warn("board orphan channel adoption is unaudited", "channel", channel, "err", auditErr)
		}
	}
	return true, nil
}

// Run is the repair loop. It scans every channel.created event and the earliest
// root message.posted of each channel once at startup and then periodically; a
// channel whose request-path grant succeeded is skipped in-process. The scan is
// an operator read: it sets keel.bypass_rls for its single connection because
// it is cross-tenant by nature (one channel per tenant), and the connection is
// closed before the next pass.
func (c *ChannelOwners) Run(ctx context.Context, interval time.Duration) {
	if c == nil || c.Store == nil {
		return
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	c.scan(ctx)
	if interval < 0 {
		return
	}
	if interval == 0 {
		interval = defaultChannelScan
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.scan(ctx)
		}
	}
}

type channelCreatedEvent struct {
	Channel string `json:"channel"`
	By      struct {
		Actor     string `json:"actor"`
		Principal string `json:"principal"`
	} `json:"by"`
}

func (c *ChannelOwners) scan(ctx context.Context) {
	explicit, implicit, err := c.channelEvents(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c.Logger.Warn("board channel owner scan failed", "err", openError("scan channel events", err, c.DSN))
		}
		return
	}
	explicitChannels := make(map[string]bool, len(explicit))
	for _, event := range explicit {
		key := event.tenant + "\x00" + event.payload.Channel
		explicitChannels[key] = true
		c.grantFromEvent(ctx, event, "channel.created")
	}
	for _, event := range implicit {
		key := event.tenant + "\x00" + event.payload.Channel
		if explicitChannels[key] {
			// An explicit channel.created owns this channel; the first root is
			// ordinary content, not a second creator.
			continue
		}
		c.grantFromEvent(ctx, event, "first message.posted")
	}
}

// grantFromEvent records one event's creator as owner. Every pass re-checks
// the source event: the write is idempotent and only a missing tuple appends an
// audit row, so a relation an operator deleted is repaired on the next pass. A
// "seen" cache would skip exactly that case and make repair depend on scan
// timing. A platform actor has no keel Account and is left to the sync entry's
// actor, which owns platform-created channels.
func (c *ChannelOwners) grantFromEvent(ctx context.Context, event channelCreatedRow, source string) {
	accountID, ok := accountFromPrincipal(event.payload.By.Principal)
	if !ok {
		return
	}
	if err := c.GrantOwner(ctx, event.tenant, event.payload.Channel, accountID, accountID); err != nil {
		c.Logger.Warn("board channel owner repair failed",
			"channel", event.payload.Channel, "source", source, "err", err)
	}
}

type channelCreatedRow struct {
	tenant  string
	seq     string
	payload channelCreatedEvent
}

// channelEvents reads both repair sources on one operator connection: every
// explicit channel.created, and the earliest root message.posted per channel
// (which is the implicit creation board derives when a child channel first
// appears under a parent's editor). A reply has `re` set and is excluded in
// SQL, so an ordinary reply can never name a creator.
func (c *ChannelOwners) channelEvents(ctx context.Context) ([]channelCreatedRow, []channelCreatedRow, error) {
	conn, err := pgx.Connect(ctx, c.DSN)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close(context.Background())
	if c.Schema != "" {
		if _, err := conn.Exec(ctx, "SET search_path TO "+pgx.Identifier{c.Schema}.Sanitize()); err != nil {
			return nil, nil, err
		}
	}
	// The scan is the one cross-tenant board read in keel; board's core RLS
	// policy admits it only through the explicit bypass marker. The setting is
	// session-local on this dedicated connection.
	if _, err := conn.Exec(ctx, "SELECT set_config('keel.bypass_rls', 'on', false)"); err != nil {
		return nil, nil, err
	}
	table := "board_events"
	if c.Schema != "" {
		table = pgx.Identifier{c.Schema, "board_events"}.Sanitize()
	}
	explicit, err := scanChannelEvents(conn.Query(ctx,
		`SELECT tenant_id, seq, payload FROM `+table+` WHERE kind = 'channel.created' ORDER BY tenant_id, seq`))
	if err != nil {
		return nil, nil, err
	}
	implicit, err := scanChannelEvents(conn.Query(ctx, `
		SELECT DISTINCT ON (tenant_id, channel) tenant_id, seq, payload
		FROM `+table+`
		WHERE kind = 'message.posted'
		  AND (payload->>'re') IS NULL
		  AND channel IS NOT NULL
		  AND channel <> ''
		ORDER BY tenant_id, channel, seq`))
	if err != nil {
		return nil, nil, err
	}
	return explicit, implicit, nil
}

func scanChannelEvents(rows pgx.Rows, queryErr error) ([]channelCreatedRow, error) {
	if queryErr != nil {
		return nil, queryErr
	}
	defer rows.Close()
	out := make([]channelCreatedRow, 0, 32)
	for rows.Next() {
		var (
			tenant  string
			seq     int64
			payload []byte
		)
		if err := rows.Scan(&tenant, &seq, &payload); err != nil {
			return nil, err
		}
		var parsed channelCreatedEvent
		if err := json.Unmarshal(payload, &parsed); err != nil || parsed.Channel == "" {
			continue
		}
		out = append(out, channelCreatedRow{tenant: tenant, seq: strconv.FormatInt(seq, 10), payload: parsed})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// channelAncestors returns every parent path of a channel, most specific
// first: "work/production" -> ["work"]. It is the same inheritance board's
// authorizer walks, so an inherited relation counts as an existing ACL.
func channelAncestors(channel string) []string {
	channel = strings.Trim(strings.TrimSpace(channel), "/")
	out := make([]string, 0, 4)
	for {
		index := strings.LastIndex(channel, "/")
		if index < 0 {
			return out
		}
		channel = channel[:index]
		out = append(out, channel)
	}
}

func accountFromPrincipal(principal string) (uuid.UUID, bool) {
	if !strings.HasPrefix(principal, AccountActorPrefix) {
		return uuid.Nil, false
	}
	raw := strings.TrimPrefix(principal, AccountActorPrefix)
	if index := strings.IndexByte(raw, '+'); index >= 0 {
		raw = raw[:index]
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, false
	}
	return id, true
}
