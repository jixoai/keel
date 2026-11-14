// Host-level tests for `host.js` (the page host's realtime bridge).
//
// Run with Node's own test runner — no dependencies, no build:
//
//   node --test web/host.selftest.mjs
//
// `host.js` is a browser ES module, but the app directory has no
// `"type": "module"`, so Node would read a `.js` import as CommonJS. The file
// is loaded through a `data:` URL instead: it imports nothing, so this is a
// faithful module evaluation with only the browser globals stubbed below.
//
// Covered: the pure audio/provider helpers (resample continuity across buffer
// boundaries, PCM16 base64, provider rates), one live session driven through a
// fake WebSocket/AudioContext: `session.update` timing and shape, PCM append,
// barge-in gating (no late audio, no state flip), forwarded events without
// audio bytes, no invented keepalive, and stop; and `keel.realtime.preflight`
// against a stubbed fetch: configured/unconfigured status, rates and model
// names passed through, the same-origin request shape, a network failure, an
// HTTP failure, a missing `fetch`, a status body that is not JSON, and the
// timeout — none of them leaking a URL or a key.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

const source = readFileSync(new URL("./host.js", import.meta.url), "utf8");
const hostModule = await import(`data:text/javascript;base64,${Buffer.from(source).toString("base64")}`);
const { createHost, providerRates, providerKindOf, resampleLinear, encodePcm16Base64, publicProviderEvent } =
  hostModule;

// ---- browser stubs --------------------------------------------------------

/** The sockets the current test created, in order. */
let sockets = [];
/** Everything the host sent, in order (parsed JSON). */
let sent = [];
/** The player events, in order. */
let events = [];
/** The next `getUserMedia` call: resolved by the test, or denied. */
let mic = { mode: "granted" };
let tracks = [];
/** The `fetch` stub for the current test (`null`: the request fails, which is
 * how a network failure looks to the host). */
let fetchStub = null;
/** Every `fetch(url, init)` the host made, in order. */
let fetchCalls = [];

class FakeSocket {
  constructor(url) {
    this.url = url;
    this.readyState = 0;
    this.closed = null;
    sockets.push(this);
    setTimeout(() => {
      this.readyState = 1;
      this.onopen?.();
    }, 0);
  }
  send(raw) {
    const payload = JSON.parse(raw);
    sent.push(payload);
    if (payload.type === "session.update" && !this.silent) {
      setTimeout(() => this.emit({ type: "session.created", session: { id: "srv" } }), 5);
    }
  }
  emit(event) {
    this.onmessage?.({ data: JSON.stringify(event) });
  }
  close(code, reason) {
    this.readyState = 3;
    this.closed = { code: code ?? 1000, reason: reason ?? "" };
    this.onclose?.({ code: code ?? 1000, reason: reason ?? "" });
  }
}

class FakeContext {
  constructor() {
    this.sampleRate = 48000;
    this.currentTime = 0;
    this.destination = {};
    this.sources = [];
    FakeContext.last = this;
  }
  resume() {
    return Promise.resolve();
  }
  createBuffer(channels, count, rate) {
    const data = new Float32Array(count);
    return { duration: count / rate, copyToChannel: (frames) => data.set(frames), data, rate };
  }
  createBufferSource() {
    const source = {
      buffer: null,
      started: null,
      stopped: false,
      connect() {},
      start: (at) => {
        source.started = at;
        this.sources.push(source);
      },
      stop: () => {
        source.stopped = true;
      },
      onended: null,
    };
    return source;
  }
  createMediaStreamSource() {
    return { connect() {}, disconnect() {} };
  }
  createScriptProcessor() {
    const processor = { connect() {}, disconnect() {}, onaudioprocess: null };
    FakeContext.processor = processor;
    return processor;
  }
  createGain() {
    return { gain: { value: 1 }, connect() {}, disconnect() {} };
  }
  close() {
    return Promise.resolve();
  }
}

const installGlobals = () => {
  Object.defineProperty(globalThis, "location", {
    configurable: true,
    value: { protocol: "http:", host: "127.0.0.1:4321", origin: "http://127.0.0.1:4321" },
  });
  Object.defineProperty(globalThis, "WebSocket", { configurable: true, writable: true, value: FakeSocket });
  Object.defineProperty(globalThis, "AudioContext", { configurable: true, writable: true, value: FakeContext });
  Object.defineProperty(globalThis, "fetch", {
    configurable: true,
    writable: true,
    value: (...args) => {
      fetchCalls.push(args);
      if (!fetchStub) return Promise.reject(new TypeError("fetch failed"));
      return fetchStub(...args);
    },
  });
  Object.defineProperty(globalThis, "navigator", {
    configurable: true,
    value: {
      onLine: true,
      mediaDevices: {
        getUserMedia: (constraints) => {
          if (mic.mode === "denied") {
            const error = new Error("denied");
            error.name = "NotAllowedError";
            return Promise.reject(error);
          }
          if (mic.mode === "deferred") {
            return new Promise((resolve) => {
              mic.resolve = () => {
                const track = { enabled: true, stop() { track.stopped = true; } };
                tracks = [track];
                resolve({ getTracks: () => tracks, constraints });
              };
            });
          }
          const track = { enabled: true, stop() { track.stopped = true; } };
          tracks = [track];
          return Promise.resolve({ getTracks: () => tracks, constraints });
        },
      },
    },
  });
};

const reset = () => {
  sockets = [];
  sent = [];
  events = [];
  tracks = [];
  mic = { mode: "granted" };
  fetchStub = null;
  fetchCalls = [];
  FakeContext.processor = null;
  installGlobals();
};

const makeHost = async () => {
  const player = { hostEvent: (name, payload) => events.push({ name, payload }) };
  return createHost(player);
};

const states = () => events.filter((e) => e.name === "keel.realtime.state").map((e) => e.payload);
const lastState = () => states()[states().length - 1];

const tick = (ms = 20) => new Promise((resolve) => setTimeout(resolve, ms));

// ---- pure helpers ---------------------------------------------------------

test("resampleLinear: a steady signal stays steady across buffer boundaries", () => {
  const state = { pos: 0, last: 0 };
  const first = new Float32Array(4096).fill(1);
  const second = new Float32Array(4096).fill(1);
  const a = resampleLinear(first, 48000, 16000, state);
  const b = resampleLinear(second, 48000, 16000, state);
  for (const sample of [...a, ...b]) assert.ok(Math.abs(sample - 1) < 1e-6, `expected 1, got ${sample}`);
});

test("resampleLinear: a ramp keeps its phase (no zero/dip at the seam)", () => {
  const state = { pos: 0, last: 0 };
  const make = (offset) => Float32Array.from({ length: 4096 }, (_, index) => (offset + index) / 4096);
  const a = resampleLinear(make(0), 48000, 16000, state);
  const b = resampleLinear(make(4096), 48000, 16000, state);
  const all = [...a, ...b];
  for (let index = 1; index < all.length; index += 1) {
    assert.ok(all[index] > all[index - 1] - 0.01, `dip at ${index}: ${all[index - 1]} -> ${all[index]}`);
  }
  // The seam value sits between the two frames it interpolates, never at 0.
  assert.ok(a[a.length - 1] > 0.9 && b[0] > 0.9, `seam: ${a[a.length - 1]} / ${b[0]}`);
});

test("encodePcm16Base64: little-endian PCM16, clamped", () => {
  const bytes = Buffer.from(encodePcm16Base64(Float32Array.from([0, 0.5, -0.5, 1, -1, 2])), "base64");
  const expected = Buffer.alloc(12);
  expected.writeInt16LE(0, 0);
  expected.writeInt16LE(16384, 2);
  // Math.round(-16383.5) is -16383 (JS rounds half toward +Infinity).
  expected.writeInt16LE(-16383, 4);
  expected.writeInt16LE(32767, 6);
  expected.writeInt16LE(-32767, 8);
  expected.writeInt16LE(32767, 10);
  assert.deepEqual(bytes, expected);
});

test("providerRates: Qwen defaults, OpenAI 24k/24k, explicit args win", () => {
  const qwen = providerRates({});
  assert.equal(qwen.kind, "qwen");
  assert.deepEqual([qwen.inputRate, qwen.outputRate], [16000, 24000]);
  assert.equal(qwen.voice, "longanlingxin");
  assert.equal(qwen.transcriptionModel, "qwen3-asr-flash-realtime");

  const openai = providerRates({ providerKind: "openai" });
  assert.deepEqual([openai.inputRate, openai.outputRate], [24000, 24000]);
  assert.equal(openai.voice, "alloy");
  assert.equal(openai.transcriptionModel, "whisper-1");

  const explicit = providerRates({ providerKind: "openai", inputSampleRate: 48000, outputSampleRate: 16000 });
  assert.deepEqual([explicit.inputRate, explicit.outputRate], [48000, 16000]);

  assert.equal(providerKindOf({ providerId: "openai" }), "openai");
  assert.equal(providerKindOf({ model: "gpt-4o-realtime-preview" }), "openai");
  assert.equal(providerKindOf({ model: "qwen3-omni-flash-realtime" }), "qwen");
  // The surface's descriptor kind ("realtime") is a family, not a vendor.
  assert.equal(providerKindOf({ providerKind: "realtime", model: "gpt-4o-realtime-preview" }), "openai");
  assert.equal(providerKindOf({ providerKind: "realtime", model: "qwen3-omni-flash-realtime" }), "qwen");
});

test("publicProviderEvent: audio bytes never cross the bridge", () => {
  const forwarded = publicProviderEvent({
    type: "response.audio.delta",
    response_id: "r1",
    item_id: "i1",
    delta: Buffer.alloc(2400).toString("base64"),
  });
  assert.equal(forwarded.type, "response.audio.delta");
  assert.equal(forwarded.delta, undefined);
  assert.equal(forwarded.audioStripped, true);
  assert.equal(forwarded.audioBytes, 2400);
  // Everything else is forwarded untouched.
  const transcript = { type: "response.audio_transcript.delta", delta: "hi" };
  assert.equal(publicProviderEvent(transcript), transcript);
});

// ---- keel.realtime.preflight ----------------------------------------------

/** A status response the way `fetch` resolves it. */
const statusResponse = (body, status = 200) => ({
  ok: status >= 200 && status < 300,
  status,
  json: async () => body,
});

test("preflight: a configured service answers kind, models and rates", async () => {
  reset();
  fetchStub = async () =>
    statusResponse({
      configured: true,
      kind: "qwen",
      models: ["qwen3-omni-flash-realtime", { id: "gpt-4o-realtime-preview" }],
      inputSampleRate: 16000,
      outputSampleRate: 24000,
      // Fields the app has no business seeing never cross the bridge.
      apiKey: "sk-should-not-leak",
    });
  const host = await makeHost();
  const answer = await host.call("keel.realtime.preflight", {
    sessionId: "s-9",
    model: "qwen3-omni-flash-realtime",
  });
  assert.deepEqual(answer, {
    ok: true,
    configured: true,
    kind: "qwen",
    models: ["qwen3-omni-flash-realtime", "gpt-4o-realtime-preview"],
    inputSampleRate: 16000,
    outputSampleRate: 24000,
  });
  // Same origin, credentials included, and nothing about the call in the URL.
  assert.equal(fetchCalls.length, 1);
  const [url, init] = fetchCalls[0];
  assert.equal(url, "/v1/realtime/status");
  assert.equal(init.method, "GET");
  assert.equal(init.credentials, "include");
  assert.equal(String(url).includes("s-9"), false, "the session id stays out of the URL");
  assert.ok(init.signal, "the request carries an abort signal");
});

test("preflight: an unconfigured service answers configured:false, no extra fields", async () => {
  reset();
  fetchStub = async () => statusResponse({ configured: false, models: [], inputSampleRate: null });
  const host = await makeHost();
  const answer = await host.call("keel.realtime.preflight", {});
  assert.deepEqual(answer, { ok: true, configured: false, models: [] });
  assert.deepEqual(Object.keys(answer).sort(), ["configured", "models", "ok"]);
});

test("preflight: a network failure is a stable code and a sentence with no URL or key", async () => {
  reset();
  fetchStub = async () => {
    // The shape a browser's rejection takes: the URL is in the message.
    throw new TypeError(
      "Failed to fetch: http://127.0.0.1:4321/v1/realtime/status?api_key=sk-secret-in-url",
    );
  };
  const host = await makeHost();
  const answer = await host.call("keel.realtime.preflight", { sessionId: "s-10", model: "m" });
  assert.equal(answer.ok, false);
  assert.equal(answer.code, "preflight-failed");
  assert.equal(typeof answer.message, "string");
  assert.ok(answer.message.length > 0);
  const text = JSON.stringify(answer);
  for (const leak of ["127.0.0.1", "4321", "http", "/v1/realtime", "api_key", "sk-secret", "s-10"]) {
    assert.equal(text.includes(leak), false, `the reply leaked ${leak}: ${text}`);
  }
});

test("preflight: an HTTP failure reports the status and nothing from the body", async () => {
  reset();
  fetchStub = async () => statusResponse({ error: "no realtime provider: key sk-live missing" }, 503);
  const host = await makeHost();
  const answer = await host.call("keel.realtime.preflight", {});
  assert.equal(answer.ok, false);
  assert.equal(answer.code, "preflight-failed");
  assert.match(answer.message, /503/);
  assert.equal(answer.message.includes("sk-live"), false, "the server body must not be echoed");
});

test("preflight: a body that is not a status document is a failed preflight, not 'unconfigured'", async () => {
  reset();
  // An SPA fallback page served for any path: 200, HTML, `json()` throws.
  fetchStub = async () => ({
    ok: true,
    status: 200,
    json: async () => {
      throw new SyntaxError("Unexpected token '<'");
    },
  });
  const host = await makeHost();
  const answer = await host.call("keel.realtime.preflight", {});
  assert.equal(answer.ok, false);
  assert.equal(answer.code, "preflight-failed");
  assert.equal(answer.configured, undefined, "an unknown answer must not claim 'unconfigured'");

  // Valid JSON, but not the status shape: same, the app falls through to start.
  fetchStub = async () => statusResponse({ ok: true });
  const second = await host.call("keel.realtime.preflight", {});
  assert.equal(second.ok, false);
  assert.equal(second.code, "preflight-failed");
  assert.equal(second.configured, undefined);
});

test("preflight: a host without fetch says unsupported", async () => {
  reset();
  globalThis.fetch = undefined;
  const host = await makeHost();
  const answer = await host.call("keel.realtime.preflight", {});
  assert.equal(answer.ok, false);
  assert.equal(answer.code, "unsupported");
  assert.equal(typeof answer.message, "string");
});

test("preflight: a status request that never answers is cut off at the timeout", async (t) => {
  reset();
  t.mock.timers.enable({ apis: ["setTimeout"] });
  let aborted = false;
  fetchStub = (_url, init) =>
    new Promise((_resolve, reject) => {
      init.signal.addEventListener("abort", () => {
        aborted = true;
        const error = new Error("The operation was aborted");
        error.name = "AbortError";
        reject(error);
      });
    });
  const host = await makeHost();
  const pending = host.call("keel.realtime.preflight", {});
  // Let the request start before the clock moves.
  await Promise.resolve();
  await Promise.resolve();
  t.mock.timers.tick(3999);
  assert.equal(aborted, false, "the bound is not premature");
  t.mock.timers.tick(1);
  const answer = await pending;
  assert.equal(aborted, true, "the request is aborted at the bound");
  assert.equal(answer.ok, false);
  assert.equal(answer.code, "preflight-failed");
  assert.match(answer.message, /超时/);
  t.mock.timers.reset();
});

// ---- one live session -----------------------------------------------------

test("session: Qwen session.update (nested 16k/24k), sent before the mic prompt resolves", async () => {
  reset();
  mic = { mode: "deferred" };
  const host = await makeHost();
  const started = host.call("keel.realtime.start", {
    sessionId: "s-1",
    model: "qwen3-omni-flash-realtime",
    providerId: "qwen",
  });
  await tick(30);
  // N3: the provider is configured while the permission prompt is still open.
  const update = sent.find((payload) => payload.type === "session.update");
  assert.ok(update, "session.update must be sent before the microphone resolves");
  const session = update.session;
  assert.deepEqual(session.modalities, ["text", "audio"]);
  assert.equal(session.audio.input.format.sample_rate, 16000);
  assert.equal(session.audio.input.format.sample_format, "s16le");
  assert.equal(session.audio.input.format.channels, 1);
  assert.equal(session.audio.output.format.sample_rate, 24000);
  assert.equal(session.audio.output.voice, "longanlingxin");
  assert.equal(session.input_audio_transcription.model, "qwen3-asr-flash-realtime");
  assert.equal(session.turn_detection.type, "server_vad");
  assert.equal(session.turn_detection.silence_duration_ms, 700);

  mic.resolve();
  const reply = await started;
  assert.deepEqual(reply, { ok: true, mic: "granted", message: "" });
  // No invented keepalive event.
  assert.equal(sent.some((payload) => payload.type === "session.ping"), false);
  await host.call("keel.realtime.stop", {});
});

test("session: OpenAI takes 24k/24k from providerKind", async () => {
  reset();
  const host = await makeHost();
  const reply = await host.call("keel.realtime.start", {
    sessionId: "s-2",
    model: "gpt-4o-realtime-preview",
    providerKind: "openai",
  });
  assert.equal(reply.ok, true);
  const session = sent.find((payload) => payload.type === "session.update").session;
  assert.deepEqual(session.audio.input.format, { type: "audio/pcm", rate: 24000 });
  assert.deepEqual(session.audio.output.format, { type: "audio/pcm", rate: 24000 });
  assert.equal(session.voice, "alloy");
  assert.equal(session.input_audio_transcription.model, "whisper-1");
  await host.call("keel.realtime.stop", {});
});

test("session: mic PCM is appended continuously and keeps its level across buffers", async () => {
  reset();
  const host = await makeHost();
  await host.call("keel.realtime.start", { sessionId: "s-3", model: "qwen3-omni-flash-realtime" });
  const processor = FakeContext.processor;
  assert.ok(processor?.onaudioprocess, "the capture graph is wired");
  const feed = () =>
    processor.onaudioprocess({ inputBuffer: { getChannelData: () => new Float32Array(4096).fill(0.5) } });
  feed();
  feed();
  const appends = sent.filter((payload) => payload.type === "input_audio_buffer.append");
  assert.equal(appends.length, 2);
  const pcm = Buffer.from(appends[1].audio, "base64");
  // 4096 frames at 48 kHz → ~1365 at 16 kHz ≈ 2730 bytes, and every frame is
  // the DC value (a zero at the seam would show as a 0 sample here).
  assert.ok(pcm.length > 2600 && pcm.length < 2800, `bytes: ${pcm.length}`);
  let zeros = 0;
  for (let index = 0; index < pcm.length; index += 2) if (pcm.readInt16LE(index) === 0) zeros += 1;
  assert.ok(zeros <= 1, `unexpected seams: ${zeros} zero samples`);
  await host.call("keel.realtime.stop", {});
});

test("session: barge-in stops audio and late deltas neither play nor flip the state", async () => {
  reset();
  const host = await makeHost();
  await host.call("keel.realtime.start", { sessionId: "s-4", model: "qwen3-omni-flash-realtime" });
  const socket = sockets[sockets.length - 1];
  const context = FakeContext.last;
  const delta = Buffer.alloc(480).toString("base64");

  socket.emit({ type: "response.created", response: { id: "r1" } });
  socket.emit({ type: "response.audio.delta", response_id: "r1", delta });
  assert.equal(context.sources.length, 1);
  // The forwarded event carries no base64.
  const forwarded = events
    .filter((entry) => entry.name === "keel.realtime.event")
    .map((entry) => entry.payload.event)
    .filter((event) => event.type === "response.audio.delta");
  assert.equal(forwarded.length, 1);
  assert.equal(forwarded[0].delta, undefined);
  assert.equal(forwarded[0].audioBytes, 480);

  const beforeBargeIn = states().length;
  socket.emit({ type: "input_audio_buffer.speech_started" });
  assert.equal(context.sources[0].stopped, true);
  assert.equal(sent.some((payload) => payload.type === "response.cancel"), true);
  const interrupted = lastState();
  assert.equal(interrupted.phase, "interrupted");

  // A delta of the cancelled response arrives late: it must not play and must
  // not move the state back to "speaking".
  socket.emit({ type: "response.audio.delta", response_id: "r1", delta });
  assert.equal(context.sources.length, 1, "late delta must not be scheduled");
  assert.equal(lastState().phase, "interrupted");
  assert.equal(
    states().slice(beforeBargeIn).some((state) => state.phase === "speaking"),
    false,
    "a cancelled turn must not re-enter speaking",
  );

  // The next response lifts the gate and plays normally.
  socket.emit({ type: "response.created", response: { id: "r2" } });
  socket.emit({ type: "response.audio.delta", response_id: "r2", delta });
  assert.equal(context.sources.length, 2);
  await host.call("keel.realtime.stop", {});
});

test("session: a fatal provider close is reported once and not retried", async () => {
  reset();
  const host = await makeHost();
  await host.call("keel.realtime.start", { sessionId: "s-5", model: "qwen3-omni-flash-realtime" });
  const before = sockets.length;
  sockets[sockets.length - 1].close(4401, "realtime provider is not configured");
  await tick(60);
  assert.equal(sockets.length, before, "no reconnect storm");
  const error = states().find((state) => state.phase === "error");
  assert.ok(error && /not configured/.test(error.detail));
  assert.equal(states().some((state) => state.phase === "reconnecting"), false);
  assert.equal(lastState().phase, "closed");
});

test("session: stop releases the microphone, the socket and the context", async () => {
  reset();
  const host = await makeHost();
  await host.call("keel.realtime.start", { sessionId: "s-6", model: "qwen3-omni-flash-realtime" });
  const socket = sockets[sockets.length - 1];
  const stop = await host.call("keel.realtime.stop", {});
  assert.deepEqual(stop, { ok: true, wasLive: true });
  assert.equal(socket.readyState, 3);
  assert.equal(tracks[0].stopped, true);
  assert.equal(lastState().phase, "closed");
});
