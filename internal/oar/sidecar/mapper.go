package sidecar

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jixoai/keel/internal/oar"
)

// MapCodexLine maps one line of `codex exec --json` output into OAR record
// drafts. Seq, timestamp, session, runtime and attribution are filled by the
// server; this function only classifies the runtime event.
//
// The mapping is deliberately lossless for unknown event types: an unknown
// Codex event becomes a status record with the raw event as its output, never a
// dropped line. Codex's repeated "Reconnecting..." errors are degraded status
// records, not terminal turn failures: the CLI may still complete the turn.
func MapCodexLine(line []byte, turnID string, maxOutputBytes int) ([]oar.Record, error) {
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return nil, fmt.Errorf("codex line is not JSON: %w", err)
	}
	eventType := jsonString(fields["type"])
	if eventType == "" {
		return nil, fmt.Errorf("codex event has no type")
	}
	switch eventType {
	case "thread.started":
		output := mustRaw(map[string]any{"threadId": jsonString(fields["thread_id"])})
		return []oar.Record{{
			Kind:    oar.KindStatus,
			Name:    "thread.started",
			Status:  oar.StatusStarted,
			TurnID:  turnID,
			Output:  output,
			Message: "codex thread started",
		}}, nil
	case "turn.started":
		return []oar.Record{{
			Kind:   oar.KindStatus,
			Name:   "codex.turn.started",
			Status: oar.StatusStarted,
			TurnID: turnID,
		}}, nil
	case "turn.completed":
		var usage oar.RecordUsage
		if raw := fields["usage"]; len(raw) > 0 {
			var report struct {
				InputTokens          int64  `json:"input_tokens"`
				CachedInputTokens    int64  `json:"cached_input_tokens"`
				OutputTokens         int64  `json:"output_tokens"`
				ReasoningOutputToken int64  `json:"reasoning_output_tokens"`
				TotalTokens          int64  `json:"total_tokens"`
				Model                string `json:"model"`
			}
			if err := json.Unmarshal(raw, &report); err == nil {
				usage = oar.RecordUsage{
					Metering:     "external",
					Model:        report.Model,
					InputTokens:  report.InputTokens,
					OutputTokens: report.OutputTokens,
					TotalTokens:  report.TotalTokens,
				}
				if usage.TotalTokens == 0 && (usage.InputTokens > 0 || usage.OutputTokens > 0) {
					usage.TotalTokens = usage.InputTokens + usage.OutputTokens
				}
				return []oar.Record{{
					Kind:   oar.KindUsage,
					Name:   "turn.usage",
					TurnID: turnID,
					Usage:  &usage,
				}}, nil
			}
		}
		return nil, nil
	case "turn.failed":
		message := jsonString(fields["message"])
		if message == "" {
			message = nestedString(fields["error"], "message")
		}
		if message == "" {
			message = "codex turn failed"
		}
		return []oar.Record{{
			Kind:    oar.KindError,
			Name:    "codex.turn.failed",
			TurnID:  turnID,
			Message: message,
			Error:   &oar.RecordError{Code: "codex_turn_failed", Message: message},
		}}, nil
	case "error":
		message := jsonString(fields["message"])
		if message == "" {
			message = "codex reported an error"
		}
		return []oar.Record{{
			Kind:    oar.KindStatus,
			Name:    "codex.error",
			Status:  oar.StatusDegraded,
			TurnID:  turnID,
			Message: message,
			Output:  boundedRaw(fields["error"], maxOutputBytes),
		}}, nil
	case "item.started", "item.updated", "item.completed":
		item := fields["item"]
		return mapCodexItem(eventType, item, line, turnID, maxOutputBytes)
	default:
		return []oar.Record{{
			Kind:    oar.KindStatus,
			Name:    "codex." + eventType,
			Status:  oar.StatusRunning,
			TurnID:  turnID,
			Message: "unmapped codex event",
			Output:  boundedRaw(line, maxOutputBytes),
		}}, nil
	}
}

func mapCodexItem(eventType string, itemRaw, line json.RawMessage, turnID string, maxOutputBytes int) ([]oar.Record, error) {
	var item map[string]json.RawMessage
	if len(itemRaw) == 0 || json.Unmarshal(itemRaw, &item) != nil {
		return []oar.Record{{
			Kind:    oar.KindStatus,
			Name:    "codex." + eventType,
			Status:  oar.StatusRunning,
			TurnID:  turnID,
			Message: "codex item event without an item",
			Output:  boundedRaw(line, maxOutputBytes),
		}}, nil
	}
	itemType := jsonString(item["type"])
	itemID := jsonString(item["id"])
	if itemID == "" {
		itemID = "codex-" + shortHash(line)
	}
	switch itemType {
	case "agent_message", "reasoning":
		if eventType != "item.completed" {
			return nil, nil
		}
		text := firstString(item["text"], item["message"])
		kind := oar.KindMessage
		name := itemType
		return []oar.Record{{
			Kind:    kind,
			Name:    name,
			TurnID:  turnID,
			Message: text,
			Output:  boundedRaw(itemRaw, maxOutputBytes),
		}}, nil
	case "error":
		message := firstString(item["message"], item["text"])
		if message == "" {
			message = "codex item error"
		}
		// Codex reports configuration warnings and non-fatal item errors this
		// way and still completes the turn, so this is a degraded status, not a
		// terminal error record.
		return []oar.Record{{
			Kind:    oar.KindStatus,
			Name:    "codex.item.error",
			Status:  oar.StatusDegraded,
			TurnID:  turnID,
			Message: message,
			Output:  boundedRaw(itemRaw, maxOutputBytes),
		}}, nil
	}

	switch eventType {
	case "item.started":
		return []oar.Record{{
			Kind:    oar.KindCall,
			Name:    firstNonEmpty(itemType, "codex.item"),
			CallID:  itemID,
			TurnID:  turnID,
			Status:  oar.StatusStarted,
			Input:   boundedRaw(itemRaw, maxOutputBytes),
			Message: "codex item started",
		}}, nil
	case "item.updated":
		return []oar.Record{{
			Kind:    oar.KindStatus,
			Name:    "codex.item.updated",
			TurnID:  turnID,
			Status:  oar.StatusRunning,
			Message: firstNonEmpty(itemType, "codex item") + " updated",
			Output:  boundedRaw(itemRaw, maxOutputBytes),
		}}, nil
	default: // item.completed
		status := oar.StatusCompleted
		if itemFailed(item) {
			status = oar.StatusFailed
		}
		record := oar.Record{
			Kind:   oar.KindResult,
			Name:   firstNonEmpty(itemType, "codex.item"),
			CallID: itemID,
			TurnID: turnID,
			Status: status,
			Output: boundedRaw(itemRaw, maxOutputBytes),
		}
		if status == oar.StatusFailed {
			message := firstString(item["error"], item["message"])
			if message == "" {
				message = "codex item failed"
			}
			record.Error = &oar.RecordError{Code: "codex_item_failed", Message: message}
			record.Message = message
		}
		return []oar.Record{record}, nil
	}
}

func itemFailed(item map[string]json.RawMessage) bool {
	if status := jsonString(item["status"]); status == "failed" || status == "error" {
		return true
	}
	var exitCode int64
	if raw, ok := item["exit_code"]; ok && json.Unmarshal(raw, &exitCode) == nil && exitCode != 0 {
		return true
	}
	return false
}

func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

func firstString(values ...json.RawMessage) string {
	for _, value := range values {
		if text := jsonString(value); text != "" {
			return text
		}
	}
	return ""
}

func nestedString(raw json.RawMessage, field string) string {
	if len(raw) == 0 {
		return ""
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return ""
	}
	return jsonString(object[field])
}

func shortHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:4])
}

// boundedRaw keeps a JSON value within the record budget. An oversized value
// becomes a lossless envelope with the byte count and a rune-safe preview; the
// record never grows past maxOutputBytes (plus envelope overhead).
func boundedRaw(raw json.RawMessage, maxBytes int) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	if maxBytes <= 0 {
		maxBytes = 64 << 10
	}
	if len(raw) <= maxBytes {
		return raw
	}
	preview := string(raw)
	if len(preview) > 2048 {
		preview = preview[:2048]
	}
	for len(preview) > 0 && !utf8.ValidString(preview) {
		preview = preview[:len(preview)-1]
	}
	envelope, _ := json.Marshal(map[string]any{
		"truncated": true,
		"bytes":     len(raw),
		"preview":   preview,
	})
	return envelope
}
