// Process-level handoff test: two real `cmd/runtime` processes share one
// PostgreSQL database, one drains while the other takes over, and a real SSE
// client follows the session across the swap.
//
// This is the "两个真实 runtime 进程" evidence the runtime-handoff spec asks
// for, not a handler-level approximation: the runtimes are spawned from the
// built binary, the client speaks plain HTTP/SSE, and the audit tables are read
// back with SQL.
//
// The test needs KEEL_TEST_DSN (it is skipped without one) and creates its own
// database so the other test packages sharing the DSN cannot cross-talk with
// it.
package handoff

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const processAdminKey = "handoff-process-key"

// runtimeBinary builds cmd/runtime once per test process. The binary lives in a
// process-wide directory (not a test's TempDir) so several tests can spawn it,
// and TestMain removes it before the test binary exits.
var (
	buildOnce sync.Once
	buildPath string
	buildErr  error
	builtDir  string
)

// buildDir returns the process-wide directory the runtime binary is built into.
func buildDir() string {
	if builtDir == "" {
		dir, err := os.MkdirTemp("", "keel-handoff-bin-")
		if err != nil {
			panic(err)
		}
		builtDir = dir
	}
	return builtDir
}

func TestMain(m *testing.M) {
	code := m.Run()
	if builtDir != "" {
		_ = os.RemoveAll(builtDir)
	}
	os.Exit(code)
}

func runtimeBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		root, err := repoRoot()
		if err != nil {
			buildErr = err
			return
		}
		out := filepath.Join(buildDir(), "keel-runtime")
		cmd := exec.Command("go", "build", "-o", out, "./cmd/runtime")
		cmd.Dir = root
		if combined, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build ./cmd/runtime: %v: %s", err, combined)
			return
		}
		buildPath = out
	})
	if buildErr != nil {
		t.Fatalf("build runtime binary: %v", buildErr)
	}
	return buildPath
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(raw), "module github.com/jixoai/keel") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("repository root not found from %s", dir)
		}
		dir = parent
	}
}

// isolatedDatabase creates a dedicated database for this test and drops it
// again, so the shared DSN's other users (other packages running in parallel)
// can never interfere.
func isolatedDatabase(t *testing.T, dsn string) string {
	t.Helper()
	ctx := context.Background()
	// The DSN is rebuilt as a URL (pgx's ConnConfig.ConnString() keeps the
	// original database), so the runtimes really connect to the isolated one.
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		t.Fatalf("KEEL_TEST_DSN must be a URL DSN for this test, got %q", dsn)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	name := "keel_handoff_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect for createdb: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close(ctx)
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Logf("cleanup: reconnect for dropdb: %v", err)
			return
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Logf("cleanup: drop database %s: %v", name, err)
			return
		}
		t.Logf("cleanup: dropped database %s", name)
	})
	scoped := *parsed
	scoped.Path = "/" + name
	return scoped.String()
}

// runtimeProcess is one spawned `cmd/runtime`.
type runtimeProcess struct {
	t        *testing.T
	id       string
	base     string
	cmd      *exec.Cmd
	logFile  string
	done     chan struct{}
	stopOnce sync.Once
}

// exited reports whether the process has exited.
func (p *runtimeProcess) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// startRuntime spawns the binary against dsn and waits for /v1/health.
func startRuntime(t *testing.T, bin, dsn, runtimeID string, extraEnv ...string) *runtimeProcess {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	logFile := filepath.Join(t.TempDir(), runtimeID+".log")
	logHandle, err := os.Create(logFile)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", port),
		"-dsn", dsn,
		"-handoff-window", "3s",
	)
	cmd.Env = append(os.Environ(),
		"KEEL_RUNTIME_ID="+runtimeID,
		"KEEL_ADMIN_KEY="+processAdminKey,
		"KEEL_LOADOUT_ID=process-test",
		"KEEL_LOADOUT_REVISION=1",
		"KEEL_DRAIN_WINDOW=10s",
		"KEEL_DRAIN_CLOSE_RATE=200",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdout = logHandle
	cmd.Stderr = logHandle
	if err := cmd.Start(); err != nil {
		logHandle.Close()
		t.Fatalf("start runtime %s: %v", runtimeID, err)
	}
	p := &runtimeProcess{
		t:       t,
		id:      runtimeID,
		base:    fmt.Sprintf("http://127.0.0.1:%d", port),
		cmd:     cmd,
		logFile: logFile,
		done:    make(chan struct{}),
	}
	go func() {
		_ = cmd.Wait()
		logHandle.Close()
		close(p.done)
	}()
	t.Cleanup(func() {
		p.stop()
		if code := p.exitCode(); code != 0 && code != -1 {
			t.Logf("runtime %s exited with code %d", runtimeID, code)
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(p.base + "/v1/health")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				return p
			}
		}
		select {
		case <-p.done:
			t.Fatalf("runtime %s exited during boot\n%s", runtimeID, p.logs())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("runtime %s never became healthy\n%s", runtimeID, p.logs())
	return nil
}

// stop terminates the process (SIGKILL) and waits for it.
func (p *runtimeProcess) stop() {
	p.stopOnce.Do(func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			p.t.Errorf("runtime %s did not exit after kill", p.id)
		}
	})
}

// signalTERM sends SIGTERM (the shutdown drain trigger).
func (p *runtimeProcess) signalTERM(t *testing.T) {
	t.Helper()
	if p.cmd.Process == nil {
		t.Fatal("runtime not started")
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
}

func (p *runtimeProcess) exitCode() int {
	if p.cmd.ProcessState == nil {
		return -1
	}
	return p.cmd.ProcessState.ExitCode()
}

func (p *runtimeProcess) logs() string {
	raw, err := os.ReadFile(p.logFile)
	if err != nil {
		return "<no log>"
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) > 30 {
		lines = lines[len(lines)-30:]
	}
	return strings.Join(lines, "\n")
}

// adminHeaders are the operator credentials the runtime requires for
// provisioning and the runtime control routes.
func adminHeaders(tenant string) map[string]string {
	return map[string]string{
		"X-Tenant-ID":   tenant,
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + processAdminKey,
	}
}

// httpJSON performs a request and decodes a JSON object response.
func httpJSON(t *testing.T, method, url string, headers map[string]string, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer res.Body.Close()
	var decoded map[string]any
	raw, _ := io.ReadAll(res.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return res.StatusCode, decoded
}

// streamFrame is one SSE frame.
type streamFrame struct {
	id    string
	event string
	data  string
}

// streamClient is a background SSE reader: frames arrive on a channel so the
// test can assert with timeouts.
type streamClient struct {
	t      *testing.T
	cancel context.CancelFunc
	frames chan streamFrame
	errs   chan error
	closed chan struct{}
	body   io.ReadCloser
}

func openStream(t *testing.T, base, tenant, session string, lastEventID string) *streamClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	req, err := http.NewRequestWithContext(ctx, "GET",
		base+"/v1/sessions/"+session+"/events/stream", nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", tenant)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open stream: %v", err)
	}
	if res.StatusCode != 200 {
		res.Body.Close()
		cancel()
		t.Fatalf("stream status = %d", res.StatusCode)
	}
	c := &streamClient{
		t:      t,
		cancel: cancel,
		frames: make(chan streamFrame, 64),
		errs:   make(chan error, 1),
		closed: make(chan struct{}),
		body:   res.Body,
	}
	go func() {
		defer close(c.closed)
		br := bufio.NewReader(res.Body)
		var frame streamFrame
		have := false
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				select {
				case c.errs <- err:
				default:
				}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if have || frame.event != "" {
					select {
					case c.frames <- frame:
					case <-ctx.Done():
						return
					}
				}
				frame = streamFrame{}
				have = false
			case strings.HasPrefix(line, "id: "):
				frame.id = strings.TrimPrefix(line, "id: ")
				have = true
			case strings.HasPrefix(line, "event: "):
				frame.event = strings.TrimPrefix(line, "event: ")
				have = true
			case strings.HasPrefix(line, "data: "):
				frame.data = strings.TrimPrefix(line, "data: ")
				have = true
			}
		}
	}()
	t.Cleanup(c.close)
	return c
}

func (c *streamClient) close() {
	c.cancel()
	c.body.Close()
}

// next returns the next frame, or fails after the timeout. Queued frames win
// over a pending read error: the server may close the stream right after
// writing its last frame, and both become ready at once.
func (c *streamClient) next(timeout time.Duration, what string) streamFrame {
	c.t.Helper()
	select {
	case frame := <-c.frames:
		return frame
	default:
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case frame := <-c.frames:
			return frame
		case err := <-c.errs:
			select {
			case frame := <-c.frames:
				return frame
			default:
			}
			c.t.Fatalf("stream ended while waiting for %s: %v", what, err)
		case <-timer.C:
			c.t.Fatalf("timeout waiting for %s", what)
		}
	}
}

// waitClosed asserts the stream ends (the drain close).
func (c *streamClient) waitClosed(timeout time.Duration) {
	c.t.Helper()
	select {
	case <-c.closed:
	case <-time.After(timeout):
		c.t.Fatal("stream did not close")
	}
}

// sqlDB opens a read-only-ish connection to the isolated database.
func sqlDB(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// TestRuntimeHandoffProcess drives the full handoff across two real runtimes:
// A answers the first message, starts a handoff drain, keeps accepting edge
// input, and B takes over; a real SSE client resumes on B with Last-Event-ID.
func TestRuntimeHandoffProcess(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	if runtime.GOOS == "windows" {
		t.Skip("process handoff test runs on unix signals")
	}
	bin := runtimeBinary(t)
	dbDSN := isolatedDatabase(t, dsn)
	conn := sqlDB(t, dbDSN)

	a := startRuntime(t, bin, dbDSN, "runtime-a")
	b := startRuntime(t, bin, dbDSN, "runtime-b")

	tenant, session := uuid.New(), uuid.New()
	headers := map[string]string{"X-Tenant-ID": tenant.String(), "Content-Type": "application/json"}
	if code, body := httpJSON(t, "POST", a.base+"/v1/tenants", adminHeaders(tenant.String()), `{"name":"handoff"}`); code != 201 {
		t.Fatalf("create tenant = %d %v", code, body)
	}
	if code, body := httpJSON(t, "POST", a.base+"/v1/sessions", headers,
		fmt.Sprintf(`{"sessionId":%q}`, session.String())); code != 201 {
		t.Fatalf("create session = %d %v", code, body)
	}

	inputPath := "/v1/sessions/" + session.String() + "/input"
	eventsPath := "/v1/sessions/" + session.String() + "/events"

	// 1. A answers the first message (the run completes on A).
	if code, body := httpJSON(t, "POST", a.base+inputPath, headers, `{"text":"one","requestId":"p1"}`); code != 201 {
		t.Fatalf("first input = %d %v", code, body)
	}
	firstReply, ok := waitForAssistant(t, a.base, headers, eventsPath, 1, 30*time.Second)
	if !ok {
		t.Fatalf("A never answered the first message\n--- runtime-a log ---\n%s\n--- runtime-b log ---\n%s",
			a.logs(), b.logs())
	}
	// The attempt records its own summary (run.finished) right before the settle,
	// so the log cursor a client resumes from is that frame, not the reply.
	cursor, ok := waitForFinished(t, a.base, headers, eventsPath, 1, 30*time.Second)
	if !ok {
		t.Fatalf("A never recorded the first run summary\n--- runtime-a log ---\n%s", a.logs())
	}
	if cursor <= firstReply {
		t.Fatalf("run.finished seq = %d, want after the reply at %d", cursor, firstReply)
	}

	// 2. A client opens the stream on A and reads up to the settled run.
	streamA := openStream(t, a.base, tenant.String(), session.String(), "")
	var lastID string
	for {
		frame := streamA.next(15*time.Second, "the first run's frames")
		if frame.id == "" {
			continue
		}
		lastID = frame.id
		if frame.event == "run.finished" {
			break
		}
	}
	if lastID != fmt.Sprintf("%d", cursor) {
		t.Fatalf("stream cursor = %s, log cursor = %d", lastID, cursor)
	}

	// 3. The operator triggers a handoff drain on A.
	code, body := httpJSON(t, "POST", a.base+"/v1/runtime/drain",
		map[string]string{"Authorization": "Bearer " + processAdminKey, "Content-Type": "application/json"},
		`{"mode":"handoff","deadlineMs":4000}`)
	if code != 200 {
		t.Fatalf("handoff drain = %d %v", code, body)
	}
	if body["drainMode"] != "handoff" || body["claiming"] != false {
		t.Fatalf("drain body = %v", body)
	}

	// 4. A is draining but keeps accepting edge input, and does not claim it.
	code, body = httpJSON(t, "POST", a.base+inputPath, headers, `{"text":"two","requestId":"p2"}`)
	if code != 201 {
		t.Fatalf("input during handoff = %d %v (the edge path must stay open)", code, body)
	}
	secondSeq, _ := body["seq"].(float64)
	if int64(secondSeq) != cursor+1 {
		t.Fatalf("input during handoff landed at seq %v, want %d", body["seq"], cursor+1)
	}
	// The old runtime is not claiming: the input stays pending until B picks it
	// up (B answers below, and A's claim gate is reported closed).
	if code, body := httpJSON(t, "GET", a.base+"/v1/runtime/status",
		map[string]string{"Authorization": "Bearer " + processAdminKey}, ""); code != 200 || body["claiming"] != false {
		t.Fatalf("A status while draining = %d %v", code, body)
	}

	// 5. A's stream flushes what was committed by the time it drains, then
	// closes with the drain hint. Frames are contiguous from the start.
	var (
		drainFrame streamFrame
		// Step 2 consumed frames 1..cursor; the drain flush continues from there.
		lastA int64 = cursor
	)
	for {
		frame := streamA.next(15*time.Second, "the drain frame (A log: "+a.logs()+")")
		if frame.event == "drain" {
			drainFrame = frame
			break
		}
		if frame.id == "" || frame.event == "" {
			continue
		}
		id, err := strconv.ParseInt(frame.id, 10, 64)
		if err != nil {
			t.Fatalf("frame id %q", frame.id)
		}
		if id != lastA+1 {
			t.Fatalf("gap or repeat on A's stream: frame %d after %d", id, lastA)
		}
		lastA = id
	}
	streamA.waitClosed(15 * time.Second)
	if !strings.Contains(drainFrame.data, "retry_after_ms") {
		t.Fatalf("drain frame = %q", drainFrame.data)
	}
	if lastA != cursor && lastA != cursor+1 {
		t.Fatalf("A's stream stopped at %d, want %d (the reply) or %d (the drained input)", lastA, cursor, cursor+1)
	}

	// 6. The client reconnects to the NEW runtime with Last-Event-ID: the first
	// frame is the cursor + 1, with no repeats and no gap, and the drained
	// input's reply arrives on the successor.
	streamB := openStream(t, b.base, tenant.String(), session.String(), fmt.Sprintf("%d", lastA))
	var seen []streamFrame
	for {
		frame := streamB.next(30*time.Second, "the successor's frames")
		if frame.id == "" || frame.event == "" {
			continue
		}
		id, err := strconv.ParseInt(frame.id, 10, 64)
		if err != nil {
			t.Fatalf("frame id %q", frame.id)
		}
		if want := lastA + int64(len(seen)) + 1; id != want {
			t.Fatalf("resume frame %d, want %d (cursor+1, no repeat, no gap)", id, want)
		}
		seen = append(seen, frame)
		// The successor answers the drained input and records the attempt
		// summary; both arrive on the resumed stream.
		if frame.event == "run.finished" && len(seen) > 1 {
			break
		}
	}
	var userFrames, assistantFrames int
	for _, frame := range seen {
		switch frame.event {
		case "user.message":
			userFrames++
		case "assistant.message":
			assistantFrames++
		}
	}
	// The drained input is only on B's stream when A's drain flush stopped before
	// it; either way the successor's reply is exactly one assistant message.
	wantUserFrames := 0
	if lastA == cursor {
		wantUserFrames = 1
	}
	if userFrames != wantUserFrames {
		t.Fatalf("resumed stream carried %d user frames, want %d: %+v", userFrames, wantUserFrames, seen)
	}
	if assistantFrames != 1 {
		t.Fatalf("resumed stream carried %d assistant frames, want 1: %+v", assistantFrames, seen)
	}
	if seen[len(seen)-1].event != "run.finished" {
		t.Fatalf("last resumed frame = %+v, want the settled attempt summary", seen[len(seen)-1])
	}

	// 7. The final log is exactly what the two messages imply: no lost events,
	// no duplicates, and the run converged.
	mustAssistant(t, b.base, headers, eventsPath, 2, 30*time.Second)
	code, body = httpJSON(t, "GET", b.base+eventsPath+"?limit=100", headers, "")
	if code != 200 {
		t.Fatalf("read log = %d", code)
	}
	log := parseEvents(t, body)
	if len(log.user) != 2 || len(log.assistant) != 2 || len(log.finished) != 2 {
		t.Fatalf("log = %d user / %d assistant / %d finished events, want 2/2/2",
			len(log.user), len(log.assistant), len(log.finished))
	}
	if log.seqs[len(log.seqs)-1] != 6 {
		t.Fatalf("log seqs = %v, want 1..6", log.seqs)
	}
	for i, seq := range log.seqs {
		if seq != int64(i+1) {
			t.Fatalf("log has a hole: seqs = %v", log.seqs)
		}
	}
	// The run settles once the successor's attempt finishes; poll for it rather
	// than racing the finish transaction.
	waitFor(t, 15*time.Second, func() bool {
		res, err := http.Get(b.base + "/v1/health")
		if err != nil {
			return false
		}
		res.Body.Close()
		code, body := httpJSON(t, "GET", b.base+"/v1/sessions/"+session.String()+"/run", headers, "")
		return code == 200 && body["status"] == "done"
	}, "the successor's run to settle done")

	// 8. The audit answers the handoff: the drain and the runtime rows are
	// durable, mode-tagged and attributable.
	waitFor(t, 10*time.Second, func() bool {
		var value string
		if err := conn.QueryRow(context.Background(),
			`SELECT status FROM runtime_instances WHERE runtime_id = 'runtime-a'`).Scan(&value); err != nil {
			return false
		}
		return value == "stopped" || value == "draining"
	}, "runtime-a audit row")
	var (
		status, mode string
		inFlight     int
	)
	if err := conn.QueryRow(context.Background(),
		`SELECT status, COALESCE(drain_mode, ''), in_flight_runs FROM runtime_instances
		 WHERE runtime_id = 'runtime-a'`).Scan(&status, &mode, &inFlight); err != nil {
		t.Fatal(err)
	}
	if mode != "handoff" {
		t.Fatalf("runtime-a drain_mode = %q, want handoff", mode)
	}
	if status != "stopped" || inFlight != 0 {
		t.Fatalf("runtime-a row = status %q in_flight %d, want a completed drain", status, inFlight)
	}
	var row struct {
		mode, from, to, rowStatus, loadout string
		fromEpoch, toEpoch                 int64
	}
	err := conn.QueryRow(context.Background(),
		`SELECT mode, from_runtime, to_runtime, status, loadout_id, from_epoch, to_epoch
		 FROM handoffs WHERE mode = 'handoff' ORDER BY started_at LIMIT 1`).
		Scan(&row.mode, &row.from, &row.to, &row.rowStatus, &row.loadout, &row.fromEpoch, &row.toEpoch)
	if err != nil {
		t.Fatalf("handoff audit row: %v", err)
	}
	if row.from != "runtime-a" || row.rowStatus != "completed" || row.loadout != "process-test" {
		t.Fatalf("handoff audit row = %+v", row)
	}

	// 9. A's log names the drain and the mode; B's log never mentions draining.
	if logs := a.logs(); !strings.Contains(logs, `"mode":"handoff"`) {
		t.Fatalf("A never logged the handoff mode:\n%s", logs)
	}
	if logs := b.logs(); strings.Contains(logs, "drain begin") {
		t.Fatalf("B drained unexpectedly:\n%s", logs)
	}
}

// TestRuntimeShutdownProcessDrain covers SIGTERM: shutdown mode refuses new
// writes with 503 + Retry-After while the successor keeps working, and the
// audit records the shutdown mode.
func TestRuntimeShutdownProcessDrain(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	if runtime.GOOS == "windows" {
		t.Skip("process handoff test runs on unix signals")
	}
	bin := runtimeBinary(t)
	dbDSN := isolatedDatabase(t, dsn)
	conn := sqlDB(t, dbDSN)

	a := startRuntime(t, bin, dbDSN, "runtime-shutdown", "KEEL_DRAIN_CLOSE_RATE=1")
	b := startRuntime(t, bin, dbDSN, "runtime-successor")

	tenant, session := uuid.New(), uuid.New()
	headers := map[string]string{"X-Tenant-ID": tenant.String(), "Content-Type": "application/json"}
	if code, body := httpJSON(t, "POST", a.base+"/v1/tenants", adminHeaders(tenant.String()), `{"name":"shutdown"}`); code != 201 {
		t.Fatalf("create tenant = %d %v", code, body)
	}
	if code, body := httpJSON(t, "POST", a.base+"/v1/sessions", headers,
		fmt.Sprintf(`{"sessionId":%q}`, session.String())); code != 201 {
		t.Fatalf("create session = %d %v", code, body)
	}

	// A client holds a stream open, so the shutdown drain has something to
	// close (at the configured rate) and the drain state stays observable.
	stream := openStream(t, a.base, tenant.String(), session.String(), "")
	a.signalTERM(t)
	waitFor(t, 20*time.Second, func() bool {
		res, err := http.Get(a.base + "/v1/ready")
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == 503
	}, "A to report draining")

	sessionHeaders := map[string]string{"X-Tenant-ID": tenant.String(), "Content-Type": "application/json"}
	code, body := httpJSON(t, "POST", a.base+"/v1/sessions/"+session.String()+"/input", sessionHeaders,
		`{"text":"during shutdown","requestId":"s1"}`)
	if code != 503 {
		t.Fatalf("input during shutdown drain = %d %v, want 503", code, body)
	}
	// The drained stream ends; the old runtime then exits on its own.
	stream.waitClosed(20 * time.Second)
	waitFor(t, 20*time.Second, a.exited, "the shutdown runtime to exit")

	// The client retries the same request against the successor: idempotency
	// spans processes, so the retry lands exactly once.
	code, body = httpJSON(t, "POST", b.base+"/v1/sessions/"+session.String()+"/input", sessionHeaders,
		`{"text":"during shutdown","requestId":"s1"}`)
	if code != 201 {
		t.Fatalf("retry against the successor = %d %v", code, body)
	}
	mustAssistant(t, b.base, headers, "/v1/sessions/"+session.String()+"/events", 1, 30*time.Second)

	// The audit records the shutdown mode for A.
	waitFor(t, 15*time.Second, func() bool {
		var mode string
		err := conn.QueryRow(context.Background(),
			`SELECT COALESCE(drain_mode, '') FROM runtime_instances WHERE runtime_id = 'runtime-shutdown'`).Scan(&mode)
		return err == nil && mode == "shutdown"
	}, "shutdown audit row")
	var rowStatus string
	if err := conn.QueryRow(context.Background(),
		`SELECT status FROM handoffs WHERE from_runtime = 'runtime-shutdown' AND mode = 'shutdown'`).Scan(&rowStatus); err != nil {
		t.Fatalf("shutdown handoff row: %v", err)
	}
}

// logDump summarizes a session's event log.
type logDump struct {
	seqs      []int64
	user      []int64
	assistant []int64
	finished  []int64
}

func parseEvents(t *testing.T, body map[string]any) logDump {
	t.Helper()
	var dump logDump
	raw, ok := body["events"].([]any)
	if !ok {
		t.Fatalf("events body = %v", body)
	}
	for _, entry := range raw {
		ev, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("event = %v", entry)
		}
		seq := int64(ev["seq"].(float64))
		kind, _ := ev["kind"].(string)
		dump.seqs = append(dump.seqs, seq)
		switch kind {
		case "user.message":
			dump.user = append(dump.user, seq)
		case "assistant.message":
			dump.assistant = append(dump.assistant, seq)
		case "run.finished":
			dump.finished = append(dump.finished, seq)
		}
	}
	return dump
}

// waitForFinished polls the log until it holds n attempt summaries and returns
// the seq of the last one. ok=false means the timeout passed.
func waitForFinished(t *testing.T, base string, headers map[string]string, eventsPath string, n int, timeout time.Duration) (int64, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		code, body := httpJSON(t, "GET", base+eventsPath+"?limit=100", headers, "")
		if code == 200 {
			dump := parseEvents(t, body)
			if len(dump.finished) >= n {
				return dump.finished[len(dump.finished)-1], true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0, false
}

// waitForAssistant polls the log until it holds n assistant messages and
// returns the seq of the last one. ok=false means the timeout passed.
func waitForAssistant(t *testing.T, base string, headers map[string]string, eventsPath string, n int, timeout time.Duration) (int64, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		code, body := httpJSON(t, "GET", base+eventsPath+"?limit=100", headers, "")
		if code == 200 {
			dump := parseEvents(t, body)
			if len(dump.assistant) >= n {
				return dump.assistant[len(dump.assistant)-1], true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0, false
}

// mustAssistant waits for a reply or fails, naming the session log it read.
func mustAssistant(t *testing.T, base string, headers map[string]string, eventsPath string, n int, timeout time.Duration) int64 {
	t.Helper()
	seq, ok := waitForAssistant(t, base, headers, eventsPath, n, timeout)
	if !ok {
		code, body := httpJSON(t, "GET", base+eventsPath+"?limit=100", headers, "")
		raw, _ := json.Marshal(body)
		t.Fatalf("%d assistant message(s) never arrived; last log read: %d %s", n, code, raw)
	}
	return seq
}

// waitFor polls a predicate until it holds or the timeout passes.
func waitFor(t *testing.T, timeout time.Duration, pred func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
