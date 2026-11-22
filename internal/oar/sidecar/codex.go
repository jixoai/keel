package sidecar

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/jixoai/keel/internal/oar"
)

// runCodexTurn runs one `codex exec --json` turn in its own process group,
// maps its JSONL event stream into OAR records and guarantees the process tree
// is gone before it returns. A cancel terminates the group (SIGTERM, then
// SIGKILL); a crash of the sidecar is covered by the host's group shutdown and
// the pid file.
func (s *Server) runCodexTurn(ctx context.Context, session *sessionState, turn *turnState, prompt string) turnResult {
	if err := ctx.Err(); err != nil {
		return turnResult{err: err}
	}
	s.codexTurn.Lock()
	defer s.codexTurn.Unlock()
	if err := ctx.Err(); err != nil {
		return turnResult{err: err}
	}
	path, err := s.cfg.resolveCodexPath()
	if err != nil {
		return turnResult{err: err}
	}
	workdir := firstNonEmpty(session.workdir, s.cfg.Codex.Workdir)
	if workdir != "" {
		if info, statErr := os.Stat(workdir); statErr != nil || !info.IsDir() {
			if statErr == nil {
				statErr = fmt.Errorf("%s is not a directory", workdir)
			}
			return turnResult{err: &oar.Error{
				Code:    oar.CodeRuntimeUnavailable,
				Runtime: oar.RuntimeCodex,
				Detail:  "codex working directory is not usable",
				Err:     statErr,
			}}
		}
	}
	sandbox := firstNonEmpty(session.sandbox, s.cfg.Codex.CodexSandbox())
	model := firstNonEmpty(session.model, s.cfg.Codex.Model)
	args := []string{"exec", "--json", "--skip-git-repo-check", "--ephemeral", "--color", "never"}
	if sandbox != "" {
		args = append(args, "--sandbox", sandbox)
	}
	if workdir != "" {
		args = append(args, "-C", workdir)
	}
	if model != "" {
		args = append(args, "-m", model)
	}
	args = append(args, s.cfg.Codex.ExtraArgs...)
	args = append(args, "--", prompt)

	command := exec.Command(path, args...)
	command.Dir = workdir
	// The sidecar's own environment is already the filtered child environment
	// (gateway.MinimalEnv plus explicit variables). Passing it on keeps Codex's
	// auth/config lookup working without re-reading the host environment.
	command.Env = os.Environ()
	command.Stdin = nil
	stderr := newBoundedBuffer(64<<10, os.Stderr)
	command.Stderr = stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		return turnResult{err: fmt.Errorf("codex stdout pipe: %w", err)}
	}
	configureProcessGroup(command)
	if err := command.Start(); err != nil {
		return turnResult{err: &oar.Error{
			Code:    oar.CodeRuntimeUnavailable,
			Runtime: oar.RuntimeCodex,
			Detail:  "cannot start codex",
			Err:     err,
		}}
	}
	pid := command.Process.Pid
	if s.cfg.CodexPIDFile != "" {
		_ = os.WriteFile(s.cfg.CodexPIDFile, []byte(fmt.Sprintf("%d\n", pid)), 0o644)
	}
	defer func() {
		if s.cfg.CodexPIDFile != "" {
			_ = os.Remove(s.cfg.CodexPIDFile)
		}
	}()

	// Cancellation is a process-group kill, not a polite request that a stuck
	// CLI can ignore. SIGTERM first, SIGKILL after a bounded grace.
	watcherDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = terminateProcess(command.Process)
			select {
			case <-watcherDone:
			case <-time.After(2 * time.Second):
				_ = killProcessTree(command.Process)
			}
		case <-watcherDone:
		}
	}()

	var (
		readErr       error
		terminalFail  string
		errorRecorded bool
	)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		eventType := codexEventType(line)
		records, mapErr := MapCodexLine(line, turn.id, s.cfg.maxOutputBytes())
		if mapErr != nil {
			session.emit(oar.Record{
				Kind:    oar.KindStatus,
				Name:    "codex.unmapped",
				Status:  oar.StatusDegraded,
				TurnID:  turn.id,
				Message: mapErr.Error(),
				Output:  boundedRaw(line, s.cfg.maxOutputBytes()),
			})
			continue
		}
		for _, record := range records {
			if record.Kind == oar.KindError {
				errorRecorded = true
			}
			if eventType == "turn.failed" {
				terminalFail = record.Message
			}
			session.emit(record)
		}
	}
	if err := scanner.Err(); err != nil {
		readErr = err
	}
	waitErr := command.Wait()
	close(watcherDone)
	// The leader may have exited while its shell children keep the group alive.
	_ = killProcessTree(command.Process)
	// Give the kernel a moment to reap the group before the host asks whether
	// any process survived.
	for attempt := 0; attempt < 20 && processGroupAlive(pid); attempt++ {
		time.Sleep(10 * time.Millisecond)
	}

	switch {
	case ctx.Err() != nil:
		return turnResult{err: ctx.Err(), errorRecorded: errorRecorded}
	case terminalFail != "":
		return turnResult{err: fmt.Errorf("codex turn failed: %s", terminalFail), errorRecorded: errorRecorded}
	case readErr != nil:
		return turnResult{err: fmt.Errorf("read codex event stream: %w", readErr), errorRecorded: errorRecorded}
	case waitErr != nil:
		tail := strings.TrimSpace(stderr.String())
		if tail != "" {
			tail = ": " + truncateText(tail, 2048)
		}
		return turnResult{err: fmt.Errorf("codex exited: %v%s", waitErr, tail), errorRecorded: errorRecorded}
	}
	return turnResult{}
}

// codexEventType extracts only the event's type for terminal classification.
func codexEventType(line []byte) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(line, &fields) != nil {
		return ""
	}
	return jsonString(fields["type"])
}

func truncateText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

// boundedBuffer keeps the last limit bytes of output and optionally tees to a
// diagnostic writer.
type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
	tee   io.Writer
}

func newBoundedBuffer(limit int, tee io.Writer) *boundedBuffer {
	if limit <= 0 {
		limit = 64 << 10
	}
	return &boundedBuffer{limit: limit, tee: tee}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.data = append(b.data, p...)
	if len(b.data) > b.limit {
		b.data = append([]byte(nil), b.data[len(b.data)-b.limit:]...)
	}
	b.mu.Unlock()
	if b.tee != nil {
		_, _ = b.tee.Write(p)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

var _ io.Writer = (*boundedBuffer)(nil)
