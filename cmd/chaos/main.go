// Command chaos is the fault-injection acceptance harness for the runtime:
// it keeps writing input while randomly SIGKILLing processes and handing the
// session off, then asserts the ledger invariants that must survive any of it.
//
//	mode=kill    : random SIGKILL of the runtime, restarted by the harness,
//	               for the whole duration.
//	mode=handoff : a second runtime takes over the first (handoff drain), the
//	               successor is SIGKILLed right after it starts claiming (epoch
//	               jumps), and a third runtime finishes the job.
//
// Invariants checked on every touched session at the end:
//   - the committed seq is contiguous from 1 (no lost or duplicated event);
//   - every posted requestId appears exactly once as a user.message;
//   - every turn has at most one final (non-tool-call) assistant message;
//   - no run stays "running" once the harness stops killing things;
//   - the session converges (done/pending/failed, never stuck in running).
//
// Usage:
//
//	chaos -bin /tmp/keel-runtime -dsn 'postgres://…/keel_chaos_e2e?sslmode=disable' \
//	      -port 13795 -sessions 8 -duration 60s -kill-every 5s -mode kill
package main

import (
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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type config struct {
	bin       string
	dsn       string
	adminKey  string
	port      int
	sessions  int
	duration  time.Duration
	killEvery time.Duration
	mode      string
	settleFor time.Duration
	jsonOut   string
}

type runtimeProc struct {
	cmd *exec.Cmd
	id  string
}

type harness struct {
	cfg      config
	base     string
	curPort  int
	mu       sync.Mutex
	procs    []*runtimeProc
	logFile  *os.File
	killed   atomic.Int64
	restarts atomic.Int64
	pool     *pgxpool.Pool
}

type sessionState struct {
	tenant    string
	session   string
	mu        sync.Mutex
	requestID []string
	posted    atomic.Int64
}

// checkPortFree refuses to run against a foreign process: a stale runtime on
// the port would answer the harness's later requests.
func checkPortFree(port int) error {
	out, err := exec.Command("lsof", "-nP", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN").Output()
	if err == nil && len(bytes.TrimSpace(out)) > 0 {
		holder := ""
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) > 1 {
			fields := strings.Fields(lines[1])
			if len(fields) > 1 {
				holder = fields[1]
			}
		}
		return fmt.Errorf("port %d is already in use (holder pid %s): refusing to run", port, holder)
	}
	return nil
}

func main() {
	var c config
	flag.StringVar(&c.bin, "bin", "/tmp/keel-runtime", "runtime binary")
	flag.StringVar(&c.dsn, "dsn", os.Getenv("KEEL_DSN"), "dedicated PostgreSQL DSN (name must look dedicated)")
	flag.StringVar(&c.adminKey, "admin-key", "chaos-admin-key", "admin key for tenant provisioning")
	flag.IntVar(&c.port, "port", 13795, "base listen port (the successor uses port+1)")
	flag.IntVar(&c.sessions, "sessions", 8, "concurrent sessions")
	flag.DurationVar(&c.duration, "duration", 60*time.Second, "fault-injection duration")
	flag.DurationVar(&c.killEvery, "kill-every", 5*time.Second, "interval between faults")
	flag.StringVar(&c.mode, "mode", "kill", "kill | handoff")
	flag.DurationVar(&c.settleFor, "settle", 20*time.Second, "quiet period after the last fault (must exceed the lease TTL)")
	flag.StringVar(&c.jsonOut, "out", "", "write the result JSON here")
	flag.Parse()
	if c.dsn == "" {
		log.Fatal("KEEL_DSN (or -dsn) is required")
	}
	if c.mode != "kill" && c.mode != "handoff" {
		log.Fatalf("unknown mode %q", c.mode)
	}
	if err := checkPortFree(c.port); err != nil {
		log.Fatal(err)
	}
	if err := checkPortFree(c.port + 1); err != nil {
		log.Fatal(err)
	}
	if err := resetDatabase(context.Background(), c.dsn); err != nil {
		log.Fatal(err)
	}
	pool, err := pgxpool.New(context.Background(), c.dsn)
	if err != nil {
		log.Fatalf("open dsn: %v", err)
	}
	h := &harness{cfg: c, base: fmt.Sprintf("http://127.0.0.1:%d", c.port), pool: pool}
	defer pool.Close()
	f, err := os.OpenFile("/tmp/keel-chaos-runtime.log", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	h.logFile = f
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), c.duration+3*time.Minute)
	defer cancel()

	if err := h.start(ctx, "runtime-a", c.port); err != nil {
		h.fatalf("start runtime: %v", err)
	}
	defer h.stopAll()

	sessions := make([]*sessionState, c.sessions)
	for i := range sessions {
		s := &sessionState{}
		if err := h.newTenantSession(ctx, s); err != nil {
			h.fatalf("create session %d: %v", i, err)
		}
		sessions[i] = s
	}

	// Writers keep posting user input so there is always something in flight
	// when a process dies.
	writeCtx, stopWriters := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func(s *sessionState) {
			defer wg.Done()
			h.writeLoop(writeCtx, s)
		}(s)
	}

	// Fault injection.
	faults := time.NewTicker(c.killEvery)
	deadline := time.Now().Add(c.duration)
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-time.After(time.Until(deadline)):
			break loop
		case <-faults.C:
			if c.mode == "kill" {
				h.sigkillNewest()
				if err := h.start(ctx, h.nextRuntimeID(), h.curPort); err != nil {
					h.fatalf("restart: %v", err)
				}
			} else {
				h.handoffAndKillSuccessor(ctx)
			}
		}
	}
	faults.Stop()
	stopWriters()
	wg.Wait()

	// Quiet period: no more faults, let the last runtime settle everything.
	time.Sleep(c.settleFor)

	result := h.verify(ctx, sessions)
	raw, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(raw))
	if c.jsonOut != "" {
		_ = os.WriteFile(c.jsonOut, raw, 0o644)
	}
	if !result.Pass {
		// os.Exit skips defers: stop the runtimes before leaving, or the next
		// run finds the port held by a dead harness's child.
		h.stopAll()
		os.Exit(1)
	}
}

type result struct {
	Mode       string   `json:"mode"`
	Faults     int64    `json:"faults"`
	Restarts   int64    `json:"restarts"`
	Sessions   int      `json:"sessions"`
	Checks     []string `json:"checks"`
	Failures   []string `json:"failures"`
	Pass       bool     `json:"pass"`
	LastErrors []string `json:"lastErrors,omitempty"`
}

// --- process control ------------------------------------------------------

func (h *harness) nextRuntimeID() string {
	return fmt.Sprintf("runtime-%d", h.restarts.Load()+2)
}

func (h *harness) start(ctx context.Context, id string, port int) error {
	cmd := exec.Command(h.cfg.bin, "-addr", fmt.Sprintf("127.0.0.1:%d", port),
		"-cas", "/tmp/keel-chaos-cas", "-streams", "64", "-runs", "16")
	env := []string{}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "KEEL_") {
			continue
		}
		env = append(env, kv)
	}
	// A short lease keeps the fault cycle observable: after a SIGKILL the
	// session is unavailable only until the lease expires (that wait is the
	// design, not a bug — see docs/HANDOFF.md on the residual window).
	cmd.Env = append(env, "KEEL_DSN="+h.cfg.dsn, "KEEL_ADMIN_KEY="+h.cfg.adminKey,
		"KEEL_RUNTIME_ID="+id, "KEEL_FAKE_DELAY_MS=250", "KEEL_LEASE_MS=2000")
	cmd.Stdout = h.logFile
	cmd.Stderr = h.logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	proc := &runtimeProc{cmd: cmd, id: id}
	h.mu.Lock()
	h.procs = append(h.procs, proc)
	h.mu.Unlock()
	if err := h.waitHealthy(ctx, fmt.Sprintf("http://127.0.0.1:%d", port), cmd.Process.Pid); err != nil {
		return err
	}
	h.curPort = port
	h.base = fmt.Sprintf("http://127.0.0.1:%d", port)
	return nil
}

// waitHealthy waits until the runtime answers, and fails fast when the
// answering process is not the one just started (a stale runtime on the same
// port would make every later assertion meaningless).
func (h *harness) waitHealthy(ctx context.Context, base string, pid int) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, "GET", base+"/v1/health", nil)
		if res, err := http.DefaultClient.Do(req); err == nil {
			var body struct {
				PID int `json:"pid"`
			}
			_ = json.NewDecoder(res.Body).Decode(&body)
			res.Body.Close()
			if res.StatusCode == 200 {
				if body.PID != 0 && body.PID != pid {
					return fmt.Errorf("port %s answers with pid %d, not %d (stale runtime?)", base, body.PID, pid)
				}
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("runtime did not become healthy")
}

// sigkillNewest kills the newest runtime the way a machine failure would.
func (h *harness) sigkillNewest() {
	h.mu.Lock()
	proc := h.procs[len(h.procs)-1]
	h.mu.Unlock()
	_ = syscall.Kill(-proc.cmd.Process.Pid, syscall.SIGKILL)
	_ = proc.cmd.Wait()
	h.killed.Add(1)
	log.Printf("SIGKILL %s (pid %d)", proc.id, proc.cmd.Process.Pid)
	time.Sleep(200 * time.Millisecond)
}

// handoffAndKillSuccessor runs the split-brain scenario, one cycle:
//
//	old (port P, live) -> successor (port P+1, starts and claims)
//	-> old drained (handoff) and exits -> successor SIGKILLed right after it
//	started working -> a fresh runtime on P adopts the session (epoch jumps).
//
// Every cycle therefore ends with exactly one live, non-draining runtime on
// h.curPort, which is where the harness posts its input.
func (h *harness) handoffAndKillSuccessor(ctx context.Context) {
	old := h.current()
	if old == nil {
		return
	}
	oldBase := h.base
	nextPort := h.otherPort()
	successorID := h.nextRuntimeID()
	if err := h.start(ctx, successorID, nextPort); err != nil {
		log.Printf("handoff successor start: %v", err)
		return
	}
	h.restarts.Add(1)
	// Drain the predecessor through its own base.
	body, _ := json.Marshal(map[string]any{"mode": "handoff", "deadlineMs": 5000})
	req, _ := http.NewRequestWithContext(ctx, "POST", oldBase+"/v1/runtime/drain", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+h.cfg.adminKey)
	req.Header.Set("Content-Type", "application/json")
	drained := false
	for attempt := 0; attempt < 2 && !drained; attempt++ {
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("drain %s: %v", old.id, err)
			continue
		}
		code := res.StatusCode
		res.Body.Close()
		// 200: drain started; 409: already draining (a slow predecessor) — both
		// mean the handoff is in effect. Anything else means this cycle did not
		// inject the fault it claims to inject.
		drained = code == http.StatusOK || code == http.StatusConflict
		if !drained {
			log.Printf("drain %s: status %d", old.id, code)
		}
	}
	if !drained {
		log.Printf("handoff cycle skipped: the predecessor never accepted the drain")
		return
	}
	// Let the successor claim, then kill it hard: the next runtime must adopt
	// from a higher epoch without duplicating an answer.
	time.Sleep(1500 * time.Millisecond)
	h.sigkillNewest()
	// The predecessor exits once its drain completes; if it is still holding
	// its port after the drain window, kill it (a stuck old process must not
	// block the upgrade either). Only then is its port free for the fresh
	// successor.
	for i := 0; i < 80; i++ {
		if !processAlive(old.cmd.Process.Pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(old.cmd.Process.Pid) {
		_ = syscall.Kill(-old.cmd.Process.Pid, syscall.SIGKILL)
		_ = old.cmd.Wait()
	}
	finalPort := h.curPort
	finalID := h.nextRuntimeID()
	if err := h.start(ctx, finalID, finalPort); err != nil {
		log.Printf("fresh successor start: %v", err)
		return
	}
	h.pruneDead()
}

// processAlive reports whether the pid still exists (signal 0 probe).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// current returns the newest runtime the harness started.
func (h *harness) current() *runtimeProc {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.procs) == 0 {
		return nil
	}
	return h.procs[len(h.procs)-1]
}

// pruneDead drops the oldest process records, keeping the list bounded.
func (h *harness) pruneDead() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.procs) > 8 {
		h.procs = h.procs[len(h.procs)-4:]
	}
}

// fatalf stops every runtime this harness started before exiting: log.Fatal
// skips deferred cleanup, and a leaked runtime would hold the port (and the
// ledger's leases) for the next run.
func (h *harness) fatalf(format string, args ...any) {
	h.stopAll()
	if h.logFile != nil {
		h.logFile.Close()
	}
	log.Fatalf(format, args...)
}

func (h *harness) stopAll() {
	h.mu.Lock()
	procs := append([]*runtimeProc(nil), h.procs...)
	h.mu.Unlock()
	for _, p := range procs {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		_ = p.cmd.Wait()
	}
}

// otherPort is the second listen port: the handoff cycle alternates between
// curPort and its neighbour so a successor never has to wait for the old
// process to release its socket.
func (h *harness) otherPort() int {
	if h.curPort == h.cfg.port {
		return h.cfg.port + 1
	}
	return h.cfg.port
}

// runReadState reads the durable read facts the kernel keeps on the run row:
// how far the last attempt read and whether unread input is still flagged.
func (h *harness) runReadState(ctx context.Context, session string) (int64, bool, error) {
	var watermark int64
	var pending bool
	err := h.pool.QueryRow(ctx,
		`SELECT read_watermark, pending_input FROM runs
		 WHERE session_id = $1 ORDER BY updated_at DESC LIMIT 1`, session).
		Scan(&watermark, &pending)
	if err != nil {
		return 0, false, err
	}
	return watermark, pending, nil
}

// --- client work ----------------------------------------------------------

func (h *harness) newTenantSession(ctx context.Context, s *sessionState) error {
	tenant := randomUUID()
	s.tenant = tenant
	var out struct {
		TenantID string `json:"tenantId"`
	}
	if code, err := h.postAdmin(ctx, "/v1/tenants", tenant, map[string]string{"name": "chaos"}, &out); err != nil || code != 201 {
		return fmt.Errorf("create tenant: %d %v", code, err)
	}
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	if code, err := h.post(ctx, "/v1/sessions", tenant, map[string]string{}, &sess); err != nil || code != 201 {
		return fmt.Errorf("create session: %d %v", code, err)
	}
	s.session = sess.SessionID
	return nil
}

func (h *harness) writeLoop(ctx context.Context, s *sessionState) {
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(300+int(time.Now().UnixNano()%400)) * time.Millisecond):
		}
		n++
		requestID := fmt.Sprintf("%s-%d", s.session[:8], n)
		body := map[string]any{"text": fmt.Sprintf("chaos %d please echo this", n), "requestId": requestID}
		code, err := h.post(ctx, "/v1/sessions/"+s.session+"/input", s.tenant, body, nil)
		if err == nil && code == 201 {
			s.mu.Lock()
			s.requestID = append(s.requestID, requestID)
			s.mu.Unlock()
			s.posted.Add(1)
		}
	}
}

// postAdmin is post with the admin bearer token: tenant provisioning is an
// admin surface, the session and input endpoints are tenant-scoped.
func (h *harness) postAdmin(ctx context.Context, path, tenant string, body any, out any) (int, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", h.base+path, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.cfg.adminKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if out != nil {
		_ = json.NewDecoder(res.Body).Decode(out)
	} else {
		_, _ = io.Copy(io.Discard, res.Body)
	}
	return res.StatusCode, nil
}

func (h *harness) post(ctx context.Context, path, tenant string, body any, out any) (int, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", h.base+path, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if out != nil {
		_ = json.NewDecoder(res.Body).Decode(out)
	} else {
		_, _ = io.Copy(io.Discard, res.Body)
	}
	return res.StatusCode, nil
}

// --- verification ---------------------------------------------------------

type event struct {
	Seq     int64           `json:"seq"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

func (h *harness) verify(ctx context.Context, sessions []*sessionState) result {
	res := result{Mode: h.cfg.mode, Faults: h.killed.Load(), Restarts: h.restarts.Load(), Sessions: len(sessions)}
	fail := func(format string, args ...any) {
		res.Failures = append(res.Failures, fmt.Sprintf(format, args...))
	}
	for _, s := range sessions {
		events, err := h.loadEvents(ctx, s)
		if err != nil {
			fail("session %s: load events: %v", s.session, err)
			continue
		}
		// Contiguous seq from 1.
		for i, ev := range events {
			if ev.Seq != int64(i+1) {
				fail("session %s: seq gap at index %d: got %d", s.session, i, ev.Seq)
				break
			}
		}
		// Every posted requestId appears exactly once.
		seen := map[string]int{}
		for _, ev := range events {
			if ev.Kind != "user.message" {
				continue
			}
			var p struct {
				RequestID string `json:"requestId"`
			}
			_ = json.Unmarshal(ev.Payload, &p)
			seen[p.RequestID]++
		}
		s.mu.Lock()
		want := append([]string(nil), s.requestID...)
		s.mu.Unlock()
		for _, rid := range want {
			if seen[rid] != 1 {
				fail("session %s: requestId %s appears %d times (want exactly 1)", s.session, rid, seen[rid])
			}
		}
		// Anti-duplication, structurally: the log order cannot delineate a
		// "turn" — input that arrives mid-run sits before the previous
		// attempt's answer (see docs/HANDOFF.md and the B5 watermark), so
		// counting finals per user message produces false positives on a
		// correct log. What must hold instead: never more final (non-tool-call)
		// answers than the user messages that asked for them, and never an
		// orphan tool result.
		users, finals, calls, results := 0, 0, 0, 0
		for _, ev := range events {
			switch ev.Kind {
			case "user.message":
				users++
			case "tool.result":
				results++
			case "assistant.message":
				var m struct {
					Content []struct {
						Type string `json:"type"`
					} `json:"content"`
				}
				if err := json.Unmarshal(ev.Payload, &m); err != nil {
					fail("session %s: undecodable assistant payload at seq %d: %v", s.session, ev.Seq, err)
					continue
				}
				hasCall := false
				for _, c := range m.Content {
					if c.Type == "toolCall" {
						hasCall = true
						calls++
					}
				}
				if !hasCall {
					finals++
				}
			}
		}
		if finals > users {
			fail("session %s: %d final replies for %d user messages (duplicated answer)", s.session, finals, users)
		}
		if results > calls {
			fail("session %s: %d tool results for %d tool calls (orphan result)", s.session, results, calls)
		}
		// Per-input answer accounting. The fake provider quotes the input text in
		// the answer it gives ("fake provider received: <text>" or "tool echo
		// returned: <text>"), which makes two things observable:
		//   - an input answered TWICE is a duplicate final reply (the failure the
		//     crash-before-Finish window produced);
		//   - the NEWEST input must be answered, or the conversation did not
		//     converge on what the user last asked.
		// Earlier inputs are not required to have their own final answer: with
		// several unread messages in one transcript a model answers the newest
		// (and may act on the rest in the same turn), so demanding one reply per
		// message would encode a fake-provider artifact, not a product rule.
		// That every input was at least READ is asserted from the durable fact
		// below (read_watermark + pending_input), which is what the kernel
		// actually guarantees.
		type inputRef struct {
			text string
			seq  int64
		}
		finalsWithText := func(text string, afterSeq int64) int {
			n := 0
			for _, ev := range events {
				if ev.Kind != "assistant.message" || ev.Seq <= afterSeq {
					continue
				}
				var m struct {
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				}
				if err := json.Unmarshal(ev.Payload, &m); err != nil {
					continue
				}
				hasCall := false
				texts := ""
				for _, c := range m.Content {
					if c.Type == "toolCall" {
						hasCall = true
					}
					texts += c.Text
				}
				if !hasCall && strings.Contains(texts, text) {
					n++
				}
			}
			return n
		}
		var inputs []inputRef
		for _, ev := range events {
			if ev.Kind != "user.message" {
				continue
			}
			var p struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(ev.Payload, &p)
			if p.Text != "" {
				inputs = append(inputs, inputRef{text: p.Text, seq: ev.Seq})
			}
		}
		for _, in := range inputs {
			if got := finalsWithText(in.text, in.seq); got > 1 {
				fail("session %s: input %q (seq %d) has %d final answers (duplicated reply)", s.session, in.text, in.seq, got)
			}
		}
		if len(inputs) > 0 {
			last := inputs[len(inputs)-1]
			if finalsWithText(last.text, last.seq) != 1 {
				fail("session %s: the newest input %q (seq %d) has no final answer (conversation did not converge)", s.session, last.text, last.seq)
			}
		}
		// Durable read fact: after quiescence every committed input must be read
		// and nothing may still be flagged unread.
		if maxSeq, pending, err := h.runReadState(ctx, s.session); err != nil {
			fail("session %s: read state: %v", s.session, err)
		} else {
			if pending {
				fail("session %s: run still carries unread input after the quiet period", s.session)
			}
			var newestUser int64
			for _, in := range inputs {
				if in.seq > newestUser {
					newestUser = in.seq
				}
			}
			if maxSeq < newestUser {
				fail("session %s: read watermark %d does not cover the newest input %d", s.session, maxSeq, newestUser)
			}
		}
		// The run converges.
		var run struct {
			Status string `json:"status"`
		}
		if code, err := h.get(ctx, "/v1/sessions/"+s.session+"/run", s.tenant, &run); err != nil || code != 200 {
			fail("session %s: run status: %d %v", s.session, code, err)
			continue
		}
		switch run.Status {
		case "done", "pending", "failed":
		default:
			fail("session %s: run stuck in status %q after the quiet period", s.session, run.Status)
		}
	}
	if len(res.Failures) == 0 {
		res.Pass = true
		res.Checks = []string{
			"seq contiguous from 1 on every session",
			"every posted requestId committed exactly once",
			"newest input answered; no input answered twice", "read watermark covers every committed input", "no unread-input flag left behind",
			"no orphan tool results",
			"every run converged (done/pending/failed)",
		}
	}
	return res
}

func (h *harness) get(ctx context.Context, path, tenant string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", h.base+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Tenant-ID", tenant)
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

func (h *harness) loadEvents(ctx context.Context, s *sessionState) ([]event, error) {
	var out []event
	for {
		var page struct {
			Events []event `json:"events"`
		}
		after := int64(0)
		if len(out) > 0 {
			after = out[len(out)-1].Seq
		}
		code, err := h.get(ctx, fmt.Sprintf("/v1/sessions/%s/events?after=%d&limit=500", s.session, after), s.tenant, &page)
		if err != nil {
			return nil, err
		}
		if code != 200 {
			return nil, fmt.Errorf("status %d", code)
		}
		out = append(out, page.Events...)
		if len(page.Events) < 500 {
			return out, nil
		}
	}
}

// --- database -------------------------------------------------------------

func resetDatabase(ctx context.Context, dsn string) error {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse dsn: %w", err)
	}
	name := strings.ToLower(cfg.ConnConfig.Database)
	dedicated := strings.Contains(name, "e2e") || strings.Contains(name, "test") ||
		strings.Contains(name, "chaos") || strings.Contains(name, "smoke")
	if !dedicated {
		return fmt.Errorf("refusing to reset %q: the database name must look dedicated", cfg.ConnConfig.Database)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, "DROP SCHEMA public CASCADE"); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA public"); err != nil {
		return err
	}
	return nil
}

func randomUUID() string { return uuid.NewString() }
