package capturepolicy

// Capture modes select when the execution space's changes settle into layerfs
// as an fs.capture event (change keel-harness-v2, W3 capture policy override).
//
// The vocabulary lives in this leaf package, not in layerfs: the capture policy
// is an execution policy the kernel runner pins per attempt, and the kernel
// must not depend on a storage service (internal/architecture
// TestKernelDoesNotImportV2Layers). The host configuration document, the bash
// bridge and the run runner all name the same three values. Nothing in this
// file touches a store or a workspace; it is a pure data contract.

import (
	"context"
	"fmt"
	"strings"
)

// CaptureMode is one capture policy.
type CaptureMode string

const (
	// CaptureModePerToolCall settles the execution space after every bash
	// execution (the default, and the pre-override behavior).
	CaptureModePerToolCall CaptureMode = "per-tool-call"
	// CaptureModeCheckpoint suppresses per-execution settles and captures once
	// when the run (attempt) completes. The run's terminal hook is the trigger;
	// an explicit checkpoint is also accepted.
	CaptureModeCheckpoint CaptureMode = "checkpoint"
	// CaptureModeManual disables every automatic settle. Only an explicit
	// checkpoint surface (bash {action:"checkpoint"}) captures.
	CaptureModeManual CaptureMode = "manual"

	// DefaultCaptureMode is the policy an empty configuration selects.
	DefaultCaptureMode = CaptureModePerToolCall
)

// CaptureModeError rejects a mode outside the closed vocabulary. It is the
// typed error the config validator and the runtime seam return; an unknown
// value never falls back silently to a default.
type CaptureModeError struct {
	Mode CaptureMode
}

func (e *CaptureModeError) Error() string {
	return fmt.Sprintf("capturepolicy: unknown capture mode %q (want per-tool-call|checkpoint|manual)", string(e.Mode))
}

// Code is the typed wire code for a policy vocabulary error.
func (e *CaptureModeError) Code() string { return "invalid_capture_mode" }

// CaptureTriggerError reports a mode that has no configured trigger. Choosing
// checkpoint/manual without the run-completion hook (checkpoint) or the
// explicit checkpoint surface (manual) would silently swallow capture, so the
// runtime refuses the attempt instead.
type CaptureTriggerError struct {
	Mode CaptureMode
	// Missing names the absent trigger ("run-completion checkpoint hook" or
	// "explicit checkpoint surface").
	Missing string
}

func (e *CaptureTriggerError) Error() string {
	missing := strings.TrimSpace(e.Missing)
	if missing == "" {
		missing = "capture trigger"
	}
	return fmt.Sprintf("capturepolicy: capture mode %q has no %s; refusing to swallow capture", string(e.Mode), missing)
}

// Code is the typed wire code for a mode with no trigger.
func (e *CaptureTriggerError) Code() string { return "capture_trigger_missing" }

// ParseCaptureMode parses one mode value. Empty is the default (a tenant
// document omits the field to inherit); any other value must belong to the
// closed set.
func ParseCaptureMode(raw string) (CaptureMode, error) {
	mode := CaptureMode(strings.TrimSpace(raw))
	switch mode {
	case "":
		return DefaultCaptureMode, nil
	case CaptureModePerToolCall, CaptureModeCheckpoint, CaptureModeManual:
		return mode, nil
	default:
		return "", &CaptureModeError{Mode: mode}
	}
}

// Valid reports whether the mode is one of the closed vocabulary. Empty is not
// valid here: callers that allow omission use Effective.
func (m CaptureMode) Valid() bool {
	switch m {
	case CaptureModePerToolCall, CaptureModeCheckpoint, CaptureModeManual:
		return true
	default:
		return false
	}
}

// Effective resolves an omitted mode to the default. An unknown non-empty mode
// is returned unchanged so the caller fails closed instead of guessing.
func (m CaptureMode) Effective() CaptureMode {
	if m == "" {
		return DefaultCaptureMode
	}
	return m
}

type captureModeKey struct{}

// WithCaptureMode pins the attempt's capture mode on the context tools execute
// under. The runner stamps it once per attempt, so a configuration rotation
// cannot change the policy of a run already in flight.
func WithCaptureMode(ctx context.Context, mode CaptureMode) context.Context {
	if mode == "" {
		mode = DefaultCaptureMode
	}
	return context.WithValue(ctx, captureModeKey{}, mode)
}

// CaptureModeFromContext reads the pinned mode. A context without the value
// carries no policy: the caller applies the default.
func CaptureModeFromContext(ctx context.Context) (CaptureMode, bool) {
	mode, ok := ctx.Value(captureModeKey{}).(CaptureMode)
	return mode, ok && mode != ""
}
