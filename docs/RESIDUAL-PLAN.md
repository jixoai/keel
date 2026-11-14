# Keel 残留任务盘点与推进计划

> 日期：2026-11-14 · 口径：**任务板（tasks.md）只是索引，真值 = 代码 + 证据**。
> 完成标准对齐 `docs/MATURITY.md` 的 G0–G5 闸门（规格 → 单机 → 崩溃一致 → 可观测 → PI 对齐 → 规模/运维）。

## 0. 一个必须先说清的口径问题

`keel-harness-v2` 的 tasks.md 是 0/142 —— 这不代表 142 项都没做，而是**任务板从未维护**：
其中 W0（契约）、W2（内核契约）、W3（file/bash 插件）、W4（task-log）、W7（skills）等大量内容
已经以代码/测试/spec 形式落地。同理 `runtime-foundation` 的「主仓挂 submodule + PROJECT-MAP 登记」
已核实完成（`.gitmodules` + `PROJECT-MAP.md:36`）。
因此 **Wave 1 的第一步是「任务 ↔ 代码对账」**：逐项标注 已做/未做/被替代，更新勾选，避免重复实现。
对账之前的任何「完成度百分比」都不可信。

## 1. 现状总览（10 个未归档 change）

| change | 勾选/总数 | 残留性质 | 状态 |
|---|---|---|---|
| keel-agent-team-v1 | 38/61 | 9 项=运行注入（实施中）· CLI start/stop/save 3 · UI 打磨 4 · 媒体适配器 4 · 验收 3 | **进行中（A 线）** |
| keel-session-surfaces-v1 | 29/30 | 1 项=成员结果进 captain 时间线（规则草案已随 RUNTIME-PLAN c.4 交付） | 随运行注入收口 |
| keel-demo-provider-v1 | 24/24* | *1 项 `[~]`：真实麦克风桥接（已知设计缺口，非阻塞） | **已完成**（可归档） |
| keel-trace-triage-v1 | 15/15 | 负结果 1：NSH resize 整树重建 DOM（仓外运行时属性，另立 change） | **已完成**（可归档） |
| keel-ui-polish-v1 | 13/13 | — | **已完成**（可归档） |
| fix-session-delete-soft-archive | 全部 | — | **已完成**（可归档） |
| provider-secret-direct-config | 部署侧完成 | 同名密钥回退待实现（等 A 线收口，同文件不并发写） | 待落码 |
| runtime-foundation | 40/47 | **冻结历史板**（文首明示勿再勾选）：残留已并入 `keel-service-graph` | 只作历史对照 |
| keel-service-graph（已归档） | 89/91 | 归档时留 2 个**已知缺口**：DSH sidecar 参考适配器 pin、gateway 故障 scenario（crash-loop/大帧取消/stderr 洪泛） | 缺口并入 Wave 3/4 |
| keel-tenancy-usage（已归档） | 29/29 | tenant-config / usage-ledger / provider-telemetry 全交付；**不含** api_keys/auth/RLS/配额 | 完成 |
| keel-ga | 17/43 | 26 项，其中 tenancy 实为 5 真项（api_keys/auth/provisioning/RLS/带宽·存储配额）+ 2 核验项（用量字节、跨租户测试；token 用量账本已有）· provider-files 7 · blob-lifecycle 5 · agent-execution 2 · 验收 5 | **下一主战场** |
| keel-ecosystem | 4/72 | 68：observability 7 · recoverability 9 · compaction 7 · skills 9 · hooks 6 · mcp 9 · PI 基座 5 · 收口 7 · **待 Owner 决策 6** | 中期主战场 |
| keel-harness-v2 | 0/142 | W12 插件补全 40 · W0 22 · W11 验收 13 · W1-W10 各 3-8（含大量已落地未勾） | **先对账** |

## 2. 去重归并（多 change 重复项只算一次）

- **RLS / 每租户配额**：runtime-foundation ↔ keel-ga §2 → 归 **keel-ga tenancy-quotas** 执行。
  注：`keel-tenancy-usage`（已归档 29/29）只做了租户配置/用量账本/telemetry，
  api_keys、`internal/auth`、provisioning、RLS、带宽/存储配额仍是真缺口；run/流配额已由
  `keel-service-graph` 接线（internal/quota），kernel 侧 token 用量账本已存在。
- **Provider 文件（blobId/adapter/缓存）**：keel-ecosystem §7 ↔ keel-ga §3 → 归 **keel-ga provider-files**。
- **归档遗留缺口**（无活跃任务板承接，本计划接管）：
  `keel-service-graph` 的「DSH sidecar 参考适配器」→ 并入 Wave 4 插件波；
  其「gateway 故障 scenario（crash-loop/大帧取消/stderr 洪泛）」→ 并入 Wave 3 可恢复性。
- **Agent/插件补全**：keel-harness-v2 W8/W12 与 agent-team 插件面部分重叠 → 对账后合并归属。
- **媒体适配器**（图/视/音/决策）与 ecosystem 的「能力」不同轴 → 独立清单（Wave 2）。

## 3. 计划（波次；每波开头对账，结尾 G 级证据 + 勾选 + 提交）

### Wave 0 — 运行注入收口（进行中）
- 线 A：RUNTIME-PLAN P1→P6（selection 词表 → PromptResolver → 工具面过滤 → captain/成员 → run 快照/前端）
- 同名密钥回退（provider-secret-direct-config 待实现项）
- 门：wire 标记 `KITE-RUNTIME-MARK-7/8` + 全量 Go/TUI 绿 + tasks 勾选 + 提交推送

### Wave 1 — GA 底座（并行 3 条）
1. **harness-v2 任务对账**（1 条）：产出「已落地/未落地/被替代」表，更新 tasks.md；这是后续排期的前提
2. **keel-ga tenancy-quotas**（后端）：api_keys 表 + internal/auth + provisioning + RLS + 配额(429) + 用量事件 + 跨租户测试
3. **keel-ga provider-files**（后端）：ImageRefContent/事件只存 blobId/能力位/resolver(file_id>url>base64)/Gemini·OpenAI·Anthropic 适配/缓存/内存上限测试

### Wave 2 — 运维与产品尾项（并行 3 条）
4. **blob-lifecycle**：缩略图异步 + 事件引用计数 + TTL sweeper + base64 fuzz + 文档
5. **agent-team 尾项**：CLI `agent start/stop`、`team save/start`、AGENTS.md 文件引用编辑器、五态/390 复检、组合页视觉复核
6. **媒体适配器**：image/video/tts/stt/decision 五族（provider 崩溃隔离 + usage 计量不写价格）

### Wave 3 — 可观测/恢复/压缩（ecosystem P1）
observability（run.finished 已存在→指标/日志字段补齐）→ recoverability（generation 幂等/task 状态机/checkpoint 回放/10min 混沌）→ compaction（事件+checkpoint 投影/手动压缩/token 账）

### Wave 4 — 生态 P2/P3 + harness-v2 主力
skills（加载/渐进披露/租户隔离/权限）→ hooks（事件对齐/命令钩子/审计）→ mcp（stdio/传输/命名空间/重连/鉴权）→ harness-v2 对账后剩余 W 波（W0 收尾、W6/W8/W9/W10/W12）

### Wave 5 — GA 验收与收口
1000 会话/64 并发 run/2GB/SIGKILL 10min 混沌/交叉复核（子代理，本项目不用 Codex）/报告归档/全部 change archive/主仓指针跟进

## 4. 本机部署现状（今天调整）

- **模型通道 = 阿里百炼订阅**：`magpie / bailian-token-plan/qwen3.8-flash`（唯一可见 provider）。
  其余 7 个内置 provider（s2api/a6api/deepseek/anthropic/openai/openrouter/ollama）已逐个
  `--visibility hidden`（revision 17，可逆）——它们是陈旧声明，本部署从未有过对应 key，
  不应再要求用户补 key。
- **租户密钥库已启用**（`KEEL_SECRET_KEY` 在 gitignored `.env.local`）：
  设置 › 密钥 可直接保存凭据，provider 用 `secret:NAME` 引用；该链路已端到端实证
  （假上游断言 Bearer 命中，`run.finished=done`）。
- `serve-local.sh` 同时支持 `*_KEY` env 透传（云端部署形态不受影响）。

## 5. 待 Owner 决策（阻塞对应工作流）

| # | 决策 | 建议/影响 |
|---|---|---|
| 1 | Skill 是否允许携带可执行脚本 | 影响 skills 权限模型（Wave 4） |
| 2 | MCP v1 是否包含 stdio | PI 默认支持，建议包含（影响进程管理） |
| 3 | Hook 是否命令+决策两者都做 | 建议是 |
| 4 | 可观测是否上 OTLP | 默认关，需要再开 |
| 5 | 是否抽 `pkg/` 稳定 SDK | 影响对外 API 承诺 |
| 6 | PI pin 升级策略（当前 `55a5c69`） | 跟上游节奏 |
| 7 | 媒体适配器优先级（图/视/音/决策先做哪族） | Wave 2 内部排序 |

## 6. 今日已到账（证据锚点）

- 三次推送：`9d023c1`（Agents/团队产品面 + 三插件 + 团队启动模式）、`59790cf`（插件/模型尾项 + 会话软删除）、`8783372`（demo/trace 收尾 + 运行注入方案）
- 门禁：Go 全量绿 · `nsh check` · TUI 106/106
- 真库 E2E：agents/teams CLI 全链（403/409 权限与引用保护）· 会话删除 UI 全路径（37→36）· 直接配置凭据（DIRECT-CONFIG-OK）· 六插件能力树
- 审计报告（公网）：`https://local-gaubee-keel-harness.fde2e.com/report.html`
- 正在跑：运行注入实现（线 A，P1→P6）
