package bashbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	gbfs "github.com/ewhauser/gbash/fs"
)

// runInSession executes script in a fresh session and returns the result plus
// the streamed output. The streams are the ones the caller sees; gbash's own
// capture buffer is never read.
func runInSession(t *testing.T, bridge *Bridge, script string) (Result, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := bridge.NewSession(ctx)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer session.Close()
	var stdout, stderr bytes.Buffer
	result, err := session.Run(ctx, RunSpec{Command: script, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatalf("run %q: %v", script, err)
	}
	return result, stdout.String(), stderr.String()
}

func TestSandboxHasNoHostBinariesAndNoNetwork(t *testing.T) {
	bridge := New(Config{})
	result, stdout, stderr := runInSession(t, bridge, `
for c in git python3 node docker kubectl curl wget ssh scp; do
  if command -v "$c" >/dev/null 2>&1; then echo "present:$c"; else echo "absent:$c"; fi
done
echo "pid=$$"
echo "path=$PATH"
git
`)
	if result.ExitCode != 127 {
		t.Fatalf("exit = %d, want 127 for an unregistered command (stdout=%q stderr=%q)", result.ExitCode, stdout, stderr)
	}
	for _, name := range []string{"git", "python3", "node", "docker", "kubectl", "curl", "wget", "ssh", "scp"} {
		if !strings.Contains(stdout, "absent:"+name) {
			t.Fatalf("host binary %q looks reachable: %q", name, stdout)
		}
	}
	if strings.Contains(stdout, "present:") {
		t.Fatalf("a host binary is present: %q", stdout)
	}
	if !strings.Contains(stdout, "pid=1") {
		t.Fatalf("$$ = %q, want gbash's virtual pid 1", stdout)
	}
	if !strings.Contains(stdout, "path=/usr/bin:/bin") {
		t.Fatalf("PATH = %q, want the virtual stub dirs", stdout)
	}
	if !strings.Contains(stderr, "git: command not found") {
		t.Fatalf("stderr = %q, want command not found", stderr)
	}
}

func TestHostPathsAreUnreachable(t *testing.T) {
	bridge := New(Config{})
	_, stdout, _ := runInSession(t, bridge, `
cat /etc/passwd >/dev/null 2>&1; echo "etc-nonzero=$?"
ls /Users >/dev/null 2>&1; echo "users-nonzero=$?"
cat ~/.ssh/id_rsa >/dev/null 2>&1; echo "ssh-nonzero=$?"
`)
	for _, want := range []string{"etc-nonzero=", "users-nonzero=", "ssh-nonzero="} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if strings.HasSuffix(line, "=0") {
			t.Fatalf("host path was reachable: %q", line)
		}
	}
}

func TestWorkspaceAndCommandInjection(t *testing.T) {
	seeded := gbfs.SeededMemory(gbfs.InitialFiles{
		"/seed.txt": {Content: []byte("seeded-content")},
	})
	bridge := New(Config{
		Workspace: staticWorkspace{fs: seeded},
		Commands: []Command{{
			Name: "keel-note",
			Run: func(ctx context.Context, inv Invocation) error {
				if len(inv.Args) == 0 {
					fmt.Fprintln(inv.Stderr, "usage: keel-note <text>")
					return nil
				}
				payload, _ := json.Marshal(map[string]any{"args": inv.Args, "cwd": inv.Cwd})
				if err := inv.FS.WriteFile(ctx, "/workspace/note.txt", append(payload, '\n'), 0o644); err != nil {
					return err
				}
				data, err := inv.FS.ReadFile(ctx, "/workspace/note.txt")
				if err != nil {
					return err
				}
				fmt.Fprintf(inv.Stdout, "wrote=%s", data)
				return nil
			},
		}},
	})
	_, stdout, stderr := runInSession(t, bridge, `cat /workspace/seed.txt; keel-note hello world; cat /workspace/note.txt`)
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	if !strings.Contains(stdout, "seeded-content") {
		t.Fatalf("seeded workspace file not readable: %q", stdout)
	}
	if !strings.Contains(stdout, `"args":["hello","world"]`) || !strings.Contains(stdout, `"cwd":"/workspace"`) {
		t.Fatalf("injected command did not run with the expected invocation: %q", stdout)
	}
	if !strings.Contains(stdout, "wrote=") {
		t.Fatalf("injected command did not read back its write: %q", stdout)
	}
}

func TestDefaultWorkspaceIsScratchAndMountIsNotRoot(t *testing.T) {
	// A workspace mount at "/" must be refused: gbash refuses it and the
	// sandbox layout would pollute the backend.
	bridge := New(Config{Workspace: staticWorkspace{fs: gbfs.Memory(), point: "/"}})
	ctx := context.Background()
	if _, err := bridge.NewSession(ctx); err == nil {
		t.Fatal("mounting the workspace at / must fail")
	}

	// The default workspace is a per-session scratch dir, removed on close.
	temp := TempDirWorkspace{Root: t.TempDir()}
	mount, err := temp.Mount(ctx)
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	if mount.Point != DefaultMountPoint {
		t.Fatalf("mount point = %q, want %q", mount.Point, DefaultMountPoint)
	}
	if mount.Cleanup == nil {
		t.Fatal("temp workspace has no cleanup")
	}
	if err := mount.Cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

func TestBoundedOutputIsFixedWindow(t *testing.T) {
	bridge := New(Config{HeadLimit: 64, TailLimit: 64})
	result, stdout, stderr := runInSession(t, bridge, `i=0; while [ $i -lt 500 ]; do echo "line-$i-abcdefghij"; i=$((i+1)); done`)
	if result.ExitCode != 0 {
		t.Fatalf("exit = %d stderr=%q", result.ExitCode, stderr)
	}
	if len(stdout) < 5000 {
		t.Fatalf("streamed output = %d bytes, want the full stream", len(stdout))
	}
	bound := NewBounded(64, 64)
	bound.Write([]byte(stdout))
	head, tail, total := bound.Snapshot()
	if total != int64(len(stdout)) {
		t.Fatalf("total = %d, want %d", total, len(stdout))
	}
	if len(head) > 64 || len(tail) > 64 {
		t.Fatalf("head/tail = %d/%d bytes, want <= 64/64", len(head), len(tail))
	}
	if !strings.HasPrefix(stdout, head) {
		t.Fatalf("head is not the stream prefix: %q", head)
	}
	if !strings.HasSuffix(stdout, tail) {
		t.Fatalf("tail is not the stream suffix: %q", tail)
	}
	if strings.Contains(head, tail) {
		t.Fatalf("head and tail overlap: head=%q tail=%q", head, tail)
	}
}

func TestCancellationYields130(t *testing.T) {
	bridge := New(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	session, err := bridge.NewSession(ctx)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer session.Close()
	var stdout bytes.Buffer
	done := make(chan struct{})
	var result Result
	var runErr error
	go func() {
		defer close(done)
		result, runErr = session.Run(ctx, RunSpec{Command: `echo started; sleep 30; echo never`, Stdout: &stdout})
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation did not stop the command")
	}
	if runErr != nil {
		t.Fatalf("run error: %v", runErr)
	}
	if result.ExitCode != 130 {
		t.Fatalf("exit = %d, want 130 (canceled)", result.ExitCode)
	}
	if result.TimedOut {
		t.Fatal("TimedOut must stay false: the bridge never uses ExecutionRequest.Timeout")
	}
	if !strings.Contains(stdout.String(), "started") {
		t.Fatalf("pre-cancel output was lost: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "never") {
		t.Fatalf("canceled command kept running: %q", stdout.String())
	}
}

type staticWorkspace struct {
	fs    any
	point string
}

func (w staticWorkspace) Mount(context.Context) (Mount, error) {
	return Mount{Point: w.point, FS: w.fs}, nil
}

// TestSessionsAreIndependent proves the per-call session requirement: a long
// command in one session does not serialize a second session (a single gbash
// session does serialize Exec, GBASH-CONTRACT §5.3).
func TestSessionsAreIndependent(t *testing.T) {
	bridge := New(Config{})
	ctx := context.Background()
	first, err := bridge.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := bridge.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	var wg sync.WaitGroup
	start := time.Now()
	var firstDuration time.Duration
	wg.Add(1)
	go func() {
		defer wg.Done()
		begin := time.Now()
		if _, err := first.Run(ctx, RunSpec{Command: `sleep 0.4`}); err != nil {
			t.Errorf("first run: %v", err)
		}
		firstDuration = time.Since(begin)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := second.Run(ctx, RunSpec{Command: `sleep 0.4`}); err != nil {
			t.Errorf("second run: %v", err)
		}
	}()
	wg.Wait()
	if elapsed := time.Since(start); elapsed > 700*time.Millisecond {
		t.Fatalf("two sessions took %s, want parallel execution (<700ms)", elapsed)
	}
	if firstDuration > 700*time.Millisecond {
		t.Fatalf("first session took %s", firstDuration)
	}
}
