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

func TestPGBoardMirrorNeverWritesAnyCollabTable(t *testing.T) {
	st, pg := newPGBoard(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "snapshot-"+tenant.String()[:8]); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	ticket := pgSeedTicket(t, st, tenant, "evt-snapshot")
	assignment, err := st.CreateAssignment(ctx, collab.Assignment{
		ID: uuid.New(), TenantID: tenant, WorkItemID: ticket.ID,
		ModeID: superagent.DefaultModeID, BotID: "researcher", SessionID: uuid.New(),
		Status: collab.AssignmentActive, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("assignment: %v", err)
	}
	if _, err := st.CreateAttention(ctx, collab.Attention{
		ID: uuid.New(), TenantID: tenant, WorkItemID: ticket.ID,
		AssignmentID: assignment.ID, ModeID: superagent.DefaultModeID,
		SessionID: assignment.SessionID, Kind: collab.AttentionClarify, Prompt: "Which?",
		Route: collab.AttentionRouteChannel, Status: collab.AttentionOpen,
		CreatedAt: time.Now().UTC(), Deadline: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("attention: %v", err)
	}
	report, created, err := st.ClaimWorkerReport(ctx, superagent.WorkerReport{
		ID: uuid.New(), TenantID: tenant, WorkItemID: ticket.ID, AssignmentID: assignment.ID,
		SessionID: assignment.SessionID, RequestID: "report-1", State: superagent.ReportProgress,
		Summary: "halfway", CreatedAt: time.Now().UTC(),
	})
	if err != nil || !created {
		t.Fatalf("report claim: created=%v err=%v", created, err)
	}
	if _, err := st.MarkWorkerReportAudited(ctx, tenant, report.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkWorkerReportApplied(ctx, tenant, report.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	item, err := st.GetWorkItem(ctx, tenant, ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	item.Status = collab.WorkItemBlocked
	if _, err := st.UpdateWorkItem(ctx, item); err != nil {
		t.Fatal(err)
	}

	before := pgCollabSnapshot(t, st, tenant)
	clock := newTestClock()
	mirror := newMirrorForTest(t, tenant, st, pg.log, st,
		SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"}, clock)
	if _, err := mirror.SyncOnce(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	after := pgCollabSnapshot(t, st, tenant)
	if before != after {
		t.Fatalf("the mirror rewrote a collab table\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if head := boardHead(t, pg.log, tenant); head == 0 {
		t.Fatal("the mirror wrote no board events")
	}
}

func pgCollabSnapshot(t *testing.T, st *keelstore.Store, tenant uuid.UUID) string {
	t.Helper()
	tables := []string{
		"collab_work_items", "collab_assignments", "collab_worker_reports",
		"collab_attentions", "collab_links", "collab_subscriptions",
		"collab_source_cursors", "collab_source_events", "collab_outbox",
		"collab_session_index",
	}
	var builder strings.Builder
	for _, table := range tables {
		rows, err := st.Pool().Query(context.Background(),
			"SELECT row_to_json(t)::text FROM "+table+" t WHERE tenant_id = $1 ORDER BY 1", tenant)
		if err != nil {
			t.Fatalf("snapshot %s: %v", table, err)
		}
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				rows.Close()
				t.Fatalf("scan %s: %v", table, err)
			}
			builder.WriteString(table)
			builder.WriteString(":")
			builder.WriteString(row)
			builder.WriteString("\n")
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("rows %s: %v", table, err)
		}
		rows.Close()
	}
	return builder.String()
}

func TestPGBoardMirrorWritesManyAttentionAndReportLinks(t *testing.T) {
	st, pg := newPGBoard(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "many-links-"+tenant.String()[:8]); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	ticket := pgSeedTicket(t, st, tenant, "evt-many")
	assignment, err := st.CreateAssignment(ctx, collab.Assignment{
		ID: uuid.New(), TenantID: tenant, WorkItemID: ticket.ID,
		ModeID: superagent.DefaultModeID, BotID: "researcher", SessionID: uuid.New(),
		Status: collab.AssignmentActive, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("assignment: %v", err)
	}
	for index := 0; index < 2; index++ {
		attention, err := st.CreateAttention(ctx, collab.Attention{
			ID: uuid.New(), TenantID: tenant, WorkItemID: ticket.ID,
			AssignmentID: assignment.ID, ModeID: superagent.DefaultModeID,
			SessionID: assignment.SessionID, Kind: collab.AttentionClarify, Prompt: "Which?",
			Route: collab.AttentionRouteChannel, Status: collab.AttentionAnswered,
			Answer:    &collab.AttentionAnswer{OptionID: "yes", At: time.Now().UTC()},
			CreatedAt: time.Now().UTC().Add(time.Duration(index) * time.Second),
			Deadline:  time.Now().UTC().Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("attention %d: %v", index, err)
		}
		_ = attention
	}
	for index := 0; index < 2; index++ {
		report, created, err := st.ClaimWorkerReport(ctx, superagent.WorkerReport{
			ID: uuid.New(), TenantID: tenant, WorkItemID: ticket.ID, AssignmentID: assignment.ID,
			SessionID: assignment.SessionID, RequestID: "report-" + uuid.NewString(),
			State: superagent.ReportProgress, Summary: "step", CreatedAt: time.Now().UTC(),
		})
		if err != nil || !created {
			t.Fatalf("report %d: created=%v err=%v", index, created, err)
		}
		if _, err := st.MarkWorkerReportAudited(ctx, tenant, report.ID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if _, err := st.MarkWorkerReportApplied(ctx, tenant, report.ID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	mirror := newMirrorForTest(t, tenant, st, pg.log, st,
		SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"}, newTestClock())
	if _, err := mirror.SyncOnce(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	var attentions, reports int
	if err := st.Pool().QueryRow(ctx,
		`SELECT count(*) FROM board_links WHERE tenant_id=$1 AND kind='attention'`, tenant).Scan(&attentions); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool().QueryRow(ctx,
		`SELECT count(*) FROM board_links WHERE tenant_id=$1 AND kind='report'`, tenant).Scan(&reports); err != nil {
		t.Fatal(err)
	}
	if attentions != 2 || reports != 2 {
		t.Fatalf("links = attention:%d report:%d, want 2/2", attentions, reports)
	}
}
