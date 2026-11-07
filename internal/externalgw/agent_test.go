package externalgw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/gateway"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
)

// sidecarBin is the reference sidecar built once for the whole package.
var sidecarBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "keel-dsh-ref-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "temp dir:", err)
		os.Exit(1)
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	bin := filepath.Join(dir, "keel-dsh-ref")
	build := exec.Command("go", "build", "-o", bin, "./cmd/keel-dsh-ref")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build reference sidecar: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	sidecarBin = bin
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type harnessOptions struct {
	backendArgs    []string
	quota          string
	environ        []string
	lookupEnv      func(string) (string, bool)
	restart        gateway.RestartPolicy
	connectTimeout time.Duration
	committer      kernel.Committer
	docMutate      func(*host.ExternalConfig)
}

type harness struct {
	t       *testing.T
	k       *kernel.Kernel
	mem     *kernel.MemStore
	svc     *AgentService
	tenant  uuid.UUID
	session uuid.UUID
	scope   kernel.Scope
	stderr  *syncBuffer
	logs    *syncBuffer
}

func newHarness(t *testing.T, opts harnessOptions) *harness {
	t.Helper()
	if opts.quota == "" {
		opts.quota = host.ExternalUnmeteredAllow
	}
	if opts.environ == nil {
		opts.environ = os.Environ()
	}
	if opts.lookupEnv == nil {
		opts.lookupEnv = os.LookupEnv
	}
	if opts.restart.MaxRestarts == 0 {
		opts.restart = gateway.RestartPolicy{InitialDelay: 20 * time.Millisecond, MaxDelay: 200 * time.Millisecond, MaxRestarts: 3}
	}
	if opts.connectTimeout == 0 {
		opts.connectTimeout = 5 * time.Second
	}
	mem := kernel.NewMemStore()
	k := kernel.New(mem, kernel.Options{})
	committer := opts.committer
	if committer == nil {
		committer = k
	}
	tenant, session := uuid.New(), uuid.New()
	mem.CreateSession(tenant, session)
	doc := host.ExternalConfig{
		Backends: map[string]host.ExternalProviderConfig{
			"dsh": {Command: sidecarBin, Args: opts.backendArgs},
		},
		Quota: host.ExternalQuotaConfig{Unmetered: opts.quota},
	}
	if opts.docMutate != nil {
		opts.docMutate(&doc)
	}
	stderr, logs := &syncBuffer{}, &syncBuffer{}
	svc, err := NewAgentService(doc, Options{
		Committer:      committer,
		Sessions:       k,
		LookupEnv:      opts.lookupEnv,
		Environ:        opts.environ,
		Logger:         testLogger(logs),
		Stderr:         stderr,
		Restart:        opts.restart,
		ConnectTimeout: opts.connectTimeout,
	})
	if err != nil {
		t.Fatalf("NewAgentService: %v", err)
	}
	if err := svc.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := &harness{t: t, k: k, mem: mem, svc: svc, tenant: tenant, session: session,
		scope: kernel.Scope{TenantID: tenant, SessionID: session}, stderr: stderr, logs: logs}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := svc.Stop(stopCtx); err != nil {
			t.Logf("Stop: %v", err)
		}
	})
	return h
}

func (h *harness) waitReady() {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := h.svc.WaitReady(ctx); err != nil {
		h.t.Fatalf("backend did not become ready: %v (state=%+v, stderr=%s)", err, h.svc.Backends(), h.stderr.String())
	}
}

func (h *harness) newSession() kernel.Scope {
	scope := kernel.Scope{TenantID: h.tenant, SessionID: uuid.New()}
	h.mem.CreateSession(scope.TenantID, scope.SessionID)
	return scope
}

// waitForEvent polls the committed log for one projected backend event of the
// given type.
func (h *harness) waitForEvent(scope kernel.Scope, eventType string) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last []kernel.Event
	for time.Now().Before(deadline) {
		events, err := h.k.LoadEvents(context.Background(), scope, 0, 500)
		if err == nil {
			last = events
			for _, event := range events {
				if event.Kind != kernel.KindExternalEvent {
					continue
				}
				var payload map[string]any
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					h.t.Fatalf("decode external.event: %v", err)
				}
				if payload["type"] == eventType {
					return payload
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("no %s event within 10s; log = %+v", eventType, last)
	return nil
}

func TestOpenSendProjectQueryClose(t *testing.T) {
	h := newHarness(t, harnessOptions{backendArgs: []string{"-usage", "-prompt=ref:"}})
	h.waitReady()
	ctx := context.Background()
	view, err := h.svc.Open(ctx, h.scope, "dsh", gateway.SessionOpenRequest{Reference: "cli-1"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if view.StateOwner != kernel.StateOwnerExternal || view.Backend != "dsh" ||
		view.ExternalRef == "" || view.Status != kernel.ExternalStatusActive ||
		view.Metering != kernel.MeteringExternal || !view.Compatible || view.NativeRecovery {
		t.Fatalf("view = %+v", view)
	}
	marks, err := h.k.ExternalSession(ctx, h.scope)
	if err != nil {
		t.Fatal(err)
	}
	if marks.StateOwner != kernel.StateOwnerExternal || marks.Backend != "dsh" ||
		marks.Ref == "" || marks.Status != kernel.ExternalStatusActive || marks.Metering != kernel.MeteringExternal {
		t.Fatalf("marks = %+v", marks.ExternalMarks)
	}
	// The session never enters the native run/lease flow.
	if _, err := h.k.RunPin(ctx, h.scope); !errors.Is(err, kernel.ErrNoRunnable) {
		t.Fatalf("RunPin on an external session = %v, want no-runnable", err)
	}
	if _, err := h.k.ClaimScope(ctx, h.scope, "runtime-x", time.Minute); !errors.Is(err, kernel.ErrNoRunnable) {
		t.Fatalf("ClaimScope on an external session = %v, want no-runnable", err)
	}

	result, err := h.svc.Send(ctx, h.scope, json.RawMessage(`{"text":"ping"}`), "req-1")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.TurnID == "" || !result.Recorded {
		t.Fatalf("send result = %+v", result)
	}
	payload := h.waitForEvent(h.scope, "assistant.message")
	if payload["backend"] != "dsh" || payload["stateOwner"] != kernel.StateOwnerExternal ||
		payload["externalRef"] != marks.Ref || payload["direction"] != "receive" {
		t.Fatalf("projected payload = %+v", payload)
	}
	usage, _ := payload["usage"].(map[string]any)
	if usage == nil || usage["metering"] != kernel.MeteringExternal {
		t.Fatalf("usage = %+v, want an external usage record", payload["usage"])
	}
	// The send itself is recorded too.
	sendRecord := false
	events, err := h.k.LoadEvents(ctx, h.scope, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind != kernel.KindExternalEvent {
			continue
		}
		var record map[string]any
		_ = json.Unmarshal(event.Payload, &record)
		if record["direction"] == "send" {
			sendRecord = true
			if record["turnId"] != result.TurnID {
				t.Fatalf("send record turnId = %v, want %s", record["turnId"], result.TurnID)
			}
		}
	}
	if !sendRecord {
		t.Fatal("the outgoing message was not recorded as an external.event")
	}

	queried, err := h.svc.Get(ctx, h.scope)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if queried.Status != kernel.ExternalStatusActive || queried.Usage == nil || queried.Usage.TotalTokens == 0 {
		t.Fatalf("queried = %+v", queried)
	}
	if err := h.svc.Close(ctx, h.scope, "test"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed, err := h.svc.Get(ctx, h.scope)
	if err != nil {
		t.Fatalf("Get after close: %v", err)
	}
	if closed.Status != kernel.ExternalStatusClosed {
		t.Fatalf("status after close = %s", closed.Status)
	}
	if _, err := h.svc.Send(ctx, h.scope, json.RawMessage(`{"text":"again"}`), ""); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("Send after close = %v, want ErrSessionClosed", err)
	}
}

// TestConcurrentOpenIsSingleFlight pins the open critical section: concurrent
// Opens of one scope must hand the backend exactly one session. Without the
// local lock the losers would each create a second backend session that no mark
// ever records, so nothing would ever close it (and the kernel upsert would
// refuse the marks only after the orphan already exists). The sidecar's
// -open-log counts what the backend actually saw.
func TestConcurrentOpenIsSingleFlight(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "opens.log")
	h := newHarness(t, harnessOptions{backendArgs: []string{"-usage", "-open-log", logPath}})
	h.waitReady()
	ctx := context.Background()

	const attempts = 4
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		wins   int
		losers []error
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.svc.Open(ctx, h.scope, "dsh", gateway.SessionOpenRequest{Reference: "single-flight"})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
				return
			}
			losers = append(losers, err)
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1 (losers: %v)", wins, losers)
	}
	for _, err := range losers {
		if !errors.Is(err, ErrAlreadyExternal) {
			t.Fatalf("loser error = %v, want ErrAlreadyExternal", err)
		}
	}
	// The backend saw exactly one session.open.
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read open log: %v", err)
	}
	lines := 0
	if trimmed := strings.TrimSpace(string(raw)); trimmed != "" {
		lines = strings.Count(trimmed, "\n") + 1
	}
	if lines != 1 {
		t.Fatalf("backend session.open calls = %d, want 1 (log: %q)", lines, raw)
	}
	// Exactly one live link, and the marks point at the backend session it
	// wraps (a stranded second link would be the corruption this guards).
	links := h.svc.backends["dsh"].links()
	if len(links) != 1 {
		t.Fatalf("links = %d, want 1", len(links))
	}
	marks, err := h.k.ExternalSession(ctx, h.scope)
	if err != nil {
		t.Fatal(err)
	}
	if marks.Ref != links[0].ref || marks.Status != kernel.ExternalStatusActive {
		t.Fatalf("marks ref %q vs link ref %q (status %s)", marks.Ref, links[0].ref, marks.Status)
	}
}

func TestUnmeteredDenyPolicy(t *testing.T) {
	h := newHarness(t, harnessOptions{quota: host.ExternalUnmeteredDeny})
	h.waitReady()
	_, err := h.svc.Open(context.Background(), h.scope, "dsh", gateway.SessionOpenRequest{})
	if !errors.Is(err, ErrUnmeteredDenied) {
		t.Fatalf("Open = %v, want ErrUnmeteredDenied", err)
	}
	if _, err := h.k.ExternalSession(context.Background(), h.scope); err != nil {
		t.Fatalf("session marks: %v", err)
	} else {
		marks, _ := h.k.ExternalSession(context.Background(), h.scope)
		if marks.StateOwner != kernel.StateOwnerKeel || marks.CompatMode() {
			t.Fatalf("refused open persisted external marks: %+v", marks.ExternalMarks)
		}
	}
}

func TestMeteringNoneIsRecordedNotZero(t *testing.T) {
	h := newHarness(t, harnessOptions{quota: host.ExternalUnmeteredAllow})
	h.waitReady()
	ctx := context.Background()
	view, err := h.svc.Open(ctx, h.scope, "dsh", gateway.SessionOpenRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if view.Metering != kernel.MeteringNone {
		t.Fatalf("metering = %q, want none", view.Metering)
	}
	if _, err := h.svc.Send(ctx, h.scope, json.RawMessage(`{"text":"x"}`), "req-none"); err != nil {
		t.Fatal(err)
	}
	payload := h.waitForEvent(h.scope, "assistant.message")
	if _, ok := payload["usage"]; ok {
		t.Fatalf("unmetered event carries usage: %+v", payload["usage"])
	}
	queried, err := h.svc.Get(ctx, h.scope)
	if err != nil {
		t.Fatal(err)
	}
	if queried.Usage != nil {
		t.Fatalf("unmetered session reports usage %+v, want absent", queried.Usage)
	}
}

func TestBackendCrashMarksUnavailableAndIsolatesNativeFlow(t *testing.T) {
	h := newHarness(t, harnessOptions{backendArgs: []string{"-crash-on-send"}})
	h.waitReady()
	ctx := context.Background()
	if _, err := h.svc.Open(ctx, h.scope, "dsh", gateway.SessionOpenRequest{}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := h.svc.Send(ctx, h.scope, json.RawMessage(`{"text":"die"}`), "crash"); err == nil {
		t.Fatal("Send succeeded on a crash-on-send backend")
	}
	// The crash becomes a queryable terminal state, without a fabricated
	// native recovery.
	deadline := time.Now().Add(10 * time.Second)
	for {
		view, err := h.svc.Get(ctx, h.scope)
		if err == nil && view.Status == kernel.ExternalStatusUnavailable {
			if view.NativeRecovery || !view.Compatible {
				t.Fatalf("crash view = %+v", view)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session never became backend-unavailable: %+v, %v", view, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Native sessions are unaffected: a normal user.message commits while the
	// external backend is dead.
	native := h.newSession()
	if _, err := h.k.Commit(ctx, kernel.CommitRequest{
		Scope: native, Kind: kernel.KindUserMessage, RequestID: "native-1",
		Payload: json.RawMessage(`{"text":"still alive"}`),
	}); err != nil {
		t.Fatalf("native commit during an external crash: %v", err)
	}
}

func TestStuckBackendTimesOutAndServiceStaysUsable(t *testing.T) {
	h := newHarness(t, harnessOptions{
		backendArgs: []string{"-open-delay=1s"},
		docMutate: func(doc *host.ExternalConfig) {
			backend := doc.Backends["dsh"]
			backend.TimeoutMS = 100
			doc.Backends["dsh"] = backend
		},
	})
	h.waitReady()
	start := time.Now()
	_, err := h.svc.Open(context.Background(), h.scope, "dsh", gateway.SessionOpenRequest{})
	var timeout *gateway.TimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("Open = %v, want a *gateway.TimeoutError", err)
	}
	if elapsed := time.Since(start); elapsed > 800*time.Millisecond {
		t.Fatalf("Open took %s despite a 100ms timeout", elapsed)
	}
	if states := h.svc.Backends(); len(states) != 1 || states[0].State != backendReady {
		t.Fatalf("backend states after a timed-out open = %+v", states)
	}
}

func TestIncompatibleBackendDegradesWithoutFailingBoot(t *testing.T) {
	h := newHarness(t, harnessOptions{
		backendArgs: []string{"-protocol-version=v0"},
		restart:     gateway.RestartPolicy{InitialDelay: 10 * time.Millisecond, MaxDelay: 50 * time.Millisecond, MaxRestarts: 2},
	})
	states := h.svc.Backends()
	if len(states) != 1 || states[0].State != backendDegraded {
		t.Fatalf("state = %+v, want degraded", states)
	}
	if _, err := h.svc.Open(context.Background(), h.scope, "dsh", gateway.SessionOpenRequest{}); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("Open = %v, want ErrBackendUnavailable", err)
	}
}

func TestHostEnvironmentIsNotLeaked(t *testing.T) {
	environ := append(os.Environ(), "KEEL_DSN=postgres://leak", "KEEL_ADMIN_KEY=leak-key")
	h := newHarness(t, harnessOptions{
		backendArgs: []string{"-forbid-env=KEEL_DSN,KEEL_ADMIN_KEY", "-require-env=DSH_TOKEN"},
		environ:     environ,
		lookupEnv: func(name string) (string, bool) {
			if name == "DSH_TOKEN" {
				return "explicit-token", true
			}
			return "", false
		},
		docMutate: func(doc *host.ExternalConfig) {
			backend := doc.Backends["dsh"]
			backend.Env = map[string]string{"DSH_TOKEN": "env:DSH_TOKEN"}
			doc.Backends["dsh"] = backend
		},
	})
	h.waitReady()
	logs := h.logs.String()
	for _, secret := range []string{"explicit-token", "postgres://leak", "leak-key"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("service log leaked %q:\n%s", secret, logs)
		}
	}
}

func TestProjectionFailureRetriesAndKeepsTheBackendAlive(t *testing.T) {
	flaky := &flakyCommitter{failures: 2}
	h := newHarness(t, harnessOptions{backendArgs: []string{"-usage"}, committer: flaky})
	h.waitReady()
	flaky.inner = h.k
	ctx := context.Background()
	if _, err := h.svc.Open(ctx, h.scope, "dsh", gateway.SessionOpenRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Send(ctx, h.scope, json.RawMessage(`{"text":"retry"}`), "req-retry"); err != nil {
		t.Fatal(err)
	}
	// The first two projections fail transiently; the third attempt commits.
	h.waitForEvent(h.scope, "assistant.message")
	if states := h.svc.Backends(); states[0].State != backendReady {
		t.Fatalf("backend state after a projection failure = %+v", states)
	}
	if flaky.attempts < 3 {
		t.Fatalf("projection attempts = %d, want at least 3", flaky.attempts)
	}
}

func TestCrashRestartBackoffAndNoFakeResume(t *testing.T) {
	h := newHarness(t, harnessOptions{
		backendArgs: []string{"-crash-on-send"},
		restart:     gateway.RestartPolicy{InitialDelay: 20 * time.Millisecond, MaxDelay: 100 * time.Millisecond, MaxRestarts: 5},
	})
	h.waitReady()
	ctx := context.Background()
	if _, err := h.svc.Open(ctx, h.scope, "dsh", gateway.SessionOpenRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Send(ctx, h.scope, json.RawMessage(`{"text":"die"}`), "crash-2"); err == nil {
		t.Fatal("Send succeeded on a crash-on-send backend")
	}
	// Wait for the crash to be observed first: the connection can still look
	// ready for a moment after the RPC error, and the reconnect may beat this
	// poll. Then wait for the supervisor to bring a new sidecar up.
	deadline := time.Now().Add(10 * time.Second)
	for {
		states := h.svc.Backends()
		if len(states) == 1 && states[0].State != backendReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend never noticed the crash: %+v", states)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The supervisor reconnects (a new sidecar process) but the new process
	// does not know the session: the session stays backend-unavailable instead
	// of pretending a resume.
	deadline = time.Now().Add(10 * time.Second)
	for {
		states := h.svc.Backends()
		if len(states) == 1 && states[0].State == backendReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend never reconnected: %+v", states)
		}
		time.Sleep(20 * time.Millisecond)
	}
	view, err := h.svc.Get(ctx, h.scope)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != kernel.ExternalStatusUnavailable {
		t.Fatalf("status after reconnect = %s, want backend-unavailable", view.Status)
	}
	// A new session works on the restarted backend.
	other := h.newSession()
	if _, err := h.svc.Open(ctx, other, "dsh", gateway.SessionOpenRequest{}); err != nil {
		t.Fatalf("Open after reconnect: %v", err)
	}
}

// --- tool provider --------------------------------------------------------

func TestToolProviderCatalogueAndCall(t *testing.T) {
	mem := kernel.NewMemStore()
	k := kernel.New(mem, kernel.Options{})
	doc := host.ExternalConfig{
		Tools: map[string]host.ExternalProviderConfig{
			"tools": {Command: sidecarBin, Args: []string{"-kind=tool"}},
		},
	}
	logs := &syncBuffer{}
	svc, err := NewToolService(doc, Options{
		Committer:      k,
		Sessions:       k,
		LookupEnv:      os.LookupEnv,
		Environ:        os.Environ(),
		Logger:         testLogger(logs),
		Stderr:         &syncBuffer{},
		Restart:        gateway.RestartPolicy{InitialDelay: 20 * time.Millisecond, MaxDelay: 100 * time.Millisecond, MaxRestarts: 3},
		ConnectTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = svc.Stop(context.Background())
	})
	deadline := time.Now().Add(5 * time.Second)
	for len(svc.Tools()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	tools := svc.Tools()
	byName := make(map[string]*toolAdapter)
	for _, tool := range tools {
		byName[tool.Name()] = tool.(*toolAdapter)
	}
	echo, ok := byName["ext__tools__echo"]
	if !ok {
		t.Fatalf("tools = %v, want ext__tools__echo", toolNames(tools))
	}
	result, err := echo.Execute(context.Background(), "call-1", json.RawMessage(`{"text":"hi"}`), nil)
	if err != nil {
		t.Fatalf("echo: %v", err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("echo content = %+v", result.Content)
	}
	text, ok := result.Content[0].(agentcore.TextContent)
	if !ok || text.Text != `echo: {"text":"hi"}` {
		t.Fatalf("echo content = %+v", result.Content)
	}
	fail, ok := byName["ext__tools__fail"]
	if !ok {
		t.Fatalf("tools = %v, want ext__tools__fail", toolNames(tools))
	}
	failed, err := fail.Execute(context.Background(), "call-2", nil, nil)
	if err != nil || failed.IsError == nil || !*failed.IsError {
		t.Fatalf("fail tool = %+v, %v", failed, err)
	}
}

func toolNames(tools []agentcore.AgentTool) []string {
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		out = append(out, tool.Name())
	}
	return out
}

// --- helpers --------------------------------------------------------------

type flakyCommitter struct {
	inner    kernel.Committer
	mu       sync.Mutex
	failures int
	attempts int
}

func (f *flakyCommitter) Commit(ctx context.Context, req kernel.CommitRequest) (kernel.Receipt, error) {
	f.mu.Lock()
	f.attempts++
	fail := f.failures > 0
	if fail {
		f.failures--
	}
	inner := f.inner
	f.mu.Unlock()
	if fail {
		return kernel.Receipt{}, &kernel.Error{
			Reason: kernel.ReasonStoreFailure, Op: kernel.OpCommit, Scope: req.Scope,
			Kind: req.Kind, Detail: "injected projection failure", Err: kernel.ErrStoreFailure,
		}
	}
	return inner.Commit(ctx, req)
}

func (f *flakyCommitter) Lookup(ctx context.Context, scope kernel.Scope, key kernel.KeyRef) (kernel.Receipt, bool, error) {
	f.mu.Lock()
	inner := f.inner
	f.mu.Unlock()
	return inner.Lookup(ctx, scope, key)
}

// childPID reads the pid the reference sidecar printed for its spawned child.
func childPID(t *testing.T, stderr string) int {
	t.Helper()
	match := regexp.MustCompile(`spawned child pid=(\d+)`).FindStringSubmatch(stderr)
	if match == nil {
		t.Fatalf("sidecar did not report a child pid:\n%s", stderr)
	}
	pid, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("child pid: %v", err)
	}
	return pid
}

func testLogger(buf *syncBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func nilSignal() os.Signal { return syscall.Signal(0) }

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(nilSignal()) == nil
}

// TestProcessTreeReaped proves the stdio lifecycle end to end: the sidecar's
// whole process group (including a grandchild) disappears after Stop, and no
// process keeps the sidecar's command line alive.
func TestProcessTreeReaped(t *testing.T) {
	marker := "reap-" + uuid.NewString()
	h := newHarness(t, harnessOptions{backendArgs: []string{"-spawn-child", "-prompt=" + marker}})
	h.waitReady()
	ctx := context.Background()
	if _, err := h.svc.Open(ctx, h.scope, "dsh", gateway.SessionOpenRequest{}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	pid := childPID(t, h.stderr.String())
	if !processAlive(pid) {
		t.Fatalf("spawned grandchild pid %d is not alive before Stop", pid)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.svc.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		alive := processAlive(pid)
		out, _ := exec.Command("pgrep", "-f", marker).Output()
		remaining := strings.TrimSpace(string(out))
		if !alive && remaining == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process tree survived Stop: grandchild alive=%v, pgrep=%q", alive, remaining)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
