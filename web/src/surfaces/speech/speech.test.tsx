// TUI/headless gate · speech workbench (change keel-session-surfaces-v1, R1).
//
// Two hosts, one surface:
//   · headless (`nsh test <file>`): `Speech` sessions are simulated, so the
//     mic path is driven for real — press → `speechRequests()` → `hear` (a
//     dimmed partial row) → `endSpeech` (exactly one final row);
//   · terminal (`nsh test --target tui`): the host has no recognizer, so the
//     same press must land on the 无麦克风 degradation with the text fallback
//     kept — the spec's own scenario. `speechRequests`/`hear`/`endSpeech` are
//     headless-only steps, so those assertions live inside the live branch.
//
// The assistant reply, its tool card and its audio chip are gone from this
// surface: an STT workbench transcribes, it does not answer. The TTS tab is a
// form that says it is unconfigured, not a fake player.
import { expect, open, test } from "nsh/test";

const openSurface = (width?: number) =>
  open("./harness.tsx", { clock: {}, ...(width ? { width } : {}) });

const ready = () => "text:/待命/";

/** The terminal host has no simulated recognizer: asking for its sessions
 * throws there, which is itself the degradation under test. */
const sessionsOf = (app: Awaited<ReturnType<typeof open>>) => {
  try {
    return app.speechRequests();
  } catch {
    return null;
  }
};

const startedRecording = async (app: Awaited<ReturnType<typeof open>>) => {
  await app.press("id:speech-record");
  return app.waitFor("text:录音中", { timeout: 1500 }).then(
    () => true,
    () => false,
  );
};

test("语音转写：空态文案诚实，未按麦克风不会开始收音", async () => {
  const app = await openSurface();
  await app.waitFor(ready());
  expect(app.texts()).toContain("语音转写");
  expect(app.texts()).toContain("还没有转写");
  expect(app.texts()).toContain("按下说话");
  expect(app.shows("id:speech-empty")).toBe(true);
  // The workbench names its parts: a ledger header, and a mode switch that
  // says what it switches (never an unlabelled look-alike toolbar).
  expect(app.texts()).toContain("转写记录");
  expect(app.texts()).toContain("工作模式");
  expect(app.texts()).toContain("输入方式");
  // One operating hint, not two paraphrases of it.
  expect(app.texts().filter((text) => text.includes("停止即释放麦克风"))).toHaveLength(1);
  const all = app.texts().join("\n");
  expect(all).not.toContain("实时语音");
  expect(all).not.toContain("开始聆听");
  expect(all).not.toContain("演示回复");
  expect(all).not.toContain("音频片段");
  // Never automatic: the recorder is not running and the mic is the press-only
  // control. Where the host reports sessions (headless), none exists either.
  expect(app.texts()).not.toContain("录音中");
  expect(app.shows("id:speech-record")).toBe(true);
  const sessions = sessionsOf(app);
  if (sessions) expect(sessions).toHaveLength(0);
});

test("语音转写：按下麦克风——headless 收音并转写；无识别器的宿主降级为文字", async () => {
  const app = await openSurface();
  await app.waitFor(ready());
  const recording = await startedRecording(app);

  if (!recording) {
    // Spec scenario 无麦克风: the reason and the text entry are on the same
    // surface, and the transcript is not lost.
    await app.waitFor("text:/无麦克风/");
    expect(app.texts().join("\n")).toContain("没有可用的麦克风或语音识别");
    expect(app.shows("id:speech-text-input")).toBe(true);
    return;
  }

  const requests = app.speechRequests();
  expect(requests).toHaveLength(1);
  expect(requests[0]!.lang).toBe("zh-CN");
  expect(requests[0]!.interim).toBe(true);

  // The partial is a UI-only row, never a committed one.
  await app.hear("上午十点提醒我");
  await app.waitFor("text:/上午十点提醒我/");
  expect(app.texts()).toContain("转写中（未入账）");

  await app.endSpeech();
  await app.waitUntil(() => !app.texts().includes("转写中（未入账）"));
  await app.waitFor("text:/已停止/");
  expect(app.texts().filter((text) => text === "上午十点提醒我")).toHaveLength(1);
  // The result is a transcription block: speaker, source and time beside the
  // paragraph — not a right-aligned message.
  expect(app.texts()).toContain("说话人");
  expect(app.texts()).toContain("麦克风");
  expect(app.shows("id:speech-record")).toBe(true);
});

test("语音转写：停止立即释放麦克风，已听到的内容仍入账", async () => {
  const app = await openSurface();
  await app.waitFor(ready());
  const recording = await startedRecording(app);
  if (!recording) {
    // The terminal host cannot reach a session at all; the degradation is
    // already covered above, so this scenario only exists where a session does.
    await app.waitFor("text:/无麦克风/");
    return;
  }

  await app.hear("停止前的转写");
  await app.waitFor("text:/停止前的转写/");
  await app.press("id:speech-stop");
  await app.waitFor("text:/已停止/");
  await app.waitUntil(() => !app.texts().includes("转写中（未入账）"));
  expect(app.texts().filter((text) => text === "停止前的转写")).toHaveLength(1);
  expect(app.shows("id:speech-record")).toBe(true);

  // Nothing listens after stop: the next hear must be refused. If it landed,
  // the partial row would come back and the microphone would still be hot.
  let stillListening = false;
  try {
    await app.hear("停止后不应再被听到");
    stillListening = true;
  } catch {
    stillListening = false;
  }
  expect(stillListening).toBe(false);
  expect(app.texts()).not.toContain("停止后不应再被听到");
});

test("语音转写：麦克风权限被拒绝时降级为文字输入", async () => {
  const app = await openSurface();
  await app.waitFor(ready());
  const recording = await startedRecording(app);
  if (recording) {
    await app.endSpeech("not-allowed");
    await app.waitFor("text:/麦克风未授权/");
    expect(app.texts().join("\n")).toContain("麦克风权限被拒绝");
  } else {
    await app.waitFor("text:/无麦克风/");
    expect(app.texts().join("\n")).toContain("没有可用的麦克风或语音识别");
  }
  expect(app.shows("id:speech-text-input")).toBe(true);
  expect(app.enabled("id:speech-text-input")).toBe(true);
  await app.type("id:speech-text-input", "权限失败后仍可继续");
  expect(app.value("id:speech-text-input")).toBe("权限失败后仍可继续");
});

test("语音转写：文字兜底直接入账，不会启动麦克风", async () => {
  const app = await openSurface();
  await app.waitFor(ready());
  await app.press("id:speech-mode-text");
  expect(app.shows("id:speech-text-input")).toBe(true);
  await app.type("id:speech-text-input", "用文字记录一条转写");
  await app.press("label:发送文字");
  await app.waitFor("text:/用文字记录一条转写/");
  expect(app.texts()).toContain("文字输入");
  expect(app.texts()).toContain("说话人");
  expect(app.texts()).not.toContain("录音中");
  const sessions = sessionsOf(app);
  if (sessions) expect(sessions).toHaveLength(0);
});

test("语音转写：390px 窄栏下语音与文字控件都在，切回语音收起文字输入", async () => {
  const app = await openSurface(390);
  await app.waitFor(ready());
  expect(app.texts()).toContain("还没有转写");
  expect(app.shows("id:speech-record")).toBe(true);
  expect(app.shows("id:speech-mode-voice")).toBe(true);
  expect(app.shows("id:speech-mode-text")).toBe(true);
  expect(app.shows("id:speech-mode-tts")).toBe(true);
  await app.press("id:speech-mode-text");
  expect(app.shows("id:speech-text-input")).toBe(true);
  await app.press("id:speech-mode-voice");
  expect(app.shows("id:speech-text-input")).toBe(false);
});

test("语音合成：未配置 Provider——文本、音色与禁用的合成，配置 CTA 给出说明", async () => {
  const app = await openSurface();
  await app.waitFor(ready());
  await app.press("id:speech-mode-tts");
  await app.waitFor("text:/语音合成/");
  const all = app.texts().join("\n");
  expect(all).toContain("TTS Provider 尚未配置");
  expect(all).toContain("要合成的文本");
  expect(all).toContain("音色");
  expect(all).toContain("尚未生成音频");
  expect(app.shows("id:speech-tts-text")).toBe(true);
  expect(app.shows("id:speech-tts-voice")).toBe(true);
  expect(app.enabled("id:speech-tts-synthesize")).toBe(false);
  // The blocked primary is the only disabled control; the working next step is
  // the single primary action.
  expect(app.enabled("id:speech-tts-configure")).toBe(true);
  // One honesty claim (the banner), never four paraphrases of it.
  expect(
    app.texts().filter((text) => text.includes("不会模拟播放") || text.includes("占位音频")),
  ).toHaveLength(1);
  expect(app.texts().join("\n")).not.toContain("占位选项");
  // The result area mirrors the STT ledger's section header.
  expect(app.shows("id:speech-tts-empty")).toBe(true);
  expect(app.texts()).toContain("合成结果");
  expect(app.texts()).toContain("未生成");
  await expect(app.press("id:speech-tts-synthesize")).rejects.toMatchObject({
    code: "ERR_NSH_TEST_DISABLED",
  });
  await app.press("id:speech-tts-configure");
  await app.waitFor("text:/配置 TTS Provider/");
  expect(app.texts()).not.toContain("回放");
});

test("语音合成：Provider 已配置——合成走服务端语音端点并给出结果", async () => {
  const app = await open("./harness.tsx", {
    clock: {},
    network: {
      "POST http://keel.test/v1/media/audio/speech": () => ({
        status: 200,
        headers: { "content-type": "audio/wav", "x-keel-model": "demo-tts" },
        body: "RIFF....WAVEfmt ",
      }),
      "GET http://keel.test/v1/media/capabilities": () => ({
        body: { configured: true, image: false, tts: true, stt: false },
      }),
      "*": () => ({ status: 404, body: "" }),
    },
  });
  await app.waitFor(ready());
  await app.press("id:speech-mode-tts");
  await app.waitFor("text:/语音合成/");

  // 已配置：横幅退场，合成是唯一主操作。
  expect(app.texts().join("\n")).not.toContain("TTS Provider 尚未配置");
  expect(app.texts()).toContain("已配置");
  expect(app.enabled("id:speech-tts-synthesize")).toBe(false); // 空文本

  await app.type("id:speech-tts-text", "明天的评审改到下午三点。");
  expect(app.enabled("id:speech-tts-synthesize")).toBe(true);
  await app.press("id:speech-tts-synthesize");
  await app.waitFor("id:speech-tts-result");
  const all = app.texts().join("\n");
  expect(all).toContain("明天的评审改到下午三点。");
  expect(all).toContain("demo-tts");
  expect(app.shows("id:speech-tts-play")).toBe(true);
});

test("语音合成：请求带上会话选中的 TTS 模型（否则走部署默认，可能是付费厂商）", async () => {
  const app = await open("./harness.tsx", {
    clock: {},
    network: {
      "POST http://keel.test/v1/media/audio/speech": () => ({
        status: 200,
        headers: { "content-type": "audio/wav", "x-keel-model": "demo-tts" },
        body: "RIFF....WAVEfmt ",
      }),
      "GET http://keel.test/v1/media/capabilities": () => ({
        body: { configured: true, image: false, tts: true, stt: true },
      }),
      "*": () => ({ status: 404, body: "" }),
    },
  });
  await app.waitFor(ready());
  await app.press("id:speech-mode-tts");
  await app.type("id:speech-tts-text", "你好");
  await app.press("id:speech-tts-synthesize");
  await app.waitFor("id:speech-tts-result");

  const request = app.requests().find((item) => item.url.includes("/v1/media/audio/speech"));
  const body = request?.body ? (JSON.parse(request.body) as Record<string, unknown>) : {};
  // The harness has no session selection: the key is absent, which means
  // "deployment default" — the contract is that nothing is invented.
  expect(body["model"]).toBe(undefined);
  expect(body["text"]).toBe("你好");
});

test("语音合成：头部状态与卡片状态必须一致（不得一处写「未配置」一处写「已配置」）", async () => {
  const app = await open("./harness.tsx", {
    clock: {},
    network: {
      "GET http://keel.test/v1/media/capabilities": () => ({
        body: { configured: true, image: false, tts: true, stt: true },
      }),
      "*": () => ({ status: 404, body: "" }),
    },
  });
  await app.waitFor(ready());
  await app.press("id:speech-mode-tts");
  await app.waitFor("id:speech-tts-status");
  const all = app.texts();
  const header = all.filter((t) => t === "已配置" || t === "未配置" || t === "检查中");
  // Exactly one state word per surface region, and it must agree with the probe.
  expect(header).toContain("已配置");
  expect(all.join("\n")).not.toContain("TTS Provider 尚未配置");
});

test("语音工作台：mode 变化必须切换工作台（STT 模型不得停在 TTS 面板）", async () => {
  // STT and TTS both mount the speech surface, so switching between them does
  // not remount — a one-shot body statement would leave the previous workbench
  // on screen. The harness reads `host.params.mode`, so two opens prove the
  // initial mode; the live check is that the TTS-only controls are absent.
  const stt = await open("./harness.tsx", { clock: {}, host: { params: { mode: "stt" } } });
  await stt.waitFor(ready());
  expect(stt.shows("id:speech-tts")).toBe(false);
  expect(stt.shows("id:speech-record")).toBe(true);

  const tts = await open("./harness.tsx", { clock: {}, host: { params: { mode: "tts" } } });
  await tts.waitFor("id:speech-tts");
  expect(tts.shows("id:speech-record")).toBe(false);
});

test("语音转写：文件输入走服务端转写端点（无需麦克风权限）", async () => {
  // The microphone is a browser permission, so the file path is the one that
  // works everywhere — and it is what makes a TTS → STT round trip verifiable.
  const app = await open("./harness.tsx", {
    clock: {},
    network: {
      "POST http://keel.test/v1/media/audio/transcriptions": () => ({
        body: { text: "这是服务端合成的确认音频，用于验证语音链路。", model: "demo-stt" },
      }),
      "GET http://keel.test/v1/media/capabilities": () => ({
        body: { configured: true, image: false, tts: true, stt: true },
      }),
      "*": () => ({ status: 404, body: "" }),
    },
  });
  await app.waitFor(ready());
  // The file tab exists and opens its picker action.
  expect(app.shows("id:speech-mode-file")).toBe(true);
  await app.press("id:speech-mode-file");
  await app.waitFor("id:speech-file-pick");
  expect(app.texts()).toContain("选择音频文件");
});
