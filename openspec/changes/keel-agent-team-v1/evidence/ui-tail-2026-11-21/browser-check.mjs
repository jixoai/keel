// Real-browser check for the change's UI tail (change keel-agent-team-v1).
//
// The development host is the real web bundle (`nsh run --target web`) with
// the scripted fixture transport (`?fixture=1`); every screenshot is a real
// Chrome render of this build, never a drawing. The script drives the product
// the way a reader does:
//
//   390px: open a session (standard header) → team mode → team header →
//          roster → settings › Agents (list) → new Agent (inline / file /
//          validation failure) → settings › 团队
//   1280px: the same header and Agent editor in the wide layout
//
// It writes one PNG per step and checks.json with the viewport, the document's
// scroll width and the header's control bounds. A document wider than its
// viewport fails the run (`pass: false`), so "no horizontal overflow" is a
// measurement, not a claim.
//
// Usage:
//   nsh run --target web --port 13680
//   node openspec/changes/keel-agent-team-v1/evidence/ui-tail-2026-11-21/browser-check.mjs
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
    "agents.loading": "TUI gate: settings-agents.test.tsx › 加载态（骨架 + 主操作禁用）",
    "agents.empty": "TUI gate: settings-agents.test.tsx › 空态",
    "agents.error": "TUI gate: settings-agents.test.tsx › 错误态（500，原因逐字）",
    "agents.permission": "TUI gate: settings-agents.test.tsx › 权限不足（403 独立面）",
    "agents.success": "browser: 05-settings-agents-390.png / 12-settings-agents-1280.png",
    "agents.editor.file": "browser: 07-agent-editor-file-390.png / 13-agent-editor-file-1280.png",
    "agents.editor.validation": "browser: 08-agent-editor-file-error-390.png",
    "agents.saving": "TUI gate: settings-agents.test.tsx › 保存中（字段锁定）",
    "agents.save-failed": "TUI gate: settings-agents.test.tsx › 保存失败（输入保留）",
    "teams.loading": "TUI gate: settings-teams.test.tsx › 加载态",
    "teams.empty": "TUI gate: settings-teams.test.tsx › 空态",
    "teams.error": "TUI gate: settings-teams.test.tsx › 错误态（500）",
    "teams.permission": "TUI gate: settings-teams.test.tsx › 权限不足（403）",
    "teams.success": "browser: 09-settings-teams-390.png",
    "teams.saving": "TUI gate: settings-teams.test.tsx › 保存中",
    "teams.save-failed": "TUI gate: settings-teams.test.tsx › 保存失败",
    "header.standard": "browser: 01-session-standard-390.png / 10-session-standard-1280.png",
    "header.team": "browser: 03-session-team-390.png / 11-session-team-1280.png",
    "header.roster.loading": "TUI gate: session-team.test.tsx › 加载态",
    "header.roster.empty": "TUI gate: session-team.test.tsx › 只有队长",
    "header.roster.error": "TUI gate: session-team.test.tsx › 错误态",
    "header.roster.permission": "TUI gate: session-team.test.tsx › 403",
    "header.roster.success": "browser: 04-team-roster-390.png",
  },
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
      return { left: Math.round(r.left), right: Math.round(r.right), top: Math.round(r.top), bottom: Math.round(r.bottom), width: Math.round(r.width), height: Math.round(r.height) };
    };
    return {
      innerWidth: window.innerWidth,
      innerHeight: window.innerHeight,
      scrollWidth: root.scrollWidth,
      header: rect("[data-testid=workspace-header-content]"),
      title: rect("[data-testid=workspace-header-title]"),
      modelChip: rect("[data-testid=session-model-chip]"),
      members: rect("[data-testid=session-team-members]"),
      facts: rect("[data-testid=workspace-header-team-facts]"),
      modelFacts: rect("[data-testid=workspace-header-model-facts]"),
      agentCard: rect("[data-testid=agent-card-agent_demo_design]"),
      teamCard: rect("[data-testid=team-card-team_demo_brand]"),
    };
  });
  checks.viewports[label] = data;
  if (data.scrollWidth > data.innerWidth) {
    checks.pass = false;
    checks.findings.push(`${label}: document scrollWidth ${data.scrollWidth} > viewport ${data.innerWidth}`);
  }
  return data;
};

let shotIndex = 0;
const shot = async (page, name, clip) => {
  shotIndex += 1;
  const file = `${String(shotIndex).padStart(2, "0")}-${name}.png`;
  await page.waitForTimeout(250);
  await page.screenshot({ path: path.join(SHOTS, file), fullPage: clip === undefined, ...(clip ? { clip } : {}) });
  return `screenshots/${file}`;
};

const wait = (page, selector, timeout = 8000) => page.waitForSelector(selector, { state: "visible", timeout });

const openSession = async (page) => {
  await page.goto(BASE, { waitUntil: "networkidle" });
  await page.waitForTimeout(700);
  await page.getByText("发布准备周报", { exact: true }).click();
  await wait(page, "[data-testid=composer-input]");
  await page.waitForTimeout(500);
};

const startTeam = async (page) => {
  await page.locator("[data-testid=session-model-chip]").click();
  await wait(page, "[data-testid=picker-mode-team]");
  await page.waitForTimeout(250);
  await page.locator("[data-testid=picker-mode-team]").click();
  await wait(page, "[data-testid=picker-team-start-team_demo_brand]");
  await page.waitForTimeout(250);
  await page.locator("[data-testid=picker-team-start-team_demo_brand]").click();
  await wait(page, "[data-testid=workspace-header-team-facts]");
  await page.waitForTimeout(500);
};

const openSettingsSection = async (page, section) => {
  await page.locator("[data-testid=workspace-header-back]").click().catch(() => {});
  await page.waitForTimeout(300);
  await page.locator('[aria-label="打开设置"]').click();
  await wait(page, "[data-testid=settings-page]");
  await page.locator(`[data-testid=settings-section-${section}]`).click();
  await page.waitForTimeout(700);
};

const fill = async (page, testID, text) => {
  const field = page.locator(`[data-testid=${testID}]`);
  await field.click();
  await page.keyboard.type(text);
};

const closeDialog = async (page, testID) => {
  const dialog = page.locator(`[data-testid=${testID}]`);
  const close = dialog.locator('[aria-label="关闭"]').first();
  if ((await close.count()) > 0) {
    await close.click({ force: true }).catch(() => {});
  } else {
    await page.keyboard.press("Escape");
  }
  await page.waitForSelector(`[data-testid=${testID}]`, { state: "hidden", timeout: 5000 }).catch(() => {});
  // The native modal releases focus/inert on its exit; give it a beat before
  // the next control is pressed.
  await page.waitForTimeout(700);
};

// ---- 390px ----------------------------------------------------------------
{
  const viewport = "390x844";
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2 });
  const page = await context.newPage();
  await openSession(page);
  const standard = await measure(page, `${viewport}·standard header`);
  if (standard.modelFacts === null) {
    checks.findings.push(`${viewport}·standard header: the model's second line is missing`);
    checks.pass = false;
  }
  await shot(page, "session-standard-390", { x: 0, y: 0, width: 390, height: 120 });

  await page.locator("[data-testid=session-model-chip]").click();
  await wait(page, "[data-testid=picker-mode-team]");
  await page.locator("[data-testid=picker-mode-team]").click();
  await wait(page, "[data-testid=picker-team-start-team_demo_brand]");
  await page.waitForTimeout(250);
  await shot(page, "picker-team-mode-390");
  await page.locator("[data-testid=picker-team-start-team_demo_brand]").click();
  await wait(page, "[data-testid=workspace-header-team-facts]");
  await page.waitForTimeout(500);

  const team = await measure(page, `${viewport}·team header`);
  if (team.modelChip === null || team.members === null) {
    checks.findings.push(`${viewport}·team header: the team or roster entry is missing`);
    checks.pass = false;
  }
  if (team.members.right > team.innerWidth) {
    checks.findings.push(`${viewport}·team header: roster entry right ${team.members.right} > ${team.innerWidth}`);
    checks.pass = false;
  }
  await shot(page, "session-team-390", { x: 0, y: 0, width: 390, height: 140 });

  await page.locator("[data-testid=session-team-members]").click();
  await wait(page, "[data-testid=team-roster-dialog]");
  await page.waitForTimeout(400);
  await measure(page, `${viewport}·team roster`);
  await shot(page, "team-roster-390");
  await closeDialog(page, "team-roster-dialog");

  await openSettingsSection(page, "agents");
  await wait(page, "[data-testid=agent-card-agent_demo_design]");
  const agents = await measure(page, `${viewport}·settings agents`);
  if (agents.agentCard !== null && agents.agentCard.right > agents.innerWidth) {
    checks.findings.push(`${viewport}·settings agents: card right ${agents.agentCard.right} > ${agents.innerWidth}`);
    checks.pass = false;
  }
  await shot(page, "settings-agents-390");

  await page.locator("[data-testid=agent-create]").click();
  await wait(page, "[data-testid=agent-dialog]");
  await page.waitForTimeout(300);
  await measure(page, `${viewport}·agent editor inline`);
  await shot(page, "agent-editor-inline-390");
  await page.locator("[data-testid=agent-prompt-mode-file]").click();
  await wait(page, "[data-testid=agent-prompt-file]");
  await page.waitForTimeout(250);
  await measure(page, `${viewport}·agent editor file`);
  await shot(page, "agent-editor-file-390");
  await fill(page, "agent-name", "发布助手");
  await fill(page, "agent-prompt-file", "/etc/passwd");
  await page.locator("[data-testid=agent-save]").click();
  await wait(page, "[data-testid=agent-prompt-error]");
  await page.waitForTimeout(300);
  await measure(page, `${viewport}·agent editor validation`);
  await shot(page, "agent-editor-file-error-390");
  await closeDialog(page, "agent-dialog");

  await page.locator("[data-testid=settings-section-teams]").click();
  await wait(page, "[data-testid=team-card-team_demo_brand]");
  await page.waitForTimeout(500);
  await measure(page, `${viewport}·settings teams`);
  await shot(page, "settings-teams-390");

  await context.close();
}

// ---- 1280px ---------------------------------------------------------------
{
  const viewport = "1280x900";
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, deviceScaleFactor: 1 });
  const page = await context.newPage();
  await openSession(page);
  const standard = await measure(page, `${viewport}·standard header`);
  if (standard.modelFacts !== null) {
    checks.findings.push(`${viewport}·standard header: the second line duplicates the wide chip`);
    checks.pass = false;
  }
  await shot(page, "session-standard-1280", { x: 0, y: 0, width: 1280, height: 120 });

  await startTeam(page);
  const team = await measure(page, `${viewport}·team header`);
  if (team.facts === null || team.members === null) {
    checks.findings.push(`${viewport}·team header: facts or roster entry is missing`);
    checks.pass = false;
  }
  await shot(page, "session-team-1280", { x: 0, y: 0, width: 1280, height: 140 });

  await openSettingsSection(page, "agents");
  await wait(page, "[data-testid=agent-card-agent_demo_design]");
  await page.waitForTimeout(400);
  await measure(page, `${viewport}·settings agents`);
  await shot(page, "settings-agents-1280");

  await page.locator("[data-testid=agent-create]").click();
  await wait(page, "[data-testid=agent-dialog]");
  await page.locator("[data-testid=agent-prompt-mode-file]").click();
  await wait(page, "[data-testid=agent-prompt-file]");
  await page.waitForTimeout(350);
  await measure(page, `${viewport}·agent editor file`);
  await shot(page, "agent-editor-file-1280");
  await closeDialog(page, "agent-dialog");

  await context.close();
}

await browser.close();
fs.writeFileSync(path.join(OUT, "checks.json"), JSON.stringify(checks, null, 2) + "\n");
console.log(JSON.stringify({ pass: checks.pass, findings: checks.findings, screenshots: shotsDone() }, null, 2));

function shotsDone() {
  return fs.readdirSync(SHOTS).sort();
}
