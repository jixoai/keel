// TUI verification for the 账簿 shell and the identity/records chapters —
// change keel-bot-identity-access, A4.4a.
//
// The fixture transport carries the design board's sample deployment (twelve
// accounts, five permission groups, a week of audit rows), so the success cases
// drive the real screens and data layer through the app's own launch. The
// non-success states are a separate harness inside the app's module graph
// (`books.harness.tsx`), because a test's module graph is not the app's.
//
// Covered: the chapter band at 1440/1024/768/390, the account index/detail with
// capability sources, the permission matrix's scope ceiling, the ledger sheet
// (diff + signatory + high-risk tick + save), the records read/filter/detail,
// and the five states (loading / empty / error / permission / unavailable).
import { expect, open, test } from "nsh/test";

const app = (width = 1440) => open("../../../App.tsx", { width, host: { params: { fixture: true } } });
const harness = (width: number, params: Record<string, string>) =>
  open("./books.harness.tsx", { width, host: { params } });

test("TUI: 账簿壳——1440 章节带 + 身份索引 + 账号详情的能力来源", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-identity");
  await view.waitFor("id:identity-index");
  // 1440 的身份章是索引列 + 纸面（不做 drill-in），不是五列表。
  expect(view.texts()).toContain("李娜");
  expect(view.texts()).toContain("客服一号");
  expect(view.texts()).toContain("选择一个账号");
  // 账号页：唯一 H1 与章节索引。
  await view.press("label:打开账号 李娜");
  await view.waitFor("id:account-head");
  expect(view.texts()).toContain("李娜");
  expect(view.texts().join("\n")).toContain("3 个权限组");
  await view.press("id:account-section-capabilities");
  await view.waitFor("id:account-capabilities");
  const screen = view.texts().join("\n");
  expect(screen).toContain("查看会话");
  expect(screen).toContain("客服组");
  expect(screen).toContain("有效能力 = 这个账号所属权限组的并集");
});

test("TUI: 权限组矩阵——范围上限禁用、账本 Sheet 高危确认后保存并登记", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-identity");
  await view.waitFor("id:identity-index");
  await view.press("id:identity-segment-groups");
  await view.waitFor("text:客服组");
  await view.press("label:打开权限组 客服组");
  await view.waitFor("id:group-matrix");
  expect(view.texts()).toContain("调用 Bot");
  expect(view.texts()).toContain("bot.invoke");
  expect(view.texts()).toContain("已授 6 项");
  // 账号域的 ceiling：李娜持有 tenant，所以「全部」不可选，行尾写原因。
  expect(view.texts().join("\n")).toContain("你最多只能授予「整个租户」");

  // 给客服组加一项高危能力（account.permission.grant，tenant）→ 未保存。
  await view.press("id:capability-row-account.permission.grant >> text:未授予");
  // 弹层里的「整个租户」是最后一个同名单元格（上面还有三个印章）。
  await view.press("text:整个租户 >> nth=3");
  await view.waitFor("text:1 项未保存");
  expect(view.texts()).toContain("本次变更");
  expect(view.texts().join("\n")).toContain("未授予 → 整个租户");

  // 保存前必须过账本 Sheet：登记人 + 影响人数 + 高危勾选。
  await view.press("id:group-save");
  await view.waitFor("text:确认 1 项变更");
  const ledger = view.texts().join("\n");
  expect(ledger).toContain("本次登记人：李娜");
  expect(ledger).toContain("影响 8 位成员的下一次操作");
  expect(ledger).toContain("勾选后才能登记");
  await view.press("id:group-high-risk-account.permission.grant");
  await view.press("id:group-ledger-confirm");
  await view.waitFor("text:/已保存并登记 1 项变更/");
  expect(view.texts()).toContain("整个租户");
});

test("TUI: 记录——筛选、事件账、详情与导出", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-records");
  await view.waitFor("id:records-head");
  expect(view.texts()).toContain("调用 Bot 被拒");
  expect(view.texts()).toContain("拒绝");
  await view.press("id:record-row-ev-1 >> text:调用 Bot 被拒");
  await view.waitFor("id:record-detail");
  const detail = view.texts().join("\n");
  expect(detail).toContain("请求编号");
  expect(detail).toContain("k-7f31");
  expect(detail).toContain("拒绝");
  await view.press("id:records-export");
  await view.waitFor("text:/已复制 12 行 CSV|复制失败/");
});

test("TUI: 五态·加载——账号读未返回时是骨架，不是空态", async () => {
  const loading = await harness(1440, { scenario: "loading", chapter: "identity" });
  await loading.waitFor("id:accounts-index-loading");
  expect(loading.shows("id:accounts-index-loading")).toBe(true);
});

test("TUI: 五态·空——账号索引给下一步，不是「暂无数据」", async () => {
  const empty = await harness(1440, { scenario: "empty", chapter: "identity" });
  await empty.waitFor("id:accounts-index-empty");
  expect(empty.texts()).toContain("还没有人加入这台部署");
  expect(empty.texts()).toContain("新建账号");
});

test("TUI: 五态·错——保留原因与重试", async () => {
  const broken = await harness(1440, { scenario: "error", chapter: "identity" });
  await broken.waitFor("text:没能读到账号列表");
  expect(broken.texts()).toContain("重试");
});

test("TUI: 五态·权限不足——章节门给出缺什么 / 找谁 / 下一步", async () => {
  const denied = await harness(1440, { scenario: "permission", chapter: "identity" });
  await denied.waitFor("id:page-permission");
  expect(denied.texts()).toContain("你没有查看这个页面的权限");
  expect(denied.texts().join("\n")).toContain("需要「查看账号」能力");
  expect(denied.texts()).toContain("复制申请说明");
});

test("TUI: 记录五态——空 / 错 / 权限不足", async () => {
  const empty = await harness(1440, { scenario: "empty", chapter: "records" });
  await empty.waitFor("id:records-empty");
  expect(empty.texts()).toContain("还没有需要记录的操作");

  const broken = await harness(1440, { scenario: "records-error", chapter: "records" });
  await broken.waitFor("id:records-error");
  expect(broken.texts()).toContain("没能读到记录");

  const denied = await harness(1440, { scenario: "permission", chapter: "records" });
  await denied.waitFor("id:page-permission");
  expect(denied.texts().join("\n")).toContain("需要「查看记录」能力");
});

test("TUI: 记录——路由未接入时是待接入面，不是示例账本", async () => {
  const view = await harness(1440, { scenario: "unavailable", chapter: "records" });
  await view.waitFor("id:records-unavailable");
  expect(view.texts()).toContain("记录接口尚未接入");
  expect(view.texts().join("\n")).not.toContain("调用 Bot 被拒");
});

test("TUI: 四档——1440 有身份索引；1024/768 全宽；390 底部五 tab 与卡片", async () => {
  const wide = await app(1440);
  await wide.press("id:books-chapter-identity");
  await wide.waitFor("id:identity-index");
  expect(wide.shows("id:accounts-table")).toBe(false);

  const mid = await app(1024);
  await mid.press("id:books-chapter-identity");
  await mid.waitFor("id:accounts-head");
  expect(mid.shows("id:identity-index")).toBe(false);
  expect(mid.shows("id:accounts-table")).toBe(true);

  const pad = await app(768);
  await pad.press("id:books-chapter-identity");
  await pad.waitFor("id:accounts-head");
  expect(pad.shows("id:identity-index")).toBe(false);
  expect(pad.shows("id:accounts-table")).toBe(true);
  // 768 的记录页用卡片 + 详情 Sheet，不把 5 列表与 310 检查器硬塞进 720。
  await pad.press("id:books-chapter-records");
  await pad.waitFor("id:records-head");
  expect(pad.shows("id:records-table")).toBe(false);
  expect(pad.texts()).toContain("调用 Bot 被拒");

  const phone = await app(390);
  await phone.waitFor("id:books-tabbar");
  await phone.press("id:books-tab-identity");
  await phone.waitFor("id:identity-segment");
  expect(phone.shows("id:books-tabbar")).toBe(true);
  expect(phone.texts()).toContain("李娜");
  await phone.press("label:打开账号 李娜");
  await phone.waitFor("id:account-head");
  expect(phone.shows("id:books-back")).toBe(true);
});
