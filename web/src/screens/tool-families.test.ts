// The tool-family derivation, pinned without rendering anything.
//
// `familyOf` is the renderer's only classification of a call — `ToolCallView`
// is frozen and carries no family — so the words it matches and the counts the
// summary line says them with are contract-adjacent facts: the TUI gate reads
// the summary verbatim ("1 个工具"), which is why this file runs as a plain
// unit test (no `open()`, no app) beside the screen it feeds.
import { expect, test } from "nsh/test";
import type { ToolCallView } from "../data/contract";
import {
  FAMILY_ICON,
  FAMILY_LABEL,
  FAMILY_WORDS,
  familyDetail,
  familyOf,
  groupToolCalls,
  rollupCalls,
  summarizeCalls,
  toolTone,
} from "./tool-families";

const call = (name: string, extra: Partial<ToolCallView> = {}): ToolCallView => ({
  toolCallId: `id-${name}`,
  name,
  ...extra,
});
const settled = (name: string): ToolCallView => call(name, { result: { ok: true } });
const broken = (name: string): ToolCallView => call(name, { error: "boom" });

test("familyOf: 每个家族认自己的词（大小写/分隔符无关）", () => {
  expect(familyOf("shell.exec")).toBe("terminal");
  expect(familyOf("Bash")).toBe("terminal");
  expect(familyOf("process.spawn")).toBe("terminal");
  expect(familyOf("fs.write")).toBe("changes");
  expect(familyOf("git.commit")).toBe("changes");
  expect(familyOf("deploy.prepare")).toBe("changes");
  expect(familyOf("file.read")).toBe("explore");
  expect(familyOf("web.search")).toBe("explore");
  expect(familyOf("totally.unknown")).toBe("other");
  expect(familyOf("")).toBe("other");
});

test("familyOf: 一族名字里出现多族词时，按 terminal → changes → explore 取先命中", () => {
  expect(familyOf("run.git.status")).toBe("terminal");
  expect(familyOf("edit.read.file")).toBe("changes");
});

test("familyOf: 词典与标签/图标对每个家族都有定义", () => {
  for (const family of ["explore", "terminal", "changes", "other"] as const) {
    expect(FAMILY_LABEL[family].length > 0).toBe(true);
    expect(FAMILY_ICON[family].length > 0).toBe(true);
  }
  expect(FAMILY_WORDS.terminal).toContain("shell");
  expect(FAMILY_WORDS.changes).toContain("deploy");
  expect(FAMILY_WORDS.explore).toContain("read");
});

test("toolTone: 错误优先，其次是结果，都没有=进行中", () => {
  expect(toolTone(call("a.read"))).toBe("running");
  expect(toolTone(settled("a.read"))).toBe("done");
  expect(toolTone(broken("a.read"))).toBe("failed");
  // 错误与结果同时在场：错误赢（一次调用不会既成功又失败）。
  expect(toolTone(call("a.read", { result: 1, error: "boom" }))).toBe("failed");
});

test("rollupCalls: 计数与名词——只有终端一族的组说「命令」", () => {
  const terminalOnly = rollupCalls([settled("shell.exec"), call("bash")]);
  expect(terminalOnly).toMatchObject({ total: 2, done: 1, failed: 0, running: 1, noun: "命令" });

  const mixed = rollupCalls([settled("shell.exec"), settled("file.read")]);
  expect(mixed).toMatchObject({ total: 2, done: 2, failed: 0, running: 0, noun: "工具" });

  expect(rollupCalls([])).toMatchObject({ total: 0, noun: "工具" });
});

test("summarizeCalls: 失败 > 未完成 > 全部完成 > 只有计数", () => {
  // 停在问句上的那一次（TUI 门读的字面串）：一个工具、还在等结果。
  expect(summarizeCalls([call("deploy.prepare")], true)).toBe("1 个工具");
  expect(summarizeCalls([settled("a.read")], false)).toBe("1 个工具，全部完成");
  expect(summarizeCalls([call("shell.exec"), call("bash")], false)).toBe("2 个命令，2 个未完成");
  expect(summarizeCalls([settled("shell.exec"), call("bash")], true)).toBe("2 个命令");
  expect(summarizeCalls([settled("shell.exec"), call("bash")], false)).toBe("2 个命令，1 个未完成");
  expect(summarizeCalls([broken("fs.write"), settled("file.read")], false)).toBe("2 个工具，1 个失败");
  expect(summarizeCalls([broken("shell.exec")], true)).toBe("1 个命令，1 个失败");
});

test("familyDetail: 失败 > 进行中 > 全部完成", () => {
  expect(familyDetail([settled("a.read"), broken("b.read")])).toBe("1 个失败");
  expect(familyDetail([settled("a.read"), call("b.read")])).toBe("1 个进行中");
  expect(familyDetail([settled("a.read"), settled("b.read")])).toBe("全部完成");
  expect(familyDetail([])).toBe("暂无结果");
});

test("groupToolCalls: 首现顺序成族，只有 ≥2 个成员的族成组，其余单行", () => {
  const items = groupToolCalls([
    settled("file.read"), // explore #1
    call("totally.unknown"), // other，独苗 → 单行
    settled("a.read"), // explore #2 → 与 file.read 成族
  ]);
  expect(items.length).toBe(2);
  expect(items[0]).toMatchObject({ kind: "family", family: "explore" });
  const first = items[0]!;
  expect(first.kind === "family" ? first.calls.map((entry) => entry.name) : []).toEqual(["file.read", "a.read"]);
  expect(items[1]).toMatchObject({ kind: "call", call: { name: "totally.unknown" } });

  // 族行只出现一次，位置在它首现的地方。
  const families = groupToolCalls([
    settled("shell.exec"),
    settled("file.read"),
    settled("bash"),
    settled("grep.tree"),
  ]);
  expect(families.map((item) => (item.kind === "family" ? item.family : item.call.name))).toEqual([
    "terminal",
    "explore",
  ]);

  expect(groupToolCalls([])).toEqual([]);
});
