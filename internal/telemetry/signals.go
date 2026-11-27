package telemetry

// Signal contract and redaction allowlist (change keel-plugin-telemetry).
//
// The PI contract (docs/PI-PARITY.md §3) keeps its shape: spans, metrics and
// logs are the only signals. This file freezes WHAT may travel inside them:
//
//   - spans and logs may carry bounded identity (tenant/session/run/turn/tool)
//     plus a closed set of result/status keys;
//   - metric labels may only carry bounded enum values — never tenant/session/
//     run/request/trace/span, because those are unbounded dimensions;
//   - forbidden key families (prompt/message/tool arguments/results/file names/
//     paths/URLs/secrets/authorization) are dropped before any exporter sees a
//     record.
//
// The redactor is shared by every exporter so a future exporter cannot invent a
// weaker policy. It never rewrites a forbidden key into an allowed one: unknown
// input is dropped and counted.

import (
	"strings"
	"unicode/utf8"
)

// Signal is one telemetry signal family.
type Signal string

const (
	// SignalTraces is the span tree.
	SignalTraces Signal = "traces"
	// SignalMetrics is the PI instrument stream.
	SignalMetrics Signal = "metrics"
	// SignalLogs is the structured log channel.
	SignalLogs Signal = "logs"
)

// Signals returns the closed set in declaration order.
func Signals() []Signal {
	return []Signal{SignalTraces, SignalMetrics, SignalLogs}
}

// ParseSignal accepts one signal name.
func ParseSignal(raw string) (Signal, bool) {
	for _, signal := range Signals() {
		if string(signal) == raw {
			return signal, true
		}
	}
	return "", false
}

// SignalList maps the set onto its canonical spelling, preserving declaration
// order and deduplicating.
func SignalList(set []Signal) []Signal {
	seen := make(map[Signal]bool, len(set))
	out := make([]Signal, 0, len(set))
	for _, signal := range Signals() {
		if containsSignal(set, signal) {
			out = append(out, signal)
			seen[signal] = true
		}
	}
	for _, signal := range set {
		if seen[signal] {
			continue
		}
		if parsed, ok := ParseSignal(string(signal)); ok {
			out = append(out, parsed)
			seen[parsed] = true
		}
	}
	return out
}

func containsSignal(set []Signal, signal Signal) bool {
	for _, candidate := range set {
		if candidate == signal {
			return true
		}
	}
	return false
}

// HasSignal reports whether the set contains the signal.
func hasSignal(set []Signal, signal Signal) bool { return containsSignal(set, signal) }

// Attribute limits. A span/log record is bounded metadata, not a payload: a
// runaway attribute or array is truncated at the boundary instead of growing the
// queue.
const (
	// MaxAttributeStringRunes bounds one string value.
	MaxAttributeStringRunes = 128
	// MaxAttributeArrayItems bounds one array value.
	MaxAttributeArrayItems = 16
	// MaxAttributesPerRecord bounds one span/event/log record.
	MaxAttributesPerRecord = 32
	// MaxEventNameRunes bounds a span/log event code.
	MaxEventNameRunes = 96
)

// spanLogKeys is the closed allowlist for span, span-event and log attributes.
var spanLogKeys = map[string]struct{}{
	"tenant":           {},
	"session":          {},
	"run":              {},
	"turn":             {},
	"tool":             {},
	"model":            {},
	"provider":         {},
	"outcome":          {},
	"status":           {},
	"kind":             {},
	"error.kind":       {},
	"http.status_code": {},
	"mcp.server":       {},
	"plugin":           {},
	"command":          {},
	"level":            {},
	"event":            {},
	"logger":           {},
}

// metricLabelKeys is the closed allowlist for metric labels. Identity keys
// (tenant/session/run/request/trace/span) are absent by construction: they are
// unbounded dimensions and belong on the resource or a span/log.
var metricLabelKeys = map[string]struct{}{
	"model":    {},
	"provider": {},
	"outcome":  {},
	"status":   {},
	"tool":     {},
	"kind":     {},
	"result":   {},
}

// forbiddenAttributePrefixes names the key families that MUST never reach an
// exporter, even if a future allowlist entry looks similar. The check is a
// case-insensitive prefix match on the dotted path: "tool.args", "tool_args",
// "file.name" and "path" are all forbidden.
var forbiddenAttributePrefixes = []string{
	"message",
	"prompt",
	"tool.args",
	"tool_args",
	"tool.result",
	"tool_result",
	"tool.input",
	"tool.output",
	"file",
	"path",
	"url",
	"secret",
	"authorization",
	"cookie",
	"attachment",
	"api_key",
	"apikey",
	"bearer",
}

// SpanAttributeKeys returns the closed span/log allowlist (sorted for docs and
// tests).
func SpanAttributeKeys() []string { return sortedKeys(spanLogKeys) }

// MetricLabelKeys returns the closed metric-label allowlist.
func MetricLabelKeys() []string { return sortedKeys(metricLabelKeys) }

// ForbiddenAttributePrefixes returns the forbidden key prefixes.
func ForbiddenAttributePrefixes() []string {
	return append([]string(nil), forbiddenAttributePrefixes...)
}

// ForbiddenAttributeKey reports whether a key belongs to a forbidden family.
// The comparison is case-insensitive and matches a prefix at a dotted segment
// boundary, so "file.name" is forbidden while "profile" is not.
func ForbiddenAttributeKey(key string) bool {
	name := strings.ToLower(strings.TrimSpace(key))
	if name == "" {
		return true
	}
	for _, prefix := range forbiddenAttributePrefixes {
		if name == prefix || strings.HasPrefix(name, prefix+".") || strings.HasPrefix(name, prefix+"_") {
			return true
		}
	}
	return false
}

// AllowedSpanAttribute reports whether a span/log key may be exported.
func AllowedSpanAttribute(key string) bool {
	name := strings.ToLower(strings.TrimSpace(key))
	if _, ok := spanLogKeys[name]; !ok {
		return false
	}
	return !ForbiddenAttributeKey(name)
}

// AllowedMetricLabel reports whether a metric label may be exported.
func AllowedMetricLabel(key string) bool {
	name := strings.ToLower(strings.TrimSpace(key))
	_, ok := metricLabelKeys[name]
	return ok && !ForbiddenAttributeKey(name)
}

// SanitizeSpanAttributes filters span/event/log attributes against the
// allowlist, enforces string/array/count bounds and drops secret-shaped string
// values. It returns the accepted set and the number of dropped/truncated keys
// (a truncated value is not a drop; only a rejected key or value counts).
func SanitizeSpanAttributes(attributes SpanAttributes) (SpanAttributes, int) {
	return sanitizeAttributes(attributes, AllowedSpanAttribute)
}

// SanitizeMetricLabels filters metric labels against the metric allowlist.
func SanitizeMetricLabels(attributes SpanAttributes) (SpanAttributes, int) {
	return sanitizeAttributes(attributes, AllowedMetricLabel)
}

func sanitizeAttributes(attributes SpanAttributes, allowed func(string) bool) (SpanAttributes, int) {
	if len(attributes) == 0 {
		return nil, 0
	}
	names := sortedKeys(attributes)
	out := make(SpanAttributes, len(attributes))
	dropped := 0
	kept := 0
	for _, name := range names {
		if kept >= MaxAttributesPerRecord {
			dropped++
			continue
		}
		if !allowed(name) {
			dropped++
			continue
		}
		value, ok := sanitizeAttributeValue(attributes[name])
		if !ok {
			dropped++
			continue
		}
		out[name] = value
		kept++
	}
	return out, dropped
}

func sanitizeAttributeValue(value AttributeValue) (AttributeValue, bool) {
	switch typed := value.(type) {
	case string:
		clean := sanitizeString(typed)
		if clean == "" {
			return nil, false
		}
		return clean, true
	case bool:
		return typed, true
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case []string:
		items := typed
		if len(items) > MaxAttributeArrayItems {
			items = items[:MaxAttributeArrayItems]
		}
		out := make([]string, 0, len(items))
		for _, item := range items {
			clean := sanitizeString(item)
			if clean == "" {
				continue
			}
			out = append(out, clean)
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	case []float64:
		if len(typed) > MaxAttributeArrayItems {
			typed = typed[:MaxAttributeArrayItems]
		}
		return append([]float64(nil), typed...), true
	case []int:
		if len(typed) > MaxAttributeArrayItems {
			typed = typed[:MaxAttributeArrayItems]
		}
		out := make([]float64, len(typed))
		for index, item := range typed {
			out[index] = float64(item)
		}
		return out, true
	case []int64:
		if len(typed) > MaxAttributeArrayItems {
			typed = typed[:MaxAttributeArrayItems]
		}
		out := make([]float64, len(typed))
		for index, item := range typed {
			out[index] = float64(item)
		}
		return out, true
	case []bool:
		if len(typed) > MaxAttributeArrayItems {
			typed = typed[:MaxAttributeArrayItems]
		}
		return append([]bool(nil), typed...), true
	case []any:
		if len(typed) > MaxAttributeArrayItems {
			typed = typed[:MaxAttributeArrayItems]
		}
		return sanitizeAnySlice(typed)
	default:
		return nil, false
	}
}

func sanitizeAnySlice(values []any) (AttributeValue, bool) {
	stringsOut := make([]string, 0, len(values))
	numbersOut := make([]float64, 0, len(values))
	booleansOut := make([]bool, 0, len(values))
	allStrings, allNumbers, allBooleans := true, true, true
	for _, raw := range values {
		switch typed := raw.(type) {
		case string:
			stringsOut = append(stringsOut, sanitizeString(typed))
			allNumbers, allBooleans = false, false
		case float64:
			numbersOut = append(numbersOut, typed)
			allStrings, allBooleans = false, false
		case float32:
			numbersOut = append(numbersOut, float64(typed))
			allStrings, allBooleans = false, false
		case int:
			numbersOut = append(numbersOut, float64(typed))
			allStrings, allBooleans = false, false
		case int64:
			numbersOut = append(numbersOut, float64(typed))
			allStrings, allBooleans = false, false
		case bool:
			booleansOut = append(booleansOut, typed)
			allStrings, allNumbers = false, false
		default:
			return nil, false
		}
	}
	switch {
	case len(values) == 0:
		return nil, false
	case allStrings:
		return stringsOut, true
	case allNumbers:
		return numbersOut, true
	case allBooleans:
		return booleansOut, true
	default:
		return nil, false
	}
}

// sanitizeString bounds a string value and refuses secret-shaped content.
func sanitizeString(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || looksLikeSecret(value) {
		return ""
	}
	if utf8.RuneCountInString(value) > MaxAttributeStringRunes {
		runes := []rune(value)
		value = string(runes[:MaxAttributeStringRunes])
	}
	return value
}

// looksLikeSecret is a cheap value-level backstop for an allowed key that
// happens to carry a credential ("sk-..." or an Authorization header). The key
// allowlist is the primary defence; this catches operator mistakes.
func looksLikeSecret(value string) bool {
	lower := strings.ToLower(value)
	if strings.Contains(lower, "sk-") && len(lower) >= 12 {
		return true
	}
	if strings.Contains(lower, "bearer ") || strings.Contains(lower, "authorization:") {
		return true
	}
	if strings.Contains(lower, "-----begin") {
		return true
	}
	return false
}

// SanitizeEventName bounds a span/log event code; an empty result drops the
// event.
func SanitizeEventName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if utf8.RuneCountInString(name) > MaxEventNameRunes {
		runes := []rune(name)
		name = string(runes[:MaxEventNameRunes])
	}
	return name
}

func sortedKeys[V any](values map[string]V) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	// A tiny insertion sort avoids importing sort in this hot helper; the sets
	// are small and bounded.
	for index := 1; index < len(names); index++ {
		for cursor := index; cursor > 0 && names[cursor-1] > names[cursor]; cursor-- {
			names[cursor-1], names[cursor] = names[cursor], names[cursor-1]
		}
	}
	return names
}
