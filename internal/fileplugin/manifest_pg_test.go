package fileplugin_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jixoai/keel/internal/fileplugin"
	"github.com/jixoai/keel/internal/layerfs"
)

// TestPostgresManifestProjection exercises the durable SQL against the shared
// test database. The tables are created by the test (the production migration
// owner is internal/store/schema.sql; this slice must not edit it) and dropped
// afterwards. Skipped when KEEL_TEST_DSN is absent.
func TestPostgresManifestProjection(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS file_manifest, session_snapshot CASCADE`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range strings.Split(fileplugin.SchemaDDL, ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("apply DDL: %v\n%s", err, statement)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS file_manifest, session_snapshot CASCADE`)
	})
	manifest, err := fileplugin.NewPostgresManifest(pool)
	if err != nil {
		t.Fatal(err)
	}
	tenantID, sessionID := uuid.New(), uuid.New()
	first := layerfs.HashBytes([]byte("first"))
	second := layerfs.HashBytes([]byte("second"))
	entries := []fileplugin.Entry{
		{Path: "a.txt", Hash: first, Mode: 0o644, Size: 5, UpdatedSeq: 10, LastTaskID: "task-1"},
		{Path: "dir/b.txt", Hash: second, Mode: 0o600, Size: 6, UpdatedSeq: 10, LastTaskID: "task-1"},
	}
	if err := manifest.Upsert(ctx, tenantID, sessionID, entries); err != nil {
		t.Fatal(err)
	}
	got, ok, err := manifest.Get(ctx, tenantID, sessionID, "a.txt")
	if err != nil || !ok || got.Hash != first || got.UpdatedSeq != 10 || got.LastTaskID != "task-1" {
		t.Fatalf("get = %+v/%v/%v", got, ok, err)
	}
	list, err := manifest.List(ctx, tenantID, sessionID)
	if err != nil || len(list) != 2 || list[0].Path != "a.txt" || list[1].Path != "dir/b.txt" {
		t.Fatalf("list = %+v/%v", list, err)
	}
	// An older event cannot overwrite a newer projection.
	if err := manifest.Upsert(ctx, tenantID, sessionID, []fileplugin.Entry{
		{Path: "a.txt", Hash: second, Mode: 0o644, Size: 6, UpdatedSeq: 5},
	}); err != nil {
		t.Fatal(err)
	}
	got, _, err = manifest.Get(ctx, tenantID, sessionID, "a.txt")
	if err != nil || got.Hash != first || got.UpdatedSeq != 10 {
		t.Fatalf("out-of-order upsert = %+v/%v", got, err)
	}
	if err := manifest.PutSnapshot(ctx, tenantID, sessionID, 10, first); err != nil {
		t.Fatal(err)
	}
	tree, ok, err := manifest.Snapshot(ctx, tenantID, sessionID, 10)
	if err != nil || !ok || tree != first {
		t.Fatalf("snapshot = %s/%v/%v", tree, ok, err)
	}
	if err := manifest.Delete(ctx, tenantID, sessionID, "a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := manifest.Get(ctx, tenantID, sessionID, "a.txt"); err != nil || ok {
		t.Fatalf("deleted entry = %v/%v", ok, err)
	}
}
