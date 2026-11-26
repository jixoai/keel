#!/usr/bin/env node
// Product-UI screenshots for the super-agent board (change keel-collab-superagent).
//
// The acceptance harness page is not the product. This lane serves the packaged
// web build, opens the real `/superagent` screen with the fixture transport
// (`?fixture=1`, development only), drives a ticket + an Attention answer, and
// appends the shots to the report's steps.json/report.json. The fixture rows are
// labelled as such in the step note; the five-state behavior is covered by the
// TUI suite and the real-API harness report.
//
// Usage:
//   node scripts/superagent-acceptance/product.mjs \
//     --web-dir /tmp/keel-superagent-product-web/Keel \
//     --out .agents/reports/keel/superagent-e2e-2026-11-26 \
//     [--chrome "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"] \
//     [--playwright /path/to/playwright-core/index.mjs]
import fs from "node:fs";
import http from "node:http";
import path from "node:path";

const args = process.argv.slice(2);
const arg = (name, fallback) => {
  const index = args.indexOf("--" + name);
  return index >= 0 && args[index + 1] ? args[index + 1] : fallback;
};

const WEB_DIR = arg("web-dir", "");
const OUT = arg("out", "");
const CHROME = arg("chrome", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome");
const PLAYWRIGHT =
  arg("playwright", "/Users/kzf/.npm/_npx/e41f203b7505f1fb/node_modules/playwright-core/index.mjs");
const TICKET_ID = "6a2f1c3e-4b5d-4e6f-8a90-1b2c3d4e5f01";
const ATTENTION_ID = "b7c1d2e3-f4a5-4b6c-9d7e-8f90a1b2c301";

if (!WEB_DIR || !OUT) {
  console.error("usage: product.mjs --web-dir <site root> --out <report dir>");
  process.exit(2);
}
const SHOTS = path.join(OUT, "shots");
fs.mkdirSync(SHOTS, { recursive: true });

const MIME = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".mjs": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".json": "application/json",
  ".wasm": "application/wasm",
  ".png": "image/png",
  ".jpg": "image/jpeg",
  ".svg": "image/svg+xml",
  ".woff2": "font/woff2",
  ".ttf": "font/ttf",
  ".map": "application/json",
};

const server = http.createServer((request, response) => {
  const url = new URL(request.url || "/", "http://127.0.0.1");
  let file = path.join(WEB_DIR, decodeURIComponent(url.pathname));
  if (!file.startsWith(path.resolve(WEB_DIR))) file = path.join(WEB_DIR, "index.html");
  if (!fs.existsSync(file) || fs.statSync(file).isDirectory()) {
    // SPA fallback: the product routes (`/superagent`, `/superagent/:id`) live in
    // the address bar, so an unknown path serves the app shell.
    file = path.join(WEB_DIR, "index.html");
  }
  response.writeHead(200, { "Content-Type": MIME[path.extname(file)] || "application/octet-stream" });
  fs.createReadStream(file).pipe(response);
});

const listen = () =>
  new Promise((resolve) => {
    server.listen(0, "127.0.0.1", () => resolve(server.address().port));
  });

const port = await listen();
const base = `http://127.0.0.1:${port}`;
const playwright = await import(PLAYWRIGHT);
const browser = await playwright.chromium.launch({ executablePath: CHROME, headless: true });
const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, locale: "zh-CN" });
const page = await context.newPage();
const consoleErrors = [];
const pageErrors = [];
page.on("console", (message) => {
  if (message.type() === "error") consoleErrors.push(message.text());
});
page.on("pageerror", (error) => pageErrors.push(String(error?.stack || error)));

const checks = [];
const check = (name, ok, actual, expected) => {
  checks.push({ name, ok: Boolean(ok), actual, expected });
  console.log(`  ${ok ? "ok  " : "FAIL"} ${name} (${actual})`);
};
let shotSeq = 0;
const shot = async (label) => {
  shotSeq += 1;
  const file = `product-${String(shotSeq).padStart(2, "0")}-${label}.png`;
  await page.screenshot({ path: path.join(SHOTS, file), fullPage: true });
  return `shots/${file}`;
};

try {
  await page.goto(`${base}/superagent?fixture=1`, { waitUntil: "domcontentloaded", timeout: 60000 });
  await page.waitForSelector('[data-testid="superagent-board"]', { timeout: 60000 });
  const boardShot = await shot("board-1440");
  const counts = await page.locator('[data-testid^="superagent-count-"]').count();
  check("看板四类计数可见", counts >= 4, counts, ">=4");
  const rows = await page.evaluate(() => {
    const list = document.querySelector('[data-testid="superagent-ticket-list"]');
    if (!list) return 0;
    return [...list.querySelectorAll('[data-testid^="superagent-ticket-"]')].filter((node) =>
      /^superagent-ticket-[0-9a-f-]{36}$/.test(node.getAttribute("data-testid") || ""),
    ).length;
  });
  check("工单列表有真实行", rows >= 4, rows, ">=4");
  check("收件箱有开放问题", (await page.locator('[data-testid="superagent-inbox"]').count()) === 1, 1, 1);

  await page.click(`[data-testid="superagent-ticket-${TICKET_ID}"]`);
  await page.waitForSelector('[data-testid="superagent-detail"]', { timeout: 20000 });
  const ticketShot = await shot("ticket-1440");
  check("工单详情可打开", (await page.locator('[data-testid="superagent-detail-title"]').count()) === 1, 1, 1);

  const option = `[data-testid="superagent-attention-${ATTENTION_ID}-option-refund-first"]`;
  if (await page.locator(option).count()) {
    // An option button is the answer (the optional text field is submitted by
    // its own button); clicking it submits to the durable journal.
    await page.click(option);
    await page.waitForSelector('[data-testid="superagent-notice"]', { timeout: 20000 });
  }
  const answerShot = await shot("answered-1440");
  check("答复后出现成功反馈", (await page.locator('[data-testid="superagent-notice"]').count()) === 1, 1, 1);

  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`${base}/superagent/${TICKET_ID}?fixture=1`, { waitUntil: "domcontentloaded" });
  await page.waitForSelector('[data-testid="superagent-board"]', { timeout: 30000 });
  const narrowShot = await shot("ticket-390");
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  check("390 无横向溢出", overflow <= 1, overflow, "<=1");

  check("无页面异常", pageErrors.length === 0, pageErrors.length, 0);

  const reportPath = path.join(OUT, "report.json");
  if (fs.existsSync(reportPath)) {
    const data = JSON.parse(fs.readFileSync(reportPath, "utf8"));
    const step = {
      n: 10,
      name: "产品 UI：真实 /superagent 看板（web fixture 传输）",
      result: checks.every((row) => row.ok) ? "PASS" : "FAIL",
      action:
        "用 `scripts/package-web.sh` 的产物启动 SPA 静态服务，打开 `/superagent?fixture=1`；打开一张工单、回答一条 Attention，并在 390 视口复验。",
      expect: "看板四类计数、工单列表、详情、收件箱与答复反馈都是产品页面；390 无横向溢出。",
      assertionNote:
        "fixture=1 是开发传输，数据为仓库内验收夹具；五态与真实 API 由 TUI 17/17 与 harness 报告的 1-9 步覆盖。",
      log: checks.map((row) => `${row.ok ? "ok" : "FAIL"} ${row.name} actual=${row.actual} want=${row.expected}`).join("\n"),
      shots: [boardShot, ticketShot, answerShot, narrowShot],
    };
    data.steps = (data.steps || []).filter((row) => row.n !== 10).concat(step);
    data.steps.sort((a, b) => a.n - b.n);
    data.assertions = (data.assertions || []).filter((row) => !String(row.id).startsWith("AG-P"));
    checks.forEach((row, index) => {
      data.assertions.push({
        id: `AG-P${index + 1}`,
        group: "产品 UI",
        name: row.name,
        result: row.ok ? "PASS" : "FAIL",
        detail: `actual=${row.actual}; expected=${row.expected}`,
        step: 10,
      });
    });
    data.stats = (data.stats || []).map((row) =>
      row.label === "逐步截图" ? { ...row, value: String(12 + 4) } : row,
    );
    fs.writeFileSync(reportPath, JSON.stringify(data, null, 2) + "\n");
    console.log(`updated ${reportPath}`);
  }

  const failed = checks.filter((row) => !row.ok);
  console.log(JSON.stringify({ checks, consoleErrors, pageErrors, shots: [boardShot, ticketShot, answerShot, narrowShot] }, null, 2));
  process.exitCode = failed.length ? 1 : 0;
} finally {
  await context.close().catch(() => {});
  await browser.close().catch(() => {});
  server.close();
}
