package boardintegration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/access"
	keelstore "github.com/jixoai/keel/internal/store"

	"github.com/jixoai/keel-board/protocol"
)

// TestBoardHTTPIdentityRBACAndCrossTenant covers 10.2/10.3/10.4 at the HTTP
// boundary: human vs Bot actors, owner/by/for, read-vs-write 403, channel
// `*` administration, and the A/B tenant no-leak assertion.
func TestBoardHTTPIdentityRBACAndCrossTenant(t *testing.T) {
	env := newTestEnv(t, nil)
	server := env.server()

	// A board request with no edge principal is refused before any relation
	// lookup. In production the auth middleware owns this 401; the adapter must
	// also fail closed when a handler is reached without it.
	if resp := env.do(t, server, "GET", "/v1/board/channels", "", nil); resp.status != 401 {
		t.Fatalf("unauthenticated board list = %d: %s", resp.status, resp.body)
	}

	// Creating a channel must make the creator its owner (the keel adapter
	// records the relation right after the board's 201).
	created := env.do(t, server, "POST", "/v1/board/channels", "alice", map[string]any{"path": "work"})
	var channel struct {
		Created bool `json:"created"`
	}
	env.decode(t, created, &channel)
	if !channel.Created {
		t.Fatalf("create channel did not report created: %s", created.body)
	}
	owned, err := env.st.HasResourceRelation(context.Background(), env.tenant, BoardChannelResourceType, "work", keelstore.RelationOwner, keelstore.RelationSubjectAccount, env.alice.record.ID.String())
	if err != nil {
		t.Fatalf("read owner relation: %v", err)
	}
	if !owned {
		t.Fatal("the channel creator did not receive the owner relation")
	}

	// Bob gets write authority plus an editor relation; Carol is read-only.
	env.relation(env.tenant, "work", keelstore.RelationEditor, env.bob.record.ID)
	env.relation(env.tenant, "work", keelstore.RelationViewer, env.carol.record.ID)

	humanPost := env.do(t, server, "POST", "/v1/board/messages", "alice", map[string]any{"channel": "work", "text": "hello from alice"})
	var posted struct {
		Message protocol.MessageView `json:"message"`
	}
	env.decode(t, humanPost, &posted)
	if posted.Message.By == nil || posted.Message.By.Kind != protocol.ActorPerson {
		t.Fatalf("human author = %+v", posted.Message.By)
	}
	if posted.Message.By.Handle == "" || posted.Message.By.Via != nil {
		t.Fatalf("human display fields = %+v", posted.Message.By)
	}

	botPost := env.do(t, server, "POST", "/v1/board/messages", "bot", map[string]any{"channel": "work", "text": "hello from the bot"})
	env.decode(t, botPost, &posted)
	if posted.Message.By == nil || posted.Message.By.Kind != protocol.ActorAgent {
		t.Fatalf("bot author = %+v", posted.Message.By)
	}
	if posted.Message.By.Handle != env.bot.ID || posted.Message.By.Name != env.bot.Name {
		t.Fatalf("bot handle/name = %q/%q, want %q/%q", posted.Message.By.Handle, posted.Message.By.Name, env.bot.ID, env.bot.Name)
	}
	if posted.Message.By.Via == nil || posted.Message.By.Via.Name != "Alice" {
		t.Fatalf("bot via = %+v, want the owning account", posted.Message.By.Via)
	}

	// Carol may read but not write. The board must answer 403 with the missing
	// authority, not 404: she has a viewer relation.
	readOnlyWrite := env.do(t, server, "POST", "/v1/board/messages", "carol", map[string]any{"channel": "work", "text": "carol cannot write"})
	hint := env.boardError(t, readOnlyWrite, 403, string(protocol.CodeForbidden))
	if !strings.Contains(hint["hint"].(string), "board.write") && !strings.Contains(hint["hint"].(string), "channel:editor") {
		t.Fatalf("403 hint = %v", hint)
	}

	// Posting does not implicitly create a channel: only POST /channels gets
	// the creation bootstrap, so a write into a never-created channel is 404.
	if ghost := env.do(t, server, "POST", "/v1/board/messages", "alice", map[string]any{"channel": "ghost", "text": "orphan"}); ghost.status != 404 {
		t.Fatalf("post to a nonexistent channel = %d: %s", ghost.status, ghost.body)
	}
	// A writer must not use the create route to probe an existing channel it
	// cannot read: the answer is 404, not 200 created=false.
	if resp := env.do(t, server, "POST", "/v1/board/channels", "alice", map[string]any{"path": "secret"}); resp.status != 201 {
		t.Fatalf("create secret: %d", resp.status)
	}
	if probe := env.do(t, server, "POST", "/v1/board/channels", "bob", map[string]any{"path": "secret"}); probe.status != 404 {
		t.Fatalf("unreadable existing channel probe = %d: %s", probe.status, probe.body)
	}
	// A writer can create a channel that does not exist; the adapter records it
	// as the creator's owner.
	if create := env.do(t, server, "POST", "/v1/board/channels", "bob", map[string]any{"path": "bob-chan"}); create.status != 201 {
		t.Fatalf("bob create = %d: %s", create.status, create.body)
	}
	if owned, err := env.st.HasResourceRelation(context.Background(), env.tenant, BoardChannelResourceType, "bob-chan", keelstore.RelationOwner, keelstore.RelationSubjectAccount, env.bob.record.ID.String()); err != nil || !owned {
		t.Fatalf("bob channel owner relation = %v/%v", owned, err)
	}

	// A channel owner (Alice) can manage tasks. Bob, an editor, can write but
	// does not hold the owner relation.
	task := env.do(t, server, "POST", "/v1/board/tasks", "alice", map[string]any{
		"channel": "work", "text": "fix the thing",
		"for": AccountActorID(env.alice.record.ID) + "+" + env.botKey,
	})
	var createdTask struct {
		Task protocol.MessageView `json:"task"`
	}
	env.decode(t, task, &createdTask)
	if createdTask.Task.Task == nil {
		t.Fatalf("task response has no task: %s", task.body)
	}
	if createdTask.Task.Task.Owner != AccountActorID(env.alice.record.ID) || createdTask.Task.Task.By != AccountActorID(env.alice.record.ID) {
		t.Fatalf("owner/by = %q/%q", createdTask.Task.Task.Owner, createdTask.Task.Task.By)
	}
	wantFor := AccountActorID(env.alice.record.ID) + "+" + env.botKey
	if createdTask.Task.Task.For == nil || *createdTask.Task.Task.For != wantFor {
		t.Fatalf("for = %v, want %q", createdTask.Task.Task.For, wantFor)
	}

	// Bob may not claim a task assigned to Alice's Bot: the board's
	// task_assigned guard runs after keel's write authorization.
	assigned := env.do(t, server, "POST", "/v1/board/tasks/"+createdTask.Task.ID+"/claim", "bob", map[string]any{})
	env.boardError(t, assigned, 409, string(protocol.CodeTaskAssigned))

	// Alice-as-human is also not the assigned actor.
	humanClaim := env.do(t, server, "POST", "/v1/board/tasks/"+createdTask.Task.ID+"/claim", "alice", map[string]any{})
	env.boardError(t, humanClaim, 409, string(protocol.CodeTaskAssigned))

	// The Bot is the assigned actor and may claim; the response carries the
	// one-time token.
	botClaim := env.do(t, server, "POST", "/v1/board/tasks/"+createdTask.Task.ID+"/claim", "bot", map[string]any{})
	var claim struct {
		Task  protocol.MessageView `json:"task"`
		Claim protocol.TaskClaim   `json:"claim"`
	}
	env.decode(t, botClaim, &claim)
	if claim.Claim.Token == "" || claim.Claim.Epoch == 0 {
		t.Fatalf("claim = %+v", claim.Claim)
	}
	if claim.Task.Task == nil || claim.Task.Task.State != "claimed" {
		t.Fatalf("claimed task = %+v", claim.Task.Task)
	}
	if claim.Task.Task.Claim == nil || claim.Task.Task.Claim.Actor != wantFor {
		t.Fatalf("claim actor = %+v, want %q", claim.Task.Task.Claim, wantFor)
	}

	completed := env.do(t, server, "POST", "/v1/board/tasks/"+createdTask.Task.ID+"/complete", "bot",
		map[string]any{"epoch": claim.Claim.Epoch, "token": claim.Claim.Token, "result": "done"})
	env.decode(t, completed, &createdTask)
	if createdTask.Task.Task == nil || createdTask.Task.Task.State != "completed" {
		t.Fatalf("completed task = %+v", createdTask.Task.Task)
	}

	// A task cannot be assigned across principals.
	foreign := env.do(t, server, "POST", "/v1/board/tasks", "alice", map[string]any{
		"channel": "work", "text": "foreign assignee",
		"for": AccountActorID(env.other) + "+" + BotProfileToken("someone-else"),
	})
	env.boardError(t, foreign, 400, string(protocol.CodeInvalidRequest))

	// channel `*` administration is board.manage at tenant scope.
	if resp := env.do(t, server, "POST", "/v1/board/admin/projection/check", "carol", map[string]any{}); resp.status != 403 {
		t.Fatalf("read-only projection check = %d: %s", resp.status, resp.body)
	}
	// board.write does not imply board.manage.
	if resp := env.do(t, server, "POST", "/v1/board/admin/projection/check", "bob", map[string]any{}); resp.status != 403 {
		t.Fatalf("writer projection check = %d: %s", resp.status, resp.body)
	}
	if resp := env.do(t, server, "POST", "/v1/board/admin/projection/check", "alice", map[string]any{}); resp.status != 200 {
		t.Fatalf("admin projection check = %d: %s", resp.status, resp.body)
	}

	// Tenant B writes its own board. Tenant A must never see its content.
	otherCreate := env.do(t, server, "POST", "/v1/board/channels", "dave", map[string]any{"path": "other"})
	env.decode(t, otherCreate, &channel)
	davePost := env.do(t, server, "POST", "/v1/board/messages", "dave", map[string]any{"channel": "other", "text": "tenant B secret text"})
	if davePost.status != 201 {
		t.Fatalf("tenant B post = %d: %s", davePost.status, davePost.body)
	}
	var davePosted struct {
		Message protocol.MessageView `json:"message"`
	}
	env.decode(t, davePost, &davePosted)
	// Board IDs are per-tenant positions, so tenant A may legitimately have a
	// message with this number. What it must never return is tenant B's text.
	aliceByID := env.do(t, server, "GET", "/v1/board/messages/"+davePosted.Message.ID, "alice", nil)
	if strings.Contains(string(aliceByID.body), "tenant B secret text") {
		t.Fatalf("tenant A read tenant B content by id: %s", aliceByID.body)
	}
	aliceList := env.do(t, server, "GET", "/v1/board/messages?channel=other", "alice", nil)
	if strings.Contains(string(aliceList.body), "tenant B secret text") {
		t.Fatalf("tenant A read tenant B content: %s", aliceList.body)
	}
	workList := env.do(t, server, "GET", "/v1/board/messages?channel=work", "alice", nil)
	if strings.Contains(string(workList.body), "tenant B secret text") {
		t.Fatalf("tenant A work list contains tenant B content: %s", workList.body)
	}

	// An explicit viewer on a child narrows an inherited editor/owner: Alice
	// owns work but Bob creates work/secret as its owner; Alice receives only
	// viewer there, so her next post must be refused.
	secretCreate := env.do(t, server, "POST", "/v1/board/channels", "bob", map[string]any{"path": "work/secret"})
	if secretCreate.status != 201 {
		t.Fatalf("bob create work/secret = %d: %s", secretCreate.status, secretCreate.body)
	}
	env.relation(env.tenant, "work/secret", keelstore.RelationViewer, env.alice.record.ID)
	narrowed := env.do(t, server, "POST", "/v1/board/messages", "alice", map[string]any{"channel": "work/secret", "text": "should not land"})
	if narrowed.status != 403 {
		t.Fatalf("narrowed editor post = %d: %s", narrowed.status, narrowed.body)
	}
	// The parent channel remains writable for Alice.
	if parentPost := env.do(t, server, "POST", "/v1/board/messages", "alice", map[string]any{"channel": "work", "text": "parent still works"}); parentPost.status != 201 {
		t.Fatalf("parent post = %d: %s", parentPost.status, parentPost.body)
	}
}

// TestBoardCrossTenantAuthorizationIsNotReachable calls the authorizer with
// another tenant directly: the edge principal's tenant is authoritative, so a
// forged board tenant string must be refused before any relation is read.
func TestBoardCrossTenantAuthorizationIsNotReachable(t *testing.T) {
	env := newTestEnv(t, nil)
	ctx := context.Background()
	principal, ok := env.principal("alice")
	if !ok {
		t.Fatal("alice principal missing")
	}
	ctx = withTestPrincipal(ctx, principal)
	authorizer, err := NewAuthorizer(env.bridge, nil, env.surf.board, nil)
	if err != nil {
		t.Fatalf("authorizer: %v", err)
	}
	actor, err := (&Identity{Store: env.st}).Actor(testRequest(ctx))
	if err != nil {
		t.Fatalf("actor: %v", err)
	}
	err = authorizer.Authorize(ctx, env.other.String(), actor, "anything", protocol.PermissionRead)
	if err == nil {
		t.Fatal("cross-tenant authorization was allowed")
	}
	var boardErr *protocol.Error
	if !asProtocolError(err, &boardErr) || (boardErr.Status != 401 && boardErr.Status != 404) {
		t.Fatalf("cross-tenant error = %v", err)
	}
}

// TestBoardRLSFailClosed proves the board's own policies (keel.tenant_id, not
// keel's app.tenant_id) are ENABLE+FORCE on every board table and that a
// restricted role sees zero rows without a tenant setting.
func TestBoardRLSFailClosed(t *testing.T) {
	env := newTestEnv(t, nil)
	ctx := context.Background()
	server := env.server()
	if resp := env.do(t, server, "POST", "/v1/board/channels", "alice", map[string]any{"path": "work"}); resp.status != 201 {
		t.Fatalf("seed channel: %d", resp.status)
	}
	if resp := env.do(t, server, "POST", "/v1/board/messages", "alice", map[string]any{"channel": "work", "text": "tenant A text"}); resp.status != 201 {
		t.Fatalf("seed message: %d", resp.status)
	}

	assertBoardRLS(t, ctx, env.dsn, env.schema, env.tenant, env.other)
}

// TestBoardMetricsAuditAndNoSecrets covers 10.7: board families are exposed,
// a denial lands in keel's auth_events, and neither metrics nor audit carry
// message text or a claim token.
func TestBoardMetricsAuditAndNoSecrets(t *testing.T) {
	env := newTestEnv(t, nil)
	ctx := context.Background()
	server := env.server()

	if resp := env.do(t, server, "POST", "/v1/board/channels", "alice", map[string]any{"path": "work"}); resp.status != 201 {
		t.Fatalf("create channel: %d", resp.status)
	}
	env.relation(env.tenant, "work", keelstore.RelationViewer, env.carol.record.ID)
	secretText := "metrics must never carry MESSAGE-BODY-7f3a"
	if resp := env.do(t, server, "POST", "/v1/board/messages", "alice", map[string]any{"channel": "work", "text": secretText}); resp.status != 201 {
		t.Fatalf("post: %d", resp.status)
	}

	task := env.do(t, server, "POST", "/v1/board/tasks", "bot", map[string]any{"channel": "work", "text": "claim me"})
	var created struct {
		Task protocol.MessageView `json:"task"`
	}
	env.decode(t, task, &created)
	claim := env.do(t, server, "POST", "/v1/board/tasks/"+created.Task.ID+"/claim", "bot", map[string]any{})
	var claimed struct {
		Claim protocol.TaskClaim `json:"claim"`
	}
	env.decode(t, claim, &claimed)
	if claimed.Claim.Token == "" {
		t.Fatal("claim returned no token")
	}
	// A viewer's write is denied and audited.
	if resp := env.do(t, server, "POST", "/v1/board/messages", "carol", map[string]any{"channel": "work", "text": "denied body"}); resp.status != 403 {
		t.Fatalf("carol write = %d", resp.status)
	}

	exposition := &strings.Builder{}
	if err := env.surf.Expo(nil).WriteText(ctx, exposition); err != nil {
		t.Fatalf("metrics: %v", err)
	}
	metrics := exposition.String()
	if !strings.Contains(metrics, "board_events_total") || !strings.Contains(metrics, "board_tasks_live_claims") {
		t.Fatalf("board metrics missing:\n%s", metrics)
	}
	for _, forbidden := range []string{secretText, claimed.Claim.Token, env.tenant.String(), env.alice.record.ID.String()} {
		if forbidden != "" && strings.Contains(metrics, forbidden) {
			t.Fatalf("metrics leaked %q:\n%s", forbidden, metrics)
		}
	}

	events, err := env.st.ListAuthEvents(ctx, env.tenant, keelstore.AuthEventFilter{Kind: keelstore.AuthEventPermissionDenied}, 50)
	if err != nil {
		t.Fatalf("list auth events: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("a denial wrote no permission.denied auth event")
	}
	for _, event := range events {
		raw, _ := json.Marshal(event.Detail)
		text := string(raw)
		for _, forbidden := range []string{secretText, claimed.Claim.Token, "denied body"} {
			if forbidden != "" && strings.Contains(text, forbidden) {
				t.Fatalf("auth event leaked %q: %s", forbidden, text)
			}
		}
		if _, ok := event.Detail["key"]; !ok {
			t.Fatalf("denial detail names no key: %s", text)
		}
	}

	// The board's operational audit persists through board_audit (no bodies,
	// no tokens) and is readable through the admin route.
	appended, err := env.surf.AuditStore().Append(ctx, boardAuditEvent(env.tenant.String()))
	if err != nil {
		t.Fatalf("append board audit: %v", err)
	}
	if appended.ID == 0 {
		t.Fatal("board audit append returned no id")
	}
	adminAudit := env.do(t, server, "GET", "/v1/board/admin/audit?limit=10", "alice", nil)
	if adminAudit.status != 200 {
		t.Fatalf("admin audit = %d: %s", adminAudit.status, adminAudit.body)
	}
	if strings.Contains(string(adminAudit.body), secretText) || strings.Contains(string(adminAudit.body), claimed.Claim.Token) {
		t.Fatalf("admin audit leaked content: %s", adminAudit.body)
	}
}

// TestBoardDisabled is the feature-flag seam: a disabled deployment answers
// 404 board_disabled for the API and the webhook namespace.
func TestBoardDisabled(t *testing.T) {
	handler := DisabledHandler()
	server := newTestServer(handler)
	resp := envDo(t, server, "GET", "/v1/board/channels")
	if resp.status != 404 || !strings.Contains(string(resp.body), "board_disabled") {
		t.Fatalf("disabled board = %d: %s", resp.status, resp.body)
	}
}

// TestBoardChannelOwnerReconciler repairs a channel whose request-path grant
// was lost (simulated by deleting the relation after creation).
func TestBoardChannelOwnerReconciler(t *testing.T) {
	env := newTestEnv(t, nil)
	ctx := context.Background()
	server := env.server()
	if resp := env.do(t, server, "POST", "/v1/board/channels", "alice", map[string]any{"path": "work"}); resp.status != 201 {
		t.Fatalf("create channel: %d", resp.status)
	}
	if err := env.st.DeleteResourceRelation(ctx, env.tenant, BoardChannelResourceType, "work", keelstore.RelationOwner, keelstore.RelationSubjectAccount, env.alice.record.ID.String()); err != nil {
		t.Fatalf("delete owner relation: %v", err)
	}
	env.surf.owners.scan(ctx)
	has, err := env.st.HasResourceRelation(ctx, env.tenant, BoardChannelResourceType, "work", keelstore.RelationOwner, keelstore.RelationSubjectAccount, env.alice.record.ID.String())
	if err != nil {
		t.Fatalf("read relation: %v", err)
	}
	if !has {
		t.Fatal("reconciler did not restore the owner relation")
	}
}

// TestBotProfileTokenIsTheBoardProfile protects the documented interface gap:
// the board's profile grammar cannot carry a raw keel Bot handle, so the
// adapter derives a stable token and uses the same token in owner+profile
// assignment.
func TestBotProfileTokenIsTheBoardProfile(t *testing.T) {
	env := newTestEnv(t, nil)
	want := AccountActorID(env.alice.record.ID) + "+" + BotProfileToken(env.bot.ID)
	if !protocol.ValidProfile(BotProfileToken(env.bot.ID)) {
		t.Fatalf("%q is not a valid board profile", BotProfileToken(env.bot.ID))
	}
	if BotActorID(env.alice.record.ID, env.bot.ID) != want {
		t.Fatalf("bot actor = %q, want %q", BotActorID(env.alice.record.ID, env.bot.ID), want)
	}
}

// TestBoardPermissionKeysAreRegistered keeps the keel access catalog and the
// board mapping in sync.
func TestBoardPermissionKeysAreRegistered(t *testing.T) {
	for _, key := range []string{access.KeyBoardRead, access.KeyBoardWrite, access.KeyBoardManage} {
		if err := access.ValidateKey(key); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
	}
}

// asProtocolError unwraps the board's protocol error.
func asProtocolError(err error, target **protocol.Error) bool {
	for err != nil {
		if typed, ok := err.(*protocol.Error); ok {
			*target = typed
			return true
		}
		type unwrapper interface{ Unwrap() error }
		next, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = next.Unwrap()
	}
	return false
}
