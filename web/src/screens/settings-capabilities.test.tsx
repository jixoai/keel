// TUI gate for the settings「能力」section (read-only deployment capabilities).
//
// The section's two probes are answered in memory by the harness (its
// `setCapabilityFetch` seam), so the real `KeelHttp` parsing runs: status
// mapping, environment-name allowlisting and reason sanitization are
// exercised, not a second fake client. The assertions also pin the privacy
// promise — a mocked response that carries a URL, a key or a Go error must
// never reach the screen as text.
import { expect, open, test } from "nsh/test";

const HARNESS = "./settings-capabilities.harness.tsx";

const joinedTexts = (app: { texts(): string[] }): string => app.texts().join("\n");

test("TUI: 能力分区——可用/未配置/不支持三态并存，原因与地址经净化", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "mixed" } } });

  await app.waitFor("id:capability-row-chat");
  const texts = app.texts();
  const joined = joinedTexts(app);

  for (const label of ["文本对话", "图像生成", "图像编辑", "决策", "视频", "语音合成", "语音识别", "实时语音"]) {
    expect(texts).toContain(label);
  }

  // Green: an available row names the model this deployment routes to it.
  expect(joined).toContain("3 项可用");
  expect(joined).toContain("模型：deepseek-v4.1-flash");
  expect(joined).toContain("模型：image-3");

  // Neutral: the provider's own human reason is shown verbatim when safe.
  expect(joined).toContain("provider does not support image editing");

  // Warning: unconfigured rows carry the environment NAMES to set.
  expect(joined).toContain("3 项未配置");
  expect(joined).toContain("KEEL_MEDIA_MODEL_DECISION");
  expect(joined).toContain("KEEL_MEDIA_MODEL_VIDEO");
  expect(joined).toContain("KEEL_REALTIME_WS_URL");
  expect(joined).toContain("KEEL_REALTIME_API_KEY");
  expect(joined).toContain("KEEL_REALTIME_MODEL");

  // A reason that is really an infrastructure error is not printed: no URL, no
  // key, no transport text.
  expect(joined).not.toContain("media.internal");
  expect(joined).not.toContain("sk-live");
  expect(joined).not.toContain("https://");
  expect(joined).not.toContain("rejected");
});

test("TUI: 能力分区——401 显示登录态，不回显鉴权错误原文", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "auth" } } });

  await app.waitFor("id:capabilities-notice-auth");
  const joined = joinedTexts(app);
  expect(joined).toContain("需要登录后才能读取部署能力。");
  expect(joined).toContain("需要登录");
  expect(joined).not.toContain("unauthorized");
  expect(joined).not.toContain("bearer");
  expect(joined).not.toContain("sk-live");
});

test("TUI: 能力分区——服务端无探测端点时降级为「不支持」，不回显 404 原文", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "missing" } } });

  await app.waitFor("id:capabilities-notice-unsupported");
  const joined = joinedTexts(app);
  expect(joined).toContain("当前服务端不提供部分能力探测；标注「不支持」的行即该情况。");
  expect(joined).toContain("当前服务端不支持能力探测。");
  expect(joined).not.toContain("404 page not found");
  expect(joined).not.toContain("404");
});

test("TUI: 能力分区——网络不可达时给出可重试的中性提示，不回显传输错误", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "offline" } } });

  await app.waitFor("text:/暂时无法读取部署能力/");
  const joined = joinedTexts(app);
  expect(joined).toContain("状态未知");
  expect(app.shows("text:重试")).toBe(true);
  expect(joined).not.toContain("connection reset");
  expect(joined).not.toContain("peer");
});

test("TUI: 能力分区——媒体网关 503 是「未配置」而非错误，实时语音已配置即可用", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "media-unconfigured" } } });

  await app.waitFor("id:capability-row-chat");
  const joined = joinedTexts(app);
  expect(joined).toContain("1 项可用");
  expect(joined).toContain("7 项未配置");
  // Every media row names the gateway-level variables and its own model slot.
  expect(joined).toContain("KEEL_MEDIA_BASE_URL");
  expect(joined).toContain("KEEL_MEDIA_API_KEY");
  expect(joined).toContain("KEEL_MEDIA_MODEL_CHAT");
  expect(joined).toContain("KEEL_MEDIA_MODEL_STT");
  expect(joined).toContain("模型：qwen-omni-realtime");
  expect(joined).not.toContain("media gateway is not configured");
});

test("TUI: 能力分区——网关扁平形状（llm/image/decision 为真）不再被误报为全不支持", async () => {
  // The real gateway answers `{configured, llm, image, decision, video, tts,
  // stt, realtime, models}`. Before the flat-shape branch this parsed to zero
  // rows and the page claimed every capability was unsupported on a deployment
  // whose chat and image paths demonstrably worked.
  const app = await open(HARNESS, { host: { params: { scenario: "flat" } } });
  await app.waitFor("id:capability-row-chat");
  const joined = joinedTexts(app);
  expect(joined).toContain("4 项可用");
  expect(joined).toContain("模型：bailian-token-plan/qwen3.8-flash");
  expect(joined).toContain("模型：bailian-token-plan/wan2.7-image");
  // Booleans that are false are answers, not missing configuration.
  expect(joined).toContain("3 项不支持");
  expect(joined).toContain("1 项未配置");
});
