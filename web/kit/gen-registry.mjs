#!/usr/bin/env node
// gen-registry.mjs — the gallery's optional imports, resolved at build time.
//
// The gallery must run in both states of this change's parallel work: before
// web/src/ui/kit exists (every component a "missing: <name>" card) and after it
// lands (the real components rendered). A TSX app cannot conditionally import,
// so this script does the probing: it scans web/src/ui/kit, imports what it
// finds into web/kit/kit-registry.generated.tsx, and leaves what it does not to
// the gallery's placeholder cards.
//
//   node web/kit/gen-registry.mjs                 # regenerate the registry
//   node web/kit/gen-registry.mjs --emit-eval <file>          # the page probe
//   node web/kit/gen-registry.mjs --assert-page <file> [...]  # coverage gate
//
// The coverage gate is the second half of scripts/kit-gate.sh. The probe
// (--emit-eval) is piped into `agent-browser eval --stdin` at each viewport;
// it returns the page text plus, per manifest entry, the title label's box and
// whether a hard-clipping ancestor cuts it. The assertion reads those JSON
// dumps and fails when a name is missing from the kit, absent from the page,
// clipped, pushed outside the viewport horizontally, or missing its box.
//
// No dependencies; Node's own fs.
import fs from "node:fs";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const DEFAULT_KIT = path.resolve(HERE, "../src/ui/kit");
const DEFAULT_MANIFEST = path.resolve(HERE, "kit-manifest.tsx");
const DEFAULT_OUT = path.resolve(HERE, "kit-registry.generated.tsx");

const SKIP_DIRS = new Set([".nsh", "node_modules", "dist", ".git", "__tests__", "test", "tests"]);
const SKIP_FILE = /\.(test|spec)\.[jt]sx?$|\.d\.ts$/;

function parseArgs(argv) {
  const args = {
    kit: DEFAULT_KIT,
    manifest: DEFAULT_MANIFEST,
    out: DEFAULT_OUT,
    pages: [],
    emitEval: null,
    emitSections: null,
    emitSection: null,
  };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "--assert-page") {
      // one or more dump files follow, until the next flag
      let next = argv[i + 1];
      if (!next || next.startsWith("--")) throw new Error("--assert-page needs a file");
      while (next && !next.startsWith("--")) {
        args.pages.push(next);
        i++;
        next = argv[i + 1];
      }
    } else if (arg === "--emit-eval") {
      args.emitEval = argv[++i];
      if (!args.emitEval) throw new Error("--emit-eval needs a file");
    } else if (arg === "--emit-section") {
      // `--emit-section <label> <file>`: the scroll snippet for one section.
      const label = argv[++i];
      const file = argv[++i];
      if (!label || !file) throw new Error("--emit-section needs a label and a file");
      args.emitSection = { label, file };
    } else if (arg === "--emit-sections") {
      args.emitSections = argv[++i];
      if (!args.emitSections) throw new Error("--emit-sections needs a file");
    } else if (arg === "--kit") args.kit = path.resolve(argv[++i]);
    else if (arg === "--manifest") args.manifest = path.resolve(argv[++i]);
    else if (arg === "--out") args.out = path.resolve(argv[++i]);
    else throw new Error(`unknown argument: ${arg}`);
  }
  return args;
}

// ---------------------------------------------------------------------------
// Scanning web/src/ui/kit

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
    } else if (/\.(ts|tsx)$/.test(entry.name) && !SKIP_FILE.test(entry.name)) {
      files.push(full);
    }
  }
  return files;
}

/** The value exports of one module: `{ found: name -> { definition, from }, stars: export * specs }`. */
function exportsOf(text) {
  const found = new Map();
  const stars = [];
  const lines = text.split("\n");
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const trimmed = line.trim();
    if (trimmed.startsWith("//") || trimmed.startsWith("*") || trimmed.startsWith("/*")) continue;

    // export function / class / const / let / var / enum / async function
    const decl = trimmed.match(
      /^export\s+(?:declare\s+)?(?:async\s+)?(?:function|class|const|let|var|enum)\s+([A-Za-z_$][\w$]*)/,
    );
    if (decl) {
      if (!found.has(decl[1])) found.set(decl[1], { definition: true, from: null });
      continue;
    }

    // export { A, B as C } / export { A } from "./x" / export * from "./x"
    const clause = trimmed.match(/^export\s+(?:\*|\{([^}]*)\})\s*(?:from\s*["']([^"']+)["'])?/);
    if (clause) {
      const source = clause[2] ?? null;
      if (clause[1] === undefined) {
        // `export * from "./x"`: everything that module exports is re-exported.
        if (source) stars.push(source);
        continue;
      }
      const items = clause[1].split(",");
      for (const raw of items) {
        const item = raw.trim();
        if (!item || /^type\s/.test(item)) continue;
        const parts = item.split(/\s+as\s+/);
        const name = (parts[1] ?? parts[0]).trim();
        if (!/^[A-Za-z_$][\w$]*$/.test(name)) continue;
        const entry = found.get(name);
        if (!entry) found.set(name, { definition: false, from: source });
        else if (entry.definition === false && entry.from === null && source !== null) entry.from = source;
      }
    }
  }
  return { found, stars };
}

function scanKit(root) {
  const modules = new Map(); // absolute file -> { found, stars }
  for (const file of walk(root)) modules.set(file, exportsOf(fs.readFileSync(file, "utf8")));
  return modules;
}

/** The app's public exit: a root-level index/mod barrel, if the kit has one. */
function findBarrel(root) {
  for (const name of ["index.ts", "index.tsx", "mod.ts", "mod.tsx"]) {
    const file = path.join(root, name);
    if (fs.existsSync(file)) return file;
  }
  return null;
}

/** Whether `barrel` (or what it re-exports) reaches a definition of `name`. */
function barrelReaches(barrel, modules, name, depth = 0, seen = new Set()) {
  if (depth > 4 || seen.has(barrel)) return false;
  seen.add(barrel);
  const exports = modules.get(barrel);
  if (!exports) return false;
  const own = exports.found.get(name);
  if (own && own.definition) return true;
  for (const [, entry] of exports.found) {
    if (!entry.from || entry.definition) continue;
    const target = resolveSpecifier(barrel, entry.from, modules);
    if (target && barrelReaches(target, modules, name, depth + 1, seen)) return true;
  }
  for (const star of exports.stars) {
    const target = resolveSpecifier(barrel, star, modules);
    if (target && barrelReaches(target, modules, name, depth + 1, seen)) return true;
  }
  return false;
}

function resolveSpecifier(fromFile, spec, modules) {
  if (!spec.startsWith(".")) return null;
  const base = path.resolve(path.dirname(fromFile), spec);
  for (const candidate of [base, `${base}.ts`, `${base}.tsx`, path.join(base, "index.ts"), path.join(base, "index.tsx")]) {
    if (modules.has(candidate)) return candidate;
  }
  return null;
}

/** Manifest names, parsed from kit-manifest.tsx (the gate's coverage list). */
function parseManifest(file) {
  const text = fs.readFileSync(file, "utf8");
  const lines = text.split("\n");
  const subNames = new Set();
  const entries = [];
  for (let i = 0; i < lines.length; i++) {
    const trimmed = lines[i].trim();
    if (trimmed.startsWith("//") || trimmed.startsWith("*") || trimmed.startsWith("/*")) continue;
    // A composite demo's sub-components: `need("CardHeader")` in compose().
    for (const match of lines[i].matchAll(/need\("([A-Za-z_$][\w$]*)"\)/g)) subNames.add(match[1]);
    const name = trimmed.match(/^name:\s*"([^"]+)"/);
    if (!name) continue;
    const entry = { name: name[1], aliases: [] };
    for (let j = i + 1; j < Math.min(i + 14, lines.length); j++) {
      const alias = lines[j].match(/^\s*aliases:\s*\[([^\]]*)\]/);
      if (alias) {
        entry.aliases = alias[1]
          .split(",")
          .map((item) => item.trim().replace(/^["']|["']$/g, ""))
          .filter(Boolean);
        break;
      }
      if (/^\s*name:\s*"/.test(lines[j])) break;
    }
    entries.push(entry);
  }
  const groups = [];
  const groupBlock = /KIT_GROUPS[^=]*=\s*\[([\s\S]*?)\];/.exec(text);
  if (groupBlock) {
    for (const match of groupBlock[1].matchAll(/id:\s*"([a-z]+)",\s*label:\s*"([^"]+)"/g)) {
      groups.push({ id: match[1], label: match[2] });
    }
  }
  return { entries, subNames: [...subNames].sort((a, b) => a.localeCompare(b)), groups };
}

/** The scroll snippet the gate runs before each per-group screenshot: it finds
 * the group heading in the page and scrolls the gallery's scroller to it. */
function scrollScript(label) {
  return `(() => {
  const label = ${JSON.stringify(label)};
  const el = [...document.querySelectorAll("div,span,p")].find((e) => (e.textContent || "").trim() === label);
  if (!el) return "no-section: " + label;
  const scroller = el.closest(".nsh-scroll") || document.querySelector(".nsh-scroll");
  if (!scroller) return "no-scroller";
  const r = el.getBoundingClientRect();
  const sr = scroller.getBoundingClientRect();
  scroller.scrollTop += Math.round(r.top - sr.top) - 12;
  return String(scroller.scrollTop);
})()`;
}

// ---------------------------------------------------------------------------
// Writing kit-registry.generated.tsx

function importSpecifier(modulePath) {
  const relative = path.relative(HERE, modulePath).split(path.sep).join("/");
  return relative.replace(/\.tsx?$/, "");
}

function generate({ kit, manifest, out }) {
  const modules = scanKit(kit);
  const barrel = findBarrel(kit);
  const resolution = new Map(); // name -> { file, via, ambiguous: string[] }
  const all = new Map(); // name -> [files that mention it]

  for (const [file, exports] of modules) {
    for (const [name] of exports.found) {
      if (!all.has(name)) all.set(name, []);
      all.get(name).push(file);
    }
  }

  for (const [name, files] of all) {
    const definitions = files.filter((file) => modules.get(file).found.get(name)?.definition);
    const candidates = definitions.length > 0 ? definitions : files;
    const sorted = [...candidates].sort((a, b) => a.length - b.length || a.localeCompare(b));
    const viaBarrel = barrel && barrelReaches(barrel, modules, name);
    const file = viaBarrel ? barrel : sorted[0];
    resolution.set(name, {
      file,
      via: viaBarrel ? "barrel" : "direct",
      ambiguous: sorted.filter((candidate) => candidate !== file),
    });
  }

  const names = [...resolution.keys()].sort((a, b) => a.localeCompare(b));

  const { entries: manifestEntries, subNames, groups } = parseManifest(manifest);
  if (manifestEntries.length < 20) {
    throw new Error(
      `the manifest parsed to only ${manifestEntries.length} entries (${manifest}); ` +
        "expected >= 20 — keep one `name: \"<Name>\"` line per entry",
    );
  }
  const manifestNames = new Set();
  for (const entry of manifestEntries) {
    if (manifestNames.has(entry.name)) throw new Error(`duplicate manifest name: ${entry.name}`);
    manifestNames.add(entry.name);
    for (const alias of entry.aliases) manifestNames.add(alias);
  }
  // Only what the manifest (an alias, or a composite's `need("...")`) can
  // render is imported: a kit module nothing here needs must not be able to
  // break the gallery's build.
  const needed = new Set([...manifestNames, ...subNames]);
  const imported = names.filter((name) => needed.has(name));
  const unlisted = names.filter((name) => !needed.has(name));
  const missingSubs = subNames.filter((name) => !resolution.has(name));

  const body = [];
  body.push("// GENERATED by web/kit/gen-registry.mjs — do not edit by hand.");
  body.push("// Re-run: node web/kit/gen-registry.mjs");
  body.push("//");
  body.push("// The kit exports the gallery's manifest can render, imported here. A");
  body.push('// manifest name this file does not carry renders as "missing: <name>" — that');
  body.push("// is the kit's door, not a gallery bug: land the component, re-run the gate.");
  body.push("// Every other export the kit has is only listed (kitUnlisted), not imported:");
  body.push("// a module no manifest entry needs cannot break the gallery's build.");
  body.push(`// kit:      ${path.relative(process.cwd(), kit) || kit}`);
  body.push(`// barrel:   ${barrel ? `${importSpecifier(barrel)} (public exit)` : "none (no index.ts)"}`);
  body.push(`// exported: ${names.length} (imported ${imported.length}, unlisted ${unlisted.length})`);
  if (subNames.length > 0) body.push(`// composite sub-components: ${subNames.length}${missingSubs.length ? ` (missing ${missingSubs.join(", ")})` : ""}`);
  const ambiguous = imported.filter((name) => resolution.get(name).ambiguous.length > 0);
  if (ambiguous.length > 0) {
    body.push(`// defined in more than one module (the shortest path wins, others listed):`);
    for (const name of ambiguous.slice(0, 12)) {
      const others = resolution.get(name).ambiguous.map((file) => importSpecifier(file)).join(", ");
      body.push(`//   ${name}: also in ${others}`);
    }
    if (ambiguous.length > 12) body.push(`//   … and ${ambiguous.length - 12} more`);
  }
  body.push("");
  imported.forEach((name, index) => {
    const { file, via } = resolution.get(name);
    body.push(`import { ${name} as kit${index} } from "${importSpecifier(file)}";${via === "direct" && barrel ? " // direct (not re-exported by the barrel)" : ""}`);
  });
  body.push("");
  body.push("/** name -> the export, for the gallery's dynamic render. */");
  body.push("export const kitComponents: Record<string, unknown> = {");
  imported.forEach((name, index) => body.push(`  ${JSON.stringify(name)}: kit${index},`));
  body.push("};");
  body.push("");
  body.push("/** name -> the module it came from (shown in the gallery's source line). */");
  body.push("export const kitComponentSources: Record<string, string> = {");
  for (const name of imported) body.push(`  ${JSON.stringify(name)}: ${JSON.stringify(importSpecifier(resolution.get(name).file))},`);
  body.push("};");
  body.push("");
  body.push("/** name -> whether it came from the public exit or a module directly. */");
  body.push("export const kitComponentVia: Record<string, string> = {");
  for (const name of imported) body.push(`  ${JSON.stringify(name)}: ${JSON.stringify(resolution.get(name).via)},`);
  body.push("};");
  body.push("");
  body.push(`export const kitBarrelPresent: boolean = ${barrel ? "true" : "false"};`);
  body.push("");
  body.push("/** Every value name the kit exports (imported or not). */");
  body.push("export const kitDetected: string[] = [");
  for (const name of names) body.push(`  ${JSON.stringify(name)},`);
  body.push("];");
  body.push("");
  body.push("/** Detected exports no manifest entry (or alias) names. */");
  body.push("export const kitUnlisted: string[] = [");
  for (const name of unlisted) body.push(`  ${JSON.stringify(name)},`);
  body.push("];");
  body.push("");
  body.push("/** The sub-components the manifest's composite demos ask for. */");
  body.push("export const kitSubComponents: string[] = [");
  for (const name of subNames) body.push(`  ${JSON.stringify(name)},`);
  body.push("];");

  fs.writeFileSync(out, `${body.join("\n")}\n`);

  const resolvedManifest = manifestEntries.filter((entry) =>
    [entry.name, ...entry.aliases].some((candidate) => resolution.has(candidate)),
  );
  const missing = manifestEntries
    .filter((entry) => !resolvedManifest.includes(entry))
    .map((entry) => entry.name);

  return {
    resolution,
    groups,
    names,
    imported,
    manifestEntries,
    missing,
    missingSubs,
    unlisted,
    subNames,
    barrel,
    kitPresent: modules.size > 0,
  };
}

// ---------------------------------------------------------------------------
// The page probe (agent-browser eval --stdin) and its assertion

/** The probe runs in the page: it returns the page text and, per manifest
 * entry, its title label's box and whether a hard-clipping ancestor cuts it. */
function probeScript(manifest) {
  const names = manifest.map((entry) => entry.name);
  return `(() => {
  const names = ${JSON.stringify(names)};
  const inspect = (el) => {
    const rect = el.getBoundingClientRect();
    let cutX = false;
    let cutY = false;
    let scrollableX = false;
    let scrollableY = false;
    for (let a = el.parentElement; a; a = a.parentElement) {
      // A scroller anywhere around the label can bring it into view: a cut by
      // an outer clipper (the app root clips, the inner Scroll scrolls) is not
      // a layout defect.
      if (a.scrollWidth > a.clientWidth + 1) scrollableX = true;
      if (a.scrollHeight > a.clientHeight + 1) scrollableY = true;
      const s = getComputedStyle(a);
      const hides = (v) => v === "hidden" || v === "clip";
      if (hides(s.overflow) || hides(s.overflowX) || hides(s.overflowY)) {
        const r = a.getBoundingClientRect();
        if (rect.left < r.left - 1 || rect.right > r.right + 1) cutX = true;
        if (rect.top < r.top - 1 || rect.bottom > r.bottom + 1) cutY = true;
      }
    }
    // Clipped: a hard cut with nothing to scroll, or the label's own text
    // truncated inside its box.
    const clipped = (cutX && !scrollableX) || (cutY && !scrollableY) || el.scrollWidth > el.clientWidth + 1;
    return { w: Math.round(rect.width), h: Math.round(rect.height), x: Math.round(rect.left), y: Math.round(rect.top), clipped };
  };
  const labelFor = (name) => {
    const all = document.querySelectorAll("div, span, p, button, h1, h2, h3, h4, li, td, th, label, a");
    for (const el of all) {
      if ((el.textContent || "").trim() !== name) continue;
      let nested = false;
      for (const child of el.children) {
        if ((child.textContent || "").trim() === name) { nested = true; break; }
      }
      if (!nested) return el;
    }
    return null;
  };
  const labels = {};
  for (const name of names) {
    const el = labelFor(name);
    labels[name] = el ? inspect(el) : null;
  }
  const doc = document.documentElement;
  const body = document.body;
  return JSON.stringify({
    innerWidth: window.innerWidth,
    innerHeight: window.innerHeight,
    scrollWidth: Math.max(doc ? doc.scrollWidth : 0, body ? body.scrollWidth : 0),
    scrollHeight: Math.max(doc ? doc.scrollHeight : 0, body ? body.scrollHeight : 0),
    innerText: body ? body.innerText : "",
    labels,
  });
})()`;
}

/** `agent-browser eval` prints the probe's JSON string, quoted; unwrap it. */
function parseDump(raw) {
  let value = JSON.parse(raw);
  if (typeof value === "string") value = JSON.parse(value);
  return value;
}

/** The gate's coverage assertion over the probe dumps (one per viewport). */
function assertPages(files, manifest, subNames = []) {
  const problems = [];
  const lines = [];
  for (const file of files) {
    let page;
    try {
      page = parseDump(fs.readFileSync(file, "utf8"));
    } catch (error) {
      problems.push(`${path.basename(file)}: unreadable probe dump (${error.message})`);
      continue;
    }
    const text = String(page.innerText ?? "");
    if (!text.includes("Keel UI Kit Gallery")) problems.push(`${path.basename(file)}: the gallery page did not render`);
    for (const name of subNames) {
      if (text.includes(`missing child: ${name}`)) {
        problems.push(`${path.basename(file)}: composite sub-component ${name} — web/src/ui/kit does not export it (card: "missing child: ${name}")`);
      }
    }
    const overflow = Number(page.scrollWidth ?? 0) - Number(page.innerWidth ?? 0);
    if (overflow > 2) problems.push(`${path.basename(file)}: page overflows horizontally by ${overflow}px (scrollWidth ${page.scrollWidth} > innerWidth ${page.innerWidth})`);
    let absent = 0;
    let missing = 0;
    let clipped = 0;
    let outside = 0;
    let compound = 0;
    for (const entry of manifest) {
      const candidates = [entry.name, ...entry.aliases];
      if (!candidates.some((candidate) => text.includes(candidate))) {
        absent++;
        problems.push(`${path.basename(file)}: ${entry.name} — no text on the page`);
        continue;
      }
      if (text.includes(`missing: ${entry.name}`)) {
        missing++;
        problems.push(`${path.basename(file)}: ${entry.name} — missing from web/src/ui/kit (card: "missing: ${entry.name}")`);
        continue;
      }
      if (text.includes(`compound: ${entry.name}`)) compound++;
      const box = (page.labels ?? {})[entry.name];
      if (!box) {
        problems.push(`${path.basename(file)}: ${entry.name} — its title label has no element in the DOM`);
        continue;
      }
      if (box.w < 1 || box.h < 1) {
        clipped++;
        problems.push(`${path.basename(file)}: ${entry.name} — the title label measures ${box.w}x${box.h}`);
        continue;
      }
      if (box.x < -1 || box.x + box.w > Number(page.innerWidth ?? 0) + 1) {
        outside++;
        problems.push(`${path.basename(file)}: ${entry.name} — the title label sits outside the viewport horizontally (x ${box.x}..${box.x + box.w}, innerWidth ${page.innerWidth})`);
        continue;
      }
      if (box.clipped) {
        clipped++;
        problems.push(`${path.basename(file)}: ${entry.name} — the title label is cut by a clipping ancestor`);
      }
    }
    lines.push(
      `kit-coverage: ${path.basename(file)} ${page.innerWidth}x${page.innerHeight} labels=${manifest.length - absent}/${manifest.length} missing=${missing} clipped=${clipped} outside=${outside} compound=${compound} h-overflow=${overflow > 2 ? `${overflow}px` : "none"}`,
    );
  }
  return { problems, lines };
}

// ---------------------------------------------------------------------------

function main() {
  const args = parseArgs(process.argv.slice(2));

  if (args.emitSection) {
    fs.writeFileSync(args.emitSection.file, `${scrollScript(args.emitSection.label)}\n`);
    console.log(`kit-scroll: wrote ${args.emitSection.file} for ${args.emitSection.label}`);
    return 0;
  }

  const generated = generate(args);

  if (args.emitSections) {
    fs.writeFileSync(args.emitSections, `${JSON.stringify(generated.groups, null, 2)}\n`);
    console.log(`kit-sections: wrote ${args.emitSections} (${generated.groups.length} groups)`);
    return 0;
  }

  if (args.emitEval) {
    fs.writeFileSync(args.emitEval, `${probeScript(generated.manifestEntries)}\n`);
    console.log(`kit-probe: wrote ${args.emitEval} (${generated.manifestEntries.length} names)`);
    return 0;
  }

  if (args.pages.length > 0) {
    const { problems, lines } = assertPages(args.pages, generated.manifestEntries, generated.subNames);
    for (const line of lines) console.log(line);
    const missingNames = generated.manifestEntries
      .filter((entry) => !generated.resolution.has(entry.name) && !entry.aliases.some((alias) => generated.resolution.has(alias)))
      .map((entry) => entry.name);
    if (missingNames.length) console.log(`kit-coverage: missing from the kit (${missingNames.length}): ${missingNames.join(", ")}`);
    if (problems.length) {
      console.error(`kit-coverage: FAIL — ${problems.length} problem(s) over ${args.pages.length} viewport(s)`);
      for (const problem of problems) console.error(`  ${problem}`);
      return 1;
    }
    console.log(`kit-coverage: PASS — ${generated.manifestEntries.length} manifest entries over ${args.pages.length} viewport(s)`);
    return 0;
  }

  const { names, imported, manifestEntries, missing, missingSubs, subNames, unlisted, barrel, kitPresent } = generated;
  console.log(`kit-registry: kit=${kitPresent ? "present" : "absent"} barrel=${barrel ? "yes" : "no"} exported=${names.length} imported=${imported.length}`);
  console.log(`kit-registry: manifest=${manifestEntries.length} missing=${missing.length} unlisted=${unlisted.length} sub=${subNames.length} sub-missing=${missingSubs.length}`);
  if (missing.length) console.log(`missing: ${missing.join(", ")}`);
  if (missingSubs.length) console.log(`missing sub-components: ${missingSubs.join(", ")}`);
  if (unlisted.length) console.log(`unlisted: ${unlisted.join(", ")}`);
  return 0;
}

process.exit(main());
