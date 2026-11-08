package agentapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jixoai/keel/internal/store"
)

// openTestStore creates a dedicated database for one test so parallel packages
// sharing KEEL_TEST_DSN never see each other's sessions or runs. Skipped without
// a DSN, like every database test in the repository.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse test dsn: %v", err)
	}
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	name := "keel_agentapi_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close(ctx)
		t.Fatalf("create test database: %v", err)
	}
	st, err := store.Open(ctx, store.Config{DSN: dsnWithDatabase(dsn, name)})
	if err != nil {
		admin.Close(ctx)
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer dropCancel()
		_, _ = admin.Exec(dropCtx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		admin.Close(dropCtx)
	})
	return st
}

func dsnWithDatabase(dsn, name string) string {
	if parsed, err := url.Parse(dsn); err == nil && parsed.Scheme != "" {
		parsed.Path = "/" + name
		return parsed.String()
	}
	return dsn + " dbname=" + name
}

func TestPGSessionListerKeysetPagination(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	otherTenant := uuid.New()
	if err := st.CreateTenant(ctx, tenantID, "pagination"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, otherTenant, "other"); err != nil {
		t.Fatal(err)
	}
	lister := NewPGSessionLister(st)
	created := make([]uuid.UUID, 0, 120)
	for i := 0; i < 120; i++ {
		sessionID := uuid.New()
		created = append(created, sessionID)
		if err := st.CreateSession(ctx, tenantID, sessionID); err != nil {
			t.Fatal(err)
		}
	}
	// One session gets a run row, so the list has to project the run status.
	runSessionID := created[0]
	if err := st.EnqueueRun(ctx, tenantID, runSessionID); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, otherTenant, uuid.New()); err != nil {
		t.Fatal(err)
	}

	// 120 sessions, 50 per page: 50/50/20, no duplicates or omissions, and the
	// foreign tenant never appears.
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		page, err := lister.ListSessionSummaries(ctx, tenantID, 50, cursor)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		for _, summary := range page.Sessions {
			if seen[summary.SessionID] {
				t.Fatalf("duplicate %s", summary.SessionID)
			}
			seen[summary.SessionID] = true
			if summary.SessionID == runSessionID.String() {
				if summary.RunStatus != "pending" || summary.LastSeq != 0 {
					t.Fatalf("run session summary = %+v", summary)
				}
			}
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
		if cursor == "" {
			t.Fatal("hasMore without cursor")
		}
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if pages != 3 || len(seen) != 120 {
		t.Fatalf("pages=%d sessions=%d", pages, len(seen))
	}
	for _, sessionID := range created {
		if !seen[sessionID.String()] {
			t.Fatalf("session %s missing from the pages", sessionID)
		}
	}
	if _, err := lister.ListSessionSummaries(ctx, tenantID, 10, "garbage"); err == nil {
		t.Fatal("a malformed cursor must fail")
	}

	// The HTTP layer turns the same cursor into a page.
	server := httptest.NewServer(New(Config{Sessions: lister, Logger: testLogger()}))
	defer server.Close()
	var page struct {
		Sessions   []SessionSummary `json:"sessions"`
		NextCursor string           `json:"nextCursor"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions?limit=5", tenantID, nil, &page); code != 200 {
		t.Fatalf("http page: %d", code)
	}
	if len(page.Sessions) != 5 || page.NextCursor == "" {
		t.Fatalf("http page = %+v", page)
	}
}
