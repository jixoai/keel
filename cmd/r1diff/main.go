// Command r1diff is the R1 differential acceptance harness for the
// keel-service-graph change: it runs one fixed scenario against two runtime
// binaries (the frozen W0 baseline and the candidate build), captures the
// observable behaviour — HTTP responses, the committed event dump, the tool
// sequence, the SSE frames after a resume cursor and the run row — normalizes
// time and generated identifiers, and fails when anything else differs.
//
// Usage:
//
//	r1diff -old-bin /tmp/keel-baseline -new-bin /tmp/keel-candidate \
//	       -dsn "$KEEL_R1_DSN" -out /tmp/r1diff -allow r1-allow.json
//	r1diff -old-dump /tmp/r1diff/old.json -new-dump /tmp/r1diff/new.json
//
// The DSN must point at a database whose name contains "r1diff": every capture
// drops and recreates its schema, and the interlock keeps a mistyped DSN from
// wiping a development database.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jixoai/keel/internal/r1diff"
)

const scenarioName = "r1-basic-v1"

// Fixed identities keep both captures comparable: the scenario always uses the
// same tenant and session, so the dump differs only in normalized values.
const (
	r1Tenant  = "11111111-1111-4111-8111-111111111111"
	r1Session = "22222222-2222-4222-8222-222222222222"
	r1Probe   = "33333333-3333-4333-8333-333333333333"
	r1Other   = "44444444-4444-4444-8444-444444444444"
	// r1AdminKey is injected into both runtimes and sent on tenant creation:
	// the baseline ignores it, the hardened edge requires it. Carrying it on
	// both sides keeps the scenario identical across the change.
	r1AdminKey = "r1diff-admin-key"
)

type dump struct {
	Scenario string `json:"scenario"`
	// HTTP is keyed by step name (not an array) so allowlist paths stay stable
	// when the scenario gains a step; the step order lives in this file.
	HTTP      map[string]httpStep `json:"http"`
	Events    []map[string]any    `json:"events"`
	Tools     []string            `json:"tools"`
	SSE       []sseFrame          `json:"sse"`
	RunStatus map[string]any      `json:"runStatus"`
}

type httpStep struct {
	Status int `json:"status"`
	Body   any `json:"body"`
}

type sseFrame struct {
	ID    string `json:"id"`
	Event string `json:"event"`
	Data  any    `json:"data"`
}

func main() {
	var (
		oldBin  = flag.String("old-bin", "", "baseline runtime binary")
		newBin  = flag.String("new-bin", "", "candidate runtime binary")
		oldDump = flag.String("old-dump", "", "re-diff an existing baseline dump")
		newDump = flag.String("new-dump", "", "re-diff an existing candidate dump")
		dsn     = flag.String("dsn", os.Getenv("KEEL_R1_DSN"), "DSN of a dedicated r1diff database")
		out     = flag.String("out", "/tmp/r1diff", "output directory for dumps and the report")
		allow   = flag.String("allow", "", "allowlist JSON ({path,reason}[]); default: none")
		portOld = flag.Int("port-old", 13746, "port for the baseline run")
		portNew = flag.Int("port-new", 13747, "port for the candidate run")
	)
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}

	oldPath, newPath := *oldDump, *newDump
	if oldPath == "" {
		if *oldBin == "" || *dsn == "" {
			log.Fatal("need -old-bin and -dsn (or -old-dump)")
		}
		oldPath = filepath.Join(*out, "old.json")
		if err := capture("old", *oldBin, *dsn, *portOld, oldPath); err != nil {
			log.Fatalf("capture baseline: %v", err)
		}
	}
	if newPath == "" {
		if *newBin == "" || *dsn == "" {
			log.Fatal("need -new-bin and -dsn (or -new-dump)")
		}
		newPath = filepath.Join(*out, "new.json")
		if err := capture("new", *newBin, *dsn, *portNew, newPath); err != nil {
			log.Fatalf("capture candidate: %v", err)
		}
	}

	report, err := diffFiles(oldPath, newPath, *allow)
	if err != nil {
		log.Fatal(err)
	}
	reportPath := filepath.Join(*out, "report.json")
	raw, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(reportPath, raw, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Print(report.Summary())
	fmt.Printf("report: %s\n", reportPath)
	if !report.Equal {
		os.Exit(1)
	}
}

// diffFiles loads two dumps, normalizes whatever raw captures they hold and
// compares the flattened leaves.
func diffFiles(oldPath, newPath, allowPath string) (r1diff.Report, error) {
	oldFlat, err := loadFlat(oldPath)
	if err != nil {
		return r1diff.Report{}, err
	}
	newFlat, err := loadFlat(newPath)
	if err != nil {
		return r1diff.Report{}, err
	}
	rules, err := r1diff.LoadRules(allowPath)
	if err != nil {
		return r1diff.Report{}, err
	}
	diffs := r1diff.Compare(oldFlat, newFlat)
	return r1diff.BuildReport(len(oldFlat), diffs, rules), nil
}

func loadFlat(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("r1diff: parse %s: %w", path, err)
	}
	flat := map[string]string{}
	r1diff.Flatten(v, "", flat)
	return flat, nil
}

// capture runs the scenario once against one binary and writes its dump.
func capture(name, bin, dsn string, port int, outPath string) error {
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("binary %s: %w", bin, err)
	}
	if err := resetDatabase(dsn); err != nil {
		return err
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	casDir := filepath.Join(filepath.Dir(outPath), "cas-"+name)
	if err := os.MkdirAll(casDir, 0o755); err != nil {
		return err
	}
	proc, err := startRuntime(bin, dsn, base, casDir)
	if err != nil {
		return err
	}
	defer proc.stop()
	d, err := runScenario(base)
	if err != nil {
		return fmt.Errorf("scenario against %s: %w", name, err)
	}
	normalized, err := normalizeDump(d)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, raw, 0o644)
}

// resetDatabase drops and recreates the schema of the dedicated r1diff
// database so each capture starts from the same empty state.
func resetDatabase(dsn string) error {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("r1diff: parse dsn: %w", err)
	}
	if !strings.Contains(cfg.ConnConfig.Database, "r1diff") {
		return fmt.Errorf("r1diff: refusing to reset %q: the database name must contain \"r1diff\"", cfg.ConnConfig.Database)
	}
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE`); err != nil {
		return fmt.Errorf("r1diff: drop schema: %w", err)
	}
	if _, err := pool.Exec(ctx, `CREATE SCHEMA public`); err != nil {
		return fmt.Errorf("r1diff: create schema: %w", err)
	}
	return nil
}

type runtimeProc struct {
	cmd *exec.Cmd
	log *os.File
}

// startRuntime starts the runtime build with a scrubbed environment: every
// KEEL_* variable is dropped so a developer shell cannot leak a real model or
// MCP configuration into the differential.
func startRuntime(bin, dsn, base, casDir string) (*runtimeProc, error) {
	logPath := filepath.Join(filepath.Dir(casDir), filepath.Base(casDir)+".log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	addr := base[strings.LastIndexByte(base, ':')+1:]
	cmd := exec.Command(bin, "-addr", "127.0.0.1:"+addr, "-cas", casDir, "-streams", "64", "-admit-rate", "2000", "-runs", "4")
	env := []string{}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "KEEL_") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, "KEEL_DSN="+dsn, "GOMEMLIMIT=805306368", "KEEL_ADMIN_KEY="+r1AdminKey)
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, err
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(base + "/v1/health")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				return &runtimeProc{cmd: cmd, log: f}, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Wait()
	f.Close()
	return nil, errors.New("runtime did not become healthy (see " + logPath + ")")
}

func (p *runtimeProc) stop() {
	if p.cmd == nil {
		return
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
	p.cmd = nil
	if p.log != nil {
		p.log.Close()
		p.log = nil
	}
}

// runScenario is the fixed script both captures execute. It exercises the edge
// commit path (two user messages, one of which makes the fake provider call the
// echo tool), the tightened edges (owner-scoped kind, foreign tenant), the
// event log dump, the run row and one SSE resume.
func runScenario(base string) (*dump, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	d := &dump{Scenario: scenarioName, HTTP: map[string]httpStep{}}

	var created struct {
		TenantID  string `json:"tenantId"`
		SessionID string `json:"sessionId"`
	}
	st, body, err := doJSONHeaders(ctx, "POST", base+"/v1/tenants", r1Tenant,
		map[string]string{"name": "r1"}, map[string]string{"Authorization": "Bearer " + r1AdminKey}, &created)
	if err != nil {
		return nil, err
	}
	d.HTTP["tenant.create"] = httpStep{st, body}
	st, body, err = doJSON(ctx, "POST", base+"/v1/sessions", r1Tenant, map[string]string{"sessionId": r1Session}, &created)
	if err != nil {
		return nil, err
	}
	d.HTTP["session.create"] = httpStep{st, body}

	// The conversation runs through /input, the write path the frozen baseline
	// and the candidate share: it appends the user.message and enqueues the run,
	// so the same script produces the same conversation on both sides.
	//
	// Each message is posted only after the previous one settled. Posting both
	// back to back would let a claim land between them, and how a runtime
	// handles input that arrives mid-run is a *behaviour* under active change
	// (the audit's B2/B3), not something this parity gate should pin down: the
	// semantic assertion for that lives in the handoff suite, where a single
	// deterministic interleaving is required instead of a race with the worker.
	inputURL := base + "/v1/sessions/" + r1Session + "/input"
	st, body, err = doJSON(ctx, "POST", inputURL, r1Tenant, map[string]any{"text": "hello r1", "requestId": "r1-1"}, nil)
	if err != nil {
		return nil, err
	}
	d.HTTP["input.user1"] = httpStep{st, body}
	if _, err := waitSettled(ctx, base); err != nil {
		return nil, fmt.Errorf("settle after user1: %w", err)
	}
	st, body, err = doJSON(ctx, "POST", inputURL, r1Tenant, map[string]any{"text": "please echo this", "requestId": "r1-2"}, nil)
	if err != nil {
		return nil, err
	}
	d.HTTP["input.user2"] = httpStep{st, body}

	// The tightened edges are probed on their own session so a refused write
	// cannot shift the conversation's seq numbering.
	st, body, err = doJSON(ctx, "POST", base+"/v1/sessions", r1Tenant, map[string]string{"sessionId": r1Probe}, &created)
	if err != nil {
		return nil, err
	}
	d.HTTP["session.create.probe"] = httpStep{st, body}
	probeEventsURL := base + "/v1/sessions/" + r1Probe + "/events"
	st, body, err = doJSON(ctx, "POST", probeEventsURL, r1Tenant, map[string]any{
		"kind": "assistant.message", "payload": map[string]any{"text": "edge attempt"}}, nil)
	if err != nil {
		return nil, err
	}
	d.HTTP["events.owner_scoped"] = httpStep{st, body}

	// Wait for the scripted conversation to settle: a run row that is done and
	// a log that stopped growing.
	events, err := waitSettled(ctx, base)
	if err != nil {
		return nil, err
	}
	d.Events = events
	for _, ev := range events {
		kind, _ := ev["kind"].(string)
		switch kind {
		case "assistant.message":
			d.Tools = append(d.Tools, toolCallsOf(ev)...)
		case "tool.result":
			if payload, ok := ev["payload"].(map[string]any); ok {
				if name, ok := payload["toolName"].(string); ok {
					d.Tools = append(d.Tools, "result:"+name)
				}
			}
		}
	}

	// The probe session's log: the baseline accepted the owner-scoped write,
	// the tightened edge refuses it.
	st, body, err = doJSON(ctx, "GET", probeEventsURL, r1Tenant, nil, nil)
	if err != nil {
		return nil, err
	}
	d.HTTP["events.probe_session_log"] = httpStep{st, body}

	// A foreign tenant must not read the log.
	st, body, err = doJSON(ctx, "GET", base+"/v1/sessions/"+r1Session+"/events", r1Other, nil, nil)
	if err != nil {
		return nil, err
	}
	d.HTTP["events.foreign_tenant"] = httpStep{st, body}

	var run struct {
		Status    string `json:"status"`
		Attempts  int    `json:"attempts"`
		CursorSeq *int64 `json:"cursorSeq"`
	}
	st, body, err = doJSON(ctx, "GET", base+"/v1/sessions/"+r1Session+"/run", r1Tenant, nil, &run)
	if err != nil {
		return nil, err
	}
	d.RunStatus = map[string]any{"step": "run.status", "status": st, "body": body}

	// SSE resume from the second user message: the frames must be exactly the
	// committed events after that cursor, in order.
	after, seen := int64(0), 0
	for _, ev := range events {
		if ev["kind"] != "user.message" {
			continue
		}
		seen++
		if seen == 2 {
			if seq, ok := ev["seq"].(float64); ok {
				after = int64(seq)
			}
		}
	}
	if after == 0 {
		return nil, errors.New("scenario: the second user message is missing from the log")
	}
	frames, err := captureSSE(ctx, base+"/v1/sessions/"+r1Session+"/events/stream", r1Tenant, after)
	if err != nil {
		return nil, err
	}
	d.SSE = frames
	return d, nil
}

// waits for the scripted conversation: three assistant messages (two replies
// plus the final answer after the tool result), one tool result, and a settled
// run row.
func waitSettled(ctx context.Context, base string) ([]map[string]any, error) {
	deadline := time.Now().Add(60 * time.Second)
	eventsURL := base + "/v1/sessions/" + r1Session + "/events?limit=500"
	var (
		lastLen     = -1
		stableSince time.Time
	)
	for {
		events, err := listEvents(ctx, eventsURL)
		if err != nil {
			return nil, err
		}
		var run struct {
			Status string `json:"status"`
		}
		_, _, runErr := doJSON(ctx, "GET", base+"/v1/sessions/"+r1Session+"/run", r1Tenant, nil, &run)
		// Settled means: the run row is done and the log stopped growing for a
		// full second. Counting events directly would encode which conversation
		// shape we expect; the shape is exactly what the differential compares.
		if runErr == nil && run.Status == "done" && len(events) > 1 {
			if len(events) == lastLen {
				if time.Since(stableSince) >= time.Second {
					return events, nil
				}
			} else {
				lastLen = len(events)
				stableSince = time.Now()
			}
		} else {
			lastLen = -1
		}
		if time.Now().After(deadline) {
			raw, _ := json.Marshal(events)
			return nil, fmt.Errorf("conversation did not settle in 60s (status=%q events=%d, log tail: %s)",
				run.Status, len(events), tail(raw, 600))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func listEvents(ctx context.Context, url string) ([]map[string]any, error) {
	_, body, err := doJSON(ctx, "GET", url, r1Tenant, nil, nil)
	if err != nil {
		return nil, err
	}
	m, ok := body.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("list events: unexpected body %T", body)
	}
	raw, ok := m["events"].([]any)
	if !ok {
		return nil, errors.New("list events: no events array")
	}
	out := make([]map[string]any, 0, len(raw))
	for _, ev := range raw {
		if em, ok := ev.(map[string]any); ok {
			out = append(out, em)
		}
	}
	return out, nil
}

// toolCallsOf extracts the tool names of one assistant message, independent of
// the generated call ids.
func toolCallsOf(ev map[string]any) []string {
	payload, _ := ev["payload"].(map[string]any)
	if payload == nil {
		return nil
	}
	content, _ := payload["content"].([]any)
	var out []string
	for _, c := range content {
		cm, _ := c.(map[string]any)
		if cm == nil {
			continue
		}
		if t, _ := cm["type"].(string); t == "toolCall" {
			if name, _ := cm["name"].(string); name != "" {
				out = append(out, "call:"+name)
			}
		}
	}
	return out
}

// captureSSE opens the stream at the given cursor and collects frames until it
// goes idle (1.5s without a frame), so the harness does not depend on heartbeat
// timing.
func captureSSE(ctx context.Context, url, tenant string, after int64) ([]sseFrame, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("Last-Event-ID", fmt.Sprintf("%d", after))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("sse: status %d", res.StatusCode)
	}
	reader := bufio.NewReader(res.Body)
	frames := []sseFrame{}
	deadline := time.Now().Add(20 * time.Second)
	for len(frames) < 50 && time.Now().Before(deadline) {
		var frame *rawFrame
		if len(frames) == 0 {
			frame, err = readFrame(reader)
		} else {
			frame, err = readFrameWithTimeout(reader, 1500*time.Millisecond)
			if errors.Is(err, errIdle) {
				break
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				break
			}
			return nil, err
		}
		if frame == nil { // pure heartbeat
			continue
		}
		var data any
		if frame.event != "" && frame.rawData != "" {
			_ = json.Unmarshal([]byte(frame.rawData), &data)
		}
		frames = append(frames, sseFrame{ID: frame.id, Event: frame.event, Data: data})
	}
	return frames, nil
}

var errIdle = errors.New("r1diff: sse idle")

type rawFrame struct {
	id, event, rawData string
}

// readFrame reads one SSE frame, skipping comment lines. A frame with no
// fields at all (pure heartbeat) returns nil, nil.
func readFrame(r *bufio.Reader) (*rawFrame, error) {
	f := &rawFrame{}
	saw := false
	for {
		line, err := r.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "id:"):
			f.id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			saw = true
		case strings.HasPrefix(line, "event:"):
			f.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			saw = true
		case strings.HasPrefix(line, "data:"):
			f.rawData += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			saw = true
		case line == "":
			if saw {
				return f, nil
			}
		case strings.HasPrefix(line, ":"):
			// comment/heartbeat
		}
		if err != nil {
			if saw {
				return f, nil
			}
			return nil, err
		}
	}
}

// readFrameWithTimeout reports errIdle when no frame starts within the window.
func readFrameWithTimeout(r *bufio.Reader, d time.Duration) (*rawFrame, error) {
	type result struct {
		f   *rawFrame
		err error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := readFrame(r)
		ch <- result{f, err}
	}()
	select {
	case res := <-ch:
		return res.f, res.err
	case <-time.After(d):
		return nil, errIdle
	}
}

// doJSON performs one JSON request and decodes the response body.
func doJSON(ctx context.Context, method, url, tenant string, body any, out any) (int, any, error) {
	return doJSONHeaders(ctx, method, url, tenant, body, nil, out)
}

// doJSONHeaders is doJSON with extra request headers.
func doJSONHeaders(ctx context.Context, method, url, tenant string, body any, headers map[string]string, out any) (int, any, error) {
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Tenant-ID", tenant)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	var decoded any
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			decoded = string(raw)
		}
	}
	if out != nil && decoded != nil {
		// Re-marshal into the caller's typed struct for the script's logic;
		// the dump keeps the decoded generic form.
		buf, _ := json.Marshal(decoded)
		_ = json.Unmarshal(buf, out)
	}
	return res.StatusCode, decoded, nil
}

// normalizeDump canonicalizes the captured artifacts before they are written.
func normalizeDump(d *dump) (any, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	n := r1diff.NewNormalizer()
	return n.Value(v), nil
}

func tail(raw []byte, n int) string {
	if len(raw) <= n {
		return string(raw)
	}
	return "…" + string(raw[len(raw)-n:])
}
