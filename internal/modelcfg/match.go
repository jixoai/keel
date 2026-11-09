package modelcfg

import (
	"encoding/json"
	"math"
	"net/url"
	"strings"
)

// matchModelGlob reports whether name matches pattern. The only wildcard is
// "*", which matches any sequence (including empty); every other character is
// literal. Matching is case-insensitive and covers the whole string.
func matchModelGlob(pattern, name string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	name = strings.ToLower(strings.TrimSpace(name))
	if pattern == "" {
		return false
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == name
	}
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	for _, part := range parts[1 : len(parts)-1] {
		idx := strings.Index(name, part)
		if idx < 0 {
			return false
		}
		name = name[idx+len(part):]
	}
	return strings.HasSuffix(name, parts[len(parts)-1])
}

// hasGlobWildcard reports whether a model pattern contains a wildcard. A
// pattern without one names a single model and therefore counts as an exact
// rule for the baseline-derived issue.
func hasGlobWildcard(pattern string) bool {
	return strings.Contains(pattern, "*")
}

// normalizeBaseURL canonicalizes a base URL for site-rule matching: surrounding
// whitespace and trailing slashes are trimmed and the scheme and host are
// lower-cased. Path, port and query remain significant.
func normalizeBaseURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	trimmed = strings.TrimRight(trimmed, "/")
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return trimmed
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	return parsed.String()
}

// asInt converts a decoded JSON number into an int. Only integral values are
// accepted; fractional and non-numeric values return ok=false.
func asInt(value any) (int, bool) {
	switch v := value.(type) {
	case json.Number:
		i, err := v.Int64()
		if err != nil {
			f, ferr := v.Float64()
			if ferr != nil {
				return 0, false
			}
			return floatToInt(f)
		}
		return int(i), true
	case float64:
		return floatToInt(v)
	case float32:
		return floatToInt(float64(v))
	case int:
		return v, true
	case int64:
		return int(v), true
	default:
		return 0, false
	}
}

// floatToInt reports whether f is an exact integer representable as int.
func floatToInt(f float64) (int, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) || math.Trunc(f) != f {
		return 0, false
	}
	if f > float64(math.MaxInt) || f < float64(math.MinInt) {
		return 0, false
	}
	return int(f), true
}

// intValue extracts an integral number from a request-patch value.
func intValue(value any) (int, bool) {
	return asInt(value)
}
