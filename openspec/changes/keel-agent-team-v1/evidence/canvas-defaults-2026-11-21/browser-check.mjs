// Real-browser check for the C4 canvas editing lane (change keel-agent-team-v1,
// task P5): team-level default tools/model override and the member seat editor
// (label/description, replace) in the real web bundle.
//
// The app is `nsh run --target web` on the scripted fixture transport
// (`?fixture=1`). Every PNG is a real Chrome render of that build, never a
// drawing. The fixture team already carries stored defaults, so the canvas
// opens with prefilled values instead of an empty demo form.
//
//   390px: teams › canvas → team defaults (tools + model) → member seat editor
//          (label/description) → replace-member picker
//   1280px: the same canvas in the wide layout
//
// `checks.json` records the viewport, the document scroll width, each measured
// element's bounds and the visible default values. A document wider than its
// viewport fails the run (`pass: false`), so "no horizontal overflow" is a
// measurement, not a claim.
//
// Usage:
//   nsh run --target web --port 13680
//   node openspec/changes/keel-agent-team-v1/evidence/canvas-defaults-2026-11-21/browser-check.mjs
import fs from "node:fs";
import path from "node:path";

const BASE = process.env.EVIDENCE_BASE || "http://127.0.0.1:13680/?fixture=1";
const OUT = path.resolve(import.meta.dirname);
const SHOTS = path.join(OUT, "screenshots");
const PLAYWRIGHT =
  process.env.PLAYWRIGHT_CORE || "/Users/kzf/.npm/_npx/e41f203b7505f1fb/node_modules/playwright-core/index.mjs";
const CHROME = process.env.EVIDENCE_CHROME || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";

fs.mkdirSync(SHOTS, { recursive: true });

const { chromium } = await import(PLAYWRIGHT);
const browser = await chromium.launch({ executablePath: CHROME, headless: true });

const checks = {
  base: BASE,
  generatedAt: new Date().toISOString(),
  states: {
    "canvas.defaults": "browser: 01-team-canvas-defaults-390.png / 05-team-canvas-defaults-1280.png",
    "canvas.member-edit": "browser: 02-team-canvas-seat-edit-390.png / 06-team-canvas-seat-edit-1280.png",
    "canvas.member-replace": "browser: 03-team-canvas-seat-replace-390.png / 07-team-canvas-seat-replace-1280.png",
    "canvas.member-replace-options": "browser: 04-team-canvas-seat-replace-options-390.png",
    "canvas.save/read-back": "TUI gate: settings-teams.test.tsx（defaults 保存/回读、席位文本、替换、失败留输入）",
    "canvas.five-states": "TUI gate: settings-teams.test.tsx（空/加载/错误/403/成功 + 保存中/失败）",
  },
  defaults: { tools: "", model: "" },
  viewports: {},
  pass: true,
  findings: [],
};

const measure = async (page, label) => {
  const data = await page.evaluate(() => {
    const root = document.scrollingElement ?? document.documentElement;
    const rect = (selector) => {
      const el = document.querySelector(selector);
      if (!el) return null;
      const r = el.getBoundingClientRect();
      return {
        left: Math.round(r.left),
        right: Math.round(r.right),
        top: Math.round(r.top),
        bottom: Math.round(r.bottom),
        width: Math.round(r.width),
        height: Math.round(r.height),
      };
    };
    return {
      innerWidth: window.innerWidth,
      innerHeight: window.innerHeight,
      scrollWidth: root.scrollWidth,
      dialog: rect("[data-testid=team-dialog]"),
      canvasDefaults: rect("[data-testid=canvas-defaults]"),
      defaultsTools: rect("[data-testid=team-defaults-tools]"),
      defaultsToolsFact: rect("[data-testid=team-defaults-tools-fact]"),
      defaultsModel: rect("[data-testid=team-defaults-model]"),
      canvasMembers: rect("[data-testid=canvas-members]"),
      memberRow: rect("[data-testid=canvas-member-agent_demo_research]"),
      seatEditor: rect("[data-testid=canvas-seat-editor]"),
      seatLabel: rect("[data-testid=canvas-seat-label]"),
      seatDescription: rect("[data-testid=canvas-seat-description]"),
      seatReplace: rect("[data-testid=canvas-seat-replace]"),
      seatReplaceAgent: rect("[data-testid=canvas-seat-replace-agent]"),
    };
  });
  checks.viewports[label] = data;
  if (data.scrollWidth > data.innerWidth) {
    checks.pass = false;
    checks.findings.push(`${label}: document scrollWidth ${data.scrollWidth} > viewport ${data.innerWidth}`);
  }
  for (const [name, value] of Object.entries(data)) {
    if (value && typeof value === "object" && "right" in value && value.right > data.innerWidth) {
      checks.pass = false;
      checks.findings.push(`${label}: ${name} right ${value.right} > viewport ${data.innerWidth}`);
    }
  }
  return data;
};

let shotIndex = 0;
const shot = async (page, name) => {
  shotIndex += 1;
  const file = `${String(shotIndex).padStart(2, "0")}-${name}.png`;
  await page.waitForTimeout(300);
  // The canvas is a fixed overlay; a viewport shot shows what the reader sees.
  await page.screenshot({ path: path.join(SHOTS, file) });
  return `screenshots/${file}`;
};

const wait = (page, selector, timeout = 8000) => page.waitForSelector(selector, { state: "visible", timeout });
const textOf = async (page, selector) =>
  (await page.locator(selector).first().innerText().catch(() => "")) ?? "";

const fillField = async (page, testID, value) => {
  const root = page.locator(`[data-testid="${testID}"]`);
  const tag = await root.evaluate((el) => el.tagName).catch(() => "");
  const target = tag === "INPUT" || tag === "TEXTAREA" ? root : root.locator("input, textarea").first();
  await target.fill(value);
};

const openSession = async (page) => {
  await page.goto(BASE, { waitUntil: "networkidle" });
  await page.waitForTimeout(700);
  await page.getByText("发布准备周报", { exact: true }).click();
  await wait(page, "[data-testid=composer-input]");
  await page.waitForTimeout(500);
};

const openTeams = async (page) => {
  await page.locator("[data-testid=workspace-header-back]").click().catch(() => {});
  await page.waitForTimeout(300);
  await page.locator('[aria-label="打开设置"]').click();
  await wait(page, "[data-testid=settings-page]");
  await page.locator("[data-testid=settings-section-teams]").click();
  await wait(page, "[data-testid=team-card-team_demo_brand]");
  await page.waitForTimeout(700);
};

const openCanvas = async (page) => {
  await page.getByLabel("编辑团队 品牌发布小组").click();
  await wait(page, "[data-testid=team-dialog]");
  await page.waitForTimeout(500);
};

const readDefaults = async (page) => {
  const tools = await textOf(page, "[data-testid=team-defaults-tools-fact]");
  const model = await textOf(page, "[data-testid=team-defaults-model]");
  checks.defaults = { tools, model };
  if (!tools.includes("workflow.plan") || !tools.includes("clock.now")) {
    checks.pass = false;
    checks.findings.push(`team defaults: stored tools are not visible on open: ${JSON.stringify(tools)}`);
  }
  if (!model.includes("DeepSeek V4.1 Flash")) {
    checks.pass = false;
    checks.findings.push(`team defaults: the stored model override is not visible on open: ${JSON.stringify(model)}`);
  }
};

const checkSeatEditor = (data, label) => {
  if (!data.seatEditor || !data.seatLabel || !data.seatDescription) {
    checks.pass = false;
    checks.findings.push(`${label}: the seat editor (label/description fields) is missing`);
  }
};
const checkReplace = (data, label) => {
  if (!data.seatReplace || !data.seatReplaceAgent) {
    checks.pass = false;
    checks.findings.push(`${label}: the replace-member picker is missing`);
  }
};

// ---- 390px ----------------------------------------------------------------
{
  const viewport = "390x844";
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2 });
  const page = await context.newPage();
  await openSession(page);
  await openTeams(page);
  await openCanvas(page);

  await page.locator("[data-testid=canvas-defaults]").scrollIntoViewIfNeeded().catch(() => {});
  await page.waitForTimeout(350);
  const defaults = await measure(page, `${viewport}·canvas defaults`);
  if (!defaults.canvasDefaults || !defaults.defaultsTools || !defaults.defaultsModel) {
    checks.pass = false;
    checks.findings.push(`${viewport}: the team defaults section is missing`);
  }
  await readDefaults(page);
  await shot(page, "team-canvas-defaults-390");

  await page.locator("[data-testid=canvas-member-edit-agent_demo_research]").scrollIntoViewIfNeeded().catch(() => {});
  await page.locator("[data-testid=canvas-member-edit-agent_demo_research]").click();
  await wait(page, "[data-testid=canvas-seat-editor]");
  await fillField(page, "canvas-seat-label", "资料检索").catch((error) => {
    checks.findings.push(`${viewport}: could not type into the seat label: ${String(error)}`);
  });
  await fillField(page, "canvas-seat-description", "只做检索与整理").catch((error) => {
    checks.findings.push(`${viewport}: could not type into the seat description: ${String(error)}`);
  });
  await page.locator("[data-testid=canvas-seat-editor]").scrollIntoViewIfNeeded().catch(() => {});
  await page.waitForTimeout(350);
  const editor = await measure(page, `${viewport}·seat editor`);
  checkSeatEditor(editor, viewport);
  await shot(page, "team-canvas-seat-edit-390");

  await page.locator("[data-testid=canvas-member-replace-agent_demo_research]").click();
  await wait(page, "[data-testid=canvas-seat-replace]");
  await page.locator("[data-testid=canvas-seat-replace]").scrollIntoViewIfNeeded().catch(() => {});
  await page.waitForTimeout(350);
  const replace = await measure(page, `${viewport}·seat replace`);
  checkReplace(replace, viewport);
  await shot(page, "team-canvas-seat-replace-390");

  // The picker's candidate list: 运营 is seated nowhere, so it is offered.
  await page.locator("[data-testid=canvas-seat-replace-agent]").click();
  await page.waitForTimeout(400);
  const offered = await page
    .getByText("运营", { exact: true })
    .last()
    .waitFor({ state: "visible", timeout: 2500 })
    .then(() => true)
    .catch(() => false);
  if (!offered) {
    checks.pass = false;
    checks.findings.push(`${viewport}: the replace picker does not offer the unseated Agent 运营`);
  }
  await shot(page, "team-canvas-seat-replace-options-390");

  await context.close();
}

// ---- 1280px ---------------------------------------------------------------
{
  const viewport = "1280x900";
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, deviceScaleFactor: 1 });
  const page = await context.newPage();
  await openSession(page);
  await openTeams(page);
  await openCanvas(page);

  await page.locator("[data-testid=canvas-defaults]").scrollIntoViewIfNeeded().catch(() => {});
  await page.waitForTimeout(400);
  const defaults = await measure(page, `${viewport}·canvas defaults`);
  if (!defaults.canvasDefaults || !defaults.defaultsTools || !defaults.defaultsModel) {
    checks.pass = false;
    checks.findings.push(`${viewport}: the team defaults section is missing`);
  }
  await readDefaults(page);
  await shot(page, "team-canvas-defaults-1280");

  await page.locator("[data-testid=canvas-member-edit-agent_demo_research]").scrollIntoViewIfNeeded().catch(() => {});
  await page.locator("[data-testid=canvas-member-edit-agent_demo_research]").click();
  await wait(page, "[data-testid=canvas-seat-editor]");
  await page.locator("[data-testid=canvas-seat-editor]").scrollIntoViewIfNeeded().catch(() => {});
  await page.waitForTimeout(350);
  const editor = await measure(page, `${viewport}·seat editor`);
  checkSeatEditor(editor, viewport);
  await shot(page, "team-canvas-seat-edit-1280");

  await page.locator("[data-testid=canvas-member-replace-agent_demo_research]").click();
  await wait(page, "[data-testid=canvas-seat-replace]");
  await page.locator("[data-testid=canvas-seat-replace]").scrollIntoViewIfNeeded().catch(() => {});
  await page.waitForTimeout(350);
  const replace = await measure(page, `${viewport}·seat replace`);
  checkReplace(replace, viewport);
  await shot(page, "team-canvas-seat-replace-1280");

  await context.close();
}

await browser.close();
fs.writeFileSync(path.join(OUT, "checks.json"), JSON.stringify(checks, null, 2) + "\n");
console.log(
  JSON.stringify(
    {
      pass: checks.pass,
      findings: checks.findings,
      defaults: checks.defaults,
      screenshots: fs.readdirSync(SHOTS).sort(),
    },
    null,
    2,
  ),
);
