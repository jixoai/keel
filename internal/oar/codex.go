package oar

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// CodexConfig configures the Codex CLI adapter.
type CodexConfig struct {
	// Path is the codex executable. Empty means look it up on PATH.
	Path string
	// Workdir is the directory Codex runs in. The sidecar uses it as the
	// session default; a per-session workdir may override it.
	Workdir string
	// Sandbox is the Codex sandbox mode (read-only by default). OAR never
	// defaults to danger-full-access.
	Sandbox string
	// Model is an optional model override.
	Model string
	// Timeout bounds one Codex turn. Zero uses 10 minutes.
	Timeout time.Duration
	// ExtraArgs are appended to `codex exec` before the prompt.
	ExtraArgs []string
}

// CodexInfo is the probed identity of an installed Codex CLI.
type CodexInfo struct {
	Path    string
	Version string
}

// CodexPreflight verifies that the real Codex adapter can run at all before a
// sidecar is spawned or a session is opened. It is intentionally explicit:
// a missing binary is runtime_unavailable, an old CLI without `exec --json` is
// runtime_unsupported, and neither is silently replaced with a fake.
func CodexPreflight(ctx context.Context, cfg CodexConfig) (CodexInfo, error) {
	path := strings.TrimSpace(cfg.Path)
	if path == "" {
		found, err := exec.LookPath("codex")
		if err != nil {
			return CodexInfo{}, &Error{
				Code:    CodeRuntimeUnavailable,
				Runtime: RuntimeCodex,
				Detail:  "codex is not installed or not on PATH; install the Codex CLI, or use runtime=fake for deterministic tests (a fake is never used as a live model)",
				Err:     err,
			}
		}
		path = found
	}
	info, err := runCodexProbe(ctx, path, "--version")
	if err != nil {
		return CodexInfo{}, &Error{
			Code:    CodeRuntimeUnavailable,
			Runtime: RuntimeCodex,
			Detail:  "codex --version failed",
			Err:     err,
		}
	}
	version := strings.TrimSpace(info)
	if version == "" {
		return CodexInfo{}, &Error{Code: CodeRuntimeUnavailable, Runtime: RuntimeCodex, Detail: "codex --version printed nothing"}
	}
	help, err := runCodexProbe(ctx, path, "exec", "--help")
	if err != nil {
		return CodexInfo{}, &Error{
			Code:    CodeRuntimeUnavailable,
			Runtime: RuntimeCodex,
			Detail:  "codex exec --help failed",
			Err:     err,
		}
	}
	if !strings.Contains(help, "--json") {
		return CodexInfo{}, &Error{
			Code:    CodeRuntimeUnsupported,
			Runtime: RuntimeCodex,
			Detail:  "codex exec does not support --json; OAR requires the JSONL event stream",
		}
	}
	return CodexInfo{Path: path, Version: firstLine(version)}, nil
}

// runCodexProbe runs one bounded probe command and returns combined output.
func runCodexProbe(ctx context.Context, path string, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, path, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("timed out")
		}
		return out.String(), err
	}
	return out.String(), nil
}

func firstLine(value string) string {
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		return strings.TrimSpace(value[:index])
	}
	return strings.TrimSpace(value)
}

// CodexSandbox returns the effective sandbox mode. The default is read-only:
// an external runtime is never handed danger-full-access by accident.
func (c CodexConfig) CodexSandbox() string {
	if sandbox := strings.TrimSpace(c.Sandbox); sandbox != "" {
		return sandbox
	}
	return "read-only"
}

// CodexTimeout returns the effective turn timeout (10 minutes by default).
func (c CodexConfig) CodexTimeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 10 * time.Minute
}
