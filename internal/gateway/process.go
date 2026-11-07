// Process supervision helpers for spawned providers.
//
// The transport owns the connection and the shutdown ladder of a spawned
// provider (SpawnStdio + Close). Restart policy belongs to the supervisor
// above it, but three pieces of that contract are small, testable and belong
// with the transport: the crash backoff schedule, the environment allowlist
// that keeps host credentials out of a sidecar, and log-safe environment
// rendering. README.md documents the full DSH sidecar lifecycle contract
// (exact pin, health checks, reaper, resource budget).

package gateway

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"
)

// RestartPolicy is the bounded crash backoff a supervisor applies to a
// spawned provider: exponential growth, a cap, jitter, and a restart budget.
// "Crash loop, restart forever" is not allowed - a provider that cannot stay
// up must surface as unavailable (external-gateway spec: provider unhealthy,
// degrade by policy).
type RestartPolicy struct {
	// InitialDelay is the wait before the first restart. Zero uses 250ms.
	InitialDelay time.Duration
	// MaxDelay caps the wait. Zero uses 30s.
	MaxDelay time.Duration
	// Multiplier grows the delay per attempt. Zero uses 2.
	Multiplier float64
	// JitterFraction spreads the delay in [1-j, 1+j]; zero means no jitter.
	JitterFraction float64
	// MaxRestarts is the restart budget. Zero uses 5; negative is unlimited
	// (only for supervised, budgeted-by-the-caller deployments).
	MaxRestarts int
}

// NewRestartPolicy returns the policy with defaults filled in.
func (p RestartPolicy) normalized() RestartPolicy {
	if p.InitialDelay <= 0 {
		p.InitialDelay = 250 * time.Millisecond
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = 30 * time.Second
	}
	if p.Multiplier <= 0 {
		p.Multiplier = 2
	}
	if p.JitterFraction < 0 {
		p.JitterFraction = 0
	}
	if p.JitterFraction > 1 {
		p.JitterFraction = 1
	}
	if p.MaxRestarts == 0 {
		p.MaxRestarts = 5
	}
	return p
}

// Allowed reports whether restart attempt is within the budget. Attempt
// numbering starts at 1 for the first restart.
func (p RestartPolicy) Allowed(attempt int) bool {
	p = p.normalized()
	if p.MaxRestarts < 0 {
		return true
	}
	return attempt <= p.MaxRestarts
}

// Delay is the wait before restart attempt. The delay grows exponentially up
// to MaxDelay and is jittered, so many providers recovering from one incident
// do not restart in lockstep.
func (p RestartPolicy) Delay(attempt int) time.Duration {
	p = p.normalized()
	if attempt < 1 {
		attempt = 1
	}
	delay := float64(p.InitialDelay) * math.Pow(p.Multiplier, float64(attempt-1))
	if delay > float64(p.MaxDelay) {
		delay = float64(p.MaxDelay)
	}
	if p.JitterFraction > 0 {
		spread := delay * p.JitterFraction
		delta := (rand.Float64()*2 - 1) * spread
		delay += delta
		if delay < 0 {
			delay = 0
		}
	}
	return time.Duration(delay)
}

// DefaultEnvAllowlist is the environment a spawned provider may inherit by
// default: the process basics, nothing else. Database URLs, model API keys,
// tunnel tokens and admin keys stay in the host process.
var DefaultEnvAllowlist = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR",
}

// MinimalEnv is the safe starting point for SpawnOptions.Env: the allowlisted
// basics from base (usually os.Environ()). Adapters append exactly the
// variables their provider genuinely needs - never the whole host environment.
func MinimalEnv(base []string) []string {
	return FilterEnv(base, DefaultEnvAllowlist...)
}

// FilterEnv returns the variables from base whose names appear in keep,
// preserving base order. It is the allowlist half of the sidecar hygiene
// contract: a spawned provider gets exactly the variables it needs, never the
// host environment (which holds the database password, frp tokens, model API
// keys of unrelated providers, and so on).
func FilterEnv(base []string, keep ...string) []string {
	if len(base) == 0 || len(keep) == 0 {
		return nil
	}
	allowed := make(map[string]bool, len(keep))
	for _, name := range keep {
		allowed[name] = true
	}
	out := make([]string, 0, len(keep))
	for _, entry := range base {
		name, _, found := strings.Cut(entry, "=")
		if !found || !allowed[name] {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// RedactEnv renders an environment slice for logs: "NAME=value" pairs whose
// name looks like a secret become "NAME=redacted:sha256:...". The list itself
// (which variables a process got) stays visible, which is what an audit needs.
func RedactEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			out = append(out, entry)
			continue
		}
		if IsSensitiveKey(name) {
			out = append(out, fmt.Sprintf("%s=%s", name, RedactSecret(value)))
			continue
		}
		out = append(out, entry)
	}
	return out
}
