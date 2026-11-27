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

// TestBoardManageDoesNotBypassChannelRead proves board.manage is tenant
// administration, not a content grant: the manager can run the projection
// check but cannot read or write a channel without a relation.
func TestBoardManageDoesNotBypassChannelRead(t *testing.T) {
	env := newTestEnv(t, nil)
	server := env.server()
	if resp := env.do(t, server, "POST", "/v1/board/channels", "alice", map[string]any{"path": "work"}); resp.status != 201 {
		t.Fatalf("create work: %d: %s", resp.status, resp.body)
	}
	if resp := env.do(t, server, "POST", "/v1/board/messages", "alice", map[string]any{"channel": "work", "text": "manager cannot read"}); resp.status != 201 {
		t.Fatalf("seed message: %d", resp.status)
	}
	if resp := env.do(t, server, "GET", "/v1/board/messages?channel=work", "manager", nil); resp.status != 404 {
		t.Fatalf("manager read = %d: %s", resp.status, resp.body)
	}
	if resp := env.do(t, server, "POST", "/v1/board/messages", "manager", map[string]any{"channel": "work", "text": "nope"}); resp.status != 404 {
		t.Fatalf("manager write = %d: %s", resp.status, resp.body)
	}
	if resp := env.do(t, server, "POST", "/v1/board/admin/projection/check", "manager", map[string]any{}); resp.status != 200 {
		t.Fatalf("manager admin check = %d: %s", resp.status, resp.body)
	}
}

// TestBoardEditorCanClaim covers a successful claim by an editor relation (the
// ordinary worker path), not only the owner/manage path.
func TestBoardEditorCanClaim(t *testing.T) {
	env := newTestEnv(t, nil)
	server := env.server()
	if resp := env.do(t, server, "POST", "/v1/board/channels", "alice", map[string]any{"path": "work"}); resp.status != 201 {
		t.Fatalf("create work: %d", resp.status)
	}
	env.relation(env.tenant, "work", keelstore.RelationEditor, env.bob.record.ID)
	task := env.do(t, server, "POST", "/v1/board/tasks", "alice", map[string]any{"channel": "work", "text": "claim by editor"})
	var created struct {
		Task protocol.MessageView `json:"task"`
	}
	env.decode(t, task, &created)
	claim := env.do(t, server, "POST", "/v1/board/tasks/"+created.Task.ID+"/claim", "bob", map[string]any{})
	var claimed struct {
		Task  protocol.MessageView `json:"task"`
		Claim protocol.TaskClaim   `json:"claim"`
	}
	env.decode(t, claim, &claimed)
	if claimed.Claim.Token == "" || claimed.Task.Task == nil || claimed.Task.Task.State != "claimed" {
		t.Fatalf("editor claim = %+v", claimed)
	}
	done := env.do(t, server, "POST", "/v1/board/tasks/"+created.Task.ID+"/complete", "bob",
		map[string]any{"epoch": claimed.Claim.Epoch, "token": claimed.Claim.Token})
	var completed struct {
		Task protocol.MessageView `json:"task"`
	}
	env.decode(t, done, &completed)
	if completed.Task.Task == nil || completed.Task.Task.State != "completed" {
		t.Fatalf("editor complete = %+v", completed.Task.Task)
	}
}

// TestBoardAuthorizeAnywhereUpload covers the upload seam: board.post or
// board.sync may be authorized before a channel is named, while a read-only
// caller is refused.
func TestBoardAuthorizeAnywhereUpload(t *testing.T) {
	env := newTestEnv(t, nil)
	authorizer, err := NewAuthorizer(env.bridge, nil, env.surf.board, env.surf.logger)
	if err != nil {
		t.Fatalf("authorizer: %v", err)
	}
	for _, who := range []string{"alice", "bob", "manager"} {
		ctx, actor := env.contextAndActor(who)
		if err := authorizer.AuthorizeAnywhere(ctx, env.tenant.String(), actor, protocol.PermissionSync); err != nil {
			t.Fatalf("AuthorizeAnywhere(%s) = %v, want allow", who, err)
		}
	}
	ctx, actor := env.contextAndActor("carol")
	if err := authorizer.AuthorizeAnywhere(ctx, env.tenant.String(), actor, protocol.PermissionSync); err == nil {
		t.Fatal("AuthorizeAnywhere(carol) was allowed without board.write")
	}
}

// TestBoardAuthorizerRejectsForgedActorAndForeignTenant pins the two identity
// checks: a request cannot name another Account's Actor, and a board tenant
// string cannot disagree with the authenticated principal's tenant.
func TestBoardAuthorizerRejectsForgedActorAndForeignTenant(t *testing.T) {
	env := newTestEnv(t, nil)
	authorizer, err := NewAuthorizer(env.bridge, nil, env.surf.board, env.surf.logger)
	if err != nil {
		t.Fatalf("authorizer: %v", err)
	}
	ctx, aliceActor := env.contextAndActor("alice")
	forged := protocol.Actor{
		ID: AccountActorID(env.bob.record.ID), Principal: AccountActorID(env.bob.record.ID),
		Kind: protocol.ActorPerson,
	}
	if err := authorizer.Authorize(ctx, env.tenant.String(), forged, "work", protocol.PermissionRead); err == nil {
		t.Fatal("Authorize accepted an actor for another account")
	}
	if err := authorizer.ValidateRequest(ctx, env.other.String(), aliceActor); err == nil {
		t.Fatal("ValidateRequest accepted a foreign tenant")
	}
	if err := authorizer.ValidateRequest(ctx, env.tenant.String(), forged); err == nil {
		t.Fatal("ValidateRequest accepted an actor for another account")
	}
	profile := BotProfileToken(env.bot.ID)
	humanWithProfile := aliceActor
	humanWithProfile.Profile = &profile
	if err := authorizer.ValidateRequest(ctx, env.tenant.String(), humanWithProfile); err == nil {
		t.Fatal("ValidateRequest accepted a Bot profile on a human principal")
	}
}

// TestBoardNoRelationDenialIsAudited covers P1-1: a private channel's read and
// write are 404 to the caller, but each refusal is still permission.denied in
// keel's auth_events (no body, no token).
func TestBoardNoRelationDenialIsAudited(t *testing.T) {
	env := newTestEnv(t, nil)
	ctx := context.Background()
	server := env.server()
	if resp := env.do(t, server, "POST", "/v1/board/channels", "alice", map[string]any{"path": "private"}); resp.status != 201 {
		t.Fatalf("create private: %d: %s", resp.status, resp.body)
	}
	// Bob holds board.read/write but deliberately has no relation on private.
	if read := env.do(t, server, "GET", "/v1/board/messages?channel=private", "bob", nil); read.status != 404 {
		t.Fatalf("private read = %d: %s", read.status, read.body)
	}
	if write := env.do(t, server, "POST", "/v1/board/messages", "bob", map[string]any{"channel": "private", "text": "PRIVATE-BODY"}); write.status != 404 {
		t.Fatalf("private write = %d: %s", write.status, write.body)
	}

	events, err := env.st.ListAuthEvents(ctx, env.tenant, keelstore.AuthEventFilter{Kind: keelstore.AuthEventPermissionDenied}, 100)
	if err != nil {
		t.Fatalf("list auth events: %v", err)
	}
	found := 0
	keys := map[string]bool{}
	for _, event := range events {
		if event.Detail["resource_id"] != "private" {
			continue
		}
		if event.Detail["account_id"] != env.bob.record.ID.String() {
			continue
		}
		if event.Detail["reason"] != string(access.ReasonMissingRelation) {
			continue
		}
		found++
		if key, ok := event.Detail["key"].(string); ok {
			keys[key] = true
		}
		raw, _ := json.Marshal(event.Detail)
		if strings.Contains(string(raw), "PRIVATE-BODY") {
			t.Fatalf("denial audit leaked the message body: %s", raw)
		}
	}
	if found < 2 {
		t.Fatalf("no-relation denials audited = %d, want read and write", found)
	}
	if !keys[access.KeyBoardRead] || !keys[access.KeyBoardWrite] {
		t.Fatalf("denial audit keys = %v, want board.read and board.write", keys)
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

// TestBoardImplicitChannelOwner covers the implicit creation path: a child
// channel first seen by a root message under an editor parent is owned by that
// message's creator, and the reconciler restores the relation after a crash
// window. A top-level channel still needs the explicit create route.
func TestBoardImplicitChannelOwner(t *testing.T) {
	env := newTestEnv(t, nil)
	ctx := context.Background()
	server := env.server()
	if resp := env.do(t, server, "POST", "/v1/board/channels", "alice", map[string]any{"path": "work"}); resp.status != 201 {
		t.Fatalf("create work: %d: %s", resp.status, resp.body)
	}
	env.relation(env.tenant, "work", keelstore.RelationEditor, env.bob.record.ID)

	// A top-level channel cannot be created by a message: explicit /channels is
	// required, and no owner relation is written for the failed post.
	if top := env.do(t, server, "POST", "/v1/board/messages", "bob", map[string]any{"channel": "top", "text": "top"}); top.status != 404 {
		t.Fatalf("implicit top-level create = %d: %s", top.status, top.body)
	}
	if has, _ := env.st.HasResourceRelation(ctx, env.tenant, BoardChannelResourceType, "top", keelstore.RelationOwner, keelstore.RelationSubjectAccount, env.bob.record.ID.String()); has {
		t.Fatal("a rejected top-level post wrote an owner relation")
	}

	// A child under work inherits Bob's editor relation; the first root makes
	// Bob the child's owner.
	child := env.do(t, server, "POST", "/v1/board/messages", "bob", map[string]any{"channel": "work/child", "text": "child root"})
	if child.status != 201 {
		t.Fatalf("implicit child create = %d: %s", child.status, child.body)
	}
	owned := func() bool {
		has, err := env.st.HasResourceRelation(ctx, env.tenant, BoardChannelResourceType, "work/child", keelstore.RelationOwner, keelstore.RelationSubjectAccount, env.bob.record.ID.String())
		return err == nil && has
	}
	if !owned() {
		t.Fatal("the implicit child creator did not receive the owner relation")
	}

	// The same implicit path applies to a task's new thread.
	if task := env.do(t, server, "POST", "/v1/board/tasks", "bob", map[string]any{"channel": "work/taskchild", "text": "task child"}); task.status != 201 {
		t.Fatalf("implicit task child create = %d: %s", task.status, task.body)
	}
	if has, _ := env.st.HasResourceRelation(ctx, env.tenant, BoardChannelResourceType, "work/taskchild", keelstore.RelationOwner, keelstore.RelationSubjectAccount, env.bob.record.ID.String()); !has {
		t.Fatal("the implicit task channel creator did not receive the owner relation")
	}

	// Simulate the crash window: the request-path write is lost, and the
	// reconciler reconstructs it from the first message.posted root.
	if err := env.st.DeleteResourceRelation(ctx, env.tenant, BoardChannelResourceType, "work/child", keelstore.RelationOwner, keelstore.RelationSubjectAccount, env.bob.record.ID.String()); err != nil {
		t.Fatalf("delete implicit owner: %v", err)
	}
	env.surf.owners.scan(ctx)
	if !owned() {
		t.Fatal("the reconciler did not restore the implicit channel owner")
	}

	// A reply in the explicit work channel must not make Bob its owner.
	if resp := env.do(t, server, "POST", "/v1/board/messages", "alice", map[string]any{"channel": "work", "text": "root for reply"}); resp.status != 201 {
		t.Fatalf("work root: %d", resp.status)
	}
	var root struct {
		Message protocol.MessageView `json:"message"`
	}
	posted := env.do(t, server, "POST", "/v1/board/messages", "alice", map[string]any{"channel": "work", "text": "another root"})
	env.decode(t, posted, &root)
	if reply := env.do(t, server, "POST", "/v1/board/messages", "bob", map[string]any{"re": root.Message.ID, "text": "a reply"}); reply.status != 201 {
		t.Fatalf("reply = %d: %s", reply.status, reply.body)
	}
	env.surf.owners.scan(ctx)
	if has, _ := env.st.HasResourceRelation(ctx, env.tenant, BoardChannelResourceType, "work", keelstore.RelationOwner, keelstore.RelationSubjectAccount, env.bob.record.ID.String()); has {
		t.Fatal("a reply made its author a channel owner")
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
