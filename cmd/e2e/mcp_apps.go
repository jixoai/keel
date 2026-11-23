// MCP Apps acceptance scenario (change feat-mcp-apps-events, P6.1): G1 single-
// machine end to end, G2 crash matrix and G3 observability over a real
// `keel serve` process and a real stdio MCP server.
//
// The scenario owns the process lifecycle: it writes an mcp.json pointing at
// this binary's `fake-mcp-server` subcommand, starts keel serve, drives the
// public HarnessAPI, SIGKILLs the process group at provable points, restarts it,
// and asserts against the ledger and the HTTP surface of the new process.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	mcpAppsFixtureServer = "fixture"
	mcpAppsMIMEType      = "text/html;profile=mcp-app"
	mcpAppsLeaseMS       = "1000"
)

type mcpAppsEnv struct {
	keelBin  string
	dsn      string
	base     string
	port     int
	adminKey string
	tenant   string
	// botID is the Bot the scenario's sessions are owned by. Since A4.2b a
	// session without a Bot has no principal: its runs refuse to start
	// ("session is not owned by a bot and cannot run") and tool calls fail
	// closed. The scenario provisions one once per env (it survives restarts).
	botID  string
	work   string
	out    string
	phase  string
	dbName string
	// client carries the CSRF cookie pair; local-only auth still enforces the
	// double-submit token on writes. csrf is refreshed after every boot because
	// the process's ephemeral signing key changes across SIGKILL restarts.
	client     *http.Client
	csrf       string
	csrfCookie string

	cmd     *exec.Cmd
	logFile *os.File
	logPath string

	fakeEventsPath string
	startCount     int
	extraEnv       []string

	startedAt   time.Time
	killPoints  []map[string]any
	comparisons []map[string]any
}

func runMCPAppsScenario(keelBin, outDir, dsn string, port int) []result {
	env, err := newMCPAppsEnv(keelBin, outDir, dsn, port)
	if err != nil {
		return []result{{Name: "mcp-apps setup", Detail: err.Error()}}
	}
	defer env.cleanup()

	results := []result{}
	results = append(results, env.caseG1()...)
	if env.cmd != nil {
		env.stopProcess(false)
	}
	results = append(results, env.caseKillAfterAppFetch())
	if env.cmd != nil {
		env.stopProcess(false)
	}
	results = append(results, env.caseKillDuringListenReconnect())
	if env.cmd != nil {
		env.stopProcess(false)
	}
	results = append(results, env.caseKillBeforeToolResultCommit())
	env.stopProcess(true)
	cleanup := env.finalize()
	env.writeJSON("kill-points.json", env.killPoints)
	env.writeJSON("comparisons.json", env.comparisons)
	env.writeJSON("cleanup.json", cleanup)
	env.writeJSON("scenario.json", map[string]any{
		"scenario":   "mcp-apps",
		"keelBin":    env.keelBin,
		"database":   env.dbName,
		"port":       env.port,
		"startedAt":  env.startedAt.UTC().Format(time.RFC3339),
		"finishedAt": time.Now().UTC().Format(time.RFC3339),
		"results":    len(results),
	})
	env.writeJSON("assertions.json", results)
	return results
}

func newMCPAppsEnv(keelBin, outDir, dsn string, port int) (*mcpAppsEnv, error) {
	work, err := os.MkdirTemp("", "keel-mcp-apps-e2e-")
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{"cas", "plugins"} {
		if err := os.MkdirAll(filepath.Join(work, dir), 0o755); err != nil {
			return nil, err
		}
	}
	if keelBin == "" {
		keelBin = filepath.Join(work, "keel-serve")
		cmd := exec.Command("go", "build", "-o", keelBin, "./cmd/keel")
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("build keel serve: %w", err)
		}
	}
	if _, err := os.Stat(keelBin); err != nil {
		return nil, fmt.Errorf("keel binary %s: %w", keelBin, err)
	}
	if outDir != "" {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return nil, err
		}
	}
	adminKey := os.Getenv("KEEL_ADMIN_KEY")
	if adminKey == "" {
		adminKey = "mcp-apps-e2e-admin"
	}
	dbName := ""
	if cfg, err := pgxpool.ParseConfig(dsn); err == nil {
		dbName = cfg.ConnConfig.Database
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("cookie jar: %w", err)
	}
	env := &mcpAppsEnv{
		keelBin:   keelBin,
		dsn:       dsn,
		port:      port,
		base:      fmt.Sprintf("http://127.0.0.1:%d", port),
		adminKey:  adminKey,
		work:      work,
		out:       outDir,
		dbName:    dbName,
		client:    &http.Client{Jar: jar},
		startedAt: time.Now(),
	}
	// The scenario uses a dedicated port; a stale listener would silently make
	// every assertion run against the wrong process.
	if err := requirePortFree(port); err != nil {
		env.cleanup()
		return nil, err
	}
	return env, nil
}

func requirePortFree(port int) error {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err == nil {
		conn.Close()
		return fmt.Errorf("port %d is already in use", port)
	}
	return nil
}

func (e *mcpAppsEnv) cleanup() {
	e.stopProcess(false)
	if e.work != "" {
		_ = os.RemoveAll(e.work)
	}
}

// envWithoutKeelConfig removes ambient KEEL_* deployment settings (.env.local
// may still carry the retired KEEL_SERVE_PASSWORD) so the harness starts a
// deterministic loopback-local-admin, fake-provider process; the scenario's own
// values are appended.
func envWithoutKeelConfig() []string {
	drop := []string{
		"KEEL_DSN=", "KEEL_SERVE_TENANT=", "KEEL_SERVE_PASSWORD=", "KEEL_SERVE_ADDR=",
		"KEEL_AUTH=", "KEEL_INSECURE=", "KEEL_SECRET_KEY=",
		"KEEL_MCP_CONFIG=", "KEEL_MCP_PROJECT=", "KEEL_MCP_PROTOCOL_ERA=",
		"KEEL_ADMIN_KEY=", "KEEL_FAKE_DELAY_MS=", "KEEL_TOOL_DELAY_MS=",
		"KEEL_LEASE_MS=", "KEEL_MODELS_CONFIG=", "KEEL_PROVIDER=", "KEEL_MODEL=",
		"KEEL_BASE_URL=", "KEEL_PROTOCOL=", "KEEL_API_KEY=",
	}
	out := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		skip := false
		for _, prefix := range drop {
			if strings.HasPrefix(entry, prefix) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, entry)
		}
	}
	return out
}

// beginCase gives the next process pair its own tenant, so the audit scan of
// one crash point never sees another case's bridge rows. The app catalogue is
// process-global (the deployment has no tenant scope), while sessions and
// audit rows are tenant-scoped.
func (e *mcpAppsEnv) beginCase() {
	e.tenant = uuid.NewString()
	// The Bot belongs to the previous case's tenant; the next start
	// provisions one for the new tenant.
	e.botID = ""
}

// start launches one keel serve process with a fresh mcp.json and a fresh fake
// server marker file. phase names the evidence/log files; scenario selects the
// fake server behaviour (static|evolve|reconnect|flap).
func (e *mcpAppsEnv) start(phase, scenario string, toolDelayMS int, extra ...string) error {
	if e.cmd != nil {
		return fmt.Errorf("start %s: a keel process is already running", phase)
	}
	e.startCount++
	e.phase = phase
	e.extraEnv = append([]string(nil), extra...)
	e.fakeEventsPath = filepath.Join(e.work, fmt.Sprintf("fake-events-%s-%d.jsonl", phase, e.startCount))
	e.logPath = filepath.Join(e.work, fmt.Sprintf("keel-%s-%d.log.txt", phase, e.startCount))
	if e.out != "" {
		e.logPath = filepath.Join(e.out, fmt.Sprintf("keel-%s-%d.log.txt", phase, e.startCount))
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"fake-mcp-server", "-scenario", scenario, "-events", e.fakeEventsPath,
		"-tool-delay-ms", strconv.Itoa(toolDelayMS)}
	mcpConfig := map[string]any{
		"mcpServers": map[string]any{
			mcpAppsFixtureServer: map[string]any{
				"command": exe,
				"args":    args,
				"timeout": 30,
			},
		},
	}
	raw, err := json.MarshalIndent(mcpConfig, "", "  ")
	if err != nil {
		return err
	}
	mcpConfigPath := filepath.Join(e.work, "mcp.json")
	if err := os.WriteFile(mcpConfigPath, raw, 0o644); err != nil {
		return err
	}
	logFile, err := os.OpenFile(e.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	// Each case gets its own tenant at process start. local-only maps loopback
	// requests to the built-in local-admin super admin and strips any client
	// X-Tenant-ID, so the tenant must be the process tenant rather than a
	// per-request header (the old no-password boot accepted neither model).
	if e.tenant == "" {
		e.tenant = uuid.NewString()
	}
	cmd := exec.Command(e.keelBin, "serve",
		"-addr", fmt.Sprintf("127.0.0.1:%d", e.port),
		"-dsn", e.dsn,
		"-cas", filepath.Join(e.work, "cas"),
		"-web-dir", filepath.Join(e.work, "web-missing"),
		"-plugins-root", filepath.Join(e.work, "plugins"),
		"-auth", "local-only",
		"-tenant", e.tenant,
		"-mcp", mcpConfigPath,
		"-runs", "8",
	)
	cmd.Env = append(envWithoutKeelConfig(),
		"KEEL_DSN="+e.dsn,
		"KEEL_ADMIN_KEY="+e.adminKey,
		"KEEL_MCP_PROTOCOL_ERA=modern",
	)
	cmd.Env = append(cmd.Env, e.extraEnv...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return err
	}
	e.cmd = cmd
	e.logFile = logFile
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			break
		}
		res, err := http.Get(e.base + "/v1/health")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				// local-only provisions the tenant and its local-admin identity
				// during boot; there is no separate tenant write to make. The
				// CSRF pair is per-process (ephemeral signing key), so every
				// start refreshes it before the scenario writes.
				if err := e.refreshCSRF(context.Background()); err != nil {
					e.kill9()
					return fmt.Errorf("refresh csrf: %w", err)
				}
				if err := e.ensureBot(context.Background()); err != nil {
					e.kill9()
					return fmt.Errorf("provision bot: %w", err)
				}
				e.writeText(fmt.Sprintf("start-%s-%d.txt", phase, e.startCount),
					fmt.Sprintf("pid=%d\ntenant=%s\nphase=%s\nscenario=%s\ntoolDelayMs=%d\nfakeEvents=%s\nlog=%s\n",
						cmd.Process.Pid, e.tenant, phase, scenario, toolDelayMS, e.fakeEventsPath, e.logPath))
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	tail := e.tailLog(40)
	e.kill9()
	return fmt.Errorf("keel %s did not become healthy; log tail:\n%s", phase, tail)
}

// killAndRecord performs the real SIGKILL (process group) and freezes the
// evidence that makes the crash point provable: the pid, the signal, the
// caller's proof (markers/ledger state) and the server log tail.
func (e *mcpAppsEnv) killAndRecord(point string, proof map[string]any) {
	if e.cmd == nil {
		e.killPoints = append(e.killPoints, map[string]any{
			"point": point, "error": "no process was running", "at": time.Now().UTC().Format(time.RFC3339Nano),
		})
		return
	}
	pid := e.cmd.Process.Pid
	entry := map[string]any{
		"point":        point,
		"pid":          pid,
		"signal":       "SIGKILL",
		"processGroup": true,
		"at":           time.Now().UTC().Format(time.RFC3339Nano),
		"proof":        proof,
		"logTail":      e.tailLog(12),
	}
	err := syscall.Kill(-pid, syscall.SIGKILL)
	entry["killError"] = nil
	if err != nil {
		entry["killError"] = err.Error()
	}
	e.killPoints = append(e.killPoints, entry)
	e.kill9()
}

// finalize records the post-run custody facts: the port is free, no scenario
// process survives, and which (dedicated, guarded) database was used.
func (e *mcpAppsEnv) finalize() map[string]any {
	portFree := requirePortFree(e.port) == nil
	workRemoved := false
	if e.work != "" {
		workRemoved = os.RemoveAll(e.work) == nil
		e.work = ""
	}
	return map[string]any{
		"port":           e.port,
		"portFree":       portFree,
		"processAlive":   e.cmd != nil,
		"database":       e.dbName,
		"resetGuard":     strings.Contains(strings.ToLower(e.dbName), "e2e") || strings.Contains(strings.ToLower(e.dbName), "test"),
		"workDirRemoved": workRemoved,
	}
}

// addComparison records the before/after ledger sequences of one kill point.
func (e *mcpAppsEnv) addComparison(point string, before, after []event, beforeCatalog, afterCatalog any) {
	e.comparisons = append(e.comparisons, map[string]any{
		"point":         point,
		"seqBefore":     seqsOf(before),
		"seqAfter":      seqsOf(after),
		"kindsBefore":   kindsOf(before),
		"kindsAfter":    kindsOf(after),
		"catalogBefore": beforeCatalog,
		"catalogAfter":  afterCatalog,
	})
}

func seqsOf(events []event) []int64 {
	seqs := make([]int64, len(events))
	for i, ev := range events {
		seqs[i] = ev.Seq
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	return seqs
}

func kindsOf(events []event) []string {
	kinds := make([]string, 0, len(events))
	for _, ev := range events {
		kinds = append(kinds, fmt.Sprintf("%d:%s", ev.Seq, ev.Kind))
	}
	return kinds
}

func (e *mcpAppsEnv) kill9() {
	if e.cmd == nil {
		return
	}
	_ = syscall.Kill(-e.cmd.Process.Pid, syscall.SIGKILL)
	_ = e.cmd.Wait()
	e.cmd = nil
	if e.logFile != nil {
		e.logFile.Close()
		e.logFile = nil
	}
	time.Sleep(250 * time.Millisecond)
}

func (e *mcpAppsEnv) stopProcess(graceful bool) {
	if e.cmd == nil {
		return
	}
	pid := e.cmd.Process.Pid
	if graceful {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	} else {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	done := make(chan struct{})
	go func() { _ = e.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-done
	}
	e.cmd = nil
	if e.logFile != nil {
		e.logFile.Close()
		e.logFile = nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := requirePortFree(e.port); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (e *mcpAppsEnv) tailLog(lines int) string {
	raw, err := os.ReadFile(e.logPath)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}

func (e *mcpAppsEnv) writeJSON(name string, value any) {
	if e.out == "" {
		return
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(e.out, name), append(raw, '\n'), 0o644)
}

func (e *mcpAppsEnv) writeText(name, value string) {
	if e.out == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(e.out, name), []byte(value), 0o644)
}

func (e *mcpAppsEnv) copyFakeEvents(name string) {
	if e.out == "" || e.fakeEventsPath == "" {
		return
	}
	raw, err := os.ReadFile(e.fakeEventsPath)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(e.out, name), raw, 0o644)
}

// --- fake server markers ---------------------------------------------------

type fakeMarker struct {
	Event   string `json:"event"`
	PID     int    `json:"pid"`
	Name    string `json:"name"`
	URI     string `json:"uri"`
	N       int    `json:"n"`
	Version int    `json:"version"`
	Text    string `json:"text"`
	Raw     string `json:"-"`
}

func readFakeMarkers(path string) ([]fakeMarker, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()
	var markers []fakeMarker
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var marker fakeMarker
		if err := json.Unmarshal([]byte(line), &marker); err != nil {
			continue
		}
		marker.Raw = line
		markers = append(markers, marker)
	}
	return markers, scanner.Err()
}

func (e *mcpAppsEnv) waitFakeMarker(pred func(fakeMarker) bool, timeout time.Duration, description string) ([]fakeMarker, error) {
	deadline := time.Now().Add(timeout)
	var last []fakeMarker
	for time.Now().Before(deadline) {
		markers, err := readFakeMarkers(e.fakeEventsPath)
		if err == nil {
			last = markers
			for _, marker := range markers {
				if pred(marker) {
					return markers, nil
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return last, fmt.Errorf("timeout waiting for fake marker: %s", description)
}

func markersHave(markers []fakeMarker, pred func(fakeMarker) bool) bool {
	for _, marker := range markers {
		if pred(marker) {
			return true
		}
	}
	return false
}

func countMarkers(markers []fakeMarker, pred func(fakeMarker) bool) int {
	count := 0
	for _, marker := range markers {
		if pred(marker) {
			count++
		}
	}
	return count
}

// --- HTTP helpers ----------------------------------------------------------

// refreshCSRF reads the double-submit pair. local-only authenticates the
// request as the built-in local admin, but non-GET requests still require the
// token. The cookie is carried explicitly because Go's jar refuses Secure
// cookies over plain-HTTP loopback.
func (e *mcpAppsEnv) refreshCSRF(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.base+"/v1/auth/session", nil)
	if err != nil {
		return err
	}
	res, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /v1/auth/session: HTTP %d", res.StatusCode)
	}
	var body struct {
		CSRF string `json:"csrfToken"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("decode /v1/auth/session: %w", err)
	}
	if body.CSRF == "" {
		return fmt.Errorf("GET /v1/auth/session returned no csrfToken")
	}
	e.csrf = body.CSRF
	// The cookie is Secure; Go's jar refuses to send it over plain-HTTP
	// loopback (unlike Chrome/curl), so carry the value explicitly.
	e.csrfCookie = ""
	for _, cookie := range res.Cookies() {
		if cookie.Name == "keel_csrf" {
			e.csrfCookie = cookie.Value
			break
		}
	}
	if e.csrfCookie == "" {
		return fmt.Errorf("GET /v1/auth/session set no keel_csrf cookie")
	}
	return nil
}

func (e *mcpAppsEnv) request(ctx context.Context, method, path, tenant string, body any) (int, http.Header, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.base+path, reader)
	if err != nil {
		return 0, nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && method != http.MethodHead && e.csrf != "" {
		req.Header.Set("X-Keel-CSRF", e.csrf)
		req.AddCookie(&http.Cookie{Name: "keel_csrf", Value: e.csrfCookie})
	}
	if tenant != "" {
		req.Header.Set("X-Tenant-ID", tenant)
	}
	res, err := e.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, payload, err
}

func (e *mcpAppsEnv) getJSON(ctx context.Context, path, tenant string, out any) (int, error) {
	code, _, raw, err := e.request(ctx, http.MethodGet, path, tenant, nil)
	if err != nil {
		return 0, err
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return code, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return code, nil
}

func (e *mcpAppsEnv) postJSON(ctx context.Context, path, tenant string, body, out any) (int, error) {
	code, _, raw, err := e.request(ctx, http.MethodPost, path, tenant, body)
	if err != nil {
		return 0, err
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return code, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return code, nil
}

func (e *mcpAppsEnv) createTenant(ctx context.Context, tenant, name string) (int, error) {
	raw, _ := json.Marshal(map[string]string{"name": name})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.base+"/v1/tenants", bytes.NewReader(raw))
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

func (e *mcpAppsEnv) newSession(ctx context.Context) (string, string, error) {
	if e.tenant == "" {
		return "", "", fmt.Errorf("the scenario tenant is not provisioned")
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if code, err := e.postJSON(ctx, "/v1/sessions", e.tenant, map[string]string{"botId": e.botID}, &out); err != nil || code != 201 {
		return "", "", fmt.Errorf("create session: %d %v", code, err)
	}
	return e.tenant, out.SessionID, nil
}

// ensureBot provisions the identity ladder a Bot-owned session needs:
// account→role→profile→bot (change keel-bot-identity-access, A4.2b). The Bot
// is owned by the built-in local admin, so the run's tool authorizer resolves
// the super-admin grant set narrowed by the Bot's Harness permissions. Idempotent
// within one env: the object survives process restarts in the same database.
func (e *mcpAppsEnv) ensureBot(ctx context.Context) error {
	if e.botID != "" {
		return nil
	}
	const botID = "mcp-apps-bot"
	var role struct {
		Role struct {
			ID string `json:"roleId"`
		} `json:"role"`
	}
	if code, err := e.postJSON(ctx, "/v1/roles", e.tenant, map[string]string{"name": "mcp-apps-operator"}, &role); err != nil || code != 201 {
		return fmt.Errorf("create role: %d %v", code, err)
	}
	var profile struct {
		Profile struct {
			ID string `json:"profileId"`
		} `json:"profile"`
	}
	if code, err := e.postJSON(ctx, "/v1/profiles", e.tenant, map[string]string{
		"accountId": localAdminAccountID, "roleId": role.Role.ID,
	}, &profile); err != nil || code != 201 {
		return fmt.Errorf("create profile: %d %v", code, err)
	}
	var bot struct {
		Bot struct {
			ID string `json:"botId"`
		} `json:"bot"`
	}
	if code, err := e.postJSON(ctx, "/v1/bots", e.tenant, map[string]any{
		"botId": botID, "name": "MCP Apps Bot", "profileId": profile.Profile.ID,
		"modelKind":    "text",
		"defaultModel": map[string]string{"providerId": "fake", "modelId": "fake"},
		"harness": map[string]any{
			"tools":   []string{"mcp"},
			"context": map[string]any{"permissions": []string{"mcp.use", "mcp.read"}},
		},
	}, &bot); err != nil || code != 201 {
		return fmt.Errorf("create bot: %d %v", code, err)
	}
	e.botID = bot.Bot.ID
	// A Bot-owned session's principal carries the Bot's Harness narrowing but
	// NOT the account's super-admin shortcut (accesswire.SessionPrincipal sets
	// IsSuperAdmin only for the legacy account-owned branch), so the grant keys
	// the model needs must exist as real grants on the Bot's owner account.
	var group struct {
		Group struct {
			ID string `json:"groupId"`
		} `json:"group"`
	}
	if code, err := e.postJSON(ctx, "/v1/permission-groups", e.tenant, map[string]string{"name": "mcp-apps-grants"}, &group); err != nil || code != 201 {
		return fmt.Errorf("create permission group: %d %v", code, err)
	}
	for _, grant := range [][2]string{{"mcp.use", "tenant"}, {"mcp.read", "tenant"}} {
		if code, err := e.postJSON(ctx, "/v1/permission-groups/"+group.Group.ID+"/grants", e.tenant,
			map[string]string{"key": grant[0], "scope": grant[1]}, nil); err != nil || code != 200 {
			return fmt.Errorf("add grant %s: %d %v", grant[0], code, err)
		}
	}
	if code, err := e.postJSON(ctx, "/v1/permission-groups/"+group.Group.ID+"/members", e.tenant,
		map[string]string{"accountId": localAdminAccountID}, nil); err != nil || code != 200 {
		return fmt.Errorf("add grant member: %d %v", code, err)
	}
	return nil
}

// localAdminAccountID is cmd/keel's built-in local-only administrator
// (cmd/keel/auth.go localAdminIDString).
const localAdminAccountID = "00000000-0000-0000-0000-00000000a002"

// --- shared response types ------------------------------------------------

type mcpCatalogResponse struct {
	Generation  int64             `json:"generation"`
	Apps        []mcpCatalogApp   `json:"apps"`
	Diagnostics []json.RawMessage `json:"diagnostics,omitempty"`
}

type mcpCatalogApp struct {
	AppID       string   `json:"appId"`
	Server      string   `json:"server"`
	Title       string   `json:"title,omitempty"`
	ResourceURI string   `json:"resourceUri"`
	MIMEType    string   `json:"mimeType"`
	Visibility  []string `json:"visibility"`
	Tools       []struct {
		Tool       string   `json:"tool"`
		Visibility []string `json:"visibility"`
	} `json:"tools"`
	CSP  string `json:"csp"`
	Hash string `json:"hash"`
}

type mcpServersResponse struct {
	Generation int64 `json:"generation"`
	Servers    []struct {
		Name            string `json:"name"`
		Transport       string `json:"transport"`
		ProtocolVersion string `json:"protocolVersion"`
		Era             string `json:"era"`
		Connected       bool   `json:"connected"`
		Tools           int    `json:"tools"`
		Apps            int    `json:"apps"`
		Error           string `json:"error,omitempty"`
	} `json:"servers"`
}

func (e *mcpAppsEnv) waitCatalog(ctx context.Context, pred func(mcpCatalogResponse) bool, timeout time.Duration) (mcpCatalogResponse, error) {
	deadline := time.Now().Add(timeout)
	var last mcpCatalogResponse
	for time.Now().Before(deadline) {
		var body mcpCatalogResponse
		if code, err := e.getJSON(ctx, "/v1/mcp/apps", e.tenant, &body); err == nil && code == 200 {
			last = body
			if pred(body) {
				return body, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return last, fmt.Errorf("timeout waiting for the app catalogue (generation=%d apps=%d)", last.Generation, len(last.Apps))
}

func appByID(catalog mcpCatalogResponse, appID string) (mcpCatalogApp, bool) {
	for _, app := range catalog.Apps {
		if app.AppID == appID {
			return app, true
		}
	}
	return mcpCatalogApp{}, false
}

// --- G1 + G3 ---------------------------------------------------------------

func (e *mcpAppsEnv) caseG1() []result {
	ctx := context.Background()
	var results []result
	fail := func(name string, format string, args ...any) []result {
		results = append(results, result{Name: name, Detail: fmt.Sprintf(format, args...)})
		return results
	}

	e.beginCase()
	if err := e.start("g1", "evolve", 0); err != nil {
		return fail("G1 single-machine end to end", "start: %v", err)
	}
	defer e.stopProcess(true)

	// The document fetch happens during boot, before the server listens. The
	// catalogue is the machine-readable proof that a real resources/read ran.
	catalog, err := e.waitCatalog(ctx, func(body mcpCatalogResponse) bool {
		return len(body.Apps) == 1 && body.Apps[0].ResourceURI == "ui://scenario/dashboard"
	}, 20*time.Second)
	if err != nil {
		return fail("G1 single-machine end to end", "app catalogue never published: %v", err)
	}
	appV1 := catalog.Apps[0]
	e.writeJSON("catalog-before.json", catalog)
	if appV1.AppID != mcpAppID("fixture", "ui://scenario/dashboard") {
		return fail("G1 single-machine end to end", "appId = %s, want the derived identity", appV1.AppID)
	}
	checks := []string{
		"app catalogue published once from a real resources/read",
		fmt.Sprintf("appId=%s hash=%s", appV1.AppID, appV1.Hash),
	}

	// The app tool appears in the model tool surface and the catalogue, while
	// the app-only tool stays out of the model's aggregate listing.
	var servers mcpServersResponse
	if code, err := e.getJSON(ctx, "/v1/mcp/servers", e.tenant, &servers); err != nil || code != 200 {
		return fail("G1 single-machine end to end", "servers: code=%d err=%v", code, err)
	}
	if len(servers.Servers) != 1 {
		return fail("G1 single-machine end to end", "servers = %d, want 1", len(servers.Servers))
	}
	server := servers.Servers[0]
	if server.Name != mcpAppsFixtureServer || !server.Connected || server.Era != "modern" ||
		server.ProtocolVersion != "2026-07-28" || server.Tools != 3 || server.Apps != 1 {
		return fail("G1 single-machine end to end", "server projection = %+v", server)
	}
	if servers.Generation != catalog.Generation {
		return fail("G1 single-machine end to end", "servers generation %d != catalogue %d", servers.Generation, catalog.Generation)
	}
	checks = append(checks, "server connected over modern era (2026-07-28), tools=3 apps=1")
	e.writeJSON("servers-before.json", servers)

	tenant, session, err := e.newSession(ctx)
	if err != nil {
		return fail("G1 single-machine end to end", "%v", err)
	}
	if code, err := e.postJSON(ctx, "/v1/sessions/"+session+"/input", tenant,
		map[string]string{"text": "please mcp-list", "requestId": "g1-list"}, nil); err != nil || code != 201 {
		return fail("G1 single-machine end to end", "list input: code=%d err=%v", code, err)
	}
	listEvents, err := e.waitEventsFor(ctx, tenant, session, func(events []event) bool {
		return hasFinalRun(events) && countKind(events, "tool.result") >= 1
	}, 20*time.Second)
	if err != nil {
		return fail("G1 single-machine end to end", "model tool listing: %v", err)
	}
	listResult := toolResultPayloads(listEvents)
	if len(listResult) != 1 {
		return fail("G1 single-machine end to end", "mcp list tool results = %d, want 1", len(listResult))
	}
	listText := toolResultText(listResult[0])
	if !strings.Contains(listText, "show_dashboard") || !strings.Contains(listText, "plain") {
		return fail("G1 single-machine end to end", "model tool listing = %q (missing model-visible tools)", truncate(listText, 600))
	}
	if strings.Contains(listText, `"refresh"`) || strings.Contains(listText, "refresh") {
		return fail("G1 single-machine end to end", "app-only tool refresh leaked into the model listing: %q", truncate(listText, 600))
	}
	checks = append(checks, "aggregate mcp list shows show_dashboard/plain and hides app-only refresh")
	e.writeJSON("model-tool-list.json", listResult[0])

	// /view: the document body, the four security headers and the immutable
	// hash identity. A stale hash is not served as new content.
	code, headers, body, err := e.request(ctx, http.MethodGet, "/v1/mcp/apps/"+appV1.AppID+"/view", e.tenant, nil)
	if err != nil || code != 200 {
		return fail("G1 single-machine end to end", "bare view: code=%d err=%v", code, err)
	}
	if !bytes.Contains(body, []byte(`Fixture dashboard v1`)) {
		return fail("G1 single-machine end to end", "view body is not the fetched document: %q", truncate(string(body), 300))
	}
	if headers.Get("Content-Type") != "text/html; charset=utf-8" || headers.Get("X-MCP-App-Mime") != mcpAppsMIMEType ||
		headers.Get("X-MCP-App-Hash") != appV1.Hash || headers.Get("X-Content-Type-Options") != "nosniff" ||
		headers.Get("Referrer-Policy") != "no-referrer" || headers.Get("Cache-Control") != "no-store" {
		return fail("G1 single-machine end to end", "bare view headers = %v", headers)
	}
	csp := headers.Get("Content-Security-Policy")
	for _, required := range []string{
		"sandbox allow-scripts", "default-src 'none'", "connect-src 'self' https://api.fixture.test",
		"frame-ancestors 'self'", "object-src 'none'", "script-src 'self' 'unsafe-inline'",
	} {
		if !strings.Contains(csp, required) {
			return fail("G1 single-machine end to end", "CSP %q misses %q", csp, required)
		}
	}
	code, headers, body, err = e.request(ctx, http.MethodGet,
		"/v1/mcp/apps/"+appV1.AppID+"/view?v="+appV1.Hash, e.tenant, nil)
	if err != nil || code != 200 || !bytes.Contains(body, []byte(`Fixture dashboard v1`)) ||
		headers.Get("Cache-Control") != "public, max-age=31536000, immutable" ||
		headers.Get("X-MCP-App-Hash") != appV1.Hash {
		return fail("G1 single-machine end to end", "hash view: code=%d err=%v headers=%v", code, err, headers)
	}
	if code, _, _, _ := e.request(ctx, http.MethodGet,
		"/v1/mcp/apps/"+appV1.AppID+"/view?v="+strings.Repeat("0", 64), e.tenant, nil); code != 404 {
		return fail("G1 single-machine end to end", "stale hash answered %d, want 404", code)
	}
	checks = append(checks, "view serves the fetched HTML with CSP/nosniff/hash and immutable cache")
	viewsDoc := map[string]any{
		"appId": appV1.AppID, "hash": appV1.Hash, "csp": csp,
		"headers": map[string]string{
			"contentType": headers.Get("Content-Type"), "cacheControl": headers.Get("Cache-Control"),
			"nosniff": headers.Get("X-Content-Type-Options"), "referrerPolicy": headers.Get("Referrer-Policy"),
		},
		"bodyPrefix": truncate(string(body), 300),
	}
	e.writeJSON("view.json", viewsDoc)

	// The app bridge proxies tools/call and the real MCP server observes it.
	var callResult struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	callBody := map[string]any{
		"sessionId": session, "requestId": "g1-proxy-call", "name": "refresh", "arguments": map[string]any{},
	}
	if code, err := e.postJSON(ctx, "/v1/mcp/apps/"+appV1.AppID+"/tools/call", tenant, callBody, &callResult); err != nil || code != 200 {
		return fail("G1 single-machine end to end", "app tools/call: code=%d err=%v", code, err)
	}
	if callResult.IsError || len(callResult.Content) != 1 || callResult.Content[0].Text == "" ||
		!strings.Contains(callResult.Content[0].Text, "refresh-ok") {
		return fail("G1 single-machine end to end", "app tools/call result = %+v", callResult)
	}
	proxyMarkers, err := e.waitFakeMarker(func(marker fakeMarker) bool {
		return marker.Event == "tools_call_end" && marker.Name == "refresh"
	}, 5*time.Second, "the MCP server saw the app bridge tools/call")
	if err != nil {
		return fail("G1 single-machine end to end", "%v", err)
	}
	checks = append(checks, "app bridge tools/call reached the real MCP server (refresh-ok)")
	e.writeJSON("proxy-call.json", map[string]any{"response": callResult, "markers": proxyMarkers})

	// tools/list_changed: the fake server flips to a v2 catalogue with a new
	// resource URI. The next generation must contain only the new app; the old
	// app is not a stale snapshot a client could still mount.
	baselineGeneration := catalog.Generation
	if _, err := e.waitFakeMarker(func(marker fakeMarker) bool {
		return marker.Event == "tools_list_changed_sent" && marker.Version == 2
	}, 15*time.Second, "the fake server sent tools/list_changed"); err != nil {
		return fail("G1 single-machine end to end", "%v", err)
	}
	evolved, err := e.waitCatalog(ctx, func(body mcpCatalogResponse) bool {
		return len(body.Apps) == 1 && body.Apps[0].ResourceURI == "ui://scenario/dashboard_v2" &&
			body.Generation > baselineGeneration
	}, 15*time.Second)
	if err != nil {
		return fail("G1 single-machine end to end", "post-list_changed catalogue: %v", err)
	}
	appV2 := evolved.Apps[0]
	if appV2.AppID == appV1.AppID {
		return fail("G1 single-machine end to end", "appId did not change across the catalogue generation")
	}
	oldApps := 0
	for _, app := range evolved.Apps {
		if app.AppID == appV1.AppID {
			oldApps++
		}
	}
	if oldApps != 0 {
		return fail("G1 single-machine end to end", "the old app survived the republish")
	}
	if code, _, _, _ := e.request(ctx, http.MethodGet, "/v1/mcp/apps/"+appV1.AppID+"/view", e.tenant, nil); code != 404 {
		return fail("G1 single-machine end to end", "old app view answered %d after the republish, want 404", code)
	}
	code, _, v2Body, err := e.request(ctx, http.MethodGet, "/v1/mcp/apps/"+appV2.AppID+"/view", e.tenant, nil)
	if err != nil || code != 200 || !bytes.Contains(v2Body, []byte(`Fixture dashboard v2`)) {
		return fail("G1 single-machine end to end", "new app view: code=%d err=%v", code, err)
	}
	markers, _ := readFakeMarkers(e.fakeEventsPath)
	if !markersHave(markers, func(marker fakeMarker) bool {
		return marker.Event == "resource_read" && marker.URI == "ui://scenario/dashboard_v2"
	}) {
		return fail("G1 single-machine end to end", "the v2 document was never fetched")
	}
	// The model tool surface advances too: the v2 catalogue adds show_audit.
	var serversV2 mcpServersResponse
	if code, err := e.getJSON(ctx, "/v1/mcp/servers", e.tenant, &serversV2); err != nil || code != 200 {
		return fail("G1 single-machine end to end", "servers after list_changed: code=%d err=%v", code, err)
	}
	if len(serversV2.Servers) != 1 || serversV2.Servers[0].Tools != 4 || serversV2.Servers[0].Apps != 1 ||
		serversV2.Generation != evolved.Generation {
		return fail("G1 single-machine end to end", "post-list_changed server projection = %+v", serversV2)
	}
	if code, err := e.postJSON(ctx, "/v1/sessions/"+session+"/input", tenant,
		map[string]string{"text": "please mcp-list v2", "requestId": "g1-list-v2"}, nil); err != nil || code != 201 {
		return fail("G1 single-machine end to end", "v2 list input: code=%d err=%v", code, err)
	}
	listEventsV2, err := e.waitEventsFor(ctx, tenant, session, func(events []event) bool {
		return hasFinalRun(events) && countKind(events, "tool.result") >= 2
	}, 20*time.Second)
	if err != nil {
		return fail("G1 single-machine end to end", "v2 model tool listing: %v", err)
	}
	resultsV2 := toolResultPayloads(listEventsV2)
	if len(resultsV2) != 2 {
		return fail("G1 single-machine end to end", "tool results after the republish = %d, want 2", len(resultsV2))
	}
	listTextV2 := toolResultText(resultsV2[len(resultsV2)-1])
	if !strings.Contains(listTextV2, "show_audit") || strings.Contains(listTextV2, "refresh") {
		return fail("G1 single-machine end to end", "v2 model tool listing = %q", truncate(listTextV2, 600))
	}
	checks = append(checks, "the next attempt's model tool list contains the v2-only show_audit tool")
	e.writeJSON("model-tool-list-v2.json", map[string]any{
		"servers": serversV2, "result": resultsV2[len(resultsV2)-1],
	})
	checks = append(checks, fmt.Sprintf("tools/list_changed advanced generation %d→%d and replaced the app without a stale snapshot",
		baselineGeneration, evolved.Generation))
	e.writeJSON("catalog-after.json", evolved)
	e.copyFakeEvents("fake-events-g1.jsonl")

	results = append(results, result{
		Name:   "G1 single-machine end to end",
		Pass:   true,
		Detail: "catalogue + view + proxy call + list_changed generation over real keel serve and a real stdio MCP server",
		Checks: checks,
	})

	// G3: fixed-cardinality metrics and an identifier-only audit trail.
	g3 := e.caseG3(ctx, tenant, session, appV2.AppID,
		map[string]bool{appV1.AppID: true, appV2.AppID: true})
	results = append(results, g3)
	return results
}

// --- G3 --------------------------------------------------------------------

func (e *mcpAppsEnv) caseG3(ctx context.Context, tenant, session, appID string, knownApps map[string]bool) result {
	name := "G3 observability: fixed series and identifier-only audit"
	checks := []string{}
	fail := func(format string, args ...any) result {
		return result{Name: name, Detail: fmt.Sprintf(format, args...), Checks: checks}
	}

	// Seed the audit trail with the two shapes that must never carry content:
	// a resource URI with a query string and a ui/message body.
	secretQuery := "SECRET-QUERY-TOKEN"
	secretBody := "SECRET-BODY-MARKER"
	_, _, _, _ = e.request(ctx, http.MethodPost, "/v1/mcp/apps/"+appID+"/resources/read", tenant, map[string]any{
		"sessionId": session,
		"uri":       "https://api.fixture.test/data?token=" + secretQuery,
	})
	var pending any
	code, err := e.postJSON(ctx, "/v1/mcp/apps/"+appID+"/ui/message", tenant, map[string]any{
		"sessionId": session,
		"role":      "user",
		"content":   []any{map[string]any{"type": "text", "text": secretBody}},
		"waitMs":    300,
	}, &pending)
	if err != nil || (code != http.StatusAccepted && code != http.StatusOK) {
		return fail("ui/message seed: code=%d err=%v", code, err)
	}
	if code, _, _, err := e.request(ctx, http.MethodPost, "/v1/mcp/apps/"+appID+"/tools/call", tenant, map[string]any{
		"sessionId": session, "requestId": "g3-call", "name": "refresh", "arguments": map[string]any{},
	}); err != nil || code != 200 {
		return fail("tools/call seed: code=%d err=%v", code, err)
	}

	metricsBefore, err := e.scrapeMetrics(ctx)
	if err != nil {
		return fail("scrape metrics: %v", err)
	}
	samplesBefore := parsePrometheus(metricsBefore)
	if err := assertMCPMetrics(samplesBefore); err != nil {
		return fail("%v", err)
	}
	keysBefore := metricSeriesKeys(samplesBefore)
	e.writeText("metrics-before.txt", metricsBefore)
	e.writeJSON("metrics-series-before.json", metricSeriesView(samplesBefore))

	// G3's cardinality contract: the six MCP families do not gain a series when
	// sessions multiply. Sessions are the only thing created here; no MCP
	// activity follows the baseline, so the key set must be identical.
	for i := 0; i < 20; i++ {
		var body struct {
			SessionID string `json:"sessionId"`
		}
		if code, err := e.postJSON(ctx, "/v1/sessions", tenant, map[string]string{}, &body); err != nil || code != 201 {
			return fail("create session %d: code=%d err=%v", i, code, err)
		}
		_, _ = e.getJSON(ctx, "/v1/mcp/apps", e.tenant, nil)
	}
	time.Sleep(400 * time.Millisecond)
	metricsAfter, err := e.scrapeMetrics(ctx)
	if err != nil {
		return fail("scrape metrics after sessions: %v", err)
	}
	samplesAfter := parsePrometheus(metricsAfter)
	if err := assertMCPMetrics(samplesAfter); err != nil {
		return fail("after sessions: %v", err)
	}
	keysAfter := metricSeriesKeys(samplesAfter)
	for key := range keysBefore {
		if !keysAfter[key] {
			return fail("series %s disappeared after sessions", key)
		}
	}
	for key := range keysAfter {
		if !keysBefore[key] {
			return fail("series %s appeared after 20 sessions (cardinality grew)", key)
		}
	}
	for _, sample := range samplesAfter {
		for labelName, value := range sample.Labels {
			lower := strings.ToLower(labelName)
			for _, forbidden := range []string{"tenant", "session", "app_id", "appid", "tool_call", "toolcall", "request", "trace"} {
				if strings.Contains(lower, forbidden) {
					return fail("metric %s has high-cardinality label %q", sample.Name, labelName)
				}
			}
			if strings.Contains(value, tenant) || strings.Contains(value, session) {
				return fail("metric %s carries a session/tenant identity in label %q", sample.Name, labelName)
			}
		}
	}
	if strings.Contains(metricsAfter, tenant) || strings.Contains(metricsAfter, session) {
		return fail("/metrics exposes a tenant or session identity")
	}
	checks = append(checks, "six MCP families present with closed label vocabularies")
	checks = append(checks, "series key set unchanged after 20 more sessions")
	e.writeText("metrics-after.txt", metricsAfter)
	e.writeJSON("metrics-series-after.json", metricSeriesView(samplesAfter))

	// Audit scan: API rows and the durable table, including the seeded bodies.
	rows, err := e.loadAuditRows(ctx, tenant, true)
	if err != nil {
		return fail("load audit: %v", err)
	}
	if err := scanAuditRows(rows, tenant, []string{secretQuery, secretBody}, knownApps); err != nil {
		return fail("%v", err)
	}
	checks = append(checks, fmt.Sprintf("audit rows=%d contain no message body, token, URL query or half line", len(rows)))
	e.writeJSON("audit-rows.json", rows)

	return result{
		Name:   name,
		Pass:   true,
		Detail: "Prometheus families stay fixed and the audit trail is identifier-only",
		Checks: checks,
	}
}

func (e *mcpAppsEnv) scrapeMetrics(ctx context.Context) (string, error) {
	code, _, body, err := e.request(ctx, http.MethodGet, "/metrics", "", nil)
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", fmt.Errorf("/metrics answered %d", code)
	}
	return string(body), nil
}

// --- G2 kill points --------------------------------------------------------

// caseKillAfterAppFetch kills the process after the app document was fetched
// but before the model has even produced a tool call (the fake model is
// sleeping). The restart must refetch the document, reclaim the run, execute
// the tool and commit exactly one tool.result whose details carry the app
// binding.
func (e *mcpAppsEnv) caseKillAfterAppFetch() result {
	ctx := context.Background()
	name := "G2a SIGKILL after app document fetch, before tool.result"
	checks := []string{}
	fail := func(format string, args ...any) result {
		return result{Name: name, Detail: fmt.Sprintf(format, args...), Checks: checks}
	}

	e.beginCase()
	if err := e.start("kill-a", "static", 0, "KEEL_FAKE_DELAY_MS=3000", "KEEL_LEASE_MS="+mcpAppsLeaseMS); err != nil {
		return fail("start: %v", err)
	}
	catalog, err := e.waitCatalog(ctx, func(body mcpCatalogResponse) bool {
		return len(body.Apps) == 1
	}, 20*time.Second)
	if err != nil {
		return fail("catalogue: %v", err)
	}
	app := catalog.Apps[0]
	preMarkers, _ := readFakeMarkers(e.fakeEventsPath)
	if !markersHave(preMarkers, func(marker fakeMarker) bool {
		return marker.Event == "resource_read" && marker.URI == "ui://scenario/dashboard"
	}) {
		return fail("the fake server never served the app document before the kill")
	}
	checks = append(checks, "app document fetched (fake server resource_read marker)")

	tenant, session, err := e.newSession(ctx)
	if err != nil {
		return fail("%v", err)
	}
	if code, err := e.postJSON(ctx, "/v1/sessions/"+session+"/input", tenant,
		map[string]string{"text": "mcp-call:fixture/show_dashboard", "requestId": "kill-a"}, nil); err != nil || code != 201 {
		return fail("input: code=%d err=%v", code, err)
	}
	// The model call is sleeping for 3s; no tool.result can exist yet.
	time.Sleep(900 * time.Millisecond)
	preEvents, err := loadEvents(ctx, e.base, tenant, session, 0)
	if err != nil {
		return fail("pre-kill ledger: %v", err)
	}
	if countKind(preEvents, "user.message") != 1 || countKind(preEvents, "tool.result") != 0 {
		return fail("pre-kill ledger: user=%d tool.result=%d", countKind(preEvents, "user.message"), countKind(preEvents, "tool.result"))
	}
	if markersHave(preMarkers, func(marker fakeMarker) bool { return marker.Event == "tools_call_begin" }) {
		return fail("the tool call already started; the kill point is not after-fetch-before-result")
	}
	checks = append(checks, "kill landed after the document fetch and before any tool execution")
	prePID := preMarkers[0].PID
	e.writeJSON("kill-a-pre.json", map[string]any{
		"events": preEvents, "markers": preMarkers, "appId": app.AppID, "fakeServerPID": prePID,
	})
	e.killAndRecord("G2a", map[string]any{
		"appDocumentFetched":   true,
		"fakeServerPID":        prePID,
		"toolExecutionStarted": false,
		"ledgerSeqs":           seqsOf(preEvents),
		"userMessages":         1,
		"toolResults":          0,
	})

	// Restart without the model delay: the reclaimed run must finish.
	if err := e.start("kill-a-restart", "static", 0, "KEEL_LEASE_MS="+mcpAppsLeaseMS); err != nil {
		return fail("restart: %v", err)
	}
	defer e.stopProcess(true)
	postEvents, err := e.waitEventsFor(ctx, tenant, session, hasFinalRun, 25*time.Second)
	if err != nil {
		return fail("post-restart run: %v", err)
	}
	if err := assertLedgerIntegrity(postEvents, 1, 1); err != nil {
		return fail("%v", err)
	}
	bindings := toolResultAppBindings(postEvents)
	if len(bindings) != 1 || bindings[0].AppID != app.AppID || bindings[0].ResourceURI != "ui://scenario/dashboard" {
		return fail("tool.result app bindings = %+v, want the fetched app", bindings)
	}
	if err := e.assertAppMountable(ctx, app.AppID); err != nil {
		return fail("app is not mountable after restart: %v", err)
	}
	finalCatalog, err := e.waitCatalog(ctx, func(body mcpCatalogResponse) bool {
		return len(body.Apps) == 1
	}, 10*time.Second)
	if err != nil {
		return fail("post-restart catalogue: %v", err)
	}
	postMarkers, _ := readFakeMarkers(e.fakeEventsPath)
	postPID := 0
	if len(postMarkers) > 0 {
		postPID = postMarkers[0].PID
	}
	if postPID == 0 || postPID == prePID {
		return fail("the post-restart assertions did not run against a new fake-server process (pid %d→%d)", prePID, postPID)
	}
	e.addComparison("G2a", preEvents, postEvents,
		catalogSummary(catalog), catalogSummary(finalCatalog))
	checks = append(checks,
		fmt.Sprintf("restart ran in a new process (fake server pid %d→%d)", prePID, postPID),
		"exactly one tool.result with details.app after restart",
		"app remains mountable from tool.result details",
	)
	e.writeJSON("kill-a-after.json", map[string]any{
		"events": postEvents, "markers": postMarkers, "bindings": bindings, "appId": app.AppID,
	})
	e.copyFakeEvents("fake-events-kill-a.jsonl")
	return result{Name: name, Pass: true, Checks: checks,
		Detail: "SIGKILL after the app document fetch; recovery committed one tool.result with the app binding"}
}

// caseKillDuringListenReconnect kills the process while a subscriptions/listen
// reconnect is on the wire but unacknowledged. The fake server's markers prove
// the stream timeline; the restart must establish a new listen and recompute a
// correct catalogue without touching the ledger.
func (e *mcpAppsEnv) caseKillDuringListenReconnect() result {
	ctx := context.Background()
	name := "G2b SIGKILL during subscriptions/listen reconnect"
	checks := []string{}
	fail := func(format string, args ...any) result {
		return result{Name: name, Detail: fmt.Sprintf(format, args...), Checks: checks}
	}

	e.beginCase()
	if err := e.start("kill-b", "reconnect", 0, "KEEL_LEASE_MS="+mcpAppsLeaseMS); err != nil {
		return fail("start: %v", err)
	}
	// Stream #1 announces itself before its (deliberately delayed) ack, so the
	// catalogue read below is provably the pre-ack generation.
	if _, err := e.waitFakeMarker(func(marker fakeMarker) bool {
		return marker.Event == "listen_begin" && marker.N == 1
	}, 15*time.Second, "listen #1 request"); err != nil {
		return fail("%v", err)
	}
	catalog, err := e.waitCatalog(ctx, func(body mcpCatalogResponse) bool {
		return len(body.Apps) == 1
	}, 20*time.Second)
	if err != nil {
		return fail("catalogue: %v", err)
	}
	generationAtBoot := catalog.Generation
	app := catalog.Apps[0]
	tenant, session, err := e.newSession(ctx)
	if err != nil {
		return fail("%v", err)
	}
	if code, err := e.postJSON(ctx, "/v1/sessions/"+session+"/input", tenant,
		map[string]string{"text": "hello during listen", "requestId": "kill-b"}, nil); err != nil || code != 201 {
		return fail("input: code=%d err=%v", code, err)
	}
	ledgerBefore, err := e.waitEventsFor(ctx, tenant, session, hasFinalRun, 20*time.Second)
	if err != nil {
		return fail("pre-kill run: %v", err)
	}

	// Stream #1 is acknowledged after a deliberate delay, so the generation
	// bump from the post-ack recompute is observable even if boot raced us.
	if _, err := e.waitFakeMarker(func(marker fakeMarker) bool {
		return marker.Event == "listen_ack" && marker.N == 1
	}, 20*time.Second, "listen #1 ack"); err != nil {
		return fail("%v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var afterAck mcpCatalogResponse
	for time.Now().Before(deadline) {
		body, err := e.waitCatalog(ctx, func(mcpCatalogResponse) bool { return true }, time.Second)
		if err == nil {
			afterAck = body
			if body.Generation > generationAtBoot {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if afterAck.Generation <= generationAtBoot {
		return fail("generation did not advance after the listen ack (%d)", afterAck.Generation)
	}
	checks = append(checks, fmt.Sprintf("listen ack triggered a catalogue recompute (%d→%d)", generationAtBoot, afterAck.Generation))

	if _, err := e.waitFakeMarker(func(marker fakeMarker) bool {
		return marker.Event == "listen_stall" && marker.N == 2
	}, 15*time.Second, "listen #2 waiting for an acknowledgement"); err != nil {
		return fail("%v", err)
	}
	preMarkers, _ := readFakeMarkers(e.fakeEventsPath)
	if !markersHave(preMarkers, func(marker fakeMarker) bool {
		return marker.Event == "listen_closed" && marker.N == 1
	}) {
		return fail("stream #1 never closed; the second listen is not a reconnect")
	}
	metrics, err := e.scrapeMetrics(ctx)
	if err != nil {
		return fail("metrics: %v", err)
	}
	samples := parsePrometheus(metrics)
	reconnects := 0.0
	for _, sample := range samples {
		if sample.Name == "mcp_listen_reconnects_total" {
			reconnects += sample.Value
		}
	}
	if reconnects < 1 {
		return fail("mcp_listen_reconnects_total = %v while reconnecting, want >= 1", reconnects)
	}
	checks = append(checks, "the host is provably inside a reconnect (listen #2 sent, unacknowledged, metric >= 1)")
	prePID := preMarkers[0].PID
	e.writeJSON("kill-b-pre.json", map[string]any{
		"events": ledgerBefore, "markers": preMarkers, "fakeServerPID": prePID,
		"metricsReconnects": reconnects, "generationAfterAck": afterAck.Generation,
	})
	e.writeText("metrics-kill-b-before.txt", metrics)
	e.killAndRecord("G2b", map[string]any{
		"fakeServerPID":      prePID,
		"listenStream":       "n=2 sent, ack not received",
		"listenReconnects":   reconnects,
		"generationAfterAck": afterAck.Generation,
		"ledgerSeqs":         seqsOf(ledgerBefore),
		"stream1Closed":      true,
	})

	if err := e.start("kill-b-restart", "static", 0, "KEEL_LEASE_MS="+mcpAppsLeaseMS); err != nil {
		return fail("restart: %v", err)
	}
	defer e.stopProcess(true)
	postCatalog, err := e.waitCatalog(ctx, func(body mcpCatalogResponse) bool {
		return len(body.Apps) == 1 && body.Apps[0].AppID == app.AppID && body.Apps[0].Hash == app.Hash
	}, 20*time.Second)
	if err != nil {
		return fail("post-restart catalogue: %v", err)
	}
	postMarkers, err := e.waitFakeMarker(func(marker fakeMarker) bool {
		return marker.Event == "listen_ack" && marker.N == 1
	}, 20*time.Second, "post-restart listen ack")
	if err != nil {
		return fail("%v", err)
	}
	postPID := postMarkers[0].PID
	if postPID == prePID {
		return fail("post-restart listen came from the old fake-server process (pid %d)", postPID)
	}
	postEvents, err := loadEvents(ctx, e.base, tenant, session, 0)
	if err != nil {
		return fail("post-restart ledger: %v", err)
	}
	if countKind(postEvents, "user.message") != 1 || countKind(postEvents, "assistant.message") != 1 {
		return fail("ledger changed across the listen crash: user=%d assistant=%d",
			countKind(postEvents, "user.message"), countKind(postEvents, "assistant.message"))
	}
	if err := assertContiguousSeq(postEvents); err != nil {
		return fail("%v", err)
	}
	if err := e.assertAppMountable(ctx, app.AppID); err != nil {
		return fail("app is not mountable after restart: %v", err)
	}
	checks = append(checks,
		fmt.Sprintf("restart re-established listen in a new process (pid %d→%d)", prePID, postPID),
		"catalogue recomputed to the same app identity/hash",
		"ledger unchanged (one user message, one assistant message, contiguous seq)",
	)
	e.addComparison("G2b", ledgerBefore, postEvents,
		catalogSummary(catalog), catalogSummary(postCatalog))
	e.writeJSON("kill-b-after.json", map[string]any{
		"catalog": postCatalog, "events": postEvents, "markers": postMarkers, "fakeServerPID": postPID,
	})
	e.copyFakeEvents("fake-events-kill-b.jsonl")
	return result{Name: name, Pass: true, Checks: checks,
		Detail: "SIGKILL while listen #2 awaited its ack; restart reconciled the catalogue and left the ledger intact"}
}

// caseKillBeforeToolResultCommit kills the process while the MCP tool is
// executing (the server recorded tools_call_begin and is sleeping). The
// tool call is already in the ledger; the result is not. Recovery re-executes
// the at-least-once call and commits exactly one result with the app binding.
func (e *mcpAppsEnv) caseKillBeforeToolResultCommit() result {
	ctx := context.Background()
	name := "G2c SIGKILL during MCP tool execution, before tool.result commit"
	checks := []string{}
	fail := func(format string, args ...any) result {
		return result{Name: name, Detail: fmt.Sprintf(format, args...), Checks: checks}
	}

	e.beginCase()
	if err := e.start("kill-c", "static", 5000, "KEEL_LEASE_MS="+mcpAppsLeaseMS); err != nil {
		return fail("start: %v", err)
	}
	catalog, err := e.waitCatalog(ctx, func(body mcpCatalogResponse) bool {
		return len(body.Apps) == 1
	}, 20*time.Second)
	if err != nil {
		return fail("catalogue: %v", err)
	}
	app := catalog.Apps[0]
	tenant, session, err := e.newSession(ctx)
	if err != nil {
		return fail("%v", err)
	}
	// One bridge action before the crash gives this tenant a tools/call audit
	// row; the crash case then proves the row it leaves is complete.
	var seed any
	if code, err := e.postJSON(ctx, "/v1/mcp/apps/"+app.AppID+"/tools/call", tenant, map[string]any{
		"sessionId": session, "requestId": "kill-c-bridge", "name": "refresh", "arguments": map[string]any{},
	}, &seed); err != nil || code != 200 {
		return fail("bridge seed: code=%d err=%v", code, err)
	}
	if code, err := e.postJSON(ctx, "/v1/sessions/"+session+"/input", tenant,
		map[string]string{"text": "mcp-call:fixture/show_dashboard", "requestId": "kill-c"}, nil); err != nil || code != 201 {
		return fail("input: code=%d err=%v", code, err)
	}
	if _, err := e.waitFakeMarker(func(marker fakeMarker) bool {
		return marker.Event == "tools_call_begin" && marker.Name == "show_dashboard"
	}, 20*time.Second, "the MCP server began executing show_dashboard"); err != nil {
		return fail("%v", err)
	}
	preEvents, err := e.waitEventsFor(ctx, tenant, session, func(events []event) bool {
		return hasToolCall(events)
	}, 15*time.Second)
	if err != nil {
		return fail("assistant tool call was not committed: %v", err)
	}
	if countKind(preEvents, "tool.result") != 0 {
		return fail("a tool.result already exists before the kill")
	}
	preMarkers, _ := readFakeMarkers(e.fakeEventsPath)
	if markersHave(preMarkers, func(marker fakeMarker) bool {
		return marker.Event == "tools_call_end" && marker.Name == "show_dashboard"
	}) {
		return fail("the MCP server already answered; the kill is not before the result")
	}
	checks = append(checks, "kill landed after tools/call dispatch and before the MCP result (begin marker, no end marker)")
	prePID := preMarkers[0].PID
	e.writeJSON("kill-c-pre.json", map[string]any{
		"events": preEvents, "markers": preMarkers, "fakeServerPID": prePID, "appId": app.AppID,
	})
	e.killAndRecord("G2c", map[string]any{
		"fakeServerPID":      prePID,
		"toolCallDispatched": true,
		"mcpResultReceived":  false,
		"ledgerSeqs":         seqsOf(preEvents),
		"assistantToolCalls": 1,
		"toolResults":        0,
	})

	// The retry must not inherit the 5s tool delay: the recovered call answers
	// immediately, keeping the case bounded.
	if err := e.start("kill-c-restart", "static", 0, "KEEL_LEASE_MS="+mcpAppsLeaseMS); err != nil {
		return fail("restart: %v", err)
	}
	defer e.stopProcess(true)
	postEvents, err := e.waitEventsFor(ctx, tenant, session, hasFinalRun, 30*time.Second)
	if err != nil {
		return fail("post-restart run: %v", err)
	}
	if err := assertLedgerIntegrity(postEvents, 1, 1); err != nil {
		return fail("%v", err)
	}
	bindings := toolResultAppBindings(postEvents)
	if len(bindings) != 1 || bindings[0].AppID != app.AppID {
		return fail("tool.result app bindings = %+v, want one binding", bindings)
	}
	if err := e.assertAppMountable(ctx, app.AppID); err != nil {
		return fail("app is not mountable after restart: %v", err)
	}
	postCatalog, err := e.waitCatalog(ctx, func(body mcpCatalogResponse) bool {
		return len(body.Apps) == 1
	}, 10*time.Second)
	if err != nil {
		return fail("post-restart catalogue: %v", err)
	}
	postMarkers, _ := readFakeMarkers(e.fakeEventsPath)
	if !markersHave(postMarkers, func(marker fakeMarker) bool {
		return marker.Event == "tools_call_end" && marker.Name == "show_dashboard"
	}) {
		return fail("the recovered tool call never completed on the restarted server")
	}
	postPID := 0
	if len(postMarkers) > 0 {
		postPID = postMarkers[0].PID
	}
	if postPID == 0 || postPID == prePID {
		return fail("the post-restart assertions did not run against a new fake-server process (pid %d→%d)", prePID, postPID)
	}
	// The audit table is written by both processes; it must contain complete
	// rows only, and every row that names an app must name a published one.
	rows, err := e.loadAuditRows(ctx, tenant, false)
	if err != nil {
		return fail("load audit: %v", err)
	}
	if err := scanAuditRows(rows, tenant, nil, map[string]bool{app.AppID: true}); err != nil {
		return fail("%v", err)
	}
	// The crash matrix's global audit scan: rows written by every process in
	// this run, including the ones a SIGKILL interrupted, must be complete and
	// must never name an unpublished app.
	allRows, err := e.loadAuditTable(ctx)
	if err != nil {
		return fail("load full audit table: %v", err)
	}
	knownApps := map[string]bool{
		mcpAppID(mcpAppsFixtureServer, "ui://scenario/dashboard"):    true,
		mcpAppID(mcpAppsFixtureServer, "ui://scenario/dashboard_v2"): true,
	}
	if err := scanAuditRows(allRows, "", nil, knownApps); err != nil {
		return fail("global audit scan: %v", err)
	}
	checks = append(checks, "global audit table has no half line and no orphan app")
	e.writeJSON("audit-matrix.json", allRows)
	e.addComparison("G2c", preEvents, postEvents,
		catalogSummary(catalog), catalogSummary(postCatalog))
	checks = append(checks,
		fmt.Sprintf("restart ran in a new process (fake server pid %d→%d)", prePID, postPID),
		"exactly one tool.result with details.app; the tool was at-least-once, the ledger exactly-once",
		"audit rows are complete and reference only the published app",
	)
	e.writeJSON("kill-c-after.json", map[string]any{
		"events": postEvents, "markers": postMarkers, "bindings": bindings, "auditRows": rows,
	})
	e.copyFakeEvents("fake-events-kill-c.jsonl")
	return result{Name: name, Pass: true, Checks: checks,
		Detail: "SIGKILL mid tool execution; the at-least-once retry committed one result with the app binding"}
}

// --- ledger / app helpers --------------------------------------------------

func (e *mcpAppsEnv) waitEventsFor(ctx context.Context, tenant, session string, pred func([]event) bool, timeout time.Duration) ([]event, error) {
	deadline := time.Now().Add(timeout)
	var last []event
	for time.Now().Before(deadline) {
		events, err := loadEvents(ctx, e.base, tenant, session, 0)
		if err == nil {
			last = events
			if pred(events) {
				return events, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return last, fmt.Errorf("timeout waiting for ledger events (last=%d)", len(last))
}

type toolResultPayload struct {
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	Content    []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Details json.RawMessage `json:"details"`
	IsError bool            `json:"isError"`
}

func toolResultPayloads(events []event) []toolResultPayload {
	var out []toolResultPayload
	for _, ev := range events {
		if ev.Kind != "tool.result" {
			continue
		}
		var payload toolResultPayload
		if json.Unmarshal(ev.Payload, &payload) == nil {
			out = append(out, payload)
		}
	}
	return out
}

func toolResultText(payload toolResultPayload) string {
	var text strings.Builder
	for _, block := range payload.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	return text.String()
}

type appBinding struct {
	AppID       string   `json:"appId"`
	ResourceURI string   `json:"resourceUri"`
	Visibility  []string `json:"visibility"`
}

func toolResultAppBindings(events []event) []appBinding {
	var bindings []appBinding
	for _, payload := range toolResultPayloads(events) {
		if len(payload.Details) == 0 {
			continue
		}
		var details struct {
			App *appBinding `json:"app"`
		}
		if json.Unmarshal(payload.Details, &details) == nil && details.App != nil {
			bindings = append(bindings, *details.App)
		}
	}
	return bindings
}

func assertLedgerIntegrity(events []event, wantUser, wantToolResults int) error {
	if got := countKind(events, "user.message"); got != wantUser {
		return fmt.Errorf("user.message = %d, want %d", got, wantUser)
	}
	if got := countKind(events, "tool.result"); got != wantToolResults {
		return fmt.Errorf("tool.result = %d, want %d (at-least-once execution, exactly-once ledger)", got, wantToolResults)
	}
	return assertContiguousSeq(events)
}

func assertContiguousSeq(events []event) error {
	seqs := make([]int64, len(events))
	for i, ev := range events {
		seqs[i] = ev.Seq
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			return fmt.Errorf("ledger gap: seq %d followed by %d", seqs[i-1], seqs[i])
		}
	}
	return nil
}

func (e *mcpAppsEnv) assertAppMountable(ctx context.Context, appID string) error {
	code, headers, body, err := e.request(ctx, http.MethodGet, "/v1/mcp/apps/"+appID+"/view", e.tenant, nil)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("view answered %d", code)
	}
	if headers.Get("X-MCP-App-Mime") != mcpAppsMIMEType || headers.Get("Content-Security-Policy") == "" {
		return fmt.Errorf("view headers are incomplete: %v", headers)
	}
	if !bytes.Contains(body, []byte("<!doctype html>")) {
		return fmt.Errorf("view body is not HTML")
	}
	return nil
}

func mcpAppID(server, uri string) string {
	sum := sha256.Sum256([]byte(server + "\x00" + uri))
	return "mcpapp_" + hex.EncodeToString(sum[:8])
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max] + "…"
}

// --- audit -----------------------------------------------------------------

type auditRow struct {
	AuditID   int64   `json:"auditId"`
	TS        string  `json:"ts"`
	TenantID  *string `json:"tenantId,omitempty"`
	SessionID *string `json:"sessionId,omitempty"`
	Server    string  `json:"server,omitempty"`
	AppID     string  `json:"appId,omitempty"`
	Action    string  `json:"action"`
	Result    string  `json:"result"`
	Detail    string  `json:"detail,omitempty"`
}

func (e *mcpAppsEnv) loadAuditRows(ctx context.Context, tenant string, includeGlobal bool) ([]auditRow, error) {
	// API view (the product surface) ...
	var list struct {
		Entries []auditRow `json:"entries"`
	}
	var apiRows []auditRow
	after := int64(0)
	for {
		path := fmt.Sprintf("/v1/mcp/apps/audit?limit=200&after=%d", after)
		code, err := e.getJSON(ctx, path, tenant, &list)
		if err != nil {
			return nil, err
		}
		if code != 200 {
			return nil, fmt.Errorf("audit list answered %d", code)
		}
		apiRows = append(apiRows, list.Entries...)
		if len(list.Entries) < 200 {
			break
		}
		next := list.Entries[len(list.Entries)-1].AuditID
		if next <= after {
			break
		}
		after = next
	}
	// The product API intentionally includes NULL-tenant (process-global)
	// catalogue rows for every caller. A per-case crash scan must not see the
	// previous case's catalogue, so drop them unless the caller asked for the
	// global view.
	if !includeGlobal {
		filtered := apiRows[:0]
		for _, row := range apiRows {
			if row.TenantID == nil || *row.TenantID == "" || *row.TenantID == "00000000-0000-0000-0000-000000000000" {
				continue
			}
			filtered = append(filtered, row)
		}
		apiRows = filtered
	}
	// ... and the durable table, so a row that never reached the API view is
	// still scanned. The process-global catalogue rows carry the zero tenant
	// (the deployment has no tenant scope); bridge rows carry the request
	// tenant. A crash case scans only its own tenant, G3 scans both.
	pool, err := pgxpool.New(ctx, e.dsn)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	query := `SELECT audit_id, ts::text, tenant_id::text, session_id::text, server, app_id, action, result, detail
		FROM mcp_app_audit WHERE tenant_id = $1 ORDER BY audit_id`
	if includeGlobal {
		query = `SELECT audit_id, ts::text, tenant_id::text, session_id::text, server, app_id, action, result, detail
			FROM mcp_app_audit
			WHERE tenant_id = $1 OR tenant_id IS NULL
			ORDER BY audit_id`
	}
	rows, err := pool.Query(ctx, query, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var dbRows []auditRow
	for rows.Next() {
		var row auditRow
		if err := rows.Scan(&row.AuditID, &row.TS, &row.TenantID, &row.SessionID, &row.Server, &row.AppID, &row.Action, &row.Result, &row.Detail); err != nil {
			return nil, err
		}
		dbRows = append(dbRows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(apiRows) < len(dbRows) {
		apiRows = dbRows
	}
	return apiRows, nil
}

// loadAuditTable returns every durable audit row in the dedicated database,
// across tenants and across the processes the crash matrix started. It is the
// global "no half line / no orphan app" scan.
func (e *mcpAppsEnv) loadAuditTable(ctx context.Context) ([]auditRow, error) {
	pool, err := pgxpool.New(ctx, e.dsn)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	rows, err := pool.Query(ctx,
		`SELECT audit_id, ts::text, tenant_id::text, session_id::text, server, app_id, action, result, detail
		 FROM mcp_app_audit ORDER BY audit_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var row auditRow
		if err := rows.Scan(&row.AuditID, &row.TS, &row.TenantID, &row.SessionID, &row.Server, &row.AppID, &row.Action, &row.Result, &row.Detail); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// scanAuditRows enforces the audit contract: complete rows, bounded detail,
// no message body/credential/query, and no reference to an app that was never
// published.
func scanAuditRows(rows []auditRow, tenant string, forbidden []string, knownApps map[string]bool) error {
	if len(rows) == 0 {
		if tenant == "" {
			return fmt.Errorf("the audit table is empty")
		}
		return fmt.Errorf("the audit table has no rows for this tenant")
	}
	sawToolsCall := false
	for _, row := range rows {
		if row.Action == "" || row.Result == "" {
			return fmt.Errorf("audit row %d is a half line: action=%q result=%q", row.AuditID, row.Action, row.Result)
		}
		switch row.Result {
		case "ok", "error", "refused", "closed":
		default:
			return fmt.Errorf("audit row %d has an open result vocabulary: %q", row.AuditID, row.Result)
		}
		if len(row.Detail) > 512 {
			return fmt.Errorf("audit row %d detail is %d bytes (over 512)", row.AuditID, len(row.Detail))
		}
		if !utf8.ValidString(row.Detail) || strings.ContainsAny(row.Detail, "\r\n") {
			return fmt.Errorf("audit row %d detail is not a single valid UTF-8 line", row.AuditID)
		}
		if row.Server != "" && row.Server != mcpAppsFixtureServer {
			return fmt.Errorf("audit row %d names an unknown server %q", row.AuditID, row.Server)
		}
		if row.AppID != "" {
			if !knownApps[row.AppID] {
				return fmt.Errorf("audit row %d references an app that was never published: %s", row.AuditID, row.AppID)
			}
			if !strings.HasPrefix(row.AppID, "mcpapp_") {
				return fmt.Errorf("audit row %d app id has the wrong shape: %s", row.AuditID, row.AppID)
			}
		}
		if tenant != "" && row.TenantID != nil && *row.TenantID != tenant &&
			*row.TenantID != "00000000-0000-0000-0000-000000000000" {
			return fmt.Errorf("audit row %d crossed tenants: %s", row.AuditID, *row.TenantID)
		}
		for _, secret := range forbidden {
			if secret == "" {
				continue
			}
			if strings.Contains(row.Detail, secret) || strings.Contains(row.Action, secret) ||
				strings.Contains(row.Result, secret) || strings.Contains(row.AppID, secret) {
				return fmt.Errorf("audit row %d leaked %q", row.AuditID, secret)
			}
		}
		if strings.Contains(row.Detail, "token=") || strings.Contains(row.Detail, "?") {
			return fmt.Errorf("audit row %d retained a URL query: %q", row.AuditID, row.Detail)
		}
		if row.Action == "tools/call" {
			sawToolsCall = true
		}
	}
	if !sawToolsCall {
		return fmt.Errorf("the audit trail has no tools/call row")
	}
	return nil
}

// --- Prometheus text -------------------------------------------------------

type metricSample struct {
	Name   string
	Family string
	Labels map[string]string
	Value  float64
}

func parsePrometheus(text string) []metricSample {
	var samples []metricSample
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := line
		labels := map[string]string{}
		rest := ""
		if brace := strings.IndexByte(line, '{'); brace >= 0 {
			name = line[:brace]
			end := strings.IndexByte(line[brace:], '}')
			if end < 0 {
				continue
			}
			labelText := line[brace+1 : brace+end]
			rest = strings.TrimSpace(line[brace+end+1:])
			for _, pair := range splitLabelPairs(labelText) {
				key, value, ok := strings.Cut(pair, "=")
				if !ok {
					continue
				}
				labels[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"`)
			}
		} else {
			if fields := strings.Fields(line); len(fields) >= 2 {
				name = fields[0]
				rest = strings.Join(fields[1:], " ")
			}
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		value, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		family := name
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			if strings.HasSuffix(family, suffix) {
				family = strings.TrimSuffix(family, suffix)
				break
			}
		}
		samples = append(samples, metricSample{Name: name, Family: family, Labels: labels, Value: value})
	}
	return samples
}

// splitLabelPairs splits `a="1",b="2,c"` without splitting commas inside
// quoted values (Prometheus escapes quotes with a backslash).
func splitLabelPairs(text string) []string {
	var pairs []string
	var current strings.Builder
	inQuote := false
	escaped := false
	for _, r := range text {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case r == '\\' && inQuote:
			current.WriteRune(r)
			escaped = true
		case r == '"':
			inQuote = !inQuote
			current.WriteRune(r)
		case r == ',' && !inQuote:
			pairs = append(pairs, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		pairs = append(pairs, current.String())
	}
	return pairs
}

func assertMCPMetrics(samples []metricSample) error {
	families := map[string][]metricSample{}
	for _, sample := range samples {
		families[sample.Family] = append(families[sample.Family], sample)
	}
	required := []string{
		"mcp_servers", "mcp_surface_generation", "mcp_app_fetch_total",
		"mcp_app_bridge_frames_total", "mcp_listen_reconnects_total", "mcp_mrtr_rounds",
	}
	for _, family := range required {
		if len(families[family]) == 0 {
			return fmt.Errorf("metric family %s is missing", family)
		}
	}

	servers := families["mcp_servers"]
	if len(servers) != 2 {
		return fmt.Errorf("mcp_servers has %d series, want 2 (connected|disconnected)", len(servers))
	}
	states := map[string]bool{}
	for _, sample := range servers {
		state := sample.Labels["state"]
		if state != "connected" && state != "disconnected" {
			return fmt.Errorf("mcp_servers has an open state label %q", state)
		}
		states[state] = true
	}
	if !states["connected"] || !states["disconnected"] {
		return fmt.Errorf("mcp_servers states = %v", states)
	}

	if len(families["mcp_surface_generation"]) != 1 {
		return fmt.Errorf("mcp_surface_generation has %d series, want 1", len(families["mcp_surface_generation"]))
	}
	if len(families["mcp_listen_reconnects_total"]) != 1 {
		return fmt.Errorf("mcp_listen_reconnects_total has %d series, want 1", len(families["mcp_listen_reconnects_total"]))
	}
	if len(families["mcp_app_fetch_total"]) > 4 {
		return fmt.Errorf("mcp_app_fetch_total has %d series, want <= 4", len(families["mcp_app_fetch_total"]))
	}
	fetchResults := map[string]bool{}
	for _, sample := range families["mcp_app_fetch_total"] {
		result := sample.Labels["result"]
		switch result {
		case "ok", "error", "reused", "other":
			fetchResults[result] = true
		default:
			return fmt.Errorf("mcp_app_fetch_total has an open result label %q", result)
		}
	}
	if !fetchResults["ok"] {
		return fmt.Errorf("mcp_app_fetch_total has no ok series after a document fetch")
	}
	if len(families["mcp_app_bridge_frames_total"]) > 15 {
		return fmt.Errorf("mcp_app_bridge_frames_total has %d series, want <= 15", len(families["mcp_app_bridge_frames_total"]))
	}
	methods := map[string]bool{}
	for _, sample := range families["mcp_app_bridge_frames_total"] {
		method, direction := sample.Labels["method"], sample.Labels["direction"]
		switch method {
		case "tools/call", "resources/read", "ui/message", "ui/open-link", "other":
			methods[method] = true
		default:
			return fmt.Errorf("mcp_app_bridge_frames_total has an open method label %q", method)
		}
		switch direction {
		case "in", "out", "other":
		default:
			return fmt.Errorf("mcp_app_bridge_frames_total has an open direction label %q", direction)
		}
	}
	if !methods["tools/call"] {
		return fmt.Errorf("mcp_app_bridge_frames_total has no tools/call series after the bridge call")
	}
	mrtrNames := map[string]bool{}
	for _, sample := range families["mcp_mrtr_rounds"] {
		mrtrNames[sample.Name] = true
	}
	for _, want := range []string{"mcp_mrtr_rounds_bucket", "mcp_mrtr_rounds_sum", "mcp_mrtr_rounds_count"} {
		if !mrtrNames[want] {
			return fmt.Errorf("MRTR histogram is missing %s", want)
		}
	}
	return nil
}

func metricSeriesKeys(samples []metricSample) map[string]bool {
	keys := map[string]bool{}
	for _, sample := range samples {
		switch sample.Family {
		case "mcp_servers", "mcp_surface_generation", "mcp_app_fetch_total",
			"mcp_app_bridge_frames_total", "mcp_listen_reconnects_total", "mcp_mrtr_rounds":
		default:
			continue
		}
		keys[seriesKey(sample)] = true
	}
	return keys
}

func seriesKey(sample metricSample) string {
	names := make([]string, 0, len(sample.Labels))
	for name := range sample.Labels {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	builder.WriteString(sample.Family)
	for _, name := range names {
		builder.WriteString("|")
		builder.WriteString(name)
		builder.WriteString("=")
		builder.WriteString(sample.Labels[name])
	}
	return builder.String()
}

func metricSeriesView(samples []metricSample) []map[string]any {
	var out []map[string]any
	for _, sample := range samples {
		switch sample.Family {
		case "mcp_servers", "mcp_surface_generation", "mcp_app_fetch_total",
			"mcp_app_bridge_frames_total", "mcp_listen_reconnects_total", "mcp_mrtr_rounds":
		default:
			continue
		}
		out = append(out, map[string]any{"name": sample.Name, "labels": sample.Labels, "value": sample.Value})
	}
	return out
}

// catalogSummary is the compact before/after catalogue identity recorded next
// to the ledger sequences.
func catalogSummary(catalog mcpCatalogResponse) map[string]any {
	apps := make([]map[string]any, 0, len(catalog.Apps))
	for _, app := range catalog.Apps {
		apps = append(apps, map[string]any{
			"appId": app.AppID, "resourceUri": app.ResourceURI, "hash": app.Hash,
			"tools": len(app.Tools),
		})
	}
	return map[string]any{"generation": catalog.Generation, "apps": apps}
}
