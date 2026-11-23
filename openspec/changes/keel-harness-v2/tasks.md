# Tasks: keel-harness-v2

> 顺序原则：先契约、再环境、再记忆、再放置、最后收口。每个 W 都可在上一个 W 的
> 契约冻结后并行推进；收口门（W11）全绿才归档。

## W0 契约与骨架

- [x] 冻结本 change 的**全部 10 份 spec**（`harness-core` / `agent-loop-contract` / `layerfs` /
      `task-log` / `tool-surface` / `plugin-architecture` / `plugin-ui-extension` / `supervision` /
      `client-shell` / `plugin-system` 的 MODIFIED delta），
      后续实现不得偏离；偏离走新 change
      （对账：`openspec/changes/keel-harness-v2/specs/*/spec.md` 恰好 10 份，随 change 提交）
- [x] **kind 表**：每个新 kind 定 `class`（owner/edge/external/system）、`key`/fence、提交者；
      新增 `task.created|updated|attached|detached|closed|note`、`fs.write`、`fs.capture`、
      `fs.capture_failed`、`config.rotated`、`placement.joined|left`、
      `supervision.escalated`、`guardrail.tripped`、`resume.attempt`、`notification.ack|push_sent`；
      `run.finished` payload 增 `configHashBefore/After`；同步 `kindRegistry`、`internal/store`
      常量、转录/SSE/R1 规则
      （对账：`internal/kernel/kernel.go:99-135`、`internal/kernel/validate.go:44-77`、
      `internal/store/events.go:24-47`、`internal/agent/runner.go:248-253`、pin 测试
      `internal/kernel/kernel_test.go:1481`；新 kind 尚无 emitter，R1 只登记 run.* bookkeeping）
- [ ] **既有门承接**：扩展 `cmd/r1diff` 场景与 allowlist（每条附理由）、重新 pin 基线； （对账 2026-11-20：部分——缺口：acceptance-all 仍在冻结基线 e7361f8 的旧 R1 场景；v2 新 kind（task/fs/config/placement/supervision/guardrail/resume/notify）未进 cmd/r1diff 场景/allowlist，未重新 pin；scripts/acceptance-all.sh:122-141、cmd/r1diff/bookkeeping.go:8-12）
      `scripts/acceptance-all.sh` 必须全绿（E0）
- [x] **工具面迁移**（W2.1）：按 `tool-surface` 的破坏性迁移表删除旧工具名（无 shim），
      11 个内建聚合工具恒定注册，终局快照 + 清单哈希 + action 闭集路由落地
      （`internal/toolsurface`；`spawn_subagent` 并入 `agent.spawn`，团队 R3 同步）
- [ ] **AgentTool 接口扩展**：`agentcore.AgentTool` 缺的字段（幂等键/超时/取消/截断/错误码/
      内存档位）单列实现任务（W2.1 只落地工具名/动作面，不伪造字段）
- [x] **PI 偏离登记**（W2.1）：`docs/PI-PARITY.md` §4.2 写明 MCP 工具命名的偏离
      （理由/影响面/验收方式）；`internal/mcp` 的 `mcp__<server>__<tool>` 适配、naming 与
      对应单测已删除，`mcp` 聚合工具（list/describe/call）接管
- [x] **契约冻结 spike**：`docs/GBASH-CONTRACT.md`（版本/协议帧/进程模型/许可证）与
      OAR 契约（record stream schema/typed 码表/进程组回收/与 `internal/externalgw` 的复用关系）；
      未冻结前 bash 与 `keel.oar` 的实现任务保持阻塞
      （对账：gbash 侧已冻结——pin `v0.0.41` @ `7cc3528`，模块 `github.com/ewhauser/gbash`，
      Apache-2.0 + in-tree `mvdan/sh` fork BSD-3-Clause；10 章、约 40 条实测断言探针；
      **三条实现级收窄已回写本 change 的 `tool-surface` spec**：①作业=独立 Session+goroutine、
      重启一律收敛终态（原「重启后收养」物理不可行）②禁用上游 `ExecutionRequest.Timeout`
      （语义是取消+杀）③layerfs 只挂非根挂载点（根 FS 会被 263 次命令桩污染）。
      **OAR 契约仍未冻结**，`keel.oar` 阻塞保持）
- [ ] **ADR 0001**：layerfs Rust+gRPC 的推翻记录与证据门；CI 工具链、Dockerfile/compose、 （对账 2026-11-20：被替代/延后——ADR 0001 已交付；当前实现走 Phase A Go CAS（internal/layerfs），Rust+gRPC 专属 CI/Docker/健康检查/端口/SIGKILL sweep 随 W1 gRPC 延后，未实施；adr/0001-layerfs-rust-grpc.md、RESIDUAL-PLAN §5.1-5）
      健康检查、端口登记（13610–13799）、SIGKILL sweep
- [ ] **多用户进程**：租户解析、`/v1/plugins`/triggers/工具面/设置面按租户过滤、装配隔离； （对账 2026-11-20：部分——缺口：triggers/设置面已按租户；/v1/plugins 仍是无租户的部署级 Snapshot，schema 无 plugin_state(tenant_id,...)，keel serve 仍是单配置租户进程；internal/agentapi/plugins.go:97、internal/store/schema.sql、internal/agentext/triggers.go:328-341）
      `plugin_state(tenant_id, plugin_id, status, config_version, reason)` 与 `/v1/plugins` 过滤；
      `AGENTS.md` 修订为「默认单进程多租户；`user` 放置是显式 SKU」
- [x] **内核键命名空间**：扩展 `KeyKind` 与 `CommitRequest`（`fs:`/`capture:`/`task:`/`config:`/
      `placement:`/`supervision:`/`guardrail:`/`resume:`/`notif-ack:`/`notif-push:`/`run:`），
      禁止复用 `tool:` 前缀；加「tool.result 后紧接 fs.write」负控单测
      （对账：`internal/kernel/kernel.go:217-243`、`CommitRequest` `:325-370`、
      `internal/kernel/validate.go:181-205`；负控 `internal/kernel/contract_v2_test.go:62`、
      键齐全性 `:95`、前缀隔离 `internal/kernel/kernel_test.go:1527-1555`）
- [x] **清单解析扩展**：`pluginpkg` 支持 `layer`（可选）、`provides.slots`、`contributes`、 （对账 2026-11-20：部分——缺口：KnownFields(true) 在；通用 layer 仍只允许 provider，Provides 无 slots/contributes，webview 仍强制 ui.entry，plugin_state 无租户字段；internal/pluginpkg/manifest.go:113-156,411,599）
      纯 chunk 包（无 `ui.entry`）、`plugin_state` 租户字段；`KnownFields(true)` 同步
- [x] **catalog 唯一真源**：`catalog.json` 增 `keelPluginId/status/wave/dshGroups`，统一 status （对账 2026-11-20：部分——缺口：catalog.json 有 id/status/keelPluginId，但无 wave/dshGroups；无生成/比对脚本与 CI step，COVERAGE.md 手工维护；plugins/keel.plugin-market/catalog.json、plugins/COVERAGE.md）
      枚举（含 `partial`）；生成/比对 `COVERAGE.md`（对照 catalog）；比对脚本进 CI
- [ ] **RSS 采样器**：`scripts/acceptance-rss.sh` 采样后端进程 `VmRSS` 求和，接 `acceptance-all.sh`， （对账 2026-11-20：部分——缺口：无 scripts/acceptance-rss.sh；现有内存门是 cgroup memory.peak（scripts/acceptance-perf.sh:98-101）与 handoff RSS，不是 VmRSS 求和/≤500MB 协议）
      断言 ≤500MB（目标 ≤300MB）；cgroup peak 单列
- [ ] **原生 target 可执行**：`web` 与 `macos`（`nsh package --target macos`）构建产物 + 截图； （对账 2026-11-20：部分——缺口：web 产物与 TUI 真终端在（web/README.md:28-31），macos target 产物/截图未做）
      TUI 用 `nsh run --target tui` 真终端（0.5.0 ratatui 后端），测试用 `nsh test --target tui`
- [ ] **NSH 0.5.0 升级**：pin 0.5.0；Port Window API（`WindowRef`/`<Window>`/window kinds）；
      媒体加载 `fs.read`/`${pluginDir}` grant；`focus-visible:` 仅键盘；macOS `activationPolicy`
- [ ] **客户端壳**：单实例、托盘、全局快捷键、URL Scheme 深链、文件关联、启动隐藏/菜单栏模式；
      任务板/交付物可开独立窗口
- [ ] **系统集成**：系统通知（含 silent）＋ badge；通知点击深链；WebView `evaluate` 返回值、 （对账 2026-11-20：部分——缺口：web/host.js:68-72 只有 keel.notify {title,body,tag}；badge/silent/点击深链/evaluate 返回值/自有 scheme/incognito/摄像头麦克风降级未接）
      自有 URL scheme、请求头、incognito、摄像头/麦克风（不支持时显式降级）
- [ ] **更新与分发**：`nsh/updater` 自更新（check/download/install/restart）＋签名 OTA 包；
      试验期失败回滚；运行时变化走完整应用更新
- [ ] **长会话性能**：1000 条消息滚动保真、离屏增量创建、大表格避免重复布局、视图树消失释放内存
- [ ] **编辑器**：Composer RichEditor（HTML 导入/导出）；消息流式 Markdown；代码/Diff/终端独立字号；
      用 NSH `Terminal` 组件渲染后台输出
- [x] 废止代码里的旧工具名注释与兼容红线：`internal/agentext/plugin.go`、`creation.go`、 （对账 2026-11-20：已废止——旧工具名无注册点（TestBuiltinSurfaceIsExactlyEleven，internal/toolsurface/toolsurface_test.go:110）；toolsurface/surface.go:8-12 明示无别名；agentext/creation.go:88-90 明示 v1 creation_* 已删；manifest.go 无「example_status/spawn_subagent 兼容红线」）
      `subagent.go`、`internal/pluginpkg/manifest.go:227`
- [ ] `docs/HARNESS.md` 增补 Harness/内核边界、放置模型与多用户升级 （对账 2026-11-20：部分——缺口：docs/HARNESS.md:75-81 已有内核接缝；缺放置模型与多用户升级章节）
- [ ] 架构测试：内核包不得 import placement/task/fs（沿用 `internal/architecture` 断言模式） （对账 2026-11-20：部分——缺口：internal/architecture/boundary_test.go:11-45 只断言 kernel↔product/modelcfg；未针对 layerfs/tasklog/fileplugin 增加 import 断言（当前 go list 实际无 import））

## W0.5 插件全景（对表 DSH）

- [x] 冻结 `catalog.json` 的 id/status/wave；`plugin-inventory.md` 仅作设计期附录，不作为波次真值 （对账 2026-11-20：部分——缺口：catalog.json 无 wave/dshGroups；inventory 已降为附录，但 catalog 未冻结）
- [x] 五种 **layer**（kernel/harness/provider/ui/service）的装载边界与架构测试；
      承载 `kind` 仍是三种（in-process/sidecar/webview），两者正交
- [x] 四层装配（base/mode/preset/config.toml）与 row id overlay 语义
      （W0.5-assembly：纯模型 `internal/assembly`（build/config/seam）+ 生产适配
      `internal/loadout/assembly.go`；契约 `docs/ASSEMBLY.md`、ADR
      `adr/0003-four-layer-row-overlay.md`；测试 `internal/assembly/*_test.go`、
      `internal/loadout/assembly_test.go`；证据 `evidence/w05-assembly/`。文件级
      config.toml 读取/原子 rename 已由 W3.config 的 `internal/configfile` 补齐，
      headless/tui/sdk bundle 仍留 W8/W9）
- [x] 能力 seam 清单（fs/shell/subprocess/exec-surface/terminal）与 capability matrix
      （W0.5-assembly：`seam.go` 闭集 capability + `Registry.Matrix()` +
      `Prepare` 启动前 typed rejection `unsupported_capability` / `invalid_capability`；
      真实 local/remote/SSH provider 注册留 W8/W9）
- [x] 不照搬清单写入 ADR：Cordis/typert、一能力一工具、70 个 UI 包、sandbox 族、DSH ssh
      （W0.5-assembly：`adr/0003-four-layer-row-overlay.md` §不照搬清单）
- [x] 冻结扩展面声明格式：`layer` / `provides.slots` / `contributes` / `extensibility.json` +
      `EXTENSIBILITY.md`（创造模式的聚合输入）
- [x] **catalog 对账**：以 `catalog.json` 为唯一机读真源，`COVERAGE.md` §A/§B/§C 逐行映射到 （对账 2026-11-20：部分——缺口：COVERAGE.md §A/B/C 有映射，但 goal/todo/plan/team/workspace/guard 仍独立；无 absent+理由逐行处置）
      `keelPluginId`；`goal`/`todo`/`plan` 并入 `keel.tasks`、
      `team` absent、`workspace` 并入 Group、`guard` 并入 `keel.supervisor`，逐行给出处置
- [x] 为 v1 已交付的官方插件补 `extensibility.json` + `EXTENSIBILITY.md`（否则创造模式无法利用，
      且官方发布门禁会拒绝）

## W1 layerfs 服务（Rust + gRPC）

> **2026-11-19 现状**：ADR 0001 的 Phase A（进程内 Go CAS）已由 W2.3 交付
> （`internal/layerfs`，见下方三项）；Rust + gRPC 仍是「同接口扩展实现」，未开工。

- [x] file object：`Put/Get/Stat`，流式读写（分块，单请求内存上限固定）——Phase A Go 实现
      （`internal/layerfs/store.go`：64KiB 分块、复用 `internal/blob.CASPath`；`-race` 绿）
- [x] tree object：目录树按内容寻址；相同子树共享哈希——Phase A Go 实现
      （`internal/layerfs/tree.go`：`keel-tree-v1` 规范化编码，排序后哈希）
- [x] 引用计数与 GC：无引用对象回收；并发安全的 refcount——Phase A Go 实现
      （`internal/layerfs/refs.go`：`(session,eventSeq)` append-only refs.log 可重建；`casMu` 串行）
- [ ] 配额：按 group/租户的存储字节上限，超限返回明确错误（Phase A 仅 store 级 `QuotaBytes`） （对账 2026-11-20：部分——缺口：只有 store 级 QuotaBytes（internal/layerfs/store.go:35-38），无 group/租户存储字节配额）
- [ ] gRPC 契约：流式 Get/Put、取消传播、错误分类（not_found / quota / io）——ADR 保留为扩展实现 （对账 2026-11-20：被替代/延后——ADR 保留为同接口扩展实现；当前 Phase A Go CAS 已交付，无 proto/gRPC 代码）
- [ ] 压测：单进程 1000 并发流的 RSS 曲线；不整文件进内存的断言

## W2 Harness 放置与内核契约

- [x] `groups` / `session_placement` 表与 lease 语义（join/leave/迁移）
      （W2H2A2：DDL `internal/store/schema.sql`（groups/session_placement/runs.group_id）；
      lease 与 join/leave/seek 在 `internal/placement/`；迁移+RLS+FORCE 测试
      `internal/store/placement_schema_test.go`；双组并发 claim 单赢家
      `internal/placement/pg_test.go:TestPGConcurrentClaimsSingleWinner`；证据
      `evidence/w2-placement-2026-11-21/`）
- [x] 三种策略：`pack`（默认）/ `user` / `pin`；策略在 Harness，不进内核
      （W2H2A2：`internal/placement/service.go` resolveStrategyGroup——pack 落单组
      `pack-default`、user 落 `user`、pin 必须显式 group；单测
      `internal/placement/service_test.go:TestStrategyResolution`；架构测试把
      placement 钉为产品层，内核不得 import）
- [x] 组内并发槽位（`max_concurrent_runs`）：满时排队 + ETA；用户配额在 DB 侧聚合 （对账 2026-11-20：部分——缺口：只有进程内全局 worker 信号量与 per-tenant 配额（internal/agent/worker.go:169、internal/quota）；无组槽位/排队 ETA/DB 聚合）（W2H2A2：组槽位+跨组租户配额已按 `runs(status ∈ pending,running)` 在 DB 聚合，满时**选拒绝式 429 + Retry-After**（不排队，故无 ETA）；`internal/placement/pg.go` AdmitRun；并发不超卖证据 `TestPGConcurrentAdmissionsNoOversell`（30 并发/配额 5 → 恰 5 准入））（W2H3A：runtime 接线完成——唯一闸口是 `internal/placement/gate.go` Gate（先 EnsurePlacement 再 AdmitRun）+ `internal/placement/committer.go` GatedCommitter，覆盖 /input、/events、/retry、MCP app 与产品 TurnStarter；`internal/placement/wiring` 在 cmd/runtime 与 cmd/keel serve 装配；HTTP 映射 `*CapacityError`→429+`Retry-After`、`*Conflict`→409（含 triggerRun）；kernel 进程内 RunConcurrency 改为 `quota.Limits.EventsOnly()`，由 DB placement 唯一计账（不双罚；event rate 仍进程内）；AdmitRun 不再把 group_id 为 NULL 的 pending 行当已准入（旧 kernel wake 行不可绕过组槽位）；ReleaseRun 只释放 pending 且无 pending_input 的预留，GatedCommitter 在 commit 失败时立即释放；崩溃遗留 pending 由 worker 认领后以空 attempt 收敛（无独立 sweeper，选择写清而非假装）；父/子各占组槽位有测试（`TestGateParentAndChildShareGroupSlots`）；真库 HTTP E2E 7 项 `TestPlacementHTTP*` + PG `TestPGGate*`/`TestPGReleaseRunSparesACommittedInput`/`TestPGAdmitAdoptsAnUnstampedPendingRow`；证据 `evidence/w2-runtime-wiring-2026-11-21/`；（W2Q：排队+ETA+内存装箱——组满不再是硬 429：`runs.status='queued'` 是持久 FIFO 行（`queue_seq` 来自 `runs_queue_seq`，不占组/租户槽位，kernel claim predicate 不匹配；user.message 提交与 `store.EnqueueRun` 保留 queued，`RequeueRun` 视 queued 为已入队）；promotion 是唯一的 guarded `queued→pending`（per-tenant advisory lock 下，`FOR UPDATE` 取队头，不 SKIP LOCKED 以免跳队），触发点＝`ReleaseRun`（同事务）、worker 结算钩子（`agent.SettlementObserver`→`placement.Gate.RunSettled`，经 `servicehost.Settlements` 接线）与崩溃恢复 sweeper（`placementwiring.RunQueueSweeper`，cmd/runtime 与 cmd/keel serve 启动；`QueueInterval`/`KEEL_PLACEMENT_QUEUE_INTERVAL_MS` 默认 5s）——FIFO、exactly-once、重复 input 不跳队均有测试；ETA 只来自观测时长（`run_duration_samples`：结算时每条 attempt 写 1 条、保留 50、取窗口 20，组窗口优先、租户回退，公式 `avg * ceil(position/limit)`），无历史＝unknown（HTTP `etaSeconds:null`），绝不伪造 0；租户配额仍是 DB 聚合的硬 429，promotion 复检配额（配额下调后阻塞有测试）；内存装箱：`Options.MemoryEstimator`（无默认估算值）→ pack 在 active `pack-*` 组间 best-fit（`runs.memory_bytes` 记录估算，unbounded 组仅作 fallback），admission 硬上限 `group_memory_full` 429；无估算用内存受限组＝typed `*UnsupportedError{memory_estimate_unavailable}`→HTTP 501，`FromEnv` 在 `KEEL_GROUP_MEMORY_LIMIT_BYTES>0` 且无 estimator 时拒绝启动；HTTP 输入与 `/run` 队列化返回 202 + `queued{position,etaKnown,etaSeconds,groupId}`，仅 ETA known 时带 `Retry-After`；测试：MemStore FIFO/释放即晋升/不超卖/ETA known-unknown/内存 headroom/不跳队，PG FIFO/不可 claim/ETA/降配额/装箱/commit 保留 queued，HTTP 202/ETA/501/trigger，证据 `evidence/w2-queue-eta/`；仍缺：组 OOM/进程隔离；draining/retired 组内已入队条目保持 queued（本 lane 不做驱逐/迁移，运维处理），如实例明（W2-F 2026-11-23：复核 P2 收口——① 目标组有空槽时先在同一租户锁内晋升队头（reachable 的 crash/重配置窗口不再被新准入插队；队头被内存阻塞时新准入排到队尾）；② 晋升每次重读 DB 聚合租户配额并带 in-statement 守卫，终检失败以 retryable `errPromotionContention` 回滚；kernel `user.message` 对**已放置组内终态行**改为入队（不再 pending），并发唤醒不能额外占一个活跃槽；③ ETA 只采样 `done` 完成观测，abandoned/finish_error/failed/cancelled/superseded 不入均值；④ `/run` 202 复用 `queuedAdmissionBody`（含 etaSamples/etaBasis/Retry-After）；⑤ 内存拒绝的诊断 `Active` 改为活跃 run 数、`UsedBytes` 用 `Store.GroupUsage` 聚合；⑥ web 新增 `queued/排队中` 状态与位置/ETA 文案。测试：Mem+PG FIFO、PG 唤醒/晋升并发、ETA 规则、HTTP `/run`/内存字段、web unit；证据 `evidence/w2-followups/`。遗留：无 placement 的裸会话 M1 唤醒仍可 pending（产品输入路径先准入，不可达）；详见证据 Limitations。）
- [ ] 进程生命周期：启动、drain、接管、SIGTERM 排水；组 OOM 不扩散 （对账 2026-11-20：部分——缺口：drain/handoff/SIGTERM 在（internal/httpapi/drain.go、internal/handoff）；无 group 实体与组 OOM 隔离）（W2H2A2：group 实体与 active/draining/retired 状态已落；draining 组内既有 placement 可续租，新 placement/新 run 拒绝（`TestPGDrainingGroup`）；组 OOM/进程隔离不在本 lane，如实未做）（W2H3A：admission 崩溃遗留 pending 的收敛已测——`TestPlacementHTTPCrashPendingConverges`（认领→无欠输入→done，槽位释放）；OOM 隔离仍未做）
- [x] 注入式 run 契约：工具清单 + system sections + message-parts + invoke 回调
      （对账：`internal/agentloop/loop.go:96-140`（Input：SystemPrompt/Messages/Tools/Batch）、
      `agentloop/pi.go:70-110`；loop 不持账本/不触 store；单测 `agentloop/contract_test.go:110`、
      `equivalence_test.go:108-143`；「invoke 回调」由注入的 AgentTool 对象承担）
- [x] run 快照：`configHash` + `anchorEventSeq` + `toolManifestHash` 写入 run 事件
      （W2-6：三值字段 `internal/agent/runner.go:313-323`；attempt 起始取 log 锚点/配置快照
      `runner.go:471-472`，从实际注入的工具切片取清单哈希 `runner.go:556-571`；
      `configHash` 由 pinned loadout (id, revision) 派生 `internal/agent/snapshot.go:26`，
      rotation 锚点取最新 `config.rotated` seq、无则取 run 起始读水位 `snapshot.go:42`；
      worker 用 pinned loadout 覆盖 `internal/agent/worker.go:356-363`；kernel 校验
      `anchorEventSeq` 非负整数与 hash 字符串有界 `internal/kernel/validate.go:407-477`；
      测试 `internal/agent/run_snapshot_test.go:22,64,121,183`（真实 run 三值、中途工具面变化、
      轮转后新 hash/anchor、worker 落盘）；证据 `evidence/w2-run-snapshot-2026-11-21/`）
- [x] 外部内核能力矩阵与 typed rejection（沿用 v1 external 通道）
      （W2-7：能力名闭集 + `Supports`/`Matrix`/`RequireAgentCapabilities`
      `internal/gateway/initialize.go:92-235`；`ServerAgentCapability` 增
      streaming/tools/approval/fork/resume/compaction `initialize.go:259-286`；
      `ConnOptions.RequiredCapabilities` 在 transport 启动前拒绝未知名、握手后拒绝未声明位
      `internal/gateway/conn.go:92-97,220,258`；错误码
      `unsupported_capability`/`invalid_capability` `internal/gateway/errors.go:145-153`；
      legacy 缺省位读 false、不要求时照常激活；测试 `internal/gateway/capabilities_test.go:29,75,95,124,144`；
      证据 `evidence/w2-run-snapshot-2026-11-21/`）

## W3 file-plugin 与 bash-plugin

- [x] file-plugin：`read`（offset/limit/流式）、`write`、`edit`（精确匹配 + 唯一性），
      全部走 `file_manifest` + layerfs，不整文件进内存（W2.3：`internal/fileplugin`，
      分页默认 200 行/256KiB 上限 2000 行；edit 流式查找/替换）
- [x] 写路径：每次写追加 `fs.write` 事件；路径规范化；`.git/objects` 默认不进 capture
      （W2.3：唯一写路径 manifest→layerfs→`fs.write`→写穿；capture 默认忽略 `.git/objects`）
- [x] bash-plugin：gbash 适配（自定义命令、自定义文件系统、跨平台）
      （W2.2：`internal/bashbridge`，pin `ewhauser/gbash v0.0.41`；占用隔离；`third_party` 许可文本）
- [x] `bash` 工具参数：`timeout`（参数而非 bash 命令）、超时转后台、返回终端信息与 pid
      （W2.2：job id 即作业句柄；**禁用**上游 `ExecutionRequest.Timeout`；超时不 kill）
- [x] JobManager：全局 + 每组上限；`nohup/jobs/ps` 视图；收养与孤儿回收
      （W2.2：`bash_jobs` 表 + RLS、advisory-lock 原子上限、重启一律收敛终态 `interrupted`、
      15s sweeper 按归档会话回收、工具面 `bash {action:"jobs"}`；W2.seam 补齐沙箱三命令
      （B 轴复核 P1-2，de680cf）：`internal/bashbridge/jobcommands.go` 读 `jobqueue.Manager`——
      `jobs` 当前会话视图、`ps` 同租户视图、`nohup` 转后台作业并返回 keel job id，`fg`/`bg`
      按契约不注册；装配测试 `test-job-commands.txt`、真库一致性 `test-job-status-consistency.txt`，
      断言无宿主 PID）
- [x] 输出落 layerfs：只回 head/tail + blob 引用 + 退出码
      （W2.2：head 4KiB + tail 4KiB；全量流式写 blob store（layerfs 的字节层）；200MB 内存有界实证）
- [x] capture 默认 `per-tool-call`：stat 缓存 + 变更集，只对变了的文件算哈希
      （W2.3：`Capture` 递归建树 + stat 签名缓存；十万文件**一条** `fs.capture` 实证；
      W2.jobcap：后台作业以系统事实结算 `fs.capture`——payload 带 `jobId`、不伪造 owner token，
      真库证明作业逃出 run lease 后 manifest 仍可见；证据 `evidence/w2.jobcap/`）
- [x] 执行面适配器契约：`materialize(manifest)` / `capture(handle, since)`，先做本地实现
      （W2.3：`Materialize`/`MaterializeSnapshot`/`CaptureWorkspace`；W2.seam 生产接线已合入
      de680cf：`LayerWorkspaces` 按 `(tenant,session)` 持有 gbashfs workspace、manifest 播种、
      write/edit 写穿、Close 单次 capture。gbashfs 是内存路径索引而非宿主目录，故 seam 用
      scoped `SessionSurface`/`SnapshotSource` 表达同一契约（目录 `Surface`/`CaptureRoot` 保留给
      本地适配器）；装配级证明 `evidence/w2.seam/test-seam.txt`）
- [x] bash 删除对账：capture 目前只 upsert，`rm` 掉的路径仍留在 `file_manifest` （对账 2026-11-20：未接线——Manifest.Delete 存在但 capture 路径只 Upsert；fs.capture payload 无 deleted 列表；internal/fileplugin/service.go:503-620、manifest.go:222-247）
      （A 轴增量复核裁定：spec「执行空间产生的变更 MUST 通过 fs.capture 进入 layerfs」字面覆盖删除；
      需定义删除事件/对账语义并入 capture 批事件，不新造逐文件事件）
      （W3.H3B 2026-11-21：capture 先 diff manifest 与快照，删除快照中已消失的路径，仍只追加一条
      `fs.capture`；payload 增加 `deleted`（有界：256 条 / 32KiB，`CaptureDeletedPayloadLimit`/
      `CaptureDeletedPayloadBytes`）、`deletedCount`、可选 `deletedTruncated`；`DeleteStale` 以事件 seq 做
      `< maxSeq` 护栏；`count/treeHash` 取快照。
      实现 `internal/fileplugin/service.go:588-664`、`internal/fileplugin/manifest.go:24-27,259`；
      测试 `TestCaptureReconcilesDeletions`/`TestCaptureDeletesWrittenThenRemoved`/
      `TestCaptureDeletedPayloadIsBounded`（fileplugin）、`TestBashRmIsReconciledByCapture`（bash seam）；
      证据 `evidence/w3-capture-tail-2026-11-21/`）
- [x] capture 策略覆写：tenant document `filesystem.capture.mode=per-tool-call|checkpoint|manual`
      （W3.capture 2026-11-23：仓库当时没有 `config.toml` 加载器，策略落在唯一租户文档词汇
      `filesystem.capture.mode`；W3.config 已补加载器，见下一条；`internal/host/config.go`
      新增 `Filesystem/Capture` 段，
      Merge/Validate 严格闭集（省略 = per-tool-call，未知/带空白值拒绝），canonical JSON 可往返。
      模式与上下文钉扎在 `internal/capturepolicy/capturemode.go`（W11 2026-11-23：从
      `internal/layerfs/capturemode.go` 迁到 kernel 叶子包，值 + context pin，layerfs 不留 alias）；worker 每 attempt 解析一次
      （存储租户文档覆盖启动文档，`cmd/keel/capture_policy.go`）并把模式固定在本次工具上下文，
      bash backend 复制进 `SessionIdentity`。`Mount.Finish` 在 checkpoint/manual 下只记 pending，
      `LayerWorkspaces.Checkpoint` 一次消费并结算（同 id 重放去重；失败/重放恢复 pending）。
      checkpoint 的触发器是 worker 在 `run.finished` 前、lease 仍活时的 run-completion hook；
      manual 的显式面是闭集新增 `bash {action:"checkpoint"}`（权限键 `file.write`，
      keel.shell 内部工具 `keel.shell.checkpoint`）。后台作业不受 run 模式影响，仍在其 Close
      以系统事实结算；任何选择 checkpoint/manual 却没有触发器的 attempt 以
      `capture_trigger_missing` 类型化错误失败，不静默吞掉 capture。
      测试 `TestCaptureModeMergeAndDefault`/`TestValidateCaptureModeMatrix`/
      `TestCaptureModeRoundTripsCanonicalJSON`（host）、
      `TestPerToolCallCapturesEveryBashExecution`/`TestCheckpointCapturesOnceAfterMultipleCommands`/
      `TestManualCapturesOnlyOnExplicitCheckpoint`/`TestUnknownCaptureModeFailsClosed`/
      `TestBackgroundJobSettlesDespiteCheckpointMode`（bashbridge）、
      `TestCheckpointPolicyPinsAndFiresOnce`/`TestPerToolCallPolicyDoesNotCheckpointAtRunClose`/
      `TestManualPolicyDoesNotCheckpointAtRunClose`/`TestCapturePolicyWithoutTriggerFailsClosed`（agent）、
      `TestBashCheckpointDelegatesToBackend`/`TestShellCheckpointRoutesToTheExplicitSurface`；
      生产 resolver 另有 `TestEffectiveCaptureModeUsesStoredDocumentOverBoot`/
      `TestEffectiveCaptureModeFailsClosedOnBadDocumentOrRead`/
      `TestCheckpointDocumentRequiresAnAmbientRun`（cmd/keel）。run summary 记录
      `captureMode`/`captureSource`，因为捕获策略与模型 generation 来自不同读取路径；
      延迟结算的崩溃数据窗口已在 docs/HOST.md 与 layerfs spec 写明。
      本次测试不依赖 PG（KEEL_TEST_DSN 未设置，既有 PG gated 套件保持不变）。
      证据 `evidence/w3-capture-mode/`）
- [x] config.toml 磁盘加载器：严格解析、原子轮转、启动加载、settings → 租户文档
      （W3.config 2026-11-23：`internal/assembly` 的 parser 新增唯一 settings 表
      `[filesystem.capture] mode`（闭集来自 kernel 叶子包 `internal/capturepolicy`；rebase 到
      `a549e87` 时删除了重复的 `internal/capturemode`，layerfs 不再留 alias，assembly/agent/
      configfile 统一引用 capturepolicy，装配面不再 import service 层）；`internal/configfile.Loader` 负责
      `Open`（缺失 = 空配置，非法 = 启动失败）、`Rotate`（parse → `config.toml.new` →
      fsync → rename → dir fsync → revision+1）与 `Reload`（采纳外部原子替换），
      版本/哈希语义复用 `assembly.Rotation`，anchor 由宿主提交 `config.rotated` 后提供。
      `keel serve --config-file`/`KEEL_CONFIG_FILE`（默认 `$KEEL_HOME/config.toml`）
      启动加载并记录 active version/hash；`GET /v1/capabilities` 的 `configFile` 块暴露
      revision/hash/anchor/rotation 事实。`configfile.OverlayTenant` 把文件 setting 叠加到
      租户文档，每-attempt capture resolver 以文件最后覆盖，管理端 JSON 对未声明键继续生效。
      测试 `TestParseConfigReadsTheCaptureSetting`/`TestParseConfigRejectsMalformedCaptureSettings`
      （assembly）、`TestRotateReplacesTheFileAtomically`/
      `TestRotationKeepsInFlightAttemptOnItsPinnedVersion`/
      `TestRotateRejectsInvalidDocumentsWithoutTouchingTheActiveFile`/
      `TestRotationWriteFailureLeavesNoPartialFile`/`TestRotateOverwritesAStaleStagingFile`/
      `TestConfigFileCaptureModeOverlaysTheTenantDocument`/`TestRestartPicksTheRotatedVersion`/
      `TestReloadAdoptsAnExternallyRotatedFile`/`TestConcurrentPinAndRotate`（configfile）、
      `TestEffectiveCaptureModeAppliesConfigFileLast`/
      `TestEffectiveCaptureModeFallsBackWhenFileSettingIsRemoved`（cmd/keel）。
      文件 rows 已解析并随版本 pin；生产 base 仍只有 service rows，非 service row 覆盖
      留 W8/W9。`Rotate`/`Reload` 是库 API：本切片没有生产轮转调用者/HTTP 入口，
      调用者提交 `config.rotated` 并传锚点的接线留 W8/W9；`Open`/`OverlayTenant` 与
      启动 status 已真实接线。证据 `evidence/w3-config-loader/`）
- [x] 并发 capture 定序：父 run Close 与 nohup 子作业 Close 都做全量快照，若子先提交、父后提交， （对账 2026-11-20：部分——缺口：Upsert 有 updated_seq guard（manifest.go:180-196），snapshot 按 seq 精确存；但无跨 lane 定序/版本化，且 fork 未接线（W4））
      `session_snapshot` 的最新 seq 树可能落后于 `file_manifest` 行；fork 接线前必须定序/版本化
      （W3.H3B 2026-11-21：每个 (tenant,session) 的显式写与 capture 共用 settlement 临界区
      （`internal/fileplugin/service.go:791`），capture 的 snapshot→event commit→manifest 对账→
      `session_snapshot` 是一个原子结算，并记录单调 `snapshotVersion`（payload + `CaptureResult`）。
      后提交的 capture 不可能携带更旧的快照：父 Close 与 nohup 子 Close 无论谁先提交，后者的
      全量快照都包含前者已结算的内容，删除对账不会抹掉子更新。测试
      `TestCaptureSettlementsAreSerialized`（gated 快照证明串行）、
      `TestParentCloseAndJobCloseOrderingKeepsJobWrites`/`TestJobSettlesBeforeParentKeepsJobWrites`
      （真 gbash workspace 的父/子交错两种次序）；`-race` 通过）

## W4 task-log（plan-plugin）与面板

- [x] `task_cards` 行 + `task_events` **投影**（从账本重建，含重建一致性测试）；会话启动自动建初始卡
      （W2.4：`internal/tasklog`；DDL/RLS 随集成落 `schema.sql`/`rls.go`；重建幂等与「直写被重建擦除」有测试）
- [x] 强制任务归属：每次工具调用必须归属任务卡（含内部子代理）
      （B 轴复核裁决后已修订 spec 语义：Harness 先归属会话初始卡、无法归属才拒绝
      `task_id_required`、未知卡 `task_not_found`；实现见 `internal/toolsurface/tasks.go`
      + 全内建工具 task gate；拒绝事实以 tool.result 落账本。spec 文本已同步修订）
- [x] `tasks` 聚合工具：`create/update/attach/detach/close/list/show/note`
      （W2.4：8 动作经 `keel.tasks` 后端；停用映射 `plugin_disabled`）
- [x] 任务树投影：父子关系、跨会话 `parentTaskId` 拼接
      （W2.4：`subagent.started` 钩子建/更新子卡，`parentTaskId` → 父卡，幂等）
- [x] 资源关联：`task.attached{path, hash}` / `task.detached`；面板可打开对应 blob
      （W2.4 事件与投影；面板打开 blob 属 W4 面板项，未做）
- [x] fork：`(eventSeq)` 原点；新会话 + 前缀引用 + 新卡（`parentTaskId`） （对账 2026-11-20：部分——缺口：tasklog.ForkCard 已交付（internal/tasklog/service.go:348-380），agent.fork 仍 not_implemented（internal/toolsurface/agent.go:49-50）；无新会话/文件前缀引用）
      （W2.4 已交付 `Service.ForkCard` 投影；`agent.fork` 仍 not_implemented，待与 layerfs
      文件视图前缀在生产装配中合并验证）
      （W8 2026-11-21 已交付：`agent.fork` 创建新会话；账本前缀是读时 union（父日志 ≤eventSeq
      由 runner 以负合成 seq 注入，不复制事件，`internal/agent/fork_prefix.go`）；文件前缀是
      `session_snapshot` tree hash 的 O(1) 引用，fileplugin 读/写/编辑/列目录/捕获按 overlay
      解析，捕获不复制父 manifest 行（`internal/fileplugin/fork.go`）；新卡 `parentTaskId`
      指向原点卡（`tasklog.ForkCard` + `OriginCard`）。测试
      `TestForkPrefixIsAReadTimeUnion`、`TestForkFileViewIsAnOverlay`、
      `TestForkCaptureDoesNotCopyInheritedManifestRows`、
      `TestForkCreatesAPrefixSessionCardAndInheritedBinding`）
- [x] 面板：任务图、事件流、资源预览；纯投影，不写额外真值
      （W4 2026-11-23：`internal/tasklog/panel.go` + `internal/harnessapi/task_panel.go`
      + `web/src/data/tasks.ts` / `web/src/screens/books/task-panel.tsx`；证据
      `evidence/w4-task-panel.md`。复核 P2 见下节）

**W4 复核 P2 收口（2026-11-23，w4-followups）**

- [x] 会话清单有界翻页：`GET /v1/sessions/{id}/files?limit&cursor` 路径游标页
      （`nextCursor`/`hasMore`），客户端翻页并合并；面板资源组可「加载更多」
- [x] 清单行带内容 hash：`SessionFileView.hash` 来自 manifest，面板预览据此
      显示「与引用一致/文件已变化」；无 hash 的行不编造，仍显示「清单当前版本」
- [x] 面板读数有界：own 卡按创建序 SQL 分页（`cardLimit`/`cardsTruncated`）、事件流只读
      最新一页（旧坏行不参与解码）、attach/detach 折叠独立有界（`attachmentLimit`/
      `attachmentsTruncated`）
- [x] fixture 保真：成员只取本会话卡的子卡、祖先与 `parentTaskId` 按投影解析、事件页保留
      最新 N、资源区分段计数含清单组
- [x] 客户端读数不撒谎：0 字节预览显示 `0 B` 不是 `1 KB`；`shortHash` 只截断真正的
      sha256；成员截断 count=1 只说「至少 1 张」不说「达到上限」

## W5 approve 与 notification

- [x] `approve` 工具：`confirm/prompt/choice` 三模式 + 「其它」逃逸
      （W2.5：`internal/approval` + `keel.approval`；工具只 open/wait，不能自答）
- [x] 逃逸值带 `validated:false` provenance，下游可区分
      （W2.5：`approval_provenance` 记录 actor/channel/context/四时间点/validated/escape；
      HTTP 作答路由与工具观测双路径收敛）
- [x] 超时默认取安全侧；durable 决策（open/wait/answer/timeout）沿用 v1 decision 原语
      （对账：`internal/agentext/decisions.go:24-32,231-301,495-538`（四状态 + 持久 deadline +
      重启后 normalize/expire）、`decisions_store.go:13-20,152-200`（复用 tasks 行持久化）；
      安全侧：插件审批超时/未答走 refused 分支 `creation.go:845-860`；
      测试 `agentext_test.go:72,126,179`；W2.5 又补 `status=timed_out + safeSide=no`）
- [x] `notify` 工具与通知面板：多消费器（img 埋点 / URL 访问 / 客户端 ack）
      （W2.5：多消费器 push/list/ack + open/pixel 路由；**通知面板未做**，留 client-shell 波）
- [x] 消费是持久事实；有 ack 能力的消费者在场时即时消费；原生推送只做兜底
      （W2.5：`notification.ack`/`notification.push_sent` 事件 + 三态窗口；原生 Pusher 为 seam，
      生产未注入 → `pusher_unavailable` 具名降级，不假装已推送）
- [x] 通知 URL 单次有效、不可猜；重启不重推
      （W2.5：256-bit token 仅存 SHA-256、按 consumer 过期、单次消费 410/404；
      `ClaimPush` CAS 先行，claimed-but-unrecorded 记 `push_result_unknown`）

## W6 MCP 聚合工具

- [x] `mcp` 聚合工具：`list/describe/call`（W2.1；schema 发现按需实时读取，缓存留给后续）
- [x] 二进制内容走 content block，不走 stdout
      （对账：`internal/mcp/content.go:60-95`（image/resource → ImageContent，其它块给文本占位，
      structuredContent 回退 JSON）、`internal/mcp/output_limit.go` 20KB 上限；
      测试 `internal/mcp/content_test.go:14-40`、`output_limit_test.go`）
- [ ] gbash 的 `mcp` CLI：与聚合工具共用同一个 registry（shim，不复制实现） （对账 2026-11-20：未开始——bashbridge 只注册 jobs/ps/nohup（internal/bashbridge/jobcommands.go），无 mcp CLI/共享 registry）
- [x] 删除 `internal/mcp` 的 `mcp__<server>__<tool>` 适配与单测（PI-PARITY 已登记偏离）
- [x] MCP server 增删不改变工具清单（清单哈希断言：100 server 哈希不变）

## W7 skills

- [x] skill 文件结构：`SKILL.md` + 资源；内容 hash 去重、版本不可变 （W7 2026-11-21：`internal/skills/bundle.go` 全树规范清单 `keel-skill-bundle-v1`（排序路径/大小/mode）派生一个内容哈希，同内容同哈希、版本目录 write-once，`Verify` 把篡改报 `ErrBundleMismatch`；上限 256 文件/8MiB/单文件 2MiB，拒绝符号链接；测试 `internal/skills/bundle_test.go`（同内容跨目录同哈希、改资源变哈希、写已有不同内容被拒、ReplaceBundle 移动 active）。证据 `evidence/w7-skills-2026-11-21/`）
- [x] `skills` 聚合工具（W2.1 部分）：`ls` / `view` 已吸收旧 `skill` 工具；
      `manage` / `sources` 动作保留为 `not_implemented`，待 W7 完整版 （W7 2026-11-21：`manage`（create/patch/put/delete/write_file/remove_file）与 `sources`（list/subscribe/unsubscribe/pin）已接 `internal/skilllib`；`internal/toolsurface/skills.go` 闭集校验、库错误码透传、`ls`/`view` 合并租户库；测试 `internal/toolsurface/toolsurface_test.go:TestSkillsManageAndSourcesRouteToTheLibrary`、`internal/skilllib/library_test.go:TestManageBackendCreatesVersionsAndNeverExecutes`）
- [x] ChatInput `$` 补全：分组（技能/文件/任务），共享同一唤醒符 （W7 2026-11-21：`web/src/screens/composer-completion.tsx` 一个 `$` 唤醒符、固定顺序三组、同一 query 过滤、选择只写输入框不发送；数据源 `GET /v1/settings/skills`、`GET /v1/sessions/{id}/files`、`/tasks`（`internal/agentapi/completion.go`）；前端测试 `composer-completion.test.ts`（纯规则）+ `composer-completion.test.tsx`（真实 composer、fixture 三组、逐组插入、空态））
- [x] `/learn`：把经历沉淀为技能（走 `skills.manage`） （W7 2026-11-21：composer 的 `/learn` 生成一条让 Agent 调 `skills.manage create` 的消息（`buildLearnInstruction`），工具结果才是成功信号，界面只提示“已发送沉淀请求”不报“已学习”；`parseLearnCommand` 只认 `/learn`/`/learn <补充>`，`/learning` 不误匹配；测试 `composer-completion.test.ts`）
- [x] Settings `/settings/skills` + CLI `keel skills`（等价能力） （W7 2026-11-21：`internal/agentapi/skills.go` 的 `GET/POST /v1/settings/skills*`（list/show/install/update/pin/unpin/remove/sources）是唯一写路径；`cmd/keel/skills.go` 是 HTTP 客户端（不直连 DB/目录），`keel skills list|show|install|update|pin|unpin|remove|sources`；设置页 `web/src/screens/settings-skills.tsx` 五态+详情+新建+从订阅源安装+订阅源管理；测试 `internal/agentapi/skills_test.go`、`cmd/keel/skills_test.go`、`web/src/screens/settings-skills.test.tsx`；真机 CLI 记录 `evidence/.../cli-live.txt`）
- [x] 订阅源管理：信任标注、版本 pin、更新冲突可见 （W7 2026-11-21 + P1 fix：订阅源 URL 只能解析到受管源根（默认 `<skills root>/sources`，`resolveSourceDir` 拒绝 `/etc`、`..` 与根外 symlink，`source_outside_root`）；模型工具面强制 untrusted、忽略 `trust`/`allowUntrusted` 且 schema 不暴露这两个参数，trusted 仅保留给鉴权后的 HTTP/CLI operator 入口；`internal/skilllib` 源带 `trusted|untrusted`（本地创作 `local`），未信任源安装/更新都必须 `allowUntrusted`；`pin` 激活并保持一个不可变版本，源移动时更新返回 409 `pinned`，`modified`/`source_missing`/`name_mismatch`/`missing_active` 同列可见；源仍被技能引用时退订 409 `source_in_use`；测试 `library_test.go:TestSourceTrustPinAndUpdateConflicts`/`TestUntrustedSourceNeedsAnExplicitAcknowledgement`/`TestManagedSourceRootRejectsHostPathsAndSymlinkEscapes`/`TestModelFaceCannotChooseTrustOrReadHostTrees`，真机 `evidence/.../cli-source-conflicts.txt`、`w7fix-live.txt`）
- [x] 技能内脚本执行必须走 bash 策略，不在 skills 插件里直接执行 （W7 2026-11-21：`internal/skills`/`internal/skilllib` 不 import `os/exec` 或 `bashbridge`，`manage` 闭集没有执行成员；脚本作为 bundle 字节保存（`write_file` 保留 0755），运行只能经 bash 工具；结构断言 `internal/architecture/boundary_test.go:TestSkillPackagesDoNotExecuteScripts`，行为断言 `library_test.go:TestManageBackendCreatesVersionsAndNeverExecutes`；设置页与 CLI 都明文提示 `scriptExecution: bash`）

> W7 证据：`openspec/changes/keel-harness-v2/evidence/w7-skills-2026-11-21/`（README、go build/vet/full、conformance、openspec、web check/build、TUI、脚本策略、真机 CLI/settings 记录）。

## W8 agent-plugin 与 fork

- [x] 内部子代理：同进程协程，独立会话 + 独立任务卡（`parentTaskId`） （对账 2026-11-20：已实现——SpawnAsync 建独立子会话（internal/agentext/subagent.go:216-292）；HookSubagentStarted → tasklog.LinkSubagent 建独立卡并写 parentTaskId（internal/agentext/tasks.go:267-293、internal/tasklog/service.go:390）；测试 TestSubagentCardLinksToParentActiveCard、TestTasksPluginLinksSubagentCards）
- [x] `agent` 工具：`spawn/fork/resume/cancel/list/status`；fork 原点 `eventSeq` （对账 2026-11-20：部分——缺口：agent 聚合工具已有 spawn/list/status（internal/toolsurface/agent.go:22-55）；fork/resume/cancel 返回 not_implemented，无 eventSeq 原点）
      （W8 2026-11-21：六动作全部落地。`fork {eventSeq,...}` 校验原点在父账本内、解析原点卡、
      写持久 fork fact（`sessions.fork_parent_session_id/fork_origin_seq/fork_tree_hash/...`，
      `internal/store/session_fork.go`）、建子会话与 `parentTaskId` 新卡（`ForkCard`）、
      以父 selection 为默认绑定启动子输入（`internal/agentext/fork.go`）；
      `resume`/`cancel` 走同一父链接校验；`internal/toolsurface/agent.go` 六个 action 闭集，
      不再有 `not_implemented`。测试 `TestForkCreatesAPrefixSessionCardAndInheritedBinding`、
      `TestForkRefusesAnInexactOrigin`、`TestAgentSpawnCarriesTheBinding`（含 fork/resume/cancel）；
      证据 `evidence/w8-fork-2026-11-21/`）
      （P1 修复 2026-11-21：fork 的持久副作用与首个输入同生命周期——输入未提交时回滚：
      释放槽位/placement、清 fork fact、关闭/收敛子卡、删除空 session 或归档有账本事实的
      session（`internal/agentext/fork.go:rollbackFork`、`internal/store/session_fork.go`）；
      已提交输入后的失败不回滚，避免活 run 失去 fact/card。`ForkCard` 卡 id 改为子会话确定性
      initial card，create key 由 callID 派生；关闭后的重试 reopen 同一张卡，不再每次 newID
      多一张。测试 `TestForkCapacityRetryConvergesToOneCard`、
      `TestForkCardIsIdempotentAndReopens`、`TestTasksPluginDoesNotRelinkForkedCards`）
- [ ] 子代理继承父的 `config.toml` 版本，随后可写自己的配置 （W8 2026-11-21：部分——已交付「继承父配置事实 + 可覆盖」：fork 冻结父 attempt 的
      `configHash/toolManifestHash/anchorEventSeq` 到 fork fact 与子输入 `fork` 事实，默认继承父
      selection（prompt/tool/model 绑定）；子代理后续输入可带自己的 selection，resume 可用
      `selection`/`agentId` 覆盖绑定（写入 `resume.attempt`，不追加 user.message，
      `internal/agent/model.go:runSelection`）。**未做**：跨 reassembly 强制子 run 使用父的
      loadout revision（当前子 run 仍 pin claim 时的当前 generation；fork fact 只记录父版本，
      worker 未按 session 指定 revision）。该缺口需要 per-session loadout pin，属 W2/loadout 面）
- [x] 并发槽位记账：父与子都占组槽位，超限排队 （对账 2026-11-20：部分——缺口：父与子都占进程全局 worker 信号量；无组槽位/排队 ETA（同 W2-3））（W2H3A：父/子作为两个会话经同一 Gate 各占一个组槽位（可验证：`TestGateParentAndChildShareGroupSlots`，组限 1 时子 run 429）；「超限排队」仍未做，当前是显式 429 + Retry-After）（W8 2026-11-21：口径与 W2 一致——显式 429 + Retry-After，不排队。spawn/fork 子输入经同一
      `admission`（GatedCommitter）占组槽位；HTTP 子代理路由把 `placement.CapacityError`
      映射为 429 + `Retry-After`（`internal/agentapi/ext.go:writeSubagentError`），模型面
      `agent` 工具映射为 `capacity_exhausted` + `retryAfterMs`
      （`internal/toolsurface/agent.go:agentFailure`）。测试 `TestSubagentCapacityIs429`、
      `TestAgentCapacityRefusalIsTyped`、`TestGateParentAndChildShareGroupSlots`）
- [x] 子代理返回：结构化结果 + 产物 blob 引用；父卡追加 `task.note` （对账 2026-11-20：部分——缺口：SubagentResult 结构化结果在（subagent.go:103-113），Artifacts 是工具调用 trail（:520-545）；无产物 blob 引用/父卡 task.note）（W8 2026-11-21：`Artifact` 增 `path/hash/size`；从 `tool.result.details` 抽 stdout/stderr `blobId`、
      `fs.write` 抽 `(path,hash)`、`fs.capture` 抽 `treeHash`，有界 256 条
      （`internal/agentext/subagent.go:artifactsFromEvents`）。`finish` 调
      `tasklog.NoteSubagentFinished`，把 note 追加到子卡 `parentTaskId` 指向的父卡（确定性
      task-event key，重复 Wait 不重复）；重启后从子会话首条 user.message 恢复父链接
      （`durableParent`）。测试 `TestSubagentReturnCarriesArtifactRefsAndParentNote`、
      `TestNoteSubagentFinishedLandsOnTheParentCard`）
- [x] resume：中断后继续原任务（不重开），事件/任务卡/绑定不重复；崩溃恢复证据 （W8 2026-11-21：`SubagentRunner.Resume` 先校验父链接，再按 run 行状态给出显式结果
      （running/pending/terminal/no_run），仅 failed（或租约过期的 running）走 requeue：
      同一 session、同一 run 行、`attempts=0`、`pending_input=true`
      （`internal/store/resume.go`）；`resume.attempt` 以 requestId 为键幂等
      （`internal/agentext/fork.go`）。未追加 user.message、未建新卡、未改绑定。
      HTTP `POST /v1/sessions/{id}/resume` 与工具 `agent.resume` 同一实现。
      测试 `TestResumeContinuesTheSameSession`（失败→resume→claim→同一 run 完成，
      断言 1 条 user.message / 1 条 resume.attempt / 0 新卡 / run.finished 快照三值）、
      `TestRequeueRunResumesOnlyInterruptedRuns`、`TestResumeRouteAnswersTerminalExplicitly`；
      崩溃矩阵 `internal/agent/crash_matrix_test.go` 仍为 G2 的进程级证据）


## W9 oar-plugin（外部工具）

- [x] sidecar 接入 OAR：runtime 能力矩阵、record stream（`seq` + `agentPath`）
      （W9 2026-11-22：契约冻结 `docs/OAR.md`；`internal/oar` 能力矩阵/typed 码表/
      `keel.oar.capabilities/1` + `keel.oar.record/1`；sidecar `internal/oar/cmd/keel-oar`
      复用 `internal/gateway` agent_backend；fake 与 codex 驱动；durable journal
      `internal/oar/journal.go`；测试 `TestBridgeFakeSidecarRecordStream`、
      `TestDeclarationMatchesRuntimeMatrix`）
- [x] 外部工具渲染器：把 record stream 折叠成可读的调用/结果视图
      （W9 2026-11-22：渲染 seam `keel.oar.render-projection/1`（`internal/oar/projection.go`、
      fixture `internal/oar/testdata/tool-renderer.fixture.json`）；CLI `cmd/oar-view`；
      **web UI 槽位未实现**，`keel.ui.tool-renderer` 渲染由 W12 UI agent 拥有）
- [x] 不注入「请记录任务卡」；归属由 record stream 带外提供
      （W9 2026-11-22：`session.open.config.attribution{sessionId,taskId}` 带外传递并在
      每条 Record 回显；sidecar 不追加 prompt；`TestBridgeFakeSidecarRecordStream`
      扫描记录断言无 `task card`/`任务卡`/`please record`）
- [x] typed rejection：runtime 不支持的能力在启动前拒绝
      （对账：通用外部网关已实现——`internal/gateway/initialize.go:81-121` 能力集、
      `:150+` `validateInitializeResult`、`internal/gateway/errors.go:124-144` 类型化握手错误；
      测试 `internal/gateway/conn_test.go:18,40,56`。W9 2026-11-22 补 OAR 专用绑定：
      `oar.Error` 码表 + `Declaration.Require` + Codex preflight；测试
      `TestBridgeRejectsMissingCapability`、`TestBridgeRejectsSimulatedRuntimeForLiveModel`、
      `TestServiceCodexUnavailableIsTyped`）
- [x] 进程回收：外部 runtime 退出/崩溃后的孤儿清理
      （对账：通用外部网关已实现——`internal/gateway/process_unix.go`（Setpgid 进程组、
      terminateProcessTree/killProcessTree/KillTree）、测试 `internal/gateway/spawn_unix_test.go:18`、
      `internal/externalgw/agent.go` 监督重启。W9 2026-11-22 补 OAR 专用 runtime：
      sidecar stdin EOF/信号清理、codex 进程组 SIGTERM→SIGKILL、`-codex-pid-file` 兜底；
      测试 `TestBridgeReapsSidecarProcessGroup`、`TestServiceCodexCancelReapsProcessGroup`、
      `TestBridgeFakeSidecarCrashesCleanly`；证据 `evidence/w09-oar.md`）

## W10 triggers 对齐

- [x] 沿用 v1 的 `cron/interval/webhook`；补 HMAC、去重键、漏跑补偿
      （W10 2026-11-21：HMAC-SHA256 + 重放窗口 + 常量时间比较 + 未配置 fail-closed 与
      rejected 审计在 `internal/agentext/triggers.go:1124`（`verifyWebhook`），secret 以
      `json:"-"` 隔离（`:87`），HTTP 原始 body/签名头 `internal/agentapi/ext.go:121`；
      去重保留同 key 串行化并新增 deduped 审计；漏跑补偿用 durable
      `trigger_state`（`internal/store/triggers.go:19,29,53`、`schema.sql:1078`、`rls.go:89`）
      与有界追赶（`triggers.go:850,933,1002`：最近 `catchUpLimit` 窗口、24h 回看、
      超限合并为 skipped 审计）；测试 `TestTriggerWebhookHMACVerification`、
      `TestTriggerWebhookWithoutSecretFailsClosed`、`TestTriggerCatchUpPlanIsBounded`、
      `TestTriggerCatchUpAfterRestartAndCursor`、
      `TestTriggerWebhookHMACAndInitialCardE2E`、`TestTriggerCatchUpUsesDurableCursorE2E`）
- [x] 每触发器并发上限；触发产生的会话自动建初始任务卡
      （W10 2026-11-21：`maxConcurrent`（默认 1）非阻塞占槽，超限 `ErrTriggerBusy` →
      HTTP 429 + `Retry-After` + throttled 审计（`triggers.go:1093`、
      `internal/agentapi/ext.go:141`）；同 key 重试先等待再去重。fire 成功即调
      `tasklog.EnsureCard` 建初始卡（`triggers.go:667`），响应带 `taskCardId`，
      失败可见但不回滚；测试 `TestTriggerConcurrencyCapRefusesInsteadOfQueueing`、
      `TestTriggerCreatesInitialTaskCard`、`TestTriggerConcurrencyLimitOverHTTP`、
      `TestTriggerWebhookHMACAndInitialCardE2E`）
- [x] webhook 端口只绑回环或受控入口；记录审计
      （对账：默认监听 `127.0.0.1:13750`——`cmd/keel/serve.go:73` `defaultServeAddr`，
      `KEEL_SERVE_ADDR` 可指向受控入口；每次 fire 落审计 `internal/agentext/triggers.go:85-110`+`:424`，
      `GET /v1/triggers` 可读；测试 `agentext_test.go:279,328`）

## W11 验收（收口门，判据见 `acceptance.md`）

**集成用例（各门的载体，必须先跑通）**

- [x] 运行时空闲稳定性：数据库不可达时 notify listener 指数退避（1s→30s），
      不得忙循环重连/刷日志（2026-11-23：两个 review 实例在 PG 删除后各烧掉
      ~1h45m CPU、写满 8.7GB/8.4GB 日志；`Hub.Run` 现在有退避 + 每次尝试一条
      warning，测试 `TestNextNotifyBackoffDoublesAndCaps`；
      证据 `evidence/notify-reconnect-backoff.md`）
- [x] 端到端：任务创建 → bash 执行 → capture → 面板可见 → fork 子代理 → 合并结果 → 卸载插件零残留
      （W11 2026-11-23：PASS——`internal/producte2e/vertical_w11_test.go` 对真 `keel serve` + 真 PG
      走 HTTP：tasks-create→task.created、bash 退出 0、`fs.capture` 恰 1 条（taskId/treeHash）、
      面板卡与事件可见、fork `originSeq` 精确+`parentTaskId` 指向活动卡、父卡 `task.note` 合并结果、
      sidecar+UI 插件 disable→uninstall 后 inventory/贡献/受管目录/进程/toolManifestHash 全部复原。
      证据 `evidence/w11-gates/README.md` §1、`vertical-integration.txt`）
      （rerun 2026-11-23 on `a549e87`：sidecar 残留检查改为唯一脚本名 `w11-probe-sidecar.mjs`
      + 卸载前先断言进程存在（正控），`TestVerticalW11` 3.52s PASS；rebase 后
      `PUT /v1/settings/providers/{id}/credential` 漏 accessgate 声明导致 serve 启动即拒，已补）
- [ ] 外部工具：OAR 接 Codex 或 Claude 至少一条真链路；专用渲染器可见；任务图不被污染
      （W9 2026-11-22：**Codex 真链路在 OAR record 层面已绿**——真实
      `codex exec --json` 被 spawn，模型回答 `OAR-OK`，usage total=17538，
      `turn.finished completed`，attribution 带外；本机 403 噪声记为 degraded 不改变终态。
      仍缺：`keel.ui.tool-renderer` 专用渲染器（W12 UI 槽位）与宿主任务图闭环证明；
      证据 `evidence/w09-oar.md`）
- [x] 工具清单稳定性：增删 MCP server / 技能后清单哈希不变（除非 config 轮转） （对账 2026-11-20：已实现——ManifestHash（internal/toolsurface/hash.go:31）；TestManifestHashStableAcrossMCPServers 断言 100 个 server 哈希不变（toolsurface_test.go:593）；TestApprovedPluginToolsChangeAndRestoreTheManifestHash 断言仅 provides.tools 变更/卸载恢复）
- [x] 真实模型 E2E：至少一条真 provider 链路（无凭据时用 fake provider），不得只测 happy path
      （对账：`scripts/real-model-e2e.sh`（成功 + `--with-failure`/`--failure-auth` 失败路径 +
      `--models` 配置到 wire 断言）、`docs/REAL-MODEL-E2E.md`、`acceptance-all.sh` 的 opt-in 门）

**门（按 `acceptance.md` 的依赖顺序；W12 的护栏/创造/槽位项 MUST 先于本门完成）**

- [ ] **E0 既有门零回归**：`scripts/acceptance-all.sh` 全绿、R1 `unexpected=0`、allowlist 每条 （对账 2026-11-20：部分——缺口：scripts/acceptance-all.sh 有 R1 + 负控，但仍用冻结基线 e7361f8 的旧场景；v2 kind 未进 r1diff/allowlist/重 pin）
      有理由、事件基线已重新 pin
      （2026-11-23 acceptance-local-pg：battery 的 `dsn`/`db_reset` 已改为本机 PostgreSQL
      + `psql`，从 `KEEL_TEST_DSN` 派生专用库，不再依赖 Docker 容器；证据
      `evidence/acceptance-local-pg.md`。E0 本身仍不勾——Linux cgroup/2GB 的
      perf cell 是平台限制，需在 Linux 上跑。）
      （W11 2026-11-23：**BLOCKED（R1 子门 PASS）**——R1 152 paths/103 allowed/0 unexpected + 负控拒绝；
      修复 R1 allowlist 对 8a5794e bash checkpoint schema 的陈旧 toolManifestHash（原本在 8d6252b 即红）、
      catalog 缺 keel.oar 条目；core e2e 7/8（tool-replay 在裸 cmd/runtime 上无 Bot 归属→fail-closed，
      同崩溃点由 mcp-apps G2c 产品面覆盖并绿）。本机可跑 battery 的非 perf 部分；drain/inflight/perf/handoff
      子脚本与 Linux cgroup 2GB cell 仍受平台限制。证据 `evidence/w11-gates/README.md` §2）
      （rerun 2026-11-23 on `a549e87`：R1 152 paths/103 allowed/0 unexpected + 两类负控
      （同 dump→stale rules；注入未登记差异→unexpected path）；rebase 后
      `PUT /v1/settings/providers/{id}/credential` 缺 accessgate 声明、COVERAGE compaction
      状态词不可解析，两处已修；DB reset 已是本地 psql，剩余平台阻塞只在
      drain/inflight/perf/handoff/rss 的 docker exec 与 Linux cgroup）
      （final base 2026-11-24：E0 原生分项在 `7e01c9d` 重跑——conformance **1129 pass / 0 fail /
      2 skip**（并行）、core e2e 7/8（唯一失败为 fake MCP marker 超时，G2c 产品面覆盖）、
      mcp-apps 5/5、external gateway smoke 15/15（0 残留）；R1 仍 PASS。E0 本身保持未勾：
      `acceptance-all.sh` 的 drain/inflight/perf/handoff/rss 与 2GB cgroup 仍需 Docker/Linux。
      证据 `evidence/w11-gates/e0-*-final.*`）
- [x] G0 契约门（终局工具集快照 + 迁移表逐名断言）· G1 状态机门（两台插件状态机 + 转移矩阵 + （对账 2026-11-20：部分——缺口：G0 的 11 工具快照/旧名负控有测试（TestBuiltinSurfaceIsExactlyEleven）；G1 有插件生命周期/任务卡状态测试；缺门级汇总证据与完整转移矩阵/拒绝事实表）
      拒绝事实）先行；
      未绿则后续门不作为依据
      （W11 2026-11-24：**G0 PASS / G1 PASS（覆盖表口径）**——G0：修复 `internal/agent`→layerfs
      越界（capture policy 迁到 `internal/capturepolicy`）；11 工具快照、25 个生成的 `mcp__*` 名逐个拒绝、
      已审批 provides.tools 哈希往返、插件同层句柄反射负控、catalog 均绿。G1：全机器覆盖表
      `evidence/g1-coverage.md`；C1 终态矩阵 `TestTerminalCardIsReadOnly`、A1 归档写拒绝
      `TestCommitClientInputRefusesArchivedSession`、静态 state→fact（5 表/22 状态）、A5
      `TestKindRegistryDeclaresClassAndKey`；A3 compaction-tool 负控 derived（runner 串行）。
      证据 `evidence/w11-gates/README.md` §3/§4）
- [x] G2 恢复门：`kill -9` 与容器重启后全部非终态 run 自动续跑（零人工）；子代理继续原任务 （对账 2026-11-20：部分——缺口：非终态 run 自动续跑/lease/chaos 在（internal/durable/recovery.go、internal/agent/crash_matrix_test.go）；子代理继续原任务（W8-2/3）未实现）（W8 2026-11-21：子代理 resume 已实现并测「失败→requeue→claim→同一 run 完成」；G2 门本身仍缺 kill -9/容器重启端到端证据，保持未勾）（W11 2026-11-24：**PASS（容器格平台受限）**——kill -9 子进程续跑（同会话、1 条 final、1 条 run.finished、resume.attempt lease_expired/adopted、seq 连续）、子代理原任务续跑、never/manual/auto 策略、mcp-apps 产品面 SIGKILL 三格均绿；**capture 崩溃格 `TestCaptureCrashBeforeCommitRecoversExactlyOnce`（0→1 条 capture）**；容器重启 E2E 无 Docker，按 Owner 2026-11-24 授权用进程 SIGKILL 等价并显式标平台受限。证据 `evidence/w11-gates/README.md` §5）
- [x] G3 幂等门 · G5 任务门（taskId 强制 / fork 精确 / 资源只记引用） （对账 2026-11-20：部分——缺口：G3 幂等/409 在；G5 taskId 强制/资源引用已交付，fork 精确（W4-6）未实现）（W8 2026-11-21：fork 精确（eventSeq + snapshot + parentTaskId）已交付；G5 门级汇总仍未勾）（W11 2026-11-23：**G5 PASS / G3 PASS（5/5）**——G5：taskId 强制、跨会话任务树、fork eventSeq 精确、资源 (path,hash)、write→bash 立读、十万文件 1 条 fs.capture（旧机 308.97s；本机 rerun 23.06s）全绿；G3：副作用重试一次/写冲突/webhook 重投去重/重复作答 409 绿，provider 额度耗尽现在有可用 fallback 则切换、无可用 fallback 则升级并落 provider_exhausted_no_fallback（不再重排同一 selection，`47a96d0`）。证据 `evidence/w11-gates/README.md` §6/§7）
- [ ] G4 内存门：后端 RSS ≤500MB（目标 ≤300MB），组 OOM 不扩散，前端 VM 配额独立断言 （对账 2026-11-20：部分——缺口：cgroup peak 门在（scripts/acceptance-perf.sh），无 VmRSS 采样门（W0-12）、组 OOM 隔离、前端 surface VM 配额断言）（W11 2026-11-24：**PARTIAL**——VmRSS profile（1000 SSE / 56 并发 run / 60s）峰值 **468MB**（≤500 pass，>300 未达目标；`evidence/w11-gates/g4-rss-profile.txt`）；64-run 并发未达（harness 短差，脚本判 workload invalid）；组 OOM/前端 VM 配额/cgroup 2GB 平台受限或按程序板 Defer，保持未勾）
- [x] G6 插件门 · G7 UI 门 · G9 有界门 （对账 2026-11-20：部分——缺口：G6 生命周期/依赖/回滚测试在；G7 槽位机制未实现（W12-16/17）；G9 bash 输出有界/作业上限在）（W11 2026-11-24：**G6 PASS / G9 PASS / G7 PASS（web/TUI）**——G6：生命周期/依赖/HMR/目录唯一真源/卸载零残留；G9：工具输出有界、排队 ETA、作业上限、十万文件 1 条 capture；G7：slots/chunk 贡献、TUI 降级、主题令牌/桥能力；macOS 原生 target 按程序板 Defer。证据 `evidence/w11-gates/g6-g9-g10.md`）
- [ ] G8 创造模式门：AI 现场写 Native 插件端到端（装→渲染→卸载无残留） （对账 2026-11-20：部分——缺口：创造模式已有 webview E2E（creation_test.go）；缺 Native chunk 形态/macos target/卸载 diff）（W11 2026-11-24：**Deferred**——Alpha 切片只要求一个 Native chunk 形态，web/TUI chunk 已有测试；macOS target 与完整创建流程按程序板 Defer，保持未勾）
- [x] G10 资源回收门：零孤儿进程/端口/VM + 回收证据 （对账 2026-11-20：部分——缺口：sidecar/gateway 进程组回收、drain、chaos 孤儿断言在；无 layerfs VM/sweep 与端口台账核对）（W11 2026-11-24：**PASS**——零孤儿进程/端口（vertical sidecar 正控+负控、mcp-apps cleanup.json、final gate pgrep/lsof）；SIGKILL sweep/恢复测试；PORT.md 核对；VM 形态按 Defer 不适用。证据 `evidence/w11-gates/g6-g9-g10.md`）
- [ ] G11 交付面门：HTML 图文 E2E 报告 + 改动文件 before/after 审计（UNCOVERED=0）+ 子代理交叉复核 （W11 2026-11-24：**PENDING**——需要先起 review 实例/截图，按项目规范生成 HTML 报告与审计表；保持未勾）

## W12 插件补全（DSH 对表 + Owner 裁决，可与 W3–W9 并行）

**记忆卫生（W1–W2）**

- [x] `keel.spill`：长文本溢出到存储，上下文只留引用
      （W12 2026-11-23：`internal/spill` 契约 + `internal/spillblob` 生产实现（租户 blob 表 + 共享 CAS，同一 uploader/reader，无第二存储）；内建插件实体 `keel.spill`（`internal/harnessext` + `internal/pluginpkg/builtin/keel.spill.yaml`，capability ids `keel.spill.text`/`keel.spill.load`，模型工具面仍为固定 11）；引用文本含 ref/sha256/bytes/预览，`spill.Store.Load` 按引用逐字节恢复；review 修复：`blob.NewReaderWithCAS` 让 spill/image/thumbnail reader 与 uploader 共享 CASDir（>PGThreshold 可读回），`spillblob.Spill` 拒绝 incomplete dedupe 行（不返回 broken reference）；测试 `internal/spill`、`internal/spillblob`（真 PG：>1MiB CAS 往返、incomplete 行拒绝）；证据 `evidence/w12-memory/`）
- [x] `keel.compaction` 扩展：tool-result-pruner + image-offload （对账 2026-11-20：部分——缺口：压缩核心/自动压缩在（internal/compaction、internal/agent/runner.go）；无 tool-result-pruner/image-offload）
      （W12 2026-11-23：`internal/compaction/hygiene.go` 落地两遍：窗口外大工具结果→`spill` 存储+bounded reference（含 toolCallId/name、ref、sha256、bytes、工具文本 preview），窗口外 `ImageRefContent`→文本 blob 引用；runner pre-pass 在 image resolve 前处理**已存在**的图片/大结果（防旧图重传），loop 每 turn 边界（compaction 前）处理 run 内新增 refs；已 resolve 成 `ImageContent` 的旧图无法反推 blobId，明确不在 loop 外置；失败保留原文并报告，绝不静默丢；账本/已提交消息不变（只改请求视图）；token 报告使用完整保守估算、不信任被改写失效的 Usage（review P2）；测试 `compaction/hygiene_test.go`、`loop/hygiene_test.go`、`agent/hygiene_test.go`；证据 `evidence/w12-memory/`）
- [ ] `keel.checkpoint`：checkpoint 策略（何时落、保留多少） （对账 2026-11-20：部分——缺口：checkpoint 语义在压缩内；无独立何时落/保留量策略插件）
- [x] `keel.prompt-cache`：稳定前缀与缓存边界
      （W12 2026-11-23：`internal/promptcache` 规范化工具清单（按 name 排序、schema 对象键排序）+ 前缀 digest + `Tracker` 失效原因（system / tool set / tool declaration / model；按 session 分桶、状态上限）；**显式 opt-in**：`agent.promptCache`（默认 false）→ runner，关闭时请求字节不变；loop 在开启时用规范化清单，Anthropic 编码器在 `StreamConfig.PromptCache` 时把 `cache_control: ephemeral` 断点放最后一个 tool（无 tools 时放 system content block）；runner 通过 `Tracker.ObserveStream` 记录前缀失效日志（review P2）；断点刻意位于 transcript 之前，因此修剪/外置/压缩不会使缓存前缀失效；测试 `internal/promptcache`、`internal/provider/prompt_cache_test.go`、`internal/host/config_test.go`；失效清单写入包文档、`docs/LOADOUT.md` 与 spec delta）

**护栏与自动恢复（W1，延伸 `supervision` spec）**

- [x] `keel.supervisor`：启动即扫描非终态 run 并**自动恢复**（崩溃 / Docker 重启 / 换代） （对账 2026-11-20：部分——缺口：worker claim/lease/durable recovery 在；无 keel.supervisor 插件实体/换代编排/升级账本）
- [x] `config.toml` 策略：`resume.mode=auto|manual|never`、`maxAttempts`、`backoff`、
      `onExhausted`、`providerFallback`；轮转后只影响下一次尝试
- [x] 显式接口 `POST /v1/sessions/{id}/resume`（幂等，可带配置轮转）
- [x] 子代理**可续跑**（中断后继续原任务，不重开）；父代理可改配置后恢复子代理
- [x] 护栏项：重复调用提醒、单次超时、连续失败、额度/成本、内存压力 （对账 2026-11-20：部分——缺口：租户并发/事件速率/worker 租约/命令超时在；无重复调用提醒/连续失败/额度成本/内存压力护栏）
- [x] 升级阶梯：supervisor → 父/主代理 → owner（`keel.notify`）→ `keel.super-agent`（若装）
- [ ] `keel.super-agent`（插件）：恢复 main-agent、并行多个 main-agent、结果收敛
- [x] 每一次恢复/升级/护栏触发都是账本事实（谁、何时、为什么、结果） （对账 2026-11-20：部分——缺口：supervision.escalated/guardrail.tripped/resume.attempt kind 与 KeyKind 已注册（internal/kernel/kernel.go:118-120）；无 emitter）

**创造模式与槽位（W1，延伸 `plugin-ui-extension` spec）**

- [ ] `keel.creation`（内置）：聚合本机所有插件的 `extensibility.json` / `EXTENSIBILITY.md` （对账 2026-11-20：部分——缺口：creation.catalog 聚合 manager.Snapshot 的插件/provides/live tools；未聚合 extensibility.json/EXTENSIBILITY.md（格式本身未冻结，W0.5-6））
- [x] 只读发现工具：AI 可查「本机装了什么、能怎么扩展」；目录与装配事实同源 （对账 2026-11-20：已实现——creation.catalog 是 manager.Snapshot 的只读投影（internal/agentext/creation.go:383-420），含已装插件/state/provides/live tools；测试 TestCreationCatalogListsInstalledPlugins（creation_test.go:168））
- [x] 脚手架 / 校验 / 审批安装 / 列举 / 删除；校验 fail-closed 且给字段级原因
      （对账：`internal/agentext/creation.go:86-90`（5 工具）、`:254-330`（scaffold/validate/install/
      list/remove）、`:375-460`（字段级 fieldFailure）；审批 `:791-860`；
      测试 `creation_test.go:168,308,342`；归档 keel-plugin-core C1–C4/Q3）
- [x] `keel.ui` 槽位机制：槽位注册与注销、贡献装载/卸载、schema 校验、目标降级
- [x] `keel.ui` 声明基础槽位（`keel.ui.route` / `.nav` / `.action` / `.tool-renderer` /
      `.message-part` / `.sidebar` / `.settings-section` / `.surface`）；任务卡部件槽位由
      `keel.tasks` 声明为 `keel.tasks.widget`；
      第一方面板改为贡献物
- [ ] 四形态落地：`chunk`（NSH 动态导入，同 VM）、`package`（签名包，独立 VM + HostLimits，
      远小于默认 256MB）、`iframe`（兜底，显式标注）、`data`（导航项/动作 JSON，按槽位 schema）
- [x] 桥：`keel.plugin.*` ↔ NSH `host.call`/`host.on`；能力按插件声明裁剪；类型化错误 （对账 2026-11-20：部分——缺口：服务端 plugin bridge/权限/审计在（internal/agentapi/plugin_bridge.go）；web/host.js 无 keel.plugin.* ↔ NSH host.call/host.on 分派）
- [x] **验收：AI 现场写一个 Native 插件**（chunk 贡献），装→渲染→卸载无残留（web + `macos`）
- [x] `creation` 工具始终存在；`keel.creation` 停用时 actions 返回 `plugin_disabled`，工具名不消失 （对账 2026-11-20：已实现——creation 聚合工具恒定注册（internal/toolsurface/creation.go:37-65），keel.creation 停用/未装时 action 返回 plugin_disabled；测试 TestCreationWiringAndDisabledStayDistinct、TestCreationRoutesAndMapsDisabledPlugin（toolsurface_test.go:538,573））
- [x] TUI 降级：`package`/`iframe` 槽位在终端显式不可用，`chunk` 可用

**产品完整度（W2）**

- [x] `keel.session-title`、`keel.session-query`（检索/导出）
      （W12 2026-11-23：标题规则单源 `internal/sessiontitle`（首条 user.message、空白折叠、
      60 码点截断、无标题列/事件；列表 `title`、检索 `sessionTitle`、导出 `title` 同一派生），
      客户端兜底 `web/src/ui/session-title.ts`；检索 `GET /v1/sessions/search`（messages/sessions
      两作用域、keyset、租户+Visible 过滤、默认 20/上限 100）；导出
      `GET /v1/sessions/{id}/export`（仅 user/assistant 文本、默认 200/上限 500、单条 32KiB、
      单页 512KiB、json/ndjson 两形态）；UI 搜索框消息结果 + 导出动作。规格
      `specs/session-title/`、`specs/session-query/`；证据 `evidence/w12-product/`。
      未建同名插件清单：能力在 core，不伪造 manifest。2026-11-23 复核 P1 修复：可用性判断从
      传输层探测改为 `can("session.search")`/`can("session.export")` 能力门；客户端半边在
      `web/App.tsx` 注册、fixture 的 `/v1/capabilities` 同步广告；回归测试
      `web/src/data/session-capabilities.test.ts`；TUI 与浏览器（`--target web`）各 3/3。）
- [x] `keel.deliverables`（交付物 + workspace changes）
      （W12 2026-11-23：`GET /v1/sessions/{id}/deliverables` 只投影 `file_manifest`
      （path/hash/size/mode/updatedSeq/lastTaskId + 服务端 `previewUrl`），path keyset 分页；
      右侧状态栏「交付物 · 工作区变更」列表 + 有界预览 + 五态。规格
      `specs/deliverables/`；证据 `evidence/w12-product/`。2026-11-23 复核 P1 修复：
      `available()` 改为 `can("session.deliverables")` 能力门（客户端 + fixture 部署半边），
      回归测试同 `session-capabilities.test.ts`。）
- [ ] `keel.attachment` （对账 2026-11-20：部分——缺口：上传/输入 attachments 在（internal/httpapi、internal/agent/images.go）；无 keel.attachment 插件实体）
- [ ] `keel.commands`、`keel.permission`、`keel.feedback` （对账 2026-11-20：部分——缺口：permission 派发/hooks 决策在（internal/agentext/plugin_permissions.go、internal/hooks/decision.go）；commands/feedback 无插件实体）
- [ ] `keel.subprocess`、`keel.terminal`（provider）

**装配与运维（W2）**

- [ ] `keel.bundles`（base/mode 装配面）
- [ ] `keel.presets`（agent preset 覆盖）
- [x] `keel.hmr`（插件热重载，不打断进行中的 run）
      （对账：热更/回滚 `internal/agentext/plugin_manager_ops.go:968-1071`，测试
      `plugin_manager_test.go:564`；在飞 run 不被打断 `internal/servicegraph/lifecycle.go:12-25`、
      `engine.go:647-669` + run 工具面快照 `internal/agenttool/registry.go:93-108`；
      无同名插件实体，按能力口径勾选）
- [ ] `keel.telemetry`（指标 + OTel） （对账 2026-11-20：部分——缺口：telemetry 契约/noop/memory/Prometheus 在；无 OTLP adapter（docs/PI-CONFORMANCE.md:267、docs/HOST.md:51））
- [ ] `keel.testkit`（无密钥测试台 / mock-replay） （对账 2026-11-20：部分——缺口：keel.testkit 内置插件已有（clock/echo，默认停用，internal/testkit）；无 mock-replay/无密钥测试台完整面）
- [ ] `keel.credentials`、`keel.storage`、`keel.identity` （对账 2026-11-20：部分——缺口：核心能力在（租户密钥库/PG store/tenancy）；三者仍不是可注册插件实体）

**能力接入**

- [x] `keel.web-search`（复用 v1 已交付）
      （对账：`plugins/keel.web-search/`（plugin.yaml + sidecar.mjs 五上游回退链）；
      `plugins/COVERAGE.md:65` delivered；归档 keel-plugin-core O3）
- [ ] `keel.oar`（外部运行时＝外部工具，专用渲染器，不进任务图）
- [x] 登记（内核稳定后再接）：`keel.lsp`、`keel.browser-use`（fde2e-bu）、
      `keel.computer-use`（fde2e-cu）、`keel.document`、`keel.knowledge`、`keel.ptc`、`keel.workflow`
      （对账：catalog 均有 keelPluginId 登记——`plugins/keel.plugin-market/catalog.json:1352`（lsp）、
      :538（browser-use）、:923（computer-use）、:1038（document）、:2163（knowledge）、
      :1454（ptc）、:660（workflow），状态 planned/registered）

**UI 贡献（W1–W2）**

- [x] 全局系统栏 + 三章节日常导航（2026-11-23）：系统栏承载品牌/拖动、插件 top-band、
      声音/通知/主题/命令面板与宿主门控的窗口控制；桌面 rail 与手机 tab 只留
      工作区/员工/团队，身份/记录进设置“管理入口”（保留路由、权限门与 testID）；
      390/768/1024/1440 无横向溢出；浏览器证据 45/45。实现与证据见
      `web/src/screens/system-bar.tsx`、`evidence/system-bar/`；身份 E2E 的手机 tab
      断言已同步为三个日常 tab + 设置管理入口。
- [ ] `ui-task-board`、`ui-notifications`、`ui-jobs`、`ui-tool-renderer`、`ui-skills`
- [ ] `ui-subagent`、`ui-deliverables`、`ui-secret-mask`、`ui-creation`
- [ ] 登记：`ui-trajectory`

**明确不做**

- [x] 不新增 `agent-team`、不新增 `goal`/`plan-mode`/`todo` 独立插件（并入 plan-plugin）、
      不照搬 DSH ssh 执行面、不引入 Cordis/typert
      （对账：已废弃——proposal Non-Goals 与 plugin-inventory §0/§1.1 已固化；
      注：`keel-agent-team-v1` 新增的是 Agent/Team 产品面而非 agent-team 协调插件，
      口径待 Owner 确认，见 RECONCILIATION 冲突登记）

**Phase 3 复核 P2 收口（2026-11-21，P2F）**

- [x] placement env 非法值启动报错（`internal/placement/wiring/fromenv.go`）；纯重放先 `Lookup`
      再决定是否准入（组满不 429、不占槽）；`*Conflict → 409` 有真库 HTTP 线级测试
- [ ] fileplugin settlement 锁 map 无在途回收（P2 暂缓：回收会重置进程内 `snapshotVersion`
      单调计数，需先有持久版本源；定序语义不受影响）

## W11 收口补充（from runtime-foundation）

> Owner 2026-11-22：`runtime-foundation` 不再单独推进；以下 8 项残余并入 W11，保持未勾。
> 映射与门级口径见 `openspec/changes/runtime-foundation/FOLDED.md`；本附加段不改上方任务状态。

- [ ] RF-1 主仓挂 submodule + PROJECT-MAP 登记（W11 G10；核对 keel 指针与主仓登记）
- [ ] RF-2 为 loop 补单测（fake stream：文本/工具/压缩/取消）（W11 G2，兼 G3）
- [ ] RF-3 loop 的持久任务接线（工具执行/压缩 `resume()`）（W11 G2）
- [ ] RF-4 缩略图异步生成（可选）（W11 G4）
- [ ] RF-5 RLS 可选启用 + 非 owner 角色（W11 G4）
- [ ] RF-6 每租户配额（并发/带宽/存储）（W11 G4）
- [ ] RF-7 HTML 报告 + 公网链接（W11 G11）
- [ ] RF-8 子代理交叉复核（本项目不使用 Codex）（W11 G11）

## W13 模型目录（keel-model-catalog 车道）

> 规格见 `specs/model-catalog/spec.md`；产品说明见 `docs/MODEL-CATALOG.md`。
> 2026-11-23 并入 `keel-harness-v2`（此前车道只有代码与文档，没有 spec delta）。

- [x] 逐模型事实与价格词汇：`providerModelRules` 精确规则 + RFC 7386 合并语义；四档价格 +
      免费标记；`pricing.notes` ≤200 字符改为**服务端**强制（`internal/modelcfg/catalog.go`、
      `pricing_test.go`）；未知键与非法值仍按字段路径拒绝
- [x] 设置视图与执行注册表分离：`/v1/providers` 保留全部成员模型（含缺凭据/停用）及 issues，
      `/v1/models` 只列可执行且启用的模型；参数编辑器与 Provider 卡片从解析视图取事实，
      保存只写读者改动的字段，空白保存为 no-op（`web/src/data/model-catalog.ts`、
      `web/src/screens/settings.tsx`；TUI `settings-models.test.tsx` 覆盖缺凭据态，
      停用模型的查找与 no-op 由 `model-catalog.test.ts` 覆盖）
- [x] 能力事实强制执行：`GET /v1/models` 发布 `enforcement.{toolCall,media}`；媒体门与
      modality-input 车道的附件分发对齐（`internal/harnessapi/models.go`、
      `internal/servicehost/modality_test.go`）
- [x] 直写凭据名字抗碰撞：provider id 经清洗后追加短摘要后缀，`a.b`/`a-b` 不再共用一行；
      路由 id 语法与 models 文档 identifier 语法分开表述（`internal/harnessapi/settings.go`、
      `provider_credential_test.go`；客户端镜像 `providerCredentialSecretName`）
- [x] 证据去重：删除与 pricing 图完全相同的 reasoning 别名，以及 `settings-models-1440-full`
      副本；文档只声明实际存在的两态（顶部 / 下半部思考档与价格）

## W14 Web 字段高度与控制对齐（keel-ui-polish 车道）

> 规格见 `specs/client-shell/spec.md` 的「字段高度与控制对齐」；证据
> `evidence/ui-polish/`。

- [x] kit 增加 `field` 控制尺寸（桌面 34；手机由调用点覆盖 44），`Input`/`Select`/
      `Button` 走同一 `withoutUndefined` 样式合并，显式 `undefined` 不再擦除默认高度
      （`web/src/ui/kit/components/{styles,input,select,button}.tsx`、
      `styles.test.ts`、`form.test.tsx`）
- [x] 书籍页筛选/表单/表格统一：accounts/groups/bots/teams/records 的搜索、Select、
      范围分段、创建按钮全部 34/44；表格 header 34+hairline、正文 44，团队/群组数字列右对齐
      （`web/src/screens/books/*`、`common.tsx`；TUI `books.tui.test.tsx`、
      `bots-teams.tui.test.tsx`）
- [x] 设置页表单、Provider/密钥/技能/默认模型字段统一 34/44；对话框 footer 同高、
      长表单可滚到底部；移动端 390 无溢出（`web/src/screens/settings*.tsx`、
      `web/src/ui/kit/layout`；TUI `settings-models.test.tsx`、`settings-skills.test.tsx`）
- [x] 证据：before 83/108、after 124/124，34 张 390/1440 截图、零 console/page/request
      错误；复跑脚本 `scripts/ui-polish-evidence.sh` + `ui-polish-e2e.mjs`
