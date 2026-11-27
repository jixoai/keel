package superagentboard

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jixoai/keel/internal/collab"
	keelstore "github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/superagent"

	boardlog "github.com/jixoai/keel-board/log"
	boardstore "github.com/jixoai/keel-board/store"
)

// pgBoard is the real-PostgreSQL fixture: keel's source of truth plus a board
// log in a throwaway schema. It proves the mirror's link/claim/idempotency
// survives a process restart, not just an in-memory map.
type pgBoard struct {
	dsn    string
	schema string
	board  *boardstore.Postgres
	log    *LogBoard
}

func newPGBoard(t *testing.T) (*keelstore.Store, *pgBoard) {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := keelstore.Open(ctx, keelstore.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open keel: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		t.Fatalf("migrate keel: %v", err)
	}
	schema := "board_mirror_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := st.Pool().Exec(ctx, "CREATE SCHEMA "+pgIdent(schema)); err != nil {
		st.Close()
		t.Fatalf("create schema: %v", err)
	}
	pg := &pgBoard{dsn: dsn, schema: schema}
	pg.board, pg.log = openPGBoard(t, dsn, schema)
	t.Cleanup(func() {
		pg.board.Close()
		_, _ = st.Pool().Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgIdent(schema)+" CASCADE")
		st.Close()
	})
	return st, pg
}

func openPGBoard(t *testing.T, dsn, schema string) (*boardstore.Postgres, *LogBoard) {
	t.Helper()
	store, err := boardstore.OpenPostgres(context.Background(), boardstore.PostgresOptions{DSN: dsn, SearchPath: schema})
	if err != nil {
		t.Fatalf("open board store: %v", err)
	}
	return store, &LogBoard{Board: boardlog.New(boardlog.Options{Store: store})}
}

func pgIdent(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }

func pgSeedTicket(t *testing.T, st *keelstore.Store, tenantID uuid.UUID, eventID string) superagent.Ticket {
	t.Helper()
	event := superagent.Event{
		SourceID: "github", EventID: eventID, Kind: superagent.EventIssueOpened,
		Actor:  superagent.Actor{ID: "octocat", Login: "octocat"},
		Thread: superagent.ThreadRef{Source: "github", Kind: "issue", ID: "99"},
		Title:  "PG ticket", Body: "Body",
	}
	ticket := superagent.NewTicket(tenantID, superagent.DefaultModeID, event,
		superagent.Verdict{Action: superagent.ActionAccept, Strength: superagent.MandateWrite},
		"researcher", time.Now().UTC())
	ticket.IdempotencyKey = event.IdempotencyKey()
	item, err := st.CreateWorkItem(context.Background(), ticket.Record())
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	return superagent.TicketFromWorkItem(item)
}

func TestPGBoardMirrorSurvivesRestartAndAtoB(t *testing.T) {
	st, pg := newPGBoard(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	for _, tenant := range []uuid.UUID{tenantA, tenantB} {
		if err := st.CreateTenant(ctx, tenant, "mirror-"+tenant.String()[:8]); err != nil {
			t.Fatalf("create tenant: %v", err)
		}
	}
	ticketA := pgSeedTicket(t, st, tenantA, "evt-a")
	pgSeedTicket(t, st, tenantB, "evt-b")

	clock := newTestClock()
	mirrorA := newMirrorForTest(t, tenantA, st, pg.log, st,
		SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"}, clock)
	if _, err := mirrorA.SyncOnce(ctx); err != nil {
		t.Fatalf("sync A: %v", err)
	}
	linkA := mustLink(t, st, tenantA, ticketA.ID.String(), keelstore.BoardLinkTicket)
	taskA := mustTask(t, pg.log, tenantA, linkA.BoardTaskID)
	if taskA.State != StateReady {
		t.Fatalf("state = %q", taskA.State)
	}
	if _, found, err := st.GetBoardLink(ctx, tenantB, "superagent", ticketA.ID.String(), keelstore.BoardLinkTicket); err != nil || found {
		t.Fatalf("tenant B sees A's link: found=%v err=%v", found, err)
	}

	// Simulate a process restart: a new board store (same schema) and a new
	// mirror. The link row and the board log are the durable facts.
	pg.board.Close()
	pg.board, pg.log = openPGBoard(t, pg.dsn, pg.schema)
	headBefore := boardHead(t, pg.log, tenantA)
	restarted := newMirrorForTest(t, tenantA, st, pg.log, st,
		SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"}, clock)
	if _, err := restarted.SyncOnce(ctx); err != nil {
		t.Fatalf("restart sync: %v", err)
	}
	linkAgain := mustLink(t, st, tenantA, ticketA.ID.String(), keelstore.BoardLinkTicket)
	if linkAgain.BoardTaskID != linkA.BoardTaskID {
		t.Fatalf("restart task = %d, want %d", linkAgain.BoardTaskID, linkA.BoardTaskID)
	}
	if headBefore != boardHead(t, pg.log, tenantA) {
		t.Fatal("restart appended a second board thread")
	}
	// Tenant B never mirrored: its board is empty.
	if head := boardHead(t, pg.log, tenantB); head != 0 {
		t.Fatalf("tenant B board head = %d", head)
	}
}

func TestPGBoardClaimEpochPersistsAndIsRecoverable(t *testing.T) {
	st, pg := newPGBoard(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "mirror-claim-"+tenant.String()[:8]); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	ticket := pgSeedTicket(t, st, tenant, "evt-claim")
	assignment, err := st.CreateAssignment(ctx, collab.Assignment{
		ID: uuid.New(), TenantID: tenant, WorkItemID: ticket.ID,
		ModeID: superagent.DefaultModeID, BotID: "researcher", SessionID: uuid.New(),
		Status: collab.AssignmentActive, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("assignment: %v", err)
	}
	item, err := st.GetWorkItem(ctx, tenant, ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	item.Status = collab.WorkItemActive
	if _, err := st.UpdateWorkItem(ctx, item); err != nil {
		t.Fatal(err)
	}

	clock := newTestClock()
	mirror := newMirrorForTest(t, tenant, st, pg.log, st,
		SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"}, clock)
	if _, err := mirror.SyncOnce(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	link := mustLink(t, st, tenant, ticket.ID.String(), keelstore.BoardLinkTicket)
	if link.ClaimEpoch != 1 {
		t.Fatalf("claim epoch = %d, want 1", link.ClaimEpoch)
	}
	task := mustTask(t, pg.log, tenant, link.BoardTaskID)
	if task.State != StateClaimed || task.ClaimActor != WorkerActor(assignment.BotID).ID {
		t.Fatalf("task = %+v", task)
	}

	// A restart rebuilds the same deterministic token and sees the live claim
	// as already mirrored; it neither claims again nor renews early.
	tokens, _ := NewTokenSource("test-secret")
	token, err := tokens.ClaimToken(tenant, assignment.ID, assignment.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	again, made, err := pg.log.Claim(ctx, tenant, ClaimRequest{
		TaskID: link.BoardTaskID, Actor: WorkerActor(assignment.BotID), Token: token, Lease: DefaultLease,
	})
	if err != nil || made || again.Epoch != 1 {
		t.Fatalf("rebuild claim = %+v made=%v err=%v", again, made, err)
	}
	if _, err := mirror.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	linkAgain := mustLink(t, st, tenant, ticket.ID.String(), keelstore.BoardLinkTicket)
	if linkAgain.ClaimEpoch != 1 {
		t.Fatalf("restart changed claim epoch to %d", linkAgain.ClaimEpoch)
	}
}

// silence the unused-import guard when a build tags variant removes pgx.
var _ = pgx.ErrNoRows
