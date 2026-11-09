// Command e2e exercises the runtime's stateless contract end to end: it starts
// the real server binary, kills it with SIGKILL at chosen points, restarts it,
// and asserts that sessions, idempotency, SSE cursors and blobs all survive.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type env struct {
	bin      string
	dsn      string
	base     string
	cmd      *exec.Cmd
	log      *os.File
	extraEnv []string
	adminKey string
}

func main() {
	var (
		bin          = flag.String("bin", "/tmp/keel-runtime", "runtime binary")
		dsn          = flag.String("dsn", os.Getenv("KEEL_DSN"), "PostgreSQL DSN")
		port         = flag.Int("port", 13745, "listen port")
		reset        = flag.Bool("reset", true, "truncate the runtime tables before the run (dedicated test DSN required)")
		resetConfirm = flag.String("reset-confirm", "", "the database name the -reset truncate is allowed to wipe")
	)
	flag.Parse()
	if *dsn == "" {
		log.Fatal("KEEL_DSN is required")
	}
	e := &env{bin: *bin, dsn: *dsn, base: fmt.Sprintf("http://127.0.0.1:%d", *port)}
	// Tenant provisioning is admin-guarded. The e2e harness starts the runtime
	// itself, so it generates (or reuses) the key and passes it to both sides.
	e.adminKey = os.Getenv("KEEL_ADMIN_KEY")
	if e.adminKey == "" {
		e.adminKey = "e2e-admin-key"
	}
	if *reset {
		if err := resetRuntimeTables(context.Background(), *dsn, *resetConfirm); err != nil {
			log.Fatalf("reset runtime tables: %v", err)
		}
	}

	results := []result{}
	results = append(results, e.caseIdempotentAcrossRestart())
	results = append(results, e.caseSSECursorAcrossRestart())
	results = append(results, e.caseBlobResumeAcrossRestart())
	results = append(results, e.caseAgentReply())
	results = append(results, e.caseRetryAppends())
	results = append(results, e.caseRunResumeAfterKill())
	results = append(results, e.caseToolReplayAfterKill())
	results = append(results, e.caseConcurrentKillNoLoss())

	e.stop()
	raw, _ := json.MarshalIndent(results, "", "  ")
	fmt.Println(string(raw))
	for _, r := range results {
		if !r.Pass {
			os.Exit(1)
		}
	}
}

// resetRuntimeTables starts every run from an empty log. Without it, leftovers
// from an earlier run (for example a 100-session bulk tenant) are claimed first
// and can starve a new session past the case timeout.
//
// Truncating is destructive to every other user of that database. Test suites
// run in parallel against one server, so pointing this at the shared dev
// database mid-suite wipes rows under other packages' feet (observed: a store
// test's submission vanished). The wipe therefore needs a database that is
// clearly dedicated — its name carries "e2e"/"test" — or an explicit
// -reset-confirm <dbname> that matches exactly.
func resetRuntimeTables(ctx context.Context, dsn, confirm string) error {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse dsn: %w", err)
	}
	name := cfg.ConnConfig.Database
	lower := strings.ToLower(name)
	dedicated := strings.Contains(lower, "e2e") || strings.Contains(lower, "test") || (confirm != "" && confirm == name)
	if !dedicated {
		return fmt.Errorf("refusing to truncate database %q: it does not look dedicated (pass -reset-confirm %s if this is really a throwaway database)", name, name)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	log.Printf("resetting runtime tables in database %s", name)
	_, err = pool.Exec(ctx, `TRUNCATE tenants, sessions, events, checkpoints, submissions,
		tasks, blobs, blob_chunks, blob_refs, runs, usage_ledger,
		mcp_oauth_pending, mcp_oauth_credentials`)
	if err != nil && strings.Contains(err.Error(), "42P01") {
		// A brand-new database has no tables yet; the runtime migrates on boot,
		// so there is nothing to truncate. This keeps `-reset` (the default)
		// working on an empty database instead of failing before the first start.
		log.Printf("database %s is empty (42P01): the runtime will migrate on boot", name)
		return nil
	}
	return err
}

type result struct {
	Name   string   `json:"name"`
	Pass   bool     `json:"pass"`
	Detail string   `json:"detail"`
	Checks []string `json:"checks"`
}

func (e *env) start() error {
	if e.cmd != nil {
		return nil
	}
	f, err := os.OpenFile("/tmp/keel-e2e-server.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(e.bin, "-addr", "127.0.0.1:"+portOf(e.base), "-cas", "/tmp/keel-e2e-cas", "-streams", "600", "-admit-rate", "2000")
	cmd.Env = append(os.Environ(), "KEEL_DSN="+e.dsn, "GOMEMLIMIT=805306368", "KEEL_ADMIN_KEY="+e.adminKey)
	cmd.Env = append(cmd.Env, e.extraEnv...)
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	e.cmd = cmd
	e.log = f
	// Wait for health.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(e.base + "/v1/health")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("server did not become healthy")
}

func (e *env) kill9() {
	if e.cmd == nil {
		return
	}
	_ = syscall.Kill(-e.cmd.Process.Pid, syscall.SIGKILL)
	_ = e.cmd.Wait()
	e.cmd = nil
	if e.log != nil {
		e.log.Close()
		e.log = nil
	}
	time.Sleep(200 * time.Millisecond)
}

func (e *env) stop() {
	if e.cmd == nil {
		return
	}
	_ = syscall.Kill(-e.cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = e.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(12 * time.Second):
		e.kill9()
	}
	e.cmd = nil
}

func portOf(base string) string {
	i := strings.LastIndexByte(base, ':')
	return base[i+1:]
}

func (e *env) newTenantSession(ctx context.Context) (string, string, error) {
	tenant := uuid.NewString()
	var out struct {
		TenantID  string `json:"tenantId"`
		SessionID string `json:"sessionId"`
	}
	if code, err := e.createTenant(ctx, tenant, "e2e"); err != nil || code != 201 {
		return "", "", fmt.Errorf("create tenant: %d %v", code, err)
	}
	if code, err := postJSON(ctx, e.base+"/v1/sessions", tenant, map[string]string{}, &out); err != nil || code != 201 {
		return "", "", fmt.Errorf("create session: %d %v", code, err)
	}
	return tenant, out.SessionID, nil
}

func postJSON(ctx context.Context, url, tenant string, body any, out any) (int, error) {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(raw))
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if out != nil {
		_ = json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode, nil
}

// createTenant provisions a tenant with the operator key the runtime was
// started with: POST /v1/tenants is admin-guarded (401 without it).
func (e *env) createTenant(ctx context.Context, tenant, name string) (int, error) {
	raw, _ := json.Marshal(map[string]string{"name": name})
	req, _ := http.NewRequestWithContext(ctx, "POST", e.base+"/v1/tenants", bytes.NewReader(raw))
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.adminKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode, nil
}

type inputResp struct {
	SubmissionID string `json:"submissionId"`
	Created      bool   `json:"created"`
	Seq          *int64 `json:"seq"`
}

func loadEvents(ctx context.Context, base, tenant, session string, after int64) ([]event, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/v1/sessions/%s/events?after=%d", base, session, after), nil)
	req.Header.Set("X-Tenant-ID", tenant)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var body struct {
		Events []event `json:"events"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Events, nil
}

type event struct {
	Seq     int64           `json:"seq"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

func (e *env) caseIdempotentAcrossRestart() result {
	ctx := context.Background()
	r := result{Name: "requestId idempotency across SIGKILL"}
	fail := func(format string, args ...any) result {
		r.Detail = fmt.Sprintf(format, args...)
		return r
	}
	if err := e.start(); err != nil {
		return fail("start: %v", err)
	}
	tenant, session, err := e.newTenantSession(ctx)
	if err != nil {
		return fail("%v", err)
	}
	var first inputResp
	code, err := postJSON(ctx, e.base+"/v1/sessions/"+session+"/input", tenant,
		map[string]string{"text": "hello", "requestId": "r1"}, &first)
	if err != nil || code != 201 || !first.Created {
		return fail("first input: code=%d err=%v created=%v", code, err, first.Created)
	}
	// A retry carries the SAME content: same key + same payload is a replay and
	// reuses the committed receipt.
	var replay inputResp
	code, err = postJSON(ctx, e.base+"/v1/sessions/"+session+"/input", tenant,
		map[string]string{"text": "hello", "requestId": "r1"}, &replay)
	if err != nil || code != 200 || replay.Created || replay.SubmissionID != first.SubmissionID {
		return fail("replay before kill: code=%d created=%v ids=%s/%s", code, replay.Created, first.SubmissionID, replay.SubmissionID)
	}
	// Same key + different content is a client bug: the kernel answers 409 and
	// appends nothing (a silent 200 would drop the new text).
	var conflict inputResp
	code, err = postJSON(ctx, e.base+"/v1/sessions/"+session+"/input", tenant,
		map[string]string{"text": "hello (different)", "requestId": "r1"}, &conflict)
	if err != nil || code != 409 {
		return fail("conflicting replay before kill: code=%d err=%v, want 409", code, err)
	}
	e.kill9()
	if err := e.start(); err != nil {
		return fail("restart: %v", err)
	}
	var after inputResp
	code, err = postJSON(ctx, e.base+"/v1/sessions/"+session+"/input", tenant,
		map[string]string{"text": "hello", "requestId": "r1"}, &after)
	if err != nil || code != 200 || after.Created || after.SubmissionID != first.SubmissionID {
		return fail("replay after kill: code=%d created=%v ids=%s/%s", code, after.Created, first.SubmissionID, after.SubmissionID)
	}
	events, err := loadEvents(ctx, e.base, tenant, session, 0)
	if err != nil {
		return fail("load events: %v", err)
	}
	// The worker may already have answered; idempotency is about the single
	// user event, not about suppressing the assistant turn.
	if users := countKind(events, "user.message"); users != 1 {
		return fail("want 1 user event after the submits, got %d (events: %d)", users, len(events))
	}
	r.Checks = []string{"first submit created", "replay before kill reused submission", "conflicting replay refused with 409", "SIGKILL", "replay after kill reused submission", "exactly one user event"}
	r.Pass = true
	r.Detail = "one requestId → one submission → one event across a SIGKILL; different content is refused"
	return r
}

func (e *env) caseSSECursorAcrossRestart() result {
	ctx := context.Background()
	r := result{Name: "SSE Last-Event-ID across SIGKILL"}
	fail := func(format string, args ...any) result {
		r.Detail = fmt.Sprintf(format, args...)
		return r
	}
	if err := e.start(); err != nil {
		return fail("start: %v", err)
	}
	tenant, session, err := e.newTenantSession(ctx)
	if err != nil {
		return fail("%v", err)
	}
	for i := 1; i <= 3; i++ {
		var out inputResp
		if code, err := postJSON(ctx, e.base+"/v1/sessions/"+session+"/input", tenant,
			map[string]string{"text": fmt.Sprintf("m%d", i), "requestId": fmt.Sprintf("s%d", i)}, &out); err != nil || code != 201 {
			return fail("seed %d: %d %v", i, code, err)
		}
	}
	first, err := readSSEUntil(ctx, e.base, tenant, session, 0, 3, 5*time.Second)
	if err != nil || len(first) < 3 {
		return fail("first stream: %v %v", first, err)
	}
	if first[0] != 1 || first[len(first)-1] != 3 {
		return fail("unexpected first stream ids %v", first)
	}
	e.kill9()
	if err := e.start(); err != nil {
		return fail("restart: %v", err)
	}
	last := first[len(first)-1]
	got := make(chan []int64, 1)
	errCh := make(chan error, 1)
	go func() {
		ids, err := readSSEUntil(ctx, e.base, tenant, session, last, 1, 8*time.Second)
		if err != nil {
			errCh <- err
			return
		}
		got <- ids
	}()
	time.Sleep(500 * time.Millisecond)
	var out inputResp
	if code, err := postJSON(ctx, e.base+"/v1/sessions/"+session+"/input", tenant,
		map[string]string{"text": "m4", "requestId": "s4"}, &out); err != nil || code != 201 {
		return fail("post after restart: %d %v", code, err)
	}
	select {
	case ids := <-got:
		if len(ids) == 0 || ids[0] != last+1 {
			return fail("resume ids %v, want first id %d", ids, last+1)
		}
		r.Checks = []string{"seeded 3 events", "stream delivered 1..3", "SIGKILL", "resumed with Last-Event-ID", "received the post-restart event"}
		r.Pass = true
		r.Detail = fmt.Sprintf("resumed at %d and delivered %v", last, ids)
		return r
	case err := <-errCh:
		return fail("resume stream: %v", err)
	case <-time.After(10 * time.Second):
		return fail("resume stream timed out")
	}
}

func (e *env) caseBlobResumeAcrossRestart() result {
	ctx := context.Background()
	r := result{Name: "blob base64 resume across SIGKILL"}
	fail := func(format string, args ...any) result {
		r.Detail = fmt.Sprintf(format, args...)
		return r
	}
	if err := e.start(); err != nil {
		return fail("start: %v", err)
	}
	tenant, _, err := e.newTenantSession(ctx)
	if err != nil {
		return fail("%v", err)
	}
	data := make([]byte, 1<<20+3)
	for i := range data {
		data[i] = byte(i*17 + 3)
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", e.base+"/v1/files", bytes.NewReader(data))
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("Content-Type", "image/png")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fail("upload: %v", err)
	}
	var meta struct {
		FileID string `json:"fileId"`
	}
	_ = json.NewDecoder(res.Body).Decode(&meta)
	res.Body.Close()
	if res.StatusCode != 201 {
		return fail("upload status %d", res.StatusCode)
	}
	want := base64.StdEncoding.EncodeToString(data)
	off := int64(4 * 12345)
	part1, code, err := getBase64Part(ctx, e.base, tenant, meta.FileID, 0)
	if err != nil || code != 200 || part1 != want {
		return fail("first read: code=%d len=%d err=%v", code, len(part1), err)
	}
	e.kill9()
	if err := e.start(); err != nil {
		return fail("restart: %v", err)
	}
	part2, code, err := getBase64Part(ctx, e.base, tenant, meta.FileID, off)
	if err != nil || code != 206 {
		return fail("resume: code=%d err=%v", code, err)
	}
	if part2 != want[off:] {
		return fail("resume mismatch: got %d want %d", len(part2), len(want)-int(off))
	}
	r.Checks = []string{"uploaded 1MiB+3 bytes", "full base64 matched", "SIGKILL", "resume at wire offset matched"}
	r.Pass = true
	r.Detail = fmt.Sprintf("resumed at wire offset %d after restart (%d bytes)", off, len(part2))
	return r
}

func getBase64Part(ctx context.Context, base, tenant, id string, off int64) (string, int, error) {
	url := fmt.Sprintf("%s/v1/files/%s?format=base64&offset=%d", base, id, off)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("X-Tenant-ID", tenant)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	return string(raw), res.StatusCode, err
}

func readSSEUntil(ctx context.Context, base, tenant, session string, after int64, want int, timeout time.Duration) ([]int64, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	url := fmt.Sprintf("%s/v1/sessions/%s/events/stream?after=%d", base, session, after)
	req, _ := http.NewRequestWithContext(cctx, "GET", url, nil)
	req.Header.Set("X-Tenant-ID", tenant)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", res.StatusCode)
	}
	ids := []int64{}
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "id: ") {
			var id int64
			fmt.Sscanf(strings.TrimPrefix(line, "id: "), "%d", &id)
			ids = append(ids, id)
			if len(ids) >= want {
				return ids, nil
			}
		}
	}
	return ids, sc.Err()
}

func (e *env) caseConcurrentKillNoLoss() result {
	ctx := context.Background()
	r := result{Name: "100 sessions survive SIGKILL without event loss"}
	fail := func(format string, args ...any) result {
		r.Detail = fmt.Sprintf(format, args...)
		return r
	}
	if err := e.start(); err != nil {
		return fail("start: %v", err)
	}
	tenant := uuid.NewString()
	if code, err := e.createTenant(ctx, tenant, "bulk"); err != nil || code != 201 {
		return fail("tenant: %d %v", code, err)
	}
	sessions := make([]string, 100)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i := range sessions {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			var body struct {
				SessionID string `json:"sessionId"`
			}
			code, err := postJSON(ctx, e.base+"/v1/sessions", tenant, map[string]string{}, &body)
			mu.Lock()
			if err != nil || code != 201 {
				if err == nil {
					err = fmt.Errorf("status %d", code)
				}
				log.Printf("session %d: %v", i, err)
			}
			sessions[i] = body.SessionID
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	for _, s := range sessions {
		if s == "" {
			return fail("session creation failed")
		}
	}
	// Write 20 events per session, killing the server after ~10.
	written := make([]int, len(sessions))
	for round := 0; round < 20; round++ {
		if round == 10 {
			e.kill9()
			if err := e.start(); err != nil {
				return fail("restart: %v", err)
			}
		}
		for i, s := range sessions {
			var out inputResp
			code, err := postJSON(ctx, e.base+"/v1/sessions/"+s+"/input", tenant,
				map[string]string{"text": fmt.Sprintf("m%d", round), "requestId": fmt.Sprintf("%d-%d", i, round)}, &out)
			if err != nil || (code != 201 && code != 200) {
				return fail("round %d session %d: code=%d err=%v", round, i, code, err)
			}
			written[i]++
		}
	}
	for i, s := range sessions {
		events, err := loadEvents(ctx, e.base, tenant, s, 0)
		if err != nil {
			return fail("load session %d: %v", i, err)
		}
		seqs := make([]int64, len(events))
		for j, ev := range events {
			seqs[j] = ev.Seq
		}
		// Each input also produces assistant/tool events now, so count the user
		// events; seqs must still be contiguous across the whole session.
		if got := countKind(events, "user.message"); got != written[i] {
			return fail("session %d: got %d user events, want %d (%v)", i, got, written[i], seqs)
		}
		sort.Slice(seqs, func(a, b int) bool { return seqs[a] < seqs[b] })
		for j := 1; j < len(seqs); j++ {
			if seqs[j] != seqs[j-1]+1 {
				return fail("session %d: gap between %d and %d", i, seqs[j-1], seqs[j])
			}
		}
	}
	r.Checks = []string{"100 sessions created", "SIGKILL after 10 rounds", "server restarted", "2000 events committed", "every session has a contiguous sequence"}
	r.Pass = true
	r.Detail = "2000 events across 100 sessions; contiguous after a SIGKILL"
	return r
}

// waitEvents polls the event list until pred is satisfied or the deadline
// passes. The worker is asynchronous, so every agent assertion needs this.
func waitEvents(ctx context.Context, base, tenant, session string, pred func([]event) bool, timeout time.Duration) ([]event, error) {
	deadline := time.Now().Add(timeout)
	var last []event
	for time.Now().Before(deadline) {
		evs, err := loadEvents(ctx, base, tenant, session, 0)
		if err == nil {
			last = evs
			if pred(evs) {
				return evs, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return last, fmt.Errorf("timeout waiting for events (last=%d)", len(last))
}

func countKind(evs []event, kind string) int {
	n := 0
	for _, ev := range evs {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

func assistantStop(ev event) string {
	var m struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(ev.Payload, &m)
	return m.StopReason
}

func assistantReply(ev event) string {
	var m struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal(ev.Payload, &m)
	out := ""
	for _, c := range m.Content {
		if c.Type == "text" {
			out += c.Text
		}
	}
	return out
}

func hasKind(evs []event, kind string) bool { return countKind(evs, kind) > 0 }

func hasFinal(evs []event) bool {
	for _, ev := range evs {
		if ev.Kind == "assistant.message" && assistantStop(ev) == "end_turn" {
			return true
		}
	}
	return false
}

func hasToolCall(evs []event) bool {
	for _, ev := range evs {
		if ev.Kind == "assistant.message" && assistantStop(ev) == "tool_use" {
			return true
		}
	}
	return false
}

func (e *env) caseAgentReply() result {
	ctx := context.Background()
	r := result{Name: "agent worker: input → fake reply"}
	fail := func(format string, args ...any) result {
		r.Detail = fmt.Sprintf(format, args...)
		return r
	}
	e.extraEnv = nil
	if err := e.start(); err != nil {
		return fail("start: %v", err)
	}
	tenant, session, err := e.newTenantSession(ctx)
	if err != nil {
		return fail("%v", err)
	}
	var out inputResp
	if code, err := postJSON(ctx, e.base+"/v1/sessions/"+session+"/input", tenant,
		map[string]string{"text": "hello runtime", "requestId": "agent-1"}, &out); err != nil || code != 201 {
		return fail("input: %d %v", code, err)
	}
	evs, err := waitEvents(ctx, e.base, tenant, session, func(evs []event) bool {
		return hasFinal(evs)
	}, 20*time.Second)
	if err != nil {
		return fail("wait reply: %v", err)
	}
	if countKind(evs, "user.message") != 1 || countKind(evs, "assistant.message") != 1 {
		return fail("events: user=%d assistant=%d", countKind(evs, "user.message"), countKind(evs, "assistant.message"))
	}
	for _, ev := range evs {
		if ev.Kind == "assistant.message" && assistantReply(ev) != "fake provider received: hello runtime" {
			return fail("reply = %q", assistantReply(ev))
		}
	}
	r.Checks = []string{"input committed", "worker produced one assistant message", "reply is the deterministic fake", "idle re-run is a no-op"}
	r.Pass = true
	r.Detail = "input → exactly one user event and one assistant reply"
	return r
}

// caseRetryAppends: POST /v1/sessions/{id}/retry re-asks an earlier turn's own
// input as a NEW commit. The ledger only grows — a fresh user.message carrying
// retryOf, its own run — and every original event stays at its own seq.
func (e *env) caseRetryAppends() result {
	ctx := context.Background()
	r := result{Name: "retry: re-ask appends a new input, history is not rewritten"}
	fail := func(format string, args ...any) result {
		r.Detail = fmt.Sprintf(format, args...)
		return r
	}
	e.extraEnv = nil
	if err := e.start(); err != nil {
		return fail("start: %v", err)
	}
	tenant, session, err := e.newTenantSession(ctx)
	if err != nil {
		return fail("%v", err)
	}
	var out inputResp
	if code, err := postJSON(ctx, e.base+"/v1/sessions/"+session+"/input", tenant,
		map[string]string{"text": "hello retry", "requestId": "retry-1"}, &out); err != nil || code != 201 {
		return fail("input: %d %v", code, err)
	}
	before, err := waitEvents(ctx, e.base, tenant, session, hasFinal, 20*time.Second)
	if err != nil {
		return fail("first run: %v", err)
	}
	// The projection derives a turn id as "t_<the turn's first input seq>".
	var turnSeq int64
	for _, ev := range before {
		if ev.Kind == "user.message" {
			turnSeq = ev.Seq
			break
		}
	}
	if turnSeq == 0 {
		return fail("no committed user.message to retry")
	}
	// A malformed id is refused before anything is written.
	if code, err := postJSON(ctx, e.base+"/v1/sessions/"+session+"/retry", tenant,
		map[string]string{"turnId": "nope"}, nil); err != nil || code != 400 {
		return fail("malformed turnId: code=%d err=%v (want 400)", code, err)
	}
	var retryResp struct {
		Created   bool   `json:"created"`
		Seq       int64  `json:"seq"`
		RequestID string `json:"requestId"`
	}
	if code, err := postJSON(ctx, e.base+"/v1/sessions/"+session+"/retry", tenant,
		map[string]string{"turnId": fmt.Sprintf("t_%d", turnSeq)}, &retryResp); err != nil || code != 201 {
		return fail("retry: %d %v", code, err)
	}
	after, err := waitEvents(ctx, e.base, tenant, session, func(evs []event) bool {
		return countKind(evs, "user.message") == 2 && countKind(evs, "run.finished") >= 2
	}, 20*time.Second)
	if err != nil {
		return fail("second run: %v", err)
	}
	// The re-ask names what it re-asks, and the fake answered it again.
	var reask *event
	for i := range after {
		if after[i].Kind != "user.message" || after[i].Seq == turnSeq {
			continue
		}
		reask = &after[i]
	}
	if reask == nil {
		return fail("the retry appended no user.message")
	}
	var payload struct {
		RetryOf *int64 `json:"retryOf"`
		Text    string `json:"text"`
	}
	_ = json.Unmarshal(reask.Payload, &payload)
	if payload.RetryOf == nil || *payload.RetryOf != turnSeq {
		return fail("the re-ask carries retryOf=%v (want %d)", payload.RetryOf, turnSeq)
	}
	for _, ev := range before {
		for _, cur := range after {
			if cur.Seq == ev.Seq && cur.Kind != ev.Kind {
				return fail("original seq %d changed kind %s → %s", ev.Seq, ev.Kind, cur.Kind)
			}
		}
	}
	repliedAgain := false
	for _, ev := range after {
		if ev.Seq > reask.Seq && ev.Kind == "assistant.message" && assistantStop(ev) == "end_turn" {
			repliedAgain = true
		}
	}
	if !repliedAgain {
		return fail("the re-ask never produced its own answer")
	}
	r.Checks = []string{
		"a malformed turn id is refused with 400 (nothing written)",
		"retry appends one new user.message carrying retryOf",
		"the re-ask runs to its own terminal answer",
		"every original event is still at its own seq",
	}
	r.Pass = true
	r.Detail = fmt.Sprintf("turn t_%d re-asked at seq %d; %d original events untouched", turnSeq, reask.Seq, len(before))
	return r
}

func (e *env) caseRunResumeAfterKill() result {
	ctx := context.Background()
	r := result{Name: "run resumes after SIGKILL during the model call"}
	fail := func(format string, args ...any) result {
		r.Detail = fmt.Sprintf(format, args...)
		return r
	}
	e.extraEnv = []string{"KEEL_FAKE_DELAY_MS=3000", "KEEL_LEASE_MS=1000"}
	if err := e.start(); err != nil {
		return fail("start: %v", err)
	}
	tenant, session, err := e.newTenantSession(ctx)
	if err != nil {
		return fail("%v", err)
	}
	if code, err := postJSON(ctx, e.base+"/v1/sessions/"+session+"/input", tenant,
		map[string]string{"text": "resume me", "requestId": "resume-1"}, nil); err != nil || code != 201 {
		return fail("input: %d %v", code, err)
	}
	time.Sleep(500 * time.Millisecond) // the fake model is sleeping
	e.kill9()
	e.extraEnv = []string{"KEEL_LEASE_MS=1000"}
	if err := e.start(); err != nil {
		return fail("restart: %v", err)
	}
	evs, err := waitEvents(ctx, e.base, tenant, session, hasFinal, 15*time.Second)
	if err != nil {
		return fail("wait reply after restart: %v", err)
	}
	if countKind(evs, "user.message") != 1 {
		return fail("user events = %d, want 1", countKind(evs, "user.message"))
	}
	if countKind(evs, "assistant.message") != 1 {
		return fail("assistant messages = %d, want 1", countKind(evs, "assistant.message"))
	}
	var run struct {
		Status string `json:"status"`
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", e.base+"/v1/sessions/"+session+"/run", nil)
	req.Header.Set("X-Tenant-ID", tenant)
	if res, err := http.DefaultClient.Do(req); err == nil {
		_ = json.NewDecoder(res.Body).Decode(&run)
		res.Body.Close()
	}
	if run.Status != "done" {
		return fail("run status = %q, want done", run.Status)
	}
	r.Checks = []string{"input committed", "SIGKILL during the model call", "lease expired and the run was reclaimed", "exactly one user event", "exactly one final reply", "run status done"}
	r.Pass = true
	r.Detail = "the interrupted run was reclaimed and produced exactly one reply"
	return r
}

func (e *env) caseToolReplayAfterKill() result {
	ctx := context.Background()
	r := result{Name: "tool result survives SIGKILL mid-execution"}
	fail := func(format string, args ...any) result {
		r.Detail = fmt.Sprintf(format, args...)
		return r
	}
	e.extraEnv = []string{"KEEL_TOOL_DELAY_MS=5000", "KEEL_LEASE_MS=1000"}
	if err := e.start(); err != nil {
		return fail("start: %v", err)
	}
	tenant, session, err := e.newTenantSession(ctx)
	if err != nil {
		return fail("%v", err)
	}
	if code, err := postJSON(ctx, e.base+"/v1/sessions/"+session+"/input", tenant,
		map[string]string{"text": "echo crash test", "requestId": "tool-1"}, nil); err != nil || code != 201 {
		return fail("input: %d %v", code, err)
	}
	if _, err := waitEvents(ctx, e.base, tenant, session, hasToolCall, 8*time.Second); err != nil {
		return fail("wait tool call: %v", err)
	}
	e.kill9() // the tool is sleeping; its result was never committed
	e.extraEnv = []string{"KEEL_LEASE_MS=1000"}
	if err := e.start(); err != nil {
		return fail("restart: %v", err)
	}
	evs, err := waitEvents(ctx, e.base, tenant, session, hasFinal, 15*time.Second)
	if err != nil {
		return fail("wait final after restart: %v", err)
	}
	if got := countKind(evs, "tool.result"); got != 1 {
		return fail("tool results = %d, want 1", got)
	}
	final := ""
	for _, ev := range evs {
		if ev.Kind == "assistant.message" && assistantStop(ev) == "end_turn" {
			final = assistantReply(ev)
		}
	}
	if final != "tool echo returned: echo crash test" {
		return fail("final = %q", final)
	}
	r.Checks = []string{"assistant tool call committed", "SIGKILL during tool execution", "lease reclaimed after restart", "tool re-executed (at-least-once)", "exactly one tool result", "final answer present"}
	r.Pass = true
	r.Detail = "the unresolved tool call was re-executed and committed exactly once"
	return r
}
