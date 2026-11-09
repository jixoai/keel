package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Model settings integration tests (change keel-model-settings). Skipped when
// KEEL_TEST_DSN is unset, like the other store integration tests.

// modelSettingsHistoryRow is one raw history row, read back through the pool so
// the test verifies what was actually persisted.
type modelSettingsHistoryRow struct {
	Revision  int64
	Document  *string
	Source    string
	CreatedAt time.Time
}

func readModelSettingsHistory(t *testing.T, st *Store, tenantID uuid.UUID) []modelSettingsHistoryRow {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(),
		`SELECT revision, document::text, source, created_at
		 FROM model_settings_history WHERE tenant_id = $1 ORDER BY revision`, tenantID)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	defer rows.Close()
	var out []modelSettingsHistoryRow
	for rows.Next() {
		var row modelSettingsHistoryRow
		if err := rows.Scan(&row.Revision, &row.Document, &row.Source, &row.CreatedAt); err != nil {
			t.Fatalf("scan history: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("history rows: %v", err)
	}
	return out
}

func TestModelSettingsReadWriteAndHistory(t *testing.T) {
	st := tenancyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()

	// Nothing stored: the tenant runs on its deployment document, revision 0.
	doc, revision, updatedAt, found, err := st.GetModelSettings(ctx, tenant)
	if err != nil {
		t.Fatalf("get empty: %v", err)
	}
	if found || doc != nil || revision != 0 || !updatedAt.IsZero() {
		t.Fatalf("empty settings = (%s, %d, %v, %v), want (nil, 0, zero, false)", doc, revision, updatedAt, found)
	}

	first := json.RawMessage(`{"providers":[{"id":"s2api"}],"selection":{"providerId":"s2api","modelId":"deepseek-v4.1-flash"}}`)
	revision, err = st.SetModelSettings(ctx, tenant, 0, first)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if revision != 1 {
		t.Fatalf("first write revision = %d, want 1", revision)
	}
	doc, revision, updatedAt, found, err = st.GetModelSettings(ctx, tenant)
	if err != nil {
		t.Fatalf("get stored: %v", err)
	}
	if !found || revision != 1 {
		t.Fatalf("stored settings = (found %v, revision %d), want (true, 1)", found, revision)
	}
	if updatedAt.IsZero() {
		t.Fatal("stored settings must carry updated_at")
	}
	if !reflect.DeepEqual(decodeRawJSON(t, doc), decodeRawJSON(t, first)) {
		t.Fatalf("stored document = %s, want %s", doc, first)
	}

	// A second write advances the revision and reuses the one row.
	second := json.RawMessage(`{"selection":{"providerId":"s2api","modelId":"claude-sonnet-5"}}`)
	revision, err = st.SetModelSettings(ctx, tenant, 0, second)
	if err != nil {
		t.Fatalf("set second: %v", err)
	}
	if revision != 2 {
		t.Fatalf("second write revision = %d, want 2", revision)
	}
	doc, _, _, _, err = st.GetModelSettings(ctx, tenant)
	if err != nil {
		t.Fatalf("get second: %v", err)
	}
	if !reflect.DeepEqual(decodeRawJSON(t, doc), decodeRawJSON(t, second)) {
		t.Fatalf("stored document = %s, want %s", doc, second)
	}

	// The history is append-only, one row per accepted write.
	history := readModelSettingsHistory(t, st, tenant)
	if len(history) != 2 {
		t.Fatalf("history rows = %d, want 2: %+v", len(history), history)
	}
	for i, want := range []json.RawMessage{first, second} {
		row := history[i]
		if row.Revision != int64(i+1) || row.Source != ModelSettingsSourceStored {
			t.Fatalf("history[%d] = %+v, want revision %d source stored", i, row, i+1)
		}
		if row.Document == nil || !reflect.DeepEqual(decodeRawJSON(t, []byte(*row.Document)), decodeRawJSON(t, want)) {
			t.Fatalf("history[%d] document = %v, want %s", i, row.Document, want)
		}
	}

	// A reset deletes the override and appends a deployment row.
	revision, err = st.DeleteModelSettings(ctx, tenant, 0)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if revision != 3 {
		t.Fatalf("reset revision = %d, want 3", revision)
	}
	doc, revision, _, found, err = st.GetModelSettings(ctx, tenant)
	if err != nil {
		t.Fatalf("get after reset: %v", err)
	}
	if found || doc != nil {
		t.Fatalf("after reset: found=%v doc=%s, want no override", found, doc)
	}
	if revision != 3 {
		t.Fatalf("after reset revision = %d, want the history head 3", revision)
	}
	history = readModelSettingsHistory(t, st, tenant)
	if len(history) != 3 {
		t.Fatalf("history rows after reset = %d, want 3", len(history))
	}
	reset := history[2]
	if reset.Revision != 3 || reset.Source != ModelSettingsSourceDeployment || reset.Document != nil {
		t.Fatalf("reset history row = %+v, want revision 3 source deployment with no document", reset)
	}

	// A reset with no override is refused; the caller answers idempotently.
	if _, err := st.DeleteModelSettings(ctx, tenant, 0); !errors.Is(err, ErrNoModelSettings) {
		t.Fatalf("second reset err = %v, want ErrNoModelSettings", err)
	}
}

// TestModelSettingsRevisionMonotonicAcrossReset pins the change identity: a
// write after a reset continues the sequence instead of restarting, so a client
// polling the revision never sees it move backwards.
func TestModelSettingsRevisionMonotonicAcrossReset(t *testing.T) {
	st := tenancyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	doc := json.RawMessage(`{"selection":{"providerId":"s2api","modelId":"deepseek-v4.1-flash"}}`)

	var revisions []int64
	for i := 0; i < 2; i++ {
		revision, err := st.SetModelSettings(ctx, tenant, 0, doc)
		if err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
		revisions = append(revisions, revision)
	}
	revision, err := st.DeleteModelSettings(ctx, tenant, 0)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	revisions = append(revisions, revision)
	revision, err = st.SetModelSettings(ctx, tenant, 0, doc)
	if err != nil {
		t.Fatalf("set after reset: %v", err)
	}
	revisions = append(revisions, revision)

	for i := 1; i < len(revisions); i++ {
		if revisions[i] != revisions[i-1]+1 {
			t.Fatalf("revisions = %v, want a strictly increasing +1 sequence", revisions)
		}
	}
	// The revision stays readable while the override is in place and after a
	// reset, so a reader always has a change identity.
	if _, revision, _, found, err := st.GetModelSettings(ctx, tenant); err != nil || !found || revision != 4 {
		t.Fatalf("stored head = (%d, found %v, err %v), want revision 4", revision, found, err)
	}
	if _, err := st.DeleteModelSettings(ctx, tenant, 0); err != nil {
		t.Fatalf("second reset: %v", err)
	}
	if _, revision, _, found, err := st.GetModelSettings(ctx, tenant); err != nil || found || revision != 5 {
		t.Fatalf("reset head = (%d, found %v, err %v), want revision 5", revision, found, err)
	}
}

func TestModelSettingsCrossTenantIsolation(t *testing.T) {
	st := tenancyTestStore(t)
	ctx := context.Background()
	a, b := uuid.New(), uuid.New()
	docA := json.RawMessage(`{"selection":{"providerId":"s2api","modelId":"a-model"}}`)
	docB := json.RawMessage(`{"selection":{"providerId":"s2api","modelId":"b-model"}}`)

	if _, err := st.SetModelSettings(ctx, a, 0, docA); err != nil {
		t.Fatalf("set a: %v", err)
	}
	if _, err := st.SetModelSettings(ctx, b, 0, docB); err != nil {
		t.Fatalf("set b: %v", err)
	}
	// Both tenants start their own revision sequence: the counters are per
	// tenant, and neither read can see the other's document.
	if _, revision, _, found, err := st.GetModelSettings(ctx, b); err != nil || !found || revision != 1 {
		t.Fatalf("tenant b = (revision %d, found %v, err %v), want its own revision 1", revision, found, err)
	}
	gotA, _, _, _, err := st.GetModelSettings(ctx, a)
	if err != nil {
		t.Fatalf("get a: %v", err)
	}
	gotB, _, _, _, err := st.GetModelSettings(ctx, b)
	if err != nil {
		t.Fatalf("get b: %v", err)
	}
	if !reflect.DeepEqual(decodeRawJSON(t, gotA), decodeRawJSON(t, docA)) ||
		!reflect.DeepEqual(decodeRawJSON(t, gotB), decodeRawJSON(t, docB)) {
		t.Fatalf("documents leaked across tenants: a=%s b=%s", gotA, gotB)
	}
	// Clearing b leaves a untouched.
	if _, err := st.DeleteModelSettings(ctx, b, 0); err != nil {
		t.Fatalf("delete b: %v", err)
	}
	if _, _, _, found, err := st.GetModelSettings(ctx, b); err != nil || found {
		t.Fatalf("tenant b after reset: found %v err %v, want no override", found, err)
	}
	if _, _, _, found, err := st.GetModelSettings(ctx, a); err != nil || !found {
		t.Fatalf("tenant a after b's reset: found %v err %v, want its override", found, err)
	}
	if history := readModelSettingsHistory(t, st, a); len(history) != 1 || history[0].Revision != 1 {
		t.Fatalf("tenant a history = %+v, want one row", history)
	}
}

// TestModelSettingsExpectedRevisionGuard covers both guard outcomes: a stale
// expectation is refused with the revision that won, and two concurrent writers
// holding the same expectation cannot both land.
func TestModelSettingsExpectedRevisionGuard(t *testing.T) {
	st := tenancyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	doc := json.RawMessage(`{"selection":{"providerId":"s2api","modelId":"m"}}`)

	if _, err := st.SetModelSettings(ctx, tenant, 0, doc); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.SetModelSettings(ctx, tenant, 7, doc); err == nil {
		t.Fatal("a stale expectedRevision must be refused")
	} else {
		var conflict *ModelSettingsConflictError
		if !errors.As(err, &conflict) || conflict.Current != 1 {
			t.Fatalf("stale guard err = %v, want a conflict carrying revision 1", err)
		}
		if !errors.Is(err, ErrModelSettingsConflict) {
			t.Fatalf("conflict must match the sentinel: %v", err)
		}
	}
	// The refused write changed nothing.
	if _, revision, _, _, err := st.GetModelSettings(ctx, tenant); err != nil || revision != 1 {
		t.Fatalf("revision after refusal = %d (err %v), want 1", revision, err)
	}
	// A matching expectation lands.
	if revision, err := st.SetModelSettings(ctx, tenant, 1, doc); err != nil || revision != 2 {
		t.Fatalf("matching guard = (%d, %v), want revision 2", revision, err)
	}
	// A reset guard works the same way.
	if _, err := st.DeleteModelSettings(ctx, tenant, 1); !errors.Is(err, ErrModelSettingsConflict) {
		t.Fatalf("stale reset guard err = %v, want conflict", err)
	}
	if revision, err := st.DeleteModelSettings(ctx, tenant, 2); err != nil || revision != 3 {
		t.Fatalf("reset with matching guard = (%d, %v), want revision 3", revision, err)
	}

	// Concurrent writers holding the same expectation: exactly one wins.
	tenant2 := uuid.New()
	if _, err := st.SetModelSettings(ctx, tenant2, 0, doc); err != nil {
		t.Fatalf("seed tenant2: %v", err)
	}
	const writers = 4
	var wg sync.WaitGroup
	results := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.SetModelSettings(ctx, tenant2, 1, doc)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrModelSettingsConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent write error: %v", err)
		}
	}
	if wins != 1 || conflicts != writers-1 {
		t.Fatalf("concurrent guard: %d wins, %d conflicts, want 1 and %d", wins, conflicts, writers-1)
	}
	if _, revision, _, _, err := st.GetModelSettings(ctx, tenant2); err != nil || revision != 2 {
		t.Fatalf("revision after the concurrent round = %d (err %v), want 2", revision, err)
	}
	if history := readModelSettingsHistory(t, st, tenant2); len(history) != 2 {
		t.Fatalf("concurrent history rows = %d, want 2 (the seed and the one winner)", len(history))
	}
}
