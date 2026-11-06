// Ported from pi/packages/telemetry/src/render.ts @ 55a5c6914570e2ae31ca36297654483bbbf46883.
// Renders stored spans as a chronological forest for logs and test evidence.

package telemetry

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// control matches C0/C1 control characters and DEL: stored ids, names and
// messages may come from another process, so none reaches a terminal raw.
var control = regexp.MustCompile(`[\x00-\x1f\x7f-\x9f]`)

// escaped writes every control character as a \uXXXX escape.
func escaped(text string) string {
	return control.ReplaceAllStringFunc(text, func(char string) string {
		r := []rune(char)[0]
		return fmt.Sprintf(`\u%04x`, r)
	})
}

// microseconds rounds ms to the microsecond, like the adapters record spans.
func microseconds(ms float64) float64 {
	return float64(int64(ms*1000+0.5)) / 1000
}

func formatNumber(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func formatAttributes(attributes SpanAttributes) string {
	if len(attributes) == 0 {
		return ""
	}
	keys := make([]string, 0, len(attributes))
	for key, value := range attributes {
		if value == nil {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		raw, err := json.Marshal(attributes[key])
		if err != nil {
			continue
		}
		b.WriteString(" ")
		b.WriteString(escaped(key))
		b.WriteString("=")
		b.Write(raw)
	}
	return b.String()
}

// RenderTrace renders stored spans as a chronological forest, treating absent
// parents as roots. Durations and event times are milliseconds to the
// microsecond. Control characters are escaped; attribute values are JSON.
func RenderTrace(spans []StoredSpan) string {
	byID := make(map[string]StoredSpan, len(spans))
	for _, span := range spans {
		byID[span.ID] = span
	}
	ordered := make([]StoredSpan, 0, len(byID))
	for _, span := range byID {
		ordered = append(ordered, span)
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].StartedMs < ordered[j].StartedMs })

	var roots []StoredSpan
	children := make(map[string][]StoredSpan)
	for _, span := range ordered {
		if span.ParentID == "" {
			roots = append(roots, span)
			continue
		}
		if _, ok := byID[span.ParentID]; !ok {
			roots = append(roots, span)
			continue
		}
		children[span.ParentID] = append(children[span.ParentID], span)
	}

	type frame struct {
		span  StoredSpan
		depth int
	}
	pending := make([]frame, 0, len(ordered))
	for i := len(roots) - 1; i >= 0; i-- {
		pending = append(pending, frame{span: roots[i]})
	}
	var lines []string
	for len(pending) > 0 {
		last := len(pending) - 1
		current := pending[last]
		pending = pending[:last]
		indent := strings.Repeat("  ", current.depth)
		errorText := ""
		if current.span.Status.Status == SpanStatusError {
			errorText = " error"
			if current.span.Status.Error != nil {
				errorText += " " + escaped(current.span.Status.Error.Name) + ": " +
					escaped(current.span.Status.Error.Message)
			}
		}
		lines = append(lines, fmt.Sprintf("%s%s%s (%sms)%s",
			indent, escaped(current.span.Name), formatAttributes(current.span.Attributes),
			formatNumber(microseconds(current.span.EndedMs-current.span.StartedMs)), errorText))
		for _, event := range current.span.Events {
			lines = append(lines, fmt.Sprintf("%s  · %s%s (+%sms)",
				indent, escaped(event.Name), formatAttributes(event.Attributes),
				formatNumber(microseconds(event.TimeMs))))
		}
		for _, link := range current.span.Links {
			lines = append(lines, fmt.Sprintf("%s  → link trace %s span %s%s",
				indent, escaped(link.TraceID), escaped(link.SpanID), formatAttributes(link.Attributes)))
		}
		descendants := children[current.span.ID]
		for i := len(descendants) - 1; i >= 0; i-- {
			pending = append(pending, frame{span: descendants[i], depth: current.depth + 1})
		}
	}
	return strings.Join(lines, "\n")
}
