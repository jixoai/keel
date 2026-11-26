// TUI verification for the 员工 / 团队 / 运行 chapters — change
// keel-bot-identity-access, A4.4b.
//
// Success faces run on the fixture transport (the design board's sample
// deployment): six Bots, three teams and one real (in-memory) run ledger. The
// five states run in a harness inside the app's module graph, because a test's
// module graph is not the app's.
import { expect, open, test } from "nsh/test";

const app = (width = 1440) => open("../../../App.tsx", { width, host: { params: { fixture: true } } });
const harness = (width: number, params: Record<string, string>) =>
  open("./bots.harness.tsx", { width, host: { params } });
const waitRun = (view: { waitFor: (selector: string, options?: { timeout?: number }) => Promise<void> }) =>
  view.waitFor("id:run-lanes", { timeout: 20000 });

test("TUI: 新建 Bot 表单——名字/身份档案/模型类型/默认模型四个 Field，缺项不可提交", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-bots");
  await view.waitFor("id:bots-head");
  await view.press("id:bots-create");
  await view.waitFor("id:bot-create-dialog");
  const form = view.texts().join("\n");
  expect(form).toContain("身份档案（账号 + 人设）");
  expect(form).toContain("模型类型");
  expect(form).toContain("默认模型");
  expect(view.enabled("id:bot-create-confirm")).toBe(false);
});

test("TUI: 新建团队表单——团队名/使命/队长三个 Field，缺项不可提交", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-teams");
  await view.waitFor("id:teams-head");
  await view.press("id:teams-create");
  await view.waitFor("id:team-create-dialog");
  const form = view.texts().join("\n");
  expect(form).toContain("团队名");
  expect(form).toContain("使命");
  expect(form).toContain("队长");
  expect(view.enabled("id:team-create-confirm")).toBe(false);
});

test("TUI: 员工索引——真实 Bot、账号、模型、状态与筛选", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-bots");
  await view.waitFor("id:bots-head");
  expect(view.texts()).toContain("客服一号");
  expect(view.texts()).toContain("订单查询");
  expect(view.texts()).toContain("财务助手");
  expect(view.texts()).toContain("已归档");
  // The shell's top band is the brand / plugin strip; the chapter rail carries
  // the destinations, and no gray 「未连接」 sits on a page that never opened a
  // stream.
  expect(view.texts()).toContain("KEEL");
  expect(view.shows("id:books-rail")).toBe(true);
  expect(view.texts()).not.toContain("未连接");
  // No implementation language leaks into the product copy.
  const screen = view.texts().join("\n");
  expect(screen).not.toContain("A4.4b");
  expect(screen).not.toContain("这一片");

  // Status filter: archived only.
  await view.press("label:按状态筛选");
  await view.press("text:已归档 >> nth=1");
  await view.waitFor("text:财务助手");
  expect(view.texts()).not.toContain("客服一号");

  // Search finds by name.
  await view.press("label:按状态筛选");
  await view.press("text:全部状态");
  await view.type("id:bots-search", "退款");
  expect(view.texts()).toContain("退款专员");
  expect(view.texts()).not.toContain("订单查询");
});

test("TUI: Bot 页——五列能力表、Harness 编辑、账本高危确认", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-bots");
  await view.waitFor("id:bots-head");
  await view.press("label:打开 Bot 客服一号");
  await view.waitFor("id:bot-capabilities");

  const screen = view.texts().join("\n");
  expect(screen).toContain("账号范围");
  expect(screen).toContain("Bot 授权");
  expect(screen).toContain("有效能力");
  expect(screen).toContain("空名单表示它什么也不能做");
  // The high-risk capability is visible by default, not hidden behind a fold.
  expect(view.shows("id:bot-capability-bot.invoke")).toBe(true);
  expect(screen).toContain("调用 Bot");

  // Allow a plain capability: a real draft diff, then the ledger.
  await view.press("label:删除会话 的 Bot 授权");
  // 8 rows already show 「允许」 in their trigger; the 9th exact 「允许」 is the menu item.
  await view.press("text:允许 >> nth=8");
  await view.waitFor("text:1 项未保存");
  expect(view.texts().join("\n")).toContain("本次变更");
  await view.press("id:bot-save");
  await view.waitFor("text:确认 1 项变更");
  expect(view.texts().join("\n")).toContain("本次登记人");
  await view.press("id:bot-ledger-confirm");
  await view.waitFor("text:/已登记 1 项变更/");
});

test("TUI: Bot 页——工作方式编辑走账本确认", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-bots");
  await view.waitFor("id:bots-head");
  await view.press("label:打开 Bot 客服一号");
  await view.waitFor("id:bot-capabilities");
  await view.press("id:bot-section-work");
  await view.waitFor("id:bot-work");
  expect(view.texts()).toContain("模型类型");
  expect(view.texts()).toContain("默认模型");
  expect(view.texts()).toContain("工作说明");
  await view.type("id:bot-instruction", "先查订单，再拟回复；金额异常时升级人工。");
  await view.waitFor("text:/有未保存的变更/");
  await view.press("id:bot-save");
  await view.waitFor("text:确认 1 项变更");
  await view.press("id:bot-ledger-confirm");
  await view.waitFor("text:/已登记 1 项变更/");
});

test("TUI: Bot 页——高危能力保存必须先勾选", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-bots");
  await view.waitFor("id:bots-head");
  await view.press("label:打开 Bot 客服一号");
  await view.waitFor("id:bot-capabilities");
  // 读取密钥值 is a critical key the account does not hold; allowing it in the
  // harness is still a high-risk edit and must be confirmed.
  await view.press("label:读取密钥值 的 Bot 授权");
  await view.press("text:允许 >> nth=8");
  await view.waitFor("text:1 项高危 · 需确认");
  await view.press("id:bot-save");
  await view.waitFor("text:确认 1 项变更");
  expect(view.texts().join("\n")).toContain("勾选后才能登记");
  await view.press("id:bot-high-risk-secret.read.value");
  await view.press("id:bot-ledger-confirm");
  await view.waitFor("text:/已登记 1 项变更/");
});

test("TUI: Bot 页——密钥列表、签发（明文只显示一次）与吊销", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-bots");
  await view.waitFor("id:bots-head");
  await view.press("label:打开 Bot 客服一号");
  await view.waitFor("id:bot-capabilities");
  await view.press("id:bot-section-keys");
  await view.waitFor("id:bot-keys");
  expect(view.texts()).toContain("生产 Webhook");
  expect(view.texts()).toContain("已吊销");

  await view.press("id:bot-key-issue");
  await view.waitFor("id:bot-issue-dialog");
  await view.type("id:bot-issue-label", "验收密钥");
  await view.press("id:bot-issue-confirm");
  await view.waitFor("id:bot-issued-dialog");
  expect(view.texts()).toContain("这个密钥只显示一次，关闭后不能回读。");
  expect(view.shows("id:bot-issued-plaintext")).toBe(true);
  await view.press("id:bot-issued-done");
  await view.waitFor("text:验收密钥");

  await view.press("label:吊销密钥 生产 Webhook");
  await view.waitFor("id:bot-revoke-dialog");
  await view.press("id:bot-revoke-dialog-confirm");
  await view.waitFor("text:/密钥已吊销/");
  expect(view.texts()).toContain("已吊销");
});

test("TUI: Bot 页——试跑走真实 invoke 回执并打开会话", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-bots");
  await view.waitFor("id:bots-head");
  await view.press("label:打开 Bot 客服一号");
  await view.waitFor("id:bot-capabilities");
  await view.press("label:试跑这个 Bot");
  await view.waitFor("id:bot-try-run");
  await view.press("id:bot-try-run");
  await view.waitFor("id:bot-try-success");
  expect(view.texts().join("\n")).toContain("这一轮已经创建了会话");
  expect(view.texts()).toContain("打开会话");
});

test("TUI: 团队索引——队长、使命、状态与待处理", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-teams");
  await view.waitFor("id:teams-head");
  expect(view.texts()).toContain("客服团队");
  expect(view.texts()).toContain("内容运营");
  expect(view.texts()).toContain("财务对账");
  expect(view.texts()).toContain("有成员需处理");
  // The cross-object todo count left the top band (A4.4f); the team page's own
  // summary still reports the same pending item.
  expect(view.texts().join("\n")).toContain("1 项待处理");
});

test("TUI: 团队页——使命、名单、启动前检查与启动", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-teams");
  await view.waitFor("id:teams-head");
  await view.press("label:打开团队 客服团队");
  await view.waitFor("id:team-roster");
  const screen = view.texts().join("\n");
  expect(screen).toContain("团队本身不拥有权限");
  expect(screen).toContain("启动前检查");
  expect(screen).toContain("3 / 3");
  expect(screen).toContain("可调用");
  await view.press("id:team-start");
  await view.waitFor("id:team-start-dialog");
  await view.press("id:team-start-dialog-confirm");
  await waitRun(view);
  expect(view.texts()).toContain("客服团队");
  expect(view.texts()).toContain("售后升级");
  expect(view.texts().join("\n")).toContain("缺「调用 Bot」能力");
});

test("TUI: 团队页——添加成员走账本确认", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-teams");
  await view.waitFor("id:teams-head");
  await view.press("label:打开团队 客服团队");
  await view.waitFor("id:team-roster");
  await view.press("id:team-add-member");
  await view.waitFor("id:team-member-search");
  await view.press("label:加入 管家");
  await view.waitFor("text:/有未保存的变更/");
  await view.press("id:team-save");
  await view.waitFor("text:确认 1 项变更");
  expect(view.texts().join("\n")).toContain("管家");
  await view.press("id:team-ledger-confirm");
  await view.waitFor("text:/已登记 1 项变更/");
  expect(view.texts().join("\n")).toContain("6 名成员");
});

test("TUI: 团队页——更换队长必须高危确认", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-teams");
  await view.waitFor("id:teams-head");
  await view.press("label:打开团队 客服团队");
  await view.waitFor("id:team-roster");
  await view.press("id:team-change-captain");
  await view.waitFor("id:team-captain-dialog");
  await view.press("text:请选择");
  await view.press("text:订单查询");
  await view.press("id:team-captain-confirm");
  await view.waitFor("text:/1 项高危/");
  await view.press("id:team-save");
  await view.waitFor("text:确认 1 项变更");
  expect(view.texts().join("\n")).toContain("我确认更换队长为「订单查询」（高危）");
  await view.press("id:team-high-risk-captain");
  await view.press("id:team-ledger-confirm");
  await view.waitFor("text:/已登记 1 项变更/");
});

test("TUI: Bot 页——改绑身份档案", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-bots");
  await view.waitFor("id:bots-head");
  await view.press("label:打开 Bot 客服一号");
  await view.waitFor("id:bot-capabilities");
  await view.press("id:bot-section-identity");
  await view.waitFor("id:bot-identity");
  await view.press("id:bot-rebind");
  await view.waitFor("id:bot-rebind-dialog");
  await view.press("text:请选择");
  await view.press("text:李娜 · 订单专家");
  await view.press("id:bot-rebind-confirm");
  await view.waitFor("text:/已改绑身份档案/");
});

test("TUI: Bot 页——归档后只读", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-bots");
  await view.waitFor("id:bots-head");
  await view.press("label:打开 Bot 客服一号");
  await view.waitFor("id:bot-capabilities");
  await view.press("label:更多 Bot 操作");
  await view.press("text:归档 Bot");
  await view.waitFor("id:bot-archive-dialog");
  await view.press("id:bot-archive-dialog-confirm");
  await view.waitFor("text:/Bot 已归档/");
  expect(view.texts()).toContain("已归档");
});

test("TUI: 团队页——使命保存走账本确认", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-teams");
  await view.waitFor("id:teams-head");
  await view.press("label:打开团队 客服团队");
  await view.waitFor("id:team-roster");
  await view.press("id:team-section-mission");
  await view.waitFor("id:team-mission");
  await view.type("id:team-mission-input", "更新后的使命：先查订单，再升级人工。");
  await view.waitFor("text:/有未保存的变更/");
  await view.press("id:team-save");
  await view.waitFor("text:确认 1 项变更");
  expect(view.texts().join("\n")).toContain("影响下一次启动");
  await view.press("id:team-ledger-confirm");
  await view.waitFor("text:/已登记 1 项变更/");
});

test("TUI: 运行页——泳道、失败横条、默认失败检查器与图切换", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-teams");
  await view.waitFor("id:teams-head");
  await view.press("label:打开团队 客服团队");
  await view.waitFor("id:team-roster");
  await view.press("id:team-start");
  await view.waitFor("id:team-start-dialog");
  await view.press("id:team-start-dialog-confirm");
  await waitRun(view);

  expect(view.shows("id:run-failure-banner")).toBe(true);
  expect(view.texts().join("\n")).toContain("售后升级 失败");
  expect(view.texts().join("\n")).toContain("缺「调用 Bot」能力");
  // Default selection is the failed node; the inspector names its owner.
  expect(view.shows("id:run-inspector-refusal")).toBe(true);
  expect(view.texts().join("\n")).toContain("对象归属者");
  // Tools come from the captain's own ledger.
  expect(view.texts()).toContain("运行事件");

  await view.press("id:run-mode-graph");
  await view.waitFor("id:run-graph");
  expect(view.texts()).toContain("下发使命");
  await view.press("id:run-mode-lanes");
  await waitRun(view);
});

test("TUI: 团队页——启动后出现最近运行入口", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-teams");
  await view.waitFor("id:teams-head");
  await view.press("label:打开团队 客服团队");
  await view.waitFor("id:team-roster");
  await view.press("id:team-start");
  await view.waitFor("id:team-start-dialog");
  await view.press("id:team-start-dialog-confirm");
  await waitRun(view);
  await view.press("id:run-team-section-runs");
  await view.waitFor("id:team-runs");
  expect(view.texts().join("\n")).toContain("运行 · ");
  await view.press("text:/运行 · /");
  await waitRun(view);
});

test("TUI: 运行页——停止二次确认与停止后的状态", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-teams");
  await view.waitFor("id:teams-head");
  await view.press("label:打开团队 客服团队");
  await view.waitFor("id:team-roster");
  await view.press("id:team-start");
  await view.waitFor("id:team-start-dialog");
  await view.press("id:team-start-dialog-confirm");
  await waitRun(view);
  await view.press("id:run-stop");
  await view.waitFor("id:run-stop-dialog");
  expect(view.texts().join("\n")).toContain("已完成的会话会保留");
  await view.press("id:run-stop-dialog-confirm");
  await view.waitFor("text:/已发出停止信号/");
  expect(view.texts().join("\n")).toContain("已发出停止信号");
  await view.waitFor("text:已停止");
});

test("TUI: 四档——1440 页边+检查器、1024 页边、768 摘要带+检查器入口、390 时间线", async () => {
  const wide = await app(1440);
  await wide.press("id:books-chapter-bots");
  await wide.waitFor("id:bots-head");
  await wide.press("label:打开 Bot 客服一号");
  await wide.waitFor("id:bot-spine");
  expect(wide.shows("id:bot-segment")).toBe(false);

  const mid = await app(1024);
  await mid.press("id:books-chapter-bots");
  await mid.waitFor("id:bots-head");
  await mid.press("label:打开 Bot 客服一号");
  await mid.waitFor("id:bot-spine");

  const pad = await app(768);
  await pad.press("id:books-chapter-bots");
  await pad.waitFor("id:bots-head");
  await pad.press("label:打开 Bot 客服一号");
  await pad.waitFor("id:bot-segment");
  expect(pad.shows("id:bot-spine")).toBe(false);

  const phone = await app(390);
  await phone.press("id:books-tab-bots");
  await phone.waitFor("id:bots-head");
  await phone.press("label:打开 Bot 客服一号");
  await phone.waitFor("id:bot-segment");
  expect(phone.shows("id:bot-spine")).toBe(false);
  expect(phone.shows("id:bot-mobile-bar")).toBe(true);
});

test("TUI: 运行页四档——768 检查器入口、390 时间线与检查器 Sheet", async () => {
  const pad = await app(768);
  await pad.press("id:books-chapter-teams");
  await pad.waitFor("id:teams-head");
  await pad.press("label:打开团队 客服团队");
  await pad.waitFor("id:team-roster");
  await pad.press("id:team-start");
  await pad.waitFor("id:team-start-dialog");
  await pad.press("id:team-start-dialog-confirm");
  await waitRun(pad);
  expect(pad.shows("id:run-inspector-entry")).toBe(true);
  await pad.press("id:run-open-inspector");
  await pad.waitFor("text:对象归属者");

  const phone = await app(390);
  await phone.press("id:books-tab-teams");
  await phone.waitFor("id:teams-head");
  await phone.press("label:打开团队 客服团队");
  await phone.waitFor("id:team-roster");
  await phone.press("id:team-start");
  await phone.waitFor("id:team-start-dialog");
  await phone.press("id:team-start-dialog-confirm");
  await phone.waitFor("id:run-timeline", { timeout: 20000 });
  expect(phone.texts()).toContain("查看原因");
  await phone.press("label:查看节点 售后升级，失败");
  await phone.waitFor("text:对象归属者");
  expect(phone.texts().join("\n")).toContain("缺「调用 Bot」能力");
}, 120_000);

test("TUI: 员工五态——加载 / 空 / 错 / 权限不足", async () => {
  const loading = await harness(1440, { scenario: "bot-loading", chapter: "bots" });
  await loading.waitFor("id:bots-index-loading");

  const empty = await harness(1440, { scenario: "bot-empty", chapter: "bots" });
  await empty.waitFor("id:bots-index-empty");
  expect(empty.texts()).toContain("还没有 AI 员工");

  const broken = await harness(1440, { scenario: "bot-error", chapter: "bots" });
  await broken.waitFor("id:bots-index-error");
  expect(broken.texts()).toContain("没能读到 Bot 列表");
  expect(broken.texts()).toContain("请求编号 k-test-1");

  const denied = await harness(1440, { scenario: "bot-permission", chapter: "bots" });
  await denied.waitFor("id:page-permission");
  expect(denied.texts().join("\n")).toContain("需要「查看 Bot」能力");
});

test("TUI: 团队五态——加载 / 空 / 错 / 权限不足", async () => {
  const loading = await harness(1440, { scenario: "team-loading", chapter: "teams" });
  await loading.waitFor("id:teams-index-loading");

  const empty = await harness(1440, { scenario: "team-empty", chapter: "teams" });
  await empty.waitFor("id:teams-index-empty");
  expect(empty.texts()).toContain("还没有团队");

  const broken = await harness(1440, { scenario: "team-error", chapter: "teams" });
  await broken.waitFor("id:teams-index-error");
  expect(broken.texts()).toContain("没能读到团队列表");
  expect(broken.texts()).toContain("请求编号 k-test-1");

  const denied = await harness(1440, { scenario: "team-permission", chapter: "teams" });
  await denied.waitFor("id:page-permission");
  expect(denied.texts().join("\n")).toContain("需要「查看团队」能力");
});

test("TUI: 运行五态——错与权限不足（结构可见）", async () => {
  const broken = await harness(1440, { scenario: "run-error", chapter: "run" });
  await broken.waitFor("id:run-error");
  expect(broken.texts()).toContain("没能读到这次运行");
  expect(broken.texts()).toContain("请求编号 k-test-1");

  const denied = await harness(1440, { scenario: "run-permission", chapter: "run" });
  await denied.waitFor("id:run-lanes");
  expect(denied.texts().join("\n")).toContain("只显示已经读到的部分");
  expect(denied.texts()).toContain("客服团队");
});

test("TUI: 员工 / 团队详情五态——错误带请求编号与重试", async () => {
  const bot = await harness(1440, { scenario: "bot-detail-error", chapter: "bots" });
  await bot.waitFor("id:bots-head");
  await bot.press("label:打开 Bot 客服一号");
  await bot.waitFor("id:bot-error");
  expect(bot.texts()).toContain("没能读到这个 Bot");
  expect(bot.texts()).toContain("请求编号 k-test-1");

  const team = await harness(1440, { scenario: "team-detail-error", chapter: "teams" });
  await team.waitFor("id:teams-head");
  await team.press("label:打开团队 客服团队");
  await team.waitFor("id:team-error");
  expect(team.texts()).toContain("没能读到这个团队");
  expect(team.texts()).toContain("请求编号 k-test-1");
});
