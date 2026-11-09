// TUI verification: the same App.tsx on NSH's terminal host.
//
//   nsh test --target tui
//
// Each test opens the app on an in-memory terminal (the real TUI host, not a
// second UI): it finds views by their accessibility labels/test ids and reads
// the terminal's texts. The flows cover the change's TUI scenario: session
// list → open → type → reply → approve, plus guide/stop and the partial
// switch. The settings page and the session model picker (change
// keel-model-settings) are covered below against the fixture transport.
import { expect, open, test } from "nsh/test";

const TITLE = (name: string) => `label:打开会话 ${name}`;

test("TUI: 会话列表 → 打开会话 → 输入 → 显示回复", async () => {
  const app = await open("./App.tsx");
  expect(app.texts()).toContain("发布准备周报");
  expect(app.texts()).toContain("发布审批");
  expect(app.texts()).toContain("演示数据 · 未连接服务");

  await app.press(TITLE("发布准备周报"));
  await app.waitFor("text:/本周发布准备已基本就绪/");
  await app.waitFor("text:/完成 ·/");

  await app.type("id:composer-input", "请给我一份发布检查清单");
  await app.press("label:发送");
  await app.waitFor("text:/已检查当前工作区/", { timeout: 10_000 });
  await app.waitFor("text:/下一步建议/");
});

test("TUI: 审批卡一键作答", async () => {
  const app = await open("./App.tsx");
  await app.press(TITLE("发布审批"));
  await app.waitFor("text:需要你的确认");
  expect(app.texts()).toContain("推荐");
  await app.press("text:批准发布");
  await app.waitFor("text:/已确认/");
  await app.waitFor("text:/已记录你的选择/");
});

test("TUI: guide 等待与停止", async () => {
  const app = await open("./App.tsx");
  await app.press(TITLE("会议纪要整理"));
  await app.waitFor("text:等待引导当前任务…");
  await app.press("text:引导");
  await app.type("id:composer-input", "把行动项按负责人分组，并标注截止日期。");
  await app.press("label:发送");
  await app.waitFor("text:等待引导当前任务…");
  await app.press("label:停止当前任务");
  await app.waitFor("text:/已停止/");
});

test("TUI: partial 开关", async () => {
  const app = await open("./App.tsx");
  expect(app.texts()).toContain("partial 开");
  await app.press("label:切换 token 级 partial");
  await app.waitFor("text:partial 关");
  expect(app.texts()).toContain("partial 关");
});

test("TUI: 设置页——模型分区读/写/重置与分区导航", async () => {
  const app = await open("./App.tsx");
  await app.press(TITLE("发布准备周报"));
  await app.waitFor("text:/本周发布准备已基本就绪/");

  // 工作区入口（窄屏在会话列表头部）→ 独立设置页（工作区不再是当前 shell）。
  await app.press("label:返回会话列表");
  await app.press("label:打开设置");
  await app.waitFor("text:当前生效");
  expect(app.texts()).toContain("部署默认");
  expect(app.texts()).toContain("revision 3");
  expect(app.texts()).toContain("s2api/deepseek-v4.1-flash · 高");
  expect(app.texts()).toContain("模型列表");
  expect(app.texts()).toContain("Provider 列表");
  // 不可执行的 provider 的模型不可选（anthropic 在演示里没有凭据）。
  expect(app.texts()).toContain("不可执行");
  await expect(app.press("label:设为默认 anthropic/claude-sonnet-5")).rejects.toMatchObject({
    code: "ERR_NSH_TEST_DISABLED",
  });

  // 坏文档：400 的字段级 issues 原样呈现。
  await app.clear("id:settings-raw-document");
  await app.type(
    "id:settings-raw-document",
    '{"schemaVersion":1,"providers":[{"id":"s2api","api":{"type":"openai-chat-completions","baseUrl":"https://s2api.top/v1"},"access":{"type":"api-key","env":"S2API_KEY"},"modelIds":["deepseek-v4.1-flash"]}],"selection":{"providerId":"s2api","modelId":"nope"}}',
  );
  await app.press("label:保存模型配置");
  await app.waitFor("text:/保存被拒绝/");
  expect(app.texts()).toContain("selection.modelId");
  expect(app.texts()).toContain("selection s2api/nope does not match any declared model");
  // revision 没有被写走。
  expect(app.texts()).toContain("revision 3");

  // 好文档：保存即生效，revision +1，来源变「已保存覆盖」。
  await app.clear("id:settings-raw-document");
  await app.type(
    "id:settings-raw-document",
    '{"schemaVersion":1,"providers":[{"id":"s2api","api":{"type":"openai-chat-completions","baseUrl":"https://s2api.top/v1"},"access":{"type":"api-key","env":"S2API_KEY"},"modelIds":["deepseek-v4.1-flash"]}],"selection":{"providerId":"s2api","modelId":"deepseek-v4.1-flash","reasoningLevel":"low"}}',
  );
  await app.press("label:保存模型配置");
  await app.waitFor("text:/revision 4/");
  expect(app.texts()).toContain("已保存覆盖");
  await app.waitFor("text:/s2api\\/deepseek-v4\\.1-flash · 低/");

  // 重置（两步）→ 回部署默认。
  await app.press("label:重置为部署默认");
  await app.waitFor("text:/重置会删除已保存的覆盖/");
  await app.press("label:确认重置");
  await app.waitFor("text:/已重置为部署默认/");
  expect(app.texts()).toContain("部署默认");
  await app.waitFor("text:/revision 5/");

  // 分区导航：密钥（名字与时间，值不回显）与插件（清单/触发器）。
  await app.press("label:设置分区 密钥");
  await app.waitFor("text:写入 / 覆盖");
  expect(app.texts()).toContain("webhook-token");
  expect(app.texts()).toContain("secret:webhook-token");

  await app.press("label:设置分区 插件");
  await app.waitFor("text:插件清单");
  expect(app.texts()).toContain("generation 3");
  expect(app.texts()).toContain("demo.workflow");
  expect(app.texts()).toContain("start failed: listen tcp 127.0.0.1:0: permission denied");
  // 装载语义必须写清：插件在启动时装载，这里只看状态与控制触发。
  expect(app.texts()).toContain(
    "插件在进程启动时装载并与依赖一起装配；这里看状态与控制触发，不做代码级配置。",
  );

  // 手动触发触发器并看到结果。
  await app.press("label:手动触发 demo.workflow.daily");
  await app.waitFor("text:/触发结果/");

  // 返回工作区：工作区原样回来（会话列表仍在，原会话仍是打开状态——store 持有
  // activeId/engine，跨 shell 切换不丢；列表行回到会话即见到此前的内容）。
  await app.press("label:返回工作区");
  await app.waitFor("text:发布准备周报");
  await app.press(TITLE("发布准备周报"));
  await app.waitFor("label:本会话模型");
  await app.waitFor("text:/本周发布准备已基本就绪/");
});

test("TUI: 本会话模型选择器——选择/恢复默认与租户默认分开", async () => {
  const app = await open("./App.tsx");
  await app.press(TITLE("发布准备周报"));
  await app.waitFor("text:/本周发布准备已基本就绪/");

  // 头部 chip 未选择时显示租户默认（打开会话时载入生效文档）。
  await app.waitFor("text:/默认 s2api\\/deepseek-v4\\.1-flash/");
  await app.press("label:本会话模型");
  await app.waitFor("text:本会话当前");
  expect(app.texts()).toContain("租户默认");
  expect(app.texts()).toContain("s2api/deepseek-v4.1-flash · 高");

  // 选择 claude-sonnet-5（fixture 里 anthropic 无凭据 → 不在选择器里）。
  expect(app.texts()).not.toContain("claude-sonnet-5");
  await app.press("label:选择模型 s2api/deepseek-v4.1-flash");
  await app.waitFor("text:本会话使用中");
  // 思考档：选择器内点「低」（两个 provider 同名模型的档位都叫「低」，取第一个）。
  await app.press("label:本会话思考档 低 >> nth=0");
  await app.press("label:关闭");
  expect(app.texts()).toContain("s2api/deepseek-v4.1-flash · 低");

  // 之后的输入携带该选择；输入本身不带任何模型文字。
  await app.type("id:composer-input", "用当前会话模型回一句话");
  await app.press("label:发送");
  await app.waitFor("text:/已收到/", { timeout: 10_000 });

  // 恢复默认：输入不再带选择（chip 回到租户默认）。
  await app.press("label:本会话模型");
  await app.press("label:恢复默认");
  await app.press("label:关闭");
  expect(app.texts()).toContain("默认 s2api/deepseek-v4.1-flash · 高");
});
