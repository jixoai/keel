// Realtime voice session surface (`keel-session-surfaces-v1` §5).
//
// This is NOT a record-then-transcribe panel. A press on 开始实时对话 opens a
// real duplex session through the web host bridge (`keel.realtime.start`):
// the browser keeps a WebSocket to `/v1/realtime`, streams microphone PCM16
// 16 kHz continuously, and schedules the provider's 24 kHz PCM as it arrives.
// Server VAD ends turns, the user can barge in, and transcripts are events
// that accompany the audio — they are not the input mechanism.
//
// What this file owns: the call's projection. Service events reduce into
// transcript rows (final user/assistant turns, tool requests, audio playback
// state) and one call phase; the host owns audio devices, resampling and
// playback scheduling. The mic, the socket and the service conversation are
// real; when no realtime model is configured (or no session is open) the
// surface says so *before* a press, disables its one primary control and puts
// the configuration action beside it instead of simulating a conversation.
// There is no client-local event source: every row here came from a live
// session, so "没有连接" means no transcript, never a fabricated reply.
//
// Ledger discipline (§5.3): only final transcripts become rows. Partials live
// in `partialUser`/streaming rows and are replaced in place; the reconnect
// path never replays a committed transcript and says the service session is
// new, because it is. This build writes **no ledger entry**: the rows live in
// the page's projection only, so "final" means final here — the surface says
// that in a muted line instead of implying a durable ledger.
//
// NSH components run once: every signal read lives in JSX or an `<If>`, never
// copied into a body-level local.
import {
  cleanup,
  host,
  Scroll,
  signal,
  Text,
  View,
  windowSize,
  type JSX,
} from "nsh";
import { store } from "../../data/store";
import {
  Button,
  color,
  ComposerDock,
  ComposerSurface,
  ConversationColumn,
  Empty,
  EmptyDescription,
  EmptyMedia,
  EmptyTitle,
  Input,
  lineHeight,
  space,
  typeScale,
} from "../../ui/kit";
import { resolveSurface } from "../registry";
import {
  CallControls,
  StatusStrip,
  SystemLine,
  UserTranscriptRow,
  AssistantTranscriptRow,
  type AssistantAudioPhase,
  type AssistantRowStatus,
  type RealtimePhase,
  type ToolCallView,
  type UserSource,
} from "./components";

type Row =
  | { kind: "user"; id: string; text: string; at: number; source: UserSource }
  | {
      kind: "assistant";
      id: string;
      text: string;
      at: number;
      status: AssistantRowStatus;
      tools: ToolCallView[];
      audio: AssistantAudioPhase;
    }
  | { kind: "system"; id: string; text: string; at: number };

/** What a `keel.realtime.*` call answers (the host never throws for expected
 * failures: see web/host.js). */
interface HostReply {
  ok?: boolean;
  code?: string;
  message?: string;
  mic?: string;
}

// ---- state (module level: a tab switch keeps the call and its transcript) --

const phase = signal<RealtimePhase>("idle");
const detail = signal("");
const rows = signal<Row[]>([]);
/** The live partial transcript: UI only, never a row, never persisted. */
const partialUser = signal("");
const partialAt = signal(0);
/** The host reports a live session. */
const active = signal(false);
const starting = signal(false);
const muted = signal(false);
const mic = signal<"unknown" | "granted" | "denied" | "missing" | "unsupported">("unknown");
const inputLevel = signal(0);
const outputLevel = signal(0);
const startedAt = signal(0);
/** The clock the call timer reads. */
const tick = signal(Date.now());
/** The reader asked for the model settings from the blocked panel: the panel
 * acknowledges it in place (the shim that swaps the route has no other way to
 * answer while the surface is still mounted). */
const configAsked = signal(false);

let rowSeq = 0;
let toolSeq = 0;
/** The assistant row the current response is writing into. */
let activeRowId: string | null = null;
/** A response was started and then interrupted: its late deltas/done belong
 * to a cancelled turn and must not open a new completed row. */
let interruptedTurn = false;
/** Whether a provider response is in flight (set on response.created). */
let responseLive = false;
/** Guards an in-flight start from landing after a stop/unmount. */
let callToken = 0;

const nextId = () => `rt-${++rowSeq}`;
const clampLevel = (value: unknown) => Math.max(0, Math.min(1, Number(value) || 0));

const setPhase = (next: RealtimePhase, note = "") => {
  phase.set(next);
  detail.set(note);
};

const pushRow = (row: Row) => rows.update((list) => [...list, row]);
const pushSystem = (text: string) => pushRow({ kind: "system", id: nextId(), text, at: Date.now() });

/** Appends a system line only when the same fact is not already the newest
 * one: a reconnect/close/error can be reported by a state event and a call
 * reply both, and two copies of one reason read as two failures. A reason that
 * one path words as "实时会话错误：X" and the other as "X" is the same fact.
 * The window keeps a genuinely repeated fact later in the call readable. */
let lastSystem = { text: "", at: 0 };
const sameFact = (a: string, b: string) => a === b || a.includes(b) || b.includes(a);
const pushSystemOnce = (text: string) => {
  if (!text) return;
  const now = Date.now();
  if (lastSystem.text && sameFact(lastSystem.text, text) && now - lastSystem.at < 5000) return;
  const list = rows();
  const last = list[list.length - 1];
  if (last && last.kind === "system" && sameFact(last.text, text)) return;
  lastSystem = { text, at: now };
  pushRow({ kind: "system", id: nextId(), text, at: now });
};

const patchAssistant = (
  id: string,
  patch: Partial<Extract<Row, { kind: "assistant" }>> | ((row: Extract<Row, { kind: "assistant" }>) => Partial<Extract<Row, { kind: "assistant" }>>),
) => {
  rows.update((list) =>
    list.map((row) => {
      if (row.kind !== "assistant" || row.id !== id) return row;
      return { ...row, ...(typeof patch === "function" ? patch(row) : patch) };
    }),
  );
};

const commitUser = (text: string, source: UserSource) => {
  const clean = text.trim();
  if (!clean) return;
  partialUser.set("");
  partialAt.set(0);
  pushRow({ kind: "user", id: nextId(), text: clean, at: Date.now(), source });
};

/** The assistant row a response writes into; created once per turn so a
 * partial and its final never become two rows. */
const ensureAssistantRow = (status: AssistantRowStatus): string | null => {
  if (activeRowId) {
    const current = rows().find((row) => row.id === activeRowId);
    if (current && current.kind === "assistant" && (current.status === "thinking" || current.status === "streaming")) {
      return current.id;
    }
  }
  // A cancelled turn keeps its interrupted row: a late delta must not start a
  // second, "complete" reply after the user already stopped it.
  if (interruptedTurn) return null;
  const id = nextId();
  activeRowId = id;
  pushRow({
    kind: "assistant",
    id,
    text: "",
    at: Date.now(),
    status,
    tools: [],
    audio: "idle",
  });
  return id;
};

/** Marks every in-flight assistant turn interrupted: what arrived stays, and
 * nothing is completed on the reader's behalf. */
const markInterrupted = () => {
  rows.update((list) =>
    list.map((row) =>
      row.kind === "assistant" && (row.status === "thinking" || row.status === "streaming")
        ? {
            ...row,
            status: "interrupted" as const,
            audio: row.audio === "playing" ? ("stopped" as const) : row.audio,
            tools: row.tools.map((tool) =>
              tool.status === "requested" ? { ...tool, status: "aborted" as const } : tool,
            ),
          }
        : row,
    ),
  );
  // A manual barge-in already cleared `responseLive`, so the provider's own
  // cancelled done must not clear the guard it left behind: the late
  // transcript/done of that cancelled response belong to no row at all. Only
  // the next `response.created` starts a new turn and clears the guard.
  interruptedTurn = interruptedTurn || responseLive;
  responseLive = false;
  activeRowId = null;
};

const addTool = (item: Record<string, unknown>): string => {
  const id = ensureAssistantRow("streaming");
  const toolId = String(item.call_id ?? item.id ?? `tool-${++toolSeq}`);
  if (!id) return toolId;
  patchAssistant(id, (row) =>
    row.tools.some((tool) => tool.id === toolId)
      ? {}
      : {
          tools: [
            ...row.tools,
            {
              id: toolId,
              name: String(item.name ?? "tool"),
              args: String(item.arguments ?? ""),
              status: "requested" as const,
            },
          ],
        },
  );
  return toolId;
};

const patchTool = (toolId: string, patch: Partial<ToolCallView>) => {
  rows.update((list) =>
    list.map((row) =>
      row.kind === "assistant" && row.tools.some((tool) => tool.id === toolId)
        ? { ...row, tools: row.tools.map((tool) => (tool.id === toolId ? { ...tool, ...patch } : tool)) }
        : row,
    ),
  );
};

/** The tool a delta belongs to: its id when the provider names it, else the
 * last request that has not finished. */
const toolFor = (event: Record<string, unknown>): string | null => {
  const named = String(event.item_id ?? event.call_id ?? "");
  if (named) return named;
  const list = rows();
  for (let index = list.length - 1; index >= 0; index -= 1) {
    const row = list[index]!;
    if (row.kind !== "assistant") continue;
    for (let t = row.tools.length - 1; t >= 0; t -= 1) {
      if (row.tools[t]!.status === "requested") return row.tools[t]!.id;
    }
  }
  return null;
};

// ---- provider events → the projection -------------------------------------

/** Phase changes belong to a live call: an event outside a session draws no
 * state, so the strip never claims a call that does not exist. */
const notePhase = (next: RealtimePhase, note: string) => {
  if (active()) setPhase(next, note);
};

const handleProviderEvent = (event: Record<string, unknown>) => {
  const type = String(event.type ?? "");
  if (!type) return;

  if (type.endsWith("input_audio_transcription.delta")) {
    if (!partialAt()) partialAt.set(Date.now());
    partialUser.update((text) => text + String(event.delta ?? event.text ?? ""));
    notePhase("listening", "正在实时转写你说的内容…");
    return;
  }
  if (type.endsWith("input_audio_transcription.completed")) {
    commitUser(String(event.transcript ?? event.text ?? partialUser()), "speech");
    notePhase("thinking", "已收到你说的话，等待助手回复…");
    return;
  }
  if (type.endsWith("input_audio_transcription.failed")) {
    const reason = String((event.error as { message?: string } | undefined)?.message ?? "语音转写失败");
    partialUser.set("");
    partialAt.set(0);
    pushSystem(`转写失败：${reason}`);
    return;
  }
  if (type === "input_audio_buffer.speech_started") {
    markInterrupted();
    notePhase("interrupted", "检测到你开口，助手已停下");
    return;
  }
  if (type === "input_audio_buffer.speech_stopped") return;
  if (type === "response.created") {
    interruptedTurn = false;
    responseLive = true;
    ensureAssistantRow("thinking");
    notePhase("thinking", "助手正在组织回复…");
    return;
  }
  if (type === "response.audio_transcript.delta" || type === "response.text.delta") {
    const id = ensureAssistantRow("streaming");
    if (!id) return;
    patchAssistant(id, (row) => ({ text: row.text + String(event.delta ?? ""), status: "streaming" as const }));
    if (phase() !== "speaking") notePhase("speaking", "助手回复正在流式到达…");
    return;
  }
  if (type === "response.audio_transcript.done" || type === "response.text.done") {
    const id = ensureAssistantRow("streaming");
    if (!id) return;
    const text = String(event.transcript ?? event.text ?? "");
    patchAssistant(id, (row) => ({ text: text || row.text, status: "final" as const }));
    return;
  }
  if (type === "response.audio.delta") {
    const id = ensureAssistantRow("streaming");
    if (!id) return;
    patchAssistant(id, { audio: "playing" });
    notePhase("speaking", "助手语音正在流式播放…");
    return;
  }
  if (type === "response.audio.done") return;
  if (type === "response.output_item.added") {
    const item = event.item as Record<string, unknown> | undefined;
    if (item && item.type === "function_call") addTool(item);
    return;
  }
  if (type === "response.function_call_arguments.delta") {
    const toolId = toolFor(event);
    if (!toolId) return;
    rows.update((list) =>
      list.map((row) =>
        row.kind === "assistant" && row.tools.some((tool) => tool.id === toolId)
          ? {
              ...row,
              tools: row.tools.map((tool) =>
                tool.id === toolId ? { ...tool, args: tool.args + String(event.delta ?? "") } : tool,
              ),
            }
          : row,
      ),
    );
    return;
  }
  if (type === "response.function_call_arguments.done") {
    const toolId = toolFor(event);
    if (!toolId) return;
    patchTool(toolId, { args: String(event.arguments ?? ""), status: "done" });
    return;
  }
  if (type === "response.output_item.done") {
    const item = event.item as Record<string, unknown> | undefined;
    if (item && item.type === "function_call") {
      const toolId = addTool(item);
      patchTool(toolId, { args: String(item.arguments ?? ""), status: "done" });
    }
    return;
  }
  if (type === "response.done") {
    const response = event.response as Record<string, unknown> | undefined;
    const status = String(response?.status ?? "");
    if (status === "cancelled" || status === "failed") {
      markInterrupted();
      notePhase("interrupted", status === "failed" ? "本轮回复失败，已停止" : "本轮回复已取消");
      return;
    }
    // A completed done for a turn the user already interrupted keeps the row
    // interrupted: the reply is not completed retroactively.
    if (interruptedTurn) {
      interruptedTurn = false;
      responseLive = false;
      activeRowId = null;
      notePhase("ready", "这一轮已被打断，收尾事件不会补全回复。");
      return;
    }
    responseLive = false;
    const id = activeRowId;
    if (id) {
      patchAssistant(id, (row) =>
        row.status === "interrupted"
          ? {}
          : { status: "final" as const, audio: row.audio === "playing" ? ("ended" as const) : row.audio },
      );
    }
    activeRowId = null;
    notePhase("ready", "这一轮结束，可以继续说下一句。");
    return;
  }
  if (type === "error" || type === "session.error") {
    const reason = String(
      (event.error as { message?: string } | undefined)?.message ?? event.message ?? "语音服务返回错误",
    );
    pushSystem(`实时会话错误：${reason}`);
    notePhase("error", reason);
    return;
  }
};

// ---- host events ----------------------------------------------------------

const onProviderEvent = (data: unknown) => {
  const event = (data as { event?: Record<string, unknown> } | null)?.event;
  // A closed/idle panel ignores the tail events a stopped call may still
  // deliver (they would otherwise resurrect a finished session).
  if (!event || !active()) return;
  handleProviderEvent(event);
};

/** The call clock starts with the first state that says a session is up — a
 * host-driven start may never pass through our own start reply, and a strip
 * whose timer stays at 00:00 on a live call is a lie. */
const ensureClock = () => {
  if (startedAt()) return;
  const now = Date.now();
  startedAt.set(now);
  tick.set(now);
};

const onState = (data: unknown) => {
  const state = data as Record<string, unknown> | null;
  const next = String(state?.phase ?? "");
  const note = String(state?.detail ?? "");
  switch (next) {
    case "ready": {
      active.set(true);
      ensureClock();
      if (mic() === "denied") setPhase("permission-denied", note || "麦克风未授权：可以用文字继续这段会话。");
      else setPhase("ready", note || "会话已就绪：开口即可说话。");
      if (state?.reconnected) {
        pushSystemOnce("已重连：这是一段新的实时会话，断线前的转写不会重放（本地记录保留）。");
      }
      return;
    }
    case "listening":
      active.set(true);
      ensureClock();
      setPhase("listening", note);
      return;
    case "thinking":
      ensureClock();
      setPhase("thinking", note);
      return;
    case "speaking":
      ensureClock();
      setPhase("speaking", note);
      return;
    case "interrupted":
      markInterrupted();
      setPhase("interrupted", note || "本轮回复已停止。");
      return;
    case "reconnecting":
      active.set(true);
      ensureClock();
      setPhase("reconnecting", note || "连接中断，正在重连…");
      pushSystemOnce(note);
      return;
    case "closed":
      active.set(false);
      starting.set(false);
      startedAt.set(0);
      muted.set(false);
      mic.set("unknown");
      partialUser.set("");
      partialAt.set(0);
      inputLevel.set(0);
      outputLevel.set(0);
      markInterrupted();
      setPhase("closed", note || "通话已结束");
      pushSystemOnce(note);
      return;
    case "error":
      // A host that never reached a configured voice service reports the same
      // fact as a start reply: land on the configuration state, not on a bare
      // failure the reader cannot act on.
      if (unconfiguredService("", note)) {
        setPhase("no-provider", `实时语音服务未配置：${note || "没有可用的实时语音模型"}`);
      } else {
        setPhase("error", note || "实时会话出错");
      }
      pushSystemOnce(note);
      return;
    default:
      return;
  }
};

const onLevel = (data: unknown) => {
  const level = data as { input?: unknown; output?: unknown } | null;
  inputLevel.set(clampLevel(level?.input));
  outputLevel.set(clampLevel(level?.output));
};

// ---- the call -------------------------------------------------------------

/** A call target: the model the session talks to. */
interface CallTarget {
  sessionId: string;
  model: string;
  providerId?: string;
}

/** The registry row a selection names, when the registry knows it. */
const modelFor = (selection: { modelId?: string; providerId?: string }) =>
  store.models().find(
    (row) => row.modelId === selection.modelId && (!selection.providerId || row.providerId === selection.providerId),
  ) ?? null;

/** The model this session talks to: the session's own selection, else the
 * tenant default the shell resolved the panel from, else the first selectable
 * realtime model in the registry. The same order the conversation shell uses to
 * decide which surface to mount, so the panel and its header never disagree
 * about the model.
 *
 * `null` without an open session: the bridge's WebSocket is keyed by a real
 * session UUID, so a panel with no session has nothing to connect to and must
 * say so instead of inventing an id. */
const resolveTarget = (): CallTarget | null => {
  const sessionId = store.activeId();
  if (!sessionId) return null;
  const selection = store.sessionSelection() ?? store.tenantSelection();
  if (selection?.modelId) {
    const model = modelFor(selection);
    // A registry row decides when it is known; a selection the registry has
    // not loaded yet keeps the descriptor the panel was mounted on. A row that
    // resolves to another surface never opens a realtime call.
    const realtime = model
      ? resolveSurface(model).kind === "realtime"
      : !selection.surfaceKind || selection.surfaceKind === "realtime";
    if (!realtime) return null;
    return {
      sessionId,
      model: selection.modelId,
      ...(selection.providerId ? { providerId: selection.providerId } : {}),
    };
  }
  const model = store.models().find(
    (row) => row.selectable !== false && row.enabled !== false && resolveSurface(row).kind === "realtime",
  );
  if (model) return { sessionId, model: model.modelId, providerId: model.providerId };
  return null;
};

/** Why the call cannot start right now. The primary control is disabled in
 * these states instead of failing after a press: `""` means it can start. */
const startBlocker = (): "" | "no-session" | "no-model" => {
  if (!store.activeId()) return "no-session";
  const target = resolveTarget();
  if (!target) return "no-model";
  return "";
};

const BLOCKED_NOTE: Record<"no-session" | "no-model", string> = {
  "no-session": "还没有打开会话：先打开一个会话，再开始实时对话。",
  "no-model": "没有可用的实时语音模型：先去设置里配置一个，再回来开始通话。",
};

/** Codes the host reports → the phase that owns the next step. */
const phaseForCode = (code: string | undefined): RealtimePhase => {
  switch (code) {
    case "not-allowed":
      return "permission-denied";
    case "unsupported":
      return "unsupported";
    case "bad-request":
      return "no-provider";
    default:
      return "error";
  }
};

const callHost = async (
  name: string,
  args: unknown,
  timeout: number,
): Promise<{ ok: true; value: unknown } | { ok: false; message: string; code: string }> => {
  try {
    return { ok: true, value: await host.call(name, args, { timeout }) };
  } catch (error) {
    const code = String((error as { code?: unknown } | null)?.code ?? "");
    return {
      ok: false,
      message: error instanceof Error ? error.message : String(error),
      code,
    };
  }
};

/** A host that refuses `host.call` outright: no browser bridge on this
 * platform, or the app was not granted `keel.*`. */
const unsupportedHost = (message: string) =>
  /no host|not granted|ERR_ACCESS_DENIED|ERR_FEATURE_UNAVAILABLE_ON_PLATFORM/i.test(message);

/** A start that never reached a voice service because none is configured: an
 * unconfigured provider, an unreachable gateway. These are a configuration
 * fact (with a next step), not a call failure. */
const unconfiguredService = (code: string, message: string) =>
  /503|unconfigured|not configured|no provider|provider .*(missing|unset)|未配置|没有配置|无法连接/i.test(message) ||
  code === "unconfigured";

/** A host that has the realtime bridge but not the preflight call: preflight is
 * newer than `start`, and its absence must not block a call that may work. */
const preflightMissing = (message: string) =>
  /no host function|unknown host function|not implemented|ERR_NSH_UNKNOWN|ERR_FEATURE_UNAVAILABLE/i.test(message);

/** The resolved model's provider family: the fallback `kind` when the
 * preflight does not name one. `host.js` treats "realtime" as the generic
 * family, so an unknown fallback stays safe. */
const providerKindOf = (target: CallTarget): string => {
  const model = modelFor({ modelId: target.model, ...(target.providerId ? { providerId: target.providerId } : {}) });
  return model?.providerType ?? model?.surfaceKind ?? "realtime";
};

/** `keel.realtime.preflight` (a same-origin GET in the host): is a voice
 * service configured for this model, and what does it speak? The answer decides
 * between the configuration state and a real call — before a socket is opened,
 * so a missing provider never surfaces as a WebSocket error. */
interface PreflightReply {
  ok?: boolean;
  configured?: boolean;
  /** The provider's handshake kind (`qwen`, `openai`, …), not the family. */
  kind?: string;
  inputSampleRate?: number;
  outputSampleRate?: number;
  message?: string;
  code?: string;
}

type PreflightResult =
  | { state: "configured"; kind?: string; inputSampleRate?: number; outputSampleRate?: number }
  | { state: "unconfigured"; message: string }
  | { state: "unsupported"; message: string }
  /** The host cannot answer (no preflight call, a flaky GET): let `start` speak. */
  | { state: "unknown" };

const rateOf = (value: unknown): number | undefined => {
  const rate = Number(value);
  return Number.isFinite(rate) && rate >= 8000 && rate <= 192000 ? rate : undefined;
};

const preflightVoiceService = async (target: CallTarget): Promise<PreflightResult> => {
  const reply = await callHost(
    "keel.realtime.preflight",
    {
      sessionId: target.sessionId,
      model: target.model,
      ...(target.providerId ? { providerId: target.providerId } : {}),
    },
    8000,
  );
  if (!reply.ok) {
    if (preflightMissing(reply.message)) return { state: "unknown" };
    if (unsupportedHost(reply.message) || reply.code === "ERR_FEATURE_UNAVAILABLE_ON_PLATFORM") {
      return { state: "unsupported", message: reply.message };
    }
    return { state: "unknown" };
  }
  const value = (reply.value ?? {}) as PreflightReply;
  if (value.ok === false) {
    const message = value.message ?? "实时语音服务未配置";
    if (unconfiguredService(String(value.code ?? ""), message) || value.configured === false) {
      return { state: "unconfigured", message };
    }
    return { state: "unknown" };
  }
  if (value.configured === false) {
    return { state: "unconfigured", message: value.message ?? "没有可用的实时语音模型" };
  }
  return {
    state: "configured",
    ...(value.kind ? { kind: value.kind } : {}),
    ...(rateOf(value.inputSampleRate) ? { inputSampleRate: rateOf(value.inputSampleRate) } : {}),
    ...(rateOf(value.outputSampleRate) ? { outputSampleRate: rateOf(value.outputSampleRate) } : {}),
  };
};

const startCall = async () => {
  if (starting() || active()) return;
  const target = resolveTarget();
  if (!target) {
    setPhase("no-provider", store.activeId() ? BLOCKED_NOTE["no-model"] : BLOCKED_NOTE["no-session"]);
    return;
  }
  callToken += 1;
  const token = callToken;
  starting.set(true);
  // Ask the bridge whether a voice service exists BEFORE opening a socket: a
  // plain 503 at the upgrade can only reach the browser as a bare 1006 close,
  // which would read as a network fault instead of "configure this".
  setPhase("connecting", "正在检查实时语音服务…");
  const preflight = await preflightVoiceService(target);
  if (token !== callToken) return;
  if (preflight.state === "unconfigured") {
    starting.set(false);
    setPhase("no-provider", preflight.message);
    return;
  }
  if (preflight.state === "unsupported") {
    starting.set(false);
    setPhase("unsupported", preflight.message);
    return;
  }
  const rates =
    preflight.state === "configured"
      ? {
          ...(preflight.kind ? { providerKind: preflight.kind } : {}),
          ...(preflight.inputSampleRate ? { inputSampleRate: preflight.inputSampleRate } : {}),
          ...(preflight.outputSampleRate ? { outputSampleRate: preflight.outputSampleRate } : {}),
        }
      : {};
  muted.set(false);
  mic.set("unknown");
  setPhase("connecting", `正在连接 ${target.model} …`);
  const reply = await callHost(
    "keel.realtime.start",
    {
      // A real session UUID: the bridge builds `/v1/realtime?session_id=…` from
      // it, and the server resolves the session by it.
      sessionId: target.sessionId,
      model: target.model,
      providerKind: providerKindOf(target),
      ...rates,
      ...(target.providerId ? { providerId: target.providerId } : {}),
    },
    30000,
  );
  if (token !== callToken) return;
  starting.set(false);
  if (!reply.ok) {
    active.set(false);
    if (unsupportedHost(reply.message) || reply.code === "ERR_FEATURE_UNAVAILABLE_ON_PLATFORM") {
      setPhase(
        "unsupported",
        "当前宿主没有浏览器音频桥（keel.realtime.* 只在 web 构建可用），无法建立实时语音会话。",
      );
      return;
    }
    // A configuration failure lands on the configuration state, whose action
    // (打开模型设置) is the way out — never a bare "error" with no next step.
    // Nothing was live, so the strip's own sentence is the one record; a second
    // copy in the transcript would just repeat it.
    if (unconfiguredService(reply.code, reply.message)) {
      setPhase("no-provider", `实时语音服务未配置：${reply.message}`);
      return;
    }
    setPhase("error", `实时会话没有建立：${reply.message}`);
    return;
  }
  const value = (reply.value ?? {}) as HostReply;
  if (value.ok === false) {
    active.set(false);
    const code = value.message ?? "实时会话没有建立";
    if (unconfiguredService(String(value.code ?? ""), code)) {
      setPhase("no-provider", `实时语音服务未配置：${code}`);
      return;
    }
    setPhase(phaseForCode(value.code), code);
    return;
  }
  active.set(true);
  startedAt.set(Date.now());
  tick.set(Date.now());
  const micCode = value.mic ?? "granted";
  if (micCode === "granted") {
    mic.set("granted");
    if (phase() === "connecting" || phase() === "idle" || phase() === "closed") {
      setPhase("ready", "会话已就绪：麦克风持续收音，开口即可说话。");
    }
  } else {
    // Any non-granted microphone keeps the session as text-in/voice-out; the
    // strip names the exact state and the reason stays in the hint.
    mic.set(
      micCode === "not-allowed"
        ? "denied"
        : micCode === "unsupported"
          ? "unsupported"
          : "missing",
    );
    const note =
      value.message ||
      (micCode === "not-allowed" ? "麦克风未授权" : "没有可用的麦克风");
    setPhase("permission-denied", `${note}：可以用文字继续这段会话，助手语音仍会播放。`);
    pushSystemOnce(`${note}：已降级为文字输入，助手语音仍会播放。`);
  }
};

/** The blocked panel's way out: the same settings route the header's model chip
 * and the command palette open (`store.openSettings` → `/settings`). The panel
 * acknowledges the press in place, because a host without the route shim (the
 * TUI harness) has no settings page to swap in. */
const openModelSettings = () => {
  configAsked.set(true);
  store.openSettings();
  // The settings page's 模型 section reads the effective document; prime it
  // when a server is there, so the section is not a second empty state.
  if (store.mode() === "http" && !store.settings() && !store.settingsLoading()) void store.loadModelsSettings();
};

const stopCall = async (reason: string) => {  callToken += 1;
  starting.set(false);
  if (host.available()) await callHost("keel.realtime.stop", {}, 5000);
  active.set(false);
  startedAt.set(0);
  muted.set(false);
  mic.set("unknown");
  partialUser.set("");
  partialAt.set(0);
  inputLevel.set(0);
  outputLevel.set(0);
  markInterrupted();
  setPhase("closed", reason);
  pushSystemOnce(reason);
};

/** The UI stops first, then tells the provider: a barge-in must never wait for
 * a round trip to stop the audio. */
const interrupt = () => {
  if (!active()) return;
  markInterrupted();
  setPhase(
    "interrupted",
    muted() ? "已手动打断；麦克风仍处于静音。" : "已手动打断：助手音频与生成已停止。",
  );
  void callHost("keel.realtime.send", { payload: { type: "response.cancel" } }, 5000);
};

const toggleMute = async () => {
  if (!active()) return;
  const next = !muted();
  const reply = await callHost("keel.realtime.mute", { muted: next }, 5000);
  if (!reply.ok) {
    pushSystem(`静音失败：${reply.message}`);
    return;
  }
  muted.set(next);
};

const draft = signal("");

const sendText = async () => {
  const text = draft().trim();
  if (!text || !active()) return;
  const sent = await callHost(
    "keel.realtime.send",
    {
      payload: {
        type: "conversation.item.create",
        item: { type: "message", role: "user", content: [{ type: "input_text", text }] },
      },
    },
    8000,
  );
  if (!sent.ok) {
    pushSystem(`文字没有发出：${sent.message}`);
    return;
  }
  const created = sent.value as HostReply;
  if (created?.ok === false) {
    pushSystem(`文字没有发出：${created.message ?? "实时会话未连接"}`);
    return;
  }
  draft.set("");
  commitUser(text, "text");
  const triggered = await callHost("keel.realtime.send", { payload: { type: "response.create" } }, 8000);
  if (!triggered.ok) pushSystem(`未能触发回复：${triggered.message}`);
};

// ---- the surface ----------------------------------------------------------

const clockText = (ms: number): string => {
  const total = Math.max(0, Math.floor(ms / 1000));
  return `${String(Math.floor(total / 60)).padStart(2, "0")}:${String(total % 60).padStart(2, "0")}`;
};

export function RealtimeSurface(props: { wide: boolean }): JSX.Element {
  const stacked = () => !props.wide;
  const unsubs: Array<() => void> = [];
  if (host.available()) {
    const listen = (name: string, handler: (data: unknown) => void) => {
      try {
        unsubs.push(host.on(name, handler));
      } catch {
        /* a host that refuses listeners: the surface keeps its local state */
      }
    };
    listen("keel.realtime.event", onProviderEvent);
    listen("keel.realtime.state", onState);
    listen("keel.realtime.level", onLevel);
  }
  const beat = setInterval(() => {
    if (active()) tick.set(Date.now());
  }, 250);
  cleanup(() => {
    clearInterval(beat);
    for (const off of unsubs) {
      try {
        off();
      } catch {
        /* already released */
      }
    }
    // Leaving the surface ends the call (or cancels a connect still in
    // flight): nothing may listen or stream with the panel gone.
    if (active()) void stopCall("离开面板，通话已结束");
    else if (starting()) void stopCall("离开面板，连接已取消");
  });

  const empty = () => rows().length === 0 && !partialUser();
  const elapsed = () => (startedAt() ? clockText(tick() - startedAt()) : "00:00");
  /** Why the call cannot start ("" when it can): the same fact disables the
   * primary control and picks the honest phase. */
  const blocker = () => startBlocker();
  const blocked = () => blocker() !== "" && !active();
  /** The configuration action is also offered after a start reply says the
   * service is not configured, even when the registry made the model look
   * available. */
  const showConfig = () => blocked() || (!active() && phase() === "no-provider");
  /** The phase the strip and the quiet line show: a blocking fact outranks the
   * idle/closed phase it leaves behind. */
  const shownPhase = (): RealtimePhase => {
    if (active()) return phase();
    if (blocker() === "no-session") return "no-session";
    if (blocker() === "no-model") return "no-provider";
    return phase();
  };
  const micLabel = () => {
    if (!active()) return "麦克风未开启";
    if (muted()) return "麦克风已静音";
    switch (mic()) {
      case "granted":
        return "麦克风已授权";
      case "denied":
        return "麦克风未授权";
      case "missing":
        return "无可用麦克风";
      case "unsupported":
        return "麦克风不可用";
      default:
        return "麦克风状态未知";
    }
  };
  const connection = () => {
    if (phase() === "reconnecting") return "重连中";
    if (active()) return "已连接";
    return "未连接";
  };
  /** The composer's quiet line: the next step only. The reason itself stays in
   * the strip — one fact, one place. */
  const note = () => {
    if (starting()) return "正在建立会话并申请麦克风…";
    if (blocker() === "no-session") return "打开一个会话后，这个按钮即可用。";
    if (blocker()) return "配置好之后，这个按钮会自动可用。";
    if (phase() === "permission-denied") return "文字输入可用；助手语音仍会播放。";
    return "";
  };
  const stripDetail = () => detail() || (blocker() ? BLOCKED_NOTE[blocker() as keyof typeof BLOCKED_NOTE] : "");

  return (
    <View testID="realtime-surface" style={{ flex: 1, minHeight: 0, backgroundColor: color.background }}>
      {StatusStrip({
        phase: shownPhase(),
        detail: stripDetail(),
        active: active(),
        muted: muted(),
        mic: mic(),
        elapsed: elapsed(),
        connection: connection(),
        connectionOk: active() && phase() !== "reconnecting" && phase() !== "error",
      })}

      <View style={{ flex: 1, minHeight: 0 }}>
        <If when={empty()}>
          <View style={{ flex: 1, justifyContent: "center" }}>
            <ConversationColumn rail={false} windowWidthPx={windowSize().width} testID="realtime-empty-column">
              {/* The empty state explains; it never repeats the call control.
                  There is exactly one 开始实时对话, and it lives in the dock. */}
              <Empty accessibilityLabel="空转写" testID="realtime-empty">
                <EmptyMedia icon="message" tone="brand" />
                <View style={{ alignItems: "center", gap: space.sm, paddingHorizontal: space.xl }}>
                  <EmptyTitle label="还没有转写" />
                  <EmptyDescription label="开始实时对话后，你和助手的话会实时出现在这里：麦克风持续收音、助手语音边说边播，不是录音后再转写。" />
                  <If when={!blocker()}>
                    <EmptyDescription label="用下方的「开始实时对话」开始。" />
                  </If>
                </View>
              </Empty>
            </ConversationColumn>
          </View>
        </If>
        <If when={!empty()}>
          <Scroll testID="realtime-transcript" style={{ flex: 1 }}>
            {/* One measure: the transcript's column is the composer's column
                (`ConversationColumn` caps and insets both), so the user bubble,
                the assistant card and the dock share one pair of edges. */}
            <ConversationColumn rail={false} windowWidthPx={windowSize().width} testID="realtime-transcript-column">
              <View style={{ paddingHorizontal: space.xl, paddingVertical: space.lg, gap: space.lg }}>
                {rows().map((row) =>
                  row.kind === "system"
                    ? SystemLine({ text: row.text, at: row.at })
                    : row.kind === "user"
                      ? UserTranscriptRow({
                          text: row.text,
                          at: row.at,
                          source: row.source,
                          maxWidth: "100%",
                        })
                      : AssistantTranscriptRow({
                          text: row.text,
                          at: row.at,
                          status: row.status,
                          tools: row.tools,
                          audio: row.audio,
                          outputLevel: outputLevel(),
                          maxWidth: "100%",
                        }),
                )}
                <If when={!!partialUser()}>
                  {UserTranscriptRow({
                    text: partialUser(),
                    at: partialAt() || tick(),
                    source: "speech",
                    partial: true,
                    maxWidth: "100%",
                    testID: "realtime-partial",
                  })}
                </If>
              </View>
            </ConversationColumn>
          </Scroll>
        </If>
      </View>

      <ComposerDock rail={false} windowWidthPx={windowSize().width} testID="realtime-dock">
        <View style={{ gap: space.sm }}>
          <ComposerSurface
            testID="realtime-composer"
            error={phase() === "error" || phase() === "permission-denied"}
          >
            {CallControls({
              stacked: stacked(),
              phase: shownPhase(),
              active: active(),
              muted: muted(),
              busy: starting(),
              interrupting: false,
              blocked: blocked(),
              showConfig: showConfig(),
              elapsed: elapsed(),
              inputLevel: inputLevel(),
              micLabel: micLabel(),
              note: note(),
              onStart: () => void startCall(),
              onStop: () => void stopCall("通话已结束"),
              onMute: () => void toggleMute(),
              onInterrupt: () => interrupt(),
              onOpenSettings: () => openModelSettings(),
            })}
            <If when={showConfig() && configAsked()}>
              <Text
                testID="realtime-settings-note"
                style={{
                  fontSize: typeScale.xs,
                  lineHeight: Math.round(typeScale.xs * lineHeight.snug),
                  color: color.info,
                }}
              >
                已打开「设置 › 模型」页。
              </Text>
            </If>
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
              <Input
                testID="realtime-text-input"
                accessibilityLabel="文字输入（实时会话兜底）"
                value={draft()}
                placeholder={active() ? "输入一句话，回车作为文字发到实时会话…" : "会话建立后可用文字输入"}
                submitBehavior="submit"
                disabled={!active()}
                onChange={(value) => draft.set(value)}
                onSubmit={() => void sendText()}
                style={{ flex: 1, minWidth: 0 }}
              />
              <Button
                testID="realtime-text-send"
                variant="secondary"
                size="icon"
                icon="arrow-up"
                accessibilityLabel="发送文字"
                disabled={!active() || !draft().trim()}
                onPress={() => void sendText()}
              />
            </View>
            <Text
              testID="realtime-disclaimer"
              style={{
                fontSize: typeScale.xs,
                lineHeight: Math.round(typeScale.xs * lineHeight.snug),
                color: color.mutedForeground,
              }}
            >
              文字会作为用户消息发到同一个实时会话并触发回复，不经过语音识别；本面板不执行工具调用。
            </Text>
            {/* The transcript's durability, said once and quietly: this build
                keeps the rows in the page only. Not an error — just what the
                build does — so it is muted, not a warning banner. */}
            <Text
              testID="realtime-persistence-note"
              style={{
                marginTop: -space.xs,
                fontSize: typeScale.xs,
                lineHeight: Math.round(typeScale.xs * lineHeight.snug),
                color: color.mutedForeground,
              }}
            >
              本构建的实时转写只保留在当前页面；刷新后不会从账本恢复。
            </Text>
          </ComposerSurface>
        </View>
      </ComposerDock>
    </View>
  );
}
