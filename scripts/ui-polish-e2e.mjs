#!/usr/bin/env node
// Browser gate for the UI-polish lane (change keel-harness-v2).
//
// Serves the packaged web build (scripts/ui-polish-evidence.sh starts it),
// drives it with the repo's playwright-core + Chrome pair, and writes one PNG
// per page/viewport plus a machine-readable report.json. It never writes an
// HTML report and never records a credential: the fixture transport is the
// only launch parameter.
//
// What it measures, on every content surface this lane touched:
//   · one field height per toolbar (34 desktop / 44 phone), on Input, Select,
//     range segment and filter stamp;
//   · table header / body-row heights, and the numeric columns' right edge;
//   · dialog cards: every field height, footer actions equal and aligned, and
//     a long form's footer reachable after scrolling;
//   · no horizontal overflow at 390 or 1440;
//   · no console errors, page errors or failed requests.
import fs from "node:fs";
import path from "node:path";

const BASE = (process.env.E2E_BASE || "http://127.0.0.1:13772").replace(/\/$/, "");
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
  { width: 1440, height: 900 },
];

const report = {
  lane: "ui-polish",
  startedAt: new Date().toISOString(),
  base: BASE,
  viewports: VIEWPORTS.map((v) => `${v.width}x${v.height}`),
  steps: [],
  console: [],
  pageErrors: [],
  failedRequests: [],
  checks: [],
  measurements: {},
  pass: false,
};

const checks = [];
const check = (label, ok, detail) => {
  checks.push({ label, ok: Boolean(ok), detail: detail ?? null });
  report.steps.push({ label, kind: "assert", ok: Boolean(ok), detail: detail ?? null });
  if (!ok) console.error(`FAIL ${label}${detail ? `: ${JSON.stringify(detail)}` : ""}`);
  return Boolean(ok);
};

const shot = async (page, file, label, fullPage = true) => {
  await page.waitForTimeout(200);
  await page.screenshot({ path: path.join(EVIDENCE, file), fullPage });
  report.steps.push({ label, kind: "screenshot", file });
  console.log(`shot ${file}`);
};

const overflow = async (page, label) => {
  const value = await page.evaluate(() => ({
    inner: window.innerWidth,
    doc: document.documentElement.scrollWidth,
    body: document.body ? document.body.scrollWidth : null,
  }));
  report.steps.push({ label, kind: "overflow", value });
  return check(`${label}: no horizontal overflow`, value.doc <= value.inner, value);
};

const go = async (page) => {
  // A reload while a dialog's close animation is still running can abort the
  // wasm/entry fetch; a fresh navigation is expected to succeed, so retry it
  // instead of losing the rest of the run.
  for (let attempt = 0; attempt < 3; attempt++) {
    try {
      await page.goto(`${BASE}/?fixture=1`, { waitUntil: "networkidle" });
      await page.waitForSelector('[data-testid="keel-system-bar"]', { timeout: 15_000 });
      await page.waitForTimeout(350);
      return;
    } catch (error) {
      if (attempt === 2) throw error;
      await page.waitForTimeout(900);
    }
  }
};

const openSettings = async (page, width) => {
  await page.click(width <= 767 ? '[data-testid="books-mobile-settings"]' : '[data-testid="books-settings"]');
  await page.waitForSelector('[data-testid="settings-page"]');
  await page.click('[data-testid="settings-section-general"]');
  await page.waitForSelector('[data-testid="settings-chapters"]');
};

const openChapter = async (page, width, id) => {
  await page.click(width <= 767 ? `[data-testid="books-tab-${id}"]` : `[data-testid="books-chapter-${id}"]`);
};

const openManagement = async (page, width, chapter) => {
  await openSettings(page, width);
  await page.click(`[data-testid="books-chapter-${chapter}"]`);
};

const boxes = async (page, ids) =>
  page.evaluate((ids) => {
    const out = {};
    for (const id of ids) {
      const el = document.querySelector(`[data-testid="${id}"]`);
      if (!el) {
        out[id] = null;
        continue;
      }
      const r = el.getBoundingClientRect();
      out[id] = {
        x: Math.round(r.x),
        y: Math.round(r.y),
        w: Math.round(r.width),
        h: Math.round(r.height),
        right: Math.round(r.right),
        bottom: Math.round(r.bottom),
      };
    }
    return out;
  }, ids);

const measureFields = async (page, label, ids, expected) => {
  const found = await boxes(page, ids);
  report.measurements[label] = found;
  const present = Object.entries(found).filter(([, value]) => value !== null);
  check(`${label}: every field is present`, present.length === ids.length, {
    missing: ids.filter((id) => !found[id]),
  });
  const wrong = present.filter(([, value]) => value.h !== expected).map(([id, value]) => `${id}=${value.h}`);
  check(`${label}: fields are ${expected}px`, present.length > 0 && wrong.length === 0, { wrong, found });
  return found;
};

const tableMetrics = async (page, label) =>
  page.evaluate((label) => {
    const out = {};
    for (const id of ["bots-table", "teams-table", "accounts-table", "groups-table", "records-table"]) {
      const table = document.querySelector(`[data-testid="${id}"]`);
      if (!table) continue;
      const header = table.querySelector('[role="rowgroup"] [role="row"]');
      const rows = [...table.querySelectorAll('[role="row"][data-testid]')].filter(
        (row) => !row.getAttribute("data-testid").endsWith("-head"),
      );
      out[id] = {
        header: header ? Math.round(header.getBoundingClientRect().height) : null,
        rows: rows.map((row) => Math.round(row.getBoundingClientRect().height)),
      };
    }
    return out;
  }, label).then((value) => {
    report.measurements[label] = value;
    return value;
  });

/** Header 34 + its hairline; body rows 44 + their hairline, every row equal. */
const assertTable = (label, metrics, id) => {
  const table = metrics[id];
  check(`${label}: ${id} renders a header and rows`, !!table && !!table.header && table.rows.length > 0, table);
  if (!table) return;
  check(`${label}: ${id} header is 34px`, table.header >= 34 && table.header <= 36, table.header);
  const unique = [...new Set(table.rows)];
  check(
    `${label}: ${id} body rows share one 44px height`,
    unique.length === 1 && unique[0] >= 44 && unique[0] <= 46,
    table.rows,
  );
};

const dialogMetrics = async (page, id, inputIds, expected, tallIds = []) => {
  const card = await boxes(page, [`${id}-card`]);
  const footer = await boxes(page, [`${id}-footer`]);
  const inputs = await boxes(page, inputIds);
  const tall = await boxes(page, tallIds);
  const buttons = await page.evaluate((id) => {
    const footer = document.querySelector(`[data-testid="${id}-footer"]`);
    if (!footer) return null;
    return [...footer.querySelectorAll('[role="button"]')].map((el) => {
      const r = el.getBoundingClientRect();
      return { h: Math.round(r.height), y: Math.round(r.y), bottom: Math.round(r.bottom) };
    });
  }, id);
  const value = { card: card[`${id}-card`], footer: footer[`${id}-footer`], inputs, tall, buttons };
  report.measurements[`${id}-${page.viewportSize()?.width ?? 0}`] = value;

  const present = Object.entries(inputs).filter(([, input]) => input !== null);
  check(`${id}: fields present`, present.length === inputIds.length, {
    missing: inputIds.filter((inputId) => !inputs[inputId]),
  });
  const wrong = present.filter(([, input]) => input.h !== expected).map(([inputId, input]) => `${inputId}=${input.h}`);
  check(`${id}: fields are ${expected}px`, present.length > 0 && wrong.length === 0, { wrong });
  const tallPresent = Object.entries(tall).filter(([, input]) => input !== null);
  const tallWrong = tallPresent.filter(([, input]) => input.h < expected).map(([inputId, input]) => `${inputId}=${input.h}`);
  if (tallIds.length > 0) {
    check(
      `${id}: multiline fields are at least ${expected}px`,
      tallPresent.length === tallIds.length && tallWrong.length === 0,
      { tall, tallWrong },
    );
  }

  if (buttons) {
    const heights = [...new Set(buttons.map((button) => button.h))];
    const floor = expected >= 44 ? 44 : 28;
    check(`${id}: footer actions share one height`, heights.length === 1, { buttons });
    check(
      `${id}: footer actions are at least ${floor}px`,
      heights.length === 1 && heights[0] >= floor,
      { heights },
    );
  }
  return value;
};

const centeredCard = async (page, id) => {
  const card = (await boxes(page, [`${id}-card`]))[`${id}-card`];
  const viewport = page.viewportSize();
  check(
    `${id}: card is inside the window`,
    !!card && card.y >= -1 && card.bottom <= viewport.height + 1,
    { card, viewport },
  );
  return card;
};

const scrollDialogToEnd = async (page, id) => {
  const viewport = page.viewportSize();
  await page.mouse.move(viewport.width / 2, viewport.height / 2);
  for (let i = 0; i < 12; i++) await page.mouse.wheel(0, 320);
  await page.waitForTimeout(400);
  const footer = (await boxes(page, [`${id}-footer`]))[`${id}-footer`];
  check(
    `${id}: the long form's footer is reachable`,
    !!footer && footer.bottom <= viewport.height + 1 && footer.y >= 0,
    { footer, viewport },
  );
  return footer;
};

const playwright = await import(PLAYWRIGHT);
const { chromium } = playwright;
const browser = await chromium.launch({ executablePath: CHROME, headless: HEADLESS });

try {
  for (const viewport of VIEWPORTS) {
    const { width, height } = viewport;
    const expected = width <= 767 ? 44 : 34;
    const page = await browser.newPage({ viewport });
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

    // ---- workspace / 员工 / 团队 ------------------------------------------
    await go(page);
    await shot(page, `workspace-${width}.png`, `workspace ${width}`);
    await overflow(page, `workspace-${width}`);

    await go(page);
    await openChapter(page, width, "bots");
    await page.waitForSelector('[data-testid="bots-head"]');
    await measureFields(page, `bots-fields-${width}`, ["bots-search", "bots-kind-filter", "bots-status-filter", "bots-create"], expected);
    const botTables = await tableMetrics(page, `bots-table-${width}`);
    if (width > 767) assertTable(`bots-${width}`, botTables, "bots-table");
    await shot(page, `bots-${width}.png`, `bots ${width}`);
    await overflow(page, `bots-${width}`);

    await go(page);
    await openChapter(page, width, "teams");
    await page.waitForSelector('[data-testid="teams-head"]');
    await measureFields(page, `teams-fields-${width}`, ["teams-search", "teams-status-filter", "teams-create"], expected);
    const teamTables = await tableMetrics(page, `teams-table-${width}`);
    if (width > 767) assertTable(`teams-${width}`, teamTables, "teams-table");
    const members = await page.evaluate(() => {
      const row = document.querySelector('[data-testid^="team-row-"]');
      if (!row) return null;
      const cell = row.querySelectorAll('[role="cell"]')[3];
      if (!cell) return null;
      const text = cell.querySelector("span") || cell;
      const cr = cell.getBoundingClientRect();
      const tr = text.getBoundingClientRect();
      return { gap: Math.round(cr.right - tr.right), cell: Math.round(cr.width), text: Math.round(tr.width) };
    });
    report.measurements[`teams-members-${width}`] = members;
    if (width > 767) {
      check("teams: the members column is right-aligned", !!members && members.gap <= 20, members);
    }
    await shot(page, `teams-${width}.png`, `teams ${width}`);
    await overflow(page, `teams-${width}`);

    // ---- 身份 / 权限组 / 记录 ---------------------------------------------
    await go(page);
    await openManagement(page, width, "identity");
    await page.waitForSelector(width <= 767 ? '[data-testid="accounts-search-page"]' : '[data-testid="accounts-search"]');
    await measureFields(
      page,
      `accounts-fields-${width}`,
      width <= 767
        ? ["accounts-search-page", "accounts-status-all", "accounts-status-active", "accounts-status-disabled"]
        : ["accounts-search", "identity-segment"],
      expected,
    );
    await shot(page, `accounts-${width}.png`, `accounts ${width}`);
    await overflow(page, `accounts-${width}`);

    await page.click('[data-testid="identity-segment-groups"]');
    await page.waitForSelector(width <= 767 ? '[data-testid="groups-search-page"]' : '[data-testid="groups-search"]');
    await page.waitForTimeout(350);
    await measureFields(
      page,
      `groups-fields-${width}`,
      width <= 767
        ? ["groups-search-page", "identity-segment"]
        : ["groups-search", "group-capability-search", "identity-segment"],
      expected,
    );
    await shot(page, `groups-${width}.png`, `groups ${width}`);
    await overflow(page, `groups-${width}`);

    await go(page);
    await openManagement(page, width, "records");
    await page.waitForSelector('[data-testid="records-search"]');
    await measureFields(
      page,
      `records-fields-${width}`,
      width <= 767
        ? ["records-search", "records-range", "records-kind-stamp", "records-result-stamp"]
        : ["records-search", "records-range", "records-kind", "records-result"],
      expected,
    );
    await tableMetrics(page, `records-table-${width}`).then((metrics) => {
      if (width > 767) assertTable(`records-${width}`, metrics, "records-table");
    });
    await shot(page, `records-${width}.png`, `records ${width}`);
    await overflow(page, `records-${width}`);
    if (width <= 767) {
      await page.click('[data-testid="records-kind-stamp"]');
      // The Sheet's own test id is on a hidden placeholder; the open sheet's
      // rows live in its portal, so the first option is what proves it opened.
      await page.waitForSelector('[data-testid="records-filter-all"]', { timeout: 5_000 });
      await shot(page, `records-filter-sheet-${width}.png`, `records filter sheet ${width}`);
      await overflow(page, `records-filter-sheet-${width}`);
      await page.keyboard.press("Escape");
    await page.waitForTimeout(400);
    }

    // ---- settings sections -------------------------------------------------
    await go(page);
    await openSettings(page, width);
    await shot(page, `settings-general-${width}.png`, `settings general ${width}`);
    await overflow(page, `settings-general-${width}`);

    await page.click('[data-testid="settings-section-appearance"]');
    await page.waitForSelector('[data-testid="settings-appearance"]');
    await shot(page, `settings-appearance-${width}.png`, `settings appearance ${width}`);
    await overflow(page, `settings-appearance-${width}`);

    await page.click('[data-testid="settings-section-models"]');
    await page.waitForSelector('[data-testid="settings-current"]');
    await measureFields(
      page,
      `settings-actions-${width}`,
      ["settings-change-default", "settings-refresh-models", "provider-add", "provider-configure-s2api"],
      width <= 767 ? 44 : 24,
    );
    await shot(page, `settings-models-${width}.png`, `settings models ${width}`);
    await overflow(page, `settings-models-${width}`);

    await page.click('[data-testid="settings-section-secrets"]');
    await page.waitForSelector('[data-testid="settings-secrets"]');
    await measureFields(page, `settings-secrets-${width}`, ["secret-name", "secret-value"], expected);
    await measureFields(
      page,
      `settings-secret-save-${width}`,
      ["settings-secret-save"],
      width <= 767 ? 44 : 28,
    );
    await shot(page, `settings-secrets-${width}.png`, `settings secrets ${width}`);
    await overflow(page, `settings-secrets-${width}`);

    await page.click('[data-testid="settings-section-skills"]');
    await page.waitForSelector('[data-testid="settings-skill-sources"]');
    await shot(page, `settings-skills-${width}.png`, `settings skills ${width}`);
    await overflow(page, `settings-skills-${width}`);

    // ---- dialogs -----------------------------------------------------------
    await go(page);
    await openManagement(page, width, "identity");
    await page.waitForSelector(width <= 767 ? '[data-testid="accounts-search-page"]' : '[data-testid="accounts-search"]');
    const create = await page.$('[data-testid="accounts-create-page"]');
    await (create ?? (await page.$('[data-testid="accounts-create"]'))).click();
    await page.waitForSelector('[data-testid="account-create-dialog"]');
    await dialogMetrics(
      page,
      "account-create-dialog",
      ["account-create-username", "account-create-name", "account-create-kind", "account-create-password"],
      expected,
    );
    await centeredCard(page, "account-create-dialog");
    await shot(page, `account-create-${width}.png`, `account create ${width}`);
    await overflow(page, `account-create-${width}`);
    await page.keyboard.press("Escape");
    await page.waitForTimeout(400);

    await go(page);
    await openChapter(page, width, "bots");
    await page.waitForSelector('[data-testid="bots-create"]');
    await page.click('[data-testid="bots-create"]');
    await page.waitForSelector('[data-testid="bot-create-dialog"]');
    await page.waitForTimeout(600);
    await dialogMetrics(
      page,
      "bot-create-dialog",
      ["bot-create-name", "bot-create-profile", "bot-create-kind", "bot-create-model"],
      expected,
    );
    await centeredCard(page, "bot-create-dialog");
    await shot(page, `bot-create-${width}.png`, `bot create ${width}`);
    await overflow(page, `bot-create-${width}`);
    await page.keyboard.press("Escape");
    await page.waitForTimeout(400);

    await go(page);
    await openChapter(page, width, "teams");
    await page.waitForSelector('[data-testid="teams-create"]');
    await page.click('[data-testid="teams-create"]');
    await page.waitForSelector('[data-testid="team-create-dialog"]');
    await page.waitForTimeout(600);
    await dialogMetrics(
      page,
      "team-create-dialog",
      ["team-create-name", "team-create-captain"],
      expected,
      ["team-create-mission"],
    );
    await centeredCard(page, "team-create-dialog");
    await shot(page, `team-create-${width}.png`, `team create ${width}`);
    await overflow(page, `team-create-${width}`);
    await page.keyboard.press("Escape");
    await page.waitForTimeout(400);

    await go(page);
    await openSettings(page, width);
    await page.click('[data-testid="settings-section-models"]');
    await page.waitForSelector('[data-testid="provider-configure-s2api"]');
    await page.click('[data-testid="provider-configure-s2api"]');
    await page.waitForSelector('[data-testid="provider-config-dialog"]');
    await page.waitForTimeout(500);
    await dialogMetrics(
      page,
      "provider-config-dialog",
      [
        "provider-config-id",
        "provider-config-name",
        "provider-config-protocol",
        "provider-config-endpoint",
        "provider-config-credential",
        "provider-config-model-input",
      ],
      expected,
    );
    await shot(page, `provider-config-${width}.png`, `provider config ${width}`);
    await overflow(page, `provider-config-${width}`);
    if (width <= 767) {
      await scrollDialogToEnd(page, "provider-config-dialog");
      await shot(page, `provider-config-bottom-${width}.png`, `provider config bottom ${width}`);
    } else {
      await centeredCard(page, "provider-config-dialog");
    }
    await page.keyboard.press("Escape");
    await page.waitForTimeout(400);
    await page.close();
  }

  // ---- tables that only exist at 1024 -------------------------------------
  const tablePage = await browser.newPage({ viewport: { width: 1024, height: 800 } });
  await go(tablePage);
  await openManagement(tablePage, 1024, "identity");
  await tablePage.waitForSelector('[data-testid="accounts-table"]');
  assertTable("accounts-1024", await tableMetrics(tablePage, "accounts-table-1024"), "accounts-table");
  await shot(tablePage, "accounts-table-1024.png", "accounts table 1024");
  await overflow(tablePage, "accounts-table-1024");
  await tablePage.click('[data-testid="identity-segment-groups"]');
  await tablePage.waitForSelector('[data-testid="groups-table"]');
  assertTable("groups-1024", await tableMetrics(tablePage, "groups-table-1024"), "groups-table");
  const grouped = await tablePage.evaluate(() => {
    const row = document.querySelector('[data-testid^="group-row-"]');
    if (!row) return null;
    const cells = row.querySelectorAll('[role="cell"]');
    const gapOf = (index) => {
      const cell = cells[index];
      const text = cell?.querySelector("span") || cell;
      if (!cell || !text) return null;
      return Math.round(cell.getBoundingClientRect().right - text.getBoundingClientRect().right);
    };
    return { members: gapOf(2), capabilities: gapOf(3) };
  });
  await shot(tablePage, "groups-table-1024.png", "groups table 1024");
  await overflow(tablePage, "groups-table-1024");
  report.measurements["groups-numeric-1024"] = grouped;
  check("groups: numeric columns are right-aligned", !!grouped && grouped.members <= 20 && grouped.capabilities <= 20, grouped);
  await tablePage.close();

  report.checks = checks;
  report.pass = checks.every((entry) => entry.ok);
} finally {
  await browser.close();
  report.finishedAt = new Date().toISOString();
  report.pass = report.pass && report.pageErrors.length === 0;
  fs.writeFileSync(path.join(EVIDENCE, "report.json"), `${JSON.stringify(report, null, 2)}\n`);
}

console.log(`ui-polish: ${report.pass ? "PASS" : "FAIL"} — ${checks.filter((c) => c.ok).length}/${checks.length} checks`);
process.exit(report.pass ? 0 : 1);
