package harnessapi

// The external-run projection route's wire contract (change keel-oar-renderer):
// GET /v1/sessions/{id}/external/projection. The six cells the change names
// are covered here against a real PostgreSQL (KEEL_TEST_DSN) and a temporary
// journal directory:
//
//   - records:   200 + the exact keel.oar.render-projection/1 document;
//   - empty:     missing and empty journals answer the same-schema empty document;
//   - corrupt:   500 projection_corrupt (never silently swallowed);
//   - native:    404 not_external_session (no empty projection pretending a run);
//   - unwired:   501 oar_not_wired (the route is always registered);
//   - cross-tenant: 404 with no record content.
//
// The route is read-only; nothing in this file asserts a write because the
// handler has no write seam. The task-graph non-pollution proof is separate
// (external_projection_taskgraph_test.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/oar"
	"github.com/jixoai/keel/internal/store"
)

type externalProjectionEnv struct {
	t       *testing.T
	st      *store.Store
	tenant  uuid.UUID
	session uuid.UUID
	// ref is the backend session handle stored in sessions.external_ref and
	// used as the journal file name (session-<ref>.ndjson).
	ref     string
	journal string
}

func newExternalProjectionEnv(t *testing.T) *externalProjectionEnv {
	t.Helper()
	st := openTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "oarproj-"+tenant.String()[:8]); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	session := uuid.New()
	const ref = "oar-ext-1"
	if _, err := st.CreateExternalSession(ctx, tenant, session, store.ExternalMarks{
		Backend: "oar",
		Ref:     ref,
	}); err != nil {
		t.Fatalf("create external session: %v", err)
	}
	env := &externalProjectionEnv{t: t, st: st, tenant: tenant, session: session, ref: ref, journal: t.TempDir()}
	// The journal file name is the contract the route resolves from
	// sessions.external_ref; pin it once here so a read-side drift is loud.
	if name := filepath.Base(oar.JournalPath(env.journal, ref)); name != "session-"+ref+".ndjson" {
		t.Fatalf("journal file name = %q", name)
	}
	return env
}

// externalProjectionRecords is one fake-runtime stream: a status pair, two
// folded calls (root + child), a message and usage. It carries the out-of-band
// attribution the host sets at session.open.
func externalProjectionRecords(hostSession, backendRef, taskID string) []oar.Record {
	timestamp := func(seq int64) time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Millisecond)
	}
	attribution := oar.Attribution{SessionID: hostSession, TaskID: taskID}
	base := func(seq int64, kind oar.RecordKind, path string) oar.Record {
		return oar.Record{
			Schema:      oar.RecordSchema,
			Seq:         seq,
			AgentPath:   path,
			SessionID:   backendRef,
			TurnID:      "turn-1",
			Kind:        kind,
			Runtime:     oar.RuntimeFake,
			Attribution: attribution,
			Timestamp:   timestamp(seq),
		}
	}
	start := base(1, oar.KindStatus, oar.RootAgentPath)
	start.Name = "turn.started"
	start.Status = oar.StatusStarted
	start.Message = "turn started"
	call := base(2, oar.KindCall, oar.RootAgentPath)
	call.CallID = "call-1"
	call.Name = "shell"
	call.Status = oar.StatusStarted
	call.Input = json.RawMessage(`{"command":"printf OAR-OK"}`)
	result := base(3, oar.KindResult, oar.RootAgentPath)
	result.CallID = "call-1"
	result.Name = "shell"
	result.Status = oar.StatusCompleted
	result.Output = json.RawMessage(`{"stdout":"OAR-OK","exitCode":0}`)
	childCall := base(4, oar.KindCall, "/main/worker-1")
	childCall.CallID = "call-2"
	childCall.Name = "read_file"
	childCall.Status = oar.StatusStarted
	childCall.Input = json.RawMessage(`{"path":"README.md"}`)
	childResult := base(5, oar.KindResult, "/main/worker-1")
	childResult.CallID = "call-2"
	childResult.Name = "read_file"
	childResult.Status = oar.StatusCompleted
	childResult.Output = json.RawMessage(`{"bytes":42}`)
	message := base(6, oar.KindMessage, oar.RootAgentPath)
	message.Name = "agent_message"
	message.Message = "fake answer to: inspect the repo"
	usage := base(7, oar.KindUsage, oar.RootAgentPath)
	usage.Name = "turn.usage"
	usage.Usage = &oar.RecordUsage{Metering: "external", Model: "fake-model", InputTokens: 12, OutputTokens: 5, TotalTokens: 17}
	finish := base(8, oar.KindStatus, oar.RootAgentPath)
	finish.Name = "turn.finished"
	finish.Status = oar.StatusCompleted
	finish.Message = "turn completed"
	return []oar.Record{start, call, result, childCall, childResult, message, usage, finish}
}

// writeExternalProjectionJournal writes records through the real Journal API
// (the host's writer) and returns the file path.
func writeExternalProjectionJournal(t *testing.T, dir, ref string, records []oar.Record) string {
	t.Helper()
	journal, err := oar.NewJournal(dir, oar.JournalOptions{NoSync: true})
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	for _, record := range records {
		if _, err := journal.Append(record); err != nil {
			t.Fatalf("append record seq %d: %v", record.Seq, err)
		}
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	path := oar.JournalPath(dir, ref)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("journal file: %v", err)
	}
	return path
}

func newExternalProjectionServer(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = testLogger()
	}
	return settingsServer(t, func(c *Config) { *c = cfg })
}

func getExternalProjection(t *testing.T, server *httptest.Server, tenant uuid.UUID, sessionID string) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/sessions/"+sessionID+"/external/projection", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("X-Tenant-ID", tenant.String())
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response, body
}

// transcriptExternalProjection logs the real request/response pair and, when
// KEEL_EVIDENCE_DIR is set, appends it to the change's api-contract evidence.
func transcriptExternalProjection(t *testing.T, label string, tenant uuid.UUID, sessionID string, response *http.Response, body []byte) {
	t.Helper()
	entry := fmt.Sprintf("=== %s\nGET /v1/sessions/%s/external/projection\nX-Tenant-ID: %s\n\nHTTP %d\nContent-Type: %s\nCache-Control: %s\n\n%s\n\n",
		label, sessionID, tenant, response.StatusCode,
		response.Header.Get("Content-Type"), response.Header.Get("Cache-Control"), strings.TrimRight(string(body), "\n"))
	t.Logf("\n%s", entry)
	dir := strings.TrimSpace(os.Getenv("KEEL_EVIDENCE_DIR"))
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create evidence dir: %v", err)
	}
	file, err := os.OpenFile(filepath.Join(dir, "api-contract.txt"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open evidence file: %v", err)
	}
	defer file.Close()
	if _, err := file.WriteString(entry); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
}

func projectionErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var payload struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("error body is not JSON (%v): %s", err, body)
	}
	return payload.Code
}

func TestExternalProjectionRouteWithRecords(t *testing.T) {
	env := newExternalProjectionEnv(t)
	records := externalProjectionRecords(env.session.String(), env.ref, "task-card-oar-1")
	writeExternalProjectionJournal(t, env.journal, env.ref, records)
	server := newExternalProjectionServer(t, Config{ExternalSessions: env.st, OARJournalDir: env.journal})

	response, body := getExternalProjection(t, server, env.tenant, env.session.String())
	transcriptExternalProjection(t, "with-records (200)", env.tenant, env.session.String(), response, body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	var projection oar.Projection
	if err := json.Unmarshal(body, &projection); err != nil {
		t.Fatalf("projection body: %v", err)
	}
	if projection.Schema != oar.ProjectionSchema {
		t.Fatalf("schema = %q", projection.Schema)
	}
	if projection.BackendSessionID != env.ref || projection.HostSessionID != env.session.String() || projection.TaskID != "task-card-oar-1" {
		t.Fatalf("projection identity = %+v", projection)
	}
	if projection.Runtime != oar.RuntimeFake || !projection.Simulated {
		t.Fatalf("runtime/simulated = %q/%v", projection.Runtime, projection.Simulated)
	}
	if len(projection.Agents) != 2 {
		t.Fatalf("agents = %+v", projection.Agents)
	}
	if projection.Agents[0].AgentPath != oar.RootAgentPath || projection.Agents[0].Calls != 1 || projection.Agents[0].Status != oar.StatusCompleted {
		t.Fatalf("root agent rollup = %+v", projection.Agents[0])
	}
	if projection.Agents[1].AgentPath != "/main/worker-1" || projection.Agents[1].Calls != 1 {
		t.Fatalf("child agent rollup = %+v", projection.Agents[1])
	}
	// The call and its result are one folded item, keeping the call's seq.
	var folded *oar.ProjectionItem
	for index := range projection.Items {
		item := &projection.Items[index]
		if item.Kind == oar.KindCall && item.CallID == "call-1" {
			folded = item
		}
	}
	if folded == nil || folded.Seq != 2 || folded.ResultSeq != 3 || folded.State != oar.StatusCompleted {
		t.Fatalf("folded call = %+v", folded)
	}
	if folded.Output == nil || !bytes.Contains(folded.Output, []byte("OAR-OK")) {
		t.Fatalf("folded output = %s", folded.Output)
	}
	// The route serves exactly the fold document, byte for byte.
	want, err := oar.ProjectionJSON(oar.BuildProjection(records))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("route body is not oar.ProjectionJSON(BuildProjection(records)):\n got: %s\nwant: %s", body, want)
	}
}

func TestExternalProjectionRouteEmptyJournal(t *testing.T) {
	env := newExternalProjectionEnv(t)
	server := newExternalProjectionServer(t, Config{ExternalSessions: env.st, OARJournalDir: env.journal})

	// A fresh external session has no journal file yet: that is an empty
	// projection, not a 404 (the session exists) and not a 500.
	missingResponse, missingBody := getExternalProjection(t, server, env.tenant, env.session.String())
	transcriptExternalProjection(t, "empty-journal-missing-file (200)", env.tenant, env.session.String(), missingResponse, missingBody)
	if missingResponse.StatusCode != http.StatusOK {
		t.Fatalf("missing journal status = %d, body = %s", missingResponse.StatusCode, missingBody)
	}
	emptyPath := filepath.Join(env.journal, "session-"+env.ref+".ndjson")
	if err := os.WriteFile(emptyPath, nil, 0o644); err != nil {
		t.Fatalf("write empty journal: %v", err)
	}
	emptyResponse, emptyBody := getExternalProjection(t, server, env.tenant, env.session.String())
	transcriptExternalProjection(t, "empty-journal-empty-file (200)", env.tenant, env.session.String(), emptyResponse, emptyBody)
	if emptyResponse.StatusCode != http.StatusOK {
		t.Fatalf("empty journal status = %d, body = %s", emptyResponse.StatusCode, emptyBody)
	}
	if !bytes.Equal(missingBody, emptyBody) {
		t.Fatalf("missing and empty journals answer different documents:\n%s\n%s", missingBody, emptyBody)
	}
	var projection oar.Projection
	if err := json.Unmarshal(emptyBody, &projection); err != nil {
		t.Fatalf("empty projection body: %v", err)
	}
	if projection.Schema != oar.ProjectionSchema || projection.Items == nil || projection.Agents == nil {
		t.Fatalf("empty projection = %+v (items/agents must be empty arrays, not null)", projection)
	}
	if len(projection.Items) != 0 || len(projection.Agents) != 0 {
		t.Fatalf("empty projection has rows: %+v", projection)
	}
	for _, fragment := range []string{`"items": []`, `"agents": []`} {
		if !bytes.Contains(emptyBody, []byte(fragment)) {
			t.Fatalf("empty projection lacks %s: %s", fragment, emptyBody)
		}
	}
}

func TestExternalProjectionRouteCorruptJournal(t *testing.T) {
	env := newExternalProjectionEnv(t)
	path := filepath.Join(env.journal, "session-"+env.ref+".ndjson")
	if err := os.WriteFile(path, []byte("{\"schema\":\"keel.oar.record/1\",\"seq\":1,\n"), 0o644); err != nil {
		t.Fatalf("write corrupt journal: %v", err)
	}
	server := newExternalProjectionServer(t, Config{ExternalSessions: env.st, OARJournalDir: env.journal})

	response, body := getExternalProjection(t, server, env.tenant, env.session.String())
	transcriptExternalProjection(t, "corrupt-journal (500)", env.tenant, env.session.String(), response, body)
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	if code := projectionErrorCode(t, body); code != ExternalProjectionCorrupt {
		t.Fatalf("code = %q, body = %s", code, body)
	}
	if bytes.Contains(body, []byte(env.journal)) || bytes.Contains(body, []byte("projection\"")) {
		t.Fatalf("corrupt answer leaks the journal path or a partial projection: %s", body)
	}
}

func TestExternalProjectionRouteNonExternalSession(t *testing.T) {
	env := newExternalProjectionEnv(t)
	ctx := context.Background()
	server := newExternalProjectionServer(t, Config{ExternalSessions: env.st, OARJournalDir: env.journal})

	native := uuid.New()
	if err := env.st.CreateSession(ctx, env.tenant, native); err != nil {
		t.Fatalf("create native session: %v", err)
	}
	response, body := getExternalProjection(t, server, env.tenant, native.String())
	transcriptExternalProjection(t, "native-session (404)", env.tenant, native.String(), response, body)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("native session status = %d, body = %s", response.StatusCode, body)
	}
	if code := projectionErrorCode(t, body); code != ExternalProjectionNotExternalSession {
		t.Fatalf("native session code = %q, body = %s", code, body)
	}
	if bytes.Contains(body, []byte(oar.ProjectionSchema)) {
		t.Fatalf("native session answered a projection: %s", body)
	}

	// An external-marked row without a backend handle cannot locate a journal:
	// it is the same typed 404, never an empty projection.
	handleless := uuid.New()
	if _, err := env.st.CreateExternalSession(ctx, env.tenant, handleless, store.ExternalMarks{Backend: "oar"}); err != nil {
		t.Fatalf("create handleless external session: %v", err)
	}
	response, body = getExternalProjection(t, server, env.tenant, handleless.String())
	transcriptExternalProjection(t, "external-without-ref (404)", env.tenant, handleless.String(), response, body)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("handleless external status = %d, body = %s", response.StatusCode, body)
	}
	if code := projectionErrorCode(t, body); code != ExternalProjectionNotExternalSession {
		t.Fatalf("handleless external code = %q, body = %s", code, body)
	}

	// A session that does not exist answers the product API's own 404.
	missing := uuid.New()
	response, body = getExternalProjection(t, server, env.tenant, missing.String())
	transcriptExternalProjection(t, "missing-session (404)", env.tenant, missing.String(), response, body)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing session status = %d, body = %s", response.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(ErrSessionNotFound.Error())) {
		t.Fatalf("missing session body = %s", body)
	}
}

func TestExternalProjectionRouteNotWired(t *testing.T) {
	env := newExternalProjectionEnv(t)
	writeExternalProjectionJournal(t, env.journal, env.ref, externalProjectionRecords(env.session.String(), env.ref, "task-card-oar-1"))

	// Registered but no journal directory: 501 with the typed code, never an
	// empty projection pretending the capability exists.
	unwired := newExternalProjectionServer(t, Config{ExternalSessions: env.st})
	response, body := getExternalProjection(t, unwired, env.tenant, env.session.String())
	transcriptExternalProjection(t, "unwired-journal-dir (501)", env.tenant, env.session.String(), response, body)
	if response.StatusCode != http.StatusNotImplemented {
		t.Fatalf("unwired status = %d, body = %s", response.StatusCode, body)
	}
	if code := projectionErrorCode(t, body); code != ExternalProjectionNotWired {
		t.Fatalf("unwired code = %q, body = %s", code, body)
	}
	if bytes.Contains(body, []byte(oar.ProjectionSchema)) {
		t.Fatalf("unwired deployment answered a projection: %s", body)
	}

	// A deployment without the external-marks seam is the same capability gap.
	noSeam := newExternalProjectionServer(t, Config{OARJournalDir: env.journal})
	response, body = getExternalProjection(t, noSeam, env.tenant, env.session.String())
	transcriptExternalProjection(t, "unwired-session-seam (501)", env.tenant, env.session.String(), response, body)
	if response.StatusCode != http.StatusNotImplemented {
		t.Fatalf("no-seam status = %d, body = %s", response.StatusCode, body)
	}
	if code := projectionErrorCode(t, body); code != ExternalProjectionNotWired {
		t.Fatalf("no-seam code = %q, body = %s", code, body)
	}
}

func TestExternalProjectionRouteCrossTenant(t *testing.T) {
	env := newExternalProjectionEnv(t)
	records := externalProjectionRecords(env.session.String(), env.ref, "task-card-oar-1")
	writeExternalProjectionJournal(t, env.journal, env.ref, records)
	server := newExternalProjectionServer(t, Config{ExternalSessions: env.st, OARJournalDir: env.journal})
	ctx := context.Background()
	otherTenant := uuid.New()
	if err := env.st.CreateTenant(ctx, otherTenant, "oarproj-other-"+otherTenant.String()[:8]); err != nil {
		t.Fatalf("create other tenant: %v", err)
	}

	response, body := getExternalProjection(t, server, otherTenant, env.session.String())
	transcriptExternalProjection(t, "cross-tenant (404)", otherTenant, env.session.String(), response, body)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant status = %d, body = %s", response.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(ErrSessionNotFound.Error())) {
		t.Fatalf("cross-tenant body = %s", body)
	}
	for _, leaked := range []string{oar.ProjectionSchema, env.ref, "task-card-oar-1", "OAR-OK"} {
		if bytes.Contains(body, []byte(leaked)) {
			t.Fatalf("cross-tenant answer leaks %q: %s", leaked, body)
		}
	}

	// A foreign tenant asking for a random id gets the same answer.
	foreign := uuid.New()
	response, body = getExternalProjection(t, server, otherTenant, foreign.String())
	transcriptExternalProjection(t, "foreign-missing (404)", otherTenant, foreign.String(), response, body)
	if response.StatusCode != http.StatusNotFound || !bytes.Contains(body, []byte(ErrSessionNotFound.Error())) {
		t.Fatalf("foreign missing status = %d, body = %s", response.StatusCode, body)
	}
}
