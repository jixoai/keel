// Screenshot matrix for the keel-board task-face prototype.
//
// Usage:
//   PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs node shoot.mjs
//   node shoot.mjs --only queue-1440-success
//
// Requires a local Chrome (channel "chrome"). The prototype is a static file;
// every shot is a URL, so any Playwright/Chrome install can reproduce it:
//   file:///.../prototype/index.html?chrome=0&scene=queue&state=success
//
// Output: ./shots/<name>.png (do not commit node_modules or browser caches).
import { createHash } from "node:crypto";
import { mkdir, readFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const modulePath = process.env.PLAYWRIGHT_MODULE || "playwright";
const { chromium } = await import(modulePath);
const only = process.argv.includes("--only") ? process.argv[process.argv.indexOf("--only") + 1] : null;
const base = pathToFileURL(join(here, "index.html")).href;

const shots = [
  // 1440 — queue and five states
  ["queue-1440-success", "scene=queue&state=success", 1440, 1000],
  ["queue-1440-loading", "scene=queue&state=loading", 1440, 1000],
  ["queue-1440-empty", "scene=queue&state=empty", 1440, 1000],
  ["queue-1440-filtered-empty", "scene=queue&state=filtered", 1440, 1000],
  ["queue-1440-error", "scene=queue&state=error", 1440, 1000],
  ["queue-1440-permission", "scene=queue&state=permission&me=reader", 1440, 1000],
  ["queue-1440-disabled", "scene=queue&state=disabled", 1440, 1000],
  ["queue-1440-stale", "scene=queue&state=stale", 1440, 1000],
  ["queue-1440-readonly", "scene=queue&state=success&me=reader", 1440, 1000],
  // 1440 — task states
  ["task-1440-claimed", "scene=task&id=812&state=success", 1440, 1000],
  ["task-1440-ready", "scene=task&id=809&state=success", 1440, 1000],
  ["task-1440-waiting", "scene=task&id=830&state=success", 1440, 1000],
  ["task-1440-blocked", "scene=task&id=814&state=success", 1440, 1000],
  ["task-1440-blocked-owner", "scene=task&id=850&state=success", 1440, 1000],
  ["task-1440-completed", "scene=task&id=806&state=success", 1440, 1000],
  ["task-1440-failed", "scene=task&id=848&state=success", 1440, 1000],
  ["task-1440-failed-readonly", "scene=task&id=810&state=success", 1440, 1000],
  ["task-1440-canceled", "scene=task&id=820&state=success", 1440, 1000],
  ["task-1440-claim-lost", "scene=task&id=812&state=success&claim=lost", 1440, 1000],
  ["task-1440-readonly", "scene=task&id=809&state=success&me=reader", 1440, 1000],
  ["task-1440-mirror-readonly", "scene=task&id=814&state=success&bridge=0", 1440, 1000],
  ["task-1440-empty-replies", "scene=task&id=828&state=success", 1440, 1000],
  ["task-1440-composer-error", "scene=task&id=812&state=success&composer=error", 1440, 1000],
  ["task-1440-loading", "scene=task&id=812&state=loading", 1440, 1000],
  ["task-1440-error", "scene=task&id=812&state=error", 1440, 1000],
  ["task-1440-permission", "scene=task&id=812&state=permission&me=reader", 1440, 1000],
  // 1440 — action dialogs
  ["task-1440-dialog-complete", "scene=task&id=812&state=success&dialog=complete", 1440, 1000],
  ["task-1440-dialog-fail", "scene=task&id=812&state=success&dialog=fail", 1440, 1000],
  ["task-1440-dialog-block", "scene=task&id=812&state=success&dialog=block", 1440, 1000],
  ["task-1440-dialog-assign", "scene=task&id=812&state=success&dialog=assign", 1440, 1000],
  ["task-1440-dialog-needs", "scene=task&id=830&state=success&dialog=needs", 1440, 1000],
  ["task-1440-dialog-cancel", "scene=task&id=812&state=success&dialog=cancel", 1440, 1000],
  ["task-1440-dialog-release", "scene=task&id=812&state=success&dialog=release", 1440, 1000],
  ["task-1440-dialog-reopen", "scene=task&id=848&state=success&dialog=reopen", 1440, 1000],
  ["task-1440-dialog-answer", "scene=task&id=814&state=success&dialog=answer", 1440, 1000],
  ["task-1440-dialog-unblock", "scene=task&id=850&state=success&dialog=unblock", 1440, 1000],
  ["task-1440-dialog-access", "scene=task&id=812&state=success&dialog=access", 1440, 1000],
  ["task-1440-dialog-shortcuts", "scene=task&id=812&state=success&dialog=shortcuts", 1440, 1000],
  ["task-1440-dialog-new", "scene=queue&state=success&dialog=new", 1440, 1000],
  // 1440 — sync / automation / retired route
  ["sync-1440-success", "scene=sync&state=success", 1440, 1000],
  ["sync-1440-loading", "scene=sync&state=loading", 1440, 1000],
  ["sync-1440-empty", "scene=sync&state=empty", 1440, 1000],
  ["sync-1440-error", "scene=sync&state=error", 1440, 1000],
  ["sync-1440-permission", "scene=sync&state=permission&me=reader", 1440, 1000],
  ["sync-1440-deadletters", "scene=sync&state=success&scroll=dead", 1440, 1000],
  ["sync-1440-credential-invalid", "scene=sync&state=success&scroll=linear", 1440, 1000],
  ["automation-1440-success", "scene=automation&state=success", 1440, 1000],
  ["automation-1440-loading", "scene=automation&state=loading", 1440, 1000],
  ["automation-1440-empty", "scene=automation&state=empty", 1440, 1000],
  ["automation-1440-error", "scene=automation&state=error", 1440, 1000],
  ["automation-1440-permission", "scene=automation&state=permission&me=reader", 1440, 1000],
  ["automation-1440-failure", "scene=automation&state=success&scroll=schedules", 1440, 1000],
  ["gone-1440", "scene=gone", 1440, 1000],
  // 768
  ["queue-768", "scene=queue&state=success", 768, 1000],
  ["queue-768-loading", "scene=queue&state=loading", 768, 1000],
  ["task-768-claimed", "scene=task&id=812&state=success", 768, 1000],
  ["task-768-dialog-block", "scene=task&id=812&state=success&dialog=block", 768, 1000],
  ["sync-768", "scene=sync&state=success", 768, 1000],
  ["automation-768", "scene=automation&state=success", 768, 1000],
  // 390
  ["queue-390-success", "scene=queue&state=success", 390, 844],
  ["queue-390-loading", "scene=queue&state=loading", 390, 844],
  ["queue-390-empty", "scene=queue&state=empty", 390, 844],
  ["queue-390-error", "scene=queue&state=error", 390, 844],
  ["queue-390-permission", "scene=queue&state=permission&me=reader", 390, 844],
  ["queue-390-filters", "scene=queue&state=success&dialog=filters", 390, 844],
  ["task-390-claimed", "scene=task&id=812&state=success", 390, 844],
  ["task-390-facts", "scene=task&id=812&state=success&facts=1", 390, 844],
  ["task-390-thread", "scene=task&id=812&state=success&scroll=thread", 390, 844],
  ["task-390-composer", "scene=task&id=812&state=success&scroll=composer", 390, 844],
  ["task-390-dialog-complete", "scene=task&id=812&state=success&dialog=complete", 390, 844],
  ["task-390-dialog-answer", "scene=task&id=814&state=success&dialog=answer", 390, 844],
  ["sync-390", "scene=sync&state=success", 390, 844],
  ["automation-390", "scene=automation&state=success&scroll=schedules", 390, 844],
  ["gone-390", "scene=gone", 390, 844]
];

await mkdir(join(here, "shots"), { recursive: true });
const browser = await chromium.launch({ headless: true, channel: "chrome" });
const results = [];
let failed = 0;
for (const [name, query, width, height] of shots) {
  if (only && name !== only) continue;
  const page = await browser.newPage({ viewport: { width, height }, deviceScaleFactor: 1 });
  const errors = [];
  page.on("pageerror", (error) => errors.push(`pageerror: ${error.message}`));
  page.on("console", (message) => { if (message.type() === "error") errors.push(`console: ${message.text()}`); });
  await page.goto(`${base}?chrome=0&${query}`, { waitUntil: "load" });
  await page.waitForTimeout(350);
  const metrics = await page.evaluate(() => ({
    scrollWidth: document.documentElement.scrollWidth,
    clientWidth: document.documentElement.clientWidth,
    scrollHeight: document.documentElement.scrollHeight
  }));
  const path = join(here, "shots", `${name}.png`);
  await page.screenshot({ path, fullPage: false });
  const bytes = await readFile(path);
  const hash = createHash("sha1").update(bytes).digest("hex").slice(0, 12);
  results.push({ name, width, height, bytes: bytes.length, hash, overflow: metrics.scrollWidth > metrics.clientWidth, errors });
  if (metrics.scrollWidth > metrics.clientWidth) { console.error(`OVERFLOW ${name}: ${metrics.scrollWidth} > ${metrics.clientWidth}`); failed += 1; }
  if (errors.length) { console.error(`CONSOLE ${name}: ${errors.join(" | ")}`); failed += 1; }
  await page.close();
}
await browser.close();
for (const row of results) console.log(JSON.stringify(row));
console.log(`\n${results.length} shots; failures: ${failed}`);
process.exitCode = failed ? 1 : 0;
