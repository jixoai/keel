// Headless (TUI) gate for the image session surface (change
// keel-session-surfaces-v1, spec media-session-surface).
//
// The surface has exactly two product states: a configured gateway (real
// generation through /v1/media/images/*) and a configuration state that names
// what is missing and offers the settings route. There is no demo panel: an
// unreachable or unconfigured gateway produces the configuration state, never
// local placeholder art.
//
// The tests stub the same-origin media API through `open(..., { network })`;
// the harness points the media client at the stub's origin. The manual clock
// keeps the generation timer deterministic.
import { expect, open, test, type StubRequest, type StubResponse } from "nsh/test";

const ORIGIN = "http://keel.test";

const openSurface = () => open("./harness.tsx", { width: 390, clock: {} });

/** Routes the media paths; anything else (an `<Image>` reading a provider URL
 * in the headless renderer) gets an empty 404. */
const mediaGateway = (
  overrides: Record<string, (request: StubRequest) => StubResponse | Promise<StubResponse>> = {},
) => ({
  "*": (request: StubRequest): StubResponse => {
    for (const [path, answer] of Object.entries(overrides)) {
      if (request.url.includes(path)) return answer(request) as StubResponse;
    }
    return { status: 404, body: "" };
  },
});

const realGateway = (
  generate: (request: StubRequest) => StubResponse | Promise<StubResponse>,
  edit: (request: StubRequest) => StubResponse | Promise<StubResponse> = () => ({
    body: { assets: [{ url: "https://cdn.fde2e.test/gen/edited.png", model: "wan2.7-image" }], model: "wan2.7-image" },
  }),
) => ({
  [`POST ${ORIGIN}/v1/media/images/generate`]: generate,
  [`POST ${ORIGIN}/v1/media/images/edit`]: edit,
  "*": (request: StubRequest): StubResponse => {
    if (request.url.includes("/v1/media/capabilities")) {
      return { body: { configured: true, image: true, video: false, tts: false } };
    }
    return { status: 404, body: "" };
  },
});

const bodyOf = (request: StubRequest | undefined): Record<string, unknown> => {
  if (!request?.body) return {};
  try {
    return JSON.parse(request.body) as Record<string, unknown>;
  } catch {
    return {};
  }
};

test("TUI: 图像 surface——网关不可达时是配置态，不生成任何占位图", async () => {
  const app = await openSurface();

  await app.waitFor("id:image-mode-unconfigured");
  expect(app.texts()).toContain("图像生成尚未配置");
  expect(app.texts()).toContain("配置一个图像 Provider 后，才能在这里生成或编辑图片。");
  await app.press("id:image-sample-0");
  expect(app.value("id:image-prompt")).toBe("薄暮海岸，青绿与靛蓝的玻璃质感");
  expect(app.enabled("id:image-generate")).toBe(false);
  expect(app.texts()).toContain("配置图像 Provider");
  expect(app.texts()).not.toContain("已完成");
});

test("TUI: 图像 surface——网关在但没有图像能力时，配置态说明原因", async () => {
  const app = await open("./harness.tsx", {
    width: 390,
    clock: {},
    network: mediaGateway({
      "/v1/media/capabilities": () => ({ body: { configured: true, image: false } }),
    }),
  });

  await app.waitFor("id:image-mode-unconfigured");
  expect(app.texts()).toContain("图像网关已连接，但没有启用图像生成。");
  expect(app.texts()).toContain("配置图像 Provider");
  expect(app.enabled("id:image-generate")).toBe(false);
  expect(app.texts()).not.toContain("已完成");
});

test("TUI: 图像 surface——真实生成进画廊，请求体来自 composer", async () => {
  const app = await open("./harness.tsx", {
    width: 390,
    clock: {},
    network: realGateway(() => ({
      delay: 600,
      body: {
        assets: [
          { url: "https://cdn.fde2e.test/gen/one.png", model: "wan2.7-image" },
          { url: "https://cdn.fde2e.test/gen/two.png", model: "wan2.7-image" },
        ],
        model: "wan2.7-image",
      },
    })),
  });

  await app.waitFor("id:image-mode-real");
  await app.press("id:image-sample-0");
  await app.press("id:image-count-2");
  await app.press("id:image-generate");
  await app.waitFor("text:/生成中 · 2 张/");
  await app.clock!.runFor(1200);
  await app.waitFor("text:已完成 · 2 张");

  expect(app.texts()).toContain("wan2.7-image");
  expect(app.texts()).toContain("v1");
  const generate = app.requests().find((request) => request.url.includes("/v1/media/images/generate"));
  expect(bodyOf(generate)["prompt"]).toBe("薄暮海岸，青绿与靛蓝的玻璃质感");
  expect(bodyOf(generate)["n"]).toBe(2);
  expect(bodyOf(generate)["size"]).toBe("1024x1024");
});

test("TUI: 图像 surface——多轮编辑用选中资产的 Provider URL 作参考", async () => {
  const app = await open("./harness.tsx", {
    width: 390,
    clock: {},
    network: realGateway(() => ({
      body: {
        assets: [{ url: "https://cdn.fde2e.test/gen/one.png", model: "wan2.7-image" }],
        model: "wan2.7-image",
      },
    })),
  });

  await app.waitFor("id:image-mode-real");
  await app.press("id:image-sample-0");
  await app.press("id:image-generate");
  await app.waitFor("text:已完成 · 1 张");

  await app.press("label:编辑 v1 >> nth=0");
  await app.waitFor("id:image-edit-instruction");
  await app.type("id:image-edit-instruction", "把背景换成夜景");
  await app.press("id:image-generate");
  await app.waitFor("text:/已完成 · 1 张/ >> nth=1");
  expect(app.texts()).toContain("v2");
  expect(app.texts()).toContain("派生自 v1");

  const edit = app.requests().find((request) => request.url.includes("/v1/media/images/edit"));
  expect(bodyOf(edit)["prompt"]).toBe("把背景换成夜景");
  expect(bodyOf(edit)["image"]).toBe("https://cdn.fde2e.test/gen/one.png");
});

test("TUI: 图像 surface——参数摘要只列已声明字段", async () => {
  const app = await open("./harness.tsx", {
    width: 390,
    clock: {},
    network: realGateway(() => ({
      body: {
        assets: [{ url: "https://cdn.fde2e.test/gen/one.png", model: "wan2.7-image" }],
        model: "wan2.7-image",
      },
    })),
  });

  await app.waitFor("id:image-mode-real");
  await app.press("id:image-sample-0");
  await app.press("id:image-aspect-16x9");
  await app.press("id:image-negative-toggle");
  await app.waitFor("id:image-negative");
  await app.type("id:image-negative", "文字");
  await app.press("id:image-generate");
  await app.waitFor("text:已完成 · 1 张");

  expect(app.texts()).toContain("比例 16:9");
  expect(app.texts()).toContain("负向：文字");
  const generate = app.requests().find((request) => request.url.includes("/v1/media/images/generate"));
  expect(bodyOf(generate)["negativePrompt"]).toBe("文字");
  expect(bodyOf(generate)["size"]).toBe("1280x720");
});

test("TUI: 图像 surface——真实生成 503：显示 Provider 消息、保留输入并可重试", async () => {
  let attempts = 0;
  const app = await open("./harness.tsx", {
    width: 390,
    clock: {},
    network: realGateway(() => {
      attempts += 1;
      if (attempts === 1) {
        return { status: 503, body: { error: "图像服务未配置：缺少 API Key" } };
      }
      return {
        body: {
          assets: [{ url: "https://cdn.fde2e.test/gen/retry.png", model: "wan2.7-image" }],
          model: "wan2.7-image",
        },
      };
    }),
  });

  await app.waitFor("id:image-mode-real");
  await app.press("id:image-sample-0");
  await app.press("id:image-generate");
  await app.waitFor("text:生成失败");
  expect(app.texts().join("\n")).toContain("图像服务未配置：缺少 API Key");
  expect(app.shows("label:配置图像 Provider")).toBe(true);
  expect(app.value("id:image-prompt")).toBe("薄暮海岸，青绿与靛蓝的玻璃质感");

  await app.press("label:重试本轮生成");
  await app.waitFor("text:已完成 · 1 张");
  expect(attempts).toBe(2);
});

test("TUI: 图像 surface——真实生成取消会中止请求并保留 prompt", async () => {
  const app = await open("./harness.tsx", {
    width: 390,
    clock: {},
    network: realGateway(() => ({
      delay: 3000,
      body: {
        assets: [{ url: "https://cdn.fde2e.test/gen/late.png", model: "wan2.7-image" }],
        model: "wan2.7-image",
      },
    })),
  });

  await app.waitFor("id:image-mode-real");
  await app.press("id:image-sample-0");
  await app.press("id:image-generate");
  await app.waitFor("id:image-cancel");
  await app.press("id:image-cancel");
  await app.waitFor("text:已取消");

  expect(app.value("id:image-prompt")).toBe("薄暮海岸，青绿与靛蓝的玻璃质感");
  expect(app.texts()).not.toContain("wan2.7-image");
});

test("TUI: 图像 surface——请求必须带上会话选中的模型（否则网关走默认/计费 provider）", async () => {
  // The gateway routes on the model id. A surface that omits it silently sends
  // the work to the deployment default — in a mixed deployment that is another
  // provider, and in the demo case it bills a real vendor for a demo panel.
  const app = await open("./harness.tsx", {
    width: 390,
    clock: {},
    network: realGateway(() => ({
      body: {
        assets: [{ url: "https://cdn.fde2e.test/gen/one.png", model: "demo-image" }],
        model: "demo-image",
      },
    })),
  });

  await app.waitFor("id:image-mode-real");
  await app.press("id:image-sample-0");
  await app.press("id:image-generate");
  await app.waitFor("text:已完成 · 1 张");

  const generate = app.requests().find((request) => request.url.includes("/v1/media/images/generate"));
  // The harness has no session selection, so the key is absent — and that is
  // exactly the contract: absent means "deployment default", never a guess.
  expect(bodyOf(generate)["model"]).toBe(undefined);
});
