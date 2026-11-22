package oar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jixoai/keel/internal/gateway"
	"github.com/jixoai/keel/internal/host"
)

// Config configures the OAR vertical slice. Runtime is the adapter id:
// "codex" for the first real adapter, "fake" for deterministic tests. Empty
// means OAR is disabled: NewService returns ErrDisabled and never starts a
// process.
type Config struct {
	// Runtime is "codex" or "fake".
	Runtime string
	// SidecarPath is the OAR sidecar executable. Empty looks at KEEL_OAR_BIN,
	// then `keel-oar` on PATH.
	SidecarPath string
	// SidecarArgs overrides the sidecar arguments. Empty uses
	// ["sidecar", "-runtime=<Runtime>", ...runtime flags].
	SidecarArgs []string
	// JournalDir is the durable record journal directory. Empty uses
	// <user cache>/keel/oar.
	JournalDir string
	// Workdir is the default runtime working directory.
	Workdir string
	// Model is an optional runtime model override.
	Model string
	// Sandbox is the Codex sandbox mode (read-only by default).
	Sandbox string
	// Required is the extra capability set the caller needs.
	Required []Capability
	// RequireLiveModel rejects a simulated runtime. Production Codex wiring
	// sets this; tests may leave it false.
	RequireLiveModel bool
	// Codex configures the Codex adapter.
	Codex CodexConfig
	// Stderr receives the sidecar's stderr.
	Stderr io.Writer
	// Env is the exact sidecar environment. Nil means gateway.MinimalEnv plus
	// CODEX_HOME when the host has it.
	Env []string
	// RequestTimeout bounds one gateway request.
	RequestTimeout time.Duration
	// RecordBuffer is the live record channel capacity.
	RecordBuffer int
}

// Service is a started OAR sidecar plus its durable record journal. It is the
// small seam a host process registers; it never fakes a live model.
type Service struct {
	cfg     Config
	bridge  *Bridge
	journal *Journal
	info    CodexInfo
}

// NewService validates the configuration. It does not spawn anything; call
// Start. An unconfigured OAR returns an error wrapping ErrDisabled.
func NewService(cfg Config) (*Service, error) {
	runtime := strings.TrimSpace(cfg.Runtime)
	if runtime == "" {
		return nil, &Error{Code: CodeDisabled, Detail: "OAR is not configured", Err: ErrDisabled}
	}
	switch runtime {
	case RuntimeCodex, RuntimeFake:
	default:
		return nil, &Error{
			Code:    CodeRuntimeUnsupported,
			Runtime: runtime,
			Detail:  fmt.Sprintf("unknown runtime %q (known: %s)", runtime, strings.Join(KnownRuntimes(), ", ")),
		}
	}
	if runtime == RuntimeFake && cfg.RequireLiveModel {
		return nil, &Error{
			Code:    CodeSimulatedRuntime,
			Runtime: runtime,
			Detail:  "runtime=fake is simulated and cannot satisfy a live-model requirement",
		}
	}
	return &Service{cfg: cfg}, nil
}

// Register is the single registration seam a host process (serve.go / runtime)
// calls. It is deliberately small: configuration in, a Service out, and a
// disabled deployment is a typed error the caller can ignore.
func Register(cfg Config) (*Service, error) { return NewService(cfg) }

// Start probes the runtime (for Codex), spawns the sidecar and opens the
// journal. A failure leaves no process behind.
func (s *Service) Start(ctx context.Context) error {
	if s == nil {
		return &Error{Code: CodeDisabled, Detail: "OAR service is nil", Err: ErrDisabled}
	}
	if s.bridge != nil {
		return errors.New("oar: service is already started")
	}
	command, args, err := s.sidecarCommand(ctx)
	if err != nil {
		return err
	}
	if s.cfg.Runtime == RuntimeCodex {
		info, err := CodexPreflight(ctx, s.cfg.Codex)
		if err != nil {
			return err
		}
		s.info = info
		// The preflight may have resolved the binary from PATH; make sure the
		// sidecar uses the exact same binary.
		args = replaceFlag(args, "-codex-path", info.Path)
	}
	journalDir := s.cfg.JournalDir
	if journalDir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			cache = os.TempDir()
		}
		journalDir = filepath.Join(cache, "keel", "oar")
	}
	journal, err := NewJournal(journalDir, JournalOptions{})
	if err != nil {
		return err
	}
	env := s.cfg.Env
	if env == nil {
		env = gateway.MinimalEnv(os.Environ())
		if codexHome, ok := os.LookupEnv("CODEX_HOME"); ok && codexHome != "" {
			env = append(env, "CODEX_HOME="+codexHome)
		}
	}
	required := append([]Capability(nil), s.cfg.Required...)
	if s.cfg.RequireLiveModel {
		required = append(required, CapabilityLiveModel)
	}
	bridge, err := Dial(ctx, SpawnOptions{
		Command:        command,
		Args:           args,
		Dir:            s.cfg.Workdir,
		Env:            env,
		Stderr:         s.cfg.Stderr,
		Runtime:        s.cfg.Runtime,
		Required:       required,
		Journal:        journal,
		RequestTimeout: s.cfg.RequestTimeout,
		RecordBuffer:   s.cfg.RecordBuffer,
	})
	if err != nil {
		_ = journal.Close()
		return err
	}
	s.journal = journal
	s.bridge = bridge
	return nil
}

// Stop closes the sidecar's process group and the journal. It is idempotent.
func (s *Service) Stop(context.Context) error {
	if s == nil || s.bridge == nil {
		return nil
	}
	closeErr := s.bridge.Close()
	s.bridge = nil
	if s.journal != nil {
		if err := s.journal.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
		s.journal = nil
	}
	return closeErr
}

// Enabled reports whether a service is configured.
func (s *Service) Enabled() bool { return s != nil && s.cfg.Runtime != "" }

// Runtime returns the configured adapter id.
func (s *Service) Runtime() string {
	if s == nil {
		return ""
	}
	return s.cfg.Runtime
}

// CodexInfo returns the probed Codex identity (zero before Start).
func (s *Service) CodexInfo() CodexInfo {
	if s == nil {
		return CodexInfo{}
	}
	return s.info
}

// Declaration returns the negotiated declaration (zero before Start).
func (s *Service) Declaration() Declaration {
	if s == nil || s.bridge == nil {
		return Declaration{}
	}
	return s.bridge.Declaration()
}

// PID is the sidecar process id (0 before Start).
func (s *Service) PID() int {
	if s == nil || s.bridge == nil {
		return 0
	}
	return s.bridge.PID()
}

// JournalPath is the journal file of one backend session.
func (s *Service) JournalPath(sessionRef string) string {
	if s == nil || s.journal == nil {
		return ""
	}
	return s.journal.Path(sessionRef)
}

// Records is the live record view (nil before Start).
func (s *Service) Records() <-chan Record {
	if s == nil || s.bridge == nil {
		return nil
	}
	return s.bridge.Records()
}

// Errors reports record-contract violations (nil before Start).
func (s *Service) Errors() <-chan error {
	if s == nil || s.bridge == nil {
		return nil
	}
	return s.bridge.Errors()
}

// Open opens a session.
func (s *Service) Open(ctx context.Context, request OpenRequest) (Session, error) {
	if s == nil || s.bridge == nil {
		return Session{}, &Error{Code: CodeDisabled, Detail: "OAR service is not started", Err: ErrDisabled}
	}
	if request.Workdir == "" {
		request.Workdir = s.cfg.Workdir
	}
	if request.Model == "" {
		request.Model = s.cfg.Model
	}
	return s.bridge.Open(ctx, request)
}

// Send hands one message to a session.
func (s *Service) Send(ctx context.Context, sessionRef string, message json.RawMessage) (gateway.MessageSendResult, error) {
	if s == nil || s.bridge == nil {
		return gateway.MessageSendResult{}, &Error{Code: CodeDisabled, Detail: "OAR service is not started", Err: ErrDisabled}
	}
	return s.bridge.Send(ctx, sessionRef, message)
}

// Cancel cancels a session's current turn.
func (s *Service) Cancel(ctx context.Context, sessionRef, turnID, reason string) error {
	if s == nil || s.bridge == nil {
		return &Error{Code: CodeDisabled, Detail: "OAR service is not started", Err: ErrDisabled}
	}
	return s.bridge.Cancel(ctx, sessionRef, turnID, reason)
}

// CloseSession closes one session.
func (s *Service) CloseSession(ctx context.Context, sessionRef, reason string) error {
	if s == nil || s.bridge == nil {
		return &Error{Code: CodeDisabled, Detail: "OAR service is not started", Err: ErrDisabled}
	}
	return s.bridge.CloseSession(ctx, sessionRef, reason)
}

// Health probes the sidecar.
func (s *Service) Health(ctx context.Context) (gateway.HealthResult, error) {
	if s == nil || s.bridge == nil {
		return gateway.HealthResult{}, &Error{Code: CodeDisabled, Detail: "OAR service is not started", Err: ErrDisabled}
	}
	return s.bridge.Health(ctx)
}

// Bridge returns the underlying bridge (nil before Start). It is for tests and
// for a host that needs the raw gateway client; most callers use the Service
// methods.
func (s *Service) Bridge() *Bridge {
	if s == nil {
		return nil
	}
	return s.bridge
}

// sidecarCommand resolves the sidecar executable and arguments.
func (s *Service) sidecarCommand(context.Context) (string, []string, error) {
	path := strings.TrimSpace(s.cfg.SidecarPath)
	if path == "" {
		path = strings.TrimSpace(os.Getenv("KEEL_OAR_BIN"))
	}
	if path == "" {
		found, err := exec.LookPath("keel-oar")
		if err != nil {
			return "", nil, &Error{
				Code:    CodeRuntimeUnavailable,
				Runtime: s.cfg.Runtime,
				Detail:  "OAR sidecar executable not found; set SidecarPath or KEEL_OAR_BIN, or install keel-oar",
				Err:     err,
			}
		}
		path = found
	}
	args := append([]string(nil), s.cfg.SidecarArgs...)
	if len(args) == 0 {
		args = []string{"sidecar", "-runtime=" + s.cfg.Runtime}
		switch s.cfg.Runtime {
		case RuntimeCodex:
			args = append(args,
				"-codex-path", s.cfg.Codex.Path,
				"-workdir", s.cfg.Workdir,
				"-sandbox", s.cfg.Codex.CodexSandbox(),
				"-model", s.cfg.Model,
				"-timeout", s.cfg.Codex.CodexTimeout().String(),
			)
			for _, extra := range s.cfg.Codex.ExtraArgs {
				args = append(args, "-codex-arg", extra)
			}
		case RuntimeFake:
			args = append(args, "-fake-epoch", "2026-01-01T00:00:00Z")
		}
	}
	// An empty -codex-path is meaningful (PATH lookup); drop the pair so the
	// sidecar sees its own default instead of an empty flag value.
	args = dropEmptyFlag(args, "-codex-path")
	args = dropEmptyFlag(args, "-workdir")
	args = dropEmptyFlag(args, "-model")
	return path, args, nil
}

func dropEmptyFlag(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		if args[index] == flag && index+1 < len(args) && strings.TrimSpace(args[index+1]) == "" {
			index++
			continue
		}
		out = append(out, args[index])
	}
	return out
}

func replaceFlag(args []string, flag, value string) []string {
	if value == "" {
		return args
	}
	out := make([]string, 0, len(args)+2)
	replaced := false
	for index := 0; index < len(args); index++ {
		if args[index] == flag {
			out = append(out, flag, value)
			replaced = true
			if index+1 < len(args) {
				index++
			}
			continue
		}
		if strings.HasPrefix(args[index], flag+"=") {
			out = append(out, flag+"="+value)
			replaced = true
			continue
		}
		out = append(out, args[index])
	}
	if !replaced {
		out = append(out, flag, value)
	}
	return out
}

// ExternalBackendConfig returns the provider entry that makes the existing
// external gateway (host.ExternalConfig.Backends -> externalgw) spawn this
// sidecar as a normal agent_backend. That path reuses external.event,
// stateOwner=external and the native /input 409 rule; it does not introduce a
// second external-session model. OAR's own journal is a developer view of the
// same records.
func (c Config) ExternalBackendConfig() (host.ExternalProviderConfig, error) {
	runtime := strings.TrimSpace(c.Runtime)
	switch runtime {
	case RuntimeCodex, RuntimeFake:
	default:
		return host.ExternalProviderConfig{}, &Error{
			Code:    CodeRuntimeUnsupported,
			Runtime: runtime,
			Detail:  fmt.Sprintf("unknown runtime %q", runtime),
		}
	}
	command := strings.TrimSpace(c.SidecarPath)
	if command == "" {
		command = strings.TrimSpace(os.Getenv("KEEL_OAR_BIN"))
	}
	if command == "" {
		found, err := exec.LookPath("keel-oar")
		if err != nil {
			return host.ExternalProviderConfig{}, &Error{
				Code:    CodeRuntimeUnavailable,
				Runtime: runtime,
				Detail:  "OAR sidecar executable not found; set SidecarPath or KEEL_OAR_BIN",
				Err:     err,
			}
		}
		command = found
	}
	_, args, err := (&Service{cfg: c}).sidecarCommand(context.Background())
	if err != nil {
		return host.ExternalProviderConfig{}, err
	}
	return host.ExternalProviderConfig{
		Transport: host.ExternalTransportStdio,
		Command:   command,
		Args:      args,
		Dir:       c.Workdir,
		Env:       map[string]string{},
	}, nil
}

// ParseRuntime validates a configured runtime id.
func ParseRuntime(raw string) (string, bool) {
	for _, runtime := range KnownRuntimes() {
		if runtime == raw {
			return runtime, true
		}
	}
	return "", false
}
