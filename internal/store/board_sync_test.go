package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestBoardSyncEntryCRUD covers the keel-side configuration table: create,
// read/list, revisioned update, enable/disable and delete, plus the rollback
// delete used when the board's local row cannot be written.
func TestBoardSyncEntryCRUD(t *testing.T) {
	st := tenancyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	record := BoardSyncEntryRecord{
		TenantID: tenant, Key: "slack", Platform: "slack", Enabled: true,
		SecretRef:     map[string]string{"signingSecret": "SLACK_SIGNING"},
		Settings:      json.RawMessage(`{"botUserId":"U1"}`),
		Routes:        map[string]string{"C1": "support"},
		StartChannels: []string{"C1"},
	}
	created, err := st.CreateBoardSyncEntry(ctx, record)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Revision != 1 || !created.Enabled || created.SecretRef["signingSecret"] != "SLACK_SIGNING" {
		t.Fatalf("created = %+v", created)
	}
	if _, err := st.CreateBoardSyncEntry(ctx, record); !errors.Is(err, ErrBoardSyncEntryExists) {
		t.Fatalf("duplicate create = %v", err)
	}
	got, found, err := st.GetBoardSyncEntry(ctx, tenant, "slack")
	if err != nil || !found || got.Key != "slack" || got.Routes["C1"] != "support" {
		t.Fatalf("get = %+v/%v/%v", got, found, err)
	}
	list, err := st.ListBoardSyncEntries(ctx, tenant)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v/%v", list, err)
	}
	if _, err := st.ListBoardSyncEntries(ctx, uuid.New()); err != nil {
		t.Fatalf("list another tenant: %v", err)
	}

	next := got
	next.Settings = json.RawMessage(`{"botUserId":"U2"}`)
	next.Enabled = false
	updated, err := st.UpdateBoardSyncEntry(ctx, next, 1)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Revision != 2 || updated.Enabled {
		t.Fatalf("updated = %+v", updated)
	}
	if _, err := st.UpdateBoardSyncEntry(ctx, next, 1); !errors.Is(err, ErrBoardSyncEntryConflict) {
		t.Fatalf("stale update = %v", err)
	}
	if _, err := st.UpdateBoardSyncEntry(ctx, BoardSyncEntryRecord{TenantID: tenant, Key: "missing"}, 1); !errors.Is(err, ErrBoardSyncEntryMissing) {
		t.Fatalf("missing update = %v", err)
	}
	enabled, err := st.SetBoardSyncEntryEnabled(ctx, tenant, "slack", true)
	if err != nil || !enabled.Enabled || enabled.Revision != 3 {
		t.Fatalf("enable = %+v/%v", enabled, err)
	}
	if deleted, err := st.DeleteBoardSyncEntry(ctx, tenant, "slack"); err != nil || !deleted {
		t.Fatalf("delete = %v/%v", deleted, err)
	}
	if _, found, err := st.GetBoardSyncEntry(ctx, tenant, "slack"); err != nil || found {
		t.Fatalf("deleted get = %v/%v", found, err)
	}
}

// TestBoardBlobKeyCRUD covers the attachment key translation: idempotent put,
// conflict on a different object, reverse reference check and delete.
func TestBoardBlobKeyCRUD(t *testing.T) {
	st := tenancyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	blob := uuid.New()
	key := "tenants/deadbeef/attachments/a1"
	record := BoardBlobKey{Key: key, TenantID: tenant, BlobID: blob, ContentType: "text/plain", Size: 3}
	if err := st.PutBoardBlobKey(ctx, record); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := st.PutBoardBlobKey(ctx, record); err != nil {
		t.Fatalf("idempotent put: %v", err)
	}
	got, found, err := st.GetBoardBlobKey(ctx, key)
	if err != nil || !found || got.BlobID != blob || got.Size != 3 {
		t.Fatalf("get = %+v/%v/%v", got, found, err)
	}
	referenced, err := st.BoardBlobKeyReferenced(ctx, tenant, blob)
	if err != nil || !referenced {
		t.Fatalf("referenced = %v/%v", referenced, err)
	}
	other := record
	other.BlobID = uuid.New()
	if err := st.PutBoardBlobKey(ctx, other); err == nil {
		t.Fatal("different blob under one key was accepted")
	}
	if deleted, found, err := st.DeleteBoardBlobKey(ctx, key); err != nil || !found || deleted.Key != key {
		t.Fatalf("delete = %+v/%v/%v", deleted, found, err)
	}
	if _, found, err := st.GetBoardBlobKey(ctx, key); err != nil || found {
		t.Fatalf("deleted get = %v/%v", found, err)
	}
}
