// @keel/chat-surface — public entry point.
//
// Headless chat projection for keel: committed wire events in, ConversationView
// out. Zero DOM, zero React, zero runtime dependencies; the EventSource is
// injected by the host.

export type {
  ConnectionState,
  ConversationView,
  DecisionView,
  Delivery,
  EventSource,
  InputView,
  QueueItemView,
  RunSummary,
  SurfaceCommands,
  SurfaceEngine,
  ToolCallView,
  TurnOrigin,
  TurnStatus,
  TurnView,
  WireEvent,
  WorkSegment,
} from "./types";

export {
  ConversationProjection,
  DEFAULT_WINDOW_TURNS,
  type ApplyResult,
  type ProjectionOptions,
} from "./projection";

export {
  ChatSurfaceEngine,
  createSurfaceEngine,
  type AttachOptions,
  type EngineOptions,
  type PartialFrame,
  type SubmitOptions,
} from "./engine";

export {
  toAgUi,
  type AgUiEvent,
  type AgUiInterrupt,
  type AgUiOptions,
  type AgUiRunOutcome,
  type CustomAgUiEvent,
  type MessagesSnapshotEvent,
  type ReasoningEndEvent,
  type ReasoningMessageContentEvent,
  type ReasoningMessageEndEvent,
  type ReasoningMessageStartEvent,
  type ReasoningStartEvent,
  type RunErrorEvent,
  type RunFinishedEvent,
  type RunStartedEvent,
  type StateDeltaEvent,
  type StateSnapshotEvent,
  type StepFinishedEvent,
  type StepStartedEvent,
  type TextMessageContentEvent,
  type TextMessageEndEvent,
  type TextMessageStartEvent,
  type ToolCallArgsEvent,
  type ToolCallEndEvent,
  type ToolCallResultEvent,
  type ToolCallStartEvent,
} from "./agui";

export {
  parseAssistantMessage,
  parseCompaction,
  parseDecision,
  parseRunFinished,
  parseToolResult,
  parseUserMessage,
  type ParsedAssistantMessage,
  type ParsedCompaction,
  type ParsedDecision,
  type ParsedRunFinished,
  type ParsedToolCall,
  type ParsedToolResult,
  type ParsedUserMessage,
} from "./events";
