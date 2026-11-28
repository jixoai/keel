// Package producte2e holds process-level end-to-end tests of the product
// surface (cmd/keel). The tests spawn the real binary against a dedicated
// PostgreSQL database and drive the same HTTP routes the web client uses; they
// are the executable form of the W11 vertical slice.
package producte2e

// TestVerticalW11 is the W11 acceptance slice:
//
//	task create → bash execute → capture → task panel visible
//	→ fork subagent → merge result → uninstall plugin with zero residue
//
// Every step is observed on the real product surface: cmd/keel serve boots the
// product layer (agent worker, jobqueue/bash workspace, file plugin, task log,
// sub-agent runner, plugin manager) against a dedicated PostgreSQL database,
// and the test reads the same HTTP routes the web UI reads (/v1/sessions/...,
// /v1/sessions/{id}/task-panel, /v1/plugins). It needs KEEL_TEST_DSN and skips
// without one, like the rest of the repository's database tests.

import (
	"bytes"
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
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// localAdminAccountID is the built-in local-only super administrator
// (cmd/keel/auth.go: localAdminIDString). local-only mints it on boot, so a
// fixture can hand a Bot that account without creating a password identity.
const localAdminAccountID = "00000000-0000-0000-0000-00000000a002"

var (
	buildOnce sync.Once
	buildPath string
	buildDir  string
	buildErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

func keelBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		root, err := repoRoot()
		if err != nil {
			buildErr = err
			return
		}
		dir, err := os.MkdirTemp("", "keel-producte2e-bin-")
		if err != nil {
			buildErr = err
			return
		}
		buildDir = dir
		out := filepath.Join(dir, "keel")
		cmd := exec.Command("go", "build", "-o", out, "./cmd/keel")
		cmd.Dir = root
		if combined, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build ./cmd/keel: %v: %s", err, combined)
			return
		}
		buildPath = out
	})
	if buildErr != nil {
		t.Fatalf("build keel binary: %v", buildErr)
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

// isolatedDatabase creates a dedicated database and drops it again, so the
// shared KEEL_TEST_DSN never sees this test's rows.
func isolatedDatabase(t *testing.T, dsn string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		t.Fatalf("KEEL_TEST_DSN must be a URL DSN for this test, got %q", dsn)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	name := "keel_w11_e2e_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	ctx := t.Context()
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect for createdb: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close(ctx)
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Logf("cleanup: reconnect to drop %s: %v", name, err)
			admin.Close(ctx)
			return
		}
		_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		conn.Close(ctx)
		admin.Close(ctx)
	})
	parsed.Path = "/" + name
	return parsed.String()
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

// serverProcess is one spawned `keel serve`.
type serverProcess struct {
	cmd  *exec.Cmd
	base string
	port int
	log  string
}

func startServer(t *testing.T, bin, dsn, work string, tenant uuid.UUID) *serverProcess {
	t.Helper()
	port := freePort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	logPath := filepath.Join(work, "keel-serve.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "serve",
		"-addr", fmt.Sprintf("127.0.0.1:%d", port),
		"-dsn", dsn,
		"-cas", filepath.Join(work, "cas"),
		"-web-dir", filepath.Join(work, "web-missing"),
		"-plugins-root", filepath.Join(work, "plugins"),
		"-auth", "local-only",
		"-tenant", tenant.String(),
		"-runs", "4",
	)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "KEEL_HOME="+filepath.Join(work, "home"))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		t.Fatalf("start keel serve: %v", err)
	}
	server := &serverProcess{cmd: cmd, base: base, port: port, log: logPath}
	t.Cleanup(func() {
		server.stop(t)
		if err := server.waitPortFree(10 * time.Second); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	deadline := time.Now().Add(40 * time.Second)
	client := &http.Client{Timeout: 2 * time.Second}
	healthy := false
	for time.Now().Before(deadline) {
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("keel serve exited before health; log:\n%s", tailFile(logPath, 40))
		}
		res, err := client.Get(base + "/v1/health")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				healthy = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !healthy {
		server.stop(t)
		t.Fatalf("keel serve did not become healthy; log:\n%s", tailFile(logPath, 40))
	}
	// Readiness is the boot schema contract, not just liveness: a fresh product
	// database must have the handoff/plugin tables before the test speaks to the
	// API.
	readyDeadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(readyDeadline) {
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("keel serve exited before ready; log:\n%s", tailFile(logPath, 40))
		}
		res, err := client.Get(base + "/v1/ready")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				return server
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	server.stop(t)
	t.Fatalf("keel serve did not become ready; log:\n%s", tailFile(logPath, 40))
	return nil
}

func (s *serverProcess) stop(t *testing.T) {
	t.Helper()
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	pid := s.cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	done := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Errorf("keel serve pid %d did not exit after SIGKILL", pid)
	}
	s.cmd = nil
}

func (s *serverProcess) waitPortFree(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.port))
		if err == nil {
			listener.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("port %d is still in use", s.port)
}

func tailFile(path string, lines int) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "(unreadable: " + err.Error() + ")"
	}
	parts := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}

// apiClient speaks the product's HTTP surface the way the web client does: a
// local-only session authenticates the loopback request as the built-in local
// admin, and writes carry the double-submit CSRF pair (cookie + header).
type apiClient struct {
	t      *testing.T
	base   string
	http   *http.Client
	csrf   string
	cookie string
}

func newAPIClient(t *testing.T, base string) *apiClient {
	t.Helper()
	client := &apiClient{t: t, base: base, http: &http.Client{Timeout: 60 * time.Second}}
	code, headers, raw := client.raw("GET", "/v1/auth/session", nil, false)
	if code != 200 {
		t.Fatalf("GET /v1/auth/session = %d: %s", code, raw)
	}
	var body struct {
		CSRF string `json:"csrfToken"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.CSRF == "" {
		t.Fatalf("GET /v1/auth/session carried no csrfToken: %v body=%s", err, raw)
	}
	client.csrf = body.CSRF
	cookies := headers.Values("Set-Cookie")
	if len(cookies) == 0 {
		t.Fatalf("GET /v1/auth/session set no cookie: %v", headers)
	}
	client.cookie = strings.SplitN(cookies[0], ";", 2)[0]
	return client
}

// raw performs one request. write=true adds the CSRF pair.
func (c *apiClient) raw(method, path string, body any, write bool) (int, http.Header, []byte) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if write {
		req.Header.Set("X-Keel-CSRF", c.csrf)
		req.Header.Set("Cookie", c.cookie)
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, raw
}

// do sends a request and decodes the JSON body, failing on an unexpected code.
func (c *apiClient) do(method, path string, body any, want int, out any) {
	c.t.Helper()
	code, _, raw := c.raw(method, path, body, method != "GET")
	if code != want {
		c.t.Fatalf("%s %s = %d (want %d): %s", method, path, code, want, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			c.t.Fatalf("%s %s: decode: %v body=%s", method, path, err, raw)
		}
	}
}

// event is one committed session event.
type event struct {
	Seq     int64           `json:"seq"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

func (c *apiClient) events(session string) []event {
	var body struct {
		Events []event `json:"events"`
	}
	c.do("GET", "/v1/sessions/"+session+"/events?limit=200", nil, 200, &body)
	return body.Events
}

func (c *apiClient) waitRunDone(session string, timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var run struct {
			Status    string `json:"status"`
			CursorSeq int64  `json:"cursorSeq"`
			LastError string `json:"lastError"`
		}
		c.do("GET", "/v1/sessions/"+session+"/run", nil, 200, &run)
		if run.Status == "done" {
			return
		}
		if run.Status == "failed" {
			c.t.Fatalf("run failed: %s", run.LastError)
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatalf("session %s did not run to done in %s", session, timeout)
}

// botSetup creates the identity ladder a Bot-owned session needs: an
// account→role→profile→bot chain plus a permission group granting the tools the
// fixture exercises. The Bot is owned by the built-in local admin so the
// run-time authorizer resolves the super-admin grant set.
func (c *apiClient) botSetup(botID string) {
	c.t.Helper()
	var role struct {
		Role struct {
			ID string `json:"roleId"`
		} `json:"role"`
	}
	c.do("POST", "/v1/roles", map[string]any{"name": "w11-operator"}, 201, &role)
	var profile struct {
		Profile struct {
			ID string `json:"profileId"`
		} `json:"profile"`
	}
	c.do("POST", "/v1/profiles", map[string]any{
		"accountId": localAdminAccountID, "roleId": role.Role.ID,
	}, 201, &profile)

	permissions := []string{"shell.exec", "file.read", "file.write", "task.read", "task.write", "session.read", "session.write"}
	var bot struct {
		Bot struct {
			ID string `json:"botId"`
		} `json:"bot"`
	}
	c.do("POST", "/v1/bots", map[string]any{
		"botId": botID, "name": "W11 Bot", "profileId": profile.Profile.ID,
		"modelKind":    "text",
		"defaultModel": map[string]any{"providerId": "fake", "modelId": "fake"},
		"harness": map[string]any{
			"tools":   []string{"bash", "read", "write", "edit", "tasks"},
			"context": map[string]any{"permissions": permissions},
		},
	}, 201, &bot)

	// Persist the same narrowing on the Bot (the run reads the stored record).
	c.do("PATCH", "/v1/bots/"+botID, map[string]any{
		"harness": map[string]any{
			"tools":   []string{"bash", "read", "write", "edit", "tasks"},
			"context": map[string]any{"permissions": permissions},
		},
	}, 200, nil)

	var group struct {
		Group struct {
			ID string `json:"groupId"`
		} `json:"group"`
	}
	c.do("POST", "/v1/permission-groups", map[string]any{"name": "w11-grants"}, 201, &group)
	for _, grant := range [][2]string{
		{"shell.exec", "tenant"}, {"file.read", "tenant"}, {"file.write", "tenant"},
		{"task.read", "tenant"}, {"task.write", "tenant"},
		{"bot.invoke", "tenant"}, {"session.read", "own"}, {"session.write", "own"},
	} {
		c.do("POST", "/v1/permission-groups/"+group.Group.ID+"/grants", map[string]any{
			"key": grant[0], "scope": grant[1],
		}, 200, nil)
	}
	c.do("POST", "/v1/permission-groups/"+group.Group.ID+"/members", map[string]any{
		"accountId": localAdminAccountID,
	}, 200, nil)
}

// installPlugin runs the two-phase durable approval and returns the installed
// plugin view.
func (c *apiClient) installPlugin(path string) map[string]any {
	c.t.Helper()
	var opened struct {
		Decision struct {
			ID string `json:"decisionId"`
		} `json:"decision"`
		AnswerURL string `json:"answerUrl"`
	}
	c.do("POST", "/v1/plugins/install", map[string]any{"path": path}, 202, &opened)
	if opened.Decision.ID == "" || opened.AnswerURL == "" {
		c.t.Fatalf("install approval = %+v", opened)
	}
	c.do("POST", opened.AnswerURL, map[string]any{"optionId": "allow"}, 200, nil)
	var installed struct {
		Plugin map[string]any `json:"plugin"`
	}
	c.do("POST", "/v1/plugins/install", map[string]any{"path": path, "decisionId": opened.Decision.ID}, 201, &installed)
	return installed.Plugin
}

func (c *apiClient) enablePlugin(id string) map[string]any {
	c.t.Helper()
	var opened struct {
		Decision struct {
			ID string `json:"decisionId"`
		} `json:"decision"`
		AnswerURL string `json:"answerUrl"`
	}
	c.do("POST", "/v1/plugins/"+id+"/enable", map[string]any{}, 202, &opened)
	c.do("POST", opened.AnswerURL, map[string]any{"optionId": "allow"}, 200, nil)
	var enabled struct {
		Plugin map[string]any `json:"plugin"`
	}
	c.do("POST", "/v1/plugins/"+id+"/enable", map[string]any{"decisionId": opened.Decision.ID}, 200, &enabled)
	return enabled.Plugin
}

func (c *apiClient) pluginInventory() (generation int64, tools []string, plugins map[string]string) {
	c.t.Helper()
	var body struct {
		Generation int64    `json:"generation"`
		Tools      []string `json:"tools"`
		Plugins    []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"plugins"`
	}
	c.do("GET", "/v1/plugins", nil, 200, &body)
	plugins = make(map[string]string, len(body.Plugins))
	for _, plugin := range body.Plugins {
		plugins[plugin.ID] = plugin.State
	}
	return body.Generation, body.Tools, plugins
}

// toolManifestHash starts a fresh session with plain input and returns the
// run.finished toolManifestHash the attempt recorded.
func (c *apiClient) toolManifestHash(botID string) string {
	c.t.Helper()
	session := uuid.NewString()
	c.do("POST", "/v1/sessions", map[string]any{"sessionId": session, "botId": botID}, 201, nil)
	c.do("POST", "/v1/sessions/"+session+"/input", map[string]any{
		"text": "manifest probe", "requestId": "manifest-" + uuid.NewString(),
	}, 201, nil)
	c.waitRunDone(session, 30*time.Second)
	for _, ev := range c.events(session) {
		if ev.Kind != "run.finished" {
			continue
		}
		var payload struct {
			ToolManifestHash string `json:"toolManifestHash"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			c.t.Fatal(err)
		}
		if payload.ToolManifestHash != "" {
			return payload.ToolManifestHash
		}
	}
	c.t.Fatalf("probe run recorded no toolManifestHash")
	return ""
}

func TestVerticalW11(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KEEL_TEST_DSN"))
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	if runtime.GOOS == "windows" {
		t.Skip("process-level product E2E runs on unix signals")
	}
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	bin := keelBinary(t)
	dbDSN := isolatedDatabase(t, dsn)
	work := t.TempDir()
	tenant := uuid.New()
	server := startServer(t, bin, dbDSN, work, tenant)
	client := newAPIClient(t, server.base)

	const botID = "w11-bot"
	client.botSetup(botID)

	// --- 1+2. task create → bash execute → capture -------------------------
	session := uuid.NewString()
	client.do("POST", "/v1/sessions", map[string]any{"sessionId": session, "botId": botID}, 201, nil)
	// The fake provider executes the two scripted directives in order: create a
	// real card through the `tasks` aggregate tool, then run a real command
	// through the `bash` aggregate tool. The user text is also the marker the
	// capture assertion looks for in the workspace file.
	const marker = "hello-from-bash"
	client.do("POST", "/v1/sessions/"+session+"/input", map[string]any{
		"text": "vertical slice\n" +
			"tasks-create: write a report\n" +
			"bash:printf '" + marker + "\\n' > out.txt",
		"requestId": "vertical-1",
	}, 201, nil)
	client.waitRunDone(session, 45*time.Second)
	events := client.events(session)

	var (
		createdCard  string
		captures     []event
		bashResults  int
		baselineHash string
	)
	for _, ev := range events {
		switch ev.Kind {
		case "task.created":
			// The explicit directive's card, not the auto-created initial card.
			var payload struct {
				TaskID string `json:"taskId"`
				Title  string `json:"title"`
				Origin string `json:"origin"`
			}
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Title == "write a report" {
				if createdCard != "" {
					t.Fatalf("two cards named %q", payload.Title)
				}
				createdCard = payload.TaskID
			}
			if payload.TaskID == "" {
				t.Fatalf("task.created without taskId: %s", ev.Payload)
			}
		case "fs.capture":
			captures = append(captures, ev)
		case "tool.result":
			var payload struct {
				ToolName string `json:"toolName"`
				IsError  *bool  `json:"isError"`
			}
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.ToolName == "bash" {
				if payload.IsError != nil && *payload.IsError {
					t.Fatalf("bash tool result is an error: %s", ev.Payload)
				}
				bashResults++
			}
		case "run.finished":
			var payload struct {
				ToolManifestHash string `json:"toolManifestHash"`
			}
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			baselineHash = payload.ToolManifestHash
		}
	}
	if createdCard == "" {
		t.Fatalf("the tasks directive produced no card: %v", events)
	}
	if bashResults != 1 {
		t.Fatalf("bash tool results = %d, want 1", bashResults)
	}
	if len(captures) != 1 {
		t.Fatalf("fs.capture events = %d, want exactly 1 (one settle per bash execution)", len(captures))
	}
	var capture struct {
		TaskID   string `json:"taskId"`
		Count    int    `json:"count"`
		TreeHash string `json:"treeHash"`
		BaseSeq  int64  `json:"baseSeq"`
	}
	if err := json.Unmarshal(captures[0].Payload, &capture); err != nil {
		t.Fatal(err)
	}
	if capture.TaskID == "" || capture.Count != 1 || !strings.HasPrefix(capture.TreeHash, "sha256:") {
		t.Fatalf("capture payload = %s", captures[0].Payload)
	}
	if baselineHash == "" {
		t.Fatal("the vertical run recorded no toolManifestHash")
	}

	// --- 3. task panel visible --------------------------------------------
	var panel struct {
		Cards []struct {
			TaskID     string `json:"taskId"`
			Title      string `json:"title"`
			Status     string `json:"status"`
			CreatedSeq int64  `json:"createdSeq"`
		} `json:"cards"`
		Events []struct {
			Seq   int64  `json:"seq"`
			Kind  string `json:"kind"`
			Title string `json:"title"`
		} `json:"events"`
	}
	client.do("GET", "/v1/sessions/"+session+"/task-panel", nil, 200, &panel)
	foundCard := false
	for _, card := range panel.Cards {
		if card.TaskID == createdCard && card.Title == "write a report" {
			foundCard = true
		}
	}
	if !foundCard {
		t.Fatalf("the panel does not show the created card: %+v", panel.Cards)
	}
	if len(panel.Events) == 0 || panel.Events[0].Seq == 0 {
		t.Fatalf("the panel carries no task event stream: %+v", panel.Events)
	}

	// --- 4. write → bash read + resource reference ------------------------
	var files struct {
		Files []struct {
			Path       string `json:"path"`
			Size       int64  `json:"size"`
			LastTaskID string `json:"lastTaskId"`
		} `json:"files"`
	}
	client.do("GET", "/v1/sessions/"+session+"/files", nil, 200, &files)
	if len(files.Files) != 1 || files.Files[0].Path != "out.txt" || files.Files[0].Size != int64(len(marker)+1) {
		t.Fatalf("session files = %+v, want out.txt with the bash bytes", files.Files)
	}
	var preview struct {
		Path    string `json:"path"`
		Hash    string `json:"hash"`
		Content string `json:"content"`
		Matches bool   `json:"matchesReference"`
	}
	client.do("GET", "/v1/sessions/"+session+"/file-preview?path=out.txt", nil, 200, &preview)
	if preview.Content != marker+"\n" || !strings.HasPrefix(preview.Hash, "sha256:") {
		t.Fatalf("preview = %+v", preview)
	}

	// --- 5. fork at an exact origin ---------------------------------------
	var beforeFork struct {
		Events []event `json:"events"`
	}
	client.do("GET", "/v1/sessions/"+session+"/events?limit=200", nil, 200, &beforeFork)
	origin := beforeFork.Events[len(beforeFork.Events)-1].Seq
	var forked struct {
		Subagent struct {
			SessionID        string `json:"sessionId"`
			Origin           string `json:"origin"`
			OriginSeq        int64  `json:"originSeq"`
			ParentTaskID     string `json:"parentTaskId"`
			ConfigHash       string `json:"configHash"`
			ToolManifestHash string `json:"toolManifestHash"`
		} `json:"subagent"`
		Result struct {
			Status  string `json:"status"`
			Summary string `json:"summary"`
		} `json:"result"`
	}
	client.do("POST", "/v1/sessions/"+session+"/fork", map[string]any{
		"task": "summarize the report", "title": "child card",
		"wait": true, "budget": map[string]any{"timeoutMs": 30000},
	}, 200, &forked)
	if forked.Subagent.Origin != "fork" || forked.Subagent.OriginSeq != origin {
		t.Fatalf("fork origin = %+v, want fork at seq %d", forked.Subagent, origin)
	}
	if forked.Subagent.ParentTaskID != createdCard {
		t.Fatalf("fork parentTaskId = %q, want the active card %q", forked.Subagent.ParentTaskID, createdCard)
	}
	if forked.Subagent.ConfigHash == "" || forked.Subagent.ToolManifestHash == "" {
		t.Fatalf("fork inherited no run snapshot: %+v", forked.Subagent)
	}
	if forked.Result.Status != "done" || forked.Result.Summary == "" {
		t.Fatalf("fork result = %+v", forked.Result)
	}
	child := forked.Subagent.SessionID

	// The child log carries only its own input (the prefix is read, not copied).
	childEvents := client.events(child)
	childInputs := 0
	for _, ev := range childEvents {
		if ev.Kind == "user.message" {
			childInputs++
		}
	}
	if childInputs != 1 {
		t.Fatalf("child log has %d inputs, want exactly 1", childInputs)
	}

	// --- 6. merge result ---------------------------------------------------
	var merged struct {
		Members []struct {
			TaskID    string `json:"taskId"`
			Title     string `json:"title"`
			SessionID string `json:"sessionId"`
			Origin    string `json:"origin"`
			OriginSeq int64  `json:"originSeq"`
		} `json:"members"`
		Events []struct {
			Seq  int64  `json:"seq"`
			Kind string `json:"kind"`
			Text string `json:"text"`
		} `json:"events"`
	}
	client.do("GET", "/v1/sessions/"+session+"/task-panel", nil, 200, &merged)
	memberOK := false
	for _, member := range merged.Members {
		if member.SessionID == child && member.Origin == "fork" && member.OriginSeq == origin {
			memberOK = true
		}
	}
	if !memberOK {
		t.Fatalf("the parent panel does not show the fork member: %+v", merged.Members)
	}
	noteOK := false
	for _, ev := range merged.Events {
		if ev.Kind == "task.note" && strings.Contains(ev.Text, child) && strings.Contains(ev.Text, forked.Result.Summary) {
			noteOK = true
		}
	}
	if !noteOK {
		t.Fatalf("the child result did not land on the parent card: %+v", merged.Events)
	}

	// --- 7. uninstall a plugin with zero residue ---------------------------
	// A tool-contributing sidecar makes the residue check real: the manifest
	// hash before/after must match, so no tool name survives the uninstall.
	sidecarDir := filepath.Join(work, "w11-probe")
	writeSidecarPackage(t, sidecarDir)
	beforeGeneration, _, beforePlugins := client.pluginInventory()
	if beforePlugins["w11.probe"] != "" {
		t.Fatalf("fixture plugin already present: %v", beforePlugins)
	}
	installed := client.installPlugin(sidecarDir)
	if installed["state"] != "installed" {
		t.Fatalf("installed plugin state = %v", installed["state"])
	}
	enabled := client.enablePlugin("w11.probe")
	if enabled["state"] != "enabled" {
		t.Fatalf("enabled plugin state = %v", enabled["state"])
	}
	genAfterEnable, toolsAfterEnable, _ := client.pluginInventory()
	if genAfterEnable <= beforeGeneration {
		t.Fatalf("generation did not advance on enable: %d -> %d", beforeGeneration, genAfterEnable)
	}
	// A sidecar declares local tool names; the host publishes them namespaced
	// (pluginpkg.PublishedTools): w11.probe + w11_probe -> w11_probe_w11_probe.
	const publishedProbe = "w11_probe_w11_probe"
	if !contains(toolsAfterEnable, publishedProbe) {
		t.Fatalf("the enabled plugin's tool is not on the surface: %v", toolsAfterEnable)
	}
	// Positive half of the residue check: the sidecar must be a live process
	// before the uninstall, otherwise "no process afterwards" proves nothing.
	waitForProcess(t, sidecarScriptName, 10*time.Second)
	// The run surface is the Bot's whitelist ∩ the deployment registry, so the
	// probe tool must be granted to this Bot before its presence can show up in
	// the run snapshot. The extra entry is kept after the uninstall on purpose:
	// a harness entry the registry no longer carries grants nothing, so the
	// restored hash must equal the pre-install baseline exactly.
	client.do("PATCH", "/v1/bots/"+botID, map[string]any{
		"harness": map[string]any{
			"tools": []string{"bash", "read", "write", "edit", "tasks", publishedProbe},
			"context": map[string]any{"permissions": []string{
				"shell.exec", "file.read", "file.write", "task.read", "task.write", "session.read", "session.write",
			}},
		},
	}, 200, nil)
	withToolHash := client.toolManifestHash(botID)
	if withToolHash == baselineHash {
		t.Fatalf("an enabled provides.tools plugin did not change the manifest hash")
	}

	// UI-contributing fixture: install + enable + uninstall proves contributions
	// leave nothing behind either.
	uiPkg := filepath.Join(root, "internal", "harnessext", "testdata", "w12-ui-fixture")
	uiInstalled := client.installPlugin(uiPkg)
	contributions, _ := uiInstalled["contributes"].([]any)
	if len(contributions) != 6 {
		t.Fatalf("ui fixture contributions = %d, want 6", len(contributions))
	}
	client.enablePlugin("keel.w12.fixture")

	// Disable is the documented precondition of uninstall.
	client.do("POST", "/v1/plugins/w11.probe/disable", map[string]any{}, 200, nil)
	client.do("POST", "/v1/plugins/keel.w12.fixture/disable", map[string]any{}, 200, nil)
	client.do("POST", "/v1/plugins/w11.probe/uninstall", nil, 200, nil)
	client.do("POST", "/v1/plugins/keel.w12.fixture/uninstall", nil, 200, nil)

	afterGeneration, afterTools, afterPlugins := client.pluginInventory()
	if afterPlugins["w11.probe"] != "" || afterPlugins["keel.w12.fixture"] != "" {
		t.Fatalf("uninstalled plugins survive in the inventory: %v", afterPlugins)
	}
	if contains(afterTools, publishedProbe) {
		t.Fatalf("the uninstalled plugin's tool is still on the surface: %v", afterTools)
	}
	if afterGeneration <= genAfterEnable {
		t.Fatalf("generation did not advance on uninstall: %d -> %d", genAfterEnable, afterGeneration)
	}
	// The managed directories are removed.
	for _, dir := range []string{filepath.Join(work, "plugins", "w11.probe"), filepath.Join(work, "plugins", "keel.w12.fixture")} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("managed directory %s survived the uninstall (err=%v)", dir, err)
		}
	}
	// No sidecar process from the package survives the uninstall. The needle is
	// the script name the process command line carries (the sidecar runs with
	// its package directory as cwd, so the directory itself is not on the
	// command line).
	waitNoProcess(t, sidecarScriptName, 5*time.Second)
	restoredHash := client.toolManifestHash(botID)
	if restoredHash != baselineHash {
		t.Fatalf("the tool manifest hash did not restore after uninstall: %s vs %s", restoredHash, baselineHash)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// sidecarScriptName is unique to this fixture so pgrep -f can tell the sidecar
// apart from every other node process (the entry command is `node <name>`, a
// relative path resolved against the installed package directory).
const sidecarScriptName = "w11-probe-sidecar.mjs"

// waitForProcess fails when no process whose command line contains needle
// appears before the deadline. It proves the sidecar was really running, so
// the post-uninstall waitNoProcess is not vacuous.
func waitForProcess(t *testing.T, needle string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("pgrep", "-f", needle).Output()
		if err == nil && len(bytes.TrimSpace(out)) > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no process matching %q appeared; the sidecar never started", needle)
}

// waitNoProcess fails when a process whose command line contains needle is
// still alive. It is the orphan check for the sidecar the enable started.
func waitNoProcess(t *testing.T, needle string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("pgrep", "-f", needle).Output()
		if err != nil || len(bytes.TrimSpace(out)) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("a process matching %q survived the uninstall", needle)
}

var sidecarScript = `import readline from 'node:readline';
const rl = readline.createInterface({ input: process.stdin });
const send = (id, result) => process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id, result }) + '\n');
rl.on('line', (line) => {
  if (!line.trim()) return;
  let msg;
  try { msg = JSON.parse(line); } catch { return; }
  switch (msg.method) {
    case 'initialize':
      send(msg.id, { protocolVersion: '2026-11-07', capabilities: { tools: { listChanged: false } },
        serverInfo: { name: 'w11-probe', version: '1.0.0' }, provider: { type: 'tool_provider' } });
      break;
    case 'tools/list':
      send(msg.id, { tools: [{ name: 'w11_probe', description: 'W11 vertical probe',
        inputSchema: { type: 'object', additionalProperties: true } }] });
      break;
    case 'tools/call':
      send(msg.id, { content: [{ type: 'text', text: 'probe-ok' }], structuredContent: { ok: true } });
      break;
    case 'close':
      process.exit(0);
      break;
    default:
      if (msg.id !== undefined) {
        process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id: msg.id,
          error: { code: -32601, message: 'method not found' } }) + '\n');
      }
  }
});
`

// writeSidecarPackage writes the tool-contributing fixture package.
func writeSidecarPackage(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"plugin.yaml": `id: w11.probe
version: 1.0.0
kind: sidecar
description: "W11 vertical fixture: one tool-contributed plugin"
engines:
  keel: ">=0.1.0 <1.0.0"
provides:
  tools:
    - w11_probe
permissions:
  tools:
    - w11_probe
entry:
  command: [node, w11-probe-sidecar.mjs]
`,
		sidecarScriptName: sidecarScript,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
