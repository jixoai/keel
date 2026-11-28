#!/usr/bin/env node
// Board Phase 2 gate evidence — script-generated, never hand-written.
//
// Runs, from the repo root:
//   1. nsh check (web)
//   2. nsh test src/data/board.test.ts (headless data layer)
//   3. nsh test --target tui src/screens/board/board.tui.test.tsx (JUnit)
//   4. nsh test --target web board-phase2-capture.test.tsx (real screenshots)
//
// Writes <evidence>/index.html with every screenshot, the command outputs and
// the JUnit summary; the screenshot test is copied from
// `web/evidence/board-capture.template.tsx` and removed afterwards.
import { execFileSync, spawn } from "node:child_process";
import { cpSync, existsSync, mkdirSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const stamp = new Date().toISOString().replace(/[-:]/g, "").replace(/\.\d+Z$/, "Z");
const evidence = join(root, "openspec/changes/keel-board-sync-v2/evidence", `ui-gates-${stamp}`);
const web = join(root, "web");
const shots = join(evidence, "shots/web");
mkdirSync(shots, { recursive: true });

const results = [];
function run(name, command, args, cwd, options = {}) {
  const started = Date.now();
  let status = 0;
  let output = "";
  try {
    output = execFileSync(command, args, {
      cwd,
      encoding: "utf8",
      stdio: ["ignore", "pipe", "pipe"],
      maxBuffer: 64 * 1024 * 1024,
      ...options,
    });
  } catch (error) {
    status = typeof error.status === "number" ? error.status : 1;
    output = `${error.stdout ?? ""}\n${error.stderr ?? ""}`;
  }
  const duration = ((Date.now() - started) / 1000).toFixed(1);
  writeFileSync(join(evidence, `${name}.log`), output);
  results.push({ name, command: [command, ...args].join(" "), status, duration, output });
  console.log(`[${status === 0 ? "ok" : "FAIL"}] ${name} (${duration}s)`);
  return status;
}

// 1. Type check.
run("nsh-check", "nsh", ["check"], web);
// 2. Data layer.
run("data-tests", "nsh", ["test", "src/data/board.test.ts"], web);
run("sse-unit-tests", "nsh", ["test", "src/data/board-sse.test.ts"], web);

// 3. TUI behavior (JUnit for a machine-readable summary).
const junit = join(evidence, "tui-junit.xml");
run("tui-tests", "nsh", ["test", "--target", "tui", "--junit", junit, "src/screens/board/board.tui.test.tsx"], web);

// 4. Real web screenshots. The template is not a discovered test.
const template = join(web, "evidence/board-capture.template.tsx");
const discovered = join(web, "board-phase2-capture.test.tsx");
const source = readFileSync(template, "utf8").replaceAll("__REPORT_DIR__", evidence.replaceAll("\\", "/"));
writeFileSync(discovered, source);
try {
  run("web-screenshot-tests", "nsh", ["test", "--target", "web", "board-phase2-capture.test.tsx"], web);
} finally {
  rmSync(discovered, { force: true });
}

// 4b. Real SSE against a fake HTTP server (a separate template because it
// needs the server's base URL at compile time).
const sseTemplate = join(web, "evidence/board-sse-live.template.tsx");
const sseDiscovered = join(web, "board-sse-live.test.tsx");
const sseServer = spawn(process.execPath, [join(root, "scripts/board-fake-sse-server.mjs"), "--port=0"], {
  cwd: root,
  stdio: ["ignore", "pipe", "pipe"],
});
try {
  const base = await new Promise((resolve, reject) => {
    let buffer = "";
    const timer = setTimeout(() => reject(new Error("fake SSE server did not start")), 10_000);
    sseServer.stdout.on("data", (chunk) => {
      buffer += String(chunk);
      const line = buffer.split("\n").find((entry) => entry.trim().startsWith("{"));
      if (!line) return;
      clearTimeout(timer);
      try {
        resolve(JSON.parse(line).base);
      } catch (error) {
        reject(error);
      }
    });
    sseServer.on("exit", (code) => reject(new Error(`fake SSE server exited ${code}`)));
  });
  const source = readFileSync(sseTemplate, "utf8")
    .replaceAll("__SSE_BASE__", base)
    .replaceAll("__REPORT_DIR__", evidence.replaceAll("\\", "/"));
  writeFileSync(sseDiscovered, source);
  run("sse-live-test", "nsh", ["test", "--target", "web", "board-sse-live.test.tsx"], web);
} catch (error) {
  results.push({
    name: "sse-live-test",
    command: "nsh test --target web board-sse-live.test.tsx",
    status: 1,
    duration: "0.0",
    output: String(error?.message ?? error),
  });
  console.error(`[FAIL] sse-live-test: ${error?.message ?? error}`);
} finally {
  sseServer.kill("SIGTERM");
  rmSync(sseDiscovered, { force: true });
}

// 5. The report.
function pngSize(path) {
  const buffer = readFileSync(path);
  if (buffer.length < 24 || buffer.toString("ascii", 1, 4) !== "PNG") return { width: 0, height: 0 };
  return { width: buffer.readUInt32BE(16), height: buffer.readUInt32BE(20), bytes: statSync(path).size };
}

const images = existsSync(shots)
  ? readdirSync(shots)
      .filter((name) => name.endsWith(".png"))
      .sort()
      .map((file) => ({ name: file.replace(/\.png$/, ""), ...pngSize(join(shots, file)) }))
  : [];

let junitSummary = { tests: 0, failures: 0, errors: 0, skipped: 0 };
let junitCases = [];
if (existsSync(junit)) {
  const xml = readFileSync(junit, "utf8");
  const suite = xml.match(/<testsuite\b[^>]*>/)?.[0] ?? "";
  const number = (key) => Number(suite.match(new RegExp(`${key}="(\\d+)"`))?.[1] ?? 0);
  junitSummary = {
    tests: number("tests"),
    failures: number("failures"),
    errors: number("errors"),
    skipped: number("skipped"),
  };
  junitCases = [...xml.matchAll(/<testcase\b[^>]*name="([^"]*)"[^>]*time="([^"]*)"[^>]*>/g)].map((match) => ({
    name: match[1],
    time: match[2],
  }));
}

const esc = (value) =>
  String(value).replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;").replaceAll('"', "&quot;");
const statusText = (status) => (status === 0 ? "PASS" : "FAIL");
const summaryRows = results
  .map(
    (result) =>
      `<tr class="${result.status === 0 ? "pass" : "fail"}"><td>${esc(result.name)}</td><td><code>${esc(result.command)}</code></td><td>${statusText(result.status)}</td><td>${result.duration}s</td></tr>`,
  )
  .join("\n");
const testRows = junitCases
  .map((test) => `<tr><td>${esc(test.name)}</td><td>${test.time}s</td></tr>`)
  .join("\n");
const gallery = images
  .map(
    (image) => `<figure id="${esc(image.name)}">
  <img src="shots/web/${esc(image.name)}.png" alt="${esc(image.name)}" loading="lazy" />
  <figcaption><code>${esc(image.name)}</code> · ${image.width}×${image.height} · ${(image.bytes / 1024).toFixed(1)} KB</figcaption>
</figure>`,
  )
  .join("\n");
// The acceptance matrix: every cell names one or more screenshots (or the
// SSE test) that prove it. A missing name is UNCOVERED and fails the run.
const MATRIX = [
  { screen: "queue", state: "success", viewports: [390, 768, 1024, 1440], files: ["04-queue-success-390", "03-queue-success-768", "02-queue-success-1024", "01-queue-success-1440"] },
  { screen: "queue", state: "loading", viewports: [390, 1440], files: ["05b-queue-loading-390", "05-queue-loading-1440"] },
  { screen: "queue", state: "empty", viewports: [390, 1440], files: ["14-queue-empty-390", "06-queue-empty-1440"] },
  { screen: "queue", state: "error", viewports: [1440], files: ["08-queue-error-1440"] },
  { screen: "queue", state: "permission", viewports: [1440], files: ["10-queue-not-found-1440", "11-queue-migrate-1440", "12-queue-reader-1440"] },
  { screen: "task", state: "success", viewports: [390, 1440], files: ["25-task-ready-390", "21-task-ready-1440", "22-task-blocked-mirror-1440", "23-task-completed-1440", "24-task-archived-1440"] },
  { screen: "task", state: "empty", viewports: [1440], files: ["23b-task-empty-thread-1440"] },
  { screen: "task", state: "loading", viewports: [1440], files: ["25c-task-loading-1440"] },
  { screen: "task", state: "error", viewports: [1440], files: ["25d-task-error-1440"] },
  { screen: "task", state: "permission", viewports: [1440], files: ["25b-task-permission-1440"] },
  { screen: "sync", state: "success", viewports: [390, 1440], files: ["30b-sync-success-390", "30-sync-success-1440"] },
  { screen: "sync", state: "loading", viewports: [1440], files: ["33b-sync-loading-1440"] },
  { screen: "sync", state: "empty", viewports: [1440], files: ["33c-sync-empty-1440"] },
  { screen: "sync", state: "error", viewports: [1440], files: ["32-sync-error-1440"] },
  { screen: "sync", state: "permission", viewports: [1440], files: ["33-sync-permission-1440"] },
  { screen: "automation", state: "success", viewports: [390, 1440], files: ["37-automation-390", "34-automation-success-1440"] },
  { screen: "automation", state: "loading", viewports: [1440], files: ["35b-automation-loading-1440"] },
  { screen: "automation", state: "empty", viewports: [1440], files: ["35c-automation-empty-1440"] },
  { screen: "automation", state: "error", viewports: [1440], files: ["35-automation-error-1440"] },
  { screen: "automation", state: "permission", viewports: [1440], files: ["34b-automation-permission-1440"] },
  { screen: "claim", state: "before/after", viewports: [1440], files: ["40-before-claim-1440", "41-after-claim-1440"] },
  { screen: "complete", state: "before/after", viewports: [1440], files: ["42-before-complete-1440", "43-after-complete-1440"] },
  { screen: "sync-retry", state: "before/after", viewports: [1440], files: ["44-before-retry-1440", "45-after-retry-1440"] },
  { screen: "watcher-toggle", state: "before/after", viewports: [1440], files: ["46-before-watcher-toggle-1440", "47-after-watcher-toggle-1440"] },
  { screen: "mobile-filter", state: "before/after", viewports: [390], files: ["48-before-filter-390", "49-filter-sheet-390"] },
  { screen: "sse", state: "success", viewports: [1440], files: ["sse-live-test"] },
];
const imageNames = new Set(images.map((image) => image.name));
const matrixRows = MATRIX.map((cell) => {
  const missing = cell.files.filter((file) => !imageNames.has(file) && file !== "sse-live-test");
  const sse = cell.files.includes("sse-live-test") ? results.find((result) => result.name === "sse-live-test") : undefined;
  const covered = missing.length === 0 && (sse === undefined || sse.status === 0);
  return { ...cell, missing, covered };
});
const uncovered = matrixRows.filter((row) => !row.covered);
const matrixHtml = matrixRows
  .map(
    (row) =>
      `<tr class="${row.covered ? "pass" : "fail"}"><td>${esc(row.screen)}</td><td>${esc(row.state)}</td><td>${row.viewports.join(", ")}</td><td>${row.covered ? "COVERED" : `UNCOVERED: ${esc(row.missing.join(", ") || "sse-live-test")}`}</td></tr>`,
  )
  .join("\n");
const gatesPass = results.every((result) => result.status === 0) && uncovered.length === 0;
const html = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>keel-board-sync-v2 · UI gates</title>
<style>
:root { color-scheme: dark; --bg:#0d0d10; --panel:#17171c; --ink:#f4f4f5; --muted:#a1a1aa; --line:#2a2a32; --ok:#4ade80; --bad:#f87171; --brand:#93c5fd; }
* { box-sizing: border-box; }
body { margin:0; background:var(--bg); color:var(--ink); font:14px/1.55 system-ui,-apple-system,"PingFang SC",sans-serif; }
main { max-width: 1320px; margin: 0 auto; padding: 32px 24px 64px; }
h1 { font-size: 28px; margin: 0 0 6px; letter-spacing: -0.4px; }
h2 { font-size: 18px; margin: 32px 0 12px; }
p { color: var(--muted); margin: 4px 0; }
code { font-family: ui-monospace,SFMono-Regular,Menlo,monospace; font-size: 12px; }
.badge { display:inline-block; padding:2px 10px; border-radius:999px; border:1px solid var(--line); color:var(--muted); font-size:12px; }
.banner { margin:16px 0 24px; padding:14px 16px; border-radius:12px; border:1px solid ${gatesPass ? "#14532d" : "#7f1d1d"}; background:${gatesPass ? "#052e16" : "#450a0a"}; }
table { width:100%; border-collapse: collapse; background:var(--panel); border:1px solid var(--line); border-radius:12px; overflow:hidden; }
th,td { text-align:left; padding:10px 12px; border-bottom:1px solid var(--line); vertical-align:top; }
tr:last-child td { border-bottom:none; }
th { color:var(--muted); font-weight:600; font-size:12px; text-transform:uppercase; letter-spacing:.4px; }
td.pass { color: var(--ok); } tr.fail td { color: var(--bad); }
.grid { display:grid; grid-template-columns: repeat(auto-fill,minmax(360px,1fr)); gap:18px; }
figure { margin:0; background:var(--panel); border:1px solid var(--line); border-radius:14px; overflow:hidden; }
figure img { width:100%; display:block; background:#000; }
figcaption { padding:10px 12px; color:var(--muted); font-size:12px; }
details { margin-top:12px; background:var(--panel); border:1px solid var(--line); border-radius:12px; padding:10px 14px; }
summary { cursor:pointer; color:var(--brand); }
pre { white-space:pre-wrap; word-break:break-word; color:var(--muted); font-size:12px; max-height:420px; overflow:auto; }
.small { font-size:12px; color:var(--muted); }
</style>
</head>
<body>
<main>
  <h1>keel-board-v1 · Phase 2 UI gates</h1>
  <p>Generated by <code>scripts/board-phase2-ui-evidence.mjs</code> at ${esc(new Date().toISOString())}.</p>
  <p><span class="badge">nsh check</span> <span class="badge">data tests</span> <span class="badge">TUI tests</span> <span class="badge">real web screenshots</span></p>
  <div class="banner"><strong>${gatesPass ? "All gates exit 0." : "A gate failed; see the table and the logs."}</strong>
  <p>Evidence directory: <code>${esc(evidence)}</code></p></div>

  <h2>Gates</h2>
  <table><thead><tr><th>Gate</th><th>Command</th><th>Result</th><th>Time</th></tr></thead><tbody>
  ${summaryRows}
  </tbody></table>

  <h2>TUI tests (${junitSummary.tests} tests, ${junitSummary.failures} failures, ${junitSummary.errors} errors)</h2>
  <table><thead><tr><th>Test</th><th>Time</th></tr></thead><tbody>
  ${testRows}
  </tbody></table>

  <h2>Screenshots (${images.length})</h2>
  <p class="small">Real web builds of the same TSX, captured with <code>nsh test --target web</code>; each frame follows a real state transition on the harness transport.</p>
  <div class="grid">
  ${gallery}
  </div>

  <h2>Coverage matrix (${matrixRows.length} cells, ${uncovered.length} uncovered)</h2>
  <table><thead><tr><th>Screen</th><th>State</th><th>Viewports</th><th>Result</th></tr></thead><tbody>
  ${matrixHtml}
  </tbody></table>

  <h2>Command output</h2>
  ${results
    .map(
      (result) =>
        `<details><summary>${esc(result.name)} · exit ${result.status} · ${result.duration}s</summary><pre>${esc(result.output)}</pre></details>`,
    )
    .join("\n")}
</main>
</body>
</html>
`;
writeFileSync(join(evidence, "index.html"), html);
writeFileSync(
  join(evidence, "README.txt"),
  [
    `keel-board-sync-v2 UI evidence — ${stamp}`,
    "",
    ...results.map((result) => `${result.name}: exit ${result.status} (${result.duration}s) — ${result.command}`),
    "",
    `Screenshots: ${images.length} (${images.map((image) => `${image.name} ${image.width}x${image.height}`).join(", ")})`,
    `Coverage matrix: ${matrixRows.length - uncovered.length}/${matrixRows.length} covered; uncovered: ${uncovered.map((row) => `${row.screen}/${row.state}`).join(", ") || "none"}`,
    `Report: index.html`,
    "",
    "Generated by scripts/board-phase2-ui-evidence.mjs; do not edit by hand.",
  ].join("\n"),
);

console.log(`evidence: ${evidence}`);
console.log(`screenshots: ${images.length}`);
console.log(`gates: ${gatesPass ? "PASS" : "FAIL"}`);
process.exit(gatesPass ? 0 : 1);
