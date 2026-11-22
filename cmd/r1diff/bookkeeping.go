package main

import "strconv"

// bookkeepingReasons classifies every event kind that is an attempt/contract
// fact rather than conversation. A kind listed here is separated from the
// conversation, so adding one never shifts the seq of every later conversational
// event. The reason is mandatory for the same purpose as an allowlist reason:
// "it is bookkeeping" is a contract claim, and a future kind that changes what
// the model sees must not be silently hidden by this classifier.
//
// The v2 kinds (task/fs/config/placement/supervision/guardrail/resume/notify)
// are registered in the kernel vocabulary today. Most still have no emitter in
// the differential scenario; classifying them here is what keeps the gate
// index-stable when W9/W12 land their emitters (evidence/w05-contract.md).
var bookkeepingReasons = map[string]string{
	"run.started":  "attempt lifecycle: the run started; conversation is the user/assistant/tool sequence",
	"run.failed":   "attempt lifecycle: the attempt's failure summary, not a model-visible message",
	"run.finished": "attempt lifecycle: the attempt's own account (turns/tokens/stop reason/config snapshot)",

	// task-log (keel-harness-v2 task-log spec): every task fact is a projection
	// of the ledger, never a transcript message.
	"task.created":  "v2 task-log: card creation fact; the transcript decoder skips it",
	"task.updated":  "v2 task-log: card update fact; not a model-visible message",
	"task.attached": "v2 task-log: resource attachment fact; projected, not spoken",
	"task.detached": "v2 task-log: resource detachment fact; projected, not spoken",
	"task.closed":   "v2 task-log: card close fact; projected, not spoken",
	"task.note":     "v2 task-log: durable task note; not part of the conversation transcript",

	// file/layerfs facts.
	"fs.write":          "v2 file fact: one explicit write (path/hash) committed with the tool result",
	"fs.capture":        "v2 file fact: one batch capture (count/tree hash); never per-file events",
	"fs.capture_failed": "v2 file fact: capture failure marker; the tool result is unaffected",

	// configuration, placement, supervision, guardrails, resume.
	"config.rotated":         "v2 config fact: immutable config version anchored to an event seq",
	"placement.joined":       "v2 placement fact: a session joined a group",
	"placement.left":         "v2 placement fact: a session left a group",
	"supervision.escalated":  "v2 supervision fact: a guardrail escalated to the next owner",
	"guardrail.tripped":      "v2 supervision fact: a guardrail fired; the transcript is unchanged",
	"resume.attempt":         "v2 recovery fact: an automatic resume attempt",
	"notification.ack":       "v2 notify fact: a notification was consumed",
	"notification.push_sent": "v2 notify fact: the push fallback fired (including degraded)",
}

// bookkeepingKinds is the membership view of bookkeepingReasons (kept as a
// set for the hot path).
var bookkeepingKinds = func() map[string]bool {
	out := make(map[string]bool, len(bookkeepingReasons))
	for kind := range bookkeepingReasons {
		out[kind] = true
	}
	return out
}()

// splitBookkeeping separates the conversation from the attempt facts and
// renumbers the conversation's seqs to their position in the array.
//
// The absolute seq is a counter that moves whenever a bookkeeping fact is
// added; the conversation's *order and content* is what the parity gate
// promises to keep. Pinning the absolute counter would turn every additive
// bookkeeping fact into dozens of index-shift rules, which is exactly the
// noise an allowlist must not become. The bookkeeping facts themselves are
// still compared, as a separate section with only their contract fields
// (kind/status/stopReason/turns + the v2 run snapshot fields).
func splitBookkeeping(events []map[string]any) (conversation, bookkeeping []map[string]any) {
	for _, ev := range events {
		kind, _ := ev["kind"].(string)
		if bookkeepingKinds[kind] {
			fact := map[string]any{"kind": kind}
			if payload, ok := ev["payload"].(map[string]any); ok {
				for _, field := range bookkeepingPayloadFields {
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

// bookkeepingPayloadFields are the contract fields compared for a bookkeeping
// fact. Volatile measurements (durationMs, tokens) belong to the attempt's own
// observability, not to this gate. The run snapshot fields are v2 additions:
// configHashBefore/After, anchorEventSeq and toolManifestHash answer "which
// config and which tool manifest did this attempt see".
var bookkeepingPayloadFields = []string{
	"status", "stopReason", "turns",
	"configHashBefore", "configHashAfter", "anchorEventSeq", "toolManifestHash",
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
