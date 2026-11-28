package boardintegration

// Contract additions at the keel mount layer.
//
// board renders its task projection without a viewer-dependent action list or
// sync credential state. The UI's one action authority is `TaskView.moves`, so
// keel augments the board's JSON responses rather than teaching the client a
// second rule set (keel-board-v1 UI DESIGN §6.1/§15). The board module stays
// untouched and keeps its own events: this file rewrites JSON response bodies
// only, never SSE frames.
//
// Every addition is derived from facts the board already authorised:
//   - the task projection (state, owner, by, for, claim),
//   - the caller's channel relation and board keys (probed without writing a
//     permission.denied audit row).
//
// A task whose flags cannot be established gets `moves: []`, which reads as
// read-only. It never gets an invented permission.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel-board/protocol"
	"github.com/jixoai/keel/internal/auth"
)

// ArchivedLabel marks a task imported as already terminal history. It is
// written through the board log, so the reserved board.* namespace stays
// reserved. An archived task is read-only history.
const ArchivedLabel = "board.archived"

// API returns the board surface with the Phase 2 JSON contract additions. A
// caller that mounts the surface directly (tests, an embedding deployment) gets
// the same contract as keel serve.
func (s *Surface) API() http.Handler {
	return s.Augment(s)
}

// Augment wraps next with the JSON contract additions. SSE, metrics, non-JSON
// bodies and error responses pass through untouched; a response that cannot be
// decoded or re-encoded is written exactly as the board produced it.
func (s *Surface) Augment(next http.Handler) http.Handler {
	if s == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !augmentsPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		buffered := newBufferedResponse()
		next.ServeHTTP(buffered, r)
		status, header, body := buffered.result()
		augmented, changed := s.augmentBody(r, status, header, body)
		if changed {
			writeBuffered(w, header, status, augmented)
			return
		}
		writeBuffered(w, header, status, body)
	})
}

// augmentsPath names the response shapes that may carry a task or sync entry.
// `/v1/board/events` is deliberately absent: SSE frames are the board's event
// contract and are never rewritten.
func augmentsPath(path string) bool {
	path = strings.TrimSuffix(path, "/")
	switch {
	case path == "/v1/board/tasks" || strings.HasPrefix(path, "/v1/board/tasks/"):
		return true
	case path == "/v1/board/messages" || strings.HasPrefix(path, "/v1/board/messages/"):
		return true
	case path == "/v1/board/threads":
		return true
	case path == "/v1/board/sync/entries" || strings.HasPrefix(path, "/v1/board/sync/entries/"):
		return true
	case path == "/v1/board/sync/status":
		return true
	default:
		return false
	}
}

func (s *Surface) augmentBody(r *http.Request, status int, header http.Header, body []byte) ([]byte, bool) {
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, false
	}
	contentType := strings.ToLower(header.Get("Content-Type"))
	if !strings.Contains(contentType, "application/json") {
		return nil, false
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		// More than one JSON value: not a shape this layer understands.
		return nil, false
	}
	changed := false
	if isTaskPath(r.URL.Path) {
		changed = s.augmentTasks(r, value) || changed
	}
	if isSyncPath(r.URL.Path) {
		changed = s.augmentSync(r, value) || changed
	}
	if !changed {
		return nil, false
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, false
	}
	return encoded.Bytes(), true
}

func isTaskPath(path string) bool {
	path = strings.TrimSuffix(path, "/")
	return path == "/v1/board/tasks" || strings.HasPrefix(path, "/v1/board/tasks/") ||
		path == "/v1/board/messages" || strings.HasPrefix(path, "/v1/board/messages/") ||
		path == "/v1/board/threads"
}

func isSyncPath(path string) bool {
	path = strings.TrimSuffix(path, "/")
	return path == "/v1/board/sync/entries" || strings.HasPrefix(path, "/v1/board/sync/entries/") ||
		path == "/v1/board/sync/status"
}

// ---- task moves -----------------------------------------------------------

type taskAugmentSession struct {
	ctx        context.Context
	now        time.Time
	actor      protocol.Actor
	tenantID   uuid.UUID
	authorizer *Authorizer
	abilities  map[string]ChannelAbilities
	changed    bool
}

func (s *Surface) augmentTasks(r *http.Request, value any) bool {
	if s == nil || s.identity == nil || s.authorizer == nil {
		return false
	}
	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok || principal.TenantID == uuid.Nil || principal.AccountID == uuid.Nil {
		return false
	}
	actor, err := s.identity.Actor(r)
	if err != nil {
		return false
	}
	tenant, err := s.identity.Tenant(r)
	if err != nil {
		return false
	}
	tenantID, err := ValidateTenantUUID(tenant)
	if err != nil {
		return false
	}
	session := &taskAugmentSession{
		ctx: r.Context(), now: time.Now().UTC(),
		actor: actor, tenantID: tenantID,
		authorizer: s.authorizer,
		abilities:  map[string]ChannelAbilities{},
	}
	session.walk(value)
	return session.changed
}

func (s *taskAugmentSession) walk(value any) {
	switch node := value.(type) {
	case map[string]any:
		if isMessageObject(node) {
			s.augmentMessage(node)
			return
		}
		if isTaskObject(node) {
			s.augmentTask(node, nil)
			return
		}
		for _, child := range node {
			s.walk(child)
		}
	case []any:
		for _, child := range node {
			s.walk(child)
		}
	}
}

func isMessageObject(node map[string]any) bool {
	if _, ok := node["channel"].(string); !ok {
		return false
	}
	if _, ok := node["id"]; !ok {
		return false
	}
	if _, ok := node["task"]; ok {
		return true
	}
	if _, ok := node["text"]; ok {
		return true
	}
	_, ok := node["createdAt"]
	return ok
}

func isTaskObject(node map[string]any) bool {
	if _, ok := node["state"].(string); !ok {
		return false
	}
	if _, ok := node["owner"]; !ok {
		return false
	}
	_, epoch := node["epoch"]
	_, by := node["by"]
	return epoch || by
}

func (s *taskAugmentSession) augmentMessage(message map[string]any) {
	task, ok := message["task"].(map[string]any)
	if !ok {
		return
	}
	s.augmentTask(task, message)
}

func (s *taskAugmentSession) augmentTask(task map[string]any, message map[string]any) {
	if task == nil {
		return
	}
	s.changed = true
	labels := map[string]any{}
	if message != nil {
		labels = jsonObject(message["labels"])
	}
	archived := strings.EqualFold(jsonText(labels[ArchivedLabel]), "true")
	if archived {
		task["archived"] = true
	}
	task["moves"] = s.movesFor(task, message, archived)
}

func (s *taskAugmentSession) abilitiesFor(channel string) ChannelAbilities {
	if channel == "" || s.authorizer == nil {
		return ChannelAbilities{}
	}
	if cached, ok := s.abilities[channel]; ok {
		return cached
	}
	abilities := s.authorizer.ChannelAbilities(s.ctx, s.tenantID.String(), s.actor, channel)
	s.abilities[channel] = abilities
	return abilities
}

// movesFor is the server-side rule table (UI DESIGN §6.1) applied to one
// task. It mirrors the board's own authorization checks, including the claim
// fence and the owner rule, so the UI never offers an action the next request
// would refuse. Token-bearing actions are offered to the claim's actor only;
// the tab that actually holds the token filters the list itself.
func (s *taskAugmentSession) movesFor(task map[string]any, message map[string]any, archived bool) []string {
	if archived {
		return []string{}
	}
	var channel string
	if message != nil {
		channel = jsonText(message["channel"])
	}
	if channel == "" {
		return []string{}
	}
	abilities := s.abilitiesFor(channel)
	if !abilities.Read {
		return []string{}
	}
	state := jsonText(task["state"])
	owner := jsonText(task["owner"])
	by := jsonText(task["by"])
	forRef := ""
	if value, ok := task["for"]; ok && value != nil {
		forRef = jsonText(value)
	}
	claim := jsonObject(task["claim"])
	claimActor := ""
	claimLive := false
	if claim != nil {
		claimActor = jsonText(claim["actor"])
		if until := jsonText(claim["until"]); until != "" {
			if at, err := time.Parse(time.RFC3339, until); err == nil && s.now.Before(at) {
				claimLive = true
			}
		}
	}
	effective := state
	if state == "claimed" && !claimLive {
		effective = "ready"
	}
	holder := state == "claimed" && claimLive && claimActor != "" && claimActor == s.actor.ID
	owns := abilities.Manage ||
		(owner != "" && owner == s.actor.Principal &&
			(s.actor.Profile == nil || by == s.actor.ID) &&
			(abilities.Post || abilities.Label))

	moves := make([]string, 0, 8)
	add := func(move string) { moves = append(moves, move) }
	switch effective {
	case "waiting":
		if owns {
			add("assign")
			add("needs")
			add("cancel")
		}
		if abilities.Post {
			add("reply")
		}
	case "ready":
		if abilities.Label && (forRef == "" || forRef == s.actor.ID) {
			add("claim")
		}
		if owns {
			add("block")
			add("assign")
			add("needs")
			add("cancel")
		}
		if abilities.Post {
			add("reply")
		}
	case "claimed":
		if holder {
			add("renew")
			add("release")
			add("complete")
			add("fail")
			add("block")
		}
		if owns {
			// The board's own claim fence refuses an owner block while a live
			// claim exists, so only the holder path above may offer it.
			add("assign")
			add("cancel")
		}
		if abilities.Post {
			add("reply")
		}
	case "blocked":
		if owns {
			add("unblock")
			add("assign")
			add("needs")
			add("cancel")
		}
		if abilities.Post {
			add("reply")
		}
	case "failed":
		if owns {
			add("reopen")
			add("cancel")
		}
		if abilities.Post {
			add("reply")
		}
	case "completed", "canceled":
		if abilities.Post {
			add("reply")
		}
	default:
		return []string{}
	}
	moves = dedupeMoves(moves)
	return moves
}

func dedupeMoves(moves []string) []string {
	if len(moves) < 2 {
		return moves
	}
	seen := make(map[string]bool, len(moves))
	out := moves[:0]
	for _, move := range moves {
		if seen[move] {
			continue
		}
		seen[move] = true
		out = append(out, move)
	}
	return out
}

// bridgeMoves and filterBridgeMoves were removed with the legacy
// superagentboard command bridge (change keel-plugin-split). A dependent plugin
// now reaches the board only through the board service; there is no second
// action namespace to filter.

// ---- sync credential merge ------------------------------------------------

// augmentSync merges the keel-side credential state into the board's entry
// views. board v0.1.2 measures targets, lag and 24h failures itself; those
// fields pass through untouched. A field the mounted board didn't send is
// stated as null, which the client reads as unknown rather than a zero.
// The board's own response shape and status are unchanged; an entry missing
// from this process's configuration is reported as credential "unknown",
// never invented as valid.
func (s *Surface) augmentSync(r *http.Request, value any) bool {
	tenantID, ok := requestTenant(r)
	if !ok {
		return false
	}
	if s.manager == nil || s.manager.tenant == uuid.Nil || s.manager.tenant != tenantID {
		// Sync configuration is deployment-tenant scoped. Merging it into
		// another tenant's board response would cross a tenant boundary.
		return false
	}
	views, err := s.manager.List(r.Context())
	if err != nil {
		return false
	}
	byKey := make(map[string]SyncEntryView, len(views))
	for _, view := range views {
		byKey[view.Key] = view
	}
	root, ok := value.(map[string]any)
	if !ok {
		return false
	}
	changed := false
	entries := func(collection []any) {
		for _, item := range collection {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			key := jsonText(entry["key"])
			if view, found := byKey[key]; found {
				entry["credential"] = boardCredentialLabel(view.Credential)
				if view.RestartRequired {
					entry["restartRequired"] = true
				}
			} else {
				entry["credential"] = "unknown"
			}
			// A mounted board older than v0.1.2 sends no health facts. null
			// says "unknown"; a measured board value is preserved as-is.
			if _, measured := entry["lag"]; !measured {
				entry["lag"] = nil
			}
			if _, measured := entry["targets"]; !measured {
				entry["targets"] = nil
			}
			if _, measured := entry["failures24h"]; !measured {
				entry["failures24h"] = nil
			}
			// The design names the flat oldest-pending age; the mounted board
			// reports the same fact as lag.{inbox,outbox}.ageSeconds. Derive
			// the flat field only when the board did not send one, and leave
			// it null when no queue has an outstanding row.
			if _, measured := entry["oldestPendingAgeSeconds"]; !measured {
				entry["oldestPendingAgeSeconds"] = oldestPendingAgeSeconds(entry["lag"])
			}
			changed = true
		}
	}
	hasList := false
	if collection, ok := root["entries"].([]any); ok {
		hasList = true
		entries(collection)
	}
	if collection, ok := root["keys"].([]any); ok {
		hasList = true
		entries(collection)
		if flags, err := s.flags.Get(r.Context(), tenantID); err == nil {
			root["syncEnabled"] = flags.FullyEnabled()
		}
		changed = true
	}
	if !hasList {
		if key := jsonText(root["key"]); key != "" {
			entries([]any{root})
		}
	}
	if changed {
		if _, measured := root["lag"]; !measured {
			root["lag"] = nil
		}
		if _, measured := root["oldestPendingAgeSeconds"]; !measured {
			root["oldestPendingAgeSeconds"] = oldestPendingAgeSeconds(root["lag"])
		}
	}
	return changed
}

// oldestPendingAgeSeconds is the design's flat field over the board's lag
// shape: the oldest outstanding row across both queues, or null when neither
// queue has one (or the board sent no measurement).
func oldestPendingAgeSeconds(lag any) any {
	row, ok := lag.(map[string]any)
	if !ok {
		return nil
	}
	var oldest int64
	found := false
	for _, name := range []string{"inbox", "outbox"} {
		queue, ok := row[name].(map[string]any)
		if !ok {
			continue
		}
		age, ok := jsonAgeSeconds(queue["ageSeconds"])
		if !ok {
			continue
		}
		if !found || age > oldest {
			oldest = age
			found = true
		}
	}
	if !found {
		return nil
	}
	return oldest
}

func jsonAgeSeconds(value any) (int64, bool) {
	switch number := value.(type) {
	case json.Number:
		age, err := number.Int64()
		return age, err == nil
	case float64:
		return int64(number), true
	case int64:
		return number, true
	case int:
		return int64(number), true
	default:
		return 0, false
	}
}

func boardCredentialLabel(status CredentialStatus) string {
	switch status {
	case CredentialConfigured:
		return "valid"
	case CredentialMissing, CredentialInvalid:
		return "invalid"
	default:
		return "unknown"
	}
}

// ---- JSON helpers ---------------------------------------------------------

func jsonObject(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}

func jsonText(value any) string {
	text, _ := value.(string)
	return text
}

func jsonBool(value any) bool {
	boolean, _ := value.(bool)
	return boolean
}

func refNumber(value any) uint64 {
	switch ref := value.(type) {
	case string:
		number, err := strconv.ParseUint(strings.TrimSpace(ref), 10, 64)
		if err == nil {
			return number
		}
	case json.Number:
		number, err := strconv.ParseUint(ref.String(), 10, 64)
		if err == nil {
			return number
		}
	}
	return 0
}

// ---- buffered response ----------------------------------------------------

type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newBufferedResponse() *bufferedResponse {
	return &bufferedResponse{header: make(http.Header)}
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

func (b *bufferedResponse) result() (int, http.Header, []byte) {
	status := b.status
	if status == 0 {
		status = http.StatusOK
	}
	return status, b.header, b.body.Bytes()
}

func writeBuffered(w http.ResponseWriter, header http.Header, status int, body []byte) {
	for key, values := range header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	// The buffered body's length may differ from an upstream Content-Length.
	w.Header().Del("Content-Length")
	w.WriteHeader(status)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

// boardMeChannelCan is the channel `can` mapping `/v1/board/me` returns.
type boardMeChannelCan struct {
	Read   bool `json:"read"`
	Write  bool `json:"write"`
	Manage bool `json:"manage"`
}
