// Package sidecar implements the OAR sidecar process: an agent_backend that
// speaks the existing external-gateway protocol and reports an out-of-band
// record stream for one external runtime.
//
// Two drivers share the protocol:
//
//   - fake: deterministic records for tests and for developers without a
//     runtime installed. It declares simulated=true and never claims
//     live_model.
//   - codex: spawns `codex exec --json`, maps its JSONL event stream into OAR
//     records, and kills the Codex process group on cancel/close/EOF/crash.
//
// The sidecar itself is spawned by the host through gateway.SpawnStdio, so it
// already runs in its own process group. It never writes to stdout except
// protocol frames; diagnostics go to stderr.
package sidecar

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jixoai/keel/internal/oar"
)

// Runtime ids accepted by the sidecar.
const (
	RuntimeFake  = oar.RuntimeFake
	RuntimeCodex = oar.RuntimeCodex
)

// Config is the sidecar's startup configuration. The fake fields are ignored by
// the Codex driver and vice versa.
type Config struct {
	// Runtime selects the driver ("fake" or "codex"). Empty means fake.
	Runtime string
	// Name and Version are the initialize server identity.
	Name    string
	Version string
	// RuntimeVersion is the underlying runtime's own version (for example
	// "codex-cli 0.166.0"), reported in the OAR declaration. Empty falls back to
	// the sidecar version.
	RuntimeVersion string

	// Capabilities overrides the runtime's frozen declaration. It exists for
	// negative tests (a runtime that omits a capability must be rejected); the
	// normal path uses oar.DeclarationFor.
	Capabilities []oar.Capability
	// Simulated forces simulated=true on the declaration (fake defaults to it).
	Simulated bool
	// Usage declares and emits token usage.
	Usage bool

	// Fake driver options.
	Epoch        time.Time
	StepDelay    time.Duration
	PromptPrefix string
	CrashAfter   int
	IgnoreCancel bool

	// SpawnChild starts a long-lived child at startup and writes its pid to
	// ChildPIDFile. It is the process-group reaping observable.
	SpawnChild   bool
	ChildPIDFile string

	// Codex driver options.
	Codex oar.CodexConfig
	// CodexPIDFile receives the running Codex process id per turn, so a host
	// supervisor can reap a group whose sidecar was SIGKILLed.
	CodexPIDFile string
	// MaxOutputBytes bounds one tool/model output embedded in a record.
	MaxOutputBytes int
}

func (c Config) runtime() string {
	if strings.TrimSpace(c.Runtime) == "" {
		return RuntimeFake
	}
	return strings.TrimSpace(c.Runtime)
}

func (c Config) name() string {
	if c.Name != "" {
		return c.Name
	}
	return "keel-oar-sidecar"
}

func (c Config) version() string {
	if c.Version != "" {
		return c.Version
	}
	return "1.0.0"
}

func (c Config) maxOutputBytes() int {
	if c.MaxOutputBytes > 0 {
		return c.MaxOutputBytes
	}
	return 64 << 10
}

func (c Config) validate() error {
	switch c.runtime() {
	case RuntimeFake, RuntimeCodex:
	default:
		return fmt.Errorf("oar sidecar: unknown runtime %q (want fake|codex)", c.runtime())
	}
	return nil
}

// Declaration returns the sidecar's capability declaration. The fake runtime is
// always simulated and never claims live_model.
func (c Config) Declaration() oar.Declaration {
	runtime := c.runtime()
	runtimeVersion := c.RuntimeVersion
	if runtimeVersion == "" {
		runtimeVersion = c.Version
	}
	declaration := oar.DeclarationFor(runtime, runtimeVersion)
	if runtime == RuntimeFake {
		declaration.Simulated = true
	}
	if c.Simulated {
		declaration.Simulated = true
		declaration.Capabilities = withoutCapability(declaration.Capabilities, oar.CapabilityLiveModel)
	}
	if runtime == RuntimeFake && !c.Usage {
		declaration.Capabilities = withoutCapability(declaration.Capabilities, oar.CapabilityUsage)
	}
	if c.Capabilities != nil {
		declaration.Capabilities = append([]oar.Capability(nil), c.Capabilities...)
	}
	return declaration
}

func withoutCapability(capabilities []oar.Capability, remove oar.Capability) []oar.Capability {
	out := make([]oar.Capability, 0, len(capabilities))
	for _, capability := range capabilities {
		if capability != remove {
			out = append(out, capability)
		}
	}
	return out
}

// fakeEpoch returns the deterministic timestamp base of the fake driver.
func (c Config) fakeEpoch() time.Time {
	if !c.Epoch.IsZero() {
		return c.Epoch
	}
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
}

// resolveCodexPath returns the Codex executable or a typed error.
func (c Config) resolveCodexPath() (string, error) {
	path := strings.TrimSpace(c.Codex.Path)
	if path == "" {
		found, err := exec.LookPath("codex")
		if err != nil {
			return "", &oar.Error{
				Code:    oar.CodeRuntimeUnavailable,
				Runtime: oar.RuntimeCodex,
				Detail:  "codex is not installed or not on PATH; set -codex-path, install the Codex CLI, or use -runtime=fake for deterministic tests",
				Err:     err,
			}
		}
		path = found
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", &oar.Error{
			Code:    oar.CodeRuntimeUnavailable,
			Runtime: oar.RuntimeCodex,
			Detail:  "codex executable is not accessible",
			Err:     err,
		}
	}
	if info.IsDir() {
		return "", &oar.Error{Code: oar.CodeRuntimeUnavailable, Runtime: oar.RuntimeCodex, Detail: "codex path is a directory"}
	}
	return path, nil
}
