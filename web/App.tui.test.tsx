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
//
// The fixture is development-only (change keel-harness-shell, task X1): a test
// asks for it the way the TUI host does, in the launch parameters
// (`host.params.fixture === true`). Without them the app is offline, which is
// the product's real face.
import { expect, open, test } from "nsh/test";

const TITLE = (name: string) => `label:打开会话 ${name}`;

/** The app on the terminal host, with the development transport enabled. */
const openApp = () => open("./App.tsx", { host: { params: { fixture: true } } });

test("TUI: 会话列表 → 打开会话 → 输入 → 显示回复", async () => {
  const app = await openApp();
  expect(app.texts()).toContain("发布准备周报");
  expect(app.texts()).toContain("发布审批");
  expect(app.texts()).toContain("未连接");

  await app.press(TITLE("发布准备周报"));
  await app.waitFor("text:/本周发布准备已基本就绪/");
  await app.waitFor("text:/完成 ·/");

  await app.type("id:composer-input", "请给我一份发布检查清单");
  await app.press("label:发送");
  await app.waitFor("text:/已检查当前工作区/", { timeout: 10_000 });
  await app.waitFor("text:/下一步建议/");
});

test("TUI: 审批卡一键作答", async () => {
  const app = await openApp();
  await app.press(TITLE("发布审批"));
  await app.waitFor("text:需要你的确认");
  expect(app.texts()).toContain("推荐");
  await app.press("text:批准发布");
  await app.waitFor("text:/已确认/");
  await app.waitFor("text:/已记录你的选择/");
});

test("TUI: guide 等待与停止", async () => {
  const app = await openApp();
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
  const app = await openApp();
  // The token-level partial switch lives in the settings page's 通用 section
  // (change keel-harness-shell: the sidebar footer carries only the connection
  // badge — the workspace shows no switch). One switch, the store's one fact.
  await app.press("label:打开设置");
  await app.press("id:settings-section-general");
  await app.waitFor("text:partial 开");
  await app.press("label:切换 token 级 partial");
  await app.waitFor("text:partial 关");
  expect(app.texts()).toContain("partial 关");
});

test("TUI: 设置页——模型分区读/写/重置与分区导航", async () => {
  const app = await openApp();
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

  // 坏文档：400 的字段级 issues 原样呈现。原始文档在「高级」折叠里（spec：
  // 原始 JSON 只放折叠内），先展开再编辑。
  await app.press("id:settings-advanced");
  await app.waitFor("id:settings-raw-document");
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

  // 分区导航：密钥（名字与时间，值不回显）与插件（清单/触发器）。分区入口是
  // kit 的图标 rail，按 testID 寻址（`settings-section-<id>`）。
  await app.press("id:settings-section-secrets");
  await app.waitFor("text:写入 / 覆盖");
  expect(app.texts()).toContain("webhook-token");
  expect(app.texts()).toContain("secret:webhook-token");

  await app.press("id:settings-section-plugins");
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

test("TUI: 本会话模型选择器——选择/恢复默认与租户默认分开", async () => {  const app = await openApp();
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
  await app.press("label:低 >> nth=0");
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

// The workspace-polish change (keel-workspace-polish) on the same fixture:
// titles never leak ids, a parked question is not "thinking", a failed turn
// carries its reason in a banner that can retry, and a suggestion only
// prefills the composer (never sends).
test("TUI: 打磨——不落裸 id/等待确认不是思考/错误横幅与重试/空态只预填", async () => {
  const app = await openApp();
  const screen = () => app.texts().join("\n");

  // C1：行上是标题（未命名会话是「新会话 · 时间」）；屏幕上没有任何裸 session id。
  await app.waitFor("text:/新会话 · /");
  expect(screen()).not.toContain("demo-");
  expect(app.texts()).toContain("发布准备周报");
  // C2（列表侧）：停在未决问句上的会话带「等待确认」。
  expect(app.texts()).toContain("等待确认");

  // C2（对话侧）：等待 ≠ 思考——问句在场时对话自己说的是「等待确认」，
  // 段头不再自称「工作中」（等待态的工具行也不转圈：同一道 waiting 门）。
  await app.press(TITLE("发布审批"));
  await app.waitFor("text:需要你的确认");
  expect(app.texts()).toContain("等待确认");
  expect(app.texts()).not.toContain("工作中");
  await app.press("text:批准发布");
  await app.waitFor("text:/已确认/");

  // C2 的最难一条：问句先到、工具结果还没回——工具行存在、仍在等待，且不被说成工作。
  await app.press("label:返回会话列表");
  await app.press(TITLE("部署卡在确认"));
  await app.waitFor("text:/预发包已就绪/");
  expect(app.texts()).toContain("等待确认");
  expect(app.texts()).not.toContain("工作中");
  expect(screen()).toContain("1 个工具");

  // C3 的诚实性：没有已提交输入的回合（只剩压缩摘要）不给「重试」，
  // 也不渲染成助手空壳（只有压缩分隔条，没有 Keel 头）。
  await app.press("label:返回会话列表");
  await app.press(TITLE("仅压缩记录"));
  await app.waitFor("text:/消息已压缩/");
  expect(app.shows("label:重试本回合")).toBe(false);
  expect(app.count("text:Keel")).toBe(0);

  // C6/C3：失败回合的错误横幅（原因逐字来自账本）＋重试把同一输入再次提交。
  await app.press("label:返回会话列表");
  await app.press(TITLE("生成发布说明"));
  await app.waitFor("text:这次运行失败了");
  expect(app.texts()).toContain("upstream 429: rate limit exceeded (retry-after 30s)");
  await app.press("label:回合失败 >> label:重试本回合");
  await app.waitFor("text:/已检查当前工作区/", { timeout: 15_000 });
  expect(app.texts()).not.toContain("这次运行失败了");

  // C8：空态建议只预填——草稿就是建议原文，且空态仍在（没有自动发送）。
  await app.press("label:返回会话列表");
  await app.press("text:还没有消息");
  await app.waitFor("text:/我是 Keel/");
  await app.press("label:使用建议 把行动项按负责人分组");
  await app.waitFor("text:/把行动项按负责人分组/");
  expect(app.value("id:composer-input")).toBe("把行动项按负责人分组");
  expect(app.texts()).toContain("发送一条消息，Keel 会在这里开始工作。");
});

// The global command palette (change keel-harness-shell, S2): Mod+K opens it
// over the workspace; its search field takes the keys at once (a bare key
// lands in the field — no press on it first); a row runs its action (the
// settings entry leaves the workspace for the settings page); Escape closes.
test("TUI: 命令面板——Mod+K 打开、焦点在搜索框、选中设置", async () => {
  const app = await openApp();
  await app.waitFor("id:workspace-shell");

  await app.keyboard.press("mod+k");
  await app.waitFor("id:command-palette");

  // The field has the focus on open: a key pressed without addressing it is
  // the query (the native Command's `autoFocus`, forwarded by the kit).
  await app.keyboard.press("s");
  expect(app.value("id:command-palette")).toBe("s");
  // The query filters: `settings` (the settings row's keyword) is the search
  // that leaves the settings entry, and pressing it runs `store.openSettings`.
  await app.type("id:command-palette", "ettings");
  await app.waitFor("text:打开设置");
  // Scoped to the palette: the session list's own header carries a settings
  // action with the same text, and the palette's row is the one being tested.
  await app.press("id:command-palette >> text:打开设置");
  await app.waitFor("text:当前生效");
});

test("TUI: 命令面板——Esc 关闭", async () => {
  const app = await openApp();
  await app.waitFor("id:workspace-shell");
  await app.keyboard.press("mod+k");
  await app.waitFor("id:command-palette");
  // The palette opens on an empty query: one Escape closes it (the native
  // command reports cancel; with a typed query its own view-level Escape
  // binding clears the query first — the file's header note).
  await app.keyboard.press("escape");
  await app.wait(150);
  expect(app.shows("id:command-palette")).toBe(false);
});
