// TUI gate for the settings「团队」section (change keel-agent-team-v1).
//
// The mandatory captain is the fact this gate protects: the card owns a captain
// slot, the canvas cannot save without one, and the captain never appears as a
// removable member. The canvas's member library/new-Agent flow and the Team CLI
// panel are gated here too. The harness answers the registry through the store's
// transport seam, so the real `KeelHttp` parsing runs.
import { expect, open, test } from "nsh/test";

const HARNESS = "./settings-teams.harness.tsx";

const joined = (app: { texts(): string[] }): string => app.texts().join("\n");

const openSection = (scenario: string) => open(HARNESS, { host: { params: { scenario } } });
const openList = () => openSection("list");

test("TUI: 团队——卡片明确队长、成员数与成员头像", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "list" } } });

  await app.waitFor("id:team-card-team_brand");
  const texts = app.texts();
  expect(texts).toContain("品牌发布小组");
  // The captain is its own slot, not a line borrowed from the member list.
  expect(app.shows("id:team-captain-slot-team_brand")).toBe(true);
  expect(joined(app)).toContain("队长");
  expect(texts).toContain("设计师");
  expect(joined(app)).toContain("成员 2 人");
  expect(joined(app)).toContain("成员 0 人");
  expect(joined(app)).toContain("只有队长");
  expect(joined(app)).toContain("2 支团队 · 队长必选、唯一");
  // The second team's captain is a different agent: the slot is per card.
  expect(app.shows("id:team-captain-slot-team_research")).toBe(true);
  expect(texts).toContain("研究员");
  // The per-team Team CLI switch is a card fact, not an invented capability.
  expect(joined(app)).toContain("Team CLI 已开");
  expect(joined(app)).toContain("允许：member.add · member.remove");
  expect(joined(app)).toContain("Team CLI 已关");
  expect(joined(app)).toContain("队长不能通过 CLI 改团队");
  expect(app.shows("label:编辑团队 品牌发布小组")).toBe(true);
  expect(app.shows("label:删除团队 资料组")).toBe(true);
});

test("TUI: 团队——空态说明「先有 Agent 才能组队」", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "empty" } } });

  await app.waitFor("text:/还没有团队，创建一个/");
  expect(joined(app)).toContain("先在「Agents」里创建 Agent");
  expect(app.shows("label:空态新建团队")).toBe(true);
});

test("TUI: 团队——无队长时保存禁用并说明原因", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "list" } } });
  await app.waitFor("id:team-card-team_brand");

  await app.press("id:team-create");
  await app.waitFor("id:team-dialog");
  expect(joined(app)).toContain("团队必须有队长");
  expect(app.enabled("id:team-save")).toBe(false);

  await app.type("id:team-name", "新小组");
  // Still disabled: a name is not a captain.
  expect(app.enabled("id:team-save")).toBe(false);

  await app.press("text:选择队长（必选）");
  await app.waitFor("text:设计师");
  await app.press("text:设计师");
  expect(app.enabled("id:team-save")).toBe(true);
});

test("TUI: 团队——保存成功后卡片出现且队长行正确", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "list" } } });
  await app.waitFor("id:team-card-team_brand");

  await app.press("id:team-create");
  await app.waitFor("id:team-dialog");
  await app.type("id:team-name", "新小组");
  await app.type("id:team-description", "负责新渠道试投");
  await app.press("text:选择队长（必选）");
  await app.waitFor("text:设计师");
  await app.press("text:设计师");
  await app.press("id:team-save");

  await app.waitFor("id:team-dialog", { state: "gone" });
  await app.waitFor("id:team-card-team_new_1");
  expect(joined(app)).toContain("新小组");
  expect(joined(app)).toContain("负责新渠道试投");
  expect(app.shows("id:team-captain-slot-team_new_1")).toBe(true);
  expect(joined(app)).toContain("设计师");
  expect(joined(app)).toContain("3 支团队 · 队长必选、唯一");
});

test("TUI: 团队画布——队长在独立槽位，不作为可移除成员", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "list" } } });
  await app.waitFor("id:team-card-team_brand");

  await app.press("label:编辑团队 品牌发布小组");
  await app.waitFor("id:team-dialog");
  await app.waitFor("id:canvas-captain-seat");
  expect(joined(app)).toContain("队长槽");
  expect(joined(app)).toContain("设计师");
  // Editing starts from the stored document, not from an empty form: the
  // canvas re-keys its dialog on the open transition so the uncontrolled
  // inputs mount with the fetched values.
  expect(app.value("id:team-name")).toBe("品牌发布小组");
  expect(app.value("id:team-description")).toBe("负责新品发布的全链路交付");
  // The captain seat never grows a remove control; the two members do.
  expect(app.shows("id:canvas-remove-agent_design")).toBe(false);
  expect(app.shows("id:canvas-remove-agent_research")).toBe(true);
  expect(app.shows("id:canvas-remove-agent_ops")).toBe(true);
  expect(joined(app)).toContain("队长始终在队内，不能当作普通成员移除");
});

test("TUI: 团队——加载态：骨架与说明在场，主操作禁用", async () => {
  const app = await openSection("loading");

  await app.waitFor("id:teams-loading");
  expect(joined(app)).toContain("正在读取团队清单");
  expect(app.shows("label:新建团队")).toBe(true);
  expect(app.enabled("id:team-create")).toBe(false);
  expect(app.shows("id:team-card-team_brand")).toBe(false);
});

test("TUI: 团队——错误态：服务端原因逐字呈现，可重试", async () => {
  const app = await openSection("error");

  await app.waitFor("id:teams-error");
  expect(joined(app)).toContain("团队注册表暂时不可用");
  expect(app.shows("label:加载失败")).toBe(true);
  expect(app.shows("label:重试")).toBe(true);
  expect(app.shows("text:/还没有团队，创建一个/")).toBe(false);
});

test("TUI: 团队——权限不足（403）有自己的面，不冒充加载失败", async () => {
  const app = await openSection("permission");

  await app.waitFor("id:settings-permission");
  expect(joined(app)).toContain("没有查看团队注册表的权限");
  expect(joined(app)).toContain("当前账号没有 team.read 权限");
  expect(app.shows("id:teams-error")).toBe(false);
  expect(app.shows("label:加载失败")).toBe(false);
  expect(app.shows("label:重试")).toBe(true);
});

test("TUI: 团队——保存中：字段与按钮锁定，保存按钮带进行中文案", async () => {
  const app = await openSection("slow");
  await app.waitFor("id:team-card-team_brand");

  await app.press("id:team-create");
  await app.waitFor("id:team-dialog");
  await app.type("id:team-name", "慢速小组");
  await app.press("text:选择队长（必选）");
  await app.waitFor("text:设计师");
  await app.press("text:设计师");
  // The new controls take part in the same lock: defaults pickers and the
  // member actions cannot be edited while the write is on the wire.
  await app.press("id:canvas-add-agent_research");
  await app.waitFor("id:canvas-member-agent_research");
  await app.press("id:team-save");

  await app.waitFor("text:/保存中…/");
  expect(app.enabled("id:team-save")).toBe(false);
  expect(app.enabled("id:team-name")).toBe(false);
  expect(app.enabled("id:team-cancel")).toBe(false);
  // The native choice controls dim their inner field and expose no pressable
  // view while disabled; the refused press is what proves the lock (an
  // enabled control would resolve the step).
  await expect(app.press("id:team-defaults-tools")).rejects.toMatchObject({ code: "ERR_NSH_TEST_NOT_FOUND" });
  await expect(app.press("id:team-defaults-model")).rejects.toMatchObject({ code: "ERR_NSH_TEST_NOT_FOUND" });
  expect(app.enabled("id:canvas-member-edit-agent_research")).toBe(false);
  expect(app.enabled("id:canvas-member-replace-agent_research")).toBe(false);
  expect(app.enabled("id:canvas-remove-agent_research")).toBe(false);
  // The close control is the fourth way to discard a write in flight; it is
  // gone while the request owns the dialog.
  expect(app.shows("label:关闭")).toBe(false);
  expect(app.shows("id:team-dialog")).toBe(true);
  // The harness holds the response until this event: the assertions above ran
  // inside the saving state, not inside a wall-clock window.
  await app.host.send("harness.release-save");
  await app.waitFor("id:team-dialog", { state: "gone" });
  await app.waitFor("id:team-card-team_new_1");
});

test("TUI: 团队——Agent 注册表不可读时队长显示可读占位，不泄漏裸 id", async () => {
  const app = await openSection("agents-permission");

  await app.waitFor("id:team-card-team_brand");
  expect(joined(app)).toContain("未载入的 Agent");
  expect(joined(app)).not.toContain("agent_design");
});

test("TUI: 团队——保存失败：原因可读，名字与队长保留", async () => {
  const app = await openSection("refuse");
  await app.waitFor("id:team-card-team_brand");

  await app.press("id:team-create");
  await app.waitFor("id:team-dialog");
  await app.type("id:team-name", "新小组");
  await app.press("text:选择队长（必选）");
  await app.waitFor("text:设计师");
  await app.press("text:设计师");
  await app.press("id:team-save");

  await app.waitFor("id:team-save-error");
  expect(joined(app)).toContain("队长 agent_design 当前不可用");
  expect(app.shows("id:team-dialog")).toBe(true);
  expect(app.value("id:team-name")).toBe("新小组");
});

test("TUI: 团队——缺队长是阻塞态：卡片与画布都说不能启动/保存", async () => {
  const app = await openSection("no-captain");
  await app.waitFor("id:team-card-team_brand");

  expect(app.shows("id:team-captain-slot-team_brand")).toBe(true);
  expect(joined(app)).toContain("缺少队长");
  expect(joined(app)).toContain("缺队长 · 不能启动");
  expect(joined(app)).toContain("这支团队不能启动");

  await app.press("label:编辑团队 品牌发布小组");
  await app.waitFor("id:team-dialog");
  await app.waitFor("id:canvas-captain-blocker");
  expect(joined(app)).toContain("缺队长：团队不能保存，也不能用于会话启动。");
  expect(app.enabled("id:team-save")).toBe(false);
  expect(app.shows("id:canvas-captain-clear")).toBe(false);
});

test("TUI: 团队画布——成员库加入/移出只改草稿，队长不在库内", async () => {
  const app = await openSection("list");
  await app.waitFor("id:team-card-team_research");

  await app.press("label:编辑团队 资料组");
  await app.waitFor("id:team-dialog");
  // The captain (研究员) is not offered as an ordinary member; the other two
  // stored Agents are.
  expect(app.shows("id:canvas-library-agent_research")).toBe(false);
  expect(app.shows("id:canvas-library-agent_design")).toBe(true);
  expect(app.shows("id:canvas-library-agent_ops")).toBe(true);

  await app.press("id:canvas-add-agent_design");
  await app.waitFor("id:canvas-member-agent_design");
  expect(joined(app)).toContain("1 位协作者（队长之外）");
  expect(app.shows("id:canvas-library-agent_design")).toBe(false);

  await app.press("id:canvas-remove-agent_design");
  await app.waitFor("id:canvas-member-agent_design", { state: "gone" });
  expect(joined(app)).toContain("只有队长");
  expect(joined(app)).toContain("从下方成员库添加协作者");
});

test("TUI: 团队画布——加入成员后保存落库，卡片成员数更新", async () => {
  const app = await openList();
  await app.waitFor("id:team-card-team_research");

  await app.press("label:编辑团队 资料组");
  await app.waitFor("id:team-dialog");
  await app.press("id:canvas-add-agent_design");
  await app.waitFor("id:canvas-member-agent_design");

  await app.press("id:team-save");
  await app.waitFor("id:team-dialog", { state: "gone" });
  await app.waitFor("id:team-card-team_research");
  expect(joined(app)).toContain("成员 1 人");
});

test("TUI: 团队画布——编辑名字保存走团队文档，卡片同步", async () => {
  const app = await openList();
  await app.waitFor("id:team-card-team_brand");

  await app.press("label:编辑团队 品牌发布小组");
  await app.waitFor("id:team-dialog");
  await app.clear("id:team-name");
  await app.type("id:team-name", "品牌发布小组·改");
  await app.press("id:team-save");

  await app.waitFor("id:team-dialog", { state: "gone" });
  await app.waitFor("id:team-card-team_brand");
  expect(joined(app)).toContain("品牌发布小组·改");
  expect(joined(app)).toContain("负责新品发布的全链路交付");
});

test("TUI: 团队画布——更换队长保存后卡片队长槽同步，队长不留在成员列表", async () => {
  const app = await openList();
  await app.waitFor("id:team-card-team_brand");

  await app.press("label:编辑团队 品牌发布小组");
  await app.waitFor("id:team-dialog");
  await app.press("label:选择队长");
  await app.waitFor("text:运营");
  await app.press("text:运营");
  // The new captain leaves the roster, and no seat can hold it twice.
  expect(app.shows("id:canvas-remove-agent_ops")).toBe(false);
  await app.press("id:team-save");

  await app.waitFor("id:team-dialog", { state: "gone" });
  await app.waitFor("id:team-card-team_brand");
  expect(joined(app)).toContain("运营");
  expect(joined(app)).toContain("成员 1 人");
});

test("TUI: 团队画布——新建 Agent 落在成员库，空队长槽自动坐下", async () => {
  const app = await openList();

  await app.press("id:team-create");
  await app.waitFor("id:team-dialog");
  await app.press("id:canvas-create-agent");
  await app.waitFor("id:canvas-new-agent");

  await app.type("id:canvas-new-agent-name", "发布助手");
  await app.type("id:canvas-new-agent-description", "负责发布物料与渠道沟通");
  await app.press("id:canvas-new-agent-save");

  await app.waitFor("id:canvas-new-agent", { state: "gone" });
  await app.waitFor("text:发布助手");
  // The empty captain slot takes the just-created Agent: it sits in the slot,
  // not in the library, and the team can now save.
  expect(joined(app)).toContain("队长槽");
  expect(app.shows("id:canvas-library-agent_new_1")).toBe(false);
  expect(app.enabled("id:team-save")).toBe(true);
});

test("TUI: 团队画布——改名+加成员保存保留 CLI label/description 与 inline 席位", async () => {
  const app = await openSection("seats");
  await app.waitFor("id:team-card-team_seats");
  expect(joined(app)).toContain("初稿评审");
  expect(joined(app)).toContain("内联席位");

  await app.press("label:编辑团队 席位组");
  await app.waitFor("id:team-dialog");
  // The inline seat keeps its own text: the form edits membership, not the
  // seat's label/description/definition.
  expect(joined(app)).toContain("团队内定义");
  // Re-seat an existing member: the draft is a bare agentId again, so the
  // saved seat must merge back onto the stored one (label/description kept).
  await app.press("id:canvas-remove-agent_research");
  await app.waitFor("id:canvas-member-agent_research", { state: "gone" });
  await app.press("id:canvas-add-agent_research");
  await app.waitFor("id:canvas-member-agent_research");
  await app.press("id:canvas-add-agent_ops");
  await app.waitFor("id:canvas-member-agent_ops");
  await app.clear("id:team-name");
  await app.type("id:team-name", "席位组·改");
  await app.press("id:team-save");

  await app.waitFor("id:team-dialog", { state: "gone" });
  await app.waitFor("id:team-card-team_seats");
  expect(joined(app)).toContain("席位组·改");
  // The harness' PUT refuses a seat whose label/description/inline is lost or
  // whose agentId became blank, so this remaining text is the data contract.
  expect(joined(app)).toContain("初稿评审");
  expect(joined(app)).toContain("内联席位");
  expect(joined(app)).toContain("成员 3 人");
  expect(app.shows("id:team-save-error")).toBe(false);
});

test("TUI: 团队画布——加载态：成员库说明正在读取，主操作禁用", async () => {
  const app = await openSection("agents-loading");
  await app.waitFor("id:team-card-team_brand");

  await app.press("id:team-create");
  await app.waitFor("id:team-dialog");
  await app.waitFor("id:canvas-agents-loading");
  expect(joined(app)).toContain("正在读取 Agent 成员库");
  expect(app.enabled("id:team-save")).toBe(false);
});

test("TUI: 团队画布——错误态：服务端原因逐字呈现，可重试", async () => {
  const app = await openSection("agents-error");
  await app.waitFor("id:team-card-team_brand");

  await app.press("id:team-create");
  await app.waitFor("id:team-dialog");
  await app.waitFor("id:canvas-agents-error");
  expect(joined(app)).toContain("Agent 注册表暂时不可用");
  expect(app.shows("label:重试")).toBe(true);
});

test("TUI: 团队画布——成员库 403 是权限面，不冒充加载失败", async () => {
  const app = await openSection("agents-permission");
  await app.waitFor("id:team-card-team_brand");

  await app.press("id:team-create");
  await app.waitFor("id:team-dialog");
  await app.waitFor("id:canvas-agents-permission");
  expect(joined(app)).toContain("没有查看 Agent 成员库的权限");
  expect(joined(app)).toContain("当前账号没有 agent.read 权限");
  expect(app.shows("id:canvas-agents-error")).toBe(false);
});

test("TUI: 团队画布——空成员库引导先建 Agent", async () => {
  const app = await openSection("agents-empty");
  await app.waitFor("id:team-card-team_brand");

  await app.press("id:team-create");
  await app.waitFor("id:team-dialog");
  await app.waitFor("id:canvas-agents-empty");
  expect(joined(app)).toContain("还没有 Agent");
  expect(app.shows("label:空态新建 Agent")).toBe(true);
  await app.press("id:canvas-create-agent-empty");
  await app.waitFor("id:canvas-new-agent");
});

test("TUI: 团队——删除二次确认后卡片消失，列表数量同步", async () => {
  const app = await openList();
  await app.waitFor("id:team-card-team_research");

  await app.press("label:删除团队 资料组");
  await app.waitFor("id:team-delete-dialog");
  expect(joined(app)).toContain("删除团队 资料组？");
  await app.press("label:确认删除团队 资料组");

  await app.waitFor("id:team-delete-dialog", { state: "gone" });
  await app.waitFor("id:team-card-team_research", { state: "gone" });
  await app.waitFor("id:team-card-team_brand");
  expect(joined(app)).toContain("1 支团队 · 队长必选、唯一");
});

test("TUI: 团队——删除被会话引用时 Dialog 留场并逐字给出原因", async () => {
  const app = await openSection("referenced");
  await app.waitFor("id:team-card-team_brand");

  await app.press("label:删除团队 品牌发布小组");
  await app.waitFor("id:team-delete-dialog");
  await app.press("label:确认删除团队 品牌发布小组");

  await app.waitFor("id:team-delete-error");
  expect(joined(app)).toContain("删除失败：团队「品牌发布小组」正在运行任务，暂不能删除。");
  expect(app.shows("id:team-delete-dialog")).toBe(true);
  expect(app.shows("id:team-card-team_brand")).toBe(true);
});

test("TUI: 团队画布——默认工具/模型覆盖：预填、只改名字保留、保存后回读", async () => {
  const app = await openSection("defaults");
  await app.waitFor("id:team-card-team_defaults");

  await app.press("label:编辑团队 默认组");
  await app.waitFor("id:team-dialog");
  await app.waitFor("id:canvas-defaults");
  // The stored defaults are the draft's starting point, not an empty form.
  expect(joined(app)).toContain("workspace.read");
  expect(joined(app)).toContain("Qwen3.8 Flash · S2API");

  // A name-only edit must not wipe the defaults the form did not touch.
  await app.clear("id:team-name");
  await app.type("id:team-name", "默认组·改");
  await app.press("id:team-save");
  await app.waitFor("id:team-dialog", { state: "gone" });
  await app.waitFor("id:team-card-team_defaults");
  expect(joined(app)).toContain("默认组·改");

  await app.press("label:编辑团队 默认组·改");
  await app.waitFor("id:team-dialog");
  expect(joined(app)).toContain("workspace.read");
  expect(joined(app)).toContain("Qwen3.8 Flash · S2API");

  // Add a tool and replace the model override; both are canvas drafts until
  // 保存.
  await app.type("id:team-defaults-tools", "workspace.write");
  await app.press("text:workspace.write");
  await app.press("text:Qwen3.8 Flash · S2API");
  await app.waitFor("text:DeepSeek V4.1 Flash · S2API");
  await app.press("text:DeepSeek V4.1 Flash · S2API");
  await app.press("id:team-save");

  await app.waitFor("id:team-dialog", { state: "gone" });
  await app.waitFor("id:team-card-team_defaults");
  await app.press("label:编辑团队 默认组·改");
  await app.waitFor("id:team-dialog");
  // Read back from the saved document: both tools and the new override.
  expect(joined(app)).toContain("workspace.read");
  expect(joined(app)).toContain("workspace.write");
  expect(joined(app)).toContain("DeepSeek V4.1 Flash · S2API");
  expect(joined(app)).not.toContain("Qwen3.8 Flash · S2API");
});

test("TUI: 团队画布——成员 label/description 可编辑（含 inline 席位）并保存回读", async () => {
  const app = await openSection("seats-edit");
  await app.waitFor("id:team-card-team_seats");

  await app.press("label:编辑团队 席位组");
  await app.waitFor("id:team-dialog");
  await app.press("id:canvas-member-edit-agent_research");
  await app.waitFor("id:canvas-seat-editor");
  expect(app.value("id:canvas-seat-label")).toBe("初稿评审");
  expect(app.value("id:canvas-seat-description")).toBe("只审初稿");

  await app.clear("id:canvas-seat-label");
  await app.type("id:canvas-seat-label", "终稿评审");
  await app.clear("id:canvas-seat-description");
  await app.type("id:canvas-seat-description", "复核终稿");
  await app.press("id:canvas-seat-apply");
  await app.waitFor("id:canvas-seat-editor", { state: "gone" });
  expect(joined(app)).toContain("终稿评审");

  // The inline seat's own text is editable too; its definition stays with it.
  await app.press("id:canvas-member-edit-agent_inline");
  await app.waitFor("id:canvas-seat-editor");
  expect(app.value("id:canvas-seat-label")).toBe("内联席位");
  await app.clear("id:canvas-seat-label");
  await app.type("id:canvas-seat-label", "内联合伙人");
  await app.press("id:canvas-seat-apply");
  await app.waitFor("id:canvas-seat-editor", { state: "gone" });
  expect(joined(app)).toContain("内联合伙人");

  await app.press("id:team-save");
  await app.waitFor("id:team-dialog", { state: "gone" });
  await app.waitFor("id:team-card-team_seats");

  await app.press("label:编辑团队 席位组");
  await app.waitFor("id:team-dialog");
  expect(joined(app)).toContain("终稿评审");
  expect(joined(app)).toContain("复核终稿");
  expect(joined(app)).toContain("内联合伙人");
  // The inline definition was carried through the text edit, not dropped.
  expect(joined(app)).toContain("团队内定义");
  expect(app.shows("id:team-save-error")).toBe(false);
});

test("TUI: 团队画布——替换成员保留席位 label/description，保存回读新 Agent", async () => {
  const app = await openSection("seats-edit");
  await app.waitFor("id:team-card-team_seats");

  await app.press("label:编辑团队 席位组");
  await app.waitFor("id:team-dialog");
  await app.press("id:canvas-member-replace-agent_research");
  await app.waitFor("id:canvas-seat-replace");
  expect(joined(app)).toContain("替换只换成员本人；席位名称与描述保留。");
  // 运营 is the only stored agent not captain and not already seated.
  await app.press("text:选择一位 Agent");
  await app.waitFor("text:运营");
  await app.press("text:运营");
  await app.press("id:canvas-seat-replace-apply");
  await app.waitFor("id:canvas-seat-replace", { state: "gone" });

  // The underlying agent changed, the seat's own text did not.
  expect(app.shows("id:canvas-member-agent_ops")).toBe(true);
  expect(app.shows("id:canvas-library-agent_ops")).toBe(false);
  expect(joined(app)).toContain("初稿评审");
  expect(joined(app)).toContain("只审初稿");

  await app.press("id:team-save");
  await app.waitFor("id:team-dialog", { state: "gone" });
  await app.waitFor("id:team-card-team_seats");

  await app.press("label:编辑团队 席位组");
  await app.waitFor("id:team-dialog");
  expect(app.shows("id:canvas-member-agent_ops")).toBe(true);
  expect(joined(app)).toContain("初稿评审");
  expect(joined(app)).toContain("只审初稿");
});

test("TUI: 团队画布——保存失败保留 defaults 与席位编辑草稿", async () => {
  const app = await openSection("refuse");
  await app.waitFor("id:team-card-team_brand");

  await app.press("label:编辑团队 品牌发布小组");
  await app.waitFor("id:team-dialog");
  await app.type("id:team-defaults-tools", "workspace.write");
  await app.press("text:workspace.write");
  await app.press("text:跟随部署默认（不覆盖）");
  await app.waitFor("text:DeepSeek V4.1 Flash · S2API");
  await app.press("text:DeepSeek V4.1 Flash · S2API");
  await app.press("id:canvas-member-edit-agent_research");
  await app.waitFor("id:canvas-seat-editor");
  await app.type("id:canvas-seat-label", "失败保留");
  await app.press("id:canvas-seat-apply");

  await app.press("id:team-save");
  await app.waitFor("id:team-save-error");
  expect(joined(app)).toContain("队长 agent_design 当前不可用");
  expect(app.shows("id:team-dialog")).toBe(true);
  // The refused write did not discard the draft: default tool, model override
  // and the edited seat label are still on screen.
  expect(joined(app)).toContain("workspace.write");
  expect(joined(app)).toContain("DeepSeek V4.1 Flash · S2API");
  expect(joined(app)).toContain("失败保留");

  await app.press("id:canvas-member-edit-agent_research");
  await app.waitFor("id:canvas-seat-editor");
  expect(app.value("id:canvas-seat-label")).toBe("失败保留");
});

test("TUI: Team CLI 能力面板——403 是权限面，不冒充加载失败", async () => {
  const app = await openSection("plugins-permission");

  await app.waitFor("id:team-cli-permission");
  expect(joined(app)).toContain("没有查看插件能力清单的权限");
  expect(joined(app)).toContain("当前账号没有 plugin.read 权限");
  expect(app.shows("id:team-cli-error")).toBe(false);
});

test("TUI: Team CLI 能力面板——manifest 声明逐条渲染，权限与可调用方原样", async () => {
  const app = await openList();

  await app.waitFor("id:team-cli-panel");
  expect(joined(app)).toContain("2 条命令 · 声明即所见");
  expect(joined(app)).toContain("keel team list");
  expect(joined(app)).toContain("权限：team.read");
  expect(joined(app)).toContain("可调用：人工 · Agent · 队长");
  expect(joined(app)).toContain("keel team member remove");
  expect(joined(app)).toContain("破坏性");
  expect(joined(app)).toContain("权限：team.write");
  expect(joined(app)).toContain("可调用：人工 · 队长");
  // Only what the manifest declares: no row is invented for a missing command.
  expect(joined(app)).not.toContain("keel team delete");
});

test("TUI: Team CLI 能力面板——没有声明时明确说没有，不编命令", async () => {
  const app = await openSection("no-manifest");

  await app.waitFor("id:team-cli-empty");
  expect(joined(app)).toContain("没有 keel.team 的 CLI 声明");
  expect(joined(app)).toContain("面板不展示任何未声明的命令");
  expect(joined(app)).not.toContain("keel team list");
});