// Ported from pi/packages/telemetry/test/render.test.ts @ 55a5c691.
// Known difference: Go maps lose insertion order, so attributes render sorted
// by key; pi renders them in object insertion order.

package telemetry

import "testing"

func renderSpan(id string, override func(*StoredSpan)) StoredSpan {
	span := StoredSpan{
		ID:        id,
		TraceID:   "trace",
		Name:      id,
		StartedMs: 0,
		EndedMs:   10,
		Status:    OK(),
	}
	if override != nil {
		override(&span)
	}
	return span
}

func TestRenderTrace(t *testing.T) {
	t.Run("renders an unordered trace with attributes, durations, errors, events, and links", func(t *testing.T) {
		root := renderSpan("root", func(s *StoredSpan) {
			s.StartedMs, s.EndedMs = 1000, 1020
			s.Attributes = SpanAttributes{
				"request": "a b",
				"attempt": float64(2),
				"cached":  false,
				"tags":    []string{"a", "b"},
				"sizes":   []float64{1, 2},
				"flags":   []bool{true, false},
			}
			s.Events = []StoredEvent{
				{Name: "checkpoint", TimeMs: 2, Attributes: SpanAttributes{"phase": "tools"}},
				{Name: "tick", TimeMs: 3.5},
			}
			s.Links = []StoredLink{
				{TraceID: "remote-trace", SpanID: "remote", Attributes: SpanAttributes{"reason": "steer"}},
				{TraceID: "other-trace", SpanID: "other"},
			}
		})
		first := renderSpan("first", func(s *StoredSpan) {
			s.ParentID = "root"
			s.StartedMs, s.EndedMs = 1005, 1009.5
			s.Status = ErrorStatus(&SpanError{Name: "Error", Message: "failed call"})
		})
		nested := renderSpan("nested", func(s *StoredSpan) {
			s.ParentID = "first"
			s.StartedMs, s.EndedMs = 1006, 1007
		})
		second := renderSpan("second", func(s *StoredSpan) {
			s.ParentID = "root"
			s.StartedMs, s.EndedMs = 1010, 1012
			s.Status = ErrorStatus(nil)
		})
		want := "root attempt=2 cached=false flags=[true,false] request=\"a b\" sizes=[1,2] tags=[\"a\",\"b\"] (20ms)\n" +
			"  · checkpoint phase=\"tools\" (+2ms)\n" +
			"  · tick (+3.5ms)\n" +
			"  → link trace remote-trace span remote reason=\"steer\"\n" +
			"  → link trace other-trace span other\n" +
			"  first (4.5ms) error Error: failed call\n" +
			"    nested (1ms)\n" +
			"  second (2ms) error"
		if got := RenderTrace([]StoredSpan{second, nested, first, root}); got != want {
			t.Fatalf("RenderTrace() =\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("treats missing parents as roots and orders roots and children by start time", func(t *testing.T) {
		later := renderSpan("later", func(s *StoredSpan) {
			s.ParentID = "external"
			s.StartedMs, s.EndedMs = 20, 30
		})
		child := renderSpan("child", func(s *StoredSpan) {
			s.ParentID = "later"
			s.StartedMs, s.EndedMs = 25, 30
		})
		earlier := renderSpan("earlier", func(s *StoredSpan) {
			s.StartedMs, s.EndedMs = 10, 15
			s.Links = []StoredLink{{TraceID: "trace", SpanID: "later"}}
		})
		want := "earlier (5ms)\n  → link trace trace span later\nlater (10ms)\n  child (5ms)"
		if got := RenderTrace([]StoredSpan{later, child, earlier}); got != want {
			t.Fatalf("RenderTrace() =\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("escapes control characters", func(t *testing.T) {
		span := renderSpan("a\x00b", func(s *StoredSpan) {
			s.Name = "na\nme"
		})
		want := "na\\u000ame (10ms)"
		if got := RenderTrace([]StoredSpan{span}); got != want {
			t.Fatalf("RenderTrace() = %q, want %q", got, want)
		}
	})
}
