// Process-helper tests: the crash backoff schedule and the environment
// allowlist/redaction that keep host credentials out of a sidecar.

package gateway

import (
	"strings"
	"testing"
	"time"
)

func TestRestartPolicyBackoff(t *testing.T) {
	policy := RestartPolicy{InitialDelay: 100 * time.Millisecond, MaxDelay: time.Second, Multiplier: 2, MaxRestarts: 5}
	if got := policy.Delay(1); got != 100*time.Millisecond {
		t.Errorf("delay(1) = %v, want 100ms", got)
	}
	if got := policy.Delay(2); got != 200*time.Millisecond {
		t.Errorf("delay(2) = %v, want 200ms", got)
	}
	if got := policy.Delay(5); got != time.Second {
		t.Errorf("delay(5) = %v, want it capped at 1s", got)
	}
	if !policy.Allowed(5) {
		t.Error("attempt 5 must be within a budget of 5")
	}
	if policy.Allowed(6) {
		t.Error("attempt 6 must be outside a budget of 5")
	}

	unlimited := RestartPolicy{MaxRestarts: -1}
	if !unlimited.Allowed(1000) {
		t.Error("a negative budget must be unlimited")
	}
	defaults := RestartPolicy{}
	if !defaults.Allowed(1) || defaults.Allowed(6) {
		t.Error("default budget should allow 1..5 attempts")
	}
	if got := defaults.Delay(0); got <= 0 {
		t.Errorf("delay(0) = %v, want the initial delay", got)
	}
}

func TestRestartPolicyJitterStaysInBounds(t *testing.T) {
	policy := RestartPolicy{
		InitialDelay:   100 * time.Millisecond,
		MaxDelay:       time.Second,
		Multiplier:     2,
		JitterFraction: 0.5,
	}
	for attempt := 1; attempt <= 4; attempt++ {
		base := RestartPolicy{
			InitialDelay: policy.InitialDelay,
			MaxDelay:     policy.MaxDelay,
			Multiplier:   policy.Multiplier,
		}.Delay(attempt)
		low := time.Duration(float64(base) * 0.5)
		high := time.Duration(float64(base) * 1.5)
		for iteration := 0; iteration < 64; iteration++ {
			got := policy.Delay(attempt)
			if got < low || got > high {
				t.Fatalf("jittered delay(attempt=%d) = %v, want within [%v, %v]", attempt, got, low, high)
			}
		}
	}
}

func TestFilterEnvKeepsOnlyTheAllowlist(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"HOME=/root",
		"A6API_KEY=sk-live-9f8e7d6c5b4a",
		"FRPS_TOKEN={{SECRET_vq7t4rzq}}",
		"KEEL_DB_PASSWORD=pass",
	}
	filtered := FilterEnv(base, "PATH", "HOME")
	joined := strings.Join(filtered, "\n")
	if !strings.Contains(joined, "PATH=/usr/bin") || !strings.Contains(joined, "HOME=/root") {
		t.Fatalf("filtered env = %v, want PATH and HOME", filtered)
	}
	for _, secret := range []string{"sk-live-9f8e7d6c5b4a", "{{SECRET_vq7t4rzq}}", "pass"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("filtered env leaked %q: %v", secret, filtered)
		}
	}
	if len(FilterEnv(base)) != 0 {
		t.Error("an empty allowlist must yield an empty environment")
	}
	if len(FilterEnv(nil, "PATH")) != 0 {
		t.Error("a nil base must yield an empty environment")
	}
}

func TestRedactEnvMasksSecretValues(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"A6API_KEY=sk-live-9f8e7d6c5b4a",
		"USER=keel",
	}
	redacted := RedactEnv(env)
	joined := strings.Join(redacted, "\n")
	if strings.Contains(joined, "sk-live-9f8e7d6c5b4a") {
		t.Fatalf("redacted env leaks the key: %v", redacted)
	}
	if !strings.Contains(joined, "PATH=/usr/bin") || !strings.Contains(joined, "USER=keel") {
		t.Errorf("redacted env = %v, want benign variables intact", redacted)
	}
	if !strings.Contains(joined, "A6API_KEY="+RedactSecret("sk-live-9f8e7d6c5b4a")) {
		t.Errorf("redacted env = %v, want the key masked with its fingerprint", redacted)
	}
}
