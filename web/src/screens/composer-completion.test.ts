// The `$` completion and `/learn` rules, pinned without rendering anything.
//
// `completionTrigger` is the one parser of the wake-up character and
// `completionGroups` is the one grouper; the TUI gate
// (composer-completion.test.tsx) reads the same functions through the real
// composer. Keeping the word boundaries here means a `$` inside a price or a
// shell variable is never a completion, and `/learning` is never `/learn`.
import { expect, test } from "nsh/test";
import type { SessionFileView, SessionTaskView, SkillRecordView } from "../data/contract";
import {
  buildLearnInstruction,
  completionGroups,
  completionTrigger,
  insertCompletion,
  parseLearnCommand,
} from "./composer-completion";

const skill = (name: string, description = ""): SkillRecordView => ({
  name,
  description,
  trust: "local",
  currentHash: "a".repeat(64),
});

const file = (path: string, size = 10): SessionFileView => ({ path, size, updatedSeq: 1 });
const task = (title: string, status = "open"): SessionTaskView => ({
  taskId: `task-${title}`,
  title,
  status,
});

const groups = (query: string) =>
  completionGroups(query, [skill("deploy-flow", "发布检查"), skill("pdf")], [file("src/composer.tsx")], [task("回滚演练")]);

test("completionTrigger: $ 只在词首唤醒，词中/含空白的不是补全", () => {
  expect(completionTrigger("$")).toEqual({ start: 0, query: "" });
  expect(completionTrigger("请看 $dep")).toEqual({ start: 3, query: "dep" });
  expect(completionTrigger("第一行\n$PDF")).toEqual({ start: 4, query: "pdf" });
  // A price / a shell variable is not a completion.
  expect(completionTrigger("价格 $5.00")).toBeNull();
  expect(completionTrigger("a$b")).toBeNull();
  // Once the token has a space (or a second $), the menu is closed.
  expect(completionTrigger("$dep 继续")).toBeNull();
  expect(completionTrigger("$a$b")).toBeNull();
  expect(completionTrigger("没有唤醒符")).toBeNull();
});

test("insertCompletion: 替换活动 token，保留前后文本，不发送", () => {
  expect(insertCompletion("请看 $dep", 3, "deploy-flow")).toBe("请看 $deploy-flow ");
  expect(insertCompletion("$", 0, "$pdf")).toBe("$pdf ");
  expect(insertCompletion("前 $x 后", 2, "file.md")).toBe("前 $file.md ");
});

test("completionGroups: 固定顺序（技能/文件/任务），同一 query 过滤，空组不画", () => {
  expect(groups("").map((group) => group.id)).toEqual(["skills", "files", "tasks"]);
  expect(groups("dep").map((group) => group.id)).toEqual(["skills"]);
  expect(groups("dep")[0]?.items[0]?.label).toBe("deploy-flow");
  // The query matches the detail too (the Chinese description).
  expect(groups("发布")[0]?.items[0]?.label).toBe("deploy-flow");
  expect(groups("src/").map((group) => group.id)).toEqual(["files"]);
  expect(groups("回滚").map((group) => group.id)).toEqual(["tasks"]);
  // Nothing matched: no empty headings.
  expect(groups("zzz")).toEqual([]);
});

test("completionGroups: 每组有上限，行里没有伪造的 id", () => {
  const many = Array.from({ length: 12 }, (_, index) => skill(`skill-${index}`));
  const limited = completionGroups("", many, [], [], 6);
  expect(limited[0]?.items.length).toBe(6);
  expect(limited[0]?.items.every((item) => !!item.id && !!item.label)).toBe(true);
});

test("parseLearnCommand: 只认 /learn 命令本身，/learning 不是", () => {
  expect(parseLearnCommand("/learn")).toEqual({ prompt: "" });
  expect(parseLearnCommand("/learn 发布流程")).toEqual({ prompt: "发布流程" });
  expect(parseLearnCommand("/learn\n发布流程")).toEqual({ prompt: "发布流程" });
  expect(parseLearnCommand("/learning 发布流程")).toBeNull();
  expect(parseLearnCommand("请 /learn 发布流程")).toBeNull();
  expect(parseLearnCommand("/learn-more")).toBeNull();
});

test("buildLearnInstruction: 走 skills.manage create，脚本走 bash，禁止未证实的成功", () => {
  const text = buildLearnInstruction("发布流程");
  expect(text).toContain("skills");
  expect(text).toContain("manage");
  expect(text).toContain("create");
  expect(text).toContain("bash");
  expect(text).toContain("不要声称已经保存成功");
  expect(text).toContain("发布流程");
  // It must not instruct the model to write the library by hand or to run a
  // script from inside the skill.
  expect(text).not.toContain("直接写入技能库");
});
