// TUI/headless gate · realtime (duplex) surface — change keel-session-surfaces-v1 §5.
//
// Two hosts, one surface:
//   · headless (`nsh test <file>`): the test plays the web page host
//     (`host.handlers`), so `keel.realtime.start/stop/send/mute` are answered
//     for real and `app.host.send` delivers the events the browser bridge
//     would send (provider JSON, state, levels). Every duplex assertion runs
//     in this branch.
//   · terminal (`nsh test --target tui`): the terminal has no `host` at all
//     (`host.available()` is false, `host.handlers` is rejected with
//     `ERR_NSH_TEST_UNSUPPORTED`), so the same surface must land on the honest
//     "宿主不支持" state and keep its controls — the spec's degradation path.
//
// The manual clock (`clock: {}`) makes the surface's own timers deterministic:
// `waitFor` moves it a frame at a time.
import { expect, open, test } from "nsh/test";

type App = Awaited<ReturnType<typeof open>>;

const HANDLERS = {
  // Preflight answers "configured" so the call reaches start; the tests that
  // need the unconfigured state override it.
  "keel.realtime.preflight": { ok: true, configured: true, kind: "qwen" },
  "keel.realtime.start": { ok: true, mic: "granted", message: "" },
  "keel.realtime.stop": { ok: true, wasLive: true },
  "keel.realtime.send": { ok: true },
  "keel.realtime.mute": { ok: true, muted: true },
};

/** Opens the harness with the fake web host. The terminal target refuses
 * `host.handlers`, so it opens without one and the test takes the honest
 * unsupported branch instead. */
const launch = async (
  options: { width?: number; params?: Record<string, unknown>; start?: unknown; preflight?: unknown } = {},
) => {
  const handlers = {
    ...HANDLERS,
    ...(options.start === undefined ? {} : { "keel.realtime.start": options.start }),
    ...(options.preflight === undefined ? {} : { "keel.realtime.preflight": options.preflight }),
  };
  try {
    const app = await open("./harness.tsx", {
      clock: {},
      ...(options.width ? { width: options.width } : {}),
      host: { params: options.params ?? {}, handlers },
    });
    return { app, hosted: true as const };
  } catch (error) {
    const code = String((error as { code?: unknown } | null)?.code ?? "");
    if (code !== "ERR_NSH_TEST_UNSUPPORTED") throw error;
    const app = await open("./harness.tsx", {
      clock: {},
      ...(options.width ? { width: options.width } : {}),
    });
    return { app, hosted: false as const };
  }
};

const sendEvent = (app: App, event: Record<string, unknown>) =>
  app.host.send("keel.realtime.event", { event });

const sendState = (app: App, phase: string, detail = "") =>
  app.host.send("keel.realtime.state", { phase, detail });

const startCall = async (app: App) => {
  await app.press("id:realtime-primary");
  await app.waitFor("text:/就绪/");
};

/** The terminal branch: no browser audio bridge, and the surface says so
 * rather than simulating a conversation. */
const unsupportedBranch = async (app: App) => {
  await app.waitFor("id:realtime-primary");
  expect(app.texts()).toContain("还没有转写");
  await app.press("id:realtime-primary");
  await app.waitFor("text:/宿主不支持/");
  expect(app.texts().join("\n")).toContain("仅 web 构建带浏览器音频桥");
  // No session exists, so the text fallback is not pretending to send.
  expect(app.enabled("id:realtime-text-input")).toBe(false);
  expect(app.host.calls()).toHaveLength(0);
};

test("实时语音：开始通话走 host 桥，流式转写只落一条 final", async () => {
  const { app, hosted } = await launch();
  if (!hosted) {
    await unsupportedBranch(app);
    return;
  }
  await app.waitFor("id:realtime-primary");
  expect(app.texts()).toContain("还没有转写");
  // Exactly one primary action: the empty state explains, it does not carry a
  // second 开始实时对话.
  expect(app.shows("id:realtime-empty-start")).toBe(false);
  expect(app.texts().filter((text) => text === "开始实时对话")).toHaveLength(1);

  await startCall(app);
  const starts = app.host.calls().filter((call) => call.name === "keel.realtime.start");
  expect(starts).toHaveLength(1);
  expect(starts[0]!.data).toMatchObject({
    sessionId: "6f1c2d3e-4a5b-4c6d-8e7f-901234567890",
    model: "qwen3-omni-flash-realtime",
    providerId: "qwen",
    // The preflight's handshake kind wins over the registry fallback: it is the
    // one answer that knows what the configured service actually speaks.
    providerKind: "qwen",
  });

  // The timer is part of the state machine: it only exists once a session is
  // up, and it moves.
  expect(app.texts()).toContain("00:00");
  await app.clock!.runFor(3200);
  expect(app.texts()).toContain("00:03");

  // The host reports the live phase, then the provider's transcript events.
  await sendState(app, "listening", "正在听");
  await sendEvent(app, { type: "conversation.item.input_audio_transcription.delta", delta: "上午" });
  await sendEvent(app, { type: "conversation.item.input_audio_transcription.delta", delta: "十点提醒我" });
  await app.waitFor("text:/转写中（仅界面）/");
  expect(app.texts().filter((text) => text === "上午十点提醒我")).toHaveLength(1);

  await sendEvent(app, {
    type: "conversation.item.input_audio_transcription.completed",
    transcript: "上午十点提醒我",
  });
  await app.waitFor("text:/实时转写/");
  expect(app.texts()).not.toContain("转写中（仅界面）");
  // The panel is honest about durability: this build keeps the transcript in
  // the page only, and says so without raising an alarm.
  expect(app.texts()).toContain("本构建的实时转写只保留在当前页面；刷新后不会从账本恢复。");
  // The final replaces the partial row: still exactly one user row.
  expect(app.texts().filter((text) => text === "上午十点提醒我")).toHaveLength(1);

  // The assistant streams a partial, then a final that replaces it in place.
  await sendEvent(app, { type: "response.created" });
  await sendEvent(app, { type: "response.audio_transcript.delta", delta: "好的，" });
  await sendEvent(app, { type: "response.audio_transcript.delta", delta: "已安排" });
  await app.waitFor("text:/说话中/");
  expect(app.texts()).toContain("好的，已安排");

  await sendEvent(app, { type: "response.audio_transcript.done", transcript: "好的，已安排。" });
  await sendEvent(app, { type: "response.done", response: { status: "completed" } });
  await app.waitFor("text:/已完成/");
  expect(app.texts()).toContain("好的，已安排。");
  // The partial text is gone: one row, replaced, never duplicated.
  expect(app.texts().filter((text) => text === "好的，已安排")).toHaveLength(0);
  expect(app.texts().filter((text) => text === "好的，已安排。")).toHaveLength(1);
});

test("实时语音：连接中如实显示阶段，未连接就没有转写", async () => {
  // The start answer is late (1.5s on the manual clock): the surface stays in
  // its connecting state meanwhile instead of inventing a session or a row.
  const { app, hosted } = await launch({
    start: () =>
      new Promise((resolve) => {
        setTimeout(() => resolve({ ok: true, mic: "granted", message: "" }), 1500);
      }),
  });
  if (!hosted) {
    await unsupportedBranch(app);
    return;
  }
  await app.press("id:realtime-primary");
  await app.waitFor("text:/连接中/");
  expect(app.texts().join("\n")).toContain("正在连接 qwen3-omni-flash-realtime");
  expect(app.texts()).toContain("还没有转写");
  expect(app.texts()).not.toContain("已完成");
  expect(app.shows("id:realtime-elapsed")).toBe(false);
  expect(app.host.calls().filter((call) => call.name === "keel.realtime.start")).toHaveLength(1);
  // No transcript exists until the service speaks: the text fallback stays off.
  expect(app.enabled("id:realtime-text-input")).toBe(false);

  // The late answer lands: the call becomes real, and only then is text usable.
  await app.clock!.runFor(1600);
  await app.waitFor("text:/就绪/");
  expect(app.enabled("id:realtime-text-input")).toBe(true);
});

test("实时语音：打断立即标记 interrupted，后续 done 不会补成完整回复", async () => {
  const { app, hosted } = await launch();
  if (!hosted) {
    await unsupportedBranch(app);
    return;
  }
  await startCall(app);
  await sendEvent(app, { type: "response.created" });
  await sendEvent(app, { type: "response.audio_transcript.delta", delta: "我查一下明天的安排" });
  await sendEvent(app, { type: "response.audio.delta", delta: "" });
  await app.waitFor("text:/语音播放中/");

  // Server VAD hears the user: the host stops audio and cancels the response.
  await sendEvent(app, { type: "input_audio_buffer.speech_started" });
  await app.waitFor("text:/已打断/");
  expect(app.texts()).toContain("已停止生成与播放，不会补全成本轮完整回复。");
  expect(app.texts()).toContain("播放已停止");

  // Late events of the cancelled turn never open a second, "complete" row.
  await sendEvent(app, { type: "response.audio_transcript.done", transcript: "我查一下明天的安排。" });
  await sendEvent(app, { type: "response.done", response: { status: "completed" } });
  await app.clock!.runFor(2000);
  expect(app.texts()).not.toContain("已完成");
  expect(app.texts().filter((text) => text.startsWith("我查一下明天的安排"))).toHaveLength(1);
  // A cancelled done is also fine, and keeps the same row.
  await sendEvent(app, { type: "response.done", response: { status: "cancelled" } });
  expect(app.texts().filter((text) => text.startsWith("我查一下明天的安排"))).toHaveLength(1);
});

test("实时语音：手动打断后 cancelled done 不清守卫，迟到的完整回复不落行", async () => {
  const { app, hosted } = await launch();
  if (!hosted) {
    await unsupportedBranch(app);
    return;
  }
  await startCall(app);

  // One full turn first: the guard is clean when the text turn starts.
  await sendEvent(app, { type: "response.created" });
  await sendEvent(app, { type: "response.audio_transcript.delta", delta: "第一轮回复" });
  await sendEvent(app, { type: "response.audio_transcript.done", transcript: "第一轮回复。" });
  await sendEvent(app, { type: "response.done", response: { status: "completed" } });
  await app.waitFor("text:/已完成/");

  // A text turn, streaming, then the reader presses 打断: the local interrupt
  // clears `responseLive` before the provider answers, so the provider's
  // cancelled done arrives with no live response to mark.
  await app.type("id:realtime-text-input", "再帮我查一下");
  await app.press("id:realtime-text-send");
  await app.waitFor("text:/再帮我查一下/");
  await sendEvent(app, { type: "response.created" });
  await sendEvent(app, { type: "response.audio_transcript.delta", delta: "我再看一下" });
  await app.press("label:打断助手");
  await app.waitFor("text:/已手动打断/");

  // The provider reports the cancellation, then 350ms later the late
  // transcript/done of the same cancelled response arrives.
  await sendEvent(app, { type: "response.done", response: { status: "cancelled" } });
  await app.clock!.runFor(350);
  await sendEvent(app, {
    type: "response.audio_transcript.done",
    transcript: "我再看一下，迟到的完整回复。",
  });
  await sendEvent(app, { type: "response.done", response: { status: "completed" } });
  await app.clock!.runFor(500);

  // The interrupted row stays the only row of that turn: the late transcript
  // neither opens a second row nor retroactively completes this one.
  expect(app.count("id:realtime-assistant-row")).toBe(2);
  expect(app.texts().filter((text) => text === "已完成")).toHaveLength(1);
  expect(app.texts()).toContain("已打断");
  expect(app.texts().filter((text) => text === "我再看一下")).toHaveLength(1);
  expect(app.texts()).not.toContain("我再看一下，迟到的完整回复。");
});

test("实时语音：一个原因只落一条系统行（状态事件与请求回复不重复）", async () => {
  const { app, hosted } = await launch();
  if (!hosted) {
    await unsupportedBranch(app);
    return;
  }
  await startCall(app);
  await sendEvent(app, { type: "error", error: { message: "连接被重置" } });
  await sendState(app, "error", "连接被重置");
  await app.waitFor("text:/出错/");
  expect(app.texts().filter((text) => text.startsWith("连接被重置"))).toHaveLength(1);
  expect(app.texts().filter((text) => text.includes("实时会话错误"))).toHaveLength(1);
});

test("实时语音：结束通话调用 host.stop，麦克风状态回到未开启", async () => {
  const { app, hosted } = await launch();
  if (!hosted) {
    await unsupportedBranch(app);
    return;
  }
  await startCall(app);
  expect(app.texts()).toContain("麦克风开启 · 持续收音");
  await app.press("label:结束通话");
  await app.waitFor("text:/已结束/");
  const stops = app.host.calls().filter((call) => call.name === "keel.realtime.stop");
  expect(stops.length).toBeGreaterThan(0);
  expect(app.texts()).toContain("麦克风未开启");
  expect(app.enabled("id:realtime-text-input")).toBe(false);
  // The clock is a call fact: it goes away with the call.
  expect(app.shows("id:realtime-elapsed")).toBe(false);
});

test("实时语音：麦克风被拒 → 权限态，文字兜底仍发到同一个实时会话", async () => {
  const { app, hosted } = await launch({
    start: {
      ok: true,
      mic: "not-allowed",
      message: "麦克风权限被拒绝：可以继续用文字和助手对话，助手语音仍会播放",
    },
  });
  if (!hosted) {
    await unsupportedBranch(app);
    return;
  }
  await app.press("id:realtime-primary");
  await app.waitFor("text:/麦克风未授权/");
  expect(app.texts().join("\n")).toContain("可以继续用文字和助手对话");
  expect(app.enabled("id:realtime-text-input")).toBe(true);

  await app.type("id:realtime-text-input", "用文字改约时间");
  await app.press("id:realtime-text-send");
  await app.waitFor("text:/用文字改约时间/");
  const sends = app.host.calls().filter((call) => call.name === "keel.realtime.send");
  expect(sends).toHaveLength(2);
  expect(sends[0]!.data).toMatchObject({
    payload: { type: "conversation.item.create", item: { role: "user" } },
  });
  expect(sends[1]!.data).toMatchObject({ payload: { type: "response.create" } });
});

test("实时语音：静音调用 host.mute，打断按钮发 response.cancel", async () => {
  const { app, hosted } = await launch();
  if (!hosted) {
    await unsupportedBranch(app);
    return;
  }
  await startCall(app);
  await app.press("label:静音");
  await app.waitFor("text:/麦克风已静音/");
  const mutes = app.host.calls().filter((call) => call.name === "keel.realtime.mute");
  expect(mutes).toHaveLength(1);
  expect(mutes[0]!.data).toMatchObject({ muted: true });

  await sendEvent(app, { type: "response.created" });
  await sendEvent(app, { type: "response.audio_transcript.delta", delta: "正在说" });
  await app.press("label:打断助手");
  await app.waitFor("text:/已手动打断/");
  const cancels = app.host.calls().filter(
    (call) =>
      call.name === "keel.realtime.send" &&
      (call.data as { payload?: { type?: string } })?.payload?.type === "response.cancel",
  );
  expect(cancels).toHaveLength(1);
  expect(app.texts()).toContain("已停止生成与播放，不会补全成本轮完整回复。");
});

test("实时语音：没有实时语音模型时主控禁用，配置入口可点且不静默失败", async () => {
  const { app, hosted } = await launch({ params: { noModel: true } });
  if (!hosted) {
    await unsupportedBranch(app);
    return;
  }
  // Blocked before any press: the primary is not bright and enabled-looking.
  await app.waitFor("text:/未配置语音模型/");
  expect(app.enabled("id:realtime-primary")).toBe(false);
  expect(app.texts().join("\n")).toContain("没有可用的实时语音模型");
  expect(app.texts()).toContain("还没有转写");
  expect(app.enabled("id:realtime-text-input")).toBe(false);

  // The way out is a real control, and pressing it answers in place.
  expect(app.shows("id:realtime-open-settings")).toBe(true);
  await app.press("id:realtime-open-settings");
  await app.waitFor("text:/设置 › 模型/");
  expect(app.texts().join("\n")).toContain("已打开「设置 › 模型」页");
  // Nothing was sent to the bridge.
  expect(app.host.calls().filter((call) => call.name === "keel.realtime.start")).toHaveLength(0);
});

test("实时语音：没有打开会话时不会编造会话 id", async () => {
  const { app, hosted } = await launch({ params: { noSession: true } });
  if (!hosted) {
    await unsupportedBranch(app);
    return;
  }
  await app.waitFor("text:/未打开会话/");
  expect(app.enabled("id:realtime-primary")).toBe(false);
  expect(app.texts().join("\n")).toContain("先打开一个会话");
  expect(app.host.calls().filter((call) => call.name === "keel.realtime.start")).toHaveLength(0);
});

test("实时语音：预检未配置时不开 WebSocket，直接落在配置态", async () => {
  const { app, hosted } = await launch({
    preflight: { ok: true, configured: false, models: [] },
  });
  if (!hosted) {
    await unsupportedBranch(app);
    return;
  }
  await app.press("id:realtime-primary");
  await app.waitFor("id:realtime-open-settings");
  const joined = app.texts().join("\n");
  expect(joined).toContain("未配置");
  expect(joined).not.toContain("1006");
  expect(joined).not.toContain("实时会话没有建立");
  // The preflight answered first: no call was attempted.
  expect(app.host.calls().some((call) => call.name === "keel.realtime.start")).toBe(false);
  expect(app.shows("id:realtime-open-settings")).toBe(true);
});

test("实时语音：390px 下主控、静音/打断、文字兜底与电平都在", async () => {
  const { app, hosted } = await launch({ width: 390 });
  await app.waitFor("id:realtime-primary");
  expect(app.shows("id:realtime-primary")).toBe(true);
  expect(app.shows("id:realtime-text-input")).toBe(true);
  expect(app.shows("id:realtime-text-send")).toBe(true);
  if (!hosted) {
    await unsupportedBranch(app);
    return;
  }
  await startCall(app);
  expect(app.shows("id:realtime-mute")).toBe(true);
  expect(app.shows("id:realtime-interrupt")).toBe(true);
  expect(app.shows("id:realtime-input-level")).toBe(true);
  expect(app.shows("id:realtime-call-timer")).toBe(true);
  // The stacked composer keeps one row per control: no horizontal overflow.
  expect(app.shows("id:realtime-composer")).toBe(true);
});
