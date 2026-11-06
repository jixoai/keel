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
)
