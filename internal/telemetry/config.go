package telemetry

// Telemetry plugin configuration (change keel-plugin-telemetry §4).
//
// The tenant document / KEEL_TELEMETRY environment remain the one configuration
// input; this file is the single parser the service factory (status preview) and
// the plugin (activation) share. Parsing is side-effect free: secrets are only
// referenced, never resolved here, and every failure is field-level so a client
// can show which key is wrong.
//
//   adapter: otlp | none | memory     # "" and none mean "plugin enabled, inactive"
//   endpoint: https://collector:4318
//   headers: { Authorization: "secret:OTLP_TOKEN" }  # secret refs only
//   service_name: keel
//   signals: [traces, metrics, logs]  # omitted => traces+metrics
//   sample_ratio: 1.0                 # 0..1
//   log_ratio: 1.0                    # 0..1, errors always pass
//   log_min_level: info               # debug|info|warn|error

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Configuration bounds. They keep one tenant document from turning a status
// line or an HTTP request into an unbounded allocation.
const (
	MaxEndpointBytes        = 2048
	MaxServiceNameBytes     = 128
	MaxConfigHeaders        = 16
	MaxHeaderNameBytes      = 128
	MaxHeaderReferenceBytes = 256
)

// LogLevel is the closed log severity set.
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

// LogLevels returns the closed set, lowest first.
func LogLevels() []LogLevel {
	return []LogLevel{LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError}
}

// ParseLogLevel parses a severity name (case-insensitive). An empty value is
// info.
func ParseLogLevel(raw string) (LogLevel, bool) {
	name := strings.ToLower(strings.TrimSpace(raw))
	if name == "" {
		return LogLevelInfo, true
	}
	for _, level := range LogLevels() {
		if string(level) == name {
			return level, true
		}
	}
	return "", false
}

// LogLevelRank orders severities; errors are always rank 3.
func LogLevelRank(level LogLevel) int {
	switch level {
	case LogLevelError:
		return 3
	case LogLevelWarn:
		return 2
	case LogLevelInfo:
		return 1
	default:
		return 0
	}
}

// Config is the telemetry section of the tenant document.
type Config struct {
	Adapter     string
	Endpoint    string
	Headers     map[string]string
	ServiceName string
	// Signals nil means the default (traces+metrics); an explicit empty slice
	// means "no signals".
	Signals     []string
	SampleRatio *float64
	LogRatio    *float64
	LogMinLevel string
	TenantID    string
}

// EffectiveConfig is a validated config with normalized values.
type EffectiveConfig struct {
	Adapter      string
	Endpoint     string
	EndpointHost string
	Headers      map[string]string
	HeaderNames  []string
	ServiceName  string
	Signals      []Signal
	SampleRatio  float64
	LogRatio     float64
	LogMinLevel  LogLevel
	TenantID     string
}

// Inactive reports whether the plugin enabled but has no exporter (adapter
// missing/none/memory).
func (c EffectiveConfig) Inactive() bool { return len(c.Signals) == 0 }

// ConfigError is one field-level configuration failure.
type ConfigError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e *ConfigError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

// ConfigErrorFields extracts field-level failures from err.
func ConfigErrorFields(err error) []ConfigError {
	if err == nil {
		return nil
	}
	if fields, ok := err.(*ConfigErrors); ok {
		return fields.Errors
	}
	return nil
}

// ConfigErrors aggregates field-level failures.
type ConfigErrors struct {
	Errors []ConfigError
}

func (e *ConfigErrors) Error() string {
	if len(e.Errors) == 1 {
		return e.Errors[0].Error()
	}
	parts := make([]string, 0, len(e.Errors))
	for _, failure := range e.Errors {
		parts = append(parts, failure.Error())
	}
	if len(parts) == 0 {
		return "telemetry configuration is invalid"
	}
	return "telemetry configuration is invalid: " + strings.Join(parts, "; ")
}

// ParseConfig validates and normalizes the telemetry section. Adapter memory or
// none (including empty) is a legal inactive state: no endpoint is required and
// signals are empty. Adapter otlp requires an absolute http(s) endpoint and
// secret-reference headers.
func ParseConfig(config Config) (EffectiveConfig, error) {
	var failures ConfigErrors
	add := func(field, format string, args ...any) {
		failures.Errors = append(failures.Errors, ConfigError{Field: field, Message: fmt.Sprintf(format, args...)})
	}
	effective := EffectiveConfig{
		TenantID:    strings.TrimSpace(config.TenantID),
		ServiceName: strings.TrimSpace(config.ServiceName),
		SampleRatio: 1,
		LogRatio:    1,
		LogMinLevel: LogLevelInfo,
	}
	if effective.ServiceName == "" {
		effective.ServiceName = "keel"
	} else if len(effective.ServiceName) > MaxServiceNameBytes {
		add("serviceName", "must be at most %d bytes", MaxServiceNameBytes)
	}
	adapter := strings.ToLower(strings.TrimSpace(config.Adapter))
	switch adapter {
	case "":
		effective.Adapter = "none"
	case "none", "memory", "otlp":
		effective.Adapter = adapter
	default:
		add("adapter", "%q is not one of otlp|none|memory", config.Adapter)
	}

	if config.SampleRatio != nil {
		effective.SampleRatio = *config.SampleRatio
		if effective.SampleRatio < 0 || effective.SampleRatio > 1 {
			add("sampleRatio", "must be between 0 and 1")
		}
	}
	if config.LogRatio != nil {
		effective.LogRatio = *config.LogRatio
		if effective.LogRatio < 0 || effective.LogRatio > 1 {
			add("logRatio", "must be between 0 and 1")
		}
	}
	if level, ok := ParseLogLevel(config.LogMinLevel); ok {
		effective.LogMinLevel = level
	} else {
		add("logMinLevel", "%q is not one of debug|info|warn|error", config.LogMinLevel)
	}

	if effective.Adapter == "otlp" {
		endpoint := strings.TrimSpace(config.Endpoint)
		if endpoint == "" {
			add("endpoint", "adapter otlp requires an absolute http(s) endpoint")
		} else if len(endpoint) > MaxEndpointBytes {
			add("endpoint", "must be at most %d bytes", MaxEndpointBytes)
		} else if parsed, err := url.Parse(endpoint); err != nil || parsed.Scheme == "" || parsed.Host == "" {
			add("endpoint", "must be an absolute http(s) URL")
		} else if parsed.Scheme != "http" && parsed.Scheme != "https" {
			add("endpoint", "scheme must be http or https")
		} else if parsed.User != nil {
			add("endpoint", "must not carry userinfo")
		} else if parsed.RawQuery != "" || parsed.Fragment != "" {
			add("endpoint", "must not carry a query or fragment")
		} else if err := endpointPolicy(parsed); err != nil {
			add("endpoint", "%v", err)
		} else {
			effective.Endpoint = strings.TrimRight(endpoint, "/")
			effective.EndpointHost = parsed.Host
		}
		effective.Signals = parseSignalConfig(config.Signals, add)
		if containsSignal(effective.Signals, SignalLogs) && effective.LogMinLevel == "" {
			effective.LogMinLevel = LogLevelInfo
		}
	}

	headers, names, headerErr := parseHeaderRefs(config.Headers)
	if headerErr != nil {
		add("headers", "%v", headerErr)
	}
	effective.Headers = headers
	effective.HeaderNames = names

	if len(failures.Errors) > 0 {
		return effective, &failures
	}
	return effective, nil
}

// endpointPolicy refuses plaintext collector endpoints outside loopback or a
// literal private IP. A tenant document is admin-controlled, but a public
// plaintext endpoint would send telemetry (and its headers) in clear text; the
// intent is a local dev collector or an in-VPC collector, never the open
// internet. Redirects are refused separately by the HTTP client.
func endpointPolicy(parsed *url.URL) error {
	if parsed.Scheme != "http" {
		return nil
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("plaintext endpoint must use a loopback/private IP literal or https")
	}
	if !ip.IsLoopback() && !ip.IsPrivate() {
		return fmt.Errorf("plaintext endpoint must target a loopback or private address")
	}
	return nil
}

func parseSignalConfig(raw []string, add func(string, string, ...any)) []Signal {
	if raw == nil {
		return []Signal{SignalTraces, SignalMetrics}
	}
	if len(raw) == 0 {
		return []Signal{}
	}
	seen := make(map[Signal]bool, len(raw))
	out := make([]Signal, 0, len(raw))
	for index, name := range raw {
		parsed, ok := ParseSignal(strings.ToLower(strings.TrimSpace(name)))
		if !ok {
			add(fmt.Sprintf("signals[%d]", index), "%q is not one of traces|metrics|logs", name)
			continue
		}
		if seen[parsed] {
			continue
		}
		seen[parsed] = true
		out = append(out, parsed)
	}
	return SignalList(out)
}

// parseHeaderRefs validates header names and secret references. It returns the
// references (never values) and the sorted names for status/approval output.
func parseHeaderRefs(headers map[string]string) (map[string]string, []string, error) {
	if len(headers) == 0 {
		return nil, nil, nil
	}
	if len(headers) > MaxConfigHeaders {
		return nil, nil, fmt.Errorf("at most %d headers are allowed", MaxConfigHeaders)
	}
	refs := make(map[string]string, len(headers))
	names := make([]string, 0, len(headers))
	for name, value := range headers {
		trimmed := strings.TrimSpace(name)
		if !validHeaderName(trimmed) {
			return nil, nil, fmt.Errorf("header name %q is not an HTTP token", name)
		}
		if len(trimmed) > MaxHeaderNameBytes {
			return nil, nil, fmt.Errorf("header name must be at most %d bytes", MaxHeaderNameBytes)
		}
		reference := strings.TrimSpace(value)
		if len(reference) > MaxHeaderReferenceBytes {
			return nil, nil, fmt.Errorf("header %s reference must be at most %d bytes", trimmed, MaxHeaderReferenceBytes)
		}
		if !strings.HasPrefix(reference, "secret:") {
			return nil, nil, fmt.Errorf("header %s must reference a secret (secret:NAME); plaintext values are refused", trimmed)
		}
		secretName := strings.TrimSpace(strings.TrimPrefix(reference, "secret:"))
		if !validSecretName(secretName) {
			return nil, nil, fmt.Errorf("header %s names an invalid secret reference", trimmed)
		}
		refs[trimmed] = reference
		names = append(names, trimmed)
	}
	sortStrings(names)
	return refs, names, nil
}

// ResolveHeaders resolves the secret references through lookup. The returned
// values never enter status, audit or logs.
func (c EffectiveConfig) ResolveHeaders(lookup func(string) (string, bool)) (map[string]string, error) {
	if len(c.Headers) == 0 {
		return nil, nil
	}
	if lookup == nil {
		return nil, &ConfigErrors{Errors: []ConfigError{{Field: "headers", Message: "no secret store is wired in this deployment"}}}
	}
	resolved := make(map[string]string, len(c.Headers))
	for name, reference := range c.Headers {
		secretName := strings.TrimPrefix(reference, "secret:")
		value, ok := lookup(secretName)
		if !ok || strings.TrimSpace(value) == "" {
			// The secret NAME is not part of the status/audit surface: it can
			// disclose infrastructure naming. The field still names the header.
			return nil, &ConfigErrors{Errors: []ConfigError{{
				Field:   "headers." + name,
				Message: "the referenced secret is not available",
			}}}
		}
		resolved[name] = value
	}
	return resolved, nil
}

// ConfigSummary is the sanitized status/approval projection of a config. It
// never carries header values.
type ConfigSummary struct {
	// Configured is false for a tenant with no telemetry section at all.
	Configured   bool     `json:"configured"`
	Valid        bool     `json:"valid"`
	Reason       string   `json:"reason,omitempty"`
	Adapter      string   `json:"adapter,omitempty"`
	EndpointHost string   `json:"endpointHost,omitempty"`
	Signals      []Signal `json:"signals"`
	SampleRatio  float64  `json:"sampleRatio"`
	LogRatio     float64  `json:"logRatio"`
	HeaderNames  []string `json:"headerNames,omitempty"`
	ServiceName  string   `json:"serviceName,omitempty"`
}

// configDeclared reports whether the tenant actually wrote a telemetry section.
// A zero config must not render configured=true just because the summary
// normalizes defaults.
func configDeclared(config Config) bool {
	return strings.TrimSpace(config.Adapter) != "" ||
		strings.TrimSpace(config.Endpoint) != "" ||
		len(config.Headers) > 0 ||
		strings.TrimSpace(config.ServiceName) != "" ||
		config.Signals != nil ||
		config.SampleRatio != nil ||
		config.LogRatio != nil ||
		strings.TrimSpace(config.LogMinLevel) != ""
}

// SummarizeConfig produces the status projection without resolving secrets.
func SummarizeConfig(config Config) ConfigSummary {
	summary := ConfigSummary{
		Configured:  configDeclared(config),
		Adapter:     strings.ToLower(strings.TrimSpace(config.Adapter)),
		SampleRatio: 1,
		LogRatio:    1,
		Signals:     []Signal{},
	}
	if summary.Adapter == "" {
		summary.Adapter = "none"
	}
	if config.SampleRatio != nil {
		summary.SampleRatio = *config.SampleRatio
	}
	if config.LogRatio != nil {
		summary.LogRatio = *config.LogRatio
	}
	if parsed, err := url.Parse(strings.TrimSpace(config.Endpoint)); err == nil && parsed.Host != "" {
		summary.EndpointHost = parsed.Host
	}
	if names, err := headerNames(config.Headers); err == nil {
		summary.HeaderNames = names
	}
	effective, err := ParseConfig(config)
	if err != nil {
		summary.Valid = false
		summary.Reason = err.Error()
		return summary
	}
	summary.Valid = true
	summary.Adapter = effective.Adapter
	summary.EndpointHost = effective.EndpointHost
	summary.Signals = effective.Signals
	summary.SampleRatio = effective.SampleRatio
	summary.LogRatio = effective.LogRatio
	summary.HeaderNames = effective.HeaderNames
	summary.ServiceName = effective.ServiceName
	return summary
}

func headerNames(headers map[string]string) ([]string, error) {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sortStrings(names)
	return names, nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}

func validSecretName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for index, r := range name {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (index > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for cursor := index; cursor > 0 && values[cursor-1] > values[cursor]; cursor-- {
			values[cursor-1], values[cursor] = values[cursor], values[cursor-1]
		}
	}
}

// FormatRatio renders a ratio for audit/approval text without float noise.
func FormatRatio(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
