// Surface resolution pinned without rendering: the provider family is the one
// input that decides composer + timeline, so a wrong fallback would quietly
// turn a voice session into a text one.
import { expect, test } from "nsh/test";
import type { ModelSelection, ModelView, ProviderView } from "../data/contract";
import {
  SURFACE_DESCRIPTORS,
  checkSurfaceSwitch,
  resolveCaptainSurface,
  resolveSelectionSurface,
  resolveSurface,
  surfaceFact,
} from "./registry";

const model = (extra: Partial<ModelView>): ModelView => ({
  providerId: "p",
  modelId: "m",
  ...extra,
});
const provider = (extra: Partial<ProviderView>): ProviderView => ({ providerId: "p", ...extra });

test("resolveSurface: 六类 provider + 旧数据默认 llm", () => {
  expect(resolveSurface(model({ providerType: "llm" })).kind).toBe("llm");
  expect(resolveSurface(model({ providerType: "realtime" })).kind).toBe("realtime");
  expect(resolveSurface(model({ providerType: "image" })).kind).toBe("image");
  expect(resolveSurface(model({ providerType: "video" })).kind).toBe("video");
  expect(resolveSurface(model({ providerType: "tts" })).kind).toBe("speech");
  expect(resolveSurface(model({ providerType: "stt" })).kind).toBe("speech");
  expect(resolveSurface(model({})).kind).toBe("llm");
});

test("resolveSurface: 插件 descriptor 优先于 providerType", () => {
  const resolved = resolveSurface(model({ providerType: "llm", surfaceKind: "image" }));
  expect(resolved.kind).toBe("image");
  expect(resolved.descriptor.composer).toBe("prompt-media");
});

test("resolveSurface: provider 上的类型在 model 未携带时生效", () => {
  expect(resolveSurface(model({}), provider({ providerType: "realtime" })).kind).toBe("realtime");
});

test("resolveSurface: 未知 providerType 回退 generic 并给出原因，不伪装 llm", () => {
  const resolved = resolveSurface(model({ providerType: "hologram" }));
  expect(resolved.kind).toBe("generic");
  expect(resolved.unknownProviderType).toBe("hologram");
  expect(resolved.descriptor.reason).toContain("hologram");
  expect(resolved.descriptor.timeline).toBe("messages");
});

test("resolveSurface: 旧数据按 outputFormat 推断 medium", () => {
  expect(
    resolveSurface(
      model({
        properties: { outputFormat: { image: true }, inputFormat: { text: true, image: true } },
      }),
    ).kind,
  ).toBe("image");
  expect(resolveSurface(model({ properties: { outputFormat: { video: true } } })).kind).toBe("video");
  expect(
    resolveSurface(model({ properties: { outputFormat: { audio: true }, inputFormat: { audio: true } } })).kind,
  ).toBe("realtime");
  expect(resolveSurface(model({ properties: { outputFormat: { audio: true } } })).kind).toBe("speech");
});

test("resolveSurface: tts/stt 共用 speech 但工作台不同", () => {
  expect(resolveSurface(model({ providerType: "tts" })).descriptor.speechMode).toBe("tts");
  expect(resolveSurface(model({ providerType: "stt" })).descriptor.speechMode).toBe("stt");
  expect(resolveSurface(model({ providerType: "tts" })).descriptor.label).toBe("文本转语音");
});

test("resolveSelectionSurface: selection 自带的 surfaceKind 能被解析", () => {
  const selection: ModelSelection = { providerId: "p", modelId: "m-image", surfaceKind: "image" };
  expect(resolveSelectionSurface(selection, []).kind).toBe("image");
  expect(
    resolveSelectionSurface(
      { providerId: "p", modelId: "m-rt" },
      [model({ modelId: "m-rt", providerType: "realtime" })],
    ).kind,
  ).toBe("realtime");
  expect(resolveSelectionSurface(null, []).kind).toBe("llm");
});

test("checkSurfaceSwitch: 同 surface 自由，跨 surface 必须新建会话", () => {
  expect(checkSurfaceSwitch("realtime", "realtime").allowed).toBe(true);
  const crossed = checkSurfaceSwitch("llm", "realtime");
  expect(crossed.allowed).toBe(false);
  expect(crossed.requiresNewSession).toBe(true);
  expect(crossed.reason).toContain("新建会话");
});

test("surfaceFact: 每种 surface 都有一句可读事实", () => {
  for (const kind of Object.keys(SURFACE_DESCRIPTORS) as (keyof typeof SURFACE_DESCRIPTORS)[]) {
    expect(SURFACE_DESCRIPTORS[kind].label.length > 0).toBe(true);
  }
  expect(surfaceFact(model({ providerType: "image" }))).toContain("图像生成");
  // A medium without a shipped panel must not promise one in the picker.
  expect(surfaceFact(model({ providerType: "video" }))).toContain("暂无专用面板");
  expect(surfaceFact(model({ providerType: "hologram" }))).toContain("暂无专用面板");
});

test("video: 有明确原因（不是空面板承诺），placeholder 直接引用", () => {
  const video = SURFACE_DESCRIPTORS.video;
  expect(video.reason).toContain("视频生成面板尚未提供");
  expect(resolveSurface(model({ providerType: "video" })).descriptor.reason).toContain("视频生成面板尚未提供");
});

test("resolveCaptainSurface: 队长模型决定主会话 surface，无队长回退 llm", () => {
  const realtime = { providerId: "p", modelId: "m-rt", providerType: "realtime" };
  const image = { providerId: "p", modelId: "m-img", providerType: "image" };
  expect(resolveCaptainSurface({ providerId: "p", modelId: "m-rt" }, [realtime, image]).kind).toBe("realtime");
  expect(resolveCaptainSurface({ providerId: "p", modelId: "m-img" }, [realtime, image]).kind).toBe("image");
  // The captain's own descriptor wins over the registry row.
  expect(resolveCaptainSurface({ providerId: "p", modelId: "m-rt", surfaceKind: "image" }, [realtime]).kind).toBe("image");
  // Unknown id without a descriptor is the text baseline, never generic noise.
  expect(resolveCaptainSurface({ providerId: "p", modelId: "gone" }, [realtime]).kind).toBe("llm");
  expect(resolveCaptainSurface(null, []).kind).toBe("llm");
  expect(resolveCaptainSurface(undefined, []).kind).toBe("llm");
});
