// The command catalog's client rules, pinned without rendering.
//
// The parser keeps the server's order, the draft parser accepts only a first
// token `/name`, the form subset is closed (anything else is a JSON field), and
// every unavailability reason has one readable sentence. The TUI gates read the
// same functions through the real Composer and palette.
import { expect, test } from "nsh/test";
import type { CommandView } from "./contract";
import {
  applyTargetAvailability,
  buildCommandArgs,
  commandAvailabilityText,
  commandDraft,
  commandFieldErrors,
  commandFieldName,
  commandFields,
  commandGroups,
  commandNeedsForm,
  commandResultSummary,
  commandRowLabel,
  commandStatusLabel,
  commandStatusTone,
  commandUsesFreeText,
  initialCommandValues,
  matchingCommands,
  parseCommandCatalog,
  parseCommand,
  cleanDisplayText,
  commandErrorText,
  localizeDecision,
} from "./commands";

const command = (patch: Partial<CommandView> & { name: string }): CommandView => ({
  source: "plugin",
  summary: "summary",
  argsSchema: { type: "object" },
  danger: "write",
  idempotent: false,
  targets: ["web", "native", "tui"],
  requiredPermissions: ["session.write"],
  available: true,
  ...patch,
});

const CATALOG = {
  commands: [
    command({ name: "compact", source: "builtin", summary: "压缩当前会话", danger: "write" }),
    command({
      name: "vendor.search:reindex",
      summary: "重建索引",
      argsSchema: { type: "object", properties: { path: { type: "string" } }, required: ["path"] },
    }),
    command({ name: "vendor.search:purge", summary: "清空索引", danger: "destructive", idempotent: false }),
    command({ name: "skill:deploy-flow", source: "skill", summary: "Load skill deploy-flow", danger: "read" }),
  ],
  truncated: false,
  generation: 7,
};

test("parseCommandCatalog: 保持服务端顺序，缺省安全，未知来源不隐藏", () => {
  const parsed = parseCommandCatalog(CATALOG);
  expect(parsed.commands.map((row) => row.name)).toEqual([
    "compact",
    "vendor.search:reindex",
    "vendor.search:purge",
    "skill:deploy-flow",
  ]);
  expect(parsed.generation).toBe(7);
  expect(parsed.truncated).toBe(false);
  expect(parsed.commands[0]!.targets).toEqual(["web", "native", "tui"]);
  expect(parsed.commands[1]!.available).toBe(true);
  // A malformed entry without a name is dropped; unknown values survive.
  const unknown = parseCommandCatalog({
    commands: [{ name: "" }, { name: "x:y", source: "future", available: true }],
  });
  expect(unknown.commands).toHaveLength(1);
  expect(unknown.commands[0]!.source).toBe("future");
  expect(parseCommand(null)).toBeNull();
});

test("applyTargetAvailability: 只降级不升级，也不重排", () => {
  const parsed = parseCommandCatalog({
    commands: [
      command({ name: "a", targets: ["web", "native"] }),
      command({ name: "b", targets: ["tui"] }),
      command({ name: "c", available: false, reason: "plugin_failed" }),
    ],
  });
  const tui = applyTargetAvailability(parsed, "tui");
  expect(tui.commands.map((row) => row.name)).toEqual(["a", "b", "c"]);
  expect(tui.commands.map((row) => row.available)).toEqual([false, true, false]);
  expect(tui.commands[0]!.reason).toBe("target_unsupported");
  expect(tui.commands[2]!.reason).toBe("plugin_failed");
});

test("commandDraft: 只有首 token `/` 是命令；未知名字/正文里的斜杠不是", () => {
  const rows = parseCommandCatalog(CATALOG).commands;
  expect(commandDraft("/", rows)).toEqual({ kind: "menu", query: "" });
  expect(commandDraft("/re", rows)).toEqual({ kind: "menu", query: "re" });
  const matched = commandDraft("/vendor.search:reindex docs", rows);
  expect(matched?.kind).toBe("command");
  if (matched?.kind === "command") {
    expect(matched.command.name).toBe("vendor.search:reindex");
    expect(matched.rest).toBe("docs");
  }
  expect(commandDraft("/known-only-in-menu", rows)).toEqual({ kind: "menu", query: "known-only-in-menu" });
  expect(commandDraft("/learning 发布", rows)).toBeNull();
  expect(commandDraft("请看 /compact", rows)).toBeNull();
  expect(commandDraft("普通消息", rows)).toBeNull();
});

test("matchingCommands/commandGroups: 同一顺序，按来源分组，空组不画", () => {
  const rows = parseCommandCatalog(CATALOG).commands;
  expect(matchingCommands(rows, "").map((row) => row.name)).toEqual(rows.map((row) => row.name));
  expect(matchingCommands(rows, "PURGE").map((row) => row.name)).toEqual(["vendor.search:purge"]);
  expect(matchingCommands(rows, "索引").map((row) => row.name)).toEqual([
    "vendor.search:reindex",
    "vendor.search:purge",
  ]);
  const groups = commandGroups(rows);
  expect(groups.map((group) => group.id)).toEqual(["builtin", "plugin", "skill"]);
  expect(groups.flatMap((group) => group.commands.map((row) => row.name))).toEqual(rows.map((row) => row.name));
  const unknown = commandGroups([command({ name: "x:y", source: "future" })]);
  expect(unknown.map((group) => group.label)).toEqual(["其他来源"]);
});

test("可用性/状态文案：每种稳定原因都有一句话", () => {
  expect(commandAvailabilityText(command({ name: "a", available: false, reason: "plugin_pending" }))).toContain("装配");
  expect(commandAvailabilityText(command({ name: "a", available: false, reason: "plugin_failed" }))).toContain("失败");
  expect(
    commandAvailabilityText(
      command({ name: "a", available: false, reason: "missing_permissions", missingPermissions: ["plugin.invoke"] }),
    ),
  ).toContain("plugin.invoke");
  expect(
    commandAvailabilityText(command({ name: "a", available: false, reason: "target_unsupported", targets: ["web"] })),
  ).toContain("web");
  expect(commandAvailabilityText(command({ name: "a", available: false, reason: "not_wired" }))).toContain("未接通");
  expect(commandAvailabilityText(command({ name: "a" }))).toBe("");
  expect(commandRowLabel(command({ name: "a", available: false, reason: "not_wired" }))).toContain("未接通");
  expect(commandStatusLabel("ok")).toBe("成功");
  expect(commandStatusLabel("refused")).toBe("已拒绝");
  expect(commandStatusLabel("timed_out")).toBe("审批超时");
  expect(commandStatusLabel("pending_decision")).toBe("等待审批");
  expect(commandStatusTone("error")).toBe("destructive");
  expect(commandStatusTone("ok")).toBe("success");
});

test("commandFields: 受控子集（字符串/数字/布尔/枚举/数组/JSON）与默认值", () => {
  const fields = commandFields(
    command({
      name: "vendor.search:reindex",
      argsSchema: {
        type: "object",
        required: ["path"],
        properties: {
          path: { type: "string", minLength: 1 },
          depth: { type: "integer", minimum: 1, maximum: 9, default: 3 },
          dryRun: { type: "boolean", default: false },
          mode: { type: "string", enum: ["fast", "full"] },
          tags: { type: "array", items: { type: "string" } },
          nested: { type: "object", properties: { a: { type: "string" } } },
        },
      },
    }),
  );
  expect(fields.map((field) => [field.name, field.kind, field.required])).toEqual([
    ["path", "string", true],
    ["depth", "integer", false],
    ["dryRun", "boolean", false],
    ["mode", "enum", false],
    ["tags", "array", false],
    ["nested", "json", false],
  ]);
  expect(fields[3]!.options?.map((option) => option.value)).toEqual(["fast", "full"]);
  expect(fields[1]!.defaultValue).toBe("3");
  expect(fields[4]!.itemsKind).toBe("string");
});

test("buildCommandArgs: 空可选项不发送，必填/类型/范围是字段级错误", () => {
  const fields = commandFields(
    command({
      name: "vendor.search:reindex",
      argsSchema: {
        type: "object",
        required: ["path", "text"],
        properties: {
          path: { type: "string", minLength: 2, maxLength: 5 },
          text: { type: "string" },
          depth: { type: "integer", minimum: 1 },
          dryRun: { type: "boolean" },
          mode: { type: "string", enum: ["fast", "full"] },
          tags: { type: "array", items: { type: "string" } },
        },
      },
    }),
  );
  const values = initialCommandValues(fields);
  expect(values.dryRun).toBe(false);

  const bad = buildCommandArgs(fields, { ...values, path: "a", text: "", depth: "0", mode: "nope" });
  expect(bad.errors.path).toBe("至少 2 个字符");
  expect(bad.errors.text).toBe("必填");
  expect(bad.errors.depth).toBe("不得小于 1");
  expect(bad.errors.mode).toBe("不在允许的取值内");

  const good = buildCommandArgs(fields, {
    path: "docs",
    text: "hello",
    depth: "2",
    dryRun: true,
    mode: "fast",
    tags: "a\n\nb\n",
  });
  expect(good.errors).toEqual({});
  expect(good.args).toEqual({
    path: "docs",
    text: "hello",
    depth: 2,
    dryRun: true,
    mode: "fast",
    tags: ["a", "b"],
  });
});

test("buildCommandArgs: 数组项类型与原始 JSON 字段", () => {
  const fields = commandFields(
    command({
      name: "x:y",
      argsSchema: {
        type: "object",
        properties: {
          nums: { type: "array", items: { type: "integer" } },
          doc: { type: "object" },
        },
      },
    }),
  );
  const bad = buildCommandArgs(fields, { nums: "1\nx", doc: "{oops" });
  expect(bad.errors.nums).toBe("每项必须是整数");
  expect(bad.errors.doc).toBe("必须是合法 JSON");
  const good = buildCommandArgs(fields, { nums: "1\n2", doc: '{"a":1}' });
  expect(good.args).toEqual({ nums: [1, 2], doc: { a: 1 } });
});

test("buildCommandArgs: enum 保留 schema 的原始类型（数字/布尔）", () => {
  const fields = commandFields(
    command({
      name: "x:y",
      argsSchema: {
        type: "object",
        properties: {
          count: { type: "integer", enum: [1, 2] },
          flag: { type: "boolean", enum: [true, false] },
        },
      },
    }),
  );
  const built = buildCommandArgs(fields, { count: "2", flag: "true" });
  expect(built.errors).toEqual({});
  expect(built.args).toEqual({ count: 2, flag: true });
});

test("自由文本映射：只有单个 text 字符串属性时把剩余文本当 args.text", () => {
  const textOnly = command({
    name: "vendor.notes:add",
    argsSchema: { type: "object", properties: { text: { type: "string" } } },
  });
  expect(commandUsesFreeText(textOnly)).toBe(true);
  expect(commandNeedsForm(textOnly)).toBe(false);
  const twoFields = command({
    name: "vendor.notes:add2",
    argsSchema: { type: "object", properties: { text: { type: "string" }, tag: { type: "string" } } },
  });
  expect(commandUsesFreeText(twoFields)).toBe(false);
  expect(commandNeedsForm(twoFields)).toBe(true);
  expect(commandNeedsForm(command({ name: "compact" }))).toBe(false);
});

test("控制字符/ANSI：目录与结果展示前净化，不伪造终端行", () => {
  const parsed = parseCommandCatalog({
    commands: [
      command({ name: "vendor.one:run", summary: "\u001b[31mRed\u001b[0m\u0007 summary" }),
      command({ name: "bad\u0007name" }),
    ],
    warnings: ["skills\u001b[2Junavailable"],
  });
  expect(parsed.commands).toHaveLength(1);
  expect(parsed.commands[0]!.summary).toBe("Red summary");
  expect(parsed.warnings).toEqual(["skillsunavailable"]);
  expect(commandResultSummary("\u001b[2Jresult\u0000ok")).toBe("result ok");
  expect(cleanDisplayText("a\nb")).toBe("a b");
  expect(cleanDisplayText("a\nb", true)).toBe("a\nb");
});

test("破坏性命令审批与错误文案中文化，参数预览保留", () => {
  const localized = localizeDecision({
    decisionId: "d1",
    seq: 1,
    question:
      'Run the destructive command /e2e.commands:purge? It is declared danger=destructive; approving it will execute the command once.\nArguments: {"confirm":true}',
    options: [
      { optionId: "allow", label: "Run" },
      { optionId: "deny", label: "Cancel" },
    ],
    status: "open",
  });
  expect(localized.question).toContain("是否执行破坏性命令 /e2e.commands:purge");
  expect(localized.question).toContain('参数：{"confirm":true}');
  expect(localized.options.map((option) => option.label)).toEqual(["执行", "取消"]);
  // A non-command decision keeps the server's question and labels.
  const other = localizeDecision({
    decisionId: "d2",
    seq: 2,
    question: "继续导出吗？",
    options: [{ optionId: "yes", label: "继续" }],
    status: "open",
  });
  expect(other.question).toBe("继续导出吗？");
  expect(commandErrorText("decision_refused", "the destructive command was refused")).toBe(
    "审批被拒绝，命令没有执行。",
  );
  expect(commandErrorText("custom", "custom failure")).toBe("custom failure");
});

test("服务端 400 字段路径映射到表单字段", () => {
  expect(commandFieldName("args.path")).toBe("path");
  expect(commandFieldName("args.tags[0]")).toBe("tags");
  expect(commandFieldName("args.nested.a")).toBe("nested");
  expect(commandFieldName("args")).toBe("");
  expect(commandFieldErrors([{ field: "args.path", message: "is required" }])).toEqual({
    path: "is required",
  });
});

test("commandResultSummary: 有界、插件信封取结构化结果、不编造空结果", () => {
  expect(commandResultSummary(undefined)).toBe("");
  expect(commandResultSummary({ indexed: 42 })).toBe('{"indexed":42}');
  expect(
    commandResultSummary({
      content: [{ type: "text", text: "indexed docs: 7 documents" }],
      structuredContent: { path: "docs", indexed: 7 },
    }),
  ).toBe('{"path":"docs","indexed":7}');
  expect(commandResultSummary("x".repeat(10), 4)).toBe("xxxx…");
});
