// The session-surface registry (change keel-session-surfaces-v1 S0).
//
// A model's provider kind decides what a session looks like: an LLM session is
// a text composer over conversation history, a realtime session is a voice
// composer over transcripts, an image session is a prompt/reference composer
// over a gallery. This module is the one place that maps a resolved model to
// that surface. It is pure (no NSH, no store), so the mapping is unit-testable
// without opening the app and every screen reads the same answer.
//
// Resolution order:
//   1. model.surfaceKind / provider.surfaceKind — a provider plugin's own
//      descriptor always wins;
//   2. model.providerType / provider.providerType — the provider family;
//   3. the model's declared output format (image/video/audio) — the fallback
//      for servers that predate providerType;
//   4. llm — the legacy text provider.
// A providerType that is present but unrecognized resolves to `generic`, never
// silently to llm: an unknown provider must say "no dedicated panel" instead of
// pretending to be a chat model.
import type { ModelSelection, ModelView, ProviderView } from "../data/contract";

export type SurfaceKind = "llm" | "realtime" | "image" | "video" | "speech" | "generic";
export type ComposerKind = "text" | "voice" | "prompt-media" | "audio-file";
export type TimelineKind = "messages" | "transcript" | "gallery" | "transcript-audio" | "audio-history";

export type SpeechMode = "tts" | "stt";

export interface SurfaceCapabilities {
  /** The composer takes a microphone stream. */
  audioInput?: boolean;
  /** The timeline can play assistant audio. */
  audioOutput?: boolean;
  /** A user turn can cancel the assistant's current output. */
  interruption?: boolean;
  /** The composer accepts reference images. */
  referenceImages?: boolean;
  /** A generated asset can seed the next turn. */
  multiTurnEdit?: boolean;
  toolCalls?: boolean;
  streamingTranscript?: boolean;
  /** The timeline is a gallery of generated assets. */
  gallery?: boolean;
}

export interface SurfaceDescriptor {
  kind: SurfaceKind;
  label: string;
  /** The composer form the shell mounts. */
  composer: ComposerKind;
  /** The timeline form the shell mounts. */
  timeline: TimelineKind;
  /** TTS/STT share the speech surface; this separates their workbenches. */
  speechMode?: SpeechMode;
  capabilities: SurfaceCapabilities;
  /** Why the surface is generic, shown verbatim. */
  reason?: string;
}

export const SURFACE_DESCRIPTORS: Record<SurfaceKind, SurfaceDescriptor> = {
  llm: {
    kind: "llm",
    label: "对话",
    composer: "text",
    timeline: "messages",
    capabilities: { audioInput: false, toolCalls: true },
  },
  realtime: {
    kind: "realtime",
    label: "实时语音",
    composer: "voice",
    timeline: "transcript-audio",
    capabilities: {
      audioInput: true,
      audioOutput: true,
      interruption: true,
      streamingTranscript: true,
      toolCalls: true,
    },
  },
  image: {
    kind: "image",
    label: "图像生成",
    composer: "prompt-media",
    timeline: "gallery",
    capabilities: { referenceImages: true, multiTurnEdit: true, gallery: true },
  },
  video: {
    kind: "video",
    label: "视频生成",
    composer: "prompt-media",
    timeline: "gallery",
    capabilities: { referenceImages: true, multiTurnEdit: true, gallery: true },
    reason: "视频生成面板尚未提供：当前没有已接入的视频 Provider，所以这里不显示生成输入框，也不会生成任何占位结果。",
  },
  speech: {
    kind: "speech",
    label: "语音工作台",
    composer: "audio-file",
    timeline: "audio-history",
    capabilities: { audioOutput: true, audioInput: true },
  },
  generic: {
    kind: "generic",
    label: "通用会话",
    composer: "text",
    timeline: "messages",
    capabilities: {},
    reason: "该 Provider 类型尚无专用面板，使用通用文本会话。",
  },
};

const ALIASES: Record<string, SurfaceKind> = {
  llm: "llm",
  chat: "llm",
  text: "llm",
  "text-generation": "llm",
  realtime: "realtime",
  "realtime-audio": "realtime",
  voice: "realtime",
  "voice-agent": "realtime",
  image: "image",
  "image-generation": "image",
  "text-to-image": "image",
  t2i: "image",
  video: "video",
  "video-generation": "video",
  "text-to-video": "video",
  t2v: "video",
  tts: "speech",
  "text-to-speech": "speech",
  stt: "speech",
  "speech-to-text": "speech",
  speech: "speech",
  audio: "speech",
  generic: "generic",
  other: "generic",
};

/** The lowercased token a descriptor or provider document carries. */
const token = (value: string | undefined): string => (value ?? "").trim().toLowerCase();

/** A model's explicit kind, if any. */
const explicitKind = (model?: ModelView | null, provider?: ProviderView | null): string | undefined => {
  const value = model?.surfaceKind ?? provider?.surfaceKind ?? model?.providerType ?? provider?.providerType;
  return value && token(value) ? token(value) : undefined;
};

/** The kind from a model's declared output/input formats, the pre-providerType
 * fallback. Return `undefined` when the facts do not name a medium at all. */
const inferFromFormats = (model?: ModelView | null): SurfaceKind | undefined => {
  const output = model?.properties?.outputFormat;
  const input = model?.properties?.inputFormat;
  if (output?.video) return "video";
  if (output?.image) return "image";
  if (output?.audio) return input?.audio ? "realtime" : "speech";
  if (input?.audio && !output?.text) return "speech";
  return undefined;
};

export interface ResolvedSurface {
  kind: SurfaceKind;
  descriptor: SurfaceDescriptor;
  /** True when the provider type was present but had no mapping. */
  unknownProviderType?: string;
}

/** Resolve a model to its surface. `provider` is optional; the model carries
 * the same fields when the registry already flattened them. */
export function resolveSurface(model?: ModelView | null, provider?: ProviderView | null): ResolvedSurface {
  const explicit = explicitKind(model, provider);
  if (explicit) {
    const known = ALIASES[explicit];
    if (known) {
      const base = SURFACE_DESCRIPTORS[known];
      // A provider may declare surfaceKind "speech" while its family is
      // stt/tts; the family still decides which workbench opens.
      const family = token(model?.providerType ?? provider?.providerType);
      const speechToken = family === "stt" || family === "tts" ? family : explicit;
      const speechMode: SpeechMode | undefined =
        known === "speech"
          ? speechToken === "stt" || speechToken === "speech-to-text"
            ? "stt"
            : speechToken === "tts" || speechToken === "text-to-speech"
              ? "tts"
              : undefined
          : undefined;
      return {
        kind: known,
        descriptor: speechMode ? { ...base, speechMode, label: speechMode === "tts" ? "文本转语音" : "语音转文本" } : base,
      };
    }
    return {
      kind: "generic",
      descriptor: {
        ...SURFACE_DESCRIPTORS.generic,
        reason: `Provider 类型「${explicit}」尚无专用面板，使用通用文本会话。`,
      },
      unknownProviderType: explicit,
    };
  }
  const inferred = inferFromFormats(model);
  if (inferred) return { kind: inferred, descriptor: SURFACE_DESCRIPTORS[inferred] };
  return { kind: "llm", descriptor: SURFACE_DESCRIPTORS.llm };
}

/** Resolve from a selection alone. The selection's `surfaceKind` (if the
 * picker recorded one) wins; otherwise the registry's matching model does. */
export function resolveSelectionSurface(
  selection: ModelSelection | null | undefined,
  models: ModelView[],
  providers: ProviderView[] = [],
): ResolvedSurface {
  if (!selection) return { kind: "llm", descriptor: SURFACE_DESCRIPTORS.llm };
  const model =
    models.find(
      (row) =>
        row.modelId === selection.modelId &&
        (!selection.providerId || row.providerId === selection.providerId),
    ) ?? null;
  const provider =
    providers.find((row) => row.providerId === (selection.providerId ?? model?.providerId)) ?? null;
  if (selection.surfaceKind) {
    return resolveSurface(
      { ...(model ?? { providerId: selection.providerId ?? "", modelId: selection.modelId }), surfaceKind: selection.surfaceKind },
      provider,
    );
  }
  return resolveSurface(model, provider);
}

export interface SurfaceSwitch {
  allowed: boolean;
  /** The user must start a new session for this change. */
  requiresNewSession: boolean;
  from: SurfaceKind;
  to: SurfaceKind;
  reason?: string;
}

/** Same-surface switches are free; a cross-surface switch starts a new session
 * (a transcript history must never be presented as an image gallery's past). */
export function checkSurfaceSwitch(from: SurfaceKind, to: SurfaceKind): SurfaceSwitch {
  if (from === to) return { allowed: true, requiresNewSession: false, from, to };
  return {
    allowed: false,
    requiresNewSession: true,
    from,
    to,
    reason: `从「${SURFACE_DESCRIPTORS[from].label}」切换到「${SURFACE_DESCRIPTORS[to].label}」需要新建会话：两种会话的历史不能混用。`,
  };
}

/** The model's picker fact: the provider family and the surface it opens. */
export function surfaceFact(model: ModelView, provider?: ProviderView | null): string {
  const resolved = resolveSurface(model, provider);
  if (resolved.kind === "llm") return resolved.descriptor.label;
  // A medium whose panel is not shipped yet says so here rather than promising
  // a 专用面板 the user will not find.
  if (resolved.kind === "video" || resolved.kind === "generic") return `${resolved.descriptor.label} · 暂无专用面板`;
  return `${resolved.descriptor.label} · 专用面板`;
}

/** The team's captain decides the main session surface: an AgentTeam whose
 * captain runs a realtime model opens the realtime surface, an image captain
 * opens the gallery, and a team without a captain falls back to the text
 * baseline. The web shell has no team UI yet (`keel-agent-team-v1` is backend
 * only), so this is the seam the captain chip will call — pinned by test now so
 * the team screen cannot quietly invent a second rule. */
export function resolveCaptainSurface(
  captain: { providerId?: string; modelId?: string; surfaceKind?: string } | null | undefined,
  models: ModelView[],
  providers: ProviderView[] = [],
): ResolvedSurface {
  if (!captain || (!captain.modelId && !captain.surfaceKind)) {
    return { kind: "llm", descriptor: SURFACE_DESCRIPTORS.llm };
  }
  return resolveSelectionSurface(
    {
      modelId: captain.modelId ?? "",
      ...(captain.providerId ? { providerId: captain.providerId } : {}),
      ...(captain.surfaceKind ? { surfaceKind: captain.surfaceKind } : {}),
    },
    models,
    providers,
  );
}

