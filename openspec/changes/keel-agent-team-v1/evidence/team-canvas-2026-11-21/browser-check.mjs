// Real-browser check for the team canvas lane (change keel-agent-team-v1,
// task P5: team card · captain slot · member library · Team CLI panel).
//
// The app is the real web bundle (`nsh run --target web`) on the scripted
// fixture transport (`?fixture=1`). Every PNG is a real Chrome render of that
// build, never a drawing. The script walks the product the way a reader does:
//
//   390px: open a session → settings › 团队 → card (captain slot, members,
//          Team CLI facts) → Team CLI panel → 团队画布 → member library →
//          inline new-Agent form
//   1280px: the same card and canvas in the wide layout
//
// `checks.json` records the viewport, the document scroll width, each measured
// element's bounds and the manifest command rows found on screen. A document
// wider than its viewport fails the run (`pass: false`), so "no horizontal
// overflow" is a measurement, not a claim.
//
// Usage:
//   nsh run --target web --port 13680
//   node openspec/changes/keel-agent-team-v1/evidence/team-canvas-2026-11-21/browser-check.mjs
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

const MANIFEST_COMMANDS = [
  "keel team list",
  "keel team show",
  "keel team create",
  "keel team update",
  "keel team delete",
  "keel team member add",
  "keel team member remove",
  "keel team save",
  "keel team start",
];

const checks = {
  base: BASE,
  generatedAt: new Date().toISOString(),
  states: {
    "teams.card": "browser: 01-settings-teams-390.png / 08-settings-teams-1280.png",
    "teams.captain-slot": "measurement: captainSlot in checks.json",
    "teams.team-cli-facts": "measurement: teamCliFacts in checks.json",
    "teams.cli-panel": "browser: 02-team-cli-panel-390.png",
    "teams.no-captain": "browser: 06-settings-teams-no-captain-390.png",
    "canvas.top": "browser: 03-team-canvas-390.png / 09-team-canvas-1280.png",
    "canvas.no-captain": "browser: 07-team-canvas-no-captain-390.png",
    "canvas.library": "browser: 04-team-canvas-library-390.png",
    "canvas.new-agent": "browser: 05-team-canvas-new-agent-390.png",
    "canvas.five-states": "TUI gate: settings-teams.test.tsx（空/加载/错误/403/成功 + 保存中/失败）",
  },
  manifest: {
    expected: MANIFEST_COMMANDS,
    rendered: [],
    extras: [],
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
      teamCard: rect("[data-testid=team-card-team_demo_brand]"),
      captainSlot: rect("[data-testid=team-captain-slot-team_demo_brand]"),
      memberFacts: rect("[data-testid=team-members-team_demo_brand]"),
      teamCliFacts: rect("[data-testid=team-cli-facts-team_demo_brand]"),
      blockedCard: rect("[data-testid=team-card-team_demo_pending]"),
      blockedSlot: rect("[data-testid=team-captain-slot-team_demo_pending]"),
      captainBlocker: rect("[data-testid=canvas-captain-blocker]"),
      cliPanel: rect("[data-testid=team-cli-panel]"),
      cliRow: rect("[data-testid=cli-capability-team-list]"),
      dialog: rect("[data-testid=team-dialog]"),
      canvasCaptain: rect("[data-testid=canvas-captain-seat]"),
      canvasMembers: rect("[data-testid=canvas-members]"),
      canvasLibrary: rect("[data-testid=canvas-library]"),
      newAgent: rect("[data-testid=canvas-new-agent]"),
      save: rect("[data-testid=team-save]"),
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
const shot = async (page, name, clip) => {
  shotIndex += 1;
  const file = `${String(shotIndex).padStart(2, "0")}-${name}.png`;
  await page.waitForTimeout(250);
  await page.screenshot({ path: path.join(SHOTS, file), fullPage: clip === undefined, ...(clip ? { clip } : {}) });
  return `screenshots/${file}`;
};

const wait = (page, selector, timeout = 8000) => page.waitForSelector(selector, { state: "visible", timeout });

const openSession = async (page, url = BASE) => {
  await page.goto(url, { waitUntil: "networkidle" });
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

const readManifestRows = async (page) =>
  page.evaluate(() =>
    Array.from(document.querySelectorAll("[data-testid^=cli-capability-]"))
      .map((el) => el.textContent ?? "")
      .map((text) => {
        const match = text.match(/keel\s+team(?:\s+[a-z]+)+/);
        return match ? match[0] : "";
      })
      .filter(Boolean),
  );

// ---- 390px ----------------------------------------------------------------
{
  const viewport = "390x844";
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2 });
  const page = await context.newPage();
  await openSession(page);
  await openTeams(page);

  const teams = await measure(page, `${viewport}·settings teams`);
  if (teams.captainSlot === null) {
    checks.findings.push(`${viewport}·settings teams: the captain slot is missing`);
    checks.pass = false;
  }
  if (teams.memberFacts === null || teams.teamCliFacts === null) {
    checks.findings.push(`${viewport}·settings teams: member/capability facts are missing`);
    checks.pass = false;
  }
  checks.manifest.rendered = await readManifestRows(page);
  checks.manifest.extras = checks.manifest.rendered.filter((cmd) => !MANIFEST_COMMANDS.includes(cmd));
  for (const command of MANIFEST_COMMANDS) {
    if (!checks.manifest.rendered.includes(command)) {
      checks.findings.push(`${viewport}·CLI panel: manifest command missing on screen: ${command}`);
      checks.pass = false;
    }
  }
  if (checks.manifest.extras.length > 0) {
    checks.findings.push(`${viewport}·CLI panel: commands not declared by the manifest: ${checks.manifest.extras.join(", ")}`);
    checks.pass = false;
  }
  await shot(page, "settings-teams-390");
  await page.locator("[data-testid=team-cli-panel]").scrollIntoViewIfNeeded().catch(() => {});
  await page.waitForTimeout(300);
  await shot(page, "team-cli-panel-390");

  await page.getByLabel("编辑团队 品牌发布小组").click();
  await wait(page, "[data-testid=team-dialog]");
  await page.waitForTimeout(500);
  const canvas = await measure(page, `${viewport}·team canvas`);
  if (canvas.dialog === null || canvas.canvasCaptain === null || canvas.canvasLibrary === null) {
    checks.findings.push(`${viewport}·team canvas: dialog/captain slot/library missing`);
    checks.pass = false;
  }
  await shot(page, "team-canvas-390");

  await page.locator("[data-testid=canvas-library]").scrollIntoViewIfNeeded().catch(() => {});
  await page.waitForTimeout(400);
  await measure(page, `${viewport}·team canvas library`);
  await shot(page, "team-canvas-library-390");

  await page.locator("[data-testid=canvas-create-agent]").click();
  await wait(page, "[data-testid=canvas-new-agent]");
  await page.locator("[data-testid=canvas-new-agent]").scrollIntoViewIfNeeded().catch(() => {});
  await page.waitForTimeout(350);
  await measure(page, `${viewport}·team canvas new-agent`);
  await shot(page, "team-canvas-new-agent-390");

  await context.close();
}

// ---- 390px · missing-captain blocker --------------------------------------
{
  const viewport = "390x844·no-captain";
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2 });
  const page = await context.newPage();
  await openSession(page, `${BASE}&teamNoCaptain=1`);
  await openTeams(page);

  await page.waitForSelector("[data-testid=team-card-team_demo_pending]", { state: "visible", timeout: 5000 });
  await page.locator("[data-testid=team-card-team_demo_pending]").scrollIntoViewIfNeeded().catch(() => {});
  await page.waitForTimeout(350);
  const data = await measure(page, viewport);
  if (data.blockedSlot === null) {
    checks.findings.push(`${viewport}: the missing-captain slot is missing`);
    checks.pass = false;
  }
  await shot(page, "settings-teams-no-captain-390");

  await page.getByLabel("编辑团队 待定小组").click();
  await wait(page, "[data-testid=team-dialog]");
  await page.waitForTimeout(500);
  const canvas = await measure(page, `${viewport}·canvas`);
  if (canvas.captainBlocker === null) {
    checks.findings.push(`${viewport}: the canvas captain blocker is missing`);
    checks.pass = false;
  }
  await shot(page, "team-canvas-no-captain-390");

  await context.close();
}

// ---- 1280px ---------------------------------------------------------------
{
  const viewport = "1280x900";
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, deviceScaleFactor: 1 });
  const page = await context.newPage();
  await openSession(page);
  await openTeams(page);

  await measure(page, `${viewport}·settings teams`);
  await shot(page, "settings-teams-1280");

  await page.getByLabel("编辑团队 品牌发布小组").click();
  await wait(page, "[data-testid=team-dialog]");
  await page.waitForTimeout(500);
  await measure(page, `${viewport}·team canvas`);
  await shot(page, "team-canvas-1280");

  await context.close();
}

await browser.close();
fs.writeFileSync(path.join(OUT, "checks.json"), JSON.stringify(checks, null, 2) + "\n");
console.log(JSON.stringify({ pass: checks.pass, findings: checks.findings, manifest: checks.manifest, screenshots: shotsDone() }, null, 2));

function shotsDone() {
  return fs.readdirSync(SHOTS).sort();
}
