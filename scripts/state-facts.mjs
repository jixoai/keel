#!/usr/bin/env node
// Static check for the state-machine registry:
//   every row of a `状态 | 权威事实` table in state-machines.md must name an
//   authority fact; a state that only exists in memory fails the gate.
// Prints the state -> fact mapping (machine, state, authority) so the G1
// evidence has the table the acceptance gate asks for.
//
// Usage: node scripts/state-facts.mjs [--json]
import { readFileSync } from "node:fs";

const doc = new URL("../openspec/changes/keel-harness-v2/state-machines.md", import.meta.url);
const lines = readFileSync(doc, "utf8").split("\n");

const SEPARATOR = /^:?-{2,}:?$/;
const rows = [];
const problems = [];
let machine = "";
let table = null;

const clean = (cell) => String(cell ?? "").replaceAll("`", "").replaceAll("**", "").trim();
const machineOf = (line) => {
  const match = line.match(/^#{2,3}\s+([A-Z]\d+\.\s+.+?)\s*$/);
  return match ? match[1] : "";
};

for (const line of lines) {
  const named = machineOf(line);
  if (named) {
    machine = named;
    table = null;
    continue;
  }
  if (/^#{2,3}\s+/.test(line)) {
    machine = "";
    table = null;
    continue;
  }
  const trimmed = line.trim();
  if (!trimmed.startsWith("|")) {
    table = null;
    continue;
  }
  const cells = trimmed.slice(1, -1).split("|").map((cell) => cell.trim());
  if (!table) {
    if (cells.map(clean).includes("状态") && cells.map(clean).includes("权威事实")) {
      table = cells.map(clean);
    }
    continue;
  }
  if (cells.every((cell) => SEPARATOR.test(clean(cell)))) continue;
  const state = clean(cells[table.indexOf("状态")]);
  const fact = clean(cells[table.indexOf("权威事实")]);
  if (!state) continue;
  if (!fact) {
    problems.push(`${machine}: state ${state} has no authority fact`);
    continue;
  }
  if (/^(仅)?内存(态)?$/.test(fact) || fact.includes("仅内存")) {
    problems.push(`${machine}: state ${state} is memory-only (${fact})`);
    continue;
  }
  rows.push({ machine, state, fact });
}

const machines = new Set(rows.map((row) => row.machine).filter(Boolean));
if (rows.length < 20) problems.push(`only ${rows.length} state rows parsed; the document changed shape?`);
if (machines.size < 5) problems.push(`only ${machines.size} machines parsed; the document changed shape?`);

if (process.argv.includes("--json")) {
  process.stdout.write(JSON.stringify({ machines: [...machines], rows, problems }, null, 2) + "\n");
} else {
  console.log(`state -> fact mapping (${machines.size} machines, ${rows.length} states)`);
  for (const row of rows) console.log(`${row.machine}\t${row.state}\t${row.fact}`);
}
if (problems.length) {
  for (const problem of problems) console.error(`state-facts: ${problem}`);
  process.exit(1);
}
