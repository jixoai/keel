#!/usr/bin/env node
// kit-imports.mjs — the consumer gate for the Keel app (change keel-ui-kit,
// task G3): after the kit lands, **web/src/screens and the app root web/App.tsx**
// may only be built from the kit and must not re-invent the shell. Four rules,
// one violation each:
//
//   1. `from "../ui/primitives"` — the pre-kit atoms; the rewritten screens use
//      web/src/ui/kit only.
//   2. `ResizablePanel` / `Sidebar` / `SidePane` used in a screen — the layout
//      primitives are kit internals: a screen goes through the layout blocks
//      (WorkspaceShell / SettingsShell / ConversationColumn / ComposerDock).
//   3. Shell widths as width/height values in a screen (264 / 320 / 480 / 360 /
//      45% / 65%) — the shell's numbers live in the kit's blocks (zcode's
//      parameters), never in a page.
//   4. Raw colours (hex, rgb()/rgba(), hsl(), oklch()) — colours come from the
//      kit's tokens (`web/src/ui/kit/tokens.ts`, i.e. web/theme.json). Pass
//      `--no-colors` to skip this rule; `kit-imports: allow-color` on the line
//      excuses one occurrence (an icon's own bitmap, a test fixture).
//
// Usage (cwd-independent; the automation calls this shape):
//   node <repo>/scripts/kit-imports.mjs --web <repo>/web
//   node <repo>/scripts/kit-imports.mjs --self-test
//   node <repo>/scripts/kit-imports.mjs --self-test --web <repo>/web
//
// Exit codes: 0 clean, 1 violations (or a failed --self-test), 2 usage/setup
// error. Violations are `path:line reason` on stdout, one per line; the
// automation only reads the exit code.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const DEFAULT_WEB = path.resolve(HERE, "../web");

const MAGIC_WIDTHS = ["264", "320", "480", "360"];
const MAGIC_PERCENTS = ["45", "65"];

const RULES = [
  {
    id: "legacy-primitives",
    // the pre-kit atoms: `../ui/primitives` from a screen, with or without an
    // extension. The specifier is a string literal, so this reads `text`.
    match: (code, text) => /\bfrom\s*["']\.\.\/ui\/primitives(\.tsx?|\.jsx?)?["']/.test(text),
    reason: 'imports "../ui/primitives" — the pre-kit atoms; screens import web/src/ui/kit only',
  },
  {
    id: "layout-primitive",
    match: (code) =>
      /\b(?:ResizablePanel|Sidebar|SidePane)\b/.test(code) &&
      (/\b(?:import|export)\b[^\n]*\b(?:ResizablePanel|Sidebar|SidePane)\b/.test(code) ||
        /<\/?(?:ResizablePanel|Sidebar|SidePane)\b/.test(code) ||
        /\b(?:ResizablePanel|Sidebar|SidePane)\.\w/.test(code) ||
        /new\s+(?:ResizablePanel|Sidebar|SidePane)\b/.test(code)),
    reason: "uses a layout primitive directly — go through the kit's layout blocks (WorkspaceShell / SettingsShell / ConversationColumn / ComposerDock)",
  },
  {
    id: "shell-magic-width",
    match: (code, text) =>
      new RegExp(`\\b(?:width|minWidth|maxWidth|height|minHeight|maxHeight|flexBasis|basis)\\s*:\\s*(?:${MAGIC_WIDTHS.join("|")})\\b`).test(code) ||
      new RegExp(`\\b(?:width|minWidth|maxWidth|height|minHeight|maxHeight|flexBasis|basis)\\s*:\\s*["'](?:${MAGIC_PERCENTS.join("|")})%["']`).test(text) ||
      new RegExp(`["'](?:${MAGIC_WIDTHS.join("|")})px["']`).test(text),
    reason: "writes a shell width (264/320/480/360/45%/65%) — the shell's numbers belong to the kit's layout blocks",
  },
  {
    id: "raw-color",
    needed: (options) => options.colors,
    match: (code, text) =>
      // #rgb / #rgba / #rrggbb / #rrggbbaa, or a colour function
      /(?:^|[^\w])#[0-9a-fA-F]{3,8}\b/.test(text) || /\b(?:rgb|rgba|hsl|hsla|oklch|oklab)\(/.test(text),
    reason: "writes a raw colour — colours come from web/theme.json through the kit tokens",
    excuse: /kit-imports:\s*allow-color/,
  },
];

const SKIP_DIRS = new Set([".nsh", "node_modules", "dist", ".git"]);

function walk(dir, files = []) {
  let entries;
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true });
  } catch {
    return files;
  }
  for (const entry of entries) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      if (!SKIP_DIRS.has(entry.name) && !entry.name.startsWith(".")) walk(full, files);
    } else if (/\.(ts|tsx)$/.test(entry.name) && !/\.(test|spec)\.tsx?$/.test(entry.name)) {
      files.push(full);
    }
  }
  return files;
}

/** `line` without a trailing `//` comment (quotes respected), literals intact. */
function stripComment(line) {
  let quote = null;
  for (let i = 0; i < line.length; i++) {
    const ch = line[i];
    if (quote) {
      if (ch === "\\") i++;
      else if (ch === quote) quote = null;
      continue;
    }
    if (ch === '"' || ch === "'" || ch === "`") {
      quote = ch;
      continue;
    }
    if (ch === "/" && line[i + 1] === "/") return line.slice(0, i);
  }
  return line;
}

/** The code of `line`: no comment, no string-literal contents — so an
 * identifier rule never fires on prose. */
function codeOnly(line) {
  const stripped = stripComment(line);
  let out = "";
  let quote = null;
  for (let i = 0; i < stripped.length; i++) {
    const ch = stripped[i];
    if (quote) {
      if (ch === "\\") i++;
      else if (ch === quote) quote = null;
      continue;
    }
    if (ch === '"' || ch === "'" || ch === "`") {
      quote = ch;
      continue;
    }
    out += ch;
  }
  return out;
}

/** The violations of one source file. `path` is what the caller prints. */
function scanFile(file, display, options) {
  const violations = [];
  const lines = fs.readFileSync(file, "utf8").split("\n");
  for (let index = 0; index < lines.length; index++) {
    const raw = lines[index];
    const code = codeOnly(raw);
    const text = stripComment(raw);
    for (const rule of RULES) {
      if (rule.needed && !rule.needed(options)) continue;
      if (rule.excuse && rule.excuse.test(raw)) continue;
      if (!rule.match(code, text)) continue;
      violations.push({ file: display, line: index + 1, rule: rule.id, reason: rule.reason });
    }
  }
  return violations;
}

/** <web>/src/screens/**, the screens' consumer surface. */
function scanScreens(webDir, options) {
  const root = path.join(webDir, "src", "screens");
  if (!fs.existsSync(root)) throw new Error(`no screens directory at ${root} (pass --web <dir>)`);
  const displayRoot = path.join(path.basename(webDir), "src", "screens");
  const violations = [];
  for (const file of walk(root)) {
    const display = path.join(displayRoot, path.relative(root, file));
    violations.push(...scanFile(file, display, options));
  }
  return violations;
}

/** <web>/App.tsx — the app root: the same four rules. It wires the shell the
 * screens live in (the route branches, the h-12 header's slots, the overlays),
 * so its widths, layout primitives and colours are the kit's too. */
function scanAppEntry(webDir, options) {
  const file = path.join(webDir, "App.tsx");
  if (!fs.existsSync(file)) return [];
  return scanFile(file, path.join(path.basename(webDir), "App.tsx"), options);
}

/** Every consumer surface this gate owns: the screens and the app root. */
function scanConsumer(webDir, options) {
  return [...scanScreens(webDir, options), ...scanAppEntry(webDir, options)];
}

// ---------------------------------------------------------------------------
// --self-test: a fixture with one violation per rule must fail, and the same
// fixture with the violations removed must pass (the negative control Q4).

function runSelfTest() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "kit-imports-"));
  const screens = path.join(dir, "web", "src", "screens");
  fs.mkdirSync(screens, { recursive: true });
  const write = (name, text) => fs.writeFileSync(path.join(screens, name), `${text}\n`);

  write(
    "bad-legacy.tsx",
    `import { Button, Press } from "../ui/primitives";\nexport function Bad() {\n  return <Press><Button /></Press>;\n}`,
  );
  write(
    "bad-layout.tsx",
    `import { Sidebar } from "../ui/kit";\nimport { SidePane } from "../ui/kit/layout/side-pane";\nexport function Bad() {\n  return <SidePane width={264} />;\n}`,
  );
  write(
    "bad-magic.tsx",
    `export function Bad() {\n  return <View style={{ maxWidth: 480, width: "45%" }} />;\n}`,
  );
  write(
    "bad-color.tsx",
    `export function Bad() {\n  return <View style={{ backgroundColor: "#8b5cf6" }} />;\n}`,
  );
  write(
    "good-screen.tsx",
    `import { Button, Card, WorkspaceShell } from "../ui/kit";\nimport { color } from "../ui/kit/tokens";\nexport function Good() {\n  return (\n    <WorkspaceShell>\n      <Card title="ok">\n        <Button tint={color.brand}>fine</Button>\n      </Card>\n    </WorkspaceShell>\n  );\n}`,
  );

  const options = { colors: true };
  const bad = scanScreens(path.join(dir, "web"), options);
  const expectedRules = ["legacy-primitives", "layout-primitive", "shell-magic-width", "raw-color"];
  const seen = new Set(bad.map((violation) => violation.rule));
  const problems = [];
  if (bad.length === 0) problems.push("the violating fixture produced no violation (the negative control is broken)");
  for (const rule of expectedRules) {
    if (!seen.has(rule)) problems.push(`no violation for rule ${rule}`);
  }
  if (bad.length > 0 && !bad.every((violation) => violation.line > 0 && violation.file.includes("web/src/screens/"))) {
    problems.push("violations lack a file:line");
  }

  // The app root is a consumer surface too: one violation there is caught, and
  // removing it scans clean (the second negative control).
  const appEntry = path.join(dir, "web", "App.tsx");
  fs.writeFileSync(
    appEntry,
    `import { Sidebar } from "./src/ui/kit";\nexport function App() {\n  return <Sidebar collapsed={false} />;\n}\n`,
  );
  const appBad = scanAppEntry(path.join(dir, "web"), options);
  if (appBad.length === 0) problems.push("a violating web/App.tsx produced no violation");
  if (appBad.length > 0 && !appBad.every((violation) => violation.file === path.join("web", "App.tsx"))) {
    problems.push("the web/App.tsx control reports the wrong file");
  }
  fs.rmSync(appEntry);
  const appClean = scanAppEntry(path.join(dir, "web"), options);
  if (appClean.length > 0) problems.push(`a clean web/App.tsx still reports ${appClean.length} violation(s)`);

  // Remove the violations: the same fixture must now scan clean.
  fs.rmSync(path.join(screens, "bad-legacy.tsx"));
  fs.rmSync(path.join(screens, "bad-layout.tsx"));
  fs.rmSync(path.join(screens, "bad-magic.tsx"));
  fs.rmSync(path.join(screens, "bad-color.tsx"));
  const clean = scanScreens(path.join(dir, "web"), options);
  if (clean.length > 0) problems.push(`the clean fixture still reports ${clean.length} violation(s)`);

  fs.rmSync(dir, { recursive: true, force: true });

  if (problems.length > 0) {
    console.error("kit-imports: self-test FAIL");
    for (const problem of problems) console.error(`  ${problem}`);
    return 1;
  }
  console.log(`kit-imports: self-test PASS — the violating fixture exits 1 with ${bad.length} violation(s) (${expectedRules.join(", ")}), the clean fixture exits 0`);
  return 0;
}

// ---------------------------------------------------------------------------

function parseArgs(argv) {
  const args = { web: DEFAULT_WEB, webExplicit: false, selfTest: false, colors: true };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "--web") {
      const next = argv[++i];
      if (!next) throw new Error("--web needs a directory");
      args.web = path.resolve(next);
      args.webExplicit = true;
    } else if (arg === "--self-test") args.selfTest = true;
    else if (arg === "--no-colors") args.colors = false;
    else if (arg === "--help" || arg === "-h") args.help = true;
    else throw new Error(`unknown argument: ${arg}`);
  }
  return args;
}

function main() {
  let args;
  try {
    args = parseArgs(process.argv.slice(2));
  } catch (error) {
    console.error(`kit-imports: ${error.message}`);
    return 2;
  }
  if (args.help) {
    console.log("usage: node scripts/kit-imports.mjs [--web <dir>] [--no-colors] [--self-test]");
    return 0;
  }

  let failed = 0;
  if (args.selfTest) failed = runSelfTest() || failed;
  // `--self-test` alone is the control: it must come back green (its fixture
  // came back red). The real scan runs when --web is given, or when only the
  // scan was asked for.
  if (args.selfTest && !args.webExplicit) return failed;

  let violations;
  try {
    violations = scanConsumer(args.web, { colors: args.colors });
  } catch (error) {
    console.error(`kit-imports: ${error.message}`);
    return 2;
  }

  for (const violation of violations) {
    console.log(`${violation.file}:${violation.line} ${violation.reason}`);
  }
  const webRoot = path.basename(args.web);
  const screens = path.join(webRoot, "src", "screens");
  const appEntry = path.join(webRoot, "App.tsx");
  if (violations.length > 0) {
    console.log(`kit-imports: ${violations.length} violation(s) in ${screens} and ${appEntry}`);
    return 1;
  }
  console.log(
    `kit-imports: clean — ${screens} and ${appEntry} import only the kit, write no shell widths and no raw colours`,
  );
  return failed;
}

process.exit(main());
