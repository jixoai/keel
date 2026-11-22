#!/usr/bin/env node
// plugin-catalog.mjs — the catalog single-source gate.
//
// `plugins/keel.plugin-market/catalog.json` is the only machine-readable truth
// for the official/community coverage matrix. This script:
//
//   1. validates the catalog schema and its frozen enums (status/wave);
//   2. checks every keelPluginId / dshGroup reference resolves;
//   3. maps every COVERAGE.md §A/§B/§C row to a catalog keelPluginId (or an
//      explicit absent/merged disposition with a reason);
//   4. cross-checks the installed official packages under plugins/ against the
//      catalog (an installed official plugin must be delivered/partial);
//   5. `--write` regenerates the machine-readable §H section of COVERAGE.md;
//      `--check` (the CI mode) fails when it is stale or any rule is broken.
//
// No dependencies. Usage:
//     node scripts/plugin-catalog.mjs --check
//     node scripts/plugin-catalog.mjs --write
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const CATALOG = path.join(ROOT, "plugins/keel.plugin-market/catalog.json");
const COVERAGE = path.join(ROOT, "plugins/COVERAGE.md");
const PLUGINS = path.join(ROOT, "plugins");

const STATUSES = ["delivered", "partial", "planned", "registered", "merged", "absent"];
const WAVES = ["W0.5", "W1", "W2", "W3", "never"];
const TERMINAL_STATUSES = new Set(["absent", "merged"]);
const BEGIN = "<!-- keel-plugin-catalog:begin -->";
const END = "<!-- keel-plugin-catalog:end -->";

const errors = [];
const fail = (message) => errors.push(message);

const read = (file) => fs.readFileSync(file, "utf8");
const catalog = JSON.parse(read(CATALOG));
const coverage = read(COVERAGE);
const entries = Array.isArray(catalog.entries) ? catalog.entries : [];

// --- catalog ---------------------------------------------------------------
function validateCatalog() {
  if (catalog.schema !== "keel.plugin-market.catalog/1") {
    fail(`catalog.schema = ${JSON.stringify(catalog.schema)}, want "keel.plugin-market.catalog/1"`);
  }
  if (!/^\d{4}-\d{2}-\d{2}$/.test(String(catalog.generatedAt || ""))) {
    fail("catalog.generatedAt must be a YYYY-MM-DD date");
  }
  if (!entries.length) fail("catalog.entries is empty");

  const ids = new Set();
  const keelIds = new Set();
  for (const entry of entries) {
    const at = `entry ${entry.id ?? "<missing id>"}`;
    if (!entry.id || typeof entry.id !== "string") {
      fail(`${at}: id is required`);
      continue;
    }
    if (ids.has(entry.id)) fail(`${at}: duplicate id`);
    ids.add(entry.id);
    for (const field of ["name", "summary"]) {
      if (!entry[field] || typeof entry[field] !== "string") fail(`${at}: ${field} is required`);
    }
    if (!Array.isArray(entry.capabilities) || entry.capabilities.length === 0) {
      fail(`${at}: capabilities must be a non-empty array`);
    }
    if (!entry.source || typeof entry.source !== "object") {
      fail(`${at}: source is required`);
    } else if (!["dsh-official", "dsh-community", "keel-official"].includes(entry.source.kind)) {
      fail(`${at}: source.kind ${JSON.stringify(entry.source.kind)} is not dsh-official|dsh-community|keel-official`);
    }
    if (!Array.isArray(entry.dshPackages)) fail(`${at}: dshPackages must be an array`);
    if (!WAVES.includes(entry.wave)) {
      fail(`${at}: wave ${JSON.stringify(entry.wave)} is not one of ${WAVES.join("|")}`);
    }
    if (!Array.isArray(entry.dshGroups)) {
      fail(`${at}: dshGroups must be an array (use [] when none)`);
    }
    const coverage = entry.coverage;
    if (!coverage || typeof coverage !== "object") {
      fail(`${at}: coverage is required`);
      continue;
    }
    if (typeof coverage.covered !== "boolean") fail(`${at}: coverage.covered must be boolean`);
    if (!STATUSES.includes(coverage.status)) {
      fail(`${at}: coverage.status ${JSON.stringify(coverage.status)} is not one of ${STATUSES.join("|")}`);
    }
    if (!Array.isArray(coverage.tools)) fail(`${at}: coverage.tools must be an array`);
    if (coverage.keelPluginId) keelIds.add(coverage.keelPluginId);

    const status = coverage.status;
    if (status === "absent") {
      if (coverage.keelPluginId) fail(`${at}: absent rows must not name a keelPluginId`);
      if (!coverage.note || coverage.note.trim().length < 8) {
        fail(`${at}: absent rows need a note with the reason`);
      }
    } else if (status === "merged") {
      if (!coverage.keelPluginId && !coverage.mergedInto) {
        fail(`${at}: merged rows need keelPluginId or mergedInto`);
      }
      if (!coverage.note || coverage.note.trim().length < 8) {
        fail(`${at}: merged rows need a note with the destination`);
      }
    } else if (!coverage.keelPluginId) {
      fail(`${at}: status ${status} requires coverage.keelPluginId`);
    }
    if (status === "delivered") {
      if (!Array.isArray(coverage.tools) || coverage.tools.length === 0) {
        fail(`${at}: delivered rows must list their published tools`);
      }
      if (entry.id.startsWith("keel.") && entry.id !== coverage.keelPluginId) {
        fail(`${at}: a keel-official row must name itself as keelPluginId`);
      }
    }
    if (entry.id.startsWith("dsh.")) {
      if (!Array.isArray(entry.dshGroups) || !entry.dshGroups.includes(entry.id)) {
        fail(`${at}: a dsh.* entry must include its own id in dshGroups`);
      }
    }
  }

  // Reference closure: every keelPluginId and dshGroup resolves.
  const retired = Array.isArray(catalog.retiredIds) ? catalog.retiredIds : [];
  if (!Array.isArray(catalog.retiredIds)) fail("catalog.retiredIds must be an array (use [] when none)");
  const retiredIds = new Set();
  for (const item of retired) {
    const at = `retiredId ${item.id ?? "<missing id>"}`;
    if (!item.id) {
      fail(`${at}: id is required`);
      continue;
    }
    if (retiredIds.has(item.id)) fail(`${at}: duplicate id`);
    retiredIds.add(item.id);
    if (!["merged", "absent"].includes(item.status)) {
      fail(`${at}: status must be merged|absent`);
    }
    if (item.status === "merged" && !item.into) fail(`${at}: merged ids need into`);
    if (!item.reason || item.reason.trim().length < 8) fail(`${at}: reason is required`);
  }
  const resolve = (id) =>
    ids.has(id) || keelIds.has(id) || retiredIds.has(id) ||
    (Array.isArray(retired) && retired.some((item) => item.into === id));
  for (const entry of entries) {
    for (const id of entry.dshGroups || []) {
      if (!ids.has(id)) fail(`entry ${entry.id}: dshGroup ${JSON.stringify(id)} does not name an entry`);
    }
    const keelId = entry.coverage?.keelPluginId;
    if (keelId && !resolve(keelId)) {
      fail(`entry ${entry.id}: keelPluginId ${JSON.stringify(keelId)} is not an entry, a keelPluginId or a retired id`);
    }
  }
}

// --- COVERAGE.md -----------------------------------------------------------
function section(name) {
  const parts = coverage.split(/^## /m);
  return parts.find((part) => part.startsWith(`${name}.`)) || "";
}

function tableRows(sectionText) {
  const rows = [];
  for (const line of sectionText.split("\n")) {
    if (!line.trim().startsWith("|")) continue;
    const cells = line.trim().split("|").map((cell) => cell.trim());
    cells.shift();
    cells.pop();
    if (!cells.length) continue;
    if (cells.every((cell) => /^:?-{3,}:?$/.test(cell))) continue;
    if (/^(DSH 组|DSH bundle|社区项目)/.test(cells[0])) continue;
    rows.push(cells);
  }
  return rows;
}

function backtickedIds(cell) {
  return [...String(cell).matchAll(/`([a-z][a-z0-9._-]*)`/g)].map((match) => match[1]);
}

function boldDeliveredIds(cell) {
  const out = [];
  for (const match of String(cell).matchAll(/\*\*`([a-z][a-z0-9._-]*)`\*\*/g)) out.push(match[1]);
  return out;
}

function statusesIn(cell) {
  return STATUSES.filter((status) => new RegExp(`(?<![a-z])${status}(?![a-z])`).test(String(cell)));
}

function wavesIn(cell) {
  return [...String(cell).matchAll(/\b(W0\.5|W[123]|never)\b/g)].map((match) => match[1]);
}

function validateCoverage() {
  const sections = { A: section("A"), B: section("B"), C: section("C") };
  for (const [name, text] of Object.entries(sections)) {
    if (!text) fail(`COVERAGE.md §${name} is missing`);
  }
  const resolve = (id) =>
    entries.some((entry) => entry.id === id || entry.coverage?.keelPluginId === id) ||
    (catalog.retiredIds || []).some((item) => item.id === id);

  const mapRows = (name, sectionText, mappingIndex, waveIndex, statusIndex) => {
    const rows = tableRows(sectionText);
    if (!rows.length) fail(`COVERAGE.md §${name} has no capability rows`);
    for (const cells of rows) {
      const at = `COVERAGE §${name} row ${JSON.stringify(cells[0])}`;
      const mapping = cells[mappingIndex] ?? "";
      const waveCell = cells[waveIndex] ?? "";
      const statusCell = cells[statusIndex] ?? "";
      const statuses = statusesIn(statusCell);
      if (!statuses.length) {
        fail(`${at}: status cell must name one of ${STATUSES.join("|")}`);
        continue;
      }
      for (const status of statuses) {
        if (status === "absent" && !/absent[（(].{4,}/.test(statusCell)) {
          fail(`${at}: absent needs a reason in the cell`);
        }
      }
      const waves = wavesIn(waveCell);
      if (!waves.length && !statuses.includes("absent")) {
        fail(`${at}: wave cell must name W0.5|W1|W2|W3|never`);
      }
      const ids = backtickedIds(mapping);
      const terminal = statuses.includes("absent") || statuses.includes("merged");
      if (name === "B" && /`keel\.bundles`/.test(mapping)) {
        // §B bundle rows map to keel.bundles by spec; nothing more to resolve.
      } else if (!ids.some(resolve)) {
        if (!terminal) fail(`${at}: mapping ${JSON.stringify(mapping)} does not resolve to a catalog keelPluginId`);
        if (terminal && !/absent[（(]|merged/.test(statusCell)) {
          fail(`${at}: an unresolvable mapping needs an explicit absent/merged disposition`);
        }
      }
      if (statuses.includes("delivered")) {
        for (const id of boldDeliveredIds(mapping)) {
          const target = entries.find((entry) => entry.coverage?.keelPluginId === id);
          if (!target) {
            fail(`${at}: delivered bold id ${id} has no catalog entry`);
          } else if (!["delivered", "partial"].includes(target.coverage.status)) {
            fail(`${at}: ${id} is marked delivered in COVERAGE but ${target.coverage.status} in the catalog`);
          }
        }
      }
    }
  };
  mapRows("A", sections.A, 2, 4, 5);
  mapRows("B", sections.B, 1, 2, 3);
  mapRows("C", sections.C, 2, 3, 4);

  // Every DSH group in the catalog must have a §A row (group tokens may be
  // combined like "identity/telemetry").
  const tokens = new Set();
  for (const cells of tableRows(sections.A)) {
    for (const token of String(cells[0]).split("/")) tokens.add(token.trim());
  }
  for (const entry of entries) {
    if (!entry.id.startsWith("dsh.")) continue;
    const group = entry.id.slice(4);
    if (!tokens.has(group)) fail(`catalog group ${entry.id} has no §A row in COVERAGE.md`);
  }
}

// --- installed packages ----------------------------------------------------
function validateInstalledPackages() {
  for (const dirent of fs.readdirSync(PLUGINS, { withFileTypes: true })) {
    if (!dirent.isDirectory() || dirent.name.startsWith(".")) continue;
    const dir = path.join(PLUGINS, dirent.name);
    const manifestPath = path.join(dir, "plugin.yaml");
    if (!fs.existsSync(manifestPath)) continue;
    const id = read(manifestPath).match(/^id:\s*(\S+)/m)?.[1];
    if (!id) {
      fail(`${path.relative(ROOT, manifestPath)}: no id`);
      continue;
    }
    const entry = entries.find((candidate) => candidate.coverage?.keelPluginId === id || candidate.id === id);
    if (!entry) {
      fail(`installed official plugin ${id} has no catalog entry`);
      continue;
    }
    if (!["delivered", "partial"].includes(entry.coverage.status)) {
      fail(`installed official plugin ${id} is ${entry.coverage.status} in the catalog, want delivered|partial`);
    }
    for (const file of ["extensibility.json", "EXTENSIBILITY.md"]) {
      if (!fs.existsSync(path.join(dir, file))) {
        fail(`installed official plugin ${id} is missing ${file}`);
      }
    }
  }
}

// --- generated section -----------------------------------------------------
function generatedSection() {
  const lines = [
    BEGIN,
    "## H. Catalog 机读真源（自动生成）",
    "",
    "> 本段由 `node scripts/plugin-catalog.mjs --write` 生成，`--check` 校验；",
    "> 禁止手工编辑。`plugins/keel.plugin-market/catalog.json` 是唯一机读真源。",
    "",
    "| 条目 | keel 插件 id | 状态 | 波次 | dshGroups |",
    "|---|---|---|---|---|",
  ];
  const sorted = [...entries].sort((left, right) => left.id.localeCompare(right.id));
  for (const entry of sorted) {
    const keelId = entry.coverage?.keelPluginId ? `\`${entry.coverage.keelPluginId}\`` : "—";
    const groups = (entry.dshGroups || []).length ? entry.dshGroups.map((id) => `\`${id}\``).join(", ") : "—";
    lines.push(`| \`${entry.id}\` | ${keelId} | ${entry.coverage?.status} | ${entry.wave} | ${groups} |`);
  }
  lines.push("", END);
  return lines.join("\n");
}

function currentGeneratedSection() {
  const start = coverage.indexOf(BEGIN);
  const end = coverage.indexOf(END);
  if (start === -1 || end === -1 || end < start) return null;
  return coverage.slice(start, end + END.length);
}

function writeGeneratedSection() {
  const next = generatedSection();
  const current = currentGeneratedSection();
  let updated;
  if (current) {
    updated = coverage.replace(current, next);
  } else {
    updated = `${coverage.trimEnd()}\n\n${next}\n`;
  }
  fs.writeFileSync(COVERAGE, updated);
}

// --- main ------------------------------------------------------------------
const write = process.argv.includes("--write");
const check = process.argv.includes("--check");
if (!write && !check) {
  console.error("usage: node scripts/plugin-catalog.mjs --check|--write");
  process.exit(2);
}

validateCatalog();
validateCoverage();
validateInstalledPackages();

if (errors.length) {
  console.error(`plugin-catalog: ${errors.length} problem(s)`);
  for (const message of errors) console.error(`  - ${message}`);
  process.exit(1);
}

if (write) {
  writeGeneratedSection();
  console.log(`plugin-catalog: regenerated COVERAGE.md §H (${entries.length} entries)`);
  process.exit(0);
}

const current = currentGeneratedSection();
if (!current) {
  fail("COVERAGE.md has no generated §H section; run: node scripts/plugin-catalog.mjs --write");
} else if (current !== generatedSection()) {
  fail("COVERAGE.md §H is stale; run: node scripts/plugin-catalog.mjs --write");
}
if (errors.length) {
  console.error(`plugin-catalog: ${errors.length} problem(s)`);
  for (const message of errors) console.error(`  - ${message}`);
  process.exit(1);
}
console.log(`plugin-catalog: OK (${entries.length} entries, §A/§B/§C mapped, §H current)`);
