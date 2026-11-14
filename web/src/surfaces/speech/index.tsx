// The speech workbench surface — change keel-session-surfaces-v1 (R1),
// reframed as the `stt` / `tts` panel.
//
// Both halves are workbenches, never chat threads: the STT side is a
// transcription ledger (a left-aligned document column of blocks, each with a
// speaker label and a time) plus its recording controls; the TTS side is a
// configuration form. There is no bubble, no assistant reply and no composer
// shell anywhere in this file — the conversation's ComposerDock is the chat
// surface's, not this one's.
//
// What is real here:
//
//   STT — the microphone. A press starts NSH's `Speech` session (zh-CN, interim
//   results on); the partial transcript is a UI-only row that the next final
//   result replaces; stopping calls `session.stop()` and clears the recording
//   indicator in the same frame, so nothing listens in the background.
//   `not-allowed` / `audio-capture` / `unavailable` are reported in place and
//   the composer falls back to text with the transcript intact. A press is the
//   only way speech ever starts.
//
//   TTS — the deployment's speech endpoint. The workbench probes
//   `GET /v1/media/capabilities` on mount; `tts: true` enables the composer and
//   `POST /v1/media/audio/speech` returns the audio bytes this panel plays. A
//   deployment without a TTS provider answers 503 and the panel becomes its
//   configuration state — no placeholder audio and no simulated playback.
//
// What is deliberately absent: the old realtime assistant reply, its tool row
// and its audio chip. An STT workbench turns speech into text; it does not
// answer. Nothing in the panel is fabricated: an empty ledger is empty.
//
// Ledger discipline (design §5.3): only final transcripts become rows. The
// partial row is signal-local, and a session commits at most one row.
//
// NSH components run once and update through fine-grained bindings: every
// signal is read inside JSX or an `<If>`, never copied into a body local. The
// row components take their changing values as getters (the partial row's text,
// the recorder's state), so the binding re-reads the signal without a remount.
import {
  cleanup,
  effect,
  Picker,
  Speech,
  signal,
  Text,
  View,
  type JSX,
  type SpeechErrorCode,
  type SpeechSession,
} from "nsh";
import {
  alpha,
  Badge,
  Button,
  Card,
  CardHeader,
  Field,
  FieldGroup,
  Input,
  ScrollArea,
  Select,
  Tab,
  Tabs,
  Textarea,
  color,
  lineHeight,
  radius,
  space,
  typeScale,
  weight,
  type BadgeVariant,
} from "../../ui/kit";
import { mediaCapabilities, MediaGatewayError, synthesizeSpeech, transcribeAudio } from "../../data/media";
import { store } from "../../data/store";
import { resolveSurface } from "../registry";
import { RecordButton } from "./components/record-button";
import { TranscriptRow, type TranscriptSource } from "./components/transcript-row";
import { Waveform } from "./components/waveform";

export type SpeechSurfaceMode = "stt" | "tts";

// ---- state -----------------------------------------------------------------

/** What the STT workbench is doing (spec: 待命 / 录音中 / 已停止 / 麦克风未授权
 * / 无麦克风 / 识别中断). */
type SttStatus = "idle" | "listening" | "stopped" | "denied" | "missing" | "error";

/** Voice is the microphone, text is typed, and file is a recording the user
 * already has — the third input the server's transcription endpoint accepts, and
 * the one that works without a microphone grant (a reviewer can feed a
 * synthesized clip straight back in). */
type InputMode = "voice" | "text" | "file";

interface TranscriptEntry {
  kind: "transcript";
  id: string;
  text: string;
  at: number;
  source: Exclude<TranscriptSource, "note">;
}

interface NoteEntry {
  kind: "note";
  id: string;
  text: string;
  at: number;
}

type Row = TranscriptEntry | NoteEntry;

/** The workbench's document column: results, controls and the TTS form share
 * one readable width, centred in the pane. */
const WORKBENCH_MAX_WIDTH_PX = 760;

const STT_STATUS_META: Record<SttStatus, { label: string; variant: BadgeVariant; hint: string }> = {
  idle: {
    label: "待命",
    variant: "secondary",
    hint: "按下开始录音；停止即释放麦克风。",
  },
  listening: {
    label: "录音中",
    variant: "destructive",
    hint: "正在录音并转写；点「停止并释放」立即释放麦克风。",
  },
  stopped: {
    label: "已停止",
    variant: "success",
    hint: "麦克风已释放；可以再次按下说话，或切换到文字输入。",
  },
  denied: {
    label: "麦克风未授权",
    variant: "destructive",
    hint: "麦克风权限被拒绝；可以直接用文字继续。",
  },
  missing: {
    label: "无麦克风",
    variant: "destructive",
    hint: "没有可用的麦克风或语音识别；可以直接用文字继续。",
  },
  error: {
    label: "识别中断",
    variant: "warning",
    hint: "语音识别中断；可以再按一次麦克风，或改用文字。",
  },
};

const TTS_PROVIDER_NOTE = "请到 设置 → 模型 配置 TTS Provider；配置后这里才会真正合成音频。";
const TTS_VOICES = [
  { value: "auto", title: "自动（Provider 默认）" },
  { value: "female-warm", title: "女声 · 温暖" },
  { value: "male-calm", title: "男声 · 沉稳" },
];

const activeMode = signal<SpeechSurfaceMode>("stt");
const inputMode = signal<InputMode>("voice");
const status = signal<SttStatus>("idle");
/** Why the status changed (a permission denial, a no-speech stop): it replaces
 * the generic next-step sentence, which would otherwise hide the reason. */
const note = signal("");
const rows = signal<Row[]>([]);
/** The live partial transcript: UI only, never a row, never persisted. */
const partialUser = signal("");
const draft = signal("");
const ttsText = signal("");
const ttsVoice = signal<string | null>("auto");
const ttsNote = signal("");
/** The speech endpoint's answer: `checking` until the capabilities probe
 * returns, then `ready` (a TTS provider is configured) or `unconfigured`. */
const ttsState = signal<"checking" | "ready" | "unconfigured">("checking");
const ttsBusy = signal(false);
/** The file-transcription path: its own busy flag and its own note, so a failure
 * there never writes into the microphone's status. */
const fileBusy = signal(false);
const fileNote = signal("");
/** The clip this panel produced: the provider's audio URL (an object URL owned
 * here), the model that made it and the text it read. */
const ttsResult = signal<{ url: string; model?: string; text: string } | null>(null);
let ttsProbed = false;
const listeningSince = signal(0);
/** The clock the elapsed timer and the live waveform read. */
const tick = signal(Date.now());

let rowSeq = 0;
/** The module signals outlive one mount (a tab switch keeps the transcript), so
 * the live session is owned per mount. */
let live = false;
let sessionToken = 0;
let activeSession: SpeechSession | null = null;
/** The provider mode this surface was last opened for: a remount under the same
 * provider keeps the user's tab, a different provider opens its own workbench. */
let seededMode: SpeechSurfaceMode | undefined;

const nextId = () => `sp-${++rowSeq}`;

const setStatus = (next: SttStatus, detail = "") => {
  status.set(next);
  note.set(detail);
};

const pushRow = (row: Row) => rows.update((list) => [...list, row]);

const commitTranscript = (text: string, source: Exclude<TranscriptSource, "note">) => {
  const clean = text.trim();
  if (!clean) return;
  partialUser.set("");
  pushRow({ kind: "transcript", id: nextId(), text: clean, at: Date.now(), source });
};

const empty = () => rows().length === 0 && !partialUser();
const listening = () => status() === "listening";
const micBlocked = () => status() === "denied" || status() === "missing";

// ---- formatting ------------------------------------------------------------

/** `00:07` — the recorder's own clock, not a duration chip. */
const elapsedClock = (ms: number): string => {
  const total = Math.max(0, Math.floor(ms / 1000));
  return `${String(Math.floor(total / 60)).padStart(2, "0")}:${String(total % 60).padStart(2, "0")}`;
};

const elapsed = () => {
  const since = listeningSince();
  return since ? elapsedClock(tick() - since) : "00:00";
};

// ---- session ---------------------------------------------------------------

const thrownCode = (error: unknown): SpeechErrorCode => {
  const code =
    typeof error === "object" && error !== null && "code" in error
      ? String((error as { code?: unknown }).code ?? "")
      : "";
  if (code.includes("ACCESS_DENIED")) return "not-allowed";
  if (code.includes("FEATURE_UNAVAILABLE")) return "unavailable";
  return "failed";
};

const speechFailure = (code: SpeechErrorCode, error?: unknown) => {
  listeningSince.set(0);
  partialUser.set("");
  const detail = error instanceof Error && error.message ? `（${error.message}）` : "";
  if (code === "not-allowed") {
    inputMode.set("text");
    setStatus("denied", `麦克风权限被拒绝${detail}；已切换到文字输入，可以直接继续。`);
    return;
  }
  if (code === "audio-capture" || code === "unavailable" || code === "language-not-supported") {
    inputMode.set("text");
    setStatus("missing", `没有可用的麦克风或语音识别${detail}；已切换到文字输入，可以直接继续。`);
    return;
  }
  if (code === "no-speech") {
    setStatus("stopped", "没有听到声音；可以再按一次麦克风，或改用文字。");
    return;
  }
  setStatus("error", `语音识别中断${detail}；可以再按一次麦克风，或改用文字。`);
};

const releaseSession = (abort: boolean) => {
  if (abort) sessionToken += 1;
  const session = activeSession;
  activeSession = null;
  listeningSince.set(0);
  if (!session) return;
  try {
    if (abort) {
      session.abort();
      session.dispose();
    } else {
      session.stop();
    }
  } catch {
    /* already ended or already freed */
  }
};

/** The only entry point that starts listening: nothing here runs on mount. */
const startListening = () => {
  if (micBlocked() || listening()) return;
  if (activeSession) {
    if (!activeSession.ended) return;
    try {
      activeSession.dispose();
    } catch {
      /* already freed */
    }
    activeSession = null;
  }
  sessionToken += 1;
  const token = sessionToken;
  let committed = false;
  try {
    const session = Speech.start({ lang: "zh-CN", interim: true });
    activeSession = session;
    listeningSince.set(Date.now());
    tick.set(Date.now());
    setStatus("listening");
    const commit = (text: string) => {
      if (!live || token !== sessionToken || committed) return;
      const clean = text.trim();
      if (!clean) return;
      committed = true;
      commitTranscript(clean, "speech");
    };
    session.on("result", (result) => {
      if (!live || token !== sessionToken) return;
      if (result.isFinal) commit(result.text);
      else partialUser.set(result.text);
    });
    session.on("end", (end) => {
      if (activeSession === session) activeSession = null;
      try {
        session.dispose();
      } catch {
        /* already freed */
      }
      if (!live || token !== sessionToken) return;
      listeningSince.set(0);
      if (end.error) {
        speechFailure(end.error);
        return;
      }
      commit(end.text);
      if (listening()) {
        setStatus(
          "stopped",
          committed ? "" : "没有听到声音；可以再按一次麦克风，或改用文字。",
        );
      }
    });
  } catch (error) {
    speechFailure(thrownCode(error), error);
  }
};

/** Stop is immediate: the indicator and the mic go in the same frame. The
 * recognizer's last final result still lands as a row (it was heard). */
const stopListening = () => {
  if (listening()) setStatus("stopped");
  releaseSession(false);
};

/** Release the device IS the stop: nothing keeps listening in the background. */
const releaseAfterFailure = () => {
  releaseSession(true);
  partialUser.set("");
};

// ---- surface state ---------------------------------------------------------

const settleStaleState = () => {
  partialUser.set("");
  if (listening()) setStatus("stopped");
};

const rowSource = (item: Row): TranscriptSource =>
  item.kind === "note" ? "note" : item.source;

/** Ask the deployment once whether a TTS provider is configured. A probe never
 * throws into the screen: 503 and an unreachable gateway both end in the
 * configuration state with a neutral sentence. */

/** The TTS model this session selected, if any. The speech endpoints route on
 * the model id exactly like the image gateway: naming it is what makes the
 * provider the user picked the provider that answers. A selection that is not a
 * speech/tts model leaves the deployment default in charge. */
const selectedSpeechModel = (mode: "tts" | "stt"): string | undefined => {
  const selection = store.sessionSelection() ?? store.tenantSelection();
  if (!selection?.modelId) return undefined;
  const row = store
    .models()
    .find((model) => model.modelId === selection.modelId && (!selection.providerId || model.providerId === selection.providerId));
  if (!row) return undefined;
  const resolved = resolveSurface(row);
  if (resolved.kind !== "speech") return undefined;
  const family = (row.providerType ?? "").toLowerCase();
  return resolved.descriptor.speechMode === mode || family === mode ? selection.modelId : undefined;
};

const probeTts = async () => {
  if (ttsProbed) return;
  ttsProbed = true;
  try {
    const capabilities = await mediaCapabilities();
    if (capabilities.tts === true) {
      ttsState.set("ready");
      return;
    }
    ttsState.set("unconfigured");
    ttsNote.set(TTS_PROVIDER_NOTE);
  } catch (error) {
    ttsState.set("unconfigured");
    ttsNote.set(
      error instanceof MediaGatewayError && error.kind === "notConfigured"
        ? TTS_PROVIDER_NOTE
        : "无法连接语音服务，请确认服务可用后重试。",
    );
  }
};

const releaseTtsResult = () => {
  const current = ttsResult();
  if (current) URL.revokeObjectURL(current.url);
  ttsResult.set(null);
};

/** Transcribe a recording the user already has. This is the microphone-free path
 * through the same endpoint the mic path will use once the capture bridge lands:
 * pick → POST multipart → the transcript becomes a ledger row. */
const transcribeFile = async () => {
  if (fileBusy()) return;
  fileBusy.set(true);
  fileNote.set("");
  try {
    const picked = await Picker.pickFiles({
      filters: [{ name: "音频", extensions: ["wav", "mp3", "m4a", "webm", "ogg", "flac"] }],
    });
    if (!picked.length) return;
    const file = picked[0]!.file;
    const model = selectedSpeechModel("stt");
    const result = await transcribeAudio({
      audio: file,
      filename: file.name || "recording.wav",
      ...(model ? { model } : {}),
    });
    commitTranscript(result.text, "file");
  } catch (error) {
    if (error instanceof MediaGatewayError && error.kind === "notConfigured") {
      fileNote.set("这台部署没有配置语音识别服务；配置后可以上传录音转写。");
    } else {
      fileNote.set(error instanceof Error && error.message ? error.message : "转写失败，请重试。");
    }
  } finally {
    fileBusy.set(false);
  }
};

const synthesize = async () => {
  const text = ttsText().trim();
  if (!text || ttsBusy() || ttsState() !== "ready") return;
  ttsBusy.set(true);
  ttsNote.set("");
  try {
    const voice = ttsVoice();
    const model = selectedSpeechModel("tts");
    const result = await synthesizeSpeech({
      text,
      ...(voice && voice !== "auto" ? { voice } : {}),
      ...(model ? { model } : {}),
    });
    releaseTtsResult();
    ttsResult.set({ ...result, text });
  } catch (error) {
    if (error instanceof MediaGatewayError && error.kind === "notConfigured") {
      ttsState.set("unconfigured");
      ttsNote.set(TTS_PROVIDER_NOTE);
    } else {
      ttsNote.set(error instanceof Error && error.message ? error.message : "合成失败，请重试。");
    }
  } finally {
    ttsBusy.set(false);
  }
};

/** Play the clip this panel produced. The web build has an `Audio` element; a
 * platform without one says so instead of pretending to play. */
const playTtsResult = () => {
  const current = ttsResult();
  if (!current) return;
  const Ctor = (globalThis as { Audio?: new (src: string) => { play(): Promise<void> } }).Audio;
  if (!Ctor) {
    ttsNote.set("当前平台不支持音频播放。");
    return;
  }
  void new Ctor(current.url).play().catch(() => ttsNote.set("播放失败，请重试。"));
};

// ---- the surface -----------------------------------------------------------

export function SpeechSurface(props: {
  wide: boolean;
  /** Which workbench opens first; the switch stays usable in both. */
  mode?: "stt" | "tts";
}): JSX.Element {
  live = true;
  const requestedMode = props.mode ?? "stt";
  // The surface is not remounted when the session switches between an STT and a
  // TTS model (both are the speech surface), so this must be reactive: a plain
  // body-statement runs once and a Demo STT selection would keep showing the
  // TTS workbench. The "own tab" rule survives — the effect only fires when the
  // requested mode actually changes.
  effect(() => {
    const mode = props.mode ?? "stt";
    if (seededMode === mode) return;
    seededMode = mode;
    activeMode.set(mode);
  });
  settleStaleState();
  void probeTts();
  const beat = setInterval(() => {
    if (live && listening()) tick.set(Date.now());
  }, 250);
  cleanup(() => {
    // Nothing may schedule after this point: a session's `end` can arrive while
    // it is being released.
    live = false;
    clearInterval(beat);
    releaseSession(true);
    partialUser.set("");
    releaseTtsResult();
    if (listening()) setStatus("stopped");
  });

  const switchMode = (next: SpeechSurfaceMode) => {
    if (activeMode() === next) return;
    if (listening()) {
      releaseAfterFailure();
      setStatus("idle");
    }
    activeMode.set(next);
  };

  const chooseInputMode = (next: InputMode) => {
    if (next === "text" && listening()) stopListening();
    inputMode.set(next);
  };

  const record = () => {
    if (listening()) {
      stopListening();
      return;
    }
    if (micBlocked()) {
      // A denied permission is retryable: the user may grant it later.
      setStatus("idle");
      inputMode.set("voice");
    }
    startListening();
  };

  const canSend = () => draft().trim().length > 0;
  const sendText = () => {
    if (!canSend()) return;
    const text = draft();
    draft.set("");
    commitTranscript(text, "text");
  };

  /** Committed transcriptions (a system note is not a transcription). */
  const transcriptCount = () => rows().filter((row) => row.kind === "transcript").length;

  /** The control panel's one operating hint: the reason a state gives wins,
   * then the state's own sentence — never a second copy floating elsewhere. */
  const controlHint = () => {
    if (note()) return note();
    if (listening()) return STT_STATUS_META.listening.hint;
    if (inputMode() === "text") return "文字会作为一条转写记录入账。";
    return STT_STATUS_META.idle.hint;
  };

  return (
    <View
      testID="speech-surface"
      style={{ flex: 1, minHeight: 0, backgroundColor: color.background }}
    >
      {/* Header: what the workbench is, one honest status badge, and the mode
          switch — which names itself 工作模式 so it cannot be mistaken for
          decoration or for the input switch in the panel below. */}
      <View
        style={{
          gap: space.sm,
          paddingHorizontal: space.xl,
          paddingVertical: space.md,
          borderBottomWidth: 1,
          borderBottomColor: color.border,
          flexShrink: 0,
        }}
      >
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
          <Text
            testID="speech-title"
            style={{ fontSize: typeScale.caption, fontWeight: weight.semibold, color: color.foreground }}
          >
            {activeMode() === "stt" ? "语音转写" : "语音合成"}
          </Text>
          <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
            {activeMode() === "stt" ? "麦克风 → 文字" : "文字 → 语音"}
          </Text>
          <View style={{ flex: 1 }} />
          <If when={activeMode() === "stt"}>
            <Badge
              variant={STT_STATUS_META[status()].variant}
              label={STT_STATUS_META[status()].label}
              testID="speech-status"
            />
          </If>
          <If when={activeMode() === "tts"}>
            {/* One state, read from the probe — never a hard-coded label. A fixed
                "未配置" here contradicted the card's own "已配置" badge on a
                deployment whose speech endpoint answered. */}
            <Badge
              variant={ttsState() === "ready" ? "default" : "warning"}
              icon={ttsState() === "ready" ? "check" : "alert"}
              label={ttsState() === "ready" ? "已配置" : ttsState() === "checking" ? "检查中" : "未配置"}
              testID="speech-tts-status"
            />
          </If>
        </View>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
          <Text
            style={{
              fontSize: typeScale.xs,
              fontWeight: weight.medium,
              color: color.mutedForeground,
            }}
          >
            工作模式
          </Text>
          <Tabs accessibilityLabel="工作模式：语音转写或语音合成" testID="speech-workbench">
            <Tab
              testID="speech-mode-stt"
              label="转写"
              icon="file"
              selected={activeMode() === "stt"}
              onPress={() => switchMode("stt")}
            />
            <Tab
              testID="speech-mode-tts"
              label="合成"
              icon="play"
              selected={activeMode() === "tts"}
              onPress={() => switchMode("tts")}
            />
          </Tabs>
        </View>
      </View>

      {/* STT: one document column for the record and one control panel — a
          transcription workbench, not a message thread. */}
      <If when={activeMode() === "stt"}>
        <View style={{ flex: 1, minHeight: 0 }}>
          <View
            style={{
              flex: 1,
              minHeight: 0,
              width: "100%",
              maxWidth: WORKBENCH_MAX_WIDTH_PX,
              marginHorizontal: "auto",
            }}
          >
            <View
              style={{
                flexDirection: "row",
                alignItems: "center",
                gap: space.sm,
                paddingHorizontal: space.xl,
                paddingTop: space.lg,
                paddingBottom: space.sm,
                flexWrap: "wrap",
                flexShrink: 0,
              }}
            >
              <Text
                style={{ fontSize: typeScale.caption, fontWeight: weight.semibold, color: color.foreground }}
              >
                转写记录
              </Text>
              <Text
                testID="speech-count"
                style={{ fontSize: typeScale.xs, color: color.mutedForeground }}
              >
                {`共 ${transcriptCount()} 条`}
              </Text>
              <View style={{ flex: 1 }} />
              <If when={listening()}>
                <Waveform active bars={14} beat={() => tick()} testID="speech-live-waveform" />
                <Text
                  testID="speech-elapsed"
                  style={{
                    fontSize: typeScale.sm,
                    color: color.foreground,
                    fontVariant: ["tabular-nums"],
                  }}
                >
                  {elapsed()}
                </Text>
              </If>
            </View>
            <View style={{ height: 1, backgroundColor: color.border, marginHorizontal: space.xl }} />

            {/* The live state owns the results area until the first row lands:
                what the workbench is doing, not an empty page. */}
            <If when={listening() && empty()}>
              <View
                testID="speech-listening"
                accessibilityLabel="正在录音"
                style={{
                  paddingHorizontal: space.xl,
                  paddingVertical: space["2xl"],
                  gap: space.sm,
                }}
              >
                <Text style={{ fontSize: typeScale.base, fontWeight: weight.medium, color: color.foreground }}>
                  正在听…
                </Text>
                <Text
                  style={{
                    fontSize: typeScale.sm,
                    color: color.mutedForeground,
                    lineHeight: Math.round(typeScale.sm * lineHeight.relaxed),
                  }}
                >
                  开口说话，转写会实时出现在这里；说完点「停止并释放」。
                </Text>
              </View>
            </If>

            <If when={!listening() && empty()}>
              <View
                testID="speech-empty"
                accessibilityLabel="空转写"
                style={{
                  paddingHorizontal: space.xl,
                  paddingVertical: space["2xl"],
                  gap: space.sm,
                }}
              >
                <Text style={{ fontSize: typeScale.base, fontWeight: weight.medium, color: color.foreground }}>
                  还没有转写
                </Text>
                <Text
                  style={{
                    fontSize: typeScale.sm,
                    color: color.mutedForeground,
                    lineHeight: Math.round(typeScale.sm * lineHeight.relaxed),
                  }}
                >
                  最终转写结果会按时间顺序出现在这里。
                </Text>
                <View
                  style={{
                    flexDirection: "row",
                    alignItems: "center",
                    gap: space.md,
                    flexWrap: "wrap",
                    paddingTop: space.xs,
                  }}
                >
                  <Badge variant="outline" icon="file" label="中文 · zh-CN" />
                </View>
              </View>
            </If>

            <If when={!empty()}>
              <ScrollArea
                testID="speech-transcript"
                style={{ flex: 1 }}
                contentStyle={{
                  paddingHorizontal: space.xl,
                  paddingVertical: space.lg,
                  gap: space.lg,
                }}
              >
                {rows().map((item) => (
                  <TranscriptRow
                    testID={rowSource(item) === "note" ? "speech-note-row" : "speech-transcript-row"}
                    text={() => item.text}
                    source={() => rowSource(item)}
                    at={() => item.at}
                  />
                ))}
                {/* The live row: dimmed, and replaced — never duplicated — by the
                    final result that follows it. */}
                <If when={!!partialUser()}>
                  <TranscriptRow
                    testID="speech-partial-row"
                    text={() => partialUser()}
                    source={() => "speech"}
                    partial
                  />
                </If>
              </ScrollArea>
            </If>
          </View>

          {/* The control panel: the workbench's own controls (input switch,
              recorder or the text fallback, one hint), not a chat composer. */}
          <View
            style={{
              borderTopWidth: 1,
              borderTopColor: color.border,
              backgroundColor: color.panel,
              flexShrink: 0,
            }}
          >
            <View
              style={{
                width: "100%",
                maxWidth: WORKBENCH_MAX_WIDTH_PX,
                marginHorizontal: "auto",
                paddingHorizontal: space.xl,
                paddingVertical: space.lg,
                gap: space.md,
              }}
            >
              <View
                style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}
              >
                <Text
                  style={{
                    fontSize: typeScale.xs,
                    fontWeight: weight.medium,
                    color: color.mutedForeground,
                  }}
                >
                  输入方式
                </Text>
                <Tabs accessibilityLabel="输入方式：语音或文字" testID="speech-input-mode">
                  <Tab
                    testID="speech-mode-voice"
                    label="语音"
                    icon="message"
                    selected={inputMode() === "voice"}
                    onPress={() => chooseInputMode("voice")}
                  />
                  <Tab
                    testID="speech-mode-text"
                    label="文字"
                    icon="keyboard"
                    selected={inputMode() === "text"}
                    onPress={() => chooseInputMode("text")}
                  />
                  <Tab
                    testID="speech-mode-file"
                    label="文件"
                    icon="file"
                    selected={inputMode() === "file"}
                    onPress={() => chooseInputMode("file")}
                  />
                </Tabs>
              </View>

              {/* The text fallback is always reachable, and it is where a denied
                  microphone lands: same record, no audio asked. */}
              <If when={inputMode() === "text"}>
                <View
                  style={{
                    flexDirection: props.wide ? "row" : "column",
                    alignItems: props.wide ? "center" : "stretch",
                    gap: space.sm,
                  }}
                >
                  <Input
                    testID="speech-text-input"
                    accessibilityLabel="文字输入（麦克风兜底）"
                    value={draft()}
                    placeholder="输入一句话，回车加入转写…"
                    submitBehavior="submit"
                    onChange={(value) => draft.set(value)}
                    onSubmit={() => sendText()}
                    style={{ flex: 1 }}
                  />
                  <Button
                    testID="speech-text-send"
                    variant="secondary"
                    size={props.wide ? "icon" : "default"}
                    icon="arrow-up"
                    label={props.wide ? undefined : "发送"}
                    accessibilityLabel="发送文字"
                    disabled={!canSend()}
                    onPress={() => sendText()}
                  />
                </View>
              </If>

              {/* A recording the user already has: the same endpoint the mic path
                  posts to, without needing a microphone grant. */}
              <If when={inputMode() === "file"}>
                <View style={{ gap: space.sm }}>
                  <Button
                    testID="speech-file-pick"
                    variant="secondary"
                    icon="paperclip"
                    label={fileBusy() ? "转写中…" : "选择音频文件"}
                    loading={fileBusy()}
                    accessibilityLabel="选择音频文件并转写"
                    onPress={() => void transcribeFile()}
                  />
                  <If when={!!fileNote()}>
                    <Text
                      testID="speech-file-note"
                      style={{
                        fontSize: typeScale.xs,
                        lineHeight: Math.round(typeScale.xs * lineHeight.normal),
                        color: color.mutedForeground,
                      }}
                    >
                      {fileNote()}
                    </Text>
                  </If>
                </View>
              </If>

              {/* The recorder — one control per state, and no path that listens
                  without a press. */}
              <If when={inputMode() === "voice"}>
                <RecordButton
                  listening={() => listening()}
                  blocked={() => micBlocked()}
                  onPress={record}
                />
              </If>

              <View style={{ gap: space.xxs }}>
                <Text
                  testID="speech-hint"
                  style={{
                    fontSize: typeScale.xs,
                    lineHeight: Math.round(typeScale.xs * lineHeight.snug),
                    color: color.mutedForeground,
                  }}
                >
                  {controlHint()}
                </Text>
                <Text
                  testID="speech-disclaimer"
                  style={{ fontSize: typeScale.xs, color: color.mutedForeground }}
                >
                  转写来自系统语音识别；本面板不模拟音频，也不连接对话模型。
                </Text>
              </View>
            </View>
          </View>
        </View>
      </If>

      {/* TTS: a composer over the deployment's speech endpoint. While the
          probe or a 503 says there is no provider, one banner owns the state
          (and the only live action is configuring it); once the endpoint
          answers, the composer synthesizes and the result area plays what came
          back. */}
      <If when={activeMode() === "tts"}>
        <ScrollArea
          testID="speech-tts"
          style={{ flex: 1 }}
          contentStyle={{ padding: space.xl, gap: space.lg }}
        >
          <View
            style={{
              width: "100%",
              maxWidth: WORKBENCH_MAX_WIDTH_PX,
              marginHorizontal: "auto",
              gap: space.lg,
            }}
          >
            <If when={ttsState() !== "ready"}>
            <View
              testID="speech-tts-warning"
              style={{
                flexDirection: "row",
                alignItems: "flex-start",
                gap: space.sm,
                padding: space.md,
                borderRadius: radius.lg,
                borderWidth: 1,
                borderColor: alpha(color.warning, 0.3),
                backgroundColor: color.warningTint,
              }}
            >
              <Badge
                variant="warning"
                icon="alert"
                label={ttsState() === "checking" ? "正在检查语音服务" : "TTS Provider 尚未配置"}
              />
              <Text
                style={{
                  flex: 1,
                  fontSize: typeScale.sm,
                  lineHeight: Math.round(typeScale.sm * lineHeight.relaxed),
                  color: color.warning,
                }}
              >
                {ttsState() === "checking"
                  ? "正在确认这台部署是否配置了 TTS Provider。"
                  : "合成按钮暂不可用；这里不会生成占位音频，也不会模拟播放。"}
              </Text>
            </View>
            </If>

            <Card testID="speech-tts-card">
              <CardHeader
                title="语音合成"
                description="把文字交给 TTS Provider 合成语音。"
                action={
                  <Badge
                    variant={ttsState() === "ready" ? "default" : "warning"}
                    icon={ttsState() === "ready" ? "check" : "alert"}
                    label={ttsState() === "ready" ? "已配置" : ttsState() === "checking" ? "检查中" : "未配置"}
                  />
                }
              />
              <FieldGroup>
                <Field label="要合成的文本" testID="speech-tts-text-field">
                  <Textarea
                    testID="speech-tts-text"
                    accessibilityLabel="要合成的文本"
                    value={ttsText()}
                    placeholder="输入要合成的文字…"
                    onChange={(value) => ttsText.set(value)}
                  />
                </Field>
                <Field
                  label="音色"
                  hint="音色列表由 Provider 声明。"
                  testID="speech-tts-voice-field"
                >
                  <Select
                    testID="speech-tts-voice"
                    accessibilityLabel="音色"
                    items={TTS_VOICES}
                    value={ttsVoice()}
                    onChange={(value) => ttsVoice.set(value ?? "auto")}
                  />
                </Field>
              </FieldGroup>
              {/* The blocked primary keeps the same language as the no-model
                  duplex: a flat, dimmed outline. The one filled primary is the
                  action that actually works — configuring the provider. */}
              <View
                style={{
                  flexDirection: props.wide ? "row" : "column",
                  alignItems: props.wide ? "center" : "stretch",
                  gap: space.sm,
                }}
              >
                <Button
                  testID="speech-tts-synthesize"
                  variant={ttsState() === "ready" ? "default" : "outline"}
                  label={ttsBusy() ? "合成中…" : "合成"}
                  icon="play"
                  loading={ttsBusy()}
                  accessibilityLabel={
                    ttsState() === "ready" ? "合成语音" : "合成（未配置 TTS Provider，不可用）"
                  }
                  disabled={ttsState() !== "ready" || ttsBusy() || ttsText().trim().length === 0}
                  onPress={() => void synthesize()}
                />
                <Button
                  testID="speech-tts-configure"
                  variant={ttsState() === "ready" ? "secondary" : "default"}
                  icon="settings"
                  label="配置 Provider"
                  accessibilityLabel="配置 TTS Provider"
                  onPress={() => ttsNote.set(TTS_PROVIDER_NOTE)}
                />
                <View style={{ flex: 1 }} />
              </View>
              <If when={!!ttsNote()}>
                <Text
                  testID="speech-tts-note"
                  style={{
                    fontSize: typeScale.sm,
                    lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                    color: color.mutedForeground,
                  }}
                >
                  {ttsNote()}
                </Text>
              </If>
            </Card>

            {/* The result area mirrors the STT ledger: the same section header
                and the same left-aligned empty block, so both halves of the
                workbench read as one product. */}
            <View
              testID={ttsResult() ? "speech-tts-result" : "speech-tts-empty"}
              accessibilityLabel="合成结果"
              style={{ gap: space.sm }}
            >
              <View
                style={{
                  flexDirection: "row",
                  alignItems: "center",
                  gap: space.sm,
                  paddingBottom: space.sm,
                }}
              >
                <Text
                  style={{
                    fontSize: typeScale.caption,
                    fontWeight: weight.semibold,
                    color: color.foreground,
                  }}
                >
                  合成结果
                </Text>
                <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
                  {ttsResult() ? (ttsResult()!.model ?? "已生成") : "未生成"}
                </Text>
              </View>
              <View style={{ height: 1, backgroundColor: color.border }} />
              <If when={!ttsResult()}>
                <Text
                  style={{
                    paddingTop: space.lg,
                    fontSize: typeScale.base,
                    fontWeight: weight.medium,
                    color: color.foreground,
                  }}
                >
                  尚未生成音频
                </Text>
                <Text
                  style={{
                    fontSize: typeScale.sm,
                    color: color.mutedForeground,
                    lineHeight: Math.round(typeScale.sm * lineHeight.relaxed),
                  }}
                >
                  配置 TTS Provider 后，合成结果会出现在这里。
                </Text>
              </If>
              <If when={!!ttsResult()}>
                <Text
                  testID="speech-tts-result-text"
                  style={{
                    paddingTop: space.lg,
                    fontSize: typeScale.sm,
                    color: color.mutedForeground,
                    lineHeight: Math.round(typeScale.sm * lineHeight.relaxed),
                  }}
                >
                  {ttsResult()?.text ?? ""}
                </Text>
                <Button
                  testID="speech-tts-play"
                  variant="secondary"
                  icon="play"
                  label="播放"
                  accessibilityLabel="播放合成结果"
                  onPress={playTtsResult}
                />
              </If>
            </View>
          </View>
        </ScrollArea>
      </If>
    </View>
  );
}
