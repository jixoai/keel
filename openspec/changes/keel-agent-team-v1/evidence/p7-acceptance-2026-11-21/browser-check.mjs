// Real-browser visual review for P7 (change keel-agent-team-v1).
//
// The build is the real web bundle (`nsh run --target web`) on the scripted
// fixture transport (`?fixture=1`); every PNG is a real Chrome render, never a
// drawing. The script walks the product page by page:
//
//   390px: workspace (standard header) -> model picker (standard/team) -> team
//          workspace -> roster -> settings (10 sections, provider dialog,
//          Agent editor, team canvas, Team CLI panel) -> plugin page (inventory,
//          detail, failed plugin) -> command palette -> missing-captain blocker
//          -> kit gallery (header + every group)
//   1280px: the same pages in the wide layout
//
// It records, per page, the document's scroll width and key element bounds; a
// document wider than its viewport, or a measured card whose right edge leaves
// the viewport, fails the run (`pass: false`). The gallery's manifest count /
// rendered count / missing list are read from its own header, so "the gallery
// renders" is a text fact, not a claim.
//
// The gallery is a dev-only route (`/kit`); the dev server serves its page at
// the root only, so `/kit?fixture=1` is fulfilled with the same dev HTML (the
// bundle and websocket are origin-rooted). This is the route `web/App.tsx`
// defines for a dev launch.
//
// Usage:
//   nsh run --target web --port 13680
//   node openspec/changes/keel-agent-team-v1/evidence/p7-acceptance-2026-11-21/browser-check.mjs
import fs from "node:fs";
import path from "node:path";

const BASE = process.env.EVIDENCE_BASE || "http://127.0.0.1:13680";
const OUT = path.resolve(import.meta.dirname);
const SHOTS = path.join(OUT, "screenshots");
const PLAYWRIGHT =
  process.env.PLAYWRIGHT_CORE || "/Users/kzf/.npm/_npx/e41f203b7505f1fb/node_modules/playwright-core/index.mjs";
const CHROME = process.env.EVIDENCE_CHROME || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const FIXTURE = `${BASE}/?fixture=1`;

fs.mkdirSync(SHOTS, { recursive: true });

const { chromium } = await import(PLAYWRIGHT);
const indexHTML = await (await fetch(`${BASE}/`)).text();
const browser = await chromium.launch({
  executablePath: CHROME,
  headless: true,
  // Chrome's local-network gate blocks the dev host's loopback websocket in
  // headless contexts; the page itself is loopback, so the check is disabled.
  args: ["--disable-features=LocalNetworkAccessChecks"],
});

const checks = {
  base: BASE,
  chrome: CHROME,
  generatedAt: new Date().toISOString(),
  gallery: { manifest: null, rendered: null, objects: null, missing: null, exports: null },
  pages: {},
  shots: [],
  states: {
    "agents.loading": "TUI gate: settings-agents.test.tsx › 加载态（骨架 + 主操作禁用）",
    "agents.empty": "TUI gate: settings-agents.test.tsx › 空态",
    "agents.error": "TUI gate: settings-agents.test.tsx › 错误态（500，原因逐字）",
    "agents.permission": "TUI gate: settings-agents.test.tsx › 权限不足（403 独立面）",
    "agents.success": "browser: settings-agents-*.png",
    "agents.editor.file": "browser: agent-editor-file-*.png",
    "agents.editor.validation": "browser: agent-editor-validation-*.png",
    "agents.saving": "TUI gate: settings-agents.test.tsx › 保存中（字段锁定）",
    "agents.save-failed": "TUI gate: settings-agents.test.tsx › 保存失败（输入保留）",
    "teams.loading": "TUI gate: settings-teams.test.tsx › 加载态",
    "teams.empty": "TUI gate: settings-teams.test.tsx › 空态",
    "teams.error": "TUI gate: settings-teams.test.tsx › 错误态（500）",
    "teams.permission": "TUI gate: settings-teams.test.tsx › 权限不足（403）",
    "teams.success": "browser: settings-teams-*.png",
    "teams.no-captain": "browser: settings-teams-no-captain-390.png / team-canvas-no-captain-390.png",
    "teams.saving": "TUI gate: settings-teams.test.tsx › 保存中",
    "teams.save-failed": "TUI gate: settings-teams.test.tsx › 保存失败",
    "teams.canvas": "browser: team-canvas-*.png",
    "teams.team-cli": "browser: team-cli-panel-390.png",
    "header.standard": "browser: workspace-standard-*.png",
    "header.team": "browser: workspace-team-*.png",
    "header.roster.loading": "TUI gate: session-team.test.tsx › 加载态",
    "header.roster.empty": "TUI gate: session-team.test.tsx › 只有队长",
    "header.roster.error": "TUI gate: session-team.test.tsx › 错误态",
    "header.roster.permission": "TUI gate: session-team.test.tsx › 403",
    "header.roster.success": "browser: team-roster-390.png",
    "plugins.success": "browser: plugins-page-*.png",
    "plugins.error": "browser: plugin-failed-390.png（demo.webhook 失败态）",
    "plugins.cli-tree": "browser: plugin-cli-tree-390.png",
    "gallery.page": "browser: kit-gallery-*.png",
    "gallery.missing": "read from the gallery header (see checks.gallery)",
    "gallery.five-states": "manifest renders default/disabled/error (or empty) per component; missing names listed",
  },
  pass: true,
  findings: [],
};

const measure = async (page, label, selectors = []) => {
  const data = await page.evaluate((selectors) => {
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
    const selected = {};
    for (const selector of selectors) selected[selector] = rect(selector);
    const offenders = [];
    for (const el of document.querySelectorAll("body *")) {
      const cs = getComputedStyle(el);
      if (cs.display === "none" || cs.visibility === "hidden" || Number(cs.opacity) === 0) continue;
      const r = el.getBoundingClientRect();
      if (r.width <= 0 || r.height <= 0) continue;
      if (r.right <= innerWidth + 0.5 && r.left >= -0.5) continue;
      const aria = el.getAttribute("aria-label") || "";
      if (/^\d+\.$/.test(aria)) continue;
      let p = el.parentElement;
      let intentional = false;
      while (p && p !== document.body) {
        const ps = getComputedStyle(p);
        if ((ps.overflowX === "auto" || ps.overflowX === "scroll") && p.scrollWidth > p.clientWidth) {
          intentional = true;
          break;
        }
        p = p.parentElement;
      }
      if (intentional) continue;
      offenders.push({
        tag: el.tagName.toLowerCase(),
        testid: el.getAttribute("data-testid"),
        aria,
        left: Math.round(r.left),
        right: Math.round(r.right),
        width: Math.round(r.width),
      });
      if (offenders.length >= 12) break;
    }
    return {
      innerWidth: window.innerWidth,
      innerHeight: window.innerHeight,
      scrollWidth: root.scrollWidth,
      scrollHeight: root.scrollHeight,
      selected,
      offenders,
    };
  }, selectors);
  checks.pages[label] = data;
  if (data.scrollWidth > data.innerWidth) {
    checks.pass = false;
    checks.findings.push(`${label}: document scrollWidth ${data.scrollWidth} > viewport ${data.innerWidth}`);
  }
  if (data.offenders.length > 0) {
    checks.pass = false;
    checks.findings.push(`${label}: ${data.offenders.length} element(s) leave the viewport: ${JSON.stringify(data.offenders)}`);
  }
  for (const [selector, value] of Object.entries(data.selected)) {
    if (value && value.right > data.innerWidth + 0.5) {
      checks.pass = false;
      checks.findings.push(`${label}: ${selector} right ${value.right} > viewport ${data.innerWidth}`);
    }
  }
  return data;
};

let shotIndex = 0;
const capture = async (page, name, label, selectors = [], before = null) => {
  if (before) await before();
  await page.waitForTimeout(300);
  await measure(page, label, selectors);
  shotIndex += 1;
  const file = `${String(shotIndex).padStart(2, "0")}-${name}.png`;
  await page.screenshot({ path: path.join(SHOTS, file) });
  checks.shots.push({ number: shotIndex, name, label, file: `screenshots/${file}`, viewport: `${checks.pages[label].innerWidth}x${checks.pages[label].innerHeight}` });
  return file;
};

const wait = (page, selector, timeout = 10000) => page.waitForSelector(selector, { state: "visible", timeout });

const openWorkspace = async (page, url = FIXTURE) => {
  await page.goto(url, { waitUntil: "networkidle" });
  await page.waitForTimeout(800);
  await page.getByText("发布准备周报", { exact: true }).click();
  await wait(page, "[data-testid=composer-input]");
  await page.waitForTimeout(500);
};

const openSettings = async (page, section) => {
  await page.locator("[data-testid=workspace-header-back]").click().catch(() => {});
  await page.waitForTimeout(350);
  await page.locator('[aria-label="打开设置"]').click();
  await wait(page, "[data-testid=settings-page]");
  await page.locator(`[data-testid=settings-section-${section}]`).click();
  await page.waitForTimeout(800);
};

const backToWorkspace = async (page) => {
  await page.locator("[data-testid=settings-back]").click().catch(() => {});
  await page.waitForTimeout(500);
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
  await page.waitForTimeout(600);
};

const fill = async (page, testID, text) => {
  const field = page.locator(`[data-testid=${testID}]`);
  await field.click();
  await page.keyboard.type(text);
};

const startTeam = async (page) => {
  await page.locator("[data-testid=session-model-chip]").click();
  await wait(page, "[data-testid=picker-mode-team]");
  await page.locator("[data-testid=picker-mode-team]").click();
  await wait(page, "[data-testid=picker-team-start-team_demo_brand]");
  await page.waitForTimeout(300);
  await page.locator("[data-testid=picker-team-start-team_demo_brand]").click();
  await wait(page, "[data-testid=workspace-header-team-facts]");
  await page.waitForTimeout(600);
};

const SETTINGS_SECTIONS = [
  "general",
  "appearance",
  "models",
  "agents",
  "teams",
  "capabilities",
  "secrets",
  "plugins",
  "shortcuts",
  "about",
];

const settingsCapture = async (page, viewport, shots) => {
  await openSettings(page, "general");
  for (const section of SETTINGS_SECTIONS) {
    if (section !== "general") {
      await page.locator(`[data-testid=settings-section-${section}]`).click();
      await page.waitForTimeout(800);
    }
    const selectors = [
      "[data-testid=settings-page]",
      "[data-testid=settings-rail]",
      "[data-testid=settings-content-card]",
      "[data-testid=settings-section-body]",
    ];
    if (section === "models") {
      selectors.push("[data-testid=settings-current]", "[data-testid=provider-card-s2api]", "[data-testid=provider-add]");
    }
    if (section === "agents") {
      selectors.push("[data-testid=agent-card-agent_demo_design]", "[data-testid=agent-create]");
    }
    if (section === "teams") {
      selectors.push("[data-testid=team-card-team_demo_brand]", "[data-testid=team-cli-panel]");
    }
    if (section === "capabilities") {
      selectors.push("[data-testid=settings-capabilities]", "[data-testid=capability-row-chat]");
    }
    if (section === "plugins") {
      selectors.push("[data-testid=settings-plugins]", "[data-testid=settings-triggers]");
    }
    await capture(page, `settings-${section}-${viewport}`, `${viewport}·settings ${section}`, selectors);

    if (section === "models") {
      await page.locator("[data-testid=provider-configure-s2api]").click();
      await wait(page, "[data-testid=provider-config-dialog]");
      await page.waitForTimeout(500);
      await capture(page, `provider-config-${viewport}`, `${viewport}·provider config`, [
        "[data-testid=provider-config-dialog]",
        "[data-testid=provider-config-name]",
        "[data-testid=provider-config-endpoint]",
        "[data-testid=provider-config-credential]",
      ]);
      await closeDialog(page, "provider-config-dialog");
    }
    if (section === "agents") {
      await page.locator("[data-testid=agent-create]").click();
      await wait(page, "[data-testid=agent-dialog]");
      await page.waitForTimeout(400);
      await capture(page, `agent-editor-inline-${viewport}`, `${viewport}·agent editor inline`, [
        "[data-testid=agent-dialog]",
        "[data-testid=agent-name]",
        "[data-testid=agent-model]",
        "[data-testid=agent-tools]",
      ]);
      await page.locator("[data-testid=agent-prompt-mode-file]").click();
      await wait(page, "[data-testid=agent-prompt-file]");
      await fill(page, "agent-name", "发布助手");
      await fill(page, "agent-prompt-file", "/etc/passwd");
      await page.locator("[data-testid=agent-save]").click();
      await wait(page, "[data-testid=agent-prompt-error]");
      await page.waitForTimeout(400);
      await capture(page, `agent-editor-validation-${viewport}`, `${viewport}·agent editor file validation`, [
        "[data-testid=agent-dialog]",
        "[data-testid=agent-prompt-file]",
        "[data-testid=agent-prompt-error]",
      ]);
      await closeDialog(page, "agent-dialog");
    }
    if (section === "teams") {
      await page.locator("[data-testid=team-cli-panel]").scrollIntoViewIfNeeded().catch(() => {});
      await page.waitForTimeout(400);
      await capture(page, `team-cli-panel-${viewport}`, `${viewport}·team CLI panel`, [
        "[data-testid=team-cli-panel]",
        "[data-testid=cli-capability-team-list]",
        "[data-testid=cli-capability-team-start]",
      ]);
      await page.getByLabel("编辑团队 品牌发布小组").click();
      await wait(page, "[data-testid=team-dialog]");
      await page.waitForTimeout(600);
      await capture(page, `team-canvas-${viewport}`, `${viewport}·team canvas`, [
        "[data-testid=team-dialog]",
        "[data-testid=canvas-captain-seat]",
        "[data-testid=canvas-members]",
        "[data-testid=canvas-library]",
        "[data-testid=team-save]",
      ]);
      await closeDialog(page, "team-dialog");
    }
  }
};

const pluginsCapture = async (page, viewport) => {
  await openSettings(page, "plugins");
  await page.getByRole("button", { name: "打开插件页" }).first().click();
  await wait(page, "[data-testid=plugins-page]");
  await page.waitForTimeout(900);
  await capture(page, `plugins-page-${viewport}`, `${viewport}·plugins inventory`, [
    "[data-testid=plugins-page]",
    "[data-testid=plugins-inventory]",
    '[data-testid="plugin-keel.team"]',
  ]);
  await page.locator('[data-testid="plugin-detail-demo.workflow"]').click().catch(() => {});
  await page.waitForTimeout(500);
  await page.locator('[data-testid="cli-capability-workflow-plan"]').scrollIntoViewIfNeeded().catch(() => {});
  await page.waitForTimeout(400);
  await capture(page, `plugin-cli-tree-${viewport}`, `${viewport}·plugin CLI tree`, [
    "[data-testid=plugins-page]",
    "[data-testid=cli-capability-workflow-plan]",
    "[data-testid=cli-capability-workflow-status]",
  ]);
  await page.locator('[data-testid="plugin-demo.webhook"]').scrollIntoViewIfNeeded().catch(() => {});
  await page.waitForTimeout(500);
  await capture(page, `plugin-failed-${viewport}`, `${viewport}·plugin failed state`, [
    "[data-testid=plugins-page]",
    '[data-testid="plugin-demo.webhook"]',
  ]);
};

const galleryCapture = async (page, viewport) => {
  await page.route("**/kit?fixture=1", (route) =>
    route.fulfill({ status: 200, contentType: "text/html", body: indexHTML }),
  );
  await page.goto(`${BASE}/kit?fixture=1`, { waitUntil: "networkidle" });
  await page.waitForTimeout(1800);
  const header = await page.evaluate(() => {
    const text = document.body.innerText;
    const match = text.match(/组件清单\s+(\d+)\s+件\s+·\s+已渲染\s+(\d+)\s+·\s+复合对象\s+(\d+)\s+·\s+缺件\s+(\d+)\s+·\s+kit 导出\s+(\d+)\s+件/);
    const missing = text.match(/缺件名单（\d+）：([^\n]+)/);
    return {
      manifest: match ? Number(match[1]) : null,
      rendered: match ? Number(match[2]) : null,
      objects: match ? Number(match[3]) : null,
      missing: match ? Number(match[4]) : null,
      exports: match ? Number(match[5]) : null,
      missingNames: missing ? missing[1].split("、").map((x) => x.trim()) : [],
      title: text.includes("Keel UI Kit Gallery"),
    };
  });
  checks.gallery = { ...header, viewport };
  if (!header.title || header.manifest === null) {
    checks.pass = false;
    checks.findings.push(`${viewport}: the gallery header did not render its manifest counts`);
  }
  if (header.missing > 0) {
    checks.findings.push(`${viewport}: gallery missing components: ${header.missingNames.join("、")}`);
  }
  await capture(page, `kit-gallery-${viewport}`, `${viewport}·kit gallery header`, ["body"]);
  const groups = [
    ["C1 · 基础件", "base"],
    ["C2 · 表单件", "form"],
    ["C3 · 容器件", "container"],
    ["C4 · 浮层与导航", "overlay"],
    ["C5 · 布局原语", "layout"],
    ["L · 布局块（zcode 参数）", "blocks"],
  ];
  for (const [label, slug] of groups) {
    const heading = page.getByText(label, { exact: true }).first();
    if ((await heading.count()) === 0) {
      checks.findings.push(`${viewport}: gallery group heading missing: ${label}`);
      continue;
    }
    await heading.scrollIntoViewIfNeeded();
    await page.waitForTimeout(450);
    await capture(page, `kit-gallery-${slug}-${viewport}`, `${viewport}·kit gallery ${label}`, ["body"]);
  }
};

// ---- 390px ----------------------------------------------------------------
{
  const viewport = "390x844";
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2 });
  const page = await context.newPage();
  await openWorkspace(page);

  await capture(page, "workspace-standard-390", "390x844·workspace standard", [
    "[data-testid=workspace-header]",
    "[data-testid=conversation-column]",
    "[data-testid=composer-dock]",
    "[data-testid=session-model-chip]",
  ]);

  await page.locator("[data-testid=session-model-chip]").click();
  await wait(page, "[data-testid=session-model-picker]");
  await page.waitForTimeout(600);
  await capture(page, "model-picker-standard-390", "390x844·model picker standard", [
    "[data-testid=session-model-picker]",
    "[data-testid=picker-mode-standard]",
    "[data-testid=picker-mode-team]",
  ]);
  await page.locator("[data-testid=picker-mode-team]").click();
  await wait(page, "[data-testid=picker-team-start-team_demo_brand]");
  await page.waitForTimeout(500);
  await capture(page, "model-picker-team-390", "390x844·model picker team", [
    "[data-testid=session-model-picker]",
    "[data-testid=picker-team-team_demo_brand]",
    "[data-testid=picker-team-start-team_demo_brand]",
  ]);
  await page.locator("[data-testid=picker-team-start-team_demo_brand]").click();
  await wait(page, "[data-testid=workspace-header-team-facts]");
  await page.waitForTimeout(700);
  await capture(page, "workspace-team-390", "390x844·workspace team", [
    "[data-testid=workspace-header]",
    "[data-testid=workspace-header-team-facts]",
    "[data-testid=session-team-members]",
  ]);
  await page.locator("[data-testid=session-team-members]").click();
  await wait(page, "[data-testid=team-roster-dialog]");
  await page.waitForTimeout(500);
  await capture(page, "team-roster-390", "390x844·team roster", ["[data-testid=team-roster-dialog]"]);
  await closeDialog(page, "team-roster-dialog");

  await settingsCapture(page, "390", {});
  await backToWorkspace(page);
  await pluginsCapture(page, "390");
  await page.locator("[data-testid=plugins-back]").click().catch(() => {});
  await page.waitForTimeout(400);
  await page.getByRole("button", { name: "返回工作区" }).first().click().catch(() => {});
  await page.waitForTimeout(700);

  // Command palette (Mod+K) over the workspace.
  await page.keyboard.press("Meta+k");
  await wait(page, "[data-testid=command-palette]").catch(() => {});
  await page.waitForTimeout(600);
  if (await page.locator("[data-testid=command-palette]").count()) {
    await capture(page, "command-palette-390", "390x844·command palette", ["[data-testid=command-palette]"]);
    await page.keyboard.press("Escape");
    await page.waitForTimeout(500);
  } else {
    checks.findings.push("390x844: Mod+K did not open the command palette");
  }

  await galleryCapture(page, "390");
  await context.close();
}

// ---- 390px · missing-captain blocker --------------------------------------
{
  const viewport = "390x844·no-captain";
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2 });
  const page = await context.newPage();
  await openWorkspace(page, `${FIXTURE}&teamNoCaptain=1`);
  await openSettings(page, "teams");
  await wait(page, "[data-testid=team-card-team_demo_pending]");
  await page.locator("[data-testid=team-card-team_demo_pending]").scrollIntoViewIfNeeded().catch(() => {});
  await page.waitForTimeout(500);
  await capture(page, "settings-teams-no-captain-390", `${viewport}·settings teams`, [
    "[data-testid=team-card-team_demo_pending]",
    "[data-testid=team-captain-slot-team_demo_pending]",
  ]);
  await page.getByLabel("编辑团队 待定小组").click();
  await wait(page, "[data-testid=team-dialog]");
  await page.waitForTimeout(600);
  await capture(page, "team-canvas-no-captain-390", `${viewport}·team canvas`, [
    "[data-testid=team-dialog]",
    "[data-testid=canvas-captain-blocker]",
    "[data-testid=team-save]",
  ]);
  await context.close();
}

// ---- 1280px ---------------------------------------------------------------
{
  const viewport = "1280x900";
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, deviceScaleFactor: 1 });
  const page = await context.newPage();
  await openWorkspace(page);
  await capture(page, "workspace-standard-1280", "1280x900·workspace standard", [
    "[data-testid=workspace-header]",
    "[data-testid=conversation-column]",
    "[data-testid=composer-dock]",
    "[data-testid=session-model-chip]",
  ]);
  await page.getByLabel("展开右侧状态栏").first().click();
  await wait(page, "[data-testid=session-status-pane]");
  await page.waitForTimeout(900);
  await capture(page, "workspace-status-pane-1280", "1280x900·workspace status pane", [
    "[data-testid=workspace-shell]",
    "[data-testid=session-status-pane]",
    "[data-testid=session-status-state]",
  ]);
  await page.getByLabel("收起右侧状态栏").first().click().catch(() => {});
  await page.waitForTimeout(500);
  await page.keyboard.press("Meta+k");
  await wait(page, "[data-testid=command-palette]").catch(() => {});
  await page.waitForTimeout(500);
  if (await page.locator("[data-testid=command-palette]").count()) {
    await capture(page, "command-palette-1280", "1280x900·command palette", ["[data-testid=command-palette]"]);
    await page.keyboard.press("Escape");
    await page.waitForTimeout(500);
  } else {
    checks.findings.push("1280x900: Mod+K did not open the command palette");
  }
  await startTeam(page);
  await capture(page, "workspace-team-1280", "1280x900·workspace team", [
    "[data-testid=workspace-header]",
    "[data-testid=workspace-header-team-facts]",
    "[data-testid=session-team-members]",
  ]);
  await settingsCapture(page, "1280", {});
  await backToWorkspace(page);
  await pluginsCapture(page, "1280");
  await galleryCapture(page, "1280");
  await context.close();
}

await browser.close();
fs.writeFileSync(path.join(OUT, "checks.json"), JSON.stringify(checks, null, 2) + "\n");
console.log(
  JSON.stringify(
    { pass: checks.pass, findings: checks.findings, gallery: checks.gallery, shots: checks.shots.length },
    null,
    2,
  ),
);
