// Package supervisor implements the recovery and guardrail policy of the
// Harness: automatic resume of interrupted runs, the escalation ladder and the
// run-level guardrails.
//
// The package owns two things and nothing else:
//
//   - the policy surface (the `[resume]` and `[guardrails]` sections of the
//     agent configuration) and its defaults, parsing and rotation identity;
//   - the orchestration that turns a non-terminal run into a ledger fact
//     (`resume.attempt`, `supervision.escalated`) and, when policy allows, a
//     re-queued attempt.
//
// The worker claim/lease path (internal/agent, internal/kernel) remains the
// recovery base. This package never claims, renews or finishes a lease; it only
// decides what the next attempt should be and records why.
package supervisor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ResumeMode is `resume.mode`. The zero value is ModeAuto: recovery is the
// default, never an opt-in.
type ResumeMode string

const (
	// ModeAuto resumes non-terminal work without an operator action.
	ModeAuto ResumeMode = "auto"
	// ModeManual leaves interrupted work parked until an operator or the parent
	// agent calls the resume endpoint.
	ModeManual ResumeMode = "manual"
	// ModeNever never resumes automatically; the run is parked and listed as
	// pending recovery.
	ModeNever ResumeMode = "never"
)

// ExhaustedAction is `resume.onExhausted`.
type ExhaustedAction string

const (
	// ExhaustedEscalate walks the escalation ladder (parent/main agent, then
	// owner, then an installed super-agent).
	ExhaustedEscalate ExhaustedAction = "escalate"
	// ExhaustedStop stops retrying and leaves the run parked.
	ExhaustedStop ExhaustedAction = "stop"
)

// Backoff is `resume.backoff`: one initial delay, a multiplier and a cap. The
// JSON/TOML form accepts either a duration string ("2s", meaning exponential
// 2s/4s/8s… capped at one minute) or an object with initial/multiplier/max.
type Backoff struct {
	Initial    time.Duration `json:"initial"`
	Multiplier float64       `json:"multiplier"`
	Max        time.Duration `json:"max"`
}

// Delay returns the wait before automatic resume attempt n (0-based). It never
// blocks; callers persist the decision in the resume.attempt fact.
func (b Backoff) Delay(attempt int) time.Duration {
	if b.Initial <= 0 {
		return 0
	}
	multiplier := b.Multiplier
	if multiplier <= 0 {
		multiplier = 2
	}
	max := b.Max
	if max <= 0 {
		max = time.Minute
	}
	delay := b.Initial
	for i := 0; i < attempt && delay < max; i++ {
		next := time.Duration(float64(delay) * multiplier)
		if next <= delay { // overflow or a multiplier below 1
			delay = max
			break
		}
		delay = next
	}
	if delay > max {
		delay = max
	}
	return delay
}

// UnmarshalJSON accepts a string or an object.
func (b *Backoff) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] == '"' {
		var raw string
		if err := json.Unmarshal(trimmed, &raw); err != nil {
			return err
		}
		parsed, err := parseDuration(raw)
		if err != nil {
			return fmt.Errorf("backoff: %w", err)
		}
		// A bare string is the common config.toml spelling: one initial delay
		// with the conventional exponential schedule.
		b.Initial = parsed
		if b.Multiplier <= 0 {
			b.Multiplier = 2
		}
		if b.Max <= 0 {
			b.Max = time.Minute
		}
		return nil
	}
	var object struct {
		Initial    *Duration `json:"initial"`
		Multiplier *float64  `json:"multiplier"`
		Max        *Duration `json:"max"`
	}
	if err := json.Unmarshal(trimmed, &object); err != nil {
		return fmt.Errorf("backoff: %w", err)
	}
	if object.Initial != nil {
		b.Initial = time.Duration(*object.Initial)
	}
	if object.Multiplier != nil {
		b.Multiplier = *object.Multiplier
	}
	if object.Max != nil {
		b.Max = time.Duration(*object.Max)
	}
	if b.Multiplier <= 0 {
		b.Multiplier = 2
	}
	if b.Max <= 0 {
		b.Max = time.Minute
	}
	return nil
}

// MarshalJSON keeps the policy document round-trippable (evidence and admin
// reads).
func (b Backoff) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"initial":    b.Initial.String(),
		"multiplier": b.Multiplier,
		"max":        b.Max.String(),
	})
}

// Duration is a time.Duration with a string JSON form ("30s", "10m").
type Duration time.Duration

// UnmarshalJSON accepts a duration string or a number of seconds.
func (d *Duration) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] == '"' {
		var raw string
		if err := json.Unmarshal(trimmed, &raw); err != nil {
			return err
		}
		parsed, err := parseDuration(raw)
		if err != nil {
			return err
		}
		*d = Duration(parsed)
		return nil
	}
	var seconds float64
	if err := json.Unmarshal(trimmed, &seconds); err != nil {
		return fmt.Errorf("duration must be a string or seconds: %w", err)
	}
	*d = Duration(time.Duration(seconds * float64(time.Second)))
	return nil
}

// MarshalJSON renders the canonical duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// String implements fmt.Stringer.
func (d Duration) String() string { return time.Duration(d).String() }

// ProviderChoice is one entry of `resume.providerFallback`. A string entry is
// "provider" or "provider/model"; the object form is the explicit version.
type ProviderChoice struct {
	ProviderID string `json:"providerId"`
	ModelID    string `json:"modelId,omitempty"`
}

// String renders the choice back to the config spelling.
func (p ProviderChoice) String() string {
	if p.ModelID == "" {
		return p.ProviderID
	}
	return p.ProviderID + "/" + p.ModelID
}

// ProviderFallback is an ordered list of fallback provider choices.
type ProviderFallback []ProviderChoice

// UnmarshalJSON accepts ["p1", "p2/m2"] and [{"providerId":"p1"}, …].
func (p *ProviderFallback) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(trimmed, &entries); err != nil {
		return fmt.Errorf("providerFallback: %w", err)
	}
	out := make(ProviderFallback, 0, len(entries))
	for _, entry := range entries {
		entry = bytes.TrimSpace(entry)
		if len(entry) == 0 {
			continue
		}
		if entry[0] == '"' {
			var raw string
			if err := json.Unmarshal(entry, &raw); err != nil {
				return err
			}
			choice, err := ParseProviderChoice(raw)
			if err != nil {
				return err
			}
			out = append(out, choice)
			continue
		}
		var choice ProviderChoice
		if err := json.Unmarshal(entry, &choice); err != nil {
			return err
		}
		choice.ProviderID = strings.TrimSpace(choice.ProviderID)
		choice.ModelID = strings.TrimSpace(choice.ModelID)
		if choice.ProviderID == "" {
			return fmt.Errorf("providerFallback entry has no providerId")
		}
		out = append(out, choice)
	}
	*p = out
	return nil
}

// ParseProviderChoice parses "provider" or "provider/model".
func ParseProviderChoice(raw string) (ProviderChoice, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ProviderChoice{}, errors.New("provider fallback entry is empty")
	}
	provider, model, found := strings.Cut(trimmed, "/")
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" {
		return ProviderChoice{}, fmt.Errorf("provider fallback %q has no provider id", raw)
	}
	if !found {
		model = ""
	}
	return ProviderChoice{ProviderID: provider, ModelID: model}, nil
}

// ResumePolicy is the `[resume]` section.
type ResumePolicy struct {
	Mode             ResumeMode       `json:"mode"`
	MaxAttempts      int              `json:"maxAttempts"`
	Backoff          Backoff          `json:"backoff"`
	OnExhausted      ExhaustedAction  `json:"onExhausted"`
	ProviderFallback ProviderFallback `json:"providerFallback,omitempty"`
}

// GuardrailPolicy is the `[guardrails]` section. A zero value disables every
// guardrail; DefaultGuardrails() is what a deployment runs unless it overrides
// the values.
type GuardrailPolicy struct {
	// DuplicateCallLimit bounds consecutive identical tool calls (same name and
	// arguments). The call that exceeds the limit is blocked with an explanatory
	// error result. Zero disables the check.
	DuplicateCallLimit int `json:"duplicateCallLimit"`
	// CallTimeout bounds one tool call. Zero disables it. Tools whose own
	// contract includes a long-running wait (bash promotes to a background job;
	// agent waits for a sub-agent) are exempt by default.
	CallTimeout       Duration `json:"callTimeout"`
	CallTimeoutExempt []string `json:"callTimeoutExempt,omitempty"`
	// ConsecutiveFailureLimit aborts the run after this many consecutive error
	// tool results. Zero disables it.
	ConsecutiveFailureLimit int `json:"consecutiveFailureLimit"`
	// MaxTokens aborts the run once the attempt's reported token total exceeds
	// the budget. Zero disables it.
	MaxTokens int `json:"maxTokens"`
	// MaxCostUSD aborts the run once the attempt's estimated cost exceeds the
	// budget. InputUSDPerMTok/OutputUSDPerMTok are the rates used; a zero rate
	// leaves that side unpriced. MaxCostUSD=0 disables the check.
	MaxCostUSD       float64 `json:"maxCostUSD"`
	InputUSDPerMTok  float64 `json:"inputUSDPerMTok"`
	OutputUSDPerMTok float64 `json:"outputUSDPerMTok"`
	// MaxMemoryMB aborts the run when the process heap in use exceeds the
	// threshold at a turn boundary. Zero disables it. Only the tripping run is
	// cancelled; other runs are untouched.
	MaxMemoryMB int `json:"maxMemoryMB"`
}

// Policy is the complete supervisor policy document (`[resume]` and
// `[guardrails]` of config.toml).
type Policy struct {
	Resume     ResumePolicy    `json:"resume"`
	Guardrails GuardrailPolicy `json:"guardrails"`
}

// ConfigVersion is the configuration identity a policy decision ran under. It
// is recorded in every resume/escalation fact so "which configuration did the
// recovery use" is answerable, and a rotation only affects facts written after
// it.
type ConfigVersion struct {
	Hash       string `json:"hash,omitempty"`
	Revision   int64  `json:"revision,omitempty"`
	Generation int64  `json:"generation,omitempty"`
}

// DefaultPolicy is the built-in default: automatic recovery, three automatic
// resumes, exponential backoff capped at one minute, escalation on exhaustion,
// and the standard guardrail set.
func DefaultPolicy() Policy {
	return Policy{
		Resume: ResumePolicy{
			Mode:        ModeAuto,
			MaxAttempts: 3,
			Backoff:     Backoff{Initial: time.Second, Multiplier: 2, Max: time.Minute},
			OnExhausted: ExhaustedEscalate,
		},
		Guardrails: DefaultGuardrails(),
	}
}

// DefaultGuardrails is the guardrail set a deployment runs without overrides.
func DefaultGuardrails() GuardrailPolicy {
	return GuardrailPolicy{
		DuplicateCallLimit:      3,
		CallTimeout:             Duration(10 * time.Minute),
		CallTimeoutExempt:       []string{"bash", "agent"},
		ConsecutiveFailureLimit: 5,
	}
}

// Validate rejects values the supervisor cannot honor. It is fail-closed: an
// invalid policy is refused rather than silently approximated.
func (p Policy) Validate() error {
	switch p.Resume.Mode {
	case ModeAuto, ModeManual, ModeNever:
	default:
		return fmt.Errorf("resume.mode %q is not one of auto|manual|never", p.Resume.Mode)
	}
	if p.Resume.MaxAttempts < 0 {
		return fmt.Errorf("resume.maxAttempts must be >= 0")
	}
	if p.Resume.Backoff.Initial < 0 {
		return fmt.Errorf("resume.backoff initial delay must be >= 0")
	}
	if p.Resume.Backoff.Initial > 0 && p.Resume.Backoff.Multiplier < 1 {
		return fmt.Errorf("resume.backoff multiplier must be >= 1")
	}
	switch p.Resume.OnExhausted {
	case ExhaustedEscalate, ExhaustedStop:
	default:
		return fmt.Errorf("resume.onExhausted %q is not one of escalate|stop", p.Resume.OnExhausted)
	}
	if p.Guardrails.DuplicateCallLimit < 0 {
		return fmt.Errorf("guardrails.duplicateCallLimit must be >= 0")
	}
	if p.Guardrails.CallTimeout < 0 {
		return fmt.Errorf("guardrails.callTimeout must be >= 0")
	}
	if p.Guardrails.ConsecutiveFailureLimit < 0 {
		return fmt.Errorf("guardrails.consecutiveFailureLimit must be >= 0")
	}
	if p.Guardrails.MaxTokens < 0 || p.Guardrails.MaxCostUSD < 0 || p.Guardrails.MaxMemoryMB < 0 {
		return fmt.Errorf("guardrail budgets must be >= 0")
	}
	return nil
}

// Hash returns the canonical policy identity written into recovery facts.
func (p Policy) Hash() string {
	encoded, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ParsePolicy parses the policy document. JSON is detected by its leading `{`;
// everything else is read as the TOML subset used by config.toml (`[resume]`,
// `[guardrails]`, scalar keys and string arrays). Unknown sections in a larger
// config.toml are ignored; unknown keys inside the two supervised sections are
// refused by the strict JSON decode.
func ParsePolicy(data []byte) (Policy, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return DefaultPolicy(), nil
	}
	policy := DefaultPolicy()
	if trimmed[0] == '{' {
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&policy); err != nil {
			return Policy{}, fmt.Errorf("supervisor: parse policy JSON: %w", err)
		}
	} else {
		document, err := parseTOMLSubset(trimmed)
		if err != nil {
			return Policy{}, err
		}
		encoded, err := json.Marshal(document)
		if err != nil {
			return Policy{}, fmt.Errorf("supervisor: encode policy TOML: %w", err)
		}
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&policy); err != nil {
			return Policy{}, fmt.Errorf("supervisor: parse policy TOML: %w", err)
		}
	}
	if err := policy.Validate(); err != nil {
		return Policy{}, fmt.Errorf("supervisor: %w", err)
	}
	return policy, nil
}

// LoadPolicy reads the deployment policy: a file named by
// KEEL_SUPERVISOR_POLICY_FILE, an inline document in KEEL_SUPERVISOR_POLICY,
// then per-field KEEL_RESUME_*/KEEL_GUARDRAIL_* overrides (highest priority).
// An unreadable or invalid document is an error; callers decide whether to fail
// boot or fall back to a manual policy.
func LoadPolicy(getenv func(string) string) (Policy, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	policy := DefaultPolicy()
	path := strings.TrimSpace(getenv("KEEL_SUPERVISOR_POLICY_FILE"))
	inline := strings.TrimSpace(getenv("KEEL_SUPERVISOR_POLICY"))
	switch {
	case path != "":
		data, err := os.ReadFile(path)
		if err != nil {
			return Policy{}, fmt.Errorf("supervisor: read policy file %q: %w", path, err)
		}
		policy, err = ParsePolicy(data)
		if err != nil {
			return Policy{}, err
		}
	case inline != "":
		var err error
		policy, err = ParsePolicy([]byte(inline))
		if err != nil {
			return Policy{}, err
		}
	}
	applyEnvironment(&policy, getenv)
	if err := policy.Validate(); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

// PolicyFromEnvironment returns the default policy with environment overrides.
// It is the guardrail source the runner consults when no deployment policy was
// injected.
func PolicyFromEnvironment(getenv func(string) string) Policy {
	policy := DefaultPolicy()
	applyEnvironment(&policy, getenv)
	return policy
}

// GuardrailsFromEnvironment is the runner's per-attempt source. The runner
// captures the value once per attempt, so a policy rotation changes only later
// attempts.
func GuardrailsFromEnvironment(getenv func(string) string) GuardrailPolicy {
	return PolicyFromEnvironment(getenv).Guardrails
}

func applyEnvironment(policy *Policy, getenv func(string) string) {
	if getenv == nil {
		return
	}
	if value := strings.TrimSpace(getenv("KEEL_RESUME_MODE")); value != "" {
		policy.Resume.Mode = ResumeMode(value)
	}
	if value := strings.TrimSpace(getenv("KEEL_RESUME_MAX_ATTEMPTS")); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed >= 0 {
			policy.Resume.MaxAttempts = parsed
		}
	}
	if value := strings.TrimSpace(getenv("KEEL_RESUME_BACKOFF")); value != "" {
		if parsed, err := parseDuration(value); err == nil {
			policy.Resume.Backoff.Initial = parsed
		}
	}
	if value := strings.TrimSpace(getenv("KEEL_RESUME_ON_EXHAUSTED")); value != "" {
		policy.Resume.OnExhausted = ExhaustedAction(value)
	}
	if value := strings.TrimSpace(getenv("KEEL_RESUME_PROVIDER_FALLBACK")); value != "" {
		entries := strings.Split(value, ",")
		fallbacks := make(ProviderFallback, 0, len(entries))
		for _, entry := range entries {
			choice, err := ParseProviderChoice(entry)
			if err == nil {
				fallbacks = append(fallbacks, choice)
			}
		}
		policy.Resume.ProviderFallback = fallbacks
	}
	if value := strings.TrimSpace(getenv("KEEL_GUARDRAIL_DUPLICATE_LIMIT")); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed >= 0 {
			policy.Guardrails.DuplicateCallLimit = parsed
		}
	}
	if value := strings.TrimSpace(getenv("KEEL_GUARDRAIL_CALL_TIMEOUT")); value != "" {
		if parsed, err := parseDuration(value); err == nil && parsed >= 0 {
			policy.Guardrails.CallTimeout = Duration(parsed)
		}
	}
	if value := strings.TrimSpace(getenv("KEEL_GUARDRAIL_FAILURE_LIMIT")); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed >= 0 {
			policy.Guardrails.ConsecutiveFailureLimit = parsed
		}
	}
	if value := strings.TrimSpace(getenv("KEEL_GUARDRAIL_MAX_TOKENS")); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed >= 0 {
			policy.Guardrails.MaxTokens = parsed
		}
	}
	if value := strings.TrimSpace(getenv("KEEL_GUARDRAIL_MAX_COST_USD")); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil && parsed >= 0 {
			policy.Guardrails.MaxCostUSD = parsed
		}
	}
	if value := strings.TrimSpace(getenv("KEEL_GUARDRAIL_INPUT_USD_PER_MTOK")); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil && parsed >= 0 {
			policy.Guardrails.InputUSDPerMTok = parsed
		}
	}
	if value := strings.TrimSpace(getenv("KEEL_GUARDRAIL_OUTPUT_USD_PER_MTOK")); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil && parsed >= 0 {
			policy.Guardrails.OutputUSDPerMTok = parsed
		}
	}
	if value := strings.TrimSpace(getenv("KEEL_GUARDRAIL_MAX_MEMORY_MB")); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed >= 0 {
			policy.Guardrails.MaxMemoryMB = parsed
		}
	}
}

func parseDuration(raw string) (time.Duration, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, errors.New("duration is empty")
	}
	// A bare number is seconds, matching the JSON number form.
	if !strings.ContainsAny(trimmed, "hmsuµn") {
		if seconds, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return time.Duration(seconds * float64(time.Second)), nil
		}
	}
	parsed, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", raw)
	}
	return parsed, nil
}

// --- minimal TOML subset ----------------------------------------------------

// parseTOMLSubset reads the subset a config.toml policy section needs: table
// headers, scalar key/value pairs, quoted strings, numbers, booleans and
// one-dimensional arrays of strings. Unknown tables are skipped, so the parser
// can point at a full config.toml without understanding it.
func parseTOMLSubset(data []byte) (map[string]any, error) {
	document := map[string]any{}
	section := ""
	for lineNo, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(stripTOMLComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") || strings.HasPrefix(line, "[[") {
				return nil, fmt.Errorf("supervisor: policy TOML line %d: unsupported table header %q", lineNo+1, line)
			}
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			switch section {
			case "resume", "guardrails":
			default:
				section = "" // unrelated config.toml table
			}
			continue
		}
		if section == "" {
			continue
		}
		key, rawValue, ok := splitTOMLKeyValue(line)
		if !ok {
			return nil, fmt.Errorf("supervisor: policy TOML line %d: expected key = value", lineNo+1)
		}
		value, err := parseTOMLValue(rawValue)
		if err != nil {
			return nil, fmt.Errorf("supervisor: policy TOML line %d (%s.%s): %w", lineNo+1, section, key, err)
		}
		table, _ := document[section].(map[string]any)
		if table == nil {
			table = map[string]any{}
			document[section] = table
		}
		table[key] = value
	}
	return document, nil
}

func stripTOMLComment(line string) string {
	var quote rune
	escaped := false
	for i, char := range line {
		if escaped {
			escaped = false
			continue
		}
		if quote != 0 {
			if char == '\\' && quote == '"' {
				escaped = true
				continue
			}
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '"', '\'':
			quote = char
		case '#':
			return line[:i]
		}
	}
	return line
}

func splitTOMLKeyValue(line string) (string, string, bool) {
	index := strings.IndexByte(line, '=')
	if index <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:index]), strings.TrimSpace(line[index+1:]), true
}

func parseTOMLValue(raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("value is empty")
	}
	switch raw[0] {
	case '"':
		var value string
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return nil, fmt.Errorf("invalid string: %w", err)
		}
		return value, nil
	case '\'':
		if len(raw) < 2 || raw[len(raw)-1] != '\'' {
			return nil, errors.New("unterminated literal string")
		}
		return raw[1 : len(raw)-1], nil
	case '[':
		return parseTOMLArray(raw)
	}
	switch strings.ToLower(raw) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	if integer, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return integer, nil
	}
	if number, err := strconv.ParseFloat(raw, 64); err == nil {
		return number, nil
	}
	return nil, fmt.Errorf("unsupported value %q (quote duration/string values)", raw)
}

func parseTOMLArray(raw string) ([]any, error) {
	if !strings.HasSuffix(raw, "]") {
		return nil, errors.New("unterminated array")
	}
	inner := strings.TrimSpace(raw[1 : len(raw)-1])
	if inner == "" {
		return []any{}, nil
	}
	parts := splitTOMLArray(inner)
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		value, err := parseTOMLValue(part)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}

func splitTOMLArray(raw string) []string {
	var parts []string
	var quote rune
	escaped := false
	start := 0
	for i, char := range raw {
		if escaped {
			escaped = false
			continue
		}
		if quote != 0 {
			if char == '\\' && quote == '"' {
				escaped = true
				continue
			}
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '"', '\'':
			quote = char
		case ',':
			parts = append(parts, strings.TrimSpace(raw[start:i]))
			start = i + 1
		}
	}
	parts = append(parts, strings.TrimSpace(raw[start:]))
	return parts
}
