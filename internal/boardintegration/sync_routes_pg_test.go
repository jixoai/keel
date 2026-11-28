package boardintegration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/access"
	keelstore "github.com/jixoai/keel/internal/store"
)

// TestSyncEntryRouteRequiresProvisionedChannel is the P0-1 regression: a route
// target that does not exist, or exists without any board_channel relation, is
// refused with an actionable 400 instead of being stored and importing into an
// invisible channel. After the channel is created through the API (creator
// becomes owner) the same entry is accepted.
func TestSyncEntryRouteRequiresProvisionedChannel(t *testing.T) {
	env := newTestEnv(t, nil)
	env.grant(env.tenant, env.alice.record.ID, access.KeySecretWrite)
	server := env.server()
	ctx := context.Background()

	body := map[string]any{
		"key": "slack", "platform": "slack",
		"secretRef":     map[string]string{"signingSecret": "SLACK_SIGNING", "botToken": "SLACK_BOT"},
		"settings":      map[string]any{"botUserId": "U_APP", "botId": "B_APP", "apiBaseUrl": "http://127.0.0.1:1/api"},
		"routes":        map[string]string{"C0SUPPORT": "fresh"},
		"startChannels": []string{"C0SUPPORT"},
		"enabled":       true,
	}
	missing := env.do(t, server, http.MethodPost, "/v1/board/sync/entries", "alice", body)
	if missing.status != http.StatusBadRequest {
		t.Fatalf("entry for a missing channel = %d %s", missing.status, missing.body)
	}
	if !strings.Contains(string(missing.body), "does not exist") ||
		!strings.Contains(string(missing.body), "POST /v1/board/channels") {
		t.Fatalf("missing route error is not actionable: %s", missing.body)
	}
	if _, found, err := env.st.GetBoardSyncEntry(ctx, env.tenant, "slack"); err != nil || found {
		t.Fatalf("refused entry was stored: found=%v err=%v", found, err)
	}

	if resp := env.ensureChannel(t, server, "fresh"); resp.status != http.StatusCreated {
		t.Fatalf("fresh channel = %d %s", resp.status, resp.body)
	}
	created := env.do(t, server, http.MethodPost, "/v1/board/sync/entries", "alice", body)
	if created.status != http.StatusCreated {
		t.Fatalf("entry after provisioning = %d %s", created.status, created.body)
	}
	// A created entry is pending adoption: the board sync row is disabled until
	// a process restart loads it.
	entry, found, err := env.surf.SyncStore().Entry(ctx, env.tenant.String(), "slack")
	if err != nil || !found || entry.Enabled {
		t.Fatalf("pending board sync entry = %+v/%v/%v", entry, found, err)
	}
	// PATCH to another missing channel is refused too.
	badPatch := env.do(t, server, http.MethodPatch, "/v1/board/sync/entries/slack", "alice", map[string]any{
		"routes": map[string]string{"C0SUPPORT": "missing-again"},
	})
	if badPatch.status != http.StatusBadRequest {
		t.Fatalf("PATCH to a missing channel = %d %s", badPatch.status, badPatch.body)
	}
	// A plain enable of the pending entry is safe but inert (restart required).
	enable := env.do(t, server, http.MethodPatch, "/v1/board/sync/entries/slack", "alice", map[string]any{"enabled": true})
	var result SyncEntryResult
	env.decode(t, enable, &result)
	if !result.RestartRequired {
		t.Fatalf("enable of a pending entry = %+v", result)
	}

	// Disabling must work even when the route just lost its relation; enabling
	// it again is the check that fails closed.
	if err := env.st.DeleteResourceRelation(ctx, env.tenant, BoardChannelResourceType, "fresh",
		keelstore.RelationOwner, keelstore.RelationSubjectAccount, env.alice.record.ID.String()); err != nil {
		t.Fatalf("delete route relation: %v", err)
	}
	if resp := env.do(t, server, http.MethodPatch, "/v1/board/sync/entries/slack", "alice", map[string]any{"enabled": false}); resp.status != http.StatusOK {
		t.Fatalf("disable with an unhealthy route = %d %s", resp.status, resp.body)
	}
	entry, found, err = env.surf.SyncStore().Entry(ctx, env.tenant.String(), "slack")
	if err != nil || !found || entry.Enabled {
		t.Fatalf("disabled entry row = %+v/%v/%v", entry, found, err)
	}
	if resp := env.do(t, server, http.MethodPatch, "/v1/board/sync/entries/slack", "alice", map[string]any{"enabled": true}); resp.status != http.StatusBadRequest {
		t.Fatalf("enable with an unhealthy route = %d %s", resp.status, resp.body)
	}
}

// TestSyncEntryRouteInheritsAncestorRelation covers board's most-specific-wins
// inheritance on the write path: a route target with no exact relation is
// provisioned when an ancestor carries one. A completely relation-free target
// is still refused.
func TestSyncEntryRouteInheritsAncestorRelation(t *testing.T) {
	env := newTestEnv(t, nil)
	env.grant(env.tenant, env.alice.record.ID, access.KeySecretWrite)
	server := env.server()
	ctx := context.Background()

	for _, channel := range []string{"work", "work/production"} {
		if resp := env.ensureChannel(t, server, channel); resp.status != http.StatusCreated {
			t.Fatalf("create %s: %d %s", channel, resp.status, resp.body)
		}
	}
	// Remove the exact relation, leaving only the inherited relation on work.
	if err := env.st.DeleteResourceRelation(ctx, env.tenant, BoardChannelResourceType, "work/production",
		keelstore.RelationOwner, keelstore.RelationSubjectAccount, env.alice.record.ID.String()); err != nil {
		t.Fatalf("delete exact relation: %v", err)
	}
	body := map[string]any{
		"key": "slack", "platform": "slack", "enabled": true,
		"secretRef": map[string]string{"signingSecret": "SLACK_SIGNING", "botToken": "SLACK_BOT"},
		"settings":  map[string]any{"botUserId": "U_APP", "apiBaseUrl": "http://127.0.0.1:1/api"},
		"routes":    map[string]string{"C0WORK": "work/production"},
	}
	if resp := env.do(t, server, http.MethodPost, "/v1/board/sync/entries", "alice", body); resp.status != http.StatusCreated {
		t.Fatalf("entry with an inherited relation = %d %s", resp.status, resp.body)
	}

	// A target whose whole path is relation-free remains refused.
	if resp := env.ensureChannel(t, server, "isolated"); resp.status != http.StatusCreated {
		t.Fatalf("create isolated: %d %s", resp.status, resp.body)
	}
	if err := env.st.DeleteResourceRelation(ctx, env.tenant, BoardChannelResourceType, "isolated",
		keelstore.RelationOwner, keelstore.RelationSubjectAccount, env.alice.record.ID.String()); err != nil {
		t.Fatalf("delete isolated relation: %v", err)
	}
	body["key"] = "isolated"
	body["routes"] = map[string]string{"C0ISOLATED": "isolated"}
	if resp := env.do(t, server, http.MethodPost, "/v1/board/sync/entries", "alice", body); resp.status != http.StatusBadRequest {
		t.Fatalf("relation-free target = %d %s", resp.status, resp.body)
	}
}

// TestSyncEntryDisabledStagingSkipsRouteCheck lets an operator stage a disabled
// entry before its channel exists; enabling it is what requires provisioning.
func TestSyncEntryDisabledStagingSkipsRouteCheck(t *testing.T) {
	env := newTestEnv(t, nil)
	env.grant(env.tenant, env.alice.record.ID, access.KeySecretWrite)
	server := env.server()
	body := map[string]any{
		"key": "staged", "platform": "slack", "enabled": false,
		"secretRef": map[string]string{"signingSecret": "SLACK_SIGNING", "botToken": "SLACK_BOT"},
		"settings":  map[string]any{"botUserId": "U_APP", "apiBaseUrl": "http://127.0.0.1:1/api"},
		"routes":    map[string]string{"C0STAGED": "not-created-yet"},
	}
	if resp := env.do(t, server, http.MethodPost, "/v1/board/sync/entries", "alice", body); resp.status != http.StatusCreated {
		t.Fatalf("disabled staging = %d %s", resp.status, resp.body)
	}
	if resp := env.do(t, server, http.MethodPatch, "/v1/board/sync/entries/staged", "alice", map[string]any{"enabled": true}); resp.status != http.StatusBadRequest {
		t.Fatalf("enabling a missing route = %d %s", resp.status, resp.body)
	}
}

// TestSyncBootDisablesUnprovisionedRouteEntry covers the historical-entry half
// of P0-1: an entry stored before the ACL existed stays disabled at boot, names
// the issue in its status, never starts a worker, and starts importing only
// after the channel is provisioned and the process restarts.
func TestSyncBootDisablesUnprovisionedRouteEntry(t *testing.T) {
	env := newTestEnvBefore(t, nil, func(e *testEnv) {
		e.grant(e.tenant, e.alice.record.ID, access.KeySecretWrite)
		ctx := context.Background()
		if _, err := e.st.PutTenantSecret(ctx, e.tenant, "SLACK_SIGNING", []byte(testSlackSigning), e.box); err != nil {
			e.t.Fatalf("put signing secret: %v", err)
		}
		if _, err := e.st.PutTenantSecret(ctx, e.tenant, "SLACK_BOT", []byte(testSlackToken), e.box); err != nil {
			e.t.Fatalf("put bot secret: %v", err)
		}
		record := keelstore.BoardSyncEntryRecord{
			TenantID: e.tenant, Key: "slack", Platform: "slack", Enabled: true,
			SecretRef:     map[string]string{"signingSecret": "SLACK_SIGNING", "botToken": "SLACK_BOT"},
			Settings:      mustRawJSON(map[string]any{"botUserId": "U_APP", "botId": "B_APP", "apiBaseUrl": "http://127.0.0.1:1/api"}),
			Routes:        map[string]string{"C0SUPPORT": "legacy"},
			StartChannels: []string{"C0SUPPORT"},
		}
		if _, err := e.st.CreateBoardSyncEntry(ctx, record); err != nil {
			e.t.Fatalf("create sync entry: %v", err)
		}
	})
	server := env.server()
	ctx := context.Background()

	entry, found, err := env.surf.SyncStore().Entry(ctx, env.tenant.String(), "slack")
	if err != nil || !found || entry.Enabled {
		t.Fatalf("boot-disabled board sync entry = %+v/%v/%v", entry, found, err)
	}
	status := env.do(t, server, http.MethodGet, "/v1/board/sync/credentials", "alice", nil)
	if status.status != http.StatusOK ||
		!strings.Contains(string(status.body), `"routesReady":false`) ||
		!strings.Contains(string(status.body), "legacy") {
		t.Fatalf("unprovisioned route status = %d %s", status.status, status.body)
	}
	headBefore, err := env.surf.Board().Head(env.tenant.String())
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	env.pushSlackInbox(t, "must not import before provisioning", "1700002000.000001")
	time.Sleep(300 * time.Millisecond)
	headAfter, err := env.surf.Board().Head(env.tenant.String())
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if headAfter != headBefore {
		t.Fatalf("a boot-disabled route entry wrote the board: head %d -> %d", headBefore, headAfter)
	}

	// Provisioning the channel fixes the status; the loaded provider is still
	// the old (absent) one, so adoption needs a restart.
	env.ensureChannel(t, server, "legacy")
	status = env.do(t, server, http.MethodGet, "/v1/board/sync/credentials", "alice", nil)
	if !strings.Contains(string(status.body), `"routesReady":true`) {
		t.Fatalf("provisioned route status = %d %s", status.status, status.body)
	}
	env.surf.Close()
	next, err := New(ctx, env.options)
	if err != nil {
		t.Fatalf("restart surface: %v", err)
	}
	env.surf = next
	env.pushSlackInbox(t, "imports after provisioning and restart", "1700002001.000001")
	if _, err := env.waitSyncMessage(t, server, "legacy", "imports after provisioning and restart"); err != nil {
		t.Fatalf("entry did not adopt after restart: %v", err)
	}
}

// waitChannelOwner waits for the createChannel handler's post-response owner
// grant. The handler writes the 201 before granting the owner relation, so a
// test that deletes the relation immediately can race the grant and see it
// recreated; every real client must tolerate that eventual grant too.
func (env *testEnv) waitChannelOwner(t *testing.T, channel string, owner kAccount) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var relations []keelstore.ResourceRelation
	var err error
	for {
		relations, err = env.st.ListResourceRelations(context.Background(), env.tenant, BoardChannelResourceType, channel, 0)
		if err == nil {
			for _, relation := range relations {
				if relation.Relation == keelstore.RelationOwner && relation.SubjectID == owner.record.ID.String() {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("channel %s owner %s was not recorded: relations=%+v err=%v", channel, owner.record.ID, relations, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestChannelCreateAdoptsOrphanForManager covers the explicit repair path: a
// tenant board.manage holder may adopt a channel that has zero relations, but
// never takes over a channel that already has any ACL.
func TestChannelCreateAdoptsOrphanForManager(t *testing.T) {
	env := newTestEnv(t, nil)
	server := env.server()
	ctx := context.Background()

	if resp := env.do(t, server, http.MethodPost, "/v1/board/channels", "alice", map[string]any{"path": "support"}); resp.status != http.StatusCreated {
		t.Fatalf("create channel: %d %s", resp.status, resp.body)
	}
	env.waitChannelOwner(t, "support", env.alice)
	deleteOwner := func() {
		t.Helper()
		if err := env.st.DeleteResourceRelation(ctx, env.tenant, BoardChannelResourceType, "support",
			keelstore.RelationOwner, keelstore.RelationSubjectAccount, env.alice.record.ID.String()); err != nil {
			t.Fatalf("delete owner relation: %v", err)
		}
	}
	deleteOwner()

	// A board.write-only caller cannot adopt: the create route refuses to probe
	// an existing channel it has no relation on.
	if resp := env.do(t, server, http.MethodPost, "/v1/board/channels", "bob", map[string]any{"path": "support"}); resp.status != http.StatusNotFound {
		t.Fatalf("bob adopted an orphan = %d %s", resp.status, resp.body)
	}

	// The manager holds only tenant board.manage. The create route succeeds
	// (200 created=false) and the adapter records it as owner.
	if resp := env.do(t, server, http.MethodPost, "/v1/board/channels", "manager", map[string]any{"path": "support"}); resp.status != http.StatusOK {
		t.Fatalf("manager adoption = %d %s", resp.status, resp.body)
	}
	relations, err := env.st.ListResourceRelations(ctx, env.tenant, BoardChannelResourceType, "support", 0)
	if err != nil {
		t.Fatalf("relations: %v", err)
	}
	if len(relations) != 1 || relations[0].SubjectID != env.manager.record.ID.String() || relations[0].Relation != keelstore.RelationOwner {
		t.Fatalf("adopted relations = %+v", relations)
	}
	if resp := env.do(t, server, http.MethodGet, "/v1/board/messages?channel=support", "manager", nil); resp.status != http.StatusOK {
		t.Fatalf("manager cannot read the adopted channel = %d %s", resp.status, resp.body)
	}

	// A channel that already has any relation is left untouched, even when the
	// caller is a manager.
	if err := env.st.DeleteResourceRelation(ctx, env.tenant, BoardChannelResourceType, "support",
		keelstore.RelationOwner, keelstore.RelationSubjectAccount, env.manager.record.ID.String()); err != nil {
		t.Fatalf("delete manager owner: %v", err)
	}
	env.relation(env.tenant, "support", keelstore.RelationViewer, env.carol.record.ID)
	if resp := env.do(t, server, http.MethodPost, "/v1/board/channels", "manager", map[string]any{"path": "support"}); resp.status != http.StatusOK {
		t.Fatalf("manager create on an owned channel = %d %s", resp.status, resp.body)
	}
	relations, err = env.st.ListResourceRelations(ctx, env.tenant, BoardChannelResourceType, "support", 0)
	if err != nil {
		t.Fatalf("relations: %v", err)
	}
	if len(relations) != 1 || relations[0].SubjectID != env.carol.record.ID.String() {
		t.Fatalf("an existing ACL was changed: %+v", relations)
	}
	if resp := env.do(t, server, http.MethodGet, "/v1/board/messages?channel=support", "manager", nil); resp.status != http.StatusNotFound {
		t.Fatalf("manager bypassed an existing ACL = %d %s", resp.status, resp.body)
	}

	// An inherited relation also blocks adoption: work/production is authorized
	// through work, so the manager cannot become its owner.
	for _, channel := range []string{"work", "work/production"} {
		if resp := env.ensureChannel(t, server, channel); resp.status != http.StatusCreated {
			t.Fatalf("create %s: %d %s", channel, resp.status, resp.body)
		}
	}
	if err := env.st.DeleteResourceRelation(ctx, env.tenant, BoardChannelResourceType, "work/production",
		keelstore.RelationOwner, keelstore.RelationSubjectAccount, env.alice.record.ID.String()); err != nil {
		t.Fatalf("delete production owner: %v", err)
	}
	if resp := env.do(t, server, http.MethodPost, "/v1/board/channels", "manager", map[string]any{"path": "work/production"}); resp.status != http.StatusOK {
		t.Fatalf("manager create on an inherited channel = %d %s", resp.status, resp.body)
	}
	if relations, err := env.st.ListResourceRelations(ctx, env.tenant, BoardChannelResourceType, "work/production", 0); err != nil || len(relations) != 0 {
		t.Fatalf("inherited channel relations = %+v/%v", relations, err)
	}
}

// TestChannelCreateDoesNotAdoptParentOfOwnedChild covers the last adoption
// boundary: a parent with a relation-bearing descendant is not an orphan, so a
// tenant manager cannot inherit onto the child's content. A fully relation-free
// leaf remains adoptable.
func TestChannelCreateDoesNotAdoptParentOfOwnedChild(t *testing.T) {
	env := newTestEnv(t, nil)
	server := env.server()
	ctx := context.Background()
	for _, channel := range []string{"work", "work/production", "work/leaf"} {
		if resp := env.ensureChannel(t, server, channel); resp.status != http.StatusCreated {
			t.Fatalf("create %s: %d %s", channel, resp.status, resp.body)
		}
	}
	deleteOwner := func(channel string) {
		t.Helper()
		if err := env.st.DeleteResourceRelation(ctx, env.tenant, BoardChannelResourceType, channel,
			keelstore.RelationOwner, keelstore.RelationSubjectAccount, env.alice.record.ID.String()); err != nil {
			t.Fatalf("delete owner %s: %v", channel, err)
		}
	}
	for _, channel := range []string{"work", "work/production", "work/leaf"} {
		deleteOwner(channel)
	}
	env.relation(env.tenant, "work/production", keelstore.RelationViewer, env.carol.record.ID)

	if resp := env.do(t, server, http.MethodPost, "/v1/board/channels", "manager", map[string]any{"path": "work"}); resp.status != http.StatusOK {
		t.Fatalf("manager create work = %d %s", resp.status, resp.body)
	}
	if relations, err := env.st.ListResourceRelations(ctx, env.tenant, BoardChannelResourceType, "work", 0); err != nil || len(relations) != 0 {
		t.Fatalf("work relations = %+v/%v (parent of an owned child was adopted)", relations, err)
	}

	// The leaf has no relation on itself, its ancestors or its descendants.
	if resp := env.do(t, server, http.MethodPost, "/v1/board/channels", "manager", map[string]any{"path": "work/leaf"}); resp.status != http.StatusOK {
		t.Fatalf("manager create leaf = %d %s", resp.status, resp.body)
	}
	relations, err := env.st.ListResourceRelations(ctx, env.tenant, BoardChannelResourceType, "work/leaf", 0)
	if err != nil {
		t.Fatalf("leaf relations: %v", err)
	}
	if len(relations) != 1 || relations[0].SubjectID != env.manager.record.ID.String() || relations[0].Relation != keelstore.RelationOwner {
		t.Fatalf("leaf relations = %+v", relations)
	}
}
