# keel-harness-v2 任务板 ↔ 代码真值对账（2026-11-20）

> 范围：`openspec/changes/keel-harness-v2/tasks.md` 的 103 条未勾项（对账前 43/146）。
> 基线：keel `6e53bff`，worktree `keel-phase0-reconcile`。本轮只改任务板/文档/证据，未改产品代码。
> 口径：**任务板只是索引，真值 = 代码 + 测试 + 证据**；每一条勾选必须能落到文件/符号或可复跑的测试名。

## 1. 计数

| 指标 | 对账前 | 对账后 |
|---|---|---|
| 已勾 | 43/146 | **48/146** |
| 未勾 | 103 | 98 |
| 未勾中：部分完成 | 未分类 | 45 |
| 未勾中：被替代/延后 | 未分类 | 2 |
| 未勾中：未开始 | 未分类 | 51 |

### 1.1 分 W 计数

| W | 总数 | 前勾 | 后勾 | 未勾 | 部分 | 被替代/延后 | 未开始 |
|---|---:|---:|---:|---:|---:|---:|---:|
| W0 契约与骨架 | 23 | 6 | 7 | 16 | 9 | 1 | 6 |
| W0.5 插件全景 | 8 | 0 | 0 | 8 | 2 | 0 | 6 |
| W1 layerfs | 6 | 3 | 3 | 3 | 1 | 1 | 1 |
| W2 放置与内核契约 | 7 | 1 | 1 | 6 | 4 | 0 | 2 |
| W3 file/bash 插件 | 11 | 8 | 8 | 3 | 1 | 0 | 2 |
| W4 task-log | 7 | 5 | 5 | 2 | 1 | 0 | 1 |
| W5 approve/notify | 6 | 6 | 6 | 0 | 0 | 0 | 0 |
| W6 MCP 聚合 | 5 | 4 | 4 | 1 | 0 | 0 | 1 |
| W7 skills | 7 | 1 | 1 | 6 | 1 | 0 | 5 |
| W8 agent/fork | 5 | 0 | 1 | 4 | 3 | 0 | 1 |
| W9 OAR | 5 | 2 | 2 | 3 | 0 | 0 | 3 |
| W10 triggers | 3 | 1 | 1 | 2 | 2 | 0 | 0 |
| W11 验收门 | 13 | 1 | 2 | 11 | 8 | 0 | 3 |
| W12 插件补全 | 40 | 5 | 7 | 33 | 13 | 0 | 20 |
| **合计** | **146** | **43** | **48** | **98** | **45** | **2** | **51** |

## 2. 本轮新勾选（5 条，均有测试名）

| 条目 | 判定 | 证据 |
|---|---|---|
| W0 废止旧工具名注释与兼容红线 | 已实现 | `internal/toolsurface/surface.go:8-12` 明示 v1 名（`spawn_subagent`/`creation_*`/`skill`/`clock`/`echo`/`mcp__*`）无别名；`internal/agentext/creation.go:88-90` 明示 v1 `creation_*` 已删；`internal/pluginpkg/manifest.go` 已无「`example_status`/`spawn_subagent` 必须不变」的兼容红线。测试：`TestBuiltinSurfaceIsExactlyEleven`（`internal/toolsurface/toolsurface_test.go:110`，逐名断言旧名未注册且调用返回 unknown tool）、`internal/host/host_test.go:50` 同断言。 |
| W8 内部子代理独立会话 + 独立任务卡 | 已实现 | `internal/agentext/subagent.go:216-292` `SpawnAsync` 建独立子会话并 emit `HookSubagentStarted`；`internal/agentext/tasks.go:267-293` 该 hook 调 `tasklog.LinkSubagent`；`internal/tasklog/service.go:390` 建子卡并写 `parentTaskId`。测试：`TestSubagentCardLinksToParentActiveCard`（`internal/tasklog/tasklog_test.go:328`）、`TestTasksPluginLinksSubagentCards`（`internal/agentext/tasks_test.go:192`）。 |
| W11 工具清单稳定性 | 已实现 | `internal/toolsurface/hash.go:31` `ManifestHash`（按名排序、schema 规范化）。测试：`TestManifestHashStableAcrossMCPServers`（`internal/toolsurface/toolsurface_test.go:593`，100 个随机 server 哈希与空集相同）、`TestApprovedPluginToolsChangeAndRestoreTheManifestHash`（`:620`，只有 `provides.tools` 变更/卸载才移动并恢复哈希）。技能只以 `skills` 工具的数据出现，工具 schema/description 静态（`internal/toolsurface/skills.go:20-40`）。 |
| W12 只读发现工具 | 已实现 | `internal/agentext/creation.go:383-420` `creation.catalog` 是 `manager.Snapshot()` 的只读投影，返回已装插件的 id/version/kind/state/bundled/provides/live tools；`internal/toolsurface/creation.go:20-26` 将其暴露为 `catalog` action。测试：`TestCreationCatalogListsInstalledPlugins`（`internal/agentext/creation_test.go:168`）。 |
| W12 `creation` 工具恒定存在 | 已实现 | `internal/toolsurface/creation.go:37-65` 聚合工具由 harness 恒定注册；`keel.creation` 未装/停用时把 `ErrPluginNotFound`/`ErrPluginState` 映射为 `plugin_disabled`，工具名不消失。测试：`TestCreationWiringAndDisabledStayDistinct`（`internal/toolsurface/toolsurface_test.go:538`）、`TestCreationRoutesAndMapsDisabledPlugin`（`:573`）。 |

## 3. 被替代/延后（2 条）

| 条目 | 去向 | 依据 |
|---|---|---|
| W0 ADR 0001 + Rust 服务基础设施 | ADR 已交付；Rust+gRPC 专属 CI/Docker/健康检查/端口/SIGKILL sweep 随 W1 gRPC 延后 | `adr/0001-layerfs-rust-grpc.md`；RESIDUAL-PLAN §5.1-5 裁决 Phase A = Go 原生 CAS（`internal/layerfs` 已交付），Rust 保留为同接口扩展实现。 |
| W1 gRPC 契约 | ADR 保留为扩展实现，未开工 | 无 proto/gRPC 代码；Phase A Go CAS 的 `Put/Get/Stat` 在 `internal/layerfs/store.go`，不冒充 gRPC。 |

## 4. 逐项去向（98 条未勾）

> 状态：**部分** = 主体在但有明确缺口；**未开始** = 无实质实现；**被替代/延后** = 见 §3。
> 行内证据是可直接点验的文件/符号；没有测试名的“部分”不冒充完成。

### W0 契约与骨架（16）

| 条目 | 状态 | 缺口 / 证据 |
|---|---|---|
| 既有门承接（r1diff/allowlist/重 pin） | 部分 | `scripts/acceptance-all.sh:122-141` 仍跑冻结基线 `e7361f8` 的旧 R1 场景；`cmd/r1diff/bookkeeping.go:8-12` 只登记 `run.started/failed/finished`，v2 新 kind（task/fs/config/placement/supervision/guardrail/resume/notify）未进场景/allowlist。 |
| AgentTool 接口扩展 | 未开始 | `internal/agentcore/tool.go:28-45` 仍只有 Name/Description/Schema/ExecutionMode/Execute；幂等键/超时/取消/截断/错误码/内存档位未实现，也未单列新任务。 |
| ADR 0001 + Rust 基础设施 | 被替代/延后 | 见 §3；ADR 在 `adr/0001-layerfs-rust-grpc.md`，基础设施未做。 |
| 多用户进程 | 部分 | triggers/设置面已按租户（`internal/agentext/triggers.go:328-341`、`internal/agentapi/settings.go:9-10`）；`/v1/plugins` 仍是 `Snapshot()` 的部署级库存（`internal/agentapi/plugins.go:97`），schema 无 `plugin_state(tenant_id,...)`，`keel serve` 仍是单配置租户进程。 |
| 清单解析扩展 | 部分 | `KnownFields(true)` 在 `internal/pluginpkg/manifest.go:411`；`layer` 仍只允许 `provider`（`:148-156`），`Provides` 无 `slots`/`contributes`（`:113-145`），webview 仍强制 `ui.entry`（`:599`），`plugin_state` 无租户字段。 |
| catalog 唯一真源 | 部分 | `plugins/keel.plugin-market/catalog.json` 有 id/status/keelPluginId，但无 `wave`/`dshGroups`；无生成/比对脚本与 CI step，`plugins/COVERAGE.md` 手工维护。 |
| RSS 采样器 | 部分 | 无 `scripts/acceptance-rss.sh`；现有内存门是 cgroup `memory.peak`（`scripts/acceptance-perf.sh:98-101`）与 handoff RSS，不是 VmRSS 求和/≤500MB 协议。 |
| 原生 target 可执行 | 部分 | web 产物与 TUI 真终端在（`web/README.md:28-31`）；`nsh package --target macos` 产物/截图未做。 |
| NSH 0.5.0 升级 | 未开始 | `web/README.md:11` 仍 pin nsh 0.4.0 nightly；Window API/媒体 grant/`focus-visible:`/`activationPolicy` 未接。 |
| 客户端壳 | 未开始 | `web/nsh.json` 只有 appearance/theme/capabilities/web.host；无单实例/托盘/全局快捷键/深链/文件关联/启动隐藏/独立窗口。 |
| 系统集成 | 部分 | `web/host.js:68-72` 只有 `keel.notify {title,body,tag}`；badge/silent/点击深链/`evaluate` 返回值/自有 scheme/incognito/摄像头麦克风降级未接。 |
| 更新与分发 | 未开始 | 全仓无 `nsh/updater` 集成或签名 OTA 流水线；`.github/workflows/release.yml` 只是产物发布。 |
| 长会话性能 | 未开始 | 无 1000 条消息滚动保真/离屏增量/视图树释放的测试或策略。 |
| 编辑器 | 未开始 | `web/src/screens/composer.tsx:12-17` 仍是 `nsh/base` Input；无 RichEditor/独立字号/NSH Terminal。 |
| `docs/HARNESS.md` 增补 | 部分 | `docs/HARNESS.md:75-81` 已有内核接缝；缺放置模型与多用户升级章节。 |
| 架构测试（kernel 不 import placement/task/fs） | 部分 | `internal/architecture/boundary_test.go:11-45` 只断言 kernel↔product/modelcfg；未对 `layerfs`/`tasklog`/`fileplugin` 增加 import 断言（当前 `go list -deps ./internal/kernel` 实际无这些 import）。 |

### W0.5 插件全景（8）

| 条目 | 状态 | 缺口 / 证据 |
|---|---|---|
| 冻结 catalog id/status/wave | 部分 | catalog 有 id/status/keelPluginId，无 `wave`/`dshGroups`；`plugin-inventory.md` 已降为附录，但 catalog 未冻结。 |
| 五种 layer 装载边界 | 未开始 | `internal/pluginpkg/manifest.go:31-48` kind 仍只有 in-process/sidecar/webview；`layer` 只有 provider 一种。 |
| 四层装配 + row id overlay | 未开始 | 无 `config.toml`/base/mode/preset bundle/row id overlay；现有是 `internal/loadout` 的单一默认服务图。 |
| 能力 seam + capability matrix | 未开始 | `internal/loadout/loadout.go:16-49` 只有 ctx.store/llm/prompt/tools/mcp/loop/compaction/session-projection/telemetry/worker/sandbox/external-*；无 fs/shell/subprocess/exec-surface/terminal seam。 |
| 不照搬清单写入 ADR | 未开始 | `adr/` 只有 0001；Cordis/typert/一能力一工具/70 UI 包/sandbox 族/DSH ssh 未落 ADR。 |
| 冻结扩展面声明格式 | 未开始 | 无 `extensibility.json`/`EXTENSIBILITY.md`；`Provides` 无 `slots`/`contributes`。 |
| catalog 对账 | 部分 | `plugins/COVERAGE.md` §A/B/C 有映射，但 `keel.goal`/`keel.todo`/`keel.plan`/`keel.team`/`keel.workspace`/`keel.guard` 仍独立；无 absent+理由逐行处置。 |
| v1 官方插件补 extensibility 文档 | 未开始 | `plugins/keel.web-search`、`keel.context-memory`、`keel.plugin-market` 均无 `extensibility.json`/`EXTENSIBILITY.md`。 |

### W1 layerfs（3）

| 条目 | 状态 | 缺口 / 证据 |
|---|---|---|
| 配额（group/租户） | 部分 | 只有 store 级 `QuotaBytes`（`internal/layerfs/store.go:35-38`，测试 `TestQuotaRejectsAtCommitPoint`）；无 group/租户存储字节配额。 |
| gRPC 契约 | 被替代/延后 | 见 §3；无 proto/gRPC。 |
| 1000 并发流压测 | 未开始 | 无 1000 并发流 RSS 曲线；只有 `TestLargePutAndReadAreChunked`（`internal/layerfs/store_test.go:174`）。 |

### W2 放置与内核契约（6）

| 条目 | 状态 | 缺口 / 证据 |
|---|---|---|
| `groups`/`session_placement` + lease | 未开始 | `internal/store/schema.sql` 无这两张表；只有 sessions 的 owner/epoch/lease。 |
| pack/user/pin 策略 | 未开始 | 无策略代码。 |
| 组内并发槽位（排队+ETA） | 部分 | 只有进程内全局 worker 信号量（`internal/agent/worker.go:169`）与 per-tenant 配额（`internal/quota`）；无组槽位/ETA/DB 聚合。 |
| 进程生命周期（组 OOM） | 部分 | drain/handoff/SIGTERM 在（`internal/httpapi/drain.go`、`internal/handoff`）；无 group 实体与组 OOM 隔离。 |
| run 快照三值 | 部分 | `run.finished` 有 `configHashBefore/After`（`internal/agent/runner.go:290-291`）；无 `anchorEventSeq`/`toolManifestHash` 写入 run 事件。 |
| 外部能力矩阵 + typed rejection | 部分 | `internal/gateway/initialize.go:81-115` 只有 Tools/Agent{Events,Cancel,Usage}；无 fork/approval/compaction 能力位。 |

### W3 file/bash 插件尾项（3）

| 条目 | 状态 | 缺口 / 证据 |
|---|---|---|
| bash 删除对账 | 未开始 | `Manifest.Delete` 存在（`internal/fileplugin/manifest.go:222-230`）但 capture 路径只 `Upsert`（`internal/fileplugin/service.go:503-620`）；`fs.capture` payload 无 deleted 列表。 |
| capture 策略覆写 | 未开始 | 无 `config.toml`；`CaptureMode`/checkpoint/manual 覆写通道未接。 |
| 并发 capture 定序 | 部分 | `Upsert` 有 `updated_seq` guard（`internal/fileplugin/manifest.go:180-196`），snapshot 按 seq 精确存（`:239-247`）；但无跨 lane 定序/版本化，且 fork 未接线（W4）。 |

### W4 task-log（2）

| 条目 | 状态 | 缺口 / 证据 |
|---|---|---|
| fork（eventSeq 原点/前缀引用/新卡） | 部分 | `tasklog.ForkCard` 已交付（`internal/tasklog/service.go:348-380`，测试 `TestForkCreatesANewCardPointingAtTheOrigin`）；`agent.fork` 仍 `not_implemented`（`internal/toolsurface/agent.go:49-50`），无新会话/文件前缀引用。 |
| 面板 | 未开始 | 无任务板 API/页面；`web/src/screens` 无 tasks。 |

### W6 MCP 聚合（1）

| 条目 | 状态 | 缺口 / 证据 |
|---|---|---|
| gbash 的 `mcp` CLI | 未开始 | `internal/bashbridge/jobcommands.go` 只注册 jobs/ps/nohup；无 mcp CLI/共享 registry。 |

### W7 skills（6）

| 条目 | 状态 | 缺口 / 证据 |
|---|---|---|
| skill 文件结构/hash/版本 | 部分 | `internal/skills/skills.go:46,138,338` 有 SKILL.md 解析与 `ContentHash`；无内容 hash 去重/版本不可变注册表。 |
| ChatInput `$` 补全 | 未开始 | `web/src/screens/composer.tsx` 无 `$` 触发/补全。 |
| `/learn` | 未开始 | 无该路径。 |
| Settings/CLI `skills` | 未开始 | `cmd/keel/main.go:34-48` 无 `skills` 子命令；web settings 无 skills 页。 |
| 订阅源管理 | 未开始 | 无订阅源/信任/版本 pin 模型。 |
| 技能内脚本走 bash 策略 | 未开始 | `skills` 只有 ls/view；无脚本执行路径（依赖 bash 策略）。 |

### W8 agent/fork（4）

| 条目 | 状态 | 缺口 / 证据 |
|---|---|---|
| `agent` 工具全动作 | 部分 | `internal/toolsurface/agent.go:22-55` 已有 spawn/list/status；fork/resume/cancel 返回 `not_implemented`，无 eventSeq 原点。 |
| 子代理继承 config 版本 | 未开始 | 无 `config.toml`/版本继承；`SpawnRequest` 只有 selection。 |
| 父子并发槽位记账 | 部分 | 父与子都占进程全局 worker 信号量；无组槽位/排队 ETA（同 W2-3）。 |
| 子代理返回产物引用/父卡 note | 部分 | `SubagentResult` 在（`internal/agentext/subagent.go:103-113`），`Artifacts` 是工具调用 trail（`:520-545`）；无产物 blob 引用/父卡 `task.note`。 |

### W9 OAR（3）

| 条目 | 状态 | 缺口 / 证据 |
|---|---|---|
| sidecar 接入 OAR/record stream | 未开始 | 全仓无 OAR 插件/record stream；`internal/dshref` 是 DSH 参考 sidecar，不是 OAR。 |
| 外部工具渲染器 | 未开始 | 无 record stream 折叠视图。 |
| 不注入「请记录任务卡」 | 未开始 | 无 OAR 链路；纪律只写在 spec/design。 |

### W10 triggers（2）

| 条目 | 状态 | 缺口 / 证据 |
|---|---|---|
| HMAC/去重键/漏跑补偿 | 部分 | `internal/agentext/triggers.go:303-460` 有 cron/interval/webhook、幂等键、审计、重试；无 HMAC 签名校验/漏跑补偿。 |
| 每触发器并发上限/初始卡 | 部分 | 同一幂等键串行化（`triggers.go:345-348`）；无 per-trigger 并发上限；初始任务卡由会话启动路径覆盖，未按触发器单独验证。 |

### W11 验收门（11）

| 条目 | 状态 | 缺口 / 证据 |
|---|---|---|
| 端到端全链 | 未开始 | 依赖 W4 面板/fork 与插件卸载链路，尚未串起。 |
| 外部工具 OAR 真链路 | 未开始 | 依赖 W9 OAR/渲染器。 |
| E0 既有门零回归 | 部分 | `scripts/acceptance-all.sh` 有 R1 + 负控，但仍用冻结基线 `e7361f8` 的旧场景；v2 kind 未进 r1diff/allowlist/重 pin。 |
| G0 契约门 + G1 状态机门 | 部分 | G0 的 11 工具快照/旧名负控有测试（`TestBuiltinSurfaceIsExactlyEleven`）；G1 有插件生命周期/任务卡状态测试；缺门级汇总证据与完整转移矩阵/拒绝事实表。 |
| G2 恢复门 | 部分 | 非终态 run 自动续跑/lease/chaos 在（`internal/durable/recovery.go`、`internal/agent/crash_matrix_test.go`）；子代理继续原任务（W8-2/3）未实现。 |
| G3 幂等门 + G5 任务门 | 部分 | G3 幂等/409 在；G5 taskId 强制/资源引用已交付，fork 精确（W4-6）未实现。 |
| G4 内存门 | 部分 | cgroup peak 门在（`scripts/acceptance-perf.sh`）；无 VmRSS 采样门（W0-12）、组 OOM 隔离、前端 surface VM 配额断言。 |
| G6 插件门 + G7 UI 门 + G9 有界门 | 部分 | G6 生命周期/依赖/回滚测试在；G7 槽位机制未实现（W12-16/17）；G9 bash 输出有界/作业上限在。 |
| G8 创造模式门 | 部分 | 创造模式已有 webview E2E（`internal/agentext/creation_test.go`）；缺 Native chunk 形态/macos target/卸载 diff。 |
| G10 资源回收门 | 部分 | sidecar/gateway 进程组回收、drain、chaos 孤儿断言在；无 layerfs VM/sweep 与端口台账核对。 |
| G11 交付面门 | 未开始 | 本 change 无 HTML 图文 E2E 报告/UNCOVERED=0 审计/子代理交叉复核产物。 |

### W12 插件补全（33）

| 条目 | 状态 | 缺口 / 证据 |
|---|---|---|
| `keel.spill` | 未开始 | 无 spill 插件/机制。 |
| `keel.compaction` 扩展 | 部分 | 压缩核心/自动压缩在（`internal/compaction`、`internal/agent/runner.go`）；无 tool-result-pruner/image-offload。 |
| `keel.checkpoint` | 部分 | checkpoint 语义在压缩内；无独立何时落/保留量策略插件。 |
| `keel.prompt-cache` | 未开始 | 无稳定前缀/缓存边界策略。 |
| `keel.supervisor` | 部分 | worker claim/lease/durable recovery 在；无插件实体/换代编排/升级账本。 |
| `config.toml` 策略 | 未开始 | 无 config.toml；恢复参数仍是代码默认。 |
| `POST /v1/sessions/{id}/resume` | 未开始 | 无该端点。 |
| 子代理可续跑 | 未开始 | 子代理是一次性 wait 模型；只有 `cancelChild`。 |
| 护栏项 | 部分 | 租户并发/事件速率/worker 租约/命令超时在；无重复调用提醒/连续失败/额度成本/内存压力护栏。 |
| 升级阶梯 | 未开始 | 无阶梯编排；`supervision.escalated` 只有 kind。 |
| `keel.super-agent` | 未开始 | 无该插件。 |
| 恢复/升级/护栏账本事实 | 部分 | kind/KeyKind 已注册（`internal/kernel/kernel.go:118-120`）；无 emitter。 |
| `keel.creation` 聚合 extensibility | 部分 | `creation.catalog` 聚合 manager.Snapshot 的插件/provides/live tools；未聚合 `extensibility.json`/`EXTENSIBILITY.md`（格式本身未冻结，W0.5-6）。 |
| `keel.ui` 槽位机制 | 未开始 | 无槽位注册/注销/装载/schema/降级。 |
| `keel.ui` 基础槽位声明 | 未开始 | 无 `keel.ui.route/.nav/.action/...`；第一方面板仍是硬编码页面。 |
| 四形态 chunk/package/iframe/data | 未开始 | `internal/pluginpkg/manifest.go:31-48` 只有 in-process/sidecar/webview。 |
| `keel.plugin.*` ↔ NSH 桥 | 部分 | 服务端 bridge/权限/审计在（`internal/agentapi/plugin_bridge.go`）；`web/host.js` 无 `keel.plugin.*` ↔ `host.call`/`host.on` 分派。 |
| Native 插件现场写/装/渲染/卸载 | 未开始 | 无 chunk 形态/macos target；现有创造模式产出是 webview 模板。 |
| TUI 降级 | 未开始 | 无槽位形态；TUI 对插件 UI 只有 openUrl 文本兜底。 |
| `keel.session-title`/`session-query` | 部分 | 标题派生在（`internal/agentapi/sessions.go:52`、`web/src/ui/session-title.ts`）；无 session-query 检索/导出/插件实体。 |
| `keel.deliverables` | 未开始 | 无交付物登记/工作区变更投影。 |
| `keel.attachment` | 部分 | 上传/输入 attachments 在（`internal/httpapi`、`internal/agent/images.go`）；无插件实体。 |
| `keel.commands`/`permission`/`feedback` | 部分 | permission 派发/hooks 决策在（`internal/agentext/plugin_permissions.go`、`internal/hooks/decision.go`）；commands/feedback 无插件实体。 |
| `keel.subprocess`/`terminal` | 未开始 | 无共享子进程/持久终端 provider。 |
| `keel.bundles` | 未开始 | 无 bundle 概念；`internal/loadout` 是单一默认蓝图。 |
| `keel.presets` | 未开始 | 无 agent preset 覆盖面。 |
| `keel.telemetry` | 部分 | telemetry 契约/noop/memory/Prometheus 在；无 OTLP adapter（`docs/PI-CONFORMANCE.md:267`、`docs/HOST.md:51`）。 |
| `keel.testkit` | 部分 | `keel.testkit` 内置插件已有（clock/echo，默认停用，`internal/testkit`）；无 mock-replay/无密钥测试台完整面。 |
| `keel.credentials`/`storage`/`identity` | 部分 | 核心能力在（租户密钥库/PG store/tenancy）；三者仍不是可注册插件实体。 |
| `keel.oar` | 未开始 | 无 oar 插件（W9）。 |
| UI 插件：task-board/notifications/jobs/tool-renderer/skills | 未开始 | `plugins/` 无这些实体；catalog 只在 DSH 包名数组里出现。 |
| UI 插件：subagent/deliverables/secret-mask/creation | 未开始 | 同上。 |
| 登记 `ui-trajectory` | 未开始 | 只出现在 `catalog.json` 的 DSH `client` 包名数组，无自己的 keel 插件登记行。 |

## 5. 关键证据锚点（已交付项，供复核）

| 范围 | 证据 |
|---|---|
| W2 合并树全量门禁 | `evidence/w2-merged-verification.txt`（`go build`、`go test -p 1 ./...` 54 包 ok、openspec strict、agent-runtime-e2e PASS、端口无残留） |
| W2.2 bash/JobManager | `evidence/w2.2-bash/`、`evidence/w2.seam/`、`evidence/w2.jobcap/` |
| W2.3 layerfs/file-plugin | `evidence/w2.3-layerfs/`（`test-race.txt`、`test-scale.txt`、`test-write-through.txt`、`test-fs-write-order.txt`） |
| W2.4 task-log | `evidence/w2.4-tasklog/RESULTS.md`（`TestInitialCardIsIdempotentAndAutoAttributed`、`TestProjectionRebuildIsIdempotentAndRepairsStrayRows`、`TestForkCreatesANewCardPointingAtTheOrigin`、`TestSubagentCardLinksToParentActiveCard`） |
| W2.5 approve/notify | `evidence/w2.5-approve-notify/gates.txt`、`test-race.txt`、`orphans.txt` |
| MCP Apps P6 | `openspec/changes/archive/2026-11-20-feat-mcp-apps-events/evidence/p6/ACCEPTANCE.md`、`p6/docs/parity.md`、`p6/REVIEW.md` |
| 真实模型 E2E | `scripts/real-model-e2e.sh`、`docs/REAL-MODEL-E2E.md`（已勾项原有证据） |

## 6. 方法与边界

- 逐条读实现/测试：先按 tasks.md 的 103 条未勾项定位包，再查符号、调用点、测试名；**没有读测试/实现就不勾**。
- 判定标准：全部子项有可复现证据才勾；主体在但缺至少一个明确子项判“部分”；被 Owner 裁决/路线变更取代判“被替代/延后”；无实质实现判“未开始”。
- 本轮**未重跑全量 `go test ./...`**（需要 `KEEL_TEST_DSN` 与 Postgres；避免与在途 WIP 争用）。已交付门的原始转录在 `evidence/w2-*`；本轮只做静态对账与测试名核对。
- 本轮复跑的无 DB 子集：`go test ./internal/toolsurface -run 'TestBuiltinSurfaceIsExactlyEleven|TestManifestHashStableAcrossMCPServers|TestCreationWiringAndDisabledStayDistinct|TestCreationRoutesAndMapsDisabledPlugin' -count=1` → `ok ... 0.954s`。
- `pnpm exec openspec validate keel-harness-v2 --strict`：valid（本轮修改后复跑）；`git diff --check`：clean。
- 未改产品代码；主仓 `fde2e/keel` 子模块指针仍未更新（他会话 WIP），见 `docs/RESIDUAL-PLAN.md` §1 的指针注记。
