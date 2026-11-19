package bashbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/ewhauser/gbash"
	gbfs "github.com/ewhauser/gbash/fs"
	"github.com/ewhauser/gbash/policy"
)

// Default gbash capture limits. gbash keeps its own bounded copy of stdout for
// ExecutionResult.Stdout; the streaming writer is the only path that sees the
// full output (GBASH-CONTRACT §6.3). 64KiB is enough for diagnostics and small
// enough that a 200MB command never makes gbash retain 1MiB x2 per execution.
const (
	DefaultCaptureBytes = 64 << 10
	DefaultMaxFileBytes = 8 << 20
)

// Config configures a Bridge. All fields are optional.
type Config struct {
	// Workspace supplies the writable workspace. Nil uses TempDirWorkspace.
	Workspace Workspace
	// Commands are extra sandbox commands (registry injection).
	Commands []Command
	// BaseEnv is merged into every execution before request env overrides.
	BaseEnv map[string]string
	// MaxStdoutCapture / MaxStderrCapture bound gbash's own capture buffer.
	// Non-positive values use DefaultCaptureBytes.
	MaxStdoutCapture int64
	MaxStderrCapture int64
	// MaxFileBytes bounds a single file read inside the sandbox. Non-positive
	// keeps gbash's default (8MiB); policy.Unlimited would disable DoS
	// protection and is deliberately not offered.
	MaxFileBytes int64
	// HeadLimit / TailLimit bound the retained output window per stream.
	HeadLimit int
	TailLimit int
}

// Bridge builds isolated sandbox sessions. It is stateless after New and safe
// for concurrent use.
type Bridge struct {
	workspace Workspace
	commands  []Command
	baseEnv   map[string]string
	policy    *policy.Static
	workDir   string
	head      int
	tail      int
}

// New builds a Bridge. It does not execute anything or touch the database.
func New(cfg Config) *Bridge {
	workspace := cfg.Workspace
	if workspace == nil {
		workspace = TempDirWorkspace{}
	}
	captureOut := cfg.MaxStdoutCapture
	if captureOut <= 0 {
		captureOut = DefaultCaptureBytes
	}
	captureErr := cfg.MaxStderrCapture
	if captureErr <= 0 {
		captureErr = DefaultCaptureBytes
	}
	maxFile := cfg.MaxFileBytes
	if maxFile <= 0 {
		maxFile = DefaultMaxFileBytes
	}
	// Limits are fixed, never Unlimited: gbash is not a hardened sandbox and
	// the runtime must keep its DoS floors (GBASH-CONTRACT R2/R9).
	limits := policy.Limits{
		MaxStdoutBytes:         captureOut,
		MaxStderrBytes:         captureErr,
		MaxFileBytes:           maxFile,
		MaxCommandCount:        10000,
		MaxLoopIterations:      10000,
		MaxGlobOperations:      100000,
		MaxBraceExpansionWords: 100000,
		MaxSubstitutionDepth:   50,
		MaxFunctionDepth:       1000,
	}
	head := cfg.HeadLimit
	if head <= 0 {
		head = DefaultHeadLimit
	}
	tail := cfg.TailLimit
	if tail <= 0 {
		tail = DefaultTailLimit
	}
	return &Bridge{
		workspace: workspace,
		commands:  append([]Command(nil), cfg.Commands...),
		baseEnv:   copyEnv(cfg.BaseEnv),
		policy: policy.NewStatic(&policy.Config{
			Limits:      limits,
			SymlinkMode: policy.SymlinkDeny,
		}),
		workDir: DefaultMountPoint,
		head:    head,
		tail:    tail,
	}
}

// HeadLimit / TailLimit expose the configured output window to the caller.
func (b *Bridge) HeadLimit() int { return b.head }
func (b *Bridge) TailLimit() int { return b.tail }

// NewBounded builds a Bounded configured with this bridge's window.
func (b *Bridge) NewBounded() *Bounded { return NewBounded(b.head, b.tail) }

// Session is one isolated gbash sandbox: one gbash Session, one workspace
// mount, one command registry. A tool call or a background job owns exactly one
// Session and closes it when done.
type Session struct {
	bridge  *Bridge
	runtime *gbash.Runtime
	session *gbash.Session
	mount   Mount
	workDir string

	closeOnce sync.Once
	closeErr  error
}

// NewSession provisions a sandbox session. The session has network disabled,
// the default virtual host ($$ == 1) and only the registry's commands; host
// binaries are unreachable by construction.
func (b *Bridge) NewSession(ctx context.Context) (*Session, error) {
	mount, err := b.workspace.Mount(ctx)
	if err != nil {
		return nil, fmt.Errorf("bashbridge: mount workspace: %w", err)
	}
	cleanup := mount.Cleanup
	closeMount := func() error {
		if cleanup == nil {
			return nil
		}
		return cleanup()
	}
	fsFactory, point, err := mountFactory(mount)
	if err != nil {
		_ = closeMount()
		return nil, err
	}

	registry := gbash.DefaultRegistry()
	if err := registerCommands(registry, b.commands); err != nil {
		_ = closeMount()
		return nil, err
	}
	mounts := []gbfs.MountConfig{
		{MountPoint: point, Factory: fsFactory},
		// Explicit memory mounts keep "which backend owns this path" a property
		// of this list, not of upstream layout internals (GBASH-CONTRACT §4.2).
		{MountPoint: "/bin", Factory: gbfs.Memory()},
		{MountPoint: "/usr/bin", Factory: gbfs.Memory()},
		{MountPoint: "/tmp", Factory: gbfs.Memory()},
	}
	options := []gbash.Option{
		gbash.WithRegistry(registry),
		gbash.WithFileSystem(gbash.MountableFileSystem(gbash.MountableFileSystemOptions{
			Base:       gbfs.Memory(),
			Mounts:     mounts,
			WorkingDir: point,
		})),
		gbash.WithPolicy(b.policy),
		// No WithNetwork / WithHTTPAccess: curl and every other network command
		// stays unregistered (GBASH-CONTRACT §4.3).
		// No ExecutionRequest.Timeout is ever set: upstream's timeout kills the
		// command, keel's timeout keeps it running (GBASH-CONTRACT §6.1).
	}
	if len(b.baseEnv) > 0 {
		options = append(options, gbash.WithBaseEnv(copyEnv(b.baseEnv)))
	}
	runtime, err := gbash.New(options...)
	if err != nil {
		_ = closeMount()
		return nil, fmt.Errorf("bashbridge: new runtime: %w", err)
	}
	session, err := runtime.NewSession(ctx)
	if err != nil {
		_ = closeMount()
		return nil, fmt.Errorf("bashbridge: new session: %w", err)
	}
	return &Session{
		bridge:  b,
		runtime: runtime,
		session: session,
		mount:   mount,
		workDir: point,
	}, nil
}

// WorkDir returns the session's default working directory (the mount point).
func (s *Session) WorkDir() string { return s.workDir }

// Run executes one script in this session. It blocks until the script ends or
// ctx is canceled; there is no internal timeout.
type RunSpec struct {
	Command string
	// WorkDir is a sandbox path. Empty uses the workspace mount point.
	WorkDir string
	// Env is merged over the runtime base environment.
	Env    map[string]string
	Stdout io.Writer
	Stderr io.Writer
}

// Result is the terminal fact of one execution. TimedOut is always false: the
// bridge never sets ExecutionRequest.Timeout, so gbash cannot report its own
// timeout. A canceled ctx yields ExitCode 130 and ControlStderr
// "execution canceled", exactly like upstream.
type Result struct {
	ExitCode      int
	ShellExited   bool
	ControlStderr string
	TimedOut      bool
	Duration      time.Duration
	StartedAt     time.Time
	FinishedAt    time.Time
}

// Run executes spec in the session.
func (s *Session) Run(ctx context.Context, spec RunSpec) (Result, error) {
	if s.session == nil {
		return Result{}, errors.New("bashbridge: session is closed")
	}
	if strings.TrimSpace(spec.Command) == "" {
		return Result{}, errors.New("bashbridge: command is empty")
	}
	workDir := strings.TrimSpace(spec.WorkDir)
	if workDir == "" {
		workDir = s.workDir
	} else if !strings.HasPrefix(workDir, "/") {
		return Result{}, fmt.Errorf("bashbridge: workdir %q is not an absolute sandbox path", workDir)
	} else {
		workDir = path.Clean(workDir)
	}
	stdout := spec.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := spec.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	res, err := s.session.Exec(ctx, &gbash.ExecutionRequest{
		Name:         "keel-bash",
		Script:       spec.Command,
		WorkDir:      workDir,
		Env:          copyEnv(spec.Env),
		Stdout:       stdout,
		Stderr:       stderr,
		ShellVariant: gbash.ShellVariantBash,
	})
	if err != nil {
		return Result{}, fmt.Errorf("bashbridge: exec: %w", err)
	}
	return Result{
		ExitCode:      res.ExitCode,
		ShellExited:   res.ShellExited,
		ControlStderr: res.ControlStderr,
		TimedOut:      res.TimedOut,
		Duration:      res.Duration,
		StartedAt:     res.StartedAt,
		FinishedAt:    res.FinishedAt,
	}, nil
}

// Close releases the workspace mount. gbash sessions need no explicit close;
// the mounted backend is the only resource with a lifetime.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.session = nil
		if s.mount.Cleanup != nil {
			s.closeErr = s.mount.Cleanup()
		}
	})
	return s.closeErr
}

func copyEnv(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]string, len(src))
	for key, value := range src {
		out[key] = value
	}
	return out
}
