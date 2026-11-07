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
	// EventExternalEvent is Keel's compatibility log of an external session:
	// a message handed to an AgentBackend or an event the backend pushed back.
	// It is written only through the kernel's external commit lane and only
	// into sessions marked state_owner=external; the native transcript decoder
	// ignores it.
	EventExternalEvent = "external.event"
)
