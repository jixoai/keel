package store

// Event kinds. The event log is the only truth; these names are the durable
// contract between the HTTP shell, the worker, and any external consumer.
const (
	EventUserMessage      = "user.message"
	EventAssistantMessage = "assistant.message"
	EventToolResult       = "tool.result"
	EventCompaction       = "compaction"
	EventRunStarted       = "run.started"
	EventRunFailed        = "run.failed"
	// EventRunFinished is the attempt's own account of what it did (turns,
	// tokens, tool timings, stop reason). It is committed by the worker right
	// before the attempt settles, is ignored by the transcript decoder, and
	// never enters the model context.
	EventRunFinished = "run.finished"
	// EventExternalEvent is Keel's compatibility log of an external session:
	// a message handed to an AgentBackend or an event the backend pushed back.
	// It is written only through the kernel's external commit lane and only
	// into sessions marked state_owner=external; the native transcript decoder
	// ignores it.
	EventExternalEvent = "external.event"

	// v2 contract kinds. They are bookkeeping facts: the transcript decoder
	// skips them, while SSE replays them like any other Keel-recorded event.
	EventTaskCreated  = "task.created"
	EventTaskUpdated  = "task.updated"
	EventTaskAttached = "task.attached"
	EventTaskDetached = "task.detached"
	EventTaskClosed   = "task.closed"
	EventTaskNote     = "task.note"

	EventFSWrite         = "fs.write"
	EventFSCapture       = "fs.capture"
	EventFSCaptureFailed = "fs.capture_failed"

	EventConfigRotated = "config.rotated"

	EventPlacementJoined = "placement.joined"
	EventPlacementLeft   = "placement.left"

	EventSupervisionEscalated = "supervision.escalated"
	EventGuardrailTripped     = "guardrail.tripped"
	EventResumeAttempt        = "resume.attempt"

	EventNotificationAck      = "notification.ack"
	EventNotificationPushSent = "notification.push_sent"
)
