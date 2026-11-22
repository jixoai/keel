// Real-PostgreSQL tests for the A4.4c audit read surface.
//
// They run only when KEEL_TEST_DSN is set, like the other identity PG tests.
// Each test gets a fresh tenant from newIdentityEnv; the database itself is a
// disposable one created by the task runner and DROPped after the run.
package harnessapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/store"
)

// seedAudit inserts one audit row with an explicit created_at so the time and
// keyset filters are deterministic. The production write path always uses now().
func seedAudit(t *testing.T, env *identityEnv, tenant uuid.UUID, accountID, actorID uuid.UUID,
	kind, result string, detail map[string]any, createdAt time.Time) int64 {
	t.Helper()
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := env.st.Pool().QueryRow(context.Background(),
		`INSERT INTO auth_events
		    (tenant_id, account_id, actor_account_id, kind, result, detail, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)
		 RETURNING event_id`,
		tenant, auditNullableUUID(accountID), auditNullableUUID(actorID),
		kind, result, raw, createdAt.UTC()).Scan(&id); err != nil {
		t.Fatalf("seed audit %s: %v", kind, err)
	}
	return id
}

func auditNullableUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func auditWindow(from, to time.Time) url.Values {
	return url.Values{
		"from":  {from.UTC().Format(time.RFC3339Nano)},
		"to":    {to.UTC().Format(time.RFC3339Nano)},
		"limit": {"200"},
	}
}

func auditPath(params url.Values) string {
	return "/v1/audit/events?" + params.Encode()
}

func auditList(t *testing.T, env *identityEnv, params url.Values) (int, map[string]any) {
	t.Helper()
	return env.asAlice(http.MethodGet, auditPath(params), nil)
}

func auditEvents(t *testing.T, body map[string]any) []any {
	t.Helper()
	raw, ok := body["events"].([]any)
	if !ok {
		t.Fatalf("events is not an array: %v", body)
	}
	return raw
}

func auditEventByID(t *testing.T, events []any, eventID int64) map[string]any {
	t.Helper()
	want := strconv.FormatInt(eventID, 10)
	for _, raw := range events {
		event, _ := raw.(map[string]any)
		if event["eventId"] == want {
			return event
		}
	}
	t.Fatalf("event %s not in page: %v", want, events)
	return nil
}

func auditWantString(t *testing.T, event map[string]any, field, want string) {
	t.Helper()
	if got, _ := event[field].(string); got != want {
		t.Fatalf("%s = %q, want %q; event=%v", field, got, want, event)
	}
}

func auditEventIDs(t *testing.T, events []any) []int64 {
	t.Helper()
	out := make([]int64, 0, len(events))
	for _, raw := range events {
		event, _ := raw.(map[string]any)
		id, err := strconv.ParseInt(fmt.Sprint(event["eventId"]), 10, 64)
		if err != nil {
			t.Fatalf("eventId %v is not an integer: %v", event["eventId"], err)
		}
		out = append(out, id)
	}
	return out
}

func auditWantIDs(t *testing.T, body map[string]any, want ...int64) {
	t.Helper()
	got := auditEventIDs(t, auditEvents(t, body))
	if len(got) != len(want) {
		t.Fatalf("event ids = %v, want %v; body=%v", got, want, body)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event ids = %v, want %v (order matters)", got, want)
		}
	}
}

// TestAuditReadContractAndFilters pins the Web contract field by field and the
// time/kind/result/account/q filters against a real ledger.
func TestAuditReadContractAndFilters(t *testing.T) {
	env := newIdentityEnv(t)
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	sessionID := uuid.New()

	loginID := seedAudit(t, env, env.tenant, env.alice.ID, env.alice.ID,
		store.AuthEventLoginSucceeded, store.AuthResultOK, map[string]any{
			"username": env.alice.Username, "session_id": sessionID.String(),
			"request_id": "req-login-1", "ip_hash": "deadbeefdeadbeef",
		}, base)
	denyID := seedAudit(t, env, env.tenant, env.alice.ID, env.alice.ID,
		store.AuthEventPermissionDenied, store.AuthResultDenied, map[string]any{
			"key": "bot.invoke", "reason": "missing_grant",
			"resource_type": "bot", "resource_id": env.supportBot.ID,
		}, base.Add(time.Minute))
	grantID := seedAudit(t, env, env.tenant, env.alice.ID, env.super.ID,
		store.AuthEventPermissionGrant, store.AuthResultOK, map[string]any{
			"action": "grant", "via": "group_membership",
			"group_id": env.opsGroup.ID.String(), "key": "bot.read", "scope": "tenant",
		}, base.Add(2*time.Minute))
	invokeID := seedAudit(t, env, env.tenant, env.alice.ID, env.alice.ID,
		store.AuthEventBotInvoked, store.AuthResultOK, map[string]any{
			"bot_id": env.supportBot.ID, "session_id": sessionID.String(), "result": "failed",
		}, base.Add(3*time.Minute))
	relationID := seedAudit(t, env, env.tenant, uuid.Nil, env.alice.ID,
		store.AuthEventRelationChanged, store.AuthResultOK, map[string]any{
			"action": "create", "resource_type": "team_bot", "resource_id": "team-alpha",
			"relation": "owner", "subject_type": "account", "subject_id": env.alice.ID.String(),
		}, base.Add(4*time.Minute))
	superID := seedAudit(t, env, env.tenant, env.super.ID, env.super.ID,
		store.AuthEventSuperAdmin, store.AuthResultOK, map[string]any{
			"action": "account.create", "target_account_id": env.bob.ID.String(),
		}, base.Add(5*time.Minute))
	createdID := seedAudit(t, env, env.tenant, env.bob.ID, env.alice.ID,
		store.AuthEventAccountCreated, store.AuthResultOK, map[string]any{
			"username": env.bob.Username, "kind": "human",
		}, base.Add(6*time.Minute))
	otherID := seedAudit(t, env, env.otherTenant, env.otherAcct.ID, env.otherAcct.ID,
		store.AuthEventLoginFailed, store.AuthResultFailed, map[string]any{
			"username": env.otherAcct.Username, "reason": "unknown_account",
		}, base.Add(7*time.Minute))

	window := auditWindow(base.Add(-time.Hour), time.Now().UTC().Add(time.Hour))
	status, body := auditList(t, env, window)
	wantStatus(t, status, body, http.StatusOK, "list audit events")
	events := auditEvents(t, body)
	if len(events) != 7 {
		t.Fatalf("events = %d, want 7; body=%v", len(events), body)
	}
	for _, raw := range events {
		event, _ := raw.(map[string]any)
		if event["eventId"] == strconv.FormatInt(otherID, 10) {
			t.Fatal("cross-tenant audit event leaked into the list")
		}
	}

	// Newest first, by occurredAt (the page's ordering key).
	previous := time.Now().UTC().Add(24 * time.Hour)
	for _, raw := range events {
		event, _ := raw.(map[string]any)
		occurred, err := time.Parse(time.RFC3339Nano, fmt.Sprint(event["occurredAt"]))
		if err != nil {
			t.Fatalf("occurredAt %v is not RFC3339: %v", event["occurredAt"], err)
		}
		if occurred.After(previous) {
			t.Fatalf("events are not newest-first: %v then %v", previous, occurred)
		}
		previous = occurred
	}

	// e2: a denial names the missing capability and the Bot owner.
	deny := auditEventByID(t, events, denyID)
	auditWantString(t, deny, "kind", "deny")
	auditWantString(t, deny, "action", store.AuthEventPermissionDenied)
	auditWantString(t, deny, "result", "denied")
	auditWantString(t, deny, "actorName", "alice")
	auditWantString(t, deny, "actorKind", "human")
	auditWantString(t, deny, "actorId", env.alice.ID.String())
	auditWantString(t, deny, "accountId", env.alice.ID.String())
	auditWantString(t, deny, "objectType", "Bot")
	auditWantString(t, deny, "objectName", env.supportBot.ID)
	auditWantString(t, deny, "objectId", env.supportBot.ID)
	auditWantString(t, deny, "objectOwnerName", "bot-owner")
	auditWantString(t, deny, "objectOwnerId", env.botAccount.ID.String())
	auditWantString(t, deny, "capabilityKey", "bot.invoke")
	if summary, _ := deny["summary"].(string); summary == "" {
		t.Fatal("denial has no summary")
	}
	detail, _ := deny["detail"].(map[string]any)
	if detail["key"] != "bot.invoke" {
		t.Fatalf("detail.key = %v, want bot.invoke", detail["key"])
	}
	grantable, _ := deny["grantableBy"].([]any)
	if len(grantable) == 0 {
		t.Fatalf("denial has no grantableBy hint: %v", deny)
	}
	foundSuper := false
	for _, raw := range grantable {
		if raw == "super-admin" {
			foundSuper = true
		}
	}
	if !foundSuper {
		t.Fatalf("grantableBy = %v, want the seeded super-admin group", grantable)
	}

	// e1: login success carries the request id and the masked hash detail.
	login := auditEventByID(t, events, loginID)
	auditWantString(t, login, "kind", "login")
	auditWantString(t, login, "action", store.AuthEventLoginSucceeded)
	auditWantString(t, login, "result", "ok")
	auditWantString(t, login, "requestId", "req-login-1")
	auditWantString(t, login, "objectType", "账号")
	auditWantString(t, login, "objectName", "alice")
	loginDetail, _ := login["detail"].(map[string]any)
	if loginDetail["ip_hash"] != "[REDACTED]" {
		t.Fatalf("detail.ip_hash = %v, want [REDACTED]", loginDetail["ip_hash"])
	}
	if loginDetail["session_id"] != sessionID.String() {
		t.Fatalf("detail.session_id = %v, want %s", loginDetail["session_id"], sessionID)
	}
	if _, present := login["source"]; present {
		t.Fatalf("a hash-only IP must not become a source: %v", login["source"])
	}

	// e3: a group membership grant names the group, not the target account.
	grant := auditEventByID(t, events, grantID)
	auditWantString(t, grant, "kind", "grant")
	auditWantString(t, grant, "action", store.AuthEventPermissionGrant)
	auditWantString(t, grant, "actorName", "super")
	auditWantString(t, grant, "objectType", "权限组")
	auditWantString(t, grant, "objectName", "ops")
	auditWantString(t, grant, "objectId", env.opsGroup.ID.String())

	// e4: the invocation's product outcome comes from detail.result.
	invoke := auditEventByID(t, events, invokeID)
	auditWantString(t, invoke, "kind", "invoke")
	auditWantString(t, invoke, "result", "error")
	auditWantString(t, invoke, "objectType", "Bot")
	auditWantString(t, invoke, "objectName", env.supportBot.ID)
	auditWantString(t, invoke, "runId", sessionID.String())
	if summary, _ := invoke["summary"].(string); !strings.Contains(summary, "失败") {
		t.Fatalf("failed invoke summary = %q, want 失败", summary)
	}

	// e5: a TeamBot relation change opens the team.
	relation := auditEventByID(t, events, relationID)
	auditWantString(t, relation, "kind", "relation")
	auditWantString(t, relation, "objectType", "团队")
	auditWantString(t, relation, "objectId", "team-alpha")

	// e6/e7: super-admin and account buckets keep their raw action token.
	super := auditEventByID(t, events, superID)
	auditWantString(t, super, "kind", "super-admin")
	auditWantString(t, super, "action", store.AuthEventSuperAdmin)
	auditWantString(t, super, "objectType", "账号")
	auditWantString(t, super, "objectId", env.bob.ID.String())
	created := auditEventByID(t, events, createdID)
	auditWantString(t, created, "kind", "account")
	auditWantString(t, created, "objectType", "账号")
	auditWantString(t, created, "objectName", "bob")
	auditWantString(t, created, "actorName", "alice")

	// Filters. Every check keeps the same bounded window.
	filtered := func(mutate func(url.Values)) map[string]any {
		params := auditWindow(base.Add(-time.Hour), time.Now().UTC().Add(time.Hour))
		mutate(params)
		status, body := auditList(t, env, params)
		wantStatus(t, status, body, http.StatusOK, "filtered audit list")
		return body
	}
	auditWantIDs(t, filtered(func(v url.Values) { v.Set("kind", "deny") }), denyID)
	auditWantIDs(t, filtered(func(v url.Values) { v.Set("kind", store.AuthEventPermissionDenied) }), denyID)
	auditWantIDs(t, filtered(func(v url.Values) { v.Set("kind", "grant") }), grantID)
	auditWantIDs(t, filtered(func(v url.Values) { v.Set("result", "denied") }), denyID)
	// error includes the failed bot.invoked whose stored row result is ok.
	auditWantIDs(t, filtered(func(v url.Values) { v.Set("result", "error") }), invokeID)
	auditWantIDs(t, filtered(func(v url.Values) { v.Set("accountId", env.bob.ID.String()) }), createdID)
	auditWantIDs(t, filtered(func(v url.Values) { v.Set("accountId", env.alice.ID.String()) }),
		createdID, relationID, invokeID, grantID, denyID, loginID)
	auditWantIDs(t, filtered(func(v url.Values) { v.Set("q", "ops") }), grantID)
	auditWantIDs(t, filtered(func(v url.Values) { v.Set("q", env.supportBot.ID) }), invokeID, denyID)
	auditWantIDs(t, filtered(func(v url.Values) { v.Set("q", store.AuthEventPermissionDenied) }), denyID)
	auditWantIDs(t, filtered(func(v url.Values) {
		v.Set("from", base.Add(2*time.Minute).Format(time.RFC3339Nano))
		v.Set("to", base.Add(3*time.Minute).Format(time.RFC3339Nano))
	}), grantID)

	// `since` is the Web client's lower bound; `to` defaults to now.
	since := url.Values{"limit": {"200"}, "since": {base.Add(2 * time.Minute).Format(time.RFC3339Nano)}}
	status, body = auditList(t, env, since)
	wantStatus(t, status, body, http.StatusOK, "since alias")
	auditWantIDs(t, body, createdID, superID, relationID, invokeID, grantID)

	// Unix seconds are accepted as well as RFC3339.
	unix := url.Values{
		"limit": {"200"},
		"from":  {strconv.FormatInt(base.Add(2*time.Minute).Unix(), 10)},
		"to":    {strconv.FormatInt(base.Add(3*time.Minute).Unix(), 10)},
	}
	status, body = auditList(t, env, unix)
	wantStatus(t, status, body, http.StatusOK, "unix bounds")
	auditWantIDs(t, body, grantID)

	// Bad input: malformed timestamps, an unparseable account and an over-wide
	// window all answer 400, never an empty 200 that hides the mistake.
	status, body = env.asAlice(http.MethodGet, "/v1/audit/events?from=yesterday", nil)
	wantStatus(t, status, body, http.StatusBadRequest, "bad from")
	status, body = env.asAlice(http.MethodGet, "/v1/audit/events?to=not-a-time", nil)
	wantStatus(t, status, body, http.StatusBadRequest, "bad to")
	status, body = env.asAlice(http.MethodGet, "/v1/audit/events?accountId=nope", nil)
	wantStatus(t, status, body, http.StatusBadRequest, "bad accountId")
	status, body = env.asAlice(http.MethodGet,
		"/v1/audit/events?tenantId=not-a-uuid", nil)
	wantStatus(t, status, body, http.StatusBadRequest, "bad tenantId")
	status, body = env.asAlice(http.MethodGet, "/v1/audit/events?"+url.Values{
		"from": {time.Now().UTC().Add(-100 * 24 * time.Hour).Format(time.RFC3339)},
		"to":   {time.Now().UTC().Format(time.RFC3339)},
	}.Encode(), nil)
	wantStatus(t, status, body, http.StatusBadRequest, "over-wide range")
}

// TestAuditReadPaginationIsStable proves the keyset page neither repeats nor
// skips a row, and that the cursor aliases and limit bounds hold.
func TestAuditReadPaginationIsStable(t *testing.T) {
	env := newIdentityEnv(t)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	ids := make([]int64, 0, 7)
	for i := 0; i < 7; i++ {
		ids = append(ids, seedAudit(t, env, env.tenant, env.alice.ID, env.alice.ID,
			store.AuthEventLoginSucceeded, store.AuthResultOK, map[string]any{
				"username": env.alice.Username, "session_id": uuid.NewString(),
			}, base.Add(time.Duration(i)*time.Second)))
	}

	params := auditWindow(base.Add(-time.Minute), time.Now().UTC().Add(time.Minute))
	params.Set("limit", "2")
	seen := map[string]bool{}
	got := make([]string, 0, len(ids))
	for page := 0; page < 10; page++ {
		status, body := auditList(t, env, params)
		wantStatus(t, status, body, http.StatusOK, "paged audit list")
		events := auditEvents(t, body)
		if len(events) > 2 {
			t.Fatalf("page %d has %d events, want <= 2", page, len(events))
		}
		for _, raw := range events {
			event, _ := raw.(map[string]any)
			id := fmt.Sprint(event["eventId"])
			if seen[id] {
				t.Fatalf("event %s repeated across pages", id)
			}
			seen[id] = true
			got = append(got, id)
		}
		hasMore, _ := body["hasMore"].(bool)
		cursor, _ := body["nextCursor"].(string)
		if !hasMore {
			if cursor != "" {
				t.Fatalf("last page carries a nextCursor %q", cursor)
			}
			break
		}
		if cursor == "" {
			t.Fatal("hasMore=true with an empty nextCursor")
		}
		params.Set("after", cursor)
	}
	if len(got) != len(ids) {
		t.Fatalf("paged %d events, want %d: %v", len(got), len(ids), got)
	}
	for i := range ids {
		want := strconv.FormatInt(ids[len(ids)-1-i], 10)
		if got[i] != want {
			t.Fatalf("page order = %v, want newest-first %v", got, ids)
		}
	}

	// `cursor` is an alias for `after`.
	first := auditWindow(base.Add(-time.Minute), time.Now().UTC().Add(time.Minute))
	first.Set("limit", "1")
	status, body := auditList(t, env, first)
	wantStatus(t, status, body, http.StatusOK, "first page")
	cursor, _ := body["nextCursor"].(string)
	if cursor == "" {
		t.Fatal("first page has no cursor")
	}
	second := auditWindow(base.Add(-time.Minute), time.Now().UTC().Add(time.Minute))
	second.Set("limit", "1")
	second.Set("cursor", cursor)
	status, body = auditList(t, env, second)
	wantStatus(t, status, body, http.StatusOK, "cursor alias")
	aliasIDs := auditEventIDs(t, auditEvents(t, body))
	if len(aliasIDs) != 1 || aliasIDs[0] != ids[len(ids)-2] {
		t.Fatalf("cursor alias page = %v, want [%d]", aliasIDs, ids[len(ids)-2])
	}

	// Invalid cursor and out-of-range limits are client errors.
	bad := auditWindow(base.Add(-time.Minute), time.Now().UTC().Add(time.Minute))
	bad.Set("after", "%%%not-base64%%%")
	status, body = auditList(t, env, bad)
	wantStatus(t, status, body, http.StatusBadRequest, "bad cursor")

	for _, limit := range []string{"0", "201"} {
		params := auditWindow(base.Add(-time.Minute), time.Now().UTC().Add(time.Minute))
		params.Set("limit", limit)
		status, body := auditList(t, env, params)
		wantStatus(t, status, body, http.StatusBadRequest, "limit="+limit)
	}
	params = auditWindow(base.Add(-time.Minute), time.Now().UTC().Add(time.Minute))
	params.Set("limit", "200")
	status, body = auditList(t, env, params)
	wantStatus(t, status, body, http.StatusOK, "limit=200")
	if hasMore, _ := body["hasMore"].(bool); hasMore {
		t.Fatalf("limit=200 over 7 rows reports hasMore: %v", body)
	}
}

// TestAuditReadAuthAndTenantBoundary covers 401/403, the super-admin cross-
// tenant read and the 404 for another tenant's row id.
func TestAuditReadAuthAndTenantBoundary(t *testing.T) {
	env := newIdentityEnv(t)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	ownID := seedAudit(t, env, env.tenant, env.alice.ID, env.alice.ID,
		store.AuthEventLoginSucceeded, store.AuthResultOK, map[string]any{
			"username": env.alice.Username,
		}, base)
	otherID := seedAudit(t, env, env.otherTenant, env.otherAcct.ID, env.otherAcct.ID,
		store.AuthEventLoginSucceeded, store.AuthResultOK, map[string]any{
			"username": env.otherAcct.Username,
		}, base.Add(time.Minute))

	// 401: the request reaches the product API without an edge principal.
	api := New(Config{Identity: env.st, Access: env.bridge, Logger: testLogger()})
	server := httptest.NewServer(api)
	defer server.Close()
	req, err := http.NewRequest(http.MethodGet, server.URL+"/v1/audit/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", env.tenant.String())
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated audit read = %d, want 401", res.StatusCode)
	}

	// 403: bob holds no audit.read. List and detail answer the same way.
	status, body := env.do(http.MethodGet, "/v1/audit/events", env.bob, false, nil)
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "bob lists audit")
	status, body = env.do(http.MethodGet, "/v1/audit/events/"+strconv.FormatInt(ownID, 10), env.bob, false, nil)
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "bob reads audit detail")

	// An ordinary tenant admin may not name another tenant.
	status, body = env.asAlice(http.MethodGet,
		"/v1/audit/events?tenantId="+env.otherTenant.String(), nil)
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "alice crosses tenants")

	// The super admin can; the answer contains only that tenant's rows.
	status, body = env.asSuper(http.MethodGet,
		"/v1/audit/events?tenantId="+env.otherTenant.String(), nil)
	wantStatus(t, status, body, http.StatusOK, "super lists other tenant")
	auditWantIDs(t, body, otherID)

	// The default list is tenant-scoped: no other-tenant row. The three refused
	// attempts above are themselves audited as permission.denied rows, so this
	// assertion narrows to the login bucket rather than expecting a clean ledger.
	status, body = env.asAlice(http.MethodGet,
		"/v1/audit/events?kind=login&from="+url.QueryEscape(base.Add(-time.Minute).Format(time.RFC3339Nano)), nil)
	wantStatus(t, status, body, http.StatusOK, "alice lists own tenant")
	auditWantIDs(t, body, ownID)

	// Detail: another tenant's id is 404 for an ordinary caller; the explicit
	// tenantId asks for the other ledger and is 403; the super admin gets it.
	status, body = env.asAlice(http.MethodGet,
		"/v1/audit/events/"+strconv.FormatInt(otherID, 10), nil)
	wantStatus(t, status, body, http.StatusNotFound, "cross-tenant event id")
	status, body = env.asAlice(http.MethodGet,
		"/v1/audit/events/"+strconv.FormatInt(otherID, 10)+"?tenantId="+env.otherTenant.String(), nil)
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "alice detail cross tenant")
	status, body = env.asSuper(http.MethodGet,
		"/v1/audit/events/"+strconv.FormatInt(otherID, 10)+"?tenantId="+env.otherTenant.String(), nil)
	wantStatus(t, status, body, http.StatusOK, "super detail cross tenant")
	detail, _ := body["event"].(map[string]any)
	if detail["eventId"] != strconv.FormatInt(otherID, 10) {
		t.Fatalf("detail = %v, want event %d", body, otherID)
	}
	// The super admin's own tenant does not contain the other row.
	status, body = env.asSuper(http.MethodGet,
		"/v1/audit/events/"+strconv.FormatInt(otherID, 10), nil)
	wantStatus(t, status, body, http.StatusNotFound, "super default tenant misses other row")

	// Malformed/nonexistent ids.
	status, body = env.asAlice(http.MethodGet, "/v1/audit/events/not-a-number", nil)
	wantStatus(t, status, body, http.StatusBadRequest, "bad event id")
	status, body = env.asAlice(http.MethodGet, "/v1/audit/events/999999999", nil)
	wantStatus(t, status, body, http.StatusNotFound, "missing event id")
}

// TestAuditReadRedactsSensitiveDetail proves no password/token/key/hash value
// crosses the wire, including values inside nested maps, lists and free text.
func TestAuditReadRedactsSensitiveDetail(t *testing.T) {
	env := newIdentityEnv(t)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	seedAudit(t, env, env.tenant, env.alice.ID, env.alice.ID,
		store.AuthEventPermissionDenied, store.AuthResultDenied, map[string]any{
			"username":        env.alice.Username,
			"password":        "hunter2-password",
			"password_hash":   "$2a$10$abcdefghijklmnopqrstuv",
			"token":           "tok_live_1234567890",
			"token_hash":      "deadbeefdeadbeefdeadbeef",
			"api_key":         "sk-test-0123456789abcdef",
			"private_key":     "-----BEGIN PRIVATE KEY-----",
			"ip_hash":         "cafebabecafebabe",
			"user_agent_hash": "feedfacefeedface",
			"key_prefix":      "keel_bot_abc123",
			"note":            "Authorization: Bearer abcdefghijklmnopqrstuvwxyz",
			"key":             "session.read",
			"nested":          map[string]any{"secret": "s3cr3t-value", "safe": "ok"},
			"list":            []any{"token=abc123456", "plain"},
		}, base)

	status, raw := auditGetRaw(t, env, "/v1/audit/events?limit=1", env.alice, false)
	if status != http.StatusOK {
		t.Fatalf("audit redaction read = %d, body=%s", status, raw)
	}
	bodyText := string(raw)
	for _, secret := range []string{
		"hunter2-password", "$2a$10$", "tok_live_1234567890", "deadbeefdeadbeefdeadbeef",
		"sk-test-0123456789abcdef", "BEGIN PRIVATE KEY", "cafebabecafebabe",
		"feedfacefeedface", "keel_bot_abc123", "abcdefghijklmnopqrstuvwxyz", "s3cr3t-value", "abc123456",
	} {
		if strings.Contains(bodyText, secret) {
			t.Fatalf("response leaked %q: %s", secret, bodyText)
		}
	}
	if !strings.Contains(bodyText, `"key":"session.read"`) {
		t.Fatalf("response dropped the capability key: %s", bodyText)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	events := auditEvents(t, decoded)
	event, _ := events[0].(map[string]any)
	detail, _ := event["detail"].(map[string]any)
	for _, field := range []string{"password", "password_hash", "token", "token_hash", "api_key", "private_key", "ip_hash", "user_agent_hash", "key_prefix"} {
		if detail[field] != "[REDACTED]" {
			t.Fatalf("detail.%s = %v, want [REDACTED]", field, detail[field])
		}
	}
	if detail["username"] != env.alice.Username {
		t.Fatalf("detail.username = %v, want %s", detail["username"], env.alice.Username)
	}
	nested, _ := detail["nested"].(map[string]any)
	if nested["secret"] != "[REDACTED]" || nested["safe"] != "ok" {
		t.Fatalf("nested redaction = %v", nested)
	}

	// q must not become an oracle over a secret: it searches the exposed
	// identifiers only, not the whole detail JSON.
	status, body := env.asAlice(http.MethodGet, "/v1/audit/events?q=hunter2-password", nil)
	wantStatus(t, status, body, http.StatusOK, "q over a secret")
	if len(auditEvents(t, body)) != 0 {
		t.Fatalf("q matched a redacted secret: %v", body)
	}
	status, body = env.asAlice(http.MethodGet, "/v1/audit/events?q=session.read", nil)
	wantStatus(t, status, body, http.StatusOK, "q over a capability key")
	if len(auditEvents(t, body)) != 1 {
		t.Fatalf("q=session.read = %v, want the seeded denial", body)
	}
}

// TestAuditReadEmptyPageShape pins the empty-page wire shape: an array, never
// null, plus explicit paging fields the client checks.
func TestAuditReadEmptyPageShape(t *testing.T) {
	env := newIdentityEnv(t)
	status, raw := auditGetRaw(t, env,
		"/v1/audit/events?tenantId="+env.otherTenant.String(), env.super, true)
	if status != http.StatusOK {
		t.Fatalf("empty audit read = %d, body=%s", status, raw)
	}
	bodyText := string(raw)
	if !strings.Contains(bodyText, `"events":[]`) {
		t.Fatalf("empty page events is not []: %s", bodyText)
	}
	if !strings.Contains(bodyText, `"hasMore":false`) {
		t.Fatalf("empty page hasMore is not false: %s", bodyText)
	}
	if !strings.Contains(bodyText, `"nextCursor":""`) {
		t.Fatalf("empty page nextCursor is not empty: %s", bodyText)
	}
}

// auditGetRaw issues one request through the fixture server and returns the
// undecoded body, so a test can assert a secret is absent from the bytes.
func auditGetRaw(t *testing.T, env *identityEnv, path string, account store.AccountRecord, super bool) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, env.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-Account", account.ID.String())
	if super {
		req.Header.Set("X-Test-Super", "1")
	}
	res, err := env.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, raw
}
