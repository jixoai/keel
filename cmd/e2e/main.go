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
)

type env struct {
	bin  string
	dsn  string
	base string
	cmd  *exec.Cmd
	log  *os.File
}

func main() {
	var (
		bin  = flag.String("bin", "/tmp/keel-runtime", "runtime binary")
		dsn  = flag.String("dsn", os.Getenv("KEEL_DSN"), "PostgreSQL DSN")
		port = flag.Int("port", 13745, "listen port")
	)
	flag.Parse()
	if *dsn == "" {
		log.Fatal("KEEL_DSN is required")
	}
	e := &env{bin: *bin, dsn: *dsn, base: fmt.Sprintf("http://127.0.0.1:%d", *port)}

	results := []result{}
	results = append(results, e.caseIdempotentAcrossRestart())
	results = append(results, e.caseSSECursorAcrossRestart())
	results = append(results, e.caseBlobResumeAcrossRestart())
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
	cmd.Env = append(os.Environ(), "KEEL_DSN="+e.dsn, "GOMEMLIMIT=805306368")
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
	if code, err := postJSON(ctx, e.base+"/v1/tenants", tenant, map[string]string{"name": "e2e"}, &out); err != nil || code != 201 {
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
	var replay inputResp
	code, err = postJSON(ctx, e.base+"/v1/sessions/"+session+"/input", tenant,
		map[string]string{"text": "hello (replayed)", "requestId": "r1"}, &replay)
	if err != nil || code != 200 || replay.Created || replay.SubmissionID != first.SubmissionID {
		return fail("replay before kill: code=%d created=%v ids=%s/%s", code, replay.Created, first.SubmissionID, replay.SubmissionID)
	}
	e.kill9()
	if err := e.start(); err != nil {
		return fail("restart: %v", err)
	}
	var after inputResp
	code, err = postJSON(ctx, e.base+"/v1/sessions/"+session+"/input", tenant,
		map[string]string{"text": "hello (after kill)", "requestId": "r1"}, &after)
	if err != nil || code != 200 || after.Created || after.SubmissionID != first.SubmissionID {
		return fail("replay after kill: code=%d created=%v ids=%s/%s", code, after.Created, first.SubmissionID, after.SubmissionID)
	}
	events, err := loadEvents(ctx, e.base, tenant, session, 0)
	if err != nil {
		return fail("load events: %v", err)
	}
	if len(events) != 1 {
		return fail("want 1 event after three submits, got %d", len(events))
	}
	r.Checks = []string{"first submit created", "replay before kill reused submission", "SIGKILL", "replay after kill reused submission", "exactly one user event"}
	r.Pass = true
	r.Detail = "one requestId → one submission → one event, across a SIGKILL"
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
	if code, err := postJSON(ctx, e.base+"/v1/tenants", tenant, map[string]string{"name": "bulk"}, nil); err != nil || code != 201 {
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
		if len(events) != written[i] {
			return fail("session %d: got %d events, want %d (%v)", i, len(events), written[i], seqs)
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
