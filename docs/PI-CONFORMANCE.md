# PI 官方测试对齐（Conformance）

> 目标：**Keel 是 PI 的重写，不是重新定义。** 官方测试是行为契约的第一手证据；
> 我们逐条移植，而不是自拟期望。Pin：`pi@55a5c69`（2026-11-05）。

## 0. 官方测试盘点

| 套件 | 测试文件数 | 处理策略 |
|---|---|---|
| `packages/agent/test` | 5（agent-loop 2234 行、agent 1229 行、e2e 416 行、proxy、typebox） | **逐条移植核心行为**（本轮重点） |
| `packages/durable/test` | 102（compaction 2547 行、tool-hooks 806 行等） | 先移植 compaction/task/checkpoint 核心；其余按能力推进 |
| `packages/mcp/test` | 14 | **纯函数全量移植**（naming/schema/content/limit/config） |
| `packages/telemetry/test` | 6 | 移植 conformance/metrics/store/propagation 核心 |
| `packages/coding-agent/test` | 349 | 先移植 skills 核心；extensions/compaction 宿主用例后续 |

**诚实边界**：102 + 349 个文件不可能一轮全搬。本轮以「load-bearing 行为」为准：
事件顺序、工具语义、压缩切点、MCP 命名/裁剪、SKILL.md 解析、遥测 span/metric。

## 1. 移植纪律

1. **测试名保留 PI 的 `it(...)` 标题**（可加前缀），便于上游对照。
2. **期望值来自 PI 源码/测试**，不得自拟「更合理」的结果；发现 PI 行为反直觉时，
   先照抄，再在报告里标注。
3. **fixture 来自上游**：SKILL.md、MCP JSON、事件流尽量复制 PI 测试样例。
4. 发现 Keel 与 PI 不一致 → 改 Keel，不改测试；若 PI 自身矛盾，记录并标注。
5. 每个移植测试在文件头注明上游文件与行号范围。

## 2. 本轮矩阵

### 2.1 Agent loop（`internal/loop/conformance_test.go`）

| PI 测试（`packages/agent/test/agent-loop.test.ts`） | Go 测试 | 状态 |
|---|---|---|
| should emit events with AgentMessage types | `TestConformanceShouldEmitEventsWithAgentMessageTypes` | ✅ PASS |
| tool_execution_end completion order / results source order | `TestConformanceToolExecutionEndCompletionOrderPersistSourceOrder` | ✅ PASS |
| length-truncated assistant: no tool execution, error results | `TestConformanceLengthTruncatedAssistantFailsToolCalls` | ✅ PASS |
| mutated beforeToolCall args without revalidation | `TestConformanceMutatedBeforeToolCallArgsWithoutRevalidation` | ✅ PASS |
| terminate semantics (whole batch / mixed / blocked) | `TestConformanceTerminateRequiresWholeBatch/*` | ✅ PASS |
| agentLoopContinue: no user message event / empty throws | `TestConformanceAgentLoopContinue/*` | ✅ PASS |
| steering injected after all tool calls complete | `TestConformanceSteeringInjectedAfterAllToolCallsComplete` | ✅ PASS |
| follow-up consumed after inner loop | `TestConformanceFollowUpConsumedAfterInnerLoop` | ✅ PASS |
| prepareNextTurn snapshot | `TestConformancePrepareNextTurnSnapshotBeforeContinuing` | ✅ PASS |
| prepareRequest initial request / no steering after | `TestConformancePrepareRequest/*` | ✅ PASS |
| tool duration excludes hooks | `TestConformanceToolDurationExcludesHooks` | ✅ PASS |
| custom message types via convertToLlm | `TestConformanceCustomMessageTypesViaConvertToLlm` | ✅ PASS |
| settle 事件（宿主层，非 PI 内核） | `TestConformanceAgentLifecycleSettleEvents/*` | ✅ PASS（`EmitSettleEvents=true`；默认关闭） |

**内核默认与 PI 一致**：默认只发 `agent_start → turn_start → message_* → turn_end → agent_end`
（PI `types.ts` 的 10 类事件，无 settle）。`agent_before_settle` / `agent_settled` 是
`packages/coding-agent` 的宿主事件，Keel 保留类型并由 `RunConfig.EmitSettleEvents` 门控
（默认 false），测试中显式开启并标注 host-level extension。

修正的 12 处行为差异（摘要）：steering 三处轮询、纯文本 turn 后 steering 不丢、prepareRequest、
StartContinue、length 截断发工具事件、beforeToolCall 参数不再二次校验、blocked 结果可 terminate、
terminate 不提前跳过 follow-up/steering、工具耗时（排除 hook）、settle 门控、produced 列表、
prepareNextTurn 可追加消息。

未覆盖（诚实登记）：
- prompt/steering/toolResult 的 message_start/message_end：Keel 当前只持久化 assistant/tool 结果，
  补发会导致 `internal/agent` 双写；已在 loop.go 文件头登记，待宿主事件模型落地后补。
- PI 的 `finishTurn`（一次纯上下文续跑）与 `declareToolChanges`（system 消息宣告工具变更）未实现。
- 并行批次 `tool_execution_start` 的源顺序（PI 先 preflight 再发，Keel 在 goroutine 内发）。
- 自定义消息用 `CompactionMessage` 验证（Go 的 Message 是封闭接口）。

### 2.2 Compaction（`internal/compaction/conformance_test.go`）

| PI 测试（`packages/durable/test/harness-compaction.test.ts`） | Go 测试 | 状态 |
|---|---|---|
| range selection: keepRecentTokens / cut candidates / never at tool result | `TestConformanceRangeSelection/*`（11 子例） | ✅ PASS |
| serialization: transcript / tool truncation / omit system+thinking | `TestConformanceSerialization/*` | ✅ PASS |
| DEFAULT_COMPACTION_POLICY / below / at threshold | `TestConformanceThresholdPolicy/*` | ✅ PASS |
| summary request / checkpoint / rebuild / previous summary | `TestConformanceSummaryRequestAndCheckpoint/*`（6 子例） | ✅ PASS |
| summary failure: model_error / length / tool call / empty text | `TestConformanceSummaryFailure/*` | ✅ PASS |
| token estimation: chars/3.5, image chars, usage selection | `TestConformanceTokenEstimation/*`（7 子例） | ✅ PASS |

修正的 16 处行为差异（摘要）：token 除数 4→3.5、checkpoint 用渲染后文本、切点回退用最后候选、
预算未达标返回 no-cut、悬空 toolResult 不可切、error/aborted 三处剔除、thinking 省略、
旧 checkpoint 进摘要源、摘要验收/失败文案、文本块 `\n` 连接 + trim、usage 时间戳规则、
`backgroundTokens`、`CoveredMessages`、提示词补句。

未覆盖：session/task/storage 集成用例、beforeCompact hooks、instructions 通道、摘要重试/退避、
per-model overrides、background 实际触发（loop 仍只做 blocking）。

### 2.3 MCP（`internal/mcp/*_test.go`）

| PI 测试（`packages/mcp/test`） | Go 测试（`internal/mcp`） | 状态 |
|---|---|---|
| naming: joins / 64-shorten / taken hash / namespace | `TestCreateToolName/*`、`TestNamespace/*` | ✅ PASS |
| input-schema: object + properties / keep keywords | `TestNormalizeInputSchema/*`（+ 非法 JSON 加固） | ✅ PASS |
| content: text/image/audio/link/resource/structured | `TestToLLMContent/*` | ✅ PASS |
| output-limit: 20KB middle cut / save / unicode | `TestLimitContent/*`（+ `.txt` extension + FullOutputPath） | ✅ PASS |

实现备注：`CreateToolName` 的 hash 覆盖**未清洗**的 `server\0tool`（与 PI 字节一致）；
`LimitContent` 返回 `{Content, FullOutputPath}`，`saveOutput(data, extension)` 与 PI 同形。

### 2.4 Skills（`internal/skills/*_test.go`）

| PI 测试（`packages/coding-agent/test/skills.test.ts`） | Go 测试 | 状态 |
|---|---|---|
| load valid / name mismatch / invalid chars / long name | `TestLoadSkillsFromDir_*` | ✅ PASS |
| missing description / unknown fields / nested / root preferred | 同上 | ✅ PASS |
| no frontmatter / invalid YAML / multiline / consecutive hyphens | 同上 | ✅ PASS |
| all fixtures / non-existent dir / parent fallback | 同上 | ✅ PASS |
| disable-model-invocation + default | 同上 | ✅ PASS |
| formatSkillsForPrompt: empty / XML / intro / escape / multiple / disabled | `TestFormatForPrompt_*`（Node 逐字节 golden） | ✅ PASS |
| SDK: temp skills dir discover | `TestDiscover_TempSkillsDir` | ✅ PASS |
| name collision keep first | `TestDiscover_Collision*` | ✅ PASS |

合计 49 个测试，全部 PASS，coverage 85.6%。

已知差异（诚实登记）：
- YAML 解析器不同（npm yaml vs yaml.v3）；重复 key 显式拒绝、timestamp 标量兼容、null 文档按空处理。
- 字段计数：PI 用 UTF-16 code unit，Go 用 rune（仅 emoji 等补充平面字符不同；name 限定 ASCII）。
- 发现顺序：PI 依赖 OS readdir 顺序，Go 按文件名排序（同名兄弟冲突赢家可能不同）。
- 未移植：`.gitignore/.ignore` 过滤（需新依赖）、`sourceInfo` scope、project-trust、`resourceLoader` 装配。
- 未知字段：按 PI 测试静默忽略（任务文字的「warning」以 PI 测试为准）。

### 2.5 Telemetry（`internal/telemetry/*_test.go`）

| PI 测试（`packages/telemetry/test/*`） | Go 测试 | 状态 |
|---|---|---|
| capabilities / propagation round-trip / roots | `TestConformanceCapabilities`、`TestConformancePropagation*` | ✅ PASS |
| callback lifecycle / rejection / explicit status | `TestConformanceCallbackLifecycle`、`TestConformancePreservesRejectionValues`、`TestConformanceExplicitStatusWins` | ✅ PASS |
| attributes+events / inert after settle / parentage | `TestConformanceMergesAttributesAndEvents`、`TestConformanceCallsAfterSettlementAreInert`、`TestConformanceParentageIncludesConcurrentChildren` | ✅ PASS |
| links initial/late/detached | `TestLinks*`、`TestPropagationDetaches*` | ✅ PASS |
| metrics ordered/exemplars/aggregate/detached/noop | `TestMetrics*`、`TestNoopMetricsShareInstruments` | ✅ PASS |
| store settled/detached / filters / cursor / remote trace | `TestStore*` | ✅ PASS |

合计 34 个测试，全部 PASS，coverage 80.6%，`-race` 通过。

已知差异（诚实登记）：
- `FindMetrics` 按 `(interval,name,kind,unit,attrs)` 聚合 count/sum/min/max；PI memory adapter 返回 count-one 原始行
  （PI 的 `StoredMetric` 形状本身是聚合形状，这是 adapter 实现差异）。原始顺序由 `Measurements()` 保留。
- PI `telemetryNow()` 的 10ms 重锚时钟未移植；Go 用 `time.Now()` + 可注入 clock，µs 分辨率与顺序测试通过。
- TS 编译期 schema 推导、render、OTLP/Prometheus adapter 未在本次范围。

## 2.6 汇总（2026-11-06）

`scripts/conformance.sh` 的 `go test -json` 统计：**205 个测试（含子测试）PASS / 0 FAIL**。

| 套件 | PASS |
|---|---|
| loop | 30 |
| compaction | 42 |
| mcp | 31 |
| skills | 49 |
| telemetry | 34 |
| 运行时（agent/httpapi/blob/stream） | 19 |

## 3. 运行

```bash
cd keel
GOPROXY=https://goproxy.cn,direct GOTOOLCHAIN=go1.27.3 go test ./internal/loop ./internal/compaction ./internal/mcp ./internal/skills ./internal/telemetry
```

## 4. 未覆盖与后续

- `packages/durable/test` 其余 ~100 个文件：session/storage/JSONL/提交语义，按 `keel-ecosystem` 的
  recoverability 工作流逐批移植。
- `packages/coding-agent/test` 的 extensions/compaction 宿主用例：等宿主层扩展模型落地后移植。
- MCP 网络传输（stdio/streamable HTTP/OAuth）：本轮先做纯函数；网络层在 `keel-ecosystem` P3 移植。
- 所有未覆盖项必须在报告里显式登记，不得用「已对齐」一笔带过。
