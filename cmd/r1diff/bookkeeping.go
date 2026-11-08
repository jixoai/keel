package main

import "strconv"

// bookkeepingKinds are the runtime's attempt-level facts (the run summary and
// the reserved lifecycle kinds). They are additive: a change may introduce a
// new one without touching the conversation, and the parity gate must register
// that explicitly instead of drowning in shifted array indices.
var bookkeepingKinds = map[string]bool{
	"run.started":  true,
	"run.failed":   true,
	"run.finished": true,
}

// splitBookkeeping separates the conversation from the attempt facts and
// renumbers the conversation's seqs to their position in the array.
//
// The absolute seq is a counter that moves whenever a bookkeeping fact is
// added; the conversation's *order and content* is what the parity gate
// promises to keep. Pinning the absolute counter would turn every additive
// bookkeeping fact into dozens of index-shift rules, which is exactly the
// noise an allowlist must not become. The bookkeeping facts themselves are
// still compared, as a separate section with only their contract fields
// (kind/status/stopReason/turns); volatile measurements (durationMs, tokens)
// belong to the attempt's own observability, not to this gate.
func splitBookkeeping(events []map[string]any) (conversation, bookkeeping []map[string]any) {
	for _, ev := range events {
		kind, _ := ev["kind"].(string)
		if bookkeepingKinds[kind] {
			fact := map[string]any{"kind": kind}
			if payload, ok := ev["payload"].(map[string]any); ok {
				for _, field := range []string{"status", "stopReason", "turns"} {
					if v, ok := payload[field]; ok {
						fact[field] = v
					}
				}
			}
			bookkeeping = append(bookkeeping, fact)
			continue
		}
		// Copy: the caller still needs the raw seq (the SSE cursor).
		copied := make(map[string]any, len(ev))
		for k, v := range ev {
			copied[k] = v
		}
		copied["seq"] = float64(len(conversation) + 1)
		conversation = append(conversation, copied)
	}
	return conversation, bookkeeping
}

// normalizeFrames drops bookkeeping frames from an SSE capture and renumbers
// the remaining ids to their position (the same normalization the log gets).
func normalizeFrames(frames []sseFrame) []sseFrame {
	out := make([]sseFrame, 0, len(frames))
	for _, frame := range frames {
		if bookkeepingKinds[frame.Event] {
			continue
		}
		out = append(out, frame)
	}
	for i := range out {
		out[i].ID = strconv.Itoa(i + 1)
	}
	return out
}
