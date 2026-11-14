// The Keel web host: what a packaged web build adds to its page.
//
// `nsh package --target web` copies this module into the site and runs
// `createHost(player)` before the app starts. The functions the app may call
// are granted by `capabilities.host` in nsh.json (`keel.*`):
//
//   keel.notify   { title, body, tag }  system notification; resolves with
//                                       the permission state ("granted",
//                                       "denied", "default", "unsupported")
//   keel.info     {}                    build/runtime diagnostics
//   keel.route    {}                    the page's route ({ path, search }):
//                                       the app has no `location` of its own,
//                                       so `/settings` deep links read it here
//   keel.navigate { path }              pushState: keeps the page URL in step
//                                       with the app's own route
//
// Events: `keel.network` { online: boolean } whenever the browser's network
// state changes; `keel.route` { path, search } on browser back/forward, so the
// back button follows the app's routes.
//
// Realtime duplex audio (`keel.realtime.*`) is the browser half of a realtime
// voice session. The app never touches the microphone or an AudioContext:
// this module owns the continuous upload (getUserMedia → PCM16 at the
// provider's input rate → `input_audio_buffer.append`), the streaming playback
// (`response.audio.delta` → AudioBufferSourceNodes scheduled at its output
// rate) and the WebSocket lifecycle. `keel.realtime.start` accepts
// `providerKind` / `inputSampleRate` / `outputSampleRate` / `voice`: Qwen-Omni
// defaults to 16 kHz in and 24 kHz out, OpenAI to 24 kHz both ways. Provider
// JSON reaches the app as `keel.realtime.event` — transcript deltas, VAD
// events and tool calls untouched, audio deltas without their base64 payload
// (the bytes never cross the JSON bridge) — and the coarse call state as
// `keel.realtime.state` / `keel.realtime.level`.
//
// `keel.realtime.preflight` asks the same origin whether a realtime provider
// is configured before any socket exists: a GET of `/v1/realtime/status`
// (credentials included, 4 s bound) that reaches the app as
// `{ ok: true, configured, kind, models, inputSampleRate, outputSampleRate }`.
// A failed GET answers `{ ok: false, code: "preflight-failed", message }` (a
// host without `fetch`: `{ ok: false, code: "unsupported" }`). The message is
// one of a few fixed sentences — the URL, the key and the server's own error
// body never reach the app — so "not configured" can be a configuration state
// in the panel instead of a WebSocket close the app has to decode.
//
// Realtime functions answer `{ ok: true, ... }` or `{ ok: false, code,
// message }` rather than throwing: a host `call` rejection reaches the app as
// its message alone (a thrown error's `code` is lost at the bridge), and the
// panel must tell "permission denied" (text fallback) from "provider not
// configured" (configuration state). The rejection, when it happens, carries
// the same sentence.
export async function createHost(player) {
  const route = () => {
    if (typeof location === "undefined") return { path: "/", search: "" };
    return { path: location.pathname || "/", search: location.search || "" };
  };

  const navigate = (args) => {
    const path = String(args?.path ?? "/");
    if (typeof history !== "undefined") {
      try {
        history.pushState(null, "", path);
      } catch {
        /* the page refused the entry (a cross-origin frame): the route stays in-app */
      }
    }
    return route();
  };

  const notify = async (args) => {
    if (typeof Notification === "undefined") return "unsupported";
    let permission = Notification.permission;
    if (permission === "default") {
      try {
        permission = await Notification.requestPermission();
      } catch {
        return "default";
      }
    }
    if (permission !== "granted") return permission;
    try {
      const note = new Notification(String(args?.title ?? "Keel"), {
        body: String(args?.body ?? ""),
        tag: args?.tag ? String(args.tag) : undefined,
      });
      note.onclick = () => {
        try {
          window.focus();
        } catch {
          /* the page may refuse focus */
        }
        note.close();
      };
      return "granted";
    } catch {
      return "denied";
    }
  };

  const info = () => ({
    version: String(globalThis.__KEEL_VERSION__ ?? "0.1.0"),
    online: navigator.onLine,
    userAgent: navigator.userAgent,
    origin: location.origin,
  });

  // ---- realtime duplex audio ----------------------------------------------

  const realtime = createRealtime(player);

  const call = async (name, args) => {
    switch (name) {
      case "keel.notify":
        return notify(args);
      case "keel.info":
        return info();
      case "keel.route":
        return route();
      case "keel.navigate":
        return navigate(args);
      case "keel.realtime.start":
        return realtime.start(args);
      case "keel.realtime.preflight":
        return realtime.preflight();
      case "keel.realtime.send":
        return realtime.send(args);
      case "keel.realtime.mute":
        return realtime.mute(args);
      case "keel.realtime.stop":
        return realtime.stop();
      case "keel.realtime.status":
        return realtime.status();
      default:
        throw new Error(`no host function ${name}`);
    }
  };

  const sendNetwork = () => {
    try {
      player.hostEvent("keel.network", { online: navigator.onLine });
    } catch {
      /* an event that is not JSON must not break the page */
    }
  };
  // The page host also runs in hosts with no DOM globals (a headless test's
  // page host has only timers and console): every listener is optional.
  const onGlobal = (name, listener) => {
    try {
      if (typeof globalThis.addEventListener === "function") globalThis.addEventListener(name, listener);
    } catch {
      /* the host has no event loop to register on */
    }
  };
  onGlobal("online", sendNetwork);
  onGlobal("offline", sendNetwork);
  try {
    sendNetwork();
  } catch {
    /* no navigator in this host: nothing to report */
  }

  const sendRoute = () => {
    try {
      player.hostEvent("keel.route", route());
    } catch {
      /* same: never let an event break the page */
    }
  };
  onGlobal("popstate", sendRoute);

  // A call must not keep listening with the page hidden or gone: the browser
  // may throttle a background tab but the microphone keeps capturing, so the
  // session is ended instead of pretending to be a background call.
  if (typeof document !== "undefined" && typeof document.addEventListener === "function") {
    document.addEventListener("visibilitychange", () => {
      if (document.visibilityState === "hidden") realtime.end("页面已切到后台，通话已结束");
    });
  }
  onGlobal("pagehide", () => realtime.end("页面已关闭，通话已结束"));

  return { call };
}

// ---- pure audio/provider helpers (exported for the host-level tests) ------

/** The provider family this session configures. An explicit `providerKind`
 * wins; a provider id or model name that says OpenAI is the fallback for
 * callers that predate the argument. */
export function providerKindOf(args) {
  const explicit = String(args?.providerKind ?? "").trim().toLowerCase();
  // The surface resolves `providerKind` from the model descriptor, where a
  // realtime provider commonly says just "realtime": that is a family, not a
  // vendor, so it falls through to the id/model names.
  const generic = new Set(["", "realtime", "realtime-audio", "voice", "voice-agent", "generic", "audio"]);
  if (explicit && !generic.has(explicit)) return explicit;
  const text = `${args?.providerId ?? ""} ${args?.model ?? ""}`.toLowerCase();
  if (/openai|gpt-|whisper|(^|[^a-z0-9])o[134]([^a-z0-9]|$)/.test(text)) return "openai";
  return "qwen";
}

/** Sample rates and defaults per provider: Qwen-Omni streams PCM16 16 kHz in
 * and 24 kHz out; OpenAI's realtime API streams 24 kHz both ways. Explicit
 * `inputSampleRate` / `outputSampleRate` arguments always win. */
export function providerRates(args) {
  const kind = providerKindOf(args);
  const openai = kind === "openai";
  const rate = (value, fallback) => {
    const number = Number(value);
    return Number.isFinite(number) && number > 0 ? number : fallback;
  };
  return {
    kind,
    inputRate: rate(args?.inputSampleRate, openai ? 24000 : 16000),
    outputRate: rate(args?.outputSampleRate, 24000),
    voice: String(args?.voice ?? (openai ? "alloy" : "longanlingxin")),
    transcriptionModel: String(
      args?.transcriptionModel ?? (openai ? "whisper-1" : "qwen3-asr-flash-realtime"),
    ),
  };
}

const bytesToBase64 = (bytes) => {
  let binary = "";
  const step = 0x8000;
  for (let index = 0; index < bytes.length; index += step) {
    binary += String.fromCharCode.apply(null, bytes.subarray(index, Math.min(index + step, bytes.length)));
  }
  return btoa(binary);
};

/** Float32 [-1, 1] → base64 of PCM16 little-endian bytes. */
export function encodePcm16Base64(samples) {
  const bytes = new Uint8Array(samples.length * 2);
  const view = new DataView(bytes.buffer);
  for (let index = 0; index < samples.length; index += 1) {
    const sample = samples[index] < -1 ? -1 : samples[index] > 1 ? 1 : samples[index];
    view.setInt16(index * 2, Math.round(sample * 32767), true);
  }
  return bytesToBase64(bytes);
}

/**
 * Streaming linear resampler. `state = { pos, last }` carries the fractional
 * read position and the previous buffer's last frame across calls, so a
 * buffer boundary is interpolated between the two real frames instead of
 * restarting at a missing (zero) sample — that zero was a click at every
 * 4096-frame boundary (~11.7 Hz at 48 kHz).
 *
 * `pos` may be negative in (-1, 0): the next output falls between
 * `state.last` (the previous buffer's last frame) and `input[0]`. The last
 * interval of a buffer needs the NEXT buffer's first frame, so it is deferred
 * (never interpolated against a sample that does not exist yet).
 */
export function resampleLinear(input, from, to, state) {
  if (input.length === 0) return new Float32Array(0);
  if (from === to) {
    state.pos = 0;
    state.last = input[input.length - 1];
    return input;
  }
  const ratio = from / to;
  const out = [];
  let pos = state.pos;
  while (pos < input.length - 1) {
    if (pos < 0) {
      // Between the previous buffer's last frame and this buffer's first.
      const fraction = pos + 1;
      out.push(state.last + (input[0] - state.last) * fraction);
    } else {
      const at = Math.floor(pos);
      const fraction = pos - at;
      const a = input[at];
      const b = input[at + 1];
      out.push(a + (b - a) * fraction);
    }
    pos += ratio;
  }
  state.last = input[input.length - 1];
  state.pos = pos - input.length;
  return Float32Array.from(out);
}

/** The event the app may see: provider audio payloads are host-side only. A
 * delta's base64 is replaced by its size, so `keel.realtime.event` never
 * carries PCM through the JSON bridge (or into app state) — the type is kept,
 * so the timeline still knows the assistant is speaking. */
export function publicProviderEvent(payload) {
  if (String(payload?.type ?? "") !== "response.audio.delta") return payload;
  const { delta, audio, ...rest } = payload;
  const encoded = String(delta ?? audio ?? "");
  return { ...rest, audioStripped: true, audioBytes: Math.floor((encoded.length * 3) / 4) };
}

// ---- the realtime session ------------------------------------------------

/**
 * One live duplex session at most (a second `start` replaces the first).
 * Everything the session owns is closed together in `end()`: tracks, the
 * capture graph, the audio context, scheduled playback, timers and the socket.
 */
function createRealtime(player) {
  const MAX_RECONNECTS = 3;
  /** A preflight is a quick question before a call, not a call: a status
   * endpoint that does not answer in this window is reported as a failed
   * preflight (the app treats that as "unknown" and lets `start` speak). */
  const PREFLIGHT_TIMEOUT_MS = 4000;
  /** Close codes and sentences that say the provider cannot serve this call
   * at all. A reconnect cannot fix a missing key or an unsupported model and
   * would only hammer the backend. */
  const FATAL_CODES = new Set([4400, 4401, 4403, 4404, 4408]);
  // Deliberately narrow: a transient "service unavailable" is a reconnect,
  // a missing key or an unsupported model is not.
  const FATAL_REASON =
    /not configured|no realtime provider|provider (is )?(unavailable|not available)|invalid (api )?key|unauthorized|forbidden|invalid model|unsupported model|model not found|不(可用|受支持)|未配置|未授权|鉴权|密钥/i;
  const fatal = (close, reason) =>
    FATAL_CODES.has(Number(close?.code)) || FATAL_REASON.test(String(reason ?? ""));

  const now = () => Date.now();

  const event = (name, payload) => {
    try {
      player.hostEvent(name, payload);
    } catch {
      /* never let a host event break the page (or the audio loop) */
    }
  };

  const state = (phase, detail, extra) => {
    event("keel.realtime.state", { phase, detail: String(detail ?? ""), at: now(), ...(extra ?? {}) });
  };

  /** `{ ok, ... }`; the message is the only part a rejection would keep. */
  const failure = (code, message) => ({ ok: false, code, message: String(message) });

  const wsUrl = (args) => {
    if (typeof location === "undefined") throw new Error("no page origin: the realtime bridge needs a browser page");
    const scheme = location.protocol === "https:" ? "wss:" : "ws:";
    const query = new URLSearchParams({
      session_id: String(args?.sessionId ?? ""),
      model: String(args?.model ?? ""),
    });
    if (args?.providerId) query.set("provider_id", String(args.providerId));
    return `${scheme}//${location.host}/v1/realtime?${query.toString()}`;
  };

  /** The session object shared by every callback; `null` when no call is up. */
  let session = null;

  // ---- base64 / PCM ------------------------------------------------------

  const fromBase64 = (value) => {
    const binary = atob(String(value ?? ""));
    const bytes = new Uint8Array(binary.length);
    for (let index = 0; index < binary.length; index += 1) bytes[index] = binary.charCodeAt(index);
    return bytes;
  };

  const rms = (samples) => {
    if (!samples || samples.length === 0) return 0;
    let sum = 0;
    for (let index = 0; index < samples.length; index += 1) sum += samples[index] * samples[index];
    return Math.sqrt(sum / samples.length);
  };

  /** RMS → 0..1 for the waveform; a quiet voice still moves the bars. */
  const level = (value) => Math.min(1, Math.sqrt(Math.max(0, value)) * 3);

  // ---- session lifecycle -------------------------------------------------

  const clearTimers = (target) => {
    for (const id of target.timers) clearTimeout(id);
    target.timers.clear();
  };

  const later = (target, delay, fn) => {
    const id = setTimeout(() => {
      target.timers.delete(id);
      if (target.alive) fn();
    }, delay);
    target.timers.add(id);
    return id;
  };

  const sendJson = (target, payload) => {
    if (!target.ws || target.ws.readyState !== 1) return false;
    try {
      target.ws.send(JSON.stringify(payload));
      return true;
    } catch {
      return false;
    }
  };

  const stopPlayback = (target) => {
    for (const source of target.sources) {
      try {
        source.stop();
      } catch {
        /* already ended */
      }
    }
    target.sources.clear();
    target.nextPlayAt = 0;
    target.outputPeak = 0;
    target.speaking = false;
  };

  const playDelta = (target, base64) => {
    const context = target.context;
    // A cancelled turn's audio never reaches the speakers: deltas after a
    // barge-in are stale by definition.
    if (!context || !base64 || target.cancelGate) return;
    let bytes;
    try {
      bytes = fromBase64(base64);
    } catch {
      return;
    }
    const count = Math.floor(bytes.length / 2);
    if (count === 0) return;
    const frames = new Float32Array(count);
    const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    let peak = 0;
    for (let index = 0; index < count; index += 1) {
      const sample = view.getInt16(index * 2, true) / 32768;
      frames[index] = sample;
      const magnitude = Math.abs(sample);
      if (magnitude > peak) peak = magnitude;
    }
    if (peak > target.outputPeak) target.outputPeak = peak;
    let buffer;
    try {
      buffer = context.createBuffer(1, count, target.outputRate);
      buffer.copyToChannel(frames, 0);
    } catch {
      return;
    }
    const source = context.createBufferSource();
    source.buffer = buffer;
    source.connect(context.destination);
    const startAt = Math.max(context.currentTime + 0.02, target.nextPlayAt || 0);
    try {
      source.start(startAt);
    } catch {
      return;
    }
    target.nextPlayAt = startAt + buffer.duration;
    target.sources.add(source);
    source.onended = () => target.sources.delete(source);
    if (!target.speaking) {
      target.speaking = true;
      state("speaking", "助手正在说话（音频流式播放）");
    }
  };

  /** Barge-in: stop every scheduled frame now and cancel the response. */
  const bargeIn = (target, detail) => {
    const wasPlaying = target.sources.size > 0;
    const wasActive = target.responseActive;
    // Everything this response streams from now on belongs to a cancelled
    // turn; the gate stays until the provider starts the next response.
    target.cancelGate = true;
    stopPlayback(target);
    if (wasActive) {
      sendJson(target, { type: "response.cancel" });
      target.responseActive = false;
    }
    target.speaking = false;
    state(
      "interrupted",
      detail ?? (wasPlaying || wasActive ? "已打断：助手音频与生成已停止" : "检测到用户开口"),
    );
  };

  const sessionUpdate = (target) => {
    const openai = target.providerKind === "openai";
    // The nested format is what the current realtime APIs read. Qwen-Omni
    // documents the descriptive form; OpenAI's GA API names the same PCM by
    // its media type and rate. The flat `*_audio_format` fields stay for
    // servers from before the nested form.
    const inputFormat = openai
      ? { type: "audio/pcm", rate: target.inputRate }
      : {
          type: "pcm",
          sample_rate: target.inputRate,
          sample_format: "s16le",
          channels: 1,
          packing: "interleaved",
          channel_layout: "mono",
        };
    const outputFormat = openai
      ? { type: "audio/pcm", rate: target.outputRate }
      : { type: "pcm", sample_rate: target.outputRate };
    sendJson(target, {
      type: "session.update",
      session: {
        modalities: ["text", "audio"],
        voice: target.voice,
        instructions: target.instructions,
        input_audio_format: "pcm16",
        output_audio_format: "pcm16",
        input_audio_transcription: { model: target.transcriptionModel },
        turn_detection: {
          type: "server_vad",
          threshold: 0.5,
          prefix_padding_ms: 300,
          silence_duration_ms: 700,
          create_response: true,
          interrupt_response: true,
        },
        audio: {
          input: { format: inputFormat },
          output: { voice: target.voice, format: outputFormat },
        },
      },
    });
  };

  const markReady = (target, detail, extra, fromProvider = false) => {
    if (target.ready) return;
    target.ready = true;
    // Only the provider's own session event proves the session is real; the
    // grace fallback below must not reset the reconnect budget.
    if (fromProvider) target.attempts = 0;
    state("ready", detail, extra);
  };

  const handleEvent = (target, payload) => {
    const type = String(payload?.type ?? "");
    switch (type) {
      case "session.created":
      case "session.updated":
        markReady(target, "实时会话已就绪：麦克风持续上行，开口即可说话", undefined, true);
        return;
      case "input_audio_buffer.speech_started":
        bargeIn(target, "检测到你开口，助手已停下");
        return;
      case "response.created":
        // A new response is the one thing that lifts a cancelled turn's gate.
        target.cancelGate = false;
        target.responseActive = true;
        target.speaking = false;
        state("thinking", "助手正在组织回复");
        return;
      case "response.audio.delta":
        if (target.cancelGate) return;
        playDelta(target, payload?.delta ?? payload?.audio);
        return;
      case "response.audio.done":
        // Audio of a cancelled turn stays stopped.
        if (target.cancelGate) return;
        return;
      case "response.done":
        target.responseActive = false;
        if (target.cancelGate) return;
        if (target.speaking) {
          target.speaking = false;
          state("ready", "这一轮结束，可以继续说话");
        }
        return;
      case "error": {
        const message = String(payload?.error?.message ?? payload?.message ?? "Provider 返回错误");
        state("error", message, { recoverable: true });
        return;
      }
      default:
        return;
    }
  };

  const attachSocket = (target) => {
    let socket;
    try {
      socket = new WebSocket(target.url);
    } catch (error) {
      return Promise.reject(error);
    }
    target.ws = socket;
    return new Promise((resolve, reject) => {
      let settled = false;
      // Returns whether it settled: a call that was answered already (the
      // socket opened) is not rejected by a later close.
      const settle = (fn, value) => {
        if (settled) return false;
        settled = true;
        fn(value);
        return true;
      };
      const timer = setTimeout(() => {
        settle(reject, new Error("实时连接在 8 秒内没有建立"));
        try {
          socket.close();
        } catch {
          /* already closed */
        }
      }, 8000);
      socket.onopen = () => {
        clearTimeout(timer);
        settle(resolve, socket);
      };
      socket.onerror = () => {
        /* `onclose` carries the reason; an error alone must not settle */
      };
      socket.onmessage = (message) => {
        let payload;
        try {
          payload = JSON.parse(String(message.data));
        } catch {
          return; // not provider JSON: nothing to forward
        }
        // Audio bytes stay host-side; the app gets the event without them.
        event("keel.realtime.event", { event: publicProviderEvent(payload) });
        if (payload?.type === "error") {
          target.lastReason = String(payload?.error?.message ?? payload?.message ?? "Provider 返回错误");
        }
        if (!target.ready && payload?.type === "error") {
          settle(reject, new Error(target.lastReason || "Provider 拒绝了会话"));
        }
        if (target.alive) handleEvent(target, payload);
      };
      socket.onclose = (close) => {
        clearTimeout(timer);
        if (target.ws !== socket) return; // an older socket: already replaced
        target.ws = null;
        const reason = String(close?.reason || "").trim();
        // The provider's own sentence, when it gave one: it is what `start`
        // reports to the app (a bare code says nothing about a missing key).
        if (reason) target.lastReason = reason;
        else if (!target.lastReason) target.lastReason = `连接中断（${close?.code ?? "?"}）`;
        if (!target.alive) return;
        if (!target.ready) {
          // Before the session event, a close fails the pending start (its
          // catch ends the session). After the socket opened, the open answer
          // already stands: the loss is a reconnect, not a failed start.
          if (settle(reject, new Error(reason || `实时连接被服务端关闭（${close?.code ?? "?"}）`))) return;
        }
        if (fatal(close, target.lastReason)) {
          // Provider-unavailable is not a network blip: retrying it would
          // storm the backend with the same refused handshake.
          state("error", `Provider 不可用：${target.lastReason}`, { recoverable: false });
          end("实时会话已结束");
          return;
        }
        scheduleReconnect(target, reason || `连接中断（${close?.code ?? "?"}）`);
      };
    });
  };

  const scheduleReconnect = (target, reason) => {
    if (fatal({}, reason)) {
      state("error", `Provider 不可用：${reason}`, { recoverable: false });
      end("实时会话已结束");
      return;
    }
    target.ready = false;
    target.speaking = false;
    target.responseActive = false;
    stopPlayback(target);
    target.attempts += 1;
    if (target.attempts > MAX_RECONNECTS) {
      state("error", `连接已断开，重连 ${MAX_RECONNECTS} 次都失败：${reason}`, { recoverable: false });
      end("连接已断开");
      return;
    }
    state("reconnecting", `连接中断，正在重连（第 ${target.attempts} 次）：${reason}`, {
      attempt: target.attempts,
    });
    later(target, 400 * target.attempts, () => {
      attachSocket(target)
        .then((socket) => {
          // A new socket is a new provider session: the old session's
          // transcript is not resumed, and the app is told so. The fallback
          // belongs to THIS socket: a later reconnect must not read its timer
          // as this one's readiness.
          sessionUpdate(target);
          later(target, 800, () => {
            if (!target.alive || target.ws !== socket) return;
            if (!target.ready) {
              markReady(target, "已重连（新的 Provider 会话，断线前的转写不会重放）", { reconnected: true });
            }
          });
        })
        .catch((error) => {
          if (!target.alive) return;
          scheduleReconnect(target, error?.message ?? "重连失败");
        });
    });
  };

  const requestMic = async (target) => {
    const media = typeof navigator !== "undefined" ? navigator.mediaDevices : undefined;
    if (!media || typeof media.getUserMedia !== "function") {
      return { ok: false, code: "unsupported", message: "这个浏览器没有麦克风接口（navigator.mediaDevices）" };
    }
    let stream;
    try {
      stream = await media.getUserMedia({
        audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true },
      });
    } catch (error) {
      const name = String(error?.name ?? "");
      const denied = name === "NotAllowedError" || name === "SecurityError" || name === "PermissionDeniedError";
      return {
        ok: false,
        code: denied
          ? "not-allowed"
          : name === "NotFoundError" || name === "DevicesNotFoundError"
            ? "no-device"
            : "mic-failed",
        message: denied
          ? "麦克风权限被拒绝：可以继续用文字和助手对话，助手语音仍会播放"
          : name
            ? `无法使用麦克风（${name}）`
            : "无法使用麦克风",
      };
    }
    if (!target.alive || !target.micWanted) {
      // The call moved on (a stop, or the permission prompt timed out) while
      // the prompt was open: release the device instead of listening on.
      for (const track of stream.getTracks()) {
        try {
          track.stop();
        } catch {
          /* already stopped */
        }
      }
      return { ok: false, code: "mic-timeout", message: "麦克风授权超时：可以继续用文字和助手对话" };
    }
    target.stream = stream;
    return { ok: true, code: "ok", message: "" };
  };

  const startCapture = (target) => {
    const context = target.context;
    if (!context || !target.stream) return;
    let source;
    try {
      source = context.createMediaStreamSource(target.stream);
      // 4096 frames ≈ 85 ms at 48 kHz: small enough to feel live, large
      // enough that the resample and base64 cost stay trivial.
      target.processor = context.createScriptProcessor(4096, 1, 1);
    } catch {
      return;
    }
    target.capture = source;
    // A muted gain stage keeps the processor pulled by the graph without
    // echoing the microphone back to the speakers.
    target.silencer = context.createGain();
    target.silencer.gain.value = 0;
    target.processor.onaudioprocess = (audio) => {
      if (!target.alive) return;
      const input = audio.inputBuffer.getChannelData(0);
      const peak = rms(input);
      if (peak > target.inputPeak) target.inputPeak = peak;
      if (target.muted || !target.ready) return;
      const resampled = resampleLinear(input, target.sourceRate, target.inputRate, target.resampleState);
      sendJson(target, {
        type: "input_audio_buffer.append",
        audio: encodePcm16Base64(resampled),
      });
    };
    source.connect(target.processor);
    target.processor.connect(target.silencer);
    target.silencer.connect(context.destination);
  };

  const stopCapture = (target) => {
    if (target.processor) {
      try {
        target.processor.disconnect();
        target.silencer?.disconnect();
        target.capture?.disconnect();
      } catch {
        /* already detached */
      }
      target.processor.onaudioprocess = null;
    }
    target.processor = null;
    target.silencer = null;
    target.capture = null;
    if (target.stream) {
      for (const track of target.stream.getTracks()) {
        try {
          track.stop();
        } catch {
          /* already stopped */
        }
      }
    }
    target.stream = null;
  };

  const emitLevels = (target) => {
    if (!target.alive) return;
    const input = level(target.inputPeak);
    const output = level(target.outputPeak);
    target.inputPeak = 0;
    target.outputPeak = 0;
    event("keel.realtime.level", { input, output });
  };

  /** Resolves when the provider's session event arrives, or after a bounded
   * grace period for servers that never send one. */
  const waitForReady = (target) =>
    new Promise((resolve) => {
      const started = Date.now();
      const settle = (value) => {
        if (target.settleReady !== settle) return;
        target.settleReady = null;
        resolve(value);
      };
      target.settleReady = settle;
      const poll = setInterval(() => {
        if (!target.alive) {
          clearInterval(poll);
          settle(false);
          return;
        }
        if (target.ready) {
          clearInterval(poll);
          settle(true);
          return;
        }
        if (Date.now() - started > 6000) {
          clearInterval(poll);
          markReady(target, "实时会话已开始（Provider 未发 session.created，按已就绪处理）");
          settle(true);
        }
      }, 100);
      target.timers.add(poll);
    });

  /** Ends a session: every resource it owns goes, and the app is told why. */
  function end(reason, code) {
    const target = session;
    session = null;
    if (!target) {
      state("closed", reason ?? "通话已结束");
      return;
    }
    target.alive = false;
    target.micWanted = false;
    // A `start` still waiting for the provider's session event must settle:
    // clearing the poll below would otherwise leave its promise pending until
    // the app's own timeout.
    if (target.settleReady) {
      target.settleReady(false);
      target.settleReady = null;
    }
    if (target.settleMic) {
      // The microphone prompt may outlive the call: answer it as ended, and
      // `requestMic` releases the stream if it is granted later.
      target.settleMic({ ok: false, code: "closed", message: "实时会话已结束" });
      target.settleMic = null;
    }
    clearTimers(target);
    if (target.meter) clearInterval(target.meter);
    stopCapture(target);
    stopPlayback(target);
    const socket = target.ws;
    target.ws = null;
    if (socket) {
      try {
        socket.close(1000, "client closed");
      } catch {
        /* already closed */
      }
    }
    if (target.context) {
      const context = target.context;
      target.context = null;
      try {
        if (typeof context.close === "function") context.close();
      } catch {
        /* already closed */
      }
    }
    state("closed", reason ?? "通话已结束", code ? { code } : undefined);
  }

  /** Model ids from the status payload: strings stay as they are, an object
   * entry contributes its id/name. Anything else is dropped rather than
   * stringified into "[object Object]". */
  const modelNames = (value) => {
    if (!Array.isArray(value)) return [];
    return value
      .map((entry) =>
        typeof entry === "string" ? entry : String(entry?.id ?? entry?.model ?? entry?.name ?? ""),
      )
      .filter((name) => name.length > 0);
  };

  /** A rate the app can use, or `undefined` (the app validates the range
   * itself; the host only refuses nonsense). */
  const rateOf = (value) => {
    const rate = Number(value);
    return Number.isFinite(rate) && rate > 0 ? rate : undefined;
  };

  /**
   * `keel.realtime.preflight`: is a realtime provider configured on this
   * origin, and what does it speak? A same-origin GET, so the answer arrives
   * before a socket exists — the panel can show "voice is not configured"
   * (with a next step) instead of a WebSocket close (1006) it has to decode.
   *
   * The reply never carries the URL, a key or the server's error body: every
   * failure is one of the fixed sentences below with a stable `code`. The
   * `AbortController` bound is what keeps "unreachable" from hanging a call
   * attempt; when the host has no `fetch` the answer is `unsupported`.
   */
  const preflight = async () => {
    if (typeof fetch !== "function") {
      return failure("unsupported", "这个宿主没有 fetch，无法检查实时语音服务状态");
    }
    const controller = typeof AbortController === "function" ? new AbortController() : null;
    let timedOut = false;
    let timer = null;
    // The race settles the call even where `AbortController` is missing (the
    // fetch itself cannot be cancelled there, but its answer is ignored).
    const timeout = new Promise((_, reject) => {
      timer = setTimeout(() => {
        timedOut = true;
        controller?.abort();
        reject(new Error("preflight timeout"));
      }, PREFLIGHT_TIMEOUT_MS);
    });
    try {
      const response = await Promise.race([
        fetch("/v1/realtime/status", {
          method: "GET",
          credentials: "include",
          headers: { Accept: "application/json" },
          cache: "no-store",
          ...(controller ? { signal: controller.signal } : {}),
        }),
        timeout,
      ]);
      if (!response?.ok) {
        const status = Number(response?.status);
        return failure(
          "preflight-failed",
          Number.isFinite(status) && status > 0
            ? `实时语音状态接口返回 HTTP ${status}`
            : "实时语音状态接口没有正常响应",
        );
      }
      let payload;
      try {
        payload = await response.json();
      } catch {
        return failure("preflight-failed", "实时语音状态响应不是 JSON");
      }
      if (!payload || typeof payload !== "object" || typeof payload.configured !== "boolean") {
        // Not a status document (an SPA fallback page, a proxy error page):
        // "unknown" is the honest answer, not a made-up "unconfigured".
        return failure("preflight-failed", "实时语音状态响应缺少 configured 字段");
      }
      const inputRate = rateOf(payload.inputSampleRate);
      const outputRate = rateOf(payload.outputSampleRate);
      return {
        ok: true,
        configured: payload.configured,
        ...(typeof payload.kind === "string" && payload.kind ? { kind: payload.kind } : {}),
        models: modelNames(payload.models),
        ...(inputRate ? { inputSampleRate: inputRate } : {}),
        ...(outputRate ? { outputSampleRate: outputRate } : {}),
      };
    } catch {
      // The error is deliberately dropped: a browser's fetch rejection can
      // quote the URL, and the URL is not the app's business.
      return failure(
        "preflight-failed",
        timedOut
          ? `实时语音状态检查超时（${PREFLIGHT_TIMEOUT_MS / 1000} 秒）`
          : "无法连接实时语音状态接口",
      );
    } finally {
      clearTimeout(timer);
    }
  };

  return {
    async start(args) {
      if (typeof WebSocket === "undefined") {
        return failure("unsupported", "这个宿主没有 WebSocket，无法建立实时会话");
      }
      const AudioContextClass =
        typeof AudioContext !== "undefined"
          ? AudioContext
          : typeof globalThis !== "undefined" && globalThis.webkitAudioContext
            ? globalThis.webkitAudioContext
            : undefined;
      if (!AudioContextClass) {
        return failure("unsupported", "这个浏览器没有 Web Audio（AudioContext），无法播放助手语音");
      }
      if (!String(args?.model ?? "").trim()) {
        return failure("bad-request", "没有指定实时模型：先选择一个 realtime Provider 的模型");
      }
      if (session) end("上一次实时会话已被新的会话替换");

      // Qwen 16k/24k, OpenAI 24k/24k, both overridable: the caller may pass
      // `providerKind` / `inputSampleRate` / `outputSampleRate`.
      const rates = providerRates(args);
      const target = {
        alive: true,
        ready: false,
        muted: false,
        speaking: false,
        responseActive: false,
        /** A response was cancelled (barge-in or explicit cancel): its late
         * audio deltas must not play or flip the state back to speaking. */
        cancelGate: false,
        attempts: 0,
        timers: new Set(),
        sources: new Set(),
        nextPlayAt: 0,
        inputPeak: 0,
        outputPeak: 0,
        resampleState: { pos: 0, last: 0 },
        providerKind: rates.kind,
        voice: rates.voice,
        instructions: String(
          args?.instructions ?? "你是 Keel 的实时语音助手。回答简短、口语化，需要时再展开。",
        ),
        transcriptionModel: rates.transcriptionModel,
        inputRate: rates.inputRate,
        outputRate: rates.outputRate,
        stream: null,
        context: null,
        processor: null,
        capture: null,
        silencer: null,
        sourceRate: rates.inputRate,
        ws: null,
        meter: null,
        micWanted: false,
        settleReady: null,
        settleMic: null,
        lastReason: "",
        url: "",
      };
      session = target;
      state("connecting", "正在连接实时会话…");

      try {
        target.url = wsUrl(args);
      } catch (error) {
        const reply = failure("unsupported", error?.message ?? "无法确定页面地址");
        end(reply.message);
        return reply;
      }

      try {
        target.context = new AudioContextClass();
        if (typeof target.context.resume === "function") {
          // Not awaited: a resumed context still plays what is scheduled
          // after this point, and a refusal must not stall the call.
          target.context.resume().catch(() => {});
        }
        target.sourceRate = Number(target.context.sampleRate) || target.inputRate;
      } catch (error) {
        const reply = failure("unsupported", `无法启动音频上下文：${error?.message ?? error}`);
        end(reply.message);
        return reply;
      }

      try {
        await attachSocket(target);
      } catch (error) {
        const reply = failure("unavailable", error?.message ?? "实时连接没有建立");
        end(reply.message);
        return reply;
      }

      // Configure the session at once: the microphone prompt may stay open
      // for seconds, and the provider must not wait for the user's answer
      // before learning the modality, formats and VAD it should use.
      sessionUpdate(target);
      const readyPromise = waitForReady(target);

      // Ask for the microphone while the provider creates its session. A
      // denied microphone does not end the call: the session continues with
      // text in and the assistant's voice out. A prompt the user never
      // answers is bounded, so the promise settles.
      target.micWanted = true;
      const mic = await new Promise((resolve) => {
        const settle = (value) => {
          if (target.settleMic !== settle) return;
          target.settleMic = null;
          resolve(value);
        };
        target.settleMic = settle;
        requestMic(target).then(settle);
        // A permission prompt the user never answers (or a stop while it is
        // open) must still settle this call.
        later(target, 20000, () =>
          settle({ ok: false, code: "mic-timeout", message: "麦克风授权超时：可以继续用文字和助手对话" }),
        );
      });
      target.micWanted = false;
      if (mic.ok) startCapture(target);

      const ready = await readyPromise;

      if (!ready || !target.alive) {
        const reason = target.lastReason;
        const reply = failure(
          reason ? "provider-error" : "closed",
          reason ? `实时会话在就绪前关闭：${reason}` : "实时会话在就绪前关闭了",
        );
        // A reconnect failure already ended (and reported) the session: the
        // app gets the reason once, not a second "closed" on top.
        if (target.alive) end(reply.message);
        return reply;
      }

      target.meter = setInterval(() => emitLevels(target), 100);

      if (!mic.ok) {
        // Keep the call: the app shows the permission state and its text
        // fallback sends through the same session.
        state("ready", mic.message, { mic: mic.code });
        return { ok: true, mic: mic.code, message: mic.message };
      }
      return { ok: true, mic: "granted", message: "" };
    },

    send(args) {
      const payload = args?.payload;
      if (!payload || typeof payload !== "object") {
        return failure("bad-request", "payload 必须是 Provider JSON 事件");
      }
      const target = session;
      if (!target || !target.ws || target.ws.readyState !== 1) {
        return failure("closed", "实时会话未连接：先开始实时对话");
      }
      if (!sendJson(target, payload)) {
        return failure("unavailable", "事件没有发出：连接正在关闭");
      }
      if (payload.type === "response.cancel") {
        target.cancelGate = true;
        stopPlayback(target);
        target.responseActive = false;
      }
      return { ok: true };
    },

    mute(args) {
      const target = session;
      const muted = Boolean(args?.muted);
      if (!target) {
        return failure("closed", "实时会话未连接：没有可静音的麦克风");
      }
      target.muted = muted;
      if (target.stream) {
        for (const track of target.stream.getTracks()) track.enabled = !muted;
      }
      state(
        target.ready ? "ready" : "connecting",
        muted ? "麦克风已静音（仍在通话中，助手语音不受影响）" : "麦克风已恢复",
        { muted },
      );
      return { ok: true, muted };
    },

    stop() {
      const wasLive = session !== null;
      end("通话已结束");
      return { ok: true, wasLive };
    },

    /** A question, not a session: it never touches (or reports through) the
     * live call. */
    preflight,

    status() {
      const target = session;
      return target
        ? {
            ok: true,
            live: true,
            ready: target.ready,
            muted: target.muted,
            attempts: target.attempts,
            providerKind: target.providerKind,
            inputRate: target.inputRate,
            outputRate: target.outputRate,
          }
        : { ok: true, live: false };
    },

    /** Ends from the page side (hidden/closed): no caller to answer. */
    end(reason) {
      if (session) end(reason);
    },
  };
}
