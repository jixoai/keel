#!/usr/bin/env node
// TUI/headless fallback for the super-agent board acceptance pass.
//
// Usage:
//   node scripts/superagent-acceptance/tui.mjs --base http://127.0.0.1:PORT --tenant <uuid>
//
// It reads the same two product API routes the browser harness reads and
// prints a plain-text board/inbox. No dependencies, no writes, no secrets.

const args = process.argv.slice(2);
function arg(name, fallback) {
  const index = args.indexOf("--" + name);
  return index >= 0 && args[index + 1] ? args[index + 1] : fallback;
}

const base = arg("base", "");
const tenant = arg("tenant", "");
if (!base || !tenant) {
  console.error("usage: node tui.mjs --base http://127.0.0.1:PORT --tenant <uuid>");
  process.exit(2);
}

const statusLabels = {
  open: "待接单", active: "执行中", blocked: "待人工", done: "已完成",
  failed: "失败", cancelling: "停止中", cancelled: "已停止",
};
const label = (status) => statusLabels[status] || status || "未知";
const shortID = (value) => (value ? String(value).slice(0, 8) : "—");
const pad = (value, width) => String(value == null ? "" : value).padEnd(width).slice(0, width);

async function get(path) {
  const response = await fetch(base + path, {
    headers: { "X-Tenant-ID": tenant },
  });
  const text = await response.text();
  let body = null;
  try { body = text ? JSON.parse(text) : null; } catch (_) { body = null; }
  if (!response.ok) {
    throw new Error("HTTP " + response.status + ": " + (body && (body.error || body.detail) || text || ""));
  }
  return body;
}

try {
  const [board, inbox] = await Promise.all([
    get("/v1/superagent/tickets"),
    get("/v1/superagent/attention"),
  ]);
  const lines = [];
  lines.push("KEEL SUPER-AGENT / TUI FALLBACK");
  lines.push("tenant   " + tenant);
  lines.push("mode     " + (board.modeId || "superagent"));
  lines.push("updated  " + new Date().toISOString());
  lines.push("");
  lines.push("TICKETS (" + (board.tickets || []).length + ")");
  lines.push(pad("STATUS", 10) + pad("ID", 10) + pad("TITLE", 36) + pad("ASSIGNMENT", 18) + "OPEN");
  lines.push("-".repeat(96));
  for (const ticket of board.tickets || []) {
    const assignment = ticket.activeAssignment
      ? (ticket.activeAssignment.botId || "worker") + "/" + shortID(ticket.activeAssignment.sessionId)
      : (["done", "failed", "cancelled"].includes(ticket.status) ? "ended" : "unassigned");
    lines.push(pad(label(ticket.status), 10) + pad(shortID(ticket.id), 10) + pad(ticket.title || "", 36) + pad(assignment, 18) + (ticket.openAttentionCount || 0));
  }
  lines.push("");
  lines.push("BOARD COUNTS " + JSON.stringify(board.counts || {}));
  lines.push("");
  lines.push("ATTENTION INBOX (" + (inbox.items || []).length + " open)");
  lines.push("-".repeat(96));
  if (!(inbox.items || []).length) lines.push("(empty)");
  for (const item of inbox.items || []) {
    lines.push("[" + (item.status || "open") + "] " + (item.kind || "attention") + " request=" + (item.requestId || "—"));
    lines.push("  prompt: " + (item.prompt || ""));
    lines.push("  ticket: " + (item.ticketTitle || "—") + "  thread=" + (item.thread && item.thread.id || "—"));
    lines.push("  options: " + ((item.options || []).map((o) => o.id + (o.label ? "/" + o.label : "")).join("  ") || "(none)"));
    lines.push("  deadline: " + (item.deadline || "—"));
  }
  console.log(lines.join("\n"));
} catch (error) {
  console.error("tui: " + error.message);
  process.exit(1);
}
