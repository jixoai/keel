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

### 2.1 agent loop（`internal/loop/conformance_test.go`）

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
| client: initialize / tools list + pagination / call / timeout / server requests | `TestClient*` | ✅ PASS |
| stdio transport: framing / stderr / child exit / close / stubborn child | `TestStdio*` | ✅ PASS（1 例按平台跳过） |
| Streamable HTTP: JSON + SSE / session + protocol headers / GET stream | `TestStreamableHTTPTransport/*` | ✅ PASS |
| Streamable HTTP: Last-Event-ID resume / broken response stream / reconnect | 同上 | ✅ PASS |
| Streamable HTTP: 202-without-response / JSON size limit / BOM + UTF-8 | 同上 | ✅ PASS |
| Streamable HTTP: error excerpt / body in message / 401 + insufficient_scope / close DELETE / expired session | 同上 | ✅ PASS |
| SSE parser: CR/LF/CRLF / chunk splits / comments / multiline / size limit | `TestConsumeSSEStream/*` | ✅ PASS |
| config: stdio + HTTP entries / transport picking / field errors / oauth | `TestValidateMcpServerConfig/*` | ✅ PASS |
| config: CIMD callback / unknown fields / override predicate | 同上 | ✅ PASS |
| mcp.json: global + project merge / override keys / namespace clash | `TestLoadConfigFiles/*` | ✅ PASS |
| manager: connect / tool adapters / partial failure / prompt section / close | `TestManagerConnect` | ✅ PASS |

实现备注：`CreateToolName` 的 hash 覆盖**未清洗**的 `server\0tool`（与 PI 字节一致）；
`LimitContent` 返回 `{Content, FullOutputPath}`，`saveOutput(data, extension)` 与 PI 同形。
Streamable HTTP 保留 PI 的请求分类（JSON/SSE/202）、SSE 断点续传、GET 流重连与退避、
AuthProvider 的一次 401/403 步进重试、`Mcp-Session-Id` / `MCP-Protocol-Version` 头，
以及 close 时用最后一个 token 发 DELETE（不重新取 token）。

### 2.3.1 MCP OAuth（`internal/mcp/oauth/*_test.go`）

| PI 测试（`packages/mcp/test`） | Go 测试 | 状态 |
|---|---|---|
| oauth: discovery + DCR + PKCE + 401 refresh 全链路 | `TestOAuthDiscoversRegistersAuthorizesAndRefreshesOn401` | ✅ PASS |
| oauth: 并发 401 共享一次 refresh（rotation） | `TestOAuthSharesOneRefreshBetweenConcurrent401s` | ✅ PASS |
| oauth: insufficient_scope 不 refresh，保留旧 grant | `TestOAuthAsksForAuthorizationOnInsufficientScope` | ✅ PASS |
| oauth: 凭据绑定精确 server URL / application_type / issuer / 非 HTTP endpoint | `TestOAuthBindsCredentialsToExactServerURL`、`TestOAuthRegistrationApplicationType`、`TestOAuthRejectsIssuerMismatch`、`TestOAuthRejectsNonHTTPAuthorizationEndpoints` | ✅ PASS |
| oauth: 配置 metadata 文档 / iss 参数矩阵 / ctx 取消 / callback 页面 | `TestOAuthUsesConfiguredAuthorizationServerMetadataDocument`、`TestOAuthExchangesCodeOnlyWhenIssParameterMatches`、`TestOAuthStopsWhenContextIsCancelled`、`TestOAuthCallbackServerPages/*` | ✅ PASS |
| sign-in: 二次调用完成 / refresh / 锁 / stored client / CIMD / 取消 | `TestSignIn*`（含 5 个 stored-client、10 个 CIMD、4 个取消子例） | ✅ PASS |
| auth-provider: refresh / pending 保留 / 文档刷新 / invalid_client / server/network error | `TestStoredAuthProvider*` | ✅ PASS |
| loopback: 回调完成 / 错路径 / 粘贴 URL / 取消 | `TestLoopbackSignIn*`、`TestPastedRedirectURLs` | ✅ PASS |

运行时替换（PI 无对应）：独立锁池 advisory lock（MaxConns=2 并发回归）、pending 原子
消费、AES-GCM AAD/key id、SSRF 私网拒绝、token 错误脱敏、HTTP sign-in/callback 与
同进程重连 E2E。**未覆盖**：CIMD 公共文档的静态托管（部署项）、宿主 per-tenant
manager 缓存（Runner/host 范围）、过期 pending 的后台清理。

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

合计 38 个测试，全部 PASS，coverage 80.6%，`-race` 通过。`render.go` 增加 span/trace/metric
的稳定文本渲染（排序属性）用于报告与调试；聚合器差异见下。

已知差异（诚实登记）：
- `FindMetrics` 按 `(interval,name,kind,unit,attrs)` 聚合 count/sum/min/max；PI memory adapter 返回 count-one 原始行
  （PI 的 `StoredMetric` 形状本身是聚合形状，这是 adapter 实现差异）。原始顺序由 `Measurements()` 保留。
- PI `telemetryNow()` 的 10ms 重锚时钟未移植；Go 用 `time.Now()` + 可注入 clock，µs 分辨率与顺序测试通过。
- TS 编译期 schema 推导、render、OTLP/Prometheus adapter 未在本次范围。

### 2.6 Extensions / Hooks（`internal/hooks/*_test.go`）

| PI 事件 | Go 测试 | 状态 |
|---|---|---|
| beforeToolCall allow/deny/modify + revalidation | `TestBeforeToolCall*` | ✅ PASS |
| afterToolCall replace / structured content / error results | `TestAfterToolCall*` | ✅ PASS |
| command hooks: JSON stdin/stdout / 5s timeout / failure policy / audit | `TestCommandHook*`、`TestAudit*` | ✅ PASS |
| registry ordering / snapshots / wrap tool | `TestRegistry*`、`TestWrapTool*` | ✅ PASS |

合计 81 个测试，全部 PASS，coverage 79.9%。

### 2.7 Durable（`internal/durable/*_test.go`）

| PI durable 语义 | Go 测试 | 状态 |
|---|---|---|
| task state machine: pending/running/waiting/sleeping/completing/terminal | `TestTasks*` | ✅ PASS |
| submissions: input/write/continue, queued/placed/done/unanswered, requestId dedupe | `TestSubmissions_*` | ✅ PASS |
| run failure / failed-run inbox / continue after failure | `TestRunFailure_*`、`TestInputsQueued*`、`TestContinue*` | ✅ PASS |
| generation idempotency / retry policy / restart limit | `TestGeneration*`、`TestTasks_Retry*` | ✅ PASS |
| tool tasks: call→execute, replay safe/unsafe, restart/abandoned slots | `TestToolHooks*`、`TestNestedTools*` | ✅ PASS |
| nested tools: per-call task, keys, abort/reuse, parent result | `TestNestedTools*` | ✅ PASS |
| structured output: schema validation / diagnostics / afterTool revalidation | `TestStructuredOutput*` | ✅ PASS |

合计 100 个测试，全部 PASS。PG 侧 `internal/store/tasks.go` 持久化 tasks/submissions；
entries/inbox/live/nested documents 仍由内存 harness 承担（见 §4）。

### 2.8 Host / Runner 装配（`internal/host`、`internal/agent`）

| PI 宿主能力 | Go 测试 | 状态 |
|---|---|---|
| system prompt sections + skills XML + file-read wording | `TestBuildSystemPrompt*`、`TestHost*` | ✅ PASS |
| skill discovery + skill tool (progressive disclosure) | `TestHost*` | ✅ PASS |
| runner uses host prompt and tool registry | `TestRunnerUsesHostAssembly` | ✅ PASS |
| runner run span + run counter/duration metric | `TestRunnerRecordsTelemetry` | ✅ PASS |
| provider stream proxy events / toolcall deltas / unterminated terminal | `TestProxy*` | ✅ PASS |

## 2.9 汇总（2026-11-08）

**终轮实测（复核修复后重跑，证据 `conformance.json`）：674 PASS / 0 FAIL / 1 SKIP**（13 个包；`scripts/conformance.sh` + `KEEL_TEST_DSN` 口径）。修复前的终轮（`6875d89`）为 673/0/1（`conformance-current-dsn-673/`）——差的 1 项＝复核修复新增的 `TestExternalControlBodyIsValidated`（cancel/close 请求体校验）。

同一套包在**无 DSN**（PG 集成用例按设计 skip）口径下的对照值：

| 口径 | 基线 `e7361f8`（service-graph 之前） | 当前（含本 change + 复核修复） |
|---|---|---|
| 带 DSN | 495 PASS / 0 FAIL / 1 SKIP（`conformance-baseline-dsn-495/`） | **674 PASS / 0 FAIL / 1 SKIP**（本轮 `conformance.json`；修复前 673 见 `conformance-current-dsn-673/`） |
| 无 DSN | 481 PASS / 0 FAIL / 15 SKIP（496 项；`conformance-baseline-593/`） | 584 PASS / 0 FAIL / 79 SKIP（663 项；`conformance-current-no-dsn/`） |

口径差来源：无 DSN 时 PG 集成用例在包内 `t.Skip`（基线 14 个：agent 7、httpapi 3、blob 4；当前 78 个：agent 22、httpapi 52、blob 4），另 1 个 skip 恒为 MCP `TestHelperProcess`（辅助子进程入口，设计行为）。当前带 DSN 的 674 = 无 DSN 的 584 + 78 个由 SKIP 转 PASS 的集成用例 + `TestAdminConfigRejectsInvalidDocuments` 的 11 个子测试（父用例在无 DSN 时整体 skip，子测试不入账）+ 1 个复核修复新增的体校验测试。

**历史「593」口径的更正**：`e7361f8` + `scripts/conformance.sh` 无法复现 593——同口径实测为 495（带 DSN）/481（无 DSN），其中 495 与本文件历史记载一致；仓库内也没有 593 的机器工件可追溯。按可复现证据，冻结 main 的基线记为 **495（带 DSN）/481（无 DSN）**；593 不再作为回归门数字引用。

证据落位：`.agents/reports/keel/2026-11-07-service-graph/` 下 `conformance.json`（终轮）、`conformance-current-dsn-673/`、`conformance-current-no-dsn/`、`conformance-baseline-dsn-495/`、`conformance-baseline-593/`（目录名为任务约定保留，内容为 `e7361f8` 无 DSN 实测 481）。

| 套件 | PASS（带 DSN，终轮） |
|---|---|
| loop | 70 |
| compaction | 44 |
| mcp | 90（1 skip） |
| skills | 49 |
| telemetry | 38 |
| hooks | 81 |
| durable | 100 |
| host | 53 |
| agenttool | 2 |
| 运行时（agent/httpapi/blob/stream） | 147 |
| **合计** | **674** |

## 3. 运行

```bash
cd keel
# 带 DSN（与终轮 674 同口径）
KEEL_TEST_DSN='postgres://…' scripts/conformance.sh /tmp/keel-conformance.json

# 无 DSN 对照（PG 集成用例 skip；481/584 口径）
GOPROXY=https://goproxy.cn,direct GOTOOLCHAIN=go1.27.3 go test -json -count=1 \
  ./internal/loop ./internal/compaction ./internal/mcp ./internal/skills \
  ./internal/telemetry ./internal/hooks ./internal/host ./internal/durable \
  ./internal/agenttool ./internal/agent ./internal/httpapi ./internal/blob ./internal/stream \
  > /tmp/keel-conformance-no-dsn.json
python3 scripts/summarize-conformance.py /tmp/keel-conformance-no-dsn.json
```

## 4. 未覆盖与后续

- **MCP OAuth 宿主侧**：协议与存储已全量移植；CIMD 公共文档静态托管、per-tenant
  manager 缓存（多租户同时在线）与过期 pending 后台清理仍未实现。
- **durable 存储层**：PI 的 SQLite/JSONL、commit publication/observer、watch/ops、document
  family 生命周期、`persistWithinMs` 延迟提交与 slot 快照未复刻；Keel 用事件日志 + PG 投影替代。
  PG 目前只落 tasks/submissions，entries/inbox/live/nested docs 仍在进程内 harness。
- **durable 任务版本迁移**（v1→v2→v3）、orphaned/blocked/background task、read-only harness、
  wake/sleeping timer、compaction 任务、provider deferred response/poll 未移植。
- **usage ledger**：schema/store 契约已交付（`usage_ledger` 以 request_id 幂等，
  `settled`/`unaccounted` 状态与聚合查询）；request_id 生成、请求元数据事件、崩溃补偿
  扫描与 `AppendUsage` 调用属于 composition root，待 Runner 升级 change 接线。
- **coding-agent 宿主**：extensions loader 的完整注册/信任模型、project-trust UI、交互式 `/mcp`
  菜单、TUI/CLI 不复刻（明确非目标）。
- **telemetry**：OTLP / Prometheus adapter 未实现；`pi.ai.request` 请求级 span/token 指标
  模块已交付（`internal/agent.NewObservedStream` + memory adapter 测试），composition root
  待 Runner 接线；当前 run 级 span + run counter/duration 已在线。
- 所有未覆盖项必须在报告里显式登记，不得用「已对齐」一笔带过。
