// Package r1diff implements the R1 differential acceptance harness: capture a
// runtime's observable behaviour into a normalized dump, then compare two dumps
// under a whitelist of accepted differences.
//
// The whitelist is deliberately narrow — time, random identifiers and log
// ordering are the only things a dump may not pin down. Everything else (which
// events, in which order, with which payload semantics) is compared verbatim:
// "byte-for-byte" is never claimed, but "semantically identical after time/id
// normalization" is, and any difference outside the whitelist fails the run.
package r1diff

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// Normalizer canonicalizes one dump. It keeps a per-dump registry so entities
// that appear more than once (a toolCall id in the assistant event and again in
// the tool result) map to the same placeholder, preserving the pairing the
// differential is meant to check.
type Normalizer struct {
	ids map[string]string
	n   int
}

// NewNormalizer returns an empty normalizer.
func NewNormalizer() *Normalizer { return &Normalizer{ids: map[string]string{}} }

// idPattern matches runtime-generated identifiers: tool call ids ("fake-…",
// "call_…"), message ids ("msg_…"), generation ids and run ids. Placeholders
// are assigned in first-appearance order, so equal sequences compare equal.
var idPattern = regexp.MustCompile(`^(?:fake|call|tc|toolcall|msg|gen|run|req|sub|blob|file|entry|task)[-_][A-Za-z0-9._-]+$`)

// Value canonicalizes any decoded JSON value.
func (n *Normalizer) Value(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = n.Value(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = n.Value(val)
		}
		return out
	case string:
		return n.String(t)
	case float64:
		// Unix milliseconds (and microseconds) are time, not semantics.
		if t >= 1e12 {
			return "$tsnum"
		}
		return t
	default:
		return v
	}
}

// String canonicalizes one string leaf.
func (n *Normalizer) String(s string) string {
	if _, err := uuid.Parse(s); err == nil {
		return "$uuid"
	}
	if looksLikeTime(s) {
		return "$ts"
	}
	if idPattern.MatchString(s) {
		return n.placeholder(s)
	}
	return s
}

// timeLayouts are the shapes a timestamp actually reaches a dump in: Go's
// RFC3339 (with or without nanoseconds) and PostgreSQL's "date time" text.
// Anything else stays verbatim — a user message that merely starts with a date
// ("2026-11-07 please remember") is content, not a clock reading, and must not
// be normalized away.
var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
}

func looksLikeTime(s string) bool {
	if len(s) < 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for _, layout := range timeLayouts {
		if _, err := time.Parse(layout, s); err == nil {
			return true
		}
	}
	return false
}

func (n *Normalizer) placeholder(id string) string {
	if p, ok := n.ids[id]; ok {
		return p
	}
	n.n++
	p := fmt.Sprintf("$id%d", n.n)
	n.ids[id] = p
	return p
}

// JSON canonicalizes a decoded value and returns its canonical JSON encoding.
func (n *Normalizer) JSON(v any) string {
	raw, err := json.Marshal(n.Value(v))
	if err != nil {
		return fmt.Sprintf("$unencodable:%v", err)
	}
	return string(raw)
}

// Flatten walks a dump and returns path -> canonical JSON for every leaf.
// Paths use "/" separators indexed by map key or array position, e.g.
// "/events/3/payload/content/0/type". The shape is part of the comparison: a
// missing leaf on one side shows up as a diff.
func Flatten(v any, prefix string, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			Flatten(val, prefix+"/"+k, out)
		}
	case []any:
		if len(t) == 0 {
			// An empty array is a leaf so "no frames" differs from "absent".
			out[prefix] = "[]"
			return
		}
		for i, val := range t {
			Flatten(val, fmt.Sprintf("%s/%d", prefix, i), out)
		}
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			raw = []byte(fmt.Sprintf("%v", t))
		}
		out[prefix] = string(raw)
	}
}
