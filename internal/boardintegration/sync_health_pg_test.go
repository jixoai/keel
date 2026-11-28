package boardintegration

import (
	"context"
	"errors"
	"net/http"
	"testing"

	boardsync "github.com/jixoai/keel-board/sync"
	syncstore "github.com/jixoai/keel-board/syncstore"
	"github.com/jixoai/keel/internal/store"
)

// TestSyncHealthFromBoardStore proves the mount exposes board v0.1.2's measured
// targets, lag and 24h failure count instead of a client-side guess.
func TestSyncHealthFromBoardStore(t *testing.T) {
	env := newTestEnvBefore(t, nil, func(env *testEnv) {
		_, err := env.st.CreateBoardSyncEntry(context.Background(), store.BoardSyncEntryRecord{
			TenantID: env.tenant, Key: "slack", Platform: "slack",
			SecretRef: map[string]string{"signingSecret": "MISSING_SIGNING"}, Enabled: true,
		})
		if err != nil {
			env.t.Fatalf("create sync entry: %v", err)
		}
	})
	server := env.server()
	ctx := context.Background()
	tenant := env.tenant.String()

	if _, err := env.surf.syncdb.UpsertThread(ctx, syncstore.Thread{
		Tenant: tenant, BoardThread: 812, Key: "slack", Platform: "slack",
		Channel: "C0123", Thread: "C0123:1727.9", Kind: boardsync.TargetThread,
	}); err != nil {
		t.Fatalf("upsert thread: %v", err)
	}
	if _, err := env.surf.syncdb.PushInbox(ctx, tenant, "slack", []boardsync.Record{{
		Dedupe: "d1",
		Event: boardsync.Event{
			ID: "m1", Key: "slack", Platform: "slack",
			Kind: boardsync.KindMessage, Type: boardsync.TypeMessage,
			Channel: "C0123", Thread: "C0123:1727.9", Message: "m1",
			Text: "hello", Author: boardsync.Author{ID: "slack:user:U1", Handle: "ada", Name: "Ada"},
		},
	}}); err != nil {
		t.Fatalf("push inbox: %v", err)
	}
	target := boardsync.ResolvedTarget{
		Key: "slack", Platform: "slack", Channel: "C0123",
		Thread: "C0123:1727.9", Kind: boardsync.TargetThread,
	}
	if _, err := env.surf.syncdb.EnqueueOutbox(ctx, tenant, "slack", boardsync.BoardEvent{
		ID: 1, Type: boardsync.BoardMessagePosted, Thread: 812,
		Message: &boardsync.Message{ID: 812},
	}, boardsync.Message{ID: 812, Thread: 812, Channel: "support"},
		[]boardsync.ResolvedTarget{target}); err != nil {
		t.Fatalf("enqueue outbox: %v", err)
	}

	type lagView struct {
		Inbox *struct {
			Outstanding int   `json:"outstanding"`
			AgeSeconds  int64 `json:"ageSeconds"`
		} `json:"inbox"`
		Outbox *struct {
			Outstanding int   `json:"outstanding"`
			AgeSeconds  int64 `json:"ageSeconds"`
		} `json:"outbox"`
	}
	type targetView struct {
		Target      string `json:"target"`
		Root        string `json:"root"`
		Status      string `json:"status"`
		Pending     int    `json:"pending"`
		Dead        int    `json:"dead"`
		LastError   string `json:"lastError"`
		Undelivered int    `json:"undelivered"`
	}
	type keyView struct {
		Key                     string       `json:"key"`
		Lag                     lagView      `json:"lag"`
		Targets                 []targetView `json:"targets"`
		Failures24h             int          `json:"failures24h"`
		OldestPendingAgeSeconds *int64       `json:"oldestPendingAgeSeconds"`
	}

	readStatus := func() (keyView, lagView) {
		status := env.do(t, server, http.MethodGet, "/v1/board/sync/status", "alice", nil)
		if status.status != http.StatusOK {
			t.Fatalf("sync status = %d %s", status.status, status.body)
		}
		var body struct {
			Keys []keyView `json:"keys"`
			Lag  lagView   `json:"lag"`
		}
		env.decode(t, status, &body)
		if len(body.Keys) != 1 || body.Keys[0].Key != "slack" {
			t.Fatalf("status keys = %s", status.body)
		}
		return body.Keys[0], body.Lag
	}

	key, rootLag := readStatus()
	if key.Lag.Inbox == nil || key.Lag.Inbox.Outstanding != 1 || key.Lag.Inbox.AgeSeconds < 0 {
		t.Fatalf("inbox lag = %+v", key.Lag.Inbox)
	}
	if key.Lag.Outbox == nil || key.Lag.Outbox.Outstanding != 1 || key.Lag.Outbox.AgeSeconds < 0 {
		t.Fatalf("outbox lag = %+v", key.Lag.Outbox)
	}
	if key.OldestPendingAgeSeconds == nil || *key.OldestPendingAgeSeconds < 0 {
		t.Fatalf("oldestPendingAgeSeconds = %v", key.OldestPendingAgeSeconds)
	}
	if rootLag.Inbox == nil || rootLag.Outbox == nil {
		t.Fatalf("tenant lag = %+v", rootLag)
	}
	if len(key.Targets) != 1 {
		t.Fatalf("targets = %+v", key.Targets)
	}
	targetHealth := key.Targets[0]
	if targetHealth.Target != "slack:C0123:1727.9" || targetHealth.Root != "812" ||
		targetHealth.Status != "pending" || targetHealth.Undelivered != 1 || targetHealth.Pending != 1 {
		t.Fatalf("target = %+v", targetHealth)
	}
	if key.Failures24h != 0 {
		t.Fatalf("failures24h = %d, want 0", key.Failures24h)
	}

	// The by-key enumeration returns the same target.
	targets := env.do(t, server, http.MethodGet, "/v1/board/sync/entries/slack/targets", "alice", nil)
	if targets.status != http.StatusOK {
		t.Fatalf("targets = %d %s", targets.status, targets.body)
	}
	var targetsBody struct {
		Targets []targetView `json:"targets"`
	}
	env.decode(t, targets, &targetsBody)
	if len(targetsBody.Targets) != 1 || targetsBody.Targets[0].Root != "812" {
		t.Fatalf("targets = %s", targets.body)
	}

	// A permanent delivery failure is the 24h failure the page reports.
	jobs, err := env.surf.syncdb.LeaseOutbox(ctx, tenant, []boardsync.Key{"slack"}, 10, "test")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("lease outbox = %+v, %v", jobs, err)
	}
	if ack, err := env.surf.syncdb.AckOutbox(ctx, tenant, jobs[0].ID, jobs[0].Lease,
		boardsync.Permanent(errors.New("rate_limited")), nil); err != nil || !ack.Dead {
		t.Fatalf("dead letter = %+v, %v", ack, err)
	}
	key, _ = readStatus()
	if key.Failures24h != 1 {
		t.Fatalf("failures24h = %d, want 1", key.Failures24h)
	}
	if len(key.Targets) != 1 || key.Targets[0].Status != "failed" || key.Targets[0].Dead != 1 ||
		key.Targets[0].LastError == "" {
		t.Fatalf("failed target = %+v", key.Targets)
	}

	// Other tenants and callers without board.manage never see the health.
	if other := env.do(t, server, http.MethodGet, "/v1/board/sync/status", "dave", nil); other.status != http.StatusOK {
		t.Fatalf("other tenant status = %d %s", other.status, other.body)
	}
	bob := env.do(t, server, http.MethodGet, "/v1/board/sync/status", "bob", nil)
	if bob.status != http.StatusForbidden {
		t.Fatalf("status without manage = %d %s", bob.status, bob.body)
	}
}
