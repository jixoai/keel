package boardintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	boardlog "github.com/jixoai/keel-board/log"
	"github.com/jixoai/keel-board/protocol"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/auth"
	keelstore "github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/superagentboard"
)

// The Phase 2 wiring tests: the mount layer must make the board an operable
// task surface without changing the board module. Every assertion below runs
// through the real PostgreSQL fixture and the real HTTP handler, with tenant A
// and tenant B accounts.

type taskViewJSON struct {
	State    string     `json:"state"`
	Owner    string     `json:"owner"`
	By       string     `json:"by"`
	For      *string    `json:"for"`
	Epoch    uint64     `json:"epoch"`
	Claim    *claimJSON `json:"claim"`
	Moves    []string   `json:"moves"`
	Archived bool       `json:"archived"`
	Mirror   *struct {
		Source   string `json:"source"`
		ReadOnly bool   `json:"readOnly"`
		Bridge   bool   `json:"bridge"`
	} `json:"mirror"`
	Origin *struct {
		Platform string `json:"platform"`
		Ref      string `json:"ref"`
	} `json:"origin"`
}

type claimJSON struct {
	Actor string `json:"actor"`
	Until string `json:"until"`
}

type messageJSON struct {
	ID      string            `json:"id"`
	Channel string            `json:"channel"`
	Text    string            `json:"text"`
	Labels  map[string]string `json:"labels"`
	NoSync  bool              `json:"noSync"`
	Task    *taskViewJSON     `json:"task"`
}

type meJSON struct {
	Account struct {
		ID          string `json:"id"`
		Username    string `json:"username"`
		DisplayName string `json:"displayName"`
	} `json:"account"`
	Bot *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"bot"`
	Actor     string `json:"actor"`
	ActorName string `json:"actorName"`
	Tenant    string `json:"tenant"`
	Can       struct {
		Board struct {
			Read   bool `json:"read"`
			Write  bool `json:"write"`
			Manage bool `json:"manage"`
		} `json:"board"`
	} `json:"can"`
	Channels map[string]struct {
		Read   bool `json:"read"`
		Write  bool `json:"write"`
		Manage bool `json:"manage"`
	} `json:"channels"`
}

func (e *testEnv) createChannel(t *testing.T, server *httptest.Server, who, channel string) {
	t.Helper()
	resp := e.do(t, server, http.MethodPost, "/v1/board/channels", who, map[string]any{"path": channel})
	if resp.status != http.StatusCreated && resp.status != http.StatusOK {
		t.Fatalf("create channel %s = %d: %s", channel, resp.status, resp.body)
	}
}

func (e *testEnv) createTask(t *testing.T, server *httptest.Server, who, channel, text string) string {
	t.Helper()
	resp := e.do(t, server, http.MethodPost, "/v1/board/tasks", who, map[string]any{"channel": channel, "text": text})
	if resp.status != http.StatusCreated {
		t.Fatalf("create task = %d: %s", resp.status, resp.body)
	}
	var created struct {
		Task messageJSON `json:"task"`
	}
	e.decode(t, resp, &created)
	if created.Task.Task == nil {
		t.Fatalf("created task has no projection: %s", resp.body)
	}
	return created.Task.ID
}

func (e *testEnv) taskThread(t *testing.T, server *httptest.Server, who, id string) messageJSON {
	t.Helper()
	resp := e.do(t, server, http.MethodGet, "/v1/board/messages/"+id, who, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("thread %s as %s = %d: %s", id, who, resp.status, resp.body)
	}
	var thread struct {
		Message messageJSON `json:"message"`
	}
	e.decode(t, resp, &thread)
	if thread.Message.Task == nil {
		t.Fatalf("thread %s has no task: %s", id, resp.body)
	}
	return thread.Message
}

func (e *testEnv) taskMoves(t *testing.T, server *httptest.Server, who, id string) map[string]bool {
	t.Helper()
	message := e.taskThread(t, server, who, id)
	return moveSet(t, message.Task)
}

func moveSet(t *testing.T, task *taskViewJSON) map[string]bool {
	t.Helper()
	if task == nil {
		t.Fatal("nil task")
	}
	if task.Moves == nil {
		t.Fatal("task has no moves field")
	}
	out := make(map[string]bool, len(task.Moves))
	for _, move := range task.Moves {
		out[move] = true
	}
	return out
}

func wantMoves(t *testing.T, got map[string]bool, want ...string) {
	t.Helper()
	for _, move := range want {
		if !got[move] {
			t.Fatalf("moves %v miss %q", keysOf(got), move)
		}
	}
}

func wantNoMoves(t *testing.T, got map[string]bool, absent ...string) {
	t.Helper()
	for _, move := range absent {
		if got[move] {
			t.Fatalf("moves %v should not contain %q", keysOf(got), move)
		}
	}
}

func keysOf(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	return out
}

func (e *testEnv) emitClaim(t *testing.T, taskID string, epoch uint64, until time.Time, actor string) {
	t.Helper()
	id := parseTaskRef(t, taskID)
	author := boardlog.Author{Actor: actor, Principal: actor}
	err := e.surf.Board().Update(context.Background(), e.tenant.String(), boardlog.UpdateOptions{}, func(tx *boardlog.Tx) error {
		tx.Emit(boardlog.EventTaskClaimed, &boardlog.TaskClaimedPayload{
			ID: id, Thread: boardlog.ThreadKey(id), Epoch: epoch,
			Until: protocol.FormatTime(until), Token: strings.Repeat("a", 64),
			By: author,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("emit claim: %v", err)
	}
}

func (e *testEnv) emitArchivedLabel(t *testing.T, taskID string) {
	t.Helper()
	id := parseTaskRef(t, taskID)
	author := boardlog.Author{Actor: AccountActorID(e.alice.record.ID), Principal: AccountActorID(e.alice.record.ID)}
	err := e.surf.Board().Update(context.Background(), e.tenant.String(), boardlog.UpdateOptions{}, func(tx *boardlog.Tx) error {
		tx.Emit(boardlog.EventLabelsChanged, &boardlog.LabelsChangedPayload{
			ID: id, Thread: boardlog.ThreadKey(id), Set: map[string]string{ArchivedLabel: "true"}, By: author,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("emit archived label: %v", err)
	}
}

// emitMirrorTask writes the root exactly as the superagent mirror does: a
// noSync message with source/ticket labels, then its task.created event.
func (e *testEnv) emitMirrorTask(t *testing.T, channel, text string) string {
	t.Helper()
	author := boardlog.Author{Actor: AccountActorID(e.alice.record.ID), Principal: AccountActorID(e.alice.record.ID)}
	var taskID uint64
	err := e.surf.Board().Update(context.Background(), e.tenant.String(), boardlog.UpdateOptions{}, func(tx *boardlog.Tx) error {
		taskID = tx.NextID(0)
		tx.Emit(boardlog.EventMessagePosted, &boardlog.MessagePostedPayload{
			ID: taskID, Channel: channel, Text: text,
			Labels: map[string]string{"source": "superagent", "ticket": "ticket-1"},
			NoSync: true, By: author,
		})
		tx.Emit(boardlog.EventTaskCreated, &boardlog.TaskCreatedPayload{
			ID: taskID, Thread: boardlog.ThreadKey(taskID), By: author,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("emit mirror task: %v", err)
	}
	return protocol.FormatRef(taskID)
}

func parseTaskRef(t *testing.T, ref string) uint64 {
	t.Helper()
	id, ok := protocol.ParseRef(ref)
	if !ok {
		t.Fatalf("bad task ref %q", ref)
	}
	return id
}

type fakeMirrorFact struct {
	mirrored bool
	source   string
	readOnly bool
	bridge   bool
	err      error
}

type fakeMirrorFacts struct {
	tasks        map[uint64]fakeMirrorFact
	canBridge    bool
	canBridgeErr error
}

func (f fakeMirrorFacts) TaskMirror(_ context.Context, _ uuid.UUID, taskID uint64) (bool, string, bool, bool, error) {
	fact, ok := f.tasks[taskID]
	if !ok {
		return false, "", true, false, nil
	}
	return fact.mirrored, fact.source, fact.readOnly, fact.bridge, fact.err
}

func (f fakeMirrorFacts) CanBridge(_ context.Context, _ uuid.UUID) (bool, error) {
	return f.canBridge, f.canBridgeErr
}

func TestBoardMeIdentityPermissionsAndTenancy(t *testing.T) {
	env := newTestEnv(t, nil)
	server := env.server()
	env.createChannel(t, server, "alice", "work")
	env.relation(env.tenant, "work", "editor", env.bob.record.ID)
	env.relation(env.tenant, "work", "viewer", env.carol.record.ID)

	// No principal: 401 with the board's error shape.
	unauth := envDo(t, server, http.MethodGet, "/v1/board/me")
	if unauth.status != http.StatusUnauthorized || !containsCode(unauth.body, "unauthorized") {
		t.Fatalf("unauthenticated /me = %d %s", unauth.status, unauth.body)
	}

	alice := decodeMe(t, env, server, "alice")
	if alice.Actor != AccountActorID(env.alice.record.ID) {
		t.Fatalf("alice actor = %q", alice.Actor)
	}
	if alice.Account.ID != env.alice.record.ID.String() || alice.Tenant != env.tenant.String() {
		t.Fatalf("alice account/tenant = %q/%q", alice.Account.ID, alice.Tenant)
	}
	if !alice.Can.Board.Read || !alice.Can.Board.Write || !alice.Can.Board.Manage {
		t.Fatalf("alice can = %+v", alice.Can.Board)
	}
	if can := alice.Channels["work"]; !can.Read || !can.Write || !can.Manage {
		t.Fatalf("alice work can = %+v", can)
	}

	bob := decodeMe(t, env, server, "bob")
	if !bob.Can.Board.Read || !bob.Can.Board.Write || bob.Can.Board.Manage {
		t.Fatalf("bob can = %+v", bob.Can.Board)
	}
	if can := bob.Channels["work"]; !can.Read || !can.Write || can.Manage {
		t.Fatalf("bob work can = %+v", can)
	}

	carol := decodeMe(t, env, server, "carol")
	if !carol.Can.Board.Read || carol.Can.Board.Write || carol.Can.Board.Manage {
		t.Fatalf("carol can = %+v", carol.Can.Board)
	}
	if can := carol.Channels["work"]; !can.Read || can.Write || can.Manage {
		t.Fatalf("carol work can = %+v", can)
	}

	manager := decodeMe(t, env, server, "manager")
	if !manager.Can.Board.Read || !manager.Can.Board.Write || !manager.Can.Board.Manage {
		t.Fatalf("manager can = %+v", manager.Can.Board)
	}
	if len(manager.Channels) != 0 {
		t.Fatalf("manager without a relation sees channels: %+v", manager.Channels)
	}

	bot := decodeMe(t, env, server, "bot")
	if bot.Bot == nil || bot.Bot.ID != env.bot.ID || bot.Bot.Name != env.bot.Name {
		t.Fatalf("bot = %+v, want %s/%s", bot.Bot, env.bot.ID, env.bot.Name)
	}
	if bot.Actor != BotActorID(env.alice.record.ID, env.bot.ID) || bot.Account.ID != env.alice.record.ID.String() {
		t.Fatalf("bot actor/account = %q/%q", bot.Actor, bot.Account.ID)
	}
	if can := bot.Channels["work"]; !can.Read || !can.Write || !can.Manage {
		t.Fatalf("bot work can = %+v", can)
	}

	// Tenant B sees its own identity and none of tenant A's channels.
	dave := decodeMe(t, env, server, "dave")
	if dave.Tenant != env.other.String() || dave.Account.ID != env.dave.record.ID.String() {
		t.Fatalf("dave tenant/account = %q/%q", dave.Tenant, dave.Account.ID)
	}
	if len(dave.Channels) != 0 {
		t.Fatalf("dave sees tenant A channels: %+v", dave.Channels)
	}
	taskID := env.createTask(t, server, "alice", "work", "cross-tenant")
	if resp := env.do(t, server, http.MethodGet, "/v1/board/messages/"+taskID, "dave", nil); resp.status != http.StatusNotFound {
		t.Fatalf("dave read tenant A task = %d %s", resp.status, resp.body)
	}
	var page struct {
		Tasks []struct {
			Message messageJSON `json:"message"`
		} `json:"tasks"`
	}
	resp := env.do(t, server, http.MethodGet, "/v1/board/tasks?state=all", "dave", nil)
	env.decode(t, resp, &page)
	if len(page.Tasks) != 0 {
		t.Fatalf("dave queue has tenant A tasks: %s", resp.body)
	}

	// A disabled board still answers 404 before identity: /me must not leak
	// that a caller would have been authenticated.
	if _, err := env.st.SetTenantConfig(context.Background(), env.tenant, json.RawMessage(`{"board":{"enabled":false}}`)); err != nil {
		t.Fatalf("disable board: %v", err)
	}
	disabled := env.do(t, server, http.MethodGet, "/v1/board/me", "alice", nil)
	if disabled.status != http.StatusNotFound || !containsCode(disabled.body, "board_disabled") {
		t.Fatalf("disabled /me = %d %s", disabled.status, disabled.body)
	}
}

func decodeMe(t *testing.T, env *testEnv, server *httptest.Server, who string) meJSON {
	t.Helper()
	resp := env.do(t, server, http.MethodGet, "/v1/board/me", who, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("/me as %s = %d: %s", who, resp.status, resp.body)
	}
	var me meJSON
	if err := json.Unmarshal(resp.body, &me); err != nil {
		t.Fatalf("decode /me: %v: %s", err, resp.body)
	}
	return me
}

func TestTaskMovesMatrixAcrossStatesAndRoles(t *testing.T) {
	env := newTestEnv(t, nil)
	env.relation(env.tenant, "work", "editor", env.bob.record.ID)
	env.relation(env.tenant, "work", "viewer", env.carol.record.ID)
	server := env.server()
	env.createChannel(t, server, "alice", "work")
	// The channel create grants owner to Alice; re-assert the other relations
	// because the create may race with the fixture's writes in another helper.
	env.relation(env.tenant, "work", "editor", env.bob.record.ID)
	env.relation(env.tenant, "work", "viewer", env.carol.record.ID)

	ready := env.createTask(t, server, "alice", "work", "ready task")
	alice := env.taskMoves(t, server, "alice", ready)
	wantMoves(t, alice, "claim", "block", "assign", "needs", "cancel", "reply")
	bob := env.taskMoves(t, server, "bob", ready)
	wantMoves(t, bob, "claim", "reply")
	wantNoMoves(t, bob, "assign", "needs", "cancel", "block")
	carol := env.taskMoves(t, server, "carol", ready)
	if len(carol) != 0 {
		t.Fatalf("viewer moves = %v, want none", keysOf(carol))
	}
	if resp := env.do(t, server, http.MethodGet, "/v1/board/messages/"+ready, "manager", nil); resp.status != http.StatusNotFound {
		t.Fatalf("manager read without relation = %d %s", resp.status, resp.body)
	}

	// Bob claims: only the holder is offered the token-bearing actions.
	claim := env.do(t, server, http.MethodPost, "/v1/board/tasks/"+ready+"/claim", "bob", map[string]any{"lease": 900})
	if claim.status != http.StatusOK {
		t.Fatalf("bob claim = %d %s", claim.status, claim.body)
	}
	var claimed struct {
		Claim struct {
			Epoch uint64 `json:"epoch"`
			Token string `json:"token"`
		} `json:"claim"`
	}
	env.decode(t, claim, &claimed)
	if claimed.Claim.Token == "" || claimed.Claim.Epoch == 0 {
		t.Fatalf("claim answer = %s", claim.body)
	}
	bob = env.taskMoves(t, server, "bob", ready)
	wantMoves(t, bob, "renew", "release", "complete", "fail", "block", "reply")
	wantNoMoves(t, bob, "claim", "assign", "needs", "cancel")
	alice = env.taskMoves(t, server, "alice", ready)
	wantMoves(t, alice, "assign", "cancel", "reply")
	wantNoMoves(t, alice, "renew", "release", "complete", "fail", "block", "claim")
	carol = env.taskMoves(t, server, "carol", ready)
	if len(carol) != 0 {
		t.Fatalf("viewer moves on claimed = %v", keysOf(carol))
	}

	// Owner cancel is allowed on a live claim; the task becomes terminal and
	// no state action remains.
	cancel := env.do(t, server, http.MethodPost, "/v1/board/tasks/"+ready+"/cancel", "alice", map[string]any{"reason": "duplicate"})
	if cancel.status != http.StatusOK {
		t.Fatalf("cancel = %d %s", cancel.status, cancel.body)
	}
	alice = env.taskMoves(t, server, "alice", ready)
	wantNoMoves(t, alice, "assign", "needs", "cancel", "claim", "block", "reopen")
	wantMoves(t, alice, "reply")
	bob = env.taskMoves(t, server, "bob", ready)
	wantMoves(t, bob, "reply")
	wantNoMoves(t, bob, "renew", "release", "complete", "fail")

	// Failed tasks: owner may reopen/cancel; an editor may only reply.
	failedTask := env.createTask(t, server, "alice", "work", "failed task")
	failClaim := env.do(t, server, http.MethodPost, "/v1/board/tasks/"+failedTask+"/claim", "bob", map[string]any{"lease": 900})
	env.decode(t, failClaim, &claimed)
	failed := env.do(t, server, http.MethodPost, "/v1/board/tasks/"+failedTask+"/fail", "bob",
		map[string]any{"epoch": claimed.Claim.Epoch, "token": claimed.Claim.Token, "reason": "broken"})
	if failed.status != http.StatusOK {
		t.Fatalf("fail = %d %s", failed.status, failed.body)
	}
	alice = env.taskMoves(t, server, "alice", failedTask)
	wantMoves(t, alice, "reopen", "cancel", "reply")
	bob = env.taskMoves(t, server, "bob", failedTask)
	wantMoves(t, bob, "reply")
	wantNoMoves(t, bob, "reopen", "cancel")

	// Blocked tasks: owner unblocks/assigns/needs/cancels; an editor replies.
	blockedTask := env.createTask(t, server, "alice", "work", "blocked task")
	block := env.do(t, server, http.MethodPost, "/v1/board/tasks/"+blockedTask+"/block", "alice",
		map[string]any{"reason": "waiting for a person", "kind": "needs_input"})
	if block.status != http.StatusOK {
		t.Fatalf("block = %d %s", block.status, block.body)
	}
	alice = env.taskMoves(t, server, "alice", blockedTask)
	wantMoves(t, alice, "unblock", "assign", "needs", "cancel", "reply")
	wantNoMoves(t, alice, "claim", "renew", "complete", "fail")
	bob = env.taskMoves(t, server, "bob", blockedTask)
	wantMoves(t, bob, "reply")

	// Waiting tasks: prerequisites are the owner's business, no claim.
	waitingTask := env.createTask(t, server, "alice", "work", "waiting task")
	needs := env.do(t, server, http.MethodPost, "/v1/board/tasks/"+waitingTask+"/needs", "alice",
		map[string]any{"needs": []string{failedTask}})
	if needs.status != http.StatusOK {
		t.Fatalf("needs = %d %s", needs.status, needs.body)
	}
	alice = env.taskMoves(t, server, "alice", waitingTask)
	wantMoves(t, alice, "assign", "needs", "cancel", "reply")
	wantNoMoves(t, alice, "claim", "block")

	// An expired claim is ready: the old holder's token actions disappear and
	// anyone the task is for may claim again.
	expired := env.createTask(t, server, "alice", "work", "expired claim")
	env.emitClaim(t, expired, 1, time.Now().Add(-time.Minute), AccountActorID(env.bob.record.ID))
	alice = env.taskMoves(t, server, "alice", expired)
	wantMoves(t, alice, "claim", "block", "assign", "needs", "cancel", "reply")
	wantNoMoves(t, alice, "renew", "release", "complete", "fail")
	bob = env.taskMoves(t, server, "bob", expired)
	wantMoves(t, bob, "claim", "reply")
	wantNoMoves(t, bob, "renew", "release", "complete", "fail")

	// An assigned task is claimable only by that actor. A task is assigned to
	// one of its owner's profiles, so Alice assigns her own Bot.
	assigned := env.createTask(t, server, "alice", "work", "assigned")
	botActor := AccountActorID(env.alice.record.ID) + "+" + env.botKey
	assign := env.do(t, server, http.MethodPost, "/v1/board/tasks/"+assigned+"/assign", "alice",
		map[string]any{"for": botActor})
	if assign.status != http.StatusOK {
		t.Fatalf("assign = %d %s", assign.status, assign.body)
	}
	alice = env.taskMoves(t, server, "alice", assigned)
	wantNoMoves(t, alice, "claim")
	bob = env.taskMoves(t, server, "bob", assigned)
	wantNoMoves(t, bob, "claim")
	bot := env.taskMoves(t, server, "bot", assigned)
	wantMoves(t, bot, "claim", "reply")
	carol = env.taskMoves(t, server, "carol", assigned)
	if len(carol) != 0 {
		t.Fatalf("viewer moves on assigned = %v", keysOf(carol))
	}

	// An archived task is history: no action at all, even for its owner, and
	// the queue's archive column is the only place it belongs.
	archived := env.createTask(t, server, "alice", "work", "archived")
	env.emitArchivedLabel(t, archived)
	message := env.taskThread(t, server, "alice", archived)
	if !message.Task.Archived {
		t.Fatalf("archived task has no archived fact: %s", message.Labels)
	}
	if len(message.Task.Moves) != 0 {
		t.Fatalf("archived moves = %v", message.Task.Moves)
	}
	if resp := env.do(t, server, http.MethodGet, "/v1/board/tasks?archived=1&state=all", "alice", nil); resp.status != http.StatusOK {
		t.Fatalf("archived queue = %d %s", resp.status, resp.body)
	}
}

func TestTaskMovesMirrorFacts(t *testing.T) {
	env := newTestEnv(t, nil)
	env.relation(env.tenant, "work", "owner", env.alice.record.ID)
	env.relation(env.tenant, "work", "editor", env.bob.record.ID)
	server := env.server()
	env.createChannel(t, server, "alice", "work")
	mirrorID := env.emitMirrorTask(t, "work", "mirrored ticket")
	id := parseTaskRef(t, mirrorID)

	// A mirror-looking task with no bridge fact is read-only, never a board
	// task with invented actions.
	message := env.taskThread(t, server, "alice", mirrorID)
	if message.Task.Mirror == nil || !message.Task.Mirror.ReadOnly || message.Task.Mirror.Source != "superagent" {
		t.Fatalf("mirror fact = %+v", message.Task.Mirror)
	}
	if message.Task.Origin == nil || message.Task.Origin.Ref != "ticket-1" {
		t.Fatalf("origin = %+v", message.Task.Origin)
	}
	if len(message.Task.Moves) != 0 {
		t.Fatalf("read-only mirror moves = %v", message.Task.Moves)
	}

	// The bridge is configured but this caller lacks tenant task.write: the
	// mirror config alone must not advertise actions the supervisor would
	// refuse.
	env.surf.SetMirrorFacts(fakeMirrorFacts{
		tasks: map[uint64]fakeMirrorFact{id: {mirrored: true, source: "superagent", readOnly: false, bridge: true}},
	})
	message = env.taskThread(t, server, "alice", mirrorID)
	if message.Task.Mirror == nil || !message.Task.Mirror.ReadOnly || message.Task.Mirror.Bridge {
		t.Fatalf("no task.write mirror fact = %+v", message.Task.Mirror)
	}
	if len(message.Task.Moves) != 0 {
		t.Fatalf("no task.write mirror moves = %v", message.Task.Moves)
	}

	// A failed CanBridge probe is also read-only, never an invented action.
	env.surf.SetMirrorFacts(fakeMirrorFacts{
		tasks:        map[uint64]fakeMirrorFact{id: {mirrored: true, source: "superagent", readOnly: false, bridge: true}},
		canBridgeErr: errors.New("task.write probe failed"),
	})
	message = env.taskThread(t, server, "alice", mirrorID)
	if message.Task.Mirror == nil || !message.Task.Mirror.ReadOnly || len(message.Task.Moves) != 0 {
		t.Fatalf("failed probe mirror = %+v moves=%v", message.Task.Mirror, message.Task.Moves)
	}

	// Bridge configured and the caller holds tenant task.write: only the
	// actions the command bridge routes.
	env.surf.SetMirrorFacts(fakeMirrorFacts{
		tasks:     map[uint64]fakeMirrorFact{id: {mirrored: true, source: "superagent", readOnly: false, bridge: true}},
		canBridge: true,
	})
	message = env.taskThread(t, server, "alice", mirrorID)
	if message.Task.Mirror == nil || message.Task.Mirror.ReadOnly || !message.Task.Mirror.Bridge {
		t.Fatalf("bridge mirror fact = %+v", message.Task.Mirror)
	}
	wantMoves(t, moveSet(t, message.Task), "claim", "block", "assign", "cancel")
	wantNoMoves(t, moveSet(t, message.Task), "reply", "needs", "renew", "release", "reopen", "complete", "fail")

	// An archived mirror is read-only regardless of the bridge.
	env.emitArchivedLabel(t, mirrorID)
	message = env.taskThread(t, server, "alice", mirrorID)
	if !message.Task.Archived || len(message.Task.Moves) != 0 {
		t.Fatalf("archived mirror = archived=%v moves=%v", message.Task.Archived, message.Task.Moves)
	}

	// A normal task with source/ticket labels but no noSync is not a mirror.
	normal := env.createTask(t, server, "alice", "work", "normal")
	message = env.taskThread(t, server, "alice", normal)
	if message.Task.Mirror != nil {
		t.Fatalf("normal task reported mirror: %+v", message.Task.Mirror)
	}
}

// pgTaskWriteGate mirrors cmd/keel's superagentOperator.AuthorizeTaskWrite
// without importing package main: the caller's own effective grants must hold
// task.write at tenant scope. The real PG fixture supplies the tenant-admin
// and bot-operator group grants, so the scope distinction is the production
// one rather than a hand-written boolean.
type pgTaskWriteGate struct {
	bridge *accesswire.Bridge
}

func (g pgTaskWriteGate) AuthorizeTaskWrite(ctx context.Context, tenantID uuid.UUID) error {
	if g.bridge == nil {
		return errors.New("operator unavailable")
	}
	principal, err := g.bridge.Principal(ctx, tenantID)
	if err != nil {
		return err
	}
	effective, err := g.bridge.Effective(ctx, principal)
	if err != nil {
		return err
	}
	scope, held := effective.ScopeOf(access.KeyTaskWrite)
	if !held || !scope.Covers(access.ScopeTenant) {
		return errors.New("task.write at tenant scope is required")
	}
	return nil
}

// getAs issues a GET with an explicit test principal, for accounts created by
// this test rather than the fixed fixture principals.
func (e *testEnv) getAs(t *testing.T, record keelstore.AccountRecord, path string) response {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	principal := e.human(record.Username, record)
	request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
	recorder := httptest.NewRecorder()
	e.surf.API().ServeHTTP(recorder, request)
	return response{status: recorder.Code, body: recorder.Body.Bytes()}
}

// doVia issues one request through an explicit handler (the guard-only bridge)
// with a test principal.
func (e *testEnv) doVia(t *testing.T, handler http.Handler, record keelstore.AccountRecord, method, path string, body any) response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != nil {
		request.Header.Set("content-type", "application/json")
	}
	principal := e.human(record.Username, record)
	request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return response{status: recorder.Code, body: recorder.Body.Bytes()}
}

// TestTaskMovesMirrorRequiresTenantTaskWrite is the end-to-end half of the
// CanBridge contract: a mirrored task is only actionable when the caller's own
// task.write reaches tenant scope. tenant-admin has that grant and sees the
// bridged claim; bot-operator only has team task.write, so its mirror is
// read-only with no moves instead of an action that would 403 at the bridge.
func TestTaskMovesMirrorRequiresTenantTaskWrite(t *testing.T) {
	env := newTestEnv(t, nil)
	ctx := context.Background()
	admin := env.account(env.tenant, "tenantadmin", "human")
	operator := env.account(env.tenant, "botoperator", "human")
	for _, member := range []struct {
		account keelstore.AccountRecord
		group   string
	}{
		{admin.record, "tenant-admin"},
		{operator.record, "bot-operator"},
	} {
		if _, err := env.st.AddPermissionGroupMember(ctx, env.tenant,
			access.BuiltinGroupID(env.tenant, member.group), member.account.ID, env.alice.record.ID); err != nil {
			t.Fatalf("add %s to %s: %v", member.account.Username, member.group, err)
		}
	}
	env.relation(env.tenant, "work", "editor", admin.record.ID)
	env.relation(env.tenant, "work", "editor", operator.record.ID)

	mirrorID := env.emitMirrorTask(t, "work", "mirrored ticket")
	id := parseTaskRef(t, mirrorID)
	if _, err := env.st.PutBoardLink(ctx, keelstore.BoardLink{
		TenantID: env.tenant, Source: "superagent", SourceID: "ticket-1",
		Kind: keelstore.BoardLinkTicket, BoardTaskID: id,
	}); err != nil {
		t.Fatalf("put board link: %v", err)
	}
	tokens, err := superagentboard.NewTokenSource("test-secret")
	if err != nil {
		t.Fatalf("token source: %v", err)
	}
	bridge := &superagentboard.Bridge{Operator: pgTaskWriteGate{bridge: env.bridge}}
	bridge.Attach(env.st,
		superagentboard.StaticConfigReader{Config: superagentboard.SuperagentConfig{
			Mirror: true, Bridge: true, Channel: "work",
		}},
		&superagentboard.Mirror{TenantID: env.tenant, Secret: tokens},
		&superagentboard.SupervisorCommands{},
		&superagentboard.LogBoard{Board: env.surf.Board()},
	)
	env.surf.SetMirrorFacts(bridge)

	adminView := env.getAs(t, admin.record, "/v1/board/messages/"+mirrorID)
	var adminThread struct {
		Message messageJSON `json:"message"`
	}
	env.decode(t, adminView, &adminThread)
	if adminThread.Message.Task == nil || adminThread.Message.Task.Mirror == nil {
		t.Fatalf("tenant-admin task = %s", adminView.body)
	}
	if adminThread.Message.Task.Mirror.ReadOnly || !adminThread.Message.Task.Mirror.Bridge {
		t.Fatalf("tenant-admin mirror = %+v", adminThread.Message.Task.Mirror)
	}
	wantMoves(t, moveSet(t, adminThread.Message.Task), "claim")
	wantNoMoves(t, moveSet(t, adminThread.Message.Task), "reply", "complete", "renew", "release")

	operatorView := env.getAs(t, operator.record, "/v1/board/messages/"+mirrorID)
	var operatorThread struct {
		Message messageJSON `json:"message"`
	}
	env.decode(t, operatorView, &operatorThread)
	if operatorThread.Message.Task == nil || operatorThread.Message.Task.Mirror == nil {
		t.Fatalf("bot-operator task = %s", operatorView.body)
	}
	if !operatorThread.Message.Task.Mirror.ReadOnly || operatorThread.Message.Task.Mirror.Bridge {
		t.Fatalf("bot-operator mirror = %+v", operatorThread.Message.Task.Mirror)
	}
	if len(operatorThread.Message.Task.Moves) != 0 {
		t.Fatalf("bot-operator moves = %v, want none", operatorThread.Message.Task.Moves)
	}
}

// TestMirroredThreadsReadOnlyWithoutSupervisor pins the guard-only mount: the
// bridge is constructed before a supervisor exists, so mirror/commands are nil
// while links/config/board are attached. A linked mirrored thread must reject
// reply/edit/delete and task actions as mirror_read_only, while an ordinary
// thread reply still reaches the board.
func TestMirroredThreadsReadOnlyWithoutSupervisor(t *testing.T) {
	env := newTestEnv(t, nil)
	ctx := context.Background()
	env.relation(env.tenant, "work", "owner", env.alice.record.ID)
	normalServer := env.server()
	normalID := env.createTask(t, normalServer, "alice", "work", "normal task")
	mirrorID := env.emitMirrorTask(t, "work", "mirrored ticket")
	id := parseTaskRef(t, mirrorID)
	if _, err := env.st.PutBoardLink(ctx, keelstore.BoardLink{
		TenantID: env.tenant, Source: "superagent", SourceID: "ticket-1",
		Kind: keelstore.BoardLinkTicket, BoardTaskID: id,
	}); err != nil {
		t.Fatalf("put board link: %v", err)
	}

	guard := &superagentboard.Bridge{
		Next: env.surf, Identity: env.surf.Identity(), Authorizer: env.surf.Authorizer(),
	}
	guard.AttachGuard(env.st, superagentboard.StoreConfigReader{Store: env.st},
		&superagentboard.LogBoard{Board: env.surf.Board()})
	env.surf.SetMirrorFacts(guard)

	mirrored, source, readOnly, bridge, err := guard.TaskMirror(ctx, env.tenant, id)
	if err != nil || !mirrored || source != "superagent" || !readOnly || bridge {
		t.Fatalf("guard TaskMirror = mirrored=%v source=%q readOnly=%v bridge=%v err=%v",
			mirrored, source, readOnly, bridge, err)
	}
	if canBridge, err := guard.CanBridge(ctx, env.tenant); err != nil || canBridge {
		t.Fatalf("guard CanBridge = %v/%v, want false", canBridge, err)
	}

	alice := env.alice.record
	reply := env.doVia(t, guard, alice, http.MethodPost, "/v1/board/messages",
		map[string]any{"re": mirrorID, "text": "reply must not land"})
	if reply.status != http.StatusConflict || !containsCode(reply.body, "mirror_read_only") {
		t.Fatalf("mirrored reply = %d %s", reply.status, reply.body)
	}
	edit := env.doVia(t, guard, alice, http.MethodPost, "/v1/board/messages/"+mirrorID+"/edit",
		map[string]any{"text": "edit must not land"})
	if edit.status != http.StatusConflict || !containsCode(edit.body, "mirror_read_only") {
		t.Fatalf("mirrored edit = %d %s", edit.status, edit.body)
	}
	claim := env.doVia(t, guard, alice, http.MethodPost, "/v1/board/tasks/"+mirrorID+"/claim", map[string]any{})
	if claim.status != http.StatusConflict || !containsCode(claim.body, "mirror_read_only") {
		t.Fatalf("mirrored claim without supervisor = %d %s", claim.status, claim.body)
	}

	// A normal thread is not mirrored: the same guard forwards the reply.
	normalReply := env.doVia(t, guard, alice, http.MethodPost, "/v1/board/messages",
		map[string]any{"re": normalID, "text": "normal reply"})
	if normalReply.status != http.StatusCreated {
		t.Fatalf("normal reply = %d %s", normalReply.status, normalReply.body)
	}

	// The read projection also says read-only: no config-only bridge claim is
	// exposed while the supervisor is absent.
	view := env.getAs(t, alice, "/v1/board/messages/"+mirrorID)
	var thread struct {
		Message messageJSON `json:"message"`
	}
	env.decode(t, view, &thread)
	if thread.Message.Task == nil || thread.Message.Task.Mirror == nil {
		t.Fatalf("guard mirror view = %s", view.body)
	}
	if !thread.Message.Task.Mirror.ReadOnly || thread.Message.Task.Mirror.Bridge {
		t.Fatalf("guard mirror fact = %+v", thread.Message.Task.Mirror)
	}
	if len(thread.Message.Task.Moves) != 0 {
		t.Fatalf("guard mirror moves = %v", thread.Message.Task.Moves)
	}
}

func TestBoardJSONErrorsAndSSESurviveAugmentation(t *testing.T) {
	env := newTestEnv(t, nil)
	env.relation(env.tenant, "work", "owner", env.alice.record.ID)
	server := env.server()
	env.createChannel(t, server, "alice", "work")
	taskID := env.createTask(t, server, "alice", "work", "error shape")

	// A board error keeps its status, content type and body.
	missing := env.do(t, server, http.MethodGet, "/v1/board/messages/999999", "alice", nil)
	if missing.status != http.StatusNotFound {
		t.Fatalf("missing message = %d %s", missing.status, missing.body)
	}
	if !strings.Contains(strings.ToLower(string(missing.body)), `"code":"message_not_found"`) {
		t.Fatalf("error body was rewritten: %s", missing.body)
	}
	badClaim := env.do(t, server, http.MethodPost, "/v1/board/tasks/"+taskID+"/complete", "alice",
		map[string]any{"epoch": 9, "token": strings.Repeat("b", 43)})
	if badClaim.status != http.StatusConflict || !containsCode(badClaim.body, "claim_lost") {
		t.Fatalf("claim lost = %d %s", badClaim.status, badClaim.body)
	}

	// A successful task response keeps its status/content type and gains the
	// contract fields; the rest of the view is untouched.
	ok := env.do(t, server, http.MethodGet, "/v1/board/messages/"+taskID, "alice", nil)
	if ok.status != http.StatusOK {
		t.Fatalf("thread = %d", ok.status)
	}
	var thread struct {
		Message messageJSON `json:"message"`
	}
	env.decode(t, ok, &thread)
	if thread.Message.ID != taskID || thread.Message.Text != "error shape" || thread.Message.Task == nil {
		t.Fatalf("thread view changed: %s", ok.body)
	}
	if thread.Message.Task.Moves == nil {
		t.Fatalf("thread task has no moves: %s", ok.body)
	}

	// The queue list and a batch write carry the same additions: every message
	// projection in a task-bearing response is augmented.
	list := env.do(t, server, http.MethodGet, "/v1/board/tasks?state=all", "alice", nil)
	var queue struct {
		Tasks []struct {
			Message messageJSON `json:"message"`
		} `json:"tasks"`
	}
	env.decode(t, list, &queue)
	if len(queue.Tasks) == 0 || queue.Tasks[0].Message.Task == nil || queue.Tasks[0].Message.Task.Moves == nil {
		t.Fatalf("queue task has no moves: %s", list.body)
	}
	batch := env.do(t, server, http.MethodPost, "/v1/board/tasks/batch", "alice", map[string]any{
		"channel": "work",
		"tasks":   []map[string]any{{"key": "first", "text": "batch task"}},
	})
	if batch.status != http.StatusCreated {
		t.Fatalf("batch = %d %s", batch.status, batch.body)
	}
	var created struct {
		Tasks []messageJSON `json:"tasks"`
	}
	env.decode(t, batch, &created)
	if len(created.Tasks) != 1 || created.Tasks[0].Task == nil || created.Tasks[0].Task.Moves == nil {
		t.Fatalf("batch task has no moves: %s", batch.body)
	}

	// SSE is not buffered or rewritten: its headers arrive and the stream stops
	// when the client closes.
	req, err := http.NewRequest(http.MethodGet, server.URL+"/v1/board/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-Who", "alice")
	req.Header.Set("Accept", "text/event-stream")
	client := &http.Client{Timeout: 2 * time.Second}
	stream, err := client.Do(req)
	if err != nil {
		t.Fatalf("sse: %v", err)
	}
	defer stream.Body.Close()
	if !strings.HasPrefix(stream.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("sse content type = %q", stream.Header.Get("Content-Type"))
	}
}

func TestSyncEntriesMergeCredentialStatusAndUnknownLag(t *testing.T) {
	env := newTestEnvBefore(t, nil, func(env *testEnv) {
		_, err := env.st.CreateBoardSyncEntry(context.Background(), keelstore.BoardSyncEntryRecord{
			TenantID: env.tenant, Key: "slack", Platform: "slack",
			SecretRef: map[string]string{"signingSecret": "MISSING_SIGNING"}, Enabled: true,
		})
		if err != nil {
			env.t.Fatalf("create sync entry: %v", err)
		}
	})
	server := env.server()

	entries := env.do(t, server, http.MethodGet, "/v1/board/sync/entries", "alice", nil)
	if entries.status != http.StatusOK {
		t.Fatalf("sync entries = %d %s", entries.status, entries.body)
	}
	var entryBody struct {
		Entries []struct {
			Key         string            `json:"key"`
			Credential  string            `json:"credential"`
			Restart     bool              `json:"restartRequired"`
			Lag         json.RawMessage   `json:"lag"`
			Targets     []json.RawMessage `json:"targets"`
			Failures24h *int              `json:"failures24h"`
		} `json:"entries"`
		Lag json.RawMessage `json:"lag"`
	}
	env.decode(t, entries, &entryBody)
	if len(entryBody.Entries) != 1 || entryBody.Entries[0].Key != "slack" {
		t.Fatalf("entries = %s", entries.body)
	}
	if entryBody.Entries[0].Credential != "invalid" {
		t.Fatalf("credential = %q: %s", entryBody.Entries[0].Credential, entries.body)
	}
	// board v0.1.2 measures the empty queues as {inbox:null,outbox:null}.
	// The mount must pass that through, not overwrite it with a bare null.
	assertEmptyLag(t, entryBody.Entries[0].Lag, entries.body)
	if entryBody.Entries[0].Failures24h == nil || *entryBody.Entries[0].Failures24h != 0 {
		t.Fatalf("failures24h = %v: %s", entryBody.Entries[0].Failures24h, entries.body)
	}
	if entryBody.Entries[0].Targets == nil {
		t.Fatalf("targets is null, want []: %s", entries.body)
	}
	if string(entryBody.Lag) != "null" {
		t.Fatalf("entries root lag = %s, want null: %s", entryBody.Lag, entries.body)
	}
	var entryRaw map[string]json.RawMessage
	if err := json.Unmarshal(entries.body, &entryRaw); err != nil {
		t.Fatalf("decode entries raw: %v", err)
	}
	if _, ok := entryRaw["lag"]; !ok {
		t.Fatalf("entries response has no lag key: %s", entries.body)
	}

	status := env.do(t, server, http.MethodGet, "/v1/board/sync/status", "alice", nil)
	if status.status != http.StatusOK {
		t.Fatalf("sync status = %d %s", status.status, status.body)
	}
	var statusBody struct {
		Keys []struct {
			Key         string          `json:"key"`
			Credential  string          `json:"credential"`
			Lag         json.RawMessage `json:"lag"`
			Failures24h *int            `json:"failures24h"`
		} `json:"keys"`
		SyncEnabled *bool           `json:"syncEnabled"`
		Lag         json.RawMessage `json:"lag"`
	}
	env.decode(t, status, &statusBody)
	if len(statusBody.Keys) != 1 || statusBody.Keys[0].Credential != "invalid" {
		t.Fatalf("status keys = %s", status.body)
	}
	if statusBody.SyncEnabled == nil || !*statusBody.SyncEnabled {
		t.Fatalf("syncEnabled = %v", statusBody.SyncEnabled)
	}
	assertEmptyLag(t, statusBody.Lag, status.body)
	assertEmptyLag(t, statusBody.Keys[0].Lag, status.body)
	if statusBody.Keys[0].Failures24h == nil || *statusBody.Keys[0].Failures24h != 0 {
		t.Fatalf("status failures24h = %v: %s", statusBody.Keys[0].Failures24h, status.body)
	}

	// Another tenant never sees the deployment's entry list merged with its
	// credentials: it has no sync entry, and the response is empty.
	daveEntries := env.do(t, server, http.MethodGet, "/v1/board/sync/entries", "dave", nil)
	if daveEntries.status != http.StatusOK || strings.Contains(string(daveEntries.body), `"slack"`) {
		t.Fatalf("cross-tenant sync entries = %d %s", daveEntries.status, daveEntries.body)
	}
}

// assertEmptyLag requires a measured lag object whose two queues are null.
// A bare null is unknown (an older board), which is a different fact.
func assertEmptyLag(t *testing.T, raw json.RawMessage, body []byte) {
	t.Helper()
	if string(raw) == "null" {
		t.Fatalf("lag = null, want a measured {inbox:null,outbox:null}: %s", body)
	}
	var lag struct {
		Inbox  *json.RawMessage `json:"inbox"`
		Outbox *json.RawMessage `json:"outbox"`
	}
	if err := json.Unmarshal(raw, &lag); err != nil {
		t.Fatalf("decode lag %s: %v", raw, err)
	}
	if lag.Inbox != nil || lag.Outbox != nil {
		t.Fatalf("lag = %s, want both queues null", raw)
	}
}
