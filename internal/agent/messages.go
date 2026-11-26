// Package agent turns the durable event log into model turns: it rebuilds the
// conversation from committed events, runs the stateless loop, and appends the
// resulting messages back as events.
package agent

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/store"
)

// MessagesFromEvents rebuilds the model context from the event log. Only
// committed events are consulted; there is no process-local state.
//
// A payload that cannot be decoded is SKIPPED with a diagnostic (when diagnose
// is supplied) instead of failing the whole run: the kernel validates payload
// shapes at commit time, so a malformed payload here can only be legacy or
// foreign data, and refusing to run the session over one unreadable event would
// freeze the conversation. The skip is recorded, never silent.
func MessagesFromEvents(events []store.Event, diagnose func(seq int64, kind string, err error)) agentcore.MessageList {
	msgs, _ := DecodeMessages(events, diagnose)
	return msgs
}

// DecodeMessages is MessagesFromEvents with the committed sequence each message
// came from. The runner needs the mapping to present input a previous attempt
// never read last (see presentUnreadInputLast).
func DecodeMessages(events []store.Event, diagnose func(seq int64, kind string, err error)) (agentcore.MessageList, []int64) {
	skip := func(ev store.Event, err error) {
		if diagnose != nil {
			diagnose(ev.Seq, ev.Kind, err)
		}
	}
	msgs := make(agentcore.MessageList, 0, len(events))
	seqs := make([]int64, 0, len(events))
	appendMsg := func(seq int64, msg agentcore.Message) {
		msgs = append(msgs, msg)
		seqs = append(seqs, seq)
	}
	for _, ev := range events {
		switch ev.Kind {
		case store.EventUserMessage:
			var p struct {
				Text        string                `json:"text"`
				Attachments []attachmentReference `json:"attachments"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				skip(ev, fmt.Errorf("decode user event %d: %w", ev.Seq, err))
				continue
			}
			content := agentcore.ContentList{agentcore.NewTextContent(p.Text)}
			content = append(content, attachmentBlocks(p.Attachments)...)
			appendMsg(ev.Seq, agentcore.UserMessage{
				RoleField: agentcore.RoleUser,
				Content:   content,
			})
		case store.EventAssistantMessage:
			var m agentcore.AssistantMessage
			if err := json.Unmarshal(ev.Payload, &m); err != nil {
				skip(ev, fmt.Errorf("decode assistant event %d: %w", ev.Seq, err))
				continue
			}
			if m.RoleField == "" {
				m.RoleField = agentcore.RoleAssistant
			}
			// A terminal failure is a record of a failed attempt, not a turn the
			// model produced: it must not be replayed into the next request (an
			// empty/error assistant message would confuse the wire and hide the
			// prompt the retry is supposed to answer). Its event stays in the log
			// for the UI and the audit.
			if m.StopReason == agentcore.StopReasonError || m.StopReason == agentcore.StopReasonAborted {
				continue
			}
			appendMsg(ev.Seq, m)
		case store.EventToolResult:
			var m agentcore.ToolResultMessage
			if err := json.Unmarshal(ev.Payload, &m); err != nil {
				skip(ev, fmt.Errorf("decode tool result event %d: %w", ev.Seq, err))
				continue
			}
			if m.RoleField == "" {
				m.RoleField = agentcore.RoleToolResult
			}
			appendMsg(ev.Seq, m)
		case store.EventCompaction:
			var p struct {
				Summary      string `json:"summary"`
				TokensBefore int    `json:"tokensBefore"`
				Timestamp    int64  `json:"timestamp"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				skip(ev, fmt.Errorf("decode compaction event %d: %w", ev.Seq, err))
				continue
			}
			appendMsg(ev.Seq, agentcore.CompactionMessage{
				RoleField:    agentcore.RoleCompaction,
				Summary:      p.Summary,
				TokensBefore: p.TokensBefore,
				Timestamp:    p.Timestamp,
			})
		default:
			// run.started / run.failed and any future bookkeeping events do not
			// participate in the model context.
		}
	}
	return msgs, seqs
}

// ContentText concatenates the text blocks of a content list.
func ContentText(content agentcore.ContentList) string {
	out := ""
	for _, c := range content {
		if t, ok := c.(agentcore.TextContent); ok {
			out += t.Text
		}
	}
	return out
}

// attachmentReference is one entry of a user.message payload's attachments
// array. The client sends what the upload endpoint returned — a fileId plus
// display metadata — and nothing else: the durable form of an attachment is a
// blob reference, never bytes. The shape is deliberately tolerant (unknown
// fields ignored) because the payload is client data and a newer client may add
// fields the runtime does not need.
type attachmentReference struct {
	FileID string `json:"fileId"`
	Name   string `json:"name"`
	Mime   string `json:"mime"`
	Size   int64  `json:"size"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// attachmentBlocks maps the attachments a turn carries onto durable references:
// every attachment becomes one reference block (imageRef for images, mediaRef
// for everything else), in the order the client sent them. An attachment without
// a blob id cannot be resolved, so it becomes an explicit text notice naming it —
// never nothing: a client that attached a file and got no reaction from the model
// is exactly the silent drop this change removes.
func attachmentBlocks(refs []attachmentReference) agentcore.ContentList {
	out := make(agentcore.ContentList, 0, len(refs))
	for _, ref := range refs {
		name := strings.TrimSpace(ref.Name)
		blobID := strings.TrimSpace(ref.FileID)
		if blobID == "" {
			out = append(out, agentcore.NewTextContent(attachmentMissingBlobNotice(name, ref.Mime)))
			continue
		}
		if isImageAttachment(ref.Mime, name) {
			out = append(out, agentcore.ImageRefContent{
				Type:     agentcore.ContentTypeImageRef,
				BlobID:   blobID,
				MimeType: strings.TrimSpace(ref.Mime),
				Size:     ref.Size,
				Width:    ref.Width,
				Height:   ref.Height,
				Name:     name,
			})
			continue
		}
		kind := agentcore.InputKindForMime(ref.Mime, name)
		out = append(out, agentcore.MediaRefContent{
			Type:     agentcore.ContentTypeMediaRef,
			Kind:     kind,
			BlobID:   blobID,
			Name:     name,
			MimeType: strings.TrimSpace(ref.Mime),
			Size:     ref.Size,
			Width:    ref.Width,
			Height:   ref.Height,
		})
	}
	return out
}

// isImageAttachment reports whether an attachment is image input. A non-empty
// MIME is authoritative: a text/plain object named `shot.png` stays a document
// (a display name cannot widen the modality). Only a reference with no MIME at
// all falls back to the extension.
func isImageAttachment(mime, name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(mime))
	if i := strings.IndexByte(normalized, ';'); i >= 0 {
		normalized = strings.TrimSpace(normalized[:i])
	}
	if normalized != "" {
		return strings.HasPrefix(normalized, "image/")
	}
	switch strings.ToLower(path.Ext(strings.TrimSpace(name))) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".heic", ".heif", ".bmp", ".tif", ".tiff":
		return true
	}
	return false
}

// attachmentMissingBlobNotice is the explicit record of an attachment that cannot
// be resolved at all: the client sent a name and MIME but no blob id, so there
// are no bytes for any provider. The model is told, and the operator can tell an
// unusable client upload from a model that ignored a working one.
func attachmentMissingBlobNotice(name, mime string) string {
	label := strings.TrimSpace(name)
	if label == "" {
		label = "unnamed attachment"
	}
	if mime = strings.TrimSpace(mime); mime != "" {
		label += " (" + mime + ")"
	}
	return "[attachment " + label + " omitted: the client sent no fileId, so no stored bytes are referenced]"
}
