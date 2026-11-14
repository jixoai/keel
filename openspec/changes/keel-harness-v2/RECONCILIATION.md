# keel-harness-v2 任务板 ↔ 代码现状对账（RECONCILIATION）

> 日期：2026-11-14 · 对账范围：`openspec/changes/keel-harness-v2/tasks.md` 全部 142 条未勾任务
> （0/142）＋ 追加：`openspec/changes/keel-ecosystem/tasks.md` 冻结板 68 条 ＋
> `archive/keel-service-graph/tasks.md` 留存的 2 个未勾缺口。
>
> 口径（RESIDUAL-PLAN §0 引申）：**任务板只是索引，真值 = 代码 + 证据**。本文件每行给
> `file:line` / 测试名 / spec / 归档 change 条目，全部可点验；不写「应该有」。
>
> 对账期间工作区有另一条线在改 `internal/servicehost/**`、`internal/modelcfg/**`、
> `internal/agent/**`、`internal/agentext/**`、`cmd/keel/serve.go`、`web/src/data/{contract,store}.ts`、
> `web/src/screens/model-picker.tsx`——这些文件只读引用，未编译全量门（避免在途代码误报），
> 对账以静态证据为主。

## 0. Owner 2026-11-14 架构口径（「已实现」的判定标准）

来源：`docs/RESIDUAL-PLAN.md` §5（Owner 原话提炼：「插件的本质，就是依赖……围绕依赖注入
搭建起来的一套系统」）。

| # | 裁决 | 本对账的判定含义 |
|---|---|---|
| 1 | **插件 = 依赖注入的模块注册** | 一项能力算「在架构里已实现」，标准是它能作为模块/服务被**注册与替换**，而不是散落代码存在即可 |
| 2 | hooks / tools / providers / skills **同一注册面** | `agentext.Plugin`（hooks/tools/triggers/effects/prompt）+ `servicegraph`（服务 DI）+ `pluginpkg`（provides.*）是注册面；skills 目前不在 `provides` 里（缺口，见 W7/W12） |
| 3 | Skill 可执行脚本**默认允许**（可选安全模式） | skills-plugin **依赖** bash-plugin；无 bash 则按能力缺失呈现。所以「W3 bash 缺失」同时是 W7 脚本执行的阻塞 |
| 4 | MCP v1 **含 stdio** | `internal/mcp/stdio_transport.go` 已实现；但 v2 要求「只经聚合工具/CLI 暴露」，见 W6 |
| 5 | Hooks **必须完备**（随插件一起） | `internal/hooks` 已全事件名 + 进程内/命令钩子 + 决策；本轮对账判「已实现」 |
| 6 | 可观测**全面支持 OTLP、默认关** | 判「部分」：`internal/telemetry` 接口/适配层在，OTLP adapter 不在（`docs/PI-CONFORMANCE.md:267`、`docs/HOST.md:51` 明说未实现） |
| 7 | decision 模型族**必须支持** | 已实现：`internal/modelprovider/provider.go:81`（`TypeDecision`）、`internal/modelcfg/decision_type_test.go`、`POST /v1/decisions/ask`（`internal/httpapi/decision.go:54`）、`POST /v1/media/decision`（`internal/httpapi/media.go:28`） |
| 8 | 稳定 SDK 暂不抽 / PI pin 跟随上游 | 生态板两条决策项据此判「已废弃」 |

## 1. 状态词汇与统计口径

- **已实现**：任务的全部子项都有可点验证据 → tasks.md 原地勾 `[x]` 并追加 `（对账：…）`。
- **部分实现（缺 X）**：主体落地，仍有明确缺项 → tasks.md 保持未勾，本文件说明缺什么。
- **未实现**：无实质落地。
- **已被其它 change 承接**：由归档/活跃 change 名 + 条目证据承载（归档：`keel-service-graph`
  / `keel-plugin-core` / `keel-kernel-loop` / `keel-tenancy-usage`；活跃：`keel-agent-team-v1`
  / `keel-ga`）。
- **已废弃（理由）**：被新裁决/新口径取代。

### 1.1 各 W 统计（本次勾选 = 「已实现」条数）

| W | 条目数 | 已实现（已勾） | 部分实现 | 未实现 | 已废弃 | 备注 |
|---|---|---|---|---|---|---|
| W0 契约与骨架 | 22 | 3 | 9 | 10 | 0 | 契约/键命名空间已冻结；实现类全空 |
| W0.5 插件全景 | 8 | 0 | 2 | 6 | 0 | catalog 半成品；layer/slots/extensibility 全无 |
| W1 layerfs | 6 | 0 | 0 | 6 | 0 | 仅 ADR + 事件词汇 |
| W2 放置与内核契约 | 7 | 1 | 4 | 2 | 0 | 注入契约已实现；placement/groups 全空 |
| W3 file/bash 插件 | 8 | 0 | 0 | 8 | 0 | gbash/工具/capture/jobs 全空 |
| W4 task-log | 7 | 0 | 0 | 7 | 0 | 仅事件 kind 词汇 |
| W5 approve/notify | 6 | 1 | 0 | 5 | 0 | decision 原语在；工具/通知面全空 |
| W6 MCP 聚合 | 5 | 1 | 0 | 4 | 0 | 20KB/图片内容在；聚合工具/CLI/迁移全空 |
| W7 skills | 7 | 0 | 1 | 6 | 0 | 解析/发现/披露在；产品面全空 |
| W8 agent-plugin | 5 | 0 | 4 | 1 | 0 | spawn 半成品；fork/resume/任务卡无 |
| W9 oar-plugin | 5 | 2 | 0 | 3 | 0 | 通用外部网关两项能力在；OAR 专用全空 |
| W10 triggers | 3 | 1 | 1 | 1 | 0 | v1 三触发 + 去重 + 审计在；HMAC/补偿/并发上限无 |
| W11 验收门 | 13 | 1 | 8 | 4 | 0 | 仅真实模型 E2E 门成立 |
| W12 插件补全 | 40 | 4 | 13 | 22 | 1 | creation/web-search/HMR/登记在；supervisor/slots 全空 |
| **合计** | **142** | **14** | **42** | **85** | **1** | 勾选只给证据无歧义者（另含 1 条 Non-Goal 按已废弃勾选） |

---

## W0 契约与骨架（22 条）

证据总纲：10 份 spec 在 `openspec/changes/keel-harness-v2/specs/*/spec.md`；v2 事件 kind 词汇在
`internal/kernel/kernel.go:99-135` ＋ `internal/kernel/validate.go:44-77`（class/key/system）＋
`internal/store/events.go:24-47`（常量镜像，pin 测试 `internal/kernel/kernel_test.go:1481`）。

| ID | 原任务（tasks.md 行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W0-1 | L8 冻结全部 10 份 spec | **已实现（已勾）** | `specs/` 下恰好 10 个 `spec.md`（harness-core/agent-loop-contract/layerfs/task-log/tool-surface/plugin-architecture/plugin-ui-extension/supervision/client-shell/plugin-system）；随 change 提交（git 091e070 等） |
| W0-2 | L12 kind 表（class/key/fence/提交者 + run.finished payload） | **已实现（已勾）** | 全部新 kind 在 `internal/kernel/kernel.go:99-135`；class/key 表 `internal/kernel/validate.go:44-77`；Key 命名空间 `:140-243`；`run.finished` 校验含 `configHashBefore/After` `internal/kernel/validate.go:405-440`；写入方 `internal/agent/runner.go:248-253`；负控 `internal/kernel/contract_v2_test.go:62`（fs.write 不与 tool.result 同键）；※ task/fs/placement 等新 kind 目前**无 emitter**，R1 差分只登记了 run.* bookkeeping（`cmd/r1diff/bookkeeping.go:8-12`）——勾选按「词汇/fsync 契约冻结」口径 |
| W0-3 | L18 既有门承接（r1diff 场景/allowlist/重 pin + acceptance-all 全绿） | **部分实现** | 门基础设施在：`scripts/acceptance-all.sh`（含 R1、负控、混乱、内存段）、`cmd/r1diff` ＋ allowlist `openspec/changes/archive/2026-11-09-keel-kernel-loop/r1-allow.json`（每条 reason）、归档 `keel-service-graph` W6 记录了全绿；缺：v2 新增 kind（task.*/fs.*/config.rotated/placement.*/supervision.*/notify.*）未进 r1diff 规则/基线（因无 emitter，暂无实际差异） |
| W0-4 | L20 工具面迁移 + 终局工具集快照 + AgentTool 接口扩展单列 | **未实现** | 旧名仍在：`spawn_subagent`（`internal/agentext/subagent.go:628`、`internal/servicehost/binding.go:143`）、`mcp__<server>__<tool>`（`internal/mcp/naming.go:27-48`）、`clock`/`echo`（`internal/agent/tools.go:14,31`、`internal/host/host.go:236+`）、`creation_*`（`internal/agentext/creation.go:86-90`）；无 bash/read/write/edit/tasks/mcp/skills/approve/notify/creation 聚合工具；无 11 工具快照断言；`agentcore.AgentTool`（`internal/agentcore/tool.go:36-45`）无幂等键/超时/取消/截断/错误码/内存档位字段且未单列新任务 |
| W0-5 | L23 PI 偏离登记 + 删 mcp__ 适配 | **未实现** | `docs/PI-PARITY.md:48` 仍把 `mcp__<server>__<tool>` 列为对齐项，无 deviation 段；`internal/mcp/tools.go:7,43-53` 与 `naming.go` 仍实现，单测 `naming_test.go:17`、`client_test.go:598` 仍在 |
| W0-6 | L25 契约冻结 spike（GBASH-CONTRACT + OAR 契约） | **未实现** | `docs/GBASH-CONTRACT.md` 不存在；全仓无 OAR 契约文档（`grep -r OAR docs/` 仅命中本 change 内部） |
| W0-7 | L28 ADR 0001 + CI/部署/端口/SIGKILL sweep | **部分实现** | ADR 在 `openspec/changes/keel-harness-v2/adr/0001-layerfs-rust-grpc.md`（推翻记录/证据门/回退路径）；缺：Rust 工具链进 CI、Dockerfile/compose 服务、健康检查、端口登记、SIGKILL sweep（layerfs 服务本身未实现，W1） |
| W0-8 | L30 多用户进程（租户解析/过滤/装配隔离/plugin_state 租户/AGENTS 修订） | **未实现** | `docs/HARNESS.md:27-28` 明示「keel serve 是单租户产品进程……多租户复用需另立 change」；`AGENTS.md:38-41` 仍写「不做每用户进程」且未修订为「默认单进程多租户」；`internal/store/plugin.go:11-15` 声明 plugin 库存是 deployment-scoped；`/v1/plugins` 无租户过滤（`internal/agentapi/plugins.go:28-46` 仅 tenantFrom 校验） |
| W0-9 | L33 内核键命名空间 + 负控单测 | **已实现（已勾）** | `internal/kernel/kernel.go:217-243`（KeyTask/FSWrite/FSCapture/FSCaptureFailed/Config/Placement/Supervision/Guardrail/Resume/NotificationAck/Push/Run）、`CommitRequest` 字段 `:325-370`、keyFor 校验 `internal/kernel/validate.go:181-205`；负控 `internal/kernel/contract_v2_test.go:62`、键齐全性 `:95`；dedupe 前缀隔离测试 `internal/kernel/kernel_test.go:1527-1555` |
| W0-10 | L36 清单解析扩展（layer/slots/contributes/chunk 包/租户 plugin_state） | **部分实现** | `KnownFields(true)` 在 `internal/pluginpkg/manifest.go:412`；已有 `provides.tools/hooks/triggers/provider_types/provider/feature_ports/agents/agent_teams/cli`（`:113-145`）与 provider 专用 `layer`（`:148-156`）；缺：通用 `layer`、`provides.slots`、`contributes`、纯 chunk 包（webview 仍要求 `ui.entry` `:600`）、`plugin_state` 租户字段 |
| W0-11 | L38 catalog 唯一真源（keelPluginId/status/wave/dshGroups + COVERAGE 生成比对 + CI） | **部分实现** | `plugins/keel.plugin-market/catalog.json` 有 `status`（delivered/partial/planned/registered/absent）与 `keelPluginId`（如 `:57-58,159-160`）；缺 `wave`/`dshGroups` 字段（`grep -c '"wave"' = 0`）——`openspec/changes/keel-harness-v2/plugin-inventory.md:207` 自认「需补 wave 与统一 status 枚举」；无生成/比对脚本进 CI（`.github/workflows/ci.yml` 无相关 step），`plugins/COVERAGE.md` 手工维护 |
| W0-12 | L40 RSS 采样器（acceptance-rss.sh，≤500MB） | **部分实现** | 不存在 `scripts/acceptance-rss.sh`；现有内存门是 `scripts/acceptance-perf.sh:98-101` 的 cgroup `memory.peak`（2GB 上限）与 `acceptance-handoff-rss.sh`——均非「VmRSS 求和、≤500MB/目标 300MB」协议；未接入 `acceptance-all.sh` |
| W0-13 | L42 原生 target（web + macos 产物截图；TUI 真终端） | **部分实现** | web target 有：`web/README.md:33-38`（`nsh package --target web`）；TUI 有：`nsh run --target tui` ＋ `web/App.tui.test.tsx`、`web/src/ui/kit/**/*.tui*`；缺：`--target macos` 产物与截图（README 只列 web/tui；无 macos 构建记录） |
| W0-14 | L44 NSH 0.5.0 升级（Window API/媒体 grant/focus-visible/activationPolicy） | **未实现** | `web/README.md:12` pin 仍是 **nsh 0.4.0 (bba76ef nightly)**；`web/nsh.json` 无新能力声明；0.5.0 只进了本 change 的 spec（git 091e070 仅改 openspec 文档） |
| W0-15 | L46 客户端壳（单实例/托盘/快捷键/深链/文件关联/启动隐藏/独立窗口） | **未实现** | `web/nsh.json` 仅有 appearance/theme/capabilities/web.host；无 tray/shortcut/deep-link/file-association 配置；无桌面壳代码 |
| W0-16 | L48 系统集成（通知 silent/badge/深链/WebView 能力/摄像头麦克风降级） | **部分实现** | web 侧桥在：`web/host.js:7-15`（`keel.notify` {title,body,tag}）、`host.js:110-118` dispatch；缺 badge/silent/点击深链、`WebView evaluate` 返回值、自有 scheme、incognito、摄像头/麦克风显式降级（realtime 有 getUserMedia 链路但非本任务口径） |
| W0-17 | L50 更新与分发（nsh/updater + 签名 OTA + 回滚） | **未实现** | 全仓无 updater 集成/签名 OTA 流水线（`.github/workflows/release.yml` 是产物发布，非应用内自更新） |
| W0-18 | L52 长会话性能（1000 消息滚动保真/离屏/大表格/视图树释放） | **未实现** | 无 1000 条消息测试/离屏增量创建/内存回落断言（`grep 1000 web/` 无相关用例；`web/src/screens/messages.tsx` 无虚拟化策略） |
| W0-19 | L53 编辑器（RichEditor HTML 导入导出/流式 Markdown/独立字号/Terminal） | **未实现** | `web/src/screens/composer.tsx:15-18` 显式用 `nsh/base` Input（非 RichEditor）；`messages.tsx` 用 Text + 单一 typeScale；无代码/Diff/终端独立字号；无 NSH Terminal 承载后台输出 |
| W0-20 | L55 废止旧工具名注释与兼容红线 | **未实现** | `internal/pluginpkg/manifest.go:263-270` 仍写「`example_status` 和 `spawn_subagent` must never change」；`internal/agentext/subagent.go` 仍以 `spawn_subagent` 为正式工具名——与该任务要求相反 |
| W0-21 | L57 docs/HARNESS.md 增补边界/放置/多用户 | **部分实现** | `docs/HARNESS.md:75-81` 已有「与内核的接缝」；缺放置模型与多用户升级章节（`:27-28` 仍是单租户口径） |
| W0-22 | L58 架构测试（内核不得 import placement/task/fs） | **部分实现** | `internal/architecture/boundary_test.go:11-45` 已按模式断言 kernel 包不 import 产品包与 modelcfg；placement/task/fs 包不存在，针对它们的断言无从谈起（W1/W2/W4 落地后需补） |

### W0 小结

- 已实现（勾选）：W0-1、W0-2、W0-9 —— 契约/事件词汇/键命名空间三块确实冻结。
- 真缺口集中在：工具面迁移（W0-4）、PI 偏离（W0-5）、GBASH/OAR 契约（W0-6）、多用户进程（W0-8）。
- 注意口径冲突：W0-4 要求删除 `spawn_subagent`/`mcp__`，而 W0-20 仍未做、`internal/servicehost/binding.go:143`
  还把 `spawn_subagent` 当 team 模式必需原语——需要与 `keel-agent-team-v1` 收口时一起裁决（见文末冲突登记）。

---

## W0.5 插件全景（对表 DSH）（8 条）

证据总纲：插件系统主体由归档 change `keel-plugin-core` 交付（清单/生命周期/sidecar/webview/
权限/审批/catalog），但 v2 的 layer/slots/extensibility 面未开工。清单 schema 见
`internal/pluginpkg/manifest.go`；catalog 见 `plugins/keel.plugin-market/catalog.json`（2523 行）。

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W0.5-1 | L62 冻结 catalog id/status/wave；inventory 仅附录 | **部分实现** | catalog 有 id/status/keelPluginId；**无 `wave`**（`grep -c '"wave"' catalog.json = 0`），`plugin-inventory.md:207` 自认「需补 wave 与统一 status 枚举」；inventory 已按 dfd975b 降为设计期附录（其文首 §0 裁决段） |
| W0.5-2 | L63 五种 layer 装载边界 + 架构测试；承载 kind 仍三种 | **未实现** | `layer` 只存在于 provider 插件（`manifest.go:148-156`，唯一合法值 `provider`）；无 kernel/harness/ui/service layer 边界与对应架构测试；承载 kind 仍 in-process/sidecar/webview（`manifest.go:41`） |
| W0.5-3 | L65 四层装配（base/mode/preset/config.toml）+ row id overlay | **未实现** | 现有装配是 `servicegraph` 服务图 + `loadout`（`internal/loadout/loadout.go:1-20`、`internal/servicegraph/engine.go:12-105`）；无 base/mode/preset bundle 与 row id overlay 语义；全仓无 `config.toml`（`find . -name config.toml` 为空） |
| W0.5-4 | L66 能力 seam 清单（fs/shell/subprocess/exec-surface/terminal）+ capability matrix | **未实现** | loadout 服务/能力只有 store/llm/prompt/tools/mcp/pi-loop/compaction/session-projection/telemetry/worker/sandbox/external-*（`internal/loadout/loadout.go:16-49`）；fs/shell/subprocess/exec-surface/terminal seam 不存在，无 capability matrix |
| W0.5-5 | L67 不照搬清单写入 ADR | **未实现** | `adr/` 下只有 `0001-layerfs-rust-grpc.md`；Cordis/typert、一能力一工具、70 UI 包、sandbox 族、DSH ssh 的「不做」只散见于 proposal/plugin-inventory 正文，无 ADR |
| W0.5-6 | L68 冻结扩展面声明格式（layer/slots/contributes/extensibility.json + EXTENSIBILITY.md） | **未实现** | 全仓无 `extensibility.json`/`EXTENSIBILITY.md`（`find` 为空）；`Provides` 无 `Slots`/`Contributes` 字段（`manifest.go:113-145`） |
| W0.5-7 | L70 catalog 对账（COVERAGE §A/B/C → keelPluginId；goal/todo/plan 并入 tasks、team absent、workspace 并入 Group、guard 并入 supervisor） | **部分实现** | `plugins/COVERAGE.md` §A/§B/§C 有逐行映射与 wave/状态列；但对 v2 裁决未执行：`keel.goal`/`keel.todo`/`keel.plan` 仍各自独立（COVERAGE.md:51-53），`keel.team`、`keel.workspace`、`keel.guard` 仍独立（:47,:66,:35），无「absent + 理由」的逐行处置 |
| W0.5-8 | L73 为 v1 官方插件补 extensibility 文档 | **未实现** | `plugins/keel.web-search`、`keel.context-memory`、`keel.plugin-market` 均无 `extensibility.json`/`EXTENSIBILITY.md`（各目录只有 README/plugin.yaml/sidecar.mjs） |

### W0.5 小结

catalog 已有骨架（id/status/keelPluginId/COVERAGE），但 v2 的 layer、四层装配、seam、扩展面
声明、extensibility 全套未动。**注意**：Owner 口径 #2「hooks/tools/providers/skills 同一注册面」
当前只覆盖 hooks/tools/triggers/providers（`manifest.go:113-145`）；skills 不在 `provides` 里
（见 W7-1 证据）。

---

## W1 layerfs 服务（Rust + gRPC）（6 条）

证据总纲：layerfs **只有书面件**——ADR（`adr/0001-layerfs-rust-grpc.md`）＋内核事件词汇
（`fs.write`/`fs.capture`/`fs.capture_failed` `internal/kernel/kernel.go:109-111`；键命名空间
`KeyFSWrite/KeyFSCapture/KeyFSCaptureFailed` `:232-234`）。无 `internal/layerfs`、无 Rust 代码、
无 gRPC proto、无 tree/refcount/GC 实现。现有 `internal/blob`（PG 分块 + 本地 CAS，
`internal/blob/stream.go`）是 v1 附件字节层，ADR 明说它是**回退路径**，不是 layerfs。

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W1-1 | L78 file object Put/Get/Stat + 流式读写 | **未实现** | 无 layerfs 代码；`internal/blob` 只有上传/读出的 PG/CAS 实现，无 Put/Get/Stat gRPC 面 |
| W1-2 | L79 tree object 内容寻址 + 子树共享哈希 | **未实现** | 无 tree object 实现（全仓 `grep tree_hash` 只命中 spec/ADR 文本） |
| W1-3 | L80 引用计数与 GC | **未实现** | `internal/blob` 无 refcount/GC（`blob_refs` 表是事件引用，非对象引用计数）；spec `layerfs/spec.md:61-70` 是要求 |
| W1-4 | L81 配额（group/租户字节上限） | **未实现** | `internal/quota` 只管并发 run/事件速率（`limits.go:1-25`），无存储字节配额；无 layerfs 配额错误码 |
| W1-5 | L82 gRPC 契约（流式/取消/错误分类） | **未实现** | 无 proto/gRPC 代码；ADR 只给接口名 `Put/Get/Stat/RefCount/GC` |
| W1-6 | L83 压测（1000 并发流 RSS/不整文件进内存） | **未实现** | 无 layerfs 压测脚本/用例（`scripts/` 无相关门） |

### W1 小结

0/6。这是 harness-v2 最大的一块「新服务」缺口：ADR 证据门（1000 并发流 RSS/p99/GC 停顿、
tree 去重收益、fork O(1)）一并空缺；G4/G10 也因此无法完整成立。若按 ADR 的回退条款，需 Owner
确认是回退进程内 Go CAS 还是继续 Rust 路线。

---

## W2 Harness 放置与内核契约（7 条）

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W2-1 | L87 `groups`/`session_placement` 表 + lease（join/leave/迁移） | **未实现** | `internal/store/schema.sql` 表清单（`tenants/sessions/events/checkpoints/submissions/tasks/blobs/blob_chunks/blob_refs/runs/usage_ledger/mcp_oauth_pending/model_settings/model_settings_history/tenant_secrets/mcp_oauth_credentials/plugins/plugin_audit/agents/agent_teams`，`:5-557`）无 groups/session_placement；无对应包；session lease 只有 v1 的 `sessions.owner/owner_epoch/lease_until`（`schema.sql:227-233`） |
| W2-2 | L88 三种策略 pack/user/pin | **未实现** | 无策略代码；`user`/`pin` 全仓只出现在 spec 文本 |
| W2-3 | L89 组内并发槽位（max_concurrent_runs：排队 + ETA；用户配额 DB 聚合） | **部分实现** | 进程内并发是全局信号量（`internal/agent/worker.go:169`「semaphore is the only queue」）；租户配额是进程内 per-tenant 计数（`internal/quota/limits.go:1-25`，claim 计槽、finish 归还，`AdmitRun`）；缺组级槽位、排队 ETA、DB 侧聚合配额 |
| W2-4 | L90 进程生命周期（启动/drain/接管/SIGTERM 排水；组 OOM 不扩散） | **部分实现** | SIGTERM/shutdown-vs-handoff 排水与接管已实现（`internal/httpapi/drain.go`、`internal/handoff/coordinator.go`、kernel lease/epoch `internal/kernel/lease.go`、归档 `keel-service-graph` W4 全勾）；缺「组」实体与组 OOM 隔离（无 cgroup 分组，见 W0-12） |
| W2-5 | L91 注入式 run 契约（工具清单 + system sections + message-parts + invoke 回调） | **已实现（已勾）** | `internal/agentloop/loop.go:96-140`（`Input`：`Context.SystemPrompt/Messages/Tools`、`Batch` 工具执行接线）、`agentloop/pi.go:70-110`（工具注入 loop，loop 不持账本/不触 store，见包注释 `:1-40`）；单测 `agentloop/contract_test.go:110`、`equivalence_test.go:108-143`；「invoke 回调」在实现里由注入的 `agentcore.AgentTool` 对象承担（loop 内 registry 执行），语义等价 |
| W2-6 | L92 run 快照（configHash + anchorEventSeq + toolManifestHash 写 run 事件） | **部分实现** | `configHashBefore/After` 已在 `run.finished` payload（`internal/agent/runner.go:248-253`、校验 `internal/kernel/validate.go:405-440`）；执行绑定还有 agentId/teamId/teamRevision/teamHash/promptHash（`:262-274`）；缺 `anchorEventSeq` 与 `toolManifestHash`（全仓 grep 无）——与 `keel-agent-team-v1` tasks L72「run manifest 冻结 agent/team/tool/prompt hash」同根 |
| W2-7 | L93 外部内核能力矩阵与 typed rejection（沿用 v1 external 通道） | **部分实现** | v1 通道已交付（归档 `keel-service-graph` W5 全勾）：`internal/gateway/initialize.go:81-121`（Tools/Agent 能力集、`ServerAgentCapability{Events,Cancel,Usage}`）、`internal/gateway/errors.go:124-144`（`HandshakeError` 码：invalid_result/missing_capability 等）、`internal/externalgw/agent.go:869+` 启动前握手拒绝；缺 spec 要求的 fork/resume/approval/compaction 能力位（`agent-loop-contract/spec.md:53-66`） |

### W2 小结

- 已实现（勾选）：W2-5（注入契约，v1 kernel-loop 交付，已由 `keel-service-graph` W3 制度化）。
- 放置模型（groups/lease/策略/槽位）几乎全空：这是「企业多租户/项目组」的承载单元，属产品级缺口。
- 快照缺 anchorEventSeq/toolManifestHash；外部能力矩阵比 spec 窄。

---

## W3 file-plugin 与 bash-plugin（8 条）

证据总纲：**没有 bash 工具，也没有 file 工具**。模型可见的工具注册表由
`internal/host/host.go:59-90`（builtin clock/echo + `cfg.ExtraTools` + skill 工具）＋
`internal/servicehost/services.go:190-249`（MCP tools、external tools、插件 tools 拼接）构成；
无 `bash`/`read`/`write`/`edit`。`go.mod` 无 gbash 依赖（`grep gbash go.mod` 为空）。
内核的 `fs.write`/`fs.capture`/`fs.capture_failed` 只有词汇（`internal/kernel/kernel.go:109-111`），
emitter 未落地（`internal/kernel/kernel.go:97-101` 自注「the emitters (task-log, layerfs,
placement, supervision, notifications) land in later waves」）。

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W3-1 | L97 file-plugin（read/write/edit，走 file_manifest + layerfs） | **未实现** | 无 file 工具、无 `file_manifest` 表/包；`internal/blob` 是附件字节层（`internal/blob/upload.go`） |
| W3-2 | L99 写路径 `fs.write` 事件 + 路径规范化 + `.git/objects` 不入 capture | **未实现** | 无 emitter/无 capture；kind 词汇在 `kernel.go:109`、键在 `:232`，仅此 |
| W3-3 | L100 bash-plugin（gbash 适配） | **未实现** | 无 gbash 依赖；`os/exec` 仅用于 MCP stdio（`internal/mcp/stdio_transport.go:131`）、gateway（`internal/gateway/stdio.go:481`）、hooks 命令（`internal/hooks/command.go:250`）、creation 模板（`internal/agentext/creation.go:37`），均非 bash 工具 |
| W3-4 | L101 bash timeout 参数/超时转后台/终端信息 + pid | **未实现** | 无 bash 工具（同 W3-3） |
| W3-5 | L102 JobManager（全局/每组上限；nohup/jobs/ps；收养/孤儿回收） | **未实现** | 无 JobManager/作业表；进程回收能力只存在于 gateway 外部 runtime（`internal/gateway/process_unix.go`）与插件 sidecar（`internal/agentext/plugin_sidecar.go`） |
| W3-6 | L103 输出落 layerfs（head/tail + blob 引用 + 退出码） | **未实现** | 无 layerfs（W1）与 bash 工具（W3-3）；无 blob 引用的工具结果通道 |
| W3-7 | L104 capture 默认 per-tool-call（stat 缓存 + 变更集） | **未实现** | 无 capture 代码（全仓 `grep -l capture` 只命中 spec/ADR 文本与媒体/网关的同名英文词） |
| W3-8 | L105 执行面适配器契约 materialize/capture（本地实现先行） | **未实现** | 无 `keel.exec-surface` 包/接口（`grep materialize` 只命中插件包拷贝语义 `internal/pluginpkg/package.go:151` 等无关项） |

### W3 小结

0/8。这是「操作系统的聚合工具」整块缺失；连锁影响：W4 的 bash 执行事实、W7-7 的技能脚本执行
（Owner 裁决：skills 依赖 bash）、W11 的端到端用例、G9 有界门全部阻塞。

---

## W4 task-log（plan-plugin）与面板（7 条）

证据总纲：任务卡系统**未实现**。`internal/store/schema.sql:101-128` 的 `tasks` 表是
`internal/durable` 的 PI 任务状态机（`store/tasks.go:9-15` 注释：「tasks and submissions are
ordinary bookkeeping rows, never new event kinds」），不是 `task_cards`/`task_events` 投影。
`task.*` 六个 kind 只有内核词汇与 fenced 提交面（`internal/kernel/validate.go:56-61`、
`internal/kernel/contract_v2_test.go:12-40` 的初始卡负控），**无 emitter、无投影读模型**。

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W4-1 | L109 `task_cards` 行 + `task_events` 投影（重建一致性）＋会话启动自动建初始卡 | **未实现** | schema 无 `task_cards`/`task_events` 表；无投影代码；「初始卡」只有内核 system lane 的写入能力测试（`contract_v2_test.go:12`），无运行时创建 |
| W4-2 | L110 强制 `taskId`（缺省拒绝，含子代理） | **未实现** | 无 taskId 校验路径；`tool.result` 只校验 toolCall 配对（`internal/kernel/validate.go:277-305`） |
| W4-3 | L111 `tasks` 聚合工具（8 actions） | **未实现** | 无该工具；模型面现有工具清单见 W3 总纲 |
| W4-4 | L112 任务树投影（父子/跨会话 parentTaskId） | **未实现** | 无任务树数据与读模型 |
| W4-5 | L113 资源关联 `task.attached/detached` + 面板打开 blob | **未实现** | kind 词汇在 `kernel.go:104-106`；无 emitter/面板 |
| W4-6 | L114 fork（eventSeq 原点 + 前缀引用 + 新卡） | **未实现** | 无 fork 会话创建；`subagent` 只能新开子会话（`subagent.go:175-233`），非事件的 eventSeq 前缀继承；`session_snapshot` 不存在 |
| W4-7 | L115 面板（任务图/事件流/资源预览，纯投影） | **未实现** | web 无任务板页面（`web/src/screens/` 无 tasks/task-board） |

### W4 小结

0/7。产品无法回答「这些动作是在完成什么」；G5 任务门（taskId 强制/fork 精确/资源只记引用）
全空。

---

## W5 approve 与 notification（6 条）

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W5-1 | L119 `approve` 工具（confirm/prompt/choice + 「其它」逃逸） | **未实现** | 无 approve 工具；现有是 decision 原语 + HTTP 面（`internal/agentapi/decisions.go`、`internal/agentext/decisions.go`），模型面只能靠插件工具 `creation_install` 间接触发审批（`creation.go:791-860`） |
| W5-2 | L120 逃逸值 `validated:false` provenance | **未实现** | 全仓无 `validated` 字段（`grep -rn validated --include='*.go'` 无） |
| W5-3 | L121 超时取安全侧 + durable 决策原语（open/wait/answer/timeout） | **已实现（已勾）** | `internal/agentext/decisions.go:24-32`（四状态）、`:231-301`（Open + 持久 deadline + 进程内定时器）、`:495-538`（durable normalize/expire，重启后仍生效）；持久层 `internal/agentext/decisions_store.go:13-20`（复用 tasks 行）、`:152-200`（PG 持久化）；等待/幂等/冲突 `:320-380`；安全侧证据：插件审批超时/未答 → `creation.go:845-860` 走 refused 分支；测试 `agentext_test.go:72,126,179` |
| W5-4 | L122 `notify` 工具与通知面板（多消费器 img/URL/ack） | **未实现** | 无 notify 工具/面板/消费器；web 侧 `keel.notify` 是宿主本地通知桥（`web/host.js:7-15`）且只被审批复用（`web/src/data/store.ts:108-115,1120-1128`），不是 spec 的多消费器模型 |
| W5-5 | L123 消费是持久事实 + ack 即时消费 + 原生推送兜底 | **未实现** | `notification.ack`/`notification.push_sent` kind 只有词汇（`kernel.go:133-134`），无 emitter/消费状态机 |
| W5-6 | L124 通知 URL 单次有效、不可猜；重启不重推 | **未实现** | 无通知 URL 机制（插件审批 URL 是 decision id 路由，非通知） |

### W5 小结

- 已实现（勾选）：W5-3（durable decision 原语，v1 交付、插件审批复用）。
- 缺的是「模型面 + 消费面」：approve/notify 工具、逃逸 provenance、多消费器与通知生命周期。

---

## W6 MCP 聚合工具（5 条）

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W6-1 | L128 `mcp` 聚合工具（list/describe/call + schema 发现缓存） | **未实现** | 无该工具；现有路径是 per-tool 适配（`internal/mcp/tools.go:27-68` `AgentTools` → `agenttool` registry） |
| W6-2 | L129 二进制内容走 content block（不走 stdout） | **已实现（已勾）** | `internal/mcp/content.go:60-95`：image 块与 image resource → `agentcore.ImageContent`；audio/resource_link/binary 给文本占位；structuredContent 回退 JSON；20KB 输出上限 `internal/mcp/output_limit.go`；测试 `internal/mcp/content_test.go:14-40`（含 image/resource 断言）、`output_limit_test.go` |
| W6-3 | L130 gbash 的 `mcp` CLI（与聚合工具共 registry） | **未实现** | 无 gbash（W3-3），无 CLI |
| W6-4 | L131 删除 `internal/mcp` 的 mcp__ 适配与单测 | **未实现** | `internal/mcp/naming.go:27-48`（`CreateToolName` = `mcp__<server>__<tool>`）、`tools.go:7,43-53` 仍在用；单测 `naming_test.go:17-86`、`client_test.go:598-608` 仍在 |
| W6-5 | L132 MCP server 增删不改变工具清单（清单哈希断言） | **未实现** | 相反：MCP server 的工具直接进模型面 registry（`internal/servicehost/services.go:195-205` 把 `mcpService.manager.Tools()` 并入 `ExtraTools`；`internal/host/host.go:59-90` buildRegistry）；无工具清单哈希概念（`toolManifestHash` 全仓无） |

### W6 小结

- 已实现（勾选）：W6-2（MCP 内容转换与裁剪，v1 交付）。
- 聚合工具/CLI/删除适配/清单稳定性四项全空；这与 W0-4/W0-5 是同一根因（恒定工具面未落地）。

---

## W7 skills（7 条）

证据总纲：技能**解析/发现/渐进披露**已交付（PI 移植，`internal/skills/skills.go`，测试
`skills_test.go` 对齐 PI fixtures；宿主把索引进 system prompt：`internal/host/prompt.go:65-67`、
`internal/host/host.go:191-220` 的 `skill` 工具按需加载正文）。但 v2 要求的产品面（聚合工具/
补全/learn/设置/订阅/脚本执行）全空。

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W7-1 | L136 skill 文件结构 + 资源；内容 hash 去重、版本不可变 | **部分实现** | `SKILL.md` 解析与 frontmatter 字段齐全（`skills.go:93-140`：name/description/license/compatibility/metadata/allowed-tools/disable-model-invocation）、`ContentHash`（`:338-345`）与 `Skill.Hash`（`:47-57`）；发现/递归/同名保留首个 + 告警（`skills.go:355-405,505+`）；**缺**：内容 hash 去重、版本不可变注册表（无注册表结构，Hash 只被计算未被消费） |
| W7-2 | L137 `skills` 聚合工具（ls 6 位 hash / view / manage） | **未实现** | 只有 `skill`（按 name 加载，`internal/host/host.go:193-220`）；无 ls/view/manage/sources |
| W7-3 | L138 ChatInput `$` 补全（技能/文件/任务分组） | **未实现** | `web/src/screens/composer.tsx` 无 `$` 触发/补全（只有 Picker 用于其它场景） |
| W7-4 | L139 `/learn` 沉淀技能 | **未实现** | 无 `/learn` 路径（web 命令面板无该命令，`web/src/screens/command-palette.tsx` 只列会话/导航动作） |
| W7-5 | L140 Settings `/settings/skills` + CLI `keel skills` | **未实现** | settings 页只有 agents/models/teams/capabilities（`web/src/screens/settings*.tsx`）；`cmd/keel/main.go` 无 `skills` 子命令（只有 `--skills-roots` 启动参数 `:131`） |
| W7-6 | L141 订阅源管理（信任标注/版本 pin/冲突可见） | **未实现** | 无订阅源模型/代码 |
| W7-7 | L142 技能内脚本执行必须走 bash 策略 | **未实现** | 无脚本执行路径、无 bash 工具（W3-3）；Owner 裁决要求 skills 依赖 bash（RESIDUAL-PLAN §5 #1），当前两者皆缺 |

### W7 小结

- 0 全实现；W7-1 部分（内容格式与 hash 在，注册表语义缺）。
- 技能是商业资产主线（已蒸馏上千 skill），但产品面（发现/管理/订阅/脚本）尚未成型。

---

## W8 agent-plugin 与 fork（5 条）

证据总纲：内部子代理已交付**第一版**（`keel.subagents` 插件 + `spawn_subagent` 工具 +
HTTP 面，change `keel-agent-team-v1`/rc 提交），但 v2 要求的任务卡归属、fork/resume、
config 继承、组槽位记账都没有。

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W8-1 | L146 内部子代理（同进程协程，独立会话 + 独立任务卡 parentTaskId） | **部分实现** | 同进程、独立子会话：`internal/agentext/subagent.go:175-233`（`SpawnAsync` 经 `TurnStarter` 对新建 child session 提交输入）、`:484-530`（List/Get/ParentOf）；HTTP 面 `internal/agentapi/api.go:211-213`；**缺**：独立任务卡/parentTaskId（W4 未实现） |
| W8-2 | L147 `agent` 工具（spawn/fork/resume/cancel/list/status；fork 原点 eventSeq） | **部分实现** | 只有 `spawn_subagent`（`subagent.go:623-680`，schema 只 task/agent/maxTurns/maxTokens/timeoutMs）+ HTTP list/get（`agentapi/ext.go:155-255`）；cancel 是内部 `cancelChild`（`:345`）非工具/公开动作；无 fork/resume/status 动作、无聚合 `agent` 工具、无 eventSeq 原点 |
| W8-3 | L148 子代理继承父 config 版本，随后可写自己配置 | **未实现** | 无 `config.toml`/版本概念（W0.5-3）；`SpawnRequest` 有 `AgentID`/`Selection`（`subagent.go:44-57`）可覆盖绑定，但不是「配置版本继承」 |
| W8-4 | L149 并发槽位记账（父与子都占组槽位，超限排队） | **部分实现** | 子代理是普通 run，占进程全局 worker 信号量（`internal/agent/worker.go:169`）；无「组槽位」（W2-3），无父子联合记账/排队 ETA |
| W8-5 | L150 子代理返回结构 + 产物 blob 引用；父卡追加 task.note | **部分实现** | 结构化结果在：`SubagentResult{Status,Summary,Artifacts,Usage,SessionID,...}`（`subagent.go:103-113`），Artifacts 是工具调用 trail（`Artifact{Type,Name,Ref}` 注释明说「never carries tool output text」）；**缺**：产物 blob 引用、父卡 `task.note`（W4 未实现） |

### W8 小结

0 全实现；W8-1/2/4/5 为部分（spawn 主线可用，v2 语义未闭环）。与 `keel-agent-team-v1`
（活跃板）在 Agent/Team 启动模式上重叠：其 RUNTIME-PLAN §a.5 记录 `Agent.tools[]`/团队 run 绑定
尚不可达，属同一批「运行注入」工作的另一半。

---

## W9 oar-plugin（外部工具）（5 条）

证据总纲：**OAR 专用插件不存在**（全仓 `grep -ri 'keel.oar\|oar-plugin'` 只命中本 change 文档）。
但 v1 的通用外部网关（`keel-service-graph` W5 交付）已具备能力协商、typed rejection 与进程组回收，
task 文本中的 L157/L158 按「能力在架构里可注册/可替换」口径判已实现。

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W9-1 | L154 sidecar 接入 OAR（runtime 能力矩阵、record stream seq+agentPath） | **未实现** | 无 oar 插件/record stream 实现；W0-6 的 OAR 契约也未冻结（阻塞项） |
| W9-2 | L155 外部工具渲染器（record stream 折叠视图） | **未实现** | 无渲染器；web 只有外部会话消息的基础渲染 |
| W9-3 | L156 不注入「请记录任务卡」；归属由 record stream 带外提供 | **未实现** | 无 OAR；不过该纪律已写进 spec（`design.md:244-250`），实现无从谈起 |
| W9-4 | L157 typed rejection（不支持能力启动前拒绝） | **已实现（已勾）** | 通用外部网关：`internal/gateway/initialize.go:81-121`（能力集）、`:150+`（`validateInitializeResult` 全量拒绝）、`internal/gateway/errors.go:124-144`（`HandshakeError` 码，含 `missing_capability`）；测试 `internal/gateway/conn_test.go:18,40,56`；OAR 专用绑定仍缺（见 L154） |
| W9-5 | L158 进程回收（外部 runtime 退出/崩溃孤儿清理） | **已实现（已勾）** | `internal/gateway/process_unix.go`（每 spawn 自带进程组 Setpgid；`terminateProcessTree`/`killProcessTree`/`KillTree`，含 leader 已死时按组回收）、测试 `internal/gateway/spawn_unix_test.go:18`；`internal/externalgw/agent.go` 监督/重启；OAR 专用 runtime 未接 |

### W9 小结

- 已实现（勾选）：W9-4、W9-5（通用外部网关能力，非 OAR）。
- 缺：OAR 契约冻结（W0-6）、sidecar 接入、record stream 渲染器。外部运行时是「外部工具」产品
  叙事的关键路径（Codex/Claude 接入）。

---

## W10 triggers 对齐（3 条）

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W10-1 | L162 沿用 v1 cron/interval/webhook + HMAC/去重键/漏跑补偿 | **部分实现** | 三种触发与幂等键/审计已实现：`internal/agentext/triggers.go:32-35`（TriggerKind）、`:305-430`（`Fire`：要求 key、去重、备份重试、审计）；测试 `agentext_test.go:204-340`（cron/manual dedupe/retries）；**缺**：HMAC 签名校验（全仓 `grep hmac` 无）、漏跑补偿（无 missed/catchup 逻辑） |
| W10-2 | L163 每触发器并发上限；触发会话自动建初始任务卡 | **未实现** | 只有同一幂等键的串行化（`triggers.go:345-348` 注释）；无 per-trigger 并发上限；无任务卡（W4 未实现） |
| W10-3 | L164 webhook 只绑回环/受控入口 + 审计 | **已实现（已勾）** | 默认监听 `127.0.0.1:13750`（`cmd/keel/serve.go:73` `defaultServeAddr`，`KEEL_SERVE_ADDR` 可改受控入口）；每次 fire 落审计（`triggers.go:85-110` `AuditEntry`、`:424+` record，`GET /v1/triggers` 可读）；测试 `agentext_test.go:279,328` |

### W10 小结

- 已实现（勾选）：W10-3。
- 触发「生产就绪」缺 HMAC/漏跑补偿/并发上限；触发会话的任务卡归属随 W4 一起阻塞。

---

## W11 验收（收口门，判据见 `acceptance.md`）（13 条）

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W11-1 | L170 端到端：任务创建→bash→capture→面板→fork→合并→卸载零残留 | **未实现** | 依赖 W3/W4/面板（均未实现），无法成立 |
| W11-2 | L171 外部工具：OAR 接 Codex/Claude 真链路 + 渲染器 | **未实现** | 依赖 W9-1/2 |
| W11-3 | L172 工具清单稳定性（增删 MCP/技能后哈希不变） | **未实现** | 相反：MCP/插件工具直接改模型面清单（W6-5 证据）；无清单哈希 |
| W11-4 | L173 真实模型 E2E（至少一条真 provider + 非 happy path） | **已实现（已勾）** | `scripts/real-model-e2e.sh`（成功链路 + `--with-failure`/`--failure-auth` 失败链路、`--models` 配置到 wire 断言、`--check` 凭据探测；事件序列断言见脚本头注释 `:14-30`）；文档 `docs/REAL-MODEL-E2E.md`；`acceptance-all.sh:200-210` 作为 opt-in 门 |
| W11-5 | L177 E0 既有门零回归 | **部分实现** | 全量门在且归档有绿证（见 W0-3）；v2 事件基线未重 pin（新 kind 无 emitter，暂不可见） |
| W11-6 | L179 G0 契约门 + G1 状态机门 | **部分实现** | G1 有一台真实状态机：插件生命周期 installed/enabled/disabled/failed/invalid/uninstalled（`internal/agentext/plugin_manager.go:29-49`、非法转移 `ErrPluginState:58`、测试 `plugin_manager_test.go:336-765`）；G0 终局工具集快照完全缺失（W0-4） |
| W11-7 | L182 G2 恢复门（kill -9/容器重启零人工续跑；子代理继续原任务） | **部分实现** | 「非终态 run 自动续跑」已实现：worker claim 循环 + lease 回收 + durable recovery（`internal/durable/recovery.go:1-100`）、crash matrix（`internal/agent/crash_matrix_test.go`）、`cmd/chaos`（10 分钟随机 SIGKILL，归档 W6 有报告）、handoff（归档 W4）；**缺**：子代理继续原任务（W8-2/3）、`resume.mode=never` 等 config 面（W12-2） |
| W11-8 | L183 G3 幂等门 · G5 任务门 | **部分实现** | G3：三键幂等 + 冲突 409 已实现（kernel；归档 W1 全勾），webhook 去重（W10-1）；G5：任务门全空（W4） |
| W11-9 | L184 G4 内存门（RSS ≤500MB/300MB；组 OOM 不扩散；前端 VM 配额） | **部分实现** | cgroup 2GB/1000 会话/图片通道已有（`scripts/acceptance-perf.sh:2-20,95,147-160`）；**缺**：RSS 采样协议（W0-12）、每 group 分解/组 OOM、前端 surface VM 配额断言（全仓无） |
| W11-10 | L185 G6 插件门 · G7 UI 门 · G9 有界门 | **部分实现** | G6：生命周期/零残留/依赖 PENDING/审计已交付（`plugin_manager.go`、`plugin_manager_test.go`、`plugin_manager_dependencies` 测试、归档 plugin-core），**缺 HMR 失败回滚门证据（机制在）与 extensibility.json 发布门禁**；G7：槽位机制全无（W12-9/10）；G9：MCP 20KB/流预算/worker 并发在（`internal/mcp/output_limit.go`、`internal/stream/budget.go`），**缺** bash 输出有界与作业上限（W3-4/5） |
| W11-11 | L186 G8 创造模式门（AI 现场写 Native 插件端到端） | **部分实现** | 创造模式 E2E 已有（webview 模板：`internal/agentext/creation_test.go:168`、归档 plugin-core Q3「一句话→生成→安装→UI 可开→删除」）；**缺**：Native chunk 槽位形态（W12-9/10）、macos target、卸载 diff 证据 |
| W11-12 | L187 G10 资源回收门（零孤儿/端口/VM + 证据） | **部分实现** | 插件 sidecar 进程组回收（`plugin_sidecar.go:544-570`）、gateway 进程组回收（W9-5）、drain 关停（`internal/httpapi/drain.go`）、`cmd/chaos` 有孤儿断言；**缺**：统一测试后 sweep/端口台账核对、layerfs VM（无 layerfs） |
| W11-13 | L188 G11 交付面门（HTML 报告 + before/after + 交叉复核） | **未实现** | 本 change 无 E2E 报告/审计产物（`.agents/reports/` 属主仓，本 worktree 内无）；门要求 UNCOVERED=0 |

### W11 小结

- 已实现（勾选）：W11-4（真实模型 E2E）。
- 其余 8 项为部分、4 项未实现；按 `acceptance.md:136-139` 的依赖顺序，E0→G0 未绿则后续门
  结果本就不能作为验收依据——**当前收口门不可用**。

---

## W12 插件补全（DSH 对表 + Owner 裁决，可与 W3–W9 并行）（40 条）

分层证据速览：插件注册面 = `internal/agentext/Plugin`（tools/hooks/triggers/effects/prompt，
`internal/agentext/plugin.go:24-115`）＋ `internal/pluginpkg`（manifest/provides）＋
`internal/servicegraph`（服务 DI）；builtin 插件只有 6 个：`keel.example`、`keel.subagents`、
`keel.model`、`keel.agent`、`keel.team`、`keel.creation`
（`cmd/keel/serve.go:629-641` 工厂表 + `internal/pluginpkg/builtin/*.yaml`）。
W12 清单里的 40 项绝大多数没有对应插件实体。

### 记忆卫生（W1–W2）

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W12-1 | L194 `keel.spill` | **未实现** | 无 spill 插件/机制（全仓 `grep -i spill` 只命中无关英文词与 durable 注释） |
| W12-2 | L195 `keel.compaction` 扩展（tool-result-pruner + image-offload） | **部分实现** | 压缩核心已交付：`internal/compaction/{compact,cutpoint,summary,tokens}.go` + conformance 测试 + 自动压缩接线（`internal/agent/runner.go:23-28,544-600`）；**缺**：tool-result-pruner、image-offload（全仓 grep 无） |
| W12-3 | L196 `keel.checkpoint`（何时落、保留多少） | **部分实现** | checkpoint 语义在压缩内（`internal/compaction/compact.go:60-72,157-158`、conformance 测试 `:454-518`）；**缺**：独立 checkpoint 策略（何时/保留量）插件 |
| W12-4 | L197 `keel.prompt-cache`（稳定前缀与缓存边界） | **未实现** | 只有 Anthropic usage 字段读取（`internal/provider/anthropic.go:120`）；无稳定前缀/缓存边界策略 |

### 护栏与自动恢复（W1，延伸 supervision spec）

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W12-5 | L201 `keel.supervisor` 启动即扫描非终态 run 自动恢复 | **部分实现** | 「启动续跑」机制在：worker claim 循环 + lease 回收（`internal/agent/worker.go`）、durable 恢复（`internal/durable/recovery.go:1-100`）、chaos/handoff 证据（归档 `keel-service-graph` W4/W6）；**缺**：supervisor 插件实体、升级/护栏记录、「换代」恢复编排（仅进程级） |
| W12-6 | L202 `config.toml` 策略（resume.mode/maxAttempts/backoff/onExhausted/providerFallback） | **未实现** | 无 `config.toml`（W0.5-3）；恢复参数是代码默认（`durable/types.go:421-470` `Settings{RestartLimit:5}` 等），无用户面策略 |
| W12-7 | L204 `POST /v1/sessions/{id}/resume`（幂等，可带轮转） | **未实现** | 无该端点（`grep '/resume' internal/` 无）；现有恢复靠新进程 claim |
| W12-8 | L205 子代理可续跑；父代理改配置后恢复 | **未实现** | 子代理是一次性 wait 模型（`subagent.go:235-260`）；无续跑/父改写配置路径 |
| W12-9 | L206 护栏项（重复调用/单次超时/连续失败/额度/内存压力） | **部分实现** | 已有：租户并发与事件速率配额（`internal/quota/limits.go:1-25,55-75`）、worker 租约/超时、hooks 命令 5s 超时；**缺**：重复调用提醒、连续失败护栏、额度/成本护栏、内存压力护栏（均无代码） |
| W12-10 | L207 升级阶梯（supervisor→父/主代理→owner→super-agent） | **未实现** | 无阶梯编排；`supervision.escalated` kind 只有词汇（`kernel.go:130`） |
| W12-11 | L208 `keel.super-agent` 插件 | **未实现** | 无该插件（builtin 6 个里没有；`grep super-agent` 只命中文档） |
| W12-12 | L209 每次恢复/升级/护栏触发都是账本事实 | **部分实现** | 三个 kind 已注册（`supervision.escalated`/`guardrail.tripped`/`resume.attempt`，`kernel.go:130-132`，system lane）＋对应 KeyKind；**无任何 emitter**（全仓 grep 无 `KindSupervisionEscalated` 使用点） |

### 创造模式与槽位（W1，延伸 plugin-ui-extension spec）

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W12-13 | L213 `keel.creation` 聚合本机 extensibility 文档 | **部分实现** | 插件实体在：`internal/agentext/creation.go:1-40`、manifest `internal/pluginpkg/builtin/keel.creation.yaml`、prompt section（`:134-144`）；**缺**：聚合 `extensibility.json`/`EXTENSIBILITY.md` 的目录（文档格式本身未冻结，W0.5-6） |
| W12-14 | L214 只读发现工具（本机装了什么/怎么扩展；与装配事实同源） | **未实现** | `creation_list` 只列本会话生成包（`creation.go:306-316`），不是本机插件/扩展面目录 |
| W12-15 | L215 脚手架/校验/审批安装/列举/删除（fail-closed 字段级） | **已实现（已勾）** | `internal/agentext/creation.go:86-90`（5 工具）、`:254-330`（实现）、`:375-460`（字段级 fieldFailure/problemFields）；审批 `:791-860`；测试 `creation_test.go:168,308,342`；归档 `keel-plugin-core` C1–C4/Q3 |
| W12-16 | L216 `keel.ui` 槽位机制（注册/注销/装载/schema/降级） | **未实现** | 无槽位模型（全仓无 slot 注册代码；web 侧只有 NSH 组件自身的 slot 概念） |
| W12-17 | L217 `keel.ui` 声明基础槽位（route/nav/action/tool-renderer/message-part/sidebar/settings-section/surface + tasks.widget） | **未实现** | 同 W12-16；第一方面板不是贡献物（面板是硬编码页面 `web/src/screens/`） |
| W12-18 | L221 四形态（chunk/package/iframe/data） | **未实现** | 承载 kind 只有 in-process/sidecar/webview（`internal/pluginpkg/manifest.go:41-45`）；无 chunk/package/iframe/data |
| W12-19 | L223 桥 `keel.plugin.*` ↔ NSH host.call/host.on；能力裁剪；类型化错误 | **部分实现** | 服务端桥已实现：四动词 state/callTool/requestApproval/subscribe（`internal/agentapi/plugin_bridge.go:1-40`）+ 越权拒绝/审计（`plugin_permissions.go`、`plugin_bridge_test.go`）；**缺**：NSH `host.call`/`host.on` 侧集成（`web/host.js` 无 keel.plugin.* 分派） |
| W12-20 | L224 验收：AI 现场写 Native 插件（chunk）装→渲染→卸载（web+macos） | **未实现** | 无 chunk 形态（W12-18）、无 macos target（W0-13）；现有创造模式产出是 webview 模板 |
| W12-21 | L225 `creation` 工具始终存在；停用返回 `plugin_disabled` | **未实现** | 无聚合 `creation` 工具；`creation_*` 工具随 `keel.creation` 插件启停出现/消失（`cmd/keel/serve.go:637-641`），停用即从工具面消失，无 `plugin_disabled` action 返回 |
| W12-22 | L226 TUI 降级（package/iframe 不可用、chunk 可用） | **未实现** | 无槽位形态（W12-16/18）；TUI 对插件 UI 只有 openUrl 文本兜底（归档 plugin-core T1/T2） |

### 产品完整度（W2）

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W12-23 | L230 `keel.session-title`、`keel.session-query` | **部分实现** | 标题派生在：`internal/agentapi/sessions.go:52 deriveSessionTitle`（列表标题）+ `web/src/ui/session-title.ts`；**缺**：session-title 插件实体、session-query（检索/导出全仓无） |
| W12-24 | L231 `keel.deliverables`（交付物 + workspace changes） | **未实现** | 无交付物登记/工作区变更投影 |
| W12-25 | L232 `keel.attachment` | **部分实现** | 附件能力在：`POST /v1/files` 上传/读取（`internal/httpapi/server.go:183-185`）、input `attachments`（`web/README.md:74`、`internal/httpapi/input_commit.go:70`）；**缺**：插件实体/注册面（provider files 仍归 `keel-ga`） |
| W12-26 | L233 `keel.commands`、`keel.permission`、`keel.feedback` | **部分实现** | permission 有执行点：插件权限派发/审计（`internal/agentext/plugin_permissions.go`）、hooks 决策（`internal/hooks/decision.go`）；commands 只有客户端命令面板（`web/src/screens/command-palette.tsx`，非注册面）；feedback 无 |
| W12-27 | L234 `keel.subprocess`、`keel.terminal`（provider） | **未实现** | 无共享子进程/持久终端 provider（gateway 的进程管理服务于外部 runtime，非该插件） |

### 装配与运维（W2）

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W12-28 | L238 `keel.bundles`（base/mode 装配面） | **未实现** | 无 bundle 概念（W0.5-3）；现有 `internal/loadout` 是单一默认蓝图 |
| W12-29 | L239 `keel.presets`（agent preset 覆盖） | **未实现** | 无 preset 覆盖面；agent-team 的 Team defaults 是产品配置，不是 preset 插件 |
| W12-30 | L240 `keel.hmr`（插件热重载，不打断进行中的 run） | **已实现（已勾）** | 插件热更 + 失败回滚：`internal/agentext/plugin_manager_ops.go:968-1071`（Update/stageUpdateLocked/restoreLocked）、测试 `plugin_manager_test.go:564`；不打断在飞 run：servicegraph 代际 RETIRING 保留引用者（`internal/servicegraph/lifecycle.go:12-25`、`engine.go:647-669`、`retire_test.go`），run 的工具面是注册表快照（`internal/agenttool/registry.go:93-108` Subset 拷贝）。注：无同名插件实体，按「能力在架构里可注册/可替换」口径勾选 |
| W12-31 | L241 `keel.telemetry`（指标 + OTel） | **部分实现** | telemetry 契约/noop/memory/Prometheus 指标在（`internal/telemetry/{telemetry,noop,memory,metrics}.go`、`internal/metrics/metrics.go`）；**缺** OTLP adapter（`docs/PI-CONFORMANCE.md:267`、`docs/HOST.md:51` 明说未实现）——Owner 裁决要求「全面支持 OTLP，默认关」，此项为真缺口 |
| W12-32 | L242 `keel.testkit`（无密钥测试台/mock-replay） | **部分实现** | 无密钥测试基建在：fake provider（`internal/agent/fake.go`、`internal/provider/demo.go`）、hermetic E2E（`cmd/e2e`）、录制式 conformance；**缺**：testkit 插件实体、mock-replay 工具 |
| W12-33 | L243 `keel.credentials`、`keel.storage`、`keel.identity` | **部分实现** | 能力在核心层：租户密钥库（`internal/store/secrets.go`，`secret:NAME` 引用链路已线上实证见 RESIDUAL-PLAN §4）、PG 存储（`internal/store`）、租户/认证（`internal/store/tenants.go`、`cmd/keel/auth.go`）；**缺**：三者都不是可注册插件（provides 面） |

### 能力接入

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W12-34 | L247 `keel.web-search`（复用 v1 已交付） | **已实现（已勾）** | `plugins/keel.web-search/`（plugin.yaml + sidecar.mjs，五上游回退链）；`plugins/COVERAGE.md:65` 标记 delivered；归档 `keel-plugin-core` O3 |
| W12-35 | L248 `keel.oar` | **未实现** | 无 oar 插件（W9） |
| W12-36 | L249 登记 lsp/browser-use/computer-use/document/knowledge/ptc/workflow | **已实现（已勾）** | catalog 均有 `keelPluginId` 登记：`keel.lsp`（`catalog.json:1352`）、`keel.browser-use`（:538）、`keel.computer-use`（:923）、`keel.document`（:1038）、`keel.knowledge`（:2163）、`keel.ptc`（:1454）、`keel.workflow`（:660），状态 planned/registered（按「登记」口径完成） |

### UI 贡献（W1–W2）

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W12-37 | L254 ui-task-board/notifications/jobs/tool-renderer/skills | **未实现** | 无这些插件；`catalog.json` 里只作为 DSH 包名出现（`dsh.client` 覆盖条目 `:789-880`），无 keel 插件实体 |
| W12-38 | L255 ui-subagent/deliverables/secret-mask/creation | **未实现** | 同上（catalog 仅在 DSH 包名列表中出现） |
| W12-39 | L256 登记 ui-trajectory | **未实现** | `ui-trajectory` 只出现在 `catalog.json` 的 DSH `client` 包名数组（`:815-880` 段），没有自己的 keel 插件登记行 |

### 明确不做

| ID | 原任务（行号） | 状态 | 证据 / 说明 |
|---|---|---|---|
| W12-40 | L260 不新增 agent-team / goal / plan-mode / todo 独立插件；不照搬 DSH ssh/Cordis | **已废弃（Non-Goal 声明，无需实现）** | `proposal.md` Non-Goals「agent-team」「goal/plan-mode/todo 独立插件」§;`plugin-inventory.md` §0/§1.1 六条不借鉴；插件实体侧确无这些插件。**注**：`keel-agent-team-v1` 新增的是 Agent/Team 产品面（另一 change），与「不新增 agent-team 子代理协调插件」不冲突但口径需 Owner 确认（见文末冲突登记） |

### W12 小结

- 已实现（勾选）：W12-15（creation 工具链）、W12-30（HMR 机制）、W12-34（web-search 插件）、
  W12-36（登记）。
- 真缺口最重：supervisor/resume/护栏/阶梯（7 项）、槽位与四形态（7 项）、
  产品件（deliverables/commands/feedback/subprocess/terminal）、bundles/presets、OTLP。

---

## 2. 生态板去向对账：`openspec/changes/keel-ecosystem/tasks.md`（冻结板，68 条未勾）

> 纪律：**未改动该文件一个字符**（其文首 2026-11-14 注记来自另一条线；本对账只读）。
> 板面分类：
> - **已承接**：由已归档 change 交付（`keel-service-graph` 为主，另含 PI 移植/`keel-plugin-core`
>   等既有交付），给归档 tasks 条目 + 代码/测试证据。
> - **已转交**：由**活跃** change（`keel-ga`）承接，尚未实现（RESIDUAL-PLAN §2 明确归口）。
> - **孤儿项**：无任何交付、无活跃任务板承接——真缺口。
> - **已废弃**：被 Owner 2026-11-14 裁决或新口径取代。

### 0. PI conformance 基座

| 行 | 原任务 | 去向 | 证据 |
|---|---|---|---|
| 18 | 录制/固化 PI 事件流 golden | 已承接 | 导出 conformance 套件：`internal/agentloop/agentlooptest/`（23 用例）、`internal/loop/conformance_test.go`（事件顺序/配对/取消/transcript 等价）；`scripts/conformance.sh`＋`docs/PI-CONFORMANCE.md`；归档 keel-service-graph W3「导出套件并记录清单 hash」 |
| 20 | AgentTool 形状对照测试 | 已承接 | `agentloop/conformance_suite_test.go`（任意 loop 实现跑套件）、`agentlooptest/suite.go:1-40`；归档 keel-service-graph W3 |
| 21 | CompactionEntry/Checkpoint 语义对照 | 已承接 | `internal/compaction/conformance_test.go:163-811`（范围选择/阈值/摘要失败/checkpoint `:454-518`） |
| 22 | TelemetryContext/Span/Metrics 形状对照 | 已承接 | `internal/telemetry/conformance_test.go:16-400`（capabilities/propagation/parentage/detach） |
| 23 | CI conformance job | 已承接（弱） | `.github/workflows/ci.yml:46-49` `go test -race ./...` 覆盖 conformance 包；无独立 job（升级 PI pin 靠人工跑 `scripts/conformance.sh`） |

### 1. observability

| 行 | 原任务 | 去向 | 证据 |
|---|---|---|---|
| 27 | PI telemetry 契约 | 已承接 | `internal/telemetry/telemetry.go`（Context/Span/Store/Metrics 接口）、`store.go` |
| 29 | noop/memory adapter | 已承接 | `internal/telemetry/noop.go`、`memory.go`（含测试） |
| 30 | run=span；turn/tool/compaction 子 span；message_* span event | **孤儿（部分）** | 仅有 run span（`internal/agent/telemetry.go:35`）与 provider span（`internal/agent/provider_telemetry.go:97`）；turn/tool/compaction 子 span 与 message_* span event 未接线（loop 内 telemetry 是自累加器 `internal/loop/telemetry.go:31-140`，不是 span 树） |
| 31 | run.finished 汇总事件 | 已承接 | `internal/agent/runner.go:226-274`（RunFinishedPayload：turns/tokens/toolTimings/context/model/duration/stopReason/stopReason）；写入 `internal/agent/worker.go:392` |
| 32 | provider usage 归一（OpenAI/Anthropic/Responses） | 已承接 | `internal/provider/usage_test.go`（三协议 usage 归一断言） |
| 33 | Prometheus adapter（runs/turns/tool/context/queue/lease/stream/pgxpool/go） | **孤儿（部分）** | `internal/metrics/metrics.go`（Prometheus 文本面）+ 已交付 runs/queue/lease/stream/pgxpool/go（板内自注）；turns/tool/context 未接（`docs/MATURITY.md`/板内注） |
| 37 | OTLP adapter（默认关） | **孤儿** | 无 OTLP 实现（`docs/PI-CONFORMANCE.md:267`、`docs/HOST.md:51` 自认未实现）；Owner 2026-11-14 裁决「全面支持 OTLP，默认关」——真缺口 |

### 2. recoverability

| 行 | 原任务 | 去向 | 证据 |
|---|---|---|---|
| 41 | 对齐 PI durable（GenerationTask/CompactionTask/retry/progress） | 已承接 | `internal/durable/generation.go:14-260`（GenerationCheckpoint/重试/checkpoint）、`types.go:412-471`（ProgressPolicy/Settings/RestartLimit） |
| 42 | 启用 tasks 表（kind/state/attempts/restart_limit/lease） | 已承接（小偏差） | `internal/store/schema.sql:101-128`（tasks：kind/state/restarts/status/owner/…）＋ `store/tasks.go`（Create/Get/Save）；attempts/lease 在 `runs` 行（`schema.sql:228-233`），不在 tasks |
| 43 | generation_id 入 assistant 事件、恢复幂等复用 | 已承接（形式差异） | generationId 是 commit 幂等键（`internal/kernel/kernel.go:222-223`、`internal/agent/runner.go:673`），落 `events.dedupe_key`（`schema.sql:59-75` 唯一索引），非 payload 字段；归档 keel-service-graph W1「Commit 幂等：requestId/generationId/toolCallId」 |
| 44 | 工具结果 `(tenant,session,tool_call_id)` 唯一约束 | 已承接 | kernel 工具配对校验（`internal/kernel/validate.go:277-305`）+ events dedupe 唯一索引；归档 keel-service-graph W1「工具配对；孤儿/重复结果拒绝」 |
| 45 | 事件追加幂等 | 已承接 | `AppendEventDedup`/`dedupe_key` 唯一索引（`internal/store/events.go:59-75`）；归档 keel-service-graph W1 |
| 46 | checkpoint 回放（PI compaction entry + checkpoint） | 已承接 | `internal/compaction/compact.go:60-72,157-158`＋`RebuildContext`；`internal/agent/messages.go:43-104` 重建 |
| 47 | 崩溃矩阵五格（模型流/工具/追加/压缩/SSE） | **孤儿（部分）** | 模型流/工具/追加已有：`internal/agent/crash_matrix_test.go`（assistant 提交前后）、`internal/handoff/safepoint_test.go:20-340`（消息后/工具调用后/工具结果后/模型流中/工具执行中）；**压缩中与 SSE 两格未单列崩溃用例** |
| 48 | nightly 10 分钟随机 SIGKILL 混沌 | **孤儿** | CI `chaos-nightly` 是占位 TODO（`.github/workflows/ci.yml:62-71`「TODO: run cmd/e2e random-kill matrix for 10 minutes」）；`cmd/chaos` 可跑 10 分钟但未进 nightly |
| 49 | G2 证据（0 丢事件/0 重复最终回复/收敛） | 已承接 | `cmd/chaos`（kill/handoff 双模式，不变量检查 `cmd/chaos/main.go:1-30`）＋归档 keel-service-graph W6「混沌…全绿」 |

### 3. compaction

| 行 | 原任务 | 去向 | 证据 |
|---|---|---|---|
| 53 | 对齐 DEFAULT_COMPACTION_POLICY | 已承接 | `internal/compaction/conformance_test.go:397`（ThresholdPolicy）、`compact.go`/`cutpoint.go` 对齐 PI 语义 |
| 54 | 不在 tool 中间切；不压缩最后一条 user | 已承接 | `internal/compaction/cutpoint.go:39`（toolResult 不可切）＋ conformance 用例 |
| 55 | 摘要走 provider；失败保留原上下文并记录 | 已承接 | `internal/compaction/summary.go`；失败路径 `conformance_test.go:660`（失败写 no checkpoint） |
| 56 | compaction entry + checkpoint tokensBefore/After | 已承接 | `compact.go:142-161`（CompactionMessage/TokensBefore）；`conformance_test.go:454-518` |
| 57 | 恢复从 checkpoint + tail 重建；历史可读 | 已承接 | `internal/agent/messages.go:88-104`（compaction 事件→CompactionMessage）；`RebuildContext` |
| 58 | 手动 `POST /v1/sessions/{id}/compact` | **孤儿** | 无该端点（`grep compact internal/httpapi` 无路由） |
| 59 | 测试：超阈值一次/重复不叠加/回放一致 | 已承接 | `conformance_test.go:613-636`（第二次压缩折入第一次摘要）、`loop_compaction_test.go`、`agent/messages.go` 回放 |

### 4. skills

| 行 | 原任务 | 去向 | 证据 |
|---|---|---|---|
| 63 | Agent Skills 规范解析（全 frontmatter 字段） | 已承接 | `internal/skills/skills.go:93-140,164-310`；测试 `skills_test.go:254-530`（PI 全部 fixture） |
| 65 | 目录发现（~/.agents、.agents、递归 SKILL.md） | 已承接 | `skills.go:355-470`（Discover/DefaultRoots/scanDir 递归）；测试 `skills_test.go:125-217` |
| 66 | 系统提示只放 name+description+path；`skill` 工具按需加载 | 已承接 | `internal/host/prompt.go:65-67`、`skills.go:576-612`（FormatForPrompt）、`internal/host/host.go:191-220`（SkillTool） |
| 67 | `/skill:name` 强制加载 + enableSkillCommands | **孤儿** | 无 slash 命令注册表/强制加载路径（web 命令面板是客户端动作，非技能通道） |
| 68 | 租户注册表：内容 hash 去重、版本不可变、同名冲突告警 | **孤儿（部分）** | Hash 计算在（`skills.go:47-57,338-345`）、同名冲突保留首个 + 告警在（`:505-560`）；**无注册表**：hash 无消费点、无版本不可变语义、无按租户注册 |
| 69 | `allowed-tools` 与工具集取交集；越权拒绝 | **孤儿（部分）** | 解析在（`skills.go:275-295`）；无执行期交集/拒绝（`grep AllowedTools` 无宿主/loop 使用点） |
| 70 | 脚本/资源默认不执行 | **已废弃** | Owner 2026-11-14 裁决：skills 可执行脚本**默认允许**（可选安全模式），依赖 bash-plugin；见 `docs/RESIDUAL-PLAN.md` §5 #1——本条旧口径被取代 |
| 71 | 热更新：下一轮生效；运行中 run 不受影响 | **孤儿** | skills 只在 host 构建/`RefreshSections` 时发现（`internal/host/host.go:46-54,100-130`），无文件监听/下一轮刷新路径 |
| 72 | G4：真实上游 SKILL.md 零改动加载 | 已承接 | `skills_test.go` 直接以 PI 上游 fixture/testdata 驱动（`testdata/skills/**`），file:line 见上 |

### 5. mcp

| 行 | 原任务 | 去向 | 证据 |
|---|---|---|---|
| 76 | McpClient（JSON-RPC/初始化/能力/超时/取消/server 请求） | 已承接 | `internal/mcp/client.go`、`protocol.go`、`streamable_http.go`、`stdio_transport.go`；测试 `client_test.go`（含 `TestClientRequestTimeout:389`） |
| 77 | 传输：stdio/Streamable HTTP/Pipe/内存；拒绝 legacy SSE | 已承接 | `internal/mcp/stdio_transport.go`、`streamable_http.go`、`memory_transport.go`、`transport.go`；归档 keel-service-graph W5 外部网关复用同一传输层 |
| 83 | 宿主 per-tenant MCP manager 缓存 | **孤儿** | 全进程单个 `mcp.Manager`（`internal/servicehost/services.go:293-320` `mcpService`），无 tenant 维度缓存；板内自注「当前 Manager 单租户」 |
| 85 | 命名 `mcp__<server>__<tool>`（替换/冲突 hash/64 上限） | **已废弃** | v1 已实现（`internal/mcp/naming.go:27-86`＋测试）；被 harness-v2 裁决删除——`specs/tool-surface/spec.md` 迁移表「`mcp__<server>__<tool>` → `mcp.call`，删除；全仓只允许在偏离登记与测试夹具中出现」＋ proposal「禁止硬编码工具名」 |
| 86 | normalizeMcpInputSchema/toLlmContent/20KB/progress | 已承接 | `internal/mcp/input_schema.go`、`content.go`、`output_limit.go`；测试 `input_schema_test.go`、`content_test.go`、`output_limit_test.go` |
| 87 | 租户 mcp_servers：mcp.json 兼容/env·headers 加密/日志脱敏 | **孤儿（部分）** | mcp.json 兼容与 env/headers 解析在（`internal/mcp/config.go:6-7,120-144`、`configfile.go`）；**加密** 指 OAuth 凭据加密（`internal/store/mcp_oauth.go` AES-256-GCM，归档 keel-tenancy-usage 已交付），但 server env/headers 本身未加密落库、无脱敏测试清单 |
| 88 | 连接池/健康检查/重连退避；崩溃不拖垮 run | **孤儿（部分）** | 有 `Manager.Reconnect`（`internal/mcp/manager.go:142-165`）被 OAuth 回调使用；无连接池/健康检查循环/退避策略；「server 崩溃不拖垮 run」无专用测试 |
| 89 | 破坏性工具默认需 hook/审批 | **孤儿** | 无该策略（hooks 可 deny，但非默认挂到 MCP 破坏性工具） |
| 90 | G4：真实 MCP server（filesystem/git）零改动接入 | **孤儿** | 测试用内存/fake 传输与内存 MCP server（`internal/mcp/memory_transport.go`）；无真实 filesystem/git server 接入证据 |

### 6. hooks

| 行 | 原任务 | 去向 | 证据 |
|---|---|---|---|
| 94 | 全事件名（before_agent_start…session_compact_failed） | 已承接 | `internal/hooks/events.go:46-62` 全量常量；测试 `runner_test.go:33`（事件名与顺序） |
| 97 | 进程内 Go：defineExtension/hook/section/wrapTool 语义 | 已承接 | `internal/hooks/registry.go`、`tool.go`（ToolWrapper）、`command.go`（section/扩展装载）；测试 `tool_test.go:12-213` |
| 98 | 命令钩子（JSON stdin/stdout；5s 超时；失败策略可配） | 已承接 | `internal/hooks/command.go:23-28`（FailurePolicy）、`:240-260`（exec + timeout）；测试 `command_test.go:14-300`（含超时/失败策略） |
| 99 | 决策 continue/deny/修改（PrepareRequest） | 已承接 | `internal/hooks/decision.go:11-40`（Action 词汇）、`boundary.go:76-92`、`tool.go`（modify input/result patch）；测试 `runner_test.go:170-199`、`tool_test.go:60-119` |
| 100 | 审计 `hook.executed`（不进上下文） | 已承接 | `internal/hooks/decision.go:154-170`（audit hook.executed，不进 model context） |
| 101 | 测试：deny 阻止/修改生效/超时不挂 run/观察失败不阻断 | 已承接 | `command_test.go:14-116`（deny/timeout）、`tool_test.go:60-119`（deny/modify）、`runner_test.go`（fail-open/closed 策略） |

### 7. provider 文件

| 行 | 原任务 | 去向 | 证据 |
|---|---|---|---|
| 105 | 事件只存 blobId；provider 能力位与 resolver | **已转交 keel-ga（活跃板，未实现）** | RESIDUAL-PLAN §2「Provider 文件（blobId/adapter/缓存）：keel-ecosystem §7 ↔ keel-ga §3 → 归 keel-ga provider-files」；当前 `agentcore` 无 `ImageRefContent`/`BlobID`（grep 无） |
| 106 | Gemini/OpenAI Responses/Anthropic adapter | **已转交 keel-ga（未实现）** | 同上；设计稿 `docs/FILE-PROTOCOL.md` |
| 107 | provider file 缓存 + TTL | **已转交 keel-ga（未实现）** | 同上 |

### 8. 收口

| 行 | 原任务 | 去向 | 证据 |
|---|---|---|---|
| 111 | 2GB cgroup：1000 SSE + 64 run + 1MiB 图片，RSS<1.8GB | 已承接 | `scripts/acceptance-perf.sh:2-20,95`（1000 sessions + image 1MiB；64 并发 run 见归档 keel-service-graph W6 条目「2GB/1000/64（A/B 对照无退化）」） |
| 112 | 10 分钟混沌报告 | 已承接 | `cmd/chaos`（kill/handoff）+ 归档 keel-service-graph W6「10 分钟随机 SIGKILL… 0 丢事件/0 重复最终回复」 |
| 113 | /v1/ready、SIGTERM 排水、Dockerfile、compose、CI（nightly chaos） | 已承接（夜更除外） | `/v1/ready`（`internal/httpapi/ready.go`）、排水（`drain.go`）、`Dockerfile`/`docker-compose.yml`、CI（`.github/workflows/ci.yml`）齐；nightly chaos 是 TODO 占位 → 已登记为孤儿（本表行 48） |
| 114 | 公开 API 面评审（是否抽 `pkg/`） | **已废弃** | Owner 2026-11-14 裁决「稳定 SDK：暂不抽（主代理决策）……GA 前再评估」——RESIDUAL-PLAN §5 #6 |
| 115 | 文档：每个能力的 spec + 示例 + runbook | **孤儿（部分）** | 已有 KERNEL/HARNESS/LOADOUT/HANDOFF/RUNBOOK/SERVICE-GRAPH/DSH-REFERENCE 等；但 layerfs/fs/shell/tasks/skills 产品面等能力无 spec/示例（随 W1–W8 一起缺） |
| 116 | 子代理交叉复核；HTML 报告 + 公网链接 | 已承接 | 归档 keel-service-graph W6「HTML 报告：…公网链接 alpha-keel-service-graph.fde2e.com/report.html」＋四轮复核记录 |
| 117 | 归档本 change | **已废弃** | 本板 2026-11-08 已冻结、被 `keel-service-graph` 取代（服务图已归档）；ecosystem 保留为历史对照，不再单独归档（其文首注记同口径） |

### 9. 待 Owner 决策（阻塞对应工作流）

| 行 | 原任务 | 去向 | 依据 |
|---|---|---|---|
| 121 | Skill 是否允许可执行脚本 | **已废弃（裁决已下）** | Owner 2026-11-14：**默认允许**（可选安全模式）；skills 依赖 bash——RESIDUAL-PLAN §5 #1 |
| 122 | MCP v1 是否包含 stdio | 已承接（裁决已下 + 已实现） | Owner 2026-11-14：**含 stdio**；实现 `internal/mcp/stdio_transport.go` + 测试 `stdio_transport_test.go` |
| 123 | Hook 是否两者都做 | 已承接（裁决已下 + 已实现） | Owner 2026-11-14：**必须完备**；进程内（`hooks/registry.go`）与命令钩子（`hooks/command.go`）都已实现（§6） |
| 124 | 可观测是否上 OTLP（默认关） | **已废弃（裁决已下；实现归 harness-v2 W12-31）** | Owner 2026-11-14：**全面支持 OTLP、默认关，架构不留死路**；实现缺口记 W12-31/孤儿行 37 |
| 125 | 是否抽 `pkg/` 稳定 SDK | **已废弃** | Owner 2026-11-14：暂不抽——行 114 同据 |
| 126 | PI pin 升级策略 | **已废弃** | Owner 2026-11-14：跟随上游，每次升级独立 change（pin + conformance + PI-PARITY）——RESIDUAL-PLAN §5 #7 |

### 生态板汇总

- **已承接 41 条**（由已归档 change 交付：`keel-service-graph` 为主，另含 PI 移植/`keel-plugin-core` 等既有交付补齐 conformance/compaction/skills/mcp/hooks）。
- **已转交 3 条**（keel-ga provider-files：行 105/106/107，尚未实现）。
- **孤儿 16 条**：行 30（span 树）、33（turns/tool/context 指标）、37（OTLP）、47（压缩/SSE 崩溃格）、
  48（nightly chaos）、58（手动 compact 端点）、67（/skill:name）、68（技能注册表）、
  69（allowed-tools 执行期交集）、71（技能热更新）、83（per-tenant MCP manager）、
  87（mcp env/headers 加密）、88（MCP 健康检查/退避）、89（破坏性 MCP 默认审批）、
  90（真实 MCP server 接入）、115（每能力文档）。
- **已废弃 8 条**：行 70、85、114、117、121、124、125、126（依据 Owner 2026-11-14 裁决或
  harness-v2 新口径）。
- 统计口径：41 + 3 + 16 + 8 = 68。若只算「已交付」，则已承接 41；「已承接（含活跃板转交）」= 44。

### 归档 change `keel-service-graph` 的 2 个未勾缺口（并入真缺口清单）

| ID | 缺口 | 现状证据 | 性质 |
|---|---|---|---|
| SG-1 | DSH sidecar 参考适配器：pin `@deepseek-ai/dsh` 0.2.1-rc.5 + Node 22；拉起/停止、env/凭据清洗、reaper、崩溃退避、资源预算（真实 DSH 手测与部署契约未进 CI） | 参考 sidecar 已交付：`cmd/keel-dsh-ref`（`-kind=agent|tool`）、`internal/dshref/dshref.go`（进程组/回收/env 清洗，测试 `dshref_test.go`）；文档 `docs/DSH-REFERENCE.md` 自注「真实 DSH 0.2.1-rc.5 手测与资源预算部署契约…未进 CI」 | 真缺口（加固/CI 化），已由 RESIDUAL-PLAN §2 并入 Wave 4 插件波 |
| SG-2 | gateway 补充故障 scenario：crash-loop/重启风暴、大帧取消、stderr 洪泛、双租户同名工具、provider 图片内容 | 已覆盖 crash-loop/重启风暴、大帧/帧上限（`internal/gateway` 套件 + `servicegraph` 生命周期测试，归档 tasks L141 自注）；**未覆盖**：双租户同名工具、provider 图片内容（归档 tasks L143 自注「登记为后续加固」） | 真缺口（两格），已由 RESIDUAL-PLAN §2 并入 Wave 3 可恢复性 |

---

## 3. 真缺口 TOP10（按对产品价值排序）

排序依据：v2 的产品承诺（恒定工具面 / 任务记忆 / 统一环境 / 默认自愈 / 一切皆插件）×
当前架构里「可注册、可替换」的缺失程度。

| # | 缺口 | 一句话理由 | 承载任务 |
|---|---|---|---|
| 1 | **恒定工具面与聚合工具**（bash/mcp/skills/tasks/approve/notify/creation/agent + read/write/edit） | 模型面现在只有 clock/echo/skill/spawn_subagent/MCP per-tool；没有聚合工具就不存在 v2 的「能力经 action 进入」，W3–W8 与 G0 全部悬空 | W0-4、W3、W4-3、W6-1、W7-2、W8-2、W12-21 |
| 2 | **task-log**（task_cards/task_events 投影、强制 taskId、tasks 工具、fork） | 产品回答不了「这些动作是在完成什么」；G5 任务门全空；子代理/触发器/交付物都缺归属根 | W4、W10-2、W12-24 |
| 3 | **layerfs + file-plugin**（内容寻址字节层、read/write/edit、capture/写穿） | 「一切皆文件」的字节层缺位；fork O(1)、资源 (path,hash)、十万文件 capture 一条事件全无落点；ADR 证据门也无从成立 | W1、W3-1/2/6/7/8、W0-7 |
| 4 | **bash-plugin + JobManager**（gbash、timeout 转后台、head/tail+blob、作业收养） | 「操作系统的聚合工具」整块缺失；Owner 裁决的技能脚本执行（默认允许）也依赖它 | W3-3/4/5、W7-7、W11-1 |
| 5 | **approve + notify 工具与通知消费链**（三模式/逃逸 provenance/多消费器/URL 生命周期） | 人类在环只有插件审批一条旁路；模型无 approve/notify 动作，通知没有持久消费事实 | W5-1/2/4/5/6 |
| 6 | **MCP 聚合工具 + 删除 mcp__ 适配** | 挂一个 MCP server 工具清单就变（清单哈希无概念），与「恒定工具面/禁硬编码名」直接冲突；PI-PARITY 偏离也未登记 | W6-1/4/5、W0-4/5 |
| 7 | **skills 产品面**（聚合工具 ls/view/manage、$ 补全、/learn、settings/CLI、订阅源） | 技能是商业资产主线（已蒸馏上千 skill），现在只有「按名加载正文」；注册表/hash 不可变语义也缺 | W7-2…7、W0.5-6 |
| 8 | **supervisor/resume/护栏闭环**（resume.mode、/resume、子代理续跑、升级阶梯、super-agent） | 「默认自愈」是 v2 核心卖点；现在只有进程级 crash 恢复，配置化/子代理续跑/阶梯全缺 | W12-5…12、W11-7 |
| 9 | **放置模型**（groups/session_placement/lease、pack/user/pin、max_concurrent_runs+ETA） | 企业多租户/项目组隔离的承载单元不存在；只剩全局 worker 并发与进程内租户配额 | W2-1/2/3、W8-4、W11-9 |
| 10 | **插件 UI 槽位与创造模式 Native chunk**（keel.ui、四形态、TUI 降级、AI 现场写插件） | 「一切皆插件 + 创造模式」缺最后一公里：面板不可贡献、AI 只能产 webview 模板、G7/G8 门不成立 | W12-16…22、W11-10/11 |

未入 TOP10 但需显式记账的真缺口：OAR 专用接入（W9-1/2/3）、NSH 0.5.0 客户端壳与自更新
（W0-14/15/16/17）、长会话性能与 RichEditor（W0-18/19）、RSS 500MB 门（W0-12）、
OTLP adapter（W12-31/生态行 37）、provider files（生态行 105–107，归 keel-ga）、
DSH sidecar CI 化与 gateway 两格故障 scenario（SG-1/SG-2）。

## 4. 冲突登记（需 Owner / 主线裁决）

1. **`spawn_subagent` 去留冲突**：harness-v2 W0-4/W0-20 要求删除旧名且废止「兼容红线」；
   但代码把 `spawn_subagent` 当 team 模式必需原语（`internal/servicehost/binding.go:138-143`
   `SpawnToolName = "spawn_subagent"`，`binding_test.go:104,159`），`keel-agent-team-v1` 的
   运行注入线正在该文件工作。建议：以 `agent.spawn`/`agent` 聚合工具为终局，同步改
   agent-team 的默认工具注入与 team 校验（不能只改展示名）。
2. **`mcp__` 命名冲突**：harness-v2 迁移表要求删除；`docs/PI-PARITY.md:48` 仍把它当对齐项，
   ecosystem 行 85 曾按 v1 口径实现。以 Owner 2026-11-11 恒定工具面裁决为准 → 需补
   PI-PARITY deviation 段（W0-5）并删适配。
3. **agent-team 口径**：W12-40「不新增 agent-team」指子代理协调插件；`keel-agent-team-v1`
   新增的是 Agent/Team 产品面。两者不必然矛盾，但任务板措辞需 Owner 确认（本文件按 Non-Goal 记）。
4. **run 快照缺口的归属**：W2-6 缺的 `anchorEventSeq/toolManifestHash` 与 `keel-agent-team-v1`
   tasks L72「run manifest 冻结 agent/team/tool/prompt hash」是同一批工作；建议合并到运行注入线
   （其 RUNTIME-PLAN 已给出 file:line 现状），避免两处实现。
5. **layerfs 路线待定**：ADR 的回退条款（进程内 Go CAS）与「继续 Rust」之间尚无证据门结论
   （W1 全空）。按 Owner 2026-11-14 架构口径「先补注册面再补能力」，可考虑把 layerfs 排在
   file/bash 插件之后以获得证据门。

## 5. 本次勾选明细（tasks.md 变更 = 15 处：14 条「已实现」＋ 1 条「已废弃」声明）

| # | tasks.md 条目 | 勾选证据（行内注记已同步） |
|---|---|---|
| 1 | W0-1 冻结 10 份 spec | `specs/*/spec.md` ×10 |
| 2 | W0-2 kind 表 | `kernel.go:99-135`、`validate.go:44-77`、`store/events.go:24-47`、`runner.go:248-253` |
| 3 | W0-9 内核键命名空间 | `kernel.go:217-243,325-370`、`validate.go:181-205`、`contract_v2_test.go:62,95` |
| 4 | W2-5 注入式 run 契约 | `agentloop/loop.go:96-140`、`pi.go:70-110`、`contract_test.go:110` |
| 5 | W5-3 durable 决策原语 | `agentext/decisions.go:24-32,231-301,495-538`、`decisions_store.go` |
| 6 | W6-2 MCP 二进制内容走 content block | `mcp/content.go:60-95`、`content_test.go:14-40` |
| 7 | W9-4 typed rejection | `gateway/initialize.go:81-121,150+`、`errors.go:124-144`、`conn_test.go:18,40,56` |
| 8 | W9-5 外部 runtime 进程回收 | `gateway/process_unix.go`、`spawn_unix_test.go:18`、`externalgw/agent.go` |
| 9 | W10-3 webhook 回环 + 审计 | `serve.go:73`、`triggers.go:85-110,424`、`agentext_test.go:279,328` |
| 10 | W11-4 真实模型 E2E | `scripts/real-model-e2e.sh`、`docs/REAL-MODEL-E2E.md` |
| 11 | W12-15 creation 工具链 | `creation.go:86-90,254-330,375-460`、`creation_test.go:168,308,342` |
| 12 | W12-30 HMR 机制 | `plugin_manager_ops.go:968-1071`、`servicegraph/lifecycle.go:12-25`、`registry.go:93-108` |
| 13 | W12-34 keel.web-search | `plugins/keel.web-search/`、`COVERAGE.md:134` |
| 14 | W12-36 登记 7 个 plan 插件 | `catalog.json:538,660,923,1038,1352,1454,2163` |
| 15 | W12-40「明确不做」Non-Goal（按已废弃勾选） | `proposal.md` Non-Goals、`plugin-inventory.md` §0/§1.1；冲突登记第 3 条 |

其余 127 条保持未勾（部分/未实现），逐条状态与证据见上表各 W 分节；
无「无法判断」条——每条都给了代码/测试/spec/归档条目锚点。

### 对账方法与边界

- 阅读顺序：`tasks.md` → `acceptance.md` / `proposal.md` / `design.md` / 10 specs →
  按任务路径线索 grep 代码与测试（`internal/...`、`cmd/...`、`scripts/...`、`plugins/...`、
  `web/...`）→ 与 `keel-agent-team-v1` / `keel-ga` / `keel-service-graph`（归档）/
  `keel-plugin-core`（归档）/ `keel-kernel-loop`（归档）/ `keel-tenancy-usage`（归档）交叉对账。
- 未跑 `go build ./...` / 全量 `go test`（在途代码线），全部结论来自静态证据 + 既有测试名；
  需要运行期证据的门（E0/G2/G4 等）一律以「部分实现 + 归档 change 记录」处理，不冒充绿。
- 未修改除 `RECONCILIATION.md` 与 `keel-harness-v2/tasks.md`（仅勾选+行尾注记）以外的任何文件；
  `keel-ecosystem/tasks.md` 未被本对账触碰。

<!-- END-OF-RECONCILIATION -->
<!-- SECTION-END -->
