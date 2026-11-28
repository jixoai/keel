// Board Phase 2 screenshot matrix — the real TSX on the web host.
//
// NOT a discovered test (its name ends .template.tsx). The evidence script
// copies it to `web/board-phase2-capture.test.tsx`, replaces __REPORT_DIR__,
// and runs `nsh test --target web board-phase2-capture.test.tsx`. Every shot
// follows a real state transition on the app's own harness transport; no
// screenshot is drawn from a mock image.
import { open, test } from "nsh/test";

const OUT = "__REPORT_DIR__/shots/web";

const view = (width: number, scenario: string, extra: Record<string, string> = {}, height = 844) =>
  open("./src/screens/board/harness.tsx", { width, height, host: { params: { scenario, ...extra } } });

const shot = async (app: { screenshot: (path?: string | null) => Promise<Uint8Array> }, name: string) => {
  await app.screenshot(`${OUT}/${name}.png`);
};

/** A visible box must stay inside the viewport: a screenshot can look fine
 * while a child overflows the page horizontally. */
const inside = (app: { layout: (selector: string) => { x: number; width: number } | null }, selector: string, width: number) => {
  const box = app.layout(selector);
  if (!box) throw new Error(`no layout for ${selector}`);
  if (box.x < 0 || box.x + box.width > width + 1) {
    throw new Error(`${selector} overflows ${width}px: x=${box.x} width=${box.width}`);
  }
};

/** A visible box must stay inside the viewport vertically too: "the DOM has a
 * rect" is not evidence that the first screen can read it. */
const onScreen = (
  app: { layout: (selector: string) => { y: number; height: number } | null },
  selector: string,
  height: number,
) => {
  const box = app.layout(selector);
  if (!box) throw new Error(`no layout for ${selector}`);
  if (box.y < 0 || box.y + box.height > height + 1) {
    throw new Error(`${selector} is off-screen: y=${box.y} height=${box.height} viewport=${height}`);
  }
};

const noDebugCopy = (app: { texts: () => string[] }, where: string) => {
  const text = app.texts().join("\n");
  for (const token of ["priority=", "team=", "board:", "provider answered", "后端暂未提供", "滞后未知", "目标 · 未知"]) {
    if (text.includes(token)) throw new Error(`${where} leaks debug copy: ${token}`);
  }
};

test("board evidence: queue and five states", async () => {
  const queue = await view(1440, "success");
  await queue.waitFor("id:board-row-812", { timeout: 20_000 });
  inside(queue, "id:board-page", 1440);
  inside(queue, "id:board-row-812", 1440);
  noDebugCopy(queue, "queue");
  await shot(queue, "01-queue-success-1440");

  const wide = await view(1024, "success");
  await wide.waitFor("id:board-queue");
  inside(wide, "id:board-page", 1024);
  await shot(wide, "02-queue-success-1024");

  const tablet = await view(768, "success");
  await tablet.waitFor("id:board-queue");
  inside(tablet, "id:board-page", 768);
  await shot(tablet, "03-queue-success-768");

  const phone = await view(390, "success");
  await phone.waitFor("id:board-queue");
  inside(phone, "id:board-page", 390);
  inside(phone, "id:board-row-798", 390);
  onScreen(phone, "id:board-state-tabs", 844);
  noDebugCopy(phone, "queue 390");
  await shot(phone, "04-queue-success-390");

  const loading = await view(1440, "loading");
  await loading.waitFor("id:board-queue-skeleton", { timeout: 20_000 });
  await shot(loading, "05-queue-loading-1440");

  const empty = await view(1440, "empty");
  await empty.waitFor("id:board-queue-empty");
  await shot(empty, "06-queue-empty-1440");

  const noChannels = await view(1440, "no-channels");
  await noChannels.waitFor("id:board-queue-empty-channels");
  await shot(noChannels, "07-queue-no-channels-1440");

  const error = await view(1440, "error");
  await error.waitFor("id:board-queue-error");
  await shot(error, "08-queue-error-1440");

  const disabled = await view(1440, "disabled");
  await disabled.waitFor("id:board-disabled");
  await shot(disabled, "09-queue-disabled-1440");

  const denied = await view(1440, "denied");
  await denied.waitFor("id:board-not-found");
  await shot(denied, "10-queue-not-found-1440");

  const legacy = await view(1440, "legacy");
  await legacy.waitFor("id:board-migrate");
  await shot(legacy, "11-queue-migrate-1440");

  const reader = await view(1440, "reader");
  await reader.waitFor("id:board-row-812");
  await shot(reader, "12-queue-reader-1440");

  const loading390 = await view(390, "loading");
  await loading390.waitFor("id:board-queue-skeleton", { timeout: 20_000 });
  await shot(loading390, "05b-queue-loading-390");

  const archive = await view(1440, "success", { search: "state=all&archived=1" });
  await archive.waitFor("id:board-row-826");
  await shot(archive, "13-queue-archive-1440");

  const phoneEmpty = await view(390, "empty");
  await phoneEmpty.waitFor("id:board-queue-empty");
  await shot(phoneEmpty, "14-queue-empty-390");

  // A deep link selects the last state tab; it must be revealed inside the
  // horizontal tab strip instead of staying past the right edge.
  const phoneArchive = await view(390, "success", { search: "state=all&archived=1" });
  await phoneArchive.waitFor("id:board-row-826");
  inside(phoneArchive, "id:board-state-tab-all", 390);
  await shot(phoneArchive, "15-queue-archive-390");
}, 600_000);

test("board evidence: task states", async () => {
  const claimed = await view(1440, "success", { task: "812" });
  await claimed.waitFor("id:board-claim-lost");
  await claimed.waitFor("id:board-thread-root", { timeout: 20_000 });
  await claimed.waitFor("id:board-reply-8121", { timeout: 20_000 });
  noDebugCopy(claimed, "claim lost");
  await shot(claimed, "20-task-claim-lost-1440");

  const ready = await view(1440, "success", { task: "809" });
  await ready.waitFor("id:board-task-card");
  await ready.waitFor("id:board-thread-root", { timeout: 20_000 });
  await ready.waitFor("id:board-reply-8091", { timeout: 20_000 });
  inside(ready, "id:board-task-page", 1440);
  noDebugCopy(ready, "task ready");
  await shot(ready, "21-task-ready-1440");

  const blocked = await view(1440, "success", { task: "814" });
  await blocked.waitFor("id:board-task-card");
  await shot(blocked, "22-task-blocked-mirror-1440");

  const completed = await view(1440, "success", { task: "818" });
  await completed.waitFor("id:board-task-card");
  await shot(completed, "23-task-completed-1440");

  const emptyThread = await view(1440, "success", { task: "816" });
  await emptyThread.waitFor("id:board-thread-empty");
  await shot(emptyThread, "23b-task-empty-thread-1440");

  const archived = await view(1440, "success", { task: "826" });
  await archived.waitFor("id:board-task-card");
  await shot(archived, "24-task-archived-1440");

  const phone = await view(390, "success", { task: "809" });
  await phone.waitFor("id:board-task-card");
  await phone.waitFor("id:board-thread-root", { timeout: 20_000 });
  await phone.waitFor("id:board-reply-8091", { timeout: 20_000 });
  inside(phone, "id:board-task-page", 390);
  inside(phone, "id:board-composer", 390);
  onScreen(phone, "id:board-thread-root", 844);
  onScreen(phone, "id:board-reply-8091", 844);
  noDebugCopy(phone, "task 390");
  await shot(phone, "25-task-ready-390");

  const reading = await view(1440, "reader", { task: "812" });
  await reading.waitFor("id:board-task-card");
  await shot(reading, "25b-task-permission-1440");

  const loadingTask = await view(1440, "task-loading");
  await loadingTask.waitFor("id:board-task-loading", { timeout: 20_000 });
  await shot(loadingTask, "25c-task-loading-1440");

  const failedTask = await view(1440, "task-error");
  await failedTask.waitFor("id:board-task-error", { timeout: 20_000 });
  await shot(failedTask, "25d-task-error-1440");
}, 600_000);

test("board evidence: task dialogs", async () => {
  const known = await view(1440, "success", { task: "809" });
  await known.waitFor("id:board-task-card");
  await known.press("id:board-action-assign");
  await known.waitFor("id:board-dialog-assign");
  await shot(known, "26-dialog-assign-1440");
  await known.press("label:取消指派");
  await known.waitUntil(() => !known.texts().join("\n").includes("指派 #809"));
  await known.press("id:board-action-needs");
  await known.waitFor("id:board-dialog-needs");
  await shot(known, "27-dialog-needs-1440");
  await known.press("label:取消修改前置");
  await known.waitUntil(() => !known.texts().join("\n").includes("前置依赖 #809"));
  await known.press("id:board-action-cancel");
  await known.waitFor("id:board-dialog-cancel");
  await shot(known, "28-dialog-cancel-1440");
}, 600_000);

test("board evidence: sync", async () => {
  const sync = await view(1440, "success");
  await sync.waitFor("id:board-row-812");
  await sync.press("id:board-view-sync");
  await sync.waitFor("id:board-sync-entry-slack");
  inside(sync, "id:board-sync", 1440);
  noDebugCopy(sync, "sync");
  await shot(sync, "30-sync-success-1440");
  await sync.press("id:board-sync-inbox-open-slack");
  await sync.waitFor("text:/收件箱 · Slack/");
  await shot(sync, "31-sync-inbox-1440");

  const syncPhone = await view(390, "success");
  await syncPhone.waitFor("id:board-row-812");
  await syncPhone.press("id:board-view-sync");
  await syncPhone.waitFor("id:board-sync-entry-slack");
  await shot(syncPhone, "30b-sync-success-390");

  const syncError = await view(1440, "sync-error");
  await syncError.waitFor("id:board-sync-error");
  await shot(syncError, "32-sync-error-1440");

  const syncReader = await view(1440, "sync-reader");
  await syncReader.waitFor("id:board-sync-permission");
  await shot(syncReader, "33-sync-permission-1440");

  const syncLoading = await view(1440, "sync-loading");
  await syncLoading.waitFor("id:board-sync-skeleton", { timeout: 20_000 });
  await shot(syncLoading, "33b-sync-loading-1440");

  const syncEmpty = await view(1440, "sync-empty");
  await syncEmpty.waitFor("id:board-sync-empty", { timeout: 20_000 });
  await shot(syncEmpty, "33c-sync-empty-1440");
}, 600_000);

test("board evidence: automation and the retired address", async () => {
  const auto = await view(1440, "success");
  await auto.waitFor("id:board-row-812");
  await auto.press("id:board-view-automation");
  await auto.waitFor("id:board-watcher-triage-inbox");
  inside(auto, "id:board-automation", 1440);
  noDebugCopy(auto, "automation");
  if (auto.texts().join("\n").includes("最近 未知")) throw new Error("automation still prints an unknown run time");
  await shot(auto, "34-automation-success-1440");

  const autoError = await view(1440, "automation-error");
  await autoError.waitFor("id:board-automation-error");
  await shot(autoError, "35-automation-error-1440");

  const autoReader = await view(1440, "automation-reader");
  await autoReader.waitFor("id:board-automation-permission");
  await shot(autoReader, "34b-automation-permission-1440");

  const autoLoading = await view(1440, "automation-loading");
  await autoLoading.waitFor("id:board-automation-skeleton", { timeout: 20_000 });
  await shot(autoLoading, "35b-automation-loading-1440");

  const autoEmpty = await view(1440, "automation-empty");
  await autoEmpty.waitFor("id:board-automation-empty", { timeout: 20_000 });
  await shot(autoEmpty, "35c-automation-empty-1440");

  const gone = await view(1440, "gone");
  await gone.waitFor("id:board-gone");
  await shot(gone, "36-superagent-gone-1440");

  const phoneAuto = await view(390, "success");
  await phoneAuto.waitFor("id:board-row-812");
  await phoneAuto.press("id:board-view-automation");
  await phoneAuto.waitFor("id:board-watcher-triage-inbox");
  inside(phoneAuto, "id:board-automation", 390);
  await shot(phoneAuto, "37-automation-390");
}, 600_000);

test("board evidence: before/after action transitions", async () => {
  const claim = await view(1440, "success", { task: "809" });
  await claim.waitFor("id:board-task-card");
  await shot(claim, "40-before-claim-1440");
  await claim.keyboard.press("c");
  await claim.waitFor("id:board-notice", { timeout: 20_000 });
  await shot(claim, "41-after-claim-1440");

  const complete = await view(1440, "success", { task: "809" });
  await complete.waitFor("id:board-task-card");
  await complete.keyboard.press("c");
  await complete.waitFor("id:board-notice", { timeout: 20_000 });
  await complete.keyboard.press("e");
  await complete.waitFor("id:board-dialog-complete", { timeout: 20_000 });
  await shot(complete, "42-before-complete-1440");
  await complete.type("id:board-dialog-complete-input", "已交付并验证。");
  await complete.press("id:board-dialog-complete-submit");
  await complete.waitFor("id:board-notice", { timeout: 20_000 });
  await shot(complete, "43-after-complete-1440");

  const sync = await view(1440, "success");
  await sync.waitFor("id:board-row-812");
  await sync.press("id:board-view-sync");
  await sync.waitFor("id:board-sync-entry-slack");
  await shot(sync, "44-before-retry-1440");
  await sync.press("id:board-sync-retry-all");
  await sync.waitFor("id:board-notice", { timeout: 20_000 });
  await shot(sync, "45-after-retry-1440");

  const auto = await view(1440, "success");
  await auto.waitFor("id:board-row-812");
  await auto.press("id:board-view-automation");
  await auto.waitFor("id:board-watcher-triage-inbox");
  await shot(auto, "46-before-watcher-toggle-1440");
  await auto.press("id:board-watcher-switch-triage-inbox");
  await auto.waitFor("id:board-notice", { timeout: 20_000 });
  await shot(auto, "47-after-watcher-toggle-1440");

  const phone = await view(390, "success");
  await phone.waitFor("id:board-row-812");
  await shot(phone, "48-before-filter-390");
  await phone.press("id:board-filter-open");
  await phone.waitFor("text:/筛选任务/", { timeout: 20_000 });
  await phone.waitFor("id:board-filter-reset", { timeout: 20_000 });
  inside(phone, "id:board-filter-reset", 390);
  inside(phone, "id:board-filter-selected-count", 390);
  const forField = phone.layout("id:board-filter-for");
  const archiveField = phone.layout("id:board-filter-archive");
  if (!forField || !archiveField) throw new Error("filter sheet fields are missing");
  if (Math.abs(forField.width - archiveField.width) > 1) {
    throw new Error(`archive width ${archiveField.width} != select width ${forField.width}`);
  }
  await shot(phone, "49-filter-sheet-390");

  // 390x667: the sheet body scrolls and the footer wraps, so every action is
  // still reachable above the home indicator.
  const short = await view(390, "success", {}, 667);
  await short.waitFor("id:board-row-812");
  await short.press("id:board-filter-open");
  await short.waitFor("id:board-filter-reset", { timeout: 20_000 });
  inside(short, "id:board-filter-sheet-body", 390);
  onScreen(short, "id:board-filter-reset", 667);
  await shot(short, "49b-filter-sheet-390x667");

  // The task copy's raw provider sentence is behind a disclosure, not in the
  // first screen a reader sees.
  const copy = await view(390, "success", { task: "812" });
  await copy.waitFor("id:board-thread-root", { timeout: 20_000 });
  if (copy.texts().join("\n").includes("provider answered 429")) {
    throw new Error("raw provider error is visible without opening the disclosure");
  }
  await copy.press("id:board-task-copy-error-toggle-slack-work-production");
  await copy.waitFor("text:/provider answered 429/", { timeout: 20_000 });
  await shot(copy, "50-task-copy-error-disclosure-390");
}, 600_000);
