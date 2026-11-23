#!/usr/bin/env node
// Browser shell gate for the system-bar change.
//
// Serves the packaged web build (the caller starts it from
// `scripts/system-bar-evidence.sh`), drives it with the same playwright-core +
// Chrome pair the repo's other browser lanes use, and writes one PNG per step
// plus a machine-readable report.json. It never writes an HTML report and
// never records a credential: the fixture transport is the only launch
// parameter.
//
// What it asserts:
//   · the global system bar on every viewport, with sound / sound settings /
//     notifications / theme / command present and no horizontal overflow;
//   · the daily rail is 工作区 / 员工 / 团队; 身份 / 记录 live in the settings
//     page's management card with their old `books-chapter-*` test ids;
//   · the phone keeps three daily tabs plus the settings entry;
//   · the notification panel lists the fixture's waiting approvals and the
//     sound dialog reads the same preference;
//   · the plugin top-band slot renders inside the system bar, including at 390;
//   · no app-drawn window controls appear in a browser (the host exposes none).
import fs from "node:fs";
import path from "node:path";

const BASE = (process.env.E2E_BASE || "http://127.0.0.1:13762").replace(/\/$/, "");
const EVIDENCE = process.env.E2E_EVIDENCE || "";
const CHROME = process.env.E2E_CHROME || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const PLAYWRIGHT =
  process.env.PLAYWRIGHT_CORE ||
  "/Users/kzf/.npm/_npx/e41f203b7505f1fb/node_modules/playwright-core/index.mjs";
const HEADLESS = (process.env.E2E_HEADLESS || "true") !== "false";

if (!EVIDENCE) {
  console.error("E2E_EVIDENCE is required");
  process.exit(2);
}
fs.mkdirSync(EVIDENCE, { recursive: true });

const VIEWPORTS = [
  { width: 390, height: 844 },
  { width: 768, height: 1024 },
  { width: 1024, height: 800 },
  { width: 1440, height: 900 },
];

const report = {
  lane: "system-bar",
  startedAt: new Date().toISOString(),
  base: BASE,
  viewports: VIEWPORTS.map((v) => `${v.width}x${v.height}`),
  steps: [],
  console: [],
  pageErrors: [],
  failedRequests: [],
  pass: false,
};

const checks = [];
const check = (label, ok, detail) => {
  checks.push({ label, ok: Boolean(ok), detail: detail ?? null });
  report.steps.push({ label, kind: "assert", ok: Boolean(ok), detail: detail ?? null });
  if (!ok) console.error(`FAIL ${label}${detail ? `: ${JSON.stringify(detail)}` : ""}`);
  return Boolean(ok);
};

const overflow = async (page, label) => {
  const value = await page.evaluate(() => ({
    innerWidth: window.innerWidth,
    docScrollWidth: document.documentElement.scrollWidth,
    docClientWidth: document.documentElement.clientWidth,
    bodyScrollWidth: document.body ? document.body.scrollWidth : null,
  }));
  report.steps.push({ label, kind: "overflow", ok: value.docScrollWidth <= value.innerWidth, value });
  return check(`${label}: no horizontal overflow`, value.docScrollWidth <= value.innerWidth, value);
};

const shot = async (page, file, label) => {
  await page.waitForTimeout(250);
  const full = path.join(EVIDENCE, file);
  await page.screenshot({ path: full, fullPage: true });
  report.steps.push({ label, kind: "screenshot", file });
  console.log(`shot ${file}`);
};

const playwright = await import(PLAYWRIGHT);
const { chromium } = playwright;
const browser = await chromium.launch({ executablePath: CHROME, headless: HEADLESS });

try {
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await context.newPage();
  page.on("console", (message) => {
    const text = message.text();
    if (message.type() === "error" && !text.includes("favicon")) report.console.push(text);
  });
  page.on("pageerror", (error) => report.pageErrors.push(String(error)));
  page.on("requestfailed", (request) => {
    if (!request.url().includes("favicon")) {
      report.failedRequests.push({ url: request.url(), failure: request.failure()?.errorText ?? "" });
    }
  });

  // ---- the workspace at every viewport -----------------------------------
  for (const viewport of VIEWPORTS) {
    await page.setViewportSize(viewport);
    await page.goto(`${BASE}/?fixture=1`, { waitUntil: "networkidle" });
    await page.waitForSelector('[data-testid="keel-system-bar"]', { timeout: 20_000 });
    await page.waitForTimeout(400);

    const width = viewport.width;
    const state = await page.evaluate(() => {
      const q = (id) => document.querySelector(`[data-testid="${id}"]`);
      const ids = (prefix) =>
        [...document.querySelectorAll(`[data-testid^="${prefix}"]`)].map((node) => node.getAttribute("data-testid"));
      return {
        systemBar: !!q("keel-system-bar"),
        booksTop: !!q("books-top"),
        controls: ["system-bar-sound", "system-bar-sound-settings", "system-bar-notifications", "system-bar-theme", "system-bar-command"].filter((id) => !!q(id)),
        windowControls: !!q("system-bar-window-controls"),
        rail: !!q("books-rail"),
        railChapters: ids("books-chapter-"),
        tabs: ids("books-tab-"),
        mobileSettings: !!q("books-mobile-settings"),
      };
    });

    check(`${width}: system bar is mounted`, state.systemBar);
    check(`${width}: plugin/drag slot is inside the bar`, state.booksTop);
    check(`${width}: five system controls present`, state.controls.length === 5, state.controls);
    check(`${width}: no fake window controls in a browser`, !state.windowControls);
    if (width > 767) {
      check(`${width}: desktop rail has the three daily chapters`, ["books-chapter-workspace", "books-chapter-bots", "books-chapter-teams"].every((id) => state.railChapters.includes(id)) && state.railChapters.length === 3, state.railChapters);
      check(`${width}: identity/records are not rail entries`, !state.railChapters.includes("books-chapter-identity") && !state.railChapters.includes("books-chapter-records"), state.railChapters);
    } else {
      check(`${width}: phone has three daily tabs`, ["books-tab-workspace", "books-tab-bots", "books-tab-teams"].every((id) => state.tabs.includes(id)) && state.tabs.length === 3, state.tabs);
      check(`${width}: phone keeps the settings entry`, state.mobileSettings);
    }
    await overflow(page, `workspace-${width}`);
    await shot(page, `workspace-${width}.png`, `workspace ${width}`);
  }

  // ---- settings: the management doors ------------------------------------
  for (const width of [390, 1440]) {
    const viewport = VIEWPORTS.find((v) => v.width === width);
    await page.setViewportSize(viewport);
    await page.goto(`${BASE}/?fixture=1`, { waitUntil: "networkidle" });
    await page.waitForSelector('[data-testid="keel-system-bar"]', { timeout: 20_000 });
    const opener = width === 390 ? '[data-testid="books-mobile-settings"]' : '[data-testid="books-settings"]';
    await page.click(opener);
    await page.waitForSelector('[data-testid="settings-page"]');
    await page.click('[data-testid="settings-section-general"]');
    await page.waitForSelector('[data-testid="settings-chapters"]');
    const doors = await page.evaluate(() => {
      const q = (id) => document.querySelector(`[data-testid="${id}"]`);
      return {
        identity: !!q("books-chapter-identity"),
        records: !!q("books-chapter-records"),
        account: !!q("settings-account"),
        logout: !!q("settings-logout"),
        todo: !!q("settings-todo"),
        live: !!q("settings-live"),
      };
    });
    check(`${width}: settings exposes identity/records doors`, doors.identity && doors.records, doors);
    check(`${width}: settings keeps account/logout/todo/live`, doors.account && doors.logout && doors.todo && doors.live, doors);
    await overflow(page, `settings-${width}`);
    await shot(page, `settings-general-${width}.png`, `settings general ${width}`);

    if (width === 390) {
      await page.click('[data-testid="books-chapter-identity"]');
      await page.waitForSelector('[data-testid="identity-segment"]');
      check("390: the settings door opens the identity chapter", true);
    }
  }

  // ---- overlays: notifications, sound, theme, palette --------------------
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${BASE}/?fixture=1`, { waitUntil: "networkidle" });
  await page.waitForSelector('[data-testid="keel-system-bar"]');

  await page.click('[data-testid="system-bar-notifications"]');
  await page.waitForSelector('[data-testid="notifications-panel"]');
  const panel = await page.evaluate(() => ({
    approvals: document.querySelectorAll('[data-testid^="notifications-approval-"]').length,
    live: !!document.querySelector('[data-testid="notifications-live"]'),
    text: document.querySelector('[data-testid="notifications-panel"]')?.innerText ?? "",
  }));
  check("notification panel lists the waiting approvals", panel.approvals >= 1, panel);
  check("notification panel names the live channel", panel.live);
  await shot(page, "notifications-1440.png", "notifications 1440");
  await page.keyboard.press("Escape");
  await page.waitForTimeout(400);

  const soundBefore = await page.getAttribute('[data-testid="system-bar-sound"]', "aria-label");
  await page.click('[data-testid="system-bar-sound"]');
  await page.waitForTimeout(250);
  const soundAfter = await page.getAttribute('[data-testid="system-bar-sound"]', "aria-label");
  check("sound toggle flips its action label", soundBefore !== soundAfter && !!soundAfter, { soundBefore, soundAfter });
  await page.click('[data-testid="system-bar-sound-settings"]');
  await page.waitForSelector('[data-testid="sound-settings"]');
  const sound = await page.evaluate(() => ({
    enabled: !!document.querySelector('[data-testid="sound-settings-enabled"]'),
    volumes: ["low", "medium", "high"].every((id) => !!document.querySelector(`[data-testid="sound-settings-volume-${id}"]`)),
    preview: !!document.querySelector('[data-testid="sound-settings-preview"]'),
  }));
  check("sound settings dialog exposes switch, volumes and preview", sound.enabled && sound.volumes && sound.preview, sound);
  await shot(page, "sound-settings-1440.png", "sound settings 1440");
  await page.keyboard.press("Escape");
  await page.waitForTimeout(400);

  const themeBefore = await page.getAttribute('[data-testid="system-bar-theme"]', "aria-label");
  await page.click('[data-testid="system-bar-theme"]');
  await page.waitForTimeout(500);
  const themeAfter = await page.getAttribute('[data-testid="system-bar-theme"]', "aria-label");
  check("theme switch flips light/dark", themeBefore !== themeAfter && !!themeAfter, { themeBefore, themeAfter });
  await shot(page, "light-theme-1440.png", "light theme 1440");

  await page.click('[data-testid="system-bar-command"]');
  await page.waitForSelector('[data-testid="command-palette"]');
  check("command palette opens from the system bar", true);
  await page.keyboard.press("Escape");

  // ---- the plugin slot inside the bar ------------------------------------
  for (const viewport of [{ width: 390, height: 844 }, { width: 1440, height: 900 }]) {
    await page.setViewportSize(viewport);
    await page.goto(`${BASE}/?fixture=1&plugins=1`, { waitUntil: "networkidle" });
    await page.waitForSelector('[data-testid="plugin-top-panels"]', { timeout: 20_000 });
    const nested = await page.evaluate(() => {
      const panel = document.querySelector('[data-testid="plugin-top-panels"]');
      const top = document.querySelector('[data-testid="books-top"]');
      const bar = document.querySelector('[data-testid="keel-system-bar"]');
      return !!(panel && top && bar && bar.contains(top) && top.contains(panel));
    });
    check(`${viewport.width}: plugin top panel renders inside the system bar`, nested);
    await overflow(page, `plugin-${viewport.width}`);
    await shot(page, `plugin-topbar-${viewport.width}.png`, `plugin top bar ${viewport.width}`);
  }

  report.checks = checks;
  report.pass = checks.every((entry) => entry.ok);
} finally {
  await browser.close();
  report.finishedAt = new Date().toISOString();
  report.pass = report.pass && report.pageErrors.length === 0;
  fs.writeFileSync(path.join(EVIDENCE, "report.json"), `${JSON.stringify(report, null, 2)}\n`);
}

console.log(`system-bar: ${report.pass ? "PASS" : "FAIL"} — ${checks.filter((c) => c.ok).length}/${checks.length} checks`);
process.exit(report.pass ? 0 : 1);
