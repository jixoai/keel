// Package logsafe wraps a slog.Handler and redacts credentials and message
// bodies before records reach the sink.
//
// Two layers:
//   - attribute keys: authorization, API keys, tokens, secrets, passwords,
//     bodies, and message text are replaced wholesale;
//   - attribute values: strings, errors, fmt.Stringers, and nested map, slice,
//     and struct values are inspected (slog.KindAny included, which is how
//     `slog.Any("err", err)` arrives), and credential-shaped substrings such as
//     "Bearer <token>" or JSON `"api_key":"..."` are masked in place.
//
// Redaction is deliberately conservative: an operator who needs a value for
// debugging must attach it under a non-sensitive key, which makes the decision
// explicit in code review.

package logsafe

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"strings"
)

const (
	// Redacted replaces a sensitive value.
	Redacted = "[REDACTED]"
	// Unredactable marks a value the handler cannot inspect safely (for
	// example a map with non-string keys, a channel, or a function).
	Unredactable = "[UNREDACTABLE]"
	// truncated marks a value beyond the recursion limit (cycle guard).
	truncated = "[TRUNCATED]"
	// maxRedactDepth bounds recursive inspection of maps, slices, and structs.
	maxRedactDepth = 8
)

// Handler redacts attributes before delegating to next.
type Handler struct {
	next slog.Handler
}

// New wraps next.
func New(next slog.Handler) *Handler {
	if next == nil {
		next = slog.Default().Handler()
	}
	return &Handler{next: next}
}

// Enabled implements slog.Handler.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle implements slog.Handler.
func (h *Handler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, redactText(rec.Message), rec.PC)
	rec.Attrs(func(attr slog.Attr) bool {
		if kept, ok := filterAttr(attr); ok {
			out.AddAttrs(kept)
		}
		return true
	})
	return h.next.Handle(ctx, out)
}

// WithAttrs implements slog.Handler.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	filtered := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		if kept, ok := filterAttr(attr); ok {
			filtered = append(filtered, kept)
		}
	}
	return &Handler{next: h.next.WithAttrs(filtered)}
}

// WithGroup implements slog.Handler.
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{next: h.next.WithGroup(name)}
}

// filterAttr redacts one attribute, recursing into groups.
func filterAttr(attr slog.Attr) (slog.Attr, bool) {
	return filterAttrDepth(attr, 0)
}

func filterAttrDepth(attr slog.Attr, depth int) (slog.Attr, bool) {
	if attr.Equal(slog.Attr{}) {
		return slog.Attr{}, false
	}
	if sensitiveKey(attr.Key) {
		return slog.String(attr.Key, Redacted), true
	}
	attr.Value = filterValue(attr.Value, depth)
	return attr, true
}

// filterValue handles every slog kind that can carry text. KindAny is the
// important one in production: `slog.Any("err", err)` and provider errors are
// KindAny, not KindString.
func filterValue(value slog.Value, depth int) slog.Value {
	switch value.Kind() {
	case slog.KindString:
		return slog.StringValue(redactText(value.String()))
	case slog.KindGroup:
		group := value.Group()
		filtered := make([]slog.Attr, 0, len(group))
		for _, child := range group {
			if kept, ok := filterAttrDepth(child, depth+1); ok {
				filtered = append(filtered, kept)
			}
		}
		return slog.GroupValue(filtered...)
	case slog.KindAny:
		return slog.AnyValue(redactAny(value.Any(), depth))
	case slog.KindLogValuer:
		return filterValue(value.Resolve(), depth)
	default:
		return value
	}
}

// redactAny converts an arbitrary attribute value into a redacted, JSON-safe
// value. Errors and Stringers become their text form; maps, slices, and
// structs are inspected recursively so nested credential keys are caught too.
func redactAny(value any, depth int) any {
	if value == nil {
		return nil
	}
	if depth > maxRedactDepth {
		return truncated
	}
	if text, ok := value.(string); ok {
		return redactText(text)
	}
	if err, ok := value.(error); ok {
		return redactText(safeError(err))
	}
	if stringer, ok := value.(fmt.Stringer); ok {
		return redactText(safeString(stringer))
	}
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = redactField(key, item, depth)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(typed))
		for key, item := range typed {
			if sensitiveKey(key) {
				out[key] = Redacted
				continue
			}
			out[key] = redactText(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = redactAny(item, depth+1)
		}
		return out
	}
	return redactReflect(reflect.ValueOf(value), depth)
}

func redactField(key string, value any, depth int) any {
	if sensitiveKey(key) {
		return Redacted
	}
	return redactAny(value, depth+1)
}

// redactReflect handles typed maps/slices/structs that did not match the
// map[string]any fast path. Unsupported kinds are marked explicitly instead of
// being rendered verbatim.
func redactReflect(value reflect.Value, depth int) any {
	if !value.IsValid() {
		return nil
	}
	if depth > maxRedactDepth {
		return truncated
	}
	switch value.Kind() {
	case reflect.Interface, reflect.Pointer:
		if value.IsNil() {
			return nil
		}
		elem := value.Elem()
		if !elem.CanInterface() {
			return Unredactable
		}
		return redactAny(elem.Interface(), depth+1)
	case reflect.Map:
		if value.IsNil() {
			return nil
		}
		if value.Type().Key().Kind() != reflect.String {
			return Unredactable
		}
		out := make(map[string]any, value.Len())
		iter := value.MapRange()
		for iter.Next() {
			key := iter.Key().String()
			item := iter.Value()
			if !item.CanInterface() {
				out[key] = Unredactable
				continue
			}
			out[key] = redactField(key, item.Interface(), depth)
		}
		return out
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			return nil
		}
		// Binary blobs (images, RawMessage) are not useful as JSON and can hide
		// arbitrary payloads; report only their size.
		if value.Kind() == reflect.Slice && value.Type().Elem().Kind() == reflect.Uint8 {
			return fmt.Sprintf("[%d bytes]", value.Len())
		}
		out := make([]any, 0, value.Len())
		for i := 0; i < value.Len(); i++ {
			item := value.Index(i)
			if !item.CanInterface() {
				out = append(out, Unredactable)
				continue
			}
			out = append(out, redactAny(item.Interface(), depth+1))
		}
		return out
	case reflect.Struct:
		out := make(map[string]any, value.NumField())
		typ := value.Type()
		for i := 0; i < value.NumField(); i++ {
			field := value.Field(i)
			if !field.CanInterface() {
				continue
			}
			name, keep := structFieldName(typ.Field(i))
			if !keep {
				continue
			}
			out[name] = redactField(name, field.Interface(), depth)
		}
		return out
	case reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Complex64, reflect.Complex128:
		return Unredactable
	default:
		if value.CanInterface() {
			return value.Interface()
		}
		return Unredactable
	}
}

// structFieldName prefers the JSON field name so `APIKey string `json:"api_key"“
// is caught by the same key rules as a map. A json:"-" field is omitted.
func structFieldName(field reflect.StructField) (string, bool) {
	if tag, ok := field.Tag.Lookup("json"); ok {
		name := strings.Split(tag, ",")[0]
		if name == "-" {
			return "", false
		}
		if name != "" {
			return name, true
		}
	}
	return field.Name, true
}

func safeError(err error) (out string) {
	defer func() {
		if recover() != nil {
			out = Unredactable
		}
	}()
	return err.Error()
}

func safeString(stringer fmt.Stringer) (out string) {
	defer func() {
		if recover() != nil {
			out = Unredactable
		}
	}()
	return stringer.String()
}

// sensitiveKeys are exact (normalized) matches.
var sensitiveKeys = map[string]bool{
	"authorization": true,
	"x_api_key":     true,
	"api_key":       true,
	"apikey":        true,
	"token":         true,
	"access_token":  true,
	"refresh_token": true,
	"id_token":      true,
	"password":      true,
	"passwd":        true,
	"secret":        true,
	"client_secret": true,
	"private_key":   true,
	"body":          true,
	"text":          true,
	"message_body":  true,
	"request_body":  true,
	"response_body": true,
	"messages":      true,
	"payload":       true,
	"content":       true,
	"prompt":        true,
	"completion":    true,
}

func sensitiveKey(key string) bool {
	normalized := strings.ToLower(key)
	normalized = strings.NewReplacer("-", "_", " ", "_", ".", "_").Replace(normalized)
	if sensitiveKeys[normalized] {
		return true
	}
	for _, fragment := range []string{"token", "secret", "password", "api_key", "apikey"} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}

var (
	bearerPattern = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]{8,}`)
	// key=value, key: value, or JSON "key":"value" for credential-ish names.
	assignmentPattern = regexp.MustCompile(`(?i)\b(authorization|api[_-]?key|access[_-]?token|refresh[_-]?token|token|password|secret)\b"?\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;]+)`)
	// Query-string API keys, e.g. Gemini's ?key=AIza...
	queryKeyPattern = regexp.MustCompile(`(?i)([?&]key=)[^&\s"'<>]+`)
	// Well-known provider token shapes. These never match max_tokens/token budget
	// because they require a provider prefix, not the word "token".
	tokenShapePatterns = []*regexp.Regexp{
		regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`),
		regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{20,}`),
		regexp.MustCompile(`\bghp_[A-Za-z0-9]{20,}`),
		regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`),
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	}
)

// redactText masks credential-shaped substrings in free text (for example an
// error string from an upstream provider).
// Redact masks credential-shaped substrings in free text (Bearer tokens, JSON
// `"api_key":"…"`, `token=` query values). Callers that return text to a
// client — an error body, a settings message — use it so a future error that
// happens to embed a credential cannot leak through that path.
func Redact(s string) string { return redactText(s) }

func redactText(s string) string {
	if s == "" {
		return s
	}
	s = bearerPattern.ReplaceAllString(s, "$1 "+Redacted)
	s = assignmentPattern.ReplaceAllString(s, "$1="+Redacted)
	s = queryKeyPattern.ReplaceAllString(s, "$1"+Redacted)
	for _, pattern := range tokenShapePatterns {
		s = pattern.ReplaceAllString(s, Redacted)
	}
	return s
}
