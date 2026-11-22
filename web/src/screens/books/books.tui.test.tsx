// TUI verification for the 账簿 shell and the identity/records chapters —
// change keel-bot-identity-access, A4.4a/A4.4c.
//
// The fixture transport carries the design board's sample deployment (eleven
// accounts, five permission groups, a week of audit rows), so the success cases
// drive the real screens and data layer through the app's own launch. The
// non-success states are a separate harness inside the app's module graph
// (`books.harness.tsx`), because a test's module graph is not the app's.
//
// Covered: the chapter band at 1440/1024/768/390, the default account/group/
// record selection, the account activity derived from the ledger, the role
// words, the permission matrix's ceiling micro-label and scope sheet, the
// ledger sheet (diff + signatory + high-risk tick + save), the records detail's
// missing key/owner/grantable/links, and the five states.
import { expect, open, test } from "nsh/test";

const app = (width = 1440) => open("../../../App.tsx", { width, host: { params: { fixture: true } } });
const harness = (width: number, params: Record<string, string>) =>
  open("./books.harness.tsx", { width, host: { params } });

test("TUI: 账簿壳——1440 身份默认选中当前账号，详情有角色与账本摘要", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-identity");
  await view.waitFor("id:identity-index");
  // 首屏不是空白：默认选中当前账号（李娜），而不是「选择一个账号」。
  await view.waitFor("id:account-head");
  expect(view.shows("id:accounts-welcome")).toBe(false);
  expect(view.texts()).toContain("李娜");
  expect(view.texts().join("\n")).toContain("3 个权限组");
  expect(view.texts().join("\n")).toContain("租户管理员");
  expect(view.texts().join("\n")).toContain("最近活动 今天");
  expect(view.shows("id:books-live")).toBe(true);
  expect(view.texts()).toContain("实时");
  // 账号页：唯一 H1 与章节索引。
  await view.press("id:account-section-capabilities");
  await view.waitFor("id:account-capabilities");
  const screen = view.texts().join("\n");
  expect(screen).toContain("查看会话");
  expect(screen).toContain("客服组");
  expect(screen).toContain("有效能力 = 这个账号所属权限组的并集");
});

test("TUI: 超管——统一叫超级管理员，概览显示全部能力", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-identity");
  await view.waitFor("id:identity-index");
  await view.press("label:打开账号 王强");
  await view.waitFor("id:account-head");
  const screen = view.texts().join("\n");
  expect(screen).toContain("超级管理员");
  expect(screen).not.toContain("部署管理员");
  expect(screen).toContain("全部能力（53 项）");
  expect(screen).toContain("身份由部署决定");
  expect(view.shows("id:account-add-group-head")).toBe(false);
});

test("TUI: 权限组矩阵——行高密度、单一上限说明、范围 Sheet 锁定与账本登记", async () => {
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
  const screen = view.texts().join("\n");
  // 一条全局上限说明 + 行内微标，不再逐行重复整句。
  expect(screen).toContain("你只能授予自己持有的范围");
  expect(screen).toContain("最高：整个租户");
  expect(screen).not.toContain("你最多只能授予");
  // 未改时没有「放弃」；组头也没有未保存徽标。
  expect(view.shows("id:group-discard")).toBe(false);
  expect(view.shows("id:group-unsaved-head")).toBe(false);
  expect(view.texts().join("\n")).toContain("无变更");

  // 范围是印章：打开 Sheet，超过自己上限的选项带锁与原因。
  await view.press("id:capability-scope-session.delete");
  await view.waitFor("text:选择这个组在这项能力上的范围。");
  expect(view.texts().join("\n")).toContain("最高 本团队，超出你的权限");
  await view.press("id:scope-choice-team");
  await view.waitFor("text:1 项未保存");
  expect(view.texts()).toContain("本次变更");
  expect(view.texts().join("\n")).toContain("未授予 → 本团队");
  // 再给一项租户级高危能力：Sheet 说明上限是「整个租户」。
  await view.press("id:capability-scope-account.permission.grant");
  await view.waitFor("text:选择这个组在这项能力上的范围。");
  expect(view.texts().join("\n")).toContain("最高 整个租户，超出你的权限");
  await view.press("id:scope-choice-tenant");
  await view.waitFor("text:2 项未保存");
  expect(view.shows("id:group-discard")).toBe(true);
  expect(view.shows("id:group-unsaved-head")).toBe(true);
  expect(view.texts().join("\n")).toContain("本次登记人：李娜 · 租户管理员");

  // 保存前必须过账本 Sheet：登记人 + 影响人数 + 高危勾选。
  await view.press("id:group-save");
  await view.waitFor("text:确认 2 项变更");
  const ledger = view.texts().join("\n");
  expect(ledger).toContain("本次登记人：李娜 · 租户管理员");
  expect(ledger).toContain("影响 8 位成员的下一次操作");
  expect(ledger).toContain("勾选后才能登记");
  await view.press("id:group-high-risk-account.permission.grant");
  await view.press("id:group-ledger-confirm");
  await view.waitFor("text:/已登记 2 项变更/");
  expect(view.texts()).toContain("整个租户");
  expect(view.texts().join("\n")).toContain("去记录查看");
});

test("TUI: 记录——默认打开首条，详情写缺什么/归属/可授予与对象链路", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-records");
  await view.waitFor("id:records-head");
  await view.waitFor("id:record-detail");
  expect(view.texts().join("\n")).toContain("调用 Bot 被拒");
  const first = view.texts().join("\n");
  expect(first).toContain("缺少能力");
  expect(first).toContain("bot.invoke");
  expect(first).toContain("对象归属");
  expect(first).toContain("王强");
  expect(first).toContain("可授予组/人");
  expect(first).toContain("客服组");
  expect(view.shows("id:record-open-object")).toBe(true);
  expect(view.shows("id:record-open-owner")).toBe(true);

  // 授权的对象是权限组：详情给「打开权限组」。
  await view.press("id:record-row-ev-4 >> text:保存权限组");
  await view.waitFor("id:record-detail");
  expect(view.texts().join("\n")).toContain("打开权限组");
  // 团队事件给「打开运行」。
  await view.press("id:record-row-ev-12 >> text:启动团队");
  await view.waitFor("id:record-detail");
  expect(view.texts().join("\n")).toContain("打开运行");
  // 超管操作有「超管」徽标。
  await view.press("id:record-row-ev-10 >> text:禁用账号");
  await view.waitFor("id:record-detail");
  expect(view.texts()).toContain("超级管理员");

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
  // 空态只在主区出现一次；索引列只留一句说明，不重复 CTA。
  expect(empty.texts().filter((text) => text === "新建账号").length).toBe(1);
  expect(empty.texts().join("\n")).not.toContain("共 0 个账号");
});

test("TUI: 五态·错——保留原因、请求编号与重试", async () => {
  const broken = await harness(1440, { scenario: "error", chapter: "identity" });
  await broken.waitFor("text:没能读到账号列表");
  expect(broken.texts()).toContain("重试");
  expect(broken.texts().join("\n")).toContain("请求编号 req-42");
  // 错误态不渲染空态；右栏说明读取失败，索引列提示读取后再新建。
  expect(broken.texts().join("\n")).toContain("账号列表读取失败");
  expect(broken.texts().join("\n")).not.toContain("还没有人加入这台部署");
  expect(broken.texts().join("\n")).toContain("读取完成后再新建账号");
});

test("TUI: 五态·权限不足——章节门给出缺什么 / 找谁 / 下一步", async () => {
  const denied = await harness(1440, { scenario: "permission", chapter: "identity" });
  await denied.waitFor("id:page-permission");
  expect(denied.texts()).toContain("你没有查看这个页面的权限");
  expect(denied.texts().join("\n")).toContain("需要「查看账号」能力");
  expect(denied.texts()).toContain("复制申请说明");
});

test("TUI: 权限矩阵五态——加载 / 空 / 错 / 只读", async () => {
  const loading = await harness(1440, { scenario: "group-loading", section: "groups", object: "grp-support" });
  await loading.waitFor("id:group-loading");
  expect(loading.shows("id:group-loading")).toBe(true);
  expect(loading.texts().join("\n")).toContain("读取中…");
  expect(loading.texts().join("\n")).not.toContain("成员与能力未读到");

  const empty = await harness(1440, { scenario: "group-empty", section: "groups", object: "grp-support" });
  await empty.waitFor("id:group-matrix-empty");
  expect(empty.texts().join("\n")).toContain("这个组还没有任何能力");

  const broken = await harness(1440, { scenario: "group-error", section: "groups", object: "grp-support" });
  await broken.waitFor("id:group-error");
  expect(broken.texts().join("\n")).toContain("没能读到这个权限组");
  expect(broken.texts()).toContain("重试");
  expect(broken.texts().join("\n")).toContain("请求编号 req-group");

  const readonly = await harness(1440, { scenario: "group-readonly", section: "groups", object: "grp-support" });
  await readonly.waitFor("id:group-matrix");
  expect(readonly.texts().join("\n")).toContain("你可以查看，但不能修改");
  expect(readonly.shows("id:group-discard")).toBe(false);
});

test("TUI: 记录五态——空 / 错 / 权限不足", async () => {
  const empty = await harness(1440, { scenario: "empty", chapter: "records" });
  await empty.waitFor("id:records-empty");
  expect(empty.texts()).toContain("还没有需要记录的操作");

  const broken = await harness(1440, { scenario: "records-error", chapter: "records" });
  await broken.waitFor("id:records-error");
  expect(broken.texts()).toContain("没能读到记录");
  expect(broken.texts().join("\n")).toContain("请求编号 req-records");

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
  expect(wide.shows("id:account-head")).toBe(true);

  const mid = await app(1024);
  await mid.press("id:books-chapter-identity");
  await mid.waitFor("id:accounts-head");
  expect(mid.shows("id:identity-index")).toBe(false);
  expect(mid.shows("id:accounts-table")).toBe(true);
  // 1024 的权限组：索引表 → 对象页矩阵。
  await mid.press("id:identity-segment-groups");
  await mid.waitFor("id:groups-head");
  expect(mid.shows("id:groups-table")).toBe(true);
  const groupsHeader = mid.texts().join("\n");
  expect(groupsHeader).toContain("权限组");
  expect(groupsHeader).toContain("类型");
  expect(groupsHeader).toContain("成员");
  expect(groupsHeader).toContain("最近修改");
  await mid.press("label:打开权限组 客服组");
  await mid.waitFor("id:group-matrix");
  expect(mid.texts().join("\n")).toContain("最高：本团队");
  // 1024 记录：表格保留，详情是抽屉而不是第三列。
  await mid.press("id:books-chapter-records");
  await mid.waitFor("id:records-head");
  expect(mid.shows("id:records-table")).toBe(true);
  expect(mid.texts().join("\n")).not.toContain("缺少能力");
  await mid.press("id:record-row-ev-1 >> text:调用 Bot 被拒：缺「调用 Bot」（bot.invoke）");
  await mid.waitFor("text:记录详情");
  expect(mid.texts().join("\n")).toContain("缺少能力");

  const pad = await app(768);
  await pad.press("id:books-chapter-identity");
  await pad.waitFor("id:accounts-head");
  expect(pad.shows("id:identity-index")).toBe(false);
  expect(pad.shows("id:accounts-table")).toBe(true);
  // 768 的记录页用卡片 + 详情 Sheet，不把 5 列表与 330 检查器硬塞进 720。
  await pad.press("id:books-chapter-records");
  await pad.waitFor("id:records-head");
  expect(pad.shows("id:records-table")).toBe(false);
  expect(pad.texts().join("\n")).toContain("调用 Bot 被拒");
  await pad.press("id:record-card-ev-1");
  await pad.waitFor("text:记录详情");
  expect(pad.texts().join("\n")).toContain("缺少能力");

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

test("TUI: 390 账号——危险操作走溢出菜单，二次确认后才禁用", async () => {
  const phone = await app(390);
  await phone.press("id:books-tab-identity");
  await phone.waitFor("id:identity-segment");
  await phone.press("label:打开账号 李娜");
  await phone.waitFor("id:account-head");
  await phone.press("id:account-more-trigger");
  await phone.waitFor("id:account-disable-menu");
  expect(phone.texts()).toContain("禁用账号");
  await phone.press("id:account-disable-menu");
  await phone.waitFor("id:account-disable-dialog");
  const confirm = phone.texts().join("\n");
  expect(confirm).toContain("禁用账号？");
  expect(confirm).toContain("全部会话");
});

test("TUI: 账号最近活动——审计不可读时只说明一次，不逐行写「未记录」", async () => {
  const view = await harness(1024, { scenario: "unavailable", chapter: "identity" });
  await view.waitFor("id:accounts-head");
  const screen = view.texts().join("\n");
  expect(screen).toContain("活动追踪待接入");
  expect(screen).not.toContain("最近活动未记录");
  expect(screen.split("活动追踪待接入").length - 1).toBe(1);
});

test("TUI: 内置权限组——底部只说只读与另存为副本，没有登记人/保存", async () => {
  const view = await app(1440);
  await view.press("id:books-chapter-identity");
  await view.waitFor("id:identity-index");
  await view.press("id:identity-segment-groups");
  await view.waitFor("text:超级管理员");
  await view.press("label:打开权限组 超级管理员");
  await view.waitFor("id:group-matrix");
  expect(view.shows("id:group-readonly-signoff")).toBe(true);
  const screen = view.texts().join("\n");
  expect(screen).toContain("只读 · 另存为副本后可修改");
  expect(screen).not.toContain("本次登记人");
  expect(view.shows("id:group-save")).toBe(false);
});
