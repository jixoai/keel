// The external run card — change keel-oar-renderer.
//
// The same file runs on both hosts (the terminal suite runs every web test):
// the card is a graphical face, so a terminal run asserts the text row the
// host must keep, and a native run asserts the card. Which host the *opened
// app* runs as is read from the app itself — the harness prints its target, and
// the app's face is its own evidence (the card appears, or the text row stays).
//
// The harness proves the states; the app's own fixture transport proves the
// product's wiring (one projection read, one card per session, an attribution
// that opens the existing card and nothing else).
import { expect, open, test } from "nsh/test";
import { oarAccess } from "../data/oar";
import { demoOarProjection } from "../data/fixture-oar";
import { store } from "../data/store";

const harness = (scenario: string) =>
  open("./oar-run.harness.tsx", { width: 900, host: { params: { scenario } } });

/** The app on its fixture transport, asking for the OAR sample session. */
const app = () => open("../../App.tsx", { width: 1440, host: { params: { fixture: true, oar: true } } });

const TITLE = "label:打开会话 Codex 外部运行";

/** The target the harness app runs as (`tui` there is the terminal face). */
const harnessTarget = async (view: Awaited<ReturnType<typeof harness>>): Promise<string> => {
  await view.waitFor("id:oar-harness-target");
  const line = view.texts().find((text) => text.startsWith("target="));
  return line?.slice("target=".length) ?? "";
};

/** How many times `needle` occurs in the screen's texts. */
const count = (view: { texts(): string[] }, needle: string): number =>
  view.texts().join("\n").split(needle).length - 1;

const settle = () => new Promise((resolve) => setTimeout(resolve, 20));

test("投影缓存：同一批记录只读一次，更新的记录再读一次", async () => {
  let reads = 0;
  store.skipBootForTest();
  store.setIdentityTransport({
    async listAccounts() {
      return { items: [], limit: 100, hasMore: false };
    },
    async externalProjection(sessionId: string) {
      reads += 1;
      return demoOarProjection(sessionId);
    },
  });
  oarAccess.reset();
  oarAccess.ensure("cache-session", 3);
  await settle();
  // The same burst: never a second request.
  oarAccess.ensure("cache-session", 3);
  oarAccess.ensure("cache-session", 2);
  expect(reads).toBe(1);
  expect(oarAccess.state("cache-session").status).toBe("ready");
  // A later turn's record refolds the run once; nothing else re-reads it.
  oarAccess.ensure("cache-session", 9);
  await settle();
  oarAccess.ensure("cache-session", 9);
  expect(reads).toBe(2);
  oarAccess.reset();
  store.setIdentityTransport(null);
});

test("外部运行卡：runtime、simulated 徽标与逐行记录（终端只剩文本行）", async () => {
  const view = await harness("oar-card");
  if ((await harnessTarget(view)) === "tui") {
    // Spec: TUI 目标。 The card is not drawn; the record keeps its text row.
    await view.waitFor("text:/外部事件 · oar.record/");
    expect(view.shows("id:oar-run-card")).toBe(false);
    expect(view.texts().join("\n")).not.toContain("模拟运行时");
    return;
  }
  await view.waitFor("id:oar-run-card", { timeout: 15_000 });
  const screen = view.texts().join("\n");
  expect(screen).toContain("外部运行");
  // The runtime and the simulated mark: a fake stream is never shown as live.
  expect(screen).toContain("fake");
  expect(screen).toContain("模拟运行时");
  expect(view.shows("id:oar-run-simulated")).toBe(true);
  // Agent rollups, rows, usage.
  expect(screen).toContain("/main");
  expect(screen).toContain("/main/worker-1");
  expect(screen).toContain("shell");
  expect(screen).toContain("read_file");
  expect(screen).toContain("turn.finished");
  expect(screen).toContain("17 tokens");
  // Attribution names the existing card only.
  expect(screen).toContain("关联任务");
  expect(screen).toContain("task-oar-codex");
  expect(view.shows("id:oar-run-open-task")).toBe(true);
});

test("外部运行卡：行可展开，展开后是投影的原始字段", async () => {
  const view = await harness("oar-card");
  if ((await harnessTarget(view)) === "tui") return;
  await view.waitFor("id:oar-run-card", { timeout: 15_000 });
  // callId lives only in the expansion: the collapsed row never shows it.
  expect(view.texts().join("\n")).not.toContain("call-1");
  await view.press("id:oar-run-row-2");
  await view.waitFor("text:callId");
  const screen = view.texts().join("\n");
  expect(screen).toContain("call-1");
  // The folded result seq is the row's own fact (2 → 3).
  expect(screen).toContain("2 → 3");
  expect(screen).toContain("exitCode");
});

test("外部运行卡：live 形态不带 simulated 徽标，也不伪造归属", async () => {
  const view = await harness("oar-codex");
  if ((await harnessTarget(view)) === "tui") return;
  await view.waitFor("id:oar-run-card", { timeout: 15_000 });
  const screen = view.texts().join("\n");
  expect(screen).toContain("codex");
  expect(screen).not.toContain("模拟运行时");
  expect(view.shows("id:oar-run-simulated")).toBe(false);
  expect(view.shows("id:oar-run-task")).toBe(false);
});

test("外部运行卡：runtime=fake 而 simulated 标记缺失，仍然显示模拟徽标", async () => {
  const view = await harness("oar-fake-unmarked");
  if ((await harnessTarget(view)) === "tui") return;
  await view.waitFor("id:oar-run-card", { timeout: 15_000 });
  expect(view.shows("id:oar-run-simulated")).toBe(true);
  expect(view.texts().join("\n")).toContain("模拟运行时");
});

test("空投影：卡片说 journal 是空的，不冒充有记录", async () => {
  const view = await harness("oar-empty");
  if ((await harnessTarget(view)) === "tui") {
    await view.waitFor("text:/外部事件 · oar.record/");
    expect(view.shows("id:oar-run-card")).toBe(false);
    return;
  }
  await view.waitFor("id:oar-run-card", { timeout: 15_000 });
  expect(view.texts().join("\n")).toContain("还没有外部运行记录");
});

test("投影不可用：501 / 无 transport / 无贡献 / 读取中，都只留文本行", async () => {
  for (const scenario of ["oar-error", "oar-no-transport", "oar-no-contribution", "oar-loading"]) {
    const view = await harness(scenario);
    await view.waitFor("text:/外部事件 · oar.record/");
    expect(view.shows("id:oar-run-card")).toBe(false);
    // The rest of the conversation is untouched: one text row, no plugin
    // failure face and no error banner.
    expect(view.shows("id:plugin-failure-oar-run")).toBe(false);
  }
});

test("App：一个会话一张卡，关联任务打开已有卡（不新建卡）", async () => {
  const view = await app();
  await view.press(TITLE);
  // The app's own face says which host rendered it. A fixture read lands in
  // well under a second; the bound only has to separate "card" from "terminal".
  const card = await view
    .waitFor("id:oar-run-card", { timeout: 6000 })
    .then(() => true)
    .catch(() => false);
  if (!card) {
    // The terminal: the record keeps its text row, one row for the one run.
    await view.waitFor("text:/外部事件 · oar.record/");
    expect(view.shows("id:oar-run-card")).toBe(false);
    expect(count(view, "外部事件 · oar.record")).toBe(1);
    return;
  }
  const screen = view.texts().join("\n");
  // Three oar.record events, one card (the projection is the session's), and
  // the records are not repeated as text rows beside it.
  expect(count(view, "模拟运行时")).toBe(1);
  expect(screen).not.toContain("外部事件 · oar.record");
  expect(screen).toContain("task-oar-codex");
  // The attribution opens the existing card through the task panel's own
  // selection; the notice names it.
  await view.press("id:oar-run-open-task");
  await view.waitFor("text:/已定位任务卡 task-oar-codex/");
  expect(count(view, "模拟运行时")).toBe(1);
});
