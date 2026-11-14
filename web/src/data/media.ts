// The media gateway: Keel's same-origin image API (change
// keel-session-surfaces-v1, media session surface — real provider path).
//
//   GET  /v1/media/capabilities        → { configured, image, … }
//   POST /v1/media/images/generate     → { assets: [{ url, model }], model }
//   POST /v1/media/images/edit         → { assets: [{ url, model }], model }
//
// Requests go to this origin with the session cookie (`credentials:
// "same-origin"`); a dev origin is not configurable here because the gateway
// exists only behind `keel serve`. The gateway sanitizes provider failures; this
// client preserves that message verbatim (collapsed to one line) and classifies
// the failure so the surface can tell "not configured" (503) from a transient
// error and from an abort it caused itself.
//
// Nothing here invents a capability: a response that does not say `image: true`
// leaves the surface in its labelled demo mode, and video/tts stay out of this
// client entirely.
export type MediaErrorKind = "notConfigured" | "aborted" | "http" | "network" | "invalidResponse";

export class MediaGatewayError extends Error {
  readonly kind: MediaErrorKind;
  /** The HTTP status, when a response was read. */
  readonly status: number | undefined;

  constructor(kind: MediaErrorKind, message: string, status?: number) {
    super(message);
    this.name = "MediaGatewayError";
    this.kind = kind;
    this.status = status;
  }

  /** The gateway has no image provider: the surface offers its configuration
   * action instead of a retry that would fail the same way. */
  get notConfigured(): boolean {
    return this.kind === "notConfigured";
  }

  get aborted(): boolean {
    return this.kind === "aborted";
  }
}

/** `GET /v1/media/capabilities`. `configured` says the gateway has usable
 * media providers; `image` says image generation is one of them. Both must be
 * true for a real image turn. */
export interface MediaCapabilities {
  configured: boolean;
  image: boolean;
  tts?: boolean;
  stt?: boolean;
}

/** `POST /v1/media/images/generate`. `size` is a provider-facing `WxH` box;
 * `n` is the number of images; `model` pins a model when the caller knows one
 * (absent: the gateway's configured default). */
export interface GenerateImagesRequest {
  prompt: string;
  negativePrompt?: string;
  size?: string;
  n?: number;
  model?: string;
}

/** `POST /v1/media/images/edit`. `image` is the reference and must be an
 * http(s) URL (a gallery asset's provider URL qualifies); the gateway rejects
 * data URLs, so the client never sends one. */
export interface EditImageRequest {
  prompt: string;
  image: string;
  model?: string;
}

/** One generated image. `model` is the model that produced it, when the
 * gateway names one. */
export interface MediaAsset {
  url: string;
  model?: string;
}

export interface MediaImagesResult {
  assets: MediaAsset[];
  /** The model that served the turn (the per-asset value wins when both are
   * present). */
  model?: string;
}

export interface MediaRequestOptions {
  signal?: AbortSignal;
}

const CAPABILITIES_PATH = "/v1/media/capabilities";
const GENERATE_PATH = "/v1/media/images/generate";
const EDIT_PATH = "/v1/media/images/edit";

/** The origin the media paths resolve against. Production leaves it empty:
 * the gateway is same-origin behind `keel serve`. A headless renderer has no
 * origin for a relative URL to resolve against, so its harness sets a
 * synthetic one and the same request/response path (and the same parsing)
 * still runs. */
let mediaBase = "";

/** Test/host seam: pass `""` to restore same-origin requests. */
export function setMediaBase(base: string): void {
  mediaBase = base.replace(/\/$/, "");
}

const mediaUrl = (path: string): string => `${mediaBase}${path}`;

const asRecord = (value: unknown): Record<string, unknown> =>
  value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : {};

const textOf = (value: unknown): string => (typeof value === "string" ? value.trim() : "");

/** A provider message kept to one readable line: the gateway already redacts
 * credentials, so this only folds whitespace and bounds what the surface draws. */
const oneLine = (text: string): string => {
  const flat = text.replace(/\s+/g, " ").trim();
  return flat.length > 320 ? `${flat.slice(0, 319)}…` : flat;
};

const abortError = (): MediaGatewayError => new MediaGatewayError("aborted", "已取消");

const isAbort = (error: unknown, signal?: AbortSignal): boolean => {
  if (signal?.aborted) return true;
  if (!error || typeof error !== "object") return false;
  const name = String((error as { name?: unknown }).name ?? "");
  return name === "AbortError" || name === "TimeoutError";
};

/** A response body's message: `error`, then `message`, then the raw text. */
async function readErrorMessage(response: Response): Promise<string> {
  try {
    const text = await response.text();
    if (!text) return `HTTP ${response.status}`;
    try {
      const body = asRecord(JSON.parse(text));
      const message = textOf(body.error) || textOf(body.message);
      if (message) return oneLine(message);
    } catch {
      /* not JSON: the text itself is the server's note */
    }
    return oneLine(text) || `HTTP ${response.status}`;
  } catch {
    return `HTTP ${response.status}`;
  }
}

async function requestJson(
  path: string,
  init: RequestInit,
  options?: MediaRequestOptions,
): Promise<unknown> {
  const { signal } = options ?? {};
  let response: Response;
  try {
    response = await fetch(mediaUrl(path), {
      credentials: "same-origin",
      ...init,
      ...(signal ? { signal } : {}),
    });
  } catch (error) {
    if (isAbort(error, signal)) throw abortError();
    throw new MediaGatewayError(
      "network",
      oneLine(error instanceof Error ? error.message : String(error)) || "无法连接图像网关",
    );
  }
  if (!response.ok) {
    const message = await readErrorMessage(response);
    if (response.status === 503) {
      throw new MediaGatewayError("notConfigured", message || "图像服务未配置", response.status);
    }
    throw new MediaGatewayError("http", message || `HTTP ${response.status}`, response.status);
  }
  try {
    return (await response.json()) as unknown;
  } catch {
    if (signal?.aborted) throw abortError();
    throw new MediaGatewayError("invalidResponse", "图像网关返回了无法解析的响应", response.status);
  }
}

/** `GET /v1/media/capabilities`: what the gateway can actually serve right
 * now. A probe never throws on a non-2xx from a static host: the surface's
 * caller treats every failure as "demo only". */
export async function mediaCapabilities(options?: MediaRequestOptions): Promise<MediaCapabilities> {
  const body = asRecord(await requestJson(CAPABILITIES_PATH, { method: "GET" }, options));
  return {
    configured: body.configured === true,
    image: body.image === true,
    tts: body.tts === true,
    stt: body.stt === true,
  };
}

const assetsOf = (value: unknown): MediaAsset[] => {
  const body = asRecord(value);
  const rows = Array.isArray(body.assets) ? body.assets : [];
  const assets: MediaAsset[] = [];
  for (const row of rows) {
    const item = asRecord(row);
    const url = textOf(item.url);
    if (!url) continue;
    const model = textOf(item.model);
    assets.push(model ? { url, model } : { url });
  }
  return assets;
};

const resultOf = (value: unknown): MediaImagesResult => {
  const body = asRecord(value);
  const model = textOf(body.model);
  return {
    assets: assetsOf(body),
    ...(model ? { model } : {}),
  };
};

const postJson = async (
  path: string,
  payload: Record<string, unknown>,
  options?: MediaRequestOptions,
): Promise<MediaImagesResult> => {
  const value = await requestJson(
    path,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    },
    options,
  );
  return resultOf(value);
};

/** `POST /v1/media/images/generate`: the real generation path. A 503 carries
 * the gateway's sanitized "not configured" note; an abort carries `aborted`. */
export async function generateImages(
  request: GenerateImagesRequest,
  options?: MediaRequestOptions,
): Promise<MediaImagesResult> {
  const payload: Record<string, unknown> = { prompt: request.prompt };
  if (request.negativePrompt?.trim()) payload.negativePrompt = request.negativePrompt.trim();
  if (request.size) payload.size = request.size;
  if (request.n !== undefined) payload.n = request.n;
  if (request.model) payload.model = request.model;
  return postJson(GENERATE_PATH, payload, options);
}

/** `POST /v1/media/images/edit`: one image in, the edited image family out.
 * The multi-turn chain is the surface's: the gateway sees one reference and one
 * instruction. */
export async function editImage(
  request: EditImageRequest,
  options?: MediaRequestOptions,
): Promise<MediaImagesResult> {
  const payload: Record<string, unknown> = { prompt: request.prompt, image: request.image };
  if (request.model) payload.model = request.model;
  return postJson(EDIT_PATH, payload, options);
}

// ---- speech (TTS / STT) ----------------------------------------------------
//
// Same gateway, same failure classification: a deployment without a speech
// provider answers 503, and the workbench shows its configuration state. There
// is no client-side sample: the text that comes back is the provider's.

const SPEECH_PATH = "/v1/media/audio/speech";
const TRANSCRIBE_PATH = "/v1/media/audio/transcriptions";

/** A synthesized clip: `url` is an object URL the caller owns (revoke it on
 * unmount); `model` names the provider model that produced it. */
export interface SpeechResult {
  url: string;
  model?: string;
}

/** `POST /v1/media/audio/speech`. The body is audio bytes, not JSON, so this
 * path bypasses `requestJson`. */
export async function synthesizeSpeech(
  request: { text: string; voice?: string; model?: string },
  options?: MediaRequestOptions,
): Promise<SpeechResult> {
  let response: Response;
  try {
    response = await fetch(mediaUrl(SPEECH_PATH), {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        text: request.text,
        ...(request.voice ? { voice: request.voice } : {}),
        ...(request.model ? { model: request.model } : {}),
      }),
      ...(options?.signal ? { signal: options.signal } : {}),
    });
  } catch (error) {
    if (isAbort(error, options?.signal)) throw abortError();
    throw new MediaGatewayError("network", oneLine(error instanceof Error ? error.message : String(error)) || "无法连接语音服务");
  }
  if (!response.ok) {
    const message = await readErrorMessage(response);
    if (response.status === 503) throw new MediaGatewayError("notConfigured", message || "语音服务未配置", 503);
    throw new MediaGatewayError("http", message || `HTTP ${response.status}`, response.status);
  }
  const bytes = await response.arrayBuffer();
  if (bytes.byteLength === 0) throw new MediaGatewayError("invalidResponse", "语音服务返回了空音频", response.status);
  const type = response.headers.get("content-type") ?? "audio/wav";
  const model = response.headers.get("x-keel-model") ?? undefined;
  const url = URL.createObjectURL(new Blob([bytes], { type }));
  return model ? { url, model } : { url };
}

/** `POST /v1/media/audio/transcriptions` — multipart audio in, text out. */
export async function transcribeAudio(
  request: { audio: Blob; filename?: string; model?: string; language?: string },
  options?: MediaRequestOptions,
): Promise<{ text: string; model?: string }> {
  const form = new FormData();
  form.append("file", request.audio, request.filename ?? "speech.webm");
  if (request.model) form.append("model", request.model);
  if (request.language) form.append("language", request.language);
  let response: Response;
  try {
    response = await fetch(mediaUrl(TRANSCRIBE_PATH), {
      method: "POST",
      credentials: "same-origin",
      body: form,
      ...(options?.signal ? { signal: options.signal } : {}),
    });
  } catch (error) {
    if (isAbort(error, options?.signal)) throw abortError();
    throw new MediaGatewayError("network", oneLine(error instanceof Error ? error.message : String(error)) || "无法连接语音服务");
  }
  if (!response.ok) {
    const message = await readErrorMessage(response);
    if (response.status === 503) throw new MediaGatewayError("notConfigured", message || "语音服务未配置", 503);
    throw new MediaGatewayError("http", message || `HTTP ${response.status}`, response.status);
  }
  const body = asRecord(await response.json().catch(() => ({})));
  const text = textOf(body.text);
  if (!text) throw new MediaGatewayError("invalidResponse", "语音服务没有返回转写文本", response.status);
  const model = textOf(body.model);
  return model ? { text, model } : { text };
}
