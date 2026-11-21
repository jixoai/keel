// The composer's `$` completion and `/learn` command (change
// keel-harness-v2, W7 skills).
//
// One wake-up character, three groups: a skill (the library's installed
// skills), a workspace file (the open session's manifest) and a task (the
// session's cards). The groups share `$` on purpose: the reader does not have
// to remember three sigils, and the menu is one list with three headings. The
// groups are data the server already serves — GET /v1/settings/skills (fast
// read), GET /v1/sessions/{id}/files and /tasks — so the client never invents
// a path or a task title.
//
// `/learn` is NOT a second write path. It sends one message that asks the agent
// to call `skills.manage create` with what the session proved; the library's
// tool result is the only success signal. The composer never claims "learned"
// before a tool actually answered.
//
// The pure functions (`completionTrigger`, `completionGroups`,
// `insertCompletion`, `parseLearnCommand`, `buildLearnInstruction`) are
// exported so the TUI test can pin the behavior without a DOM.
import { Text, View } from "nsh";
import type { SessionFileView, SessionTaskView, SkillRecordView } from "../data/contract";
import { store } from "../data/store";
import { Button, Card, CardContent, CardHeader, color, lineHeight, space, typeScale, weight } from "../ui/kit";

/** Where the active `$` token starts and what has been typed after it. */
export interface CompletionTrigger {
  /** Index of the `$` in the draft. */
  start: number;
  /** Lower-case query after the `$` (may be empty). */
  query: string;
}

/**
 * The active completion token: the last `$` in the draft whose preceding
 * character is the start or whitespace, followed by no whitespace and no
 * second `$`. A `$` inside a word (a price, a shell variable) is not a wake-up.
 */
export function completionTrigger(draft: string): CompletionTrigger | null {
  const last = draft.lastIndexOf("$");
  if (last < 0) return null;
  if (last > 0) {
    const before = draft[last - 1] ?? "";
    if (!/\s/.test(before)) return null;
  }
  const after = draft.slice(last + 1);
  if (after.includes("$") || /\s/.test(after)) return null;
  // A bare number is a price, not a name: `$5`, `$5.00`. A path that starts
  // with digits keeps a slash (`2026/report.md`), so it stays a completion.
  if (/^\d+(\.\d+)?$/.test(after)) return null;
  return { start: last, query: after.toLowerCase() };
}

/** Replace the active token with `$insert` plus one space, keeping the rest. */
export function insertCompletion(draft: string, start: number, insert: string): string {
  const token = insert.startsWith("$") ? insert : `$${insert}`;
  return `${draft.slice(0, start)}${token} `;
}

export interface CompletionItem {
  id: string;
  label: string;
  detail: string;
  /** What choosing it writes after the `$`. */
  insert: string;
}

export interface CompletionGroup {
  id: "skills" | "files" | "tasks";
  label: string;
  items: CompletionItem[];
}

/** Filter one group's rows by the query; an empty query keeps everything. */
const matching = (items: CompletionItem[], query: string): CompletionItem[] => {
  if (!query) return items;
  return items.filter((item) => {
    const haystack = `${item.label} ${item.detail}`.toLowerCase();
    return haystack.includes(query);
  });
};

/**
 * Build the three groups. The order is the fixed reading order (skills, files,
 * tasks), each group is filtered by the same query, and a group with no match
 * is omitted rather than drawn empty.
 */
export function completionGroups(
  query: string,
  skills: SkillRecordView[],
  files: SessionFileView[],
  tasks: SessionTaskView[],
  limit = 6,
): CompletionGroup[] {
  const skillItems: CompletionItem[] = skills.map((skill) => ({
    id: `skill:${skill.name}`,
    label: skill.name,
    detail: skill.description ?? skill.trust ?? "",
    insert: skill.name,
  }));
  const fileItems: CompletionItem[] = files.map((file, index) => ({
    id: `file:${index}`,
    label: file.path,
    detail: `${file.size} 字节`,
    insert: file.path,
  }));
  const taskItems: CompletionItem[] = tasks.map((task) => ({
    id: `task:${task.taskId}`,
    label: task.title,
    detail: task.status,
    insert: task.title,
  }));
  const groups: CompletionGroup[] = [
    { id: "skills", label: "技能", items: matching(skillItems, query).slice(0, limit) },
    { id: "files", label: "文件", items: matching(fileItems, query).slice(0, limit) },
    { id: "tasks", label: "任务", items: matching(taskItems, query).slice(0, limit) },
  ];
  return groups.filter((group) => group.items.length > 0);
}

/** The `/learn` command's payload, or null when the text is not that command. */
export function parseLearnCommand(text: string): { prompt: string } | null {
  const trimmed = text.trim();
  if (trimmed === "/learn") return { prompt: "" };
  if (!trimmed.startsWith("/learn ") && !trimmed.startsWith("/learn\n")) return null;
  return { prompt: trimmed.slice("/learn".length).trim() };
}

/**
 * The message `/learn` sends. It asks the model for the skill and names the
 * exact tool action; it also says what not to do (claim success before a tool
 * answered, invent an install). The model's tool result is the visible proof.
 */
export function buildLearnInstruction(prompt: string): string {
  const topic = prompt.trim();
  return [
    "/learn：把本次会话里已经验证有效的做法沉淀为一个技能。",
    "",
    "请调用 skills 工具的 manage 动作（op = create）：",
    "- name：kebab-case，短且稳定；",
    "- description：一行说明“什么时候该用这个技能”；",
    "- body：可复用的步骤、判断依据和已知坑，写成给下一次的自己的说明。",
    "如果会话里有可复用的脚本/清单，用 manage 的 write_file 放到技能资源里；",
    "不要在 skills 里执行脚本——运行请用 bash 工具。",
    "",
    "不要声称已经保存成功，除非 skills 工具的结果明确返回成功；失败就原样说明原因，",
    "不要把失败包装成“已学习”。",
    topic ? `\n用户补充：${topic}` : "",
  ]
    .join("\n")
    .trim();
}

/** The menu's rows, drawn only while a `$` token is active. */
export function CompletionMenu(props: { query: string; onPick: (insert: string) => void }) {
  const groups = () =>
    completionGroups(props.query, store.completionSkills(), store.completionFiles(), store.completionTasks());
  const total = () => groups().reduce((count, group) => count + group.items.length, 0);
  return (
    <Card size="sm" testID="composer-completion" accessibilityLabel="$ 补全">
      <CardHeader
        title="$ 补全"
        description="技能、文件、任务共用同一个唤醒符；选择后写入输入框，不直接发送。"
      />
      <CardContent>
        <If when={store.completionLoading() && total() === 0}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>正在读取补全项…</Text>
        </If>
        <If when={!!store.completionError()}>
          <Text testID="composer-completion-error" style={{ fontSize: typeScale.sm, color: color.warning }}>
            {store.completionError() ?? ""}
          </Text>
        </If>
        <If when={!store.completionLoading() && total() === 0 && !store.completionError()}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            没有匹配的技能、文件或任务。
          </Text>
        </If>
        <View style={{ gap: space.md }}>
          {groups().map((group) => (
            <View testID={`composer-completion-group-${group.id}`} style={{ gap: space.xs }}>
              <Text
                style={{
                  fontSize: typeScale.xs,
                  fontWeight: weight.semibold,
                  color: color.mutedForeground,
                }}
              >
                {group.label}
              </Text>
              {group.items.map((item) => (
                <View
                  testID={`composer-completion-row-${item.id}`}
                  style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}
                >
                  <Button
                    size="sm"
                    variant="ghost"
                    label={item.label}
                    accessibilityLabel={`插入 ${group.label} ${item.label}`}
                    onPress={() => props.onPick(item.insert)}
                  />
                  <Text
                    style={{
                      flex: 1,
                      fontSize: typeScale.xs,
                      lineHeight: Math.round(typeScale.xs * lineHeight.normal),
                      color: color.mutedForeground,
                      numberOfLines: 1,
                    }}
                  >
                    {item.detail}
                  </Text>
                </View>
              ))}
            </View>
          ))}
        </View>
      </CardContent>
    </Card>
  );
}
