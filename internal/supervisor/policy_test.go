package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultPolicyIsAutomatic(t *testing.T) {
	policy := DefaultPolicy()
	if policy.Resume.Mode != ModeAuto {
		t.Fatalf("default resume.mode = %q, want auto", policy.Resume.Mode)
	}
	if policy.Resume.MaxAttempts != 3 {
		t.Fatalf("default maxAttempts = %d, want 3", policy.Resume.MaxAttempts)
	}
	if policy.Resume.OnExhausted != ExhaustedEscalate {
		t.Fatalf("default onExhausted = %q, want escalate", policy.Resume.OnExhausted)
	}
	if policy.Guardrails.DuplicateCallLimit == 0 || policy.Guardrails.ConsecutiveFailureLimit == 0 || policy.Guardrails.CallTimeout == 0 {
		t.Fatalf("default guardrails are not armed: %+v", policy.Guardrails)
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("default policy invalid: %v", err)
	}
}

func TestParsePolicyJSON(t *testing.T) {
	document := `{
		"resume": {
			"mode": "manual",
			"maxAttempts": 5,
			"backoff": "2s",
			"onExhausted": "stop",
			"providerFallback": ["p1", {"providerId": "p2", "modelId": "m2"}]
		},
		"guardrails": {
			"duplicateCallLimit": 2,
			"callTimeout": "30s",
			"callTimeoutExempt": ["bash"],
			"consecutiveFailureLimit": 4,
			"maxTokens": 1000,
			"maxCostUSD": 1.5,
			"inputUSDPerMTok": 2,
			"outputUSDPerMTok": 8,
			"maxMemoryMB": 512
		}
	}`
	policy, err := ParsePolicy([]byte(document))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	if policy.Resume.Mode != ModeManual || policy.Resume.MaxAttempts != 5 ||
		policy.Resume.OnExhausted != ExhaustedStop {
		t.Fatalf("resume = %+v", policy.Resume)
	}
	if policy.Resume.Backoff.Initial != 2*time.Second || policy.Resume.Backoff.Multiplier != 2 {
		t.Fatalf("backoff = %+v", policy.Resume.Backoff)
	}
	if len(policy.Resume.ProviderFallback) != 2 ||
		policy.Resume.ProviderFallback[0].ProviderID != "p1" ||
		policy.Resume.ProviderFallback[1].String() != "p2/m2" {
		t.Fatalf("providerFallback = %+v", policy.Resume.ProviderFallback)
	}
	if policy.Guardrails.DuplicateCallLimit != 2 || policy.Guardrails.ConsecutiveFailureLimit != 4 ||
		policy.Guardrails.MaxTokens != 1000 || policy.Guardrails.MaxMemoryMB != 512 {
		t.Fatalf("guardrails = %+v", policy.Guardrails)
	}
	if time.Duration(policy.Guardrails.CallTimeout) != 30*time.Second {
		t.Fatalf("callTimeout = %s", policy.Guardrails.CallTimeout)
	}
}

func TestParsePolicyTOMLSubset(t *testing.T) {
	document := `
# a full config.toml is allowed: unrelated tables are ignored
[agent]
model = "unused"

[resume]
mode = "never" # inline comment
maxAttempts = 1
backoff = "250ms"
onExhausted = "escalate"
providerFallback = ["fallback-a", "fallback-b/model-b"]

[guardrails]
duplicateCallLimit = 7
callTimeout = "45s"
callTimeoutExempt = ["bash", "agent"]
consecutiveFailureLimit = 3
maxTokens = 2048
maxCostUSD = 0.25
maxMemoryMB = 768
`
	policy, err := ParsePolicy([]byte(document))
	if err != nil {
		t.Fatalf("ParsePolicy TOML: %v", err)
	}
	if policy.Resume.Mode != ModeNever || policy.Resume.MaxAttempts != 1 ||
		policy.Resume.Backoff.Initial != 250*time.Millisecond {
		t.Fatalf("resume = %+v", policy.Resume)
	}
	if len(policy.Resume.ProviderFallback) != 2 || policy.Resume.ProviderFallback[1].ModelID != "model-b" {
		t.Fatalf("providerFallback = %+v", policy.Resume.ProviderFallback)
	}
	if policy.Guardrails.DuplicateCallLimit != 7 || time.Duration(policy.Guardrails.CallTimeout) != 45*time.Second ||
		policy.Guardrails.ConsecutiveFailureLimit != 3 || policy.Guardrails.MaxCostUSD != 0.25 {
		t.Fatalf("guardrails = %+v", policy.Guardrails)
	}
}

func TestParsePolicyRejectsInvalidValues(t *testing.T) {
	cases := []string{
		`{"resume":{"mode":"sometimes"}}`,
		`{"resume":{"maxAttempts":-1}}`,
		`{"resume":{"onExhausted":"panic"}}`,
		`{"resume":{"backoff":{"initial":"1s","multiplier":0.5}}}`,
		`{"resume":{"unknown":true}}`,
		`[resume]
mode = "auto"
unknown = true`,
	}
	for _, document := range cases {
		if _, err := ParsePolicy([]byte(document)); err == nil {
			t.Fatalf("ParsePolicy(%q) accepted an invalid document", document)
		}
	}
}

func TestPolicyEnvironmentOverrides(t *testing.T) {
	environment := map[string]string{
		"KEEL_RESUME_MODE":               "manual",
		"KEEL_RESUME_MAX_ATTEMPTS":       "9",
		"KEEL_RESUME_BACKOFF":            "3s",
		"KEEL_RESUME_ON_EXHAUSTED":       "stop",
		"KEEL_RESUME_PROVIDER_FALLBACK":  "p1,p2/m2",
		"KEEL_GUARDRAIL_DUPLICATE_LIMIT": "1",
		"KEEL_GUARDRAIL_CALL_TIMEOUT":    "5s",
		"KEEL_GUARDRAIL_FAILURE_LIMIT":   "2",
		"KEEL_GUARDRAIL_MAX_TOKENS":      "100",
		"KEEL_GUARDRAIL_MAX_COST_USD":    "0.5",
		"KEEL_GUARDRAIL_MAX_MEMORY_MB":   "256",
	}
	getenv := func(key string) string { return environment[key] }
	policy := PolicyFromEnvironment(getenv)
	if policy.Resume.Mode != ModeManual || policy.Resume.MaxAttempts != 9 ||
		policy.Resume.Backoff.Initial != 3*time.Second || policy.Resume.OnExhausted != ExhaustedStop {
		t.Fatalf("resume = %+v", policy.Resume)
	}
	if len(policy.Resume.ProviderFallback) != 2 || policy.Resume.ProviderFallback[1].String() != "p2/m2" {
		t.Fatalf("fallbacks = %+v", policy.Resume.ProviderFallback)
	}
	if policy.Guardrails.DuplicateCallLimit != 1 || policy.Guardrails.ConsecutiveFailureLimit != 2 ||
		policy.Guardrails.MaxTokens != 100 || policy.Guardrails.MaxCostUSD != 0.5 ||
		policy.Guardrails.MaxMemoryMB != 256 || time.Duration(policy.Guardrails.CallTimeout) != 5*time.Second {
		t.Fatalf("guardrails = %+v", policy.Guardrails)
	}
}

func TestLoadPolicyFileAndInline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[resume]\nmode = \"never\"\nmaxAttempts = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := LoadPolicy(func(key string) string {
		if key == "KEEL_SUPERVISOR_POLICY_FILE" {
			return path
		}
		return ""
	})
	if err != nil {
		t.Fatalf("LoadPolicy(file): %v", err)
	}
	if policy.Resume.Mode != ModeNever || policy.Resume.MaxAttempts != 2 {
		t.Fatalf("file policy = %+v", policy.Resume)
	}

	policy, err = LoadPolicy(func(key string) string {
		if key == "KEEL_SUPERVISOR_POLICY" {
			return `{"resume":{"mode":"manual"}}`
		}
		return ""
	})
	if err != nil {
		t.Fatalf("LoadPolicy(inline): %v", err)
	}
	if policy.Resume.Mode != ModeManual {
		t.Fatalf("inline policy = %+v", policy.Resume)
	}

	if _, err := LoadPolicy(func(key string) string {
		if key == "KEEL_SUPERVISOR_POLICY_FILE" {
			return filepath.Join(dir, "missing.toml")
		}
		return ""
	}); err == nil {
		t.Fatal("a missing policy file was accepted")
	}
}

func TestBackoffDelay(t *testing.T) {
	backoff := Backoff{Initial: time.Second, Multiplier: 2, Max: 5 * time.Second}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for attempt, expected := range want {
		if got := backoff.Delay(attempt); got != expected {
			t.Fatalf("Delay(%d) = %s, want %s", attempt, got, expected)
		}
	}
	if got := (Backoff{}).Delay(3); got != 0 {
		t.Fatalf("disabled backoff = %s, want 0", got)
	}
}

func TestPolicyHashChangesWithRotation(t *testing.T) {
	first := DefaultPolicy()
	second := DefaultPolicy()
	second.Resume.MaxAttempts = 4
	if first.Hash() == second.Hash() {
		t.Fatal("policy hash did not change when maxAttempts changed")
	}
	if !strings.HasPrefix(first.Hash(), "sha256:") {
		t.Fatalf("hash = %q", first.Hash())
	}
}
