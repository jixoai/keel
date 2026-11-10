// Tool families (C9) — the display side of the frozen `ToolCallView` contract.
//
// `ToolCallView` (packages/chat-surface/src/types.ts, frozen) carries the call's
// own facts only — `toolCallId / name / args / result / error / startedAt /
// finishedAt`: a call has no `family` and no `kind` field, and the contract is
// not the renderer's to change. So the family is *derived from the tool's own
// name* here, and so are the counts the summary line and the family badges are
// made of (the TUI gate reads the summary line verbatim: "1 个工具").
//
// Everything in this module is pure and view-free on purpose: `nsh test` runs
// `tool-families.test.ts` without mounting anything, and the renderer
// (`screens/messages.tsx`) is the only other reader.
import type { ToolCallView } from "../data/contract";
import type { IconName } from "../ui/icons";

/** The four buckets a tool name falls into (C9). */
export type ToolFamily = "explore" | "terminal" | "changes" | "other";

/** How a call is reading right now: no result yet, a result, or an error. */
export type ToolTone = "running" | "done" | "failed";

/** The words a tool name carries its family in (matched on `a.b_c-d` tokens). */
export const FAMILY_WORDS: Record<Exclude<ToolFamily, "other">, string[]> = {
  terminal: ["shell", "bash", "sh", "zsh", "exec", "run", "terminal", "command", "cmd", "process", "spawn"],
  changes: [
    "edit", "write", "patch", "apply", "replace", "create", "delete", "remove", "mkdir",
    "move", "rename", "git", "commit", "diff", "build", "test", "install", "deploy", "publish",
  ],
  explore: [
    "read", "list", "search", "grep", "glob", "find", "open", "load", "fetch", "view", "show",
    "cat", "ls", "tree", "stat", "head", "tail", "inspect", "check", "health", "status",
    "context", "workspace", "calendar", "transcript", "web", "http",
  ],
};

/** The family of one call, from its name alone (`deploy.prepare` → changes). A
 * name carrying words of several families takes the first that matches, in the
 * order terminal → changes → explore. */
export function familyOf(name: string): ToolFamily {
  const tokens = name.toLowerCase().split(/[^a-z0-9]+/).filter(Boolean);
  const has = (words: string[]) => tokens.some((token) => words.includes(token));
  if (has(FAMILY_WORDS.terminal)) return "terminal";
  if (has(FAMILY_WORDS.changes)) return "changes";
  if (has(FAMILY_WORDS.explore)) return "explore";
  return "other";
}

/** The family's word, as the group's row and its accessibility name say it. */
export const FAMILY_LABEL: Record<ToolFamily, string> = {
  explore: "探索",
  terminal: "终端",
  changes: "变更",
  other: "工具",
};

/** The family's glyph (the kit's one monochrome icon language). */
export const FAMILY_ICON: Record<ToolFamily, IconName> = {
  explore: "search",
  terminal: "terminal",
  changes: "file",
  other: "tool",
};

/** A call's state, read the one way: an error wins, a result settles it. */
export function toolTone(call: ToolCallView): ToolTone {
  if (call.error) return "failed";
  return call.result !== undefined ? "done" : "running";
}

/** What a set of calls adds up to — the facts every summary line is derived
 * from, so the counts on screen never come from a second reading of the list. */
export interface ToolRollup {
  total: number;
  done: number;
  failed: number;
  running: number;
  /** The noun the count is said with: a group that is terminal and nothing
   * else is counting commands. */
  noun: "命令" | "工具";
}

export function rollupCalls(calls: readonly ToolCallView[]): ToolRollup {
  let done = 0;
  let failed = 0;
  for (const call of calls) {
    const tone = toolTone(call);
    if (tone === "done") done += 1;
    else if (tone === "failed") failed += 1;
  }
  const families = new Set(calls.map((call) => familyOf(call.name)));
  return {
    total: calls.length,
    done,
    failed,
    running: calls.length - done - failed,
    noun: families.size === 1 && families.has("terminal") ? "命令" : "工具",
  };
}

/** The line every tool block is read by: how many, how many failed. */
export function summarizeCalls(calls: readonly ToolCallView[], running: boolean): string {
  const { total, done, failed, running: pending, noun } = rollupCalls(calls);
  if (failed > 0) return `${total} 个${noun}，${failed} 个失败`;
  if (!running && pending > 0) return `${total} 个${noun}，${pending} 个未完成`;
  if (total > 0 && done === total) return `${total} 个${noun}，全部完成`;
  return `${total} 个${noun}`;
}

/** A family's own one-liner, for the badge on its row. */
export function familyDetail(calls: readonly ToolCallView[]): string {
  const { done, failed, running } = rollupCalls(calls);
  if (failed > 0) return `${failed} 个失败`;
  if (running > 0) return `${running} 个进行中`;
  return done > 0 ? "全部完成" : "暂无结果";
}

/** One row of a group's detail: a family with its members, or a lone call. */
export type ToolGroupItem =
  | { kind: "family"; family: ToolFamily; calls: ToolCallView[] }
  | { kind: "call"; call: ToolCallView };

/** Families in first-appearance order, only those with two or more members;
 * everything else stays a standalone row (C9). */
export function groupToolCalls(calls: readonly ToolCallView[]): ToolGroupItem[] {
  const groups = new Map<ToolFamily, ToolCallView[]>();
  for (const call of calls) {
    const family = familyOf(call.name);
    const bucket = groups.get(family);
    if (bucket) bucket.push(call);
    else groups.set(family, [call]);
  }
  const emitted = new Set<ToolFamily>();
  const list: ToolGroupItem[] = [];
  for (const call of calls) {
    const family = familyOf(call.name);
    const bucket = groups.get(family) ?? [];
    if (bucket.length < 2) {
      list.push({ kind: "call", call });
      continue;
    }
    if (emitted.has(family)) continue;
    emitted.add(family);
    list.push({ kind: "family", family, calls: bucket });
  }
  return list;
}
